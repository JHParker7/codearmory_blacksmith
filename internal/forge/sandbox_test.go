package forge

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func acquire(t *testing.T, f *fakeForge, c *Client) *Sandbox {
	t.Helper()
	sb, err := c.Acquire(context.Background(), SandboxSpec{
		Image: "golang:1.25", RunnerClass: "agent-dev", TimeoutSecs: 600,
		CloneURL: "https://git.example/org/repo", SecretRef: "git:repo", Branch: "main",
		IdleTimeoutSecs: 300, MaxLifetimeSecs: 3600,
	})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	return sb
}

func lastCommand(t *testing.T, f *fakeForge) string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.submitted) == 0 {
		t.Fatal("nothing was executed")
	}
	return strings.Join(f.submitted[len(f.submitted)-1].Command, " ")
}

// IMAGE, RUNNER CLASS AND SECRETS ARE FIXED BY THE LEASE, and forge REJECTS a
// leased execution that sets them — they would describe a sandbox other than the
// one it runs in.
func TestALeasedExecutionDescribesOnlyTheCommand(t *testing.T) {
	f, c := newFakeForge(t)
	sb := acquire(t, f, c)

	if _, err := sb.Run(context.Background(), nil, "echo hello\n"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	spec := f.submitted[len(f.submitted)-1]
	if spec.LeaseID != "l-1" {
		t.Errorf("the command was not sent to the held sandbox: %q", spec.LeaseID)
	}
	if spec.Image != "" || spec.RunnerClass != "" || len(spec.SecretRefs) != 0 {
		t.Errorf("a leased execution described a sandbox, which forge rejects: "+
			"image=%q class=%q secrets=%v", spec.Image, spec.RunnerClass, spec.SecretRefs)
	}
}

// THE CREDENTIAL NEVER PASSES THROUGH THIS PROCESS: the lease carries a
// REFERENCE that forge resolves at dispatch.
func TestTheLeaseCarriesACredentialReferenceRatherThanACredential(t *testing.T) {
	f, c := newFakeForge(t)
	acquire(t, f, c)

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.created) != 1 {
		t.Fatalf("%d leases were created", len(f.created))
	}
	lease := f.created[0]
	if lease.Checkout == nil || lease.Checkout.Ref != "main" {
		t.Errorf("the lease does not clone the base branch: %+v", lease.Checkout)
	}
	if lease.SecretRefs["GIT_CLONE_URL"] != "git:repo" {
		t.Errorf("the clone URL is not a secret reference: %v", lease.SecretRefs)
	}
	// The sizing the caller asked for reaches forge, which then caps it.
	if lease.IdleTimeout != 300 || lease.MaxLifetime != 3600 {
		t.Errorf("the lease bounds were not sent: %+v", lease)
	}
	if lease.Image != "golang:1.25" {
		t.Errorf("the lease was booted with %q", lease.Image)
	}
}

// A PUBLIC REPOSITORY HAS NO CREDENTIAL TO REFERENCE, and forge rejects an
// empty one.
//
// Sending secret_refs {"GIT_CLONE_URL": ""} earns a 400 naming the four forms
// forge accepts. On the local plane, where no AGENTS_REPO_SECRET_REF is set,
// that failed every stage that wanted a sandbox in the same second it claimed —
// so a ticket burned all three attempts and was blocked before anything ran.
// The clone URL goes as a plain env var instead; forge's checkout reads the same
// name either way.
func TestAPublicRepositoryIsClonedWithoutASecretReference(t *testing.T) {
	f, c := newFakeForge(t)
	_, err := c.Acquire(context.Background(), SandboxSpec{
		Image:    "golang:1.25",
		CloneURL: "git://git-local:9418/demo.git",
		Branch:   "main",
	})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	lease := f.created[0]
	if _, ok := lease.SecretRefs["GIT_CLONE_URL"]; ok {
		t.Errorf("an empty credential reference was sent: %v — forge rejects it "+
			"with a 400 and the ticket dies before it starts", lease.SecretRefs)
	}
	if lease.Env["GIT_CLONE_URL"] != "git://git-local:9418/demo.git" {
		t.Errorf("the clone URL did not reach the sandbox: %v", lease.Env)
	}
	// AND IT JOINS THE ENVIRONMENT RATHER THAN REPLACING IT. Assigning a fresh
	// map here drops everything the toolchain needs, and only on the path with no
	// credential — which is the local plane, where it would never be noticed by a
	// test that always supplies one.
	if lease.Env["HOME"] == "" || lease.Env["GOCACHE"] == "" {
		t.Errorf("the clone URL replaced the toolchain environment: %v", lease.Env)
	}
	if lease.Checkout == nil || lease.Checkout.Env != "GIT_CLONE_URL" {
		t.Errorf("the checkout does not read the same name: %+v", lease.Checkout)
	}
}

