package forge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// ReleaseTimeout bounds the release of a sandbox whose work has finished.
//
// SHORT, AND DELIBERATELY NOT THE CALLER'S CONTEXT. By the time a release runs
// the context is often already cancelled, and that is exactly when releasing
// matters most — a cancelled run is the one that would otherwise leave a runner
// class's worth of memory held until a timeout collects it.
const ReleaseTimeout = 20 * time.Second

// full is the checkout depth meaning a complete clone. Forge's default is
// shallow, and the field is a pointer precisely so "0" can be said explicitly
// rather than being indistinguishable from "unset".
var full = func() *int { d := 0; return &d }()

// SandboxSpec is what a held sandbox is booted with.
type SandboxSpec struct {
	Image       string
	RunnerClass string
	TimeoutSecs int64

	// CloneURL and SecretRef are how the sandbox gets the repository. The
	// CREDENTIAL IS NEVER HELD HERE: forge resolves the reference at dispatch, so
	// this process never sees it.
	CloneURL  string
	SecretRef string
	Branch    string

	// IdleTimeoutSecs and MaxLifetimeSecs bound a sandbox this process fails to
	// release — a crash, a kill, a host shutdown. They are a BACKSTOP FOR A
	// CRASHED AGENT, not a substitute for a stage that simply finished.
	//
	// Forge caps both whatever is asked for (see MaxIdleTimeout and MaxLifetime),
	// so a caller must size its work to what it will actually get rather than to
	// what it requested.
	IdleTimeoutSecs int64
	MaxLifetimeSecs int64
}

// Sandbox is one held container for the length of one ticket.
//
// A stage runs a survey, several reads and several verification passes against
// ONE checkout, and holding the sandbox across them removes a microVM boot and a
// clone from every command but the first — which measured as most of a task's
// wall time.
//
// IT IS ACQUIRED PER TICKET AND NEVER REUSED ACROSS THEM: a sandbox that
// outlived one ticket would carry its working tree, and anything a prompt
// injection had done in it, into the next.
type Sandbox struct {
	client  *Client
	leaseID string
	spec    Spec

	// adopt runs before every Run. See AdoptBranch.
	adopt string

	// boot is what this sandbox was acquired with, so an equivalent one can be
	// booted if forge takes this one away. See Run.
	boot SandboxSpec
}

// Acquire boots a sandbox and waits for it to accept commands.
//
// A FAILURE IS REPORTED RATHER THAN WORKED AROUND. This used to fall back to a
// container per command, which kept the ticket moving at roughly a tenth of the
// speed and told nobody — a stage that is silently ten times slower is worse
// than one that stops, because nothing points at the cause.
func (c *Client) Acquire(ctx context.Context, spec SandboxSpec) (*Sandbox, error) {
	// No image guard here: CreateLease already refuses one, and its message says
	// which call failed. A second copy would only be a second place to change.
	lease := LeaseSpec{
		Image:       spec.Image,
		RunnerClass: spec.RunnerClass,
		IdleTimeout: spec.IdleTimeoutSecs,
		MaxLifetime: spec.MaxLifetimeSecs,
	}
	if spec.CloneURL != "" {
		// FULL HISTORY, NOT SHALLOW. This one checkout serves the agent's reads and
		// its verification, and the dependency-regression check diffs the branch
		// against its base — which a single-commit clone has no base for. Paid once
		// per ticket rather than once per command, which is the point of holding
		// the sandbox at all.
		lease.Checkout = &CheckoutSpec{Env: "GIT_CLONE_URL", Ref: spec.Branch, Depth: full}

		// THE CLONE URL REACHES THE SANDBOX AS AN ENV VAR EITHER WAY: as a resolved
		// credential when there is a SecretRef, and as the plain URL for a public
		// repository. forge's checkout reads that one name in both cases.
		//
		// Sending the reference unconditionally is what broke the local plane: with
		// no AGENTS_REPO_SECRET_REF set it posted secret_refs {"GIT_CLONE_URL": ""},
		// and forge rejects an empty reference with a 400 naming the four forms it
		// accepts. Every stage that wanted a sandbox failed in the same second it
		// claimed, so a ticket burned all three attempts and was blocked before
		// anything had run.
		if spec.SecretRef != "" {
			lease.SecretRefs = map[string]string{"GIT_CLONE_URL": spec.SecretRef}
		} else {
			lease.Env = map[string]string{"GIT_CLONE_URL": spec.CloneURL}
		}
	}

	created, err := c.CreateLease(ctx, lease)
	if err != nil {
		return nil, fmt.Errorf("sandbox: create: %w", err)
	}
	ready, err := c.WaitReady(ctx, created.LeaseID)
	if err != nil {
		// The lease exists even though it never became usable, so it still has to
		// be given back.
		c.releaseQuietly(ctx, created.LeaseID)
		return nil, fmt.Errorf("sandbox: %w", err)
	}

	return &Sandbox{
		client:  c,
		leaseID: ready.LeaseID,
		// KEPT SO A REAPED LEASE CAN BE REPLACED. See Run.
		boot: spec,
		// IMAGE, RUNNER CLASS AND SECRETS ARE DELIBERATELY ABSENT. They are fixed
		// by the lease, and forge REJECTS a leased execution that sets them —
		// because they would describe a sandbox other than the one it runs in.
		spec: Spec{
			LeaseID:     ready.LeaseID,
			TimeoutSecs: spec.TimeoutSecs,
		},
	}, nil
}

