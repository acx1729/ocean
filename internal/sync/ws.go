package sync

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"google.golang.org/protobuf/proto"

	kbv1 "github.com/acx1729/ocean/gen/kb/v1"
)

// Handler serves the WebSocket transport at /ws/sync: one socket per tab,
// multiplexing docs, binary frames carrying kb.v1.SyncFrame messages.
func (h *Hub) Handler() http.Handler {
	return http.HandlerFunc(h.serveWS)
}

const (
	helloTimeout  = 10 * time.Second
	writeTimeout  = 10 * time.Second
	pingInterval  = 25 * time.Second
	sendQueueSize = 512
)

// conn is one WebSocket connection.
type conn struct {
	h      *Hub
	c      *websocket.Conn
	sess   *session
	sendCh chan []byte
	done   chan struct{}
	once   sync.Once

	mu     sync.Mutex
	docs   map[string]*docSub
	pushes *bucket
}

type docSub struct {
	room *room
	sub  *subscriber
}

// bucket is a per-connection token bucket for pushes.
type bucket struct {
	tokens float64
	last   time.Time
	rate   float64
	max    float64
}

func (b *bucket) allow(now time.Time) bool {
	b.tokens += now.Sub(b.last).Seconds() * b.rate
	if b.tokens > b.max {
		b.tokens = b.max
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func (h *Hub) serveWS(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: h.Origins, CompressionMode: websocket.CompressionContextTakeover})
	if err != nil {
		return
	}
	c.SetReadLimit(h.Limits.UpdateBytes*2 + 64*1024)
	cn := &conn{h: h, c: c, sendCh: make(chan []byte, sendQueueSize), done: make(chan struct{}), docs: map[string]*docSub{},
		pushes: &bucket{tokens: float64(h.Limits.PushesPerSecond), last: h.Now(), rate: float64(h.Limits.PushesPerSecond), max: float64(h.Limits.PushesPerSecond)}}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go cn.writer(ctx)
	defer cn.close(websocket.StatusNormalClosure, "bye")

	// The first frame authenticates the connection.
	hctx, hcancel := context.WithTimeout(ctx, helloTimeout)
	frame, err := cn.read(hctx)
	hcancel()
	if err != nil {
		return
	}
	hello := frame.GetHello()
	if hello == nil {
		cn.sendFrame(errorFrame("", errf(CodeBadFrame, "first frame must be hello")))
		return
	}
	sess, err := h.authenticate(ctx, hello)
	if err != nil {
		cn.writeNow(ctx, errorFrame("", err))
		cn.close(websocket.StatusPolicyViolation, "unauthenticated")
		return
	}
	cn.sess = sess
	cn.sendFrame(&kbv1.SyncFrame{Kind: &kbv1.SyncFrame_Welcome{Welcome: &kbv1.Welcome{Principal: sess.id.Principal, ServerVersion: h.Version}}})

	go cn.pinger(ctx)
	for {
		frame, err := cn.read(ctx)
		if err != nil {
			return
		}
		cn.dispatch(ctx, frame)
	}
}

func (cn *conn) read(ctx context.Context) (*kbv1.SyncFrame, error) {
	typ, data, err := cn.c.Read(ctx)
	if err != nil {
		return nil, err
	}
	if typ != websocket.MessageBinary {
		return nil, errors.New("text frames are not part of the protocol")
	}
	var frame kbv1.SyncFrame
	if err := proto.Unmarshal(data, &frame); err != nil {
		return nil, err
	}
	return &frame, nil
}

// sendFrame queues a frame; it reports false when the connection is gone or
// the client is too slow, in which case the connection is closed.
func (cn *conn) sendFrame(frame *kbv1.SyncFrame) bool {
	data, err := proto.Marshal(frame)
	if err != nil {
		return false
	}
	select {
	case <-cn.done:
		return false
	default:
	}
	select {
	case cn.sendCh <- data:
		return true
	case <-cn.done:
		return false
	default:
		cn.h.Log.Warn("sync: slow client, closing", "client", cn.clientID())
		cn.close(websocket.StatusPolicyViolation, "slow consumer")
		return false
	}
}

// writeNow writes a frame synchronously (used before closing the socket).
func (cn *conn) writeNow(ctx context.Context, frame *kbv1.SyncFrame) {
	data, err := proto.Marshal(frame)
	if err != nil {
		return
	}
	wctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	_ = cn.c.Write(wctx, websocket.MessageBinary, data)
}

func (cn *conn) clientID() string {
	if cn.sess == nil {
		return ""
	}
	return cn.sess.clientID
}

func (cn *conn) writer(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-cn.done:
			return
		case data := <-cn.sendCh:
			wctx, cancel := context.WithTimeout(ctx, writeTimeout)
			err := cn.c.Write(wctx, websocket.MessageBinary, data)
			cancel()
			if err != nil {
				cn.close(websocket.StatusAbnormalClosure, "write failed")
				return
			}
		}
	}
}

func (cn *conn) pinger(ctx context.Context) {
	t := time.NewTicker(pingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-cn.done:
			return
		case <-t.C:
			pctx, cancel := context.WithTimeout(ctx, writeTimeout)
			err := cn.c.Ping(pctx)
			cancel()
			if err != nil {
				cn.close(websocket.StatusAbnormalClosure, "ping failed")
				return
			}
		}
	}
}

