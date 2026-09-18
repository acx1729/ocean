package policy

import (
	"context"
	"fmt"
	"sort"
	"strings"

	openfgav1 "github.com/openfga/api/proto/openfga/v1"
	"github.com/openfga/language/pkg/go/transformer"
	"github.com/openfga/openfga/pkg/typesystem"
)

// Object types of the generated model that are not assignment levels.
const (
	TypeBlock        = "block"
	TypeAttributeDef = "attribute_def"
)

// Compiled is a scheme compiled to an OpenFGA authorization model.
type Compiled struct {
	// Scheme is the scheme the model was generated from.
	Scheme Scheme
	// DSL is the generated model in OpenFGA DSL syntax (schema 1.1).
	DSL string
	// Model is the parsed model, validated by the OpenFGA typesystem. Its Id
	// is empty until a store assigns one.
	Model *openfgav1.AuthorizationModel
	// RoleRelations maps object type -> role -> relation for every relation
	// that accepts direct assignment: workspace/project/doc roles at their
	// assignable levels and reflected roles on block. Grant tuples are
	// written to these relations.
	RoleRelations map[string]map[string]string
	// PermissionRelations maps object type -> permission -> relation that
	// answers a check for that permission ("can_view" on doc, "writer" on
	// attribute_def for "set_property:*", "can_set_<id>" on block for
	// "set_property:<id>", ...).
	PermissionRelations map[string]map[string]string
	// PublicDocRelations lists the doc relations that accept "user:*", i.e.
	// the read-only roles a publication can grant to the public.
	PublicDocRelations []string

	relationRoles map[string]map[string]string // type -> relation -> role, all role relations
}

// Compile validates a scheme, generates its OpenFGA model, parses the DSL
// with the OpenFGA language transformer and validates the result with the
// OpenFGA typesystem. Validation failures are returned as SchemeErrors.
func Compile(s Scheme) (*Compiled, error) {
	a, errs := analyze(s)
	if len(errs) > 0 {
		return nil, errs
	}
	b := newBuilder(a)
	b.build()
	dsl := renderDSL(b.typeBlocks(), a.conditions)

	model, err := parseDSL(dsl, a.conditions)
	if err != nil {
		return nil, fmt.Errorf("policy: generated model does not parse: %w", err)
	}
	if _, err := typesystem.NewAndValidate(context.Background(), model); err != nil {
		return nil, fmt.Errorf("policy: generated model rejected by OpenFGA: %w", err)
	}
	return &Compiled{
		Scheme:              s.Clone(),
		DSL:                 dsl,
		Model:               model,
		RoleRelations:       b.roleRels,
		PermissionRelations: b.permRels,
		PublicDocRelations:  b.public,
		relationRoles:       b.allRoles,
	}, nil
}

// parseDSL turns the generated DSL into the authorization model. The context
// roots a condition receives (block, doc, principal) are maps of mixed
// values, which the model expresses as map<any>; the DSL grammar has no
// spelling for that type, so the text is parsed with map<string> in its
// place and the parameters are widened on the parsed model.
func parseDSL(dsl string, conds []conditionInfo) (*openfgav1.AuthorizationModel, error) {
	model, err := transformer.TransformDSLToProto(strings.ReplaceAll(dsl, dslMapAny, "map<string>"))
	if err != nil {
		return nil, err
	}
	for _, c := range conds {
		mc, ok := model.GetConditions()[c.name]
		if !ok {
			continue
		}
		for _, p := range c.params {
			if p.dslType != dslMapAny {
				continue
			}
			if param, ok := mc.GetParameters()[p.name]; ok {
				param.TypeName = openfgav1.ConditionParamTypeRef_TYPE_NAME_MAP
				param.GenericTypes = []*openfgav1.ConditionParamTypeRef{{TypeName: openfgav1.ConditionParamTypeRef_TYPE_NAME_ANY}}
			}
		}
	}
	return model, nil
}

// src is one origin of a permission: a role held at a level.
type src struct{ level, role string }

