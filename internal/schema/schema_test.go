package schema

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/acx1729/ocean/internal/testutil"
)

func TestDefaultsAndValidation(t *testing.T) {
	d := testutil.NewDB(t)
	ctx := context.Background()
	var ws, proj string
	err := d.System(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO workspaces (slug, name, fga_store_id, fga_model_id) VALUES ('schema', 'S', 's', 'm') RETURNING id`).Scan(&ws); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `INSERT INTO projects (workspace_id, slug, name) VALUES ($1, 'main', 'Main') RETURNING id`, ws).Scan(&proj); err != nil {
			return err
		}
		var r Repo
		if err := r.EnsureProjectDefaults(ctx, tx, ws, proj); err != nil {
			return err
		}
		if err := r.EnsureProjectDefaults(ctx, tx, ws, proj); err != nil { // idempotent
			return err
		}
		s, err := r.Load(ctx, tx, ws, proj)
		if err != nil {
			return err
		}
		if len(s.Types) != 7 || len(s.Props) != 7 || len(s.Relations) != 4 {
			t.Fatalf("defaults: types=%d props=%d rels=%d", len(s.Types), len(s.Props), len(s.Relations))
		}
		task := s.TypesByName["task"]
		if !s.IsA(task.ID, "work") || !s.OwnsDoc(task.ID) || !s.Numbered(task.ID) || s.OwnsDoc(s.TypesByName["view"].ID) {
			t.Fatal("closure flags")
		}
		if got := len(s.Descendants(s.TypesByName["work"].ID)); got != 5 {
			t.Fatalf("work descendants: %d", got)
		}
		status := s.PropsByName["status"]
		props := s.ApplyDefaults(task.ID, map[string]any{})
		if props[status.ID] != "open" {
			t.Fatalf("default status: %v", props)
		}
		v := s.Validate(task.ID, map[string]any{status.ID: "bogus", "nope": 1, s.PropsByName["due"].ID: "2026-09-18", s.PropsByName["estimate"].ID: 3.5})
		if v.Invalid[status.ID] == "" || v.Invalid["nope"] == "" || len(v.Invalid) != 2 {
			t.Fatalf("validation: %+v", v)
		}
		if Normalize(status, "Open") != "open" {
			t.Fatal("select normalization by name")
		}
		tags := s.PropsByName["tags"]
		tags.Config["options"] = []any{map[string]any{"id": "t1", "name": "urgent"}}
		if m, ok := Normalize(tags, []any{"urgent"}).(map[string]any); !ok || m["t1"] != true {
			t.Fatalf("multi-select normalization: %v", Normalize(tags, []any{"urgent"}))
		}
		// A custom type restricting allowed props.
		custom := &Type{WorkspaceID: ws, ProjectID: proj, Name: "decision", AllowedProps: []string{status.ID}, RequiredProps: []string{status.ID}}
		if err := r.CreateType(ctx, tx, custom); err != nil {
			return err
		}
		s, _ = r.Load(ctx, tx, ws, proj)
		v = s.Validate(custom.ID, map[string]any{s.PropsByName["due"].ID: "2026-01-01"})
		if len(v.Missing) != 1 || v.Invalid[s.PropsByName["due"].ID] != "not allowed by type" {
			t.Fatalf("custom validation: %+v", v)
		}
		if err := r.DeleteType(ctx, tx, ws, s.TypesByName["work"].ID); err != ErrInUse {
			t.Fatalf("deleting a parent type must fail: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
