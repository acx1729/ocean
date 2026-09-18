package auth

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

	kbv1 "github.com/acx1729/ocean/gen/kb/v1"
	"github.com/acx1729/ocean/gen/kb/v1/kbv1connect"
	"github.com/acx1729/ocean/internal/config"
	"github.com/acx1729/ocean/internal/did"
	"github.com/acx1729/ocean/internal/keyring"
	"github.com/acx1729/ocean/internal/testutil"
)

type harness struct {
	cfg    *config.Config
	store  *Store
	authn  *Authenticator
	server *httptest.Server
	auth   kbv1connect.AuthServiceClient
	agents kbv1connect.AgentsServiceClient
	now    time.Time
	op     string
}

func newHarness(t *testing.T) *harness {
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
	tokens, _ := NewTokenIssuer(node, cfg.PublicURL, cfg.SessionTTL)
	h := &harness{cfg: cfg, store: NewStore(d), now: time.Now()}
	clock := func() time.Time { return h.now }
	h.op = OperatorToken(node)
	h.authn = NewAuthenticator(AuthenticatorOptions{Tokens: tokens, Store: h.store, Limiter: NewMemoryLimiter(clock), Limits: cfg.Limits, OperatorToken: h.op, Clock: clock})
	svc := NewService(cfg, h.store, tokens, clock, nil)
	agents := NewAgentsService(h.store, h.authn, nil, clock)
	mux := http.NewServeMux()
	opts := connect.WithInterceptors(NewInterceptor(h.authn))
	mux.Handle(kbv1connect.NewAuthServiceHandler(svc, opts))
	mux.Handle(kbv1connect.NewAgentsServiceHandler(agents, opts))
	h.server = httptest.NewServer(mux)
	t.Cleanup(h.server.Close)
	h.auth = kbv1connect.NewAuthServiceClient(h.server.Client(), h.server.URL)
	h.agents = kbv1connect.NewAgentsServiceClient(h.server.Client(), h.server.URL)
	return h
}

