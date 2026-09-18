// Package authz is the permission gate every service calls. The Guard
// interface is implemented first by RoleGuard (workspace membership roles)
// and, once the OpenFGA layer is wired, by the FGA-backed guard that adds
// doc-level grants, boundaries and the doc_access projection.
package authz

import (
	"context"

	"github.com/acx1729/ocean/internal/auth"
)

// Permission names are closed (specification section 6).
type Permission string

const (
	View          Permission = "view"
	Edit          Permission = "edit"
	Share         Permission = "share"
	Publish       Permission = "publish"
	ChangeType    Permission = "change_type"
	SetProperty   Permission = "set_property"
	ManageSchema  Permission = "manage_schema"
	ManageMembers Permission = "manage_members"
	Admin         Permission = "admin"
)

// Resource identifies what a check is about.
type Resource struct {
	Type      string // workspace, project, doc, block
	ID        string
	ProjectID string // for docs and blocks, when known
	DocID     string // for blocks
}

// Workspace, Project, Doc and Block build resources.
func Workspace(id string) Resource { return Resource{Type: "workspace", ID: id} }

// Project resource.
func Project(id string) Resource { return Resource{Type: "project", ID: id, ProjectID: id} }

// Doc resource.
func Doc(id, projectID string) Resource {
	return Resource{Type: "doc", ID: id, ProjectID: projectID, DocID: id}
}

// Block resource.
func Block(id, docID, projectID string) Resource {
	return Resource{Type: "block", ID: id, ProjectID: projectID, DocID: docID}
}

// Guard answers permission questions. Check returns nil when allowed and a
// Connect error otherwise: NotFound when the caller may not even learn the
// resource exists, PermissionDenied when it may.
type Guard interface {
	Check(ctx context.Context, id *auth.Identity, workspaceID string, perm Permission, res Resource) error
	// Role returns the workspace role of the caller's owner, "" for non-members.
	Role(ctx context.Context, id *auth.Identity, workspaceID string) (string, error)
	// ViewSets returns the doc_access set ids that filter queries for the
	// caller; nil means unrestricted, an empty slice means nothing is visible.
	ViewSets(ctx context.Context, id *auth.Identity, workspaceID string) ([]string, error)
}

// RoleGrants maps the default scheme's roles to permissions. The FGA guard
// derives this from the compiled scheme instead.
var RoleGrants = map[string][]Permission{
	"viewer":    {View},
	"editor":    {View, Edit, ChangeType, SetProperty, Share},
	"publisher": {View, Publish},
	"admin":     {View, Edit, Share, Publish, ChangeType, SetProperty, ManageSchema, ManageMembers, Admin},
}

// RoleAllows reports whether role grants perm under RoleGrants.
func RoleAllows(role string, perm Permission) bool {
	for _, p := range RoleGrants[role] {
		if p == perm {
			return true
		}
	}
	return false
}
