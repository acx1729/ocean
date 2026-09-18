package query

import (
	"fmt"
	"strings"
	"time"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/ast"
	"cel.dev/cel-go/common/functions"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
	"github.com/google/uuid"
)

// evalCtx is the per-evaluation context handed to in-process bindings.
type evalCtx struct {
	env    *Env
	block  *BlockInput
	r      Resolver
	caller string
	now    time.Time
}

// fnSpec is one row of the function table. The CEL declaration, the
// in-process binding and the SQL emitter live in the same row so that the
// two evaluation paths cannot drift apart.
type fnSpec struct {
	name     string
	overload string
	member   bool
	args     []*cel.Type
	result   *cel.Type
	eval     func(c *evalCtx, args []ref.Val) ref.Val
	sql      func(g *sqlGen, e ast.Expr, args []frag) (frag, error)
}

// decl declares the function in the environment. Bindings are late: they
// are supplied per evaluation because they close over the block, the
// resolver, the caller and the clock.
func (s *fnSpec) decl() cel.EnvOption {
	if s.member {
		return cel.Function(s.name, cel.MemberOverload(s.overload, s.args, s.result, cel.LateFunctionBinding()))
	}
	return cel.Function(s.name, cel.Overload(s.overload, s.args, s.result, cel.LateFunctionBinding()))
}

// binding returns the run-time overload bound to one evaluation context.
func (s *fnSpec) binding(c *evalCtx) *functions.Overload {
	o := &functions.Overload{Operator: s.overload}
	switch len(s.args) {
	case 1:
		o.Unary = func(a ref.Val) ref.Val { return s.eval(c, []ref.Val{a}) }
	case 2:
		o.Binary = func(a, b ref.Val) ref.Val { return s.eval(c, []ref.Val{a, b}) }
	default:
		o.Function = func(args ...ref.Val) ref.Val { return s.eval(c, args) }
	}
	return o
}

