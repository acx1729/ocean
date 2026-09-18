package policy

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files")

// typeSection returns the lines of one "type <name>" block of a DSL.
func typeSection(t *testing.T, dsl, name string) string {
	t.Helper()
	_, rest, ok := strings.Cut(dsl, "\ntype "+name+"\n")
	if !ok {
		t.Fatalf("type %s missing from DSL:\n%s", name, dsl)
	}
	if i := strings.Index(rest, "\ntype "); i >= 0 {
		rest = rest[:i]
	}
	if i := strings.Index(rest, "\ncondition "); i >= 0 {
		rest = rest[:i]
	}
	return rest
}

func mustContain(t *testing.T, haystack, name string, needles ...string) {
	t.Helper()
	for _, n := range needles {
		if !strings.Contains(haystack, n) {
			t.Errorf("%s: missing %q in:\n%s", name, n, haystack)
		}
	}
}

func TestCompileDefaultGolden(t *testing.T) {
	c, err := Compile(DefaultScheme())
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	golden := filepath.Join("testdata", "default.fga")
	if *update {
		if err := os.WriteFile(golden, []byte(c.DSL), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden (run with -update to create): %v", err)
	}
	if string(want) != c.DSL {
		t.Errorf("generated DSL differs from %s\n--- want\n%s\n--- got\n%s", golden, want, c.DSL)
	}
	if c.Model == nil || c.Model.GetSchemaVersion() != "1.1" {
		t.Fatalf("model not parsed: %+v", c.Model)
	}
	if len(c.Model.GetTypeDefinitions()) != 8 {
		t.Errorf("expected 8 type definitions, got %d", len(c.Model.GetTypeDefinitions()))
	}
	if _, ok := c.Model.GetConditions()["not_closed"]; !ok {
		t.Errorf("condition not_closed missing from model: %v", c.Model.GetConditions())
	}

	// The spec's generated model, line by line (direct assignments also
	// accept the declared condition; publish reaches workspace admins).
	direct := "[user, agent, group#member, user with not_closed, agent with not_closed, group#member with not_closed]"
	mustContain(t, typeSection(t, c.DSL, "workspace"), "workspace",
		"define admin: "+direct+"\n",
		"define editor: "+direct+" or admin\n",
		"define viewer: "+direct+" or editor\n",
		"define blocked: [user, agent]\n",
		"define can_manage_members: admin but not blocked\n",
		"define can_manage_schema: admin but not blocked\n",
	)
	mustContain(t, typeSection(t, c.DSL, "project"), "project",
		"define workspace: [workspace]\n",
		"define publisher: "+direct+"\n",
		"define editor: "+direct+" or editor from workspace\n",
		"define viewer: "+direct+" or editor or publisher or viewer from workspace\n",
		"define blocked: blocked from workspace\n",
		"define can_view: viewer but not blocked\n",
		"define can_edit: editor but not blocked\n",
		"define can_publish: (publisher or admin from workspace) but not blocked\n",
	)
	mustContain(t, typeSection(t, c.DSL, "doc"), "doc",
		"define project: [project]\n",
		"define parent: [doc]\n",
		"define editor: "+direct+" or editor from parent or editor from project\n",
		"define viewer: [user, agent, group#member, user:*, user with not_closed, agent with not_closed, group#member with not_closed] or editor or viewer from parent or viewer from project\n",
		"define blocked: blocked from project\n",
		"define can_view: viewer but not blocked\n",
		"define can_edit: editor but not blocked\n",
		"define can_share: editor but not blocked\n",
		"define can_change_type: editor but not blocked\n",
	)
	mustContain(t, typeSection(t, c.DSL, "block"), "block",
		"define doc: [doc]\n",
		"define assignee: [user]\n",
		"define can_edit: can_edit from doc\n",
		"define can_set_status: can_edit from doc or assignee\n",
	)
	mustContain(t, typeSection(t, c.DSL, "attribute_def"), "attribute_def",
		"define project: [project]\n",
		"define writer: [user, agent, group#member] or editor from project\n",
	)
	mustContain(t, c.DSL, "conditions", "\ncondition not_closed(status: string) {\n  status != 'closed'\n}\n")

	wantRoles := map[string]map[string]string{
		"workspace":     {"admin": "admin", "editor": "editor", "viewer": "viewer"},
		"project":       {"editor": "editor", "publisher": "publisher", "viewer": "viewer"},
		"doc":           {"editor": "editor", "viewer": "viewer"},
		"block":         {"assignee": "assignee"},
		"attribute_def": nil,
	}
	for typ, want := range wantRoles {
		got := c.RoleRelations[typ]
		if len(got) != len(want) {
			t.Errorf("RoleRelations[%s] = %v, want %v", typ, got, want)
			continue
		}
		for role, rel := range want {
			if got[role] != rel {
				t.Errorf("RoleRelations[%s][%s] = %q, want %q", typ, role, got[role], rel)
			}
		}
	}
	wantPerms := map[string]map[string]string{
		"workspace":     {"manage_members": "can_manage_members", "manage_schema": "can_manage_schema"},
		"project":       {"view": "can_view", "edit": "can_edit", "publish": "can_publish"},
		"doc":           {"view": "can_view", "edit": "can_edit", "share": "can_share", "change_type": "can_change_type"},
		"block":         {"edit": "can_edit", "set_property:status": "can_set_status"},
		"attribute_def": {"set_property:*": "writer"},
	}
	for typ, want := range wantPerms {
		got := c.PermissionRelations[typ]
		if len(got) != len(want) {
			t.Errorf("PermissionRelations[%s] = %v, want %v", typ, got, want)
			continue
		}
		for perm, rel := range want {
			if got[perm] != rel {
				t.Errorf("PermissionRelations[%s][%s] = %q, want %q", typ, perm, got[perm], rel)
			}
		}
	}
	if len(c.PublicDocRelations) != 1 || c.PublicDocRelations[0] != "viewer" {
		t.Errorf("PublicDocRelations = %v, want [viewer]", c.PublicDocRelations)
	}
}

func TestCompileIsDeterministic(t *testing.T) {
	first, err := Compile(DefaultScheme())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		again, err := Compile(DefaultScheme())
		if err != nil {
			t.Fatal(err)
		}
		if again.DSL != first.DSL {
			t.Fatalf("DSL changed between compilations:\n%s\n---\n%s", first.DSL, again.DSL)
		}
	}
}

