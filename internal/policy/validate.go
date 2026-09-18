package policy

import (
	"fmt"
	"sort"
	"strings"
)

// analysis is the validated, normalised view of a scheme that the compiler
// works from.
type analysis struct {
	s          Scheme
	roles      []string                   // sorted role names
	listed     map[string]bool            // permissions listed in the scheme
	reflected  map[string]bool            // reflected roles
	assignable map[string]map[string]bool // level -> role -> assignable
	widest     map[string]string          // role -> widest assignable level
	eff        map[string]map[string]bool // role -> effective literal grants (own and inherited)
	direct     map[string]map[string]bool // role -> own literal grants
	properties []string                   // sorted property ids named by set_property:<id> grants
	conditions []conditionInfo            // sorted by name
	exclusions map[string][]string        // exclusion key -> covered permissions ("*" allowed)
}

// Validate checks a scheme against the compiler's rules and returns a
// SchemeErrors listing every violation with its JSON path, or nil.
func Validate(s Scheme) error {
	if _, errs := analyze(s); len(errs) > 0 {
		return errs
	}
	return nil
}

// analyze validates a scheme and, when it is valid, returns the analysis
// the compiler needs.
func analyze(s Scheme) (*analysis, SchemeErrors) {
	a := &analysis{
		s:          s,
		roles:      sortedKeys(s.Roles),
		listed:     map[string]bool{},
		reflected:  map[string]bool{},
		assignable: map[string]map[string]bool{},
		widest:     map[string]string{},
		eff:        map[string]map[string]bool{},
		direct:     map[string]map[string]bool{},
		exclusions: map[string][]string{},
	}
	for _, l := range Levels {
		a.assignable[l] = map[string]bool{}
	}
	var errs SchemeErrors
	fail := func(path, format string, args ...any) {
		errs = append(errs, SchemeError{Path: path, Message: fmt.Sprintf(format, args...)})
	}

	if s.Version < 1 {
		fail("version", "must be at least 1")
	}
	a.checkPermissions(fail)
	a.checkRoles(fail)
	a.checkAssignableAt(fail)
	a.checkInherits(fail)
	a.checkAdmin(fail)
	a.checkExclusions(fail)
	for _, name := range sortedKeys(s.Conditions) {
		if info, ok := checkCondition(name, s.Conditions[name], &errs); ok {
			a.conditions = append(a.conditions, info)
		}
	}
	if len(errs) > 0 {
		return nil, errs
	}
	a.computeEffectiveGrants()
	return a, nil
}

type failFunc func(path, format string, args ...any)

func (a *analysis) checkPermissions(fail failFunc) {
	if len(a.s.Permissions) == 0 {
		fail("permissions", "at least one permission is required")
	}
	for i, p := range a.s.Permissions {
		path := fmt.Sprintf("permissions[%d]", i)
		if _, _, ok := parsePermission(p); !ok {
			fail(path, "unknown permission %q", p)
			continue
		}
		if a.listed[p] {
			fail(path, "duplicate permission %q", p)
		}
		a.listed[p] = true
	}
}

// grantListed reports whether a grant is covered by the permission list: it
// is listed itself, or it is a property-scoped grant and "set_property:*" is
// listed.
func (a *analysis) grantListed(grant string) bool {
	if a.listed[grant] {
		return true
	}
	base, prop, ok := parsePermission(grant)
	return ok && base == PermissionSetPropertyPrefix && prop != "*" && a.listed[PermissionSetPropertyAll]
}

func (a *analysis) checkRoles(fail failFunc) {
	if len(a.s.Roles) == 0 {
		fail("roles", "at least one role is required")
	}
	props := map[string]bool{}
	for _, name := range a.roles {
		r := a.s.Roles[name]
		base := "roles." + name
		if !identRE.MatchString(name) {
			fail(base, "role names must be lower-case snake_case of at most 50 characters")
		} else if isReservedRoleName(name) {
			fail(base, "%q is reserved for a structural relation of the generated model", name)
		}
		if r.ReflectedFrom != "" {
			a.reflected[name] = true
			if !reflectedFromRE.MatchString(r.ReflectedFrom) {
				fail(base+".reflected_from", "must name a block property as props.<name>")
			}
			if len(r.Inherits) > 0 {
				fail(base+".inherits", "reflected roles cannot inherit other roles")
			}
		}
		if len(r.Grants) == 0 && len(r.Inherits) == 0 {
			fail(base+".grants", "role must grant at least one permission")
		}
		seen := map[string]bool{}
		for i, g := range r.Grants {
			gpath := fmt.Sprintf("%s.grants[%d]", base, i)
			if seen[g] {
				fail(gpath, "duplicate grant %q", g)
				continue
			}
			seen[g] = true
			if g == GrantAll {
				if r.ReflectedFrom != "" {
					fail(gpath, "reflected roles may only grant set_property:<id>")
				}
				continue
			}
			pbase, prop, ok := parsePermission(g)
			if !ok {
				fail(gpath, "unknown permission %q", g)
				continue
			}
			if !a.grantListed(g) {
				fail(gpath, "grant %q names a permission that is not listed in permissions", g)
				continue
			}
			if pbase == PermissionSetPropertyPrefix && prop != "*" {
				props[prop] = true
			} else if r.ReflectedFrom != "" {
				fail(gpath, "reflected roles may only grant set_property:<id>")
			}
		}
	}
	a.properties = sortedKeys(props)
}

