package query

import (
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"cel.dev/cel-go/common/ast"
	"cel.dev/cel-go/common/operators"
	"github.com/google/uuid"
)

// flipOp mirrors a comparison operator when its operands are swapped.
func flipOp(op string) string {
	switch op {
	case "<":
		return ">"
	case ">":
		return "<"
	case "<=":
		return ">="
	case ">=":
		return "<="
	}
	return op
}

func isOrdered(op string) bool { return op != "=" && op != "<>" }

// ordText forces byte-order collation on ordered text comparisons so SQL
// agrees with CEL's code-point ordering.
func ordText(sql string, t sqlType, op string) string {
	if isOrdered(op) && t == tText {
		return sql + ` COLLATE "C"`
	}
	return sql
}

// compare translates ==, !=, <, <=, >, >=.
func (g *sqlGen) compare(e ast.Expr, op string, l, r frag) (frag, error) {
	if l.isList || r.isList || l.list != nil || r.list != nil {
		return frag{}, g.unsupported(e, "lists cannot be compared; use in, exists() or all()")
	}
	if l.isConst && !r.isConst {
		l, r, op = r, l, flipOp(op)
	}
	if l.isConst && r.isConst {
		b, err := cmpValues(op, l.val, r.val)
		if err != nil {
			return frag{}, g.unsupported(e, "%v", err)
		}
		return boolFrag(b), nil
	}
	if l.isPred || r.isPred || isCheckbox(l) || isCheckbox(r) {
		return g.compareBool(e, op, l, r)
	}
	if l.isTypeName {
		return g.compareType(e, op, r)
	}
	if r.isTypeName {
		return frag{}, g.unsupported(e, "type can only be compared with a constant type name")
	}
	if l.selectOf != nil || r.selectOf != nil {
		var err error
		if l, r, err = g.resolveSelect(e, op, l, r); err != nil {
			return frag{}, err
		}
	}
	if r.isConst {
		return g.compareConst(e, op, l, r)
	}
	ls, err := g.scalar(l)
	if err != nil {
		return frag{}, g.unsupported(e, "%v", err)
	}
	rs, err := g.scalar(r)
	if err != nil {
		return frag{}, g.unsupported(e, "%v", err)
	}
	ls, rs = coerce(l, ls, r, rs)
	nullable := l.nullable || r.nullable
	if op == "<>" && nullable {
		return predFrag("("+ls+" IS DISTINCT FROM "+rs+")", true), nil
	}
	return predFrag("("+ordText(ls, l.t, op)+" "+op+" "+rs+")", !nullable), nil
}

func isCheckbox(f frag) bool { return f.prop != nil && f.prop.Kind == KindCheckbox }

// coerce makes uuid and text operands comparable.
func coerce(l frag, ls string, r frag, rs string) (string, string) {
	if l.t == tUUID && r.t == tText {
		ls += "::text"
	} else if l.t == tText && r.t == tUUID {
		rs += "::text"
	}
	return ls, rs
}

// compareBool translates equality between boolean expressions.
func (g *sqlGen) compareBool(e ast.Expr, op string, l, r frag) (frag, error) {
	if isOrdered(op) {
		return frag{}, g.unsupported(e, "booleans cannot be ordered")
	}
	lp, ltwo, err := g.asPred(e, l)
	if err != nil {
		return frag{}, err
	}
	if r.isConst {
		b, ok := r.val.(bool)
		if !ok {
			return frag{}, g.unsupported(e, "booleans can only be compared with booleans")
		}
		if b == (op == "=") {
			return predFrag(lp, ltwo), nil
		}
		if !ltwo {
			lp = "COALESCE(" + lp + ", false)"
		}
		return predFrag("NOT ("+lp+")", true), nil
	}
	rp, rtwo, err := g.asPred(e, r)
	if err != nil {
		return frag{}, err
	}
	if !ltwo {
		lp = "COALESCE(" + lp + ", false)"
	}
	if !rtwo {
		rp = "COALESCE(" + rp + ", false)"
	}
	return predFrag("(("+lp+") "+op+" ("+rp+"))", true), nil
}

