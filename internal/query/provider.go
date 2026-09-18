package query

import (
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"time"

	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
	"cel.dev/cel-go/common/types/traits"
)

// propsTypeName is the CEL object type of the props variable.
const propsTypeName = "kb.Props"

var propsObjectType = types.NewObjectType(propsTypeName)

// propsProvider is the type provider that types props.<name> from the
// property definitions of the schema (the DeclType pattern), so that
// props.due > 'x' is a check-time error. Everything else is delegated to an
// empty registry.
type propsProvider struct {
	base   *types.Registry
	ix     *schemaIndex
	fields map[string]*types.FieldType
	names  []string
}

func newPropsProvider(ix *schemaIndex) (*propsProvider, error) {
	base, err := types.NewRegistry()
	if err != nil {
		return nil, err
	}
	p := &propsProvider{base: base, ix: ix, fields: map[string]*types.FieldType{}}
	for _, ident := range ix.identifiers() {
		prop := ix.props[ident]
		name := ident
		p.fields[name] = &types.FieldType{
			Type: prop.Kind.celType(),
			IsSet: func(target any) bool {
				pv, ok := target.(*propsVal)
				return ok && pv.isSet(name)
			},
			GetFrom: func(target any) (any, error) {
				pv, ok := target.(*propsVal)
				if !ok {
					return nil, fmt.Errorf("props value of type %T", target)
				}
				return pv.get(name).Value(), nil
			},
		}
		p.names = append(p.names, name)
	}
	sort.Strings(p.names)
	return p, nil
}

// EnumValue implements types.Provider.
func (p *propsProvider) EnumValue(name string) ref.Val { return p.base.EnumValue(name) }

// FindIdent implements types.Provider.
func (p *propsProvider) FindIdent(name string) (ref.Val, bool) { return p.base.FindIdent(name) }

// FindStructType implements types.Provider.
func (p *propsProvider) FindStructType(name string) (*types.Type, bool) {
	if name == propsTypeName {
		return types.NewTypeTypeWithParam(propsObjectType), true
	}
	return p.base.FindStructType(name)
}

// FindStructFieldNames implements types.Provider.
func (p *propsProvider) FindStructFieldNames(name string) ([]string, bool) {
	if name == propsTypeName {
		return p.names, true
	}
	return p.base.FindStructFieldNames(name)
}

// FindStructFieldType implements types.Provider.
func (p *propsProvider) FindStructFieldType(structType, field string) (*types.FieldType, bool) {
	if structType == propsTypeName {
		f, ok := p.fields[field]
		return f, ok
	}
	return p.base.FindStructFieldType(structType, field)
}

// NewValue implements types.Provider; props cannot be constructed in an
// expression.
func (p *propsProvider) NewValue(structType string, fields map[string]ref.Val) ref.Val {
	if structType == propsTypeName {
		return types.NewErr("%s cannot be constructed", propsTypeName)
	}
	return p.base.NewValue(structType, fields)
}

// propsVal is the run-time value of props for one block. Values are keyed by
// CEL identifier and already converted to ref.Val.
type propsVal struct {
	ix   *schemaIndex
	vals map[string]ref.Val
}

// newPropsVal converts the Props map of a BlockInput (keyed by property name,
// with typed Go values) into a propsVal. select and multi_select values may
// be option ids or option names; ids are translated to names.
func newPropsVal(ix *schemaIndex, in map[string]any) (*propsVal, error) {
	pv := &propsVal{ix: ix, vals: map[string]ref.Val{}}
	for name, raw := range in {
		ident := identFor(name)
		prop, ok := ix.props[ident]
		if !ok || raw == nil {
			continue
		}
		v, err := convertPropValue(ix, prop, raw)
		if err != nil {
			return nil, fmt.Errorf("query: property %q: %w", name, err)
		}
		if v != nil {
			pv.vals[ident] = v
		}
	}
	return pv, nil
}

