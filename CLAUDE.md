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
```

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

One `package main` at the root. The clusters, by prefix:

| files | what |
|---|---|
| `agent_*.go` | one file per stage; `agent_dev.go` is the developer loop and by far the largest |
| `agent_dev_prompt.go` | the developer's prompt, tool schema and reply parsing |
| `dispatch.go`, `workflow.go`, `projdispatch.go` | claiming, columns, per-project scheduling |
| `codearmory.go`, `forge.go`, `sandbox.go` | platform, forge and lease clients |
| `inference.go`, `classes.go`, `queue.go` | the model gateway, serving classes, admission |
| `transcript.go`, `telemetry_metrics.go` | JSONL transcripts and OTel metrics |
| `tui.go` | the terminal UI |

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
