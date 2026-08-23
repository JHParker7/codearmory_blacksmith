package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A Sandbox is the place one agent runs commands for one ticket.
//
// ONE HELD CONTAINER, for the whole session. It boots once, clones once, and
// every command afterwards execs into it — so the working tree, the git objects
// and the build caches all persist between the agent's turns. Every script an
// agent writes can therefore assume the same thing, "I am standing in the
// repository's working tree", without knowing anything about how it got there.
//
// There used to be a second, one-shot mode: a container per command, each
// cloning from nothing, kept as a fallback for a forge too old to hold a lease.
// It is gone. It was an order of magnitude slower, it was selected SILENTLY when
// a lease could not be had, and its re-clone-per-command semantics were the
// direct cause of the worst bug this pipeline has had — a developer's push
// recreating its branch from the base and destroying the specification tests
// written by the stage before it. Two modes that must behave identically, where
// one is rarely exercised, is a seam bugs live in.
type Sandbox struct {
	// releaseSlot gives back this host's local lease slot. See holdLeaseSlot.
	releaseSlot func()
	api         *CodeArmory
	// leaseID is the held container. Empty only on a zero value.
	leaseID string
	// spec is the template each command is submitted with: the lease id and the
	// timeout. Image, runner class and credentials belong to the lease, and forge
	// rejects a leased execution that restates them.
	spec SandboxSpec
	// prelude is prepended to every script, and makes the working directory and
	// environment the same for every command the agent writes.
	prelude string
	// cloneURL is kept for reporting and for scripts that need to name the remote.
	cloneURL string
	// adopt runs after the prelude on every command, standing the working tree on
	// a branch that already exists rather than on the base.
	//
	// Part of the prelude rather than a one-off command, and that distinction once
	// cost a whole pipeline: when a container was NOT held, every command re-cloned
	// the base, so a single checkout was discarded the moment it returned. The
	// agent then read a tree without the previous stage's work in it and its push
	// recreated the branch from the base — destroying the specification tests it
	// was supposed to satisfy. The held container makes that impossible now; this
	// stays because standing on the right branch is still the correct default, and
	// applying it uniformly costs nothing.
	adopt string
}

// AdoptBranch makes every subsequent command stand on branch, if it exists.
//
// Best effort by design: a branch that is not there yet is the normal first
// case, and the right answer then is to carry on from the base — so this never
// fails a command, it only changes where one starts.
func (s *Sandbox) AdoptBranch(branch string) {
	if s == nil || branch == "" {
		return
	}
	q := shellSingleQuote(branch)
	s.adopt = "git fetch -q origin " + q + " 2>/dev/null && " +
		"git checkout -q -B " + q + " FETCH_HEAD 2>/dev/null || true\n"
}

// AlsoMerge appends a merge to the adopted checkout, so the working tree the
// agent READS already contains it.
//
// The push path merges too, but that is not enough on its own: it happens at
// verification, and by then the agent has spent its whole turn budget looking
// for code that was not in the tree. This is what puts the dependency in front
// of it on turn one.
func (s *Sandbox) AlsoMerge(script string) {
	if s == nil || script == "" {
		return
	}
	s.adopt += script
}

// SandboxRequest is what an agent needs a sandbox for.
type SandboxRequest struct {
	Image       string
	RunnerClass string
	TimeoutSecs int64
	// CloneURL and SecretRef locate the repository. SecretRef is a forge
	// credential reference; its value is resolved by forge and never held here.
	CloneURL  string
	SecretRef string
	Branch    string
	// IdleTimeoutSecs and MaxLifetimeSecs bound a lease this agent fails to
	// release. They are the backstop for a crashed agent; the normal end of a
	// lease is an explicit Release.
	IdleTimeoutSecs int64
	MaxLifetimeSecs int64
}

// leasedPrelude is prepended to every command in a held sandbox.
//
// It is short because the lease already did the work: forge set the environment
// when the container started and put the working tree at the working directory's
// root, so a command arrives already standing in the repository. Only the cache
// directory needs creating, because the environment names paths under /tmp that
// nothing has made yet.
const leasedPrelude = `set -e
mkdir -p /tmp/.cache
`