// compareType translates comparisons of the `type` variable, which is
// spelled as a type name and stored as a type id.
func (g *sqlGen) compareType(e ast.Expr, op string, r frag) (frag, error) {
	name, ok := r.val.(string)
	if !r.isConst || !ok {
		return frag{}, g.unsupported(e, "type can only be compared with a constant type name")
	}
	if isOrdered(op) {
		return frag{}, g.unsupported(e, "type names cannot be ordered")
	}
	if name == "" {
		if op == "=" {
			return predFrag("b.type_id IS NULL", true), nil
		}
		return predFrag("b.type_id IS NOT NULL", true), nil
	}
	t, err := g.env.ix.typeByName(name)
	if err != nil {
		return frag{}, errAt(g.a, e.ID(), nil, name, "%v", err)
	}
	g.resolved.Types[name] = t.ID
	p := g.param(t.ID, tUUID)
	if op == "=" {
		return predFrag("b.type_id = "+p, false), nil
	}
	return predFrag("(b.type_id IS NULL OR b.type_id <> "+p+")", true), nil
}

// resolveSelect rewrites option names of select values into option ids.
func (g *sqlGen) resolveSelect(e ast.Expr, op string, l, r frag) (frag, frag, error) {
	if isOrdered(op) {
		return l, r, g.unsupported(e, "select values cannot be ordered")
	}
	if l.selectOf != nil && r.selectOf != nil {
		if l.selectOf.ID != r.selectOf.ID {
			return l, r, g.unsupported(e, "select values of different properties cannot be compared")
		}
		l.selectOf, r.selectOf = nil, nil
		return l, r, nil
	}
	if l.selectOf == nil {
		l, r = r, l
	}
	name, ok := r.val.(string)
	if !r.isConst || !ok {
		return l, r, g.unsupported(e, "select values can only be compared with option names")
	}
	if name != "" {
		id, found := g.env.ix.optionID(l.selectOf, name)
		if !found {
			return l, r, errAt(g.a, e.ID(), nil, name, "unknown option %q for property %q", name, l.selectOf.Name)
		}
		r.val = id
	}
	r.t = tText
	l.selectOf = nil
	return l, r, nil
}

// compareConst translates <dynamic> op <constant>.
func (g *sqlGen) compareConst(e ast.Expr, op string, l, r frag) (frag, error) {
	if l.t == tUUID {
		s, ok := r.val.(string)
		if !ok {
			return frag{}, g.unsupported(e, "ids can only be compared with strings")
		}
		if isOrdered(op) {
			return frag{}, g.unsupported(e, "ids cannot be ordered")
		}
		if s == "" {
			if !l.nullable {
				return boolFrag(op == "<>"), nil
			}
			if op == "=" {
				return predFrag(l.sql+" IS NULL", true), nil
			}
			return predFrag(l.sql+" IS NOT NULL", true), nil
		}
		u, err := uuid.Parse(s)
		if err != nil {
			return boolFrag(op == "<>"), nil
		}
		r.val, r.t = u.String(), tUUID
	}
	if l.prop != nil {
		return g.propCmp(l.prop, op, r)
	}
	ls, err := g.scalar(l)
	if err != nil {
		return frag{}, g.unsupported(e, "%v", err)
	}
	core := "(" + ordText(ls, l.t, op) + " " + op + " " + g.bind(r, l.t) + ")"
	if l.nullable {
		zeroSat, err := cmpValues(op, "", r.val)
		if err != nil {
			return frag{}, g.unsupported(e, "%v", err)
		}
		if zeroSat {
			return predFrag("("+ls+" IS NULL OR "+core+")", true), nil
		}
		return predFrag(core, false), nil
	}
	return predFrag(core, true), nil
}

// scalar renders a fragment as a scalar SQL expression.
func (g *sqlGen) scalar(f frag) (string, error) {
	switch {
	case f.prop != nil:
		return g.propScalar(f.prop), nil
	case f.isConst:
		return g.bind(f, f.t), nil
	case f.isPred:
		return "(" + f.sql + ")", nil
	case f.sql != "":
		return f.sql, nil
	}
	return "", fmt.Errorf("expression has no scalar value")
}

// bind binds a constant as a parameter typed for the given target.
func (g *sqlGen) bind(c frag, target sqlType) string {
	switch v := c.val.(type) {
	case string:
		if target == tUUID {
			return g.param(v, tUUID)
		}
		return g.param(v, tText)
	case int64:
		return g.param(v, tInt)
	case float64:
		return g.param(v, tNumeric)
	case bool:
		return g.param(v, tBool)
	case time.Time:
		if !g.now.IsZero() && v.Equal(g.now) {
			if g.nowParam == "" {
				g.nowParam = g.param(g.now, tTimestamp)
			}
			return g.nowParam
		}
		return g.param(v, tTimestamp)
	case time.Duration:
		return g.param(v, tInterval)
	}
	return g.param(fmt.Sprint(c.val), tText)
}

