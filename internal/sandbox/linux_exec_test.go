//go:build linux

package sandbox

import (
	"context"
	"strings"
	"testing"
	"time"
)

func testLinuxSession(t *testing.T) Session {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	sess, err := DefaultHost().NewSession(ctx)
	if err != nil {
		t.Skipf("sandbox session unavailable: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

func TestLinuxExecEcho(t *testing.T) {
	sess := testLinuxSession(t)
	res := sess.Exec(context.Background(), Spec{
		Role: RoleUser,
		Args: []string{"/bin/echo", "ok"},
		Env:  DefaultEnv(),
		Limits: Limits{
			CPU:         time.Second,
			Wall:        3 * time.Second,
			MemoryBytes: 32 << 20,
			OutputBytes: 1 << 20,
			Processes:   8,
		},
	})
	if res.RunnerFailed() {
		t.Skipf("sandbox exec unavailable: %s", res.Error)
	}
	if res.Status != StatusOK || strings.TrimSpace(string(res.Stdout)) != "ok" {
		t.Fatalf("echo: status=%v exit=%d out=%q err=%q", res.Status, res.ExitCode, res.Stdout, res.Error)
	}
}

func TestLinuxExecPairEOF(t *testing.T) {
	sess := testLinuxSession(t)
	lim := Limits{
		CPU:         time.Second,
		Wall:        3 * time.Second,
		MemoryBytes: 32 << 20,
		OutputBytes: 1 << 20,
		Processes:   8,
	}
	user, peer := sess.ExecPair(context.Background(), Spec{
		Role:   RoleUser,
		Args:   []string{"/bin/echo", "ping"},
		Env:    DefaultEnv(),
		Limits: lim,
	}, Spec{
		Role:   RoleInteractor,
		Args:   []string{"/bin/cat"},
		Env:    DefaultEnv(),
		Limits: lim,
	})
	if user.RunnerFailed() || peer.RunnerFailed() {
		t.Skipf("sandbox pair unavailable: user=%s peer=%s", user.Error, peer.Error)
	}
	if user.Status == StatusTimeLimit || peer.Status == StatusTimeLimit {
		t.Fatal("interactive peer waited for EOF until timeout; write ends were not closed")
	}
	if user.Status != StatusOK || user.ExitCode != 0 {
		t.Fatalf("user: status=%v exit=%d err=%q", user.Status, user.ExitCode, user.Error)
	}
	if peer.Status != StatusOK || peer.ExitCode != 0 {
		t.Fatalf("peer: status=%v exit=%d err=%q", peer.Status, peer.ExitCode, peer.Error)
	}
}
