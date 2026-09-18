package api

import (
	"context"
	"strings"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	kbv1 "github.com/acx1729/ocean/gen/kb/v1"
	"github.com/acx1729/ocean/gen/kb/v1/kbv1connect"
	"github.com/acx1729/ocean/internal/apierr"
	"github.com/acx1729/ocean/internal/authz"
	"github.com/acx1729/ocean/internal/schema"
	"github.com/acx1729/ocean/internal/truth"
)

// SchemaService implements kb.v1.SchemaService.
type SchemaService struct{ *core }

var _ kbv1connect.SchemaServiceHandler = (*SchemaService)(nil)

// truthEvent builds a workspace-level outbox event.
func truthEvent(kind, ws, actor string, payload map[string]any) *truth.Event {
	return &truth.Event{Kind: kind, WorkspaceID: ws, Actor: actor, Payload: payload}
}

func typeProto(t *schema.Type) *kbv1.BlockType {
	out := &kbv1.BlockType{
		Id: t.ID, ProjectId: t.ProjectID, Name: t.Name, ExtendsTypeId: t.ExtendsTypeID, CollectionCapable: t.CollectionCapable,
		OwnsDoc: t.OwnsDoc, Numbered: t.Numbered, RequiredProps: t.RequiredProps, AllowedProps: t.AllowedProps, AllowAnyProps: t.AllowedProps == nil,
		Defaults: toStruct(t.Defaults), Icon: t.Icon, CreatedAt: ts(t.CreatedAt), UpdatedAt: ts(t.UpdatedAt),
	}
	if out.AllowedProps == nil {
		out.AllowedProps = []string{}
	}
	return out
}

func propertyProto(p *schema.Property) *kbv1.PropertyDefinition {
	return &kbv1.PropertyDefinition{Id: p.ID, ProjectId: p.ProjectID, Name: p.Name, Kind: kindToProto(p.Kind), Config: toStruct(p.Config), CreatedAt: ts(p.CreatedAt), UpdatedAt: ts(p.UpdatedAt)}
}

func relationProto(r *schema.RelationType) *kbv1.RelationType {
	return &kbv1.RelationType{Id: r.ID, ProjectId: r.ProjectID, Name: r.Name, InverseName: r.InverseName, Symmetric: r.Symmetric, Dag: r.DAG, SourceTypes: r.SourceTypes, TargetTypes: r.TargetTypes}
}

var kindNames = map[kbv1.PropertyKind]schema.PropertyKind{
	kbv1.PropertyKind_PROPERTY_KIND_TEXT: schema.KindText, kbv1.PropertyKind_PROPERTY_KIND_NUMBER: schema.KindNumber,
	kbv1.PropertyKind_PROPERTY_KIND_DATE: schema.KindDate, kbv1.PropertyKind_PROPERTY_KIND_SELECT: schema.KindSelect,
	kbv1.PropertyKind_PROPERTY_KIND_MULTI_SELECT: schema.KindMultiSelect, kbv1.PropertyKind_PROPERTY_KIND_RELATION: schema.KindRelation,
	kbv1.PropertyKind_PROPERTY_KIND_USER: schema.KindUser, kbv1.PropertyKind_PROPERTY_KIND_CHECKBOX: schema.KindCheckbox,
	kbv1.PropertyKind_PROPERTY_KIND_URL: schema.KindURL,
}

func kindToProto(k schema.PropertyKind) kbv1.PropertyKind {
	for pk, sk := range kindNames {
		if sk == k {
			return pk
		}
	}
	return kbv1.PropertyKind_PROPERTY_KIND_UNSPECIFIED
}

