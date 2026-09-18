package policy

import (
	"fmt"
	"sort"
	"strings"

	"cel.dev/cel-go/cel"
	celast "cel.dev/cel-go/common/ast"
)

// Context roots the API supplies to conditions at check time. The keys
// available below each root are listed in allowedContextFields; "now" is a
// timestamp with no fields.
const (
	contextBlock     = "block"
	contextDoc       = "doc"
	contextPrincipal = "principal"
	contextNow       = "now"
)

// allowedContextFields maps a context root to the fields a condition may
// select from it; a true value allows arbitrary nested selection (typed
// block properties live under block.props.<name>).
var allowedContextFields = map[string]map[string]bool{
	contextBlock:     {"props": true, "type": false},
	contextDoc:       {"kind": false},
	contextPrincipal: {"kind": false, "did": false},
	contextNow:       {},
}

// dslMapAny is the parameter type of a context root: a map of mixed values.
// The model expresses it as TYPE_NAME_MAP over TYPE_NAME_ANY; see parseDSL for
// how the DSL text is parsed.
const dslMapAny = "map<any>"

// contextRootTypes gives the OpenFGA condition parameter type used for a
// context root when a condition references it.
var contextRootTypes = map[string]string{
	contextBlock:     dslMapAny,
	contextDoc:       dslMapAny,
	contextPrincipal: dslMapAny,
	contextNow:       "timestamp",
}

// paramType pairs the CEL type of a supported parameter type with its
// spelling in the OpenFGA DSL.
type paramType struct {
	cel *cel.Type
	dsl string
}

// paramTypes lists the parameter types a scheme may declare. Both the
// scheme spelling "list(string)" and the DSL spelling "list<string>" are
// accepted for string lists.
var paramTypes = map[string]paramType{
	"string":       {cel.StringType, "string"},
	"int":          {cel.IntType, "int"},
	"bool":         {cel.BoolType, "bool"},
	"timestamp":    {cel.TimestampType, "timestamp"},
	"list(string)": {cel.ListType(cel.StringType), "list<string>"},
	"list<string>": {cel.ListType(cel.StringType), "list<string>"},
}

// conditionParam is one parameter of a compiled OpenFGA condition.
type conditionParam struct {
	name    string
	dslType string
}

// conditionInfo is the compiler's view of a validated condition: its
// declared parameters plus one implicit parameter per context root the
// expression references, sorted by name.
type conditionInfo struct {
	name   string
	cel    string
	params []conditionParam
}

// checkCondition validates a condition's name, parameters and CEL
// expression. It appends every problem to errs and returns the compiled
// condition when there were none.
func checkCondition(name string, c Condition, errs *SchemeErrors) (conditionInfo, bool) {
	base := "conditions." + name
	ok := true
	fail := func(path, msg string) {
		*errs = append(*errs, SchemeError{Path: path, Message: msg})
		ok = false
	}
	if !identRE.MatchString(name) {
		fail(base, "condition names must be lower-case snake_case of at most 50 characters")
	}

	envOpts := []cel.EnvOption{cel.ParserRecursionLimit(32), cel.ParserExpressionSizeLimit(4096)}
	params := map[string]bool{}
	info := conditionInfo{name: name, cel: strings.TrimSpace(c.CEL)}
	for _, p := range sortedKeys(c.Params) {
		ppath := base + ".params." + p
		if !identRE.MatchString(p) {
			fail(ppath, "parameter names must be lower-case snake_case of at most 50 characters")
			continue
		}
		if _, reserved := allowedContextFields[p]; reserved {
			fail(ppath, fmt.Sprintf("%q is a reserved context key and cannot be declared as a parameter", p))
			continue
		}
		pt, known := paramTypes[c.Params[p]]
		if !known {
			fail(ppath, fmt.Sprintf("unsupported parameter type %q (supported: %s)", c.Params[p], strings.Join(supportedParamTypes(), ", ")))
			continue
		}
		params[p] = true
		envOpts = append(envOpts, cel.Variable(p, pt.cel))
		info.params = append(info.params, conditionParam{name: p, dslType: pt.dsl})
	}
	if info.cel == "" {
		fail(base+".cel", "expression is required")
	}
	if !ok {
		return conditionInfo{}, false
	}

	for root := range allowedContextFields {
		if root == contextNow {
			envOpts = append(envOpts, cel.Variable(root, cel.TimestampType))
		} else {
			envOpts = append(envOpts, cel.Variable(root, cel.MapType(cel.StringType, cel.DynType)))
		}
	}
	env, err := cel.NewEnv(envOpts...)
	if err != nil {
		fail(base+".cel", "cannot build CEL environment: "+err.Error())
		return conditionInfo{}, false
	}
	ast, iss := env.Compile(info.cel)
	if iss != nil && iss.Err() != nil {
		fail(base+".cel", "invalid CEL expression: "+firstIssue(iss))
		return conditionInfo{}, false
	}
	if !ast.OutputType().IsExactType(cel.BoolType) {
		fail(base+".cel", fmt.Sprintf("expression must evaluate to bool, got %s", ast.OutputType()))
		return conditionInfo{}, false
	}

	roots, problems := inspectReferences(ast, params)
	for _, p := range problems {
		fail(base+".cel", p)
	}
	if !ok {
		return conditionInfo{}, false
	}
	for _, root := range sortedKeys(roots) {
		info.params = append(info.params, conditionParam{name: root, dslType: contextRootTypes[root]})
	}
	sort.Slice(info.params, func(i, j int) bool { return info.params[i].name < info.params[j].name })
	return info, true
}

