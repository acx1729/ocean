package query

import (
	"fmt"
	"strings"
	"time"

	"cel.dev/cel-go/common/ast"
	"cel.dev/cel-go/common/operators"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
)

// sqlType is the PostgreSQL type of a scalar fragment or bind parameter.
type sqlType int

const (
	tUnknown sqlType = iota
	tText
	tUUID
	tInt
	tNumeric
	tReal
	tBool
	tTimestamp
	tInterval
	tLquery
)

// cast returns the SQL type name used in explicit parameter casts.
func (t sqlType) cast() string {
	switch t {
	case tText:
		return "text"
	case tUUID:
		return "uuid"
	case tInt:
		return "bigint"
	case tNumeric:
		return "numeric"
	case tReal:
		return "real"
	case tBool:
		return "boolean"
	case tTimestamp:
		return "timestamptz"
	case tInterval:
		return "interval"
	case tLquery:
		return "lquery"
	}
	return "text"
}

// srcKind identifies a multi-valued source that comprehensions, `in` and
// size() iterate over.
type srcKind int

const (
	srcPropList  srcKind = iota // relation / multi_select property rows
	srcEdgesOut                 // edges_out(rel): target ids
	srcEdgesIn                  // edges_in(rel): source ids
	srcPath                     // path: ancestor ids incl. the block itself
	srcConstList                // a constant list literal
)

// listSrc describes a multi-valued source.
type listSrc struct {
	kind  srcKind
	prop  *Property
	relID string
	vals  []string
}

// frag is the translation of one CEL sub-expression. Exactly one of the
// representations is populated: a rendered scalar/predicate (sql), a
// constant (isConst/val), a single-valued property reference (prop), a
// multi-valued source (list) or a list literal (isList/items).
type frag struct {
	sql        string
	t          sqlType
	nullable   bool // rendered column may be NULL (type_id, parent_block_id, key)
	isPred     bool // sql is a boolean expression
	twoValued  bool // predicate never yields NULL
	isConst    bool
	val        any // string, int64, float64, bool, time.Time, time.Duration
	prop       *Property
	list       *listSrc
	isList     bool
	items      []frag
	selectOf   *Property // string values are option names of this property
	isTypeName bool      // the `type` variable: compared by name, stored as id
}

func predFrag(sql string, twoValued bool) frag {
	return frag{sql: sql, isPred: true, twoValued: twoValued, t: tBool}
}

func boolFrag(b bool) frag {
	f := predFrag("FALSE", true)
	if b {
		f.sql = "TRUE"
	}
	f.isConst, f.val = true, b
	return f
}

func constFrag(v any) frag {
	return frag{isConst: true, val: v, t: sqlTypeOf(v)}
}

func sqlTypeOf(v any) sqlType {
	switch v.(type) {
	case string:
		return tText
	case int64:
		return tInt
	case float64:
		return tNumeric
	case bool:
		return tBool
	case time.Time:
		return tTimestamp
	case time.Duration:
		return tInterval
	}
	return tUnknown
}

// Resolved lists the schema objects an expression referenced, keyed by the
// symbol used in the expression and valued by id.
type Resolved struct {
	Properties    map[string]string
	Types         map[string]string
	RelationTypes map[string]string
}

func newResolved() Resolved {
	return Resolved{Properties: map[string]string{}, Types: map[string]string{}, RelationTypes: map[string]string{}}
}

// Cost weights of the estimate (specification section 8).
const (
	costNode    = 1
	costExists  = 5
	costRegex   = 10
	costLimit   = 50
	maxASTNodes = 200
	maxInList   = 1000
)

// sqlGen walks a checked AST and builds one parameterized predicate.
type sqlGen struct {
	env         *Env
	a           *ast.AST
	workspaceID string
	caller      string
	now         time.Time
	args        []any
	wsParam     string
	nowParam    string
	callerParam string
	cost        int
	hasSearch   bool
	searchParam string
	aliasN      int
	scopes      []map[string]frag
	resolved    Resolved
}

func newSQLGen(env *Env, a *ast.AST, workspaceID, caller string, now time.Time) *sqlGen {
	return &sqlGen{env: env, a: a, workspaceID: workspaceID, caller: caller, now: now, resolved: newResolved()}
}