func (a *analysis) checkAssignableAt(fail failFunc) {
	for _, role := range sortedKeys(a.s.AssignableAt) {
		levels := a.s.AssignableAt[role]
		base := "assignable_at." + role
		if _, ok := a.s.Roles[role]; !ok {
			fail(base, "unknown role %q", role)
			continue
		}
		if a.reflected[role] {
			fail(base, "reflected roles are not assignable")
			continue
		}
		if len(levels) == 0 {
			fail(base, "at least one level is required")
		}
		seen := map[string]bool{}
		for i, l := range levels {
			lpath := fmt.Sprintf("%s[%d]", base, i)
			if levelRank(l) >= len(Levels) {
				fail(lpath, "unknown level %q (expected one of %s)", l, strings.Join(Levels, ", "))
				continue
			}
			if seen[l] {
				fail(lpath, "duplicate level %q", l)
				continue
			}
			seen[l] = true
			a.assignable[l][role] = true
			if w, ok := a.widest[role]; !ok || levelRank(l) < levelRank(w) {
				a.widest[role] = l
			}
		}
	}
	for _, role := range a.roles {
		if _, ok := a.widest[role]; !ok && !a.reflected[role] {
			fail("assignable_at."+role, "role %q must be assignable at some level or reflected from a property", role)
		}
	}
}

func (a *analysis) checkInherits(fail failFunc) {
	for _, name := range a.roles {
		r := a.s.Roles[name]
		base := "roles." + name + ".inherits"
		for i, inh := range r.Inherits {
			ipath := fmt.Sprintf("%s[%d]", base, i)
			if inh == name {
				fail(ipath, "role cannot inherit itself")
				continue
			}
			if _, ok := a.s.Roles[inh]; !ok {
				fail(ipath, "unknown role %q", inh)
				continue
			}
			if a.reflected[inh] {
				fail(ipath, "cannot inherit reflected role %q", inh)
				continue
			}
			w, wi := a.widest[name], a.widest[inh]
			if w != "" && wi != "" && levelRank(wi) > levelRank(w) {
				fail(ipath, "inherits %q, which is assignable at a narrower level (%s) than %q (%s)", inh, wi, name, w)
			}
		}
		if a.onInheritanceCycle(name) {
			fail(base, "inheritance cycle through %q", name)
		}
	}
}

// onInheritanceCycle reports whether following inherits from role leads back
// to it.
func (a *analysis) onInheritanceCycle(role string) bool {
	visited := map[string]bool{}
	var walk func(cur string) bool
	walk = func(cur string) bool {
		for _, inh := range a.s.Roles[cur].Inherits {
			if inh == role {
				return true
			}
			if !visited[inh] {
				visited[inh] = true
				if _, ok := a.s.Roles[inh]; ok && walk(inh) {
					return true
				}
			}
		}
		return false
	}
	return walk(role)
}

func (a *analysis) checkAdmin(fail failFunc) {
	admin, ok := a.s.Roles[PermissionAdmin]
	if !ok {
		fail("roles.admin", "scheme must keep an admin role granting \"*\" assignable at workspace")
		return
	}
	grantsAll := false
	for _, g := range admin.Grants {
		grantsAll = grantsAll || g == GrantAll
	}
	if !grantsAll {
		fail("roles.admin.grants", "admin role must grant \"*\"")
	}
	if !a.assignable[LevelWorkspace][PermissionAdmin] {
		fail("assignable_at.admin", "admin role must be assignable at workspace")
	}
}