// inspectReferences walks a checked expression and reports the context roots
// it references together with any reference to an identifier or context key
// that conditions may not use.
func inspectReferences(ast *cel.Ast, params map[string]bool) (map[string]bool, []string) {
	roots := map[string]bool{}
	seen := map[string]bool{}
	var problems []string
	report := func(msg string) {
		if !seen[msg] {
			seen[msg] = true
			problems = append(problems, msg)
		}
	}
	visitor := celast.NewExprVisitor(func(e celast.Expr) {
		switch e.Kind() {
		case celast.IdentKind:
			name := e.AsIdent()
			if params[name] {
				return
			}
			if _, isRoot := allowedContextFields[name]; isRoot {
				roots[name] = true
				return
			}
			report(fmt.Sprintf("references unknown identifier %q", name))
		case celast.SelectKind:
			root, path := selectPath(e)
			if root == "" || params[root] {
				return
			}
			fields, isRoot := allowedContextFields[root]
			if !isRoot {
				return // reported as an unknown identifier by the IdentKind case
			}
			if !pathAllowed(fields, path) {
				report(fmt.Sprintf("references unknown context key %q", root+"."+strings.Join(path, ".")))
			}
		}
	})
	celast.PreOrderVisit(ast.NativeRep().Expr(), visitor)
	return roots, problems
}

// selectPath resolves a chain of field selections down to its root
// identifier; root is "" when the chain does not start at an identifier.
func selectPath(e celast.Expr) (root string, path []string) {
	for e.Kind() == celast.SelectKind {
		sel := e.AsSelect()
		path = append([]string{sel.FieldName()}, path...)
		e = sel.Operand()
	}
	if e.Kind() != celast.IdentKind {
		return "", nil
	}
	return e.AsIdent(), path
}

// pathAllowed reports whether a field path below a context root is one of
// the keys the API supplies.
func pathAllowed(fields map[string]bool, path []string) bool {
	if len(path) == 0 {
		return true
	}
	nested, ok := fields[path[0]]
	if !ok {
		return false
	}
	return nested || len(path) == 1
}

// firstIssue renders the first CEL issue on one line.
func firstIssue(iss *cel.Issues) string {
	if errs := iss.Errors(); len(errs) > 0 {
		return strings.Join(strings.Fields(errs[0].Message), " ")
	}
	return strings.Join(strings.Fields(iss.Err().Error()), " ")
}

// supportedParamTypes lists the scheme spellings of the parameter types.
func supportedParamTypes() []string {
	return []string{"string", "int", "bool", "timestamp", "list(string)"}
}
