//go:build linux

package sandbox

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"sync"
	"syscall"
	"time"

	"github.com/criyle/go-sandbox/container"
	"github.com/criyle/go-sandbox/pkg/cgroup"
	"github.com/criyle/go-sandbox/pkg/mount"
	"github.com/criyle/go-sandbox/pkg/rlimit"
	"github.com/criyle/go-sandbox/pkg/seccomp"
	"github.com/criyle/go-sandbox/runner"
	"golang.org/x/sys/unix"
)

func init() {
	if err := container.Init(); err != nil {
		fmt.Fprintf(os.Stderr, "container init: %v\n", err)
		os.Exit(1)
	}
}

func DefaultHost() Host {
	return &linuxHost{
		seccomp: buildSeccomp(),
		cgRoot:  tryCgroupRoot(),
	}
}

type linuxHost struct {
	seccomp seccomp.Filter
	cgRoot  cgroup.Cgroup
}

func (h *linuxHost) NewSession(ctx context.Context) (Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	env, err := newEnv(h.seccomp)
	if err != nil {
		return nil, err
	}
	return &linuxSession{host: h, env: env}, nil
}

type linuxSession struct {
	host *linuxHost
	env  container.Environment
}

func newEnv(filter seccomp.Filter) (container.Environment, error) {
	mt := mount.NewDefaultBuilder().
		WithProc().
		WithBind("/etc", "etc", true).
		WithBind("/dev/null", "dev/null", false).
		WithBind("/dev/urandom", "dev/urandom", true).
		WithTmpfs("w", "").
		WithTmpfs("tmp", "").
		FilterNotExist().Mounts
	b := &container.Builder{
		WorkDir: "/w",
		Mounts:  mt,
		Seccomp: filter,
	}
	return b.Build()
}

func (s *linuxSession) Close() error {
	if s.env == nil {
		return nil
	}
	return s.env.Destroy()
}

func (s *linuxSession) Exec(ctx context.Context, spec Spec) Result {
	if err := s.env.Reset(); err != nil {
		return Result{Status: StatusRunnerError, Error: err.Error()}
	}
	return execEnv(ctx, s.host, s.env, spec, nil, nil)
}

func (s *linuxSession) ExecPair(ctx context.Context, user, peer Spec) (Result, Result) {
	peerEnv, err := newEnv(s.host.seccomp)
	if err != nil {
		e := Result{Status: StatusRunnerError, Error: err.Error()}
		return e, e
	}
	defer peerEnv.Destroy()
	if err := s.env.Reset(); err != nil {
		e := Result{Status: StatusRunnerError, Error: err.Error()}
		return e, e
	}

	uIn, pOut, err := os.Pipe()
	if err != nil {
		e := Result{Status: StatusRunnerError, Error: err.Error()}
		return e, e
	}
	pIn, uOut, err := os.Pipe()
	if err != nil {
		uIn.Close()
		pOut.Close()
		e := Result{Status: StatusRunnerError, Error: err.Error()}
		return e, e
	}

	userStarted := make(chan struct{})
	peerStarted := make(chan struct{})
	type pair struct{ res Result }
	uch := make(chan pair, 1)
	pch := make(chan pair, 1)
	go func() {
		uch <- pair{execEnv(ctx, s.host, s.env, user, []*os.File{uIn, uOut}, func() { close(userStarted) })}
	}()
	go func() {
		pch <- pair{execEnv(ctx, s.host, peerEnv, peer, []*os.File{pIn, pOut}, func() { close(peerStarted) })}
	}()
	<-userStarted
	<-peerStarted
	uIn.Close()
	uOut.Close()
	pIn.Close()
	pOut.Close()
	ur := (<-uch).res
	pr := (<-pch).res
	return ur, pr
}