func TestCompileCustomReviewerRole(t *testing.T) {
	s := DefaultScheme()
	s.Roles["reviewer"] = Role{Grants: []string{PermissionView, PermissionPublish}}
	s.AssignableAt["reviewer"] = []string{LevelProject}
	c, err := Compile(s)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	project := typeSection(t, c.DSL, "project")
	mustContain(t, project, "project",
		"define reviewer: [user, agent, group#member, user with not_closed, agent with not_closed, group#member with not_closed]\n",
		"define can_publish: (publisher or reviewer or admin from workspace) but not blocked\n",
		"define viewer: [user, agent, group#member, user with not_closed, agent with not_closed, group#member with not_closed] or editor or publisher or reviewer or viewer from workspace\n",
	)
	if strings.Contains(typeSection(t, c.DSL, "doc"), "reviewer") {
		t.Errorf("reviewer must not appear on doc:\n%s", typeSection(t, c.DSL, "doc"))
	}
	if c.RoleRelations["project"]["reviewer"] != "reviewer" {
		t.Errorf("RoleRelations[project][reviewer] = %q", c.RoleRelations["project"]["reviewer"])
	}
}

func TestCompileInheritsAndPassThrough(t *testing.T) {
	s := DefaultScheme()
	// A workspace-only role that grants exactly what viewer grants: nothing
	// implies it, so project and doc reach it through a pass-through.
	s.Roles["auditor"] = Role{Grants: []string{PermissionView}}
	s.AssignableAt["auditor"] = []string{LevelWorkspace}
	// A project role that inherits editor and adds publish: it folds into
	// editor (so editor-derived permissions reach it through editor) and is
	// named wherever its own grant matters; publisher does not absorb it since
	// its own grants do not cover publisher's.
	s.Roles["lead"] = Role{Grants: []string{PermissionPublish}, Inherits: []string{"editor"}}
	s.AssignableAt["lead"] = []string{LevelProject}
	c, err := Compile(s)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	project := typeSection(t, c.DSL, "project")
	mustContain(t, project, "project",
		"define auditor: auditor from workspace\n",
		"define editor: [user, agent, group#member, user with not_closed, agent with not_closed, group#member with not_closed] or lead or editor from workspace\n",
		"define publisher: [user, agent, group#member, user with not_closed, agent with not_closed, group#member with not_closed]\n",
		"define can_view: (auditor or viewer) but not blocked\n",
		"define can_edit: editor but not blocked\n",
		"define can_publish: (lead or publisher or admin from workspace) but not blocked\n",
	)
	mustContain(t, typeSection(t, c.DSL, "doc"), "doc",
		"define can_view: (viewer or auditor from project) but not blocked\n",
		"define can_edit: editor but not blocked\n",
	)
	if strings.Contains(typeSection(t, c.DSL, "doc"), "lead") {
		t.Errorf("lead is reached through editor from project and must not appear on doc:\n%s", typeSection(t, c.DSL, "doc"))
	}
	if _, ok := c.RoleRelations["project"]["auditor"]; ok {
		t.Errorf("pass-through relation must not be assignable: %v", c.RoleRelations["project"])
	}
	if got := c.roleFor("project:P", "auditor"); got != "auditor" {
		t.Errorf("roleFor pass-through = %q", got)
	}
}

func TestCompileConditionsWithContextKeys(t *testing.T) {
	s := DefaultScheme()
	s.Conditions["office_hours"] = Condition{
		Params: map[string]string{"start": "timestamp", "kinds": "list(string)"},
		CEL:    "now >= start && principal.kind in kinds && block.props.status != 'closed' && block.type == 'task' && doc.kind != 'boundary' && has(block.props.due)",
	}
	c, err := Compile(s)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	mustContain(t, c.DSL, "conditions",
		"\ncondition office_hours(block: map<any>, doc: map<any>, kinds: list<string>, now: timestamp, principal: map<any>, start: timestamp) {\n",
		"user with office_hours, agent with office_hours, group#member with office_hours",
	)
	if _, ok := c.Model.GetConditions()["office_hours"]; !ok {
		t.Errorf("office_hours missing from model conditions")
	}
}

func TestCompileRejectsInvalidScheme(t *testing.T) {
	s := DefaultScheme()
	delete(s.Roles, "admin")
	_, err := Compile(s)
	var errs SchemeErrors
	if !asSchemeErrors(err, &errs) {
		t.Fatalf("expected SchemeErrors, got %T %v", err, err)
	}
	if !containsString(errs.Paths(), "roles.admin") {
		t.Errorf("paths = %v", errs.Paths())
	}
}
