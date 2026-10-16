package node

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	judgev1 "github.com/jellyfish-p/ephyra/protocol/judge/v1"
)

// connectOnce dials one stream. handed is true after the server accepts
// the handshake, so Run can reset reconnect backoff. Running attempts are
// owned by runtime, not by this stream; disconnect does not cancel them.
func connectOnce(ctx context.Context, opts Options, rt *runtime) (handed bool, err error) {
	conn, err := dial(opts)
	if err != nil {
		return false, err
	}
	defer conn.Close()

	stream, err := judgev1.NewJudgeControlClient(conn).Connect(ctx)
	if err != nil {
		return false, err
	}
	if err := stream.Send(&judgev1.NodeMessage{
		Body: &judgev1.NodeMessage_Handshake{Handshake: &judgev1.HandshakeRequest{
			ProtocolVersion:     protocolVersion,
			NodeId:              opts.NodeID,
			Credential:          opts.Credential,
			ExecutionProfileIds: slices.Clone(opts.ExecutionProfileIDs),
			MaxSlots:            opts.MaxSlots,
		}},
	}); err != nil {
		return false, err
	}
	msg, err := stream.Recv()
	if err != nil {
		return false, err
	}
	hs := msg.GetHandshake()
	if hs == nil {
		return false, errFirstMessageNotHandshake
	}
	if !hs.GetAccepted() {
		return false, rejectErr(hs.GetRejectReason())
	}

	interval := time.Duration(hs.GetHeartbeatIntervalMs()) * time.Millisecond
	if interval <= 0 {
		interval = defaultHeartbeat
	}

	sess := newSession(rt, stream)
	rt.bind(sess)
	defer rt.unbind(sess)
	rt.flushPending()

	sessCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	errCh := make(chan error, 2)
	go func() { errCh <- sess.heartbeatLoop(sessCtx, interval) }()
	go func() { errCh <- sess.recvLoop(sessCtx) }()

	select {
	case <-sessCtx.Done():
		return true, sessCtx.Err()
	case err := <-errCh:
		return true, err
	}
}

var errFirstMessageNotHandshake = errors.New("first server message was not a handshake response")

type streamDuplex interface {
	Send(*judgev1.NodeMessage) error
	Recv() (*judgev1.ServerMessage, error)
}

type session struct {
	rt     *runtime
	stream streamDuplex
	sendMu sync.Mutex
}

func newSession(rt *runtime, stream streamDuplex) *session {
	return &session{rt: rt, stream: stream}
}

func (s *session) send(msg *judgev1.NodeMessage) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	return s.stream.Send(msg)
}

func (s *session) heartbeatLoop(ctx context.Context, interval time.Duration) error {
	if err := s.sendHeartbeat(); err != nil {
		return err
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
			if err := s.sendHeartbeat(); err != nil {
				return err
			}
		}
	}
}

func (s *session) sendHeartbeat() error {
	busy, free, ids := s.rt.snapshot()
	return s.send(&judgev1.NodeMessage{
		Body: &judgev1.NodeMessage_Heartbeat{Heartbeat: &judgev1.Heartbeat{
			UnixMs:           time.Now().UnixMilli(),
			BusySlots:        busy,
			FreeSlots:        free,
			ActiveAttemptIds: ids,
		}},
	})
}

func (s *session) recvLoop(ctx context.Context) error {
	for {
		msg, err := s.stream.Recv()
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		switch {
		case msg.GetDispatch() != nil:
			s.rt.start(msg.GetDispatch())
		case msg.GetInvalidate() != nil:
			s.rt.invalidate(msg.GetInvalidate().GetAttemptId())
		case msg.GetHeartbeatAck() != nil:
			for _, id := range msg.GetHeartbeatAck().GetInvalidateAttemptIds() {
				s.rt.invalidate(id)
			}
		}
	}
}
