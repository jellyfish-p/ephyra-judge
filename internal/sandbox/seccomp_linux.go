//go:build linux

package sandbox

import (
	"log/slog"

	"github.com/criyle/go-sandbox/pkg/seccomp"
	"github.com/criyle/go-sandbox/pkg/seccomp/libseccomp"
)

func buildSeccomp() seccomp.Filter {
	extra := []string{
		"clone3", "rseq", "openat2", "faccessat2", "close_range",
		"futex_waitv", "statx", "faccessat", "newfstatat",
	}
	f, err := assembleSeccomp(append(append([]string{}, seccompAllow...), extra...))
	if err == nil {
		return f
	}
	f, err = assembleSeccomp(seccompAllow)
	if err != nil {
		slog.Warn("seccomp filter not installed", "err", err)
		return nil
	}
	return f
}

func assembleSeccomp(allow []string) (seccomp.Filter, error) {
	return (&libseccomp.Builder{
		Allow:   allow,
		Default: libseccomp.ActionKill,
	}).Build()
}

// seccompAllow is a compute/compile whitelist: no sockets, ptrace, mount,
// or module loading. The container also unshares the network namespace.
var seccompAllow = []string{
	"read", "write", "readv", "writev", "pread64", "pwrite64",
	"preadv", "pwritev", "preadv2", "pwritev2",
	"close", "lseek", "dup", "dup2", "dup3",
	"fstat", "stat", "lstat", "fstatat64", "stat64", "lstat64",
	"ioctl", "fcntl", "fcntl64", "fadvise64",
	"mmap", "mmap2", "mprotect", "munmap", "brk", "mremap", "msync", "mincore", "madvise",
	"rt_sigaction", "rt_sigprocmask", "rt_sigreturn", "rt_sigpending",
	"rt_sigtimedwait", "rt_sigsuspend", "sigaltstack", "sigreturn",
	"getcwd", "chdir", "fchdir", "umask",
	"exit", "exit_group",
	"gettimeofday", "clock_gettime", "clock_gettime64", "clock_nanosleep",
	"nanosleep", "times", "time",
	"getrlimit", "setrlimit", "prlimit64", "getrusage",
	"restart_syscall",
	"execve", "execveat",
	"open", "openat", "access", "faccessat", "readlink", "readlinkat",
	"getdents", "getdents64",
	"unlink", "unlinkat", "mkdir", "mkdirat", "rmdir",
	"chmod", "fchmod", "fchmodat",
	"rename", "renameat", "renameat2",
	"link", "linkat", "symlink", "symlinkat",
	"pipe", "pipe2", "eventfd2",
	"clone", "fork", "vfork", "wait4", "waitid", "waitpid",
	"futex", "set_tid_address", "set_robust_list",
	"getpid", "gettid", "getppid", "getuid", "geteuid", "getgid", "getegid", "getgroups",
	"arch_prctl", "set_thread_area",
	"sched_yield", "sched_getaffinity", "sched_setaffinity",
	"sched_getscheduler", "sched_getparam", "sched_get_priority_max", "sched_get_priority_min",
	"uname", "newuname", "sysinfo", "getrandom",
	"poll", "ppoll", "pselect6", "select",
	"prctl", "tgkill", "membarrier",
	"statfs", "fstatfs", "fstatfs64", "statfs64",
	"utimensat", "futimesat", "newfstatat",
	"socketpair",
	"capget", "capset",
	"getpriority", "setpriority",
}
