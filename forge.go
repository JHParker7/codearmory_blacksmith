package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

// Sandboxed execution, through forge.
//
// blacksmith does not run containers. forge already provides the runtime
// backends, runner classes, resource caps, admission control, reaping, workspace
// volumes, git checkout and the egress NetworkPolicy templates — the largest
// body of work in this design, already written and deployed. This file is a
// client for it and deliberately nothing more.

// forgeService is conductor's routing prefix, used when reaching forge THROUGH
// the platform. Same trap as the tickets service: an unprefixed path does not
// 404, it falls through to the portal and returns the SPA's HTML with a 200.
const forgeService = "/forge"

// forgePath builds a forge URL for whichever forge this client talks to.
//
// TWO DEPLOYMENTS, and the difference is not cosmetic:
//
//   - THROUGH CONDUCTOR (default): the platform routes by service name, so the
//     path carries the /forge prefix and sandboxes run on the cluster.
//   - DIRECT TO A LOCAL FORGE (AGENTS_FORGE_URL): the agent host runs its own
//     forge, and blacksmith submits to it without going through conductor — so
//     there is NO service prefix, because nothing is routing by service name.
//
// The local deployment is the intended one: it is what puts the sandbox on the
// same box as the model, and it is why the department's egress boundary
// collapses to a single outbound hole. Sending sandboxes to the cluster is the
// stopgap until that exists, not the design.
func (c *CodeArmory) forgePath(suffix string) string {
	if c.forgeURL != "" {
		return suffix
	}
	return forgeService + suffix
}

// Forge execution statuses.
const (
	ExecPending   = "pending"
	ExecRunning   = "running"
	ExecCompleted = "completed"
	ExecFailed    = "failed"
	ExecTimedOut  = "timed_out"
	ExecCancelled = "cancelled"
)

// SandboxSpec describes one command to run in isolation.
type SandboxSpec struct {
	Image       string            `json:"image"`
	Command     []string          `json:"command"`
	Env         map[string]string `json:"env,omitempty"`
	RunnerClass string            `json:"runner_class,omitempty"`
	TimeoutSecs int64             `json:"timeout,omitempty"`
	// OutputEnv names environment variables to capture after the command runs,
	// which is how a sandbox returns structured data rather than making the agent
	// parse its own stdout.
	OutputEnv []string `json:"output_env,omitempty"`
	// SecretRefs maps an env var name to a credential reference forge resolves at
	// dispatch. The values are never sent by blacksmith and never logged — this
	// is how a sandbox gets a git credential without the agent ever holding one.
	SecretRefs map[string]string `json:"secret_refs,omitempty"`
	// LeaseID runs the command inside a sandbox the agent already holds rather than
	// creating one for it. Image, RunnerClass and SecretRefs are then fixed by the
	// lease and MUST be left empty — forge rejects a leased execution that sets
	// them, because they would describe a sandbox other than the one it runs in.
	LeaseID string `json:"lease_id,omitempty"`
}

// Execution is forge's view of a submitted command.
type Execution struct {
	ExecutionID string            `json:"execution_id"`
	Status      string            `json:"status"`
	ExitCode    *int              `json:"exit_code,omitempty"`
	Stdout      *string           `json:"stdout,omitempty"`
	Stderr      *string           `json:"stderr,omitempty"`
	Outputs     map[string]string `json:"outputs,omitempty"`
	RunnerClass string            `json:"runner_class,omitempty"`
}

// SandboxResult is a finished execution, flattened for the caller.
type SandboxResult struct {
	ExecutionID string
	Status      string
	ExitCode    int
	Stdout      string
	Stderr      string
	Outputs     map[string]string
	Duration    time.Duration
}

// OK reports whether the command both finished and succeeded. Kept separate from
// the error return because "the sandbox ran and the tests failed" is a normal
// result an agent must reason about, not a transport failure.
func (r SandboxResult) OK() bool { return r.Status == ExecCompleted && r.ExitCode == 0 }

// isTerminalExec reports whether a status will not change again.
func isTerminalExec(status string) bool {
	switch status {
	case ExecCompleted, ExecFailed, ExecTimedOut, ExecCancelled:
		return true
	}
	return false
}