// AND A PRIVATE ONE STILL SENDS NO URL IN THE CLEAR. The two paths are mutually
// exclusive: a credential resolved by forge must not be shadowed by a plain
// value this process put beside it.
func TestACredentialledRepositorySendsNoPlainURL(t *testing.T) {
	f, c := newFakeForge(t)
	acquire(t, f, c)

	f.mu.Lock()
	defer f.mu.Unlock()
	lease := f.created[0]
	if lease.SecretRefs["GIT_CLONE_URL"] != "git:repo" {
		t.Errorf("the reference was not sent: %v", lease.SecretRefs)
	}
	if _, ok := lease.Env["GIT_CLONE_URL"]; ok {
		t.Errorf("a plain clone URL was sent alongside the reference: %v — it would "+
			"shadow the credential forge resolves", lease.Env)
	}
}

// FULL HISTORY, NOT SHALLOW. One checkout serves the agent's reads and its
// verification, and the dependency-regression check diffs the branch against its
// base — which a single-commit clone has no base for.
func TestTheCheckoutAsksForFullHistory(t *testing.T) {
	f, c := newFakeForge(t)
	acquire(t, f, c)

	f.mu.Lock()
	defer f.mu.Unlock()
	co := f.created[0].Checkout
	if co == nil || co.Depth == nil {
		t.Fatalf("no depth was asked for: %+v — forge's default is shallow, and "+
			"unset is indistinguishable from it", co)
	}
	if *co.Depth != 0 {
		t.Errorf("depth = %d, want 0 (a complete clone)", *co.Depth)
	}
}

// A REPOSITORY IS NOT ALWAYS WANTED: a sandbox used for a one-off command has
// nothing to clone, and asking forge to check out an empty URL fails the boot.
func TestASandboxWithNoRepositoryClonesNothing(t *testing.T) {
	f, c := newFakeForge(t)
	if _, err := c.Acquire(context.Background(), SandboxSpec{Image: "alpine"}); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.created[0].Checkout != nil {
		t.Errorf("a sandbox with no repository was given a checkout: %+v", f.created[0].Checkout)
	}
}

// EVERY SCRIPT STARTS WITH THE PREAMBLE. Forge runs sandboxes with a read-only
// root and one writable tmpfs, so a script assuming it can write anywhere fails
// on "Read-only file system" in a way that reads like a broken toolchain.
func TestALeasedCommandRunsInTheCheckoutRatherThanElsewhere(t *testing.T) {
	f, c := newFakeForge(t)
	sb := acquire(t, f, c)

	if _, err := sb.Run(context.Background(), nil, "echo hello\n"); err != nil {
		t.Fatal(err)
	}
	got := lastCommand(t, f)
	if !strings.Contains(got, "set -e") {
		t.Errorf("the command does not carry the leased prelude:\n%s", got)
	}
	// A LEASE ALREADY HOLDS A CHECKOUT, cloned by forge at boot into the
	// container's own working directory. Moving anywhere else runs every command
	// outside the repository.
	//
	// Measured by booting a real sandbox: the checkout is at /workspace, the
	// one-off preamble moved to an empty /tmp/work, and `git status` answered
	// "fatal: not a git repository". Every verification failed, and the agent read
	// and rewrote the same files to the 200-turn ceiling with nothing on the
	// ticket to say why — which reads exactly like a model that will not converge.
	if strings.Contains(got, "cd ") {
		t.Errorf("a leased command changes directory, so it runs outside the "+
			"checkout forge made for it:\n%s", got)
	}

	if _, err := sb.RunOnBranch(context.Background(), nil, "agent/t-1", "echo hi\n"); err != nil {
		t.Fatal(err)
	}
	if got := lastCommand(t, f); strings.Contains(got, "cd ") {
		t.Errorf("a branch command changes directory:\n%s", got)
	}
}

