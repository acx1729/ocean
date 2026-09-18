package query

import (
	"cel.dev/cel-go/common/ast"
)

// propCorrelation renders the join of a block_properties alias to the block.
func (g *sqlGen) propCorrelation(alias string, p *Property) string {
	return alias + ".workspace_id = b.workspace_id AND " + alias + ".block_id = b.id AND " +
		alias + ".property_id = " + g.param(p.ID, tUUID)
}

// propHas renders has(props.x).
func (g *sqlGen) propHas(p *Property) frag {
	a := g.alias("bp")
	g.cost += costExists - costNode
	return predFrag("EXISTS (SELECT 1 FROM block_properties "+a+" WHERE "+g.propCorrelation(a, p)+")", true)
}

// propPred renders a predicate over a single-valued property using the zero
// rule: when the zero value of the kind satisfies the predicate (so absent
// properties match), it becomes NOT EXISTS (... AND NOT pred); otherwise
// EXISTS (... AND pred). Both are exactly COALESCE(value, zero) satisfying
// pred, expressed as semi-joins the planner can drive from block_properties.
func (g *sqlGen) propPred(p *Property, inner func(col string) string, zeroSat bool) frag {
	a := g.alias("bp")
	corr := g.propCorrelation(a, p)
	pred := inner(a + "." + p.Kind.column())
	g.cost += costExists - costNode
	if zeroSat {
		return predFrag("NOT EXISTS (SELECT 1 FROM block_properties "+a+" WHERE "+corr+" AND NOT ("+pred+"))", true)
	}
	return predFrag("EXISTS (SELECT 1 FROM block_properties "+a+" WHERE "+corr+" AND "+pred+")", true)
}

// propCmp renders props.x op <constant>.
func (g *sqlGen) propCmp(p *Property, op string, c frag) (frag, error) {
	zeroSat, err := cmpValues(op, p.Kind.zero(), c.val)
	if err != nil {
		return frag{}, err
	}
	t := p.Kind.sqlType()
	return g.propPred(p, func(col string) string {
		return ordText(col, t, op) + " " + op + " " + g.bind(c, t)
	}, zeroSat), nil
}

// propIn renders props.x in [constants].
func (g *sqlGen) propIn(e ast.Expr, p *Property, vals []any) (frag, error) {
	zeroSat := containsValue(vals, p.Kind.zero())
	if _, err := checkArray(vals, p.Kind.sqlType()); err != nil {
		return frag{}, g.unsupported(e, "%v", err)
	}
	return g.propPred(p, func(col string) string {
		arr, _ := g.arrayParam(vals, p.Kind.sqlType())
		return col + " = ANY(" + arr + ")"
	}, zeroSat), nil
}

// checkArray validates list elements against a target type without binding.
func checkArray(vals []any, t sqlType) (bool, error) {
	probe := &sqlGen{}
	if _, err := probe.arrayParam(vals, t); err != nil {
		return false, err
	}
	return true, nil
}

// propScalar renders a single-valued property as a scalar expression with
// the zero default; used when the other operand is not a constant.
func (g *sqlGen) propScalar(p *Property) string {
	a := g.alias("bp")
	g.cost += costExists - costNode
	return "COALESCE((SELECT " + a + "." + p.Kind.column() + " FROM block_properties " + a + " WHERE " +
		g.propCorrelation(a, p) + " ORDER BY " + a + ".ord LIMIT 1), " + g.bind(constFrag(p.Kind.zero()), p.Kind.sqlType()) + ")"
}
