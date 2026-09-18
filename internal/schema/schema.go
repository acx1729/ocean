// Package schema manages user-defined block types, property definitions and
// relation types (specification section 3, "Schema"), the shipped defaults
// every project starts with, and property validation for SetType and
// SetProperties.
package schema

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

// PropertyKind enumerates value kinds.
type PropertyKind string

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

// ValidKind reports whether k is a known property kind.
func ValidKind(k PropertyKind) bool {
	switch k {
	case KindText, KindNumber, KindDate, KindSelect, KindMultiSelect, KindRelation, KindUser, KindCheckbox, KindURL:
		return true
	}
	return false
}

// Reserved property keys live in the bag without a definition.
const (
	KeyProp = "key" // KB-123 for numbered types
)

// Option is one choice of a select property.
type Option struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Property is a property definition.
type Property struct {
	ID          string
	WorkspaceID string
	ProjectID   string // "" = workspace-wide
	Name        string
	Kind        PropertyKind
	Config      map[string]any
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Options returns the select options from Config.
func (p *Property) Options() []Option {
	raw, _ := p.Config["options"].([]any)
	out := make([]Option, 0, len(raw))
	for _, r := range raw {
		m, ok := r.(map[string]any)
		if !ok {
			continue
		}
		id, _ := m["id"].(string)
		name, _ := m["name"].(string)
		if id != "" {
			out = append(out, Option{ID: id, Name: name})
		}
	}
	return out
}

// RelationTypeID returns the relation type a relation property points through.
func (p *Property) RelationTypeID() string {
	s, _ := p.Config["relation_type_id"].(string)
	return s
}

// Type is a block type.
type Type struct {
	ID                string
	WorkspaceID       string
	ProjectID         string
	Name              string
	ExtendsTypeID     string
	CollectionCapable bool
	OwnsDoc           bool
	Numbered          bool
	RequiredProps     []string
	AllowedProps      []string // nil = any
	Defaults          map[string]any
	Icon              string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// RelationType names a directed relation between blocks.
type RelationType struct {
	ID          string
	WorkspaceID string
	ProjectID   string
	Name        string
	InverseName string
	Symmetric   bool
	DAG         bool
	SourceTypes []string
	TargetTypes []string
	CreatedAt   time.Time
}

// Reserved type names.
const (
	TypePage = "page"
	TypeView = "view"
	TypeWork = "work"
)

var nameRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

// ValidName reports whether a type, property or relation name is acceptable.
func ValidName(s string) bool { return nameRe.MatchString(s) }

// Snapshot is the resolved schema of one project (project definitions plus
// workspace-wide properties), used for validation and query compilation.
type Snapshot struct {
	ProjectID     string
	Version       int64
	Types         map[string]*Type
	TypesByName   map[string]*Type
	Props         map[string]*Property
	PropsByName   map[string]*Property
	Relations     map[string]*RelationType
	RelationNames map[string]*RelationType
}

// Type resolves a type by id or name.
func (s *Snapshot) Type(idOrName string) (*Type, bool) {
	if t, ok := s.Types[idOrName]; ok {
		return t, true
	}
	t, ok := s.TypesByName[idOrName]
	return t, ok
}

// Closure returns the type and its ancestors (nearest first); cycles stop.
func (s *Snapshot) Closure(typeID string) []*Type {
	var out []*Type
	seen := map[string]bool{}
	for id := typeID; id != "" && !seen[id]; {
		seen[id] = true
		t, ok := s.Types[id]
		if !ok {
			break
		}
		out = append(out, t)
		id = t.ExtendsTypeID
	}
	return out
}

// IsA reports whether typeID extends (or is) the type named ancestor.
func (s *Snapshot) IsA(typeID, ancestor string) bool {
	for _, t := range s.Closure(typeID) {
		if t.Name == ancestor || t.ID == ancestor {
			return true
		}
	}
	return false
}

// Descendants returns every type id whose closure contains typeID (inclusive).
func (s *Snapshot) Descendants(typeID string) []string {
	var out []string
	for id := range s.Types {
		for _, t := range s.Closure(id) {
			if t.ID == typeID {
				out = append(out, id)
				break
			}
		}
	}
	return out
}

// OwnsDoc and Numbered follow the closure: a subtype of work owns a doc.
func (s *Snapshot) OwnsDoc(typeID string) bool {
	for _, t := range s.Closure(typeID) {
		if t.OwnsDoc {
			return true
		}
	}
	return false
}

// Numbered reports whether blocks of the type receive keys.
func (s *Snapshot) Numbered(typeID string) bool {
	for _, t := range s.Closure(typeID) {
		if t.Numbered {
			return true
		}
	}
	return false
}

// Violation lists what a property bag gets wrong for a type.
type Violation struct {
	Missing []string
	Invalid map[string]string
}

// Empty reports no violations.
func (v *Violation) Empty() bool { return len(v.Missing) == 0 && len(v.Invalid) == 0 }

// ErrInvalid marks validation failures.
var ErrInvalid = errors.New("schema: invalid")

// Validate checks props (keyed by property id) against the type: required
// properties present, only allowed properties set, values of the right kind.
func (s *Snapshot) Validate(typeID string, props map[string]any) *Violation {
	v := &Violation{Invalid: map[string]string{}}
	closure := s.Closure(typeID)
	allowed := map[string]bool{}
	anyAllowed := len(closure) == 0
	for _, t := range closure {
		if t.AllowedProps == nil {
			anyAllowed = true
		}
		for _, id := range t.AllowedProps {
			allowed[id] = true
		}
		for _, id := range t.RequiredProps {
			allowed[id] = true
			if val, ok := props[id]; !ok || val == nil || val == "" {
				v.Missing = append(v.Missing, id)
			}
		}
	}
	for id, val := range props {
		if id == KeyProp {
			continue
		}
		p, ok := s.Props[id]
		if !ok {
			v.Invalid[id] = "unknown property"
			continue
		}
		if !anyAllowed && !allowed[id] {
			v.Invalid[id] = "not allowed by type"
			continue
		}
		if val == nil {
			continue
		}
		if reason := CheckValue(p, val); reason != "" {
			v.Invalid[id] = reason
		}
	}
	return v
}

// ApplyDefaults fills type defaults (nearest type wins) for missing keys.
func (s *Snapshot) ApplyDefaults(typeID string, props map[string]any) map[string]any {
	out := map[string]any{}
	for k, val := range props {
		out[k] = val
	}
	closure := s.Closure(typeID)
	for i := len(closure) - 1; i >= 0; i-- {
		for k, val := range closure[i].Defaults {
			if _, ok := out[k]; !ok {
				out[k] = val
			}
		}
	}
	return out
}

// CheckValue reports why val is not acceptable for p ("" when it is).
// Select values may be given as option id or name; the caller normalizes.
func CheckValue(p *Property, val any) string {
	switch p.Kind {
	case KindText:
		if _, ok := val.(string); !ok {
			return "expected a string"
		}
	case KindURL:
		s, ok := val.(string)
		if !ok {
			return "expected a string"
		}
		u, err := url.Parse(s)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			return "expected an http(s) URL"
		}
	case KindNumber:
		switch val.(type) {
		case float64, float32, int, int64, int32:
		default:
			return "expected a number"
		}
	case KindCheckbox:
		if _, ok := val.(bool); !ok {
			return "expected a boolean"
		}
	case KindDate:
		s, ok := val.(string)
		if !ok {
			return "expected an RFC 3339 timestamp or YYYY-MM-DD date"
		}
		if _, err := ParseDate(s); err != nil {
			return "expected an RFC 3339 timestamp or YYYY-MM-DD date"
		}
	case KindUser:
		s, ok := val.(string)
		if !ok || !strings.HasPrefix(s, "did:") {
			return "expected a DID"
		}
	case KindSelect:
		s, ok := val.(string)
		if !ok {
			return "expected an option"
		}
		if OptionID(p, s) == "" {
			return fmt.Sprintf("unknown option %q", s)
		}
	case KindMultiSelect:
		items, ok := val.([]any)
		if !ok {
			if ss, ok2 := val.([]string); ok2 {
				items = make([]any, len(ss))
				for i, x := range ss {
					items[i] = x
				}
			} else {
				return "expected a list of options"
			}
		}
		for _, it := range items {
			s, ok := it.(string)
			if !ok || OptionID(p, s) == "" {
				return fmt.Sprintf("unknown option %v", it)
			}
		}
	case KindRelation:
		items, ok := val.([]any)
		if !ok {
			if ss, ok2 := val.([]string); ok2 {
				items = make([]any, len(ss))
				for i, x := range ss {
					items[i] = x
				}
			} else if s, ok3 := val.(string); ok3 {
				items = []any{s}
			} else {
				return "expected a list of block ids"
			}
		}
		for _, it := range items {
			s, ok := it.(string)
			if !ok {
				return "expected block ids"
			}
			if _, err := uuid.Parse(s); err != nil {
				return fmt.Sprintf("%q is not a block id", s)
			}
		}
	}
	return ""
}

// OptionID resolves a select value given as id or name to the option id.
func OptionID(p *Property, idOrName string) string {
	for _, o := range p.Options() {
		if o.ID == idOrName {
			return o.ID
		}
	}
	for _, o := range p.Options() {
		if strings.EqualFold(o.Name, idOrName) {
			return o.ID
		}
	}
	return ""
}

// Normalize canonicalizes values: select/multi_select names become option
// ids, relation values become string lists, dates keep their string form.
func Normalize(p *Property, val any) any {
	switch p.Kind {
	case KindSelect:
		if s, ok := val.(string); ok {
			if id := OptionID(p, s); id != "" {
				return id
			}
		}
	case KindMultiSelect:
		items, ok := val.([]any)
		if !ok {
			if ss, ok2 := val.([]string); ok2 {
				items = make([]any, len(ss))
				for i, x := range ss {
					items[i] = x
				}
			}
		}
		out := map[string]any{} // LoroMap<option_id, true> shape for concurrent adds
		for _, it := range items {
			if s, ok := it.(string); ok {
				if id := OptionID(p, s); id != "" {
					out[id] = true
				}
			}
		}
		return out
	case KindRelation:
		var items []any
		switch x := val.(type) {
		case []any:
			items = x
		case []string:
			for _, s := range x {
				items = append(items, s)
			}
		case string:
			items = []any{x}
		}
		out := map[string]any{}
		for _, it := range items {
			if s, ok := it.(string); ok {
				out[s] = true
			}
		}
		return out
	}
	return val
}

// ParseDate accepts RFC 3339 timestamps and plain dates.
func ParseDate(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("%w: bad date %q", ErrInvalid, s)
}
