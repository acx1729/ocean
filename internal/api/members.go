package api

import (
	"context"
	"errors"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	kbv1 "github.com/acx1729/ocean/gen/kb/v1"
	"github.com/acx1729/ocean/gen/kb/v1/kbv1connect"
	"github.com/acx1729/ocean/internal/apierr"
	"github.com/acx1729/ocean/internal/authz"
	"github.com/acx1729/ocean/internal/did"
)

// MembersService implements kb.v1.MembersService.
type MembersService struct{ *core }

var _ kbv1connect.MembersServiceHandler = (*MembersService)(nil)

// RoleInvalidator lets the guard drop cached roles after membership changes.
type RoleInvalidator interface {
	Invalidate(workspaceID, principal string)
}

func (c *core) invalidateRole(ws, principal string) {
	if inv, ok := c.Guard.(RoleInvalidator); ok {
		inv.Invalidate(ws, principal)
	}
}

func validRole(role string) bool {
	_, ok := authz.RoleGrants[role]
	return ok
}

// normalizeInviteDID accepts a DID or a bare EVM address (mainnet did:pkh).
func normalizeInviteDID(s string) (string, error) {
	s = strings.TrimSpace(s)
	if did.IsEVMAddress(s) {
		s = did.FromEVMAddress(1, s)
	}
	s = did.Normalize(s)
	if err := did.Validate(s); err != nil {
		return "", err
	}
	return s, nil
}

func inviteProto(row pgx.Row, publicURL string) (*kbv1.Invite, error) {
	var inv kbv1.Invite
	var expires time.Time
	var accepted, revoked *time.Time
	if err := row.Scan(&inv.Id, &inv.WorkspaceId, &inv.Did, &inv.Role, &inv.CreatedBy, &expires, &accepted, &revoked); err != nil {
		return nil, err
	}
	inv.ExpiresAt, inv.AcceptedAt, inv.RevokedAt = ts(expires), tsPtr(accepted), tsPtr(revoked)
	inv.AcceptUrl = "kb://invite/" + inv.Id
	return &inv, nil
}

const inviteSelect = `SELECT id, workspace_id, did, role, created_by, expires_at, accepted_at, revoked_at FROM invites`