// demand asks for a role to be carried down to a level through pass-through
// relations so that a permission relation can reach it.
type demand struct{ role, level string }

// candidate is one term that may appear in a permission relation together
// with the sources it covers.
type candidate struct {
	term   string
	from   bool
	covers map[src]bool
}

// builder assembles the relations of the generated model.
type builder struct {
	a *analysis
	// impl[x][y]: x's own grants strictly cover y, so x's holders are y's
	// holders; this is what coverage reasoning may rely on.
	impl map[string]map[string]bool
	// fold[x][y]: x appears as a term of y's relation, because impl[x][y] or
	// x inherits y. A role that inherits another is therefore reachable
	// through the inherited role's relation, but never the other way round.
	fold     map[string]map[string]bool
	present  map[string]map[string]bool // level -> role -> has a relation
	closures map[src]map[src]bool
	types    map[string][]relation
	roleRels map[string]map[string]string
	permRels map[string]map[string]string
	allRoles map[string]map[string]string
	public   []string
}

func newBuilder(a *analysis) *builder {
	b := &builder{a: a, impl: map[string]map[string]bool{}, fold: map[string]map[string]bool{}, present: map[string]map[string]bool{}}
	for _, x := range a.roles {
		b.impl[x] = map[string]bool{}
		b.fold[x] = map[string]bool{}
		for _, y := range a.roles {
			people := !a.reflected[x] && !a.reflected[y]
			b.impl[x][y] = people && a.implies(x, y)
			b.fold[x][y] = people && (b.impl[x][y] || a.inherits(x, y))
		}
	}
	for _, l := range Levels {
		b.present[l] = map[string]bool{}
	}
	return b
}

// build plans the model, extending role presence on demand until every
// permission relation can reach all the roles that grant it.
func (b *builder) build() {
	b.initPresence()
	for iter := 0; iter < 2*len(Levels); iter++ {
		b.closures = map[src]map[src]bool{}
		b.types = map[string][]relation{}
		b.roleRels = map[string]map[string]string{}
		b.permRels = map[string]map[string]string{}
		b.allRoles = map[string]map[string]string{}
		b.public = nil
		demands := b.plan()
		if len(demands) == 0 {
			return
		}
		for _, d := range demands {
			b.demand(d.role, d.level)
		}
	}
}

// initPresence gives every role a relation at its assignable levels, fills
// gaps between them, and carries exclusion roles down to doc.
func (b *builder) initPresence() {
	for _, r := range b.a.roles {
		if b.a.reflected[r] {
			continue
		}
		narrowest := ""
		for _, l := range Levels {
			if b.a.assignable[l][r] {
				b.present[l][r] = true
				narrowest = l
			}
		}
		b.demand(r, narrowest)
	}
	for key := range b.a.exclusions {
		if key != ExclusionBlocked {
			b.demand(key, LevelDoc)
		}
	}
}

// demand makes role present at every level between its widest assignable
// level and the given level.
func (b *builder) demand(role, level string) {
	w, ok := b.a.widest[role]
	if !ok || level == "" {
		return
	}
	for _, l := range Levels {
		if levelRank(l) > levelRank(w) && levelRank(l) <= levelRank(level) {
			b.present[l][role] = true
		}
	}
}

// plan computes every relation of the model for the current presence and
// returns the demands left unsatisfied.
func (b *builder) plan() []demand {
	var demands []demand
	for _, l := range Levels {
		demands = append(demands, b.planLevel(l)...)
	}
	demands = append(demands, b.planAttributeDef()...)
	demands = append(demands, b.planBlock()...)
	return demands
}

