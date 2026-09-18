// Package policy compiles workspace permission schemes into OpenFGA
// authorization models.
//
// A Scheme is the JSON document described in section 6 of the R1
// specification: a closed list of permissions, a set of roles that grant
// them, the levels (workspace, project, doc) at which each role can be
// assigned, optional CEL conditions and optional exclusions. Roles are data,
// not code: Compile turns a Scheme into an OpenFGA DSL model (schema 1.1),
// parses it with the OpenFGA language transformer and validates it with the
// OpenFGA typesystem before it is ever written to a store.
//
// The package performs no I/O; writing the compiled model belongs to the
// embedding service (see internal/fga).
package policy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

// Assignment levels, from widest to narrowest. A role granted at a wider
// level implies the same role at every narrower level below it.
const (
	LevelWorkspace = "workspace"
	LevelProject   = "project"
	LevelDoc       = "doc"
)

// Levels lists the assignment levels from widest to narrowest.
var Levels = []string{LevelWorkspace, LevelProject, LevelDoc}

// Permission names of the closed permission set. Property-level permissions
// use the PermissionSetPropertyPrefix followed by a property identifier or
// "*" for every property.
const (
	PermissionView           = "view"
	PermissionEdit           = "edit"
	PermissionShare          = "share"
	PermissionPublish        = "publish"
	PermissionChangeType     = "change_type"
	PermissionSetPropertyAll = "set_property:*"
	PermissionManageSchema   = "manage_schema"
	PermissionManageMembers  = "manage_members"
	PermissionAdmin          = "admin"

	// PermissionSetPropertyPrefix prefixes property-scoped permissions such
	// as "set_property:status".
	PermissionSetPropertyPrefix = "set_property:"

	// GrantAll is the wildcard grant that confers every permission.
	GrantAll = "*"
)

// ExclusionBlocked is the built-in exclusion relation: a principal that is
// blocked at the workspace loses every permission the exclusion covers.
const ExclusionBlocked = "blocked"

// Role is one named role of a scheme.
type Role struct {
	// Grants lists the permissions the role confers, or "*" for all of them.
	Grants []string `json:"grants"`
	// Inherits names roles whose grants this role also confers. A role may
	// only inherit roles assignable at the same or a wider level.
	Inherits []string `json:"inherits,omitempty"`
	// ReflectedFrom marks a role that is not assigned by people but mirrored
	// from a block property ("props.<name>"): the indexer writes the block
	// relation whenever the property changes. Reflected roles may only grant
	// property-scoped permissions.
	ReflectedFrom string `json:"reflected_from,omitempty"`
}

// Condition is a CEL predicate that can be attached to a role assignment.
// Params map parameter names to their types (string, int, bool, timestamp,
// list(string)); the CEL expression may additionally reference the context
// keys block.props.*, block.type, doc.kind, principal.kind, principal.did and
// now, which the API supplies at check time.
type Condition struct {
	Params map[string]string `json:"params"`
	CEL    string            `json:"cel"`
}

// Scheme is a workspace permission scheme.
type Scheme struct {
	Version      int                  `json:"version"`
	Permissions  []string             `json:"permissions"`
	Roles        map[string]Role      `json:"roles"`
	AssignableAt map[string][]string  `json:"assignable_at"`
	Conditions   map[string]Condition `json:"conditions,omitempty"`
	Exclusions   map[string][]string  `json:"exclusions,omitempty"`
}

// DefaultScheme returns the scheme every new workspace starts with (spec
// section 6). It differs from the specification JSON in one deliberate way:
// the editor role also grants "share", so that the compiled model carries the
// "can_share: editor but not blocked" relation shown in the specification's
// generated model.
func DefaultScheme() Scheme {
	return Scheme{
		Version: 1,
		Permissions: []string{
			PermissionView, PermissionEdit, PermissionShare, PermissionPublish, PermissionChangeType,
			PermissionSetPropertyAll, PermissionManageSchema, PermissionManageMembers, PermissionAdmin,
		},
		Roles: map[string]Role{
			"viewer":    {Grants: []string{PermissionView}},
			"editor":    {Grants: []string{PermissionView, PermissionEdit, PermissionShare, PermissionChangeType, PermissionSetPropertyAll}},
			"publisher": {Grants: []string{PermissionView, PermissionPublish}},
			"admin":     {Grants: []string{GrantAll}},
			"assignee":  {Grants: []string{PermissionSetPropertyPrefix + "status"}, ReflectedFrom: "props.assignee"},
		},
		AssignableAt: map[string][]string{
			"viewer":    {LevelWorkspace, LevelProject, LevelDoc},
			"editor":    {LevelWorkspace, LevelProject, LevelDoc},
			"publisher": {LevelProject},
			"admin":     {LevelWorkspace},
		},
		Conditions: map[string]Condition{
			"not_closed": {Params: map[string]string{"status": "string"}, CEL: "status != 'closed'"},
		},
		Exclusions: map[string][]string{ExclusionBlocked: {GrantAll}},
	}
}

