package authz

import (
	"context"
	"sync"
	"time"

	"github.com/acx1729/ocean/internal/apierr"
	"github.com/acx1729/ocean/internal/auth"
)

// RoleGuard authorizes from workspace membership roles alone: every member
// sees every doc, and an agent is bounded by the intersection of its scopes
// and its owner's role. It is the interim guard until the OpenFGA guard.
type RoleGuard struct {
	store *auth.Store

	mu    sync.Mutex
	cache map[string]roleEntry
}

type roleEntry struct {
	role string
	at   time.Time
}

// NewRoleGuard wraps the auth store.
func NewRoleGuard(store *auth.Store) *RoleGuard {
	return &RoleGuard{store: store, cache: map[string]roleEntry{}}
}

// Invalidate forgets a cached role (after membership changes).
func (g *RoleGuard) Invalidate(workspaceID, principal string) {
	g.mu.Lock()
	delete(g.cache, workspaceID+"\x00"+principal)
	g.mu.Unlock()
}

// Role implements Guard.
func (g *RoleGuard) Role(ctx context.Context, id *auth.Identity, workspaceID string) (string, error) {
	if id == nil {
		return "", nil
	}
	owner := id.EffectiveOwner()
	k := workspaceID + "\x00" + owner
	g.mu.Lock()
	e, ok := g.cache[k]
	g.mu.Unlock()
	if ok && time.Since(e.at) < 10*time.Second {
		return e.role, nil
	}
	role, member, err := g.store.IsMember(ctx, workspaceID, owner)
	if err != nil {
		return "", err
	}
	if !member {
		role = ""
	}
	g.mu.Lock()
	if len(g.cache) > 50000 {
		g.cache = map[string]roleEntry{}
	}
	g.cache[k] = roleEntry{role: role, at: time.Now()}
	g.mu.Unlock()
	return role, nil
}

// Check implements Guard.
func (g *RoleGuard) Check(ctx context.Context, id *auth.Identity, workspaceID string, perm Permission, res Resource) error {
	if id == nil {
		return apierr.Unauthenticated("")
	}
	if id.Operator {
		return apierr.PermissionDenied(string(perm), res.Type+":"+res.ID, false)
	}
	if id.Anonymous {
		if perm == View && id.WorkspaceID == workspaceID {
			return nil
		}
		return apierr.PermissionDenied(string(perm), res.Type+":"+res.ID, false)
	}
	role, err := g.Role(ctx, id, workspaceID)
	if err != nil {
		return apierr.Internal(err)
	}
	if role == "" {
		return apierr.NotFound("workspace", workspaceID)
	}
	if !RoleAllows(role, perm) {
		return apierr.PermissionDenied(string(perm), res.Type+":"+res.ID, id.IsAgent())
	}
	if id.IsAgent() {
		if id.WorkspaceID != workspaceID || !scopesAllow(id.Scopes, workspaceID, perm, res) {
			return apierr.PermissionDenied(string(perm), res.Type+":"+res.ID, false)
		}
	}
	return nil
}

// scopesAllow reports whether one of the agent's scopes covers the resource with a role granting perm.
func scopesAllow(scopes []auth.Scope, workspaceID string, perm Permission, res Resource) bool {
	for _, s := range scopes {
		if !RoleAllows(s.Role, perm) {
			continue
		}
		switch s.ResourceType {
		case "workspace":
			if s.ResourceID == workspaceID {
				return true
			}
		case "project":
			if res.ProjectID != "" && s.ResourceID == res.ProjectID {
				return true
			}
			if res.Type == "project" && s.ResourceID == res.ID {
				return true
			}
		case "doc":
			if res.DocID != "" && s.ResourceID == res.DocID {
				return true
			}
			if res.Type == "doc" && s.ResourceID == res.ID {
				return true
			}
		}
	}
	return false
}

// ViewSets implements Guard: members see everything, others nothing.
func (g *RoleGuard) ViewSets(ctx context.Context, id *auth.Identity, workspaceID string) ([]string, error) {
	if id == nil {
		return []string{}, nil
	}
	if id.Anonymous {
		if id.WorkspaceID == workspaceID {
			return nil, nil
		}
		return []string{}, nil
	}
	role, err := g.Role(ctx, id, workspaceID)
	if err != nil {
		return nil, err
	}
	if role == "" {
		return []string{}, nil
	}
	if id.IsAgent() && id.WorkspaceID != workspaceID {
		return []string{}, nil
	}
	return nil, nil
}
