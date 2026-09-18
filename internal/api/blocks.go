package api

import (
	"context"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	kbv1 "github.com/acx1729/ocean/gen/kb/v1"
	"github.com/acx1729/ocean/gen/kb/v1/kbv1connect"
	"github.com/acx1729/ocean/internal/apierr"
	"github.com/acx1729/ocean/internal/auth"
	"github.com/acx1729/ocean/internal/authz"
	"github.com/acx1729/ocean/internal/loro"
	"github.com/acx1729/ocean/internal/schema"
	"github.com/acx1729/ocean/internal/truth"
)

// BlocksService implements kb.v1.BlocksService.
type BlocksService struct{ *core }

var _ kbv1connect.BlocksServiceHandler = (*BlocksService)(nil)

// opCtx is the state shared by the block operations inside one mutation.
type opCtx struct {
	c       *core
	id      *auth.Identity
	mu      *truth.Mutation
	snap    *schema.Snapshot
	created map[string]bool // block ids created earlier in this mutation
	now     time.Time
	tx      pgx.Tx
	ctx     context.Context
	events  int
}

// tree resolves a block id to a tree id (or placeholder); "" for the page root.
func (o *opCtx) tree(blockID string) (string, error) {
	if blockID == "" || blockID == o.mu.Row.PageID {
		return "", nil
	}
	if n, ok := o.mu.State.ByID[blockID]; ok {
		return n.TreeID, nil
	}
	if o.created[blockID] {
		return loro.Placeholder(blockID), nil
	}
	return "", apierr.NotFound("block", blockID)
}

// siblings returns the current children under a parent (page root when "").
func (o *opCtx) siblings(parentID string) []*loro.Node {
	if parentID == "" || parentID == o.mu.Row.PageID {
		return o.mu.State.Roots
	}
	if n, ok := o.mu.State.ByID[parentID]; ok {
		return n.Children
	}
	return nil
}

func (o *opCtx) index(parentID string, pos *kbv1.Position) (int, error) {
	if after, ok := pos.GetAt().(*kbv1.Position_After); ok && o.created[after.After] {
		return -1, nil // after a block created in this batch: append
	}
	return resolveIndex(o.siblings(parentID), pos)
}

func (o *opCtx) create(in *kbv1.BlockOp_Create) ([]string, error) {
	parent, err := o.tree(in.GetParentBlockId())
	if err != nil {
		return nil, err
	}
	if parent == "" && o.mu.Row.Kind == truth.KindBoundary {
		return nil, apierr.InvalidArgument("parent_block_id", "required inside a restricted block")
	}
	idx, err := o.index(in.GetParentBlockId(), in.GetPosition())
	if err != nil {
		return nil, err
	}
	if int64(len(in.GetMarkdown())) > o.c.Config.Limits.BlockMarkdownBytes*int64(o.c.Config.Limits.BatchOps) {
		return nil, apierr.ResourceExhausted("markdown", "too large")
	}
	if len(o.mu.State.ByID) >= o.c.Config.Limits.BlocksPerPage {
		return nil, apierr.ResourceExhausted("blocks", "page block limit reached")
	}
	specs := o.c.Splitter.Split(in.GetMarkdown(), o.mu.State.Meta.Format == "outliner")
	if len(specs) == 0 {
		specs = []*BlockSpec{{Markdown: ""}}
	}
	typeID := in.GetTypeId()
	var props map[string]any
	if typeID != "" {
		t, ok := o.snap.Type(typeID)
		if !ok {
			return nil, apierr.InvalidArgument("type_id", "unknown type")
		}
		typeID = t.ID
		if o.snap.OwnsDoc(t.ID) {
			return nil, apierr.Unimplemented("doc-owning types inside a page (boundaries)")
		}
		props, err = normalizeProps(o.snap, t.ID, fromStruct(in.GetProps()), false)
		if err != nil {
			return nil, err
		}
	} else if in.GetProps() != nil {
		props, err = normalizeProps(o.snap, "", fromStruct(in.GetProps()), true)
		if err != nil {
			return nil, err
		}
	}
	firstID := in.GetBlockId()
	if firstID != "" && (!validUUID(firstID) || o.mu.State.ByID[firstID] != nil || o.created[firstID]) {
		return nil, apierr.InvalidArgument("block_id", "must be a fresh UUID")
	}
	ids := o.c.createFromSpecs(o.mu, specs, parent, idx, o.id.Principal, o.now, o.snap, typeID, firstID)
	for _, id := range ids {
		o.created[id] = true
	}
	if len(props) > 0 {
		for k, v := range props {
			o.mu.Add(loro.NodePropSet(loro.Placeholder(ids[0]), k, v))
		}
	}
	if typeID != "" && o.snap.Numbered(typeID) {
		key, err := o.c.allocateKey(o.ctx, o.tx, o.mu.Row.WorkspaceID, o.mu.Row.ProjectID, ids[0])
		if err != nil {
			return nil, err
		}
		o.mu.Add(loro.NodePropSet(loro.Placeholder(ids[0]), schema.KeyProp, key))
	}
	o.mu.Emit(truth.EventBlockCreated, map[string]any{"block_ids": ids, "page_id": o.mu.Row.PageID})
	return ids, nil
}

func (o *opCtx) update(in *kbv1.BlockOp_Update) error {
	tid, err := o.tree(in.GetBlockId())
	if err != nil {
		return err
	}
	if tid == "" {
		return apierr.InvalidArgument("block_id", "use PagesService.Update for the page block")
	}
	if int64(len(in.GetMarkdown())) > o.c.Config.Limits.BlockMarkdownBytes {
		return apierr.ResourceExhausted("markdown", "exceeds the block size limit")
	}
	if in.GetMarkdown() != "" || in.GetProps() == nil {
		o.mu.Add(loro.NodeText(tid, in.GetMarkdown()))
	}
	if in.GetProps() != nil {
		n := o.mu.State.ByID[in.GetBlockId()]
		merged := map[string]any{}
		if !in.GetReplaceProps() && n != nil {
			for k, v := range n.Props {
				merged[k] = v
			}
		}
		for k, v := range fromStruct(in.GetProps()) {
			merged[k] = v
		}
		typeID := ""
		if n != nil {
			typeID = n.TypeID
		}
		props, err := normalizeProps(o.snap, typeID, merged, true)
		if err != nil {
			return err
		}
		if in.GetReplaceProps() && n != nil {
			for k := range n.Props {
				if _, keep := props[k]; !keep && k != schema.KeyProp {
					o.mu.Add(loro.NodePropDel(tid, k))
				}
			}
		}
		for k, v := range props {
			if v == nil {
				o.mu.Add(loro.NodePropDel(tid, k))
			} else {
				o.mu.Add(loro.NodePropSet(tid, k, v))
			}
		}
	}
	o.mu.Emit(truth.EventBlockUpdated, map[string]any{"block_ids": []string{in.GetBlockId()}, "page_id": o.mu.Row.PageID})
	return nil
}

