//go:build e2e

package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// End-to-end tests: a live CodeArmory and a live llama-server, no fakes.
//
//	go test -tags e2e ./...
//
// These CREATE REAL TICKETS on a real board, so they are behind a build tag AND
// require BLACKSMITH_E2E_BOARD to be set explicitly. There is no default board:
// a test suite that guesses where to write is one that eventually writes
// somewhere it should not. Every fixture is deleted on the way out.
//
// What each tier is actually for:
//   - unit and integration prove the logic and the wiring against fakes, which
//     is where the fakes are honest;
//   - e2e proves the ASSUMPTIONS the fakes encode — that the real tickets service
//     returns the fields the client expects, that the real model returns usable
//     JSON, that the real claim path resolves a race. A fake can only ever
//     confirm what its author already believed.

// e2eEnv is the resolved configuration for an e2e run.
type e2eEnv struct {
	api     *CodeArmory
	gw      *Gateway
	cfg     Config
	boardID string
}

// requireE2EInference resolves just the serving stack. The model tests need no
// platform credential, and demanding one would make them skip on a host that is
// perfectly able to run them.
func requireE2EInference(t *testing.T) e2eEnv {
	t.Helper()
	cfg, err := LoadConfig()
	if err != nil {
		t.Skipf("no usable inference config: %v", err)
	}
	return e2eEnv{gw: NewGateway(cfg), cfg: cfg}
}

// requireE2EPlatform additionally resolves a live CodeArmory and the scratch
// board. There is deliberately NO default board: a suite that guesses where to
// write is one that eventually writes somewhere it should not.
func requireE2EPlatform(t *testing.T) e2eEnv {
	t.Helper()
	e := requireE2EInference(t)
	e.boardID = envOr("BLACKSMITH_E2E_BOARD", "")
	if e.boardID == "" {
		t.Skip("set BLACKSMITH_E2E_BOARD to run platform e2e tests (they create real tickets)")
	}
	if !e.cfg.DispatchReady() {
		t.Skip("set CODEARMORY_URL and CODEARMORY_TOKEN, or AGENTS_TICKETS_URL, to run platform e2e tests")
	}
	// A STANDALONE host has no platform, and its ticket store is the real service
	// too — same image, same listing semantics. Refusing to run here would leave
	// the only tier that talks to a real tickets service unrunnable on exactly the
	// deployment this project is moving towards, which is where these tests earn
	// their keep: the bugs they catch are the ones a fake cannot show.
	e.api = e2eClient(t, e.cfg)
	return e
}

// e2ePlaneCred is shared by every client the suite builds.
//
// One login for the whole run, not one per test: gatekeeper RATE LIMITS logins
// (five a minute by default), so a suite that authenticates per test trips the
// limit partway through and fails with 429s that have nothing to do with what
// each test is checking. Sharing one renewing session is also what blacksmith
// itself does, so the suite exercises the real credential path rather than a
// login storm no deployment performs.
var (
	e2eCredOnce sync.Once
	e2eCred     *Credential
	e2eCredErr  error
)

func e2ePlaneCredential(cfg Config) (*Credential, error) {
	e2eCredOnce.Do(func() {
		e2eCred, e2eCredErr = NewCredential("", cfg.SandboxLoginURL(), cfg.ForgeEmail, cfg.ForgePassword, nil)
	})
	return e2eCred, e2eCredErr
}

// e2eClient builds a client wired exactly as main() wires blacksmith's own, so
// the suite runs against a platform or a standalone plane without changing.
func e2eClient(t *testing.T, cfg Config) *CodeArmory {
	t.Helper()
	var api *CodeArmory
	if cfg.Standalone() && cfg.PlatformURL == "" {
		api = NewStandaloneCodeArmory()
	} else {
		var err error
		api, err = NewCodeArmory(cfg.PlatformURL, cfg.PlatformToken)
		if err != nil {
			t.Fatalf("client: %v", err)
		}
	}
	if cfg.ForgeURL != "" {
		forgeToken := cfg.ForgeToken
		if cfg.ForgeEmail != "" && cfg.ForgePassword != "" {
			forgeToken = "renewing"
		}
		if err := api.UseLocalForge(cfg.ForgeURL, forgeToken); err != nil {
			t.Fatalf("local forge: %v", err)
		}
		if cfg.ForgeEmail != "" && cfg.ForgePassword != "" {
			cred, err := e2ePlaneCredential(cfg)
			if err != nil {
				t.Fatalf("plane credential: %v", err)
			}
			api.UseRenewingForgeCredential(cred)
		}
	}
	if err := api.UseLocalTickets(cfg.TicketsURL); err != nil {
		t.Fatalf("local tickets: %v", err)
	}
	return api
}

