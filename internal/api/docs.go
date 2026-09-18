package api

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

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

// docInfo carries what block mapping needs about the enclosing doc.
type docInfo struct {
	ws, projectID, pageID, docID string
	seq                          int64
	updatedAt                    time.Time
	boundary                     bool
}

func infoOf(row *truth.DocRow) docInfo {
	return docInfo{ws: row.WorkspaceID, projectID: row.ProjectID, pageID: row.PageID, docID: row.ID, seq: row.CurrentSeq, updatedAt: row.UpdatedAt, boundary: row.Kind == truth.KindBoundary}
}

// publicProps strips node-managed keys (prefixed "_") from a bag.
func publicProps(m map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		if !strings.HasPrefix(k, "_") && k != schema.KeyProp {
			out[k] = v
		}
	}
	return out
}

func stringProp(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func parseTime(s string, fallback time.Time) time.Time {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	return fallback
}

// blockProto maps a tree node to the wire Block. parentID and path override
// the node's own (used when a boundary root is spliced into its parent page).
func (c *core) blockProto(n *loro.Node, di docInfo, depth int) *kbv1.Block {
	parent := di.pageID
	if n.Parent != nil {
		parent = n.Parent.ID
	}
	return c.blockProtoAt(n, di, depth, parent, append([]string{di.pageID}, n.Path()...))
}

func (c *core) blockProtoAt(n *loro.Node, di docInfo, depth int, parent string, path []string) *kbv1.Block {
	boundaryRoot := di.boundary && n.Parent == nil
	return &kbv1.Block{
		Id: n.ID, WorkspaceId: di.ws, ProjectId: di.projectID, PageId: di.pageID, DocId: di.docID, ParentBlockId: parent,
		Rank: n.FractionalIndex, TypeId: n.TypeID, Markdown: n.Content, Props: toStruct(publicProps(n.Props)), Path: path,
		Version: di.seq, Uri: c.uri(di.ws, n.ID), Compliant: n.Props["_incomplete"] != true, Restricted: n.PortalDocID != "" || boundaryRoot,
		CreatedAt: ts(parseTime(n.CreatedAt, di.updatedAt)), UpdatedAt: ts(di.updatedAt), CreatedBy: n.CreatedBy,
		Key: stringProp(n.Props, schema.KeyProp), OwnsDoc: n.PortalDocID != "" || boundaryRoot, Depth: int32(depth), Text: n.Content,
	}
}

// portalProto is what a caller without access to a boundary sees: the id only.
func (c *core) portalProto(n *loro.Node, di docInfo, depth int, parent string, path []string) *kbv1.Block {
	return &kbv1.Block{
		Id: n.ID, WorkspaceId: di.ws, ProjectId: di.projectID, PageId: di.pageID, DocId: n.PortalDocID, ParentBlockId: parent,
		Rank: n.FractionalIndex, Path: path, Version: di.seq, Uri: c.uri(di.ws, n.ID), Restricted: true, OwnsDoc: true, Depth: int32(depth),
		Props: toStruct(nil),
	}
}

// xnode is a block in the visible tree of a page, boundaries spliced in.
type xnode struct {
	n        *loro.Node
	di       docInfo
	depth    int
	parent   string
	path     []string
	denied   bool
	children []*xnode
}

// expand builds the visible tree under nodes: a portal is replaced by the
// boundary doc's own block when the caller may view that doc, and reduced to
// a stub otherwise. depth is the depth of nodes; maxDepth 0 = unlimited.
func (c *core) expand(ctx context.Context, tx pgx.Tx, id *auth.Identity, di docInfo, nodes []*loro.Node, parent string, path []string, depth, maxDepth int) ([]*xnode, error) {
	if maxDepth > 0 && depth > maxDepth {
		return nil, nil
	}
	var out []*xnode
	for _, n := range nodes {
		if n.PortalDocID != "" {
			if err := c.Guard.Check(ctx, id, di.ws, authz.View, authz.Doc(n.PortalDocID, di.projectID)); err != nil {
				out = append(out, &xnode{n: n, di: di, depth: depth, parent: parent, path: path, denied: true})
				continue
			}
			bst, brow, err := c.Mat.Read(ctx, tx, di.ws, n.PortalDocID)
			if err != nil {
				if truth.IsNotFound(err) {
					out = append(out, &xnode{n: n, di: di, depth: depth, parent: parent, path: path, denied: true})
					continue
				}
				return nil, err
			}
			bdi := infoOf(brow)
			bdi.pageID = di.pageID
			roots, err := c.expand(ctx, tx, id, bdi, bst.Roots, parent, path, depth, maxDepth)
			if err != nil {
				return nil, err
			}
			out = append(out, roots...)
			continue
		}
		x := &xnode{n: n, di: di, depth: depth, parent: parent, path: path}
		childPath := append(append([]string{}, path...), n.ID)
		children, err := c.expand(ctx, tx, id, di, n.Children, n.ID, childPath, depth+1, maxDepth)
		if err != nil {
			return nil, err
		}
		x.children = children
		out = append(out, x)
	}
	return out, nil
}

// flattenX serializes an expanded tree in pre-order.
func (c *core) flattenX(xs []*xnode) []*kbv1.Block {
	var out []*kbv1.Block
	var walk func(xs []*xnode)
	walk = func(xs []*xnode) {
		for _, x := range xs {
			if x.denied {
				out = append(out, c.portalProto(x.n, x.di, x.depth, x.parent, x.path))
				continue
			}
			out = append(out, c.blockProtoAt(x.n, x.di, x.depth, x.parent, x.path))
			walk(x.children)
		}
	}
	walk(xs)
	return out
}

// specsOfX converts an expanded tree to block specs for Markdown output.
func specsOfX(xs []*xnode) []*BlockSpec {
	out := make([]*BlockSpec, 0, len(xs))
	for _, x := range xs {
		if x.denied {
			out = append(out, &BlockSpec{Markdown: "(restricted)"})
			continue
		}
		out = append(out, &BlockSpec{Markdown: x.n.Content, Children: specsOfX(x.children)})
	}
	return out
}

// pageTree expands a whole page doc.
func (c *core) pageTree(ctx context.Context, tx pgx.Tx, id *auth.Identity, row *truth.DocRow, st *loro.State, maxDepth int) ([]*xnode, error) {
	return c.expand(ctx, tx, id, infoOf(row), st.Roots, row.PageID, []string{row.PageID}, 1, maxDepth)
}

// pageBlockProto maps the page itself to a Block row (id = page id).
func (c *core) pageBlockProto(st *loro.State, row *truth.DocRow) *kbv1.Block {
	return &kbv1.Block{
		Id: row.PageID, WorkspaceId: row.WorkspaceID, ProjectId: row.ProjectID, PageId: row.PageID, DocId: row.ID,
		TypeId: st.Meta.TypeID, Markdown: st.Meta.Title, Text: st.Meta.Title, Props: toStruct(publicProps(st.Meta.Props)), Path: []string{},
		Version: row.CurrentSeq, Uri: c.uri(row.WorkspaceID, row.PageID), Compliant: true, CreatedAt: ts(row.CreatedAt), UpdatedAt: ts(row.UpdatedAt),
		Key: stringProp(st.Meta.Props, schema.KeyProp), OwnsDoc: true,
	}
}

func formatEnum(f string) kbv1.PageFormat {
	if f == "outliner" {
		return kbv1.PageFormat_PAGE_FORMAT_OUTLINER
	}
	return kbv1.PageFormat_PAGE_FORMAT_MARKDOWN
}

func formatName(f kbv1.PageFormat) string {
	if f == kbv1.PageFormat_PAGE_FORMAT_OUTLINER {
		return "outliner"
	}
	return "markdown"
}

// pageProto maps a doc state to the wire Page.
func (c *core) pageProto(st *loro.State, row *truth.DocRow) *kbv1.Page {
	return &kbv1.Page{
		Id: row.PageID, ProjectId: row.ProjectID, DocId: row.ID, Title: st.Meta.Title, Icon: st.Meta.Icon,
		ParentPageId: stringProp(st.Meta.Props, "_parent_page_id"), JournalDate: st.Meta.JournalDate, Format: formatEnum(st.Meta.Format),
		TypeId: st.Meta.TypeID, Props: toStruct(publicProps(st.Meta.Props)), Version: row.CurrentSeq, Uri: c.uri(row.WorkspaceID, row.PageID),
		Key: stringProp(st.Meta.Props, schema.KeyProp), CreatedAt: ts(row.CreatedAt), UpdatedAt: ts(row.UpdatedAt), WorkspaceId: row.WorkspaceID,
		IndexedSeq: row.IndexedSeq, DeletedAt: tsPtr(row.DeletedAt),
	}
}

// flatten returns blocks in tree order down to maxDepth (0 = unlimited).
func (c *core) flatten(roots []*loro.Node, di docInfo, maxDepth int) []*kbv1.Block {
	var out []*kbv1.Block
	var walk func(n *loro.Node, depth int)
	walk = func(n *loro.Node, depth int) {
		if maxDepth > 0 && depth > maxDepth {
			return
		}
		out = append(out, c.blockProto(n, di, depth))
		for _, ch := range n.Children {
			walk(ch, depth+1)
		}
	}
	for _, r := range roots {
		walk(r, 1)
	}
	return out
}

// specsOf converts a subtree to block specs for Markdown output.
func specsOf(nodes []*loro.Node) []*BlockSpec {
	out := make([]*BlockSpec, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, &BlockSpec{Markdown: n.Content, Children: specsOf(n.Children)})
	}
	return out
}