// param appends a bind parameter and returns its placeholder with a cast.
func (g *sqlGen) param(v any, t sqlType) string {
	g.args = append(g.args, v)
	return fmt.Sprintf("$%d::%s", len(g.args), t.cast())
}

// paramArray appends an array bind parameter.
func (g *sqlGen) paramArray(v any, t sqlType) string {
	g.args = append(g.args, v)
	return fmt.Sprintf("$%d::%s[]", len(g.args), t.cast())
}

// workspaceParam returns the shared workspace id parameter.
func (g *sqlGen) workspaceParam() string {
	if g.wsParam == "" {
		g.wsParam = g.param(g.workspaceID, tUUID)
	}
	return g.wsParam
}

// alias returns a fresh table alias with the given prefix.
func (g *sqlGen) alias(prefix string) string {
	g.aliasN++
	return fmt.Sprintf("%s%d", prefix, g.aliasN)
}

func (g *sqlGen) unsupported(e ast.Expr, format string, args ...any) *CelError {
	return errAt(g.a, e.ID(), ErrUnsupported, "", format, args...)
}

// baseWhere renders the mandatory tenant, soft-delete, project and access
// predicates.
func (g *sqlGen) baseWhere(projectID string, sets []string) string {
	parts := []string{"b.workspace_id = " + g.workspaceParam(), "b.deleted_at IS NULL"}
	if projectID != "" {
		parts = append(parts, "b.project_id = "+g.param(projectID, tUUID))
	}
	if sets != nil {
		parts = append(parts, "b.doc_id IN (SELECT da.doc_id FROM doc_access da WHERE da.workspace_id = "+
			g.workspaceParam()+" AND da.set_id = ANY("+g.paramArray(append([]string{}, sets...), tText)+") AND da.permission = 'view')")
	}
	return strings.Join(parts, " AND ")
}

// predicate translates a boolean sub-expression.
func (g *sqlGen) predicate(e ast.Expr) (string, bool, error) {
	f, err := g.expr(e)
	if err != nil {
		return "", false, err
	}
	return g.asPred(e, f)
}

// asPred renders a fragment as a boolean SQL expression.
func (g *sqlGen) asPred(e ast.Expr, f frag) (string, bool, error) {
	switch {
	case f.isPred:
		return f.sql, f.twoValued, nil
	case f.isConst:
		if b, ok := f.val.(bool); ok {
			return boolFrag(b).sql, true, nil
		}
	case f.prop != nil && f.prop.Kind == KindCheckbox:
		out, err := g.propCmp(f.prop, "=", constFrag(true))
		if err != nil {
			return "", false, err
		}
		return out.sql, true, nil
	}
	return "", false, g.unsupported(e, "expression is not a boolean predicate")
}

// expr translates any sub-expression.
func (g *sqlGen) expr(e ast.Expr) (frag, error) {
	g.cost += costNode
	switch e.Kind() {
	case ast.LiteralKind:
		return g.literal(e)
	case ast.IdentKind:
		return g.ident(e)
	case ast.SelectKind:
		return g.selectExpr(e)
	case ast.ListKind:
		items := make([]frag, 0, e.AsList().Size())
		for _, el := range e.AsList().Elements() {
			f, err := g.expr(el)
			if err != nil {
				return frag{}, err
			}
			items = append(items, f)
		}
		g.cost -= len(items) * costNode // list elements count as part of the list
		return frag{isList: true, items: items}, nil
	case ast.CallKind:
		return g.call(e)
	case ast.ComprehensionKind:
		return g.comprehension(e)
	}
	return frag{}, g.unsupported(e, "unsupported expression kind")
}

func (g *sqlGen) literal(e ast.Expr) (frag, error) {
	switch v := e.AsLiteral().(type) {
	case types.String:
		return constFrag(string(v)), nil
	case types.Int:
		return constFrag(int64(v)), nil
	case types.Double:
		return constFrag(float64(v)), nil
	case types.Bool:
		return boolFrag(bool(v)), nil
	case types.Timestamp:
		return constFrag(v.Time), nil
	case types.Duration:
		return constFrag(v.Duration), nil
	}
	return frag{}, g.unsupported(e, "unsupported literal type %s", e.AsLiteral().Type().TypeName())
}

