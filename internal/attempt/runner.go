package attempt

import (
	"context"
	"net/http"
	"path"

	"github.com/jellyfish-p/ephyra-judge/internal/sandbox"
	judgev1 "github.com/jellyfish-p/ephyra/protocol/judge/v1"
)

// Runner is the judging brain. Callers only see Run.
type Runner struct {
	Sandbox sandbox.Host
	WorkDir string
	HTTP    *http.Client // nil → http.DefaultClient
}

func New(host sandbox.Host, workDir string) *Runner {
	return &Runner{Sandbox: host, WorkDir: workDir}
}

// Run judges one dispatch. A cancelled or invalidated attempt returns nil
// so the caller must not Send a result.
func (r *Runner) Run(ctx context.Context, d *judgev1.DispatchAttempt) *judgev1.AttemptResult {
	if ctx.Err() != nil || d == nil {
		return nil
	}

	cfg, err := parseConfig(d.GetNormalizedConfigJson())
	if err != nil {
		return systemFault(d, faultInvalidConfig)
	}
	prof, ok := lookupProfile(d.GetExecutionProfileId())
	if !ok {
		return systemFault(d, faultUnknownProfile)
	}
	if prof.languageID != d.GetLanguageId() {
		return systemFault(d, faultLanguageMismatch)
	}

	source, kind := r.fetch(ctx, d.GetSource())
	if ctx.Err() != nil {
		return nil
	}
	if kind != "" {
		return systemFault(d, kind)
	}
	data, kind := r.fetchData(ctx, d.GetDataFiles())
	if ctx.Err() != nil {
		return nil
	}
	if kind != "" {
		return systemFault(d, kind)
	}

	if r.Sandbox == nil {
		return systemFault(d, faultSandboxUnavailable)
	}
	sess, err := r.Sandbox.NewSession(ctx)
	if ctx.Err() != nil {
		return nil
	}
	if err != nil {
		return systemFault(d, faultSandboxUnavailable)
	}
	defer sess.Close()

	userBin, compilerFailed, kind := r.compileCached(ctx, sess, prof.id, shaHex(source), prof.compileArgs, map[string][]byte{
		prof.sourceName: source,
	}, "program")
	if ctx.Err() != nil {
		return nil
	}
	if kind != "" {
		return systemFault(d, kind)
	}
	if compilerFailed {
		return compilerFailedResult(d, cfg)
	}

	var checkerBin []byte
	if cfg.Checker.Kind == checkerTestlib {
		src, ok := data[baseFileName(cfg.Checker.SourceName)]
		if !ok {
			return systemFault(d, faultMissingFile)
		}
		bin, failed, kind := r.compileCached(ctx, sess, profileCPP23, shaHex(src), toolCompileArgs(cfg.Checker.SourceName, "checker"), map[string][]byte{
			path.Base(cfg.Checker.SourceName): src,
		}, "checker")
		if ctx.Err() != nil {
			return nil
		}
		if kind != "" || failed {
			return systemFault(d, faultCheckerCompile)
		}
		checkerBin = bin
	}

	var interBin []byte
	if cfg.Kind == kindInteractive {
		if cfg.Interactor == nil {
			return systemFault(d, faultInvalidConfig)
		}
		src, ok := data[baseFileName(cfg.Interactor.SourceName)]
		if !ok {
			return systemFault(d, faultMissingFile)
		}
		bin, failed, kind := r.compileCached(ctx, sess, profileCPP23, shaHex(src), toolCompileArgs(cfg.Interactor.SourceName, "interactor"), map[string][]byte{
			path.Base(cfg.Interactor.SourceName): src,
		}, "interactor")
		if ctx.Err() != nil {
			return nil
		}
		if kind != "" || failed {
			return systemFault(d, faultInteractorCompile)
		}
		interBin = bin
	}

	cases := make([]*judgev1.CaseResult, 0, len(cfg.Cases))
	outcomes := make([]caseOutcome, 0, len(cfg.Cases))
	var maxTime, maxMem int64
	for _, cs := range cfg.Cases {
		if ctx.Err() != nil {
			return nil
		}
		cr, kind := runCase(ctx, sess, d, cfg, prof, cs, data, userBin, checkerBin, interBin)
		if kind != "" {
			return systemFault(d, kind)
		}
		cases = append(cases, cr)
		outcomes = append(outcomes, caseOutcome{id: cs.ID, verdict: cr.GetVerdict()})
		if cr.GetTimeMs() > maxTime {
			maxTime = cr.GetTimeMs()
		}
		if cr.GetMemoryBytes() > maxMem {
			maxMem = cr.GetMemoryBytes()
		}
	}

	computed, err := computeSubmission(cfg, outcomes, false)
	if err != nil {
		return systemFault(d, faultInternal)
	}
	res := identity(d)
	res.Verdict = computed.verdict
	res.Score = computed.total.String()
	res.Cases = cases
	res.Subtasks = toProtoSubtasks(computed)
	res.TimeMs = maxTime
	res.MemoryBytes = maxMem
	return res
}

func identity(d *judgev1.DispatchAttempt) *judgev1.AttemptResult {
	return &judgev1.AttemptResult{
		AttemptId:           d.GetAttemptId(),
		TaskId:              d.GetTaskId(),
		Generation:          d.GetGeneration(),
		JudgeDataRevisionId: d.GetJudgeDataRevisionId(),
		ExecutionProfileId:  d.GetExecutionProfileId(),
		Score:               zeroScore.String(),
	}
}

func systemFault(d *judgev1.DispatchAttempt, kind string) *judgev1.AttemptResult {
	res := identity(d)
	res.Verdict = verdictSystemError
	res.SystemFault = true
	res.SystemFaultKind = kind
	return res
}

func compilerFailedResult(d *judgev1.DispatchAttempt, cfg normalizedConfig) *judgev1.AttemptResult {
	computed, err := computeSubmission(cfg, nil, true)
	if err != nil {
		return systemFault(d, faultInternal)
	}
	res := identity(d)
	res.Verdict = verdictCompilationError
	res.Score = computed.total.String()
	res.CompilerFailed = true
	res.Subtasks = toProtoSubtasks(computed)
	return res
}

func toProtoSubtasks(c computedScore) []*judgev1.SubtaskResult {
	out := make([]*judgev1.SubtaskResult, 0, len(c.subtasks))
	for _, st := range c.subtasks {
		out = append(out, &judgev1.SubtaskResult{
			SubtaskId: st.id,
			Score:     st.score.String(),
			Verdict:   st.verdict,
		})
	}
	return out
}
