package api

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	kbv1 "github.com/acx1729/ocean/gen/kb/v1"
)

func TestBoundariesSplitAndMerge(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	_, tok := e.user()
	created := must(e.ws.Create(ctx, as(tok, &kbv1.CreateWorkspaceRequest{Slug: "bnd", Name: "Boundaries"})))
	ws, project := created.Workspace.Id, created.Project
	page := must(e.pages.Create(ctx, as(tok, &kbv1.CreatePageRequest{WorkspaceId: ws, ProjectId: project.Id, Title: "Plan", Format: kbv1.PageFormat_PAGE_FORMAT_OUTLINER, Markdown: "- top\n  - secret\n    - deeper\n- after"})))
	if len(page.Blocks) != 4 {
		t.Fatalf("blocks: %d", len(page.Blocks))
	}
	secret := page.Blocks[1]

	// Restrict splits the subtree into its own doc; the page still reads as one tree.
	r := must(e.blocks.Restrict(ctx, as(tok, &kbv1.RestrictRequest{WorkspaceId: ws, BlockId: secret.Id, Inherit: true})))
	if r.DocId == "" || r.DocId == page.Page.DocId {
		t.Fatalf("restrict: %+v", r)
	}
	got := must(e.pages.Get(ctx, as(tok, &kbv1.GetPageRequest{WorkspaceId: ws, Ref: &kbv1.GetPageRequest_PageId{PageId: page.Page.Id}})))
	if len(got.Blocks) != 4 || got.Blocks[1].Id != secret.Id || !got.Blocks[1].Restricted || got.Blocks[1].DocId != r.DocId || got.Blocks[1].Markdown != "secret" || got.Blocks[2].DocId != r.DocId || got.Blocks[2].ParentBlockId != secret.Id || got.Blocks[3].Markdown != "after" {
		for _, b := range got.Blocks {
			t.Logf("%s doc=%s parent=%s restricted=%v md=%q", b.Id, b.DocId, b.ParentBlockId, b.Restricted, b.Markdown)
		}
		t.Fatal("spliced tree mismatch")
	}
	md := must(e.pages.Get(ctx, as(tok, &kbv1.GetPageRequest{WorkspaceId: ws, Ref: &kbv1.GetPageRequest_PageId{PageId: page.Page.Id}, Format: kbv1.ContentFormat_CONTENT_FORMAT_MARKDOWN})))
	if md.Markdown != "- top\n  - secret\n    - deeper\n- after\n" {
		t.Fatalf("markdown through boundary: %q", md.Markdown)
	}
	// Edits inside the boundary go to the boundary doc.
	ins := must(e.blocks.Create(ctx, as(tok, &kbv1.CreateBlockRequest{WorkspaceId: ws, PageId: r.DocId, ParentBlockId: secret.Id, Markdown: "inside"})))
	if ins.Blocks[0].DocId != r.DocId {
		t.Fatalf("inside block doc: %s", ins.Blocks[0].DocId)
	}
	upd := must(e.blocks.Update(ctx, as(tok, &kbv1.UpdateBlockRequest{WorkspaceId: ws, BlockId: secret.Id, Markdown: "secret plan"})))
	if upd.Block.DocId != r.DocId || upd.Block.Markdown != "secret plan" {
		t.Fatalf("update boundary root: %+v", upd.Block)
	}
	if _, err := e.blocks.Restrict(ctx, as(tok, &kbv1.RestrictRequest{WorkspaceId: ws, BlockId: secret.Id})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("double restrict: %v", err)
	}
	// Projection rows follow the doc split and keep the page-relative paths.
	err := e.db.System(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var n int
		var depth int
		if err := tx.QueryRow(ctx, `SELECT count(*), max(depth) FROM blocks WHERE workspace_id = $1 AND doc_id = $2`, ws, r.DocId).Scan(&n, &depth); err != nil {
			return err
		}
		if n != 3 || depth != 3 {
			return fmt.Errorf("boundary projection: n=%d depth=%d", n, depth)
		}
		var hasACL bool
		if err := tx.QueryRow(ctx, `SELECT has_acl FROM blocks WHERE workspace_id = $1 AND id = $2`, ws, secret.Id).Scan(&hasACL); err != nil {
			return err
		}
		if !hasACL {
			return errors.New("boundary root must be flagged has_acl")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Unrestrict merges everything back into the page doc.
	must(e.blocks.Unrestrict(ctx, as(tok, &kbv1.UnrestrictRequest{WorkspaceId: ws, BlockId: secret.Id})))
	got = must(e.pages.Get(ctx, as(tok, &kbv1.GetPageRequest{WorkspaceId: ws, Ref: &kbv1.GetPageRequest_PageId{PageId: page.Page.Id}})))
	if len(got.Blocks) != 5 || got.Blocks[1].Restricted || got.Blocks[1].DocId != page.Page.DocId || got.Blocks[1].Markdown != "secret plan" {
		t.Fatalf("merged tree: %+v", got.Blocks[1])
	}
	err = e.db.System(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM docs WHERE workspace_id = $1 AND id = $2`, ws, r.DocId).Scan(&n); err != nil {
			return err
		}
		if n != 0 {
			return errors.New("boundary doc must be purged after unrestrict")
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM blocks WHERE workspace_id = $1 AND page_id = $2 AND doc_id = $3`, ws, page.Page.Id, page.Page.DocId).Scan(&n)
	})
	if err != nil {
		t.Fatal(err)
	}
	// Deleting a portal removes its boundary doc as well.
	r2 := must(e.blocks.Restrict(ctx, as(tok, &kbv1.RestrictRequest{WorkspaceId: ws, BlockId: secret.Id, Inherit: false})))
	must(e.blocks.Delete(ctx, as(tok, &kbv1.DeleteBlocksRequest{WorkspaceId: ws, BlockIds: []string{page.Blocks[0].Id}})))
	got = must(e.pages.Get(ctx, as(tok, &kbv1.GetPageRequest{WorkspaceId: ws, Ref: &kbv1.GetPageRequest_PageId{PageId: page.Page.Id}})))
	if len(got.Blocks) != 1 || got.Blocks[0].Markdown != "after" {
		t.Fatalf("after deleting the portal's ancestor: %+v", got.Blocks)
	}
	err = e.db.System(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var deleted bool
		if err := tx.QueryRow(ctx, `SELECT deleted_at IS NOT NULL FROM docs WHERE workspace_id = $1 AND id = $2`, ws, r2.DocId).Scan(&deleted); err != nil {
			return err
		}
		if !deleted {
			return errors.New("boundary doc must be trashed with its portal")
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM blocks WHERE workspace_id = $1 AND doc_id = $2`, ws, r2.DocId).Scan(&n); err != nil {
			return err
		}
		if n != 0 {
			return fmt.Errorf("boundary rows left behind: %d", n)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
