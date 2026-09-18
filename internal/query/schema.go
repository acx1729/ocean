package query

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"cel.dev/cel-go/cel"
)

// PropertyKind is the storage kind of a property definition.
type PropertyKind string

// Property kinds of the specification (property_definitions.kind).
const (
	KindText        PropertyKind = "text"
	KindNumber      PropertyKind = "number"
	KindDate        PropertyKind = "date"
	KindSelect      PropertyKind = "select"
	KindMultiSelect PropertyKind = "multi_select"
	KindRelation    PropertyKind = "relation"
	KindUser        PropertyKind = "user"
	KindCheckbox    PropertyKind = "checkbox"
	KindURL         PropertyKind = "url"
)

// Option is one value of a select or multi_select property.
type Option struct {
	ID   string
	Name string
}

// Property is a property definition visible to the query environment.
type Property struct {
	ID             string
	Name           string
	Kind           PropertyKind
	ProjectID      string // "" = workspace-wide definition
	Options        []Option
	RelationTypeID string
}

// Type is a block type; ExtendsTypeID links it to its parent type.
type Type struct {
	ID            string
	Name          string
	ExtendsTypeID string
}

// RelationType is a named edge type between blocks.
type RelationType struct {
	ID   string
	Name string
}

// Schema describes everything the environment of one project needs. Version
// changes whenever a definition changes; cursors and cached plans are keyed
// by it.
type Schema struct {
	ProjectID     string // "" = workspace-wide environment
	Version       int64
	Properties    []Property
	Types         []Type
	RelationTypes []RelationType
	PageTypeID    string
}

// multi reports whether the kind stores one row per value.
func (k PropertyKind) multi() bool { return k == KindRelation || k == KindMultiSelect }

// valid reports whether the kind is one of the known kinds.
func (k PropertyKind) valid() bool {
	switch k {
	case KindText, KindNumber, KindDate, KindSelect, KindMultiSelect, KindRelation, KindUser, KindCheckbox, KindURL:
		return true
	}
	return false
}

// column is the typed block_properties column holding values of the kind.
func (k PropertyKind) column() string {
	switch k {
	case KindNumber:
		return "value_number"
	case KindDate:
		return "value_date"
	case KindCheckbox:
		return "value_bool"
	case KindRelation:
		return "value_ref"
	case KindUser:
		return "value_principal"
	default:
		return "value_text"
	}
}

// celType is the CEL type of props.<name> for the kind.
func (k PropertyKind) celType() *cel.Type {
	switch k {
	case KindNumber:
		return cel.DoubleType
	case KindDate:
		return cel.TimestampType
	case KindCheckbox:
		return cel.BoolType
	case KindRelation, KindMultiSelect:
		return cel.ListType(cel.StringType)
	default:
		return cel.StringType
	}
}

// sqlType is the SQL type of one value of the kind.
func (k PropertyKind) sqlType() sqlType {
	switch k {
	case KindNumber:
		return tNumeric
	case KindDate:
		return tTimestamp
	case KindCheckbox:
		return tBool
	case KindRelation:
		return tUUID
	default:
		return tText
	}
}

// zero is the CEL value of an absent single-valued property of the kind.
func (k PropertyKind) zero() any {
	switch k {
	case KindNumber:
		return float64(0)
	case KindDate:
		return time.Time{}
	case KindCheckbox:
		return false
	default:
		return ""
	}
}

// schemaIndex is the resolved, lookup-friendly form of a Schema.
type schemaIndex struct {
	schema    Schema
	props     map[string]*Property // by CEL identifier
	ambiguous map[string]bool      // identifiers claimed by several definitions
	propsByID map[string]*Property
	types     map[string][]*Type
	typesByID map[string]*Type
	children  map[string][]string // type id -> ids of direct subtypes
	relations map[string][]*RelationType
}