// cmpValues evaluates a comparison between two constants in Go; it is used
// for constant folding and for the zero rule of absent properties.
func cmpValues(op string, a, b any) (bool, error) {
	c, err := compareValues(a, b)
	if err != nil {
		return false, err
	}
	switch op {
	case "=":
		return c == 0, nil
	case "<>":
		return c != 0, nil
	case "<":
		return c < 0, nil
	case "<=":
		return c <= 0, nil
	case ">":
		return c > 0, nil
	case ">=":
		return c >= 0, nil
	}
	return false, fmt.Errorf("unknown operator %q", op)
}

func compareValues(a, b any) (int, error) {
	switch av := a.(type) {
	case string:
		if bv, ok := b.(string); ok {
			return strings.Compare(av, bv), nil
		}
	case bool:
		if bv, ok := b.(bool); ok {
			if av == bv {
				return 0, nil
			}
			return 1, nil
		}
	case time.Time:
		if bv, ok := b.(time.Time); ok {
			return av.Compare(bv), nil
		}
	case time.Duration:
		if bv, ok := b.(time.Duration); ok {
			return cmpOrdered(av, bv), nil
		}
	case int64:
		if bv, ok := b.(int64); ok {
			return cmpOrdered(av, bv), nil
		}
		if bv, ok := b.(float64); ok {
			return cmpOrdered(float64(av), bv), nil
		}
	case float64:
		if bv, ok := b.(float64); ok {
			return cmpOrdered(av, bv), nil
		}
		if bv, ok := b.(int64); ok {
			return cmpOrdered(av, float64(bv)), nil
		}
	}
	return 0, fmt.Errorf("cannot compare %T with %T", a, b)
}

func cmpOrdered[T int64 | float64 | time.Duration](a, b T) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// inOp translates `x in list`.
func (g *sqlGen) inOp(e ast.Expr, l, r frag) (frag, error) {
	if l.isList || l.list != nil {
		return frag{}, g.unsupported(e, "left operand of in must be a scalar")
	}
	if r.list != nil {
		if r.list.kind == srcPath && l.isConst {
			s, ok := l.val.(string)
			if !ok {
				return frag{}, g.unsupported(e, "path elements are ids")
			}
			u, err := uuid.Parse(s)
			if err != nil {
				return boolFrag(false), nil
			}
			lq := "*." + strings.ReplaceAll(u.String(), "-", "") + ".*"
			return predFrag("b.wbs_path ~ "+g.param(lq, tLquery), true), nil
		}
		return g.srcExists(r.list, func(el frag) (string, bool, error) {
			f, err := g.compare(e, "=", el, l)
			if err != nil {
				return "", false, err
			}
			return g.asPred(e, f)
		})
	}
	if !r.isList {
		return frag{}, g.unsupported(e, "right operand of in must be a list")
	}
	if len(r.items) > maxInList {
		return frag{}, errAt(g.a, e.ID(), ErrCostExceeded, "", "in list exceeds %d elements", maxInList)
	}
	vals := make([]any, 0, len(r.items))
	for _, it := range r.items {
		if !it.isConst {
			return g.inDynamic(e, l, r.items)
		}
		vals = append(vals, it.val)
	}
	if len(vals) == 0 {
		return boolFrag(false), nil
	}
	if l.isConst {
		for _, v := range vals {
			if eq, err := cmpValues("=", l.val, v); err == nil && eq {
				return boolFrag(true), nil
			}
		}
		return boolFrag(false), nil
	}
	if l.isPred || isCheckbox(l) {
		return frag{}, g.unsupported(e, "booleans cannot be used with in")
	}
	if l.isTypeName {
		return g.typeIn(e, vals)
	}
	if l.selectOf != nil {
		for i, v := range vals {
			name, ok := v.(string)
			if !ok {
				return frag{}, g.unsupported(e, "select values can only be compared with option names")
			}
			if name == "" {
				continue
			}
			id, found := g.env.ix.optionID(l.selectOf, name)
			if !found {
				return frag{}, errAt(g.a, e.ID(), nil, name, "unknown option %q for property %q", name, l.selectOf.Name)
			}
			vals[i] = id
		}
		l.selectOf = nil
	}
	if l.prop != nil {
		return g.propIn(e, l.prop, vals)
	}
	return g.columnIn(e, l, vals)
}