// sandboxEnv is the environment a repository's commands need. It is set on the
// lease, so every command inherits it from the container.
//
// It must agree with sandboxPreamble, which exports the same names for the
// single-command sandboxes the integrator and the architect run: a build that
// wrote its cache somewhere different depending on which of the two ran it would
// produce results that differ by caller, which is the worst kind of bug to chase.
func sandboxEnv() map[string]string {
	return map[string]string{
		"HOME":             "/tmp",
		"GOPATH":           "/tmp/go",
		"XDG_CACHE_HOME":   "/tmp/.cache",
		"GOCACHE":          "/tmp/.cache/go-build",
		"GOMODCACHE":       "/tmp/.cache/go-mod",
		"npm_config_cache": "/tmp/.cache/npm",
		"COMMIT_MSG_FILE":  "/tmp/.commit-msg",
	}
}

// delegateEnv is what an EXTERNAL coding agent needs to work inside the sandbox.
//
// A delegated developer talks to the model itself, from inside the lease, which
// the native loop never does — blacksmith calls the model from the host and the
// sandbox only runs commands. So the endpoint, the key and the model name have to
// travel with the lease, and without them the agent exits in seconds having done
// nothing: measured as four "delegated agent left the tests failing" outcomes,
// fifteen seconds apart, on a run where the agent never actually started.
//
// The endpoint is the SANDBOX's route to the model, not the host's. It must be an
// address the cluster can reach — the bridge, not loopback — which is also why
// the NetworkPolicy names that host and port explicitly.
func delegateEnv() map[string]string {
	env := map[string]string{}
	for k, v := range map[string]string{
		"OPENAI_BASE_URL": os.Getenv("AGENTS_LARGE_ENDPOINT"),
		"OPENAI_MODEL":    os.Getenv("AGENTS_LARGE_MODEL"),
		// Everything that is not the model goes through the proxy; direct egress is
		// refused, so an agent fetching a dependency needs to be told the way out.
		"HTTP_PROXY":  os.Getenv("AGENTS_SANDBOX_PROXY"),
		"HTTPS_PROXY": os.Getenv("AGENTS_SANDBOX_PROXY"),
		"NO_PROXY":    "192.168.58.1,git-local,.svc,.cluster.local,localhost,127.0.0.1",
	} {
		if v != "" {
			env[k] = v
		}
	}
	if key := secretFile(os.Getenv("AGENTS_LARGE_API_KEY_FILE")); key != "" {
		env["OPENAI_API_KEY"] = key
	}
	return env
}

