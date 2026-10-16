package attempt

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jellyfish-p/ephyra-judge/internal/sandbox"
	judgev1 "github.com/jellyfish-p/ephyra/protocol/judge/v1"
)

var dummyBin = []byte{0x7f, 'E', 'L', 'F'}

const (
	profileCPP23ID = "cpp23.gcc.linux-amd64.v0"
	srcMain        = "int main(){}\n"
)

func TestRunnerRun(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		ctx     func() context.Context
		setup   func(*testing.T) (*sandbox.Fake, *judgev1.DispatchAttempt)
		wantNil bool
		check   func(*testing.T, *judgev1.AttemptResult, *sandbox.Fake)
	}{
		{
			name:  "token AC",
			setup: tokenACSetup,
			check: func(t *testing.T, got *judgev1.AttemptResult, fake *sandbox.Fake) {
				t.Helper()
				assertNormal(t, got)
				if got.Verdict != "accepted" || got.Score != "100.00" || got.CompilerFailed {
					t.Fatalf("verdict=%s score=%s ce=%v", got.Verdict, got.Score, got.CompilerFailed)
				}
				if len(got.Subtasks) != 1 || got.Subtasks[0].Score != "100.00" || got.Subtasks[0].Verdict != "accepted" {
					t.Fatalf("subtasks=%v", got.Subtasks)
				}
				if len(got.Cases) != 1 || got.Cases[0].Verdict != "accepted" {
					t.Fatalf("cases=%v", got.Cases)
				}
				assertCompileUsesLocalProfile(t, fake)
			},
		},
		{
			name:  "exact whitespace is WA",
			setup: exactWhitespaceSetup,
			check: func(t *testing.T, got *judgev1.AttemptResult, _ *sandbox.Fake) {
				t.Helper()
				assertNormal(t, got)
				if got.Verdict != "wrong_answer" || got.Score != "0.00" {
					t.Fatalf("exact must treat extra spaces as WA, got %s %s", got.Verdict, got.Score)
				}
			},
		},
		{
			name:  "token whitespace is AC",
			setup: tokenWhitespaceSetup,
			check: func(t *testing.T, got *judgev1.AttemptResult, _ *sandbox.Fake) {
				t.Helper()
				assertNormal(t, got)
				if got.Verdict != "accepted" || got.Score != "100.00" {
					t.Fatalf("token must ignore extra spaces, got %s %s", got.Verdict, got.Score)
				}
			},
		},
		{
			name:  "compilation error emits CE subtasks at 0.00",
			setup: compileErrorSetup,
			check: func(t *testing.T, got *judgev1.AttemptResult, _ *sandbox.Fake) {
				t.Helper()
				assertNormal(t, got)
				if !got.CompilerFailed || got.Verdict != "compilation_error" || got.Score != "0.00" {
					t.Fatalf("ce=%v verdict=%s score=%s", got.CompilerFailed, got.Verdict, got.Score)
				}
				if len(got.Subtasks) != 2 {
					t.Fatalf("want 2 subtask rows, got %d", len(got.Subtasks))
				}
				for _, st := range got.Subtasks {
					if st.Score != "0.00" || st.Verdict != "compilation_error" {
						t.Fatalf("subtask %s: %s %s", st.SubtaskId, st.Score, st.Verdict)
					}
				}
			},
		},
		{
			name:  "all-or-nothing subtasks 40/60",
			setup: partialSubtaskSetup,
			check: func(t *testing.T, got *judgev1.AttemptResult, _ *sandbox.Fake) {
				t.Helper()
				assertNormal(t, got)
				// Worked example from server ComputeSubmissionScore: 1 AC, 2 WA, 3 AC
				// s1={1,2} 40.00 → 0.00; s2={3} 60.00 → 60.00; total 60.00 WA.
				if got.Verdict != "wrong_answer" || got.Score != "60.00" {
					t.Fatalf("got %s %s", got.Verdict, got.Score)
				}
				if len(got.Subtasks) != 2 {
					t.Fatalf("subtasks=%v", got.Subtasks)
				}
				if got.Subtasks[0].SubtaskId != "s1" || got.Subtasks[0].Score != "0.00" || got.Subtasks[0].Verdict != "wrong_answer" {
					t.Fatalf("s1=%+v", got.Subtasks[0])
				}
				if got.Subtasks[1].SubtaskId != "s2" || got.Subtasks[1].Score != "60.00" || got.Subtasks[1].Verdict != "accepted" {
					t.Fatalf("s2=%+v", got.Subtasks[1])
				}
				if got.TimeMs != 30 || got.MemoryBytes != 4000 {
					t.Fatalf("max time/mem = %d / %d", got.TimeMs, got.MemoryBytes)
				}
			},
		},
		{
			name:  "checker crash is system_fault not WA",
			setup: checkerCrashSetup,
			check: func(t *testing.T, got *judgev1.AttemptResult, _ *sandbox.Fake) {
				t.Helper()
				if got.Verdict == "wrong_answer" {
					t.Fatal("checker exit 3 must not be billed as wrong_answer")
				}
				if !got.SystemFault || got.SystemFaultKind != "checker_crash" {
					t.Fatalf("fault=%v kind=%q verdict=%s", got.SystemFault, got.SystemFaultKind, got.Verdict)
				}
				if got.SystemFaultKind == "" {
					t.Fatal("system_fault requires a kind")
				}
			},
		},
		{
			name:  "hash mismatch",
			setup: hashMismatchSetup,
			check: func(t *testing.T, got *judgev1.AttemptResult, _ *sandbox.Fake) {
				t.Helper()
				if !got.SystemFault || got.SystemFaultKind != "hash_mismatch" {
					t.Fatalf("fault=%v kind=%q", got.SystemFault, got.SystemFaultKind)
				}
			},
		},
		{
			name:  "missing FileRef.name",
			setup: missingNameSetup,
			check: func(t *testing.T, got *judgev1.AttemptResult, _ *sandbox.Fake) {
				t.Helper()
				if !got.SystemFault || got.SystemFaultKind != "missing_file_name" {
					t.Fatalf("fault=%v kind=%q", got.SystemFault, got.SystemFaultKind)
				}
			},
		},
		{
			name: "ctx cancel",
			ctx: func() context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			},
			setup: func(t *testing.T) (*sandbox.Fake, *judgev1.DispatchAttempt) {
				return tokenACSetup(t)
			},
			wantNil: true,
		},
		{
			name:  "interactive interactor WA",
			setup: interactiveWASetup,
			check: func(t *testing.T, got *judgev1.AttemptResult, _ *sandbox.Fake) {
				t.Helper()
				assertNormal(t, got)
				if got.Verdict != "wrong_answer" || got.Score != "0.00" {
					t.Fatalf("got %s %s", got.Verdict, got.Score)
				}
				if len(got.Cases) != 1 || got.Cases[0].Verdict != "wrong_answer" {
					t.Fatalf("cases=%v", got.Cases)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake, d := tc.setup(t)
			ctx := context.Background()
			if tc.ctx != nil {
				ctx = tc.ctx()
			}
			r := New(fake, t.TempDir())
			got := r.Run(ctx, d)
			if tc.wantNil {
				if got != nil {
					t.Fatalf("want nil result, got %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatal("unexpected nil result")
			}
			if got.AttemptId != d.AttemptId || got.Generation != d.Generation {
				t.Fatalf("identity mismatch: %+v", got)
			}
			tc.check(t, got, fake)
		})
	}
}

func tokenACSetup(t *testing.T) (*sandbox.Fake, *judgev1.DispatchAttempt) {
	t.Helper()
	files := map[string][]byte{
		"main.cpp": []byte(srcMain),
		"1.in":     []byte("1\n"),
		"1.out":    []byte("hello world\n"),
	}
	fake := &sandbox.Fake{ExecFn: func(ctx context.Context, spec sandbox.Spec) sandbox.Result {
		if spec.Role == sandbox.RoleCompile {
			return compileOK(spec)
		}
		if spec.Role == sandbox.RoleUser {
			// Extra spaces: token must still accept. Exit 0 is not itself AC.
			return sandbox.Result{
				Status:      sandbox.StatusOK,
				Stdout:      []byte("hello   world\n"),
				Time:        10 * time.Millisecond,
				MemoryBytes: 1024,
			}
		}
		t.Errorf("unexpected role %s", spec.Role)
		return sandbox.Result{Status: sandbox.StatusRunnerError}
	}}
	return fake, dispatch(t, files, []string{"1.in", "1.out"}, oneCaseConfig("token"))
}

func exactWhitespaceSetup(t *testing.T) (*sandbox.Fake, *judgev1.DispatchAttempt) {
	t.Helper()
	return whitespaceSetup(t, "exact")
}

func tokenWhitespaceSetup(t *testing.T) (*sandbox.Fake, *judgev1.DispatchAttempt) {
	t.Helper()
	return whitespaceSetup(t, "token")
}

func whitespaceSetup(t *testing.T, kind string) (*sandbox.Fake, *judgev1.DispatchAttempt) {
	t.Helper()
	files := map[string][]byte{
		"main.cpp": []byte(srcMain),
		"1.in":     []byte("x\n"),
		"1.out":    []byte("a b\n"),
	}
	fake := &sandbox.Fake{ExecFn: func(ctx context.Context, spec sandbox.Spec) sandbox.Result {
		if spec.Role == sandbox.RoleCompile {
			return compileOK(spec)
		}
		return sandbox.Result{Status: sandbox.StatusOK, Stdout: []byte("a  b\n"), Time: time.Millisecond, MemoryBytes: 1}
	}}
	return fake, dispatch(t, files, []string{"1.in", "1.out"}, oneCaseConfig(kind))
}

func compileErrorSetup(t *testing.T) (*sandbox.Fake, *judgev1.DispatchAttempt) {
	t.Helper()
	files := map[string][]byte{
		"main.cpp": []byte(srcMain),
		"1.in":     []byte("1\n"),
		"1.out":    []byte("ok\n"),
		"2.in":     []byte("2\n"),
		"2.out":    []byte("ok\n"),
		"3.in":     []byte("3\n"),
		"3.out":    []byte("ok\n"),
	}
	fake := &sandbox.Fake{ExecFn: func(ctx context.Context, spec sandbox.Spec) sandbox.Result {
		if spec.Role == sandbox.RoleCompile {
			return sandbox.Result{Status: sandbox.StatusOK, ExitCode: 1, Stderr: []byte("error: boom")}
		}
		t.Errorf("user program must not run after compile nonzero")
		return sandbox.Result{Status: sandbox.StatusOK}
	}}
	return fake, dispatch(t, files, []string{"1.in", "1.out", "2.in", "2.out", "3.in", "3.out"}, threeCaseConfig())
}

func partialSubtaskSetup(t *testing.T) (*sandbox.Fake, *judgev1.DispatchAttempt) {
	t.Helper()
	files := map[string][]byte{
		"main.cpp": []byte(srcMain),
		"1.in":     []byte("1\n"),
		"1.out":    []byte("ok\n"),
		"2.in":     []byte("2\n"),
		"2.out":    []byte("ok\n"),
		"3.in":     []byte("3\n"),
		"3.out":    []byte("ok\n"),
	}
	fake := &sandbox.Fake{ExecFn: func(ctx context.Context, spec sandbox.Spec) sandbox.Result {
		if spec.Role == sandbox.RoleCompile {
			return compileOK(spec)
		}
		out := []byte("ok\n")
		var timeMS time.Duration
		var mem int64
		switch {
		case bytes.Equal(spec.Stdin, []byte("1\n")):
			timeMS, mem = 10*time.Millisecond, 1000
		case bytes.Equal(spec.Stdin, []byte("2\n")):
			out = []byte("no\n")
			timeMS, mem = 30*time.Millisecond, 500
		case bytes.Equal(spec.Stdin, []byte("3\n")):
			timeMS, mem = 20*time.Millisecond, 4000
		}
		return sandbox.Result{Status: sandbox.StatusOK, Stdout: out, Time: timeMS, MemoryBytes: mem}
	}}
	return fake, dispatch(t, files, []string{"1.in", "1.out", "2.in", "2.out", "3.in", "3.out"}, threeCaseConfig())
}

func checkerCrashSetup(t *testing.T) (*sandbox.Fake, *judgev1.DispatchAttempt) {
	t.Helper()
	files := map[string][]byte{
		"main.cpp": []byte(srcMain),
		"chk.cpp":  []byte("// checker\n"),
		"1.in":     []byte("1\n"),
		"1.out":    []byte("ok\n"),
	}
	fake := &sandbox.Fake{ExecFn: func(ctx context.Context, spec sandbox.Spec) sandbox.Result {
		switch spec.Role {
		case sandbox.RoleCompile:
			return compileOK(spec)
		case sandbox.RoleUser:
			return sandbox.Result{Status: sandbox.StatusOK, Stdout: []byte("ok\n")}
		case sandbox.RoleChecker:
			return sandbox.Result{Status: sandbox.StatusOK, ExitCode: 3}
		default:
			return sandbox.Result{Status: sandbox.StatusRunnerError}
		}
	}}
	cfg := `{
		"version": 1,
		"kind": "batch",
		"checker": {"kind": "testlib", "source_name": "chk.cpp"},
		"time_limit_ms": 1000,
		"memory_limit_bytes": 67108864,
		"subtasks": [{"id": "s1", "score": "100.00", "case_ids": ["1"]}],
		"cases": [{"id": "1", "input_name": "1.in", "output_name": "1.out"}]
	}`
	return fake, dispatch(t, files, []string{"chk.cpp", "1.in", "1.out"}, cfg)
}

func hashMismatchSetup(t *testing.T) (*sandbox.Fake, *judgev1.DispatchAttempt) {
	t.Helper()
	fake, d := tokenACSetup(t)
	d.DataFiles[0].Sha256Hex = strings.Repeat("ab", 32)
	return fake, d
}

func missingNameSetup(t *testing.T) (*sandbox.Fake, *judgev1.DispatchAttempt) {
	t.Helper()
	fake, d := tokenACSetup(t)
	d.DataFiles[0].Name = ""
	return fake, d
}

func interactiveWASetup(t *testing.T) (*sandbox.Fake, *judgev1.DispatchAttempt) {
	t.Helper()
	files := map[string][]byte{
		"main.cpp":       []byte(srcMain),
		"interactor.cpp": []byte("// interactor\n"),
		"1.in":           []byte("1\n"),
	}
	fake := &sandbox.Fake{
		ExecFn: func(ctx context.Context, spec sandbox.Spec) sandbox.Result {
			if spec.Role == sandbox.RoleCompile {
				return compileOK(spec)
			}
			t.Errorf("interactive must use ExecPair, got Exec role=%s", spec.Role)
			return sandbox.Result{Status: sandbox.StatusOK}
		},
		PairFn: func(ctx context.Context, user, peer sandbox.Spec) (sandbox.Result, sandbox.Result) {
			if user.Role != sandbox.RoleUser || peer.Role != sandbox.RoleInteractor {
				t.Errorf("pair roles user=%s peer=%s", user.Role, peer.Role)
			}
			return sandbox.Result{Status: sandbox.StatusOK, Time: 4 * time.Millisecond, MemoryBytes: 200},
				sandbox.Result{Status: sandbox.StatusOK, ExitCode: 1}
		},
	}
	cfg := `{
		"version": 1,
		"kind": "interactive",
		"checker": {"kind": "exact"},
		"interactor": {"source_name": "interactor.cpp"},
		"time_limit_ms": 1000,
		"memory_limit_bytes": 67108864,
		"subtasks": [{"id": "s1", "score": "100.00", "case_ids": ["1"]}],
		"cases": [{"id": "1", "input_name": "1.in"}]
	}`
	return fake, dispatch(t, files, []string{"interactor.cpp", "1.in"}, cfg)
}

func compileOK(spec sandbox.Spec) sandbox.Result {
	files := map[string][]byte{}
	for _, name := range spec.Collect {
		files[name] = dummyBin
	}
	if len(files) == 0 {
		files["program"] = dummyBin
	}
	return sandbox.Result{Status: sandbox.StatusOK, Files: files}
}

func dispatch(t *testing.T, files map[string][]byte, dataNames []string, cfg string) *judgev1.DispatchAttempt {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/")
		body, ok := files[key]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	var data []*judgev1.FileRef
	for _, name := range dataNames {
		role := "data"
		if strings.Contains(name, "chk") {
			role = "checker"
		}
		if strings.Contains(name, "interactor") {
			role = "interactor"
		}
		data = append(data, fileRef(name, role, files[name], srv.URL+"/"+name))
	}
	return &judgev1.DispatchAttempt{
		AttemptId:            "att-1",
		TaskId:               "task-1",
		Generation:           1,
		JudgeDataRevisionId:  "rev-1",
		ExecutionProfileId:   profileCPP23ID,
		LanguageId:           "cpp23",
		Source:               fileRef("main.cpp", "source", files["main.cpp"], srv.URL+"/main.cpp"),
		DataFiles:            data,
		NormalizedConfigJson: cfg,
		TimeLimitMs:          1000,
		MemoryLimitBytes:     64 << 20,
	}
}

func fileRef(name, role string, body []byte, url string) *judgev1.FileRef {
	sum := sha256.Sum256(body)
	return &judgev1.FileRef{
		ObjectId:    name,
		Sha256Hex:   hex.EncodeToString(sum[:]),
		SizeBytes:   int64(len(body)),
		DownloadUrl: url,
		Name:        name,
		Role:        role,
	}
}

func oneCaseConfig(checker string) string {
	return `{
		"version": 1,
		"kind": "batch",
		"checker": {"kind": "` + checker + `"},
		"time_limit_ms": 1000,
		"memory_limit_bytes": 67108864,
		"subtasks": [{"id": "s1", "score": "100.00", "case_ids": ["1"]}],
		"cases": [{"id": "1", "input_name": "1.in", "output_name": "1.out"}]
	}`
}

func threeCaseConfig() string {
	return `{
		"version": 1,
		"kind": "batch",
		"checker": {"kind": "token"},
		"time_limit_ms": 1000,
		"memory_limit_bytes": 67108864,
		"subtasks": [
			{"id": "s1", "score": "40.00", "case_ids": ["1", "2"]},
			{"id": "s2", "score": "60.00", "case_ids": ["3"]}
		],
		"cases": [
			{"id": "1", "input_name": "1.in", "output_name": "1.out"},
			{"id": "2", "input_name": "2.in", "output_name": "2.out"},
			{"id": "3", "input_name": "3.in", "output_name": "3.out"}
		]
	}`
}

func assertNormal(t *testing.T, got *judgev1.AttemptResult) {
	t.Helper()
	if got.SystemFault || got.SystemFaultKind != "" {
		t.Fatalf("unexpected system_fault kind=%q", got.SystemFaultKind)
	}
}

func assertCompileUsesLocalProfile(t *testing.T, fake *sandbox.Fake) {
	t.Helper()
	for _, spec := range fake.PopCalls() {
		if spec.Role != sandbox.RoleCompile {
			continue
		}
		joined := strings.Join(spec.Args, " ")
		if !strings.Contains(joined, "-std=c++23") || !strings.Contains(joined, "main.cpp") {
			t.Fatalf("compile argv must come from the local profile, got %v", spec.Args)
		}
		return
	}
	t.Fatal("expected a compile Exec")
}