// THE ENVIRONMENT COMES FROM THE LEASE, since the leased prelude exports
// nothing. Without it the toolchain fails in several unrelated-looking ways
// against a read-only root — see Preamble.
func TestTheLeaseCarriesTheEnvironmentTheToolchainNeeds(t *testing.T) {
	f, c := newFakeForge(t)
	acquire(t, f, c)

	f.mu.Lock()
	defer f.mu.Unlock()
	env := f.created[0].Env
	for _, name := range []string{"HOME", "GOCACHE", "GOMODCACHE", "GOPATH",
		"XDG_CACHE_HOME", "COMMIT_MSG_FILE"} {
		if env[name] == "" {
			t.Errorf("the lease does not set %s; a leased command exports nothing "+
				"itself, so nothing else will", name)
		}
	}
}

// THE TWO PRELUDES MUST NOT DRIFT. They set the same names for opposite
// situations, and a build whose cache moved depending on which ran it would
// produce results that differ by caller.
func TestTheLeaseEnvironmentAgreesWithTheOneOffPreamble(t *testing.T) {
	for name, value := range SandboxEnv() {
		want := "export " + name + "=" + value
		if !strings.Contains(Preamble, want) {
			t.Errorf("Preamble does not carry %q, so a one-off execution and a "+
				"leased one disagree about where %s points", want, name)
		}
	}
}

// THE LEASE CLONED THE BASE BRANCH, which is the right thing to clone and the
// wrong place to start work: the specification author runs first and pushes the
// tests to this ticket's branch, so a developer surveying the base would not see
// the tests it is supposed to satisfy — and would then create the branch afresh
// and force-push them away.
func TestAdoptingABranchPutsEveryCommandOnIt(t *testing.T) {
	f, c := newFakeForge(t)
	sb := acquire(t, f, c)
	sb.AdoptBranch("agent/t-1")

	if _, err := sb.Run(context.Background(), nil, "echo hello\n"); err != nil {
		t.Fatal(err)
	}
	got := lastCommand(t, f)

	if !strings.Contains(got, "git checkout -q -B 'agent/t-1' FETCH_HEAD") {
		t.Errorf("the command does not adopt the branch:\n%s", got)
	}
	// A NO-OP WHEN THE BRANCH DOES NOT EXIST YET, which is the normal first case:
	// under `set -e` the adopt must not be able to fail the command.
	if !strings.Contains(got, "|| true") {
		t.Errorf("a missing branch would fail the command:\n%s", got)
	}
	// The adopt comes BEFORE the script, or the script runs against the base.
	if strings.Index(got, "git checkout") > strings.Index(got, "echo hello") {
		t.Errorf("the script runs before the branch is adopted:\n%s", got)
	}
}

// AND ITS DEPENDENCIES. Scheduling held this ticket until its blockers merged;
// without the merge the branch still predates them.
func TestAMergeIsAppendedToTheAdoptRatherThanReplacingIt(t *testing.T) {
	f, c := newFakeForge(t)
	sb := acquire(t, f, c)
	sb.AdoptBranch("agent/t-1")
	sb.AlsoMerge("git merge -q --no-edit FETCH_HEAD || true\n")

	if _, err := sb.Run(context.Background(), nil, "echo hello\n"); err != nil {
		t.Fatal(err)
	}
	got := lastCommand(t, f)

	if !strings.Contains(got, "agent/t-1") {
		t.Errorf("the merge replaced the branch adoption:\n%s", got)
	}
	if strings.Index(got, "git checkout") > strings.Index(got, "git merge") {
		t.Errorf("the merge runs before the branch is adopted:\n%s", got)
	}

	// Empty values change nothing rather than emitting a stray line.
	_, c2 := newFakeForge(t)
	sb2, err := c2.Acquire(context.Background(), SandboxSpec{Image: "alpine"})
	if err != nil {
		t.Fatal(err)
	}
	sb2.AdoptBranch("")
	sb2.AlsoMerge("")
	if sb2.adopt != "" {
		t.Errorf("an empty adopt produced %q", sb2.adopt)
	}
}

