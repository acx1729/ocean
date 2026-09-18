package api

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/structpb"

	kbv1 "github.com/acx1729/ocean/gen/kb/v1"
	"github.com/acx1729/ocean/gen/kb/v1/kbv1connect"
	"github.com/acx1729/ocean/internal/apierr"
	"github.com/acx1729/ocean/internal/auth"
	"github.com/acx1729/ocean/internal/authz"
	"github.com/acx1729/ocean/internal/db"
)

var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}$`)

// WorkspacesService implements kb.v1.WorkspacesService.
type WorkspacesService struct{ *core }

var _ kbv1connect.WorkspacesServiceHandler = (*WorkspacesService)(nil)

type workspaceRow struct {
	ID, Slug, Name string
	KeyVersion     int
	SchemeVersion  int
	Settings       []byte
	CreatedAt      pgxTime
	UpdatedAt      pgxTime
	DeletedAt      *pgxTime
}

func (c *core) workspaceProto(ctx context.Context, tx pgx.Tx, id string) (*kbv1.Workspace, error) {
	var w kbv1.Workspace
	var settings []byte
	var created, updated pgxTime
	var deleted *pgxTime
	err := tx.QueryRow(ctx, `SELECT id, slug, name, current_key_version, scheme_version, settings, created_at, updated_at, deleted_at FROM workspaces WHERE id = $1`, id).
		Scan(&w.Id, &w.Slug, &w.Name, &w.CurrentKeyVersion, &w.SchemeVersion, &settings, &created, &updated, &deleted)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apierr.NotFound("workspace", id)
		}
		return nil, err
	}
	w.Settings = jsonStruct(settings)
	w.CreatedAt, w.UpdatedAt = ts(created.Time), ts(updated.Time)
	if deleted != nil {
		w.DeletedAt = ts(deleted.Time)
	}
	return &w, nil
}

// Create makes a workspace: key v1 wrapped to the node, the caller as admin,
// the authorization store and a default project.
func (s *WorkspacesService) Create(ctx context.Context, req *connect.Request[kbv1.CreateWorkspaceRequest]) (*connect.Response[kbv1.CreateWorkspaceResponse], error) {
	id, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	if id.Kind != auth.KindUser {
		return nil, apierr.PermissionDenied("create_workspace", "workspace", false)
	}
	slug := strings.ToLower(strings.TrimSpace(req.Msg.GetSlug()))
	name := strings.TrimSpace(req.Msg.GetName())
	if !slugRe.MatchString(slug) {
		return nil, apierr.InvalidArgument("slug", "2 to 63 lowercase letters, digits and hyphens, starting with a letter or digit")
	}
	if name == "" || len(name) > 200 {
		return nil, apierr.InvalidArgument("name", "1 to 200 characters")
	}
	var out kbv1.CreateWorkspaceResponse
	err = s.DB.System(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var ws string
		if err := tx.QueryRow(ctx, `INSERT INTO workspaces (slug, name, fga_store_id, fga_model_id) VALUES ($1, $2, '', '') RETURNING id`, slug, name).Scan(&ws); err != nil {
			if db.IsUniqueViolation(err) {
				return apierr.AlreadyExists("workspace slug " + slug)
			}
			return err
		}
		wrapped, err := s.Keys.CreateWorkspaceKey(ws, 1)
		if err != nil {
			return err
		}
		node := s.Keys.Node().DID()
		if _, err := tx.Exec(ctx, `INSERT INTO workspace_keys (workspace_id, key_version, recipient_id, wrapped_key, wrapped_by) VALUES ($1, 1, $2, $3, $2)`, ws, node, wrapped); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO workspace_members (workspace_id, principal_id, role, invited_by) VALUES ($1, $2, 'admin', $2)`, ws, id.Principal); err != nil {
			return err
		}
		storeID, modelID, err := s.Provision.ProvisionWorkspace(ctx, tx, ws, id.Principal)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE workspaces SET fga_store_id = $2, fga_model_id = $3 WHERE id = $1`, ws, storeID, modelID); err != nil {
			return err
		}
		proj, err := s.createProject(ctx, tx, id, ws, "main", "Main", "", nil)
		if err != nil {
			return err
		}
		w, err := s.workspaceProto(ctx, tx, ws)
		if err != nil {
			return err
		}
		w.Role = "admin"
		out.Workspace, out.Project = w, proj
		s.audit(ctx, tx, id, ws, "workspace.create", "workspace", ws, map[string]any{"slug": slug})
		return nil
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return connect.NewResponse(&out), nil
}

// Get returns one workspace the caller belongs to.
func (s *WorkspacesService) Get(ctx context.Context, req *connect.Request[kbv1.GetWorkspaceRequest]) (*connect.Response[kbv1.GetWorkspaceResponse], error) {
	id, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	ws := req.Msg.GetWorkspaceId()
	if err := s.check(ctx, id, ws, authz.View, authz.Workspace(ws)); err != nil {
		return nil, err
	}
	var w *kbv1.Workspace
	err = s.DB.ReadTx(ctx, tenant(id, ws), func(ctx context.Context, tx pgx.Tx) error {
		w, err = s.workspaceProto(ctx, tx, ws)
		return err
	})
	if err != nil {
		return nil, mapErr(err)
	}
	w.Role, _ = s.Guard.Role(ctx, id, ws)
	return connect.NewResponse(&kbv1.GetWorkspaceResponse{Workspace: w}), nil
}

// List returns the caller's workspaces with their role.
func (s *WorkspacesService) List(ctx context.Context, req *connect.Request[kbv1.ListWorkspacesRequest]) (*connect.Response[kbv1.ListWorkspacesResponse], error) {
	id, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	ms, err := s.AuthStore.Memberships(ctx, id.EffectiveOwner())
	if err != nil {
		return nil, mapErr(err)
	}
	out := &kbv1.ListWorkspacesResponse{}
	for _, m := range ms {
		if id.IsAgent() && m.WorkspaceID != id.WorkspaceID {
			continue
		}
		var w *kbv1.Workspace
		err := s.DB.ReadTx(ctx, tenant(id, m.WorkspaceID), func(ctx context.Context, tx pgx.Tx) error {
			w, err = s.workspaceProto(ctx, tx, m.WorkspaceID)
			return err
		})
		if err != nil {
			continue
		}
		w.Role = m.Role
		out.Workspaces = append(out.Workspaces, w)
	}
	return connect.NewResponse(out), nil
}

// Update changes name and settings.
func (s *WorkspacesService) Update(ctx context.Context, req *connect.Request[kbv1.UpdateWorkspaceRequest]) (*connect.Response[kbv1.UpdateWorkspaceResponse], error) {
	id, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	ws := req.Msg.GetWorkspaceId()
	if err := s.check(ctx, id, ws, authz.Admin, authz.Workspace(ws)); err != nil {
		return nil, err
	}
	in := req.Msg.GetWorkspace()
	if in == nil {
		return nil, apierr.InvalidArgument("workspace", "required")
	}
	paths := maskPaths(req.Msg.GetUpdateMask(), "name", "settings")
	var w *kbv1.Workspace
	err = s.DB.Tx(ctx, tenant(id, ws), func(ctx context.Context, tx pgx.Tx) error {
		for _, p := range paths {
			switch p {
			case "name":
				if strings.TrimSpace(in.GetName()) == "" {
					return apierr.InvalidArgument("workspace.name", "required")
				}
				if _, err := tx.Exec(ctx, `UPDATE workspaces SET name = $2, updated_at = now() WHERE id = $1`, ws, strings.TrimSpace(in.GetName())); err != nil {
					return err
				}
			case "settings":
				b, _ := in.GetSettings().MarshalJSON()
				if in.GetSettings() == nil {
					b = []byte("{}")
				}
				if _, err := tx.Exec(ctx, `UPDATE workspaces SET settings = $2, updated_at = now() WHERE id = $1`, ws, b); err != nil {
					return err
				}
			default:
				return apierr.InvalidArgument("update_mask", "unknown path "+p)
			}
		}
		w, err = s.workspaceProto(ctx, tx, ws)
		if err != nil {
			return err
		}
		s.audit(ctx, tx, id, ws, "workspace.update", "workspace", ws, map[string]any{"paths": paths})
		return nil
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return connect.NewResponse(&kbv1.UpdateWorkspaceResponse{Workspace: w}), nil
}

// RotateKey creates the next key version; new writes use it immediately and a
// background job re-encrypts existing rows.
func (s *WorkspacesService) RotateKey(ctx context.Context, req *connect.Request[kbv1.RotateKeyRequest]) (*connect.Response[kbv1.RotateKeyResponse], error) {
	id, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	ws := req.Msg.GetWorkspaceId()
	if err := s.check(ctx, id, ws, authz.Admin, authz.Workspace(ws)); err != nil {
		return nil, err
	}
	var version int
	err = s.DB.Tx(ctx, tenant(id, ws), func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `UPDATE workspaces SET current_key_version = current_key_version + 1, updated_at = now() WHERE id = $1 RETURNING current_key_version`, ws).Scan(&version); err != nil {
			return err
		}
		wrapped, err := s.Keys.CreateWorkspaceKey(ws, version)
		if err != nil {
			return err
		}
		node := s.Keys.Node().DID()
		if _, err := tx.Exec(ctx, `INSERT INTO workspace_keys (workspace_id, key_version, recipient_id, wrapped_key, wrapped_by) VALUES ($1, $2, $3, $4, $5)`, ws, version, node, wrapped, id.Principal); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO outbox (workspace_id, kind, payload, actor_id) VALUES ($1, 'key.rotated', $2, $3)`, ws, []byte(`{"key_version":`+itoa(version)+`}`), id.Principal); err != nil {
			return err
		}
		s.audit(ctx, tx, id, ws, "workspace.rotate_key", "workspace", ws, map[string]any{"key_version": version})
		return nil
	})
	if err != nil {
		s.Keys.Forget(ws)
		return nil, mapErr(err)
	}
	s.Keys.Forget(ws)
	return connect.NewResponse(&kbv1.RotateKeyResponse{KeyVersion: int32(version)}), nil
}

