package api

import (
	"context"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	kbv1 "github.com/acx1729/ocean/gen/kb/v1"
	"github.com/acx1729/ocean/internal/apierr"
	"github.com/acx1729/ocean/internal/auth"
	"github.com/acx1729/ocean/internal/authz"
	"github.com/acx1729/ocean/internal/loro"
	"github.com/acx1729/ocean/internal/projection"
	"github.com/acx1729/ocean/internal/schema"
	"github.com/acx1729/ocean/internal/truth"
)

// copySubtree emits TreeCreate ops recreating n and its descendants (same
// block ids) under parent at index in the target mutation.
func copySubtree(to *truth.Mutation, n *loro.Node, parent string, index int, override func(n *loro.Node, props map[string]any) (typeID string)) {
	props := map[string]any{}
	for k, v := range n.Props {
		props[k] = v
	}
	typeID := n.TypeID
	if override != nil {
		typeID = override(n, props)
	}
	to.Add(loro.TreeCreate(n.ID, parent, index, &loro.TreeCreateInit{Content: n.Content, TypeID: typeID, CreatedAt: n.CreatedAt, CreatedBy: n.CreatedBy, Props: props}))
	for _, ch := range n.Children {
		copySubtree(to, ch, loro.Placeholder(n.ID), -1, nil)
	}
}