func (b *builder) planLevel(level string) []demand {
	var rels []relation
	switch level {
	case LevelProject:
		rels = append(rels, relation{name: "workspace", direct: []string{LevelWorkspace}})
	case LevelDoc:
		rels = append(rels, relation{name: "project", direct: []string{LevelProject}}, relation{name: "parent", direct: []string{LevelDoc}})
	}
	b.roleRels[level] = map[string]string{}
	b.allRoles[level] = map[string]string{}
	for _, r := range b.a.roles {
		if !b.present[level][r] {
			continue
		}
		rel := b.roleRelation(level, r)
		rels = append(rels, rel)
		b.allRoles[level][rel.name] = r
		if b.a.assignable[level][r] {
			b.roleRels[level][r] = rel.name
		}
	}
	if _, used := b.a.exclusions[ExclusionBlocked]; used {
		switch level {
		case LevelWorkspace:
			rels = append(rels, relation{name: ExclusionBlocked, direct: []string{"user", "agent"}})
		default:
			rels = append(rels, relation{name: ExclusionBlocked, terms: []string{ExclusionBlocked + " from " + widerLevel(level)}})
		}
	}
	var demands []demand
	b.permRels[level] = map[string]string{}
	for _, p := range basePermissions {
		if !b.a.listed[p] || !containsString(permissionTypes[p], level) {
			continue
		}
		rel, ds, ok := b.levelPermission(level, p)
		demands = append(demands, ds...)
		if ok {
			rels = append(rels, rel)
			b.permRels[level][p] = rel.name
		}
	}
	b.types[level] = rels
	return demands
}

// roleRelation renders a role's relation at a level. Where the role is
// assignable: direct assignment (with every declared condition), the roles
// that fold into it at the same level, inheritance from the parent doc and
// from the next wider level. Where it is not assignable the relation is a
// pass-through that only carries the role down from the wider level.
func (b *builder) roleRelation(level, role string) relation {
	rel := relation{name: RoleRelation(role)}
	w := widerLevel(level)
	if !b.a.assignable[level][role] {
		if w != "" && b.present[w][role] {
			rel.terms = append(rel.terms, rel.name+" from "+w)
		}
		return rel
	}
	base := []string{"user", "agent", "group#member"}
	rel.direct = append(rel.direct, base...)
	if level == LevelDoc && b.readOnly(role) {
		rel.direct = append(rel.direct, "user:*")
		b.public = append(b.public, rel.name)
	}
	for _, c := range b.a.conditions {
		for _, t := range base {
			rel.direct = append(rel.direct, t+" with "+c.name)
		}
	}
	for _, r2 := range b.a.roles {
		if r2 == role || !b.present[level][r2] || !b.fold[r2][role] {
			continue
		}
		redundant := false
		for _, r3 := range b.a.roles {
			if r3 != r2 && r3 != role && b.present[level][r3] && b.fold[r2][r3] && b.fold[r3][role] {
				redundant = true
				break
			}
		}
		if !redundant {
			rel.terms = append(rel.terms, RoleRelation(r2))
		}
	}
	if level == LevelDoc {
		rel.terms = append(rel.terms, rel.name+" from parent")
	}
	if w != "" && b.present[w][role] {
		rel.terms = append(rel.terms, rel.name+" from "+w)
	}
	return rel
}

// readOnly reports whether a role grants nothing beyond "view".
func (b *builder) readOnly(role string) bool {
	for _, atom := range b.a.atoms() {
		if atom != PermissionView && b.a.covers(role, atom) {
			return false
		}
	}
	return b.a.covers(role, PermissionView)
}

// sources lists the (level, role) pairs at or above level whose role grants
// the permission.
func (b *builder) sources(level, permission string) []src {
	var out []src
	for _, l := range Levels {
		if levelRank(l) > levelRank(level) {
			break
		}
		for _, r := range b.a.roles {
			if b.present[l][r] && b.a.covers(r, permission) {
				out = append(out, src{l, r})
			}
		}
	}
	return out
}