// Invite creates an invitation bound to a DID.
func (s *MembersService) Invite(ctx context.Context, req *connect.Request[kbv1.InviteRequest]) (*connect.Response[kbv1.InviteResponse], error) {
	id, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	ws := req.Msg.GetWorkspaceId()
	if err := s.check(ctx, id, ws, authz.ManageMembers, authz.Workspace(ws)); err != nil {
		return nil, err
	}
	target, err := normalizeInviteDID(req.Msg.GetDid())
	if err != nil {
		return nil, apierr.InvalidArgument("did", err.Error())
	}
	role := req.Msg.GetRole()
	if !validRole(role) {
		return nil, apierr.InvalidArgument("role", "unknown role")
	}
	expires := s.Clock().Add(14 * 24 * time.Hour)
	if req.Msg.GetExpiresAt() != nil {
		expires = req.Msg.GetExpiresAt().AsTime()
	}
	var inv *kbv1.Invite
	err = s.DB.Tx(ctx, tenant(id, ws), func(ctx context.Context, tx pgx.Tx) error {
		var invID string
		if err := tx.QueryRow(ctx, `INSERT INTO invites (workspace_id, did, role, created_by, expires_at) VALUES ($1, $2, $3, $4, $5) RETURNING id`, ws, target, role, id.Principal, expires).Scan(&invID); err != nil {
			return err
		}
		inv, err = inviteProto(tx.QueryRow(ctx, inviteSelect+` WHERE workspace_id = $1 AND id = $2`, ws, invID), s.Config.PublicURL)
		if err != nil {
			return err
		}
		s.audit(ctx, tx, id, ws, "member.invite", "invite", invID, map[string]any{"did": target, "role": role})
		return s.Truth.InsertOutbox(ctx, tx, truthEvent("member.invited", ws, id.Principal, map[string]any{"did": target, "role": role}))
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return connect.NewResponse(&kbv1.InviteResponse{Invite: inv}), nil
}

// ListInvites lists open and past invitations.
func (s *MembersService) ListInvites(ctx context.Context, req *connect.Request[kbv1.ListInvitesRequest]) (*connect.Response[kbv1.ListInvitesResponse], error) {
	id, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	ws := req.Msg.GetWorkspaceId()
	if err := s.check(ctx, id, ws, authz.ManageMembers, authz.Workspace(ws)); err != nil {
		return nil, err
	}
	size := pageSize(req.Msg.GetPageSize())
	cur, err := decodeCursor(req.Msg.GetCursor(), 1)
	if err != nil {
		return nil, err
	}
	out := &kbv1.ListInvitesResponse{}
	err = s.DB.ReadTx(ctx, tenant(id, ws), func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, inviteSelect+` WHERE workspace_id = $1 AND id::text > $2 ORDER BY id LIMIT $3`, ws, cur[0], size+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			inv, err := inviteProto(rows, s.Config.PublicURL)
			if err != nil {
				return err
			}
			if len(out.Invites) == size {
				out.NextCursor = encodeCursor(out.Invites[size-1].Id)
				break
			}
			out.Invites = append(out.Invites, inv)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return connect.NewResponse(out), nil
}

// RevokeInvite cancels an invitation.
func (s *MembersService) RevokeInvite(ctx context.Context, req *connect.Request[kbv1.RevokeInviteRequest]) (*connect.Response[kbv1.RevokeInviteResponse], error) {
	id, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	ws := req.Msg.GetWorkspaceId()
	if err := s.check(ctx, id, ws, authz.ManageMembers, authz.Workspace(ws)); err != nil {
		return nil, err
	}
	err = s.DB.Tx(ctx, tenant(id, ws), func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE invites SET revoked_at = now() WHERE workspace_id = $1 AND id = $2 AND accepted_at IS NULL AND revoked_at IS NULL`, ws, req.Msg.GetInviteId())
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return apierr.NotFound("invite", req.Msg.GetInviteId())
		}
		s.audit(ctx, tx, id, ws, "member.revoke_invite", "invite", req.Msg.GetInviteId(), nil)
		return nil
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return connect.NewResponse(&kbv1.RevokeInviteResponse{}), nil
}

// AcceptInvite joins the workspace; the session must belong to the invited DID.
func (s *MembersService) AcceptInvite(ctx context.Context, req *connect.Request[kbv1.AcceptInviteRequest]) (*connect.Response[kbv1.AcceptInviteResponse], error) {
	id, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	if id.IsAgent() || id.Anonymous {
		return nil, apierr.PermissionDenied("accept_invite", "invite", false)
	}
	ws := req.Msg.GetWorkspaceId()
	var m *kbv1.Membership
	err = s.DB.System(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var role string
		err := tx.QueryRow(ctx, `UPDATE invites SET accepted_at = now() WHERE workspace_id = $1 AND id = $2 AND did = $3 AND accepted_at IS NULL AND revoked_at IS NULL AND expires_at > now() RETURNING role`,
			ws, req.Msg.GetInviteId(), id.Principal).Scan(&role)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return apierr.NotFound("invite", req.Msg.GetInviteId())
			}
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO workspace_members (workspace_id, principal_id, role, invited_by) VALUES ($1, $2, $3, (SELECT created_by FROM invites WHERE workspace_id = $1 AND id = $4))
			ON CONFLICT (workspace_id, principal_id) DO UPDATE SET role = EXCLUDED.role`, ws, id.Principal, role, req.Msg.GetInviteId()); err != nil {
			return err
		}
		var w kbv1.Workspace
		if err := tx.QueryRow(ctx, `SELECT id, slug, name FROM workspaces WHERE id = $1`, ws).Scan(&w.Id, &w.Slug, &w.Name); err != nil {
			return err
		}
		w.Role = role
		m = &kbv1.Membership{Workspace: &w, Role: role}
		s.audit(ctx, tx, id, ws, "member.join", "workspace", ws, map[string]any{"role": role})
		return s.Truth.InsertOutbox(ctx, tx, truthEvent("member.joined", ws, id.Principal, map[string]any{"did": id.Principal, "role": role}))
	})
	if err != nil {
		return nil, mapErr(err)
	}
	s.invalidateRole(ws, id.Principal)
	return connect.NewResponse(&kbv1.AcceptInviteResponse{Membership: m}), nil
}

// List returns the members of a workspace.
func (s *MembersService) List(ctx context.Context, req *connect.Request[kbv1.ListMembersRequest]) (*connect.Response[kbv1.ListMembersResponse], error) {
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
	out := &kbv1.ListMembersResponse{}
	err = s.DB.ReadTx(ctx, tenant(id, ws), func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT m.principal_id, m.role, COALESCE(m.invited_by, ''), m.joined_at, p.kind, COALESCE(p.display_name, ''), p.created_at
			FROM workspace_members m JOIN principals p ON p.id = m.principal_id WHERE m.workspace_id = $1 AND m.principal_id > $2 ORDER BY m.principal_id LIMIT $3`, ws, cur[0], size+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var mem kbv1.Member
			var pr kbv1.Principal
			var kind string
			var joined, created time.Time
			if err := rows.Scan(&pr.Did, &mem.Role, &mem.InvitedBy, &joined, &kind, &pr.DisplayName, &created); err != nil {
				return err
			}
			if len(out.Members) == size {
				out.NextCursor = encodeCursor(out.Members[size-1].Principal.Did)
				break
			}
			pr.Kind = kbv1.PrincipalKind_PRINCIPAL_KIND_USER
			pr.CreatedAt = ts(created)
			mem.Principal, mem.JoinedAt = &pr, ts(joined)
			out.Members = append(out.Members, &mem)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return connect.NewResponse(out), nil
}

// UpdateRole changes a member's role; the last admin cannot be demoted.
func (s *MembersService) UpdateRole(ctx context.Context, req *connect.Request[kbv1.UpdateRoleRequest]) (*connect.Response[kbv1.UpdateRoleResponse], error) {
	id, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	ws := req.Msg.GetWorkspaceId()
	if err := s.check(ctx, id, ws, authz.ManageMembers, authz.Workspace(ws)); err != nil {
		return nil, err
	}
	if !validRole(req.Msg.GetRole()) {
		return nil, apierr.InvalidArgument("role", "unknown role")
	}
	target := did.Normalize(req.Msg.GetDid())
	var mem *kbv1.Member
	err = s.DB.Tx(ctx, tenant(id, ws), func(ctx context.Context, tx pgx.Tx) error {
		if req.Msg.GetRole() != "admin" {
			var admins int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM workspace_members WHERE workspace_id = $1 AND role = 'admin' AND principal_id <> $2`, ws, target).Scan(&admins); err != nil {
				return err
			}
			if admins == 0 {
				return apierr.FailedPrecondition("a workspace must keep at least one admin")
			}
		}
		tag, err := tx.Exec(ctx, `UPDATE workspace_members SET role = $3 WHERE workspace_id = $1 AND principal_id = $2`, ws, target, req.Msg.GetRole())
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return apierr.NotFound("member", target)
		}
		mem = &kbv1.Member{Principal: &kbv1.Principal{Did: target, Kind: kbv1.PrincipalKind_PRINCIPAL_KIND_USER}, Role: req.Msg.GetRole()}
		s.audit(ctx, tx, id, ws, "member.update_role", "member", target, map[string]any{"role": req.Msg.GetRole()})
		return s.Truth.InsertOutbox(ctx, tx, truthEvent("grant.added", ws, id.Principal, map[string]any{"did": target, "role": req.Msg.GetRole(), "resource": "workspace"}))
	})
	if err != nil {
		return nil, mapErr(err)
	}
	s.invalidateRole(ws, target)
	return connect.NewResponse(&kbv1.UpdateRoleResponse{Member: mem}), nil
}

