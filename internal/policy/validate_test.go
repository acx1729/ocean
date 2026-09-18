package policy

import (
	"errors"
	"testing"
)

func asSchemeErrors(err error, target *SchemeErrors) bool {
	return errors.As(err, target)
}

func TestValidateDefaultScheme(t *testing.T) {
	if err := Validate(DefaultScheme()); err != nil {
		t.Fatalf("default scheme invalid: %v", err)
	}
}

func TestValidateSpecJSON(t *testing.T) {
	// The scheme exactly as printed in the specification.
	s, err := ParseScheme([]byte(specJSON))
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(s); err != nil {
		t.Fatalf("spec scheme invalid: %v", err)
	}
	if _, err := Compile(s); err != nil {
		t.Fatalf("spec scheme does not compile: %v", err)
	}
}

func TestValidateNegativeFixtures(t *testing.T) {
	fixtures := []struct {
		name   string
		mutate func(s *Scheme)
		path   string
	}{
		{"version zero", func(s *Scheme) { s.Version = 0 }, "version"},
		{"unknown permission", func(s *Scheme) { s.Permissions = append(s.Permissions, "delete") }, "permissions[9]"},
		{"duplicate permission", func(s *Scheme) { s.Permissions = append(s.Permissions, "view") }, "permissions[9]"},
		{"unknown grant", func(s *Scheme) { r := s.Roles["editor"]; r.Grants = append(r.Grants, "fly"); s.Roles["editor"] = r }, "roles.editor.grants[5]"},
		{"grant not listed", func(s *Scheme) {
			s.Permissions = []string{"view", "edit", "share", "change_type", "set_property:*", "manage_schema", "manage_members", "admin"}
		}, "roles.publisher.grants[1]"},
		{"duplicate grant", func(s *Scheme) { s.Roles["viewer"] = Role{Grants: []string{"view", "view"}} }, "roles.viewer.grants[1]"},
		{"empty grants", func(s *Scheme) { s.Roles["nothing"] = Role{}; s.Assign("nothing", LevelDoc) }, "roles.nothing.grants"},
		{"invalid role name", func(s *Scheme) { s.Roles["Editor"] = Role{Grants: []string{"view"}}; s.Assign("Editor", LevelDoc) }, "roles.Editor"},
		{"reserved role name", func(s *Scheme) { s.Roles["parent"] = Role{Grants: []string{"view"}}; s.Assign("parent", LevelDoc) }, "roles.parent"},
		{"inherit narrower level", func(s *Scheme) { s.Roles["admin"] = Role{Grants: []string{"*"}, Inherits: []string{"publisher"}} }, "roles.admin.inherits[0]"},
		{"inherit unknown role", func(s *Scheme) { s.Roles["editor"] = Role{Grants: []string{"view"}, Inherits: []string{"ghost"}} }, "roles.editor.inherits[0]"},
		{"inherit self", func(s *Scheme) { s.Roles["editor"] = Role{Grants: []string{"view"}, Inherits: []string{"editor"}} }, "roles.editor.inherits[0]"},
		{"inheritance cycle", func(s *Scheme) {
			s.Roles["editor"] = Role{Grants: []string{"edit"}, Inherits: []string{"viewer"}}
			s.Roles["viewer"] = Role{Grants: []string{"view"}, Inherits: []string{"editor"}}
		}, "roles.editor.inherits"},
		{"inherit reflected role", func(s *Scheme) { s.Roles["editor"] = Role{Grants: []string{"view"}, Inherits: []string{"assignee"}} }, "roles.editor.inherits[0]"},
		{"assignable unknown role", func(s *Scheme) { s.AssignableAt["ghost"] = []string{LevelDoc} }, "assignable_at.ghost"},
		{"assignable invalid level", func(s *Scheme) { s.AssignableAt["viewer"] = []string{"universe"} }, "assignable_at.viewer[0]"},
		{"assignable duplicate level", func(s *Scheme) { s.AssignableAt["viewer"] = []string{LevelDoc, LevelDoc} }, "assignable_at.viewer[1]"},
		{"assignable empty", func(s *Scheme) { s.AssignableAt["viewer"] = nil }, "assignable_at.viewer"},
		{"role never assignable", func(s *Scheme) { s.Roles["lurker"] = Role{Grants: []string{"view"}} }, "assignable_at.lurker"},
		{"reflected role assignable", func(s *Scheme) { s.AssignableAt["assignee"] = []string{LevelDoc} }, "assignable_at.assignee"},
		{"reflected_from malformed", func(s *Scheme) {
			s.Roles["assignee"] = Role{Grants: []string{"set_property:status"}, ReflectedFrom: "status"}
		}, "roles.assignee.reflected_from"},
		{"reflected role grants view", func(s *Scheme) {
			s.Roles["assignee"] = Role{Grants: []string{"set_property:status", "view"}, ReflectedFrom: "props.assignee"}
		}, "roles.assignee.grants[1]"},
		{"reflected role inherits", func(s *Scheme) {
			s.Roles["assignee"] = Role{Grants: []string{"set_property:status"}, ReflectedFrom: "props.assignee", Inherits: []string{"viewer"}}
		}, "roles.assignee.inherits"},
		{"admin missing", func(s *Scheme) { delete(s.Roles, "admin"); delete(s.AssignableAt, "admin") }, "roles.admin"},
		{"admin without wildcard", func(s *Scheme) { s.Roles["admin"] = Role{Grants: []string{"manage_members"}} }, "roles.admin.grants"},
		{"admin not at workspace", func(s *Scheme) { s.AssignableAt["admin"] = []string{LevelProject} }, "assignable_at.admin"},
		{"exclusion unknown key", func(s *Scheme) { s.Exclusions["ghost"] = []string{"*"} }, "exclusions.ghost"},
		{"exclusion unknown permission", func(s *Scheme) { s.Exclusions["blocked"] = []string{"fly"} }, "exclusions.blocked[0]"},
		{"exclusion empty", func(s *Scheme) { s.Exclusions["blocked"] = nil }, "exclusions.blocked"},
		{"exclusion reflected role", func(s *Scheme) { s.Exclusions = map[string][]string{"assignee": {"*"}} }, "exclusions.assignee"},
		{"permission excluded twice", func(s *Scheme) { s.Exclusions["viewer"] = []string{"view"} }, "exclusions.viewer[0]"},
		{"condition unknown context key", func(s *Scheme) {
			s.Conditions["bad"] = Condition{Params: map[string]string{}, CEL: "block.owner == 'x'"}
		}, "conditions.bad.cel"},
		{"condition unknown identifier", func(s *Scheme) { s.Conditions["bad"] = Condition{CEL: "foo == 1"} }, "conditions.bad.cel"},
		{"condition not bool", func(s *Scheme) {
			s.Conditions["bad"] = Condition{Params: map[string]string{"status": "string"}, CEL: "status"}
		}, "conditions.bad.cel"},
		{"condition syntax error", func(s *Scheme) {
			s.Conditions["bad"] = Condition{Params: map[string]string{"status": "string"}, CEL: "status !="}
		}, "conditions.bad.cel"},
		{"condition empty", func(s *Scheme) { s.Conditions["bad"] = Condition{Params: map[string]string{"status": "string"}} }, "conditions.bad.cel"},
		{"condition unsupported param type", func(s *Scheme) {
			s.Conditions["bad"] = Condition{Params: map[string]string{"n": "double"}, CEL: "n > 1.0"}
		}, "conditions.bad.params.n"},
		{"condition reserved param", func(s *Scheme) {
			s.Conditions["bad"] = Condition{Params: map[string]string{"now": "timestamp"}, CEL: "now > now"}
		}, "conditions.bad.params.now"},
		{"condition bad param name", func(s *Scheme) {
			s.Conditions["bad"] = Condition{Params: map[string]string{"Status": "string"}, CEL: "Status == ''"}
		}, "conditions.bad.params.Status"},
		{"condition bad name", func(s *Scheme) {
			s.Conditions["Not-Closed"] = Condition{Params: map[string]string{"s": "string"}, CEL: "s != ''"}
		}, "conditions.Not-Closed"},
	}
	for _, f := range fixtures {
		t.Run(f.name, func(t *testing.T) {
			s := DefaultScheme()
			f.mutate(&s)
			err := Validate(s)
			var errs SchemeErrors
			if !asSchemeErrors(err, &errs) {
				t.Fatalf("expected SchemeErrors, got %T: %v", err, err)
			}
			if !containsString(errs.Paths(), f.path) {
				t.Errorf("expected an error at %q, got %v", f.path, errs)
			}
			if _, err := Compile(s); err == nil {
				t.Errorf("Compile accepted an invalid scheme")
			}
		})
	}
}

// Assign is a test helper that marks a role assignable at a level.
func (s *Scheme) Assign(role, level string) {
	s.AssignableAt[role] = append(s.AssignableAt[role], level)
}

const specJSON = `{
  "version": 1,
  "permissions": ["view", "edit", "share", "publish", "change_type",
                  "set_property:*", "manage_schema", "manage_members", "admin"],
  "roles": {
    "viewer": { "grants": ["view"] },
    "editor": { "grants": ["view", "edit", "change_type", "set_property:*"] },
    "publisher": { "grants": ["view", "publish"] },
    "admin": { "grants": ["*"] },
    "assignee": { "grants": ["set_property:status"], "reflected_from": "props.assignee" }
  },
  "assignable_at": {
    "viewer": ["workspace", "project", "doc"],
    "editor": ["workspace", "project", "doc"],
    "publisher": ["project"],
    "admin": ["workspace"]
  },
  "conditions": {
    "not_closed": { "params": { "status": "string" }, "cel": "status != 'closed'" }
  },
  "exclusions": { "blocked": ["*"] }
}`
