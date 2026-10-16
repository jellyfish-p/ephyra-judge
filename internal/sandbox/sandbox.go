package sandbox

import (
	"context"
	"time"
)

type Role string

const (
	RoleCompile    Role = "compile"
	RoleUser       Role = "user"
	RoleChecker    Role = "checker"
	RoleInteractor Role = "interactor"
)

type Limits struct {
	CPU         time.Duration
	Wall        time.Duration
	MemoryBytes int64
	OutputBytes int64
}

func (l Limits) WallOrDefault() time.Duration {
	if l.Wall > 0 {
		return l.Wall
	}
	if l.CPU <= 0 {
		return 2 * time.Second
	}
	return l.CPU*3/2 + 100*time.Millisecond
}

type Spec struct {
	Role    Role
	Args    []string
	Env     []string
	Files   map[string][]byte
	Stdin   []byte
	Limits  Limits
	Collect []string
}

type Status int

const (
	// StatusOK means the process ended. It is not "accepted".
	StatusOK Status = iota
	StatusTimeLimit
	StatusMemoryLimit
	StatusOutputLimit
	StatusRuntime
	StatusRunnerError
)

type Result struct {
	Status      Status
	ExitCode    int
	Time        time.Duration
	Wall        time.Duration
	MemoryBytes int64
	Stdout      []byte
	Stderr      []byte
	Files       map[string][]byte
	Error       string
}

func (r Result) RunnerFailed() bool {
	return r.Status == StatusRunnerError
}

type Session interface {
	Exec(ctx context.Context, spec Spec) Result
	ExecPair(ctx context.Context, user, peer Spec) (Result, Result)
	Close() error
}

type Host interface {
	NewSession(ctx context.Context) (Session, error)
}

func DefaultEnv() []string {
	return []string{
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"HOME=/w",
		"LANG=C.UTF-8",
		"LC_ALL=C.UTF-8",
	}
}

func MergeEnv(extra []string) []string {
	out := DefaultEnv()
	return append(out, extra...)
}