// Leased reports whether this sandbox holds a container.
func (s *Sandbox) Leased() bool { return s != nil && s.leaseID != "" }

// AdoptBranch makes every subsequent Run start on this ticket's branch when the
// remote already has one.
//
// THE LEASE CLONED THE BASE BRANCH, which was right when development was the
// first stage and is wrong now: the specification author runs first and pushes
// the tests to this ticket's branch, so a developer surveying the base would not
// see the tests it is supposed to satisfy — and would then create the branch
// afresh from the base and force-push the tests away.
//
// A NO-OP WHEN THE BRANCH DOES NOT EXIST YET, which is the normal first case.
func (s *Sandbox) AdoptBranch(branch string) {
	if s == nil || branch == "" {
		return
	}
	q := Quote(branch)
	s.adopt = "git fetch -q origin " + q + " 2>/dev/null && " +
		"git checkout -q -B " + q + " FETCH_HEAD 2>/dev/null || true\n"
}

// AlsoMerge appends a script to what runs before every command — the
// integration merge, so the tree the agent surveys already holds what this
// ticket waited for.
func (s *Sandbox) AlsoMerge(script string) {
	if s == nil || script == "" {
		return
	}
	s.adopt += script
}

// Run executes a script standing in the repository's working tree.
//
// The script is written as though the repository is simply there, because by the
// time it runs it is: the lease cloned it at boot. THAT IS THE WHOLE REASON THIS
// TYPE EXISTS.
func (s *Sandbox) Run(ctx context.Context, rec Recorder, script string) (Result, error) {
	// NOT REDUNDANT, though the layer below also refuses. Without this the
	// failure arrives as "image is required", which names the wrong cause: a
	// leased command has no image BY DESIGN, so the reader is sent to look for a
	// configuration problem that does not exist.
	if !s.Leased() {
		return Result{}, errors.New("sandbox: no lease is held")
	}
	spec := s.spec
	spec.Command = []string{"sh", "-c", Preamble + s.adopt + script}
	return s.run(ctx, rec, spec)
}

// ErrLeaseGone means forge no longer has the container this sandbox was holding.
var ErrLeaseGone = errors.New("the lease is no longer running")

// leaseGone reports whether an error means the container has been taken away.
//
// MATCHED ON THE MESSAGE because forge answers with a 409 whose body names the
// state — "lease is stopped, not ready" — and a 409 alone is also how it reports
// several conditions that are NOT this one. The state word is the only thing
// that distinguishes them.
func leaseGone(err error) bool {
	if err == nil {
		return false
	}
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "lease is stopped") ||
		strings.Contains(m, "lease is expired") ||
		strings.Contains(m, "lease not found") ||
		strings.Contains(m, "unknown lease")
}