// column definitions of the block projection addressable as identifiers.
type colSpec struct {
	sql      string
	t        sqlType
	nullable bool
}

var columns = map[string]colSpec{
	"id":         {"b.id", tUUID, false},
	"page_id":    {"b.page_id", tUUID, false},
	"doc_id":     {"b.doc_id", tUUID, false},
	"project_id": {"b.project_id", tUUID, false},
	"parent_id":  {"b.parent_block_id", tUUID, true},
	"type_id":    {"b.type_id", tUUID, true},
	"key":        {"b.key", tText, true},
	"text":       {"b.text", tText, false},
	"depth":      {"b.depth", tInt, false},
	"created_at": {"b.created_at", tTimestamp, false},
	"updated_at": {"b.updated_at", tTimestamp, false},
	"created_by": {"b.created_by", tText, false},
	"updated_by": {"b.updated_by", tText, false},
}

func (g *sqlGen) ident(e ast.Expr) (frag, error) {
	name := e.AsIdent()
	for i := len(g.scopes) - 1; i >= 0; i-- {
		if f, ok := g.scopes[i][name]; ok {
			return f, nil
		}
	}
	if c, ok := columns[name]; ok {
		return frag{sql: c.sql, t: c.t, nullable: c.nullable}, nil
	}
	switch name {
	case "type":
		return frag{sql: "b.type_id", t: tUUID, nullable: true, isTypeName: true}, nil
	case "is_page":
		if pt := g.env.schema.PageTypeID; pt != "" {
			return predFrag("b.type_id = "+g.param(pt, tUUID), true), nil
		}
		return predFrag("b.id = b.page_id", true), nil
	case "path":
		return frag{list: &listSrc{kind: srcPath}}, nil
	case "now":
		return constFrag(g.now), nil
	case "props":
		return frag{}, g.unsupported(e, "props must be followed by a property name")
	}
	return frag{}, errAt(g.a, e.ID(), nil, name, "unknown symbol %q", name)
}

// selectExpr translates props.<name> and has(props.<name>).
func (g *sqlGen) selectExpr(e ast.Expr) (frag, error) {
	sel := e.AsSelect()
	if sel.Operand().Kind() != ast.IdentKind || sel.Operand().AsIdent() != "props" {
		return frag{}, g.unsupported(e, "field selection is only supported on props")
	}
	g.cost += costNode // the props operand
	p, err := g.env.ix.property(sel.FieldName())
	if err != nil {
		return frag{}, errAt(g.a, e.ID(), nil, "props."+sel.FieldName(), "%s", err.Error())
	}
	g.resolved.Properties["props."+sel.FieldName()] = p.ID
	if sel.IsTestOnly() {
		return g.propHas(p), nil
	}
	if p.Kind.multi() {
		return frag{list: &listSrc{kind: srcPropList, prop: p}}, nil
	}
	f := frag{prop: p, t: p.Kind.sqlType()}
	if p.Kind == KindSelect {
		f.selectOf = p
	}
	return f, nil
}

// call dispatches operators, standard functions and the custom table.
func (g *sqlGen) call(e ast.Expr) (frag, error) {
	c := e.AsCall()
	fn := c.FunctionName()
	switch fn {
	case operators.LogicalAnd:
		return g.logical(e, "AND")
	case operators.LogicalOr:
		return g.logical(e, "OR")
	case operators.LogicalNot:
		s, two, err := g.predicate(c.Args()[0])
		if err != nil {
			return frag{}, err
		}
		if !two {
			s = "COALESCE(" + s + ", false)"
		}
		return predFrag("NOT ("+s+")", true), nil
	case operators.Equals, operators.NotEquals, operators.Less, operators.LessEquals, operators.Greater, operators.GreaterEquals:
		l, r, err := g.binaryArgs(c.Args())
		if err != nil {
			return frag{}, err
		}
		return g.compare(e, sqlOps[fn], l, r)
	case operators.In:
		l, r, err := g.binaryArgs(c.Args())
		if err != nil {
			return frag{}, err
		}
		return g.inOp(e, l, r)
	case operators.Add, operators.Subtract:
		l, r, err := g.binaryArgs(c.Args())
		if err != nil {
			return frag{}, err
		}
		return g.arith(e, fn, l, r)
	case "size":
		return g.sizeOf(e, c)
	case "startsWith", "endsWith", "matches":
		return g.stringFn(e, c)
	case "duration", "timestamp":
		return g.timeLiteral(e, c)
	}
	if spec, ok := g.env.fns[fn]; ok {
		var args []frag
		if c.IsMemberFunction() {
			t, err := g.expr(c.Target())
			if err != nil {
				return frag{}, err
			}
			args = append(args, t)
		}
		for _, a := range c.Args() {
			f, err := g.expr(a)
			if err != nil {
				return frag{}, err
			}
			args = append(args, f)
		}
		return spec.sql(g, e, args)
	}
	return frag{}, g.unsupported(e, "function %s() is not supported", fn)
}