func execEnv(ctx context.Context, host *linuxHost, env container.Environment, spec Spec, stdio []*os.File, onStarted func()) Result {
	started := func() {}
	if onStarted != nil {
		var once sync.Once
		started = func() { once.Do(onStarted) }
	}
	defer started()

	if err := copyIn(env, spec.Files); err != nil {
		return Result{Status: StatusRunnerError, Error: err.Error()}
	}

	stderr, err := tempFile("err")
	if err != nil {
		return Result{Status: StatusRunnerError, Error: err.Error()}
	}
	defer os.Remove(stderr.Name())
	defer stderr.Close()

	var stdin, stdout *os.File
	if stdio == nil {
		stdin, err = tempFile("in")
		if err != nil {
			return Result{Status: StatusRunnerError, Error: err.Error()}
		}
		defer os.Remove(stdin.Name())
		defer stdin.Close()
		if _, err := stdin.Write(spec.Stdin); err != nil {
			return Result{Status: StatusRunnerError, Error: err.Error()}
		}
		if _, err := stdin.Seek(0, io.SeekStart); err != nil {
			return Result{Status: StatusRunnerError, Error: err.Error()}
		}
		stdout, err = tempFile("out")
		if err != nil {
			return Result{Status: StatusRunnerError, Error: err.Error()}
		}
		defer os.Remove(stdout.Name())
		defer stdout.Close()
	}

	inFile, outFile := stdin, stdout
	if stdio != nil {
		inFile, outFile = stdio[0], stdio[1]
	}
	files, closeFDs, err := dupStdio(inFile, outFile, stderr)
	if err != nil {
		return Result{Status: StatusRunnerError, Error: err.Error()}
	}
	defer closeFDs()
	started()

	wall := wallOrDefault(spec.Limits)
	runCtx, cancel := context.WithTimeout(ctx, wall)
	defer cancel()

	cpuSec := cpuSeconds(spec.Limits.CPU)
	mem := spec.Limits.MemoryBytes
	if mem <= 0 {
		mem = 256 << 20
	}
	outLim := spec.Limits.OutputBytes
	if outLim <= 0 {
		outLim = 64 << 20
	}

	rlimits := (&rlimit.RLimits{
		CPU:          cpuSec,
		CPUHard:      cpuSec + 1,
		AddressSpace: uint64(mem),
		FileSize:     uint64(outLim),
		OpenFile:     256,
		DisableCore:  true,
	}).PrepareRLimit()

	envv := spec.Env
	if len(envv) == 0 {
		envv = DefaultEnv()
	}

	var cg *cgHandle
	if host != nil {
		cg = host.prepareCgroup(spec)
	}
	var cgroupFD uintptr
	var syncFunc func(pid int) error
	if cg != nil {
		defer cg.closer()
		cgroupFD = cg.fd
		syncFunc = cg.sync
		go watchCPU(runCtx, cancel, cg.cg, spec.Limits.CPU)
	}

	param := container.ExecveParam{
		Args:     spec.Args,
		Env:      envv,
		Files:    files,
		RLimits:  rlimits,
		CgroupFD: cgroupFD,
		SyncFunc: syncFunc,
	}

	start := time.Now()
	rr := env.Execve(runCtx, param)
	wallUsed := time.Since(start)

	var outBytes []byte
	if stdout != nil {
		_, _ = stdout.Seek(0, io.SeekStart)
		outBytes, _ = io.ReadAll(io.LimitReader(stdout, outLim+1))
	}
	_, _ = stderr.Seek(0, io.SeekStart)
	errBytes, _ := io.ReadAll(io.LimitReader(stderr, 1<<20))

	res := Result{
		ExitCode:    rr.ExitStatus,
		Time:        rr.Time,
		Wall:        wallUsed,
		MemoryBytes: int64(rr.Memory),
		Stdout:      outBytes,
		Stderr:      errBytes,
		Error:       rr.Error,
	}
	if int64(len(outBytes)) > outLim {
		res.Status = StatusOutputLimit
		res.Stdout = outBytes[:outLim]
		return finishCollect(env, spec, res)
	}
	res.Status = mapRunnerStatus(rr, runCtx)
	return finishCollect(env, spec, res)
}

