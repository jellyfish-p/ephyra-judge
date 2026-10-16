package sandbox

import (
	"context"
	"sync"
)

// Fake is a host for tests. It never executes untrusted code.
type Fake struct {
	mu         sync.Mutex
	Script     func(ctx context.Context, spec Spec) Result
	ExecFn     func(ctx context.Context, spec Spec) Result
	PairFn     func(ctx context.Context, user, peer Spec) (Result, Result)
	Queue      []Result
	Calls      []Spec
	NewErr     error
	SessionErr error
}

func (f *Fake) NewSession(context.Context) (Session, error) {
	if f.NewErr != nil {
		return nil, f.NewErr
	}
	return &fakeSession{host: f}, nil
}

type fakeSession struct {
	host *Fake
}

func (s *fakeSession) Exec(ctx context.Context, spec Spec) Result {
	s.host.mu.Lock()
	s.host.Calls = append(s.host.Calls, spec)
	fn := s.host.script()
	var next *Result
	if fn == nil && len(s.host.Queue) > 0 {
		r := s.host.Queue[0]
		s.host.Queue = s.host.Queue[1:]
		next = &r
	}
	s.host.mu.Unlock()
	if fn != nil {
		return fn(ctx, spec)
	}
	if next != nil {
		return *next
	}
	return Result{Status: StatusOK, Files: map[string][]byte{"program": {0x7f, 'E', 'L', 'F'}}}
}

func (s *fakeSession) ExecPair(ctx context.Context, user, peer Spec) (Result, Result) {
	s.host.mu.Lock()
	fn := s.host.PairFn
	s.host.mu.Unlock()
	if fn != nil {
		s.host.mu.Lock()
		s.host.Calls = append(s.host.Calls, user, peer)
		s.host.mu.Unlock()
		return fn(ctx, user, peer)
	}
	u := s.Exec(ctx, user)
	p := s.Exec(ctx, peer)
	return u, p
}

func (s *fakeSession) Close() error {
	return s.host.SessionErr
}

func (f *Fake) script() func(context.Context, Spec) Result {
	if f.Script != nil {
		return f.Script
	}
	return f.ExecFn
}

func (f *Fake) PopCalls() []Spec {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]Spec(nil), f.Calls...)
	f.Calls = nil
	return out
}

var (
	_ Host    = (*Fake)(nil)
	_ Session = (*fakeSession)(nil)
)