func (h *harness) signIn(t *testing.T, web bool) (string, string, string, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	d := did.FromEd25519(pub)
	ch, err := h.auth.Challenge(context.Background(), connect.NewRequest(&kbv1.ChallengeRequest{Did: d}))
	if err != nil {
		t.Fatal(err)
	}
	req := connect.NewRequest(&kbv1.VerifyRequest{Did: d, Nonce: ch.Msg.Nonce, Message: ch.Msg.Message, Signature: ed25519.Sign(priv, []byte(ch.Msg.Message))})
	if web {
		req.Header().Set("X-KB-Client", "web")
	}
	res, err := h.auth.Verify(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if web && !strings.Contains(res.Header().Get("Set-Cookie"), RefreshCookie+"=kbr_") {
		t.Fatalf("web client must receive the refresh cookie: %q", res.Header().Get("Set-Cookie"))
	}
	return d, res.Msg.AccessToken, res.Msg.RefreshToken, priv
}

func withToken[T any](req *connect.Request[T], token string) *connect.Request[T] {
	req.Header().Set("Authorization", "Bearer "+token)
	return req
}

func TestChallengeVerifyRefreshRevoke(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	d, access, refresh, priv := h.signIn(t, true)

	me, err := h.auth.Me(ctx, withToken(connect.NewRequest(&kbv1.MeRequest{}), access))
	if err != nil || me.Msg.Principal.Did != d || me.Msg.SessionId == "" {
		t.Fatalf("me: %v %+v", err, me)
	}
	if _, err := h.auth.Me(ctx, connect.NewRequest(&kbv1.MeRequest{})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("no token must be unauthenticated, got %v", err)
	}
	// A challenge cannot be replayed.
	ch, _ := h.auth.Challenge(ctx, connect.NewRequest(&kbv1.ChallengeRequest{Did: d}))
	sig := ed25519.Sign(priv, []byte(ch.Msg.Message))
	if _, err := h.auth.Verify(ctx, connect.NewRequest(&kbv1.VerifyRequest{Did: d, Nonce: ch.Msg.Nonce, Signature: sig})); err != nil {
		t.Fatal(err)
	}
	if _, err := h.auth.Verify(ctx, connect.NewRequest(&kbv1.VerifyRequest{Did: d, Nonce: ch.Msg.Nonce, Signature: sig})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("replayed nonce must fail, got %v", err)
	}
	// A wrong signature fails and counts toward lockout.
	ch, _ = h.auth.Challenge(ctx, connect.NewRequest(&kbv1.ChallengeRequest{Did: d}))
	if _, err := h.auth.Verify(ctx, connect.NewRequest(&kbv1.VerifyRequest{Did: d, Nonce: ch.Msg.Nonce, Signature: make([]byte, 64)})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("bad signature must fail, got %v", err)
	}

	// Refresh rotates; the old token is dead; reusing it revokes the session.
	r1, err := h.auth.Refresh(ctx, connect.NewRequest(&kbv1.RefreshRequest{RefreshToken: refresh}))
	if err != nil || r1.Msg.RefreshToken == refresh {
		t.Fatalf("refresh: %v", err)
	}
	if _, err := h.auth.Me(ctx, withToken(connect.NewRequest(&kbv1.MeRequest{}), r1.Msg.AccessToken)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.auth.Refresh(ctx, connect.NewRequest(&kbv1.RefreshRequest{RefreshToken: refresh})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("reused refresh must fail, got %v", err)
	}
	if _, err := h.auth.Refresh(ctx, connect.NewRequest(&kbv1.RefreshRequest{RefreshToken: r1.Msg.RefreshToken})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("session must be revoked after reuse, got %v", err)
	}
	// Cookie-based refresh for the web client.
	_, _, refresh2, _ := h.signIn(t, true)
	req := connect.NewRequest(&kbv1.RefreshRequest{})
	req.Header().Set("X-KB-Client", "web")
	req.Header().Set("Cookie", RefreshCookie+"="+refresh2)
	r2, err := h.auth.Refresh(ctx, req)
	if err != nil || !strings.Contains(r2.Header().Get("Set-Cookie"), RefreshCookie+"="+r2.Msg.RefreshToken) {
		t.Fatalf("cookie refresh: %v %q", err, r2.Header().Get("Set-Cookie"))
	}
	// Access tokens expire after the session TTL.
	h.now = h.now.Add(16 * time.Minute)
	if _, err := h.auth.Me(ctx, withToken(connect.NewRequest(&kbv1.MeRequest{}), r2.Msg.AccessToken)); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("expired access token accepted: %v", err)
	}
	// Revoke all.
	r3, _ := h.auth.Refresh(ctx, connect.NewRequest(&kbv1.RefreshRequest{RefreshToken: r2.Msg.RefreshToken}))
	if _, err := h.auth.Revoke(ctx, withToken(connect.NewRequest(&kbv1.RevokeRequest{All: true}), r3.Msg.AccessToken)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.auth.Refresh(ctx, connect.NewRequest(&kbv1.RefreshRequest{RefreshToken: r3.Msg.RefreshToken})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatal("revoked session refreshed")
	}
}

func TestLockoutAfterFailures(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	d := did.FromEd25519(pub)
	for i := 0; i < failureLimit; i++ {
		ch, _ := h.auth.Challenge(ctx, connect.NewRequest(&kbv1.ChallengeRequest{Did: d}))
		_, err := h.auth.Verify(ctx, connect.NewRequest(&kbv1.VerifyRequest{Did: d, Nonce: ch.Msg.Nonce, Signature: make([]byte, 64)}))
		if connect.CodeOf(err) != connect.CodeUnauthenticated {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	ch, _ := h.auth.Challenge(ctx, connect.NewRequest(&kbv1.ChallengeRequest{Did: d}))
	if _, err := h.auth.Verify(ctx, connect.NewRequest(&kbv1.VerifyRequest{Did: d, Nonce: ch.Msg.Nonce, Signature: make([]byte, 64)})); connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatalf("expected lockout, got %v", err)
	}
}

func TestAgentTokens(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	d, access, _, _ := h.signIn(t, false)
	// The user needs a workspace membership to mint tokens.
	ws := createWorkspace(t, h, d)

	created, err := h.agents.CreateToken(ctx, withToken(connect.NewRequest(&kbv1.CreateTokenRequest{
		WorkspaceId: ws, Name: "ci bot", Tools: []string{"search_blocks"},
		Scopes: []*kbv1.Scope{{ResourceType: kbv1.ResourceType_RESOURCE_TYPE_WORKSPACE, ResourceId: ws, Role: "viewer"}},
	}), access))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(created.Msg.Token, AgentPrefix) || created.Msg.Agent.OwnerDid != d {
		t.Fatalf("token: %+v", created.Msg)
	}
	// The agent token authenticates as the agent with the owner attached.
	me, err := h.auth.Me(ctx, withToken(connect.NewRequest(&kbv1.MeRequest{}), created.Msg.Token))
	if err != nil || me.Msg.Principal.Kind != kbv1.PrincipalKind_PRINCIPAL_KIND_AGENT || me.Msg.Owner.Did != d || len(me.Msg.Memberships) != 1 {
		t.Fatalf("agent me: %v %+v", err, me)
	}
	id, err := h.authn.Resolve(ctx, created.Msg.Token)
	if err != nil || !id.IsAgent() || id.Owner != d || id.WorkspaceID != ws || len(id.Scopes) != 1 || id.Tools[0] != "search_blocks" {
		t.Fatalf("resolve: %+v %v", id, err)
	}
	// Agents cannot mint tokens.
	if _, err := h.agents.CreateToken(ctx, withToken(connect.NewRequest(&kbv1.CreateTokenRequest{WorkspaceId: ws, Name: "x", Scopes: []*kbv1.Scope{{ResourceType: kbv1.ResourceType_RESOURCE_TYPE_WORKSPACE, ResourceId: ws, Role: "viewer"}}}), created.Msg.Token)); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("agent minting must be denied: %v", err)
	}
	list, err := h.agents.ListTokens(ctx, withToken(connect.NewRequest(&kbv1.ListTokensRequest{WorkspaceId: ws}), access))
	if err != nil || len(list.Msg.Tokens) != 1 {
		t.Fatalf("list: %v", err)
	}
	if _, err := h.agents.RevokeToken(ctx, withToken(connect.NewRequest(&kbv1.RevokeTokenRequest{WorkspaceId: ws, Id: created.Msg.Agent.Id}), access)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.auth.Me(ctx, withToken(connect.NewRequest(&kbv1.MeRequest{}), created.Msg.Token)); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("revoked agent token accepted: %v", err)
	}
	// Unknown tool names are refused.
	if _, err := h.agents.CreateToken(ctx, withToken(connect.NewRequest(&kbv1.CreateTokenRequest{WorkspaceId: ws, Name: "x", Tools: []string{"drop_tables"}, Scopes: []*kbv1.Scope{{ResourceType: kbv1.ResourceType_RESOURCE_TYPE_WORKSPACE, ResourceId: ws, Role: "viewer"}}}), access)); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("unknown tool accepted: %v", err)
	}
}

func TestOperatorTokenAndRateLimit(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	// The operator token is only valid for AdminService.
	if _, err := h.auth.Me(ctx, withToken(connect.NewRequest(&kbv1.MeRequest{}), h.op)); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("operator on AuthService: %v", err)
	}
	_, access, _, _ := h.signIn(t, false)
	limit := h.cfg.Limits.RateAccessPerMinute
	var last error
	for i := 0; i < limit+5; i++ {
		_, last = h.auth.Me(ctx, withToken(connect.NewRequest(&kbv1.MeRequest{}), access))
		if last != nil {
			break
		}
	}
	if connect.CodeOf(last) != connect.CodeResourceExhausted {
		t.Fatalf("expected rate limit after %d calls, got %v", limit, last)
	}
}
