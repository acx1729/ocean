package api

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	kbv1 "github.com/acx1729/ocean/gen/kb/v1"
	"github.com/acx1729/ocean/gen/kb/v1/kbv1connect"
	"github.com/acx1729/ocean/internal/auth"
	"github.com/acx1729/ocean/internal/authz"
	"github.com/acx1729/ocean/internal/config"
	"github.com/acx1729/ocean/internal/db"
	"github.com/acx1729/ocean/internal/did"
	"github.com/acx1729/ocean/internal/keyring"
	"github.com/acx1729/ocean/internal/testutil"
	"github.com/acx1729/ocean/internal/truth"
)

type keyStore struct {
	db      *db.DB
	nodeDID string
}

func (k *keyStore) WrappedKey(ctx context.Context, ws string, v int) ([]byte, error) {
	var w []byte
	err := k.db.System(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT wrapped_key FROM workspace_keys WHERE workspace_id = $1 AND key_version = $2 AND recipient_id = $3`, ws, v, k.nodeDID).Scan(&w)
	})
	return w, err
}

func (k *keyStore) CurrentVersion(ctx context.Context, ws string) (int, error) {
	var v int
	err := k.db.System(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT current_key_version FROM workspaces WHERE id = $1`, ws).Scan(&v)
	})
	return v, err
}

type env struct {
	t      *testing.T
	db     *db.DB
	url    string
	client *http.Client
	auth   kbv1connect.AuthServiceClient
	agents kbv1connect.AgentsServiceClient
	ws     kbv1connect.WorkspacesServiceClient
	proj   kbv1connect.ProjectsServiceClient
	mem    kbv1connect.MembersServiceClient
	schema kbv1connect.SchemaServiceClient
	pages  kbv1connect.PagesServiceClient
	blocks kbv1connect.BlocksServiceClient
}

