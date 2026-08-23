package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ── unit ──────────────────────────────────────────────────────────────────────

// A status the client wrongly thinks is terminal ends a poll early and reports a
// half-finished run as final; one it wrongly thinks is live polls forever.
func TestIsTerminalExec(t *testing.T) {
	for _, s := range []string{ExecCompleted, ExecFailed, ExecTimedOut, ExecCancelled} {
		if !isTerminalExec(s) {
			t.Errorf("isTerminalExec(%q) = false, want true", s)
		}
	}
	for _, s := range []string{ExecPending, ExecRunning, "", "unknown"} {
		if isTerminalExec(s) {
			t.Errorf("isTerminalExec(%q) = true, want false", s)
		}
	}
}

// The first polls are quick so a short command returns promptly; the interval
// then caps, so a long build is not a tight loop against the control plane.
func TestExecPollIntervalBacksOffAndCaps(t *testing.T) {
	if got := execPollInterval(0, pollMin, pollMax); got != pollMin {
		t.Errorf("first interval = %v, want %v", got, pollMin)
	}
	prev := time.Duration(0)
	for i := range 40 {
		got := execPollInterval(i, pollMin, pollMax)
		if got < prev {
			t.Errorf("interval decreased at attempt %d: %v after %v", i, got, prev)
		}
		if got > pollMax {
			t.Fatalf("interval at attempt %d = %v, exceeds the %v cap", i, got, pollMax)
		}
		prev = got
	}
	// Must survive a shift wide enough to overflow.
	if got := execPollInterval(1000, pollMin, pollMax); got != pollMax {
		t.Errorf("interval at a large attempt = %v, want the cap %v", got, pollMax)
	}
}

// "The command ran and the tests failed" is a normal result an agent reasons
// about, not a transport failure — so OK() and the error return are separate.
func TestSandboxResultOK(t *testing.T) {
	if !(SandboxResult{Status: ExecCompleted, ExitCode: 0}).OK() {
		t.Error("a completed, zero-exit run is not OK()")
	}
	for _, r := range []SandboxResult{
		{Status: ExecCompleted, ExitCode: 1},
		{Status: ExecFailed, ExitCode: 0},
		{Status: ExecTimedOut},
		{Status: ExecCancelled},
		{Status: ExecRunning},
	} {
		if r.OK() {
			t.Errorf("%+v reported OK()", r)
		}
	}
}

func TestSummariseCommandIsBounded(t *testing.T) {
	got := summariseCommand(SandboxSpec{Command: []string{strings.Repeat("x", 5000)}})
	if len([]rune(got)) > 210 {
		t.Errorf("summary is %d runes; it becomes one transcript line and must stay bounded", len([]rune(got)))
	}
	if got := summariseCommand(SandboxSpec{RunnerClass: "agent", Command: []string{"go", "test"}}); got != "agent: go test" {
		t.Errorf("summary = %q, want the runner class named", got)
	}
}

func TestSubmitExecutionValidates(t *testing.T) {
	c := &CodeArmory{}
	if _, err := c.SubmitExecution(context.Background(), SandboxSpec{Command: []string{"ls"}}); err == nil {
		t.Error("SubmitExecution with no image = nil error")
	}
	if _, err := c.SubmitExecution(context.Background(), SandboxSpec{Image: "alpine"}); err == nil {
		t.Error("SubmitExecution with no command = nil error")
	}
}

// ── integration ───────────────────────────────────────────────────────────────

func TestRunSandboxPollsToCompletion(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.execSteps = 3 // stay non-terminal for three polls, so the loop is exercised
	f.execStdout = "all tests passed"

	rec, dir := newTestRecorder(t)
	ctx := rec.Start(context.Background(), "tr-1", "ticket-1", "dev-agent")

	res, err := api.RunSandbox(ctx, rec, SandboxSpec{
		Image: "golang:1.25", Command: []string{"go", "test", "./..."}, RunnerClass: "agent",
	})
	if err != nil {
		t.Fatalf("RunSandbox() = %v", err)
	}
	if !res.OK() {
		t.Errorf("result = %+v, want OK", res)
	}
	if res.Stdout != "all tests passed" {
		t.Errorf("stdout = %q", res.Stdout)
	}
	if res.Outputs["RESULT"] != "ok" {
		t.Errorf("outputs = %v, want the captured output_env", res.Outputs)
	}
	if res.Duration <= 0 {
		t.Error("duration not measured")
	}

	// This is the first thing in the system to record an ACTION — the half of a
	// transcript that says what the agent did, not just what it said.
	var action *Record
	for _, r := range readRecords(t, dir) {
		if r.Kind == KindAction {
			action = &r
			break
		}
	}
	if action == nil {
		t.Fatal("no action recorded; a transcript with turns but no actions is half a training example")
	}
	if action.Tool != "sandbox" {
		t.Errorf("action tool = %q, want sandbox", action.Tool)
	}
	for _, want := range []string{"go test", res.ExecutionID, ExecCompleted} {
		if !strings.Contains(action.Detail, want) {
			t.Errorf("action detail %q missing %q", action.Detail, want)
		}
	}
}

