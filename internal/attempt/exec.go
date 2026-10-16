package attempt

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jellyfish-p/ephyra-judge/internal/sandbox"
	judgev1 "github.com/jellyfish-p/ephyra/protocol/judge/v1"
)

const maxCompileCache = 256

func (r *Runner) compileCached(ctx context.Context, sess sandbox.Session, role sandbox.Role, profileID string, args []string, files map[string][]byte, outName string) (bin []byte, compilerFailed bool, faultKind string) {
	cachePath := filepath.Join(r.workDir, "compile", compileCacheKey(role, profileID, outName, args, files))
	if cached, err := os.ReadFile(cachePath); err == nil && len(cached) > 0 {
		return cached, false, ""
	}
	res := sess.Exec(ctx, sandbox.Spec{
		Role:    sandbox.RoleCompile,
		Args:    args,
		Env:     sandbox.DefaultEnv(),
		Files:   files,
		Limits:  compileLimits(),
		Collect: []string{outName},
	})
	if res.RunnerFailed() || res.Status != sandbox.StatusOK {
		return nil, false, faultCompileRunner
	}
	if res.ExitCode != 0 {
		return nil, true, ""
	}
	bin = res.Files[outName]
	if len(bin) == 0 {
		return nil, true, ""
	}
	if err := atomicWriteFile(cachePath, bin, 0o644); err == nil {
		pruneCompileCache(filepath.Dir(cachePath), maxCompileCache)
	}
	return bin, false, ""
}

func compileCacheKey(role sandbox.Role, profileID, outName string, args []string, files map[string][]byte) string {
	h := sha256.New()
	write := func(s string) {
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(s)))
		h.Write(n[:])
		_, _ = io.WriteString(h, s)
	}
	write(string(role))
	write(profileID)
	write(outName)
	write(strconv.Itoa(len(args)))
	for _, a := range args {
		write(a)
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	write(strconv.Itoa(len(names)))
	for _, name := range names {
		write(name)
		sum := sha256.Sum256(files[name])
		h.Write(sum[:])
	}
	return hex.EncodeToString(h.Sum(nil))
}

func pruneCompileCache(dir string, max int) {
	if max <= 0 {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) <= max {
		return
	}
	type item struct {
		name string
		mod  time.Time
	}
	items := make([]item, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		items = append(items, item{name: e.Name(), mod: info.ModTime()})
	}
	if len(items) <= max {
		return
	}
	sort.Slice(items, func(i, j int) bool { return items[i].mod.Before(items[j].mod) })
	drop := len(items) - max*3/4
	for i := 0; i < drop; i++ {
		_ = os.Remove(filepath.Join(dir, items[i].name))
	}
}

func compileLimits() sandbox.Limits {
	return sandbox.Limits{
		CPU:         30 * time.Second,
		Wall:        45 * time.Second,
		MemoryBytes: 512 << 20,
		OutputBytes: 64 << 20,
		Processes:   64,
	}
}

func runCase(ctx context.Context, sess sandbox.Session, d *judgev1.DispatchAttempt, cfg normalizedConfig, prof profile, cs normalizedCase, data map[string][]byte, userBin, checkerBin, interBin []byte) (*judgev1.CaseResult, string) {
	in, ok := data[baseFileName(cs.InputName)]
	if !ok {
		return nil, faultMissingFile
	}
	timeLim, memLim := caseLimits(d, cfg, cs)
	userLim := sandbox.Limits{
		CPU:         time.Duration(timeLim) * time.Millisecond,
		Wall:        time.Duration(timeLim)*time.Millisecond*2 + 200*time.Millisecond,
		MemoryBytes: memLim,
		OutputBytes: 64 << 20,
		Processes:   16,
	}
	auxLim := sandbox.Limits{
		CPU:         userLim.CPU * 2,
		Wall:        userLim.Wall * 2,
		MemoryBytes: memLim,
		OutputBytes: 16 << 20,
		Processes:   32,
	}
	if cfg.Kind == kindInteractive {
		return runInteractive(ctx, sess, prof, userBin, interBin, in, userLim, auxLim, cs.ID)
	}
	var ans []byte
	if cs.OutputName != "" {
		a, ok := data[baseFileName(cs.OutputName)]
		if !ok && (cfg.Checker.Kind == checkerExact || cfg.Checker.Kind == checkerToken) {
			return nil, faultMissingFile
		}
		if ok {
			ans = a
		}
	}
	return runBatch(ctx, sess, prof, cfg, userBin, checkerBin, in, ans, userLim, auxLim, cs.ID)
}

func caseLimits(d *judgev1.DispatchAttempt, cfg normalizedConfig, cs normalizedCase) (timeMS, mem int64) {
	timeMS = cfg.TimeLimitMS
	mem = cfg.MemoryLimitBytes
	if d.GetTimeLimitMs() > 0 {
		timeMS = d.GetTimeLimitMs()
	}
	if d.GetMemoryLimitBytes() > 0 {
		mem = d.GetMemoryLimitBytes()
	}
	if cs.TimeLimitMS != nil {
		timeMS = *cs.TimeLimitMS
	}
	if cs.MemoryLimitBytes != nil {
		mem = *cs.MemoryLimitBytes
	}
	return timeMS, mem
}

