package attempt

const (
	kindBatch       = "batch"
	kindInteractive = "interactive"

	checkerExact   = "exact"
	checkerToken   = "token"
	checkerTestlib = "testlib"

	verdictAccepted         = "accepted"
	verdictWrongAnswer      = "wrong_answer"
	verdictTLE              = "time_limit_exceeded"
	verdictMLE              = "memory_limit_exceeded"
	verdictRE               = "runtime_error"
	verdictCompilationError = "compilation_error"
	verdictSystemError      = "system_error"

	faultUnknownProfile     = "unknown_profile"
	faultLanguageMismatch   = "language_mismatch"
	faultMissingFileName    = "missing_file_name"
	faultHashMismatch       = "hash_mismatch"
	faultIllegalPath        = "illegal_path"
	faultFileTooLarge       = "file_too_large"
	faultSizeMismatch       = "size_mismatch"
	faultDownloadFailed     = "download_failed"
	faultCompileRunner      = "compile_runner"
	faultCheckerCompile     = "checker_compile"
	faultInteractorCompile  = "interactor_compile"
	faultCheckerCrash       = "checker_crash"
	faultInteractorCrash    = "interactor_crash"
	faultInvalidConfig      = "invalid_config"
	faultSandboxUnavailable = "sandbox_unavailable"
	faultSandboxExec        = "sandbox_exec"
	faultMissingFile        = "missing_file"
	faultInternal           = "internal"

	maxFileBytes = 64 << 20
)