// A non-zero exit is a RESULT, not an error: the agent must be handed it to
// reason about rather than seeing a failure it cannot inspect.
func TestRunSandboxReturnsNonZeroExitAsAResult(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.execExitCode = 1
	f.execStdout = "FAIL: TestThing"

	rec, dir := newTestRecorder(t)
	ctx := rec.Start(context.Background(), "tr-2", "ticket-1", "dev-agent")

	res, err := api.RunSandbox(ctx, rec, SandboxSpec{Image: "alpine", Command: []string{"false"}})
	if err != nil {
		t.Fatalf("RunSandbox() = %v, want a result rather than an error", err)
	}
	if res.OK() {
		t.Error("a failing command reported OK()")
	}
	if res.ExitCode != 1 || !strings.Contains(res.Stdout, "FAIL") {
		t.Errorf("result = %+v, want the failure detail preserved for the agent", res)
	}
	// Still recorded, and still not as an error.
	for _, r := range readRecords(t, dir) {
		if r.Kind == KindAction && r.Error != "" {
			t.Errorf("action recorded an error for a non-zero exit: %q", r.Error)
		}
	}
}

func TestRunSandboxRecordsFailedSubmit(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.denyAll = true

	rec, dir := newTestRecorder(t)
	ctx := rec.Start(context.Background(), "tr-3", "ticket-1", "dev-agent")

	if _, err := api.RunSandbox(ctx, rec, SandboxSpec{Image: "alpine", Command: []string{"ls"}}); !errors.Is(err, ErrDenied) {
		t.Fatalf("RunSandbox() = %v, want ErrDenied", err)
	}
	var found bool
	for _, r := range readRecords(t, dir) {
		if r.Kind == KindAction && r.Error != "" {
			found = true
		}
	}
	if !found {
		t.Error("a refused submit was not recorded; the transcript would show the agent doing nothing")
	}
}

// Abandoning a sandbox holds a runner slot and a concurrency budget until it
// times out on its own, so the caller going away must take the work with it.
// This matters on a workstation, which is shut down often.
func TestRunSandboxCancelsExecutionWhenContextEnds(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.execSteps = 1000 // never finishes on its own

	rec, _ := newTestRecorder(t)
	ctx, cancel := context.WithCancel(rec.Start(context.Background(), "tr-4", "ticket-1", "dev-agent"))

	done := make(chan error, 1)
	go func() {
		_, err := api.RunSandbox(ctx, rec, SandboxSpec{Image: "alpine", Command: []string{"sleep", "3600"}})
		done <- err
	}()

	waitFor(t, "the execution to be submitted", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return len(f.execs) == 1
	})
	cancel()

	if err := <-done; err == nil {
		t.Fatal("RunSandbox() = nil error after cancellation")
	}
	f.mu.Lock()
	cancelled := len(f.cancelled)
	f.mu.Unlock()
	if cancelled != 1 {
		t.Errorf("forge was asked to cancel %d executions, want 1: an abandoned sandbox holds a runner slot", cancelled)
	}
}

// A timed-out or failed execution is terminal and must end the poll, not spin.
func TestRunSandboxHandlesTerminalFailureStatuses(t *testing.T) {
	integrationTest(t)
	for _, status := range []string{ExecFailed, ExecTimedOut, ExecCancelled} {
		t.Run(status, func(t *testing.T) {
			f, api := newFakePlatform(t)
			f.execOutcome = status
			rec, _ := newTestRecorder(t)
			ctx := rec.Start(context.Background(), "tr-"+status, "ticket-1", "dev-agent")

			res, err := api.RunSandbox(ctx, rec, SandboxSpec{Image: "alpine", Command: []string{"ls"}})
			if err != nil {
				t.Fatalf("RunSandbox() = %v, want the terminal status returned as a result", err)
			}
			if res.Status != status || res.OK() {
				t.Errorf("result = %+v, want status %q and not OK", res, status)
			}
		})
	}
}

