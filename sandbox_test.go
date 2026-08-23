package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// leaseServer stands up a fake forge that can be told how to behave when asked
// for a lease, and records the executions it is sent.
type leaseServer struct {
	mu       sync.Mutex
	srv      *httptest.Server
	execs    []SandboxSpec
	leases   []LeaseSpec
	released []string
	// createStatus is returned from POST /leases; 0 means succeed.
	createStatus int
	// readyStatus is the status GET /leases/{id} reports.
	readyStatus string
}

func newLeaseServer(t *testing.T) (*leaseServer, *CodeArmory) {
	t.Helper()
	ls := &leaseServer{readyStatus: LeaseReady}
	ls.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ls.mu.Lock()
		defer ls.mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/leases":
			if ls.createStatus != 0 {
				http.Error(w, "nope", ls.createStatus)
				return
			}
			var spec LeaseSpec
			json.NewDecoder(r.Body).Decode(&spec) //nolint:errcheck
			ls.leases = append(ls.leases, spec)
			w.WriteHeader(http.StatusAccepted)
			json.NewEncoder(w).Encode(Lease{LeaseID: "lease-1", Status: LeaseStarting}) //nolint:errcheck
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/leases/"):
			json.NewEncoder(w).Encode(Lease{ //nolint:errcheck
				LeaseID: "lease-1", Status: ls.readyStatus, Detail: "clone failed",
			})
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/leases/"):
			ls.released = append(ls.released, strings.TrimPrefix(r.URL.Path, "/leases/"))
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/executions":
			var spec SandboxSpec
			json.NewDecoder(r.Body).Decode(&spec) //nolint:errcheck
			ls.execs = append(ls.execs, spec)
			json.NewEncoder(w).Encode(Execution{ExecutionID: "e1", Status: ExecPending}) //nolint:errcheck
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/executions/"):
			code := 0
			json.NewEncoder(w).Encode(Execution{ //nolint:errcheck
				ExecutionID: "e1", Status: ExecCompleted, ExitCode: &code,
			})
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(ls.srv.Close)

	api, err := NewCodeArmory("http://platform.invalid", "platform-token")
	if err != nil {
		t.Fatal(err)
	}
	if err := api.UseLocalForge(ls.srv.URL, "forge-token"); err != nil {
		t.Fatal(err)
	}
	return ls, api
}

func devRequest() SandboxRequest {
	return SandboxRequest{
		Image: "golang:1.25", RunnerClass: "agent-dev", TimeoutSecs: 600,
		CloneURL: "git://host/demo.git", SecretRef: "git:https://host/demo.git",
		Branch: "main",
	}
}

// mustAcquire fails the test rather than returning an error, for the many cases
// where holding the sandbox is a precondition and not the thing under test.
func mustAcquire(t *testing.T, api *CodeArmory, req SandboxRequest) *Sandbox {
	t.Helper()
	sb, err := api.AcquireSandbox(context.Background(), req)
	if err != nil {
		t.Fatalf("AcquireSandbox: %v", err)
	}
	return sb
}

// A leased execution must NOT restate the image, runner class or credentials:
// forge rejects one that does, because they would describe a sandbox other than
// the one the command runs in.
func TestLeasedExecutionCarriesOnlyTheLease(t *testing.T) {
	ls, api := newLeaseServer(t)
	sb := mustAcquire(t, api, devRequest())
	if !sb.Leased() {
		t.Fatal("expected a held sandbox")
	}
	if _, err := sb.Run(context.Background(), nil, "go test ./..."); err != nil {
		t.Fatal(err)
	}
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if len(ls.execs) != 1 {
		t.Fatalf("got %d executions, want 1", len(ls.execs))
	}
	got := ls.execs[0]
	if got.LeaseID != "lease-1" {
		t.Errorf("LeaseID = %q, want lease-1", got.LeaseID)
	}
	if got.Image != "" || got.RunnerClass != "" || len(got.SecretRefs) != 0 {
		t.Errorf("a leased execution restated the sandbox's own settings; forge rejects that: %+v", got)
	}
	// The lease clones once at boot, so no command may clone again.
	if strings.Contains(strings.Join(got.Command, " "), "git clone") {
		t.Error("a leased command clones the repository; the lease already did that at boot")
	}
}