// closure lists the sources a role relation at a level reaches: the role
// itself, the roles folded into it at that level (only where the role is
// assignable, since pass-throughs fold nothing) and the same role at wider
// levels, transitively. It mirrors exactly what the rendered relations
// include, so a permission relation names a term only when no chosen term
// already reaches the source.
func (b *builder) closure(s src) map[src]bool {
	if c, ok := b.closures[s]; ok {
		return c
	}
	set := map[src]bool{}
	var visit func(cur src)
	visit = func(cur src) {
		if set[cur] {
			return
		}
		set[cur] = true
		if b.a.assignable[cur.level][cur.role] {
			for _, r2 := range b.a.roles {
				if r2 != cur.role && b.present[cur.level][r2] && b.fold[r2][cur.role] {
					visit(src{cur.level, r2})
				}
			}
		}
		if w := widerLevel(cur.level); w != "" && b.present[w][cur.role] {
			visit(src{w, cur.role})
		}
	}
	visit(s)
	b.closures[s] = set
	return set
}

// levelPermission builds "can_<permission>" on a level type as the smallest
// deterministic union of role terms covering every role that grants it,
// minus the exclusion that applies. ok is false when no role grants it.
func (b *builder) levelPermission(level, permission string) (relation, []demand, bool) {
	srcs := b.sources(level, permission)
	if len(srcs) == 0 {
		return relation{}, nil, false
	}
	var cands []candidate
	for _, r := range b.a.roles {
		if b.present[level][r] && b.a.covers(r, permission) {
			cands = append(cands, candidate{term: RoleRelation(r), covers: b.closure(src{level, r})})
		}
	}
	if w := widerLevel(level); w != "" {
		for _, r := range b.a.roles {
			if b.present[w][r] && !b.present[level][r] && b.a.covers(r, permission) {
				cands = append(cands, candidate{term: RoleRelation(r) + " from " + w, from: true, covers: b.closure(src{w, r})})
			}
		}
	}
	terms, uncovered := cover(srcs, cands)
	var demands []demand
	for _, u := range uncovered {
		demands = append(demands, demand{role: u.role, level: widerLevel(level)})
	}
	rel := relation{name: PermissionRelation(permission), terms: terms, butNot: b.exclusionFor(permission)}
	return rel, demands, true
}

// exclusionFor returns the relation subtracted from a permission relation,
// or "" when no exclusion covers the permission.
func (b *builder) exclusionFor(permission string) string {
	for _, key := range sortedKeys(b.a.exclusions) {
		for _, p := range b.a.exclusions[key] {
			if p == GrantAll || p == permission {
				if key == ExclusionBlocked {
					return ExclusionBlocked
				}
				return RoleRelation(key)
			}
		}
	}
	return ""
}

// planAttributeDef builds attribute_def#writer: direct assignment plus the
// project-level roles that grant "set_property:*".
func (b *builder) planAttributeDef() []demand {
	rel := relation{name: "writer", direct: []string{"user", "agent", "group#member"}}
	var demands []demand
	if b.a.listed[PermissionSetPropertyAll] {
		srcs := b.sources(LevelProject, PermissionSetPropertyAll)
		var cands []candidate
		for _, r := range b.a.roles {
			if b.present[LevelProject][r] && b.a.covers(r, PermissionSetPropertyAll) {
				cands = append(cands, candidate{term: RoleRelation(r) + " from project", from: true, covers: b.closure(src{LevelProject, r})})
			}
		}
		terms, uncovered := cover(srcs, cands)
		rel.terms = terms
		for _, u := range uncovered {
			demands = append(demands, demand{role: u.role, level: LevelProject})
		}
	}
	b.types[TypeAttributeDef] = []relation{{name: "project", direct: []string{LevelProject}}, rel}
	b.permRels[TypeAttributeDef] = map[string]string{PermissionSetPropertyAll: rel.name}
	return demands
}