// The forge paths need conductor's service prefix, exactly like the tickets
// ones. Unprefixed does not 404 — it returns the portal's HTML with a 200.
func TestForgePathsCarryTheServicePrefix(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	rec, _ := newTestRecorder(t)
	ctx := rec.Start(context.Background(), "tr-5", "ticket-1", "dev-agent")

	if _, err := api.RunSandbox(ctx, rec, SandboxSpec{Image: "alpine", Command: []string{"ls"}}); err != nil {
		t.Fatalf("RunSandbox() = %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var sawPrefixed bool
	for _, c := range f.calls {
		if strings.Contains(c, "/forge/executions") {
			sawPrefixed = true
		}
	}
	if !sawPrefixed {
		t.Errorf("no request went to /forge/executions; calls were %v", f.calls)
	}
}

// Two deployments, and the path differs because the routing does: through
// conductor the platform routes by service name, so the path carries /forge;
// direct to a local forge nothing is routing, so it must not.
//
// Getting this backwards fails quietly — a stray /forge prefix on a direct
// connection is a 404 from forge itself, and a missing one through conductor
// returns the portal's HTML with a 200.
func TestForgePathDependsOnWhichForgeIsConfigured(t *testing.T) {
	routed := &CodeArmory{}
	if got := routed.forgePath("/executions"); got != "/forge/executions" {
		t.Errorf("routed path = %q, want the service prefix", got)
	}

	local := &CodeArmory{}
	if err := local.UseLocalForge("http://127.0.0.1:8083", "local-token"); err != nil {
		t.Fatal(err)
	}
	if got := local.forgePath("/executions"); got != "/executions" {
		t.Errorf("direct path = %q, want no service prefix", got)
	}
}

func TestUseLocalForgeValidates(t *testing.T) {
	c := &CodeArmory{}
	if err := c.UseLocalForge("127.0.0.1:8083", "tok"); err == nil {
		t.Error("UseLocalForge accepted a schemeless URL")
	}
	// Falling back to the platform token would send a platform credential to a
	// host deliberately outside the platform's trust domain — worse than failing.
	if err := c.UseLocalForge("http://127.0.0.1:8083", ""); err == nil {
		t.Error("UseLocalForge accepted a local forge with no local credential")
	}
	if err := c.UseLocalForge("", ""); err != nil {
		t.Errorf("UseLocalForge(\"\") = %v, want it to mean 'use the platform'", err)
	}
	if c.forgeURL != "" {
		t.Error("an empty URL left a forge override set")
	}
}

// Sandboxes must go to the local forge while tickets still go to the platform —
// and the two must not interfere, since the agents run concurrently.
func TestLocalForgeIsUsedWithoutDisturbingPlatformCalls(t *testing.T) {
	integrationTest(t)
	platform, api := newFakePlatform(t)
	platform.addTicket(t, Ticket{TicketID: "t1", Title: "x"})

	localForge, forgeAPI := newFakePlatform(t)
	if err := api.UseLocalForge(forgeAPI.baseURL, "test-token"); err != nil {
		t.Fatal(err)
	}

	rec, _ := newTestRecorder(t)
	ctx := rec.Start(context.Background(), "tr", "t1", "dev-agent")

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if _, err := api.RunSandbox(ctx, rec, SandboxSpec{Image: "alpine", Command: []string{"ls"}}); err != nil {
			t.Errorf("RunSandbox() = %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if _, err := api.GetTicket(context.Background(), "t1"); err != nil {
			t.Errorf("GetTicket() = %v", err)
		}
	}()
	wg.Wait()

	localForge.mu.Lock()
	forgeCalls := append([]string(nil), localForge.calls...)
	localForge.mu.Unlock()
	platform.mu.Lock()
	platformCalls := append([]string(nil), platform.calls...)
	platform.mu.Unlock()

	var sawLocalExec bool
	for _, c := range forgeCalls {
		if strings.Contains(c, "/executions") {
			sawLocalExec = true
		}
		if strings.Contains(c, "/forge/") {
			t.Errorf("a direct forge call carried the conductor service prefix: %s", c)
		}
	}
	if !sawLocalExec {
		t.Errorf("the sandbox did not reach the local forge; calls were %v", forgeCalls)
	}
	for _, c := range platformCalls {
		if strings.Contains(c, "/executions") {
			t.Errorf("a sandbox call reached the platform instead of the local forge: %s", c)
		}
	}
}

// The two planes must use DIFFERENT credentials. Sending the platform token to
// the local forge would hand a platform credential to a host that is
// deliberately outside the platform's trust domain — which is the entire reason
// the sandbox plane runs its own gatekeeper.
func TestForgeAndPlatformUseSeparateCredentials(t *testing.T) {
	integrationTest(t)

	seen := map[string]string{}
	var mu sync.Mutex
	record := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			seen[name] = r.Header.Get("Authorization")
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"execution_id":"e1","status":"completed","exit_code":0}`))
		}
	}
	forgeSrv := httptest.NewServer(record("forge"))
	defer forgeSrv.Close()
	platformSrv := httptest.NewServer(record("platform"))
	defer platformSrv.Close()

	api, err := NewCodeArmory(platformSrv.URL, "platform-token")
	if err != nil {
		t.Fatal(err)
	}
	if err := api.UseLocalForge(forgeSrv.URL, "local-forge-token"); err != nil {
		t.Fatal(err)
	}

	if _, err := api.SubmitExecution(context.Background(), SandboxSpec{Image: "alpine", Command: []string{"ls"}}); err != nil {
		t.Fatalf("SubmitExecution() = %v", err)
	}
	if _, err := api.GetTicket(context.Background(), "t1"); err != nil {
		t.Fatalf("GetTicket() = %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if seen["forge"] != "Bearer local-forge-token" {
		t.Errorf("forge saw %q, want the local credential", seen["forge"])
	}
	if seen["platform"] != "Bearer platform-token" {
		t.Errorf("platform saw %q, want the platform credential", seen["platform"])
	}
	if seen["forge"] == seen["platform"] {
		t.Error("both planes received the same credential; the trust separation is not real")
	}
}

// Forge runs sandboxes with a read-only root filesystem and one writable tmpfs.
// Every script must therefore work somewhere writable and point its toolchain
// caches there — otherwise `git clone` fails with "Read-only file system" and Go
// fails separately on /.cache/go-build.
//
// Found by running a real clone-and-test in the deployed sandbox. No unit test
// would have caught it: the fake has no filesystem.
func TestSandboxScriptsUseAWritableWorkingDirectory(t *testing.T) {
	sec := sandboxPreamble
	syncer, err := NewSyncer(nil, nil, syncCfg())
	if err != nil {
		t.Fatal(err)
	}
	// The held container is not in this list: forge sets its environment from
	// sandboxEnv() and puts the working tree at the working directory's root, so
	// its prelude has nothing to relocate. That agreement is asserted below
	// instead, and it matters — a build whose cache landed somewhere different
	// depending on which sandbox ran it would be a miserable bug to chase.
	for name, script := range map[string]string{
		"preamble": sec,
		"sync":     syncer.script(),
	} {
		if !strings.Contains(script, "cd /tmp/work") {
			t.Errorf("%s does not move to a writable directory:\n%s", name, script)
		}
		if !strings.Contains(script, "HOME=/tmp") {
			t.Errorf("%s does not relocate HOME; npm and pip write there", name)
		}
	}
	// Go specifically needs its caches redirected or it fails after the clone.
	for _, want := range []string{"GOCACHE=", "GOMODCACHE=", "npm_config_cache="} {
		if !strings.Contains(sandboxPreamble, want) {
			t.Errorf("preamble does not set %s", want)
		}
	}
	// The held container gets the same names, from the lease rather than a
	// preamble. These two must not drift apart.
	env := sandboxEnv()
	for _, k := range []string{"HOME", "GOCACHE", "GOMODCACHE", "npm_config_cache"} {
		if env[k] == "" {
			t.Errorf("the lease environment does not set %s, but the preamble does", k)
		}
	}
	if !strings.Contains(leasedPrelude, "mkdir -p /tmp/.cache") {
		t.Error("the held container never creates the cache directory its environment names")
	}
}

// blacksmith declares its sandbox size rather than assuming forge's default,
// because forge's seeded classes are sized for containers and these sandboxes are
// microVMs. The call has to work both ways round: the class is absent on a fresh
// plane and present on every restart after that, and getting the second case
// wrong would leave a stale size in place forever.
func TestEnsureRunnerClassCreatesThenUpdates(t *testing.T) {
	var (
		mu      sync.Mutex
		methods []string
		exists  bool
		body    RunnerClassSpec
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		methods = append(methods, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodGet && !exists:
			http.Error(w, "not found", http.StatusNotFound)
		case r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(body)
		case r.Method == http.MethodPost, r.Method == http.MethodPut:
			json.NewDecoder(r.Body).Decode(&body)
			exists = true
			w.WriteHeader(http.StatusCreated)
		}
	}))
	defer srv.Close()

	api, err := NewCodeArmory("http://platform.invalid", "platform-token")
	if err != nil {
		t.Fatal(err)
	}
	if err := api.UseLocalForge(srv.URL, "forge-token"); err != nil {
		t.Fatal(err)
	}

	spec := RunnerClassSpec{Name: "agent-dev", MemoryMB: 8192, CPUMillicores: 4000, Enabled: true}
	if err := api.EnsureRunnerClass(context.Background(), spec); err != nil {
		t.Fatalf("EnsureRunnerClass() on a missing class = %v", err)
	}
	if got := methods[len(methods)-1]; got != "POST /runner-classes" {
		t.Errorf("missing class took %q, want a POST to create it", got)
	}

	// Second call: the class now exists, so it must be updated in place rather
	// than re-created (which forge rejects as a duplicate).
	spec.MemoryMB = 4096
	if err := api.EnsureRunnerClass(context.Background(), spec); err != nil {
		t.Fatalf("EnsureRunnerClass() on an existing class = %v", err)
	}
	if got := methods[len(methods)-1]; got != "PUT /runner-classes/agent-dev" {
		t.Errorf("existing class took %q, want a PUT to update it", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if body.MemoryMB != 4096 {
		t.Errorf("stored MemoryMB = %d, want the updated 4096", body.MemoryMB)
	}
}

// A name is the one field with no sensible default: without it the request would
// address the collection and silently do something other than what was asked.
func TestEnsureRunnerClassRequiresName(t *testing.T) {
	api, err := NewCodeArmory("http://platform.invalid", "tok")
	if err != nil {
		t.Fatal(err)
	}
	if err := api.EnsureRunnerClass(context.Background(), RunnerClassSpec{MemoryMB: 8192}); err == nil {
		t.Error("EnsureRunnerClass() with no name = nil, want an error")
	}
}

// The sandbox root is READ-ONLY with one writable tmpfs, so every toolchain path
// that writes must be moved into it. GOPATH was missed: builds and downloads were
// covered, but the module checksum cache under $GOPATH/pkg/sumdb was not, and
// `go run some/tool@latest` — how a sandbox reaches a scanner it does not ship
// with — failed with an error that reads like a corrupt toolchain.
func TestSandboxPreambleMovesEveryWritablePath(t *testing.T) {
	for _, want := range []string{
		"HOME=/tmp", "GOPATH=/tmp/", "GOCACHE=/tmp/", "GOMODCACHE=/tmp/",
		"XDG_CACHE_HOME=/tmp/", "npm_config_cache=/tmp/",
	} {
		if !strings.Contains(sandboxPreamble, want) {
			t.Errorf("the preamble does not set %s; a tool writing there hits the read-only root", want)
		}
	}
}

// The same invariant for a HELD sandbox, which carries it by a different
// mechanism: the environment is set on the lease when the container starts, not
// exported per script. If these two ever disagree, a build writes its cache to a
// different place depending on whether a lease was available — and the resulting
// failures would appear and vanish with forge's mood rather than with any change
// to the repository.
func TestLeasedSandboxRelocatesCachesToWritableStorage(t *testing.T) {
	env := sandboxEnv()
	for _, key := range []string{"HOME", "GOCACHE", "GOMODCACHE", "XDG_CACHE_HOME", "npm_config_cache"} {
		v, ok := env[key]
		if !ok {
			t.Errorf("a leased sandbox does not set %s; it would write under a read-only root", key)
			continue
		}
		if !strings.HasPrefix(v, "/tmp") {
			t.Errorf("%s=%q is not under the writable tmpfs", key, v)
		}
	}
	// The one-shot preamble exports the same names. Drift between them is the bug
	// this pairing exists to catch.
	for key, want := range env {
		if key == "GIT_CLONE_URL" {
			continue
		}
		if !strings.Contains(sandboxPreamble, key+"="+want) {
			t.Errorf("one-shot preamble does not export %s=%s, so the two modes disagree about where it writes", key, want)
		}
	}
}