// Polling bounds. An agent's command is usually seconds to minutes, so the first
// few polls are quick and then back off — a tight poll for the whole duration of
// a long build is load on the control plane for no information.
//
// THE CAP IS WHAT THE PIPELINE WAITS ON, and at 5s it was tuned for a workload
// this department does not have. Because the interval doubles from pollMin, the
// only instants anyone can learn an execution finished are 0.5s, 1.5s, 3.5s,
// 7.5s, 12.5s, 17.5s — and every sandbox duration recorded on r77 was one of
// exactly those six numbers, 69 of them, with nothing in between. They are not
// measurements of work; they are the moments someone looked.
//
// The waste is half the final interval per call. Measured on r77: 36 calls of 2s
// or more, 18 of them sitting on the 7.5s boundary, roughly 90s of the run's 351s
// of sandbox time spent waiting after the work was already done — about 12% of
// the whole run.
//
// One second keeps the backoff and moves the cap to where the executions
// actually are. The load it costs is bounded by the same argument that set the
// old value: a 60-second build now takes 60 polls instead of 14, which is
// nothing against a local control plane, and the executions here are seconds.
//
// The real fix is not here. Forge's worker blocks until the container exits, so
// it knows the instant an execution finishes and simply has no way to say so:
// the API is POST /executions then poll. A blocking read — GET with a wait
// parameter, held server-side until terminal — would cost nothing and need no
// inbound connection to this host, which is the constraint that rules out
// webhooks entirely. That is a change in forge, not in its client.
const (
	pollMin = 500 * time.Millisecond
	pollMax = 1 * time.Second
)

// execPollInterval backs off geometrically from min to max.
func execPollInterval(attempt int, min, max time.Duration) time.Duration {
	d := min << attempt
	if d > max || d <= 0 {
		return max
	}
	return d
}

// pollBounds returns this client's polling window, defaulting to the package
// constants. Overridable so the test suite does not spend real seconds asleep
// proving that a backoff backs off.
func (c *CodeArmory) pollBounds() (time.Duration, time.Duration) {
	min, max := c.pollMin, c.pollMax
	if min <= 0 {
		min = pollMin
	}
	if max <= 0 {
		max = pollMax
	}
	return min, max
}

// SubmitExecution queues a command and returns immediately.
func (c *CodeArmory) SubmitExecution(ctx context.Context, spec SandboxSpec) (Execution, error) {
	// A leased command inherits its image from the sandbox it runs in, so the
	// image requirement applies only to the one-shot form.
	if spec.Image == "" && spec.LeaseID == "" {
		return Execution{}, errors.New("sandbox: image is required")
	}
	if len(spec.Command) == 0 {
		return Execution{}, errors.New("sandbox: command is required")
	}
	var out Execution
	err := c.doForge(ctx, http.MethodPost, c.forgePath("/executions"), spec, &out)
	return out, err
}

// ListImages returns the images forge will accept.
//
// forge enforces an ALLOWLIST, and a rejected image comes back as a bare
// "400: image not allowed" that names neither the image nor the alternatives.
// An agent picking its own image needs to be able to see the list, and a human
// debugging one needs it even more.
func (c *CodeArmory) ListImages(ctx context.Context) ([]string, error) {
	var out []string
	err := c.doForge(ctx, http.MethodGet, c.forgePath("/images"), nil, &out)
	return out, err
}

// RunnerClassSpec is forge's sizing record for one named runner class.
//
// Note the field names: forge calls these `memory_mb` and `cpu_millicores`. The
// `memory_limit_mb` / `cpu_limit` that appear on an EXECUTION are a different
// thing — the limit stamped onto a run after it happened — and reading the class
// through those names reports nothing and invites the conclusion that the class
// is unsized when it is merely sized under another name.
type RunnerClassSpec struct {
	Name          string `json:"name"`
	MemoryMB      int64  `json:"memory_mb"`
	CPUMillicores int64  `json:"cpu_millicores"`
	PidsLimit     int64  `json:"pids_limit"`
	TmpfsMB       int64  `json:"tmpfs_mb"`
	DiskGB        int64  `json:"disk_gb"`
	Backend       string `json:"backend"`
	Enabled       bool   `json:"enabled"`
	Privileged    bool   `json:"privileged"`
}