// fnTable is the single source of truth for the custom functions of the
// environment (specification section 8, "Custom functions instead of
// comprehensions") plus the case-insensitive contains that replaces the
// standard one so that in-process evaluation matches ILIKE.
var fnTable = []*fnSpec{
	{
		name: "search", overload: "search_string", args: []*cel.Type{cel.StringType}, result: cel.BoolType,
		eval: func(c *evalCtx, a []ref.Val) ref.Val { return types.Bool(c.r.Search(c.block.Text, str(a[0]))) },
		sql:  sqlSearch,
	},
	{
		name: "is_a", overload: "is_a_string", args: []*cel.Type{cel.StringType}, result: cel.BoolType,
		eval: func(c *evalCtx, a []ref.Val) ref.Val {
			ids, err := c.env.ix.closure(str(a[0]))
			if err != nil {
				return types.False
			}
			for _, id := range ids {
				if id == c.block.TypeID {
					return types.True
				}
			}
			return types.False
		},
		sql: sqlIsA,
	},
	{
		name: "me", overload: "me", result: cel.StringType,
		eval: func(c *evalCtx, _ []ref.Val) ref.Val { return types.String(c.caller) },
		sql:  func(g *sqlGen, _ ast.Expr, _ []frag) (frag, error) { return constFrag(g.caller), nil },
	},
	{
		name: "descendant_of", overload: "descendant_of_string", args: []*cel.Type{cel.StringType}, result: cel.BoolType,
		eval: func(c *evalCtx, a []ref.Val) ref.Val {
			id, ok := canonicalID(str(a[0]))
			return types.Bool(ok && c.r.IsDescendantOf(c.block.ID, id))
		},
		sql: sqlTree("<@"),
	},
	{
		name: "ancestor_of", overload: "ancestor_of_string", args: []*cel.Type{cel.StringType}, result: cel.BoolType,
		eval: func(c *evalCtx, a []ref.Val) ref.Val {
			id, ok := canonicalID(str(a[0]))
			return types.Bool(ok && c.r.IsAncestorOf(c.block.ID, id))
		},
		sql: sqlTree("@>"),
	},
	{
		name: "has_edge", overload: "has_edge_string_string", args: []*cel.Type{cel.StringType, cel.StringType}, result: cel.BoolType,
		eval: func(c *evalCtx, a []ref.Val) ref.Val {
			rel, err := c.env.ix.relation(str(a[0]))
			if err != nil {
				return types.False
			}
			target := str(a[1])
			if target != "" {
				id, ok := canonicalID(target)
				if !ok {
					return types.False
				}
				target = id
			}
			return types.Bool(c.r.HasEdge(c.block.ID, rel.ID, target))
		},
		sql: func(g *sqlGen, e ast.Expr, args []frag) (frag, error) {
			relID, err := g.relationArg(e, args[0])
			if err != nil {
				return frag{}, err
			}
			return g.edgeExists(e, relID, args[1])
		},
	},
	{
		name: "edges_out", overload: "edges_out_string", args: []*cel.Type{cel.StringType}, result: cel.ListType(cel.StringType),
		eval: func(c *evalCtx, a []ref.Val) ref.Val {
			rel, err := c.env.ix.relation(str(a[0]))
			if err != nil {
				return stringList(nil)
			}
			return stringList(c.r.EdgesOut(c.block.ID, rel.ID))
		},
		sql: sqlEdges(srcEdgesOut),
	},
	{
		name: "edges_in", overload: "edges_in_string", args: []*cel.Type{cel.StringType}, result: cel.ListType(cel.StringType),
		eval: func(c *evalCtx, a []ref.Val) ref.Val {
			rel, err := c.env.ix.relation(str(a[0]))
			if err != nil {
				return stringList(nil)
			}
			return stringList(c.r.EdgesIn(c.block.ID, rel.ID))
		},
		sql: sqlEdges(srcEdgesIn),
	},
	{
		name: "refs", overload: "refs_string", args: []*cel.Type{cel.StringType}, result: cel.BoolType,
		eval: func(c *evalCtx, a []ref.Val) ref.Val {
			id, ok := canonicalID(str(a[0]))
			return types.Bool(ok && c.r.Refs(c.block.ID, id))
		},
		sql: sqlRefEdge(true),
	},
	{
		name: "referenced_by", overload: "referenced_by_string", args: []*cel.Type{cel.StringType}, result: cel.BoolType,
		eval: func(c *evalCtx, a []ref.Val) ref.Val {
			id, ok := canonicalID(str(a[0]))
			return types.Bool(ok && c.r.ReferencedBy(c.block.ID, id))
		},
		sql: sqlRefEdge(false),
	},
	{
		name: "in_collection", overload: "in_collection_string", args: []*cel.Type{cel.StringType}, result: cel.BoolType,
		eval: func(c *evalCtx, a []ref.Val) ref.Val {
			id, ok := canonicalID(str(a[0]))
			return types.Bool(ok && c.r.InCollection(c.block.ID, id))
		},
		sql: func(g *sqlGen, e ast.Expr, args []frag) (frag, error) {
			rel, err := g.env.ix.relation("member_of")
			if err != nil {
				return frag{}, errAt(g.a, e.ID(), nil, "member_of", "in_collection() requires the member_of relation type: %v", err)
			}
			g.resolved.RelationTypes["member_of"] = rel.ID
			return g.edgeExists(e, rel.ID, args[0])
		},
	},
	{
		name: "contains", overload: "contains_string", member: true, args: []*cel.Type{cel.StringType, cel.StringType}, result: cel.BoolType,
		eval: func(_ *evalCtx, a []ref.Val) ref.Val {
			return types.Bool(strings.Contains(strings.ToLower(str(a[0])), strings.ToLower(str(a[1]))))
		},
		sql: func(g *sqlGen, e ast.Expr, args []frag) (frag, error) {
			return g.stringPred(e, "contains", args[0], args[1])
		},
	},
}

func str(v ref.Val) string {
	if s, ok := v.(types.String); ok {
		return string(s)
	}
	return fmt.Sprint(v.Value())
}

// canonicalID parses a uuid and returns its canonical form.
func canonicalID(s string) (string, bool) {
	u, err := uuid.Parse(s)
	if err != nil {
		return "", false
	}
	return u.String(), true
}

// relationArg resolves a constant relation type name to its id.
func (g *sqlGen) relationArg(e ast.Expr, f frag) (string, error) {
	name, err := g.literalString(e, f, "relation type")
	if err != nil {
		return "", err
	}
	rel, err := g.env.ix.relation(name)
	if err != nil {
		return "", errAt(g.a, e.ID(), nil, name, "%v", err)
	}
	g.resolved.RelationTypes[name] = rel.ID
	return rel.ID, nil
}

// uuidArg extracts a constant id argument; ok is false for strings that are
// not uuids, which can never match a block.
func (g *sqlGen) uuidArg(e ast.Expr, f frag, what string) (string, bool, error) {
	s, err := g.literalString(e, f, what)
	if err != nil {
		return "", false, err
	}
	id, ok := canonicalID(s)
	return id, ok, nil
}