func finishCollect(env container.Environment, spec Spec, res Result) Result {
	if len(spec.Collect) == 0 || res.Status == StatusRunnerError {
		return res
	}
	res.Files = make(map[string][]byte, len(spec.Collect))
	for _, name := range spec.Collect {
		data, err := copyOut(env, name)
		if err != nil {
			continue
		}
		res.Files[name] = data
	}
	return res
}

func copyIn(env container.Environment, files map[string][]byte) error {
	for name, data := range files {
		base := path.Base(name)
		if base == "." || base == "/" || base == ".." {
			return fmt.Errorf("illegal file name %q", name)
		}
		opened, err := env.Open([]container.OpenCmd{{
			Path:     path.Join("/w", base),
			Flag:     os.O_CREATE | os.O_WRONLY | os.O_TRUNC,
			Perm:     0755,
			MkdirAll: true,
		}})
		if err != nil {
			return err
		}
		if len(opened) == 0 {
			return fmt.Errorf("open %s: empty result", base)
		}
		if opened[0].Err != nil {
			return opened[0].Err
		}
		f := opened[0].File
		_, err = f.Write(data)
		cerr := f.Close()
		if err != nil {
			return err
		}
		if cerr != nil {
			return cerr
		}
	}
	return nil
}

func copyOut(env container.Environment, name string) ([]byte, error) {
	base := path.Base(name)
	opened, err := env.Open([]container.OpenCmd{{
		Path: path.Join("/w", base),
		Flag: os.O_RDONLY,
	}})
	if err != nil {
		return nil, err
	}
	if len(opened) == 0 {
		return nil, fmt.Errorf("copy out %s: empty result", base)
	}
	if opened[0].Err != nil {
		return nil, opened[0].Err
	}
	f := opened[0].File
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, 64<<20))
}

func mapRunnerStatus(rr runner.Result, runCtx context.Context) Status {
	switch rr.Status {
	case runner.StatusNormal, runner.StatusNonzeroExitStatus:
		return StatusOK
	case runner.StatusTimeLimitExceeded:
		return StatusTimeLimit
	case runner.StatusMemoryLimitExceeded:
		return StatusMemoryLimit
	case runner.StatusOutputLimitExceeded:
		return StatusOutputLimit
	case runner.StatusSignalled:
		switch syscall.Signal(rr.ExitStatus) {
		case syscall.SIGXCPU, syscall.SIGKILL:
			return StatusTimeLimit
		case syscall.SIGXFSZ:
			return StatusOutputLimit
		default:
			return StatusRuntime
		}
	case runner.StatusDisallowedSyscall:
		return StatusRuntime
	default:
		if runCtx.Err() != nil {
			return StatusTimeLimit
		}
		return StatusRunnerError
	}
}

func dupStdio(in, out, errFile *os.File) ([]uintptr, func(), error) {
	inFD, err := unix.Dup(int(in.Fd()))
	if err != nil {
		return nil, nil, err
	}
	outFD, err := unix.Dup(int(out.Fd()))
	if err != nil {
		unix.Close(inFD)
		return nil, nil, err
	}
	errFD, err := unix.Dup(int(errFile.Fd()))
	if err != nil {
		unix.Close(inFD)
		unix.Close(outFD)
		return nil, nil, err
	}
	return []uintptr{uintptr(inFD), uintptr(outFD), uintptr(errFD)}, func() {
		unix.Close(inFD)
		unix.Close(outFD)
		unix.Close(errFD)
	}, nil
}

func tempFile(kind string) (*os.File, error) {
	return os.CreateTemp("", "ephyra-"+kind+"-*")
}

func cpuSeconds(d time.Duration) uint64 {
	if d <= 0 {
		return 1
	}
	sec := uint64((d + time.Second - 1) / time.Second)
	if sec == 0 {
		return 1
	}
	return sec
}

func wallOrDefault(l Limits) time.Duration {
	if l.Wall > 0 {
		return l.Wall
	}
	if l.CPU <= 0 {
		return 2 * time.Second
	}
	return l.CPU*3/2 + 100*time.Millisecond
}

var (
	_ Host    = (*linuxHost)(nil)
	_ Session = (*linuxSession)(nil)
)
