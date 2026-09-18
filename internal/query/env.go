package query

import (
	"fmt"
	"time"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/checker"
	"cel.dev/cel-go/common/ast"
	celenv "cel.dev/cel-go/common/env"
	"cel.dev/cel-go/common/types"
)

// Env is the compiled CEL environment of one project at one schema version.
// It is safe for concurrent use; callers cache it by (project, version).
type Env struct {
	schema Schema
	ix     *schemaIndex
	cel    *cel.Env
	fns    map[string]*fnSpec
}

// NewEnv builds the environment for a schema.
func NewEnv(s Schema) (*Env, error) {
	ix, err := newSchemaIndex(s)
	if err != nil {
		return nil, err
	}
	provider, err := newPropsProvider(ix)
	if err != nil {
		return nil, fmt.Errorf("query: type provider: %w", err)
	}
	opts := []cel.EnvOption{
		cel.StdLib(cel.StdLibSubset(&celenv.LibrarySubset{
			ExcludeFunctions: []*celenv.Function{celenv.NewFunction("contains")},
		})),
		cel.ClearMacros(),
		cel.Macros(cel.HasMacro, cel.ExistsMacro, cel.AllMacro),
		cel.ParserRecursionLimit(32),
		cel.ParserExpressionSizeLimit(4096),
		cel.CustomTypeProvider(provider),
		cel.Variable("id", cel.StringType),
		cel.Variable("page_id", cel.StringType),
		cel.Variable("doc_id", cel.StringType),
		cel.Variable("project_id", cel.StringType),
		cel.Variable("parent_id", cel.StringType),
		cel.Variable("key", cel.StringType),
		cel.Variable("type", cel.StringType),
		cel.Variable("type_id", cel.StringType),
		cel.Variable("text", cel.StringType),
		cel.Variable("props", cel.ObjectType(propsTypeName)),
		cel.Variable("path", cel.ListType(cel.StringType)),
		cel.Variable("depth", cel.IntType),
		cel.Variable("is_page", cel.BoolType),
		cel.Variable("created_at", cel.TimestampType),
		cel.Variable("updated_at", cel.TimestampType),
		cel.Variable("created_by", cel.StringType),
		cel.Variable("updated_by", cel.StringType),
		cel.Variable("now", cel.TimestampType),
	}
	fns := map[string]*fnSpec{}
	for _, f := range fnTable {
		fns[f.name] = f
		opts = append(opts, f.decl())
	}
	ce, err := cel.NewCustomEnv(opts...)
	if err != nil {
		return nil, fmt.Errorf("query: environment: %w", err)
	}
	return &Env{schema: s, ix: ix, cel: ce, fns: fns}, nil
}

// Schema returns the schema the environment was built from.
func (e *Env) Schema() Schema { return e.schema }

// Validation is the result of Validate.
type Validation struct {
	OK       bool
	Errors   []*CelError
	Resolved Resolved
	Cost     int
}

// Validate parses, type-checks and dry-compiles an expression, returning
// positioned errors and the resolved property, type and relation ids.
func (e *Env) Validate(src string) *Validation {
	checked, errs := e.analyze(src)
	if errs != nil {
		return &Validation{Errors: errs}
	}
	resolved, cost, err := e.dryRun(checked)
	if err != nil {
		return &Validation{Errors: toCelErrors(err), Resolved: resolved, Cost: cost}
	}
	return &Validation{OK: true, Resolved: resolved, Cost: cost}
}

// analyze parses and type-checks an expression, applying the static limits.
func (e *Env) analyze(src string) (*cel.Ast, errorList) {
	parsed, iss := e.cel.Parse(src)
	if errs := issuesToErrors(iss); errs != nil {
		return nil, errs
	}
	if errs := staticChecks(parsed.NativeRep()); errs != nil {
		return nil, errs
	}
	checked, iss := e.cel.Check(parsed)
	if errs := issuesToErrors(iss); errs != nil {
		return nil, errs
	}
	if checked.OutputType() == nil || checked.OutputType().Kind() != types.BoolKind {
		return nil, errorList{errAt(checked.NativeRep(), checked.NativeRep().Expr().ID(), nil, "",
			"expression must evaluate to bool, got %s", checked.OutputType())}
	}
	return checked, nil
}

