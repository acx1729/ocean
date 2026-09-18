package schema

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/acx1729/ocean/internal/db"
)

// ErrNotFound reports a missing definition.
var ErrNotFound = errors.New("schema: not found")

// ErrInUse reports a type that still has blocks or subtypes.
var ErrInUse = errors.New("schema: type in use")

// Repo reads and writes definitions inside the caller's transaction.
type Repo struct{}

const typeSelect = `SELECT id, workspace_id, project_id, name, COALESCE(extends_type_id::text, ''), collection_capable, owns_doc, numbered,
	required_props, allowed_props, defaults, COALESCE(icon, ''), created_at, updated_at FROM block_types`

func scanType(row pgx.Row) (*Type, error) {
	var t Type
	var req []string
	var allowed []string
	var defaults []byte
	if err := row.Scan(&t.ID, &t.WorkspaceID, &t.ProjectID, &t.Name, &t.ExtendsTypeID, &t.CollectionCapable, &t.OwnsDoc, &t.Numbered,
		&req, &allowed, &defaults, &t.Icon, &t.CreatedAt, &t.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	t.RequiredProps = req
	if req == nil {
		t.RequiredProps = []string{}
	}
	t.AllowedProps = allowed
	if err := json.Unmarshal(defaults, &t.Defaults); err != nil {
		return nil, err
	}
	if t.Defaults == nil {
		t.Defaults = map[string]any{}
	}
	return &t, nil
}

// ListTypes returns the types of a project by name.
func (Repo) ListTypes(ctx context.Context, tx pgx.Tx, ws, project string) ([]*Type, error) {
	rows, err := tx.Query(ctx, typeSelect+` WHERE workspace_id = $1 AND project_id = $2 ORDER BY name`, ws, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Type
	for rows.Next() {
		t, err := scanType(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// GetType loads one type.
func (Repo) GetType(ctx context.Context, tx pgx.Tx, ws, id string) (*Type, error) {
	return scanType(tx.QueryRow(ctx, typeSelect+` WHERE workspace_id = $1 AND id = $2`, ws, id))
}

// CreateType inserts a type; t.ID is filled.
func (Repo) CreateType(ctx context.Context, tx pgx.Tx, t *Type) error {
	defaults, err := json.Marshal(orEmpty(t.Defaults))
	if err != nil {
		return err
	}
	var ext *string
	if t.ExtendsTypeID != "" {
		ext = &t.ExtendsTypeID
	}
	return tx.QueryRow(ctx, `INSERT INTO block_types (workspace_id, project_id, name, extends_type_id, collection_capable, owns_doc, numbered, required_props, allowed_props, defaults, icon)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, NULLIF($11, '')) RETURNING id, created_at, updated_at`,
		t.WorkspaceID, t.ProjectID, t.Name, ext, t.CollectionCapable, t.OwnsDoc, t.Numbered, orEmptyList(t.RequiredProps), t.AllowedProps, defaults, t.Icon).
		Scan(&t.ID, &t.CreatedAt, &t.UpdatedAt)
}

// UpdateType rewrites a type's mutable fields.
func (Repo) UpdateType(ctx context.Context, tx pgx.Tx, t *Type) error {
	defaults, err := json.Marshal(orEmpty(t.Defaults))
	if err != nil {
		return err
	}
	var ext *string
	if t.ExtendsTypeID != "" {
		ext = &t.ExtendsTypeID
	}
	tag, err := tx.Exec(ctx, `UPDATE block_types SET name = $3, extends_type_id = $4, collection_capable = $5, owns_doc = $6, numbered = $7,
		required_props = $8, allowed_props = $9, defaults = $10, icon = NULLIF($11, ''), updated_at = now() WHERE workspace_id = $1 AND id = $2`,
		t.WorkspaceID, t.ID, t.Name, ext, t.CollectionCapable, t.OwnsDoc, t.Numbered, orEmptyList(t.RequiredProps), t.AllowedProps, defaults, t.Icon)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteType removes a type that no block or subtype uses.
func (Repo) DeleteType(ctx context.Context, tx pgx.Tx, ws, id string) error {
	var inUse bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM blocks WHERE workspace_id = $1 AND type_id = $2 AND deleted_at IS NULL)
		OR EXISTS (SELECT 1 FROM block_types WHERE workspace_id = $1 AND extends_type_id = $2)`, ws, id).Scan(&inUse); err != nil {
		return err
	}
	if inUse {
		return ErrInUse
	}
	tag, err := tx.Exec(ctx, `DELETE FROM block_types WHERE workspace_id = $1 AND id = $2`, ws, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

const propSelect = `SELECT id, workspace_id, COALESCE(project_id::text, ''), name, kind, config, created_at, updated_at FROM property_definitions`

func scanProp(row pgx.Row) (*Property, error) {
	var p Property
	var kind string
	var cfg []byte
	if err := row.Scan(&p.ID, &p.WorkspaceID, &p.ProjectID, &p.Name, &kind, &cfg, &p.CreatedAt, &p.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	p.Kind = PropertyKind(kind)
	if err := json.Unmarshal(cfg, &p.Config); err != nil {
		return nil, err
	}
	if p.Config == nil {
		p.Config = map[string]any{}
	}
	return &p, nil
}

// ListProperties returns project definitions plus workspace-wide ones
// (project "" lists only workspace-wide).
func (Repo) ListProperties(ctx context.Context, tx pgx.Tx, ws, project string) ([]*Property, error) {
	rows, err := tx.Query(ctx, propSelect+` WHERE workspace_id = $1 AND (project_id IS NULL OR project_id = NULLIF($2, '')::uuid) ORDER BY project_id NULLS FIRST, name`, ws, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Property
	for rows.Next() {
		p, err := scanProp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetProperty loads one definition.
func (Repo) GetProperty(ctx context.Context, tx pgx.Tx, ws, id string) (*Property, error) {
	return scanProp(tx.QueryRow(ctx, propSelect+` WHERE workspace_id = $1 AND id = $2`, ws, id))
}

// CreateProperty inserts a definition; p.ID is filled.
func (Repo) CreateProperty(ctx context.Context, tx pgx.Tx, p *Property) error {
	cfg, err := json.Marshal(orEmpty(p.Config))
	if err != nil {
		return err
	}
	var proj *string
	if p.ProjectID != "" {
		proj = &p.ProjectID
	}
	return tx.QueryRow(ctx, `INSERT INTO property_definitions (workspace_id, project_id, name, kind, config) VALUES ($1, $2, $3, $4, $5) RETURNING id, created_at, updated_at`,
		p.WorkspaceID, proj, p.Name, string(p.Kind), cfg).Scan(&p.ID, &p.CreatedAt, &p.UpdatedAt)
}

// UpdateProperty rewrites name and config (the kind is immutable).
func (Repo) UpdateProperty(ctx context.Context, tx pgx.Tx, p *Property) error {
	cfg, err := json.Marshal(orEmpty(p.Config))
	if err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE property_definitions SET name = $3, config = $4, updated_at = now() WHERE workspace_id = $1 AND id = $2`, p.WorkspaceID, p.ID, p.Name, cfg)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

const relSelect = `SELECT id, workspace_id, project_id, name, COALESCE(inverse_name, ''), is_symmetric, dag, source_types, target_types, created_at FROM relation_types`

func scanRel(row pgx.Row) (*RelationType, error) {
	var r RelationType
	if err := row.Scan(&r.ID, &r.WorkspaceID, &r.ProjectID, &r.Name, &r.InverseName, &r.Symmetric, &r.DAG, &r.SourceTypes, &r.TargetTypes, &r.CreatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &r, nil
}

// ListRelationTypes returns the relation types of a project.
func (Repo) ListRelationTypes(ctx context.Context, tx pgx.Tx, ws, project string) ([]*RelationType, error) {
	rows, err := tx.Query(ctx, relSelect+` WHERE workspace_id = $1 AND project_id = $2 ORDER BY name`, ws, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*RelationType
	for rows.Next() {
		r, err := scanRel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CreateRelationType inserts a relation type; r.ID is filled.
func (Repo) CreateRelationType(ctx context.Context, tx pgx.Tx, r *RelationType) error {
	return tx.QueryRow(ctx, `INSERT INTO relation_types (workspace_id, project_id, name, inverse_name, is_symmetric, dag, source_types, target_types)
		VALUES ($1, $2, $3, NULLIF($4, ''), $5, $6, $7, $8) RETURNING id, created_at`,
		r.WorkspaceID, r.ProjectID, r.Name, r.InverseName, r.Symmetric, r.DAG, r.SourceTypes, r.TargetTypes).Scan(&r.ID, &r.CreatedAt)
}

// Load builds the resolved Snapshot of a project.
func (r Repo) Load(ctx context.Context, tx pgx.Tx, ws, project string) (*Snapshot, error) {
	types, err := r.ListTypes(ctx, tx, ws, project)
	if err != nil {
		return nil, err
	}
	props, err := r.ListProperties(ctx, tx, ws, project)
	if err != nil {
		return nil, err
	}
	rels, err := r.ListRelationTypes(ctx, tx, ws, project)
	if err != nil {
		return nil, err
	}
	s := &Snapshot{ProjectID: project, Types: map[string]*Type{}, TypesByName: map[string]*Type{}, Props: map[string]*Property{}, PropsByName: map[string]*Property{}, Relations: map[string]*RelationType{}, RelationNames: map[string]*RelationType{}}
	var latest time.Time
	for _, t := range types {
		s.Types[t.ID] = t
		s.TypesByName[t.Name] = t
		if t.UpdatedAt.After(latest) {
			latest = t.UpdatedAt
		}
	}
	for _, p := range props {
		s.Props[p.ID] = p
		// Project definitions shadow workspace-wide ones with the same name.
		if existing, ok := s.PropsByName[p.Name]; !ok || existing.ProjectID == "" {
			s.PropsByName[p.Name] = p
		}
		if p.UpdatedAt.After(latest) {
			latest = p.UpdatedAt
		}
	}
	for _, rt := range rels {
		s.Relations[rt.ID] = rt
		s.RelationNames[rt.Name] = rt
		if rt.CreatedAt.After(latest) {
			latest = rt.CreatedAt
		}
	}
	s.Version = latest.UnixMicro()
	return s, nil
}

func orEmpty(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

func orEmptyList(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// IsUnique reports whether err is a name collision.
func IsUnique(err error) bool { return db.IsUniqueViolation(err) }

// String implements fmt.Stringer.
func (t *Type) String() string { return fmt.Sprintf("type %s (%s)", t.Name, t.ID) }
