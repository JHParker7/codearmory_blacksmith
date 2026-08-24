// Package forge is the client for the sandbox plane.
//
// SANDBOXES ARE FORGE EXECUTIONS, NOT LOCAL CONTAINERS. This process never runs
// user code: it submits a command and reads the result. Everything hard about
// isolation — the microVM backend, the read-only root, the writable volumes, the
// git checkout, the egress policy — lives in forge, and this is a client for it
// and deliberately nothing more.
package forge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/code-armory-app/blacksmith/internal/transport"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

// servicePrefix is what conductor routes on.
//
// SAME TRAP AS THE TICKET STORE: an unprefixed path against the platform does
// not 404, it falls through to the portal and returns the web application's HTML
// with a 200. So the prefix is fixed by the constructor and cannot be chosen per
// call.
const servicePrefix = "/forge"

// Client talks to one forge.
type Client struct {
	http   *transport.Client
	prefix string

	// PollMin and PollMax bound the execution poll loop. Zero means the package
	// defaults; a test shrinks them so proving a backoff backs off does not cost
	// real seconds.
	PollMin time.Duration
	PollMax time.Duration
}

// Routed builds a client reaching forge THROUGH conductor — sandboxes run on the
// cluster.
//
// The stopgap, not the design. See Local.
func Routed(baseURL string, cred transport.Credential) *Client {
	return &Client{http: transport.New("forge", baseURL, cred), prefix: servicePrefix}
}

// Local builds a client for a forge on this host, reached directly.
//
// THE INTENDED DEPLOYMENT: it is what puts the sandbox on the same box as the
// model, and it is why the department's egress boundary collapses to a single
// outbound hole.
//
// The credential is separate from the platform's on purpose. The workstation
// holds an ordinary account token for the platform and a LOCAL token for its own
// sandboxes, so compromising the workstation yields the ability to run sandboxes
// on it — not a platform service key and the grants that come with one. It also
// means sandboxes keep working when the platform is unreachable, which matters on
// an intermittent host.
func Local(baseURL string, cred transport.Credential) *Client {
	return &Client{http: transport.New("forge", baseURL, cred)}
}

func (c *Client) path(suffix string) string { return c.prefix + suffix }

// Execution statuses.
//
// NOTE "completed", NOT "succeeded". Polling for the wrong word spins until the
// lease is reaped — the status says the command FINISHED, and whether it worked
// is the exit code.
const (
	StatusPending   = "pending"
	StatusRunning   = "running"
	StatusCompleted = "completed"
	StatusFailed    = "failed"
	StatusTimedOut  = "timed_out"
	StatusCancelled = "cancelled"
)

// Terminal reports whether a status will not change again.
func Terminal(status string) bool {
	switch status {
	case StatusCompleted, StatusFailed, StatusTimedOut, StatusCancelled:
		return true
	}
	return false
}