func sqlSearch(g *sqlGen, e ast.Expr, args []frag) (frag, error) {
	q, err := g.literalString(e, args[0], "search() argument")
	if err != nil {
		return frag{}, err
	}
	if strings.TrimSpace(q) == "" {
		return frag{}, errAt(g.a, e.ID(), nil, "", "search() requires a non-empty query")
	}
	p := g.param(q, tText)
	g.hasSearch = true
	if g.searchParam == "" {
		g.searchParam = p
	}
	return predFrag("b.tsv @@ websearch_to_tsquery('simple', "+p+")", true), nil
}

func sqlIsA(g *sqlGen, e ast.Expr, args []frag) (frag, error) {
	name, err := g.literalString(e, args[0], "is_a() argument")
	if err != nil {
		return frag{}, err
	}
	ids, err := g.env.ix.closure(name)
	if err != nil {
		return frag{}, errAt(g.a, e.ID(), nil, name, "%v", err)
	}
	g.resolved.Types[name] = ids[0]
	return predFrag("b.type_id = ANY("+g.paramArray(ids, tUUID)+")", false), nil
}

// sqlTree emits descendant_of (<@) and ancestor_of (@>). The referenced
// block's path is an init-plan; a missing block yields a sentinel path that
// matches nothing so the predicate stays two-valued.
func sqlTree(op string) func(g *sqlGen, e ast.Expr, args []frag) (frag, error) {
	return func(g *sqlGen, e ast.Expr, args []frag) (frag, error) {
		id, ok, err := g.uuidArg(e, args[0], "block id")
		if err != nil {
			return frag{}, err
		}
		if !ok {
			return boolFrag(false), nil
		}
		g.cost += costExists - costNode
		p := g.param(id, tUUID)
		return predFrag("(b.wbs_path "+op+" COALESCE((SELECT a.wbs_path FROM blocks a WHERE a.workspace_id = "+
			g.workspaceParam()+" AND a.id = "+p+"), '_none_'::ltree) AND b.id <> "+p+")", true), nil
	}
}

// edgeExists emits the relation-edge semi-join shared by has_edge and
// in_collection. An empty target means "any edge of that relation".
func (g *sqlGen) edgeExists(e ast.Expr, relID string, target frag) (frag, error) {
	a := g.alias("e")
	conds := []string{
		a + ".workspace_id = b.workspace_id",
		a + ".source_block_id = b.id",
		a + ".relation_type_id = " + g.param(relID, tUUID),
	}
	switch {
	case target.isConst:
		s, ok := target.val.(string)
		if !ok {
			return frag{}, g.unsupported(e, "edge target must be an id")
		}
		if s != "" {
			id, ok := canonicalID(s)
			if !ok {
				return boolFrag(false), nil
			}
			conds = append(conds, a+".target_block_id = "+g.param(id, tUUID))
		}
	case target.t == tUUID && target.sql != "":
		conds = append(conds, a+".target_block_id = "+target.sql)
	default:
		return frag{}, g.unsupported(e, "edge target must be an id")
	}
	g.cost += costExists - costNode
	return predFrag("EXISTS (SELECT 1 FROM block_edges "+a+" WHERE "+strings.Join(conds, " AND ")+")", true), nil
}

func sqlEdges(kind srcKind) func(g *sqlGen, e ast.Expr, args []frag) (frag, error) {
	return func(g *sqlGen, e ast.Expr, args []frag) (frag, error) {
		relID, err := g.relationArg(e, args[0])
		if err != nil {
			return frag{}, err
		}
		return frag{list: &listSrc{kind: kind, relID: relID}}, nil
	}
}

// sqlRefEdge emits refs (outgoing inline reference) and referenced_by
// (incoming inline reference) over edge_kind = 'ref'.
func sqlRefEdge(outgoing bool) func(g *sqlGen, e ast.Expr, args []frag) (frag, error) {
	return func(g *sqlGen, e ast.Expr, args []frag) (frag, error) {
		id, ok, err := g.uuidArg(e, args[0], "block id")
		if err != nil {
			return frag{}, err
		}
		if !ok {
			return boolFrag(false), nil
		}
		a := g.alias("e")
		mine, other := a+".source_block_id", a+".target_block_id"
		if !outgoing {
			mine, other = other, mine
		}
		g.cost += costExists - costNode
		return predFrag("EXISTS (SELECT 1 FROM block_edges "+a+" WHERE "+a+".workspace_id = b.workspace_id AND "+
			mine+" = b.id AND "+a+".edge_kind = 'ref' AND "+other+" = "+g.param(id, tUUID)+")", true), nil
	}
}
