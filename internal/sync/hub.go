// Package sync implements the sync rooms of specification section 7: the
// transport for Loro clients (the web app). Rooms never materialize a doc;
// they decrypt outbound and encrypt inbound at the node edge through the
// truth store, keep subscribers per doc for fan-out, and relay awareness
// (cursor, selection, name, color) without persisting it.
package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	kbv1 "github.com/acx1729/ocean/gen/kb/v1"
	"github.com/acx1729/ocean/internal/auth"
	"github.com/acx1729/ocean/internal/authz"
	"github.com/acx1729/ocean/internal/config"
	"github.com/acx1729/ocean/internal/db"
	"github.com/acx1729/ocean/internal/schema"
	"github.com/acx1729/ocean/internal/truth"
)

// Deps are the collaborators of a Hub.
type Deps struct {
	DB      *db.DB
	Mat     *truth.Materializer
	Truth   *truth.Store
	Authn   *auth.Authenticator
	Guard   authz.Guard
	Schema  schema.Repo
	Limits  config.Limits
	Log     *slog.Logger
	SyncURL string
	// Origins allowed to open the WebSocket (host patterns).
	Origins []string
	Version string
	Now     func() time.Time
}

// Hub owns the rooms of this process.
type Hub struct {
	Deps
	mu      sync.Mutex
	rooms   map[string]*room
	indexer *indexer
	closed  bool
}

// Error codes carried in SyncError.code.
const (
	CodeUnauthenticated  = "unauthenticated"
	CodePermissionDenied = "permission_denied"
	CodeNotFound         = "not_found"
	CodeTooManyDocs      = "too_many_docs"
	CodeRateLimited      = "rate_limited"
	CodeInvalidUpdate    = "invalid_update"
	CodeTooLarge         = "too_large"
	CodeNotOpen          = "not_open"
	CodeDeleted          = "deleted"
	CodeInternal         = "internal"
	CodeBadFrame         = "bad_frame"
)

// syncError is a protocol error with a code the client can act on.
type syncError struct {
	code string
	msg  string
}

func (e *syncError) Error() string { return e.code + ": " + e.msg }

func errf(code, format string, args ...any) error {
	return &syncError{code: code, msg: fmt.Sprintf(format, args...)}
}

// awarenessTTL bounds how long a peer's ephemeral state is relayed.
const awarenessTTL = 30 * time.Second

// NewHub builds a hub and starts its indexer.
func NewHub(d Deps) *Hub {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	h := &Hub{Deps: d, rooms: map[string]*room{}}
	h.indexer = newIndexer(h)
	return h
}

// Start runs background loops until ctx is cancelled.
func (h *Hub) Start(ctx context.Context) {
	go h.indexer.run(ctx)
	go h.sweep(ctx)
}

// Close disconnects every subscriber.
func (h *Hub) Close() {
	h.mu.Lock()
	h.closed = true
	rooms := make([]*room, 0, len(h.rooms))
	for _, r := range h.rooms {
		rooms = append(rooms, r)
	}
	h.mu.Unlock()
	for _, r := range rooms {
		r.closeAll("shutdown")
	}
}

// Stats reports room and subscriber counts (metrics).
func (h *Hub) Stats() (rooms, subscribers int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.rooms {
		r.mu.Lock()
		subscribers += len(r.subs)
		r.mu.Unlock()
	}
	return len(h.rooms), subscribers
}

func roomKey(ws, docID string) string { return ws + ":" + docID }

// room gets or creates the room of a doc.
func (h *Hub) room(ws, docID string) *room {
	h.mu.Lock()
	defer h.mu.Unlock()
	k := roomKey(ws, docID)
	r, ok := h.rooms[k]
	if !ok {
		r = &room{key: k, ws: ws, docID: docID, subs: map[*subscriber]struct{}{}, awareness: map[string]*awareEntry{}}
		h.rooms[k] = r
	}
	return r
}

func (h *Hub) lookup(ws, docID string) *room {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.rooms[roomKey(ws, docID)]
}

// dropIfEmpty removes an idle room.
func (h *Hub) dropIfEmpty(r *room) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r.mu.Lock()
	empty := len(r.subs) == 0
	r.mu.Unlock()
	if empty {
		delete(h.rooms, r.key)
	}
}