// EnsureRunnerClass creates or updates the runner class the developer agent's
// sandboxes run in, so the configured sizing is the sizing forge actually
// applies.
//
// blacksmith declares this rather than assuming it, because forge's seeded
// classes are sized for containers and the sandbox plane runs microVMs. Left to
// the default, a developer task compiles inside a 256 MB guest and is OOM-killed
// partway through — which surfaces as a test failure the agent then tries to fix
// in the code, burning its whole iteration budget on a problem that is not in the
// repository at all.
//
// Idempotent: PUT the class if it exists, POST it if it does not.
func (c *CodeArmory) EnsureRunnerClass(ctx context.Context, spec RunnerClassSpec) error {
	if spec.Name == "" {
		return errors.New("runner class: name is required")
	}
	path := c.forgePath("/runner-classes/" + url.PathEscape(spec.Name))
	err := c.doForge(ctx, http.MethodGet, path, nil, nil)
	switch {
	case err == nil:
		return c.doForge(ctx, http.MethodPut, path, spec, nil)
	case errors.Is(err, ErrNotFound):
		return c.doForge(ctx, http.MethodPost, c.forgePath("/runner-classes"), spec, nil)
	default:
		return err
	}
}

// GetExecution reads an execution's current state.
func (c *CodeArmory) GetExecution(ctx context.Context, id string) (Execution, error) {
	var out Execution
	err := c.doForge(ctx, http.MethodGet, c.forgePath("/executions/"+url.PathEscape(id)), nil, &out)
	return out, err
}

// CancelExecution stops a running execution.
func (c *CodeArmory) CancelExecution(ctx context.Context, id string) error {
	return c.doForge(ctx, http.MethodDelete, c.forgePath("/executions/"+url.PathEscape(id)), nil, nil)
}

// RunSandbox submits a command, waits for it to finish, and records it.
//
// On cancellation it CANCELS THE EXECUTION rather than just returning. A
// workstation is shut down often, and an abandoned sandbox holds a runner slot
// and a concurrency budget until it times out on its own — so the caller going
// away has to take the work with it.
func (c *CodeArmory) RunSandbox(ctx context.Context, rec *Recorder, spec SandboxSpec) (SandboxResult, error) {
	// A sandbox is the most expensive thing an agent does and the one most likely
	// to be the reason a stage is slow or stuck, so it gets its own span. The
	// COMMAND is deliberately not an attribute: these scripts are hundreds of
	// lines and carry base64-encoded file content, which would bury the trace.
	ctx, span := otel.Tracer("blacksmith").Start(ctx, "sandbox")
	defer span.End()
	span.SetAttributes(
		attribute.String("sandbox.image", spec.Image),
		attribute.String("sandbox.runner_class", spec.RunnerClass),
		attribute.Int64("sandbox.timeout_secs", spec.TimeoutSecs),
	)
	start := time.Now()

	exec, err := c.SubmitExecution(ctx, spec)
	if err != nil {
		rec.Action(ctx, "sandbox", summariseCommand(spec), err)
		return SandboxResult{}, fmt.Errorf("sandbox: submit: %w", err)
	}

	final, err := c.waitForExecution(ctx, exec.ExecutionID)
	if err != nil {
		rec.Action(ctx, "sandbox", summariseCommand(spec)+" ["+exec.ExecutionID+"]", err)
		return SandboxResult{ExecutionID: exec.ExecutionID}, err
	}

	res := SandboxResult{
		ExecutionID: final.ExecutionID,
		Status:      final.Status,
		Outputs:     final.Outputs,
		Duration:    time.Since(start),
	}
	if final.ExitCode != nil {
		res.ExitCode = *final.ExitCode
	}
	if final.Stdout != nil {
		res.Stdout = *final.Stdout
	}
	if final.Stderr != nil {
		res.Stderr = *final.Stderr
	}

	// A non-zero exit is recorded but is NOT an error: a failing test suite is
	// the normal case an agent must read and act on.
	rec.Action(ctx, "sandbox",
		fmt.Sprintf("%s [%s] → %s exit=%d in %s",
			summariseCommand(spec), res.ExecutionID, res.Status, res.ExitCode, res.Duration.Round(time.Millisecond)),
		nil)
	return res, nil
}

// waitForExecution polls until the execution reaches a terminal state.
func (c *CodeArmory) waitForExecution(ctx context.Context, id string) (Execution, error) {
	minWait, maxWait := c.pollBounds()
	for attempt := 0; ; attempt++ {
		select {
		case <-ctx.Done():
			return Execution{}, c.abandon(ctx, id)
		case <-time.After(execPollInterval(attempt, minWait, maxWait)):
		}

		exec, err := c.GetExecution(ctx, id)
		if err != nil {
			// Cancellation can land DURING the read as easily as during the
			// sleep, and it has to be handled the same way in both — returning
			// here without cancelling leaks exactly the execution the ctx.Done
			// branch above exists to reclaim. (Found by shrinking the poll
			// interval in tests, which made the two cases equally likely; at the
			// production interval the read window is small enough to look fine.)
			if ctx.Err() != nil {
				return Execution{}, c.abandon(ctx, id)
			}
			// A transient read failure must not abandon a running sandbox; do()
			// has already exhausted its own retries, so only give up on errors
			// that will not improve.
			if isTransient(err) {
				continue
			}
			return Execution{}, fmt.Errorf("sandbox %s: %w", id, err)
		}
		if isTerminalExec(exec.Status) {
			return exec, nil
		}
	}
}