// run submits a command, replacing the container once if forge has reaped it.
//
// A SANDBOX IS DISPOSABLE AND NOTHING HERE DEPENDS ON ITS STATE. Every command
// this package runs writes what it needs first: a verification pushes the whole
// staged set and RunOnBranch resets the tree before doing anything. So a
// replacement container is not a degraded one, it is the same one again.
//
// This is not a rare edge. FORGE CAPS IDLE AT 300 SECONDS, and an agent thinks
// between commands — measured on this host: a specification author read and
// wrote for 24 minutes without touching the sandbox, by which time its lease had
// been gone for nineteen of them. The first command after that thinking failed
// the whole ticket, three attempts running, for a container nobody needed to
// still exist.
//
// ONCE, not in a loop: if the replacement is reaped just as quickly the problem
// is not a stale lease and retrying forever would hide it.
func (s *Sandbox) run(ctx context.Context, rec Recorder, spec Spec) (Result, error) {
	res, err := s.client.Run(ctx, rec, spec)
	if !leaseGone(err) {
		return res, err
	}
	if rerr := s.reacquire(ctx); rerr != nil {
		// The ORIGINAL failure is what the caller needs to see; the failure to
		// replace it is why nothing could be done about it.
		return Result{}, fmt.Errorf("%w, and a replacement could not be acquired: %w", err, rerr)
	}
	spec.LeaseID = s.leaseID
	return s.client.Run(ctx, rec, spec)
}

// reacquire boots a replacement container and points this sandbox at it.
func (s *Sandbox) reacquire(ctx context.Context) error {
	slog.WarnContext(ctx, "the sandbox lease was reaped; booting a replacement",
		"lease_id", s.leaseID)

	// Give the old one back in case forge still has a record of it. It is already
	// stopped, so this is bookkeeping rather than a release that frees anything.
	s.client.releaseQuietly(ctx, s.leaseID)
	s.leaseID = ""

	fresh, err := s.client.Acquire(ctx, s.boot)
	if err != nil {
		return err
	}
	s.leaseID = fresh.leaseID
	s.spec.LeaseID = fresh.leaseID
	return nil
}

// RunOnBranch executes a script standing in a checkout of a PUSHED branch.
//
// IT RESETS THE WORKING TREE — `reset --hard` and `clean -xfd` — so anything an
// agent wrote and did not push is gone before the script starts. That is the
// point: a verification has to run against what the branch actually holds, not
// against a working tree only this sandbox has seen. Anything an agent writes
// must therefore be committed and pushed in the same command.
func (s *Sandbox) RunOnBranch(ctx context.Context, rec Recorder, branch, script string) (Result, error) {
	if !s.Leased() {
		return Result{}, errors.New("sandbox: no lease is held")
	}
	spec := s.spec
	spec.Command = []string{"sh", "-c", Preamble + CheckoutBranchScript(branch) + script}
	return s.run(ctx, rec, spec)
}

// CheckoutBranchScript puts the working tree at the tip of a pushed branch.
//
// THE RESET AND THE CLEAN ARE LOAD-BEARING. Without them a retry inherits
// whatever the last command left behind — a half-applied edit, a build artefact,
// a file a formatter rewrote — and the verification then reports on a tree that
// exists nowhere else.
func CheckoutBranchScript(branch string) string {
	q := Quote(branch)
	return "git fetch -q origin " + q + "\n" +
		"git checkout -q -B " + q + " FETCH_HEAD\n" +
		"git reset -q --hard FETCH_HEAD\n" +
		"git clean -qxfd\n"
}

// Release gives the container back.
//
// ALWAYS CALLED, INCLUDING ON THE PANICKING PATH. A held sandbox is a runner
// class's worth of memory, and the timeouts that would eventually reclaim it are
// a backstop for a crashed agent — not a substitute for a stage that finished.
func (s *Sandbox) Release(ctx context.Context) {
	if !s.Leased() {
		return
	}
	s.client.releaseQuietly(ctx, s.leaseID)
	s.leaseID = ""
}

// releaseQuietly gives a lease back and reports a failure to the log rather than
// to the caller.
//
// THE CALLER IS ALREADY LEAVING. A release failure cannot change what the stage
// decided, and returning it would replace a real outcome with a cleanup error —
// but it must not vanish either, because a lease that would not die is a
// capacity problem someone has to see.
func (c *Client) releaseQuietly(ctx context.Context, leaseID string) {
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ReleaseTimeout)
	defer cancel()

	if err := c.ReleaseLease(releaseCtx, leaseID); err != nil {
		slog.WarnContext(ctx, "could not release a sandbox; it will be held until its timeout",
			"lease_id", leaseID, "error", err)
	}
}