// unsupportedMacros are the comprehension macros R1 does not register; a
// call with one of these names is refused as soon as the expression parses.
var unsupportedMacros = map[string]bool{"map": true, "filter": true, "exists_one": true, "existsOne": true}

// staticChecks enforces the parse-time limits: unsupported macros, the AST
// node cap and the in-list length cap.
func staticChecks(a *ast.AST) errorList {
	var errs errorList
	nodes := 0
	var visit func(e ast.Expr, inList bool)
	visit = func(e ast.Expr, inList bool) {
		if !(inList && e.Kind() == ast.LiteralKind) {
			nodes++
		}
		switch e.Kind() {
		case ast.CallKind:
			c := e.AsCall()
			if unsupportedMacros[c.FunctionName()] {
				errs = append(errs, errAt(a, e.ID(), ErrUnsupported, c.FunctionName(), "macro %s() is not supported", c.FunctionName()))
			}
			if c.IsMemberFunction() {
				visit(c.Target(), false)
			}
			for _, arg := range c.Args() {
				visit(arg, false)
			}
		case ast.ListKind:
			l := e.AsList()
			if l.Size() > maxInList {
				errs = append(errs, errAt(a, e.ID(), ErrCostExceeded, "", "list exceeds %d elements", maxInList))
			}
			for _, el := range l.Elements() {
				visit(el, true)
			}
		case ast.SelectKind:
			visit(e.AsSelect().Operand(), false)
		case ast.ComprehensionKind:
			c := e.AsComprehension()
			visit(c.IterRange(), false)
			visit(c.AccuInit(), false)
			visit(c.LoopCondition(), false)
			visit(c.LoopStep(), false)
			visit(c.Result(), false)
		case ast.MapKind:
			for _, en := range e.AsMap().Entries() {
				visit(en.AsMapEntry().Key(), false)
				visit(en.AsMapEntry().Value(), false)
			}
		case ast.StructKind:
			for _, f := range e.AsStruct().Fields() {
				visit(f.AsStructField().Value(), false)
			}
		}
	}
	visit(a.Expr(), false)
	if nodes > maxASTNodes {
		errs = append(errs, errAt(a, a.Expr().ID(), ErrCostExceeded, "", "expression has %d nodes, limit is %d", nodes, maxASTNodes))
	}
	return errs
}

// zeroUUID is the placeholder workspace used by dry runs.
const zeroUUID = "00000000-0000-0000-0000-000000000000"

// dryRun generates SQL against a placeholder workspace to apply every
// compile-time rule (unknown names, unsupported constructs, cost).
func (e *Env) dryRun(checked *cel.Ast) (Resolved, int, error) {
	g := newSQLGen(e, checked.NativeRep(), zeroUUID, "", time.Unix(0, 0).UTC())
	if _, _, err := g.predicate(checked.NativeRep().Expr()); err != nil {
		return g.resolved, g.cost, err
	}
	if g.cost > costLimit {
		return g.resolved, g.cost, errAt(g.a, checked.NativeRep().Expr().ID(), ErrCostExceeded, "",
			"estimated cost %d exceeds the limit of %d", g.cost, costLimit)
	}
	return g.resolved, g.cost, nil
}

// toCelErrors normalizes any compile error into positioned errors.
func toCelErrors(err error) []*CelError {
	switch x := err.(type) {
	case nil:
		return nil
	case *CelError:
		return []*CelError{x}
	case errorList:
		return x
	}
	return []*CelError{{Line: 1, Col: 1, Message: err.Error()}}
}

// noSizeEstimator lets the checker estimate program cost with its defaults.
type noSizeEstimator struct{}

func (noSizeEstimator) EstimateSize(checker.AstNode) *checker.SizeEstimate { return nil }
func (noSizeEstimator) EstimateCallCost(string, string, *checker.AstNode, []checker.AstNode) *checker.CallEstimate {
	return nil
}

// programCostLimit derives cel.CostLimit from the checker estimate.
func (e *Env) programCostLimit(checked *cel.Ast) uint64 {
	const cap = 1 << 20
	est, err := e.cel.EstimateCost(checked, noSizeEstimator{})
	if err != nil || est.Max >= cap/4 {
		return cap
	}
	limit := est.Max*4 + 1000
	if limit > cap {
		return cap
	}
	return limit
}