// columnIn renders <column> = ANY($n) with NULL and uuid handling.
func (g *sqlGen) columnIn(e ast.Expr, l frag, vals []any) (frag, error) {
	ls, err := g.scalar(l)
	if err != nil {
		return frag{}, g.unsupported(e, "%v", err)
	}
	if l.t == tUUID {
		ids, hasEmpty, err := uuidList(vals)
		if err != nil {
			return frag{}, g.unsupported(e, "%v", err)
		}
		var parts []string
		if len(ids) > 0 {
			parts = append(parts, ls+" = ANY("+g.paramArray(ids, tUUID)+")")
		}
		if hasEmpty && l.nullable {
			parts = append(parts, ls+" IS NULL")
		}
		if len(parts) == 0 {
			return boolFrag(false), nil
		}
		return predFrag("("+strings.Join(parts, " OR ")+")", !l.nullable || hasEmpty), nil
	}
	arr, err := g.arrayParam(vals, l.t)
	if err != nil {
		return frag{}, g.unsupported(e, "%v", err)
	}
	core := ls + " = ANY(" + arr + ")"
	if l.nullable {
		if containsValue(vals, "") {
			return predFrag("("+ls+" IS NULL OR "+core+")", true), nil
		}
		return predFrag("("+core+")", false), nil
	}
	return predFrag("("+core+")", true), nil
}

// typeIn renders `type in [...]`.
func (g *sqlGen) typeIn(e ast.Expr, vals []any) (frag, error) {
	var ids []string
	hasEmpty := false
	for _, v := range vals {
		name, ok := v.(string)
		if !ok {
			return frag{}, g.unsupported(e, "type names must be strings")
		}
		if name == "" {
			hasEmpty = true
			continue
		}
		t, err := g.env.ix.typeByName(name)
		if err != nil {
			return frag{}, errAt(g.a, e.ID(), nil, name, "%v", err)
		}
		g.resolved.Types[name] = t.ID
		ids = append(ids, t.ID)
	}
	var parts []string
	if len(ids) > 0 {
		parts = append(parts, "b.type_id = ANY("+g.paramArray(ids, tUUID)+")")
	}
	if hasEmpty {
		parts = append(parts, "b.type_id IS NULL")
	}
	return predFrag("("+strings.Join(parts, " OR ")+")", hasEmpty), nil
}

// inDynamic renders `x in [a, b]` when the list holds non-constant items.
func (g *sqlGen) inDynamic(e ast.Expr, l frag, items []frag) (frag, error) {
	if l.prop != nil || l.isTypeName || l.selectOf != nil {
		return frag{}, g.unsupported(e, "in with non-constant list elements is only supported on columns")
	}
	ls, err := g.scalar(l)
	if err != nil {
		return frag{}, g.unsupported(e, "%v", err)
	}
	var parts []string
	two := !l.nullable
	for _, it := range items {
		if it.isList || it.list != nil || it.isPred || it.prop != nil || it.isTypeName || it.selectOf != nil {
			return frag{}, g.unsupported(e, "unsupported list element")
		}
		if it.isConst && l.t == tUUID {
			s, _ := it.val.(string)
			u, err := uuid.Parse(s)
			if err != nil {
				continue
			}
			it.val, it.t = u.String(), tUUID
		}
		s, err := g.scalar(it)
		if err != nil {
			return frag{}, g.unsupported(e, "%v", err)
		}
		_, s = coerce(l, ls, it, s)
		two = two && !it.nullable
		parts = append(parts, s)
	}
	if len(parts) == 0 {
		return boolFrag(false), nil
	}
	return predFrag("("+ls+" IN ("+strings.Join(parts, ", ")+"))", two), nil
}

// uuidList validates uuid constants, dropping invalid ones (they can never
// match) and reporting whether "" was present.
func uuidList(vals []any) ([]string, bool, error) {
	var ids []string
	hasEmpty := false
	for _, v := range vals {
		s, ok := v.(string)
		if !ok {
			return nil, false, fmt.Errorf("ids must be strings")
		}
		if s == "" {
			hasEmpty = true
			continue
		}
		if u, err := uuid.Parse(s); err == nil {
			ids = append(ids, u.String())
		}
	}
	return ids, hasEmpty, nil
}

func containsValue(vals []any, want any) bool {
	for _, v := range vals {
		if eq, err := cmpValues("=", want, v); err == nil && eq {
			return true
		}
	}
	return false
}

