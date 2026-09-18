package sync

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	kbv1 "github.com/acx1729/ocean/gen/kb/v1"
	"github.com/acx1729/ocean/gen/kb/v1/kbv1connect"
	"github.com/acx1729/ocean/internal/apierr"
	"github.com/acx1729/ocean/internal/auth"
)

// Service is the Connect transport of the sync protocol: unary Open, Push and
// Pull plus a server-streaming Subscribe. It shares the protocol core with
// the WebSocket transport.
type Service struct {
	h *Hub
}

var _ kbv1connect.SyncServiceHandler = (*Service)(nil)

// NewService wraps a hub.
func NewService(h *Hub) *Service { return &Service{h: h} }

func (s *Service) session(ctx context.Context, ws, clientID string) (*session, error) {
	id, ok := auth.FromContext(ctx)
	if !ok {
		return nil, apierr.Unauthenticated("")
	}
	if ws == "" {
		return nil, apierr.InvalidArgument("workspace_id", "required")
	}
	if clientID == "" {
		clientID = "connect:" + id.Principal
	}
	return &session{id: id, ws: ws, clientID: clientID}, nil
}

func toConnectErr(err error) error {
	var se *syncError
	if !errors.As(err, &se) {
		return apierr.From(err)
	}
	switch se.code {
	case CodeUnauthenticated:
		return apierr.Unauthenticated(se.msg)
	case CodePermissionDenied:
		return apierr.PermissionDenied("sync", se.msg, false)
	case CodeNotFound:
		return apierr.NotFound("doc", se.msg)
	case CodeTooLarge, CodeTooManyDocs, CodeRateLimited:
		return apierr.ResourceExhausted(se.code, se.msg)
	case CodeDeleted, CodeNotOpen:
		return apierr.FailedPrecondition(se.msg)
	case CodeBadFrame, CodeInvalidUpdate:
		return apierr.InvalidArgument("update", se.msg)
	}
	return apierr.Internal(err)
}

// Open returns the snapshot and tail of a doc.
func (s *Service) Open(ctx context.Context, req *connect.Request[kbv1.OpenRequest]) (*connect.Response[kbv1.OpenResponse], error) {
	sess, err := s.session(ctx, req.Msg.GetWorkspaceId(), "")
	if err != nil {
		return nil, err
	}
	res, err := s.h.open(ctx, sess, req.Msg)
	if err != nil {
		return nil, toConnectErr(err)
	}
	return connect.NewResponse(res), nil
}

// Push appends client updates and fans them out to subscribers.
func (s *Service) Push(ctx context.Context, req *connect.Request[kbv1.PushRequest]) (*connect.Response[kbv1.PushAck], error) {
	sess, err := s.session(ctx, req.Msg.GetWorkspaceId(), req.Msg.GetClientId())
	if err != nil {
		return nil, err
	}
	ack, fan, err := s.h.push(ctx, sess, req.Msg)
	if err != nil {
		return nil, toConnectErr(err)
	}
	if len(fan) > 0 {
		if r := s.h.lookup(sess.ws, req.Msg.GetDocId()); r != nil {
			for _, u := range fan {
				r.fanout(&kbv1.SyncFrame{Kind: &kbv1.SyncFrame_Update{Update: u}}, nil)
			}
		}
	}
	return connect.NewResponse(ack), nil
}

// Pull returns updates after a seq.
func (s *Service) Pull(ctx context.Context, req *connect.Request[kbv1.PullRequest]) (*connect.Response[kbv1.PullResponse], error) {
	sess, err := s.session(ctx, req.Msg.GetWorkspaceId(), "")
	if err != nil {
		return nil, err
	}
	res, err := s.h.pull(ctx, sess, req.Msg)
	if err != nil {
		return nil, toConnectErr(err)
	}
	return connect.NewResponse(res), nil
}

// Subscribe streams updates and awareness of the requested docs until the
// client disconnects or access is revoked.
func (s *Service) Subscribe(ctx context.Context, req *connect.Request[kbv1.SubscribeRequest], stream *connect.ServerStream[kbv1.SyncFrame]) error {
	sess, err := s.session(ctx, req.Msg.GetWorkspaceId(), req.Msg.GetClientId())
	if err != nil {
		return err
	}
	if len(req.Msg.GetDocIds()) == 0 || len(req.Msg.GetDocIds()) > s.h.Limits.OpenDocsPerConn {
		return apierr.InvalidArgument("doc_ids", "1 to 20 docs")
	}
	frames := make(chan *kbv1.SyncFrame, sendQueueSize)
	done := make(chan struct{})
	var closeOnce func()
	sub := &subscriber{clientID: sess.clientID, principal: sess.id.Principal, send: func(f *kbv1.SyncFrame) bool {
		select {
		case frames <- f:
			return true
		case <-done:
			return false
		default:
			return false // slow consumer: the room drops the subscription
		}
	}}
	var rooms []*room
	for _, docID := range req.Msg.GetDocIds() {
		// Permission is checked by opening the doc without a tail.
		if _, err := s.h.open(ctx, sess, &kbv1.OpenRequest{DocId: docID, SinceSeq: 1 << 62}); err != nil {
			return toConnectErr(err)
		}
		r := s.h.room(sess.ws, docID)
		r.join(sub)
		rooms = append(rooms, r)
	}
	closeOnce = func() {
		close(done)
		for _, r := range rooms {
			r.leave(sub)
			s.h.dropIfEmpty(r)
		}
	}
	defer closeOnce()
	for {
		select {
		case <-ctx.Done():
			return nil
		case f := <-frames:
			if err := stream.Send(f); err != nil {
				return nil
			}
		}
	}
}