// abandon cancels an execution the caller is no longer waiting for.
//
// It runs on a DETACHED context: ctx is already cancelled, so reusing it would
// cancel the cancellation and leave the sandbox running — which is the whole
// failure this function exists to avoid.
func (c *CodeArmory) abandon(ctx context.Context, id string) error {
	cancelCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer stop()
	if err := c.CancelExecution(cancelCtx, id); err != nil {
		return fmt.Errorf("sandbox %s: abandoned and could not cancel: %w", id, err)
	}
	return fmt.Errorf("sandbox %s: cancelled: %w", id, ctx.Err())
}

// sandboxPreamble prepares a writable working directory.
//
// Forge runs sandboxes with a READ-ONLY ROOT FILESYSTEM and a single writable
// tmpfs at /tmp — which is correct, and which breaks every tool that assumes it
// can write somewhere. `git clone` into the working dir fails with "could not
// create work tree dir: Read-only file system", and Go then fails separately
// trying to create /.cache/go-build. npm and pip fail the same way for the same
// reason, so HOME is moved too rather than patching one toolchain at a time.
//
// GOPATH is moved for the same reason and was missed the first time: GOCACHE and
// GOMODCACHE cover building and downloading, but the module CHECKSUM cache lives
// under $GOPATH/pkg/sumdb, which still pointed at the read-only /go. Everything
// ordinary kept working, and `go run some/tool@latest` — how a sandbox reaches a
// linter or a scanner it does not have — failed with "open
// /go/pkg/sumdb/sum.golang.org/latest: no such file or directory", which reads
// like a corrupt toolchain rather than a read-only mount.
//
// Found by running a real clone-and-test in the deployed sandbox; no unit test
// would have caught it, because the fake has no filesystem.
//
// COMMIT_MSG_FILE lives here too: the commit message is written to it rather than
// formatted into a `git commit -m` argument, so model-authored text never reaches
// a shell word. See commitMessageScript.
const sandboxPreamble = `set -e
export HOME=/tmp
export GOPATH=/tmp/go
export XDG_CACHE_HOME=/tmp/.cache
export GOCACHE=/tmp/.cache/go-build
export GOMODCACHE=/tmp/.cache/go-mod
export npm_config_cache=/tmp/.cache/npm
export COMMIT_MSG_FILE=/tmp/.commit-msg
mkdir -p /tmp/.cache /tmp/work
cd /tmp/work
`

// shellSingleQuote wraps a string as one POSIX single-quoted shell word.
//
// Interpolating a Go string straight into '...' is a trap that does not look
// like one: it reads correctly and works for every value until one contains an
// apostrophe. It cost a real outage here — an evidence label reading "this
// project's own code" closed the quote early, left the script with an unbalanced
// one, and made the shell exit 2 on a SYNTAX ERROR. Nothing in the script ran,
// so every security review failed with an empty diff and no output to explain
// it. The apostrophe was in prose no one thought of as code.
//
// The escape is the POSIX one: end the quote, emit an escaped apostrophe, start
// a new quote. There is no way to escape ' inside single quotes.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// summariseCommand renders a command for the transcript: bounded, and joined so
// one action is one readable line.
func summariseCommand(spec SandboxSpec) string {
	cmd := strings.Join(spec.Command, " ")
	if len([]rune(cmd)) > 200 {
		cmd = string([]rune(cmd)[:200]) + "…"
	}
	if spec.RunnerClass != "" {
		return spec.RunnerClass + ": " + cmd
	}
	return cmd
}

// ── Leases ────────────────────────────────────────────────────────────────────

