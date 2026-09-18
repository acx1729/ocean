package api

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	kbv1 "github.com/acx1729/ocean/gen/kb/v1"
	"github.com/acx1729/ocean/gen/kb/v1/kbv1connect"
	"github.com/acx1729/ocean/internal/apierr"
	"github.com/acx1729/ocean/internal/auth"
	"github.com/acx1729/ocean/internal/authz"
	"github.com/acx1729/ocean/internal/db"
)

// pgxTime scans timestamptz columns.
type pgxTime struct{ time.Time }

// ScanTimestamptz implements pgtype scanning through time.Time.
func (t *pgxTime) Scan(src any) error {
	switch v := src.(type) {
	case time.Time:
		t.Time = v
		return nil
	case nil:
		t.Time = time.Time{}
		return nil
	}
	return errors.New("pgxTime: unsupported source")
}

var prefixRe = regexp.MustCompile(`^[A-Z][A-Z0-9]{1,9}$`)

// ProjectsService implements kb.v1.ProjectsService.
type ProjectsService struct{ *core }

var _ kbv1connect.ProjectsServiceHandler = (*ProjectsService)(nil)

func itoa(n int) string { return strconv.Itoa(n) }

// derivePrefix builds a work-key prefix from a slug (KB in KB-123).
func derivePrefix(slug string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(slug) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9' && b.Len() > 0) {
			b.WriteRune(r)
		}
		if b.Len() == 10 {
			break
		}
	}
	p := b.String()
	if !prefixRe.MatchString(p) {
		return "KB"
	}
	return p
}

func (c *core) projectProto(ctx context.Context, tx pgx.Tx, ws, id string) (*kbv1.Project, error) {
	var p kbv1.Project
	var settings []byte
	var root *string
	var created, updated pgxTime
	var archived *pgxTime
	var prefix *string
	err := tx.QueryRow(ctx, `SELECT p.workspace_id, p.id, p.slug, p.name, p.root_page_id::text, p.settings, p.archived_at, p.created_at, p.updated_at, c.prefix
		FROM projects p LEFT JOIN project_counters c ON c.workspace_id = p.workspace_id AND c.project_id = p.id WHERE p.workspace_id = $1 AND p.id = $2`, ws, id).
		Scan(&p.WorkspaceId, &p.Id, &p.Slug, &p.Name, &root, &settings, &archived, &created, &updated, &prefix)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apierr.NotFound("project", id)
		}
		return nil, err
	}
	if root != nil {
		p.RootPageId = *root
	}
	if prefix != nil {
		p.KeyPrefix = *prefix
	}
	p.Settings = jsonStruct(settings)
	p.CreatedAt, p.UpdatedAt = ts(created.Time), ts(updated.Time)
	if archived != nil {
		p.ArchivedAt = ts(archived.Time)
	}
	return &p, nil
}

