//go:build linux

package sandbox

import (
	"context"
	"syscall"
	"testing"
	"time"

	"github.com/criyle/go-sandbox/runner"
)

func TestDefaultHostLinux(t *testing.T) {
	h := DefaultHost()
	if _, ok := h.(LinuxHost); !ok {
		t.Fatalf("DefaultHost() = %T, want LinuxHost", h)
	}
}

func TestMapRunnerStatus(t *testing.T) {
	t.Parallel()
	deadline, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-deadline.Done()

	cases := []struct {
		name string
		rr   runner.Result
		ctx  context.Context
		want Status
	}{
		{name: "normal", rr: runner.Result{Status: runner.StatusNormal, ExitStatus: 0}, ctx: context.Background(), want: StatusOK},
		{name: "nonzero still ok", rr: runner.Result{Status: runner.StatusNonzeroExitStatus, ExitStatus: 1}, ctx: context.Background(), want: StatusOK},
		{name: "tle", rr: runner.Result{Status: runner.StatusTimeLimitExceeded}, ctx: context.Background(), want: StatusTimeLimit},
		{name: "mle", rr: runner.Result{Status: runner.StatusMemoryLimitExceeded}, ctx: context.Background(), want: StatusMemoryLimit},
		{name: "ole", rr: runner.Result{Status: runner.StatusOutputLimitExceeded}, ctx: context.Background(), want: StatusOutputLimit},
		{name: "sigxcpu", rr: runner.Result{Status: runner.StatusSignalled, ExitStatus: int(syscall.SIGXCPU)}, ctx: context.Background(), want: StatusTimeLimit},
		{name: "sigkill", rr: runner.Result{Status: runner.StatusSignalled, ExitStatus: int(syscall.SIGKILL)}, ctx: context.Background(), want: StatusTimeLimit},
		{name: "sigxfsz", rr: runner.Result{Status: runner.StatusSignalled, ExitStatus: int(syscall.SIGXFSZ)}, ctx: context.Background(), want: StatusOutputLimit},
		{name: "sigsegv", rr: runner.Result{Status: runner.StatusSignalled, ExitStatus: int(syscall.SIGSEGV)}, ctx: context.Background(), want: StatusRuntime},
		{name: "disallowed", rr: runner.Result{Status: runner.StatusDisallowedSyscall}, ctx: context.Background(), want: StatusRuntime},
		{name: "runner error", rr: runner.Result{Status: runner.StatusRunnerError, Error: "boom"}, ctx: context.Background(), want: StatusRunnerError},
		{name: "deadline", rr: runner.Result{Status: runner.StatusRunnerError}, ctx: deadline, want: StatusTimeLimit},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := mapRunnerStatus(tc.rr, tc.ctx); got != tc.want {
				t.Fatalf("mapRunnerStatus(%v) = %v, want %v", tc.rr.Status, got, tc.want)
			}
		})
	}
}

func TestCPUSeconds(t *testing.T) {
	t.Parallel()
	if cpuSeconds(0) != 1 {
		t.Fatal("zero cpu")
	}
	if cpuSeconds(100*time.Millisecond) != 1 {
		t.Fatal("subsecond cpu")
	}
	if cpuSeconds(1500*time.Millisecond) != 2 {
		t.Fatal("ceil cpu")
	}
}
