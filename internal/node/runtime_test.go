package node

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	judgev1 "github.com/jellyfish-p/ephyra/protocol/judge/v1"
	"google.golang.org/protobuf/proto"
)

type memStream struct {
	mu      sync.Mutex
	sendErr error
	results []*judgev1.AttemptResult
}

func (m *memStream) Send(msg *judgev1.NodeMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sendErr != nil {
		return m.sendErr
	}
	if r := msg.GetResult(); r != nil {
		m.results = append(m.results, proto.Clone(r).(*judgev1.AttemptResult))
	}
	return nil
}

func (m *memStream) Recv() (*judgev1.ServerMessage, error) {
	return nil, io.EOF
}

func (m *memStream) got() []*judgev1.AttemptResult {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]*judgev1.AttemptResult(nil), m.results...)
}

func runtimeOpts(t *testing.T, judge JudgeFunc) Options {
	t.Helper()
	if judge == nil {
		judge = func(context.Context, *judgev1.DispatchAttempt) *judgev1.AttemptResult { return nil }
	}
	return Options{
		Addr:       "unused",
		NodeID:     "node-1",
		Credential: testCredential,
		WorkDir:    t.TempDir(),
		MaxSlots:   1,
		Judge:      judge,
		ExecutionProfileIDs: []string{
			"cpp23.gcc.linux-amd64.v0",
		},
	}
}

func attach(rt *runtime, stream streamDuplex) *session {
	sess := newSession(rt, stream)
	rt.bind(sess)
	return sess
}

func TestRuntimeDoesNotRerunFinished(t *testing.T) {
	t.Parallel()
	var n atomic.Int32
	opts := runtimeOpts(t, func(_ context.Context, d *judgev1.DispatchAttempt) *judgev1.AttemptResult {
		n.Add(1)
		return &judgev1.AttemptResult{AttemptId: d.GetAttemptId(), Generation: d.GetGeneration(), Verdict: "accepted", Score: "100.00"}
	})
	ctx := context.Background()
	rt, err := newRuntime(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	stream := &memStream{}
	attach(rt, stream)

	d := &judgev1.DispatchAttempt{AttemptId: "att-1", TaskId: "task-1", Generation: 2}
	rt.start(d)
	rt.waitAll()
	if n.Load() != 1 {
		t.Fatalf("judge calls = %d", n.Load())
	}
	rt.start(d)
	rt.waitAll()
	if n.Load() != 1 {
		t.Fatal("finished attempt was executed again")
	}
	if got := stream.got(); len(got) != 2 {
		t.Fatalf("want retransmit, got %d results", len(got))
	}
}

func TestRuntimeDisconnectDoesNotCancel(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	release := make(chan struct{})
	opts := runtimeOpts(t, func(ctx context.Context, d *judgev1.DispatchAttempt) *judgev1.AttemptResult {
		close(started)
		select {
		case <-release:
			return &judgev1.AttemptResult{AttemptId: d.GetAttemptId(), Generation: d.GetGeneration(), Verdict: "accepted", Score: "100.00"}
		case <-ctx.Done():
			return nil
		}
	})
	ctx := context.Background()
	rt, err := newRuntime(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	first := &memStream{}
	sess := attach(rt, first)
	rt.start(&judgev1.DispatchAttempt{AttemptId: "att-keep", Generation: 1})
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("judge did not start")
	}
	rt.unbind(sess)

	select {
	case <-time.After(150 * time.Millisecond):
	case <-func() chan struct{} {
		rt.mu.Lock()
		job := rt.jobs["att-keep"]
		rt.mu.Unlock()
		if job == nil {
			ch := make(chan struct{})
			close(ch)
			return ch
		}
		return job.done
	}():
		t.Fatal("job finished after stream unbind")
	}

	second := &memStream{}
	attach(rt, second)
	close(release)
	rt.waitAll()
	if len(second.got()) != 1 && len(first.got()) != 1 {
		t.Fatalf("result not delivered after reconnect: first=%d second=%d", len(first.got()), len(second.got()))
	}
}

func TestRuntimeSendFailureKeepsRecord(t *testing.T) {
	t.Parallel()
	opts := runtimeOpts(t, func(_ context.Context, d *judgev1.DispatchAttempt) *judgev1.AttemptResult {
		return &judgev1.AttemptResult{AttemptId: d.GetAttemptId(), Generation: d.GetGeneration(), Verdict: "accepted", Score: "100.00"}
	})
	ctx := context.Background()
	rt, err := newRuntime(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	failing := &memStream{sendErr: errors.New("stream closed")}
	attach(rt, failing)
	rt.start(&judgev1.DispatchAttempt{AttemptId: "att-send", Generation: 1})
	rt.waitAll()
	rec := rt.ledger.get("att-send")
	if rec == nil || rec.Phase != phaseFinished || rec.Reported {
		t.Fatalf("record after send failure = %+v", rec)
	}

	ok := &memStream{}
	attach(rt, ok)
	rt.flushPending()
	got := ok.got()
	if len(got) != 1 || got[0].GetAttemptId() != "att-send" {
		t.Fatalf("retransmit = %+v", got)
	}
	if rec := rt.ledger.get("att-send"); rec == nil || !rec.Reported {
		t.Fatal("result was not marked reported after retransmit")
	}
}

func TestRuntimeOrphanIsUncertainNotRerun(t *testing.T) {
	t.Parallel()
	var n atomic.Int32
	opts := runtimeOpts(t, func(context.Context, *judgev1.DispatchAttempt) *judgev1.AttemptResult {
		n.Add(1)
		return &judgev1.AttemptResult{Verdict: "accepted"}
	})
	led, err := openLedger(filepath.Join(opts.WorkDir, "ledger"))
	if err != nil {
		t.Fatal(err)
	}
	if err := led.begin(&judgev1.DispatchAttempt{
		AttemptId: "att-orphan", TaskId: "task", Generation: 7,
		JudgeDataRevisionId: "rev", ExecutionProfileId: "cpp23.gcc.linux-amd64.v0",
	}, "node-1"); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	rt, err := newRuntime(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	stream := &memStream{}
	attach(rt, stream)
	rt.flushPending()
	got := stream.got()
	if len(got) != 1 || !got[0].GetSystemFault() || got[0].GetSystemFaultKind() != faultUncertainRestart {
		t.Fatalf("orphan flush = %+v", got)
	}

	rt.start(&judgev1.DispatchAttempt{AttemptId: "att-orphan", Generation: 7})
	rt.waitAll()
	if n.Load() != 0 {
		t.Fatal("uncertain attempt was executed")
	}
}