// A lease is a sandbox held open across many commands.
//
// The one-shot model creates and destroys a sandbox per command, which is right
// for a caller running one command and wrong for an agent: a developer task runs
// a survey, several reads, and several verify passes against ONE checkout, and
// pays a full microVM boot and a full clone for each. Measured on the agent
// plane, that is most of a task's wall time — the commands themselves are
// seconds, the sandboxes around them are not.
//
// With a lease the agent boots one sandbox at the start of a ticket, clones into
// it once, runs every command inside it, and destroys it when the ticket leaves
// the stage. What that buys beyond the boot is the WARM CACHE: the second `go
// vet` in a lease costs a tenth of the first, because the module and build caches
// survive between commands where before every command started from empty.
//
// It is deliberately per-TICKET and never shared between them. A sandbox that
// outlived one ticket would carry whatever the last one did — a dirty tree, a
// half-applied patch, or the consequences of a prompt injection — into the next.

// LeaseSpec describes a sandbox to hold open.
type LeaseSpec struct {
	Image       string            `json:"image"`
	RunnerClass string            `json:"runner_class,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	SecretRefs  map[string]string `json:"secret_refs,omitempty"`
	// Checkout asks forge to clone the repository into the sandbox's working
	// directory ONCE, at boot, rather than per command. This is most of the point.
	Checkout *CheckoutSpec `json:"checkout,omitempty"`
	// IdleTimeout and MaxLifetime bound a lease the agent fails to release. They
	// are a backstop for a crashed agent, not the expected way a lease ends — a
	// held sandbox is reserved memory, so the agent releases explicitly.
	IdleTimeout int64 `json:"idle_timeout,omitempty"`
	MaxLifetime int64 `json:"max_lifetime,omitempty"`
}

// CheckoutSpec is forge's clone configuration for a lease.
type CheckoutSpec struct {
	// Env names the variable holding the clone URL, normally populated by a
	// SecretRef so the credential never passes through blacksmith.
	Env string `json:"env,omitempty"`
	// Ref is the branch to check out. Depth 0 means a full clone; the agent needs
	// history to branch and merge, so it is not shallow by default here.
	Ref   string `json:"ref,omitempty"`
	Depth *int   `json:"depth,omitempty"`
}

// Lease is forge's view of a held sandbox.
type Lease struct {
	LeaseID string `json:"lease_id"`
	Status  string `json:"status"`
	Detail  string `json:"detail,omitempty"`
}

// Lease statuses.
const (
	LeaseStarting = "starting"
	LeaseReady    = "ready"
	LeaseStopped  = "stopped"
	LeaseFailed   = "failed"
)

// CreateLease asks forge to boot a sandbox. It returns as soon as the lease is
// recorded; the sandbox is still starting, so callers wait with WaitLeaseReady.
func (c *CodeArmory) CreateLease(ctx context.Context, spec LeaseSpec) (Lease, error) {
	if spec.Image == "" {
		return Lease{}, errors.New("lease: image is required")
	}
	var out Lease
	err := c.doForge(ctx, http.MethodPost, c.forgePath("/leases"), spec, &out)
	return out, err
}

// GetLease reads a lease's current state.
func (c *CodeArmory) GetLease(ctx context.Context, id string) (Lease, error) {
	var out Lease
	err := c.doForge(ctx, http.MethodGet, c.forgePath("/leases/"+url.PathEscape(id)), nil, &out)
	return out, err
}

// ReleaseLease destroys the sandbox. Callers should always do this rather than
// letting the timeout collect it: every second between the last command and the
// release is a runner class's worth of memory nobody is using.
func (c *CodeArmory) ReleaseLease(ctx context.Context, id string) error {
	return c.doForge(ctx, http.MethodDelete, c.forgePath("/leases/"+url.PathEscape(id)), nil, nil)
}

// WaitLeaseReady polls until the sandbox accepts commands, fails, or the context
// ends. It reuses the execution poll bounds: a lease boots in about the time a
// short execution takes, so the same back-off is right for both.
func (c *CodeArmory) WaitLeaseReady(ctx context.Context, id string) (Lease, error) {
	minWait, maxWait := c.pollBounds()
	for attempt := 0; ; attempt++ {
		lease, err := c.GetLease(ctx, id)
		if err != nil {
			return lease, fmt.Errorf("lease: poll: %w", err)
		}
		switch lease.Status {
		case LeaseReady:
			return lease, nil
		case LeaseFailed, LeaseStopped:
			// Detail carries what forge could not do — most often a clone that
			// failed, which is worth reporting verbatim rather than as "not ready".
			return lease, fmt.Errorf("lease %s: %s", lease.Status, lease.Detail)
		}
		select {
		case <-ctx.Done():
			return lease, ctx.Err()
		case <-time.After(execPollInterval(attempt, minWait, maxWait)):
		}
	}
}
