# CLAUDE.md

Guidance for Claude Code (claude.ai/code) working in this repository.

## Commands

```bash
make build          # GOWORK=off go build -o blacksmith .
make unit           # -short: logic only, no HTTP servers stood up
make test           # unit + integration (hermetic, safe anywhere)
make race           # -race -count=2 — dispatch and the queue are concurrent
make vet fmt        # go vet ./... and gofmt
go test -run TestName ./...

# The binary. Bare, it opens the TUI: type requests, watch them run.
go run .                          # or, installed: blacksmith
go run . -repo ./scratch -task "build a task tracker" -plan -dry-run
go run . service                  # the department daemon (what systemd runs)
go run . window                   # the department board view
go run . -auto                    # idle mode: work the board, security findings first
```

The plan arm is five stages: `plan-architect` writes PLAN.md (impl + tests),
`plan-test` writes the tests RED with compiling stubs (expected-red gate),
`plan-dev` makes them pass (tests locked, whole-file rewrites allowed),
`plan-sec` reviews for security, `plan-review` reviews for quality. The two
reviewers write NO code — they file findings as tickets on the plane's board,
kinded `security:` / `quality:`, deduped by exact title. SAST/SCA (gosec,
govulncheck) run before `plan-sec` and lint (staticcheck) before `plan-review`,
dropping reports under `scan/` as leads to verify — never committed. Every run
is itself a board ticket, opened in_progress and resolved on the verdict.

`-auto` is the board's consumer: with no request to run it takes the
highest-priority open finding (all security before all quality, then by
severity) and works that finding's WHOLE project on one clone — each finding
through the `fix` stage (a locked-suite developer whose task is the finding)
and the `review` stage, approved fixes accumulating as commits. A fix is not
done on the reviewer's yes alone: the SOURCE SCANNER re-runs on the fixed tree
and must go quiet — the issue gone and nothing new flagged (security re-runs
gosec+govulncheck, quality re-runs staticcheck; the kind names the tool). A fix
the detector still flags, or that lights it up somewhere new, is fed back and
revised. The merge to dev is GATED: it lands the accumulated batch only when
every finding **above low severity** (critical/high/medium) is fixed, so dev
never takes a project while a serious hole is still open. If one above-low
finding can't be fixed the whole merge is held; low findings don't gate. After
a batch merges, the project gets ANOTHER FULL RUN — the scanner suite and both
reviewers re-examine the merged tree and file anything that survived or was
newly exposed, so the loop converges: a project is done only when a full run
turns up nothing. Best-effort, bounded, a stuck fix left open with a comment.

Every run journals to git: one commit per landed write (the type and summary
each edit already declares), a mark per stage and draw, and a push to the
project's OWN repo on the plane as branch `run/<name>` — base URL in
`AGENTS_WORKSHOP_GIT_URL`, repo auto-created on first push by the
`AGENTS_WORKSHOP_MKREPO` one-liner. A new project is a new -repo flag; the
server side is `git init --bare`, nothing more. Clone any run anywhere:
`git clone -b run/003-… git://192.168.58.2:30918/myproject.git`.

`-dry-run` prints the wiring — forge URL, image, and each role's class and check
— without calling a model or acquiring a sandbox. It is the fastest way to find
out whether this host is configured to run a stage at all.

`make e2e` creates real tickets on a real board and needs `CODEARMORY_URL`,
`CODEARMORY_TOKEN` and `BLACKSMITH_E2E_BOARD`. It is not part of `make test` for
that reason.

**Always run `make test` before committing.** Several failures this repo has hit
were caught only by the full suite — a schema change that broke a prompt
assertion, a claim-counting change that broke four dispatch tests.

## What this is

blacksmith is a **client** of CodeArmory, not a component of it. It runs on a
workstation beside the GPUs and pulls work through conductor; it has no inbound
HTTP API and no registry entry. Three consequences shape everything:

- Work is **pulled**, because conductor cannot reach a workstation behind NAT.
- **Several hosts may attach to one CodeArmory**, so claiming a ticket is a
  race-safe append protocol and every transcript record carries a host.
- **Sandboxes are forge executions**, not local containers. This process never
  runs user code.

## The pipeline

A ticket moves across board columns, and each column is one stage's input. The
roles, in order: `architect-agent` → `pm-agent` → `spec-agent` →
`spec-merge-agent` → `dev-agent` → `sec-agent` → `integrator`, with
`resolver` for conflicts and `coverage-agent` after.

**It is test-first, and that is the load-bearing constraint.** The specification
author writes failing tests; the developer may never edit `*_test.go`. Most of
the machinery exists to serve that one rule:

- a refusal when the developer writes to a test file
- a **hand-back** to the author when no implementation could satisfy the tests
  (`endOnBrokenSpec`), bounded by `maxSpecRepairs`
- a **referee** (`agent_referee.go`) for the ambiguous middle: an LLM decides
  whether a failure is the spec's fault, the developer's, or the expected red of
  test-first. It acts only on high confidence, and only after several failed
  verifications.

## Layout

`main.go` at the root runs the full department; everything else is a package
under `internal/`. There are TWO SHAPES in the tree at once, deliberately:

- **the department** — board-driven, several hosts, one package per stage. This
  is what `main.go` runs and what has the operating history.
- **the simple shape** — `internal/tools` and `internal/agents`, driven from
  the root binary (`cli.go`, `tui_simple.go`): the TUI on the bare command, a
  batch CLI behind flags, no board and no claiming. This is the rebuild, it is
  where new work starts, and it owns the front door; the department stays
  reachable as `blacksmith service` and `blacksmith window`.

They share the parts that were expensive to get right — `internal/edit`,
`internal/forge`, `internal/model` — and nothing else. The simple shape is not
wired into dispatch, and dispatch is what it grows back into once several hosts
have to share a board.

| package | what |
|---|---|
| `internal/tools` | the tool set and the sandbox they run in: the in-memory workspace, the write guards, the forge-backed runner |
| `internal/agents` | the only model calls in the rebuild: a `Creator` that builds a wired agent, one loop, and one constructor per stage |
| `cli.go`, `tui_simple.go` | the front door: the TUI, the batch CLI, the reroll |
| `internal/agent/*` | the department's stages, one package each; `agent/dev` is the largest |
| `internal/dispatch`, `internal/workflow`, `internal/department` | claiming, columns, per-project scheduling |
| `internal/platform`, `internal/forge`, `internal/transport` | platform, forge and lease clients |
| `internal/model`, `internal/queue` | the model gateway, serving classes, admission |
| `internal/edit`, `internal/gate` | edit resolution and the verification gate |
| `internal/transcript`, `internal/telemetry` | JSONL transcripts and OTel metrics |
| `internal/window` | the terminal UI |

## Conventions that matter

**Comments explain WHY, with the evidence.** This codebase's comments cite the
run and the number — "measured on r63: 80 refusals on one section" — because the
non-obvious decisions here were all bought with a failed run. When you change
one, say what it cost. Do not delete that history to tidy up.

**Prefer making a mistake unrepresentable over explaining it.** Repeatedly, a
message telling the model not to do something did not stop it and a schema or
filesystem constraint did. The edit tool has one shape per way of addressing
code so a quote cannot be copied into its own replacement; `*_test.go` is
`chmod a-w` in the sandbox rather than defended by a refusal.

**Every refusal must be actionable.** A message that names the symptom and not
the cause sends a correct agent to the wrong place — the most expensive single
failure class in this repo's history. Name the cause, and give the numbers or
the symbol the agent needs.

**Closed sets for metric labels.** Reason codes are enumerated and anything
unknown becomes `other`; a growing `other` means add a code. An unbounded label
is a cardinality bug.

## Configuration

Environment, read from `~/.config/codearmory-agents/env` (systemd
`EnvironmentFile`). **No inline comments after a value** — systemd does not strip
them, so `FOO=2 # note` sets `2 # note` and blacksmith crash-loops silently.

| variable | effect |
|---|---|
| `AGENTS_REPO_IMAGE` | runner image; must also be in forge's `ALLOWED_IMAGES` |
| `AGENTS_LARGE_ENDPOINT` | model endpoint — must be reachable **from the cluster** if delegating |
| `AGENTS_LARGE_SLOTS` | must match llama-server's `--parallel` |
| `AGENTS_SANDBOX_PROXY` | the sandbox's only route to the internet |

## Traps

- **`kubectl apply` of `deploy/k8s/forge-local.yaml` resets the live deployment**
  to whatever the manifest says. It has reverted the forge image and wiped the
  runtime-backend config before. Change the manifest, not the deployment.
- **forge caps lease timeouts** (idle 300s, lifetime 3600s) whatever is asked
  for, and reports execution status as **`completed`**, not `succeeded`. Polling
  for the wrong word spins until the lease is reaped.
- **A claim is written when work starts**, so a killed process leaves one behind
  that counts against `maxAttempts` — see `pruneStaleClaims`.
- **`RunOnBranch` resets the working tree** (`reset --hard`, `clean -xfd`), so
  anything an agent writes must be committed and pushed in the same command.