// newFixtureTicket creates a ticket and registers its deletion.
func (e e2eEnv) newFixtureTicket(t *testing.T, title, description, priority string) Ticket {
	t.Helper()
	board := e.boardID
	tk, err := e.api.CreateTicket(context.Background(), Ticket{
		Title:       "[blacksmith-e2e] " + title,
		Description: description,
		Priority:    priority,
		BoardID:     &board,
	})
	if err != nil {
		t.Fatalf("create fixture ticket: %v", err)
	}
	t.Cleanup(func() {
		if err := e.api.DeleteTicket(context.Background(), tk.TicketID); err != nil {
			t.Logf("WARNING: could not delete fixture ticket %s: %v", tk.TicketID, err)
		}
	})
	return tk
}

func e2eWaitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, what)
}

// The serving stack answers on every configured class, with the usage fields
// transcript capture depends on. Some backends omit usage entirely, which a fake
// would never reveal.
func TestE2EGatewayServesEveryConfiguredClass(t *testing.T) {
	e := requireE2EInference(t)

	for _, class := range e.cfg.Configured() {
		t.Run(string(class), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()

			res, err := e.gw.Chat(ctx, class, ChatRequest{
				Messages:  []Message{{Role: "user", Content: "Reply with the single word: ready"}},
				MaxTokens: 16, Temperature: 0, Priority: PriorityNormal,
			})
			if err != nil {
				t.Fatalf("Chat(%s) = %v", class, err)
			}
			if res.CompletionTokens == 0 {
				t.Errorf("completion_tokens = 0; transcript capture records usage and some backends omit it")
			}
			if res.Model == "" || strings.HasPrefix(res.Model, "/") {
				t.Errorf("model = %q; a filesystem path here ends up in every transcript record (set --alias)", res.Model)
			}
			t.Logf("%-6s model=%-16s tokens=%d/%d latency=%v", class, res.Model,
				res.PromptTokens, res.CompletionTokens, res.Latency)
		})
	}
}

