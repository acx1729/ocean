package policy

import "strings"

// relation is one "define" line of a generated type: its direct type
// restrictions, the rewrite terms joined with "or", and an optional "but
// not" exclusion.
type relation struct {
	name   string
	direct []string
	terms  []string
	butNot string
}

// typeBlock is one "type" block of the generated model.
type typeBlock struct {
	name      string
	relations []relation
}

// rewrite renders the right-hand side of the relation's definition.
func (r relation) rewrite() string {
	var parts []string
	if len(r.direct) > 0 {
		parts = append(parts, "["+strings.Join(r.direct, ", ")+"]")
	}
	parts = append(parts, r.terms...)
	base := strings.Join(parts, " or ")
	if r.butNot == "" {
		return base
	}
	if len(parts) > 1 {
		base = "(" + base + ")"
	}
	return base + " but not " + r.butNot
}

// renderDSL prints the model in OpenFGA DSL syntax, schema 1.1, followed by
// the condition declarations.
func renderDSL(types []typeBlock, conds []conditionInfo) string {
	var sb strings.Builder
	sb.WriteString("model\n  schema 1.1\n")
	for _, t := range types {
		sb.WriteString("\ntype " + t.name + "\n")
		if len(t.relations) == 0 {
			continue
		}
		sb.WriteString("  relations\n")
		for _, r := range t.relations {
			sb.WriteString("    define " + r.name + ": " + r.rewrite() + "\n")
		}
	}
	for _, c := range conds {
		params := make([]string, len(c.params))
		for i, p := range c.params {
			params[i] = p.name + ": " + p.dslType
		}
		sb.WriteString("\ncondition " + c.name + "(" + strings.Join(params, ", ") + ") {\n  " + c.cel + "\n}\n")
	}
	return sb.String()
}
