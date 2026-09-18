package policy

import (
	"regexp"
	"strings"
)

// identRE matches scheme identifiers (role, condition and parameter names):
// lower-case snake_case, at most 50 characters, which is also what OpenFGA
// accepts for relation and condition names.
var identRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,49}$`)

// propertyIDRE matches the identifier part of a "set_property:<id>" grant:
// either a property name or a UUID.
var propertyIDRE = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]{0,63}|[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})$`)

// reflectedFromRE matches the reflected_from field of a role.
var reflectedFromRE = regexp.MustCompile(`^props\.([A-Za-z_][A-Za-z0-9_]{0,63}|[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})$`)

// nonIdentCharRE matches characters that cannot appear in a relation name.
var nonIdentCharRE = regexp.MustCompile(`[^A-Za-z0-9_]`)

// basePermissions is the closed set of permission names, in the order the
// generated model lists their relations.
var basePermissions = []string{
	PermissionView, PermissionEdit, PermissionShare, PermissionPublish, PermissionChangeType,
	PermissionSetPropertyAll, PermissionManageMembers, PermissionManageSchema, PermissionAdmin,
}

// permissionTypes maps each base permission to the object types that carry
// a "can_<permission>" relation for it. Property permissions are special:
// "set_property:*" compiles to attribute_def#writer and "set_property:<id>"
// to block#can_set_<id>; the "admin" permission is only a grant marker.
var permissionTypes = map[string][]string{
	PermissionView:          {LevelProject, LevelDoc},
	PermissionEdit:          {LevelProject, LevelDoc},
	PermissionShare:         {LevelDoc},
	PermissionPublish:       {LevelProject},
	PermissionChangeType:    {LevelDoc},
	PermissionManageMembers: {LevelWorkspace},
	PermissionManageSchema:  {LevelWorkspace},
}

// reservedRelationNames are relation names the generated model uses for
// structure; a role may not take one of them.
var reservedRelationNames = map[string]bool{
	"parent": true, "project": true, "workspace": true, "doc": true,
	"member": true, "owner": true, "writer": true, ExclusionBlocked: true,
	"user": true, "agent": true, "group": true, "block": true, "attribute_def": true,
}

// isReservedRoleName reports whether a role name collides with a structural
// relation or a generated permission relation.
func isReservedRoleName(name string) bool {
	return reservedRelationNames[name] || strings.HasPrefix(name, "can_")
}

// parsePermission splits a permission or grant into its base name and, for
// property permissions, the property identifier ("*" for all properties).
// ok is false when the name is not part of the closed permission set.
func parsePermission(p string) (base, property string, ok bool) {
	if strings.HasPrefix(p, PermissionSetPropertyPrefix) {
		id := strings.TrimPrefix(p, PermissionSetPropertyPrefix)
		if id == "*" || propertyIDRE.MatchString(id) {
			return PermissionSetPropertyPrefix, id, true
		}
		return "", "", false
	}
	for _, b := range basePermissions {
		if b == p {
			return p, "", true
		}
	}
	return "", "", false
}

// PermissionRelation returns the name of the computed relation that carries
// a permission in the generated model: "view" becomes "can_view",
// "manage_members" becomes "can_manage_members" and every property
// permission ("set_property:*" as well as "set_property:<id>") becomes
// "can_set_property"; property-level checks are answered by
// attribute_def#writer and, for reflected roles, by block#can_set_<id> (see
// PropertyRelation).
func PermissionRelation(permission string) string {
	if strings.HasPrefix(permission, PermissionSetPropertyPrefix) {
		return "can_set_property"
	}
	return "can_" + sanitizeRelation(permission)
}

// RoleRelation returns the relation name a scheme role compiles to on each
// type it is assignable at. Role names are relation names verbatim; the only
// exception is a role whose name collides with a structural relation of the
// generated model (parent, project, workspace, doc, blocked, ...) or begins
// with "can_", which Validate rejects and which is mapped to "role_<name>"
// here so that the mapping stays total.
func RoleRelation(role string) string {
	if isReservedRoleName(role) {
		return "role_" + sanitizeRelation(role)
	}
	return sanitizeRelation(role)
}

// PropertyRelation returns the block relation that answers "may this
// principal set property <id> on this block": "can_set_<id>", with
// characters that are not legal in a relation name replaced by "_".
func PropertyRelation(propertyID string) string {
	return "can_set_" + sanitizeRelation(propertyID)
}

// sanitizeRelation replaces characters that OpenFGA does not accept in
// relation names.
func sanitizeRelation(name string) string {
	return nonIdentCharRE.ReplaceAllString(name, "_")
}