func runBatch(ctx context.Context, sess sandbox.Session, prof profile, cfg normalizedConfig, userBin, checkerBin, input, answer []byte, userLim, checkLim sandbox.Limits, caseID string) (*judgev1.CaseResult, string) {
	userRes := sess.Exec(ctx, sandbox.Spec{
		Role:   sandbox.RoleUser,
		Args:   prof.runArgs,
		Env:    sandbox.DefaultEnv(),
		Files:  map[string][]byte{"program": userBin},
		Stdin:  input,
		Limits: userLim,
	})
	cr := caseFromUser(caseID, userRes)
	v, kind := userVerdict(userRes)
	if kind != "" {
		return cr, kind
	}
	if v != verdictAccepted {
		cr.Verdict = v
		return cr, ""
	}
	switch cfg.Checker.Kind {
	case checkerExact:
		if exactEqual(userRes.Stdout, answer) {
			cr.Verdict = verdictAccepted
		} else {
			cr.Verdict = verdictWrongAnswer
		}
	case checkerToken:
		if tokenEqual(userRes.Stdout, answer) {
			cr.Verdict = verdictAccepted
		} else {
			cr.Verdict = verdictWrongAnswer
		}
	case checkerTestlib:
		if len(checkerBin) == 0 {
			return cr, faultCheckerCompile
		}
		checkRes := sess.Exec(ctx, sandbox.Spec{
			Role: sandbox.RoleChecker,
			Args: []string{"./checker", "in", "user", "ans"},
			Env:  sandbox.DefaultEnv(),
			Files: map[string][]byte{
				"checker": checkerBin,
				"in":      input,
				"user":    userRes.Stdout,
				"ans":     answer,
			},
			Limits: checkLim,
		})
		cv, ck := checkerVerdict(checkRes)
		if ck != "" {
			return cr, ck
		}
		cr.Verdict = cv
	default:
		return cr, faultInvalidConfig
	}
	return cr, ""
}

func runInteractive(ctx context.Context, sess sandbox.Session, prof profile, userBin, interBin, input []byte, userLim, interLim sandbox.Limits, caseID string) (*judgev1.CaseResult, string) {
	userRes, interRes := sess.ExecPair(ctx, sandbox.Spec{
		Role:   sandbox.RoleUser,
		Args:   prof.runArgs,
		Env:    sandbox.DefaultEnv(),
		Files:  map[string][]byte{"program": userBin},
		Limits: userLim,
	}, sandbox.Spec{
		Role:   sandbox.RoleInteractor,
		Args:   []string{"./interactor", "in", "out"},
		Env:    sandbox.DefaultEnv(),
		Files:  map[string][]byte{"interactor": interBin, "in": input},
		Limits: interLim,
	})
	cr := caseFromUser(caseID, userRes)
	v, kind := interactorVerdict(userRes, interRes)
	if kind != "" {
		return cr, kind
	}
	cr.Verdict = v
	return cr, ""
}

func caseFromUser(caseID string, userRes sandbox.Result) *judgev1.CaseResult {
	return &judgev1.CaseResult{
		CaseId:      caseID,
		TimeMs:      userRes.Time.Milliseconds(),
		WallTimeMs:  userRes.Wall.Milliseconds(),
		MemoryBytes: userRes.MemoryBytes,
		ExitCode:    int32(userRes.ExitCode),
	}
}

func exactEqual(user, answer []byte) bool {
	return bytes.Equal(crlfToLF(user), crlfToLF(answer))
}

func crlfToLF(b []byte) []byte {
	return bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))
}

func tokenEqual(user, answer []byte) bool {
	return fieldsEqual(strings.Fields(string(user)), strings.Fields(string(answer)))
}

func fieldsEqual(a, b []string) bool {
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

func userVerdict(res sandbox.Result) (verdict string, systemKind string) {
	switch res.Status {
	case sandbox.StatusTimeLimit:
		return verdictTLE, ""
	case sandbox.StatusMemoryLimit:
		return verdictMLE, ""
	case sandbox.StatusOutputLimit, sandbox.StatusRuntime:
		return verdictRE, ""
	case sandbox.StatusRunnerError:
		return verdictSystemError, faultSandboxExec
	case sandbox.StatusOK:
		if res.ExitCode != 0 {
			return verdictRE, ""
		}
		// Process ended 0. That is not accepted until a checker says so.
		return verdictAccepted, ""
	default:
		return verdictSystemError, faultSandboxExec
	}
}

func checkerVerdict(res sandbox.Result) (verdict string, systemKind string) {
	if res.RunnerFailed() || res.Status != sandbox.StatusOK {
		return verdictSystemError, faultCheckerCrash
	}
	switch res.ExitCode {
	case 0:
		return verdictAccepted, ""
	case 1, 2, 4, 8:
		return verdictWrongAnswer, ""
	default:
		return verdictSystemError, faultCheckerCrash
	}
}

func interactorVerdict(user, inter sandbox.Result) (verdict string, systemKind string) {
	if user.RunnerFailed() {
		return verdictSystemError, faultSandboxExec
	}
	switch user.Status {
	case sandbox.StatusTimeLimit:
		return verdictTLE, ""
	case sandbox.StatusMemoryLimit:
		return verdictMLE, ""
	case sandbox.StatusOutputLimit, sandbox.StatusRuntime:
		return verdictRE, ""
	}
	if user.Status == sandbox.StatusOK && user.ExitCode != 0 {
		return verdictRE, ""
	}
	if user.Status != sandbox.StatusOK {
		return verdictSystemError, faultSandboxExec
	}
	if inter.RunnerFailed() || inter.Status != sandbox.StatusOK {
		return verdictSystemError, faultInteractorCrash
	}
	switch inter.ExitCode {
	case 0:
		return verdictAccepted, ""
	case 1, 2:
		return verdictWrongAnswer, ""
	default:
		return verdictSystemError, faultInteractorCrash
	}
}