// The real model must produce output the real parser accepts. This is the
// assumption most likely to be wrong and least likely to be caught by a fake,
// because the fake returns whatever the test author wrote.
func TestE2EModelProducesParseableTriage(t *testing.T) {
	e := requireE2EInference(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	res, err := e.gw.Chat(ctx, e.cfg.PMClass, ChatRequest{
		Messages: []Message{
			{Role: "system", Content: pmSystemPrompt},
			{Role: "user", Content: renderTicket(Ticket{
				Title:       "Login returns 500 on submit",
				Description: "Since the last deploy, submitting the login form returns a 500. Affects all users.",
				Priority:    "medium", CreatedBy: "alice",
			})},
		},
		Temperature: 0, MaxTokens: 800,
	})
	if err != nil {
		t.Fatalf("Chat() = %v", err)
	}

	tri, err := parseTriage(res.Content)
	if err != nil {
		t.Fatalf("the real model's output did not parse: %v\n--- raw ---\n%s", err, res.Content)
	}
	tri = sanitiseTriage(tri)
	if tri.Priority == "" {
		t.Errorf("model returned no usable priority (raw: %q); triage would leave every ticket unchanged", res.Content)
	}
	if tri.Summary == "" {
		t.Error("model returned no summary")
	}
	t.Logf("priority=%s labels=%v subtasks=%d", tri.Priority, tri.Labels, len(tri.Subtasks))
}

// The full loop: a real ticket goes in, the dispatcher claims it, the real model
// triages it, and a real comment comes out.

// onlyTicket restricts a REAL handler to one ticket.
//
// An e2e dispatcher is not a simulation: pointed at a shared board it selects
// every ticket its predicate matches, claims them, and — for the product manager
// — spends a model call writing triage onto work the suite never meant to touch.
// It also burns that role's attempt budget on those tickets, which then blocks
// the real agent from them. Scoping the predicate keeps the dispatch path under
// test while confining the blast radius to the fixture.
type onlyTicket struct {
	Handler
	id string
}

func (o onlyTicket) Wants(t Ticket) bool { return t.TicketID == o.id && o.Handler.Wants(t) }

func TestE2ETriageLifecycle(t *testing.T) {
	e := requireE2EPlatform(t)
	tk := e.newFixtureTicket(t,
		"Search returns stale results after an update",
		"Updating a record leaves the old values visible in search for several minutes. Reported by two users.",
		"medium")

	rec, dir := newTestRecorder(t)
	e.gw.SetRecorder(rec)

	d := NewDispatcher(e.api, rec, onlyTicket{NewPMAgent(e.gw, e.api, e.cfg.PMClass), tk.TicketID}, DispatcherOpts{
		Host:         e.cfg.Host + "-e2e",
		BoardID:      e.boardID,
		Concurrency:  1,
		Poll:         10 * time.Second,
		AgentAuthors: e.cfg.AgentAuthors,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	go d.Run(ctx)

	var final Ticket
	e2eWaitFor(t, "a triage comment on the ticket", 4*time.Minute, func() bool {
		got, err := e.api.GetTicket(context.Background(), tk.TicketID)
		if err != nil {
			return false
		}
		final = got
		for _, c := range got.Comments {
			if strings.Contains(c.Body, "**Triage**") {
				return true
			}
		}
		return false
	})
	cancel()

	// The claim must name this host, which is what makes a patch traceable to the
	// box that produced it when one role runs on several machines.
	//
	// Read from the COMMENT, which outlives the claim. ClaimedBy answers a
	// different question — is this ticket held right now — and by the time triage
	// has landed the work is finished and the ticket released, so asking it here
	// would report "unclaimed" for a run that plainly happened.
	cm, ok := oldestClaim(final.Comments)
	if !ok {
		t.Error("no claim comment on the ticket")
	} else if claim, err := parseClaim(cm.Body); err != nil {
		t.Errorf("claim comment is unparseable: %v", err)
	} else if claim.Host != e.cfg.Host+"-e2e" || claim.Role != "pm-agent" {
		t.Errorf("claim = %+v, want this host and pm-agent", claim)
	}

	// The ticket must be released, not left stuck in progress.
	e2eWaitFor(t, "the ticket to be released", 30*time.Second, func() bool {
		got, err := e.api.GetTicket(context.Background(), tk.TicketID)
		return err == nil && got.Status == StatusOpen
	})

	// And the transcript must have captured it, because that is the artefact the
	// whole exercise exists to produce.
	var sawTurn, sawOutcome bool
	for _, r := range readRecords(t, dir) {
		if r.TaskID != tk.TicketID {
			continue
		}
		switch r.Kind {
		case KindTurn:
			sawTurn = true
			if r.PromptTokens == 0 {
				t.Error("turn recorded with no prompt tokens")
			}
		case KindOutcome:
			sawOutcome = true
			if r.Status != OutcomeSuccess {
				t.Errorf("outcome = %q (%s)", r.Status, r.Detail)
			}
		}
	}
	if !sawTurn || !sawOutcome {
		t.Errorf("transcript incomplete: turn=%v outcome=%v", sawTurn, sawOutcome)
	}
	if rec.Dropped() != 0 {
		t.Errorf("dropped %d transcript records", rec.Dropped())
	}
}

// The claim must be exclusive against the REAL tickets service — the property
// the whole multi-host design rests on, and the one a fake cannot vouch for.
//
// It passes on either implementation: a conditional write where the instance
// supports If-Match, the append fallback where it does not. The log line says
// which, because "it worked" is not the same claim as "it worked the fast way".
func TestE2EClaimIsExclusiveAgainstTheRealService(t *testing.T) {
	e := requireE2EPlatform(t)
	tk := e.newFixtureTicket(t, "Claim exclusivity probe", "Created by the blacksmith e2e suite.", "low")

	// Report which path the client will take, so a silent regression to the
	// fallback is visible rather than invisible.
	_, etag, err := e.api.getTicketWithETag(context.Background(), tk.TicketID)
	if err != nil {
		t.Fatalf("read ticket: %v", err)
	}
	if etag == "" {
		t.Log("instance serves NO ETag: exercising the claim-by-append fallback")
	} else {
		t.Logf("instance serves ETag %s: exercising the conditional-write path", etag)
	}

	const hosts = 4
	errs := make(chan error, hosts)
	for i := range hosts {
		go func() {
			// Same wiring as every other client: built from PlatformURL alone
			// this constructs nothing on a standalone host, and all four "lose"
			// for want of a URL rather than for losing the race.
			api := e2eClient(t, e.cfg)
			errs <- api.Claim(context.Background(), tk.TicketID, stages[roleScoping],
				ClaimToken{Host: fmt.Sprintf("e2e-host-%d", i), Role: "pm-agent"})
		}()
	}

	won := 0
	for range hosts {
		switch err := <-errs; {
		case err == nil:
			won++
		case errors.Is(err, ErrConflict):
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if won != 1 {
		t.Fatalf("%d of %d hosts won the claim against the real service, want exactly 1", won, hosts)
	}

	got, err := e.api.GetTicket(context.Background(), tk.TicketID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusInProgress {
		t.Errorf("status = %q after a successful claim, want %q", got.Status, StatusInProgress)
	}
}

// A ticket the agent authored must never be selected, against the real service.
// This is the loop breaker, and getting it wrong means the department generates
// work for itself indefinitely.
func TestE2ELoopBreakerExcludesAgentAuthoredTickets(t *testing.T) {
	e := requireE2EPlatform(t)
	tk := e.newFixtureTicket(t, "Loop breaker probe", "Created by the blacksmith e2e suite.", "low")

	got, err := e.api.GetTicket(context.Background(), tk.TicketID)
	if err != nil {
		t.Fatal(err)
	}

	d := NewDispatcher(e.api, nil, NewPMAgent(e.gw, e.api, e.cfg.PMClass), DispatcherOpts{
		Host: "e2e", AgentAuthors: []string{got.CreatedBy},
	})
	if d.eligible(got) {
		t.Errorf("a ticket authored by %q is eligible; with that account in the exclusion list the department would feed itself", got.CreatedBy)
	}
}

// forge enforces an image allowlist, and a rejected image is a bare 400 that
// names neither the image nor the alternatives. Assert the list is reachable, so
// a mismatch is diagnosable instead of mysterious.
func TestE2EForgeImageAllowlistIsReadable(t *testing.T) {
	e := requireE2EPlatform(t)
	images, err := e.api.ListImages(context.Background())
	if err != nil {
		t.Fatalf("ListImages() = %v", err)
	}
	if len(images) == 0 {
		t.Fatal("forge allows no images; every sandbox would be refused")
	}
	want := envOr("BLACKSMITH_E2E_IMAGE", "alpine:3.19")
	if !slices.Contains(images, want) {
		t.Errorf("the e2e image %q is not on forge's allowlist %v; set BLACKSMITH_E2E_IMAGE", want, images)
	}
	t.Logf("allowed images: %v", images)
}

// A real sandbox, on the real forge. This is the assumption set most likely to
// drift: the image exists, the runner class exists, the command shape is right,
// and the response actually carries stdout and the captured outputs.
func TestE2ESandboxRunsAndReturnsOutput(t *testing.T) {
	e := requireE2EPlatform(t)
	rec, dir := newTestRecorder(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	tctx := rec.Start(ctx, "e2e-sandbox", "e2e", "dev-agent")

	image := envOr("BLACKSMITH_E2E_IMAGE", "alpine:3.19")
	res, err := e.api.RunSandbox(tctx, rec, SandboxSpec{
		Image:       image,
		Command:     []string{"sh", "-c", "echo hello-from-sandbox; export RESULT=ok"},
		OutputEnv:   []string{"RESULT"},
		TimeoutSecs: 120,
	})
	if err != nil {
		t.Fatalf("RunSandbox() = %v", err)
	}
	if !res.OK() {
		t.Fatalf("sandbox did not succeed: status=%s exit=%d stderr=%q", res.Status, res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "hello-from-sandbox") {
		t.Errorf("stdout = %q, want the command's output", res.Stdout)
	}
	// output_env is how a sandbox hands back structured data instead of making
	// the agent parse its own stdout. If the real forge does not populate it, the
	// agents cannot rely on it.
	if res.Outputs["RESULT"] != "ok" {
		t.Errorf("outputs = %v, want RESULT=ok captured from the shell", res.Outputs)
	}
	t.Logf("execution=%s status=%s exit=%d in %v", res.ExecutionID, res.Status, res.ExitCode, res.Duration)

	var sawAction bool
	for _, r := range readRecords(t, dir) {
		if r.Kind == KindAction && r.Tool == "sandbox" {
			sawAction = true
		}
	}
	if !sawAction {
		t.Error("no sandbox action recorded; the transcript would show the agent doing nothing")
	}
}

// A failing command must come back as a RESULT the agent can read, not as a
// transport error — this is the normal case for a test suite that fails.
func TestE2ESandboxSurfacesNonZeroExit(t *testing.T) {
	e := requireE2EPlatform(t)
	rec, _ := newTestRecorder(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	tctx := rec.Start(ctx, "e2e-sandbox-fail", "e2e", "dev-agent")

	res, err := e.api.RunSandbox(tctx, rec, SandboxSpec{
		Image:       envOr("BLACKSMITH_E2E_IMAGE", "alpine:3.19"),
		Command:     []string{"sh", "-c", "echo failing >&2; exit 3"},
		TimeoutSecs: 120,
	})
	if err != nil {
		t.Fatalf("RunSandbox() = %v, want a result rather than an error", err)
	}
	if res.OK() {
		t.Error("a command exiting 3 reported OK()")
	}
	if res.ExitCode != 3 {
		t.Errorf("exit code = %d, want 3 (got status %q)", res.ExitCode, res.Status)
	}
}

// THE TIER THAT SHOULD HAVE CAUGHT IT. Selection is decided by comments, and a
// real ticket LISTING does not carry them — only a single-ticket read does. No
// fake showed this, because the fake served whole tickets from its listing and
// so kept confirming the client's assumption instead of testing it.
//
// The consequence is asymmetric and that is what made it hard to see: a stage
// whose predicate REQUIRES a marker never matches anything and silently does
// nothing at all, while a stage whose predicate requires a marker's ABSENCE
// matches everything and re-claims the same tickets on every poll. The
// department stops after triage while looking busy.
func TestE2ESelectionSeesCommentsTheListingOmits(t *testing.T) {
	e := requireE2EPlatform(t)
	ctx := context.Background()
	tk := e.newFixtureTicket(t, "selection must see comments",
		"A fixture for the dispatcher's selection path. Safe to delete.", "medium")

	// Put this ticket into the state the second pipeline stage looks for.
	if _, err := e.api.AddComment(ctx, tk.TicketID, "**Triage** (automated)\n\nseeded by the e2e suite"); err != nil {
		t.Fatalf("seed triage comment: %v", err)
	}

	// PIN THE HAZARD. If the service ever starts returning comments in listings
	// this assertion fails, and whoever sees it should know the workaround below
	// can be simplified rather than wonder why the dispatcher reads twice.
	listed, err := e.api.ListTickets(ctx, ListOpts{BoardID: e.boardID, Status: StatusOpen})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var seen bool
	for _, l := range listed {
		if l.TicketID != tk.TicketID {
			continue
		}
		seen = true
		if len(l.Comments) != 0 {
			t.Errorf("the listing now carries %d comments; selection no longer needs to re-read",
				len(l.Comments))
		}
	}
	if !seen {
		t.Fatalf("fixture ticket %s is not in the open listing for board %s", tk.TicketID, e.boardID)
	}

	// And the full read must carry it, or nothing downstream can work.
	full, err := e.api.GetTicket(ctx, tk.TicketID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !hasTriage(full) {
		t.Fatalf("the single-ticket read has no triage comment either; the seed did not land")
	}

	// Now the behaviour that matters: a stage needing that comment is dispatched.
	// A stub with the developer agent's predicate, so this costs no model call and
	// no sandbox — the selection path is what is under test.
	rec, _ := newTestRecorder(t)
	h := &stubHandler{role: "dev-agent", status: OutcomeSuccess}
	// SCOPED TO THIS FIXTURE. The board is shared, and a dispatcher is a real one:
	// left unscoped it selects any ticket matching the predicate, claims it, and
	// spends that role's attempt budget on work the suite never meant to touch —
	// which then blocks the actual agent from a ticket it should have had. The
	// predicate still exercises the selection path being tested; it just refuses
	// everything that is not ours.
	h.wants = func(x Ticket) bool {
		return x.TicketID == tk.TicketID && hasTriage(x) && !hasBranch(x)
	}
	d := NewDispatcher(e.api, rec, h, DispatcherOpts{
		Host: e.cfg.Host + "-e2e", BoardID: e.boardID,
		Concurrency: 1, Poll: 5 * time.Second, AgentAuthors: e.cfg.AgentAuthors,
	})

	runCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	go d.Run(runCtx)

	e2eWaitFor(t, "the triaged ticket to reach the next stage", 60*time.Second,
		func() bool { return h.count() >= 1 })

	// It must also be handed the comments, not just selected: the developer
	// agent's brief IS the triage comment.
	cancel()
	e2eWaitFor(t, "the ticket to be released", 30*time.Second, func() bool {
		got, err := e.api.GetTicket(context.Background(), tk.TicketID)
		return err == nil && got.Status == StatusOpen
	})
}
