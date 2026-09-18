package schema

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// Shipped definitions every project starts with. They are rows, not code:
// admins may edit them afterwards, except the reserved page and view types.
var shippedProperties = []struct {
	name    string
	kind    PropertyKind
	options []string
}{
	{"status", KindSelect, []string{"open", "in_progress", "blocked", "done", "closed"}},
	{"priority", KindSelect, []string{"A", "B", "C"}},
	{"assignee", KindUser, nil},
	{"due", KindDate, nil},
	{"tags", KindMultiSelect, nil},
	{"alias", KindText, nil},
	{"estimate", KindNumber, nil},
}

var shippedRelations = []RelationType{
	{Name: "blocked_by", InverseName: "blocks", DAG: true},
	{Name: "related_to", Symmetric: true},
	{Name: "member_of"},
	{Name: "parent_of", InverseName: "child_of", DAG: true},
}

// EnsureProjectDefaults creates the shipped types, properties and relation
// types for a project when they are missing. It is idempotent.
func (r Repo) EnsureProjectDefaults(ctx context.Context, tx pgx.Tx, ws, project string) error {
	existing, err := r.ListTypes(ctx, tx, ws, project)
	if err != nil {
		return err
	}
	byName := map[string]*Type{}
	for _, t := range existing {
		byName[t.Name] = t
	}
	props, err := r.ListProperties(ctx, tx, ws, project)
	if err != nil {
		return err
	}
	propByName := map[string]*Property{}
	for _, p := range props {
		propByName[p.Name] = p
	}
	for _, sp := range shippedProperties {
		if _, ok := propByName[sp.name]; ok {
			continue
		}
		p := &Property{WorkspaceID: ws, ProjectID: project, Name: sp.name, Kind: sp.kind, Config: map[string]any{}}
		if sp.options != nil {
			opts := make([]any, 0, len(sp.options))
			for _, o := range sp.options {
				opts = append(opts, map[string]any{"id": o, "name": o})
			}
			p.Config["options"] = opts
		}
		if err := r.CreateProperty(ctx, tx, p); err != nil {
			return err
		}
		propByName[sp.name] = p
	}
	ensureType := func(t *Type) (*Type, error) {
		if got, ok := byName[t.Name]; ok {
			return got, nil
		}
		t.WorkspaceID, t.ProjectID = ws, project
		if err := r.CreateType(ctx, tx, t); err != nil {
			return nil, err
		}
		byName[t.Name] = t
		return t, nil
	}
	if _, err := ensureType(&Type{Name: TypePage, OwnsDoc: true, CollectionCapable: true, Icon: "file-text"}); err != nil {
		return err
	}
	if _, err := ensureType(&Type{Name: TypeView, Icon: "table"}); err != nil {
		return err
	}
	work, err := ensureType(&Type{Name: TypeWork, OwnsDoc: true, Numbered: true, Icon: "check-square",
		Defaults: map[string]any{propByName["status"].ID: "open"}})
	if err != nil {
		return err
	}
	for _, sub := range []struct{ name, icon string }{{"task", "check"}, {"bug", "bug"}, {"feature", "star"}, {"epic", "layers"}} {
		if _, err := ensureType(&Type{Name: sub.name, ExtendsTypeID: work.ID, Icon: sub.icon}); err != nil {
			return err
		}
	}
	rels, err := r.ListRelationTypes(ctx, tx, ws, project)
	if err != nil {
		return err
	}
	relByName := map[string]bool{}
	for _, rt := range rels {
		relByName[rt.Name] = true
	}
	for _, sr := range shippedRelations {
		if relByName[sr.Name] {
			continue
		}
		rt := sr
		rt.WorkspaceID, rt.ProjectID = ws, project
		if err := r.CreateRelationType(ctx, tx, &rt); err != nil {
			return err
		}
	}
	return nil
}
