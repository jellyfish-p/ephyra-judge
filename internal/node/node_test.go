package node

import (
	"bytes"
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	judgev1 "github.com/jellyfish-p/ephyra/protocol/judge/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"
)

const testCredential = "super-secret-credential-xyz"

type fakeControl struct {
	judgev1.UnimplementedJudgeControlServer

	accept     bool
	reject     string
	intervalMS int32

	handshake  chan *judgev1.HandshakeRequest
	results    chan *judgev1.AttemptResult
	heartbeats chan *judgev1.Heartbeat
	outbound   chan *judgev1.ServerMessage
	ready      chan struct{}
	readyOnce  sync.Once
	drops      chan struct{}
	conns      int32
}

func newFake(accept bool, reject string) *fakeControl {
	return &fakeControl{
		accept:     accept,
		reject:     reject,
		intervalMS: 40,
		handshake:  make(chan *judgev1.HandshakeRequest, 8),
		results:    make(chan *judgev1.AttemptResult, 8),
		heartbeats: make(chan *judgev1.Heartbeat, 64),
		outbound:   make(chan *judgev1.ServerMessage, 16),
		ready:      make(chan struct{}),
	}
}

func (f *fakeControl) Connect(stream judgev1.JudgeControl_ConnectServer) error {
	connN := atomic.AddInt32(&f.conns, 1)
	msg, err := stream.Recv()
	if err != nil {
		return err
	}
	hs := msg.GetHandshake()
	if hs == nil {
		return errors.New("first message must be handshake")
	}
	select {
	case f.handshake <- hs:
	default:
	}
	if err := stream.Send(&judgev1.ServerMessage{
		Body: &judgev1.ServerMessage_Handshake{Handshake: &judgev1.HandshakeResponse{
			Accepted:            f.accept,
			RejectReason:        f.reject,
			ProtocolVersion:     "1",
			HeartbeatIntervalMs: f.intervalMS,
			LeaseMs:             60000,
		}},
	}); err != nil {
		return err
	}
	if !f.accept {
		return nil
	}
	f.readyOnce.Do(func() { close(f.ready) })

	ctx := stream.Context()
	var drop <-chan struct{}
	if connN == 1 && f.drops != nil {
		drop = f.drops
	}
	recvErr := make(chan error, 1)
	go func() {
		for {
			m, err := stream.Recv()
			if err != nil {
				recvErr <- err
				return
			}
			switch {
			case m.GetHeartbeat() != nil:
				select {
				case f.heartbeats <- m.GetHeartbeat():
				default:
				}
			case m.GetResult() != nil:
				select {
				case f.results <- m.GetResult():
				case <-ctx.Done():
					recvErr <- ctx.Err()
					return
				}
			}
		}
	}()
	for {
		select {
		case err := <-recvErr:
			return err
		case m := <-f.outbound:
			if err := stream.Send(m); err != nil {
				return err
			}
		case <-drop:
			return errors.New("test disconnect")
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (f *fakeControl) dispatch(d *judgev1.DispatchAttempt) {
	f.outbound <- &judgev1.ServerMessage{Body: &judgev1.ServerMessage_Dispatch{Dispatch: d}}
}

func (f *fakeControl) invalidate(id string, gen int32) {
	f.outbound <- &judgev1.ServerMessage{Body: &judgev1.ServerMessage_Invalidate{
		Invalidate: &judgev1.InvalidateAttempt{AttemptId: id, Generation: gen},
	}}
}

func (f *fakeControl) ackInvalidate(ids ...string) {
	f.outbound <- &judgev1.ServerMessage{Body: &judgev1.ServerMessage_HeartbeatAck{
		HeartbeatAck: &judgev1.HeartbeatAck{
			UnixMs:               time.Now().UnixMilli(),
			InvalidateAttemptIds: ids,
		},
	}}
}

func testOpts(t *testing.T, fake *fakeControl, judge JudgeFunc) Options {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	judgev1.RegisterJudgeControlServer(gs, fake)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(func() {
		gs.Stop()
		_ = lis.Close()
	})
	if judge == nil {
		judge = func(context.Context, *judgev1.DispatchAttempt) *judgev1.AttemptResult { return nil }
	}
	return Options{
		Addr:       "passthrough:///bufnet",
		NodeID:     "node-1",
		Credential: testCredential,
		WorkDir:    t.TempDir(),
		Insecure:   true,
		MaxSlots:   1,
		Judge:      judge,
		ExecutionProfileIDs: []string{
			"cpp23.gcc.linux-amd64.v0",
			"c17.gcc.linux-amd64.v0",
			"c23.gcc.linux-amd64.v0",
		},
		dialer: func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		},
	}
}

func waitReady(t *testing.T, fake *fakeControl) {
	t.Helper()
	select {
	case <-fake.ready:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for handshake")
	}
	waitHeartbeat(t, fake, 3*time.Second, func(*judgev1.Heartbeat) bool { return true })
}

func startRun(t *testing.T, opts Options) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- Run(ctx, opts) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-errc:
			if err != nil {
				t.Errorf("Run: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("Run did not stop")
		}
	})
	return cancel
}