func newSchemaIndex(s Schema) (*schemaIndex, error) {
	ix := &schemaIndex{
		schema:    s,
		props:     map[string]*Property{},
		ambiguous: map[string]bool{},
		propsByID: map[string]*Property{},
		types:     map[string][]*Type{},
		typesByID: map[string]*Type{},
		children:  map[string][]string{},
		relations: map[string][]*RelationType{},
	}
	byIdent := map[string][]*Property{}
	for i := range s.Properties {
		p := &s.Properties[i]
		if !p.Kind.valid() {
			return nil, fmt.Errorf("query: property %q has unknown kind %q", p.Name, p.Kind)
		}
		if p.ProjectID != "" && s.ProjectID != "" && p.ProjectID != s.ProjectID {
			continue // belongs to another project
		}
		ix.propsByID[p.ID] = p
		id := identFor(p.Name)
		byIdent[id] = append(byIdent[id], p)
	}
	for id, cands := range byIdent {
		var scoped, global []*Property
		for _, p := range cands {
			if p.ProjectID != "" {
				scoped = append(scoped, p)
			} else {
				global = append(global, p)
			}
		}
		switch {
		case len(scoped) == 1:
			ix.props[id] = scoped[0] // project definitions shadow workspace-wide ones
		case len(scoped) > 1:
			ix.ambiguous[id] = true
		case len(global) == 1:
			ix.props[id] = global[0]
		default:
			ix.ambiguous[id] = true
		}
	}
	for i := range s.Types {
		t := &s.Types[i]
		ix.types[t.Name] = append(ix.types[t.Name], t)
		ix.typesByID[t.ID] = t
		if t.ExtendsTypeID != "" {
			ix.children[t.ExtendsTypeID] = append(ix.children[t.ExtendsTypeID], t.ID)
		}
	}
	for i := range s.RelationTypes {
		r := &s.RelationTypes[i]
		ix.relations[r.Name] = append(ix.relations[r.Name], r)
	}
	return ix, nil
}

// identFor maps a property name to the CEL identifier used after "props.".
// Names that are valid identifiers are used verbatim; otherwise every run of
// characters outside [A-Za-z0-9_] becomes "_" and a leading digit is
// prefixed with "_".
func identFor(name string) string {
	var b strings.Builder
	prevUnderscore := false
	for i, r := range name {
		ok := r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		if ok {
			if i == 0 && r >= '0' && r <= '9' {
				b.WriteByte('_')
			}
			b.WriteRune(r)
			prevUnderscore = false
			continue
		}
		if !prevUnderscore && b.Len() > 0 {
			b.WriteByte('_')
			prevUnderscore = true
		}
	}
	out := strings.TrimRight(b.String(), "_")
	if out == "" {
		return "_"
	}
	return out
}

// identifiers returns the addressable property identifiers, sorted.
func (ix *schemaIndex) identifiers() []string {
	out := make([]string, 0, len(ix.props))
	for id := range ix.props {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// property resolves props.<ident>.
func (ix *schemaIndex) property(ident string) (*Property, error) {
	if ix.ambiguous[ident] {
		return nil, fmt.Errorf("property %q is ambiguous in this scope", ident)
	}
	p, ok := ix.props[ident]
	if !ok {
		return nil, fmt.Errorf("unknown property %q", ident)
	}
	return p, nil
}

// optionID maps an option name of a select/multi_select property to its id.
func (ix *schemaIndex) optionID(p *Property, name string) (string, bool) {
	for _, o := range p.Options {
		if o.Name == name {
			return o.ID, true
		}
	}
	return "", false
}

// optionName maps an option id (or name) to the option name; unknown values
// are returned unchanged.
func (ix *schemaIndex) optionName(p *Property, v string) string {
	for _, o := range p.Options {
		if o.ID == v {
			return o.Name
		}
	}
	return v
}

// optionIDs lists the option ids of a property in schema order.
func (ix *schemaIndex) optionIDs(p *Property) []string {
	out := make([]string, len(p.Options))
	for i, o := range p.Options {
		out[i] = o.ID
	}
	return out
}

// typeByName resolves a type name to its definition.
func (ix *schemaIndex) typeByName(name string) (*Type, error) {
	ts := ix.types[name]
	switch len(ts) {
	case 0:
		return nil, fmt.Errorf("unknown type %q", name)
	case 1:
		return ts[0], nil
	default:
		return nil, fmt.Errorf("type %q is ambiguous", name)
	}
}

// typeName returns the name of a type id, or "" when unknown.
func (ix *schemaIndex) typeName(id string) string {
	if t, ok := ix.typesByID[id]; ok {
		return t.Name
	}
	return ""
}

// closure returns the id of the named type and of every type that
// transitively extends it.
func (ix *schemaIndex) closure(name string) ([]string, error) {
	root, err := ix.typeByName(name)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{root.ID: true}
	out := []string{root.ID}
	for i := 0; i < len(out); i++ {
		for _, c := range ix.children[out[i]] {
			if !seen[c] {
				seen[c] = true
				out = append(out, c)
			}
		}
	}
	return out, nil
}

// relation resolves a relation type name.
func (ix *schemaIndex) relation(name string) (*RelationType, error) {
	rs := ix.relations[name]
	switch len(rs) {
	case 0:
		return nil, fmt.Errorf("unknown relation type %q", name)
	case 1:
		return rs[0], nil
	default:
		return nil, fmt.Errorf("relation type %q is ambiguous", name)
	}
}