var sqlOps = map[string]string{
	operators.Equals: "=", operators.NotEquals: "<>", operators.Less: "<",
	operators.LessEquals: "<=", operators.Greater: ">", operators.GreaterEquals: ">=",
}

func (g *sqlGen) binaryArgs(args []ast.Expr) (frag, frag, error) {
	if len(args) != 2 {
		return frag{}, frag{}, fmt.Errorf("query: expected two operands, got %d", len(args))
	}
	l, err := g.expr(args[0])
	if err != nil {
		return frag{}, frag{}, err
	}
	r, err := g.expr(args[1])
	if err != nil {
		return frag{}, frag{}, err
	}
	return l, r, nil
}

func (g *sqlGen) logical(e ast.Expr, op string) (frag, error) {
	parts := make([]string, 0, len(e.AsCall().Args()))
	two := true
	for _, a := range e.AsCall().Args() {
		s, tv, err := g.predicate(a)
		if err != nil {
			return frag{}, err
		}
		parts = append(parts, s)
		two = two && tv
	}
	return predFrag("("+strings.Join(parts, " "+op+" ")+")", two), nil
}

// comprehension translates the exists()/all() macro expansions.
func (g *sqlGen) comprehension(e ast.Expr) (frag, error) {
	c := e.AsComprehension()
	init := c.AccuInit()
	step := c.LoopStep()
	if c.HasIterVar2() || init.Kind() != ast.LiteralKind || step.Kind() != ast.CallKind {
		return frag{}, g.unsupported(e, "only exists() and all() are supported")
	}
	isAll, ok := init.AsLiteral().(types.Bool)
	stepCall := step.AsCall()
	if !ok || len(stepCall.Args()) != 2 || stepCall.Args()[0].Kind() != ast.IdentKind || stepCall.Args()[0].AsIdent() != c.AccuVar() {
		return frag{}, g.unsupported(e, "only exists() and all() are supported")
	}
	if (bool(isAll) && stepCall.FunctionName() != operators.LogicalAnd) || (!bool(isAll) && stepCall.FunctionName() != operators.LogicalOr) {
		return frag{}, g.unsupported(e, "only exists() and all() are supported")
	}
	rng, err := g.expr(c.IterRange())
	if err != nil {
		return frag{}, err
	}
	src, err := g.asSource(c.IterRange(), rng)
	if err != nil {
		return frag{}, err
	}
	from, where, el := g.srcParts(src)
	g.scopes = append(g.scopes, map[string]frag{c.IterVar(): el})
	ps, two, err := g.predicate(stepCall.Args()[1])
	g.scopes = g.scopes[:len(g.scopes)-1]
	if err != nil {
		return frag{}, err
	}
	g.cost += costExists - costNode
	if bool(isAll) {
		if !two {
			ps = "COALESCE(" + ps + ", false)"
		}
		return predFrag("NOT EXISTS (SELECT 1 FROM "+from+" WHERE "+where+" AND NOT ("+ps+"))", true), nil
	}
	return predFrag("EXISTS (SELECT 1 FROM "+from+" WHERE "+where+" AND ("+ps+"))", true), nil
}