// Spec describes one command to run in isolation.
type Spec struct {
	Image       string            `json:"image"`
	Command     []string          `json:"command"`
	Env         map[string]string `json:"env,omitempty"`
	RunnerClass string            `json:"runner_class,omitempty"`
	TimeoutSecs int64             `json:"timeout,omitempty"`

	// OutputEnv names environment variables to capture after the command runs,
	// which is how a sandbox returns STRUCTURED data rather than making the agent
	// parse its own stdout.
	OutputEnv []string `json:"output_env,omitempty"`

	// SecretRefs maps an env var name to a credential reference forge resolves at
	// dispatch. The values are never sent by this process and never logged — this
	// is how a sandbox gets a git credential without the agent ever holding one.
	SecretRefs map[string]string `json:"secret_refs,omitempty"`

	// LeaseID runs the command inside a sandbox the agent ALREADY HOLDS rather
	// than creating one for it. Image, RunnerClass and SecretRefs are then fixed
	// by the lease and must be left empty — forge rejects a leased execution that
	// sets them, because they would describe a sandbox other than the one it runs
	// in.
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

// Result is a finished execution, flattened for the caller.
type Result struct {
	ExecutionID string
	Status      string
	ExitCode    int
	Stdout      string
	Stderr      string
	Outputs     map[string]string
	Duration    time.Duration
}

// OK reports whether the command both FINISHED and SUCCEEDED.
//
// Kept apart from the error return because "the sandbox ran and the tests
// failed" is a normal result an agent must reason about, not a transport
// failure. Collapsing the two would make a red test suite indistinguishable from
// an unreachable forge.
func (r Result) OK() bool { return r.Status == StatusCompleted && r.ExitCode == 0 }

// Recorder is the transcript this client writes its actions to. Nil is allowed:
// a sandbox run without a transcript still runs.
type Recorder interface {
	Action(ctx context.Context, kind, detail string, err error)
}

// Polling bounds.
//
// An agent's command is usually seconds to minutes, so the first few polls are
// quick and then back off — a tight poll for the whole duration of a long build
// is load on the control plane for no information.
//
// THE CAP IS WHAT THE PIPELINE WAITS ON, and at 5s it was tuned for a workload
// this department does not have. Because the interval doubles from the floor,
// the only instants anyone can learn an execution finished were 0.5s, 1.5s,
// 3.5s, 7.5s, 12.5s, 17.5s — and every sandbox duration recorded on r77 was one
// of exactly those six numbers, 69 of them, with nothing in between. They are
// not measurements of work; they are the moments someone looked.
//
// The waste is half the final interval per call. Measured on r77: 36 calls of 2s
// or more, 18 of them sitting on the 7.5s boundary, roughly 90s of the run's
// 351s of sandbox time spent waiting after the work was already done — about 12%
// of the run.
//
// One second keeps the backoff and moves the cap to where the executions are.
// The load it costs is bounded by the same argument that set the old value: a
// 60-second build takes 60 polls instead of 14, which is nothing against a local
// control plane.
//
// THE REAL FIX IS NOT HERE. Forge's worker blocks until the container exits, so
// it knows the instant an execution finishes and has no way to say so: the API
// is submit-then-poll. A blocking read — a wait parameter held server-side until
// terminal — would cost nothing and need no inbound connection to this host,
// which is the constraint that rules out webhooks entirely. That is a change in
// forge, not in its client.
const (
	DefaultPollMin = 500 * time.Millisecond
	DefaultPollMax = 1 * time.Second
)

func (c *Client) pollBounds() (time.Duration, time.Duration) {
	lo, hi := c.PollMin, c.PollMax
	if lo <= 0 {
		lo = DefaultPollMin
	}
	if hi <= 0 {
		hi = DefaultPollMax
	}
	return lo, hi
}

// pollInterval backs off geometrically from lo to hi.
func pollInterval(attempt int, lo, hi time.Duration) time.Duration {
	d := lo << attempt
	if d > hi || d <= 0 {
		return hi
	}
	return d
}

// Submit queues a command and returns immediately.
func (c *Client) Submit(ctx context.Context, spec Spec) (Execution, error) {
	// A leased command inherits its image from the sandbox it runs in, so the
	// image requirement applies only to the one-shot form.
	if spec.Image == "" && spec.LeaseID == "" {
		return Execution{}, errors.New("sandbox: image is required")
	}
	if len(spec.Command) == 0 {
		return Execution{}, errors.New("sandbox: command is required")
	}
	var out Execution
	err := c.http.Do(ctx, transport.Request{
		Method: http.MethodPost, Path: c.path("/executions"), Body: spec, Out: &out,
	})
	return out, err
}

// Get reads an execution's current state.
func (c *Client) Get(ctx context.Context, id string) (Execution, error) {
	var out Execution
	err := c.http.Do(ctx, transport.Request{
		Method: http.MethodGet, Path: c.path("/executions/" + url.PathEscape(id)), Out: &out,
	})
	return out, err
}

// Cancel stops a running execution.
func (c *Client) Cancel(ctx context.Context, id string) error {
	return c.http.Do(ctx, transport.Request{
		Method: http.MethodDelete, Path: c.path("/executions/" + url.PathEscape(id)),
	})
}

// Images returns the images forge will accept.
//
// Forge enforces an ALLOWLIST, and a rejected image comes back as a bare "400:
// image not allowed" naming neither the image nor the alternatives. An agent
// picking its own image needs to see the list, and a person debugging one needs
// it more.
func (c *Client) Images(ctx context.Context) ([]string, error) {
	var out []string
	err := c.http.Do(ctx, transport.Request{Method: http.MethodGet, Path: c.path("/images"), Out: &out})
	return out, err
}

// Run submits a command, waits for it to finish, and records it.
//
// ON CANCELLATION IT CANCELS THE EXECUTION rather than just returning. A
// workstation is shut down often, and an abandoned sandbox holds a runner slot
// and a concurrency budget until it times out on its own — so the caller going
// away has to take the work with it.
func (c *Client) Run(ctx context.Context, rec Recorder, spec Spec) (Result, error) {
	// A sandbox is the most expensive thing an agent does and the one most likely
	// to be why a stage is slow or stuck, so it gets its own span. THE COMMAND IS
	// DELIBERATELY NOT AN ATTRIBUTE: these scripts are hundreds of lines and carry
	// base64-encoded file content, which would bury the trace.
	ctx, span := otel.Tracer("blacksmith").Start(ctx, "sandbox")
	defer span.End()
	span.SetAttributes(
		attribute.String("sandbox.image", spec.Image),
		attribute.String("sandbox.runner_class", spec.RunnerClass),
		attribute.Int64("sandbox.timeout_secs", spec.TimeoutSecs),
	)
	start := time.Now()

	exec, err := c.Submit(ctx, spec)
	if err != nil {
		record(ctx, rec, "sandbox", Summarise(spec), err)
		return Result{}, fmt.Errorf("sandbox: submit: %w", err)
	}

	final, err := c.wait(ctx, exec.ExecutionID)
	if err != nil {
		record(ctx, rec, "sandbox", Summarise(spec)+" ["+exec.ExecutionID+"]", err)
		return Result{ExecutionID: exec.ExecutionID}, err
	}

	res := Result{
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

	// A NON-ZERO EXIT IS RECORDED BUT IS NOT AN ERROR: a failing test suite is the
	// normal case an agent must read and act on.
	record(ctx, rec, "sandbox", fmt.Sprintf("%s [%s] → %s exit=%d in %s",
		Summarise(spec), res.ExecutionID, res.Status, res.ExitCode,
		res.Duration.Round(time.Millisecond)), nil)
	return res, nil
}

func record(ctx context.Context, rec Recorder, kind, detail string, err error) {
	if rec == nil {
		return
	}
	rec.Action(ctx, kind, detail, err)
}

// wait polls until the execution reaches a terminal state.
func (c *Client) wait(ctx context.Context, id string) (Execution, error) {
	lo, hi := c.pollBounds()
	for attempt := 0; ; attempt++ {
		select {
		case <-ctx.Done():
			return Execution{}, c.abandon(ctx, id)
		case <-time.After(pollInterval(attempt, lo, hi)):
		}

		exec, err := c.Get(ctx, id)
		if err != nil {
			// CANCELLATION LANDS DURING THE READ AS EASILY AS DURING THE SLEEP, and
			// has to be handled the same way in both — returning here without
			// cancelling leaks exactly the execution the branch above exists to
			// reclaim. Found by shrinking the poll interval in tests, which made the
			// two equally likely; at the production interval the read window is small
			// enough to look fine.
			if ctx.Err() != nil {
				return Execution{}, c.abandon(ctx, id)
			}
			// A transient read failure must not abandon a RUNNING sandbox. The
			// transport has already exhausted its own retries, so only give up on
			// errors that will not improve.
			if transport.Transient(err) {
				continue
			}
			return Execution{}, fmt.Errorf("sandbox %s: %w", id, err)
		}
		if Terminal(exec.Status) {
			return exec, nil
		}
	}
}

// abandon cancels an execution the caller is no longer waiting for.
//
// It runs on a DETACHED context: ctx is already cancelled, so reusing it would
// cancel the cancellation and leave the sandbox running — which is the whole
// failure this function exists to avoid.
func (c *Client) abandon(ctx context.Context, id string) error {
	cancelCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer stop()
	if err := c.Cancel(cancelCtx, id); err != nil {
		return fmt.Errorf("sandbox %s: abandoned and could not cancel: %w", id, err)
	}
	return fmt.Errorf("sandbox %s: cancelled: %w", id, ctx.Err())
}

// Summarise renders a command for the transcript: bounded, and joined so one
// action is one readable line.
func Summarise(spec Spec) string {
	cmd := strings.Join(spec.Command, " ")
	if r := []rune(cmd); len(r) > 200 {
		cmd = string(r[:200]) + "…"
	}
	if spec.RunnerClass != "" {
		return spec.RunnerClass + ": " + cmd
	}
	return cmd
}