// RunOnBranch RESETS THE WORKING TREE, so anything an agent wrote and did not
// push is gone before the script starts. A verification has to run against what
// the branch actually holds, not against a tree only this sandbox has seen.
func TestRunningOnABranchDiscardsWhateverTheSandboxHeld(t *testing.T) {
	f, c := newFakeForge(t)
	sb := acquire(t, f, c)
	// Even with a branch adopted, a branch run does not carry the adopt: it is
	// checking what was PUSHED.
	sb.AdoptBranch("agent/t-1")

	if _, err := sb.RunOnBranch(context.Background(), nil, "agent/t-1", "go test ./...\n"); err != nil {
		t.Fatal(err)
	}
	got := lastCommand(t, f)

	for _, want := range []string{
		"git fetch -q origin 'agent/t-1'",
		"git reset -q --hard FETCH_HEAD",
		"git clean -qxfd",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("a branch run does not %q:\n%s", want, got)
		}
	}
	if strings.Index(got, "git clean") > strings.Index(got, "go test") {
		t.Errorf("the tree is reset after the script rather than before:\n%s", got)
	}
	// IT MUST NOT TOLERATE A FAILED CHECKOUT: verifying the wrong tree is worse
	// than not verifying.
	if strings.Contains(got, "|| true") {
		t.Errorf("a branch run tolerates a failed checkout:\n%s", got)
	}
}

func TestABranchNameIsQuotedWhereverItReachesTheShell(t *testing.T) {
	for _, branch := range []string{"agent/t-1", "agent/it's-odd"} {
		if got := CheckoutBranchScript(branch); !strings.Contains(got, Quote(branch)) {
			t.Errorf("branch %q was not quoted: %s", branch, got)
		}
		_, c := newFakeForge(t)
		sb, err := c.Acquire(context.Background(), SandboxSpec{Image: "alpine"})
		if err != nil {
			t.Fatal(err)
		}
		sb.AdoptBranch(branch)
		if !strings.Contains(sb.adopt, Quote(branch)) {
			t.Errorf("branch %q was not quoted in the adopt: %s", branch, sb.adopt)
		}
	}
}

// AN IMAGE IS REQUIRED, and forge refuses an execution whose image is not on the
// allowlist with a bare 400 — so a missing one is caught here, where it can say
// what is wrong.
func TestASandboxWithNoImageIsRefused(t *testing.T) {
	_, c := newFakeForge(t)
	_, err := c.Acquire(context.Background(), SandboxSpec{})
	if err == nil {
		t.Fatal("a sandbox with no image was acquired")
	}
	// AND IT SAYS WHICH CALL FAILED, which is why there is no second copy of the
	// check in Acquire: one guard, one message.
	if !strings.Contains(err.Error(), "image is required") {
		t.Errorf("the refusal does not name the cause: %v", err)
	}
}