// planBlock builds the block type: its doc, one relation per reflected role,
// can_edit delegated to the doc, and can_set_<id> for every property named
// by a set_property:<id> grant.
func (b *builder) planBlock() []demand {
	rels := []relation{{name: "doc", direct: []string{LevelDoc}}}
	b.roleRels[TypeBlock] = map[string]string{}
	b.allRoles[TypeBlock] = map[string]string{}
	b.permRels[TypeBlock] = map[string]string{}
	for _, r := range b.a.roles {
		if b.a.reflected[r] {
			rel := relation{name: RoleRelation(r), direct: []string{"user"}}
			rels = append(rels, rel)
			b.roleRels[TypeBlock][r] = rel.name
			b.allRoles[TypeBlock][rel.name] = r
		}
	}
	editRel, hasEdit := b.permRels[LevelDoc][PermissionEdit]
	if hasEdit {
		rels = append(rels, relation{name: editRel, terms: []string{editRel + " from doc"}})
		b.permRels[TypeBlock][PermissionEdit] = editRel
	}
	var demands []demand
	for _, prop := range b.a.properties {
		permission := PermissionSetPropertyPrefix + prop
		rel := relation{name: PropertyRelation(prop)}
		var srcs []src
		if hasEdit {
			rel.terms = append(rel.terms, editRel+" from doc")
		}
		for _, s := range b.sources(LevelDoc, permission) {
			if !hasEdit || !b.a.covers(s.role, PermissionEdit) {
				srcs = append(srcs, s)
			}
		}
		for _, r := range b.a.roles {
			if b.a.reflected[r] && b.a.covers(r, permission) {
				rel.terms = append(rel.terms, RoleRelation(r))
			}
		}
		var cands []candidate
		for _, r := range b.a.roles {
			if b.present[LevelDoc][r] && b.a.covers(r, permission) {
				cands = append(cands, candidate{term: RoleRelation(r) + " from doc", from: true, covers: b.closure(src{LevelDoc, r})})
			}
		}
		terms, uncovered := cover(srcs, cands)
		rel.terms = append(rel.terms, terms...)
		for _, u := range uncovered {
			demands = append(demands, demand{role: u.role, level: LevelDoc})
		}
		if len(rel.terms) == 0 {
			continue
		}
		rels = append(rels, rel)
		b.permRels[TypeBlock][permission] = rel.name
	}
	b.types[TypeBlock] = rels
	return demands
}

// cover greedily picks candidates until every source is covered, preferring
// own-type terms, then terms covering more sources, then names; it returns
// the chosen terms in a stable output order and the sources left uncovered.
func cover(sources []src, cands []candidate) ([]string, []src) {
	need := map[src]bool{}
	for _, s := range sources {
		need[s] = true
	}
	score := func(c candidate) int {
		n := 0
		for s := range c.covers {
			if need[s] {
				n++
			}
		}
		return n
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].from != cands[j].from {
			return !cands[i].from
		}
		if si, sj := score(cands[i]), score(cands[j]); si != sj {
			return si > sj
		}
		return cands[i].term < cands[j].term
	})
	covered := map[src]bool{}
	var chosen []candidate
	for _, c := range cands {
		useful := false
		for s := range c.covers {
			if need[s] && !covered[s] {
				useful = true
				break
			}
		}
		if !useful {
			continue
		}
		chosen = append(chosen, c)
		for s := range c.covers {
			covered[s] = true
		}
	}
	sort.Slice(chosen, func(i, j int) bool {
		if chosen[i].from != chosen[j].from {
			return !chosen[i].from
		}
		return chosen[i].term < chosen[j].term
	})
	terms := make([]string, len(chosen))
	for i, c := range chosen {
		terms[i] = c.term
	}
	var uncovered []src
	for _, s := range sources {
		if !covered[s] {
			uncovered = append(uncovered, s)
		}
	}
	return terms, uncovered
}

// typeBlocks returns the generated types in output order.
func (b *builder) typeBlocks() []typeBlock {
	return []typeBlock{
		{name: "user"},
		{name: "agent", relations: []relation{{name: "owner", direct: []string{"user"}}}},
		{name: "group", relations: []relation{{name: "member", direct: []string{"user", "group#member"}}}},
		{name: LevelWorkspace, relations: b.types[LevelWorkspace]},
		{name: LevelProject, relations: b.types[LevelProject]},
		{name: LevelDoc, relations: b.types[LevelDoc]},
		{name: TypeBlock, relations: b.types[TypeBlock]},
		{name: TypeAttributeDef, relations: b.types[TypeAttributeDef]},
	}
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
