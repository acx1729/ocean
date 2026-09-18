package sync

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"

	kbv1 "github.com/acx1729/ocean/gen/kb/v1"
	"github.com/acx1729/ocean/gen/kb/v1/kbv1connect"
	"github.com/acx1729/ocean/internal/api"
	"github.com/acx1729/ocean/internal/auth"
	"github.com/acx1729/ocean/internal/authz"
	"github.com/acx1729/ocean/internal/config"
	"github.com/acx1729/ocean/internal/db"
	"github.com/acx1729/ocean/internal/did"
	"github.com/acx1729/ocean/internal/keyring"
	"github.com/acx1729/ocean/internal/loro"
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
	t     *testing.T
	hub   *Hub
	srv   *httptest.Server
	auth  kbv1connect.AuthServiceClient
	ws    kbv1connect.WorkspacesServiceClient
	pages kbv1connect.PagesServiceClient
	sync  kbv1connect.SyncServiceClient
}

func newEnv(t *testing.T) *env {
	t.Helper()
	d := testutil.NewDB(t)
	cfg, err := config.Load(func(k string) string {
		return map[string]string{
			"KB_PUBLIC_URL": "http://localhost:8080", "KB_DATABASE_URL": "x", "KB_FGA_DATABASE_URL": "x", "KB_RIVER_DATABASE_URL": "x",
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
	guard := authz.NewRoleGuard(store)
	ts := truth.NewStore(keys, nil)
	mat := truth.NewMaterializer(ts, d, 8)
	hub := NewHub(Deps{DB: d, Mat: mat, Truth: ts, Authn: authn, Guard: guard, Limits: cfg.Limits, SyncURL: "ws://test", Origins: []string{"*"}, Version: "test"})
	hctx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	hub.Start(hctx)
	svcs := api.New(api.Deps{Config: cfg, DB: d, Keys: keys, Truth: ts, Mat: mat, Guard: guard, AuthStore: store, Notify: hub.Broadcast})
	mux := http.NewServeMux()
	opts := connect.WithInterceptors(auth.NewInterceptor(authn))
	mux.Handle(kbv1connect.NewAuthServiceHandler(auth.NewService(cfg, store, tokens, nil, nil), opts))
	mux.Handle(kbv1connect.NewWorkspacesServiceHandler(svcs.Workspaces, opts))
	mux.Handle(kbv1connect.NewPagesServiceHandler(svcs.Pages, opts))
	mux.Handle(kbv1connect.NewSyncServiceHandler(NewService(hub), opts))
	mux.Handle("/ws/sync", hub.Handler())
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	t.Cleanup(hub.Close)
	e := &env{t: t, hub: hub, srv: srv}
	e.auth = kbv1connect.NewAuthServiceClient(srv.Client(), srv.URL)
	e.ws = kbv1connect.NewWorkspacesServiceClient(srv.Client(), srv.URL)
	e.pages = kbv1connect.NewPagesServiceClient(srv.Client(), srv.URL)
	e.sync = kbv1connect.NewSyncServiceClient(srv.Client(), srv.URL)
	return e
}

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

func withTok[T any](tok string, m *T) *connect.Request[T] {
	r := connect.NewRequest(m)
	r.Header().Set("Authorization", "Bearer "+tok)
	return r
}

// client is a minimal protocol client over the WebSocket transport.
type client struct {
	t   *testing.T
	c   *websocket.Conn
	id  string
	doc *loro.Doc
	ctx context.Context
}

func dial(t *testing.T, e *env, tok, ws string) *client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	url := "ws" + strings.TrimPrefix(e.srv.URL, "http") + "/ws/sync"
	c, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPClient: e.srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	cl := &client{t: t, c: c, id: uuid.NewString(), doc: loro.New(), ctx: ctx}
	t.Cleanup(func() { cl.doc.Close(); _ = c.CloseNow() })
	cl.send(&kbv1.SyncFrame{Kind: &kbv1.SyncFrame_Hello{Hello: &kbv1.Hello{AccessToken: tok, ClientId: cl.id, WorkspaceId: ws}}})
	return cl
}

func (cl *client) send(f *kbv1.SyncFrame) {
	cl.t.Helper()
	b, err := proto.Marshal(f)
	if err != nil {
		cl.t.Fatal(err)
	}
	if err := cl.c.Write(cl.ctx, websocket.MessageBinary, b); err != nil {
		cl.t.Fatal(err)
	}
}

func (cl *client) recv() *kbv1.SyncFrame {
	cl.t.Helper()
	ctx, cancel := context.WithTimeout(cl.ctx, 5*time.Second)
	defer cancel()
	_, b, err := cl.c.Read(ctx)
	if err != nil {
		cl.t.Fatalf("read: %v", err)
	}
	var f kbv1.SyncFrame
	if err := proto.Unmarshal(b, &f); err != nil {
		cl.t.Fatal(err)
	}
	return &f
}

// recvSkippingAwareness reads the next frame that is not an awareness relay.
func (cl *client) recvSkippingAwareness() *kbv1.SyncFrame {
	cl.t.Helper()
	for {
		f := cl.recv()
		if f.GetAwareness() == nil {
			return f
		}
	}
}

// open opens a doc and imports the snapshot and tail into the local Loro doc.
func (cl *client) open(docID string, since int64) *kbv1.OpenResponse {
	cl.t.Helper()
	cl.send(&kbv1.SyncFrame{Kind: &kbv1.SyncFrame_Open{Open: &kbv1.OpenRequest{DocId: docID, SinceSeq: since}}})
	f := cl.recv()
	opened := f.GetOpened()
	if opened == nil {
		cl.t.Fatalf("expected opened, got %v", f)
	}
	if len(opened.Snapshot) > 0 {
		if err := cl.doc.Import(opened.Snapshot); err != nil {
			cl.t.Fatal(err)
		}
	}
	for _, u := range opened.Tail {
		if err := cl.doc.Import(u.Update); err != nil {
			cl.t.Fatal(err)
		}
	}
	return opened
}

func TestWebSocketProtocol(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	_, tok := e.user()
	created, err := e.ws.Create(ctx, withTok(tok, &kbv1.CreateWorkspaceRequest{Slug: "sync", Name: "Sync"}))
	if err != nil {
		t.Fatal(err)
	}
	ws, project := created.Msg.Workspace.Id, created.Msg.Project
	page, err := e.pages.Create(ctx, withTok(tok, &kbv1.CreatePageRequest{WorkspaceId: ws, ProjectId: project.Id, Title: "Shared", Markdown: "hello"}))
	if err != nil {
		t.Fatal(err)
	}
	docID := page.Msg.Page.DocId

	a := dial(t, e, tok, ws)
	if w := a.recv().GetWelcome(); w == nil {
		t.Fatal("expected welcome")
	}
	b := dial(t, e, tok, ws)
	b.recv()

	// Open: A gets a snapshot with the API-created block.
	opened := a.open(docID, 0)
	stA, _ := a.doc.State()
	if opened.CurrentSeq != 1 || stA.Meta.Title != "Shared" || len(stA.Roots) != 1 || stA.Roots[0].Content != "hello" {
		t.Fatalf("open: seq=%d title=%q roots=%d", opened.CurrentSeq, stA.Meta.Title, len(stA.Roots))
	}
	b.open(docID, 0)

	// A pushes a local edit; A gets an ack with the seq, B receives the update and converges.
	blockID := stA.Roots[0].ID
	ar, err := a.doc.Apply([]loro.Op{loro.NodeText(stA.Roots[0].TreeID, "hello from A")})
	if err != nil {
		t.Fatal(err)
	}
	a.send(&kbv1.SyncFrame{Kind: &kbv1.SyncFrame_Push{Push: &kbv1.PushRequest{DocId: docID, ClientId: a.id, Updates: []*kbv1.ClientUpdate{{ClientSeq: 1, Update: ar.Update}}}}})
	ack := a.recv().GetAck()
	if ack == nil || len(ack.Acked) != 1 || ack.Acked[0].Seq != 2 || ack.Acked[0].ClientSeq != 1 {
		t.Fatalf("ack: %v", ack)
	}
	upd := b.recv().GetUpdate()
	if upd == nil || upd.Seq != 2 {
		t.Fatalf("update: %v", upd)
	}
	if err := b.doc.Import(upd.Update); err != nil {
		t.Fatal(err)
	}
	stB, _ := b.doc.State()
	if stB.ByID[blockID].Content != "hello from A" {
		t.Fatalf("B did not converge: %q", stB.ByID[blockID].Content)
	}
	// A duplicate push (retry after reconnect) is acked with the original seq and not fanned out again.
	a.send(&kbv1.SyncFrame{Kind: &kbv1.SyncFrame_Push{Push: &kbv1.PushRequest{DocId: docID, ClientId: a.id, Updates: []*kbv1.ClientUpdate{{ClientSeq: 1, Update: ar.Update}}}}})
	if ack := a.recv().GetAck(); ack == nil || ack.Acked[0].Seq != 2 {
		t.Fatalf("duplicate ack: %v", ack)
	}

	// Awareness is relayed to the other peer with the sender's client id.
	a.send(&kbv1.SyncFrame{Kind: &kbv1.SyncFrame_Awareness{Awareness: &kbv1.Awareness{DocId: docID, State: []byte("cursor-a")}}})
	aw := b.recv().GetAwareness()
	if aw == nil || string(aw.State) != "cursor-a" || aw.Peer != a.id {
		t.Fatalf("awareness: %v", aw)
	}

	// An API write reaches subscribers through Broadcast.
	if _, err := e.pages.Update(ctx, withTok(tok, &kbv1.UpdatePageRequest{WorkspaceId: ws, PageId: page.Msg.Page.Id, Page: &kbv1.Page{Title: "Renamed"}})); err != nil {
		t.Fatal(err)
	}
	for _, cl := range []*client{a, b} {
		u := cl.recv().GetUpdate()
		if u == nil || u.Seq != 3 {
			t.Fatalf("broadcast: %v", u)
		}
		if err := cl.doc.Import(u.Update); err != nil {
			t.Fatal(err)
		}
	}
	stA, _ = a.doc.State()
	if stA.Meta.Title != "Renamed" {
		t.Fatalf("A title after broadcast: %q", stA.Meta.Title)
	}

	// Pull fills a gap; a late client opening with since_seq gets only the tail.
	a.send(&kbv1.SyncFrame{Kind: &kbv1.SyncFrame_Pull{Pull: &kbv1.PullRequest{DocId: docID, SinceSeq: 1}}})
	pulled := a.recv().GetPulled()
	if pulled == nil || len(pulled.Updates) != 2 || pulled.CurrentSeq != 3 {
		t.Fatalf("pull: %v", pulled)
	}
	c := dial(t, e, tok, ws)
	c.recv()
	late := c.open(docID, 1)
	if len(late.Snapshot) != 0 || len(late.Tail) != 2 || late.SnapshotSeq != 1 {
		t.Fatalf("late open: snapshot=%d tail=%d", len(late.Snapshot), len(late.Tail))
	}

	// Pushing to a doc that was not opened is refused; garbage is refused.
	c.send(&kbv1.SyncFrame{Kind: &kbv1.SyncFrame_Push{Push: &kbv1.PushRequest{DocId: uuid.NewString(), ClientId: c.id, Updates: []*kbv1.ClientUpdate{{ClientSeq: 1, Update: []byte("x")}}}}})
	if e := c.recvSkippingAwareness().GetError(); e == nil || e.Code != CodeNotOpen {
		t.Fatalf("not open: %v", e)
	}
	c.send(&kbv1.SyncFrame{Kind: &kbv1.SyncFrame_Push{Push: &kbv1.PushRequest{DocId: docID, ClientId: c.id, Updates: []*kbv1.ClientUpdate{{ClientSeq: 1, Update: []byte("not loro")}}}}})
	if e := c.recvSkippingAwareness().GetError(); e == nil || e.Code != CodeInvalidUpdate {
		t.Fatalf("garbage: %v", e)
	}

	// The projection catches up after pushes.
	if err := e.hub.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := e.pages.List(ctx, withTok(tok, &kbv1.ListPagesRequest{WorkspaceId: ws, ProjectId: project.Id}))
	if err != nil || len(got.Msg.Pages) != 1 || got.Msg.Pages[0].Title != "Renamed" {
		t.Fatalf("projection: %v", err)
	}

	// Strangers are refused at hello; a bad first frame is refused.
	_, strangerTok := e.user()
	s := dial(t, e, strangerTok, ws)
	if e := s.recv().GetError(); e == nil || e.Code != CodePermissionDenied {
		t.Fatalf("stranger: %v", e)
	}
	rooms, subs := e.hub.Stats()
	if rooms != 1 || subs != 3 {
		t.Fatalf("stats: rooms=%d subs=%d", rooms, subs)
	}
}

func TestConnectTransport(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	_, tok := e.user()
	created, err := e.ws.Create(ctx, withTok(tok, &kbv1.CreateWorkspaceRequest{Slug: "conn", Name: "Connect"}))
	if err != nil {
		t.Fatal(err)
	}
	ws := created.Msg.Workspace.Id
	page, err := e.pages.Create(ctx, withTok(tok, &kbv1.CreatePageRequest{WorkspaceId: ws, ProjectId: created.Msg.Project.Id, Title: "Unary", Markdown: "one"}))
	if err != nil {
		t.Fatal(err)
	}
	docID := page.Msg.Page.DocId
	opened, err := e.sync.Open(ctx, withTok(tok, &kbv1.OpenRequest{WorkspaceId: ws, DocId: docID}))
	if err != nil || opened.Msg.CurrentSeq != 1 || len(opened.Msg.Snapshot) == 0 {
		t.Fatalf("open: %v", err)
	}
	doc, err := loro.FromBytes(opened.Msg.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()
	st, _ := doc.State()
	ar, _ := doc.Apply([]loro.Op{loro.NodeText(st.Roots[0].TreeID, "one, edited")})
	ack, err := e.sync.Push(ctx, withTok(tok, &kbv1.PushRequest{WorkspaceId: ws, DocId: docID, ClientId: "cli-1", Updates: []*kbv1.ClientUpdate{{ClientSeq: 1, Update: ar.Update}}}))
	if err != nil || ack.Msg.Acked[0].Seq != 2 {
		t.Fatalf("push: %v", err)
	}
	pulled, err := e.sync.Pull(ctx, withTok(tok, &kbv1.PullRequest{WorkspaceId: ws, DocId: docID, SinceSeq: 0}))
	if err != nil || len(pulled.Msg.Updates) != 2 {
		t.Fatalf("pull: %v", err)
	}
	if _, err := e.sync.Open(ctx, withTok(tok, &kbv1.OpenRequest{WorkspaceId: ws, DocId: uuid.NewString()})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("unknown doc: %v", err)
	}
}