func (o *opCtx) setProps(in *kbv1.BlockOp_SetProps) error {
	tid, err := o.tree(in.GetBlockId())
	if err != nil {
		return err
	}
	if tid == "" {
		return apierr.InvalidArgument("block_id", "use PagesService.Update for the page block")
	}
	n := o.mu.State.ByID[in.GetBlockId()]
	merged := map[string]any{}
	typeID := ""
	if n != nil {
		typeID = n.TypeID
		for k, v := range n.Props {
			merged[k] = v
		}
	}
	for _, k := range in.GetUnset() {
		delete(merged, k)
	}
	changes := []map[string]any{}
	for k, v := range fromStruct(in.GetProps()) {
		if k == schema.KeyProp {
			return apierr.InvalidArgument("props.key", "keys are assigned by the node")
		}
		changes = append(changes, map[string]any{"property_id": k, "old": merged[k], "new": v})
		merged[k] = v
	}
	props, err := normalizeProps(o.snap, typeID, merged, true)
	if err != nil {
		return err
	}
	if len(props) > o.c.Config.Limits.PropertiesPerBlock {
		return apierr.ResourceExhausted("props", "too many properties on one block")
	}
	for _, k := range in.GetUnset() {
		if k != schema.KeyProp {
			o.mu.Add(loro.NodePropDel(tid, k))
		}
	}
	for k := range fromStruct(in.GetProps()) {
		if v, ok := props[k]; ok && v != nil {
			o.mu.Add(loro.NodePropSet(tid, k, v))
		} else {
			o.mu.Add(loro.NodePropDel(tid, k))
		}
	}
	o.mu.Emit(truth.EventPropsChanged, map[string]any{"block_id": in.GetBlockId(), "changes": changes})
	return nil
}

func (o *opCtx) setType(in *kbv1.BlockOp_SetType) error {
	t, ok := o.snap.Type(in.GetTypeId())
	if !ok {
		return apierr.InvalidArgument("type_id", "unknown type")
	}
	if in.GetBlockId() == o.mu.Row.PageID {
		if !o.snap.OwnsDoc(t.ID) {
			return apierr.InvalidArgument("type_id", "a page needs a doc-owning type")
		}
		old := o.mu.State.Meta.TypeID
		props, err := normalizeProps(o.snap, t.ID, o.mu.State.Meta.Props, in.GetAllowIncomplete())
		if err != nil {
			return err
		}
		for k, v := range props {
			o.mu.Add(loro.MetaPropSet(k, v))
		}
		if o.snap.Numbered(t.ID) && stringProp(o.mu.State.Meta.Props, schema.KeyProp) == "" {
			key, err := o.c.allocateKey(o.ctx, o.tx, o.mu.Row.WorkspaceID, o.mu.Row.ProjectID, o.mu.Row.PageID)
			if err != nil {
				return err
			}
			o.mu.Add(loro.MetaPropSet(schema.KeyProp, key))
		}
		o.mu.Add(loro.MetaSet("type_id", t.ID))
		o.mu.Emit(truth.EventTypeChanged, map[string]any{"block_id": in.GetBlockId(), "old_type_id": old, "new_type_id": t.ID})
		return nil
	}
	tid, err := o.tree(in.GetBlockId())
	if err != nil {
		return err
	}
	boundaryRoot := o.mu.Row.Kind == truth.KindBoundary && in.GetBlockId() == o.mu.Row.PortalBlockID
	if o.snap.OwnsDoc(t.ID) && !boundaryRoot {
		return apierr.FailedPrecondition("promoting a block to a doc-owning type requires BlocksService.SetType outside a batch")
	}
	if boundaryRoot {
		o.mu.Add(loro.MetaSet("type_id", t.ID))
	}
	n := o.mu.State.ByID[in.GetBlockId()]
	current := map[string]any{}
	old := ""
	if n != nil {
		old = n.TypeID
		for k, v := range n.Props {
			current[k] = v
		}
	}
	props, err := normalizeProps(o.snap, t.ID, current, in.GetAllowIncomplete())
	if err != nil {
		return err
	}
	for k, v := range props {
		o.mu.Add(loro.NodePropSet(tid, k, v))
	}
	if v := o.snap.Validate(t.ID, props); len(v.Missing) > 0 {
		o.mu.Add(loro.NodePropSet(tid, "_incomplete", true))
	} else {
		o.mu.Add(loro.NodePropDel(tid, "_incomplete"))
	}
	if o.snap.Numbered(t.ID) && stringProp(current, schema.KeyProp) == "" {
		key, err := o.c.allocateKey(o.ctx, o.tx, o.mu.Row.WorkspaceID, o.mu.Row.ProjectID, in.GetBlockId())
		if err != nil {
			return err
		}
		o.mu.Add(loro.NodePropSet(tid, schema.KeyProp, key))
	}
	o.mu.Add(loro.NodeSet(tid, "type_id", t.ID))
	o.mu.Emit(truth.EventTypeChanged, map[string]any{"block_id": in.GetBlockId(), "old_type_id": old, "new_type_id": t.ID})
	return nil
}