// createFromSpecs emits TreeCreate ops for specs under parentTreeID at index
// (-1 = append) and returns the new block ids in creation order.
func (c *core) createFromSpecs(mu *truth.Mutation, specs []*BlockSpec, parentTreeID string, index int, actor string, now time.Time, snap *schema.Snapshot, typeID string, firstID string) []string {
	var ids []string
	nowStr := now.UTC().Format(time.RFC3339Nano)
	var rec func(specs []*BlockSpec, parent string, index int, typeID string)
	rec = func(specs []*BlockSpec, parent string, index int, typeID string) {
		for _, sp := range specs {
			id := newID()
			if firstID != "" && len(ids) == 0 {
				id = firstID
			}
			init := &loro.TreeCreateInit{Content: sp.Markdown, CreatedBy: actor, CreatedAt: nowStr, TypeID: typeID}
			if len(sp.Props) > 0 && snap != nil {
				init.Props = map[string]any{}
				for name, raw := range sp.Props {
					if p, ok := snap.PropsByName[name]; ok {
						init.Props[p.ID] = schema.Normalize(p, raw)
					}
				}
			}
			mu.Add(loro.TreeCreate(id, parent, index, init))
			ids = append(ids, id)
			if index >= 0 {
				index++
			}
			rec(sp.Children, loro.Placeholder(id), -1, "")
		}
	}
	rec(specs, parentTreeID, index, typeID)
	return ids
}