// The lease must clone with history. A shallow clone has no base to diff the
// branch against, and the dependency-regression check needs exactly that.
func TestLeaseClonesWithHistory(t *testing.T) {
	ls, api := newLeaseServer(t)
	mustAcquire(t, api, devRequest())
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if len(ls.leases) != 1 {
		t.Fatalf("got %d leases, want 1", len(ls.leases))
	}
	c := ls.leases[0].Checkout
	if c == nil {
		t.Fatal("the lease does not clone; every command would have to")
	}
	if c.Depth == nil || *c.Depth != 0 {
		t.Errorf("lease clone depth = %v, want 0 (full history) so the dependency check has a base to diff against", c.Depth)
	}
	if c.Ref != "main" {
		t.Errorf("lease checks out %q, want the base branch", c.Ref)
	}
}

// A LEASE THAT CANNOT BE HELD IS AN ERROR, not a quiet downgrade.
//
// This test asserted the opposite until 2026-08-15. The fallback ran one
// container per command, which is roughly ten times slower, and announced itself
// at Info level to a log this host does not surface — so a forge with no /leases
// route looked exactly like a slow model. One ticket spent 23 minutes on five
// verification cycles before anyone thought to check the server.
//
// Reliability was the argument for falling back, and it was the wrong trade: the
// work still happened, just far too slowly for anyone to notice it was broken.
func TestALeaseThatCannotBeHeldFailsLoudly(t *testing.T) {
	for _, tc := range []struct {
		name     string
		set      func(*leaseServer)
		released string
	}{
		{"forge does not know what a lease is", func(l *leaseServer) { l.createStatus = http.StatusNotFound }, ""},
		{"caller is at their lease quota", func(l *leaseServer) { l.createStatus = http.StatusTooManyRequests }, ""},
		{"the sandbox never came up", func(l *leaseServer) { l.readyStatus = LeaseFailed }, "lease-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ls, api := newLeaseServer(t)
			tc.set(ls)

			sb, err := api.AcquireSandbox(context.Background(), devRequest())
			if err == nil {
				t.Fatal("a sandbox that could not be held was reported as success")
			}
			if sb != nil {
				t.Error("a failed acquisition returned a sandbox; the caller would use it")
			}
			ls.mu.Lock()
			defer ls.mu.Unlock()
			if len(ls.execs) != 0 {
				t.Errorf("commands ran anyway: %+v", ls.execs)
			}
			// A lease that came up and then failed still has to be handed back, or
			// it holds memory until the reaper notices.
			if tc.released != "" && (len(ls.released) == 0 || ls.released[0] != tc.released) {
				t.Errorf("released = %v, want the failed lease %q returned", ls.released, tc.released)
			}
		})
	}
}

// Release is deferred unconditionally by the agents, so it has to be idempotent:
// a retry path can reach it twice, and the zero value must not call forge.
func TestRelease(t *testing.T) {
	ls, api := newLeaseServer(t)

	held := mustAcquire(t, api, devRequest())
	held.Release(context.Background())
	// Releasing twice must not double-release: the agent defers it, and a retry
	// path could reach it again.
	held.Release(context.Background())

	ls.mu.Lock()
	defer ls.mu.Unlock()
	if len(ls.released) != 1 || ls.released[0] != "lease-1" {
		t.Errorf("released = %v, want exactly one release of lease-1", ls.released)
	}
}

// THE STAGES MUST NOT SHARE A CONTAINER. Each acquisition builds its own lease,
// so the test author and the developer get separate sandboxes with separate
// checkouts. What they share is the BRANCH — the tester pushes the spec and the
// developer starts from that commit — which is a handoff through git, not
// through a working tree.
func TestEachAcquisitionIsItsOwnSandbox(t *testing.T) {
	req := SandboxRequest{
		Image: "golang:1.25", RunnerClass: "agent-dev", CloneURL: "git://host/demo.git",
		Branch: "dev", TimeoutSecs: 900,
	}
	_, api := newLeaseServer(t)
	a := mustAcquire(t, api, req)
	b := mustAcquire(t, api, req)
	if a == b {
		t.Fatal("two acquisitions returned the same sandbox; stages would share a working tree")
	}
	// Adopting on one must not affect the other.
	a.AdoptBranch("agent/t1")
	if b.adopt != "" {
		t.Error("adopting a branch in one sandbox leaked into another")
	}
}