func waitResult(t *testing.T, fake *fakeControl, timeout time.Duration) *judgev1.AttemptResult {
	t.Helper()
	select {
	case r := <-fake.results:
		return r
	case <-time.After(timeout):
		t.Fatal("timeout waiting for AttemptResult")
		return nil
	}
}

func waitHeartbeat(t *testing.T, fake *fakeControl, timeout time.Duration, pred func(*judgev1.Heartbeat) bool) *judgev1.Heartbeat {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case hb := <-fake.heartbeats:
			if pred(hb) {
				return hb
			}
		case <-deadline:
			t.Fatal("timeout waiting for heartbeat")
			return nil
		}
	}
}

func TestPackageExports(t *testing.T) {
	fset := token.NewFileSet()
	want := map[string]bool{"Options": true, "JudgeFunc": true, "FromEnv": true, "Run": true}
	got := map[string]bool{}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				if d.Recv == nil && d.Name.IsExported() {
					got[d.Name.Name] = true
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					if ts, ok := spec.(*ast.TypeSpec); ok && ts.Name.IsExported() {
						got[ts.Name.Name] = true
					}
				}
			}
		}
	}
	for name := range want {
		if !got[name] {
			t.Errorf("missing export %s", name)
		}
	}
	for name := range got {
		if !want[name] {
			t.Errorf("unexpected export %s", name)
		}
	}
}

func TestFromEnvMissingAddr(t *testing.T) {
	t.Setenv("EPHYRA_JUDGE_ADDR", "")
	t.Setenv("EPHYRA_JUDGE_NODE_ID", "n")
	t.Setenv("EPHYRA_JUDGE_CREDENTIAL", testCredential)
	_, err := FromEnv()
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "EPHYRA_JUDGE_ADDR") {
		t.Fatalf("got %v", err)
	}
	if strings.Contains(err.Error(), testCredential) {
		t.Fatal("credential leaked in FromEnv error")
	}
}

func TestFromEnvDefaults(t *testing.T) {
	t.Setenv("EPHYRA_JUDGE_ADDR", "127.0.0.1:9090")
	t.Setenv("EPHYRA_JUDGE_NODE_ID", "node-1")
	t.Setenv("EPHYRA_JUDGE_CREDENTIAL", testCredential)
	t.Setenv("EPHYRA_JUDGE_WORKDIR", "")
	t.Setenv("EPHYRA_JUDGE_MAX_SLOTS", "")
	t.Setenv("EPHYRA_JUDGE_INSECURE", "1")
	t.Setenv("EPHYRA_JUDGE_CA_FILE", "")
	opts, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if opts.WorkDir != "work" || opts.MaxSlots != 1 || !opts.Insecure {
		t.Fatalf("%+v", opts)
	}
	if opts.Credential != testCredential {
		t.Fatal("credential not preserved")
	}
}

func TestRunRequiresExecutionProfileIDs(t *testing.T) {
	opts := Options{
		Addr:       "127.0.0.1:1",
		NodeID:     "node-1",
		Credential: testCredential,
		Judge:      func(context.Context, *judgev1.DispatchAttempt) *judgev1.AttemptResult { return nil },
	}
	err := Run(context.Background(), opts)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "execution profile") {
		t.Fatalf("got %v", err)
	}
}