// resolveIndex turns a Position into a child index among siblings.
func resolveIndex(siblings []*loro.Node, pos *kbv1.Position) (int, error) {
	if pos == nil {
		return -1, nil
	}
	switch at := pos.GetAt().(type) {
	case *kbv1.Position_First:
		return 0, nil
	case *kbv1.Position_Last:
		return -1, nil
	case *kbv1.Position_After:
		for i, s := range siblings {
			if s.ID == at.After {
				return i + 1, nil
			}
		}
		return 0, apierr.InvalidArgument("position.after", "not a sibling under the target parent")
	}
	return -1, nil
}

// allocateKey issues the next project key (KB-123) for a block.
func (c *core) allocateKey(ctx context.Context, tx pgx.Tx, ws, project, blockID string) (string, error) {
	var prefix string
	var n int64
	err := tx.QueryRow(ctx, `UPDATE project_counters SET next_number = next_number + 1 WHERE workspace_id = $1 AND project_id = $2 RETURNING prefix, next_number - 1`, ws, project).Scan(&prefix, &n)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", apierr.FailedPrecondition("project has no key counter")
		}
		return "", err
	}
	key := fmt.Sprintf("%s-%d", prefix, n)
	if _, err := tx.Exec(ctx, `UPDATE block_keys SET current = false WHERE workspace_id = $1 AND block_id = $2 AND current`, ws, blockID); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO block_keys (workspace_id, key, block_id) VALUES ($1, $2, $3)`, ws, key, blockID); err != nil {
		return "", err
	}
	return key, nil
}

// blockByKey resolves KB-123 to a block id.
func (c *core) blockByKey(ctx context.Context, tx pgx.Tx, ws, key string) (string, error) {
	var id string
	err := tx.QueryRow(ctx, `SELECT block_id FROM block_keys WHERE workspace_id = $1 AND key = $2`, ws, strings.ToUpper(strings.TrimSpace(key))).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", apierr.NotFound("key", key)
	}
	return id, err
}

// locate finds the doc and page of a block through the projection.
type location struct {
	pageID, docID, projectID string
	portalDocID              string
}

func (c *core) locate(ctx context.Context, tx pgx.Tx, ws, blockID string) (*location, error) {
	var l location
	var portal *string
	err := tx.QueryRow(ctx, `SELECT page_id, doc_id, project_id, portal_doc_id::text FROM blocks WHERE workspace_id = $1 AND id = $2`, ws, blockID).Scan(&l.pageID, &l.docID, &l.projectID, &portal)
	if errors.Is(err, pgx.ErrNoRows) {
		// A page id names the page block itself.
		row, derr := c.Truth.GetDoc(ctx, tx, ws, blockID)
		if derr != nil {
			return nil, apierr.NotFound("block", blockID)
		}
		return &location{pageID: row.PageID, docID: row.ID, projectID: row.ProjectID}, nil
	}
	if err != nil {
		return nil, err
	}
	if portal != nil {
		l.portalDocID = *portal
	}
	return &l, nil
}

// index writes the projection of a doc from a mutation result.
func (c *core) index(ctx context.Context, tx pgx.Tx, row *truth.DocRow, st *loro.State, seq int64, actor string, snap *schema.Snapshot) error {
	return projection.IndexDoc(ctx, tx, projection.Input{
		WorkspaceID: row.WorkspaceID, ProjectID: row.ProjectID, PageID: row.PageID, DocID: row.ID, PortalBlockID: row.PortalBlockID,
		State: st, Seq: seq, Actor: actor, Now: c.Clock(), Snap: snap, CreatedAt: row.CreatedAt, DeletedAt: row.DeletedAt,
	})
}

// deleteBoundaryDoc trashes a boundary doc whose portal is being deleted and
// drops its projection rows.
func (c *core) deleteBoundaryDoc(ctx context.Context, tx pgx.Tx, ws, docID string, now time.Time) error {
	if err := c.Truth.SetDocDeleted(ctx, tx, ws, docID, &now); err != nil {
		return err
	}
	return projection.DeleteDoc(ctx, tx, ws, docID)
}

// notify hands committed updates to the sync layer.
func (c *core) notify(ws string, results []*truth.Result, actor string) {
	if c.Notify == nil {
		return
	}
	for _, r := range results {
		if r.Changed {
			c.Notify(ws, r.DocID, r.Seq, r.Update, actor)
		}
	}
}