// arrayParam binds a typed array parameter for `= ANY`.
func (g *sqlGen) arrayParam(vals []any, t sqlType) (string, error) {
	switch t {
	case tText, tUUID:
		out := make([]string, 0, len(vals))
		for _, v := range vals {
			s, ok := v.(string)
			if !ok {
				return "", fmt.Errorf("list elements must be strings")
			}
			out = append(out, s)
		}
		return g.paramArray(out, t), nil
	case tInt, tNumeric, tReal:
		ints := make([]int64, 0, len(vals))
		floats := make([]float64, 0, len(vals))
		allInt := true
		for _, v := range vals {
			switch n := v.(type) {
			case int64:
				ints = append(ints, n)
				floats = append(floats, float64(n))
			case float64:
				allInt = false
				floats = append(floats, n)
			default:
				return "", fmt.Errorf("list elements must be numbers")
			}
		}
		if allInt && t == tInt {
			return g.paramArray(ints, tInt), nil
		}
		return g.paramArray(floats, tNumeric), nil
	case tBool:
		out := make([]bool, 0, len(vals))
		for _, v := range vals {
			b, ok := v.(bool)
			if !ok {
				return "", fmt.Errorf("list elements must be booleans")
			}
			out = append(out, b)
		}
		return g.paramArray(out, tBool), nil
	case tTimestamp:
		out := make([]time.Time, 0, len(vals))
		for _, v := range vals {
			ts, ok := v.(time.Time)
			if !ok {
				return "", fmt.Errorf("list elements must be timestamps")
			}
			out = append(out, ts)
		}
		return g.paramArray(out, tTimestamp), nil
	}
	return "", fmt.Errorf("unsupported list element type")
}

// arith translates + and - on timestamps and durations.
func (g *sqlGen) arith(e ast.Expr, fn string, l, r frag) (frag, error) {
	if l.isConst && r.isConst {
		v, err := foldArith(fn, l.val, r.val)
		if err != nil {
			return frag{}, g.unsupported(e, "%v", err)
		}
		if ts, ok := v.(time.Time); ok && (ts.Year() < 1 || ts.Year() > 9999) {
			return frag{}, errAt(g.a, e.ID(), nil, "", "timestamp out of range")
		}
		return constFrag(v), nil
	}
	ls, err := g.scalar(l)
	if err != nil {
		return frag{}, g.unsupported(e, "%v", err)
	}
	rs, err := g.scalar(r)
	if err != nil {
		return frag{}, g.unsupported(e, "%v", err)
	}
	op := "+"
	if fn == operators.Subtract {
		op = "-"
	}
	sql := "(" + ls + " " + op + " " + rs + ")"
	switch {
	case l.t == tTimestamp && r.t == tInterval, l.t == tInterval && r.t == tTimestamp && op == "+":
		return frag{sql: sql, t: tTimestamp}, nil
	case l.t == tTimestamp && r.t == tTimestamp && op == "-", l.t == tInterval && r.t == tInterval:
		return frag{sql: sql, t: tInterval}, nil
	}
	return frag{}, g.unsupported(e, "arithmetic is only supported on timestamps and durations")
}

func foldArith(fn string, a, b any) (any, error) {
	sub := fn == operators.Subtract
	switch av := a.(type) {
	case time.Time:
		switch bv := b.(type) {
		case time.Duration:
			if sub {
				return av.Add(-bv), nil
			}
			return av.Add(bv), nil
		case time.Time:
			if sub {
				return av.Sub(bv), nil
			}
		}
	case time.Duration:
		switch bv := b.(type) {
		case time.Duration:
			if sub {
				return av - bv, nil
			}
			return av + bv, nil
		case time.Time:
			if !sub {
				return bv.Add(av), nil
			}
		}
	}
	return nil, fmt.Errorf("arithmetic is only supported on timestamps and durations")
}

// stringFn translates startsWith, endsWith and matches (contains arrives
// through the function table).
func (g *sqlGen) stringFn(e ast.Expr, c ast.CallExpr) (frag, error) {
	if !c.IsMemberFunction() || len(c.Args()) != 1 {
		return frag{}, g.unsupported(e, "%s() must be called as a method with one argument", c.FunctionName())
	}
	target, err := g.expr(c.Target())
	if err != nil {
		return frag{}, err
	}
	arg, err := g.expr(c.Args()[0])
	if err != nil {
		return frag{}, err
	}
	return g.stringPred(e, c.FunctionName(), target, arg)
}