func newEnv(t *testing.T) *env {
	t.Helper()
	d := testutil.NewDB(t)
	cfg, err := config.Load(func(k string) string {
		return map[string]string{
			"KB_PUBLIC_URL": "https://kb.example.com", "KB_DATABASE_URL": "x", "KB_FGA_DATABASE_URL": "x", "KB_RIVER_DATABASE_URL": "x",
			"KB_NATS_URL": "x", "KB_OBJECT_STORE_DIR": "/tmp", "KB_NODE_KEY_PASSPHRASE": "passphrase-for-tests",
		}[k]
	})
	if err != nil {
		t.Fatal(err)
	}
	node, _ := keyring.GenerateNodeKey()
	ctx := context.Background()
	if err := d.System(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO principals (id, kind) VALUES ($1, 'node')`, node.DID())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	keys := keyring.New(node, &keyStore{db: d, nodeDID: node.DID()}, keyring.Options{})
	tokens, _ := auth.NewTokenIssuer(node, cfg.PublicURL, cfg.SessionTTL)
	store := auth.NewStore(d)
	authn := auth.NewAuthenticator(auth.AuthenticatorOptions{Tokens: tokens, Store: store, Limiter: auth.NewMemoryLimiter(nil), Limits: cfg.Limits})
	ts := truth.NewStore(keys, nil)
	svcs := New(Deps{Config: cfg, DB: d, Keys: keys, Truth: ts, Mat: truth.NewMaterializer(ts, d, 8), Guard: authz.NewRoleGuard(store), AuthStore: store})
	mux := http.NewServeMux()
	opts := connect.WithInterceptors(auth.NewInterceptor(authn))
	mux.Handle(kbv1connect.NewAuthServiceHandler(auth.NewService(cfg, store, tokens, nil, nil), opts))
	mux.Handle(kbv1connect.NewAgentsServiceHandler(auth.NewAgentsService(store, authn, nil, nil), opts))
	mux.Handle(kbv1connect.NewWorkspacesServiceHandler(svcs.Workspaces, opts))
	mux.Handle(kbv1connect.NewProjectsServiceHandler(svcs.Projects, opts))
	mux.Handle(kbv1connect.NewMembersServiceHandler(svcs.Members, opts))
	mux.Handle(kbv1connect.NewSchemaServiceHandler(svcs.Schema, opts))
	mux.Handle(kbv1connect.NewPagesServiceHandler(svcs.Pages, opts))
	mux.Handle(kbv1connect.NewBlocksServiceHandler(svcs.Blocks, opts))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	curT = t
	e := &env{t: t, db: d, url: srv.URL, client: srv.Client()}
	e.auth = kbv1connect.NewAuthServiceClient(e.client, e.url)
	e.agents = kbv1connect.NewAgentsServiceClient(e.client, e.url)
	e.ws = kbv1connect.NewWorkspacesServiceClient(e.client, e.url)
	e.proj = kbv1connect.NewProjectsServiceClient(e.client, e.url)
	e.mem = kbv1connect.NewMembersServiceClient(e.client, e.url)
	e.schema = kbv1connect.NewSchemaServiceClient(e.client, e.url)
	e.pages = kbv1connect.NewPagesServiceClient(e.client, e.url)
	e.blocks = kbv1connect.NewBlocksServiceClient(e.client, e.url)
	return e
}

// user signs in a fresh did:key principal and returns its DID and access token.
func (e *env) user() (string, string) {
	e.t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	d := did.FromEd25519(pub)
	ch, err := e.auth.Challenge(context.Background(), connect.NewRequest(&kbv1.ChallengeRequest{Did: d}))
	if err != nil {
		e.t.Fatal(err)
	}
	res, err := e.auth.Verify(context.Background(), connect.NewRequest(&kbv1.VerifyRequest{Did: d, Nonce: ch.Msg.Nonce, Signature: ed25519.Sign(priv, []byte(ch.Msg.Message))}))
	if err != nil {
		e.t.Fatal(err)
	}
	return d, res.Msg.AccessToken
}

func as[T any](token string, msg *T, headers ...string) *connect.Request[T] {
	req := connect.NewRequest(msg)
	req.Header().Set("Authorization", "Bearer "+token)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header().Set(headers[i], headers[i+1])
	}
	return req
}

// curT is the running test, so must can be applied directly to RPC results.
var curT *testing.T

func must[T any](res *connect.Response[T], err error) *T {
	curT.Helper()
	if err != nil {
		curT.Fatalf("%v", err)
	}
	return res.Msg
}

func code(err error) connect.Code { return connect.CodeOf(err) }

func TestEndToEndPagesAndBlocks(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	alice, tok := e.user()

	created := must(e.ws.Create(ctx, as(tok, &kbv1.CreateWorkspaceRequest{Slug: "acme", Name: "Acme"})))
	ws, project := created.Workspace.Id, created.Project
	if project.Slug != "main" || project.KeyPrefix != "MAIN" || created.Workspace.Role != "admin" {
		t.Fatalf("workspace bootstrap: %+v %+v", created.Workspace, project)
	}
	types := must(e.schema.ListTypes(ctx, as(tok, &kbv1.ListTypesRequest{WorkspaceId: ws, ProjectId: project.Id})))
	byName := map[string]*kbv1.BlockType{}
	for _, ty := range types.Types {
		byName[ty.Name] = ty
	}
	if len(types.Types) != 7 || byName["task"] == nil || !byName["page"].OwnsDoc {
		t.Fatalf("shipped types: %d", len(types.Types))
	}
	props := must(e.schema.ListProperties(ctx, as(tok, &kbv1.ListPropertiesRequest{WorkspaceId: ws, ProjectId: project.Id})))
	propByName := map[string]*kbv1.PropertyDefinition{}
	for _, p := range props.Properties {
		propByName[p.Name] = p
	}
	status := propByName["status"]

	// Create a page from Markdown: a heading with two paragraphs beneath it.
	page := must(e.pages.Create(ctx, as(tok, &kbv1.CreatePageRequest{WorkspaceId: ws, ProjectId: project.Id, Title: "Roadmap", Markdown: "# Q4\n\nship R1\n\nwrite docs"})))
	if page.Page.Title != "Roadmap" || page.Page.Version != 1 || len(page.Blocks) != 3 || page.Blocks[0].Markdown != "# Q4" || page.Blocks[1].ParentBlockId != page.Blocks[0].Id || page.Blocks[2].Depth != 2 {
		t.Fatalf("create page: %+v", page)
	}
	if !strings.HasPrefix(page.Page.Uri, "kb://"+ws+"/") {
		t.Fatalf("uri: %s", page.Page.Uri)
	}
	pageID := page.Page.Id
	heading, para1, para2 := page.Blocks[0], page.Blocks[1], page.Blocks[2]

	// Read back as tree and as Markdown.
	got := must(e.pages.Get(ctx, as(tok, &kbv1.GetPageRequest{WorkspaceId: ws, Ref: &kbv1.GetPageRequest_PageId{PageId: pageID}})))
	if len(got.Blocks) != 3 || got.Blocks[2].Id != para2.Id {
		t.Fatalf("get tree: %+v", got.Blocks)
	}
	md := must(e.pages.Get(ctx, as(tok, &kbv1.GetPageRequest{WorkspaceId: ws, Ref: &kbv1.GetPageRequest_Uri{Uri: page.Page.Uri}, Format: kbv1.ContentFormat_CONTENT_FORMAT_MARKDOWN})))
	if md.Markdown != "# Q4\n\nship R1\n\nwrite docs\n" {
		t.Fatalf("markdown: %q", md.Markdown)
	}

	// Insert a block after the first paragraph; order is preserved.
	ins := must(e.blocks.Create(ctx, as(tok, &kbv1.CreateBlockRequest{WorkspaceId: ws, PageId: pageID, ParentBlockId: heading.Id, Position: &kbv1.Position{At: &kbv1.Position_After{After: para1.Id}}, Markdown: "review [[Roadmap]] with #team"})))
	if len(ins.Blocks) != 1 || ins.Version != 2 {
		t.Fatalf("insert: %+v", ins)
	}
	mid := ins.Blocks[0]
	got = must(e.pages.Get(ctx, as(tok, &kbv1.GetPageRequest{WorkspaceId: ws, Ref: &kbv1.GetPageRequest_PageId{PageId: pageID}})))
	if ids := []string{got.Blocks[1].Id, got.Blocks[2].Id, got.Blocks[3].Id}; ids[0] != para1.Id || ids[1] != mid.Id || ids[2] != para2.Id {
		t.Fatalf("order after insert: %v", ids)
	}
	// The projection resolved the inline link and the tag.
	edges := must(e.blocks.ListEdges(ctx, as(tok, &kbv1.ListEdgesRequest{WorkspaceId: ws, BlockId: pageID, Direction: kbv1.EdgeDirection_EDGE_DIRECTION_IN, IncludeBlocks: true})))
	if len(edges.Edges) != 1 || edges.Edges[0].SourceBlockId != mid.Id || len(edges.Blocks) != 1 || edges.Blocks[0].PageTitle != "Roadmap" {
		t.Fatalf("backlinks: %+v", edges)
	}
	out := must(e.blocks.ListEdges(ctx, as(tok, &kbv1.ListEdgesRequest{WorkspaceId: ws, BlockId: mid.Id, Direction: kbv1.EdgeDirection_EDGE_DIRECTION_OUT})))
	if len(out.Edges) != 2 {
		t.Fatalf("outgoing edges: %+v", out.Edges)
	}

	// Update content and properties; set a type that allocates a key.
	upd := must(e.blocks.Update(ctx, as(tok, &kbv1.UpdateBlockRequest{WorkspaceId: ws, BlockId: para1.Id, Markdown: "ship R1 on time"})))
	if upd.Block.Markdown != "ship R1 on time" || upd.Version != 3 {
		t.Fatalf("update: %+v", upd)
	}
	sp := must(e.blocks.SetProperties(ctx, as(tok, &kbv1.SetPropertiesRequest{WorkspaceId: ws, BlockId: para1.Id, Props: structOf(t, map[string]any{status.Id: "Done"})})))
	if sp.Block.Props.AsMap()[status.Id] != "done" {
		t.Fatalf("select normalization: %v", sp.Block.Props.AsMap())
	}
	if _, err := e.blocks.SetProperties(ctx, as(tok, &kbv1.SetPropertiesRequest{WorkspaceId: ws, BlockId: para1.Id, Props: structOf(t, map[string]any{status.Id: "bogus"})})); code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("invalid option must be a schema violation: %v", err)
	}
	st := must(e.blocks.SetType(ctx, as(tok, &kbv1.SetTypeRequest{WorkspaceId: ws, BlockId: para1.Id, TypeId: byName["task"].Id})))
	if st.Block.Key != "MAIN-1" || st.Block.TypeId != byName["task"].Id || !st.Block.Compliant {
		t.Fatalf("set type: %+v", st.Block)
	}
	// A work item page gets the next key and is addressable by it.
	task := must(e.pages.Create(ctx, as(tok, &kbv1.CreatePageRequest{WorkspaceId: ws, ProjectId: project.Id, Title: "Fix login", TypeId: byName["bug"].Id})))
	if task.Page.Key != "MAIN-2" || task.Page.Props.AsMap()[status.Id] != "open" {
		t.Fatalf("work item: key=%s props=%v", task.Page.Key, task.Page.Props.AsMap())
	}
	byKey := must(e.pages.Get(ctx, as(tok, &kbv1.GetPageRequest{WorkspaceId: ws, Ref: &kbv1.GetPageRequest_Key{Key: "main-2"}})))
	if byKey.Page.Id != task.Page.Id {
		t.Fatal("lookup by key")
	}

	// Move and delete.
	mv := must(e.blocks.Move(ctx, as(tok, &kbv1.MoveBlocksRequest{WorkspaceId: ws, BlockIds: []string{para2.Id}, ParentBlockId: heading.Id, Position: &kbv1.Position{At: &kbv1.Position_First{First: true}}})))
	got = must(e.pages.Get(ctx, as(tok, &kbv1.GetPageRequest{WorkspaceId: ws, Ref: &kbv1.GetPageRequest_PageId{PageId: pageID}})))
	if got.Blocks[1].Id != para2.Id || mv.Versions[0].Version != got.Page.Version {
		t.Fatalf("move: %v", got.Blocks[1].Id)
	}
	if _, err := e.blocks.Move(ctx, as(tok, &kbv1.MoveBlocksRequest{WorkspaceId: ws, BlockIds: []string{heading.Id}, ParentBlockId: para1.Id})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("cycle move must be refused: %v", err)
	}
	must(e.blocks.Delete(ctx, as(tok, &kbv1.DeleteBlocksRequest{WorkspaceId: ws, BlockIds: []string{mid.Id}})))
	got = must(e.pages.Get(ctx, as(tok, &kbv1.GetPageRequest{WorkspaceId: ws, Ref: &kbv1.GetPageRequest_PageId{PageId: pageID}})))
	if len(got.Blocks) != 3 {
		t.Fatalf("after delete: %d blocks", len(got.Blocks))
	}
	versionBefore := got.Page.Version

	// Batch: three creates and an update land as one seq; a stale if_version is refused.
	batch := must(e.blocks.Batch(ctx, as(tok, &kbv1.BatchRequest{WorkspaceId: ws, PageId: pageID, IfVersion: versionBefore, Ops: []*kbv1.BlockOp{
		{Op: &kbv1.BlockOp_Create_{Create: &kbv1.BlockOp_Create{Markdown: "a", BlockId: uuid.Must(uuid.NewV7()).String()}}},
		{Op: &kbv1.BlockOp_Create_{Create: &kbv1.BlockOp_Create{Markdown: "b"}}},
		{Op: &kbv1.BlockOp_Create_{Create: &kbv1.BlockOp_Create{Markdown: "## Sub\n\nnested para"}}},
		{Op: &kbv1.BlockOp_Update_{Update: &kbv1.BlockOp_Update{BlockId: para2.Id, Markdown: "write great docs"}}},
	}})))
	if batch.Version != versionBefore+1 || len(batch.Results) != 4 || len(batch.Results[2].BlockIds) != 2 {
		t.Fatalf("batch: %+v", batch)
	}
	_, err := e.blocks.Batch(ctx, as(tok, &kbv1.BatchRequest{WorkspaceId: ws, PageId: pageID, IfVersion: versionBefore, Ops: []*kbv1.BlockOp{{Op: &kbv1.BlockOp_Delete_{Delete: &kbv1.BlockOp_Delete{BlockId: para2.Id}}}}}))
	if code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("stale if_version: %v", err)
	}
	var ce *connect.Error
	if !errorsAs(err, &ce) || len(ce.Details()) == 0 {
		t.Fatalf("version conflict detail missing: %v", err)
	}

	// Versions: history is readable and restorable without rewriting the log.
	vers := must(e.pages.ListVersions(ctx, as(tok, &kbv1.ListVersionsRequest{WorkspaceId: ws, PageId: pageID})))
	if len(vers.Versions) != int(batch.Version) || vers.Versions[0].Seq != batch.Version {
		t.Fatalf("versions: %d", len(vers.Versions))
	}
	v1 := must(e.pages.GetVersion(ctx, as(tok, &kbv1.GetVersionRequest{WorkspaceId: ws, PageId: pageID, Seq: 1})))
	if len(v1.Blocks) != 3 || v1.Blocks[1].Markdown != "ship R1" {
		t.Fatalf("version 1: %+v", v1.Blocks)
	}
	restored := must(e.pages.RestoreVersion(ctx, as(tok, &kbv1.RestoreVersionRequest{WorkspaceId: ws, PageId: pageID, Seq: 1})))
	if restored.Version != batch.Version+1 {
		t.Fatalf("restore version: %d", restored.Version)
	}
	got = must(e.pages.Get(ctx, as(tok, &kbv1.GetPageRequest{WorkspaceId: ws, Ref: &kbv1.GetPageRequest_PageId{PageId: pageID}})))
	if len(got.Blocks) != 3 || got.Blocks[1].Id != para1.Id || got.Blocks[1].Markdown != "ship R1" {
		t.Fatalf("restored tree: %+v", got.Blocks)
	}

	// Journal pages are created once per date.
	j1 := must(e.pages.Journal(ctx, as(tok, &kbv1.JournalRequest{WorkspaceId: ws, ProjectId: project.Id, Date: "2026-09-18"})))
	j2 := must(e.pages.Journal(ctx, as(tok, &kbv1.JournalRequest{WorkspaceId: ws, ProjectId: project.Id, Date: "2026-09-18"})))
	if !j1.Created || j2.Created || j1.Page.Id != j2.Page.Id || j1.Page.JournalDate != "2026-09-18" || j1.Page.Format != kbv1.PageFormat_PAGE_FORMAT_OUTLINER {
		t.Fatalf("journal: %+v %+v", j1, j2)
	}
	list := must(e.pages.List(ctx, as(tok, &kbv1.ListPagesRequest{WorkspaceId: ws, ProjectId: project.Id, TitlePrefix: "road"})))
	if len(list.Pages) != 1 || list.Pages[0].Id != pageID {
		t.Fatalf("list by prefix: %+v", list.Pages)
	}
	journals := must(e.pages.List(ctx, as(tok, &kbv1.ListPagesRequest{WorkspaceId: ws, ProjectId: project.Id, JournalsOnly: true})))
	if len(journals.Pages) != 1 {
		t.Fatalf("journals: %d", len(journals.Pages))
	}

	// Idempotency: same key and body replays; same key with a different body aborts.
	key := uuid.NewString()
	c1 := must(e.pages.Create(ctx, as(tok, &kbv1.CreatePageRequest{WorkspaceId: ws, ProjectId: project.Id, Title: "Idem"}, "Idempotency-Key", key)))
	c2 := must(e.pages.Create(ctx, as(tok, &kbv1.CreatePageRequest{WorkspaceId: ws, ProjectId: project.Id, Title: "Idem"}, "Idempotency-Key", key)))
	if c1.Page.Id != c2.Page.Id {
		t.Fatal("idempotent replay created a second page")
	}
	if _, err := e.pages.Create(ctx, as(tok, &kbv1.CreatePageRequest{WorkspaceId: ws, ProjectId: project.Id, Title: "Other"}, "Idempotency-Key", key)); code(err) != connect.CodeAborted {
		t.Fatalf("idempotency mismatch: %v", err)
	}

	// Trash blocks writes, restore re-enables them, purge needs the trash.
	must(e.pages.Trash(ctx, as(tok, &kbv1.TrashPageRequest{WorkspaceId: ws, PageId: c1.Page.Id})))
	if _, err := e.blocks.Create(ctx, as(tok, &kbv1.CreateBlockRequest{WorkspaceId: ws, PageId: c1.Page.Id, Markdown: "x"})); code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("write to trashed page: %v", err)
	}
	if _, err := e.pages.Purge(ctx, as(tok, &kbv1.PurgePageRequest{WorkspaceId: ws, PageId: pageID})); code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("purge of a live page: %v", err)
	}
	must(e.pages.Restore(ctx, as(tok, &kbv1.RestorePageRequest{WorkspaceId: ws, PageId: c1.Page.Id})))
	must(e.blocks.Create(ctx, as(tok, &kbv1.CreateBlockRequest{WorkspaceId: ws, PageId: c1.Page.Id, Markdown: "x"})))
	must(e.pages.Trash(ctx, as(tok, &kbv1.TrashPageRequest{WorkspaceId: ws, PageId: c1.Page.Id})))
	must(e.pages.Purge(ctx, as(tok, &kbv1.PurgePageRequest{WorkspaceId: ws, PageId: c1.Page.Id})))
	if _, err := e.pages.Get(ctx, as(tok, &kbv1.GetPageRequest{WorkspaceId: ws, Ref: &kbv1.GetPageRequest_PageId{PageId: c1.Page.Id}})); code(err) != connect.CodeNotFound {
		t.Fatalf("purged page: %v", err)
	}

	// Members and permissions: a viewer reads, cannot write; strangers see nothing.
	bob, bobTok := e.user()
	inv := must(e.mem.Invite(ctx, as(tok, &kbv1.InviteRequest{WorkspaceId: ws, Did: bob, Role: "viewer"})))
	must(e.mem.AcceptInvite(ctx, as(bobTok, &kbv1.AcceptInviteRequest{WorkspaceId: ws, InviteId: inv.Invite.Id})))
	must(e.pages.Get(ctx, as(bobTok, &kbv1.GetPageRequest{WorkspaceId: ws, Ref: &kbv1.GetPageRequest_PageId{PageId: pageID}})))
	if _, err := e.blocks.Create(ctx, as(bobTok, &kbv1.CreateBlockRequest{WorkspaceId: ws, PageId: pageID, Markdown: "nope"})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("viewer write: %v", err)
	}
	_, carolTok := e.user()
	if _, err := e.pages.Get(ctx, as(carolTok, &kbv1.GetPageRequest{WorkspaceId: ws, Ref: &kbv1.GetPageRequest_PageId{PageId: pageID}})); code(err) != connect.CodeNotFound {
		t.Fatalf("stranger read: %v", err)
	}
	members := must(e.mem.List(ctx, as(tok, &kbv1.ListMembersRequest{WorkspaceId: ws})))
	if len(members.Members) != 2 {
		t.Fatalf("members: %d", len(members.Members))
	}
	if _, err := e.mem.UpdateRole(ctx, as(tok, &kbv1.UpdateRoleRequest{WorkspaceId: ws, Did: alice, Role: "viewer"})); code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("last admin demotion: %v", err)
	}

	// Agent tokens act within their scope and their owner's role.
	agent := must(e.agents.CreateToken(ctx, as(tok, &kbv1.CreateTokenRequest{WorkspaceId: ws, Name: "bot", Scopes: []*kbv1.Scope{{ResourceType: kbv1.ResourceType_RESOURCE_TYPE_WORKSPACE, ResourceId: ws, Role: "viewer"}}})))
	must(e.pages.Get(ctx, as(agent.Token, &kbv1.GetPageRequest{WorkspaceId: ws, Ref: &kbv1.GetPageRequest_PageId{PageId: pageID}})))
	if _, err := e.blocks.Create(ctx, as(agent.Token, &kbv1.CreateBlockRequest{WorkspaceId: ws, PageId: pageID, Markdown: "nope"})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("agent beyond scope: %v", err)
	}

	// Projection rows exist with tree paths and typed properties.
	err = e.db.System(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var n, depth int
		if err := tx.QueryRow(ctx, `SELECT count(*), max(depth) FROM blocks WHERE workspace_id = $1 AND page_id = $2`, ws, pageID).Scan(&n, &depth); err != nil {
			return err
		}
		if n != 4 || depth != 2 { // page block + heading + two paragraphs after the restore
			return fmt.Errorf("blocks projection: n=%d depth=%d", n, depth)
		}
		var val string
		if err := tx.QueryRow(ctx, `SELECT value_text FROM block_properties WHERE workspace_id = $1 AND block_id = $2 AND property_id = $3`, ws, task.Page.Id, status.Id).Scan(&val); err != nil {
			return err
		}
		if val != "open" {
			return fmt.Errorf("status projection: %q", val)
		}
		var key string
		if err := tx.QueryRow(ctx, `SELECT key FROM blocks WHERE workspace_id = $1 AND id = $2`, ws, task.Page.Id).Scan(&key); err != nil {
			return err
		}
		if key != "MAIN-2" {
			return fmt.Errorf("key projection: %q", key)
		}
		var title string
		if err := tx.QueryRow(ctx, `SELECT title FROM pages WHERE workspace_id = $1 AND id = $2`, ws, task.Page.Id).Scan(&title); err != nil {
			return err
		}
		if title != "Fix login" {
			return fmt.Errorf("pages projection: %q", title)
		}
		var entries int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE workspace_id = $1`, ws).Scan(&entries); err != nil {
			return err
		}
		if entries < 10 {
			return fmt.Errorf("audit entries: %d", entries)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = time.Now
}

func structOf(t *testing.T, m map[string]any) *structpbStruct {
	t.Helper()
	s, err := newStruct(m)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