// createProject inserts a project with its counter, defaults and authorization object.
func (c *core) createProject(ctx context.Context, tx pgx.Tx, id *auth.Identity, ws, slug, name, prefix string, settings []byte) (*kbv1.Project, error) {
	if settings == nil {
		settings = []byte("{}")
	}
	var pid string
	if err := tx.QueryRow(ctx, `INSERT INTO projects (workspace_id, slug, name, settings) VALUES ($1, $2, $3, $4) RETURNING id`, ws, slug, name, settings).Scan(&pid); err != nil {
		if db.IsUniqueViolation(err) {
			return nil, apierr.AlreadyExists("project slug " + slug)
		}
		return nil, err
	}
	if prefix == "" {
		prefix = derivePrefix(slug)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO project_counters (workspace_id, project_id, prefix) VALUES ($1, $2, $3)`, ws, pid, prefix); err != nil {
		return nil, err
	}
	if err := c.Schema.EnsureProjectDefaults(ctx, tx, ws, pid); err != nil {
		return nil, err
	}
	if err := c.Provision.ProvisionProject(ctx, tx, ws, pid); err != nil {
		return nil, err
	}
	c.audit(ctx, tx, id, ws, "project.create", "project", pid, map[string]any{"slug": slug})
	return c.projectProto(ctx, tx, ws, pid)
}

// Create adds a project to a workspace.
func (s *ProjectsService) Create(ctx context.Context, req *connect.Request[kbv1.CreateProjectRequest]) (*connect.Response[kbv1.CreateProjectResponse], error) {
	id, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	ws := req.Msg.GetWorkspaceId()
	if err := s.check(ctx, id, ws, authz.ManageSchema, authz.Workspace(ws)); err != nil {
		return nil, err
	}
	slug := strings.ToLower(strings.TrimSpace(req.Msg.GetSlug()))
	name := strings.TrimSpace(req.Msg.GetName())
	if !regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`).MatchString(slug) {
		return nil, apierr.InvalidArgument("slug", "1 to 63 lowercase letters, digits and hyphens")
	}
	if name == "" || len(name) > 200 {
		return nil, apierr.InvalidArgument("name", "1 to 200 characters")
	}
	prefix := strings.ToUpper(strings.TrimSpace(req.Msg.GetKeyPrefix()))
	if prefix != "" && !prefixRe.MatchString(prefix) {
		return nil, apierr.InvalidArgument("key_prefix", "2 to 10 uppercase letters or digits starting with a letter")
	}
	var settings []byte
	if req.Msg.GetSettings() != nil {
		settings, _ = req.Msg.GetSettings().MarshalJSON()
	}
	var p *kbv1.Project
	err = s.DB.Tx(ctx, tenant(id, ws), func(ctx context.Context, tx pgx.Tx) error {
		p, err = s.createProject(ctx, tx, id, ws, slug, name, prefix, settings)
		return err
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return connect.NewResponse(&kbv1.CreateProjectResponse{Project: p}), nil
}

// Get returns a project.
func (s *ProjectsService) Get(ctx context.Context, req *connect.Request[kbv1.GetProjectRequest]) (*connect.Response[kbv1.GetProjectResponse], error) {
	id, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	ws, pid := req.Msg.GetWorkspaceId(), req.Msg.GetProjectId()
	if err := s.check(ctx, id, ws, authz.View, authz.Project(pid)); err != nil {
		return nil, err
	}
	var p *kbv1.Project
	err = s.DB.ReadTx(ctx, tenant(id, ws), func(ctx context.Context, tx pgx.Tx) error {
		p, err = s.projectProto(ctx, tx, ws, pid)
		return err
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return connect.NewResponse(&kbv1.GetProjectResponse{Project: p}), nil
}

// List returns the projects of a workspace.
func (s *ProjectsService) List(ctx context.Context, req *connect.Request[kbv1.ListProjectsRequest]) (*connect.Response[kbv1.ListProjectsResponse], error) {
	id, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	ws := req.Msg.GetWorkspaceId()
	if err := s.check(ctx, id, ws, authz.View, authz.Workspace(ws)); err != nil {
		return nil, err
	}
	size := pageSize(req.Msg.GetPageSize())
	cur, err := decodeCursor(req.Msg.GetCursor(), 1)
	if err != nil {
		return nil, err
	}
	out := &kbv1.ListProjectsResponse{}
	err = s.DB.ReadTx(ctx, tenant(id, ws), func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id FROM projects WHERE workspace_id = $1 AND slug > $2 AND ($3 OR archived_at IS NULL) ORDER BY slug LIMIT $4`,
			ws, cur[0], req.Msg.GetIncludeArchived(), size+1)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var pid string
			if err := rows.Scan(&pid); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, pid)
		}
		rows.Close()
		for i, pid := range ids {
			if i == size {
				out.NextCursor = encodeCursor(out.Projects[i-1].Slug)
				break
			}
			p, err := s.projectProto(ctx, tx, ws, pid)
			if err != nil {
				return err
			}
			// Agents only see projects their scopes cover.
			if id.IsAgent() && s.Guard.Check(ctx, id, ws, authz.View, authz.Project(pid)) != nil {
				continue
			}
			out.Projects = append(out.Projects, p)
		}
		return nil
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return connect.NewResponse(out), nil
}

// maskPaths returns the requested paths, or the defaults when no mask is given.
func maskPaths(m *fieldmaskpb.FieldMask, defaults ...string) []string {
	if m == nil || len(m.GetPaths()) == 0 {
		return defaults
	}
	return m.GetPaths()
}

// Update changes name, key prefix and settings; a prefix change applies to new keys only.
func (s *ProjectsService) Update(ctx context.Context, req *connect.Request[kbv1.UpdateProjectRequest]) (*connect.Response[kbv1.UpdateProjectResponse], error) {
	id, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	ws, pid := req.Msg.GetWorkspaceId(), req.Msg.GetProjectId()
	if err := s.check(ctx, id, ws, authz.ManageSchema, authz.Project(pid)); err != nil {
		return nil, err
	}
	in := req.Msg.GetProject()
	if in == nil {
		return nil, apierr.InvalidArgument("project", "required")
	}
	paths := maskPaths(req.Msg.GetUpdateMask(), "name", "key_prefix", "settings")
	var p *kbv1.Project
	err = s.DB.Tx(ctx, tenant(id, ws), func(ctx context.Context, tx pgx.Tx) error {
		for _, path := range paths {
			switch path {
			case "name":
				if strings.TrimSpace(in.GetName()) == "" {
					if len(req.Msg.GetUpdateMask().GetPaths()) == 0 {
						continue
					}
					return apierr.InvalidArgument("project.name", "required")
				}
				if _, err := tx.Exec(ctx, `UPDATE projects SET name = $3, updated_at = now() WHERE workspace_id = $1 AND id = $2`, ws, pid, strings.TrimSpace(in.GetName())); err != nil {
					return err
				}
			case "key_prefix":
				prefix := strings.ToUpper(strings.TrimSpace(in.GetKeyPrefix()))
				if prefix == "" {
					continue
				}
				if !prefixRe.MatchString(prefix) {
					return apierr.InvalidArgument("project.key_prefix", "2 to 10 uppercase letters or digits starting with a letter")
				}
				if _, err := tx.Exec(ctx, `UPDATE project_counters SET prefix = $3 WHERE workspace_id = $1 AND project_id = $2`, ws, pid, prefix); err != nil {
					return err
				}
			case "settings":
				if in.GetSettings() == nil {
					continue
				}
				b, _ := in.GetSettings().MarshalJSON()
				if _, err := tx.Exec(ctx, `UPDATE projects SET settings = $3, updated_at = now() WHERE workspace_id = $1 AND id = $2`, ws, pid, b); err != nil {
					return err
				}
			default:
				return apierr.InvalidArgument("update_mask", "unknown path "+path)
			}
		}
		p, err = s.projectProto(ctx, tx, ws, pid)
		if err != nil {
			return err
		}
		s.audit(ctx, tx, id, ws, "project.update", "project", pid, map[string]any{"paths": paths})
		return nil
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return connect.NewResponse(&kbv1.UpdateProjectResponse{Project: p}), nil
}

// Archive hides a project from listings; its pages stay readable.
func (s *ProjectsService) Archive(ctx context.Context, req *connect.Request[kbv1.ArchiveProjectRequest]) (*connect.Response[kbv1.ArchiveProjectResponse], error) {
	id, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	ws, pid := req.Msg.GetWorkspaceId(), req.Msg.GetProjectId()
	if err := s.check(ctx, id, ws, authz.ManageSchema, authz.Project(pid)); err != nil {
		return nil, err
	}
	var p *kbv1.Project
	err = s.DB.Tx(ctx, tenant(id, ws), func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE projects SET archived_at = COALESCE(archived_at, now()), updated_at = now() WHERE workspace_id = $1 AND id = $2`, ws, pid); err != nil {
			return err
		}
		p, err = s.projectProto(ctx, tx, ws, pid)
		if err != nil {
			return err
		}
		s.audit(ctx, tx, id, ws, "project.archive", "project", pid, nil)
		return nil
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return connect.NewResponse(&kbv1.ArchiveProjectResponse{Project: p}), nil
}

// Export is provided by the import/export milestone.
func (s *ProjectsService) Export(ctx context.Context, req *connect.Request[kbv1.ExportProjectRequest]) (*connect.Response[kbv1.ExportProjectResponse], error) {
	return nil, apierr.Unimplemented("ProjectsService.Export")
}