// ParseScheme decodes a scheme from strict JSON: unknown fields and trailing
// data are rejected. It does not validate the scheme; call Validate or
// Compile for that.
func ParseScheme(b []byte) (Scheme, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var s Scheme
	if err := dec.Decode(&s); err != nil {
		return Scheme{}, fmt.Errorf("policy: parse scheme: %w", err)
	}
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("trailing data after scheme document")
		}
		return Scheme{}, fmt.Errorf("policy: parse scheme: %w", err)
	}
	return s, nil
}

// MarshalCanonical encodes the scheme as deterministic JSON: object keys are
// sorted, lists whose order carries no meaning (grants, inherits, assignable
// levels, exclusion values, permissions) are sorted, and empty optional
// sections are omitted. Two semantically equal schemes marshal to identical
// bytes, which makes the output suitable for hashing and version comparison.
func (s Scheme) MarshalCanonical() ([]byte, error) {
	c := s.Clone()
	sort.Strings(c.Permissions)
	for name, r := range c.Roles {
		sort.Strings(r.Grants)
		sort.Strings(r.Inherits)
		if len(r.Inherits) == 0 {
			r.Inherits = nil
		}
		c.Roles[name] = r
	}
	for role, levels := range c.AssignableAt {
		sort.SliceStable(levels, func(i, j int) bool { return levelRank(levels[i]) < levelRank(levels[j]) })
		c.AssignableAt[role] = levels
	}
	for key, perms := range c.Exclusions {
		sort.Strings(perms)
		c.Exclusions[key] = perms
	}
	if len(c.Conditions) == 0 {
		c.Conditions = nil
	}
	if len(c.Exclusions) == 0 {
		c.Exclusions = nil
	}
	b, err := json.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("policy: marshal scheme: %w", err)
	}
	return b, nil
}

// Clone returns a deep copy of the scheme.
func (s Scheme) Clone() Scheme {
	c := Scheme{Version: s.Version}
	if s.Permissions != nil {
		c.Permissions = append([]string(nil), s.Permissions...)
	}
	if s.Roles != nil {
		c.Roles = make(map[string]Role, len(s.Roles))
		for name, r := range s.Roles {
			cr := Role{ReflectedFrom: r.ReflectedFrom}
			if r.Grants != nil {
				cr.Grants = append([]string(nil), r.Grants...)
			}
			if r.Inherits != nil {
				cr.Inherits = append([]string(nil), r.Inherits...)
			}
			c.Roles[name] = cr
		}
	}
	if s.AssignableAt != nil {
		c.AssignableAt = make(map[string][]string, len(s.AssignableAt))
		for role, levels := range s.AssignableAt {
			c.AssignableAt[role] = append([]string(nil), levels...)
		}
	}
	if s.Conditions != nil {
		c.Conditions = make(map[string]Condition, len(s.Conditions))
		for name, cond := range s.Conditions {
			cc := Condition{CEL: cond.CEL}
			if cond.Params != nil {
				cc.Params = make(map[string]string, len(cond.Params))
				for k, v := range cond.Params {
					cc.Params[k] = v
				}
			}
			c.Conditions[name] = cc
		}
	}
	if s.Exclusions != nil {
		c.Exclusions = make(map[string][]string, len(s.Exclusions))
		for key, perms := range s.Exclusions {
			c.Exclusions[key] = append([]string(nil), perms...)
		}
	}
	return c
}

// SchemeError describes one problem found in a scheme. Path is a JSON path
// into the scheme document such as "roles.editor.grants[2]".
type SchemeError struct {
	Path    string
	Message string
}

// Error implements error.
func (e SchemeError) Error() string { return e.Path + ": " + e.Message }

// SchemeErrors is the list of problems found by Validate. It is never empty
// when returned as an error.
type SchemeErrors []SchemeError

// Error implements error by joining the individual messages.
func (es SchemeErrors) Error() string {
	msgs := make([]string, len(es))
	for i, e := range es {
		msgs[i] = e.Error()
	}
	return "policy: invalid scheme: " + strings.Join(msgs, "; ")
}

// Paths returns the JSON paths of all errors, in order.
func (es SchemeErrors) Paths() []string {
	paths := make([]string, len(es))
	for i, e := range es {
		paths[i] = e.Path
	}
	return paths
}

// levelRank orders levels from widest (0) to narrowest; unknown levels sort
// last.
func levelRank(level string) int {
	for i, l := range Levels {
		if l == level {
			return i
		}
	}
	return len(Levels)
}

// widerLevel returns the level immediately wider than the given one, or ""
// for the workspace.
func widerLevel(level string) string {
	r := levelRank(level)
	if r <= 0 || r >= len(Levels) {
		return ""
	}
	return Levels[r-1]
}

// sortedKeys returns the keys of a map in sorted order.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