// close leaves every room and closes the socket once.
func (cn *conn) close(code websocket.StatusCode, reason string) {
	cn.once.Do(func() {
		close(cn.done)
		cn.mu.Lock()
		docs := cn.docs
		cn.docs = map[string]*docSub{}
		cn.mu.Unlock()
		for _, d := range docs {
			d.room.leave(d.sub)
			cn.h.dropIfEmpty(d.room)
		}
		_ = cn.c.Close(code, reason)
	})
}

func (cn *conn) dispatch(ctx context.Context, frame *kbv1.SyncFrame) {
	switch k := frame.GetKind().(type) {
	case *kbv1.SyncFrame_Open:
		cn.handleOpen(ctx, k.Open)
	case *kbv1.SyncFrame_Push:
		cn.handlePush(ctx, k.Push)
	case *kbv1.SyncFrame_Pull:
		res, err := cn.h.pull(ctx, cn.sess, k.Pull)
		if err != nil {
			cn.sendFrame(errorFrame(k.Pull.GetDocId(), err))
			return
		}
		cn.sendFrame(&kbv1.SyncFrame{Kind: &kbv1.SyncFrame_Pulled{Pulled: res}})
	case *kbv1.SyncFrame_Awareness:
		cn.handleAwareness(k.Awareness)
	case *kbv1.SyncFrame_Close:
		cn.mu.Lock()
		d, ok := cn.docs[k.Close.GetDocId()]
		delete(cn.docs, k.Close.GetDocId())
		cn.mu.Unlock()
		if ok {
			d.room.leave(d.sub)
			cn.h.dropIfEmpty(d.room)
		}
	case *kbv1.SyncFrame_Hello:
		cn.sendFrame(errorFrame("", errf(CodeBadFrame, "already authenticated")))
	default:
		cn.sendFrame(errorFrame("", errf(CodeBadFrame, "unexpected frame")))
	}
}

func (cn *conn) handleOpen(ctx context.Context, req *kbv1.OpenRequest) {
	cn.mu.Lock()
	n := len(cn.docs)
	_, already := cn.docs[req.GetDocId()]
	cn.mu.Unlock()
	if !already && n >= cn.h.Limits.OpenDocsPerConn {
		cn.sendFrame(errorFrame(req.GetDocId(), errf(CodeTooManyDocs, "at most %d open docs per connection", cn.h.Limits.OpenDocsPerConn)))
		return
	}
	res, err := cn.h.open(ctx, cn.sess, req)
	if err != nil {
		cn.sendFrame(errorFrame(req.GetDocId(), err))
		return
	}
	r := cn.h.room(cn.sess.ws, req.GetDocId())
	sub := &subscriber{clientID: cn.sess.clientID, principal: cn.sess.id.Principal, send: cn.sendFrame}
	cn.mu.Lock()
	if old, ok := cn.docs[req.GetDocId()]; ok {
		old.room.leave(old.sub)
	}
	cn.docs[req.GetDocId()] = &docSub{room: r, sub: sub}
	cn.mu.Unlock()
	r.join(sub)
	cn.sendFrame(&kbv1.SyncFrame{Kind: &kbv1.SyncFrame_Opened{Opened: res}})
	for _, aw := range r.currentAwareness(cn.sess.clientID, cn.h.Now()) {
		cn.sendFrame(&kbv1.SyncFrame{Kind: &kbv1.SyncFrame_Awareness{Awareness: aw}})
	}
}

func (cn *conn) handlePush(ctx context.Context, req *kbv1.PushRequest) {
	if !cn.pushes.allow(cn.h.Now()) {
		cn.sendFrame(errorFrame(req.GetDocId(), errf(CodeRateLimited, "too many pushes")))
		return
	}
	cn.mu.Lock()
	d, ok := cn.docs[req.GetDocId()]
	cn.mu.Unlock()
	if !ok {
		cn.sendFrame(errorFrame(req.GetDocId(), errf(CodeNotOpen, "open the doc before pushing")))
		return
	}
	if req.GetClientId() != cn.sess.clientID {
		cn.sendFrame(errorFrame(req.GetDocId(), errf(CodeBadFrame, "client_id does not match hello")))
		return
	}
	ack, fan, err := cn.h.push(ctx, cn.sess, req)
	if err != nil {
		cn.sendFrame(errorFrame(req.GetDocId(), err))
		return
	}
	cn.sendFrame(&kbv1.SyncFrame{Kind: &kbv1.SyncFrame_Ack{Ack: ack}})
	for _, u := range fan {
		d.room.fanout(&kbv1.SyncFrame{Kind: &kbv1.SyncFrame_Update{Update: u}}, d.sub)
	}
}

func (cn *conn) handleAwareness(aw *kbv1.Awareness) {
	cn.mu.Lock()
	d, ok := cn.docs[aw.GetDocId()]
	cn.mu.Unlock()
	if !ok || len(aw.GetState()) > 16*1024 {
		return
	}
	now := cn.h.Now()
	d.room.setAwareness(cn.sess.clientID, aw.GetState(), now)
	d.room.fanout(&kbv1.SyncFrame{Kind: &kbv1.SyncFrame_Awareness{Awareness: &kbv1.Awareness{DocId: aw.GetDocId(), State: aw.GetState(), Peer: cn.sess.clientID}}}, d.sub)
}