func (o *opCtx) move(in *kbv1.BlockOp_Move) error {
	tid, err := o.tree(in.GetBlockId())
	if err != nil {
		return err
	}
	if tid == "" {
		return apierr.InvalidArgument("block_id", "the page block cannot move")
	}
	parent, err := o.tree(in.GetParentBlockId())
	if err != nil {
		return err
	}
	// A block cannot move under itself or its descendants.
	if in.GetParentBlockId() != "" {
		for p := o.mu.State.ByID[in.GetParentBlockId()]; p != nil; p = p.Parent {
			if p.ID == in.GetBlockId() {
				return apierr.InvalidArgument("parent_block_id", "cannot move a block under its own subtree")
			}
		}
	}
	idx, err := o.index(in.GetParentBlockId(), in.GetPosition())
	if err != nil {
		return err
	}
	// Moving after a sibling that precedes the block in the same parent shifts the index by one.
	if after, ok := in.GetPosition().GetAt().(*kbv1.Position_After); ok && idx > 0 {
		sibs := o.siblings(in.GetParentBlockId())
		selfIdx, afterIdx := -1, -1
		for i, s := range sibs {
			if s.ID == in.GetBlockId() {
				selfIdx = i
			}
			if s.ID == after.After {
				afterIdx = i
			}
		}
		if selfIdx >= 0 && selfIdx < afterIdx {
			idx--
		}
	}
	o.mu.Add(loro.TreeMove(tid, parent, idx))
	o.mu.Emit(truth.EventBlockMoved, map[string]any{"block_ids": []string{in.GetBlockId()}, "page_id": o.mu.Row.PageID})
	return nil
}

func (o *opCtx) del(in *kbv1.BlockOp_Delete) error {
	tid, err := o.tree(in.GetBlockId())
	if err != nil {
		return err
	}
	if tid == "" {
		return apierr.InvalidArgument("block_id", "use PagesService.Trash for the page")
	}
	if o.mu.Row.Kind == truth.KindBoundary && in.GetBlockId() == o.mu.Row.PortalBlockID {
		return apierr.FailedPrecondition("delete the restricted block from its page, or unrestrict it first")
	}
	if n := o.mu.State.ByID[in.GetBlockId()]; n != nil {
		var walk func(n *loro.Node) error
		walk = func(n *loro.Node) error {
			if n.PortalDocID != "" {
				if err := o.c.deleteBoundaryDoc(o.ctx, o.tx, o.mu.Row.WorkspaceID, n.PortalDocID, o.now); err != nil {
					return err
				}
			}
			for _, ch := range n.Children {
				if err := walk(ch); err != nil {
					return err
				}
			}
			return nil
		}
		if err := walk(n); err != nil {
			return err
		}
	}
	o.mu.Add(loro.TreeDelete(tid))
	o.mu.Emit(truth.EventBlockDeleted, map[string]any{"block_ids": []string{in.GetBlockId()}, "page_id": o.mu.Row.PageID})
	return nil
}

// mutateBlocks locates the page of a block (or takes it directly), checks
// edit permission and runs fn with an opCtx; the projection is refreshed and
// the sync layer notified.
func (s *BlocksService) mutateBlocks(ctx context.Context, id *auth.Identity, ws, pageID string, ifVersion int64, fn func(o *opCtx) error) (*truth.Result, *truth.DocRow, error) {
	var snap *schema.Snapshot
	var fresh *truth.DocRow
	res, err := s.Mat.MutateOne(ctx, tenant(id, ws), pageID, truth.Options{Actor: id.Principal, IfVersion: ifVersion, OnCommit: func(ctx context.Context, tx pgx.Tx, results []*truth.Result) error {
		r := results[0]
		var err error
		fresh, err = s.Truth.GetDoc(ctx, tx, ws, pageID)
		if err != nil {
			return err
		}
		if !r.Changed {
			return nil
		}
		if err := s.index(ctx, tx, fresh, r.State, r.Seq, id.Principal, snap); err != nil {
			return err
		}
		s.audit(ctx, tx, id, ws, "blocks.write", "page", pageID, map[string]any{"seq": r.Seq})
		return nil
	}}, func(ctx context.Context, tx pgx.Tx, mu *truth.Mutation) error {
		if err := s.check(ctx, id, ws, authz.Edit, authz.Doc(mu.Row.ID, mu.Row.ProjectID)); err != nil {
			return err
		}
		var err error
		snap, err = s.Schema.Load(ctx, tx, ws, mu.Row.ProjectID)
		if err != nil {
			return err
		}
		return fn(&opCtx{c: s.core, id: id, mu: mu, snap: snap, created: map[string]bool{}, now: s.Clock(), tx: tx, ctx: ctx})
	})
	if err != nil {
		return nil, nil, mapErr(err)
	}
	s.notify(ws, []*truth.Result{res}, id.Principal)
	return res, fresh, nil
}

