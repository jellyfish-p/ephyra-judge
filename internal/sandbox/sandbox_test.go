package sandbox

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestWallOrDefault(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   Limits
		want time.Duration
	}{
		{name: "explicit wall", in: Limits{Wall: 4 * time.Second, CPU: time.Second}, want: 4 * time.Second},
		{name: "from cpu", in: Limits{CPU: 2 * time.Second}, want: 2*time.Second*3/2 + 100*time.Millisecond},
		{name: "zero", in: Limits{}, want: 2 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.in.WallOrDefault(); got != tc.want {
				t.Fatalf("WallOrDefault() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMergeEnv(t *testing.T) {
	t.Parallel()
	got := MergeEnv([]string{"FOO=bar"})
	base := DefaultEnv()
	if len(got) != len(base)+1 || got[len(got)-1] != "FOO=bar" {
		t.Fatalf("MergeEnv = %v", got)
	}
	if got[0] != "PATH=/usr/local/bin:/usr/bin:/bin" {
		t.Fatalf("default PATH missing: %v", got)
	}
}

func TestStatusOKIsProcessEnded(t *testing.T) {
	t.Parallel()
	r := Result{Status: StatusOK, ExitCode: 0}
	if r.RunnerFailed() {
		t.Fatal("StatusOK is not a runner failure")
	}
}

func TestFakeQueue(t *testing.T) {
	f := &Fake{Queue: []Result{
		{Status: StatusTimeLimit, ExitCode: 137},
		{Status: StatusOK, ExitCode: 1, Stdout: []byte("out")},
	}}
	sess, err := f.NewSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	first := sess.Exec(context.Background(), Spec{Role: RoleUser, Args: []string{"/w/program"}})
	if first.Status != StatusTimeLimit || first.ExitCode != 137 {
		t.Fatalf("queue[0] = %+v", first)
	}
	second := sess.Exec(context.Background(), Spec{Role: RoleChecker})
	if second.Status != StatusOK || second.ExitCode != 1 || string(second.Stdout) != "out" {
		t.Fatalf("queue[1] = %+v", second)
	}
	calls := f.PopCalls()
	if len(calls) != 2 || calls[0].Role != RoleUser || calls[1].Role != RoleChecker {
		t.Fatalf("calls = %+v", calls)
	}
}

func TestFakeScript(t *testing.T) {
	var seen Role
	f := &Fake{
		Queue: []Result{{Status: StatusMemoryLimit}},
		Script: func(_ context.Context, spec Spec) Result {
			seen = spec.Role
			return Result{Status: StatusRuntime, ExitCode: 139}
		},
	}
	sess, err := f.NewSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	got := sess.Exec(context.Background(), Spec{Role: RoleCompile})
	if got.Status != StatusRuntime || got.ExitCode != 139 {
		t.Fatalf("script result = %+v", got)
	}
	if seen != RoleCompile {
		t.Fatalf("script saw %q", seen)
	}
	if len(f.Queue) != 1 {
		t.Fatalf("script should not consume queue, left %d", len(f.Queue))
	}
}

func TestFakeExecFnAlias(t *testing.T) {
	f := &Fake{
		ExecFn: func(context.Context, Spec) Result {
			return Result{Status: StatusOutputLimit}
		},
	}
	sess, err := f.NewSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	got := sess.Exec(context.Background(), Spec{})
	if got.Status != StatusOutputLimit {
		t.Fatalf("ExecFn result = %+v", got)
	}
}

func TestFakeExecPairFallback(t *testing.T) {
	f := &Fake{Queue: []Result{
		{Status: StatusOK, ExitCode: 0, Stdout: []byte("user")},
		{Status: StatusOK, ExitCode: 1, Stdout: []byte("peer")},
	}}
	sess, err := f.NewSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	user, peer := sess.ExecPair(context.Background(), Spec{Role: RoleUser}, Spec{Role: RoleInteractor})
	if user.Status != StatusOK || string(user.Stdout) != "user" {
		t.Fatalf("user = %+v", user)
	}
	if peer.Status != StatusOK || peer.ExitCode != 1 || string(peer.Stdout) != "peer" {
		t.Fatalf("peer = %+v", peer)
	}
	calls := f.PopCalls()
	if len(calls) != 2 || calls[0].Role != RoleUser || calls[1].Role != RoleInteractor {
		t.Fatalf("calls = %+v", calls)
	}
}

func TestFakeExecPairFn(t *testing.T) {
	f := &Fake{
		Queue: []Result{{Status: StatusTimeLimit}},
		PairFn: func(context.Context, Spec, Spec) (Result, Result) {
			return Result{Status: StatusOK, ExitCode: 0}, Result{Status: StatusOK, ExitCode: 2}
		},
	}
	sess, err := f.NewSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	user, peer := sess.ExecPair(context.Background(), Spec{Role: RoleUser}, Spec{Role: RoleInteractor})
	if user.ExitCode != 0 || peer.ExitCode != 2 {
		t.Fatalf("pair = %+v %+v", user, peer)
	}
	if len(f.Queue) != 1 {
		t.Fatal("PairFn should not fall back to queue")
	}
}

func TestRunnerFailed(t *testing.T) {
	t.Parallel()
	if !(Result{Status: StatusRunnerError}).RunnerFailed() {
		t.Fatal("StatusRunnerError should fail the runner")
	}
	for _, st := range []Status{StatusOK, StatusTimeLimit, StatusMemoryLimit, StatusOutputLimit, StatusRuntime} {
		if (Result{Status: st}).RunnerFailed() {
			t.Fatalf("status %d is not a runner failure", st)
		}
	}
}

func TestFakeNewErr(t *testing.T) {
	f := &Fake{NewErr: errors.New("no session")}
	if _, err := f.NewSession(context.Background()); err == nil {
		t.Fatal("expected error")
	}
}

func TestFakeDefaultDoesNotRunCode(t *testing.T) {
	sess, err := (&Fake{}).NewSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	got := sess.Exec(context.Background(), Spec{
		Role: RoleUser,
		Args: []string{"/w/program"},
		Files: map[string][]byte{
			"program": []byte("this is not executed"),
		},
	})
	if got.Status != StatusOK {
		t.Fatalf("default fake status = %v", got.Status)
	}
	if string(got.Files["program"]) != "\x7fELF" {
		t.Fatalf("default files = %v", got.Files)
	}
}