// stringPred renders a string predicate on any string-valued fragment.
func (g *sqlGen) stringPred(e ast.Expr, fn string, target, arg frag) (frag, error) {
	s, err := g.literalString(e, arg, fn+"() argument")
	if err != nil {
		return frag{}, err
	}
	var op, pattern string
	var zeroSat bool
	switch fn {
	case "contains":
		if utf8.RuneCountInString(s) < 3 {
			return frag{}, errAt(g.a, e.ID(), nil, "", "contains() requires at least 3 characters")
		}
		op, pattern = "ILIKE", "%"+escapeLike(s)+"%"
	case "startsWith":
		op, pattern, zeroSat = "LIKE", escapeLike(s)+"%", s == ""
	case "endsWith":
		op, pattern, zeroSat = "LIKE", "%"+escapeLike(s), s == ""
	case "matches":
		if len(s) > 512 {
			return frag{}, errAt(g.a, e.ID(), nil, "", "regular expression exceeds 512 characters")
		}
		re, err := regexp.Compile(s)
		if err != nil {
			return frag{}, errAt(g.a, e.ID(), nil, "", "invalid regular expression: %v", err)
		}
		op, pattern, zeroSat = "~", s, re.MatchString("")
		g.cost += costRegex - costNode
	default:
		return frag{}, g.unsupported(e, "unknown string function %s", fn)
	}
	if target.isConst {
		ts, ok := target.val.(string)
		if !ok {
			return frag{}, g.unsupported(e, "%s() requires a string receiver", fn)
		}
		return boolFrag(evalStringFn(fn, ts, s)), nil
	}
	if target.selectOf != nil || target.isTypeName {
		return frag{}, g.unsupported(e, "string functions are not supported on select values or type names")
	}
	if target.list != nil || target.isList || target.isPred || isCheckbox(target) {
		return frag{}, g.unsupported(e, "%s() requires a string receiver", fn)
	}
	if target.prop != nil {
		return g.propPred(target.prop, func(col string) string {
			return col + " " + op + " " + g.param(pattern, tText)
		}, zeroSat), nil
	}
	ts, err := g.scalar(target)
	if err != nil {
		return frag{}, g.unsupported(e, "%v", err)
	}
	if target.t == tUUID {
		ts += "::text"
	}
	core := "(" + ts + " " + op + " " + g.param(pattern, tText) + ")"
	if target.nullable {
		if zeroSat {
			return predFrag("("+ts+" IS NULL OR "+core+")", true), nil
		}
		return predFrag(core, false), nil
	}
	return predFrag(core, true), nil
}

// evalStringFn evaluates a string function on constants (constant folding).
func evalStringFn(fn, s, arg string) bool {
	switch fn {
	case "contains":
		return strings.Contains(strings.ToLower(s), strings.ToLower(arg))
	case "startsWith":
		return strings.HasPrefix(s, arg)
	case "endsWith":
		return strings.HasSuffix(s, arg)
	case "matches":
		re, err := regexp.Compile(arg)
		return err == nil && re.MatchString(s)
	}
	return false
}

// escapeLike escapes the LIKE metacharacters of a literal.
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	return strings.ReplaceAll(s, `_`, `\_`)
}

// timeLiteral folds duration('...') and timestamp('...').
func (g *sqlGen) timeLiteral(e ast.Expr, c ast.CallExpr) (frag, error) {
	if c.IsMemberFunction() || len(c.Args()) != 1 {
		return frag{}, g.unsupported(e, "%s() takes one string literal", c.FunctionName())
	}
	arg, err := g.expr(c.Args()[0])
	if err != nil {
		return frag{}, err
	}
	s, err := g.literalString(e, arg, c.FunctionName()+"() argument")
	if err != nil {
		return frag{}, err
	}
	if c.FunctionName() == "duration" {
		d, err := time.ParseDuration(s)
		if err != nil {
			return frag{}, errAt(g.a, e.ID(), nil, "", "invalid duration %q", s)
		}
		return constFrag(d), nil
	}
	ts, err := time.Parse(time.RFC3339Nano, s)
	if err != nil || ts.Year() < 1 || ts.Year() > 9999 {
		return frag{}, errAt(g.a, e.ID(), nil, "", "invalid timestamp %q", s)
	}
	return constFrag(ts.UTC()), nil
}