// toBoundary splits the subtree rooted at blockID out of its doc into a new
// boundary doc that carries its own grants (specification section 6). The
// block keeps its id: the parent doc holds a portal node, the boundary doc
// holds the block and its descendants. typeID, when set, is applied to the
// block in its new doc (promotion to a doc-owning type).
func (s *BlocksService) toBoundary(ctx context.Context, id *auth.Identity, ws string, loc *location, blockID string, inherit bool, typeID string, allowIncomplete bool) (*kbv1.Block, error) {
	newDocID := newID()
	var snap *schema.Snapshot
	var rows [2]*truth.DocRow
	results, err := s.Mat.MutateWithNew(ctx, tenant(id, ws), &truth.DocRow{WorkspaceID: ws, ID: newDocID, ProjectID: loc.projectID, Kind: truth.KindBoundary, PageID: loc.pageID, ParentDocID: loc.docID, PortalBlockID: blockID},
		[]string{loc.docID, newDocID}, truth.Options{Actor: id.Principal, OnCommit: func(ctx context.Context, tx pgx.Tx, results []*truth.Result) error {
			for i, r := range results {
				fresh, err := s.Truth.GetDoc(ctx, tx, ws, r.DocID)
				if err != nil {
					return err
				}
				rows[i] = fresh
				if err := s.index(ctx, tx, fresh, r.State, r.Seq, id.Principal, snap); err != nil {
					return err
				}
			}
			s.audit(ctx, tx, id, ws, "block.restrict", "block", blockID, map[string]any{"doc_id": newDocID, "inherit": inherit, "type_id": typeID})
			return nil
		}}, func(ctx context.Context, tx pgx.Tx, docs []*truth.Mutation) error {
			parent, child := docs[0], docs[1]
			if err := s.check(ctx, id, ws, authz.Share, authz.Doc(parent.Row.ID, parent.Row.ProjectID)); err != nil {
				return err
			}
			var err error
			snap, err = s.Schema.Load(ctx, tx, ws, parent.Row.ProjectID)
			if err != nil {
				return err
			}
			n, ok := parent.State.ByID[blockID]
			if !ok {
				return apierr.NotFound("block", blockID)
			}
			if n.PortalDocID != "" {
				return apierr.FailedPrecondition("the block is already a boundary")
			}
			var count int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM docs WHERE workspace_id = $1 AND page_id = $2 AND kind = 'boundary' AND deleted_at IS NULL`, ws, parent.Row.PageID).Scan(&count); err != nil {
				return err
			}
			if count >= s.Config.Limits.BoundariesPerPage {
				return apierr.ResourceExhausted("boundaries", "a page may hold at most 200 boundaries")
			}
			finalType := n.TypeID
			finalProps := map[string]any{}
			for k, v := range n.Props {
				finalProps[k] = v
			}
			if typeID != "" {
				t, ok := snap.Type(typeID)
				if !ok {
					return apierr.InvalidArgument("type_id", "unknown type")
				}
				finalType = t.ID
				finalProps, err = normalizeProps(snap, t.ID, n.Props, allowIncomplete)
				if err != nil {
					return err
				}
				if v := snap.Validate(t.ID, finalProps); len(v.Missing) > 0 {
					finalProps["_incomplete"] = true
				}
				if snap.Numbered(t.ID) && stringProp(n.Props, schema.KeyProp) == "" {
					key, err := s.allocateKey(ctx, tx, ws, parent.Row.ProjectID, blockID)
					if err != nil {
						return err
					}
					finalProps[schema.KeyProp] = key
				}
			}
			parentTree := ""
			if n.Parent != nil {
				parentTree = n.Parent.TreeID
			}
			parent.Add(loro.TreeDelete(n.TreeID))
			parent.Add(loro.TreeCreate(blockID, parentTree, n.Index, &loro.TreeCreateInit{TypeID: finalType, CreatedAt: n.CreatedAt, CreatedBy: n.CreatedBy}))
			parent.Add(loro.NodeSet(loro.Placeholder(blockID), "portal_doc_id", newDocID))
			parent.Emit(truth.EventBoundaryAdded, map[string]any{"block_id": blockID, "doc_id": newDocID})
			child.Add(loro.MetaSet("format", parent.State.Meta.Format), loro.MetaSet("type_id", finalType))
			copySubtree(child, n, "", -1, func(root *loro.Node, props map[string]any) string {
				for k := range props {
					delete(props, k)
				}
				for k, v := range finalProps {
					props[k] = v
				}
				return finalType
			})
			if typeID != "" && finalType != n.TypeID {
				child.Emit(truth.EventTypeChanged, map[string]any{"block_id": blockID, "old_type_id": n.TypeID, "new_type_id": finalType})
			}
			parentDoc := ""
			if inherit {
				parentDoc = parent.Row.ID
			}
			return s.Provision.ProvisionDoc(ctx, tx, ws, parent.Row.ProjectID, newDocID, parentDoc)
		})
	if err != nil {
		return nil, mapErr(err)
	}
	s.notify(ws, results, id.Principal)
	child := results[1]
	if len(child.State.Roots) == 0 {
		return nil, apierr.Internal(nil)
	}
	di := infoOf(rows[1])
	di.pageID = loc.pageID
	return s.blockProtoAt(child.State.Roots[0], di, 0, "", nil), nil
}

// Restrict turns a block into a permission boundary with its own doc.
func (s *BlocksService) Restrict(ctx context.Context, req *connect.Request[kbv1.RestrictRequest]) (*connect.Response[kbv1.RestrictResponse], error) {
	id, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	ws := req.Msg.GetWorkspaceId()
	loc, err := s.pageOf(ctx, id, ws, req.Msg.GetBlockId())
	if err != nil {
		return nil, err
	}
	if loc.portalDocID != "" && loc.portalDocID == loc.docID {
		return nil, apierr.FailedPrecondition("the block is already a boundary")
	}
	b, err := s.toBoundary(ctx, id, ws, loc, req.Msg.GetBlockId(), req.Msg.GetInherit(), "", false)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&kbv1.RestrictResponse{DocId: b.DocId, Version: b.Version}), nil
}

// Unrestrict merges a boundary back into its parent doc and deletes the boundary doc.
func (s *BlocksService) Unrestrict(ctx context.Context, req *connect.Request[kbv1.UnrestrictRequest]) (*connect.Response[kbv1.UnrestrictResponse], error) {
	id, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	ws, blockID := req.Msg.GetWorkspaceId(), req.Msg.GetBlockId()
	loc, err := s.pageOf(ctx, id, ws, blockID)
	if err != nil {
		return nil, err
	}
	var boundary *truth.DocRow
	err = s.DB.ReadTx(ctx, tenant(id, ws), func(ctx context.Context, tx pgx.Tx) error {
		boundary, err = s.Truth.GetDoc(ctx, tx, ws, loc.docID)
		return err
	})
	if err != nil || boundary.Kind != truth.KindBoundary || boundary.PortalBlockID != blockID {
		return nil, apierr.FailedPrecondition("the block is not a boundary")
	}
	var parentRow *truth.DocRow
	results, err := s.Mat.Mutate(ctx, tenant(id, ws), []string{boundary.ParentDocID, boundary.ID}, truth.Options{Actor: id.Principal, OnCommit: func(ctx context.Context, tx pgx.Tx, results []*truth.Result) error {
		fresh, err := s.Truth.GetDoc(ctx, tx, ws, boundary.ParentDocID)
		if err != nil {
			return err
		}
		parentRow = fresh
		snap, err := s.Schema.Load(ctx, tx, ws, fresh.ProjectID)
		if err != nil {
			return err
		}
		if err := s.index(ctx, tx, fresh, results[0].State, results[0].Seq, id.Principal, snap); err != nil {
			return err
		}
		if err := projection.DeleteDoc(ctx, tx, ws, boundary.ID); err != nil {
			return err
		}
		if err := s.Truth.PurgeDoc(ctx, tx, ws, boundary.ID); err != nil {
			return err
		}
		s.audit(ctx, tx, id, ws, "block.unrestrict", "block", blockID, map[string]any{"doc_id": boundary.ID})
		return nil
	}}, func(ctx context.Context, tx pgx.Tx, docs []*truth.Mutation) error {
		parent, child := docs[0], docs[1]
		if err := s.check(ctx, id, ws, authz.Share, authz.Doc(parent.Row.ID, parent.Row.ProjectID)); err != nil {
			return err
		}
		if err := s.check(ctx, id, ws, authz.Share, authz.Doc(child.Row.ID, child.Row.ProjectID)); err != nil {
			return err
		}
		portal, ok := parent.State.ByID[blockID]
		if !ok || portal.PortalDocID != child.Row.ID {
			return apierr.FailedPrecondition("portal not found in the parent doc")
		}
		if len(child.State.Roots) == 0 {
			return apierr.FailedPrecondition("boundary doc is empty")
		}
		parentTree := ""
		if portal.Parent != nil {
			parentTree = portal.Parent.TreeID
		}
		parent.Add(loro.TreeDelete(portal.TreeID))
		copySubtree(parent, child.State.Roots[0], parentTree, portal.Index, nil)
		parent.Emit(truth.EventBoundaryGone, map[string]any{"block_id": blockID, "doc_id": child.Row.ID})
		return nil
	})
	s.Mat.Evict(ws, boundary.ID)
	if err != nil {
		return nil, mapErr(err)
	}
	s.notify(ws, results[:1], id.Principal)
	_ = parentRow
	return connect.NewResponse(&kbv1.UnrestrictResponse{Version: results[0].Seq}), nil
}