// sweep expires awareness states and drops empty rooms.
func (h *Hub) sweep(ctx context.Context) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		h.mu.Lock()
		rooms := make([]*room, 0, len(h.rooms))
		for _, r := range h.rooms {
			rooms = append(rooms, r)
		}
		h.mu.Unlock()
		now := h.Now()
		for _, r := range rooms {
			r.expireAwareness(now)
			h.dropIfEmpty(r)
		}
	}
}

// subscriber is one sink for a doc's frames.
type subscriber struct {
	clientID  string
	principal string
	send      func(*kbv1.SyncFrame) bool // false when the sink is gone
}

type awareEntry struct {
	state []byte
	at    time.Time
}

// room is the set of subscribers of one doc plus their awareness states.
type room struct {
	key, ws, docID string
	mu             sync.Mutex
	subs           map[*subscriber]struct{}
	awareness      map[string]*awareEntry // peer → state
}

func (r *room) join(s *subscriber) {
	r.mu.Lock()
	r.subs[s] = struct{}{}
	r.mu.Unlock()
}

func (r *room) leave(s *subscriber) {
	r.mu.Lock()
	delete(r.subs, s)
	delete(r.awareness, s.clientID)
	r.mu.Unlock()
}

// fanout delivers a frame to every subscriber except the sender.
func (r *room) fanout(frame *kbv1.SyncFrame, except *subscriber) {
	r.mu.Lock()
	subs := make([]*subscriber, 0, len(r.subs))
	for s := range r.subs {
		if s != except {
			subs = append(subs, s)
		}
	}
	r.mu.Unlock()
	for _, s := range subs {
		if !s.send(frame) {
			r.leave(s)
		}
	}
}

func (r *room) closeAll(reason string) {
	r.mu.Lock()
	subs := make([]*subscriber, 0, len(r.subs))
	for s := range r.subs {
		subs = append(subs, s)
	}
	r.subs = map[*subscriber]struct{}{}
	r.awareness = map[string]*awareEntry{}
	r.mu.Unlock()
	frame := &kbv1.SyncFrame{Kind: &kbv1.SyncFrame_Close{Close: &kbv1.Close{DocId: r.docID, Reason: reason}}}
	for _, s := range subs {
		s.send(frame)
	}
}

func (r *room) setAwareness(peer string, state []byte, now time.Time) {
	r.mu.Lock()
	if len(state) == 0 {
		delete(r.awareness, peer)
	} else {
		r.awareness[peer] = &awareEntry{state: state, at: now}
	}
	r.mu.Unlock()
}

func (r *room) expireAwareness(now time.Time) {
	r.mu.Lock()
	for peer, e := range r.awareness {
		if now.Sub(e.at) > awarenessTTL {
			delete(r.awareness, peer)
		}
	}
	r.mu.Unlock()
}

// currentAwareness returns the live states of other peers.
func (r *room) currentAwareness(exceptPeer string, now time.Time) []*kbv1.Awareness {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*kbv1.Awareness
	for peer, e := range r.awareness {
		if peer == exceptPeer || now.Sub(e.at) > awarenessTTL {
			continue
		}
		out = append(out, &kbv1.Awareness{DocId: r.docID, State: e.state, Peer: peer})
	}
	return out
}

// Broadcast fans out an update committed by the API to the room's subscribers.
func (h *Hub) Broadcast(ws, docID string, seq int64, update []byte, actor string) {
	if r := h.lookup(ws, docID); r != nil {
		r.fanout(&kbv1.SyncFrame{Kind: &kbv1.SyncFrame_Update{Update: &kbv1.Update{DocId: docID, Seq: seq, Update: update, Actor: actor}}}, nil)
	}
	h.indexer.mark(ws, docID)
}

// CloseDoc disconnects every subscriber of a doc (access revoked, trashed).
func (h *Hub) CloseDoc(ws, docID, reason string) {
	if r := h.lookup(ws, docID); r != nil {
		r.closeAll(reason)
		h.dropIfEmpty(r)
	}
}

// --- protocol core shared by the WebSocket and Connect transports ------------