func (a *analysis) checkExclusions(fail failFunc) {
	excludedBy := map[string]string{} // permission -> exclusion key
	for _, key := range sortedKeys(a.s.Exclusions) {
		perms := a.s.Exclusions[key]
		base := "exclusions." + key
		if key != ExclusionBlocked {
			if _, ok := a.s.Roles[key]; !ok {
				fail(base, "unknown exclusion %q (expected %q or a role name)", key, ExclusionBlocked)
				continue
			}
			if a.reflected[key] {
				fail(base, "reflected roles cannot be used as exclusions")
				continue
			}
		}
		if len(perms) == 0 {
			fail(base, "at least one permission or \"*\" is required")
			continue
		}
		var covered []string
		seen := map[string]bool{}
		for i, p := range perms {
			ppath := fmt.Sprintf("%s[%d]", base, i)
			if seen[p] {
				fail(ppath, "duplicate permission %q", p)
				continue
			}
			seen[p] = true
			if p != GrantAll && !a.listed[p] {
				fail(ppath, "permission %q is not listed in permissions", p)
				continue
			}
			covered = append(covered, p)
			for _, atom := range a.expandExclusion(p) {
				if other, dup := excludedBy[atom]; dup && other != key {
					fail(ppath, "permission %q is already excluded by %q; a permission may carry one exclusion", atom, other)
				} else {
					excludedBy[atom] = key
				}
			}
		}
		a.exclusions[key] = covered
	}
}

// expandExclusion lists the listed permissions an exclusion entry covers.
func (a *analysis) expandExclusion(p string) []string {
	if p != GrantAll {
		return []string{p}
	}
	out := append([]string(nil), a.s.Permissions...)
	sort.Strings(out)
	return out
}

// computeEffectiveGrants records every role's own grants and folds inherited
// grants into its effective set.
func (a *analysis) computeEffectiveGrants() {
	for _, name := range a.roles {
		own := map[string]bool{}
		for _, g := range a.s.Roles[name].Grants {
			own[g] = true
		}
		a.direct[name] = own
		set := map[string]bool{}
		visited := map[string]bool{}
		var walk func(cur string)
		walk = func(cur string) {
			if visited[cur] {
				return
			}
			visited[cur] = true
			for _, g := range a.s.Roles[cur].Grants {
				set[g] = true
			}
			for _, inh := range a.s.Roles[cur].Inherits {
				walk(inh)
			}
		}
		walk(name)
		a.eff[name] = set
	}
}

// covers reports whether a role's effective grants include a permission,
// honouring "*" and "set_property:*".
func (a *analysis) covers(role, permission string) bool {
	return coversWith(a.eff[role], permission)
}

// coversOwn is covers restricted to the grants the role declares itself.
func (a *analysis) coversOwn(role, permission string) bool {
	return coversWith(a.direct[role], permission)
}

func coversWith(set map[string]bool, permission string) bool {
	if set[GrantAll] || set[permission] {
		return true
	}
	if strings.HasPrefix(permission, PermissionSetPropertyPrefix) && permission != PermissionSetPropertyAll {
		return set[PermissionSetPropertyAll]
	}
	return false
}

// atoms lists every permission a role may be compared on: the listed
// permissions plus the property-scoped permissions named anywhere.
func (a *analysis) atoms() []string {
	out := append([]string(nil), a.s.Permissions...)
	for _, p := range a.properties {
		out = append(out, PermissionSetPropertyPrefix+p)
	}
	sort.Strings(out)
	return out
}

// implies reports whether the grants role x declares itself are a strict
// superset of everything role y confers (own and inherited), so that holders
// of x can be treated as holders of y without gaining anything. Inheritance
// is a separate, explicit relation (see inherits). implies is irreflexive
// and acyclic.
func (a *analysis) implies(x, y string) bool {
	if x == y {
		return false
	}
	superset, strict := true, false
	for _, atom := range a.atoms() {
		cx, cy := a.coversOwn(x, atom), a.covers(y, atom)
		if cy && !cx {
			superset = false
			break
		}
		if cx && !cy {
			strict = true
		}
	}
	return superset && strict
}

// inherits reports whether x inherits y directly or transitively.
func (a *analysis) inherits(x, y string) bool {
	visited := map[string]bool{}
	var walk func(cur string) bool
	walk = func(cur string) bool {
		for _, inh := range a.s.Roles[cur].Inherits {
			if inh == y {
				return true
			}
			if !visited[inh] {
				visited[inh] = true
				if walk(inh) {
					return true
				}
			}
		}
		return false
	}
	return walk(x)
}
