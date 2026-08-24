# blacksmith

The agent runtime for [CodeArmory](https://github.com/code-armory-app/codearmory) — a set of
role-scoped LLM agents (product manager, developer, security reviewer, QA) that work the
platform's own primitives: tickets, git, pipelines, environments.

## What this is, and what it deliberately is not

blacksmith is a **client** of CodeArmory, not a component of it. It runs on a workstation
beside the GPUs, reaches the platform through conductor exactly as a developer's tooling
would, and is never registered as a route. Three consequences follow, and they shape
everything else in this repo:

- **There is no inbound HTTP API and no registry manifest entry.** Work is *pulled* —
  conductor cannot reach an intermittent workstation behind NAT.
- **Several blacksmith hosts can attach to one CodeArmory at once**, the way several
  developers share one CI/CD platform. Nothing here may assume it is the only one, which is
  why claiming a ticket is a race-safe protocol (see below) and every transcript record
  carries a host.
- **Sandboxes are not run by this process.** They are forge executions submitted to a forge on
  this host (`AGENTS_FORGE_URL`), reached directly rather than through conductor — nothing is
  routing by service name, so those paths carry no `/forge` prefix. blacksmith never
  reimplements isolation. Without that setting sandboxes fall back to the platform's forge,
  which works but puts the sandbox a network away from the model.

The division of labour is the same one a human developer has with GitHub: the workstation
runs the iteration, the platform runs the **gate**. An agent's branch triggers a pipeline on
the cluster, in kata, exactly as a person's would.

| | Human dev | Agent |
|---|---|---|
| Iterates, edits, runs tests ad hoc | Laptop | blacksmith sandbox |
| Branch, PR, review, merge gate | CI on the platform | **forge, on the cluster** |

## Status

Implemented:

- **Model-class routing** (`classes.go`) — agents ask for a capability tier (`tiny` / `small`
  / `large`), never for a model or a host. This is the seam that keeps the inference layer
  replaceable; the stack behind a class has already changed twice.
- **Admission control** (`queue.go`) — bounded priority queue with the critical-work rule: a
  critical task takes the whole box, waiting for in-flight work to drain rather than
  preempting it.
- **Inference gateway** (`inference.go`) — OpenAI-compatible client. Reports queue wait and
  model latency separately, so a saturated box is not mistaken for a slow model.
- **Transcript capture** (`transcript.go`) — append-only JSONL, one record per event, so an
  interrupted task keeps what it produced.

- **Platform client** (`codearmory.go`) — tickets, comments, and the claim. See below on why
  the claim is an append rather than a compare-and-set.
- **Task dispatch** (`dispatch.go`) — the pull loop: reconcile, fill to N, refill on
  completion, poll when idle, priority-ordered, loop-breaker applied to selection.
- **The product-manager agent** (`agent_pm.go`) — triage: summary, priority, labels, proposed
  breakdown.
- **Sandboxed execution** (`forge.go`) — submit, poll, cancel, through forge. blacksmith runs
  no containers itself; forge already provides the runtime backends, resource caps, admission
  control, reaping and the egress policy, and this is a client for it and nothing more.
- **The developer agent** (`agent_dev.go`) — reads the repo, edits files, runs the tests, and
  pushes a branch when they pass. See below.

- **The security reviewer** (`agent_sec.go`) — reads the diff a developer agent pushed and
  reports findings. Its entire effect is **one ticket comment**: it cannot write, run, push or
  block. A reviewer that can block is one that can be prompt-injected into blocking, which
  turns code review into a denial-of-service on the team's own pipeline. Its input is a diff,
  which on a compromised branch is attacker-authored text arriving in the prompt — so the diff
  goes in a user turn, never the system prompt, and review output is treated as untrusted.

Not yet implemented: the QA agent.

### The agents are a pipeline, not a queue

Each handler declares which tickets it wants, using only what is already on the ticket:

| Ticket state | Taken by |
|---|---|
| no triage comment | product manager |
| triaged, no branch | developer |
| branch pushed, no review | security reviewer |
| reviewed | nobody — it is a human's to merge |

Without that predicate two agents on one host race for the same tickets, and the claim makes
the race silently destructive: whichever wins, the other never sees the ticket again.

### The developer agent's loop

Each iteration is one model call returning **one** action, which blacksmith executes and feeds
back: `read_files`, `write_files`, `run_tests`, `finish`, `give_up`. The loop is a switch
statement — no framework, because the smallest thing that works is also the only version whose
failure modes are obvious.

**Verification is not the agent's to define.** `run_tests` pushes the branch and runs it
against a definition the agent did not write — never a test command the model invented.

With `AGENTS_REPO_PIPELINE_ID` that definition is the repository's **own CodeArmory
pipeline**, the same one a person's push runs: one definition of "does this work" rather than
two that drift, visible in the platform with its own history, logs and approval gates rather
than buried in a transcript. An `awaiting_approval` run stops the agent — opening a human gate
is not its call.

With `AGENTS_REPO_TEST_COMMAND` instead, it is the **operator's** command, run in the sandbox
against the pushed branch. That is a second definition and it can drift from CI, which is the
honest cost of the thing it buys: a host whose repository no pipeline can reach still closes
its own loop. See [Verifying a change](#verifying-a-change).

**The action surface is deliberately narrow.** The model cannot run arbitrary shell. Every path
it supplies is validated against escaping the checkout, and file content is base64-encoded
before it reaches the script that writes it — that encoding is what keeps "write a file" from
becoming "run a command".

Worth being precise about what that defends. The sandbox **is** a kernel boundary today: the
deployed forge runs the `kata` backend with the `kata-clh` RuntimeClass, so agent commands
already execute in a microVM behind the egress NetworkPolicy. The narrow action list is defence
in depth, not the only wall. What it still buys is bounding what an injection can *do* rather
than only where it runs — a microVM contains a compromised sandbox, but it does not stop an
agent pushing a malicious branch, and the action list does.

**The repository's own commit hooks run.** Same principle as verification: the project already
decides what a commit must satisfy — a conventional message, no secrets, no oversized files —
and an agent that passes `--no-verify` is held to a lower standard than the people it works
alongside. It also moves those failures minutes earlier than the pipeline would. Hooks that
rewrite files (formatters) fail the commit *and* leave their fixes staged, so the commit is
retried once; a second failure is a real rejection and is reported to the model.

Commit messages are Conventional Commits, with the type supplied by the model and validated
against an allowlist. This is not cosmetic: repositories routinely run commitlint on the
`commit-msg` hook, and a message without a recognised type is a commit that cannot land.

The git credential is a forge `secret_ref`, resolved at dispatch — blacksmith never holds it
and it never reaches the model.

### Claiming a ticket

Two agent hosts can race for the same ticket, and both winning means duplicate branches,
duplicate PRs and duplicate spend. The claim resolves that in one of two ways depending on the
instance:

**Conditional write (preferred).** The tickets service serves the ticket's version as an `ETag`
and accepts `If-Match` on `PUT`, so moving a ticket to `in_progress` is a compare-and-set: the
version sits in the `WHERE` clause of the `UPDATE`, there is no window between checking and
writing, and exactly one racer wins. A loser gets `412` and yields.

**Claim by append (fallback).** Older instances have no `If-Match`, and there a conditional
write silently degrades to last-write-wins — so blacksmith detects the missing `ETag` and
switches strategy rather than issuing a write it cannot trust. Comments are append-only with
server-assigned ordering, so every host appends a claim, reads the comments back, and yields
unless the *oldest* claim is its own. Both hosts apply the same deterministic rule to the same
server-ordered list, so they agree even when one reads before the other has written. Ties break
on comment id.

Either way a claim comment is written, because it is also the **audit trail and the attempt
counter**: it records which host and role took the work, which the agent account alone cannot
express when one role runs on several machines.

## Configuration

All configuration is environment variables. Secrets follow the CodeArmory convention: `NAME`
is read from `${NAME}_FILE` first, so a mounted secret file works without changing anything.

| Variable | Default | Meaning |
|---|---|---|
| `AGENTS_<CLASS>_ENDPOINT` | — | OpenAI-compatible base URL. A class with no endpoint is simply absent, which is a supported deployment. |
| `AGENTS_<CLASS>_MODEL` | — | Model name sent in the request body. |
| `AGENTS_<CLASS>_API_KEY` | — | Optional bearer token. |
| `AGENTS_<CLASS>_SLOTS` | `4` | Concurrent requests to keep in flight. **Match this to the serving backend's own slot count.** |
| `AGENTS_<CLASS>_QUEUE` | `32` | Waiting-list bound; a burst sheds load rather than growing. |
| `AGENTS_HOST` | hostname | Identifies this host in transcripts and claims. |
| `AGENTS_TRANSCRIPT_DIR` | `/var/lib/codearmory-agents/transcripts` | Capture is **opt-out**; set to `off` to disable. |
| `CODEARMORY_URL` | — | Conductor endpoint, including any path prefix. Without it (and a token) the host serves models but pulls no work. |
| `CODEARMORY_TOKEN` | — | The agent account's credential. |
| `AGENTS_BOARD_ID` | — | Scopes which tickets are worked. Empty means every board the account can see. |
| `AGENTS_FORGE_URL` | — | A forge on **this host**, reached directly. Empty sends sandboxes to the platform's forge — which works, but is a stopgap. |
| `AGENTS_FORGE_TOKEN` | — | A static token for the sandbox plane. Works, but expires; prefer the pair below. |
| `AGENTS_FORGE_GATEKEEPER_URL` | `AGENTS_FORGE_URL` | Where the sandbox plane issues sessions. Needed for a **local** plane: forge serves no `/login`, so the default only works through conductor. |
| `AGENTS_FORGE_EMAIL` / `AGENTS_FORGE_PASSWORD` | — | Login for the sandbox plane's own gatekeeper. blacksmith exchanges these for a session and **renews on a 401**, so the host does not stop working hours after deployment. |
| `AGENTS_CONCURRENCY` | PM class's slots | Tickets worked at once. |
| `AGENTS_POLL_SECONDS` | `15` | Level-trigger interval — this is the pickup latency, and it compounds once per pipeline stage. |
| `AGENTS_PM_CLASS` | `small` | Class the product-manager agent runs on. |
| `AGENTS_DEV_CONCURRENCY` | `4` | Developer tickets worked at once. Each holds a sandbox for minutes, so this multiplies the two below. |
| `AGENTS_DEV_MEMORY_MB` | `8192` | Memory for one developer sandbox. |
| `AGENTS_DEV_CPU_MILLICORES` | `4000` | CPU for one developer sandbox (`4000` = 4 cores). |
| `AGENTS_REPO_RUNNER_CLASS` | `agent-dev` | The forge runner class the two above are written to at startup. |
| `AGENTS_REPO_PIPELINE_ID` | — | Platform pipeline that verifies a branch. **Preferred**; see below. |
| `AGENTS_REPO_TEST_COMMAND` | — | Verifies a branch in the sandbox instead, with no platform involved. |
| `AGENTS_REPO_FORMAT_COMMAND` | — | Rewrites the code before the commit, e.g. `gofmt -w .`. |
| `AGENTS_REPO_LINT_COMMAND` | — | Runs before the tests, e.g. `go vet ./...`. |
| `AGENTS_REPO_CRITICAL_COMMAND` | — | The one analysis that **gates**. Exit non-zero only above your severity threshold. |
| `AGENTS_REPO_SCAN_COMMAND` | — | SAST over this project's code. Advisory; becomes the reviewer's context. |
| `AGENTS_REPO_SCA_COMMAND` | — | Dependency scan. Advisory — except for regressions, below. |
| `AGENTS_REPO_DEPENDENCY_MANIFESTS` | go.mod, package.json, … | Paths whose change means this branch altered the dependency tree. |

`<CLASS>` is `TINY`, `SMALL` or `LARGE`.

### Verifying a change

The developer agent needs a repository **and** a way to verify it. Either variable enables it;
with neither, the host triages only.

- **`AGENTS_REPO_PIPELINE_ID`** — the agent pushes a branch and triggers the repository's own
  pipeline, exactly as a person's push would. One definition of "does this work", shared with
  every human contributor, that cannot drift from CI because it *is* CI. Prefer it.
- **`AGENTS_REPO_TEST_COMMAND`** — the agent runs the command in its own sandbox against the
  pushed branch. A second definition of "does this work", so it *can* drift from CI. What it
  buys is that the department runs **standalone**: inference, a git remote and a forge are
  enough to close the loop, so an unreachable platform degrades verification instead of
  stopping the agent.

Set both and the pipeline wins. Startup logs which one is in force — the sandbox path logs a
warning, because it is the weaker guarantee and the silent default.

Either way it is the **pushed branch** that is verified, never the agent's local state. A file
lost to a hook or a `.gitignore` fails the check rather than passing it, which is the whole
reason the agent clones again instead of testing what it already has in hand.

### Telemetry

blacksmith emits OpenTelemetry traces the way the platform's services do, so a host shared by
several developers reports into the same place as everything else:

```sh
OTEL_EXPORTER_OTLP_ENDPOINT=http://collector:4318
```

Unset, it logs locally and says so once at startup. That is the normal case on a workstation
and deliberately not a startup failure — traces are a diagnostic, not a dependency.

Three spans, chosen because they are the three questions an investigation actually starts from:

| Span | Carries | Answers |
| --- | --- | --- |
| `stage.<role>` | ticket, role, host, outcome + detail | which stage did what, to which ticket, on which host |
| `sandbox` | image, runner class, execution id | was the expensive part the sandbox |
| `inference.<class>` | model, queue wait, latency, tokens | was it the model — and was it queued or slow |

The stage span lives in the DISPATCHER rather than in each agent, so a new stage is traced the
day it is written rather than the day someone remembers to add it. `outcome.detail` is on the
span because several endings are reported as success — merged, conflict, no usable resolution
— and without it three very different outcomes look identical in a trace.

Queue wait is recorded separately from model latency for the same reason the gateway reports
them apart: a saturated box and a slow model look identical from outside and want opposite
responses.

### Watching it work

```sh
blacksmith            # the window
blacksmith service    # the department itself — what the systemd unit runs
```

The bare command opens the window, and running the department takes a word. That is the right
way round because of who runs each: a person types `blacksmith` at a prompt to see what is
happening, while the department is started by systemd, once, from a unit file where an extra
word costs nothing. The other arrangement made the common interactive case the one that needed
remembering, and made an accidental `blacksmith` start a second department on a host that
already had one.

Configuration comes from `~/.config/codearmory-agents/env` — the same file the unit reads — so
both work from any shell without exporting anything. Anything already exported wins over the
file, so a one-off override still behaves.

One screen instead of three tools. Before this, answering "what is it doing" meant tailing a
log, polling the ticket store and checking `kubectl` for a sandbox pod — and none of those
answers it alone.

It shows every ticket with the stage it has reached, derived from the **same markers the
dispatchers select on**, so a row says what the department will actually do next rather than a
second opinion about it. A ticket being worked shows the agent holding it, what it is doing
right now, and which turn it is on against the iteration cap — the cap is what silently ends a
task, and watching it approach is the difference between "thinking" and "about to give up".
`enter` opens the ticket with its triage, branch and review; `n` files a new one, which is the
other half of why it exists: handing the department work should not mean remembering which
store is authoritative on this host and curling JSON at it.

It **reads the durable record** — the ticket store and the transcripts — and holds no
connection to the running process. blacksmith has no inbound API by design, and attaching to
the process would either break that or tie the window's life to the service's, so you could
not open it on the department already running. The cost is in-memory state: queue depth and
busy serving slots live in the process and are written nowhere, so they are absent here. That
wants a local socket, which is its own piece of work.

### The harness around the agents

Every check that can be a tool is a tool. The agents are expensive, non-deterministic and
slow; a formatter, a linter and a static analyser are none of those, and each one that runs
first is work the model does not have to do — and a class of mistake it cannot make.

| Stage | When | Blocks? |
| --- | --- | --- |
| `AGENTS_REPO_FORMAT_COMMAND` | before the commit | n/a — it fixes rather than reports |
| `AGENTS_REPO_LINT_COMMAND` | first in verification | **no** — advisory |
| `AGENTS_REPO_TEST_COMMAND` | second | **yes** |
| `AGENTS_REPO_CRITICAL_COMMAND` | third | **yes** |
| `AGENTS_REPO_SCAN_COMMAND` / `_SCA_COMMAND` | before the review | **no** — the reviewer's evidence |

Three decisions in that table are load-bearing:

**Formatting runs at commit time, not in verification.** Run afterwards it would only report
on a branch that is already pushed. Run before `git add -A`, whitespace and import order stop
being something the model can get wrong at all — and the missing trailing newline that used
to appear on every file the agent touched is structurally impossible rather than fixed.

**Only two things block, and severity is the line.** The tests block because "does this work"
is binary. A critical finding blocks because the alternative is worse *for exactly the findings
that matter most*: routed to the security reviewer it becomes a ticket comment, from an agent
that cannot fix anything, and the fix arrives on a later ticket having lost the context of the
change. Blocking returns it to the agent that still has the branch open and iterations left.

**Everything else is advisory, and that is not timidity.** A linter reports things a change
cannot fix — a false positive, a rule wanting a refactor beyond the ticket, two rules that
disagree. Behind a gate each of those traps the agent: it has a fixed iteration budget, it
spends it trying, and a correct change is thrown away over a lint rule. That is the same
failure the security reviewer is deliberately advisory to avoid — a check that can block is a
check that can deny the pipeline, whether the trigger is a prompt injection or a stubborn rule.
An operator who genuinely wants lint to block has the mechanism: put it in `TestCommand`.

**The gate order is cheapest-and-most-precise first, stopping at the first failure.** A
linter says `main.go:12`; the same fault through a test runner is a wall of output with the
cause buried in it. The failing gate is named in what goes back to the model, because
"the tests failed" when it was the linter sends it looking in the wrong place, and it has
eight iterations to spend. All of it is one sandbox run — the clone and toolchain warm-up
dominate, so paying them per gate would make the fast feedback slower than what it replaced.

**A dependency scan gates only on REGRESSION.** An SCA finding is normally a property of a
tree that predates the ticket, and blocking on one stops an agent adding a function because
some transitive package has an advisory — outside its brief, possibly unfixable upstream, and
an invitation to smuggle a version bump into a feature branch where nobody is reviewing it. So
the branch is diffed against its base: if it did not touch a dependency manifest, findings are
pre-existing and advisory. If it did, this change owns them and the gate applies, because that
fix is usually one version bump the agent can make while the branch is still open. The check
**fails open** — a base that cannot be fetched is an unknown, and a gate that fires on an
unknown traps the agent for a reason nothing in its output explains.

**SAST and SCA are separated in the evidence.** A finding in your own code is a change to
make; a finding in a dependency is usually a version bump, sometimes has no upstream fix, and
is sometimes unreachable from this project at all. A reviewer that cannot tell which it is
reading gives bad advice about both, so the sections are labelled and the reviewer is told
plainly that a dependency finding is not a defect in the diff in front of it.

**The scanners run before the reviewer, on a CPU.** A static analyser finds hardcoded
credentials, unchecked errors and known-dangerous calls deterministically and repeatably,
without a GPU. Handing the reviewer that output spends the model on the judgement a scanner
cannot make — whether a finding matters in this change — instead of on rediscovering what the
tool already knew. The findings arrive in the same **user** turn as the diff and are labelled
unverified: on a compromised branch a scanner echoes the attacker's own paths and code back
verbatim, so it is attacker-influenced text exactly as the diff is, and it reaches the system
prompt no more than the diff does.

### Sizing the developer sandbox

The defaults are **8 GB and 4 cores per developer sandbox, 4 at a time** — a 32 GB / 16-core
peak on the host. blacksmith writes the sizing to its own runner class (`agent-dev`) at
startup rather than editing one of forge's, so raising `AGENTS_DEV_MEMORY_MB` re-sizes agent
work without re-sizing every other tenant's executions.

These are GBs and whole cores because a toolchain compiling inside a microVM pays for the
guest's own kernel and page cache on top of the workload, and the failure mode is not a clean
error. Measured on the sandbox plane, forge's seeded 256 MB `standard` class fails
`go test ./...` on a **one-file** repository 3 runs in 5 — OOM-killed mid-compile, surfacing
as `signal: killed` and a build failure that the agent then tries to fix in the source. 512 MB
and up passed every time; the default sits far above that floor because the floor was measured
against a trivial repository and a real one compiles many packages at once.

CPU needs no such care: the guest is sized from the limit, so build parallelism follows it.
Measured on the plane, `cpu_millicores` → cores the guest brings online and the quota it
carries:

| `cpu_millicores` | guest `nproc` | guest `cpu.max` |
| --- | --- | --- |
| 500 | 2 | 0.5 CPU |
| 1000 | 2 | 1.0 CPU |
| 4000 | 5 | 4.0 CPU |

One core above the allowance is kata's own: `default_vcpus = 1` boots the sandbox and the
container's share is hotplugged on top. It cannot be spent, because the guest cgroup carries
the real quota — a build that forks `nproc` compilers is still held to `cpu.max`.

`/sys/devices/system/cpu/possible` does report all 32 host CPUs, which is the hotplug ceiling
(`default_maxvcpus = 0` in `configuration-clh.toml` means "the host's count"). Nothing that
picks its own parallelism reads it — `nproc`, `sched_getaffinity` and `GOMAXPROCS` all use the
online set — but a runtime that sizes per-CPU structures from `possible` would over-allocate.
Bound it with `default_maxvcpus` on the node if that ever shows up.

Matching slots to the backend is not a nicety. Measured on an R9700 serving a 30B coder model
at four llama-server slots: 145 tok/s at one stream, 384 aggregate at four — and **372 at
eight**, i.e. oversubscribing is worse than matching.

## Serving stack

`deploy/systemd/` holds user units for `llama-server`. Install with:

```sh
cp deploy/systemd/*.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now llama-large llama-small
sudo loginctl enable-linger "$USER"   # otherwise they start at login, not at boot
```

Three choices in those units are deliberate and should survive editing:

- **`--host 127.0.0.1`** is a security decision, not a default. blacksmith and its sandboxes
  run on the same host, so inference never crosses the network — which is what reduces the
  egress boundary to a single outbound hole to conductor.
- **`--api-key-file`** closes the remaining local hole. `llama-server` enables CORS for all
  origins and runs unauthenticated by default, so without a key any page open in a browser on
  this machine could drive the model.
- **`--alias`** gives the API a clean model name. Without it the `model` field reports the
  full filesystem path, and that path then lands in every transcript record.

## Running it

```sh
make build && cp blacksmith ~/.local/bin/
cp deploy/systemd/blacksmith.service ~/.config/systemd/user/
systemctl --user daemon-reload && systemctl --user enable --now blacksmith
```

Without `CODEARMORY_URL` and `CODEARMORY_TOKEN` the service starts, serves models
and logs `dispatch DISABLED` — a legitimate state, warned about loudly because a host
silently working no tickets looks exactly like a host with no tickets to work.

Telemetry degrades rather than fails: no OTLP collector is the normal case on a
workstation, and traces are a diagnostic, not a dependency.

## The sandbox plane

`deploy/k8s/forge-local.yaml` deploys a **minimal, self-contained CodeArmory** onto a
kata-capable local cluster — its own gatekeeper, its own forge, its own database — and it is
deliberately **not** connected to the platform's systems.

That means blacksmith holds **two credentials**: `CODEARMORY_TOKEN` for the platform, and
`AGENTS_FORGE_TOKEN` for the sandbox plane. The separation is the point. Compromising the
workstation yields the ability to run sandboxes on it — not a platform service key, and not the
grants that would come with one. Sandboxes also keep working when the platform is unreachable,
which matters on a host that is off half the time. blacksmith refuses to fall back to the
platform token for forge; doing so would send a platform credential to a host deliberately
outside the platform's trust domain.

Standing the cluster up:

```sh
minikube start -p blacksmith --driver=docker --container-runtime=containerd \
  --cni=calico --cpus=6 --memory=12g --disk-size=40g
helm install kata-deploy oci://ghcr.io/kata-containers/kata-deploy-charts/kata-deploy \
  --version 3.21.0 -n kube-system --set env.shims="qemu clh" --set env.defaultShim=qemu
```

**Use `kata-clh`, not `kata-qemu`.** kata-qemu fails on this hardware — the guest kernel panics
during boot with a QEMU register dump and the sandbox never starts. clh works, and it matches
what the platform's cluster runs, so both environments behave the same.

Verify isolation rather than assuming it. A kata pod's kernel must differ from the node's —
that is the only evidence that settles whether it is a microVM or a container — and a
deny-egress NetworkPolicy must actually block traffic, because kindnet and flannel accept a
policy and silently ignore it.

Two traps on a minikube host:

- **DNS inside the node container.** If docker's `daemon.json` sets `dns` to the default-bridge
  gateway (`172.17.0.1`), a minikube profile on its own network gets *its* gateway as the
  resolver, where nothing is listening. The symptom is calico stuck in `Init:ErrImagePull` and
  the node `NotReady`, while ICMP works fine. `/etc/resolv.conf` in the node is a bind mount, so
  `sed -i` fails with "Device or resource busy" — write it in place.
- **kata-deploy's manifest paths have moved.** The `tools/packaging/kata-deploy/...` YAML in most
  guides now 404s; the project ships a Helm chart.

## Mirroring and sync

`sync.go` keeps a local git server in step with an upstream — the cluster's git host, GitLab,
GitHub; it does not care which. That is what lets the department work while the platform is
down, and what makes blacksmith usable by someone who runs no CodeArmory at all.

**The refspec is the security control, not the git server's authentication.** The sandbox is
the untrusted party: it runs model-authored code and legitimately holds a credential for the
local server, so a prompt-injected agent can rewrite anything there. What stops that becoming
an upstream compromise is bounding what the *syncer* will push:

| Direction | Allowed |
|---|---|
| upstream → local | everything; this direction cannot escalate |
| local `main` → upstream | the **dev**/**exp** branch only |
| local `agent/*` → upstream | `agent/*` only |
| anything → upstream `main`, tags | **never** |

Local and upstream branch names are independent — the mapping is a refspec (`main:dev`), not a
naming constraint. `SyncConfig.Validate` refuses to target `main`, `master`, `release`, `prod`
or `production` whatever it is configured with, so a typo cannot point agent output at a
release branch. Nothing is force-pushed: a rejected non-fast-forward is a person's problem to
look at, not something to overwrite. Promotion to upstream `main` stays a pipeline run and a
human — nothing in blacksmith is authorised to make that call.

The sync runs **in a sandbox**, with the upstream credential as a forge `secret_ref`, so
blacksmith itself never holds a git credential. A failed sync is not fatal: the local plane
keeps working, which is the entire point of mirroring, and the next tick retries.

### Credentials expire; blacksmith renews them

The sandbox plane's gatekeeper issues short-lived sessions — an hour on the deployment this was
built against — and its long-lived API-token endpoint is absent from the published
`alpha-0.1.0` image. A token pinned in configuration therefore produces a host that works until
it quietly does not, with 401s that read like a permissions problem rather than an expiry.

So blacksmith holds the login credential and exchanges it for a session on demand. **Renewal is
driven by a 401, not a clock** — the server is the authority on whether a token is still good,
and a timer would need a TTL that is not published and can change. Exactly one retry per call,
so a genuine permissions failure surfaces as a denial instead of becoming a login loop against
a rate-limited endpoint, and concurrent renewals collapse onto one login so several agents
hitting an expired token do not lock themselves out.

A static `AGENTS_FORGE_TOKEN` is still honoured and is the right shape where a long-lived API
token exists; it is simply never renewed.

## Testing

Three tiers, each answering a different question and each runnable alone, so a failure
says something specific:

| | Question | Touches |
|---|---|---|
| `make unit` | Is the logic right? | nothing outside the process |
| `make test` | Do the parts talk to each other? | in-process HTTP fakes |
| `make e2e` | Does it work against the real thing? | live CodeArmory + llama-server |

`make race` runs unit + integration under the race detector — dispatch and the
admission queue are concurrent, and that is the tier where it gets checked.

**Integration tests drive the real client over real HTTP** against a fake CodeArmory,
rather than mocking an interface. Most of the bugs worth catching at that level —
header handling, status classification, JSON shapes, the claim race — live in exactly
the layer an interface mock would replace.

**E2E exists because a fake can only confirm what its author already believed.** It
caught a live example: the client was calling `/tickets`, but conductor routes by
service name, so the real path is `/tickets/tickets`. The unprefixed path does not
404 — it falls through to the portal, which answers `200` with the SPA's HTML, and the
client reports a JSON decode error that reads like a malformed API response rather
than a wrong URL. Every integration test passed throughout, because the fake had been
serving the unprefixed path back. The fake now mirrors conductor's routing exactly,
doubled `tickets` and all.

E2E needs a live stack and explicit configuration:

```sh
export CODEARMORY_URL=https://your-instance/api CODEARMORY_TOKEN=...
export BLACKSMITH_E2E_BOARD=<scratch-board-uuid>   # no default, on purpose
make e2e
```

It creates real tickets and deletes them on the way out; a leftover on the scratch
board means a cleanup failed and is worth investigating. Inference-only e2e tests run
without a platform credential.

## Relationship to the monorepo

blacksmith depends on the published `codearmory_sdk` rather than a path replace, so it builds
standalone. That means an SDK change needs a publish-and-bump here, unlike the in-tree
services which pick it up immediately.

The gatekeeper-side work this depends on — the non-loginable per-role agent accounts, the
username reservation, and `agents` in the scoped-role mint allowlist — lives in the
codearmory monorepo, because gatekeeper is core. Only the runtime is here.