// Remove drops a member; it is a permission change only, keys are untouched.
func (s *MembersService) Remove(ctx context.Context, req *connect.Request[kbv1.RemoveMemberRequest]) (*connect.Response[kbv1.RemoveMemberResponse], error) {
	id, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	ws := req.Msg.GetWorkspaceId()
	if err := s.check(ctx, id, ws, authz.ManageMembers, authz.Workspace(ws)); err != nil {
		return nil, err
	}
	target := did.Normalize(req.Msg.GetDid())
	err = s.DB.Tx(ctx, tenant(id, ws), func(ctx context.Context, tx pgx.Tx) error {
		var admins int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM workspace_members WHERE workspace_id = $1 AND role = 'admin' AND principal_id <> $2`, ws, target).Scan(&admins); err != nil {
			return err
		}
		if admins == 0 {
			return apierr.FailedPrecondition("a workspace must keep at least one admin")
		}
		tag, err := tx.Exec(ctx, `DELETE FROM workspace_members WHERE workspace_id = $1 AND principal_id = $2`, ws, target)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return apierr.NotFound("member", target)
		}
		s.audit(ctx, tx, id, ws, "member.remove", "member", target, nil)
		return s.Truth.InsertOutbox(ctx, tx, truthEvent("member.removed", ws, id.Principal, map[string]any{"did": target}))
	})
	if err != nil {
		return nil, mapErr(err)
	}
	s.invalidateRole(ws, target)
	return connect.NewResponse(&kbv1.RemoveMemberResponse{}), nil
}