// ADOPTION APPLIES TO EVERY COMMAND, not once. The held container makes a
// one-off checkout survive, so this is belt and braces now — but it is the guard
// that was missing when a container per command silently discarded the
// checkout, and the developer's push then destroyed the specification tests.
func TestAdoptedBranchIsAppliedToEveryCommand(t *testing.T) {
	_, api := newLeaseServer(t)
	sb := mustAcquire(t, api, SandboxRequest{
		Image: "golang:1.25", CloneURL: "git://host/demo.git", Branch: "dev",
	})
	if sb.adopt != "" {
		t.Error("a fresh sandbox adopts a branch before being told to")
	}
	sb.AdoptBranch("agent/t1")
	if !strings.Contains(sb.adopt, "agent/t1") {
		t.Fatalf("adopt does not name the branch: %q", sb.adopt)
	}
	for _, want := range []string{"git fetch", "checkout -q -B"} {
		if !strings.Contains(sb.adopt, want) {
			t.Errorf("adopt is missing %q: %q", want, sb.adopt)
		}
	}
	// Never fatal: a branch that does not exist yet is the normal first case, and
	// the right answer then is to carry on from the base.
	if !strings.Contains(sb.adopt, "|| true") {
		t.Errorf("adopting a branch that does not exist would fail the command: %q", sb.adopt)
	}
	// An empty branch is a no-op rather than a malformed command.
	sb2 := mustAcquire(t, api, SandboxRequest{Image: "golang:1.25", CloneURL: "u", Branch: "dev"})
	sb2.AdoptBranch("")
	if sb2.adopt != "" {
		t.Error("an empty branch produced an adopt step")
	}
}

// A CAP THE PLANE ENFORCES IS ONE THE CALLER SHOULD RESPECT, NOT DISCOVER. forge
// refuses a seventh concurrent lease per user with 429 "you already hold 6
// leases (the limit); release one first" — a memory budget, not a policy, since
// the agent-dev class reserves 8GB and six of those is 48GB of a 64GB node.
// Blacksmith ran more agents than that and two tickets were failed outright
// seconds after being scoped. Queueing turns a lost ticket into a short wait.
func TestSandboxesQueueForALeaseSlot(t *testing.T) {
	t.Setenv("AGENTS_MAX_LEASES", "2")
	ls, api := newLeaseServer(t)
	_ = ls

	first, err := api.AcquireSandbox(context.Background(), devRequest())
	if err != nil {
		t.Fatalf("first AcquireSandbox() = %v", err)
	}
	second, err := api.AcquireSandbox(context.Background(), devRequest())
	if err != nil {
		t.Fatalf("second AcquireSandbox() = %v", err)
	}

	// The third must WAIT rather than fail: that is the whole difference from
	// letting the forge answer 429.
	blocked := make(chan error, 1)
	go func() {
		sb, err := api.AcquireSandbox(context.Background(), devRequest())
		if sb != nil {
			sb.Release(context.Background())
		}
		blocked <- err
	}()
	select {
	case err := <-blocked:
		t.Fatalf("a third sandbox was acquired past the cap of 2 (err=%v)", err)
	case <-time.After(150 * time.Millisecond):
	}

	// Releasing one lets it through.
	first.Release(context.Background())
	select {
	case err := <-blocked:
		if err != nil {
			t.Errorf("the queued acquisition failed after a slot freed: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Error("a slot was released and the waiter never woke; the cap leaks")
	}
	second.Release(context.Background())
}

// A FAILED ACQUISITION MUST GIVE ITS SLOT BACK, or the cap ratchets down until
// every agent waits forever on a limit that is not really full.
func TestAFailedAcquisitionReturnsItsSlot(t *testing.T) {
	t.Setenv("AGENTS_MAX_LEASES", "1")
	ls, api := newLeaseServer(t)
	ls.createStatus = http.StatusTooManyRequests

	if _, err := api.AcquireSandbox(context.Background(), devRequest()); err == nil {
		t.Fatal("a refused lease was reported as success")
	}
	ls.createStatus = 0 // the quota clears

	done := make(chan error, 1)
	go func() {
		sb, err := api.AcquireSandbox(context.Background(), devRequest())
		if sb != nil {
			sb.Release(context.Background())
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("AcquireSandbox() = %v after the quota cleared", err)
		}
	case <-time.After(3 * time.Second):
		t.Error("the failed acquisition kept its slot; the cap leaked one and never gives it back")
	}
}