// session is the authenticated context of a transport connection.
type session struct {
	id       *auth.Identity
	ws       string
	clientID string
}

func (h *Hub) tenant(s *session) *db.Tenant {
	return &db.Tenant{WorkspaceID: s.ws, PrincipalID: s.id.EffectiveOwner()}
}

// snapshotThreshold is the tail length above which Open sends a snapshot
// instead of the missing updates.
const snapshotThreshold = 2000

// open authorizes and answers an Open: snapshot plus tail since the client's seq.
func (h *Hub) open(ctx context.Context, s *session, req *kbv1.OpenRequest) (*kbv1.OpenResponse, error) {
	docID := req.GetDocId()
	if docID == "" {
		return nil, errf(CodeBadFrame, "doc_id required")
	}
	if req.GetWorkspaceId() != "" && req.GetWorkspaceId() != s.ws {
		return nil, errf(CodePermissionDenied, "workspace mismatch")
	}
	var out *kbv1.OpenResponse
	err := h.DB.Tx(ctx, h.tenant(s), func(ctx context.Context, tx pgx.Tx) error {
		row, err := h.Truth.GetDoc(ctx, tx, s.ws, docID)
		if err != nil {
			if truth.IsNotFound(err) {
				return errf(CodeNotFound, "doc %s", docID)
			}
			return err
		}
		if err := h.Guard.Check(ctx, s.id, s.ws, authz.View, authz.Doc(row.ID, row.ProjectID)); err != nil {
			return errf(CodePermissionDenied, "cannot view doc")
		}
		if row.DeletedAt != nil {
			return errf(CodeDeleted, "doc is in the trash")
		}
		out = &kbv1.OpenResponse{DocId: docID, CurrentSeq: row.CurrentSeq, SyncUrl: h.SyncURL}
		since := req.GetSinceSeq()
		if since > 0 && since <= row.CurrentSeq && row.CurrentSeq-since <= snapshotThreshold {
			updates, err := h.Truth.Updates(ctx, tx, s.ws, docID, since, 0, 0)
			if err != nil {
				return err
			}
			out.SnapshotSeq = since
			for _, u := range updates {
				out.Tail = append(out.Tail, &kbv1.Update{DocId: docID, Seq: u.Seq, Update: u.Bytes, Actor: u.Actor})
			}
			return nil
		}
		snap, _, seq, err := h.Mat.Snapshot(ctx, tx, s.ws, docID)
		if err != nil {
			return err
		}
		out.Snapshot, out.SnapshotSeq, out.CurrentSeq = snap, seq, seq
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// push authorizes and appends client updates, returning their seqs and the
// frames to fan out (non-duplicates only).
func (h *Hub) push(ctx context.Context, s *session, req *kbv1.PushRequest) (*kbv1.PushAck, []*kbv1.Update, error) {
	docID := req.GetDocId()
	if docID == "" || req.GetClientId() == "" {
		return nil, nil, errf(CodeBadFrame, "doc_id and client_id required")
	}
	if req.GetWorkspaceId() != "" && req.GetWorkspaceId() != s.ws {
		return nil, nil, errf(CodePermissionDenied, "workspace mismatch")
	}
	for _, u := range req.GetUpdates() {
		if int64(len(u.GetUpdate())) > h.Limits.UpdateBytes {
			return nil, nil, errf(CodeTooLarge, "update exceeds %d bytes", h.Limits.UpdateBytes)
		}
	}
	ack := &kbv1.PushAck{DocId: docID}
	var fan []*kbv1.Update
	err := h.DB.Tx(ctx, h.tenant(s), func(ctx context.Context, tx pgx.Tx) error {
		row, err := h.Truth.GetDoc(ctx, tx, s.ws, docID)
		if err != nil {
			if truth.IsNotFound(err) {
				return errf(CodeNotFound, "doc %s", docID)
			}
			return err
		}
		if err := h.Guard.Check(ctx, s.id, s.ws, authz.Edit, authz.Doc(row.ID, row.ProjectID)); err != nil {
			return errf(CodePermissionDenied, "cannot edit doc")
		}
		var first, last int64
		for _, u := range req.GetUpdates() {
			seq, dup, err := h.Mat.ImportUpdate(ctx, tx, s.ws, docID, truth.AppendInput{Actor: s.id.Principal, ClientID: req.GetClientId(), ClientSeq: u.GetClientSeq(), Plaintext: u.GetUpdate()})
			if err != nil {
				if errors.Is(err, truth.ErrDeleted) {
					return errf(CodeDeleted, "doc is in the trash")
				}
				var se *syncError
				if errors.As(err, &se) {
					return err
				}
				return errf(CodeInvalidUpdate, "%v", err)
			}
			ack.Acked = append(ack.Acked, &kbv1.AckedUpdate{ClientSeq: u.GetClientSeq(), Seq: seq})
			if !dup {
				fan = append(fan, &kbv1.Update{DocId: docID, Seq: seq, Update: u.GetUpdate(), Actor: s.id.Principal})
				if first == 0 {
					first = seq
				}
				last = seq
			}
		}
		if last > 0 {
			return h.Truth.InsertOutbox(ctx, tx, &truth.Event{Kind: truth.EventDocSynced, WorkspaceID: s.ws, ProjectID: row.ProjectID, DocID: docID, Seq: last, Actor: s.id.Principal,
				Payload: map[string]any{"first_seq": first, "last_seq": last, "page_id": row.PageID}})
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	if len(fan) > 0 {
		h.indexer.mark(s.ws, docID)
	}
	return ack, fan, nil
}

// pull returns the updates after since_seq.
func (h *Hub) pull(ctx context.Context, s *session, req *kbv1.PullRequest) (*kbv1.PullResponse, error) {
	docID := req.GetDocId()
	if docID == "" {
		return nil, errf(CodeBadFrame, "doc_id required")
	}
	out := &kbv1.PullResponse{DocId: docID}
	err := h.DB.Tx(ctx, h.tenant(s), func(ctx context.Context, tx pgx.Tx) error {
		row, err := h.Truth.GetDoc(ctx, tx, s.ws, docID)
		if err != nil {
			if truth.IsNotFound(err) {
				return errf(CodeNotFound, "doc %s", docID)
			}
			return err
		}
		if err := h.Guard.Check(ctx, s.id, s.ws, authz.View, authz.Doc(row.ID, row.ProjectID)); err != nil {
			return errf(CodePermissionDenied, "cannot view doc")
		}
		updates, err := h.Truth.Updates(ctx, tx, s.ws, docID, req.GetSinceSeq(), 0, 5000)
		if err != nil {
			return err
		}
		for _, u := range updates {
			out.Updates = append(out.Updates, &kbv1.Update{DocId: docID, Seq: u.Seq, Update: u.Bytes, Actor: u.Actor})
		}
		out.CurrentSeq = row.CurrentSeq
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// authenticate resolves a Hello into a session.
func (h *Hub) authenticate(ctx context.Context, hello *kbv1.Hello) (*session, error) {
	if hello.GetAccessToken() == "" || hello.GetClientId() == "" || hello.GetWorkspaceId() == "" {
		return nil, errf(CodeBadFrame, "hello needs access_token, client_id and workspace_id")
	}
	id, err := h.Authn.Resolve(ctx, hello.GetAccessToken())
	if err != nil {
		return nil, errf(CodeUnauthenticated, "invalid token")
	}
	if id.Operator {
		return nil, errf(CodePermissionDenied, "operator tokens cannot sync")
	}
	if err := h.Guard.Check(ctx, id, hello.GetWorkspaceId(), authz.View, authz.Workspace(hello.GetWorkspaceId())); err != nil {
		return nil, errf(CodePermissionDenied, "not a member of the workspace")
	}
	return &session{id: id, ws: hello.GetWorkspaceId(), clientID: hello.GetClientId()}, nil
}

func errorFrame(docID string, err error) *kbv1.SyncFrame {
	code, msg := CodeInternal, "internal error"
	var se *syncError
	if errors.As(err, &se) {
		code, msg = se.code, se.msg
	}
	return &kbv1.SyncFrame{Kind: &kbv1.SyncFrame_Error{Error: &kbv1.SyncError{DocId: docID, Code: code, Message: msg}}}
}
