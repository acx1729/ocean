package policy

import (
	"bytes"
	"strings"
	"testing"
)

func TestParseSchemeStrict(t *testing.T) {
	if _, err := ParseScheme([]byte(`{"version": 1, "permissions": ["view"], "roles": {}, "assignable_at": {}, "extra": 1}`)); err == nil {
		t.Error("unknown field accepted")
	}
	if _, err := ParseScheme([]byte(`{"version": 1, "permissions": ["view"], "roles": {}, "assignable_at": {}} {}`)); err == nil {
		t.Error("trailing data accepted")
	}
	if _, err := ParseScheme([]byte(`{"version": "1"}`)); err == nil {
		t.Error("wrong type accepted")
	}
	s, err := ParseScheme([]byte(specJSON))
	if err != nil {
		t.Fatal(err)
	}
	if s.Roles["assignee"].ReflectedFrom != "props.assignee" || len(s.AssignableAt["viewer"]) != 3 {
		t.Errorf("unexpected parse result: %+v", s)
	}
}

func TestMarshalCanonicalDeterministic(t *testing.T) {
	a, err := DefaultScheme().MarshalCanonical()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		b, err := DefaultScheme().MarshalCanonical()
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(a, b) {
			t.Fatalf("marshal differs between runs:\n%s\n%s", a, b)
		}
	}

	// Order of grants, permissions, levels and exclusion values carries no
	// meaning and must not change the canonical form.
	permuted := DefaultScheme()
	reverse(permuted.Permissions)
	for name, r := range permuted.Roles {
		reverse(r.Grants)
		permuted.Roles[name] = r
	}
	for role, levels := range permuted.AssignableAt {
		reverse(levels)
		permuted.AssignableAt[role] = levels
	}
	permuted.Roles["editor"] = Role{Grants: permuted.Roles["editor"].Grants, Inherits: []string{}}
	c, err := permuted.MarshalCanonical()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, c) {
		t.Fatalf("permuted scheme marshals differently:\n%s\n%s", a, c)
	}

	// Round trip through ParseScheme.
	parsed, err := ParseScheme(a)
	if err != nil {
		t.Fatal(err)
	}
	d, err := parsed.MarshalCanonical()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, d) {
		t.Fatalf("round trip changed canonical form:\n%s\n%s", a, d)
	}
	if err := Validate(parsed); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(a), "inherits") || strings.Contains(string(a), "\n") {
		t.Errorf("canonical form should omit empty sections and whitespace: %s", a)
	}
}

func TestCloneIsDeep(t *testing.T) {
	s := DefaultScheme()
	c := s.Clone()
	c.Roles["viewer"].Grants[0] = "edit"
	c.AssignableAt["viewer"][0] = "doc"
	c.Conditions["not_closed"].Params["status"] = "int"
	c.Exclusions["blocked"][0] = "view"
	c.Permissions[0] = "edit"
	if s.Roles["viewer"].Grants[0] != "view" || s.AssignableAt["viewer"][0] != "workspace" ||
		s.Conditions["not_closed"].Params["status"] != "string" || s.Exclusions["blocked"][0] != "*" || s.Permissions[0] != "view" {
		t.Errorf("Clone shares memory with the original: %+v", s)
	}
}

func TestSchemeErrorsFormatting(t *testing.T) {
	errs := SchemeErrors{{Path: "roles.x", Message: "bad"}, {Path: "version", Message: "worse"}}
	if got := errs.Error(); !strings.Contains(got, "roles.x: bad") || !strings.Contains(got, "version: worse") {
		t.Errorf("Error() = %q", got)
	}
	if got := errs.Paths(); len(got) != 2 || got[0] != "roles.x" {
		t.Errorf("Paths() = %v", got)
	}
}

func reverse(s []string) {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
}
