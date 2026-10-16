# ephyra-judge

Linux judge node for [Ephyra](https://github.com/jellyfish-p/ephyra). It opens
the versioned `JudgeControl` gRPC stream, downloads authorized files, compiles
and runs programs through [go-sandbox](https://github.com/criyle/go-sandbox),
and reports normalized results. It does not talk to PostgreSQL, Redis, or S3
credentials.

Protocol module: `github.com/jellyfish-p/ephyra/protocol`. This repository does
not copy `.proto` files.

## What this node does

- Handshake with protocol version `1` and only the execution profiles that
  passed a local gcc/g++ self-check
- Persist attempt identity, phase, and pending results under `WORKDIR/ledger`
  so a stream drop does not cancel work or re-run a finished attempt
- Retransmit persisted results after reconnect; a process restart that finds a
  still-running record reports `system_fault` / `uncertain_restart` instead of
  guessing a rerun
- Heartbeat on its own goroutine so a running program cannot stall the lease
- SHA-256 + size checks on every `FileRef`, with a per-download timeout and a
  task-level data budget
- Compile argv comes from local profiles, never from the task
- Compile cache identity includes role, filenames, flags, and source hashes;
  writes are temp-file plus rename
- Batch: exact / token compare, or a sandboxed testlib checker
- Interactive: user and interactor in separate containers; pipe write ends are
  closed once both processes have started so the peer can see EOF
- Linux sessions install a seccomp allowlist and, when cgroup is available,
  task-level memory / PID / CPU-usage limits
- All-or-nothing subtask scores as decimal strings; the server recomputes and
  rejects a contradictory summary
- Checker / interactor crashes are `system_fault`, not `wrong_answer`
- Process exit 0 is not accepted until the configured checker says so

## What it does not do

- OI last / IOI whole-submission / ACM penalty math
- Visibility, freeze, or publication
- Auto-redispatch after disconnect
- Claim that go-sandbox isolation has been validated on a specific kernel

## Profiles

| ID | Language | Compile |
|---|---|---|
| `cpp23.gcc.linux-amd64.v0` | cpp23 | `g++ -std=c++23 -O2 -pipe` |
| `c17.gcc.linux-amd64.v0` | c17 | `gcc -std=c17 -O2 -pipe` |
| `c23.gcc.linux-amd64.v0` | c23 | `gcc -std=c23 -O2 -pipe` |

The node advertises an ID only after `gcc`/`g++` accepts that `-std` flag on a
trivial translation unit. Checker and interactor compiles also need pinned
`testlib.h` (image default `/usr/include/testlib.h`, override with
`EPHYRA_JUDGE_TESTLIB`, or ship `testlib.h` in the problem data).

`normalized_config_json` is version `1`. The shared sample is
`internal/attempt/testdata/normalized_config_v1.json` in this repo and
`protocol/testdata/normalized_config_v1.json` in Ephyra; keep them identical.

## Run

Register the node on the business server, then:

```text
EPHYRA_JUDGE_ADDR=judge.example:9090
EPHYRA_JUDGE_NODE_ID=<uuid>
EPHYRA_JUDGE_CREDENTIAL=<secret>
EPHYRA_JUDGE_WORKDIR=/var/lib/ephyra-judge
EPHYRA_JUDGE_MAX_SLOTS=1
EPHYRA_JUDGE_TESTLIB=/usr/include/testlib.h
```

Local development against an insecure gRPC listener:

```text
EPHYRA_JUDGE_INSECURE=1
```

TLS is the default. Optional `EPHYRA_JUDGE_CA_FILE` overrides the system pool.
There is no first-release mTLS.

The binary must be the Linux judge process itself: go-sandbox re-execs
`/proc/self/exe` as the container init (`argv[1] == container_init`).

## Tests

Windows and other non-Linux hosts run the orchestration tests against a fake
sandbox. Real `go-sandbox` execution is Linux-only.

```text
go test ./...
```