// A LEASE THAT NEVER BECAME USABLE STILL EXISTS, so it still has to be given
// back — otherwise a failed boot holds a runner class until its timeout.
func TestALeaseThatNeverBecomesReadyIsStillReleased(t *testing.T) {
	f, c := newFakeForge(t)
	f.leaseSteps = []string{LeaseFailed}
	f.leaseDetail = "the image is not on the allowlist"

	if _, err := c.Acquire(context.Background(), SandboxSpec{Image: "golang:1.25"}); err == nil {
		t.Fatal("a failed lease was reported as acquired")
	} else if !strings.Contains(err.Error(), "allowlist") {
		t.Errorf("the failure does not carry forge's reason: %v", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	var deletes int
	for _, p := range f.paths {
		if strings.HasPrefix(p, "DELETE /leases/") {
			deletes++
		}
	}
	if deletes != 1 {
		t.Errorf("a failed lease was released %d times: %v", deletes, f.paths)
	}
}

// A SANDBOX BOOTS ASYNCHRONOUSLY: the create returns as soon as the lease is
// recorded, so the caller has to wait for it to accept commands.
func TestAcquireWaitsForTheSandboxToAcceptCommands(t *testing.T) {
	f, c := newFakeForge(t)
	f.leaseSteps = []string{LeaseStarting, LeaseStarting, LeaseReady}

	sb := acquire(t, f, c)
	if !sb.Leased() {
		t.Error("Acquire returned a sandbox holding nothing")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.leasePolls < len(f.leaseSteps) {
		t.Errorf("polled %d times for %d states; the sandbox was used before it was ready",
			f.leasePolls, len(f.leaseSteps))
	}
}

// A HELD SANDBOX IS A RUNNER CLASS'S WORTH OF MEMORY, and the timeouts that
// would reclaim it are a backstop for a crashed agent, not a substitute for a
// stage that finished.
func TestReleasingGivesTheContainerBackAndIsIdempotent(t *testing.T) {
	f, c := newFakeForge(t)
	sb := acquire(t, f, c)

	sb.Release(context.Background())
	sb.Release(context.Background())

	f.mu.Lock()
	defer f.mu.Unlock()
	var deletes int
	for _, p := range f.paths {
		if strings.HasPrefix(p, "DELETE /leases/") {
			deletes++
		}
	}
	if deletes != 1 {
		t.Errorf("released %d times, want exactly one: %v", deletes, f.paths)
	}
	if sb.Leased() {
		t.Error("a released sandbox still reports a lease")
	}
}

// THE RELEASE DOES NOT USE THE CALLER'S CONTEXT. By the time it runs the context
// is often already cancelled, and that is exactly when releasing matters most.
func TestACancelledContextStillReleases(t *testing.T) {
	f, c := newFakeForge(t)
	sb := acquire(t, f, c)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sb.Release(ctx)

	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.paths {
		if strings.HasPrefix(p, "DELETE /leases/") {
			return
		}
	}
	t.Errorf("a cancelled context skipped the release: %v", f.paths)
}

// A COMMAND WITHOUT A LEASE IS A PROGRAMMING ERROR, and saying so beats sending
// forge an execution that describes no sandbox at all.
func TestRunningWithoutALeaseIsRefused(t *testing.T) {
	var sb Sandbox
	// THE MESSAGE IS THE POINT, not merely that it failed. The layer below also
	// refuses, with "image is required" — which names the wrong cause, because a
	// leased command has no image by design.
	for _, run := range []func() error{
		func() error { _, err := sb.Run(context.Background(), nil, "echo hi"); return err },
		func() error { _, err := sb.RunOnBranch(context.Background(), nil, "b", "echo hi"); return err },
	} {
		err := run()
		if err == nil {
			t.Fatal("a command ran with no lease held")
		}
		if !strings.Contains(err.Error(), "no lease is held") {
			t.Errorf("the failure names the wrong cause: %v", err)
		}
	}

	// A NIL SANDBOX ANSWERS RATHER THAN PANICKING: the release runs on a deferred
	// path that may never have been given one.
	var nilbox *Sandbox
	if nilbox.Leased() {
		t.Error("a nil sandbox reported a lease")
	}
	nilbox.AdoptBranch("x")
	nilbox.AlsoMerge("y")
	nilbox.Release(context.Background())
}

// A REAPED LEASE IS RECOVERED FROM, NOT DIED ON.
//
// Forge caps idle at 300 seconds and an agent thinks between commands. Measured
// on this host: a specification author read and wrote for 24 minutes without
// touching its sandbox, and the first command after that thinking failed the
// whole ticket — three attempts running — for a container nobody needed to still
// exist. Nothing in this package depends on sandbox state: a verification pushes
// the whole staged set, and RunOnBranch resets the tree first.
func TestACommandSurvivesTheLeaseBeingReaped(t *testing.T) {
	f, c := newFakeForge(t)
	sb := acquire(t, f, c)

	f.mu.Lock()
	first := sb.leaseID
	f.failNextRun = "conflict: lease is stopped, not ready"
	f.mu.Unlock()

	res, err := sb.Run(context.Background(), nil, "go test ./...")
	if err != nil {
		t.Fatalf("Run: %v — a reaped lease must be replaced, not fatal", err)
	}
	if !res.OK() {
		t.Fatalf("the retried command did not succeed: %+v", res)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if sb.leaseID == first {
		t.Error("the sandbox kept the dead lease")
	}
	if len(f.created) != 2 {
		t.Errorf("%d leases were created, want a replacement to have been booted", len(f.created))
	}
	// THE RETRY GOES TO THE NEW CONTAINER. Re-submitting against the dead lease
	// id earns the same rejection, so the replacement would buy nothing.
	last := f.submitted[len(f.submitted)-1]
	if last.LeaseID != sb.leaseID {
		t.Errorf("the retry was submitted against lease %q, want the replacement %q",
			last.LeaseID, sb.leaseID)
	}
	if last.LeaseID == first {
		t.Errorf("the retry reused the dead lease %q", first)
	}
	// THE REPLACEMENT IS THE SAME SANDBOX AGAIN, or the retry runs somewhere
	// subtly different from where the work started.
	if f.created[1].Image != f.created[0].Image {
		t.Errorf("the replacement used image %q, want %q",
			f.created[1].Image, f.created[0].Image)
	}
	if f.created[1].Checkout == nil || f.created[0].Checkout == nil ||
		f.created[1].Checkout.Ref != f.created[0].Checkout.Ref {
		t.Error("the replacement did not clone the same branch")
	}
}

func TestRunOnBranchAlsoSurvivesAReapedLease(t *testing.T) {
	f, c := newFakeForge(t)
	sb := acquire(t, f, c)

	f.mu.Lock()
	f.failNextRun = "conflict: lease is stopped, not ready"
	f.mu.Unlock()

	if _, err := sb.RunOnBranch(context.Background(), nil, "bs/t1", "go test ./..."); err != nil {
		t.Fatalf("RunOnBranch: %v — verification is exactly when the lease has been "+
			"idle longest", err)
	}
}

// ONCE, NOT IN A LOOP. If the replacement is reaped just as fast, the problem is
// not a stale lease and retrying forever would hide it.
func TestAReplacementThatDiesTooIsReported(t *testing.T) {
	f, c := newFakeForge(t)
	sb := acquire(t, f, c)

	f.mu.Lock()
	f.failEveryRun = "conflict: lease is stopped, not ready"
	f.mu.Unlock()

	_, err := sb.Run(context.Background(), nil, "go test ./...")
	if err == nil {
		t.Fatal("a sandbox that cannot hold a lease at all reported success")
	}
	if !strings.Contains(err.Error(), "lease is stopped") {
		t.Errorf("the error lost the cause: %v", err)
	}

	// ONE REPLACEMENT, NOT A CASCADE. Each boot costs a container and a clone, so
	// a sandbox that cannot hold a lease must report that rather than work
	// through the node's capacity discovering it.
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.created) != 2 {
		t.Errorf("%d leases were created, want the original and exactly one "+
			"replacement", len(f.created))
	}
}

// A FAILURE TO BOOT A REPLACEMENT MUST NAME BOTH HALVES: what went wrong, and
// why nothing could be done about it.
func TestWhenNoReplacementCanBeAcquiredBothCausesAreNamed(t *testing.T) {
	f, c := newFakeForge(t)
	sb := acquire(t, f, c)

	f.mu.Lock()
	f.failNextRun = "conflict: lease is stopped, not ready"
	f.failCreate = errors.New("no capacity on any node")
	f.mu.Unlock()

	_, err := sb.Run(context.Background(), nil, "go test ./...")
	if err == nil {
		t.Fatal("the command reported success with no sandbox to have run in")
	}
	if !strings.Contains(err.Error(), "lease is stopped") {
		t.Errorf("the original failure is missing: %v", err)
	}
	if !strings.Contains(err.Error(), "no capacity") {
		t.Errorf("the reason no replacement was acquired is missing: %v", err)
	}
}

// AN ORDINARY FAILURE IS NOT A REAPED LEASE. Rebooting the sandbox on a failing
// test suite would hide the result and cost a container each time.
func TestAnOrdinaryFailureDoesNotRebootTheSandbox(t *testing.T) {
	f, c := newFakeForge(t)
	sb := acquire(t, f, c)

	f.mu.Lock()
	f.failNextRun = "conflict: you already hold 6 leases (the limit)"
	f.mu.Unlock()

	if _, err := sb.Run(context.Background(), nil, "go test ./..."); err == nil {
		t.Fatal("a conflict that is NOT a dead lease was treated as one")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.created) != 1 {
		t.Errorf("%d leases created; an unrelated conflict booted a replacement", len(f.created))
	}
}

func TestLeaseGoneRecognisesTheStatesForgeReports(t *testing.T) {
	gone := []string{
		"conflict: lease is stopped, not ready",
		"POST /executions: conflict: Lease Is Stopped",
		"lease is expired",
		"lease not found",
		"unknown lease abc123",
	}
	for _, m := range gone {
		if !leaseGone(errors.New(m)) {
			t.Errorf("leaseGone(%q) = false, want true", m)
		}
	}
	// A 409 IS ALSO HOW FORGE REPORTS THINGS THAT ARE NOT THIS, so the state word
	// is what has to match — not the status.
	stays := []string{
		"conflict: you already hold 6 leases (the limit); release one first",
		"400 Bad Request: image is required",
		"context deadline exceeded",
		"",
	}
	for _, m := range stays {
		if leaseGone(errors.New(m)) {
			t.Errorf("leaseGone(%q) = true, want false", m)
		}
	}
	if leaseGone(nil) {
		t.Error("leaseGone(nil) = true")
	}
}