// secretFile reads a secret from a path, empty when unset or unreadable. The key
// is read here rather than passed as configuration so it stays out of the process
// environment and the transcripts.
func secretFile(path string) string {
	if path == "" {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// AcquireSandbox holds one container for a ticket's whole session.
//
// A LEASE IS REQUIRED, and the fallback that used to be here is gone. It
// returned a one-shot sandbox — one container, and a full clone, PER COMMAND —
// and said so at Info level, which on this host reaches no log anyone reads. The
// result was a silent order-of-magnitude slowdown that looked exactly like a
// slow model: measured on a live run against a forge with no /leases route, one
// ticket spent 23 minutes on five verification cycles, every read and write
// paying for a fresh container and a fresh clone.
//
// A degradation nobody can see is worse than a failure. Reliability was the
// argument for the fallback, and it was the wrong trade: the ticket still gets
// worked, just far too slowly to notice something is broken. Failing here names
// the real problem — a forge too old to hold a sandbox — on the ticket, in front
// of a person, in seconds rather than after a wasted afternoon.
func (c *CodeArmory) AcquireSandbox(ctx context.Context, req SandboxRequest) (*Sandbox, error) {
	if c == nil {
		return nil, errors.New("sandbox: no platform client")
	}

	spec := LeaseSpec{
		Image:       req.Image,
		RunnerClass: req.RunnerClass,
		Env:         withDelegateEnv(sandboxEnv()),
		IdleTimeout: req.IdleTimeoutSecs,
		MaxLifetime: req.MaxLifetimeSecs,
		// Full history, not shallow. This ONE checkout serves both the agent's
		// reads and its verification, and the dependency-regression check diffs the
		// branch against its base — which a single-commit clone has no base for.
		// Paid once per ticket rather than once per command, which is the whole
		// point of holding the container.
		Checkout: &CheckoutSpec{Env: "GIT_CLONE_URL", Ref: req.Branch, Depth: ptrInt(0)},
	}
	// The clone URL reaches the sandbox as an env var either way: as a resolved
	// credential when there is a SecretRef, and as the plain URL for a public
	// repository. forge's checkout reads that one name in both cases.
	if req.SecretRef != "" {
		spec.SecretRefs = map[string]string{"GIT_CLONE_URL": req.SecretRef}
	} else {
		spec.Env["GIT_CLONE_URL"] = req.CloneURL
	}

	// WAIT FOR A LEASE SLOT RATHER THAN FAIL THE TICKET. forge caps how many
	// leases one user may hold at once — six here, which is not arbitrary: the
	// agent-dev runner class reserves 8GB, so six of them is 48GB of a 64GB node.
	// Blacksmith runs more agents than that concurrently, so the seventh was told
	// "you already hold 6 leases (the limit); release one first" and its ticket
	// was failed outright. Two tickets died that way in one run, seconds after
	// being scoped.
	//
	// A cap that the plane enforces is one the caller should respect, not
	// discover. Queueing here turns a lost ticket into a short wait, and the wait
	// is genuinely short because a lease lives for one ticket's stage.
	release, err := c.holdLeaseSlot(ctx)
	if err != nil {
		return nil, err
	}

	lease, err := c.CreateLease(ctx, spec)
	if err != nil {
		release()
		// The most likely cause by far, and the one worth naming: a forge without
		// the /leases route answers 404, which reads as an ordinary request failure
		// and gives no hint that the SERVER is too old.
		return nil, fmt.Errorf("sandbox: could not hold a container — this forge must support leases: %w", err)
	}
	if _, err := c.WaitLeaseReady(ctx, lease.LeaseID); err != nil {
		// Release rather than leaving it: a lease stuck starting still holds a
		// runner class's memory until the reaper notices, and this agent is about
		// to stop referring to it.
		c.releaseQuietly(ctx, lease.LeaseID)
		release()
		return nil, fmt.Errorf("sandbox: the held container never became ready: %w", err)
	}

	slog.InfoContext(ctx, "holding a sandbox for this ticket", "lease_id", lease.LeaseID)
	return &Sandbox{
		api:         c,
		releaseSlot: release,
		leaseID:     lease.LeaseID,
		spec:        SandboxSpec{LeaseID: lease.LeaseID, TimeoutSecs: req.TimeoutSecs},
		prelude:     leasedPrelude,
		cloneURL:    req.CloneURL,
	}, nil
}

// Leased reports whether commands run in a held sandbox. Used for reporting and
// by tests; agents do not branch on it.
func (s *Sandbox) Leased() bool { return s != nil && s.leaseID != "" }

// Run executes a script standing in the repository's working tree.
//
// The script is written as though the repository is simply there, because by the
// time it runs it is: the lease cloned it at boot. That is the whole reason this
// type exists.
func (s *Sandbox) Run(ctx context.Context, rec *Recorder, script string) (SandboxResult, error) {
	spec := s.spec
	spec.Command = []string{"sh", "-c", s.prelude + s.adopt + script}
	return s.api.RunSandbox(ctx, rec, spec)
}

// RunOnBranch executes a script standing in a checkout of a PUSHED branch.
//
// This is deliberately not the same as Run. Verification must judge what was
// pushed — commit hooks rewrite files, .gitignore silently drops a new file, and
// a formatter changes what actually landed — so it has to read the branch back
// from the remote rather than trust the tree the agent has been editing. Both
// modes therefore re-fetch; they differ only in how much they must transfer.
//
// The result is byte-identical to a fresh clone of the branch in both modes: the
// leased path hard-resets and cleans, so nothing the agent left behind in the
// working tree can influence the verdict.
func (s *Sandbox) RunOnBranch(ctx context.Context, rec *Recorder, branch, script string) (SandboxResult, error) {
	spec := s.spec
	spec.Command = []string{"sh", "-c", s.prelude + s.checkoutBranch(branch) + script}
	return s.api.RunSandbox(ctx, rec, spec)
}

// checkoutBranch puts the working tree at the tip of a pushed branch.
//
// The objects are already local — that is what the held container buys — so this
// is a fetch of one branch rather than a clone of the repository. reset --hard
// and clean are what make it equivalent to a fresh checkout rather than merely
// "the right commit": nothing the agent left in the working tree can influence
// the verdict.
func (s *Sandbox) checkoutBranch(branch string) string {
	q := shellSingleQuote(branch)
	return "git fetch -q origin " + q + "\n" +
		"git checkout -q -B " + q + " FETCH_HEAD\n" +
		"git reset -q --hard FETCH_HEAD\n" +
		"git clean -qxfd\n"
}

// Release destroys the held sandbox. Safe on a zero value and safe to call
// twice, so callers can defer it unconditionally.
//
// It takes its own context on purpose: the caller's is usually already cancelled
// by the time this runs — that is what ended the ticket — and releasing on a
// dead context would leak the very sandbox this exists to reclaim.
func (s *Sandbox) Release(ctx context.Context) {
	if !s.Leased() {
		return
	}
	s.api.releaseQuietly(ctx, s.leaseID)
	s.leaseID = ""
	// The local slot is given back only after forge has been told, so the next
	// agent through the queue does not race the release it is waiting on.
	if s.releaseSlot != nil {
		s.releaseSlot()
		s.releaseSlot = nil
	}
}

// releaseQuietly tears a lease down, logging rather than returning a failure.
// Nothing a caller can do about it differs from what the reaper will do anyway,
// and a release failure must not mask the outcome of the work the lease held.
func (c *CodeArmory) releaseQuietly(ctx context.Context, leaseID string) {
	if err := c.ReleaseLease(ctx, leaseID); err != nil {
		slog.WarnContext(ctx, "could not release the held sandbox; the reaper will collect it",
			"lease_id", leaseID, "error", err)
		return
	}
	slog.InfoContext(ctx, "released the held sandbox", "lease_id", leaseID)
}

func ptrInt(v int) *int { return &v }

// leaseReleaseTimeout bounds the release call an agent makes on its way out.
// Short on purpose: the reaper will collect the sandbox anyway, so a release
// that cannot complete promptly must not hold up the stage reporting its result.
const leaseReleaseTimeout = 15 * time.Second

// maxHeldLeases is how many sandboxes this host will hold at once.
//
// It MIRRORS A LIMIT THE PLANE ALREADY ENFORCES. forge refuses a seventh lease
// per user with 429 "you already hold 6 leases (the limit); release one first",
// and that number is a memory budget rather than a policy: the agent-dev runner
// class reserves 8GB, so six leases is 48GB of a 64GB node. Exceeding it does
// not queue, it fails — and it failed two tickets seconds after they were
// scoped, which is the whole reason this exists.
//
// Configurable because the limit belongs to the forge, not to blacksmith, and a
// different deployment will have a different one.
func maxHeldLeases() int {
	if v := os.Getenv("AGENTS_MAX_LEASES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 6
}

// holdLeaseSlot blocks until this CLIENT may hold another sandbox, and returns the
// function that gives the slot back.
//
// The returned release is idempotent, because the paths that call it are the
// error paths — a lease that failed to create and one that never became ready
// both unwind through here, and double-returning a slot would let the cap drift
// upward until the 429 came back.
func (c *CodeArmory) holdLeaseSlot(ctx context.Context) (func(), error) {
	c.leaseSlotsOnce.Do(func() { c.leaseSlots = make(chan struct{}, maxHeldLeases()) })
	select {
	case c.leaseSlots <- struct{}{}:
	case <-ctx.Done():
		return nil, fmt.Errorf("waiting for a sandbox slot: %w", ctx.Err())
	}
	var once sync.Once
	return func() { once.Do(func() { <-c.leaseSlots }) }, nil
}

// withDelegateEnv adds the external agent's environment when one is configured,
// and changes nothing otherwise.
func withDelegateEnv(env map[string]string) map[string]string {
	if strings.TrimSpace(os.Getenv("AGENTS_DEV_ENGINE")) == "" {
		return env
	}
	for k, v := range delegateEnv() {
		env[k] = v
	}
	return env
}
