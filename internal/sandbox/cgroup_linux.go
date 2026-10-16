//go:build linux

package sandbox

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/criyle/go-sandbox/pkg/cgroup"
)

func tryCgroupRoot() cgroup.Cgroup {
	if cgroup.DetectedCgroupType == cgroup.TypeV2 {
		if err := cgroup.EnableV2Nesting(); err != nil {
			slog.Warn("cgroup v2 nesting not enabled", "err", err)
		}
	}
	ct, err := cgroup.GetAvailableController()
	if err != nil {
		slog.Warn("cgroup controllers unavailable", "err", err)
		return nil
	}
	root, err := cgroup.New("ephyra-judge", ct)
	if err != nil {
		slog.Warn("cgroup root not created", "err", err)
		return nil
	}
	return root
}

type cgHandle struct {
	cg     cgroup.Cgroup
	file   *os.File
	fd     uintptr
	sync   func(pid int) error
	closer func()
}

func (h *linuxHost) prepareCgroup(spec Spec) *cgHandle {
	if h == nil || h.cgRoot == nil {
		return nil
	}
	cg, err := h.cgRoot.Random("run")
	if err != nil {
		slog.Warn("cgroup create failed", "err", err)
		return nil
	}
	mem := spec.Limits.MemoryBytes
	if mem <= 0 {
		mem = 256 << 20
	}
	_ = cg.SetMemoryLimit(uint64(mem))
	procs := spec.Limits.Processes
	if procs <= 0 {
		procs = defaultProcesses(spec.Role)
	}
	_ = cg.SetProcLimit(uint64(procs))
	const period = uint64(100_000_000)
	_ = cg.SetCPUBandwidth(period, period)

	out := &cgHandle{cg: cg}
	if cgroup.DetectedCgroupType == cgroup.TypeV2 {
		if f, err := cg.Open(); err == nil {
			out.file = f
			out.fd = f.Fd()
		}
	}
	out.sync = func(pid int) error { return cg.AddProc(pid) }
	out.closer = func() {
		if out.file != nil {
			_ = out.file.Close()
		}
		_ = cg.Destroy()
	}
	return out
}

func watchCPU(ctx context.Context, cancel context.CancelFunc, cg cgroup.Cgroup, limit time.Duration) {
	if cg == nil || limit <= 0 {
		return
	}
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			u, err := cg.CPUUsage()
			if err != nil {
				continue
			}
			if time.Duration(u) >= limit {
				cancel()
				return
			}
		}
	}
}

func defaultProcesses(role Role) int64 {
	if role == RoleCompile {
		return 64
	}
	return 16
}