// pageOf resolves the page a block belongs to.
func (s *BlocksService) pageOf(ctx context.Context, id *auth.Identity, ws, blockID string) (*location, error) {
	if !validUUID(blockID) {
		return nil, apierr.NotFound("block", blockID)
	}
	var loc *location
	err := s.DB.ReadTx(ctx, tenant(id, ws), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		loc, err = s.locate(ctx, tx, ws, blockID)
		return err
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return loc, nil
}

func blockOf(res *truth.Result, row *truth.DocRow, c *core, blockID string) *kbv1.Block {
	if n, ok := res.State.ByID[blockID]; ok {
		if n.PortalDocID != "" {
			return c.portalProto(n, infoOf(row), len(n.Path())+1, parentOf(n, row.PageID), append([]string{row.PageID}, n.Path()...))
		}
		return c.blockProto(n, infoOf(row), len(n.Path())+1)
	}
	if blockID == row.PageID {
		return c.pageBlockProto(res.State, row)
	}
	return nil
}

// Get returns a block and optionally its descendants, from the live doc.
func (s *BlocksService) Get(ctx context.Context, req *connect.Request[kbv1.GetBlockRequest]) (*connect.Response[kbv1.GetBlockResponse], error) {
	id, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	ws, blockID := req.Msg.GetWorkspaceId(), req.Msg.GetBlockId()
	if err := s.check(ctx, id, ws, authz.View, authz.Workspace(ws)); err != nil {
		return nil, err
	}
	out := &kbv1.GetBlockResponse{}
	err = s.DB.Tx(ctx, tenant(id, ws), func(ctx context.Context, tx pgx.Tx) error {
		loc, err := s.locate(ctx, tx, ws, blockID)
		if err != nil {
			return err
		}
		if err := s.check(ctx, id, ws, authz.View, authz.Doc(loc.docID, loc.projectID)); err != nil {
			return err
		}
		st, row, err := s.Mat.Read(ctx, tx, ws, loc.docID)
		if err != nil {
			return err
		}
		out.IndexedSeq = row.IndexedSeq
		if blockID == row.PageID {
			out.Block = s.pageBlockProto(st, row)
			xs, err := s.pageTree(ctx, tx, id, row, st, int(req.Msg.GetDepth()))
			if err != nil {
				return err
			}
			out.Descendants = s.flattenX(xs)
			return nil
		}
		n, ok := st.ByID[blockID]
		if !ok {
			return apierr.NotFound("block", blockID)
		}
		di := infoOf(row)
		di.pageID = loc.pageID
		out.Block = s.blockProto(n, di, len(n.Path())+1)
		if req.Msg.GetDepth() > 0 {
			xs, err := s.expand(ctx, tx, id, di, n.Children, n.ID, append(append([]string{loc.pageID}, n.Path()...), n.ID), 1, int(req.Msg.GetDepth()))
			if err != nil {
				return err
			}
			out.Descendants = s.flattenX(xs)
		}
		return nil
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return connect.NewResponse(out), nil
}

// Batch applies up to 500 operations to one page atomically.
func (s *BlocksService) Batch(ctx context.Context, req *connect.Request[kbv1.BatchRequest]) (*connect.Response[kbv1.BatchResponse], error) {
	id, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	ws, pageID := req.Msg.GetWorkspaceId(), req.Msg.GetPageId()
	if len(req.Msg.GetOps()) == 0 || len(req.Msg.GetOps()) > s.Config.Limits.BatchOps {
		return nil, apierr.InvalidArgument("ops", fmt.Sprintf("1 to %d operations", s.Config.Limits.BatchOps))
	}
	if !validUUID(pageID) {
		return nil, apierr.NotFound("page", pageID)
	}
	res, err := idempotent(ctx, s.core, id, ws, req, func() *kbv1.BatchResponse { return &kbv1.BatchResponse{} },
		func(record func(context.Context, pgx.Tx, *kbv1.BatchResponse) error) (*kbv1.BatchResponse, error) {
			out := &kbv1.BatchResponse{}
			var recordErr error
			r, _, err := s.mutateBlocks(ctx, id, ws, pageID, req.Msg.GetIfVersion(), func(o *opCtx) error {
				for i, op := range req.Msg.GetOps() {
					var ids []string
					var err error
					switch k := op.GetOp().(type) {
					case *kbv1.BlockOp_Create_:
						ids, err = o.create(k.Create)
					case *kbv1.BlockOp_Update_:
						err = o.update(k.Update)
						ids = []string{k.Update.GetBlockId()}
					case *kbv1.BlockOp_SetProps_:
						err = o.setProps(k.SetProps)
						ids = []string{k.SetProps.GetBlockId()}
					case *kbv1.BlockOp_SetType_:
						err = o.setType(k.SetType)
						ids = []string{k.SetType.GetBlockId()}
					case *kbv1.BlockOp_Move_:
						err = o.move(k.Move)
						ids = []string{k.Move.GetBlockId()}
					case *kbv1.BlockOp_Delete_:
						err = o.del(k.Delete)
						ids = []string{k.Delete.GetBlockId()}
					default:
						err = apierr.InvalidArgument(fmt.Sprintf("ops[%d]", i), "empty operation")
					}
					if err != nil {
						return err
					}
					out.Results = append(out.Results, &kbv1.BlockOpResult{BlockIds: ids})
				}
				return nil
			})
			if err != nil {
				return nil, err
			}
			out.Version = r.Seq
			// Store the idempotent response after the fact (the mutation transaction has committed).
			recordErr = s.DB.Tx(ctx, tenant(id, ws), func(ctx context.Context, tx pgx.Tx) error { return record(ctx, tx, out) })
			if recordErr != nil {
				s.Log.Warn("idempotency record", "error", recordErr)
			}
			return out, nil
		})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(res), nil
}

// Create adds blocks from Markdown under a parent.
func (s *BlocksService) Create(ctx context.Context, req *connect.Request[kbv1.CreateBlockRequest]) (*connect.Response[kbv1.CreateBlockResponse], error) {
	id, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	ws, pageID := req.Msg.GetWorkspaceId(), req.Msg.GetPageId()
	if !validUUID(pageID) {
		return nil, apierr.NotFound("page", pageID)
	}
	// A doc-owning type (a work item inside a page) is created inline first and
	// then split into its own boundary doc that carries the type and the key.
	ownsDoc := false
	typeID := req.Msg.GetTypeId()
	if typeID != "" {
		loc, err := s.pageOf(ctx, id, ws, pageID)
		if err != nil {
			return nil, err
		}
		err = s.DB.ReadTx(ctx, tenant(id, ws), func(ctx context.Context, tx pgx.Tx) error {
			snap, err := s.Schema.Load(ctx, tx, ws, loc.projectID)
			if err != nil {
				return err
			}
			t, ok := snap.Type(typeID)
			if !ok {
				return apierr.InvalidArgument("type_id", "unknown type")
			}
			typeID, ownsDoc = t.ID, snap.OwnsDoc(t.ID)
			return nil
		})
		if err != nil {
			return nil, mapErr(err)
		}
	}
	inlineType := typeID
	if ownsDoc {
		inlineType = ""
	}
	var ids []string
	res, row, err := s.mutateBlocks(ctx, id, ws, pageID, req.Msg.GetIfVersion(), func(o *opCtx) error {
		var err error
		ids, err = o.create(&kbv1.BlockOp_Create{ParentBlockId: req.Msg.GetParentBlockId(), Position: req.Msg.GetPosition(), Markdown: req.Msg.GetMarkdown(), Props: req.Msg.GetProps(), TypeId: inlineType, BlockId: req.Msg.GetBlockId()})
		return err
	})
	if err != nil {
		return nil, err
	}
	out := &kbv1.CreateBlockResponse{Version: res.Seq}
	for _, bid := range ids {
		if b := blockOf(res, row, s.core, bid); b != nil {
			out.Blocks = append(out.Blocks, b)
		}
	}
	if ownsDoc && len(ids) > 0 {
		loc := &location{pageID: row.PageID, docID: row.ID, projectID: row.ProjectID}
		b, err := s.toBoundary(ctx, id, ws, loc, ids[0], true, typeID, false)
		if err != nil {
			return nil, err
		}
		out.Blocks[0] = b
		out.Version = b.Version
	}
	return connect.NewResponse(out), nil
}

// Update rewrites a block's Markdown and merges or replaces its properties.
func (s *BlocksService) Update(ctx context.Context, req *connect.Request[kbv1.UpdateBlockRequest]) (*connect.Response[kbv1.UpdateBlockResponse], error) {
	id, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	ws := req.Msg.GetWorkspaceId()
	loc, err := s.pageOf(ctx, id, ws, req.Msg.GetBlockId())
	if err != nil {
		return nil, err
	}
	res, row, err := s.mutateBlocks(ctx, id, ws, loc.docID, req.Msg.GetIfVersion(), func(o *opCtx) error {
		return o.update(&kbv1.BlockOp_Update{BlockId: req.Msg.GetBlockId(), Markdown: req.Msg.GetMarkdown(), Props: req.Msg.GetProps(), ReplaceProps: req.Msg.GetReplaceProps()})
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&kbv1.UpdateBlockResponse{Block: blockOf(res, row, s.core, req.Msg.GetBlockId()), Version: res.Seq, IndexedSeq: res.Seq}), nil
}

// SetProperties sets and unsets properties.
func (s *BlocksService) SetProperties(ctx context.Context, req *connect.Request[kbv1.SetPropertiesRequest]) (*connect.Response[kbv1.SetPropertiesResponse], error) {
	id, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	ws := req.Msg.GetWorkspaceId()
	loc, err := s.pageOf(ctx, id, ws, req.Msg.GetBlockId())
	if err != nil {
		return nil, err
	}
	res, row, err := s.mutateBlocks(ctx, id, ws, loc.docID, req.Msg.GetIfVersion(), func(o *opCtx) error {
		return o.setProps(&kbv1.BlockOp_SetProps{BlockId: req.Msg.GetBlockId(), Props: req.Msg.GetProps(), Unset: req.Msg.GetUnset()})
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&kbv1.SetPropertiesResponse{Block: blockOf(res, row, s.core, req.Msg.GetBlockId()), Version: res.Seq}), nil
}

// SetType changes a block's type, applying defaults and allocating a key for numbered types.
func (s *BlocksService) SetType(ctx context.Context, req *connect.Request[kbv1.SetTypeRequest]) (*connect.Response[kbv1.SetTypeResponse], error) {
	id, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	ws := req.Msg.GetWorkspaceId()
	loc, err := s.pageOf(ctx, id, ws, req.Msg.GetBlockId())
	if err != nil {
		return nil, err
	}
	ownsDoc := false
	err = s.DB.ReadTx(ctx, tenant(id, ws), func(ctx context.Context, tx pgx.Tx) error {
		snap, err := s.Schema.Load(ctx, tx, ws, loc.projectID)
		if err != nil {
			return err
		}
		t, ok := snap.Type(req.Msg.GetTypeId())
		if !ok {
			return apierr.InvalidArgument("type_id", "unknown type")
		}
		ownsDoc = snap.OwnsDoc(t.ID)
		return nil
	})
	if err != nil {
		return nil, mapErr(err)
	}
	blockID := req.Msg.GetBlockId()
	isRoot := blockID == loc.pageID || (loc.portalDocID != "" && loc.portalDocID == loc.docID)
	if ownsDoc && !isRoot {
		if err := s.check(ctx, id, ws, authz.ChangeType, authz.Doc(loc.docID, loc.projectID)); err != nil {
			return nil, err
		}
		b, err := s.toBoundary(ctx, id, ws, loc, blockID, true, req.Msg.GetTypeId(), req.Msg.GetAllowIncomplete())
		if err != nil {
			return nil, err
		}
		return connect.NewResponse(&kbv1.SetTypeResponse{Block: b, Version: b.Version}), nil
	}
	res, row, err := s.mutateBlocks(ctx, id, ws, loc.docID, req.Msg.GetIfVersion(), func(o *opCtx) error {
		if err := s.check(o.ctx, id, ws, authz.ChangeType, authz.Doc(o.mu.Row.ID, o.mu.Row.ProjectID)); err != nil {
			return err
		}
		return o.setType(&kbv1.BlockOp_SetType{BlockId: blockID, TypeId: req.Msg.GetTypeId(), AllowIncomplete: req.Msg.GetAllowIncomplete()})
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&kbv1.SetTypeResponse{Block: blockOf(res, row, s.core, blockID), Version: res.Seq}), nil
}

// Move re-parents blocks within a page or carries subtrees to another page.
func (s *BlocksService) Move(ctx context.Context, req *connect.Request[kbv1.MoveBlocksRequest]) (*connect.Response[kbv1.MoveBlocksResponse], error) {
	id, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	ws := req.Msg.GetWorkspaceId()
	if len(req.Msg.GetBlockIds()) == 0 {
		return nil, apierr.InvalidArgument("block_ids", "required")
	}
	src, err := s.pageOf(ctx, id, ws, req.Msg.GetBlockIds()[0])
	if err != nil {
		return nil, err
	}
	target := req.Msg.GetTargetPageId()
	if target == "" || target == src.pageID {
		res, row, err := s.mutateBlocks(ctx, id, ws, src.docID, req.Msg.GetIfVersion(), func(o *opCtx) error {
			pos := req.Msg.GetPosition()
			for _, bid := range req.Msg.GetBlockIds() {
				if err := o.move(&kbv1.BlockOp_Move{BlockId: bid, ParentBlockId: req.Msg.GetParentBlockId(), Position: pos}); err != nil {
					return err
				}
				pos = &kbv1.Position{At: &kbv1.Position_After{After: bid}}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		out := &kbv1.MoveBlocksResponse{Versions: []*kbv1.PageVersion{{PageId: row.PageID, Version: res.Seq}}}
		for _, bid := range req.Msg.GetBlockIds() {
			if b := blockOf(res, row, s.core, bid); b != nil {
				out.Blocks = append(out.Blocks, b)
			}
		}
		return connect.NewResponse(out), nil
	}
	return s.moveAcross(ctx, id, ws, src, target, req.Msg)
}

// moveAcross deletes subtrees from the source doc and recreates them, ids
// preserved, in the target doc within one transaction (two seqs, one event).
func (s *BlocksService) moveAcross(ctx context.Context, id *auth.Identity, ws string, src *location, targetPage string, in *kbv1.MoveBlocksRequest) (*connect.Response[kbv1.MoveBlocksResponse], error) {
	if !validUUID(targetPage) {
		return nil, apierr.NotFound("page", targetPage)
	}
	var snaps [2]*schema.Snapshot
	var rows [2]*truth.DocRow
	results, err := s.Mat.Mutate(ctx, tenant(id, ws), []string{src.docID, targetPage}, truth.Options{Actor: id.Principal, OnCommit: func(ctx context.Context, tx pgx.Tx, results []*truth.Result) error {
		for i, r := range results {
			fresh, err := s.Truth.GetDoc(ctx, tx, ws, r.DocID)
			if err != nil {
				return err
			}
			rows[i] = fresh
			if r.Changed {
				if err := s.index(ctx, tx, fresh, r.State, r.Seq, id.Principal, snaps[i]); err != nil {
					return err
				}
			}
		}
		s.audit(ctx, tx, id, ws, "blocks.move", "page", targetPage, map[string]any{"from": src.pageID, "block_ids": in.GetBlockIds()})
		return nil
	}}, func(ctx context.Context, tx pgx.Tx, docs []*truth.Mutation) error {
		from, to := docs[0], docs[1]
		for i, d := range docs {
			if err := s.check(ctx, id, ws, authz.Edit, authz.Doc(d.Row.ID, d.Row.ProjectID)); err != nil {
				return err
			}
			var err error
			snaps[i], err = s.Schema.Load(ctx, tx, ws, d.Row.ProjectID)
			if err != nil {
				return err
			}
		}
		if to.Row.Kind != truth.KindPage {
			return apierr.InvalidArgument("target_page_id", "not a page")
		}
		o := &opCtx{c: s.core, id: id, mu: to, snap: snaps[1], created: map[string]bool{}, now: s.Clock(), tx: tx, ctx: ctx}
		parent, err := o.tree(in.GetParentBlockId())
		if err != nil {
			return err
		}
		idx, err := o.index(in.GetParentBlockId(), in.GetPosition())
		if err != nil {
			return err
		}
		crossProject := from.Row.ProjectID != to.Row.ProjectID
		for _, bid := range in.GetBlockIds() {
			n, ok := from.State.ByID[bid]
			if !ok {
				return apierr.NotFound("block", bid)
			}
			from.Add(loro.TreeDelete(n.TreeID))
			var rec func(n *loro.Node, parent string, index int)
			rec = func(n *loro.Node, parent string, index int) {
				props := map[string]any{}
				for k, v := range n.Props {
					props[k] = v
				}
				if crossProject && n.TypeID != "" && snaps[1].Numbered(n.TypeID) {
					if key, err := s.allocateKey(ctx, tx, ws, to.Row.ProjectID, n.ID); err == nil {
						props[schema.KeyProp] = key
					}
				}
				to.Add(loro.TreeCreate(n.ID, parent, index, &loro.TreeCreateInit{Content: n.Content, TypeID: n.TypeID, CreatedAt: n.CreatedAt, CreatedBy: n.CreatedBy, Props: props}))
				for _, ch := range n.Children {
					rec(ch, loro.Placeholder(n.ID), -1)
				}
			}
			rec(n, parent, idx)
			if idx >= 0 {
				idx++
			}
		}
		from.Emit(truth.EventBlockMoved, map[string]any{"block_ids": in.GetBlockIds(), "page_id": from.Row.PageID, "target_page_id": to.Row.PageID})
		return nil
	})
	if err != nil {
		return nil, mapErr(err)
	}
	s.notify(ws, results, id.Principal)
	out := &kbv1.MoveBlocksResponse{}
	for i, r := range results {
		out.Versions = append(out.Versions, &kbv1.PageVersion{PageId: rows[i].PageID, Version: r.Seq})
	}
	for _, bid := range in.GetBlockIds() {
		if b := blockOf(results[1], rows[1], s.core, bid); b != nil {
			out.Blocks = append(out.Blocks, b)
		}
	}
	return connect.NewResponse(out), nil
}

// Delete removes blocks and their subtrees.
func (s *BlocksService) Delete(ctx context.Context, req *connect.Request[kbv1.DeleteBlocksRequest]) (*connect.Response[kbv1.DeleteBlocksResponse], error) {
	id, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	ws := req.Msg.GetWorkspaceId()
	if len(req.Msg.GetBlockIds()) == 0 {
		return nil, apierr.InvalidArgument("block_ids", "required")
	}
	loc, err := s.pageOf(ctx, id, ws, req.Msg.GetBlockIds()[0])
	if err != nil {
		return nil, err
	}
	res, _, err := s.mutateBlocks(ctx, id, ws, loc.docID, req.Msg.GetIfVersion(), func(o *opCtx) error {
		for _, bid := range req.Msg.GetBlockIds() {
			if _, ok := o.mu.State.ByID[bid]; !ok {
				return apierr.NotFound("block", bid)
			}
		}
		for _, bid := range req.Msg.GetBlockIds() {
			if err := o.del(&kbv1.BlockOp_Delete{BlockId: bid}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&kbv1.DeleteBlocksResponse{Version: res.Seq}), nil
}

// ListChildren pages through a block's direct children by rank.
func (s *BlocksService) ListChildren(ctx context.Context, req *connect.Request[kbv1.ListChildrenRequest]) (*connect.Response[kbv1.ListChildrenResponse], error) {
	id, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	ws, blockID := req.Msg.GetWorkspaceId(), req.Msg.GetBlockId()
	if err := s.check(ctx, id, ws, authz.View, authz.Workspace(ws)); err != nil {
		return nil, err
	}
	size := pageSize(req.Msg.GetPageSize())
	cur, err := decodeCursor(req.Msg.GetCursor(), 1)
	if err != nil {
		return nil, err
	}
	out := &kbv1.ListChildrenResponse{}
	err = s.DB.Tx(ctx, tenant(id, ws), func(ctx context.Context, tx pgx.Tx) error {
		loc, err := s.locate(ctx, tx, ws, blockID)
		if err != nil {
			return err
		}
		if err := s.check(ctx, id, ws, authz.View, authz.Doc(loc.docID, loc.projectID)); err != nil {
			return err
		}
		st, row, err := s.Mat.Read(ctx, tx, ws, loc.docID)
		if err != nil {
			return err
		}
		out.IndexedSeq = row.IndexedSeq
		children := st.Roots
		if blockID != row.PageID {
			n, ok := st.ByID[blockID]
			if !ok {
				return apierr.NotFound("block", blockID)
			}
			children = n.Children
		}
		for _, ch := range children {
			if cur[0] != "" && ch.FractionalIndex <= cur[0] {
				continue
			}
			if len(out.Blocks) == size {
				out.NextCursor = encodeCursor(out.Blocks[size-1].Rank)
				break
			}
			out.Blocks = append(out.Blocks, s.blockProto(ch, infoOf(row), len(ch.Path())+1))
		}
		return nil
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return connect.NewResponse(out), nil
}

// blocksByIDs reads projection rows into wire Blocks.
func (s *BlocksService) blocksByIDs(ctx context.Context, tx pgx.Tx, ws string, ids []string) ([]*kbv1.Block, error) {
	rows, err := tx.Query(ctx, `SELECT b.id, b.project_id, b.page_id, b.doc_id, COALESCE(b.parent_block_id::text, ''), b.rank, COALESCE(b.type_id::text, ''), COALESCE(b.key, ''),
			b.markdown, b.text, b.attributes, b.compliant, b.portal_doc_id IS NOT NULL, b.indexed_seq, b.created_by, b.updated_by, b.created_at, b.updated_at, b.depth,
			COALESCE((SELECT title FROM pages p WHERE p.workspace_id = b.workspace_id AND p.id = b.page_id), '')
		FROM blocks b WHERE b.workspace_id = $1 AND b.id = ANY($2::uuid[]) AND b.deleted_at IS NULL`, ws, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byID := map[string]*kbv1.Block{}
	for rows.Next() {
		var b kbv1.Block
		var attrs []byte
		var created, updated time.Time
		if err := rows.Scan(&b.Id, &b.ProjectId, &b.PageId, &b.DocId, &b.ParentBlockId, &b.Rank, &b.TypeId, &b.Key, &b.Markdown, &b.Text, &attrs, &b.Compliant, &b.Restricted, &b.Version, &b.CreatedBy, &b.UpdatedBy, &created, &updated, &b.Depth, &b.PageTitle); err != nil {
			return nil, err
		}
		b.WorkspaceId = ws
		b.Props = jsonStruct(attrs)
		b.Uri = s.uri(ws, b.Id)
		b.CreatedAt, b.UpdatedAt = ts(created), ts(updated)
		b.OwnsDoc = b.Restricted || b.Id == b.PageId
		byID[b.Id] = &b
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]*kbv1.Block, 0, len(ids))
	for _, id := range ids {
		if b, ok := byID[id]; ok {
			out = append(out, b)
		}
	}
	return out, nil
}

// ListEdges lists inline and relation edges of a block from the projection.
func (s *BlocksService) ListEdges(ctx context.Context, req *connect.Request[kbv1.ListEdgesRequest]) (*connect.Response[kbv1.ListEdgesResponse], error) {
	id, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	ws, blockID := req.Msg.GetWorkspaceId(), req.Msg.GetBlockId()
	if !validUUID(blockID) {
		return nil, apierr.NotFound("block", blockID)
	}
	if err := s.check(ctx, id, ws, authz.View, authz.Workspace(ws)); err != nil {
		return nil, err
	}
	size := pageSize(req.Msg.GetPageSize())
	cur, err := decodeCursor(req.Msg.GetCursor(), 1)
	if err != nil {
		return nil, err
	}
	var after int64
	if cur[0] != "" {
		if _, err := parseInt(cur[0], &after); err != nil {
			return nil, apierr.InvalidArgument("cursor", "invalid")
		}
	}
	kind := ""
	switch req.Msg.GetKind() {
	case kbv1.EdgeKind_EDGE_KIND_REF:
		kind = "ref"
	case kbv1.EdgeKind_EDGE_KIND_TAG:
		kind = "tag"
	case kbv1.EdgeKind_EDGE_KIND_EMBED:
		kind = "embed"
	case kbv1.EdgeKind_EDGE_KIND_RELATION:
		kind = "relation"
	}
	sets, err := s.Guard.ViewSets(ctx, id, ws)
	if err != nil {
		return nil, mapErr(err)
	}
	out := &kbv1.ListEdgesResponse{}
	err = s.DB.ReadTx(ctx, tenant(id, ws), func(ctx context.Context, tx pgx.Tx) error {
		loc, err := s.locate(ctx, tx, ws, blockID)
		if err != nil {
			return err
		}
		if err := s.check(ctx, id, ws, authz.View, authz.Doc(loc.docID, loc.projectID)); err != nil {
			return err
		}
		col := "e.source_block_id"
		if req.Msg.GetDirection() == kbv1.EdgeDirection_EDGE_DIRECTION_IN {
			col = "e.target_block_id"
		}
		rows, err := tx.Query(ctx, `SELECT e.id, e.source_block_id, COALESCE(e.target_block_id::text, ''), e.edge_kind, COALESCE(e.relation_type_id::text, ''), e.origin, COALESCE(e.target_title, '')
			FROM block_edges e WHERE e.workspace_id = $1 AND `+col+` = $2 AND e.id > $3 AND ($4 = '' OR e.edge_kind = $4) AND ($5 = '' OR e.relation_type_id = $5::uuid)
			  AND ($6::text[] IS NULL OR EXISTS (SELECT 1 FROM blocks b JOIN doc_access a ON a.workspace_id = b.workspace_id AND a.doc_id = b.doc_id
			         WHERE b.workspace_id = e.workspace_id AND b.id = e.source_block_id AND a.permission = 'view' AND a.set_id = ANY($6::text[])))
			ORDER BY e.id LIMIT $7`, ws, blockID, after, kind, req.Msg.GetRelationTypeId(), sets, size+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		var otherIDs []string
		var lastID int64
		for rows.Next() {
			var e kbv1.Edge
			var eid int64
			var k, origin string
			if err := rows.Scan(&eid, &e.SourceBlockId, &e.TargetBlockId, &k, &e.RelationTypeId, &origin, &e.TargetTitle); err != nil {
				return err
			}
			if len(out.Edges) == size {
				out.NextCursor = encodeCursor(itoa(int(lastID)))
				break
			}
			lastID = eid
			e.Kind = map[string]kbv1.EdgeKind{"ref": kbv1.EdgeKind_EDGE_KIND_REF, "tag": kbv1.EdgeKind_EDGE_KIND_TAG, "embed": kbv1.EdgeKind_EDGE_KIND_EMBED, "relation": kbv1.EdgeKind_EDGE_KIND_RELATION}[k]
			if origin == "property" {
				e.Origin = kbv1.EdgeOrigin_EDGE_ORIGIN_PROPERTY
			} else {
				e.Origin = kbv1.EdgeOrigin_EDGE_ORIGIN_INLINE
			}
			out.Edges = append(out.Edges, &e)
			if req.Msg.GetDirection() == kbv1.EdgeDirection_EDGE_DIRECTION_IN {
				otherIDs = append(otherIDs, e.SourceBlockId)
			} else if e.TargetBlockId != "" {
				otherIDs = append(otherIDs, e.TargetBlockId)
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if req.Msg.GetIncludeBlocks() && len(otherIDs) > 0 {
			out.Blocks, err = s.blocksByIDs(ctx, tx, ws, otherIDs)
		}
		return err
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return connect.NewResponse(out), nil
}

// Duplicate copies a subtree with fresh ids into the same or another page.
func (s *BlocksService) Duplicate(ctx context.Context, req *connect.Request[kbv1.DuplicateRequest]) (*connect.Response[kbv1.DuplicateResponse], error) {
	id, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	ws := req.Msg.GetWorkspaceId()
	src, err := s.pageOf(ctx, id, ws, req.Msg.GetBlockId())
	if err != nil {
		return nil, err
	}
	target := req.Msg.GetTargetPageId()
	if target == "" {
		target = src.pageID
	}
	docs := []string{src.docID}
	if target != src.pageID {
		if !validUUID(target) {
			return nil, apierr.NotFound("page", target)
		}
		docs = append(docs, target)
	}
	var ids []string
	var snaps []*schema.Snapshot
	var rows []*truth.DocRow
	results, err := s.Mat.Mutate(ctx, tenant(id, ws), docs, truth.Options{Actor: id.Principal, OnCommit: func(ctx context.Context, tx pgx.Tx, results []*truth.Result) error {
		rows = make([]*truth.DocRow, len(results))
		for i, r := range results {
			fresh, err := s.Truth.GetDoc(ctx, tx, ws, r.DocID)
			if err != nil {
				return err
			}
			rows[i] = fresh
			if r.Changed {
				if err := s.index(ctx, tx, fresh, r.State, r.Seq, id.Principal, snaps[i]); err != nil {
					return err
				}
			}
		}
		return nil
	}}, func(ctx context.Context, tx pgx.Tx, muts []*truth.Mutation) error {
		snaps = make([]*schema.Snapshot, len(muts))
		for i, d := range muts {
			perm := authz.View
			if i == len(muts)-1 {
				perm = authz.Edit
			}
			if err := s.check(ctx, id, ws, perm, authz.Doc(d.Row.ID, d.Row.ProjectID)); err != nil {
				return err
			}
			var err error
			snaps[i], err = s.Schema.Load(ctx, tx, ws, d.Row.ProjectID)
			if err != nil {
				return err
			}
		}
		from, to := muts[0], muts[len(muts)-1]
		n, ok := from.State.ByID[req.Msg.GetBlockId()]
		if !ok {
			return apierr.NotFound("block", req.Msg.GetBlockId())
		}
		o := &opCtx{c: s.core, id: id, mu: to, snap: snaps[len(snaps)-1], created: map[string]bool{}, now: s.Clock(), tx: tx, ctx: ctx}
		parent, err := o.tree(req.Msg.GetParentBlockId())
		if err != nil {
			return err
		}
		idx, err := o.index(req.Msg.GetParentBlockId(), req.Msg.GetPosition())
		if err != nil {
			return err
		}
		nowStr := s.Clock().UTC().Format(time.RFC3339Nano)
		var rec func(n *loro.Node, parent string, index int)
		rec = func(n *loro.Node, parent string, index int) {
			newID := newID()
			ids = append(ids, newID)
			props := map[string]any{}
			for k, v := range n.Props {
				if k != schema.KeyProp {
					props[k] = v
				}
			}
			to.Add(loro.TreeCreate(newID, parent, index, &loro.TreeCreateInit{Content: n.Content, TypeID: n.TypeID, CreatedAt: nowStr, CreatedBy: id.Principal, Props: props}))
			for _, ch := range n.Children {
				rec(ch, loro.Placeholder(newID), -1)
			}
		}
		rec(n, parent, idx)
		to.Emit(truth.EventBlockCreated, map[string]any{"block_ids": ids, "page_id": to.Row.PageID, "duplicated_from": req.Msg.GetBlockId()})
		return nil
	})
	if err != nil {
		return nil, mapErr(err)
	}
	s.notify(ws, results, id.Principal)
	last := len(results) - 1
	out := &kbv1.DuplicateResponse{Version: results[last].Seq}
	for _, bid := range ids {
		if b := blockOf(results[last], rows[last], s.core, bid); b != nil {
			out.Blocks = append(out.Blocks, b)
		}
	}
	return connect.NewResponse(out), nil
}

func parentOf(n *loro.Node, pageID string) string {
	if n.Parent != nil {
		return n.Parent.ID
	}
	return pageID
}

var _ = errors.New