// asSource turns a fragment into an iterable source.
func (g *sqlGen) asSource(e ast.Expr, f frag) (*listSrc, error) {
	if f.list != nil {
		return f.list, nil
	}
	if f.isList {
		vals := make([]string, 0, len(f.items))
		for _, it := range f.items {
			s, ok := it.val.(string)
			if !it.isConst || !ok {
				return nil, g.unsupported(e, "list iteration requires constant strings")
			}
			vals = append(vals, s)
		}
		return &listSrc{kind: srcConstList, vals: vals}, nil
	}
	return nil, g.unsupported(e, "exists() and all() require a multi-valued property, edges_out(), edges_in() or path")
}

// srcParts renders the FROM clause, the correlation predicate and the
// element fragment of a source under a fresh alias.
func (g *sqlGen) srcParts(src *listSrc) (from, where string, el frag) {
	switch src.kind {
	case srcPropList:
		a := g.alias("bp")
		from = "block_properties " + a
		where = a + ".workspace_id = b.workspace_id AND " + a + ".block_id = b.id AND " + a + ".property_id = " + g.param(src.prop.ID, tUUID)
		el = frag{sql: a + "." + src.prop.Kind.column(), t: src.prop.Kind.sqlType()}
		if src.prop.Kind == KindMultiSelect {
			el.selectOf = src.prop
		}
	case srcEdgesOut:
		a := g.alias("e")
		from = "block_edges " + a
		where = a + ".workspace_id = b.workspace_id AND " + a + ".source_block_id = b.id AND " + a + ".relation_type_id = " + g.param(src.relID, tUUID) + " AND " + a + ".target_block_id IS NOT NULL"
		el = frag{sql: a + ".target_block_id", t: tUUID}
	case srcEdgesIn:
		a := g.alias("e")
		from = "block_edges " + a
		where = a + ".workspace_id = b.workspace_id AND " + a + ".target_block_id = b.id AND " + a + ".relation_type_id = " + g.param(src.relID, tUUID)
		el = frag{sql: a + ".source_block_id", t: tUUID}
	case srcPath:
		a := g.alias("p")
		from = "unnest(string_to_array(b.wbs_path::text, '.')) AS " + a + "(label)"
		where = "TRUE"
		el = frag{sql: a + ".label::uuid", t: tUUID}
	default:
		a := g.alias("c")
		from = "unnest(" + g.paramArray(append([]string{}, src.vals...), tText) + ") AS " + a + "(v)"
		where = "TRUE"
		el = frag{sql: a + ".v", t: tText}
	}
	return from, where, el
}

// srcExists renders EXISTS over a source with an element predicate.
func (g *sqlGen) srcExists(src *listSrc, pred func(el frag) (string, bool, error)) (frag, error) {
	from, where, el := g.srcParts(src)
	ps, _, err := pred(el)
	if err != nil {
		return frag{}, err
	}
	g.cost += costExists - costNode
	return predFrag("EXISTS (SELECT 1 FROM "+from+" WHERE "+where+" AND ("+ps+"))", true), nil
}

// sizeOf translates size(list) / list.size().
func (g *sqlGen) sizeOf(e ast.Expr, c ast.CallExpr) (frag, error) {
	var arg ast.Expr
	if c.IsMemberFunction() {
		arg = c.Target()
	} else if len(c.Args()) == 1 {
		arg = c.Args()[0]
	} else {
		return frag{}, g.unsupported(e, "size() takes one argument")
	}
	f, err := g.expr(arg)
	if err != nil {
		return frag{}, err
	}
	switch {
	case f.isList:
		return constFrag(int64(len(f.items))), nil
	case f.list != nil && f.list.kind == srcPath:
		return frag{sql: "nlevel(b.wbs_path)", t: tInt}, nil
	case f.list != nil:
		from, where, _ := g.srcParts(f.list)
		g.cost += costExists - costNode
		return frag{sql: "(SELECT count(*) FROM " + from + " WHERE " + where + ")", t: tInt}, nil
	}
	return frag{}, g.unsupported(e, "size() is only supported on lists")
}

// literalString extracts a constant string argument.
func (g *sqlGen) literalString(e ast.Expr, f frag, what string) (string, error) {
	s, ok := f.val.(string)
	if !f.isConst || !ok {
		return "", g.unsupported(e, "%s must be a string literal", what)
	}
	return s, nil
}

// refVal is a small helper for table bindings that need a ref.Val list.
func stringList(vals []string) ref.Val {
	return types.NewStringList(types.DefaultTypeAdapter, vals)
}