// CreateType adds a type to a project.
func (s *SchemaService) CreateType(ctx context.Context, req *connect.Request[kbv1.CreateTypeRequest]) (*connect.Response[kbv1.CreateTypeResponse], error) {
	id, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	ws, pid := req.Msg.GetWorkspaceId(), req.Msg.GetProjectId()
	if err := s.check(ctx, id, ws, authz.ManageSchema, authz.Project(pid)); err != nil {
		return nil, err
	}
	in := req.Msg.GetType()
	if in == nil || !schema.ValidName(in.GetName()) {
		return nil, apierr.InvalidArgument("type.name", "lowercase identifier of at most 63 characters")
	}
	if in.GetName() == schema.TypePage || in.GetName() == schema.TypeView {
		return nil, apierr.InvalidArgument("type.name", "reserved")
	}
	t := &schema.Type{WorkspaceID: ws, ProjectID: pid, Name: in.GetName(), ExtendsTypeID: in.GetExtendsTypeId(), CollectionCapable: in.GetCollectionCapable(),
		OwnsDoc: in.GetOwnsDoc(), Numbered: in.GetNumbered(), RequiredProps: in.GetRequiredProps(), Defaults: fromStruct(in.GetDefaults()), Icon: in.GetIcon()}
	if !in.GetAllowAnyProps() && (len(in.GetAllowedProps()) > 0 || len(in.GetRequiredProps()) > 0) {
		t.AllowedProps = in.GetAllowedProps()
	}
	var out *kbv1.BlockType
	err = s.DB.Tx(ctx, tenant(id, ws), func(ctx context.Context, tx pgx.Tx) error {
		snap, err := s.Schema.Load(ctx, tx, ws, pid)
		if err != nil {
			return err
		}
		if t.ExtendsTypeID != "" {
			if _, ok := snap.Types[t.ExtendsTypeID]; !ok {
				return apierr.InvalidArgument("type.extends_type_id", "unknown type")
			}
		}
		for _, p := range append(append([]string{}, t.RequiredProps...), t.AllowedProps...) {
			if _, ok := snap.Props[p]; !ok {
				return apierr.InvalidArgument("type.required_props", "unknown property "+p)
			}
		}
		if err := s.Schema.CreateType(ctx, tx, t); err != nil {
			if schema.IsUnique(err) {
				return apierr.AlreadyExists("type " + t.Name)
			}
			return err
		}
		out = typeProto(t)
		s.audit(ctx, tx, id, ws, "schema.create_type", "type", t.ID, map[string]any{"name": t.Name})
		return nil
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return connect.NewResponse(&kbv1.CreateTypeResponse{Type: out}), nil
}

// UpdateType changes a type's fields named in the mask.
func (s *SchemaService) UpdateType(ctx context.Context, req *connect.Request[kbv1.UpdateTypeRequest]) (*connect.Response[kbv1.UpdateTypeResponse], error) {
	id, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	ws := req.Msg.GetWorkspaceId()
	in := req.Msg.GetType()
	if in == nil {
		return nil, apierr.InvalidArgument("type", "required")
	}
	var out *kbv1.BlockType
	err = s.DB.Tx(ctx, tenant(id, ws), func(ctx context.Context, tx pgx.Tx) error {
		t, err := s.Schema.GetType(ctx, tx, ws, req.Msg.GetTypeId())
		if err != nil {
			return err
		}
		if err := s.check(ctx, id, ws, authz.ManageSchema, authz.Project(t.ProjectID)); err != nil {
			return err
		}
		if t.Name == schema.TypePage || t.Name == schema.TypeView {
			return apierr.FailedPrecondition("reserved types cannot be changed")
		}
		for _, p := range maskPaths(req.Msg.GetUpdateMask(), "name", "required_props", "allowed_props", "defaults", "icon", "collection_capable") {
			switch p {
			case "name":
				if !schema.ValidName(in.GetName()) {
					return apierr.InvalidArgument("type.name", "invalid")
				}
				t.Name = in.GetName()
			case "required_props":
				t.RequiredProps = in.GetRequiredProps()
			case "allowed_props":
				if in.GetAllowAnyProps() {
					t.AllowedProps = nil
				} else {
					t.AllowedProps = in.GetAllowedProps()
				}
			case "defaults":
				t.Defaults = fromStruct(in.GetDefaults())
			case "icon":
				t.Icon = in.GetIcon()
			case "collection_capable":
				t.CollectionCapable = in.GetCollectionCapable()
			case "extends_type_id":
				t.ExtendsTypeID = in.GetExtendsTypeId()
			case "owns_doc", "numbered":
				return apierr.FailedPrecondition("owns_doc and numbered cannot change after creation")
			default:
				return apierr.InvalidArgument("update_mask", "unknown path "+p)
			}
		}
		if err := s.Schema.UpdateType(ctx, tx, t); err != nil {
			return err
		}
		out = typeProto(t)
		s.audit(ctx, tx, id, ws, "schema.update_type", "type", t.ID, nil)
		return nil
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return connect.NewResponse(&kbv1.UpdateTypeResponse{Type: out}), nil
}

// ListTypes lists the types of a project.
func (s *SchemaService) ListTypes(ctx context.Context, req *connect.Request[kbv1.ListTypesRequest]) (*connect.Response[kbv1.ListTypesResponse], error) {
	id, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	ws, pid := req.Msg.GetWorkspaceId(), req.Msg.GetProjectId()
	if err := s.check(ctx, id, ws, authz.View, authz.Project(pid)); err != nil {
		return nil, err
	}
	out := &kbv1.ListTypesResponse{}
	err = s.DB.ReadTx(ctx, tenant(id, ws), func(ctx context.Context, tx pgx.Tx) error {
		types, err := s.Schema.ListTypes(ctx, tx, ws, pid)
		for _, t := range types {
			out.Types = append(out.Types, typeProto(t))
		}
		return err
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return connect.NewResponse(out), nil
}

// DeleteType removes an unused type.
func (s *SchemaService) DeleteType(ctx context.Context, req *connect.Request[kbv1.DeleteTypeRequest]) (*connect.Response[kbv1.DeleteTypeResponse], error) {
	id, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	ws := req.Msg.GetWorkspaceId()
	err = s.DB.Tx(ctx, tenant(id, ws), func(ctx context.Context, tx pgx.Tx) error {
		t, err := s.Schema.GetType(ctx, tx, ws, req.Msg.GetTypeId())
		if err != nil {
			return err
		}
		if err := s.check(ctx, id, ws, authz.ManageSchema, authz.Project(t.ProjectID)); err != nil {
			return err
		}
		if t.Name == schema.TypePage || t.Name == schema.TypeView || t.Name == schema.TypeWork {
			return apierr.FailedPrecondition("shipped base types cannot be deleted")
		}
		if err := s.Schema.DeleteType(ctx, tx, ws, t.ID); err != nil {
			return err
		}
		s.audit(ctx, tx, id, ws, "schema.delete_type", "type", t.ID, nil)
		return nil
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return connect.NewResponse(&kbv1.DeleteTypeResponse{}), nil
}

// CreateProperty adds a property definition (project or workspace-wide).
func (s *SchemaService) CreateProperty(ctx context.Context, req *connect.Request[kbv1.CreatePropertyRequest]) (*connect.Response[kbv1.CreatePropertyResponse], error) {
	id, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	ws, pid := req.Msg.GetWorkspaceId(), req.Msg.GetProjectId()
	res := authz.Workspace(ws)
	if pid != "" {
		res = authz.Project(pid)
	}
	if err := s.check(ctx, id, ws, authz.ManageSchema, res); err != nil {
		return nil, err
	}
	in := req.Msg.GetProperty()
	if in == nil || !schema.ValidName(in.GetName()) || in.GetName() == schema.KeyProp {
		return nil, apierr.InvalidArgument("property.name", "lowercase identifier of at most 63 characters")
	}
	kind, ok := kindNames[in.GetKind()]
	if !ok {
		return nil, apierr.InvalidArgument("property.kind", "required")
	}
	p := &schema.Property{WorkspaceID: ws, ProjectID: pid, Name: in.GetName(), Kind: kind, Config: fromStruct(in.GetConfig())}
	if err := validatePropertyConfig(p); err != nil {
		return nil, err
	}
	var out *kbv1.PropertyDefinition
	err = s.DB.Tx(ctx, tenant(id, ws), func(ctx context.Context, tx pgx.Tx) error {
		if err := s.Schema.CreateProperty(ctx, tx, p); err != nil {
			if schema.IsUnique(err) {
				return apierr.AlreadyExists("property " + p.Name)
			}
			return err
		}
		out = propertyProto(p)
		s.audit(ctx, tx, id, ws, "schema.create_property", "property", p.ID, map[string]any{"name": p.Name, "kind": string(kind)})
		return nil
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return connect.NewResponse(&kbv1.CreatePropertyResponse{Property: out}), nil
}

// validatePropertyConfig checks select options and relation targets.
func validatePropertyConfig(p *schema.Property) error {
	switch p.Kind {
	case schema.KindSelect, schema.KindMultiSelect:
		seen := map[string]bool{}
		for _, o := range p.Options() {
			if o.ID == "" || seen[o.ID] {
				return apierr.InvalidArgument("property.config.options", "option ids must be unique and non-empty")
			}
			seen[o.ID] = true
		}
	}
	return nil
}

// UpdateProperty renames a property or changes its options; the kind is immutable.
func (s *SchemaService) UpdateProperty(ctx context.Context, req *connect.Request[kbv1.UpdatePropertyRequest]) (*connect.Response[kbv1.UpdatePropertyResponse], error) {
	id, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	ws := req.Msg.GetWorkspaceId()
	in := req.Msg.GetProperty()
	if in == nil {
		return nil, apierr.InvalidArgument("property", "required")
	}
	var out *kbv1.PropertyDefinition
	err = s.DB.Tx(ctx, tenant(id, ws), func(ctx context.Context, tx pgx.Tx) error {
		p, err := s.Schema.GetProperty(ctx, tx, ws, req.Msg.GetPropertyId())
		if err != nil {
			return err
		}
		res := authz.Workspace(ws)
		if p.ProjectID != "" {
			res = authz.Project(p.ProjectID)
		}
		if err := s.check(ctx, id, ws, authz.ManageSchema, res); err != nil {
			return err
		}
		for _, path := range maskPaths(req.Msg.GetUpdateMask(), "name", "config") {
			switch path {
			case "name":
				if !schema.ValidName(in.GetName()) {
					return apierr.InvalidArgument("property.name", "invalid")
				}
				p.Name = in.GetName()
			case "config":
				p.Config = fromStruct(in.GetConfig())
				if err := validatePropertyConfig(p); err != nil {
					return err
				}
			case "kind":
				return apierr.FailedPrecondition("a property's kind cannot change")
			default:
				return apierr.InvalidArgument("update_mask", "unknown path "+path)
			}
		}
		if err := s.Schema.UpdateProperty(ctx, tx, p); err != nil {
			return err
		}
		out = propertyProto(p)
		s.audit(ctx, tx, id, ws, "schema.update_property", "property", p.ID, nil)
		return nil
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return connect.NewResponse(&kbv1.UpdatePropertyResponse{Property: out}), nil
}

// ListProperties lists project plus workspace-wide definitions.
func (s *SchemaService) ListProperties(ctx context.Context, req *connect.Request[kbv1.ListPropertiesRequest]) (*connect.Response[kbv1.ListPropertiesResponse], error) {
	id, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	ws, pid := req.Msg.GetWorkspaceId(), req.Msg.GetProjectId()
	res := authz.Workspace(ws)
	if pid != "" {
		res = authz.Project(pid)
	}
	if err := s.check(ctx, id, ws, authz.View, res); err != nil {
		return nil, err
	}
	out := &kbv1.ListPropertiesResponse{}
	err = s.DB.ReadTx(ctx, tenant(id, ws), func(ctx context.Context, tx pgx.Tx) error {
		props, err := s.Schema.ListProperties(ctx, tx, ws, pid)
		for _, p := range props {
			out.Properties = append(out.Properties, propertyProto(p))
		}
		return err
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return connect.NewResponse(out), nil
}

// CreateRelationType adds a relation type.
func (s *SchemaService) CreateRelationType(ctx context.Context, req *connect.Request[kbv1.CreateRelationTypeRequest]) (*connect.Response[kbv1.CreateRelationTypeResponse], error) {
	id, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	ws, pid := req.Msg.GetWorkspaceId(), req.Msg.GetProjectId()
	if err := s.check(ctx, id, ws, authz.ManageSchema, authz.Project(pid)); err != nil {
		return nil, err
	}
	in := req.Msg.GetRelationType()
	if in == nil || !schema.ValidName(in.GetName()) {
		return nil, apierr.InvalidArgument("relation_type.name", "lowercase identifier")
	}
	if in.GetInverseName() != "" && !schema.ValidName(in.GetInverseName()) {
		return nil, apierr.InvalidArgument("relation_type.inverse_name", "lowercase identifier")
	}
	r := &schema.RelationType{WorkspaceID: ws, ProjectID: pid, Name: in.GetName(), InverseName: in.GetInverseName(), Symmetric: in.GetSymmetric(), DAG: in.GetDag(), SourceTypes: in.GetSourceTypes(), TargetTypes: in.GetTargetTypes()}
	var out *kbv1.RelationType
	err = s.DB.Tx(ctx, tenant(id, ws), func(ctx context.Context, tx pgx.Tx) error {
		if err := s.Schema.CreateRelationType(ctx, tx, r); err != nil {
			if schema.IsUnique(err) {
				return apierr.AlreadyExists("relation type " + r.Name)
			}
			return err
		}
		out = relationProto(r)
		s.audit(ctx, tx, id, ws, "schema.create_relation_type", "relation_type", r.ID, map[string]any{"name": r.Name})
		return nil
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return connect.NewResponse(&kbv1.CreateRelationTypeResponse{RelationType: out}), nil
}

// ListRelationTypes lists relation types of a project.
func (s *SchemaService) ListRelationTypes(ctx context.Context, req *connect.Request[kbv1.ListRelationTypesRequest]) (*connect.Response[kbv1.ListRelationTypesResponse], error) {
	id, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	ws, pid := req.Msg.GetWorkspaceId(), req.Msg.GetProjectId()
	if err := s.check(ctx, id, ws, authz.View, authz.Project(pid)); err != nil {
		return nil, err
	}
	out := &kbv1.ListRelationTypesResponse{}
	err = s.DB.ReadTx(ctx, tenant(id, ws), func(ctx context.Context, tx pgx.Tx) error {
		rels, err := s.Schema.ListRelationTypes(ctx, tx, ws, pid)
		for _, r := range rels {
			out.RelationTypes = append(out.RelationTypes, relationProto(r))
		}
		return err
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return connect.NewResponse(out), nil
}

// normalizeProps validates and canonicalizes a property bag for a type:
// select names become option ids, lists become sets, defaults are applied.
func normalizeProps(snap *schema.Snapshot, typeID string, in map[string]any, allowIncomplete bool) (map[string]any, error) {
	out := map[string]any{}
	for k, v := range in {
		if strings.HasPrefix(k, "_") || k == schema.KeyProp {
			continue // reserved keys are managed by the node
		}
		if p, ok := snap.Props[k]; ok && v != nil {
			out[k] = schema.Normalize(p, v)
		} else {
			out[k] = v
		}
	}
	out = snap.ApplyDefaults(typeID, out)
	v := snap.Validate(typeID, out)
	if len(v.Invalid) > 0 || (len(v.Missing) > 0 && !allowIncomplete) {
		return nil, apierr.SchemaViolation(v.Missing, v.Invalid)
	}
	return out, nil
}
