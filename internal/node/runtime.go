package node

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"sort"
	"sync"

	judgev1 "github.com/jellyfish-p/ephyra/protocol/judge/v1"
)

var errNoStream = errors.New("judge control stream is not attached")

type runtime struct {
	runCtx   context.Context
	nodeID   string
	maxSlots int32
	judge    JudgeFunc
	ledger   *ledger
	slots    chan struct{}

	mu   sync.Mutex
	jobs map[string]*inflight
	sess *session
}

type inflight struct {
	id          string
	cancel      context.CancelFunc
	done        chan struct{}
	invalidated bool
}

func newRuntime(ctx context.Context, opts Options) (*runtime, error) {
	led, err := openLedger(filepath.Join(opts.WorkDir, "ledger"))
	if err != nil {
		return nil, err
	}
	return &runtime{
		runCtx:   ctx,
		nodeID:   opts.NodeID,
		maxSlots: opts.MaxSlots,
		judge:    opts.Judge,
		ledger:   led,
		slots:    make(chan struct{}, opts.MaxSlots),
		jobs:     make(map[string]*inflight),
	}, nil
}

func (rt *runtime) bind(s *session) {
	rt.mu.Lock()
	rt.sess = s
	rt.mu.Unlock()
}

func (rt *runtime) unbind(s *session) {
	rt.mu.Lock()
	if rt.sess == s {
		rt.sess = nil
	}
	rt.mu.Unlock()
}

func (rt *runtime) send(msg *judgev1.NodeMessage) error {
	rt.mu.Lock()
	s := rt.sess
	rt.mu.Unlock()
	if s == nil {
		return errNoStream
	}
	return s.send(msg)
}

func (rt *runtime) flushPending() {
	for _, rec := range rt.ledger.unreported() {
		res := unmarshalResult(rec.Result)
		if res == nil {
			continue
		}
		if res.GetNodeId() == "" {
			res.NodeId = rt.nodeID
		}
		if err := rt.send(&judgev1.NodeMessage{
			Body: &judgev1.NodeMessage_Result{Result: res},
		}); err != nil {
			slog.Info("failed to retransmit attempt result", "attempt_id", rec.AttemptID, "err", err)
			return
		}
		_ = rt.ledger.markReported(rec.AttemptID)
	}
}

func (rt *runtime) snapshot() (busy, free int32, ids []string) {
	seen := make(map[string]struct{})
	rt.mu.Lock()
	for id := range rt.jobs {
		seen[id] = struct{}{}
	}
	rt.mu.Unlock()
	for _, id := range rt.ledger.occupyingIDs() {
		seen[id] = struct{}{}
	}
	ids = make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	busy = int32(len(ids))
	free = rt.maxSlots - busy
	if free < 0 {
		free = 0
	}
	return busy, free, ids
}

func (rt *runtime) start(d *judgev1.DispatchAttempt) {
	id := d.GetAttemptId()
	if id == "" {
		slog.Warn("ignoring dispatch without attempt id", "node_id", rt.nodeID)
		return
	}

	if rec := rt.ledger.get(id); rec != nil {
		switch rec.Phase {
		case phaseInvalidated:
			return
		case phaseFinished, phaseUncertain:
			rt.retransmit(rec)
			return
		case phaseRunning:
			rt.mu.Lock()
			_, inflight := rt.jobs[id]
			rt.mu.Unlock()
			if inflight {
				return
			}
			// Process restart already converted orphans to uncertain.
			// A running record without a job is not re-executed.
			return
		}
	}

	rt.mu.Lock()
	if _, exists := rt.jobs[id]; exists {
		rt.mu.Unlock()
		return
	}
	rt.mu.Unlock()

	if err := rt.ledger.begin(d, rt.nodeID); err != nil {
		slog.Info("failed to persist attempt start", "attempt_id", id, "err", err)
		return
	}

	ctx, cancel := context.WithCancel(rt.runCtx)
	job := &inflight{id: id, cancel: cancel, done: make(chan struct{})}
	rt.mu.Lock()
	if _, exists := rt.jobs[id]; exists {
		rt.mu.Unlock()
		cancel()
		return
	}
	rt.jobs[id] = job
	rt.mu.Unlock()

	go rt.runAttempt(ctx, job, d)
}

