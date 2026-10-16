package node

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sort"
	"sync"
	"time"

	judgev1 "github.com/jellyfish-p/ephyra/protocol/judge/v1"
)

// connectOnce dials one stream. handed is true after the server accepts
// the handshake, so Run can reset reconnect backoff.
func connectOnce(ctx context.Context, opts Options) (handed bool, err error) {
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
			ExecutionProfileIds: slices.Clone(executionProfileIDs),
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

	sess := newSession(opts, stream)
	sessCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer sess.shutdown()

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
	nodeID   string
	maxSlots int32
	judge    JudgeFunc
	stream   streamDuplex

	slots chan struct{}

	sendMu sync.Mutex

	mu     sync.Mutex
	active map[string]*inflight
}

type inflight struct {
	id     string
	cancel context.CancelFunc
	done   chan struct{}
}

func newSession(opts Options, stream streamDuplex) *session {
	return &session{
		nodeID:   opts.NodeID,
		maxSlots: opts.MaxSlots,
		judge:    opts.Judge,
		stream:   stream,
		slots:    make(chan struct{}, opts.MaxSlots),
		active:   make(map[string]*inflight),
	}
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
	busy, free, ids := s.snapshot()
	return s.send(&judgev1.NodeMessage{
		Body: &judgev1.NodeMessage_Heartbeat{Heartbeat: &judgev1.Heartbeat{
			UnixMs:           time.Now().UnixMilli(),
			BusySlots:        busy,
			FreeSlots:        free,
			ActiveAttemptIds: ids,
		}},
	})
}

func (s *session) snapshot() (busy, free int32, ids []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids = make([]string, 0, len(s.active))
	for id := range s.active {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	busy = int32(len(s.active))
	free = s.maxSlots - busy
	if free < 0 {
		free = 0
	}
	return busy, free, ids
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
			s.start(ctx, msg.GetDispatch())
		case msg.GetInvalidate() != nil:
			s.invalidate(msg.GetInvalidate().GetAttemptId())
		case msg.GetHeartbeatAck() != nil:
			for _, id := range msg.GetHeartbeatAck().GetInvalidateAttemptIds() {
				s.invalidate(id)
			}
		}
	}
}

func (s *session) start(parent context.Context, d *judgev1.DispatchAttempt) {
	id := d.GetAttemptId()
	if id == "" {
		slog.Warn("ignoring dispatch without attempt id", "node_id", s.nodeID)
		return
	}
	ctx, cancel := context.WithCancel(parent)
	job := &inflight{id: id, cancel: cancel, done: make(chan struct{})}

	s.mu.Lock()
	if _, exists := s.active[id]; exists {
		s.mu.Unlock()
		cancel()
		return
	}
	s.active[id] = job
	s.mu.Unlock()

	go s.runAttempt(ctx, job, d)
}

func (s *session) runAttempt(ctx context.Context, job *inflight, d *judgev1.DispatchAttempt) {
	defer close(job.done)
	defer s.drop(job.id)

	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	case <-ctx.Done():
		return
	}

	if ctx.Err() != nil {
		return
	}

	_ = s.send(&judgev1.NodeMessage{
		Body: &judgev1.NodeMessage_Progress{Progress: &judgev1.AttemptProgress{
			AttemptId:  d.GetAttemptId(),
			Generation: d.GetGeneration(),
			Stage:      "running",
		}},
	})

	res := s.judge(ctx, d)
	if res == nil || ctx.Err() != nil {
		return
	}
	if res.GetNodeId() == "" {
		res.NodeId = s.nodeID
	}
	if err := s.send(&judgev1.NodeMessage{
		Body: &judgev1.NodeMessage_Result{Result: res},
	}); err != nil {
		slog.Info("failed to send attempt result", "attempt_id", job.id, "err", err)
	}
}

func (s *session) invalidate(id string) {
	if id == "" {
		return
	}
	s.mu.Lock()
	job := s.active[id]
	s.mu.Unlock()
	if job != nil {
		job.cancel()
	}
}

func (s *session) drop(id string) {
	s.mu.Lock()
	delete(s.active, id)
	s.mu.Unlock()
}

func (s *session) cancelAll() {
	s.mu.Lock()
	jobs := make([]*inflight, 0, len(s.active))
	for _, job := range s.active {
		jobs = append(jobs, job)
	}
	s.mu.Unlock()
	for _, job := range jobs {
		job.cancel()
	}
}

func (s *session) waitAll() {
	s.mu.Lock()
	dones := make([]chan struct{}, 0, len(s.active))
	for _, job := range s.active {
		dones = append(dones, job.done)
	}
	s.mu.Unlock()
	for _, done := range dones {
		<-done
	}
}

func (s *session) shutdown() {
	s.cancelAll()
	s.waitAll()
}
