package policy

import "testing"

func TestPermissionRelation(t *testing.T) {
	cases := map[string]string{
		"view":                "can_view",
		"edit":                "can_edit",
		"share":               "can_share",
		"publish":             "can_publish",
		"change_type":         "can_change_type",
		"set_property:*":      "can_set_property",
		"set_property:status": "can_set_property",
		"set_property:6f1c2a4e-9b3d-4c5e-8f7a-0b1c2d3e4f5a": "can_set_property",
		"manage_schema":  "can_manage_schema",
		"manage_members": "can_manage_members",
		"admin":          "can_admin",
	}
	for perm, want := range cases {
		if got := PermissionRelation(perm); got != want {
			t.Errorf("PermissionRelation(%q) = %q, want %q", perm, got, want)
		}
	}
}

func TestRoleAndPropertyRelation(t *testing.T) {
	if got := RoleRelation("editor"); got != "editor" {
		t.Errorf("RoleRelation(editor) = %q", got)
	}
	if got := RoleRelation("parent"); got != "role_parent" {
		t.Errorf("RoleRelation(parent) = %q", got)
	}
	if got := RoleRelation("can_view"); got != "role_can_view" {
		t.Errorf("RoleRelation(can_view) = %q", got)
	}
	if got := PropertyRelation("status"); got != "can_set_status" {
		t.Errorf("PropertyRelation(status) = %q", got)
	}
	if got := PropertyRelation("6f1c2a4e-9b3d-4c5e-8f7a-0b1c2d3e4f5a"); got != "can_set_6f1c2a4e_9b3d_4c5e_8f7a_0b1c2d3e4f5a" {
		t.Errorf("PropertyRelation(uuid) = %q", got)
	}
}

func TestParsePermission(t *testing.T) {
	for _, p := range []string{"view", "set_property:*", "set_property:status", "set_property:6f1c2a4e-9b3d-4c5e-8f7a-0b1c2d3e4f5a", "admin"} {
		if _, _, ok := parsePermission(p); !ok {
			t.Errorf("parsePermission(%q) rejected", p)
		}
	}
	for _, p := range []string{"", "*", "delete", "set_property:", "set_property:a b", "View"} {
		if _, _, ok := parsePermission(p); ok {
			t.Errorf("parsePermission(%q) accepted", p)
		}
	}
}