func (rt *runtime) retransmit(rec *record) {
	res := unmarshalResult(rec.Result)
	if res == nil {
		return
	}
	if res.GetNodeId() == "" {
		res.NodeId = rt.nodeID
	}
	if err := rt.send(&judgev1.NodeMessage{
		Body: &judgev1.NodeMessage_Result{Result: res},
	}); err != nil {
		slog.Info("failed to retransmit attempt result", "attempt_id", rec.AttemptID, "err", err)
		return
	}
	_ = rt.ledger.markReported(rec.AttemptID)
}

func (rt *runtime) runAttempt(ctx context.Context, job *inflight, d *judgev1.DispatchAttempt) {
	defer close(job.done)
	defer rt.dropJob(job.id)

	select {
	case rt.slots <- struct{}{}:
		defer func() { <-rt.slots }()
	case <-ctx.Done():
		if rt.jobInvalidated(job.id) {
			_ = rt.ledger.markInvalidated(job.id)
		}
		return
	}

	if ctx.Err() != nil {
		if rt.jobInvalidated(job.id) {
			_ = rt.ledger.markInvalidated(job.id)
		}
		return
	}

	_ = rt.send(&judgev1.NodeMessage{
		Body: &judgev1.NodeMessage_Progress{Progress: &judgev1.AttemptProgress{
			AttemptId:  d.GetAttemptId(),
			Generation: d.GetGeneration(),
			Stage:      "running",
		}},
	})

	res := rt.judge(ctx, d)
	if rt.jobInvalidated(job.id) {
		_ = rt.ledger.markInvalidated(job.id)
		return
	}
	if res == nil || ctx.Err() != nil {
		return
	}
	if res.GetNodeId() == "" {
		res.NodeId = rt.nodeID
	}
	if err := rt.ledger.finish(job.id, res); err != nil {
		slog.Info("failed to persist attempt result", "attempt_id", job.id, "err", err)
		return
	}
	if err := rt.send(&judgev1.NodeMessage{
		Body: &judgev1.NodeMessage_Result{Result: res},
	}); err != nil {
		slog.Info("failed to send attempt result", "attempt_id", job.id, "err", err)
		return
	}
	_ = rt.ledger.markReported(job.id)
}

func (rt *runtime) invalidate(id string) {
	if id == "" {
		return
	}
	rt.mu.Lock()
	job := rt.jobs[id]
	if job != nil {
		job.invalidated = true
		job.cancel()
	}
	rt.mu.Unlock()
	if job == nil {
		_ = rt.ledger.markInvalidated(id)
	}
}

func (rt *runtime) jobInvalidated(id string) bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	job := rt.jobs[id]
	return job != nil && job.invalidated
}

func (rt *runtime) dropJob(id string) {
	rt.mu.Lock()
	delete(rt.jobs, id)
	rt.mu.Unlock()
}

func (rt *runtime) cancelAll() {
	rt.mu.Lock()
	jobs := make([]*inflight, 0, len(rt.jobs))
	for _, job := range rt.jobs {
		jobs = append(jobs, job)
	}
	rt.mu.Unlock()
	for _, job := range jobs {
		job.cancel()
	}
}

func (rt *runtime) waitAll() {
	rt.mu.Lock()
	dones := make([]chan struct{}, 0, len(rt.jobs))
	for _, job := range rt.jobs {
		dones = append(dones, job.done)
	}
	rt.mu.Unlock()
	for _, done := range dones {
		<-done
	}
}

func (rt *runtime) shutdown() {
	rt.cancelAll()
	rt.waitAll()
}