// convertPropValue converts one typed Go value into its CEL representation.
// It returns nil for empty lists, which count as absent.
func convertPropValue(ix *schemaIndex, p *Property, raw any) (ref.Val, error) {
	switch p.Kind {
	case KindText, KindURL, KindUser:
		s, ok := raw.(string)
		if !ok {
			return nil, fmt.Errorf("expected string, got %T", raw)
		}
		return types.String(s), nil
	case KindSelect:
		s, ok := raw.(string)
		if !ok {
			return nil, fmt.Errorf("expected string, got %T", raw)
		}
		return types.String(ix.optionName(p, s)), nil
	case KindNumber:
		f, err := toFloat(raw)
		if err != nil {
			return nil, err
		}
		return types.Double(f), nil
	case KindDate:
		switch t := raw.(type) {
		case time.Time:
			return types.Timestamp{Time: t}, nil
		case string:
			parsed, err := time.Parse(time.RFC3339Nano, t)
			if err != nil {
				return nil, err
			}
			return types.Timestamp{Time: parsed}, nil
		}
		return nil, fmt.Errorf("expected time.Time, got %T", raw)
	case KindCheckbox:
		b, ok := raw.(bool)
		if !ok {
			return nil, fmt.Errorf("expected bool, got %T", raw)
		}
		return types.Bool(b), nil
	case KindRelation, KindMultiSelect:
		list, err := toStringSlice(raw)
		if err != nil {
			return nil, err
		}
		if len(list) == 0 {
			return nil, nil
		}
		if p.Kind == KindMultiSelect {
			for i, v := range list {
				list[i] = ix.optionName(p, v)
			}
		}
		return types.NewStringList(types.DefaultTypeAdapter, list), nil
	}
	return nil, fmt.Errorf("unknown kind %q", p.Kind)
}

func toFloat(raw any) (float64, error) {
	switch n := raw.(type) {
	case float64:
		return n, nil
	case float32:
		return float64(n), nil
	case int:
		return float64(n), nil
	case int32:
		return float64(n), nil
	case int64:
		return float64(n), nil
	case string:
		return strconv.ParseFloat(n, 64)
	}
	return 0, fmt.Errorf("expected number, got %T", raw)
}

func toStringSlice(raw any) ([]string, error) {
	switch l := raw.(type) {
	case []string:
		return append([]string(nil), l...), nil
	case []any:
		out := make([]string, 0, len(l))
		for _, v := range l {
			s, ok := v.(string)
			if !ok {
				return nil, fmt.Errorf("expected list of strings, got element %T", v)
			}
			out = append(out, s)
		}
		return out, nil
	case string:
		return []string{l}, nil
	}
	return nil, fmt.Errorf("expected list of strings, got %T", raw)
}

// zeroVal is the CEL value of an absent property.
func zeroVal(p *Property) ref.Val {
	switch p.Kind {
	case KindNumber:
		return types.Double(0)
	case KindDate:
		return types.Timestamp{Time: time.Time{}}
	case KindCheckbox:
		return types.False
	case KindRelation, KindMultiSelect:
		return types.NewStringList(types.DefaultTypeAdapter, nil)
	default:
		return types.String("")
	}
}

func (pv *propsVal) isSet(ident string) bool {
	_, ok := pv.vals[ident]
	return ok
}

func (pv *propsVal) get(ident string) ref.Val {
	if v, ok := pv.vals[ident]; ok {
		return v
	}
	if p, ok := pv.ix.props[ident]; ok {
		return zeroVal(p)
	}
	return types.NewErr("no such property: %s", ident)
}

// ConvertToNative implements ref.Val.
func (pv *propsVal) ConvertToNative(t reflect.Type) (any, error) {
	return nil, fmt.Errorf("%s cannot be converted to %v", propsTypeName, t)
}

// ConvertToType implements ref.Val.
func (pv *propsVal) ConvertToType(t ref.Type) ref.Val {
	if t == types.TypeType {
		return propsObjectType
	}
	return types.NewErr("type conversion error from %s to %s", propsTypeName, t.TypeName())
}

// Equal implements ref.Val.
func (pv *propsVal) Equal(other ref.Val) ref.Val {
	o, ok := other.(*propsVal)
	return types.Bool(ok && o == pv)
}

// Type implements ref.Val.
func (pv *propsVal) Type() ref.Type { return propsObjectType }

// Value implements ref.Val.
func (pv *propsVal) Value() any {
	out := map[string]any{}
	for k, v := range pv.vals {
		out[k] = v.Value()
	}
	return out
}

// Get implements traits.Indexer (field selection).
func (pv *propsVal) Get(index ref.Val) ref.Val {
	s, ok := index.(types.String)
	if !ok {
		return types.NewErr("props field must be a string, got %s", index.Type().TypeName())
	}
	return pv.get(string(s))
}

// IsSet implements traits.FieldTester (has(props.x)).
func (pv *propsVal) IsSet(field ref.Val) ref.Val {
	s, ok := field.(types.String)
	if !ok {
		return types.NewErr("props field must be a string, got %s", field.Type().TypeName())
	}
	return types.Bool(pv.isSet(string(s)))
}

var (
	_ ref.Val            = (*propsVal)(nil)
	_ traits.Indexer     = (*propsVal)(nil)
	_ traits.FieldTester = (*propsVal)(nil)
)