// Export is provided by the import/export milestone.
func (s *WorkspacesService) Export(ctx context.Context, req *connect.Request[kbv1.ExportWorkspaceRequest]) (*connect.Response[kbv1.ExportWorkspaceResponse], error) {
	return nil, apierr.Unimplemented("WorkspacesService.Export")
}

// Delete moves a workspace to the trash for 30 days.
func (s *WorkspacesService) Delete(ctx context.Context, req *connect.Request[kbv1.DeleteWorkspaceRequest]) (*connect.Response[kbv1.DeleteWorkspaceResponse], error) {
	id, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	ws := req.Msg.GetWorkspaceId()
	if err := s.check(ctx, id, ws, authz.Admin, authz.Workspace(ws)); err != nil {
		return nil, err
	}
	now := s.Clock()
	err = s.DB.Tx(ctx, tenant(id, ws), func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE workspaces SET deleted_at = $2, updated_at = now() WHERE id = $1 AND deleted_at IS NULL`, ws, now); err != nil {
			return err
		}
		s.audit(ctx, tx, id, ws, "workspace.delete", "workspace", ws, nil)
		return nil
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return connect.NewResponse(&kbv1.DeleteWorkspaceResponse{PurgeAfter: ts(now.AddDate(0, 0, 30))}), nil
}

func jsonStruct(b []byte) *structpb.Struct {
	s := &structpb.Struct{}
	if len(b) == 0 || s.UnmarshalJSON(b) != nil {
		s, _ = structpb.NewStruct(map[string]any{})
	}
	return s
}
