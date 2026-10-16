# ephyra-judge

Linux judge node for [Ephyra](https://github.com/jellyfish-p/ephyra). It opens
the versioned `JudgeControl` gRPC stream, downloads authorized files, compiles
and runs programs through [go-sandbox](https://github.com/criyle/go-sandbox),
and reports normalized results. It does not talk to PostgreSQL, Redis, or S3
credentials.

Protocol module: `github.com/jellyfish-p/ephyra/protocol`. This repository does
not copy `.proto` files.

## What this node does

- Handshake with protocol version `1` and the v0 execution profile IDs
- Heartbeat on its own goroutine so a running program cannot stall the lease
- SHA-256 + size checks on every `FileRef`
- Compile argv comes from local profiles, never from the task
- Batch: exact / token compare, or a sandboxed testlib checker
- Interactive: user and interactor in separate containers, connected by pipes
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

These IDs are placeholders until a toolchain release is locked on a recorded
Linux image.

## Run

Register the node on the business server, then:

```text
EPHYRA_JUDGE_ADDR=judge.example:9090
EPHYRA_JUDGE_NODE_ID=<uuid>
EPHYRA_JUDGE_CREDENTIAL=<secret>
EPHYRA_JUDGE_WORKDIR=/var/lib/ephyra-judge
EPHYRA_JUDGE_MAX_SLOTS=1
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