func TestRunHandshakeRejected(t *testing.T) {
	fake := newFake(false, "unknown node")
	opts := testOpts(t, fake, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := Run(ctx, opts)
	if err == nil {
		t.Fatal("expected handshake rejection")
	}
	if !errors.Is(err, errHandshakeRejected) {
		t.Fatalf("got %v", err)
	}
	if !strings.Contains(err.Error(), "unknown node") {
		t.Fatalf("reject_reason missing: %v", err)
	}
}

func TestRunDispatchAccepted(t *testing.T) {
	fake := newFake(true, "")
	opts := testOpts(t, fake, func(_ context.Context, d *judgev1.DispatchAttempt) *judgev1.AttemptResult {
		return &judgev1.AttemptResult{
			AttemptId:  d.GetAttemptId(),
			TaskId:     d.GetTaskId(),
			Generation: d.GetGeneration(),
			Verdict:    "accepted",
			Score:      "100.00",
		}
	})
	startRun(t, opts)
	waitReady(t, fake)

	hs := <-fake.handshake
	if hs.GetProtocolVersion() != "1" || hs.GetNodeId() != "node-1" {
		t.Fatalf("handshake %+v", hs)
	}
	if hs.GetCredential() != testCredential {
		t.Fatal("server did not receive credential")
	}
	want := []string{
		"cpp23.gcc.linux-amd64.v0",
		"c17.gcc.linux-amd64.v0",
		"c23.gcc.linux-amd64.v0",
	}
	if !slicesEqual(hs.GetExecutionProfileIds(), want) {
		t.Fatalf("profiles %v", hs.GetExecutionProfileIds())
	}
	if hs.GetMaxSlots() != 1 {
		t.Fatalf("max_slots %d", hs.GetMaxSlots())
	}

	fake.dispatch(&judgev1.DispatchAttempt{
		AttemptId:  "att-1",
		TaskId:     "task-1",
		Generation: 3,
	})
	res := waitResult(t, fake, 3*time.Second)
	if res.GetAttemptId() != "att-1" || res.GetGeneration() != 3 || res.GetNodeId() != "node-1" {
		t.Fatalf("result %+v", res)
	}
	if res.GetVerdict() != "accepted" {
		t.Fatalf("verdict %q", res.GetVerdict())
	}
	waitHeartbeat(t, fake, 3*time.Second, func(hb *judgev1.Heartbeat) bool {
		return hb.GetBusySlots() == 0 && hb.GetFreeSlots() == 1 && len(hb.GetActiveAttemptIds()) == 0
	})
}

func TestRunInvalidateMidJudge(t *testing.T) {
	started := make(chan struct{})
	fake := newFake(true, "")
	opts := testOpts(t, fake, func(ctx context.Context, _ *judgev1.DispatchAttempt) *judgev1.AttemptResult {
		close(started)
		<-ctx.Done()
		return nil
	})
	startRun(t, opts)
	waitReady(t, fake)

	fake.dispatch(&judgev1.DispatchAttempt{AttemptId: "att-inv", Generation: 1})
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("judge did not start")
	}

	fake.invalidate("att-inv", 1)
	waitHeartbeat(t, fake, 3*time.Second, func(hb *judgev1.Heartbeat) bool {
		return hb.GetBusySlots() == 0 && hb.GetFreeSlots() == 1 && len(hb.GetActiveAttemptIds()) == 0
	})
	select {
	case res := <-fake.results:
		t.Fatalf("unexpected result %+v", res)
	case <-time.After(150 * time.Millisecond):
	}
}

func TestRunHeartbeatAckInvalidate(t *testing.T) {
	started := make(chan struct{})
	fake := newFake(true, "")
	opts := testOpts(t, fake, func(ctx context.Context, _ *judgev1.DispatchAttempt) *judgev1.AttemptResult {
		close(started)
		<-ctx.Done()
		return nil
	})
	startRun(t, opts)
	waitReady(t, fake)

	fake.dispatch(&judgev1.DispatchAttempt{AttemptId: "att-ack", Generation: 2})
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("judge did not start")
	}
	fake.ackInvalidate("att-ack")
	waitHeartbeat(t, fake, 3*time.Second, func(hb *judgev1.Heartbeat) bool {
		return hb.GetFreeSlots() == 1 && len(hb.GetActiveAttemptIds()) == 0
	})
	select {
	case res := <-fake.results:
		t.Fatalf("unexpected result %+v", res)
	case <-time.After(150 * time.Millisecond):
	}
}

func TestRunHeartbeatWhileJudgeBlocks(t *testing.T) {
	started := make(chan struct{})
	fake := newFake(true, "")
	opts := testOpts(t, fake, func(ctx context.Context, d *judgev1.DispatchAttempt) *judgev1.AttemptResult {
		close(started)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(350 * time.Millisecond):
			return &judgev1.AttemptResult{AttemptId: d.GetAttemptId(), Generation: d.GetGeneration(), Verdict: "accepted"}
		}
	})
	startRun(t, opts)
	waitReady(t, fake)

	fake.dispatch(&judgev1.DispatchAttempt{AttemptId: "att-slow", Generation: 1})
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("judge did not start")
	}

	n := 0
	deadline := time.After(300 * time.Millisecond)
	for {
		select {
		case hb := <-fake.heartbeats:
			if containsID(hb.GetActiveAttemptIds(), "att-slow") {
				n++
			}
		case <-deadline:
			if n < 2 {
				t.Fatalf("expected heartbeats while judge blocked, got %d", n)
			}
			return
		}
	}
}

func TestRunDuplicateDispatchDoesNotRerun(t *testing.T) {
	var n atomic.Int32
	fake := newFake(true, "")
	opts := testOpts(t, fake, func(_ context.Context, d *judgev1.DispatchAttempt) *judgev1.AttemptResult {
		n.Add(1)
		return &judgev1.AttemptResult{
			AttemptId:  d.GetAttemptId(),
			Generation: d.GetGeneration(),
			Verdict:    "accepted",
			Score:      "100.00",
		}
	})
	startRun(t, opts)
	waitReady(t, fake)

	d := &judgev1.DispatchAttempt{AttemptId: "att-dup", Generation: 1}
	fake.dispatch(d)
	first := waitResult(t, fake, 3*time.Second)
	if first.GetAttemptId() != "att-dup" {
		t.Fatalf("first %+v", first)
	}
	fake.dispatch(d)
	second := waitResult(t, fake, 3*time.Second)
	if second.GetAttemptId() != "att-dup" {
		t.Fatalf("second %+v", second)
	}
	if n.Load() != 1 {
		t.Fatalf("judge calls = %d", n.Load())
	}
}

func TestRunReconnectDoesNotCancelRunning(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var n atomic.Int32
	fake := newFake(true, "")
	fake.drops = make(chan struct{})
	opts := testOpts(t, fake, func(ctx context.Context, d *judgev1.DispatchAttempt) *judgev1.AttemptResult {
		n.Add(1)
		close(started)
		select {
		case <-release:
			return &judgev1.AttemptResult{AttemptId: d.GetAttemptId(), Generation: d.GetGeneration(), Verdict: "accepted", Score: "100.00"}
		case <-ctx.Done():
			return nil
		}
	})
	startRun(t, opts)
	waitReady(t, fake)
	select {
	case <-fake.handshake:
	case <-time.After(3 * time.Second):
		t.Fatal("missing first handshake")
	}

	fake.dispatch(&judgev1.DispatchAttempt{AttemptId: "att-re", Generation: 1})
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("judge did not start")
	}
	close(fake.drops)

	select {
	case hs := <-fake.handshake:
		if hs.GetNodeId() != "node-1" {
			t.Fatalf("reconnect handshake %+v", hs)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("did not reconnect")
	}
	close(release)
	res := waitResult(t, fake, 3*time.Second)
	if res.GetAttemptId() != "att-re" || res.GetVerdict() != "accepted" {
		t.Fatalf("result %+v", res)
	}
	if n.Load() != 1 {
		t.Fatalf("judge calls = %d", n.Load())
	}
}

func TestRunDoesNotLogCredential(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	fake := newFake(false, "nope")
	opts := testOpts(t, fake, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = Run(ctx, opts)
	out := buf.String()
	if strings.Contains(out, testCredential) {
		t.Fatalf("credential leaked in logs: %s", out)
	}
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func containsID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}
