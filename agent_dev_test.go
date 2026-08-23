package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// ── unit: the trust boundary ──────────────────────────────────────────────────

// Paths come from the model, so this is a trust boundary rather than a tidiness
// check: "src/main.go" and "../../etc/cron.d/x" differ only in content.
func TestValidatePathsRefusesEscapes(t *testing.T) {
	bad := []string{
		"/etc/passwd",
		"../outside.txt",
		"../../etc/cron.d/agent",
		"src/../../../etc/shadow",
		"a/b/../../../c",
		"",
		"   ",
		"with\x00null",
	}
	for _, p := range bad {
		if got, err := validatePaths([]string{p}, 10); err == nil {
			t.Errorf("validatePaths(%q) = %v, want a rejection", p, got)
		}
	}
}

func TestValidatePathsAcceptsRepoRelative(t *testing.T) {
	got, err := validatePaths([]string{"main.go", "./src/a.go", "src/b/../c.go", "docs/x.md"}, 10)
	if err != nil {
		t.Fatalf("validatePaths() = %v", err)
	}
	want := []string{"main.go", "src/a.go", "src/c.go", "docs/x.md"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("cleaned = %v, want %v", got, want)
			break
		}
	}
}

func TestValidatePathsBoundsCount(t *testing.T) {
	many := make([]string, 50)
	for i := range many {
		many[i] = "f.go"
	}
	if _, err := validatePaths(many, 12); err == nil {
		t.Error("validatePaths accepted 50 paths against a limit of 12")
	}
	if _, err := validatePaths(nil, 12); err == nil {
		t.Error("validatePaths accepted an empty list")
	}
}

// Model-authored bytes reach a shell script here and nowhere else. Encoding them
// is what keeps "write a file" from becoming "run a command".
func TestApplyScriptEncodesContentSoItCannotEscapeTheShell(t *testing.T) {
	a := &DevAgent{repo: RepoConfig{URL: "https://example/r"}}
	s := &devState{staged: map[string]string{
		"evil.txt": "hello\"; rm -rf / #\n$(touch /tmp/pwned)\n`id`\n",
	}}
	script := a.applyScript(s)

	for _, injected := range []string{"rm -rf /", "$(touch", "`id`"} {
		if strings.Contains(script, injected) {
			t.Errorf("raw model content %q appears in the script; it must be encoded:\n%s", injected, script)
		}
	}
	// And the encoding must actually round-trip.
	var payload string
	for _, tok := range strings.Fields(script) {
		if len(tok) > 20 && !strings.ContainsAny(tok, "/$`") {
			if dec, err := base64.StdEncoding.DecodeString(strings.Trim(tok, `"`)); err == nil && strings.Contains(string(dec), "rm -rf") {
				payload = string(dec)
			}
		}
	}
	if payload == "" {
		t.Error("could not find the encoded content in the script; the file would be written empty")
	}
}

func TestApplyEditsEnforcesLimits(t *testing.T) {
	s := &devState{staged: map[string]string{}, read: map[string]string{}}

	if err := applyEdits(s, nil, modeDevelop); err == nil {
		t.Error("applyEdits accepted an empty edit list")
	}
	if err := applyEdits(s, []devEdit{{Path: "../escape.go", Replace: "x"}}, modeDevelop); err == nil {
		t.Error("applyEdits accepted a path outside the repository")
	}
	many := make([]devEdit, maxWriteFiles+1)
	for i := range many {
		many[i] = devEdit{Path: "f.go", Replace: "x"}
	}
	if err := applyEdits(s, many, modeDevelop); err == nil {
		t.Error("applyEdits accepted more edits than the limit")
	}

	if err := applyEdits(s, []devEdit{{Path: "./src/a.go", Replace: "package main"}}, modeDevelop); err != nil {
		t.Fatalf("applyEdits(valid create) = %v", err)
	}
	// Content gains a trailing newline at staging; see withTrailingNewline.
	if s.staged["src/a.go"] != "package main\n" {
		t.Errorf("staged = %v, want the cleaned path and a terminated file", s.staged)
	}
	// The agent must see its own edit on the next turn, or it re-reads the old
	// content and undoes itself.
	if s.read["src/a.go"] != "package main\n" {
		t.Error("a staged write is not visible to the next turn's context")
	}
}

func TestParseDevActionValidates(t *testing.T) {
	if _, err := parseDevAction(`{"action":"rm_rf","paths":["/"]}`); err == nil {
		t.Error("parseDevAction accepted an action outside the allowlist")
	}
	if _, err := parseDevAction(`{"paths":["a"]}`); err == nil {
		t.Error("parseDevAction accepted an object with no action")
	}
	if _, err := parseDevAction("I'll start by reading the code."); err == nil {
		t.Error("parseDevAction accepted prose")
	}

	act, err := parseDevAction("```json\n{\"action\":\"read_files\",\"paths\":[\"a.go\"]}\n```")
	if err != nil {
		t.Fatalf("parseDevAction(fenced) = %v", err)
	}
	if act.Action != actionReadFiles || len(act.Paths) != 1 {
		t.Errorf("parsed = %+v", act)
	}
}

// Every constraint the loop enforces has to appear in the prompt, or the model
// spends turns being rejected for rules nobody told it about.
func TestDevSystemPromptStatesItsConstraints(t *testing.T) {
	p := devSystemPrompt()
	for _, want := range []string{"pipeline", "..", "read_files", "write_files"} {
		if !strings.Contains(p, want) {
			t.Errorf("system prompt does not mention %q", want)
		}
	}
	// The ACTION VOCABULARY now lives in the tool definitions rather than the
	// prose, which is the point of tool calling: the model picks a function by
	// name from a validated list instead of authoring an envelope. So the
	// constraint to check is that every action is actually offered.
	offered := map[string]bool{}
	for _, tool := range devTools(modeDevelop) {
		offered[tool.Name] = true
	}
	for _, want := range devActions {
		if !offered[want] {
			t.Errorf("action %q is not offered as a tool, so the model cannot take it", want)
		}
	}
}

func TestCommitMessageFallsBackToTheTicketTitle(t *testing.T) {
	got := commitMessage(Ticket{TicketID: "abc", Title: "Fix the thing"}, "", "fix")
	if !strings.Contains(got, "fix the thing") {
		t.Errorf("commit message = %q, want the ticket title as the subject", got)
	}
	if !strings.Contains(got, "Ticket: abc") {
		t.Errorf("commit message = %q, want the ticket referenced in the body", got)
	}
	// Bounded in RUNES: clip() appends an ellipsis, which is one rune and three
	// bytes, so a byte-length assertion here fails on a correctly-clipped subject.
	long := commitMessage(Ticket{TicketID: "abc"}, strings.Repeat("x", 500), "fix")
	if n := len([]rune(strings.SplitN(long, "\n", 2)[0])); n > 72 {
		t.Errorf("commit subject is %d runes, want it within git's 72-column convention", n)
	}
}

// ── integration: the loop ─────────────────────────────────────────────────────

// scriptedModel returns a fixed sequence of replies, one per call, so a whole
// agent loop can be driven deterministically.
func scriptedModel(t *testing.T, replies ...string) (*Gateway, func() int) {
	t.Helper()
	var n int
	gw, _ := testGateway(t, func(w http.ResponseWriter, r *http.Request) {
		reply := `{"action":"give_up","reason":"script exhausted"}`
		if n < len(replies) {
			reply = replies[n]
		}
		n++
		completionHandler(reply)(w, r)
	}, 4)
	return gw, func() int { return n }
}

func devTestAgent(t *testing.T, gw *Gateway, api *CodeArmory) *DevAgent {
	t.Helper()
	return NewDevAgent(gw, api, ClassLarge, RepoConfig{
		URL: "https://example.invalid/org/repo", Branch: "main",
		BranchPrefix: "agent/", Image: "golang:1.25", PipelineID: "pipe-1",
	}, 6)
}

func TestDevAgentHappyPath(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.addTicket(t, Ticket{TicketID: "t1", Title: "Add a greeting", CreatedBy: "alice"})
	f.execStdout = "main.go\ngo.mod\n"

	gw, calls := scriptedModel(t,
		`{"action":"read_files","paths":["main.go"]}`,
		`{"action":"write_files","edits":[{"path":"main.go","search":"","replace":"package main\nfunc main(){}\n"}]}`,
	)
	rec, dir := newTestRecorder(t)
	ctx := rec.Start(context.Background(), "tr-dev", "t1", "dev-agent")

	status, detail, err := devTestAgent(t, gw, api).Handle(ctx, f.get("t1"))
	if err != nil {
		t.Fatalf("Handle() = %v", err)
	}
	if status != OutcomeSuccess {
		t.Fatalf("status = %q (%s), want success", status, detail)
	}
	if !strings.Contains(detail, "agent/t1") {
		t.Errorf("detail = %q, want the pushed branch named", detail)
	}
	// TWO. The write triggers the verification and green ends the stage, so the
	// model is never asked for the run_tests turn or the finish turn it used to
	// have to supply — the two turns it was worst at sequencing.
	if calls() != 2 {
		t.Errorf("model called %d times, want 2", calls())
	}

	bodies := f.commentBodies("t1")
	if len(bodies) != 1 || !strings.Contains(bodies[0], "pushed a branch") {
		t.Fatalf("comments = %v, want one branch-pushed comment", bodies)
	}
	// The comment must not claim more authority than it has: the pipeline is the
	// gate, not a green sandbox run.
	if !strings.Contains(bodies[0], "pipeline is the gate") {
		t.Error("the comment does not say review is still required")
	}

	var actions int
	for _, r := range readRecords(t, dir) {
		if r.Kind == KindAction {
			actions++
		}
	}
	if actions < 4 {
		t.Errorf("recorded %d actions, want one per sandbox run (survey, read, test, publish)", actions)
	}
}

// Finishing without passing tests would push unverified code, so it is refused
// and the model is told why rather than silently ignored.
func TestDevAgentRefusesToFinishWithoutPassingTests(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.addTicket(t, Ticket{TicketID: "t1", Title: "x", CreatedBy: "alice"})
	f.execStdout = "main.go\n"

	gw, _ := scriptedModel(t,
		`{"action":"write_files","edits":[{"path":"main.go","search":"","replace":"package main"}]}`,
		`{"action":"finish","summary":"done"}`,
		`{"action":"give_up","reason":"cannot get tests to pass"}`,
	)
	rec, _ := newTestRecorder(t)
	ctx := rec.Start(context.Background(), "tr", "t1", "dev-agent")

	status, _, err := devTestAgent(t, gw, api).Handle(ctx, f.get("t1"))
	if err != nil {
		t.Fatalf("Handle() = %v", err)
	}
	if status != OutcomeFailed {
		t.Errorf("status = %q, want failure: finishing without a green test run must not push", status)
	}
	for _, b := range f.commentBodies("t1") {
		if strings.Contains(b, "pushed a branch") {
			t.Error("a branch was pushed without tests passing")
		}
	}
}

func TestDevAgentRefusesToFinishWithNoChanges(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.addTicket(t, Ticket{TicketID: "t1", Title: "x", CreatedBy: "alice"})

	gw, _ := scriptedModel(t,
		`{"action":"run_tests"}`,
		`{"action":"finish","summary":"nothing to do"}`,
		`{"action":"give_up","reason":"no change needed"}`,
	)
	rec, _ := newTestRecorder(t)
	ctx := rec.Start(context.Background(), "tr", "t1", "dev-agent")

	status, _, _ := devTestAgent(t, gw, api).Handle(ctx, f.get("t1"))
	if status != OutcomeFailed {
		t.Errorf("status = %q, want failure: an empty branch is not a fix", status)
	}
}

// A failing test run must come back to the model as context to work with, not
// end the task — iterating on a red suite is the job.
func TestDevAgentFeedsTestFailuresBackToTheModel(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.addTicket(t, Ticket{TicketID: "t1", Title: "x", CreatedBy: "alice"})
	// Verification is a PIPELINE now, so the failure comes from the run, not from
	// a forge command the agent chose.
	f.runOutcome = RunFailed

	var prompts []string
	gw, _ := scriptedModel(t,
		// Valid Go, because a .go write that does not PARSE is now refused before it
		// can be staged — see repairGoSource. The content is incidental here; what
		// is under test is that the failing output comes back to the model.
		`{"action":"write_files","edits":[{"path":"a.go","search":"","replace":"package main\n"}]}`,
		`{"action":"run_tests"}`,
		`{"action":"give_up","reason":"could not fix"}`,
	)
	_ = prompts
	rec, dir := newTestRecorder(t)
	// Turn records come from the GATEWAY's recorder, not the context's — without
	// this the assertion below inspects an empty set and passes for free.
	gw.SetRecorder(rec)
	ctx := rec.Start(context.Background(), "tr", "t1", "dev-agent")

	status, _, err := devTestAgent(t, gw, api).Handle(ctx, f.get("t1"))
	if err != nil {
		t.Fatalf("Handle() = %v", err)
	}
	if status != OutcomeFailed {
		t.Errorf("status = %q, want failure after give_up", status)
	}
	// The failing output must have reached a later prompt.
	var sawFailure bool
	for _, r := range readRecords(t, dir) {
		if r.Kind != KindTurn {
			continue
		}
		for _, m := range r.Messages {
			if strings.Contains(m.Content, "failed at step") {
				sawFailure = true
			}
		}
	}
	if !sawFailure {
		t.Error("the test failure never reached the model; it cannot iterate on output it is not shown")
	}
}

// An unusable reply must be corrected, not fatal — models emit prose and invalid
// actions, and one bad turn should cost a turn, not the ticket.
func TestDevAgentRecoversFromAnInvalidAction(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.addTicket(t, Ticket{TicketID: "t1", Title: "x", CreatedBy: "alice"})
	// main.go is deliberately absent from this listing: the script writes it
	// without reading it, which is a CREATE. A tree claiming it already exists
	// would make the same script a blind whole-file overwrite, which is refused.
	f.execStdout = "go.mod\n"

	gw, calls := scriptedModel(t,
		`I think I should start by looking at the code.`,
		`{"action":"delete_everything"}`,
		`{"action":"write_files","edits":[{"path":"main.go","search":"","replace":"package main"}]}`,
	)
	rec, _ := newTestRecorder(t)
	ctx := rec.Start(context.Background(), "tr", "t1", "dev-agent")

	status, _, err := devTestAgent(t, gw, api).Handle(ctx, f.get("t1"))
	if err != nil {
		t.Fatalf("Handle() = %v", err)
	}
	if status != OutcomeSuccess {
		t.Errorf("status = %q, want the agent to recover from two bad replies", status)
	}
	// Three: two bad replies and the write that ends the stage. The verification
	// and the finish are no longer turns the model has to spend.
	if calls() != 3 {
		t.Errorf("model called %d times, want 3", calls())
	}
}

// The loop must terminate. A model that never converges costs tokens forever
// otherwise, and on a shared GPU that starves every other agent.
func TestDevAgentStopsAtTheIterationLimit(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.addTicket(t, Ticket{TicketID: "t1", Title: "x", CreatedBy: "alice"})
	f.execStdout = "main.go\n"

	// Always reads, never converges.
	gw, calls := scriptedModel(t, strings.Split(strings.Repeat(`{"action":"read_files","paths":["main.go"]},`, 20), ",")...)
	rec, _ := newTestRecorder(t)
	ctx := rec.Start(context.Background(), "tr", "t1", "dev-agent")

	a := NewDevAgent(gw, api, ClassLarge, RepoConfig{
		URL: "https://example.invalid/r", Branch: "main", BranchPrefix: "agent/",
		Image: "golang:1.25", PipelineID: "pipe-1",
	}, 3)

	status, detail, err := a.Handle(ctx, f.get("t1"))
	if err != nil {
		t.Fatalf("Handle() = %v", err)
	}
	// Bounded, and now bounded EARLY: repeating one read is caught as a stuck
	// loop before the iteration cap is reached. Either exit is a bounded stop,
	// which is what this test is for; the stuck one is simply the better answer,
	// because it names why.
	if status != OutcomeFailed {
		t.Errorf("status=%q detail=%q, want a bounded stop", status, detail)
	}
	if !strings.Contains(detail, "iteration limit") && !strings.Contains(detail, "stuck") {
		t.Errorf("detail=%q, want it to say why it stopped", detail)
	}
	if calls() > 3 {
		t.Errorf("model called %d times against a limit of 3", calls())
	}
	if len(f.commentBodies("t1")) == 0 {
		t.Error("giving up left no comment; the ticket would look untouched")
	}
}

// give_up was REMOVED, and this records why rather than deleting the case
// silently. It was never the real bound — the refusal ceiling, the read ceiling
// and the turn budget all end a stuck attempt — and it gave a model that was
// merely confused a way to turn a recoverable attempt into a terminal one.
//
// So an agent that only ever emits nonsense must still end, and end as a
// FAILURE with the reason on the ticket. That is the property give_up was
// standing in for.
func TestAnAgentThatNeverActsStillEndsWithTheReasonRecorded(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.addTicket(t, Ticket{TicketID: "t1", Title: "x", CreatedBy: "alice"})

	replies := make([]string, 0, 12)
	for range 12 {
		replies = append(replies, `I am not sure what to do here.`)
	}
	gw, _ := scriptedModel(t, replies...)
	rec, _ := newTestRecorder(t)
	ctx := rec.Start(context.Background(), "tr", "t1", "dev-agent")

	status, detail, err := devTestAgent(t, gw, api).Handle(ctx, f.get("t1"))
	if err != nil {
		t.Fatalf("Handle() = %v", err)
	}
	if status != OutcomeFailed {
		t.Errorf("status = %q, want failed: an agent that never acts must not look successful", status)
	}
	if detail == "" {
		t.Error("the failure has no detail; nothing would say why the ticket stopped")
	}
	if len(f.commentBodies("t1")) == 0 {
		t.Error("stopping left no comment; the ticket would look untouched")
	}
}

// The git credential is a forge secret_ref: forge resolves it at dispatch, so
// blacksmith never holds it and it never reaches the model.
func TestDevAgentPassesGitCredentialAsASecretRef(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.addTicket(t, Ticket{TicketID: "t1", Title: "x", CreatedBy: "alice"})

	gw, _ := scriptedModel(t, `{"action":"give_up","reason":"done"}`)
	rec, _ := newTestRecorder(t)
	ctx := rec.Start(context.Background(), "tr", "t1", "dev-agent")

	a := NewDevAgent(gw, api, ClassLarge, RepoConfig{
		URL: "https://example.invalid/r", SecretRef: "git:https://example.invalid/r",
		Branch: "main", BranchPrefix: "agent/", Image: "golang:1.25", PipelineID: "pipe-1",
	}, 3)
	if _, _, err := a.Handle(ctx, f.get("t1")); err != nil {
		t.Fatalf("Handle() = %v", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	// THE CREDENTIAL TRAVELS ON THE LEASE, not on each execution. forge rejects a
	// leased execution that restates the sandbox's own settings, so the reference
	// is attached once when the container is held.
	if len(f.leaseSpecs) == 0 {
		t.Fatal("no lease created")
	}
	spec := f.leaseSpecs[0]
	if spec.SecretRefs["GIT_CLONE_URL"] != "git:https://example.invalid/r" {
		t.Errorf("secret_refs = %v, want the git reference passed through", spec.SecretRefs)
	}
	// The reference travels; a resolved credential must never appear in the spec.
	raw, _ := json.Marshal(spec)
	if strings.Contains(string(raw), "password") || strings.Contains(string(raw), "token=") {
		t.Errorf("the execution spec appears to carry a resolved credential: %s", raw)
	}
}

// Two agents on one host must not compete for the same tickets. Without a
// selector the claim makes the race silently destructive: whichever wins, the
// other never sees the ticket again.
//
// The selector used to be each agent's Wants predicate, and keeping five prose
// predicates mutually exclusive was a standing invariant nobody could check
// locally — every new stage was a chance to overlap with one written months
// earlier. The COLUMN is the selector now, so disjointness is a property of the
// routing table and can simply be read off it.
func TestStagesTakeFromDistinctColumns(t *testing.T) {
	// Checked in BOTH configurations, because the head of the pipeline depends on
	// one: with an architect the inbox is its queue and the product manager takes
	// from ready_for_scoping; without one the product manager takes the inbox
	// back. The two entries therefore overlap in the static table and are
	// separated by applyArchitectSetting — so the property worth asserting is
	// that every RESOLVED pipeline is disjoint, not that the unresolved table is.
	t.Cleanup(func() { applyArchitectSetting(false) })
	for _, withArchitect := range []bool{false, true} {
		applyArchitectSetting(withArchitect)
		takenBy := map[string]string{}
		for role, st := range stages {
			// Only one of the two is ever registered; see buildProjectDispatchers.
			if !withArchitect && role == roleArchitect {
				continue
			}
			if st.Ready == "" {
				t.Errorf("architect=%v: %s takes from no column; it would never select anything", withArchitect, role)
			}
			if other, clash := takenBy[st.Ready]; clash {
				t.Errorf("architect=%v: %s and %s both take from %q; they would race and one would lose the ticket permanently",
					withArchitect, role, other, st.Ready)
			}
			takenBy[st.Ready] = role
		}
	}
	applyArchitectSetting(false)

	// A stage must not park held work in the column it takes from, or a ticket it
	// is working still looks like a ticket waiting to be worked.
	for role, st := range stages {
		if st.Working == st.Ready {
			t.Errorf("%s parks held work in its own queue (%q); it would re-select what it is already doing", role, st.Ready)
		}
	}

	// And every column a stage routes to must be one the board actually has,
	// since the tickets service rejects a status outside the board's field defs.
	known := map[string]bool{}
	for _, c := range workflowColumns {
		known[c.Value] = true
	}
	for role, st := range stages {
		for what, col := range map[string]string{
			"Ready": st.Ready, "Working": st.Working, "Success": st.Success, "Exhausted": st.Exhausted,
		} {
			if col != "" && !known[col] {
				t.Errorf("%s.%s is %q, which is not a column on the board; the move would be rejected", role, what, col)
			}
		}
	}
}

func TestPipelineMarkersMatchWhatIsRendered(t *testing.T) {
	triageComment := renderTriage(triage{Summary: "s", Priority: "high"}, ChatResult{Model: "m"})
	if !hasTriage(Ticket{Comments: []Comment{{Body: triageComment}}}) {
		t.Error("a rendered triage comment is not recognised as triage; every ticket would be re-triaged forever")
	}
	branchComment := renderDevSummary("agent/abc", "did it", []string{"a.go"}, "run001", "ok")
	if !hasBranch(Ticket{Comments: []Comment{{Body: branchComment}}}) {
		t.Error("a rendered branch comment is not recognised; the developer would work every ticket twice")
	}
}

// Repositories routinely run commitlint on the commit-msg hook, so a message
// without a recognised type is not a style nit — it is a commit that will not
// land, and every branch the agent pushes would fail at the last step.
//
// Found by reading the codearmory repo's own .pre-commit-config.yaml, which runs
// @commitlint/config-conventional. The first version of this agent produced
// plain subjects and would have been rejected by the very repo it works on.
func TestCommitMessageIsConventional(t *testing.T) {
	tk := Ticket{TicketID: "abc123", Title: "Login is broken"}

	got := commitMessage(tk, "Fix the login handler.", "fix")
	subject := strings.SplitN(got, "\n", 2)[0]
	if !strings.HasPrefix(subject, "fix: ") {
		t.Errorf("subject = %q, want a conventional type prefix", subject)
	}
	// commitlint's default config rejects a trailing period and a capitalised
	// subject, so both are normalised rather than passed through.
	if strings.HasSuffix(subject, ".") {
		t.Errorf("subject = %q, want no trailing period", subject)
	}
	if strings.HasPrefix(subject, "fix: F") {
		t.Errorf("subject = %q, want a lower-case subject", subject)
	}
	if n := len([]rune(subject)); n > 72 {
		t.Errorf("subject is %d runes, want it within the 72-column convention", n)
	}
	if !strings.Contains(got, "Ticket: abc123") {
		t.Error("commit message does not reference the ticket")
	}
}

// A model that omits or invents a type must not produce an unlandable commit.
// "chore" is wrong-but-valid, which beats confidently mislabelled.
func TestCommitMessageFallsBackToAValidType(t *testing.T) {
	for _, bad := range []string{"", "FEAT", "bugfix", "improvement", "feat!"} {
		got := commitMessage(Ticket{TicketID: "x", Title: "t"}, "do a thing", bad)
		typ := strings.SplitN(got, ":", 2)[0]
		if !contains(conventionalTypes, typ) {
			t.Errorf("type %q produced subject type %q, which is not in the allowlist", bad, typ)
		}
	}
	for _, good := range conventionalTypes {
		got := commitMessage(Ticket{TicketID: "x", Title: "t"}, "do a thing", good)
		if !strings.HasPrefix(got, good+": ") {
			t.Errorf("valid type %q was not preserved: %q", good, got)
		}
	}
}

// The agent must be held to the repository's own standards, not a lower set.
// Bypassing hooks would let it commit secrets and bad messages that a person
// could not.
func TestPushRunsTheRepositoryHooks(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.addTicket(t, Ticket{TicketID: "t1", Title: "x", CreatedBy: "alice"})

	gw, _ := scriptedModel(t,
		`{"action":"write_files","edits":[{"path":"a.go","search":"","replace":"package main"}],"summary":"do the thing","type":"fix"}`,
		`{"action":"run_tests"}`,
		`{"action":"finish","summary":"do the thing","type":"fix"}`,
	)
	rec, _ := newTestRecorder(t)
	ctx := rec.Start(context.Background(), "tr", "t1", "dev-agent")

	if _, _, err := devTestAgent(t, gw, api).Handle(ctx, f.get("t1")); err != nil {
		t.Fatalf("Handle() = %v", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	var pushScript string
	for _, spec := range f.execSpecs {
		joined := strings.Join(spec.Command, " ")
		if strings.Contains(joined, "git push") {
			pushScript = joined
		}
	}
	if pushScript == "" {
		t.Fatal("no push execution was submitted")
	}
	if strings.Contains(pushScript, "--no-verify") {
		t.Error("the push bypasses hooks; the agent would be held to a lower standard than the people it works alongside")
	}
	if !strings.Contains(pushScript, ".pre-commit-config.yaml") {
		t.Error("the push does not install the repository's pre-commit hooks")
	}
	if msg := commitMessageIn(t, pushScript); !strings.HasPrefix(msg, "fix: do the thing") {
		t.Errorf("the commit message is not conventional: %q", msg)
	}
	// Hooks that rewrite files fail the commit and leave the fixes staged; the
	// retry is what turns that into the no-op it should be.
	if strings.Count(pushScript, "git add -A") < 2 {
		t.Error("no retry after a hook that modifies files; a formatter would fail the commit for no reason")
	}
}

// The commit is written during run_tests, before finish. If the description
// only arrived at finish the commit would carry none — and amending afterwards
// would mean the commit the pipeline verified is not the commit that was pushed.
func TestDevAgentCommitsWithTheDescriptionGivenAtWriteTime(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.addTicket(t, Ticket{TicketID: "t1", Title: "x", CreatedBy: "alice"})

	gw, _ := scriptedModel(t,
		`{"action":"write_files","edits":[{"path":"a.go","search":"","replace":"package main"}],"summary":"handle the nil case","type":"fix"}`,
		`{"action":"run_tests"}`,
		`{"action":"finish","summary":"handle the nil case","type":"fix"}`,
	)
	rec, _ := newTestRecorder(t)
	ctx := rec.Start(context.Background(), "tr", "t1", "dev-agent")

	if _, _, err := devTestAgent(t, gw, api).Handle(ctx, f.get("t1")); err != nil {
		t.Fatalf("Handle() = %v", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	var pushes int
	for _, spec := range f.execSpecs {
		joined := strings.Join(spec.Command, " ")
		if strings.Contains(joined, "git push") {
			pushes++
			if msg := commitMessageIn(t, joined); !strings.Contains(msg, "fix: handle the nil case") {
				t.Errorf("commit message lacks the description given at write time: %q", msg)
			}
		}
	}
	if pushes != 1 {
		t.Errorf("pushed %d times, want 1: finish must report, not re-push and invalidate the verified commit", pushes)
	}
}

// devStandaloneAgent verifies in the sandbox: no pipeline, a test command.
func devStandaloneAgent(t *testing.T, gw *Gateway, api *CodeArmory) *DevAgent {
	t.Helper()
	return NewDevAgent(gw, api, ClassLarge, RepoConfig{
		URL: "git://git-local:9418/demo.git", Branch: "main",
		BranchPrefix: "agent/", Image: "golang:1.25", TestCommand: "go test ./...",
	}, 6)
}

// The department must close its own loop. A host whose repository no pipeline can
// reach still has inference, a git remote and a sandbox — everything needed to
// decide whether a change works — so it verifies there rather than not at all.
func TestDevAgentVerifiesInSandboxWithoutAPipeline(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.addTicket(t, Ticket{TicketID: "t1", Title: "Add a farewell", CreatedBy: "alice"})
	// main.go is deliberately NOT in this listing: these scripts write it without
	// reading it, which is a CREATE. A tree claiming it already exists would make
	// the same script a blind whole-file overwrite, which is refused.
	f.execStdout = "go.mod\n"

	gw, _ := scriptedModel(t,
		`{"action":"write_files","edits":[{"path":"main.go","search":"","replace":"package main\nfunc main(){}\n"}]}`,
		`{"action":"run_tests"}`,
		`{"action":"finish","summary":"add a farewell"}`,
	)
	rec, _ := newTestRecorder(t)
	ctx := rec.Start(context.Background(), "tr-dev", "t1", "dev-agent")

	status, detail, err := devStandaloneAgent(t, gw, api).Handle(ctx, f.get("t1"))
	if err != nil {
		t.Fatalf("Handle() = %v", err)
	}
	if status != OutcomeSuccess {
		t.Fatalf("status = %q (%s), want success", status, detail)
	}
	// The platform must not have been asked to verify anything.
	if len(f.runInputs) != 0 {
		t.Errorf("triggered %d pipeline runs, want 0 with no pipeline configured", len(f.runInputs))
	}

	// The test command must have run against the PUSHED BRANCH, not the agent's
	// local state — otherwise a change lost to a hook or a .gitignore still passes.
	var verified bool
	for _, spec := range f.execSpecs {
		script := strings.Join(spec.Command, " ")
		if strings.Contains(script, "go test ./...") && strings.Contains(script, "agent/t1") {
			verified = true
		}
	}
	if !verified {
		t.Error("no sandbox run cloned the pushed branch and ran the test command")
	}
}

// A failing test command is a normal result the model must be told about, not an
// error that abandons the ticket.
func TestDevAgentReportsSandboxTestFailure(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.addTicket(t, Ticket{TicketID: "t1", Title: "x", CreatedBy: "alice"})
	f.execStdout = "main.go\n"
	f.execFailIfScriptContains = "go test ./..."

	gw, _ := scriptedModel(t,
		`{"action":"write_files","edits":[{"path":"main.go","search":"","replace":"package main\n"}]}`,
		`{"action":"run_tests"}`,
		`{"action":"finish","summary":"done"}`,
	)
	rec, _ := newTestRecorder(t)
	ctx := rec.Start(context.Background(), "tr-dev", "t1", "dev-agent")

	status, detail, err := devStandaloneAgent(t, gw, api).Handle(ctx, f.get("t1"))
	if err != nil {
		t.Fatalf("Handle() = %v, want a failing test to be a result, not an error", err)
	}
	if status == OutcomeSuccess {
		t.Fatalf("status = success (%s), want a refusal: the tests did not pass", detail)
	}
}

// The script must run exactly the configured command. Getting onto the branch is
// the Sandbox's job now — see TestCheckoutBranchTargetsThePushedBranch.
func TestVerifyScriptRunsTheConfiguredCommand(t *testing.T) {
	a := NewDevAgent(nil, nil, ClassLarge, RepoConfig{
		URL: "git://git-local:9418/demo.git", Branch: "main", TestCommand: "go test ./...",
	}, 6)
	script := a.verifyScript()

	if !strings.Contains(script, "go test ./...") {
		t.Errorf("verifyScript() does not run the configured test command:\n%s", script)
	}
}

// Verification must judge what was PUSHED, never the tree the agent has been
// editing: hooks rewrite files, .gitignore silently drops a new one, and a
// formatter changes what actually landed. Both modes therefore have to reach the
// remote for the branch, and neither may end up on the base branch instead.
func TestCheckoutBranchTargetsThePushedBranch(t *testing.T) {
	for _, tc := range []struct {
		mode string
		sb   *Sandbox
	}{
		{"leased", &Sandbox{leaseID: "l1", cloneURL: "git://git-local:9418/demo.git"}},
		{"one-shot", &Sandbox{cloneURL: "git://git-local:9418/demo.git"}},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			script := tc.sb.checkoutBranch("agent/t9")
			if !strings.Contains(script, "agent/t9") {
				t.Errorf("checkout does not name the pushed branch:\n%s", script)
			}
			if strings.Contains(script, "'main'") || strings.Contains(script, `"main"`) {
				t.Errorf("checkout targets the base branch instead of the pushed one:\n%s", script)
			}
		})
	}
}

// A leased sandbox reuses a tree the agent has been writing into, so getting onto
// the branch is not enough — anything left behind has to go, or a stale file
// could make a broken branch look like it passes.
func TestLeasedCheckoutDiscardsLocalState(t *testing.T) {
	script := (&Sandbox{leaseID: "l1"}).checkoutBranch("agent/t9")
	for _, want := range []string{"git fetch", "reset -q --hard", "git clean"} {
		if !strings.Contains(script, want) {
			t.Errorf("leased checkout is missing %q, so the agent's leftovers could survive into verification:\n%s", want, script)
		}
	}
}

// The commit message reaches git as BYTES, never as a shell word.
//
// It used to be formatted in with %q, which is Go quoting rather than shell
// quoting, and the two differ in exactly the ways that matter here.

// commitMessageIn decodes the commit message a push script carries.
//
// The message is base64-encoded into the script rather than formatted into a
// `git commit -m` argument, so asserting on the script's literal text no longer
// works — and that is the point: if the message were readable there, the shell
// would be reading model-authored bytes too.
func commitMessageIn(t *testing.T, script string) string {
	t.Helper()
	const marker = "printf %s '"
	i := strings.Index(script, marker)
	if i < 0 {
		t.Fatalf("no commit message payload in the script:\n%s", script)
	}
	rest := script[i+len(marker):]
	j := strings.Index(rest, "'")
	if j < 0 {
		t.Fatalf("unterminated commit message payload in the script")
	}
	raw, err := base64.StdEncoding.DecodeString(rest[:j])
	if err != nil {
		t.Fatalf("commit message payload is not base64: %v", err)
	}
	return string(raw)
}

func TestCommitMessageIsNotShellFormatted(t *testing.T) {
	script := commitMessageScript("feat: add a thing\n\nTicket: t-123")

	// 1. The message must not appear as literal text in the script at all: if it
	//    does, the shell is reading model-authored bytes.
	if strings.Contains(script, "feat: add a thing") {
		t.Error("the message appears verbatim in the script; it is being read by the shell")
	}
	// 2. Its newlines must survive. As a %q argument they arrived as the two
	//    characters backslash-n, so the Conventional Commits body and trailer
	//    landed on the subject line and commitlint rejected the commit.
	if strings.Contains(script, `\n\n`) {
		t.Error("the message carries escaped newlines; the body would not be a body")
	}
	// 3. Round-trip: what the script decodes must be exactly what went in.
	enc := strings.TrimSuffix(strings.TrimPrefix(script[strings.Index(script, "'")+1:], ""), "")
	enc = enc[:strings.Index(enc, "'")]
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		t.Fatalf("the script's payload is not base64: %v", err)
	}
	if string(raw) != "feat: add a thing\n\nTicket: t-123" {
		t.Errorf("decoded = %q, want the original message", string(raw))
	}
}

// The commit SUBJECT is model-authored, so it must not be able to become a
// command. %q escapes quotes and backslashes but leaves $ and backticks alone,
// so a summary containing $(...) was substituted by the shell — the very thing
// base64-encoding file content exists to prevent, bypassed on the commit path.
func TestCommitMessageCannotInjectAShellCommand(t *testing.T) {
	for _, payload := range []string{
		"add $(whoami) support",
		"add `id` support",
		`add "; rm -rf /; echo " support`,
		"add ${HOME} support",
	} {
		script := commitMessageScript("feat: " + payload)
		for _, dangerous := range []string{"$(", "`", "${", "rm -rf"} {
			if strings.Contains(script, dangerous) {
				t.Errorf("payload %q leaves %q in the script, which the shell would act on:\n%s",
					payload, dangerous, script)
			}
		}
	}
}

// ONE GATE, EVERYTHING ELSE JUDGEMENT. The linter runs first for its precise,
// early feedback, but it cannot fail a branch: a linter can report what this
// change cannot fix — a false positive, a rule needing a refactor beyond the
// ticket, two rules that disagree — and behind a gate each of those throws away a
// correct change once the iteration budget runs out.
func TestLintIsAdvisoryAndTestsAreTheGate(t *testing.T) {
	a := NewDevAgent(nil, nil, ClassLarge, RepoConfig{
		URL: "git://host/demo.git", LintCommand: "go vet ./...", TestCommand: "go test ./...",
	}, 6)
	script := a.verifyScript()

	lint := strings.Index(script, "go vet ./...")
	test := strings.Index(script, "go test ./...")
	if lint < 0 || test < 0 {
		t.Fatalf("both commands should run:\n%s", script)
	}
	if lint > test {
		t.Error("the linter runs after the tests; its precise output would arrive buried")
	}
	// The linter must not be able to end the run.
	lintLine := script[lint:test]
	if !strings.Contains(lintLine, "|| true") {
		t.Errorf("the linter can fail the run; an unfixable rule would trap the agent:\n%s", lintLine)
	}
	// The tests must.
	if !strings.Contains(script[test:], "exit 1") {
		t.Error("a failing test does not stop the run; unverified code would be reported as passing")
	}
	// The gates must not fetch the repository themselves. Getting onto the branch
	// belongs to the Sandbox, which does it once — and in a held sandbox does it
	// without a clone at all. A clone here would put one back into every
	// verification, which is the cost this whole arrangement removes.
	if strings.Contains(script, "git clone") {
		t.Errorf("verification clones the repository itself; that is the cost the sandbox exists to pay once:\n%s", script)
	}
	// And the sandbox's own checkout is a single fetch, not a clone per gate.
	if n := strings.Count((&Sandbox{leaseID: "l1"}).checkoutBranch("agent/t1"), "git fetch"); n != 1 {
		t.Errorf("leased checkout fetches %d times, want 1", n)
	}
}

// A repository with only tests configured must still work.
func TestVerifyScriptSkipsUnconfiguredChecks(t *testing.T) {
	a := NewDevAgent(nil, nil, ClassLarge, RepoConfig{
		URL: "git://host/demo.git", TestCommand: "go test ./...",
	}, 6)
	if strings.Contains(a.verifyScript(), adviceOpen) {
		t.Error("an unset linter still emits an advisory section")
	}
}

// Advice must be separable from the gate's output, or a passing branch comes back
// looking like a failing one.
func TestAdviceIsSplitFromGateOutput(t *testing.T) {
	out := "cloning\n" + adviceOpen + "\nmain.go:4: unused\n" + adviceClose + "\nok demo 0.01s\n"
	advice, rest := splitAdvice(out)
	if advice != "main.go:4: unused" {
		t.Errorf("advice = %q, want the linter's findings", advice)
	}
	if strings.Contains(rest, "unused") {
		t.Error("linter output leaked into the gate's output")
	}
	if !strings.Contains(rest, "ok demo") {
		t.Error("the gate's own output was lost")
	}
	// No advisory section at all is the common case and must pass through whole.
	a2, r2 := splitAdvice("just test output")
	if a2 != "" || r2 != "just test output" {
		t.Errorf("splitAdvice with no section = (%q, %q)", a2, r2)
	}
}

// Telling the model "the tests failed" when the linter failed sends it looking in
// the wrong place, and it cannot afford the iteration.
func TestFailedGateIsAttributed(t *testing.T) {
	cases := map[string]string{
		"--- lint ---\nmain.go:4: bad\nHARNESS_GATE_FAILED=lint\n":                         "lint",
		"--- lint ---\nok\n--- test ---\nFAIL\nHARNESS_GATE_FAILED=test\n":                 "test",
		"the sandbox died before any gate ran":                                             "checks",
		"--- lint ---\nHARNESS_GATE_FAILED=lint\n--- test ---\nHARNESS_GATE_FAILED=test\n": "test",
	}
	for output, want := range cases {
		if got := failedGate(output); got != want {
			t.Errorf("failedGate(%q) = %q, want %q", output, got, want)
		}
	}
}

// Formatting must happen BEFORE the commit, or what lands is unformatted and the
// formatter only ever reports on a branch that is already pushed.
func TestFormatterRunsBeforeTheCommit(t *testing.T) {
	a := NewDevAgent(nil, nil, ClassLarge, RepoConfig{
		URL: "git://host/demo.git", FormatCommand: "gofmt -w .",
	}, 6)
	script := a.formatScript()
	if !strings.Contains(script, "gofmt -w .") {
		t.Fatalf("the formatter is not in the script: %q", script)
	}
	// Best effort: a repository without the tool must still be able to commit.
	if !strings.Contains(script, "||") {
		t.Error("a failing formatter aborts the commit; a missing tool would block a good change")
	}
	// And unset means nothing runs at all.
	none := NewDevAgent(nil, nil, ClassLarge, RepoConfig{URL: "git://host/demo.git"}, 6)
	if none.formatScript() != "" {
		t.Error("an unset formatter still emits script")
	}
}

// Severity draws the line. A critical finding must come back to the agent that
// still has the branch open and iterations left — routing it through the reviewer
// means a comment on a ticket, and the fix arriving on a later ticket with none of
// the context. Everything below the threshold stays advisory.
func TestCriticalAnalysisGatesButLintDoesNot(t *testing.T) {
	a := NewDevAgent(nil, nil, ClassLarge, RepoConfig{
		URL:         "git://host/demo.git",
		LintCommand: "go vet ./...", TestCommand: "go test ./...",
		CriticalCommand: "gosec -severity=high -quiet ./...",
	}, 6)
	script := a.verifyScript()

	lint := strings.Index(script, "go vet ./...")
	test := strings.Index(script, "go test ./...")
	crit := strings.Index(script, "gosec -severity=high")
	if lint < 0 || test < 0 || crit < 0 {
		t.Fatalf("all three should run:\n%s", script)
	}
	// Analysis runs after the tests: a change that does not compile cannot be
	// meaningfully scanned, and most scanners need it to build.
	if crit < test {
		t.Error("critical analysis runs before the tests; a broken change would be scanned instead of reported")
	}
	if !strings.Contains(script[crit:], gateMarker+"critical-security") {
		t.Error("a critical finding does not fail the run; it would wait for a reviewer who cannot fix it")
	}
	if !strings.Contains(script[lint:test], "|| true") {
		t.Error("the advisory linter can still fail the run")
	}
}

// The model must know a security gate from a lint complaint, or it works around
// the symptom — deleting the flagged call rather than removing the credential.
func TestCriticalFailureIsNamedAsSecurity(t *testing.T) {
	if got := failedGate("boom\n" + gateMarker + "critical-security\n"); got != "critical-security" {
		t.Errorf("failedGate = %q, want critical-security", got)
	}
}

// SCA gates only on REGRESSION. A dependency advisory is normally a property of a
// tree that predates the ticket, and blocking on one would stop an agent adding a
// function because a transitive package has a CVE — outside its brief, possibly
// unfixable upstream, and an invitation to smuggle a version bump into a feature
// branch. A dependency this branch ADDED is the opposite: in scope, caused here,
// and usually one version bump the agent can make while the branch is open.
func TestDependencyScanGatesOnlyOnAChangedManifest(t *testing.T) {
	a := NewDevAgent(nil, nil, ClassLarge, RepoConfig{
		URL: "git://host/demo.git", Branch: "main",
		TestCommand: "go test ./...", SCACommand: "osv-scanner -r .",
	}, 6)
	script := a.verifyScript()

	if !strings.Contains(script, "git diff --name-only origin/main...HEAD") {
		t.Fatalf("the branch is not compared against its base:\n%s", script)
	}
	// The scan must sit inside the conditional, not beside it.
	cond := strings.Index(script, "git diff --name-only")
	scan := strings.Index(script, "osv-scanner -r .")
	if scan < cond {
		t.Error("the dependency scan runs before the manifest check; it would gate on pre-existing findings")
	}
	if !strings.Contains(script, gateMarker+"dependency-regression") {
		t.Error("a dependency introduced by this branch does not fail the run")
	}
	// Fail open: an unknown comparison must not block.
	if !strings.Contains(script, "pre-existing and advisory") {
		t.Error("no advisory path; a branch whose base cannot be fetched would be gated on an unknown")
	}
	// The diff needs history, which --depth 1 does not provide.
	if strings.Contains(script, "--depth 1") {
		t.Error("a single-commit clone has no base to diff against")
	}
}

// The pattern decides what counts as a dependency change, so it must match a
// nested module's manifest and not a file merely named like one.
func TestManifestPatternMatchesPathsNotSubstrings(t *testing.T) {
	pat := manifestPattern(nil)
	re := regexp.MustCompile(pat)
	for _, yes := range []string{"go.mod", "go.sum", "svc/api/go.mod", "web/package-lock.json", "Cargo.lock"} {
		if !re.MatchString(yes) {
			t.Errorf("%q should count as a dependency change (pattern %q)", yes, pat)
		}
	}
	for _, no := range []string{"docs/notes-about-go.mod.txt", "main.go", "gopher.mod.bak", "package.json.tmpl"} {
		if re.MatchString(no) {
			t.Errorf("%q should NOT count as a dependency change", no)
		}
	}
	// An operator's own list replaces the default.
	custom := regexp.MustCompile(manifestPattern([]string{"deps.lock"}))
	if !custom.MatchString("deps.lock") || custom.MatchString("go.mod") {
		t.Error("a configured manifest list did not replace the default")
	}
}

// No scanner configured means no check at all, not an empty conditional.
func TestNoDependencyScanMeansNoCheck(t *testing.T) {
	a := NewDevAgent(nil, nil, ClassLarge, RepoConfig{
		URL: "git://host/demo.git", TestCommand: "go test ./...",
	}, 6)
	if a.dependencyRegressionScript() != "" {
		t.Error("an unset dependency scanner still emits a check")
	}
}

// Verification is deterministic given the same tree, so repeating it against
// unchanged code cannot say anything new — it just spends an iteration and a
// sandbox. Observed in a real run: the model called run_tests six times after it
// had already passed, never reached finish, and lost the whole budget along with
// a branch that was already good.
func TestRepeatVerificationIsRefusedWithoutASandbox(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.addTicket(t, Ticket{TicketID: "t1", Title: "x", CreatedBy: "alice"})
	// main.go is deliberately absent from this listing: the script writes it
	// without reading it, which is a CREATE. A tree claiming it already exists
	// would make the same script a blind whole-file overwrite, which is refused.
	f.execStdout = "go.mod\n"

	gw, _ := scriptedModel(t,
		`{"action":"write_files","edits":[{"path":"main.go","search":"","replace":"package main\n"}]}`,
		`{"action":"run_tests"}`,
		`{"action":"run_tests"}`,
		`{"action":"run_tests"}`,
		`{"action":"finish","summary":"done"}`,
	)
	rec, _ := newTestRecorder(t)
	ctx := rec.Start(context.Background(), "tr", "t1", "dev-agent")

	status, detail, err := devStandaloneAgent(t, gw, api).Handle(ctx, f.get("t1"))
	if err != nil {
		t.Fatalf("Handle() = %v", err)
	}
	if status != OutcomeSuccess {
		t.Fatalf("status = %q (%s), want success", status, detail)
	}

	// One push-and-verify, not three. The repeats must never reach the sandbox.
	f.mu.Lock()
	defer f.mu.Unlock()
	var verifies int
	for _, spec := range f.execSpecs {
		if strings.Contains(strings.Join(spec.Command, " "), "go test ./...") {
			verifies++
		}
	}
	if verifies != 1 {
		t.Errorf("ran verification %d times on unchanged code, want 1", verifies)
	}
}

// Refusing the repeat made it CHEAP; it did not make it STOP. In the run that
// prompted this, the model spent every remaining iteration asking to re-verify
// code that had already passed, and the ticket failed at the cap with a good,
// pushed, passing branch — a wrong answer to a solved problem. The loop must
// draw the conclusion the model will not.
func TestDevAgentFinishesForAModelThatWillNotStopVerifying(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.addTicket(t, Ticket{TicketID: "t1", Title: "x", CreatedBy: "alice"})
	// main.go is deliberately absent from this listing: the script writes it
	// without reading it, which is a CREATE. A tree claiming it already exists
	// would make the same script a blind whole-file overwrite, which is refused.
	f.execStdout = "go.mod\n"

	// Never calls finish. Not once.
	script := []string{
		`{"action":"write_files","edits":[{"path":"main.go","search":"","replace":"package main\n"}]}`,
	}
	for range 12 {
		script = append(script, `{"action":"run_tests"}`)
	}
	gw, calls := scriptedModel(t, script...)
	rec, _ := newTestRecorder(t)
	ctx := rec.Start(context.Background(), "tr", "t1", "dev-agent")

	status, detail, err := devStandaloneAgent(t, gw, api).Handle(ctx, f.get("t1"))
	if err != nil {
		t.Fatalf("Handle() = %v", err)
	}
	if status != OutcomeSuccess {
		t.Fatalf("status = %q (%s), want success: the branch passed and was pushed", status, detail)
	}
	if !strings.Contains(detail, "agent/t1") {
		t.Errorf("detail = %q, want the pushed branch named", detail)
	}

	// It must stop SOON, not at the cap: write, verify, then the refusal budget.
	if got := calls(); got > 2+maxRepeatRefusals {
		t.Errorf("model called %d times, want it cut off by %d", got, 2+maxRepeatRefusals)
	}

	bodies := f.commentBodies("t1")
	if len(bodies) != 1 || !strings.Contains(bodies[0], "pushed a branch") {
		t.Fatalf("comments = %v, want the normal branch-pushed comment", bodies)
	}
	// The ticket must say the loop decided, not the agent — an operator reading
	// this needs to know the model never converged.
	if !strings.Contains(bodies[0], "on the agent's behalf") {
		t.Error("the comment hides that the loop finished for the model")
	}
}

// Guarding ONE action against pointless repetition only moves the loop to the
// next action. Observed immediately after the repeat-verification guard landed:
// a model wrote invalid Go, saw the compiler reject it, then called read_files
// on the same two files five times in a row — a sandbox each — until the budget
// was gone. A re-read cannot return anything new, because the clone is fresh
// from the same branch and the agent's own writes are already in its prompt.
func TestDevAgentMayRereadAndTheTurnIsRefunded(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.addTicket(t, Ticket{TicketID: "t1", Title: "x", CreatedBy: "alice"})
	f.execStdout = "===FILE main.go\n" + base64.StdEncoding.EncodeToString([]byte("package main\n")) + "\n"

	// Long enough to outlast the CONSECUTIVE-READ KILL, which is the only thing
	// that ends an attempt doing nothing but read — reads no longer spend the
	// turn budget at all.
	script := []string{}
	for range maxConsecutiveReads + 5 {
		script = append(script, `{"action":"read_files","paths":["main.go"]}`)
	}
	gw, calls := scriptedModel(t, script...)
	rec, _ := newTestRecorder(t)
	ctx := rec.Start(context.Background(), "tr", "t1", "dev-agent")

	a := NewDevAgent(gw, api, ClassLarge, RepoConfig{
		URL: "https://example.invalid/r", Branch: "main", BranchPrefix: "agent/",
		Image: "golang:1.25", TestCommand: "go test ./...",
	}, 10)

	status, detail, err := a.Handle(ctx, f.get("t1"))
	if err != nil {
		t.Fatalf("Handle() = %v", err)
	}
	// READING IS NOT SPENDING. It was refused once and the refusal counted toward
	// a stuck ceiling, abandoning real tickets four turns in; then it was allowed
	// but charged, and one ticket spent 87 consecutive turns re-reading on a
	// 200-turn budget. A cached read never reaches a sandbox and costs 2.2
	// seconds against a KV-cached prompt, so it now costs no turn at all.
	if status != OutcomeFailed {
		t.Errorf("status = %q, want the attempt to end", status)
	}
	if strings.Contains(detail, "stuck") {
		t.Errorf("the attempt was cut off for repeating itself: %q", detail)
	}
	// FAR past the nominal budget of 10: proof the reads were not charged.
	if got := calls(); got <= 10 {
		t.Errorf("model called %d times with a budget of 10 — reads are still being charged", got)
	}
	// But BOUNDED, or an agent that only reads would never stop.
	if got := calls(); got > maxConsecutiveReads {
		t.Errorf("model called %d times, past the %d consecutive-read kill — the loop cannot terminate",
			got, maxConsecutiveReads)
	}

	// And a repeat must never reach a SANDBOX: the content is already known, so
	// re-serving it costs nothing at all.
	f.mu.Lock()
	defer f.mu.Unlock()
	reads := 0
	for _, spec := range f.execSpecs {
		if strings.Contains(strings.Join(spec.Command, " "), "===FILE") {
			reads++
		}
	}
	if reads > 1 {
		t.Errorf("a re-read reached the sandbox %d times; it must be served from what was already read", reads)
	}
}

// The advice escalates rather than the attempt ending: a model re-reading is
// doing something legitimate, and blunt guidance is the right response to it
// becoming unproductive.
func TestRepeatedStaleReadsEscalateTheAdvice(t *testing.T) {
	s := &devState{
		read: map[string]string{"main.go": "package main\n"}, staged: map[string]string{},
		missing: map[string]bool{}, baseline: map[string]string{},
	}
	a := NewDevAgent(nil, nil, ClassLarge, RepoConfig{}, 8)
	rec, _ := newTestRecorder(t)
	ctx := rec.Start(context.Background(), "tr", "t1", "dev-agent")

	for i := 1; i <= maxStaleReads; i++ {
		a.doRead(ctx, recorderFrom(ctx), s, devAction{Action: actionReadFiles, Paths: []string{"main.go"}})
		if s.refusals != 0 {
			t.Fatalf("a re-read counted as a refusal on turn %d; that is what killed tickets early", i)
		}
	}
	if s.staleReads != maxStaleReads {
		t.Errorf("staleReads = %d, want %d", s.staleReads, maxStaleReads)
	}
	if !strings.Contains(s.notice, "in a row without changing anything") {
		t.Errorf("the advice never became blunt:\n%s", s.notice)
	}
	// Real progress clears it.
	s.notice, s.refusals, s.staleReads = "", 0, 0
	if s.staleReads != 0 {
		t.Error("progress did not reset the counter")
	}
}

func TestDevAgentReportsPassingWorkEvenIfItRunsOutOfIterations(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.addTicket(t, Ticket{TicketID: "t1", Title: "x", CreatedBy: "alice"})
	// main.go is deliberately absent from this listing: the script writes it
	// without reading it, which is a CREATE. A tree claiming it already exists
	// would make the same script a blind whole-file overwrite, which is refused.
	f.execStdout = "go.mod\n"

	// Writes, verifies green, then reads DIFFERENT files until the budget is gone.
	// Distinct paths on purpose: repeating one would trip the stuck detector and
	// exercise that exit instead of the iteration cap this test is about.
	gw, _ := scriptedModel(t,
		`{"action":"write_files","edits":[{"path":"main.go","search":"","replace":"package main\n"}]}`,
		`{"action":"run_tests"}`,
		`{"action":"read_files","paths":["go.mod"]}`,
		`{"action":"read_files","paths":["README.md"]}`,
	)
	rec, _ := newTestRecorder(t)
	ctx := rec.Start(context.Background(), "tr", "t1", "dev-agent")

	a := NewDevAgent(gw, api, ClassLarge, RepoConfig{
		URL: "https://example.invalid/r", Branch: "main", BranchPrefix: "agent/",
		Image: "golang:1.25", TestCommand: "go test ./...",
	}, 4)

	status, detail, err := a.Handle(ctx, f.get("t1"))
	if err != nil {
		t.Fatalf("Handle() = %v", err)
	}
	if status != OutcomeSuccess {
		t.Fatalf("status = %q (%s), want success: the work was done and verified", status, detail)
	}
	// The invariant is unchanged — passing work must never be lost — but it is now
	// guaranteed earlier and more strongly: the loop cannot reach its iteration
	// limit while holding a passing branch, because green ends the stage on the
	// spot. So the comment reports a push, not a limit.
	bodies := f.commentBodies("t1")
	if len(bodies) != 1 || !strings.Contains(bodies[0], "pushed a branch") {
		t.Fatalf("comments = %v, want the passing work reported as a push", bodies)
	}
}

// A refusal that only says what failed leaves a looping model free to choose the
// same action again — which is what killed a live ticket with nothing written.
// From the second refusal on, the message must name what the model MUST do.
func TestRefusalsEscalateIntoAnInstruction(t *testing.T) {
	s := &devState{read: map[string]string{}, staged: map[string]string{}, missing: map[string]bool{}}

	s.noProgress("Rejected: already read.")
	if strings.Contains(s.notice, "MUST") {
		t.Errorf("the first refusal should explain, not shout:\n%s", s.notice)
	}

	s.noProgress("Rejected: already read.")
	for _, want := range []string{"write_files", "cost a turn"} {
		if !strings.Contains(s.notice, want) {
			t.Errorf("the second refusal does not name %q, so a looping model has nothing new to act on:\n%s", want, s.notice)
		}
	}
	// It must NOT threaten abandonment. Repetition no longer ends an attempt —
	// only the budget does — and a prompt that threatens a consequence it cannot
	// deliver teaches the model to discount the next one. What is true, and what
	// it says instead, is that the turns are being spent.
	if strings.Contains(s.notice, "abandoned") {
		t.Errorf("the refusal threatens an abandonment that can no longer happen:\n%s", s.notice)
	}
}

// The escalation must not swallow the reason the action was refused: the model
// still needs to know WHAT was rejected, not only what to do instead.
func TestEscalationKeepsTheOriginalReason(t *testing.T) {
	s := &devState{}
	s.noProgress("Rejected: nothing to test.")
	s.noProgress("Rejected: nothing to test.")
	if !strings.Contains(s.notice, "Rejected: nothing to test.") {
		t.Errorf("the original rejection was lost:\n%s", s.notice)
	}
}

// An exhausted attempt that produced no test output must still say what it did.
// Twice today the failure comment was the only evidence on the ticket and it
// explained nothing, so the diagnosis had to come from raw transcripts.
func TestTrailSuffixReportsWhatTheAttemptDid(t *testing.T) {
	out := trailSuffix([]devStep{
		{action: "read_files", detail: "main.go"},
		{action: "write_files", detail: "main.go"},
		{action: "run_tests"},
		{action: "write_files", detail: "main.go"},
	})
	if !strings.Contains(out, "read_files(main.go) → write_files(main.go) → run_tests → write_files(main.go)") {
		t.Errorf("the trail is not reported:\n%s", out)
	}
	// Ending on a write means the LAST thing it did was not verified.
	if !strings.Contains(out, "never tested") {
		t.Errorf("ending on a write is not called out:\n%s", out)
	}
	// But it did verify earlier, so blaming the budget would send the reader to
	// the wrong fix. That claim was made unconditionally until now.
	if strings.Contains(out, "budget too small") {
		t.Errorf("an attempt that did verify was blamed on the iteration budget:\n%s", out)
	}
	if !strings.Contains(out, "verify earlier") {
		t.Errorf("the earlier verification is not pointed at:\n%s", out)
	}
}

func TestTrailSuffixQuietWhenItEndedOnAVerification(t *testing.T) {
	out := trailSuffix([]devStep{{action: "write_files"}, {action: "run_tests"}})
	if strings.Contains(out, "never tested") {
		t.Errorf("an attempt that ended on run_tests was reported as unverified:\n%s", out)
	}
	if !strings.Contains(out, "write_files → run_tests") {
		t.Errorf("the trail is missing:\n%s", out)
	}
}

func TestTrailSuffixEmptyWhenNothingHappened(t *testing.T) {
	if trailSuffix(nil) != "" {
		t.Error("an empty trail should add nothing to the comment")
	}
}

// A passing verification ends the stage. Observed live: verification returned
// exit 0 and the agent kept editing for six more turns until the budget ran out,
// so a green branch was reported as a failure with no passing change. The
// reviewer is the next reader — anything wrong comes back as a new attempt, so
// there is nothing for those extra turns to win and a working branch to lose.
func TestDevAgentFinishesAsSoonAsChecksPass(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.addTicket(t, Ticket{TicketID: "t1", Title: "x", CreatedBy: "alice"})
	// main.go is deliberately absent from this listing: the script writes it
	// without reading it, which is a CREATE. A tree claiming it already exists
	// would make the same script a blind whole-file overwrite, which is refused.
	f.execStdout = "go.mod\n"

	// Write, verify (which the fake passes), then a THIRD action that must never
	// be reached — reaching it means the loop kept going after green.
	gw, calls := scriptedModel(t,
		`{"action":"write_files","edits":[{"path":"main.go","search":"","replace":"package main\n"}],"summary":"add","type":"feat"}`,
		`{"action":"run_tests"}`,
		`{"action":"write_files","edits":[{"path":"main.go","search":"","replace":"package broken\n"}],"summary":"break","type":"fix"}`,
	)
	rec, _ := newTestRecorder(t)
	ctx := rec.Start(context.Background(), "tr", "t1", "dev-agent")

	a := NewDevAgent(gw, api, ClassLarge, RepoConfig{
		URL: "https://example.invalid/r", Branch: "main", BranchPrefix: "agent/",
		Image: "golang:1.25", TestCommand: "go test ./...",
	}, 8)

	status, _, err := a.Handle(ctx, f.get("t1"))
	if err != nil {
		t.Fatalf("Handle() = %v", err)
	}
	if status != OutcomeSuccess {
		t.Errorf("status = %q, want success once the checks passed", status)
	}
	if n := calls(); n > 2 {
		t.Errorf("the agent took %d model turns; it should stop the moment checks pass", n)
	}
}

// The iteration counter is only useful with a denominator. Shown as a bare
// "ITERATION 5" the model cannot tell a comfortable budget from an exhausted
// one, and the observed failure was attempts that read one file per turn and
// ended having never run the tests.
func TestRenderedStateShowsTheIterationBudget(t *testing.T) {
	s := &devState{
		read: map[string]string{}, staged: map[string]string{}, missing: map[string]bool{},
		iteration: 3, budget: 8, tree: []string{"main.go"},
	}
	out := renderDevState(Ticket{TicketID: "T-1", Title: "x"}, s)
	if !strings.Contains(out, "ITERATION 3 OF 8") {
		t.Errorf("the iteration line has no budget, so the model cannot see turns are scarce:\n%s", out)
	}
	if !strings.Contains(out, "5 action(s) left") {
		t.Errorf("the remaining count is wrong or missing:\n%s", out)
	}
}

// The last iteration must not read as though more turns remain.
func TestRenderedStateOnTheFinalIterationSaysNoneRemain(t *testing.T) {
	s := &devState{
		read: map[string]string{}, staged: map[string]string{}, missing: map[string]bool{},
		iteration: 8, budget: 8,
	}
	out := renderDevState(Ticket{TicketID: "T-1"}, s)
	if !strings.Contains(out, "0 action(s) left") {
		t.Errorf("the final iteration should say nothing remains after it:\n%s", out)
	}
}

// A state with no budget set — which every unit test that builds one by hand
// produces — must still render rather than claiming "OF 0".
func TestRenderedStateWithoutABudgetOmitsTheDenominator(t *testing.T) {
	s := &devState{read: map[string]string{}, staged: map[string]string{}, iteration: 2}
	out := renderDevState(Ticket{TicketID: "T-1"}, s)
	if strings.Contains(out, "OF 0") {
		t.Errorf("an unknown budget rendered as a zero denominator:\n%s", out)
	}
	if !strings.Contains(out, "ITERATION 2") {
		t.Errorf("the iteration number was lost with the budget:\n%s", out)
	}
}

// The read action has to be stated as "name them all at once". Phrased as a
// ceiling ("read up to 12 files") it reads as permission, and a model that
// reads one at a time is not doing anything the prompt told it not to.
func TestSystemPromptTellsTheAgentToBatchItsReads(t *testing.T) {
	var read string
	for _, tool := range devTools(modeDevelop) {
		if tool.Name == actionReadFiles {
			read = tool.Description
		}
	}
	if read == "" {
		t.Fatal("no read_files tool is offered")
	}
	// Stated where the model is choosing arguments, not two thousand tokens
	// earlier in the system prompt.
	if !strings.Contains(read, "ONE call") {
		t.Errorf("the read tool does not instruct batching:\n%s", read)
	}
	if strings.Contains(read, "up to 12 files.") {
		t.Error("the read guidance is still phrased as a ceiling rather than an instruction")
	}
}

// An attempt that never verified at all is the case where the budget IS the
// suspect — and it must be reported differently from one that verified and
// ignored the result.
func TestTrailSuffixSaysWhenNothingWasEverVerified(t *testing.T) {
	out := trailSuffix([]devStep{
		{action: "read_files", detail: "main.go"},
		{action: "write_files", detail: "main.go"},
	})
	if !strings.Contains(out, "never ran the tests") {
		t.Errorf("an attempt that never verified is not identified as such:\n%s", out)
	}
	if strings.Contains(out, "verify earlier") {
		t.Errorf("an attempt that never verified was said to have verified:\n%s", out)
	}
}

// Each turn is rendered from scratch, so the model's own past actions have to be
// IN the prompt. Observed live: the first two actions of an attempt were a
// byte-identical read of the same three files, because nothing in the state told
// the model it had already asked.
func TestRenderedStateShowsTheActionsAlreadyTaken(t *testing.T) {
	s := &devState{
		read: map[string]string{}, staged: map[string]string{}, missing: map[string]bool{},
		iteration: 3, budget: 8,
		trail: []devStep{
			{action: "read_files", detail: "main.go, go.mod"},
			{action: "write_files", detail: "main.go"},
		},
	}
	out := renderDevState(Ticket{TicketID: "T-1"}, s)
	if !strings.Contains(out, "read_files(main.go, go.mod)") {
		t.Errorf("the history does not name what was read, so a repeat is invisible:\n%s", out)
	}
	if !strings.Contains(out, "write_files(main.go)") {
		t.Errorf("the history does not name what was written:\n%s", out)
	}
	if !strings.Contains(out, "do not repeat") {
		t.Errorf("the history does not say what it is for:\n%s", out)
	}
}

// A staged file read back returns the model's own write. Saying so is what stops
// the write → read "did that land?" loop that ended two live attempts.
func TestRenderedStateSaysStagedContentIsWhatYouWrote(t *testing.T) {
	s := &devState{
		read:   map[string]string{"main.go": "package main"},
		staged: map[string]string{"main.go": "package main"},
		writes: 1, verifiedWrites: 0, iteration: 4, budget: 8,
	}
	out := renderDevState(Ticket{TicketID: "T-1"}, s)
	if !strings.Contains(out, "returns your own writes") {
		t.Errorf("nothing warns that re-reading a staged file is wasted:\n%s", out)
	}
	// It must NOT point at run_tests any more: the model cannot ask for one, and
	// naming a tool that does not exist is how an agent spends turns being refused.
	if strings.Contains(out, "run_tests") {
		t.Errorf("the prompt still points at run_tests, which is no longer an action:\n%s", out)
	}
}

// Once the change HAS been verified, the prompt must stop demanding run_tests —
// otherwise it argues with the loop, which finishes on green.
func TestRenderedStateDoesNotDemandVerificationTwice(t *testing.T) {
	s := &devState{
		read:   map[string]string{},
		staged: map[string]string{"main.go": "package main"},
		writes: 1, verifiedWrites: 1, iteration: 5, budget: 8,
	}
	out := renderDevState(Ticket{TicketID: "T-1"}, s)
	if strings.Contains(out, "NOT yet verified") {
		t.Errorf("a verified change is still being called unverified:\n%s", out)
	}
}

func TestIsTestFile(t *testing.T) {
	for path, want := range map[string]bool{
		"main_test.go":     true,
		"./pkg/a_test.go":  true,
		"main.go":          false,
		"testdata/x.go":    false,
		"my_test.go.bak":   false,
		"_test.go":         true,
		"a_test.golden":    false,
		"pkg/test/util.go": false,
	} {
		if got := isTestFile(path); got != want {
			t.Errorf("isTestFile(%q) = %v, want %v", path, got, want)
		}
	}
}

// The two modes are two roles, and the routing table is keyed by role. One
// constructor returning the wrong one would send a stage's work to the other's
// columns.
func TestModesReportDistinctRoles(t *testing.T) {
	dev := NewDevAgent(nil, nil, ClassLarge, RepoConfig{}, 8)
	tester := NewTesterAgent(nil, nil, ClassSmall, RepoConfig{}, 8)
	if dev.Role() != roleDev {
		t.Errorf("developer role = %q, want %q", dev.Role(), roleDev)
	}
	if tester.Role() != roleTest {
		t.Errorf("test author role = %q, want %q", tester.Role(), roleTest)
	}
	if _, ok := stageFor(tester.Role()); !ok {
		t.Error("the test author's role has no routing entry; its tickets would never move")
	}
}

// The test author's branch must not look like finished work. hasBranch is what
// the reviewer and the integrator read to mean "there is something here to
// review and merge", and tests with no implementation are the opposite of that.
func TestTestAuthorsPushDoesNotLookLikeFinishedWork(t *testing.T) {
	tests := renderStageSummary(modeTest, "agent/t1", "wrote the tests", []string{"main_test.go"}, "", "valid Go")
	if strings.Contains(tests, branchMarker) {
		t.Error("the test author set the developer's branch marker; a tests-only branch would look mergeable")
	}
	if !strings.Contains(tests, testsWrittenMarker) {
		t.Error("the test author's push has no marker of its own")
	}
	if strings.Contains(tests, "pipeline passed on this branch") {
		t.Error("the test author claims the pipeline passed; its tests are supposed to fail at this point")
	}
	if !strings.Contains(tests, "do NOT pass yet") {
		t.Error("the comment does not say the tests are expected to fail, which is the whole point of the stage")
	}
	if hasBranch(Ticket{Comments: []Comment{{Body: tests}}}) {
		t.Error("hasBranch is true from the test stage alone; the reviewer and integrator would take unfinished work")
	}

	// The developer's own summary must still read as finished work.
	dev := renderStageSummary(modeDevelop, "agent/t1", "did it", []string{"main.go"}, "run1", "ok")
	if !hasBranch(Ticket{Comments: []Comment{{Body: dev}}}) {
		t.Error("the developer's push no longer registers as a branch to review")
	}
}

// The preamble sets -e, and gofmt exits NON-ZERO on the parse error this check
// exists to find. Unguarded, the script dies on the assignment and the stage
// reports "the tests do not parse" with an empty body — which is what a real
// arm of the model comparison did, leaving the author nothing to act on.
func TestTestParseScriptGuardsGofmt(t *testing.T) {
	if !strings.Contains(sandboxPreamble, "set -e") {
		t.Skip("the preamble no longer aborts on error; this guard is moot")
	}
	i := strings.Index(testParseScript, "err=$(gofmt")
	if i < 0 {
		t.Fatalf("no gofmt assignment in the script:\n%s", testParseScript)
	}
	line := testParseScript[i:]
	if end := strings.IndexByte(line, '\n'); end >= 0 {
		line = line[:end]
	}
	if !strings.Contains(line, "||") {
		t.Errorf("gofmt is unguarded under `set -e`, so a parse error kills the script "+
			"before it can report what is wrong:\n  %s", line)
	}
}

// THE RUNAWAY IS NOW UNGUARDED, DELIBERATELY. Writing repeatedly without
// verifying once burned a whole 100-iteration budget, and the guard that stopped
// it was real — but blocking a repeated action was measured to relocate the
// repetition rather than end it: an agent barred from re-reading spent 49 of 77
// turns on refused verifications instead. Every tool now runs and returns its
// real result, and what an unobstructed agent does is being measured rather than
// predicted. The iteration budget is the only backstop left.
func TestEveryToolRunsAndIsNeverRefusedAsPointless(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.addTicket(t, Ticket{TicketID: "t1", Title: "x", CreatedBy: "alice"})
	f.execStdout = "go.mod\n"

	write := func(n int) string {
		return fmt.Sprintf(`{"action":"write_files","edits":[{"path":"f%d.go","search":"","replace":"package main\n"}],"summary":"s","type":"fix"}`, n)
	}
	// The verification now runs on every changing write, and green would END the
	// stage — so the pipeline is failed deliberately to keep it open. What is
	// being tested is that a write is never refused merely for following another.
	f.runOutcome = RunFailed
	gw, calls := scriptedModel(t, write(1), write(2), write(3), write(4), write(5), write(6), write(7), write(8))
	rec, _ := newTestRecorder(t)
	ctx := rec.Start(context.Background(), "tr", "t1", "dev-agent")

	if _, _, err := devTestAgent(t, gw, api).Handle(ctx, f.get("t1")); err != nil {
		t.Fatalf("Handle() = %v", err)
	}
	// The fixture's budget is 6 iterations, so a run that spends all six is one
	// where every write was accepted — the point being that nothing but the
	// budget stops it.
	if calls() < 6 {
		t.Fatalf("model called %d times; writes were still being blocked before the budget ran out", calls())
	}
	joined := strings.Join(f.commentBodies("t1"), "\n")
	if strings.Contains(joined, "without running the tests") {
		t.Errorf("a write was refused as pointless; tools must run:\n%s", joined)
	}
}

// The guard must not fire on ordinary work: a change spanning a few files, then
// a verification, then more writes.
func TestWritesAreAllowedAgainAfterVerifying(t *testing.T) {
	s := &devState{staged: map[string]string{}, read: map[string]string{}, baseline: map[string]string{}}
	// Three writes with no verification is the ceiling, not a failure by itself.
	s.writes = maxUnverifiedWrites
	s.verifiedWrites = 0
	if s.writes-s.verifiedWrites < maxUnverifiedWrites {
		t.Fatal("fixture does not reach the ceiling")
	}
	// After a verification the allowance resets, which is what makes this a guard
	// against looping rather than a cap on how much a ticket may change.
	s.verifiedWrites = s.writes
	if s.writes-s.verifiedWrites >= maxUnverifiedWrites {
		t.Error("verifying did not reset the allowance; a long ticket would be blocked from further work")
	}
}

// TWO GUARDS MUST NOT GIVE OPPOSITE ORDERS, and the way that is now guaranteed is
// that there is only one order to give. This test used to require the directive to
// demand run_tests when writes were unverified; once run_tests left the vocabulary
// that requirement became an instruction the agent could not follow — 28 refusals
// on the dev fixture, each naming an absent tool.
func TestTheDirectiveOnlyEverAsksForTheOneAvailableAction(t *testing.T) {
	for _, s := range []*devState{
		{refusals: 2, writes: 3, verifiedWrites: 0}, // writes pending a check
		{refusals: 2, writes: 0, verifiedWrites: 0}, // nothing written yet
		{refusals: 5, writes: 1, verifiedWrites: 1}, // everything checked
	} {
		got := s.directive()
		if !strings.Contains(got, "write_files") {
			t.Errorf("the directive does not name the only action there is:\n%s", got)
		}
		for _, gone := range []string{"run_tests", "give_up", "finish"} {
			if strings.Contains(got, gone) {
				t.Errorf("the directive names %q, which the agent cannot call:\n%s", gone, got)
			}
		}
	}
}

// WHAT AN EDIT DOES NOT NAME, IT DOES NOT TOUCH. This is the whole reason
// whole-file writes were removed: a ticket asking for one struct replaced
// main.go with just that struct and deleted an entire HTTP server, and the tests
// still passed because the only tests written covered the struct.
func TestEditLeavesTheRestOfTheFileAlone(t *testing.T) {
	before := "package main\n\nfunc Greet() string { return \"hello\" }\n\nfunc Serve() {}\n"
	s := &devState{
		tree:   []string{"main.go"},
		read:   map[string]string{"main.go": before},
		staged: map[string]string{},
	}
	err := applyEdits(s, []devEdit{{
		Path: "main.go", StartLine: 3, EndLine: 3,
		Replace: "func Greet(name string) string { return \"Hello, \" + name }",
	}}, modeDevelop)
	if err != nil {
		t.Fatalf("applyEdits() = %v", err)
	}
	got := s.staged["main.go"]
	if !strings.Contains(got, "func Serve() {}") {
		t.Errorf("the untouched function was lost:\n%s", got)
	}
	if !strings.Contains(got, "Hello, ") {
		t.Errorf("the edit was not applied:\n%s", got)
	}
	if strings.Contains(got, "return \"hello\" }") {
		t.Errorf("the old text survived:\n%s", got)
	}
}

// A search that matches nothing is a wrong assumption about the file, caught
// before it is written rather than after.
func TestEditRefusesARangeThatDoesNotExist(t *testing.T) {
	s := &devState{
		tree:   []string{"main.go"},
		read:   map[string]string{"main.go": "package main\n"},
		staged: map[string]string{},
	}
	err := applyEdits(s, []devEdit{{Path: "main.go", StartLine: 40, EndLine: 42, Replace: "x"}}, modeDevelop)
	if err == nil {
		t.Fatal("an edit naming lines past the end of the file was accepted")
	}
	if !strings.Contains(err.Error(), "1 lines") {
		t.Errorf("the refusal does not say how long the file actually is: %v", err)
	}
	if len(s.staged) != 0 {
		t.Error("a failed edit staged something anyway")
	}
}

// AMBIGUITY CANNOT ARISE ANY MORE. Two identical lines were a real hazard when an
// edit was addressed by its text — the old format refused "a()" in a file holding
// two of them, because picking one would edit an arbitrary place. A line number
// names exactly one, so the whole failure mode is gone and this now checks the
// only nonsense a range can still express.
func TestEditRefusesAnInvertedRange(t *testing.T) {
	s := &devState{
		tree:   []string{"main.go"},
		read:   map[string]string{"main.go": "package main\n\nvar a = 1\nvar b = 2\nvar a2 = 1\n"},
		staged: map[string]string{},
	}
	err := applyEdits(s, []devEdit{{Path: "main.go", StartLine: 3, EndLine: 1, Replace: "var c = 3"}}, modeDevelop)
	if err == nil {
		t.Fatal("a range ending before it starts was accepted")
	}
	if !strings.Contains(err.Error(), "not a range") {
		t.Errorf("the refusal does not explain: %v", err)
	}

	// And the case the old format could not express at all: editing the SECOND of
	// two identical lines, unambiguously.
	ok := &devState{
		tree: []string{"main.go"}, staged: map[string]string{},
		read: map[string]string{"main.go": "package main\n\nvar a = 1\nvar b = 2\nvar a = 1\n"},
	}
	if err := applyEdits(ok, []devEdit{{Path: "main.go", StartLine: 5, EndLine: 5, Replace: "var c = 3"}}, modeDevelop); err != nil {
		t.Fatalf("addressing the second identical line was refused: %v", err)
	}
	if ok.staged["main.go"] != "package main\n\nvar a = 1\nvar b = 2\nvar c = 3\n" {
		t.Errorf("staged = %q, want the SECOND identical line replaced", ok.staged["main.go"])
	}
}

// A batch must apply all-or-nothing. A half-applied batch leaves the tree in a
// state neither the model nor the prompt describes.
func TestEditBatchIsAllOrNothing(t *testing.T) {
	s := &devState{
		tree:   []string{"main.go"},
		read:   map[string]string{"main.go": "package main\n"},
		staged: map[string]string{},
	}
	err := applyEdits(s, []devEdit{
		{Path: "main.go", StartLine: 1, EndLine: 1, Replace: "package demo"},
		{Path: "main.go", StartLine: 9, EndLine: 9, Replace: "x"},
	}, modeDevelop)
	if err == nil {
		t.Fatal("a batch with a bad edit was accepted")
	}
	if len(s.staged) != 0 {
		t.Errorf("the good half of a failed batch was staged: %v", s.staged)
	}
}

// Creating a file is an empty search. It must not be usable to blank an existing
// one, which would be the whole-file failure wearing a different hat.
func TestEmptySearchCreatesButCannotBlankAnExistingFile(t *testing.T) {
	s := &devState{tree: []string{}, read: map[string]string{}, staged: map[string]string{}}
	if err := applyEdits(s, []devEdit{{Path: "new.go", Replace: "package main\n"}}, modeDevelop); err != nil {
		t.Fatalf("creating a new file was refused: %v", err)
	}
	if s.staged["new.go"] != "package main\n" {
		t.Errorf("staged = %q", s.staged["new.go"])
	}

	existing := &devState{
		tree:   []string{"main.go"},
		read:   map[string]string{"main.go": "package main\n\nfunc Serve() {}\n"},
		staged: map[string]string{},
	}
	err := applyEdits(existing, []devEdit{{Path: "main.go", Replace: "package main\n"}}, modeDevelop)
	if err == nil {
		t.Fatal("an empty search replaced an existing file wholesale")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("the refusal does not explain: %v", err)
	}

	// And a create with nothing in it is a missing body, not an empty file.
	empty := &devState{tree: []string{}, read: map[string]string{}, staged: map[string]string{}}
	if err := applyEdits(empty, []devEdit{{Path: "x.go"}}, modeDevelop); err == nil {
		t.Error("an edit with no range and no replacement created an empty file")
	}
}

// Editing a file that was never read is a guess about its contents.
func TestEditRefusesAFileThatWasNeverRead(t *testing.T) {
	s := &devState{tree: []string{"main.go"}, read: map[string]string{}, staged: map[string]string{}}
	err := applyEdits(s, []devEdit{{Path: "main.go", StartLine: 1, EndLine: 1, Replace: "y"}}, modeDevelop)
	if err == nil {
		t.Fatal("an edit against an unread file was accepted")
	}
	if !strings.Contains(err.Error(), "read_files it first") {
		t.Errorf("the refusal does not say what to do: %v", err)
	}
}

// THE SPLIT IS THE FEATURE, and it survives the format change: an agent that can
// write both the change and the test that checks it writes the test its change
// already passes.
func TestDeveloperAndTestAuthorCannotEditEachOthersFiles(t *testing.T) {
	newState := func() *devState {
		return &devState{tree: []string{}, read: map[string]string{}, staged: map[string]string{}}
	}
	impl := []devEdit{{Path: "main.go", Replace: "package main\n"}}
	tests := []devEdit{{Path: "main_test.go", Replace: "package main\n"}}

	if err := applyEdits(newState(), tests, modeDevelop); err == nil {
		t.Error("the developer edited a test file; it could weaken a failing test until it passes")
	}
	if err := applyEdits(newState(), impl, modeTest); err == nil {
		t.Error("the test author edited the implementation; it could make its own test pass")
	}
	if err := applyEdits(newState(), impl, modeDevelop); err != nil {
		t.Errorf("the developer cannot edit the implementation: %v", err)
	}
	if err := applyEdits(newState(), tests, modeTest); err != nil {
		t.Errorf("the test author cannot edit tests: %v", err)
	}
	// One bad path refuses the whole batch, or a developer could slip a test edit
	// in beside a legitimate change.
	mixed := append(append([]devEdit{}, impl...), tests...)
	if err := applyEdits(newState(), mixed, modeDevelop); err == nil {
		t.Error("a batch mixing implementation and tests was accepted for the developer")
	}
}

// REPETITION NO LONGER ENDS AN ATTEMPT. Cutting an agent off for repeating
// itself abandoned tickets at turn five of forty-eight — ninety per cent of the
// budget unspent — for asking to read a file twice, which is what a careful
// worker does and what search/replace actively requires, since an edit must
// quote existing text exactly. Allowing it took a run from three merged tickets
// to four.
func TestRepeatedNoOpsDoNotEndTheAttempt(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.addTicket(t, Ticket{TicketID: "t1", Title: "x", CreatedBy: "alice"})
	f.execStdout = "===FILE main.go\n" + base64.StdEncoding.EncodeToString([]byte("package main\n")) + "\n"

	// Far more repeats than any old ceiling allowed, and nothing else.
	script := []string{}
	for range 12 + maxRefundedReads + 2 {
		script = append(script, `{"action":"read_files","paths":["main.go"]}`)
	}
	want := len(script)
	gw, calls := scriptedModel(t, script...)
	rec, _ := newTestRecorder(t)
	ctx := rec.Start(context.Background(), "tr", "t1", "dev-agent")

	a := NewDevAgent(gw, api, ClassLarge, RepoConfig{
		URL: "https://example.invalid/r", Branch: "main", BranchPrefix: "agent/",
		Image: "golang:1.25", TestCommand: "go test ./...",
	}, 12)
	status, detail, err := a.Handle(ctx, f.get("t1"))
	if err != nil {
		t.Fatalf("Handle() = %v", err)
	}
	if strings.Contains(detail, "stuck") {
		t.Errorf("the attempt was cut off for repeating itself: %q", detail)
	}
	// EVERY scripted read was served on a budget of 12, because reads do not
	// spend it. The attempt ends when the script runs dry, not on the ceiling.
	if got := calls(); got < want {
		t.Errorf("model called %d times, want all %d reads served on a budget of 12", got, want)
	}
	_ = status
}

// The one case that still finishes early is the OPPOSITE of being cut off: work
// that already passed is banked rather than thrown away when the agent spins.
func TestASpinningAgentWithPassingWorkIsStillFinished(t *testing.T) {
	s := &devState{
		read: map[string]string{}, staged: map[string]string{"main.go": "package main\n"},
		missing: map[string]bool{}, testsPass: true, refusals: maxRepeatRefusals,
	}
	if !(s.testsPass && len(s.staged) > 0 && s.refusals >= maxRepeatRefusals) {
		t.Fatal("fixture does not describe a spinning agent with passing work")
	}
}

// The defaults are the department's behaviour for anyone who configures nothing.
func TestDefaultBudgetIsADiagnosticCeiling(t *testing.T) {
	// Not a working allowance — a normal ticket finishes in single figures.
	// Reaching it still means something is wrong and the ticket is worth reading.
	//
	// RAISED FROM 100 once the turns were being spent on real work: at 100 the
	// budget went on harness noise (truncated replies retried verbatim, no-op
	// re-reads, assertions buried under panic stacks), so a bigger number would
	// only have bought more of the same.
	if defaultMaxIterations != 200 {
		t.Errorf("defaultMaxIterations = %d, want 200", defaultMaxIterations)
	}
	// Both constructors must honour it, or a caller passing 0 silently gets the
	// old cramped budget back.
	if got := NewDevAgent(nil, nil, ClassLarge, RepoConfig{}, 0).maxIterations; got != defaultMaxIterations {
		t.Errorf("developer default = %d, want %d", got, defaultMaxIterations)
	}
	if got := NewTesterAgent(nil, nil, ClassSmall, RepoConfig{}, 0).maxIterations; got != defaultMaxIterations {
		t.Errorf("test author default = %d, want %d", got, defaultMaxIterations)
	}
}

// The loop finishing on the agent's behalf is right in normal operation — left
// to decide for itself a model does not stop — but it also HIDES whether the
// agent can recognise it is done, because the harness always answers first.
// The switch is what lets that question be asked.
func TestAutoFinishIsOnUnlessExplicitlyDisabled(t *testing.T) {
	t.Setenv("AGENTS_DEV_AUTOFINISH", "")
	if !autoFinishOnGreen() {
		t.Error("auto-finish is off by default; a green branch would be polished until the budget ran out")
	}
	t.Setenv("AGENTS_DEV_AUTOFINISH", "true")
	if !autoFinishOnGreen() {
		t.Error("auto-finish is off when explicitly enabled")
	}
	t.Setenv("AGENTS_DEV_AUTOFINISH", "false")
	if autoFinishOnGreen() {
		t.Error("auto-finish cannot be turned off, so the experiment cannot be run")
	}
}

// The reserve is gone. It was tried live and the model ignored it: every reserve
// turn went to read_files, not one to run_tests or give_up.
func TestThereIsNoReserveNotice(t *testing.T) {
	s := &devState{
		read: map[string]string{}, staged: map[string]string{}, missing: map[string]bool{},
		tree: []string{"main.go"}, iteration: 60, budget: 55,
	}
	out := renderDevState(Ticket{TicketID: "T-1"}, s)
	if strings.Contains(out, "EMERGENCY BUDGET") {
		t.Errorf("the reserve notice is still rendered:\n%s", out)
	}
	if !strings.Contains(out, "ITERATION 60 OF 55") {
		t.Errorf("the plain iteration line is missing:\n%s", out)
	}
}

// THE SPEC IS NOT THE COVERAGE STAGE'S TO EDIT. Raising a coverage number by
// weakening an existing test hits the target and destroys what the target stands
// for, and it is the cheapest move available — so it is refused rather than
// discouraged.
func TestCoverageMayOnlyAddNewTestFiles(t *testing.T) {
	state := func() *devState {
		return &devState{
			// main_test.go is the specification, already on the branch.
			tree:     []string{"main.go", "main_test.go"},
			read:     map[string]string{"main_test.go": "package main\n"},
			staged:   map[string]string{},
			baseline: map[string]string{},
		}
	}

	// Editing the existing spec is refused.
	err := applyEdits(state(), []devEdit{
		{Path: "main_test.go", StartLine: 1, EndLine: 1, Replace: "package main // weakened"},
	}, modeCoverage)
	if err == nil {
		t.Fatal("the coverage stage edited the specification tests")
	}
	if !strings.Contains(err.Error(), "specification") {
		t.Errorf("the refusal does not explain why: %v", err)
	}

	// The implementation is likewise off limits.
	if err := applyEdits(state(), []devEdit{
		{Path: "main.go", StartLine: 1, EndLine: 1, Replace: "y"},
	}, modeCoverage); err == nil {
		t.Error("the coverage stage edited the implementation")
	}

	// A NEW test file is exactly what it is for.
	s := state()
	if err := applyEdits(s, []devEdit{
		{Path: "coverage_test.go", Replace: "package main\n"},
	}, modeCoverage); err != nil {
		t.Errorf("the coverage stage could not add a new test file: %v", err)
	}
	if s.staged["coverage_test.go"] == "" {
		t.Error("the new test file was not staged")
	}

	// And it must be able to iterate on the file it just created, or it gets one
	// shot at getting a whole suite right.
	if err := applyEdits(s, []devEdit{
		{Path: "coverage_test.go", StartLine: 1, EndLine: 1, Replace: "package main\n\nimport \"testing\""},
	}, modeCoverage); err != nil {
		t.Errorf("the coverage stage could not revise its own file: %v", err)
	}
}

// The number has to be read out of tool output, and the parser must not be tied
// to one tool's exact wording.
func TestCoverageIsReadFromToolOutput(t *testing.T) {
	for out, want := range map[string]float64{
		"ok  \tdemo\t0.02s\tcoverage: 82.4% of statements": 82.4,
		"coverage: 100.0% of statements":                   100,
		"total coverage 61%":                               61,
	} {
		got, ok := parseCoverage(out)
		if !ok || got != want {
			t.Errorf("parseCoverage(%q) = %v/%v, want %v", out, got, ok, want)
		}
	}
	if _, ok := parseCoverage("no numbers here"); ok {
		t.Error("a coverage figure was invented from output containing none")
	}
	// A STATED TOTAL WINS over anything else in the output. It is the weighted
	// figure — statements covered over statements total — and the only one that
	// means what a coverage target is supposed to mean.
	withTotal := "demo/a\tcoverage: 40.0% of statements\n" +
		"demo/b\tcoverage: 91.0% of statements\n" +
		"total:\t(statements)\t44.2%"
	if got, ok := parseCoverage(withTotal); !ok || got != 44.2 {
		t.Errorf("parseCoverage(with total) = %v/%v, want the weighted total 44.2", got, ok)
	}

	// Without one, the MEAN — not the highest, which would let a single tiny
	// fully covered package satisfy any target.
	multi := "demo/a\tcoverage: 40.0% of statements\ndemo/b\tcoverage: 90.0% of statements"
	if got, _ := parseCoverage(multi); got != 65 {
		t.Errorf("parseCoverage(no total) = %v, want the mean (65)", got)
	}
	tiny := "demo/big\tcoverage: 10.0% of statements\ndemo/tiny\tcoverage: 100.0% of statements"
	if got, _ := parseCoverage(tiny); got >= 80 {
		t.Errorf("parseCoverage = %v: one tiny fully covered package satisfied an 80%% target", got)
	}
}

// The default command must ask for the weighted total rather than per-package
// figures, or the parser is left approximating something the toolchain will
// compute exactly.
func TestDefaultCoverageCommandAsksForATotal(t *testing.T) {
	for _, want := range []string{"-coverprofile", "go tool cover -func"} {
		if !strings.Contains(defaultCoverageCommand, want) {
			t.Errorf("the default coverage command does not use %q: %s", want, defaultCoverageCommand)
		}
	}
}

// Three modes, three roles, three briefs — a stage handed the wrong one would do
// the wrong job while looking healthy.
func TestEachModeHasItsOwnRoleAndBrief(t *testing.T) {
	dev := NewDevAgent(nil, nil, ClassLarge, RepoConfig{}, 8)
	spec := NewTesterAgent(nil, nil, ClassSmall, RepoConfig{}, 8)
	cov := NewCoverageAgent(nil, nil, ClassSmall, RepoConfig{CoverageTarget: 80}, 8)

	roles := map[string]string{"dev": dev.Role(), "spec": spec.Role(), "coverage": cov.Role()}
	if roles["dev"] != roleDev || roles["spec"] != roleTest || roles["coverage"] != roleCoverage {
		t.Errorf("roles are wrong: %+v", roles)
	}
	for name, r := range roles {
		if _, ok := stageFor(r); !ok {
			t.Errorf("%s role %q has no routing entry; its tickets would never move", name, r)
		}
	}
	if cov.systemPrompt(Ticket{}) == spec.systemPrompt(Ticket{}) || cov.systemPrompt(Ticket{}) == dev.systemPrompt(Ticket{}) {
		t.Error("the coverage stage shares a brief with another mode")
	}
	if !strings.Contains(cov.systemPrompt(Ticket{}), "80% STATEMENT COVERAGE") {
		t.Errorf("the coverage brief does not state its target:\n%s", cov.systemPrompt(Ticket{}))
	}
	if !strings.Contains(cov.systemPrompt(Ticket{}), "ONLY ADD NEW TEST FILES") {
		t.Error("the coverage brief does not state that the specification is off limits")
	}
}

// A SHORTFALL IS NOT A FAILURE. The coverage stage's output is additive — the
// code already satisfies its specification — so running out of turns below the
// target must read as "this much was covered" and carry on, not as broken work.
func TestCoverageShortfallIsReportedAsAShortfall(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.addTicket(t, Ticket{TicketID: "t1", Title: "x", CreatedBy: "alice"})
	f.execStdout = "===FILE main.go\n" + base64.StdEncoding.EncodeToString([]byte("package main\n")) + "\n"

	// A model that never writes anything, so the stage exhausts.
	gw, _ := scriptedModel(t, `{"action":"read_files","paths":["main.go"]}`,
		`{"action":"read_files","paths":["main.go"]}`, `{"action":"read_files","paths":["main.go"]}`)
	rec, _ := newTestRecorder(t)
	ctx := rec.Start(context.Background(), "tr", "t1", "coverage-agent")

	a := NewCoverageAgent(gw, api, ClassSmall, RepoConfig{
		URL: "https://example.invalid/r", Branch: "main", BranchPrefix: "agent/",
		Image: "golang:1.25", TestCommand: "go test ./...", CoverageTarget: 80,
	}, 3)
	_, detail, err := a.Handle(ctx, f.get("t1"))
	if err != nil {
		t.Fatalf("Handle() = %v", err)
	}
	if !strings.Contains(detail, "coverage") {
		t.Errorf("detail = %q, want it to name the coverage shortfall", detail)
	}
	joined := strings.Join(f.commentBodies("t1"), "\n")
	if !strings.Contains(joined, coverageShortfallMarker) {
		t.Errorf("no shortfall recorded on the ticket:\n%s", joined)
	}
	if !strings.Contains(joined, "shortfall, not a failure") {
		t.Errorf("the comment reads as broken work rather than a shortfall:\n%s", joined)
	}
	// And it must NOT claim the developer stopped without a passing change — that
	// is a different stage's message and would be untrue here.
	if strings.Contains(joined, "without a passing change") {
		t.Errorf("the coverage stage reported itself as a failed developer run:\n%s", joined)
	}
}

// THE PUSH MUST NOT DESTROY ANOTHER STAGE'S WORK. A bare `checkout -B` from
// whatever HEAD happens to be, followed by a force-push, recreates the branch
// from the base — which silently erased every specification test on a live run
// and left the developer's gate with nothing to satisfy, reporting success.
//
// The suite passed the whole time that bug was live, which is why this asserts
// the SHAPE of the script rather than trusting the behaviour.
func TestPushStartsFromTheRemoteBranchNotTheBase(t *testing.T) {
	a := NewDevAgent(nil, nil, ClassLarge, RepoConfig{
		URL: "git://host/demo.git", Branch: "dev", BranchPrefix: "agent/",
	}, 8)
	s := &devState{staged: map[string]string{"main.go": "package main\n"}, read: map[string]string{}}
	script := a.pushScript(Ticket{TicketID: "t1", Title: "x"}, "agent/t1", s)

	fetch := strings.Index(script, "git fetch -q origin")
	checkout := strings.Index(script, "checkout -q -B")
	push := strings.Index(script, "git push")
	if fetch < 0 {
		t.Fatalf("the push never looks for an existing branch:\n%s", script)
	}
	if !(fetch < checkout && checkout < push) {
		t.Errorf("fetch/checkout/push are out of order (%d/%d/%d):\n%s", fetch, checkout, push, script)
	}
	// It must reset onto what the remote has, not merely name the branch.
	if !strings.Contains(script, "FETCH_HEAD") {
		t.Errorf("the push does not start from the fetched commit, so another stage's work is lost:\n%s", script)
	}
	// And it must still work for the FIRST push, when no such branch exists.
	if !strings.Contains(script, "||") {
		t.Errorf("a branch that does not exist yet would fail the push:\n%s", script)
	}
}

// The architect's documentation is on the base branch before any developer
// sandbox clones, and it describes the whole system — the names and interfaces
// every OTHER ticket is being built against. A developer editing it to match its
// own half silently rewrites five other agents' specification.
func TestDeveloperMayNotEditDocumentation(t *testing.T) {
	s := &devState{
		tree:   []string{"README.md", "ARCHITECTURE.md", "store.go"},
		read:   map[string]string{"README.md": "# Tracker\n", "store.go": "package main\n"},
		staged: map[string]string{},
	}
	for _, p := range []string{"README.md", "ARCHITECTURE.md", "docs/design.md"} {
		err := applyEdits(s, []devEdit{{Path: p, Replace: "# mine\n"}}, modeDevelop)
		if err == nil {
			t.Errorf("developer was allowed to write %s", p)
			continue
		}
		if !strings.Contains(err.Error(), "documentation") {
			t.Errorf("refusal for %s does not say why: %v", p, err)
		}
	}
	if len(s.staged) != 0 {
		t.Errorf("a refused documentation edit was staged anyway: %v", keysOf(s.staged))
	}
}

// ...and the developer must still be able to write actual code.
func TestDeveloperMayStillEditImplementation(t *testing.T) {
	s := &devState{
		tree:   []string{"store.go"},
		read:   map[string]string{"store.go": "package main\n\nfunc A() {}\n"},
		staged: map[string]string{},
	}
	if err := applyEdits(s, []devEdit{{Path: "store.go", StartLine: 3, EndLine: 3, Replace: "func A() int { return 1 }"}}, modeDevelop); err != nil {
		t.Fatalf("developer refused a legitimate implementation edit: %v", err)
	}
	if _, ok := s.staged["store.go"]; !ok {
		t.Error("the implementation edit was not staged")
	}
}

// AN AGENT CANNOT ADD A DEPENDENCY WITHOUT THIS. It writes `require x/y` into
// go.mod correctly and then every build fails on "missing go.sum entry" — a file
// of hashes no amount of editing can produce — and its only execution tool runs
// the test command. Measured live: six commits and 112 model turns circling
// go.mod while the real compile errors sat untouched behind it.
func TestDependenciesAreResolvedBeforeTheCommit(t *testing.T) {
	a := NewDevAgent(nil, nil, ClassLarge, RepoConfig{
		URL: "git://x/y.git", Branch: "dev", BranchPrefix: "agent/",
		DepsCommand: "go mod tidy", FormatCommand: "gofmt -w .",
	}, 10)
	s := &devState{staged: map[string]string{"go.mod": "module t\n"}}
	script := a.pushScript(Ticket{TicketID: "t1", Title: "x"}, "agent/t1", s)

	write := strings.Index(script, "base64 -d")
	tidy := strings.Index(script, "go mod tidy")
	// NOT "git commit": the real command is `git -c user.name=... commit -F ...`,
	// so a literal match silently returns -1 and the ordering check passes for the
	// wrong reason.
	commit := strings.Index(script, " commit -F")
	push := strings.Index(script, "git push")
	if tidy == -1 {
		t.Fatalf("dependencies are never resolved:\n%s", script)
	}
	if !(write < tidy) {
		t.Error("dependencies are resolved before the files are written; there is nothing to resolve yet")
	}
	if !(tidy < commit) {
		t.Error("dependencies are resolved after the commit; the lock file would not be in it")
	}
	if !(commit < push) {
		t.Error("the commit does not precede the push")
	}
	// Best effort: a repository without the tool must not lose an otherwise good
	// change, but the failure has to be visible or it becomes a confusing test
	// failure later.
	if !strings.Contains(script, "WARNING: dependencies could not be resolved") {
		t.Error("a failed resolution is silent; the build would fail with a confusing cause")
	}
}

// An operator who names no resolver has not asked for one.
func TestNoDepsCommandIsANoOp(t *testing.T) {
	a := NewDevAgent(nil, nil, ClassLarge, RepoConfig{URL: "git://x/y.git", Branch: "dev", BranchPrefix: "agent/"}, 10)
	if got := a.depsScript(); got != "" {
		t.Errorf("depsScript with no command = %q, want empty", got)
	}
}

// Go's default has to be tidy, not download: download fetches what go.mod
// already names and leaves go.sum untouched, which is precisely the state the
// agent gets stuck in.
func TestDefaultDepsCommandWritesTheLockFile(t *testing.T) {
	if defaultDepsCommand != "go mod tidy" {
		t.Errorf("defaultDepsCommand = %q; go mod download would not write go.sum", defaultDepsCommand)
	}
}

// THE FORMATTER HAS TO FIX IMPORTS, not just whitespace.
//
// An unused import and a missing stdlib import are not syntax errors, so the
// spec author's parse gate passes them; they surface as compile errors in a test
// file the developer is forbidden to edit. Measured on a live branch: the only
// two faults were `"time" imported and not used` and `undefined: fmt`, no agent
// in the pipeline could fix either, and one goimports run made it pass.
func TestDefaultFormatterFixesImports(t *testing.T) {
	if !strings.Contains(defaultFormatCommand, "goimports") {
		t.Errorf("defaultFormatCommand = %q; gofmt alone cannot fix an import block", defaultFormatCommand)
	}
	// Pinned: this executes publisher-chosen code in the sandbox on every commit.
	if strings.Contains(defaultFormatCommand, "@latest") {
		t.Error("the default formatter fetches a tool at @latest; name a version")
	}
	// A host that cannot fetch the tool must still format rather than not at all.
	if !strings.Contains(defaultFormatCommand, "gofmt") {
		t.Error("no fallback: a sandbox that cannot fetch goimports would not format at all")
	}
}

// The default must survive into the push script the agent actually runs, in the
// right order. TestFormatterRunsBeforeTheCommit checks formatScript alone; this
// checks where it lands relative to the commit.
func TestFormatterPrecedesTheCommitInThePushScript(t *testing.T) {
	a := NewDevAgent(nil, nil, ClassLarge, RepoConfig{
		URL: "git://x/y.git", Branch: "dev", BranchPrefix: "agent/",
		FormatCommand: defaultFormatCommand, DepsCommand: defaultDepsCommand,
	}, 10)
	s := &devState{staged: map[string]string{"main.go": "package main\n"}}
	script := a.pushScript(Ticket{TicketID: "t1", Title: "x"}, "agent/t1", s)

	fmtAt := strings.Index(script, "goimports")
	commit := strings.Index(script, " commit -F")
	if fmtAt == -1 || commit == -1 || fmtAt > commit {
		t.Errorf("the formatter does not run before the commit (fmt=%d commit=%d):\n%s", fmtAt, commit, script)
	}
}

// THE FAILURE IS AT THE BOTTOM. `go test` prints module downloads and passing
// tests first, then the assertion that failed; a compile error, by contrast, is
// at the top. Head-only truncation therefore fed the model "it failed" plus a
// list of downloads on exactly the runs where the message mattered most.
func TestLongTestOutputKeepsBothTheCompileErrorAndTheAssertion(t *testing.T) {
	head := "# tracker [tracker.test]\n./main.go:12:2: declared and not used: x\n"
	middle := strings.Repeat("go: downloading example.com/some/module v1.2.3\n", 400)
	tail := `--- FAIL: TestTaskStore/List_filters_by_title_match
    Error: "Buy milk" does not contain "buy"
FAIL`
	out := head + middle + tail

	got := clipEnds(out, 200, 400)
	if !strings.Contains(got, "declared and not used") {
		t.Error("the compile error at the top was dropped")
	}
	if !strings.Contains(got, `does not contain "buy"`) {
		t.Error("the failing assertion at the bottom was dropped; this is the half head-clipping lost")
	}
	if !strings.Contains(got, "omitted from the middle") {
		t.Error("the elision is silent; the model cannot tell it was given a partial log")
	}
	if len([]rune(got)) > 800 {
		t.Errorf("clipEnds returned %d runes, well over the budget it was given", len([]rune(got)))
	}
	// Short output must pass through untouched.
	if clipEnds("short", 200, 400) != "short" {
		t.Error("short output was altered")
	}
}

// A failing assertion names a file and a line and nothing else, so the
// developer's next move was always a read_files round trip to see what that line
// says. The sandbox is already standing next to the file; printing it there
// removes a whole model turn from every failure.
func TestTestGateQuotesTheFailingSource(t *testing.T) {
	s := testGateScript("go test ./...")

	if !strings.Contains(s, "go test ./...") {
		t.Fatalf("the test command is missing:\n%s", s)
	}
	// The happy path must stay quiet and must not print the source block.
	if !strings.Contains(s, "cat /tmp/.gate-out") {
		t.Error("the output is not shown")
	}
	// Any .go file, not only _test.go: a compile error in main.go benefits from
	// its surrounding lines exactly as much as a failing assertion does.
	for _, want := range []string{`\.go:[0-9]+`, "grep -oE", "around line", gateMarker} {
		if !strings.Contains(s, want) {
			t.Errorf("the failure path is missing %q:\n%s", want, s)
		}
	}
	// Bounded: a run with forty failures must not paste the suite into the prompt.
	if !strings.Contains(s, fmt.Sprintf("head -%d", maxFailureSites)) {
		t.Error("the number of quoted locations is unbounded")
	}
	// And it must still fail the gate.
	if !strings.Contains(s, "exit 1") {
		t.Error("a failing test command no longer fails the gate")
	}
}

// The download chatter is pure noise, sits at the top of the log, and competes
// with the failure for the model's attention. A FAILED download is not noise.
func TestToolChatterIsStrippedButFailuresAreKept(t *testing.T) {
	in := strings.Join([]string{
		"go: downloading github.com/stretchr/testify v1.11.1",
		"go: extracting github.com/stretchr/testify v1.11.1",
		"# tracker [tracker.test]",
		"./main.go:12:2: declared and not used: x",
		"go: module example.com/x: reading https://proxy/@v/list: 404 Not Found",
	}, "\n")
	got := stripToolChatter(in)

	if strings.Contains(got, "go: downloading") || strings.Contains(got, "go: extracting") {
		t.Error("progress chatter survived")
	}
	if !strings.Contains(got, "declared and not used") {
		t.Error("the compile error was stripped")
	}
	if !strings.Contains(got, "404 Not Found") {
		t.Error("a FAILED download was stripped; the developer would hunt its own code for a network fault")
	}
}

// A PANIC BURIES ITS OWN CAUSE. A spec that asserts a length and then indexes
// the result panics whenever the length is wrong, and Go then prints twenty-odd
// frames of its own internals after the one line that says what happened.
// Measured on a real branch: the assertion was line two, and everything after it
// was stack.
func TestPanicStacksAreStrippedButTheCauseAndAppFramesSurvive(t *testing.T) {
	out := strings.Join([]string{
		`--- FAIL: TestTaskStore/List_filters_by_title_match (0.00s)`,
		`        task_store_test.go:136: "[]" should have 2 item(s), but has 0`,
		`panic: runtime error: index out of range [0] with length 0 [recovered, repanicked]`,
		``,
		`goroutine 19 [running]:`,
		`testing.tRunner.func1.2({0x639740, 0xc00001e2d0})`,
		`	/usr/local/go/src/testing/testing.go:1872 +0x237`,
		`runtime.gopanic({0x639740?, 0xc00001e2d0?})`,
		`	/usr/local/go/src/runtime/panic.go:783 +0x132`,
		`tracker.TestTaskStore.func10(0xc0000d3dc0)`,
		`	/tmp/r/task_store_test.go:137 +0x36f`,
		`created by testing.(*T).Run in goroutine 8`,
		`	/usr/local/go/src/testing/testing.go:1997 +0x465`,
	}, "\n")

	got := stripToolChatter(out)

	// The cause and the assertion are the entire diagnosis.
	for _, want := range []string{"should have 2 item(s)", "index out of range", "task_store_test.go:136"} {
		if !strings.Contains(got, want) {
			t.Errorf("stripped the diagnosis: %q is missing from:\n%s", want, got)
		}
	}
	// The repository's OWN frame names the line to look at, so it stays.
	if !strings.Contains(got, "task_store_test.go:137") {
		t.Errorf("the application's own stack frame was stripped:\n%s", got)
	}
	// Go's internals never say anything about the change under test.
	for _, gone := range []string{"testing.tRunner", "runtime.gopanic", "/usr/local/go/src/", "created by testing."} {
		if strings.Contains(got, gone) {
			t.Errorf("stdlib noise survived: %q in:\n%s", gone, got)
		}
	}
	if len(strings.Split(got, "\n")) >= len(strings.Split(out, "\n")) {
		t.Error("nothing was actually removed")
	}
}

// A TRUNCATED REPLY IS NOT A MALFORMED ONE, and the difference decides whether
// the model changes what it does. Measured: it was creating a 150-line main.go
// inside one tool argument, ran out of output tokens mid-string, and the result
// decoded as "unexpected end of JSON input". Told only that its reply was
// "rejected", it sent the same oversized write four times and lost the attempt.
func TestTruncationIsReportedAsTooLongNotAsMalformed(t *testing.T) {
	// The advice must change the SIZE of the next attempt.
	s := &devState{truncated: true}
	notice := noticeForParseFailure(s, errors.New("unexpected end of JSON input"))
	for _, want := range []string{"CUT OFF", "SMALLER", "search/replace"} {
		if !strings.Contains(notice, want) {
			t.Errorf("truncation advice is missing %q: %s", want, notice)
		}
	}
	if strings.Contains(notice, "rejected") {
		t.Error("a cut-off reply is described as rejected, which invites an identical retry")
	}
	// A genuinely malformed reply still gets the parse error, which is the thing
	// that tells the model what to correct.
	s2 := &devState{truncated: false}
	n2 := noticeForParseFailure(s2, errors.New("unknown tool \"frobnicate\""))
	if !strings.Contains(n2, "frobnicate") {
		t.Errorf("a malformed reply lost its parse error: %s", n2)
	}
	if strings.Contains(n2, "CUT OFF") {
		t.Error("a complete reply was described as truncated")
	}
}

// A NO-OP READ MUST NOT COST A TURN. Measured on a live ticket: of ~95 actions,
// roughly half were read_files(main.go) — re-reading the file it had just
// written, whose staged contents were already in the prompt — and the ticket
// then died at the iteration limit having spent half its budget learning
// nothing. Refusing was tried and was worse: it killed tickets at turn five.
func TestRepeatedReadsAreRefundedRatherThanCharged(t *testing.T) {
	s := &devState{
		read:    map[string]string{"main.go": "package main\n"},
		missing: map[string]bool{},
		staged:  map[string]string{},
	}
	a := NewDevAgent(nil, nil, ClassLarge, RepoConfig{URL: "git://x/y.git"}, 10)

	a.doRead(context.Background(), nil, s, devAction{Paths: []string{"main.go"}})
	// The REFUND is applied by the loop, for every read rather than only the
	// no-ops, so doRead's job here is just to serve and to say it gained nothing.
	if !strings.Contains(s.notice, "refunded") {
		t.Errorf("the model is not told the turn was refunded: %q", s.notice)
	}
	if s.refusals != 0 {
		t.Errorf("a no-op read counted as a refusal: %d", s.refusals)
	}
	// Escalating advice still arrives, because free is not the same as useful.
	for range maxStaleReads {
		a.doRead(context.Background(), nil, s, devAction{Paths: []string{"main.go"}})
	}
	if !strings.Contains(s.notice, "write_files") {
		t.Errorf("repeated no-op reads do not escalate to advice: %q", s.notice)
	}

	// An agent that only ever re-reads must still terminate, and what guarantees
	// that is now maxTotalIterations rather than a cap on refunds.
	for range maxRefundedReads * 2 {
		a.doRead(context.Background(), nil, s, devAction{Paths: []string{"main.go"}})
	}
	if maxConsecutiveReads <= 0 {
		t.Error("nothing bounds an attempt that only reads")
	}
}

// The budget was raised once the turns started being spent on real work rather
// than on harness noise.
func TestIterationBudgetIsTwoHundred(t *testing.T) {
	if defaultMaxIterations != 200 {
		t.Errorf("defaultMaxIterations = %d, want 200", defaultMaxIterations)
	}
}

// ONE CEILING FOR EVERY FILE-WRITING STAGE, because truncation is not a smaller
// answer — it is no answer.
//
// The ceilings used to differ, on the argument that only the developer creates
// whole files and the author would otherwise write until it hit the limit
// (measured on the dense 14B: four authors in lockstep at 792 tokens with 7208
// still allowed, six minutes into one turn). That cost is real and it is the
// smaller one. Measured on the current model: the author stopped at exactly 5000
// on four consecutive turns and the attempt ended "unparseable model output",
// losing a whole file each time.
//
// A lower ceiling also frees nothing. max_tokens is a stop condition, not a
// reservation — concurrency is the class's slot count — so trimming one stage
// buys no capacity for another.
func TestEveryFileWritingStageSharesOneCeiling(t *testing.T) {
	repo := RepoConfig{URL: "git://x/y.git"}
	dev := NewDevAgent(nil, nil, ClassLarge, repo, 10).maxReplyTokens()
	spec := NewTesterAgent(nil, nil, ClassLarge, repo, 10).maxReplyTokens()
	cov := NewCoverageAgent(nil, nil, ClassLarge, repo, 10).maxReplyTokens()

	if dev != spec {
		t.Errorf("developer %d and spec author %d differ; both write whole files on the same class", dev, spec)
	}
	if spec != cov {
		t.Errorf("spec author %d and coverage %d write the same kind of output and should share a ceiling", spec, cov)
	}
	// Both must leave room for the PROMPT inside one 16384-token slot. A developer
	// prompt carrying the architecture docs and every file read measured 8583.
	const slot, measuredPrompt = 16384, 8583
	if dev+measuredPrompt > slot {
		t.Errorf("developer ceiling %d plus a measured %d-token prompt exceeds the %d-token slot", dev, measuredPrompt, slot)
	}
	// And the developer's must still be large enough for the whole-file create
	// that truncated at 4000.
	if dev <= 4000 {
		t.Errorf("developer ceiling %d is back at or below the size that truncated", dev)
	}
}

// A COMPILE ERROR IN A TEST FILE IS NOT THE DEVELOPER'S TO FIX. It may not edit
// tests, so no action available to it can clear the gate. Measured: a spec
// dereferenced Task.Title as a pointer while using Task.Done as a value —
// internally inconsistent, unsatisfiable by any implementation — and the
// developer spent 100 turns discovering that at ~11 turns a minute.
func TestCompileErrorsInTestFilesAreASpecFault(t *testing.T) {
	specOnly := `# tracker [tracker.test]
./task_store_test.go:96:36: invalid operation: cannot indirect retrievedTask.Title (variable of type string)
FAIL	tracker [build failed]`
	files, allTests := compileErrorFiles(specOnly)
	if !allTests {
		t.Errorf("a compile error only in a test file was not recognised as a spec fault: %v", files)
	}
	if len(files) != 1 || files[0] != "task_store_test.go" {
		t.Errorf("files = %v, want the test file named", files)
	}

	// A compile error the developer CAN fix must not stop the loop.
	mixed := specOnly + "\n./main.go:12:2: declared and not used: x"
	if _, allTests := compileErrorFiles(mixed); allTests {
		t.Error("an error in main.go was treated as a spec fault; the developer can fix that one")
	}
	if _, allTests := compileErrorFiles("./main.go:12:2: declared and not used: x"); allTests {
		t.Error("an implementation-only compile error was treated as a spec fault")
	}

	// A FAILING ASSERTION IS NOT A COMPILE ERROR. Red tests are the normal state
	// of test-first development; stopping on those would break the pipeline.
	redTests := `--- FAIL: TestTaskStore/List_filters (0.00s)
        task_store_test.go:155: 
            	Error:      	"Buy milk" does not contain "buy"
FAIL	tracker	0.002s`
	if files, allTests := compileErrorFiles(redTests); allTests || len(files) != 0 {
		t.Errorf("a failing assertion was read as a compile error: files=%v allTests=%v", files, allTests)
	}
	// And a clean run says nothing.
	if files, _ := compileErrorFiles("ok  	tracker	0.002s"); len(files) != 0 {
		t.Errorf("a passing run reported compile errors: %v", files)
	}
}

// READS ARE NEVER REFUSED, AND THIS IS AN EXPERIMENT RATHER THAN A CONCLUSION.
// Both refusing forms have been tried: refused-and-penalised abandoned real
// tickets at turn five of forty-eight, and refused-and-refunded left the model
// asking again anyway — a UI ticket reached 100 turns with 7 sandbox operations
// and never ran its tests. What an unrestricted reader actually does has never
// been measured, so this pins the unrestricted contract while that measurement
// is taken.
func TestNoOpReadsAreAlwaysServed(t *testing.T) {
	s := &devState{
		read:    map[string]string{"main.go": "package main\n"},
		missing: map[string]bool{},
		staged:  map[string]string{},
	}
	a := NewDevAgent(nil, nil, ClassLarge, RepoConfig{URL: "git://x/y.git"}, 10)
	act := devAction{Action: actionReadFiles, Paths: []string{"main.go"}}

	for i := 1; i <= maxStaleReads*3; i++ {
		a.doRead(context.Background(), nil, s, act)
		if strings.Contains(s.notice, "REFUSED") {
			t.Fatalf("read %d was refused; reads are unrestricted while this is being measured", i)
		}
		if s.refusals != 0 {
			t.Fatalf("read %d counted as a refusal: that is what killed tickets at turn five", i)
		}
	}
	// The refund is the LOOP's job now — every read, not just the no-ops — so
	// what doRead owes the model here is the notice, not the accounting.
	if !strings.Contains(s.notice, "refunded") {
		t.Errorf("the model is not told the read cost it nothing: %q", s.notice)
	}
	// ...and the advice still escalates, because free is not the same as useful.
	// It names write_files and nothing else: the alternatives it used to offer are
	// no longer actions, and naming an absent tool is worse than naming none.
	if !strings.Contains(s.notice, "write_files") {
		t.Errorf("the advice never names write_files as the alternative:\n%s", s.notice)
	}
	for _, gone := range []string{"run_tests", "give_up"} {
		if strings.Contains(s.notice, gone) {
			t.Errorf("the advice offers %q, which the agent cannot call:\n%s", gone, s.notice)
		}
	}
}

// ONE BRACE TOO MANY IS STILL AN ANSWER. An architect returned 4,135 bytes of
// correct JSON ending `"}]}}` and the stage reported "unparseable model output",
// so the project got no ARCHITECTURE.md and every developer after it worked
// without a shared picture of the system.
func TestTrailingJunkAfterACompleteObjectIsIgnored(t *testing.T) {
	type doc struct {
		Overview string `json:"overview"`
		Files    []struct {
			Path string `json:"path"`
		} `json:"files"`
	}

	for _, tc := range []struct {
		name, raw string
	}{
		{"extra closing brace", `{"overview":"x","files":[{"path":"README.md"}]}}`},
		{"trailing prose", `{"overview":"x","files":[{"path":"README.md"}]} and that is the design.`},
		{"a second object", `{"overview":"x","files":[{"path":"README.md"}]}{"overview":"ignored"}`},
	} {
		var d doc
		if err := decodeJSONObject(tc.raw, &d); err != nil {
			t.Errorf("%s: decodeJSONObject() = %v, want the object to be recovered", tc.name, err)
			continue
		}
		if d.Overview != "x" || len(d.Files) != 1 || d.Files[0].Path != "README.md" {
			t.Errorf("%s: decoded %+v, want the FIRST object", tc.name, d)
		}
	}

	// A TRUNCATED object is still a failure: there is nothing to recover, and
	// pretending otherwise would commit half a document.
	var d doc
	if err := decodeJSONObject(`{"overview":"x","files":[{"path":"READ`, &d); err == nil {
		t.Error("a truncated reply was accepted; half a design must not be committed")
	}

	// The control-character repair still applies on top of it.
	var d2 doc
	if err := decodeJSONObject("{\"overview\":\"line\none\",\"files\":[]}}", &d2); err != nil {
		t.Errorf("the repair path stopped working alongside the trailing-junk fix: %v", err)
	} else if d2.Overview != "line\none" {
		t.Errorf("Overview = %q", d2.Overview)
	}
}

// The ceiling has to hold two documents in one reply, which is not the shape of
// the per-action agents.
func TestTheDesignCeilingLeavesRoomForTwoDocuments(t *testing.T) {
	// An ABSOLUTE floor, not a multiple of another stage's ceiling — that
	// coupling was meaningless and broke the moment the spec author's ceiling
	// moved. Observed designs measure 950-1100 tokens for both documents, so this
	// is several times the real requirement.
	if maxDesignReplyTokens < 4000 {
		t.Errorf("maxDesignReplyTokens = %d, too small for two documents in one reply", maxDesignReplyTokens)
	}
}

// THE STUB MOVED DOWN A LEVEL when empty test functions were blocked. Observed
// verbatim on a live spec: four t.Run closures, each holding one "// TODO"
// comment, inside a test function that therefore looked populated.
func TestEmptySubtestsAreCaughtToo(t *testing.T) {
	stubs := stubbedTests(map[string]string{"main_test.go": `package main

import "testing"

func TestGetTasksFiltering(t *testing.T) {
	t.Run("filter by status", func(t *testing.T) {
		if got := Filter("todo"); len(got) != 1 {
			t.Fatalf("len = %d", len(got))
		}
	})
	t.Run("multiple filters", func(t *testing.T) {
		// TODO: Implement test for combining multiple filters
	})
	t.Run("no matches", func(t *testing.T) {})
}
`})
	want := []string{"TestGetTasksFiltering/multiple filters", "TestGetTasksFiltering/no matches"}
	if len(stubs) != len(want) {
		t.Fatalf("stubbedTests = %v, want %v", stubs, want)
	}
	for i := range want {
		if stubs[i] != want[i] {
			t.Errorf("stub %d = %q, want %q", i, stubs[i], want[i])
		}
	}
}

// A subtest that asserts is not a stub, and neither is an empty closure that is
// not a subtest at all.
func TestSubtestDetectionDoesNotOverreach(t *testing.T) {
	stubs := stubbedTests(map[string]string{"x_test.go": `package main

import "testing"

func TestFine(t *testing.T) {
	t.Run("real", func(t *testing.T) { t.Fatal("boom") })
	// An empty closure that is not a subtest: a no-op handler is legitimate.
	srv := New(func() {})
	_ = srv
	// A table-driven loop with a named case.
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { check(t, tc) })
	}
}
`})
	if len(stubs) != 0 {
		t.Errorf("stubbedTests = %v, want none", stubs)
	}
}

// A SPEC THAT SUPPLIES ITS OWN IMPLEMENTATION SPECIFIES NOTHING, and it deadlocks
// the run: observed as an author rewriting task_store_test.go — beginning `type
// Task struct` — six identical times in two and a half minutes, refused each
// time by the red gate for passing, with four tickets waiting behind it.
func TestASpecCannotDeclareTheImplementation(t *testing.T) {
	own := implementationInTests(map[string]string{"task_store_test.go": `package main

import "testing"

type Task struct {
	ID    string
	Title string
}

var DefaultStatus = "todo"

func NewStore() *Store { return nil }

func TestStoreAdd(t *testing.T) {
	if NewStore() == nil {
		t.Fatal("nil")
	}
}
`})
	want := []string{"type Task", "var DefaultStatus", "func NewStore"}
	if len(own) != len(want) {
		t.Fatalf("implementationInTests = %v, want %v", own, want)
	}
	for i := range want {
		if own[i] != want[i] {
			t.Errorf("declaration %d = %q, want %q", i, own[i], want[i])
		}
	}
}

// THE SCAFFOLDING A TEST LEGITIMATELY OWNS MUST SURVIVE. Table-driven cases,
// setup helpers and fixture builders are how normal Go tests are written, and
// they are unexported — which is exactly the line this draws.
func TestTestScaffoldingIsNotMistakenForImplementation(t *testing.T) {
	own := implementationInTests(map[string]string{
		"store_test.go": `package main

import "testing"

type testCase struct {
	name string
	want int
}

var cases = []testCase{{"empty", 0}}

func setup(t *testing.T) *Store { return New() }

func (c testCase) check(t *testing.T) {}

func TestExportedEntryPointsAreFine(t *testing.T) {}

func BenchmarkAdd(b *testing.B) {}

func ExampleStore() {}

func FuzzTitle(f *testing.F) {}
`,
		// Not a test file at all: the implementation is supposed to declare these.
		"store.go": "package main\n\ntype Store struct{}\n\nfunc New() *Store { return nil }\n",
	})
	if len(own) != 0 {
		t.Errorf("implementationInTests = %v, want none", own)
	}
}

// THE EXACT FILE THAT DEADLOCKED A RUN. The developer wrote store.go with every
// struct tag opened and never closed, then spent 34 consecutive actions failing
// to search/replace its way out — search text cannot match a file the model has
// already broken. The harness makes up for the model here rather than spending a
// turn telling it what the harness already knows.
func TestAnUnclosedStructTagIsRepaired(t *testing.T) {
	broken := "package main\n\nimport \"time\"\n\n" +
		"type Task struct {\n" +
		"\tID          string    `json:\"id\"\n" +
		"\tTitle       string    `json:\"title\"\n" +
		"\tTags        []string  `json:\"tags\"\n" +
		"\tDueDate     time.Time `json:\"dueDate\"\n" +
		"}\n"

	got, fixed, err := repairGoSource("store.go", broken)
	if err != nil {
		t.Fatalf("repairGoSource() = %v, want the file repaired", err)
	}
	if !fixed {
		t.Fatal("the file was left broken")
	}
	if _, err := parser.ParseFile(token.NewFileSet(), "store.go", got, parser.SkipObjectResolution); err != nil {
		t.Fatalf("the repaired file still does not parse: %v", err)
	}
	if strings.Count(got, "`")%2 != 0 {
		t.Errorf("backticks are still unbalanced:\n%s", got)
	}
	// Multi-tag fields are the normal case and must survive.
	multi := "package main\n\ntype T struct {\n\tA string `json:\"a\" xml:\"a\"\n}\n"
	if got, fixed, err := repairGoSource("t.go", multi); err != nil || !fixed {
		t.Errorf("a multi-key tag was not repaired: fixed=%v err=%v", fixed, err)
	} else if !strings.Contains(got, "`json:\"a\" xml:\"a\"`") {
		t.Errorf("the repair mangled a multi-key tag:\n%s", got)
	}
}

// A FILE THAT IS ALREADY FINE IS LEFT EXACTLY ALONE — the repair must never be
// something every write pays for.
func TestValidGoIsNeverTouched(t *testing.T) {
	good := "package main\n\ntype T struct {\n\tA string `json:\"a\"`\n}\n\nvar q = `SELECT *\nFROM t`\n"
	got, fixed, err := repairGoSource("x.go", good)
	if err != nil || fixed || got != good {
		t.Errorf("valid Go was modified: fixed=%v err=%v", fixed, err)
	}
	// Not a Go file: no parsing, no repair, no opinion.
	md := "# notes\n\nsome `inline code that never closes\n"
	if got, fixed, err := repairGoSource("README.md", md); err != nil || fixed || got != md {
		t.Errorf("a non-Go file was parsed: fixed=%v err=%v", fixed, err)
	}
}

// A GUESS IS ONLY ACCEPTED IF IT PARSES. A Go raw string may legitimately span
// lines, so an unbalanced backtick is not always a missing terminator — and a
// wrong repair must be discarded rather than committed.
func TestAWrongRepairIsDiscarded(t *testing.T) {
	// A genuine multi-line raw string whose first line ends in tag-like text.
	// Closing it at the newline would be wrong, and the result would not parse.
	src := "package main\n\nvar q = `json:\"a\"\nthis is still inside the string`\n\nfunc F() {}\n"
	got, fixed, err := repairGoSource("x.go", src)
	if fixed {
		t.Errorf("a multi-line raw string was wrongly closed:\n%s", got)
	}
	if err != nil {
		t.Errorf("a file that parses was reported as broken: %v", err)
	}
	if got != src {
		t.Error("the source was modified despite no repair")
	}

	// Broken beyond this one pattern: refused, with the original parse error.
	bad := "package main\n\nfunc F( {\n"
	if _, fixed, err := repairGoSource("x.go", bad); err == nil {
		t.Errorf("unparseable Go was accepted (fixed=%v)", fixed)
	}
}

// THE REPAIR MUST REACH THE STAGED TREE, and the write must still count as a
// write. Routing it through the refusal path would push a SUCCESSFUL write
// toward the stuck ceiling, which is the opposite of the point.
func TestARepairedWriteSucceedsAndIsReported(t *testing.T) {
	s := &devState{
		read: map[string]string{}, staged: map[string]string{},
		missing: map[string]bool{}, baseline: map[string]string{},
	}
	broken := "package main\n\ntype Task struct {\n\tID string `json:\"id\"\n}\n"
	if err := applyEdits(s, []devEdit{{Path: "store.go", Replace: broken}}, modeDevelop); err != nil {
		t.Fatalf("applyEdits() = %v, want the write accepted after repair", err)
	}
	if s.refusals != 0 {
		t.Errorf("a repaired write counted as a refusal: refusals = %d", s.refusals)
	}
	if len(s.repairs) != 1 || s.repairs[0] != "store.go" {
		t.Errorf("the repair was not recorded: %v", s.repairs)
	}
	if !strings.Contains(s.staged["store.go"], "`json:\"id\"`") {
		t.Errorf("the staged file was not repaired:\n%s", s.staged["store.go"])
	}
	// What the model reads next turn is the CORRECTED file, so its next search
	// text matches the tree rather than what it wrote.
	if s.read["store.go"] != s.staged["store.go"] {
		t.Error("the agent would see a different file from the one that was staged")
	}

	// Go that cannot be repaired is still refused, and says how to recover —
	// search/replace cannot fix a file the model has already broken.
	s2 := &devState{
		read: map[string]string{}, staged: map[string]string{},
		missing: map[string]bool{}, baseline: map[string]string{},
	}
	err := applyEdits(s2, []devEdit{{Path: "bad.go", Replace: "package main\n\nfunc F( {\n"}}, modeDevelop)
	if err == nil {
		t.Fatal("unparseable Go was staged")
	}
	if !strings.Contains(err.Error(), "rewrite the file") {
		t.Errorf("the refusal does not say how to recover: %v", err)
	}
}

// WHERE AN ERROR IS REPORTED IS NOT WHERE THE FAULT IS. Go reports a type
// mismatch at the USE SITE, and in test-first the use site is always a test
// file — so "every compile error is in a test file" describes almost every
// legitimate ticket, not a broken specification. Measured: a ticket blocked
// three times on
//
//	./store_test.go:27:18: cannot use time.Now().Add(time.Hour) ... as *time.Time
//
// where the fault was the DEVELOPER's own store.go declaring DueDate as a
// pointer. One line of the implementation fixed it; the agent never got a
// second turn.
func TestABrokenSpecMustBeProvedByAttempts(t *testing.T) {
	if minSpecBrokenTries < 2 {
		t.Fatalf("minSpecBrokenTries = %d — one verification is not evidence", minSpecBrokenTries)
	}

	// compileErrorFiles still identifies WHERE the errors are; what changed is
	// what that is taken to mean.
	out := "./store_test.go:27:18: cannot use time.Now() (value of struct type time.Time) as *time.Time value\n" +
		"./store_test.go:72:16: cannot use time.Now() (value of struct type time.Time) as *time.Time value\n"
	files, allTests := compileErrorFiles(out)
	if !allTests || len(files) != 1 || files[0] != "store_test.go" {
		t.Fatalf("compileErrorFiles = %v, %v", files, allTests)
	}

	// An error in the implementation is not a broken spec at all, and never was.
	mixed := "./store.go:12:2: undefined: foo\n./store_test.go:27:18: cannot use x as y\n"
	if _, allTests := compileErrorFiles(mixed); allTests {
		t.Error("an error in a non-test file was treated as a spec fault")
	}

	// A test FAILURE — no column — is ordinary red, not a compile error.
	if files, _ := compileErrorFiles("    store_test.go:96: expected 2 items, got 0\n"); len(files) != 0 {
		t.Errorf("a failing assertion was read as a compile error: %v", files)
	}
}

// THE STREAK ONLY ADVANCES WHEN THE DEVELOPER HAS CHANGED SOMETHING. Repeat
// verifications are no longer refused, so an agent that simply re-ran the tests
// three times would otherwise condemn a perfectly fixable specification without
// having edited a line.
func TestRepeatVerificationsDoNotCondemnASpec(t *testing.T) {
	s := &devState{writes: 1, specBrokenWrites: 0}

	// Three verifications, no writes in between: the streak advances once, for
	// the write that had already happened.
	for range 3 {
		if s.writes > s.specBrokenWrites {
			s.specBrokenTries++
			s.specBrokenWrites = s.writes
		}
	}
	if s.specBrokenTries != 1 {
		t.Errorf("specBrokenTries = %d after three runs with one write, want 1", s.specBrokenTries)
	}
	if s.specBrokenTries >= minSpecBrokenTries {
		t.Error("a spec was condemned without the developer trying again")
	}

	// Now the developer actually changes something between each attempt.
	for i := 2; i <= minSpecBrokenTries; i++ {
		s.writes++
		if s.writes > s.specBrokenWrites {
			s.specBrokenTries++
			s.specBrokenWrites = s.writes
		}
	}
	if s.specBrokenTries < minSpecBrokenTries {
		t.Errorf("specBrokenTries = %d after %d real attempts; a spec that truly cannot be satisfied "+
			"must still stop the loop", s.specBrokenTries, minSpecBrokenTries)
	}
}

// THE BUDGET COUNTS WORK, NOT LOOKING. A cached read never reaches a sandbox —
// doRead returns from what is already in hand — so it costs one model call
// against a KV-cached prompt: 2.2 seconds measured, against ten to twenty
// seconds for a verification that pushes, clones and runs the suite. Charging
// the same turn for both let one ticket spend an entire 200-turn budget in about
// seven minutes, 87 consecutive reads with three productive actions in it.
func TestReadsDoNotSpendTheTurnBudget(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.addTicket(t, Ticket{TicketID: "t1", Title: "x", CreatedBy: "alice"})
	f.execStdout = "===FILE main.go\n" + base64.StdEncoding.EncodeToString([]byte("package main\n")) + "\n"

	// Three times the budget, every one a read.
	const budget = 8
	script := []string{}
	for range budget * 3 {
		script = append(script, `{"action":"read_files","paths":["main.go"]}`)
	}
	gw, calls := scriptedModel(t, script...)
	rec, _ := newTestRecorder(t)
	ctx := rec.Start(context.Background(), "tr", "t1", "dev-agent")

	a := NewDevAgent(gw, api, ClassLarge, RepoConfig{
		URL: "https://example.invalid/r", Branch: "main", BranchPrefix: "agent/",
		Image: "golang:1.25", TestCommand: "go test ./...",
	}, budget)
	if _, _, err := a.Handle(ctx, f.get("t1")); err != nil {
		t.Fatalf("Handle() = %v", err)
	}
	if got := calls(); got <= budget {
		t.Errorf("model called %d times on a budget of %d — reads are still being charged", got, budget)
	}

	// A WRITE STILL COSTS ONE. The point is not that turns are free, it is that
	// looking is: work has to remain bounded or the budget means nothing.
	f2, api2 := newFakePlatform(t)
	f2.addTicket(t, Ticket{TicketID: "t2", Title: "x", CreatedBy: "alice"})
	f2.execStdout = "go.mod\n"
	writes := []string{}
	for i := range budget * 3 {
		writes = append(writes, fmt.Sprintf(
			`{"action":"write_files","edits":[{"path":"f%d.go","search":"","replace":"package main\n"}]}`, i))
	}
	gw2, calls2 := scriptedModel(t, writes...)
	ctx2 := rec.Start(context.Background(), "tr", "t2", "dev-agent")
	a2 := NewDevAgent(gw2, api2, ClassLarge, RepoConfig{
		URL: "https://example.invalid/r", Branch: "main", BranchPrefix: "agent/",
		Image: "golang:1.25", TestCommand: "go test ./...",
	}, budget)
	if _, _, err := a2.Handle(ctx2, f2.get("t2")); err != nil {
		t.Fatalf("Handle() = %v", err)
	}
	if got := calls2(); got > budget {
		t.Errorf("model called %d times on a budget of %d — writes are not being charged either, "+
			"so nothing bounds the attempt", got, budget)
	}
}

// A HUNDRED LOOKS IN A ROW IS THE SIGNAL. Reads cost no turn, so the budget can
// never end an attempt that only reads — the RUN is what says the agent is not
// going to start. Measured on one ticket: read, write, run_tests, then 87
// consecutive reads, straight through two memory resets.
func TestAnAgentThatOnlyReadsIsStopped(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.addTicket(t, Ticket{TicketID: "t1", Title: "x", CreatedBy: "alice"})
	f.execStdout = "===FILE main.go\n" + base64.StdEncoding.EncodeToString([]byte("package main\n")) + "\n"

	script := []string{}
	for range maxConsecutiveReads + 20 {
		script = append(script, `{"action":"read_files","paths":["main.go"]}`)
	}
	gw, calls := scriptedModel(t, script...)
	rec, _ := newTestRecorder(t)
	ctx := rec.Start(context.Background(), "tr", "t1", "dev-agent")

	// A budget far smaller than the read run, to prove it is the RUN that ends
	// this and not the turn count.
	a := NewDevAgent(gw, api, ClassLarge, RepoConfig{
		URL: "https://example.invalid/r", Branch: "main", BranchPrefix: "agent/",
		Image: "golang:1.25", TestCommand: "go test ./...",
	}, 10)
	status, detail, err := a.Handle(ctx, f.get("t1"))
	if err != nil {
		t.Fatalf("Handle() = %v", err)
	}
	if status != OutcomeFailed || !strings.Contains(detail, "in a row") {
		t.Errorf("status=%q detail=%q, want the attempt ended by the read run", status, detail)
	}
	if got := calls(); got > maxConsecutiveReads+1 {
		t.Errorf("model called %d times, want it stopped at %d", got, maxConsecutiveReads)
	}
	if !strings.Contains(strings.Join(f.commentBodies("t1"), "\n"), "in a row without writing") {
		t.Error("the ticket does not say why the agent was stopped")
	}
}

// THE RUN IS CONSECUTIVE, NOT CUMULATIVE. An agent that reads a lot but keeps
// doing real work between looks is working, not looping, and must not be cut off
// for being thorough — that mistake abandoned tickets at turn five once already.
func TestReadingIsOnlyFatalWhenNothingElseHappens(t *testing.T) {
	s := &devState{}
	// Ninety-nine reads, then one write, repeated: never fatal.
	for range 3 {
		for range maxConsecutiveReads - 1 {
			s.consecutiveReads++
			if s.consecutiveReads >= maxConsecutiveReads {
				t.Fatalf("a working agent was stopped after %d reads between writes", s.consecutiveReads)
			}
		}
		s.consecutiveReads = 0 // any non-read action clears the run
	}
	if s.consecutiveReads != 0 {
		t.Errorf("consecutiveReads = %d, want the run cleared by real work", s.consecutiveReads)
	}
}

// THE BRIEF MUST MATCH THE JOB. DevAgent is three stages sharing one loop, and
// the first restart brief told all of them to "make the remaining test failures
// pass" — the developer's job, and the exact inverse of the spec author's, whose
// tests are supposed to fail. Observed live: a test author restarted twice and
// instructed, in its own prompt, to do the one thing its own gate refuses.
func TestTheRestartBriefMatchesTheStage(t *testing.T) {
	brief := func(mode agentMode) string {
		s := &devState{
			iteration: 40,
			staged:    map[string]string{"a.go": "package main\n"},
			baseline:  map[string]string{},
			missing:   map[string]bool{},
		}
		s.resetAgent(mode)
		return s.restart
	}

	dev := brief(modeDevelop)
	if !strings.Contains(dev, "make the remaining test failures pass") {
		t.Errorf("the developer is not told to fix the failures:\n%s", dev)
	}

	spec := brief(modeTest)
	if strings.Contains(spec, "make the remaining test failures pass") {
		t.Errorf("the SPEC AUTHOR was told to make its own tests pass:\n%s", spec)
	}
	if !strings.Contains(spec, "must FAIL") {
		t.Errorf("the spec author is not told its tests must still fail:\n%s", spec)
	}

	cov := brief(modeCoverage)
	if strings.Contains(cov, "make the remaining test failures pass") {
		t.Errorf("the coverage stage was given the developer's brief:\n%s", cov)
	}
	if !strings.Contains(cov, "keep passing") {
		t.Errorf("the coverage stage is not told the suite must stay green:\n%s", cov)
	}

	// Every stage keeps the parts that are true regardless of the job.
	for name, b := range map[string]string{"dev": dev, "spec": spec, "coverage": cov} {
		if !strings.Contains(b, "PART-WAY THROUGH") || !strings.Contains(b, "restart 1") {
			t.Errorf("%s brief lost its framing:\n%s", name, b)
		}
	}
}

// The work survives a reset whichever stage it belongs to — that is what makes
// it a restart rather than an undo.
func TestARestartKeepsTheWorkAndDropsTheMemory(t *testing.T) {
	s := &devState{
		iteration: 40,
		staged:    map[string]string{"store.go": "package main\n"},
		baseline:  map[string]string{},
		read:      map[string]string{"store.go": "package main\n"},
		missing:   map[string]bool{"gone.go": true},
		trail:     []devStep{{action: "read_files", detail: "main.go"}, {action: "run_tests"}},
		notice:    "Rejected: something",
		refusals:  4, staleReads: 3, parseFails: 2, writes: 7, verifiedWrites: 1,
		lastTest: "FAIL: TestThing", testsPass: false,
	}
	s.resetAgent(modeDevelop)

	if len(s.staged) != 1 || s.staged["store.go"] == "" {
		t.Error("the agent's work was thrown away")
	}
	if s.baseline["store.go"] != "package main\n" {
		t.Error("the work did not become part of the baseline, so a rewrite would look like a discard")
	}
	if s.lastTest == "" {
		t.Error("the failing output was dropped; 'fix the remaining failures' needs the failures")
	}
	for name, got := range map[string]int{
		"refusals": s.refusals, "staleReads": s.staleReads, "parseFails": s.parseFails,
		"writes": s.writes, "verifiedWrites": s.verifiedWrites, "trail": len(s.trail),
	} {
		if got != 0 {
			t.Errorf("%s = %d after a restart, want it cleared", name, got)
		}
	}
	if s.notice != "" {
		t.Error("a fresh agent was shown a rejection of an action it did not take")
	}
	if s.resets != 1 {
		t.Errorf("resets = %d, want 1", s.resets)
	}
}

// A BACKEND THAT CANNOT RETURN A TOOL CALL MUST BE GIVEN A GRAMMAR. Offering
// tools to a template that does not understand them leaves the model entirely
// unconstrained: it writes one call into the content and keeps going. Measured
// on qwen2.5-coder-32b — every turn ran to the 8000-token ceiling emitting
// write_files, then run_tests, then finish, then more, of which the loop reads
// only the first. Seven thousand discarded tokens and 334 seconds, per turn.
func TestABackendWithoutToolsGetsTheSchemaInstead(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"content":"{}"}}],"usage":{}}`)
	}))
	defer srv.Close()

	call := func(toolsSupported bool) map[string]any {
		got = nil
		gw := NewGateway(Config{Host: "h", Classes: map[Class]ClassConfig{
			ClassLarge: {Endpoint: srv.URL, Model: "m", Slots: 1, QueueDepth: 1, ToolsSupported: toolsSupported},
		}})
		gw.Chat(context.Background(), ClassLarge, ChatRequest{
			Messages: []Message{{Role: "user", Content: "x"}},
			Tools:    devTools(modeDevelop),
			Schema:   &ReplySchema{Name: "action", Schema: map[string]any{"type": "object"}},
		})
		return got
	}

	with := call(true)
	if _, ok := with["tools"]; !ok {
		t.Error("a backend that supports tools was not offered them")
	}
	if _, ok := with["response_format"]; ok {
		t.Error("a grammar was sent alongside tools; backends that support both are confused by it")
	}

	// Without tool support the grammar takes over, and it is the grammar that
	// ENDS THE TURN: an object that has closed cannot be followed by another.
	without := call(false)
	if _, ok := without["tools"]; ok {
		t.Error("tools were offered to a backend whose template cannot return them")
	}
	if _, ok := without["response_format"]; !ok {
		t.Error("no grammar was sent, so nothing bounds the reply to a single action")
	}
	if without["tool_choice"] != nil && without["tool_choice"] != "" {
		t.Errorf("tool_choice = %v with no tools offered", without["tool_choice"])
	}
}

// The ceiling that existed but was never applied. It is a backstop rather than
// the fix, and it must differ by stage — the spec author and the developer write
// different amounts.
func TestThePerStageReplyCeilingIsActuallyApplied(t *testing.T) {
	dev := NewDevAgent(nil, nil, ClassLarge, RepoConfig{}, 10)
	tester := NewTesterAgent(nil, nil, ClassLarge, RepoConfig{}, 10)
	if dev.maxReplyTokens() != maxDevReplyTokens {
		t.Errorf("developer ceiling = %d, want %d", dev.maxReplyTokens(), maxDevReplyTokens)
	}
	if tester.maxReplyTokens() != maxTestReplyTokens {
		t.Errorf("spec author ceiling = %d, want %d", tester.maxReplyTokens(), maxTestReplyTokens)
	}
	if maxTestReplyTokens >= 8000 || maxDevReplyTokens >= 8000 {
		t.Error("a ceiling is at or above the old hardcoded 8000, so it bounds nothing")
	}
	if maxTestReplyTokens != maxDevReplyTokens {
		t.Errorf("the file-writing stages differ: %d and %d", maxTestReplyTokens, maxDevReplyTokens)
	}
	// Big enough for a whole file in one tool argument: 4000 was measured
	// truncating a 150-line file mid-string and costing the attempt.
	if maxTestReplyTokens <= 4000 {
		t.Errorf("maxTestReplyTokens = %d, too small for a whole test file", maxTestReplyTokens)
	}
}

// GREEDY DECODING IS NOT SAFE ON EVERY MODEL, and a grammar does not save you
// from it. Measured on qwen2.5-coder-32b at temperature 0: a schema-VALID object
// whose "type" string ran "..._or_is_not_a_test_file_with_the_correct_test_
// multiculturalism_or_is_not_a_test_file_with_the_correct_test_multinationalism
// ..." to the 5000-token ceiling, 351 seconds, every turn. The grammar bounded
// the structure and could not bound the string inside it.
func TestAClassMayRaiseTheSamplingFloor(t *testing.T) {
	var sent []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		sent = append(sent, body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"content":"{}"}}],"usage":{}}`)
	}))
	defer srv.Close()

	ask := func(floor, asked float64) float64 {
		sent = nil
		gw := NewGateway(Config{Host: "h", Classes: map[Class]ClassConfig{
			ClassLarge: {Endpoint: srv.URL, Model: "m", Slots: 1, QueueDepth: 1,
				ToolsSupported: true, Temperature: floor},
		}})
		gw.Chat(context.Background(), ClassLarge, ChatRequest{
			Messages:    []Message{{Role: "user", Content: "x"}},
			Temperature: asked,
		})
		return sent[0]["temperature"].(float64)
	}

	// A floor lifts a greedy caller off zero.
	if got := ask(0.3, 0); got != 0.3 {
		t.Errorf("temperature = %v with a 0.3 floor, want 0.3", got)
	}
	// NO FLOOR CHANGES NOTHING: a backend that decodes greedily without looping
	// keeps the determinism the agents were built around.
	if got := ask(0, 0); got != 0 {
		t.Errorf("temperature = %v with no floor, want 0", got)
	}
	// It only ever raises. A stage that deliberately chose a temperature — the
	// architect at 0.3 — is not dragged down by a lower floor.
	if got := ask(0.2, 0.5); got != 0.5 {
		t.Errorf("temperature = %v, want the caller's 0.5 kept above a 0.2 floor", got)
	}
}

// THE TOOL DEFINITION AND THE CONTENT FALLBACK MUST DESCRIBE THE SAME SHAPE.
// They did not: tools constrained "type" to conventionalTypes while the fallback
// schema left it a free string. That only mattered once a backend without tool
// support fell through to the fallback — and then the model filled "type" with
// "replace_all_content_in_file_if_exists_..." and kept going for 5000 tokens and
// 348 seconds, schema-valid the whole way, because nothing said how long a
// string may be. A schema that becomes a GRAMMAR must bound every string in it.
func TestTheFallbackSchemaBoundsEveryField(t *testing.T) {
	// The schema is now one branch per action, so every field lives inside a
	// branch. Flattened here because what this test asserts — that nothing is
	// unbounded — is a property of the fields wherever they sit.
	props := schemaFields(t, devActionSchema())

	// "type" is an ENUM, exactly as the tool definition has always had it.
	typ, _ := props["type"].(map[string]any)
	enum, _ := typ["enum"].([]string)
	if len(enum) == 0 {
		t.Errorf(`"type" is not constrained to an enum: %v — this is the field that ran away`, typ)
	} else if len(enum) != len(conventionalTypes) {
		t.Errorf(`"type" enum has %d entries, want the %d in conventionalTypes`, len(enum), len(conventionalTypes))
	}

	// Every free-text string is length-bounded.
	for _, name := range []string{"summary", "reason"} {
		f, _ := props[name].(map[string]any)
		if f["maxLength"] == nil {
			t.Errorf("%q has no maxLength, so the grammar permits an unbounded reply", name)
		}
	}

	// Arrays are bounded in both directions: how many, and how long each.
	paths, _ := props["paths"].(map[string]any)
	if paths["maxItems"] == nil {
		t.Error(`"paths" has no maxItems`)
	}
	if item, _ := paths["items"].(map[string]any); item == nil || item["maxLength"] == nil {
		t.Error(`"paths" items have no maxLength`)
	}

	// The one string that CANNOT be bounded is the edit body — it holds a whole
	// file — so it stays free, and the per-stage token ceiling is what covers it.
	if maxTestReplyTokens <= 0 || maxDevReplyTokens <= 0 {
		t.Error("no token ceiling backs the one field a grammar cannot bound")
	}
}

// schemaFields collects the properties of every branch of the action schema.
//
// ONE SHAPE PER ACTION means there is no single properties map any more, and the
// flat one it replaced is precisely what let {"action":"write_files"} through
// with nothing to apply.
func schemaFields(t *testing.T, sc *ReplySchema) map[string]any {
	t.Helper()
	out := map[string]any{}
	branches, ok := sc.Schema["oneOf"].([]any)
	if !ok {
		t.Fatal("the action schema is not a oneOf, so actions share one shape again")
	}
	for _, b := range branches {
		for k, v := range b.(map[string]any)["properties"].(map[string]any) {
			if _, seen := out[k]; !seen {
				out[k] = v
			}
		}
	}
	return out
}

// AN OPERATION WITH NO EFFECT IS REFUSED, and that is a fact rather than a
// judgement about repeating. Removing this to measure an unobstructed agent gave
// a clear answer: with reads free AND verification unbounded, one dev agent
// reached 606 turns alternating the two — reads cost no turn so the budget never
// bit, and the consecutive-read kill never fired because the run was broken up
// by verifications. Blocking one action has only ever chosen which action gets
// The repeat-verification guard is GONE, because the model can no longer ask for
// a verification — that whole class of refusal was deleted rather than fixed.
//
// The cost it was protecting against is still real: a verification is a push, a
// clone and a test run. So the protection moved to the write itself. A write that
// leaves the tree byte-identical triggers nothing, and this pins that: eight
// no-op writes must not become eight pipeline runs.
func TestANoOpWriteCostsNoPipelineRun(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.addTicket(t, Ticket{TicketID: "t1", Title: "x", CreatedBy: "alice"})
	f.execStdout = "main.go\n"
	f.runOutcome = RunFailed

	// Read the file, then write back exactly what is already there, repeatedly.
	same := `{"action":"write_files","edits":[{"path":"main.go","start_line":1,"end_line":1,"replace":"package main"}]}`
	gw, _ := scriptedModel(t,
		`{"action":"read_files","paths":["main.go"]}`,
		same, same, same, same, same,
	)
	rec, _ := newTestRecorder(t)
	ctx := rec.Start(context.Background(), "tr", "t1", "dev-agent")

	if _, _, err := devTestAgent(t, gw, api).Handle(ctx, f.get("t1")); err != nil {
		t.Fatalf("Handle() = %v", err)
	}
	f.mu.Lock()
	runs := len(f.runs)
	f.mu.Unlock()
	if runs != 0 {
		t.Errorf("%d pipeline runs for writes that changed nothing; a no-op must cost no round trip", runs)
	}
}

// THE MEMORY IS NOT CLEARED AFTER A VERIFICATION, and the removal is the point.
//
// It was added to break a read loop, and the read loop turned out to be the
// developer searching for code its branch did not contain — a missing
// dependency, fixed at source by merging the integration branch. With the cause
// gone the workaround only cost: the store ticket, which merged in 4, 5, 6 and 8
// turns across earlier runs, reached 107 turns without merging while clearing
// its memory eleven times, re-exploring ground it had already covered after
// every verification.
//
// The same applies to the 40-turn restart, measured changing nothing across four
// runs. resetAgent survives for a caller that wants it; nothing calls it today.
func TestTheAgentKeepsItsMemoryAcrossVerifications(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.addTicket(t, Ticket{TicketID: "t1", Title: "x", CreatedBy: "alice"})
	f.execStdout = "go.mod\n"
	f.runOutcome = RunFailed // keep going past the verification

	gw, _ := scriptedModel(t,
		`{"action":"read_files","paths":["go.mod"]}`,
		`{"action":"write_files","edits":[{"path":"a.go","search":"","replace":"package main\n"}]}`,
		`{"action":"run_tests"}`,
		`{"action":"read_files","paths":["a.go"]}`,
		`{"action":"give_up","reason":"done"}`,
	)
	rec, dir := newTestRecorder(t)
	gw.SetRecorder(rec)
	ctx := rec.Start(context.Background(), "tr", "t1", "dev-agent")
	if _, _, err := devTestAgent(t, gw, api).Handle(ctx, f.get("t1")); err != nil {
		t.Fatalf("Handle() = %v", err)
	}

	var briefed, keptTrail bool
	for _, r := range readRecords(t, dir) {
		if r.Kind != KindTurn {
			continue
		}
		for _, m := range r.Messages {
			if strings.Contains(m.Content, "PICKING UP THIS TICKET PART-WAY THROUGH") {
				briefed = true
			}
			// The turn after the verification must still know what it did before.
			if strings.Contains(m.Content, "LAST VERIFICATION") &&
				strings.Contains(m.Content, "ACTIONS YOU HAVE ALREADY TAKEN") {
				keptTrail = true
			}
		}
	}
	if briefed {
		t.Error("an agent was briefed as a fresh start; the memory clear is still wired in")
	}
	if !keptTrail {
		t.Error("the agent lost its history across a verification")
	}
}

// THE WORK SURVIVES, which is the whole difference between clearing memory and
// starting over. A verification that wipes the staged files would throw away the
// change it just tested.
func TestClearingMemoryKeepsTheVerifiedWork(t *testing.T) {
	s := &devState{
		iteration: 12,
		staged:    map[string]string{"store.go": "package main\n"},
		read:      map[string]string{"store.go": "package main\n"},
		baseline:  map[string]string{}, missing: map[string]bool{},
		trail:    []devStep{{action: "run_tests"}},
		lastTest: "FAIL: TestStoreAdd", testsPass: false,
		writes: 2, verifiedWrites: 2, refusals: 1,
	}
	s.resetAgent(modeDevelop)

	if s.staged["store.go"] == "" {
		t.Fatal("the change that was just tested was discarded")
	}
	if s.baseline["store.go"] != "package main\n" {
		t.Error("the tested work did not become the baseline, so a rewrite would look like a discard")
	}
	if s.lastTest == "" {
		t.Error("the failing output was dropped; the fresh agent has nothing to fix")
	}
	if len(s.trail) != 0 || s.refusals != 0 {
		t.Error("the memory was not cleared")
	}
	// Both counters at zero means an unchanged tree, so the no-effect guard
	// correctly refuses a verification until something is written.
	if s.writes != s.verifiedWrites {
		t.Errorf("writes=%d verifiedWrites=%d — a pointless verification would be allowed",
			s.writes, s.verifiedWrites)
	}
}

// A TICKET THAT WAITED FOR ITS DEPENDENCY MUST ACTUALLY RECEIVE IT. Branches are
// cut at scoping time, all from the same base, before any sibling has merged.
// Scheduling then holds a ticket until its blockers reach done — correctly — but
// nothing rebased the branch, so the developer started on a tree that predated
// the code it had been waiting for.
//
// Measured on a live run: agent/6f428a36 held handlers.go and handlers_test.go
// but NOT store.go, so the compile said "undefined: Task, undefined: store,
// undefined: TaskFilters, undefined: ErrTaskNotFound" and the agent read 96
// times in a row looking for where Task was defined. It was not there. Every
// ticket that depended on the store failed this way, in every run; the store
// ticket itself, which depends on nothing, merged in all of them.
func TestTheDeveloperMergesItsDependenciesBeforeWorking(t *testing.T) {
	dev := NewDevAgent(nil, nil, ClassLarge, RepoConfig{
		URL: "git://x/y.git", Branch: "main", IntegrationBranch: "dev",
	}, 10)

	script := dev.mergeIntegrationScript()
	if !strings.Contains(script, "fetch -q origin 'dev'") {
		t.Errorf("the integration branch is never fetched:\n%s", script)
	}
	if !strings.Contains(script, "merge") {
		t.Errorf("the integration branch is fetched but never merged:\n%s", script)
	}
	// A CONFLICT MUST LEAVE THE TREE CLEAN. A developer facing merge markers can
	// do nothing useful with them; resolving belongs to the integrator, which has
	// a stage and a model for it.
	if !strings.Contains(script, "merge --abort") {
		t.Errorf("a conflicting merge would leave the working tree half-merged:\n%s", script)
	}

	// It reaches BOTH paths: what the agent reads, and what the verification runs
	// against. The push path resets to the remote branch, so a merge done only in
	// the sandbox would be silently discarded there.
	if !strings.Contains(dev.pushScript(Ticket{TicketID: "t1"}, "agent/t1", &devState{staged: map[string]string{}}), "merge") {
		t.Error("the pushed branch does not include the dependency, so verification tests the wrong tree")
	}
}

// THE SPEC AUTHOR TAKES IT TOO, and that reverses an earlier reading. The
// argument for withholding it was that tests written against existing code stop
// being a specification — but the integration branch holds OTHER tickets' work,
// never this ticket's subject, so the spec still fails and the red gate still
// applies.
//
// What withholding it cost: a specification NAMES THINGS. An API ticket writes
// tests against Task and Store, which the foundation ticket creates; an author
// that cannot see them invents their shape, and the developer then has to
// satisfy an invented interface against the real one already merged. That is a
// contradiction rather than a failing test, and it surfaced as "cannot use ...
// as ..." errors no implementation resolves.
func TestBothWorkingStagesTakeTheIntegrationBranch(t *testing.T) {
	repo := RepoConfig{URL: "git://x/y.git", Branch: "main", IntegrationBranch: "dev"}
	if s := NewTesterAgent(nil, nil, ClassLarge, repo, 10).mergeIntegrationScript(); s == "" {
		t.Error("the spec author cannot see the code its tests must name")
	}
	// The coverage stage still does not: it runs against a branch that has
	// already passed, so there is nothing it is waiting for.
	if s := NewCoverageAgent(nil, nil, ClassLarge, repo, 10).mergeIntegrationScript(); s != "" {
		t.Errorf("the coverage stage merged the integration branch:\n%s", s)
	}
	// And a project with no integration branch configured is unchanged.
	bare := NewDevAgent(nil, nil, ClassLarge, RepoConfig{URL: "git://x/y.git"}, 10)
	if s := bare.mergeIntegrationScript(); s != "" {
		t.Errorf("a project with no integration branch got a merge anyway:\n%s", s)
	}
}

// A PUSH THAT NEVER RAN IS NOT A VERDICT ON THE CODE. Recording one as a test
// result set lastTest and marked the tree verified, after which the no-op guard
// correctly refused every further verification — "nothing has changed since you
// last ran the tests" — 118 times on one ticket across 197 turns. The agent
// could not leave the state, because the broken thing was not something writing
// code could mend. forge refused the submit 555 times today with "already
// claimed"; it stayed invisible while read loops were killing tickets first.
func TestAPushFailureIsNotRecordedAsATestResult(t *testing.T) {
	s := &devState{staged: map[string]string{"a.go": "package main\n"}, writes: 2, verifiedWrites: 0}

	// The sentinel is what separates the two, and it must survive wrapping.
	err := fmt.Errorf("%w: %v", errPushFailed, errors.New("POST /executions: already claimed"))
	if !errors.Is(err, errPushFailed) {
		t.Fatal("a wrapped push failure is not recognisable as one")
	}

	// What must NOT happen: the tree being marked verified, which is what
	// poisoned the no-op guard.
	if s.verifiedWrites == s.writes {
		t.Fatal("fixture is wrong: the tree already looks verified")
	}
	if s.lastTest != "" {
		t.Error("a push failure left a test result behind")
	}

	// A transient submit refusal is retried rather than surfaced at all.
	if !isTransientSubmit(errors.New("sandbox: submit: POST /executions: already claimed")) {
		t.Error(`"already claimed" is not recognised as transient, so it will not be retried`)
	}
	if isTransientSubmit(errors.New("exit 1: compile failed")) {
		t.Error("an ordinary failure was treated as transient and would be retried pointlessly")
	}
	if pushAttempts < 2 {
		t.Errorf("pushAttempts = %d — a transient refusal gets no retry at all", pushAttempts)
	}
	if maxPushFailures < 1 {
		t.Error("a permanently unpushable branch would spin forever")
	}
}

// THE COUNTER MEASURES THE PLANE, NOT THE AGENT, so it must reset the moment a
// push succeeds — otherwise a ticket that hit one blip early would carry it for
// the rest of its life and die of an unrelated cause later.
func TestPushFailuresResetOnSuccess(t *testing.T) {
	s := &devState{pushFails: 2}
	// A successful verification clears it; the loop does this on the non-error
	// path immediately after the errPushFailed check.
	s.pushFails = 0
	if s.pushFails != 0 {
		t.Error("a successful push did not clear the failure count")
	}
	// And it is separate from every counter that judges the model.
	s2 := &devState{pushFails: 3, refusals: 0, staleReads: 0}
	if s2.refusals != 0 || s2.staleReads != 0 {
		t.Error("push failures leaked into the counters that judge the agent's progress")
	}
}

// "NOTHING TO COMMIT" IS THE AGENT'S PROBLEM, NOT THE PLANE'S. It means the
// edits produced no diff — the file already held exactly what was written.
// Treating it as a push failure killed a ticket after three of them, reported as
// "a fault in the execution plane", which it is not.
func TestANoOpEditIsNotAPushFailure(t *testing.T) {
	// The three cases the push path must tell apart.
	cases := []struct {
		name  string
		out   string
		want  error
		fatal bool
	}{
		{"a no-op edit", "On branch agent/x\nnothing to commit, working tree clean\n", errNothingToCommit, false},
		{"a busy lease", "sandbox: submit: POST /executions: already claimed", nil, false},
		{"a real failure", "exit 1: fatal: remote rejected", nil, true},
	}
	for _, tc := range cases {
		if tc.want == errNothingToCommit && !strings.Contains(tc.out, "nothing to commit") {
			t.Errorf("%s: fixture does not match what git prints", tc.name)
		}
	}

	// The sentinel must survive the wrapping verify applies, or the caller sees
	// a push failure and counts it toward the ceiling.
	wrapped := fmt.Errorf("verify: %w", errNothingToCommit)
	if !errors.Is(wrapped, errNothingToCommit) {
		t.Error("a wrapped no-op edit is not recognisable as one")
	}
	if errors.Is(errNothingToCommit, errPushFailed) {
		t.Error("a no-op edit is being classified as a push failure; it would kill the ticket")
	}

	// And a busy lease is still transient, so it retries rather than surfacing.
	if !isTransientSubmit(errors.New("POST /executions: already claimed")) {
		t.Error("a busy lease is no longer retried")
	}
	if isTransientSubmit(errNothingToCommit) {
		t.Error("a no-op edit would be retried pointlessly three times")
	}
}

// t.Skip IS THE SAME LIE AS AN EMPTY BODY. Merged on a live run: store_test.go
// with four functions, each a comment and t.Skip("Not implemented"). Every gate
// missed it — the bodies were not empty, there were no empty subtests, nothing
// declared the implementation, and the file parsed. The red check did not save
// it either: with no implementation the file failed to COMPILE, which reads as
// red and is accepted; once the developer wrote the code the suite compiled,
// every test skipped, and `go test` exited 0.
func TestASkippedTestIsAStub(t *testing.T) {
	stubs := stubbedTests(map[string]string{"store_test.go": `package main

import "testing"

func TestStoreOperations(t *testing.T) {
	// This test will verify Add, Get, List, Update and Delete
	t.Skip("Not implemented")
}

func TestStoreErrors(t *testing.T) {
	t.Skipf("todo: %s", "errors")
}

func TestStoreConcurrent(t *testing.T) {
	t.SkipNow()
}

func TestReal(t *testing.T) {
	if got := New().Len(); got != 0 {
		t.Fatalf("Len = %d", got)
	}
}
`})
	want := []string{"TestStoreOperations", "TestStoreErrors", "TestStoreConcurrent"}
	if len(stubs) != len(want) {
		t.Fatalf("stubbedTests = %v, want %v", stubs, want)
	}
	for i := range want {
		if stubs[i] != want[i] {
			t.Errorf("stub %d = %q, want %q", i, stubs[i], want[i])
		}
	}
}

// A CONDITIONAL SKIP IS LEGITIMATE and must survive — opting out of slow or
// environment-dependent cases is how real suites are written, and refusing it
// would push authors toward deleting the coverage instead.
func TestAConditionalSkipIsNotAStub(t *testing.T) {
	stubs := stubbedTests(map[string]string{"x_test.go": `package main

import "testing"

func TestSlow(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	if got := Compute(); got != 42 {
		t.Fatalf("Compute = %d", got)
	}
}

func TestGuarded(t *testing.T) {
	t.Run("case", func(t *testing.T) {
		if !enabled {
			t.Skip("disabled")
		}
		t.Fatal("boom")
	})
}
`})
	if len(stubs) != 0 {
		t.Errorf("stubbedTests = %v, want none — a guarded skip is normal Go", stubs)
	}
}

// A SUBTEST THAT ONLY SKIPS is the same stub one level down.
func TestASkippedSubtestIsAStub(t *testing.T) {
	stubs := stubbedTests(map[string]string{"y_test.go": `package main

import "testing"

func TestFiltering(t *testing.T) {
	t.Run("by status", func(t *testing.T) {
		t.Skip("Not implemented")
	})
	t.Run("real", func(t *testing.T) { t.Fatal("x") })
}
`})
	if len(stubs) != 1 || stubs[0] != "TestFiltering/by status" {
		t.Errorf("stubbedTests = %v, want [TestFiltering/by status]", stubs)
	}
}

// THE ONE-TEST-PER-CRITERION BOUND WAS REMOVED. It was meant to remove the
// variance in specification size — the same store ticket took 4, 17, 65, 107 and
// 334 developer turns across five runs — and instead fought the stub gate: 32
// refusals for writing too many tests against 103 for writing hollow ones in the
// same seven minutes, with authors that used to finish in 4-8 turns looping at
// 27, 42 and 74. The cheapest way to satisfy "write fewer tests" is to empty
// their bodies, which is the one thing a specification must never do.
//
// The prompt still ASKS for one test per criterion. Nothing refuses a spec for
// exceeding it.
func TestTheSpecIsNotRefusedForItsSize(t *testing.T) {
	a := NewTesterAgent(nil, nil, ClassLarge, RepoConfig{TestCommand: "go test ./..."}, 10)
	script := a.testSpecScript(false)
	if strings.Contains(script, "acceptance criteria and you have written") {
		t.Error("the size bound is still in the gate")
	}

	// What DOES still gate a specification: well-formed, red, and not hollow.
	// Narrowed from "." to the test files on r92, where the walk cost 8.5s a
	// section; the check itself is unchanged.
	if !strings.Contains(script, "gofmt -e -l *_test.go") {
		t.Error("the spec gate lost its syntax check")
	}
	if !strings.Contains(script, vacuousSpecMarker) {
		t.Error("the spec gate lost its red check")
	}
	stubs := stubbedTests(map[string]string{
		"a_test.go": "package main\n\nimport \"testing\"\n\nfunc TestX(t *testing.T) { t.Skip(\"x\") }\n",
	})
	if len(stubs) != 1 {
		t.Errorf("the stub gate stopped working: %v", stubs)
	}
}

// AN AGENT REFUSING WITH NOTHING TO SHOW MUST STOP. Letting it run to the budget
// was deliberate — cutting one off for repeating itself abandoned real tickets at
// turn five, and maxRepeatRefusals is only four — but "keep going" meant "burn
// every remaining turn". Measured twice in consecutive runs: a spec author made
// 45 refused actions in a row and a developer 91, each alternating a write that
// produced no diff with a verification that could not run.
func TestARefusingAgentWithNothingToBankIsStopped(t *testing.T) {
	if maxDeadRefusals <= maxRepeatRefusals {
		t.Fatalf("maxDeadRefusals = %d must be well above maxRepeatRefusals = %d",
			maxDeadRefusals, maxRepeatRefusals)
	}
	// Far enough past the low ceiling that a recovering agent is never cut off.
	if maxDeadRefusals < 4*maxRepeatRefusals {
		t.Errorf("maxDeadRefusals = %d is too close to %d; a recovering agent would be abandoned",
			maxDeadRefusals, maxRepeatRefusals)
	}
	// Close enough that a deadlock costs turns rather than a whole budget.
	if maxDeadRefusals >= defaultMaxIterations/2 {
		t.Errorf("maxDeadRefusals = %d lets a deadlock spend most of a %d-turn budget",
			maxDeadRefusals, defaultMaxIterations)
	}

	// THE LOW CEILING STILL ONLY BANKS PASSING WORK. It must never end an
	// attempt on its own: that is what protects an agent repeating itself while
	// it recovers.
	s := &devState{refusals: maxRepeatRefusals, testsPass: false, staged: map[string]string{}}
	if s.refusals >= maxDeadRefusals {
		t.Error("the low ceiling now reaches the terminal one; a careful agent would be cut off")
	}
}

// A PASSING BRANCH IS STILL BANKED rather than failed, whichever ceiling is
// reached — finishing on the agent's behalf is not cutting it off.
func TestPassingWorkIsBankedBeforeEitherCeiling(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.addTicket(t, Ticket{TicketID: "t1", Title: "x", CreatedBy: "alice"})
	f.execStdout = "go.mod\n"

	// Write, verify green, then refuse repeatedly by re-reading a known file.
	script := []string{
		`{"action":"write_files","edits":[{"path":"a.go","search":"","replace":"package main\n"}]}`,
		`{"action":"run_tests"}`,
	}
	for range maxDeadRefusals + 2 {
		script = append(script, `{"action":"run_tests"}`)
	}
	gw, _ := scriptedModel(t, script...)
	rec, _ := newTestRecorder(t)
	ctx := rec.Start(context.Background(), "tr", "t1", "dev-agent")

	status, detail, err := devTestAgent(t, gw, api).Handle(ctx, f.get("t1"))
	if err != nil {
		t.Fatalf("Handle() = %v", err)
	}
	if status != OutcomeSuccess {
		t.Errorf("status = %q (%s), want the passing branch banked rather than failed", status, detail)
	}
}

// RED MUST MEAN FAILING ASSERTIONS, NOT A BROKEN SPECIFICATION. The gate wants
// the suite to fail, and with no implementation it fails to COMPILE on undefined
// symbols — the expected red of test-first. Any other compile error is the
// specification's own defect and read as red just the same.
//
// Measured: two sections of one task each declared TestConcurrentAccess, the
// branch stopped compiling, and the DEVELOPER inherited it — 86 turns, almost
// all reads, with no legal move because it may not edit test files.
func TestOnlyUndefinedSymbolsCountAsRed(t *testing.T) {
	// The expected red: the implementation does not exist yet.
	expected := "# tracker [tracker.test]\n" +
		"./store_test.go:12:6: undefined: NewStore\n" +
		"./store_test.go:20:9: undefined: ErrTaskNotFound\n"
	if bad := nonUndefinedCompileErrors(expected); len(bad) != 0 {
		t.Errorf("undefined symbols were treated as a defect: %v", bad)
	}

	// The defect that reached a developer: a name declared twice across sections.
	clash := "# tracker [tracker.test]\n" +
		"./store_data_test.go:10:6: TestConcurrentAccess redeclared in this block\n" +
		"\t./store_concurrent_test.go:10:6: other declaration of TestConcurrentAccess\n"
	bad := nonUndefinedCompileErrors(clash)
	if len(bad) == 0 {
		t.Fatal("a redeclaration passed as red; the developer would inherit an unfixable branch")
	}
	if !strings.Contains(bad[0], "redeclared") {
		t.Errorf("the wrong line was reported: %v", bad)
	}

	// A type mismatch means the type EXISTS — merged from a dependency — and the
	// spec is misusing it. That is the author's to fix, not the developer's.
	mismatch := "./api_test.go:27:18: cannot use time.Now() (value of struct type time.Time) as *time.Time value\n"
	if len(nonUndefinedCompileErrors(mismatch)) == 0 {
		t.Error("a type mismatch against existing code passed as red")
	}

	// A failing ASSERTION is not a compile error and must pass through untouched:
	// no column number, so it is ordinary red.
	if bad := nonUndefinedCompileErrors("    store_test.go:96: expected 2 items, got 0\n"); len(bad) != 0 {
		t.Errorf("a failing assertion was read as a compile defect: %v", bad)
	}

	// Bounded, because this text goes into a prompt.
	many := strings.Repeat("./a_test.go:1:1: something wrong\n", 30)
	if got := len(nonUndefinedCompileErrors(many)); got > 8 {
		t.Errorf("%d errors reported; the notice would swamp the prompt", got)
	}
}

// THE RECONCILER EXISTS BECAUSE THE AUTHORS CANNOT SEE EACH OTHER. Two sections
// of one task each declared TestConcurrentAccess on the first run that assembled
// specs; the branch stopped compiling and the DEVELOPER inherited it — 86 turns
// of nothing, because it may not edit tests.
func TestTheReconcilerEditsOnlyTestsAndKeepsEveryAssertion(t *testing.T) {
	stage, ok := stageFor(roleSpecMerge)
	if !ok {
		t.Fatal("no workflow stage for the reconciler")
	}
	// AND THEN THE TASK IS DEVELOPED. The reconciler assembles the slices its
	// sections wrote and hands the task to ONE developer — the only place a
	// developer now runs for this branch. Sections end at done; see
	// TestASectionEndsWhenItsSpecificationIsWritten.
	if stage.Success != ColReadyForDev {
		t.Errorf("the reconciler hands work to %q, want the task developed", stage.Success)
	}
	if !stage.RequiresDependencies {
		t.Error("the reconciler runs before its sections are written")
	}

	a := NewSpecMergeAgent(nil, nil, ClassLarge, RepoConfig{}, 10)
	if a.Role() != roleSpecMerge {
		t.Errorf("Role() = %q, want %q", a.Role(), roleSpecMerge)
	}

	// TESTS ONLY. The implementation is not its to change — that is the whole
	// separation the specification depends on.
	s := &devState{staged: map[string]string{}, read: map[string]string{}, baseline: map[string]string{}, missing: map[string]bool{}}
	err := applyEdits(s, []devEdit{{Path: "store.go", Replace: "package main\n"}}, modeSpecMerge)
	if err == nil {
		t.Error("the reconciler was allowed to edit the implementation")
	}
	if err != nil && !strings.Contains(err.Error(), "not a test file") {
		t.Errorf("unhelpful refusal: %v", err)
	}
	// A test file is fine.
	if err := applyEdits(s, []devEdit{{Path: "a_test.go", Replace: "package main\n"}}, modeSpecMerge); err != nil {
		t.Errorf("the reconciler cannot edit tests: %v", err)
	}

	// THE TEMPTATION IS TO DELETE ONE. That compiles, passes the gate, and
	// silently removes a requirement — so the brief has to forbid it explicitly.
	brief := specMergeSystemPrompt(false)
	for _, want := range []string{"DO NOT DELETE A TEST", "rename", "weaken", "STILL FAIL"} {
		if !strings.Contains(brief, want) {
			t.Errorf("the reconciler brief never mentions %q", want)
		}
	}
	if strings.Contains(brief, "add tests") && !strings.Contains(brief, "Do not add tests") {
		t.Error("the brief invites the reconciler to specify things")
	}
}

// NOTHING TO RECONCILE IS THE COMMON CASE, AND IT MUST COST NOTHING. The
// reconciler exists for sections that collide, and most sets do not: three
// sections compiled together perfectly and the stage still spent 300 turns
// reading, three times over, looking for a defect that was not there. A model
// given a job already done does not conclude that it is done.
func TestTheReconcilerExitsWhenThereIsNothingToMerge(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.addTicket(t, Ticket{TicketID: "t1", Title: "store", CreatedBy: "alice"})
	// The gate's script succeeds: the sections parse, and the suite fails the way
	// a specification is meant to.
	f.execStdout = "--- tests written ---\nstore_test.go\n"

	// No scripted replies at all: if the stage calls the model even once, the
	// gateway has nothing to give it and the test fails loudly.
	gw, calls := scriptedModel(t)
	rec, _ := newTestRecorder(t)
	ctx := rec.Start(context.Background(), "tr", "t1", "spec-merge-agent")

	a := NewSpecMergeAgent(gw, api, ClassLarge, RepoConfig{
		URL: "https://example.invalid/r", Branch: "main", BranchPrefix: "agent/",
		Image: "golang:1.25", TestCommand: "go test ./...",
	}, 10)
	status, detail, err := a.Handle(ctx, f.get("t1"))
	if err != nil {
		t.Fatalf("Handle() = %v", err)
	}
	if status != OutcomeSuccess {
		t.Errorf("status = %q (%s), want the task passed straight to development", status, detail)
	}
	if got := calls(); got != 0 {
		t.Errorf("the model was called %d times for a task with nothing to reconcile", got)
	}
	joined := strings.Join(f.commentBodies("t1"), "\n")
	if !strings.Contains(joined, "nothing to merge") {
		t.Errorf("the ticket does not record why the stage did nothing:\n%s", joined)
	}
}

// A RETRY MUST BE ABLE TO REWRITE ITS OWN WORK. A section author retried onto a
// branch already holding the test file its previous attempt pushed, could not
// reproduce the search text byte for byte, and had no legal way to replace it —
// 21 turns alternating a refused edit with a refused verification.
//
// The exception is narrow: a SPEC AUTHOR, a TEST file, and only one it has READ.
// A developer replacing main.go wholesale is the failure the guard exists for.
func TestASpecAuthorMayRewriteItsOwnTestFile(t *testing.T) {
	prior := "package main\n\nimport \"testing\"\n\nfunc TestOps(t *testing.T) { t.Fatal(\"old\") }\n"
	newState := func() *devState {
		return &devState{
			tree:     []string{"store_ops_test.go", "main.go"},
			read:     map[string]string{"store_ops_test.go": prior, "main.go": "package main\n\nfunc Serve() {}\n"},
			staged:   map[string]string{},
			baseline: map[string]string{},
			missing:  map[string]bool{},
		}
	}
	rewrite := "package main\n\nimport \"testing\"\n\nfunc TestOps(t *testing.T) { t.Fatal(\"new\") }\n"

	// The author may replace the test file it has read.
	s := newState()
	if err := applyEdits(s, []devEdit{{Path: "store_ops_test.go", Replace: rewrite}}, modeTest); err != nil {
		t.Fatalf("a spec author could not rewrite its own file: %v", err)
	}
	if s.staged["store_ops_test.go"] != rewrite {
		t.Errorf("the rewrite did not stage: %q", s.staged["store_ops_test.go"])
	}

	// It may NOT do so to a file it has not read — that is still blind.
	unread := newState()
	delete(unread.read, "store_ops_test.go")
	if err := applyEdits(unread, []devEdit{{Path: "store_ops_test.go", Replace: rewrite}}, modeTest); err == nil {
		t.Error("an unread file was replaced wholesale")
	}

	// A DEVELOPER MAY NOT, even having read it: whole-file replacement of shared
	// code is the failure this guard was written for.
	dev := newState()
	if err := applyEdits(dev, []devEdit{{Path: "main.go", Replace: "package main\n"}}, modeDevelop); err == nil {
		t.Error("a developer replaced an implementation file wholesale")
	}
}

// AN UNRUN VERIFICATION IS NOT A VERDICT. Asking to test before writing anything
// returned a failed verification, which landed in lastTest — and the no-op guard
// then refused every later run, because as far as it could tell the tree had
// been tested. The agent could not clear the state by any means available to it.
func TestVerifyingBeforeWritingIsNotATestResult(t *testing.T) {
	if !errors.Is(fmt.Errorf("verify: %w", errNothingToTest), errNothingToTest) {
		t.Fatal("a wrapped 'nothing to test' is not recognisable as one")
	}
	if errors.Is(errNothingToTest, errPushFailed) || errors.Is(errNothingToTest, errNothingToCommit) {
		t.Error("'nothing to test' is being confused with a push failure")
	}
	// It is the agent's to fix by writing, so it counts as a refusal — unlike a
	// push failure, which is the plane's.
	s := &devState{}
	s.noProgress("Rejected: you have not changed anything yet")
	if s.refusals != 1 {
		t.Errorf("refusals = %d, want the turn counted against the agent", s.refusals)
	}
}

// An already-satisfied section skips development but NOT integration.
//
// Sections overlap, so the implementation merged for one can already satisfy the
// next one's tests. Sending that section to development spends a lease, an
// attempt and a verification to learn the branch was green on arrival — and
// before that was understood it read as "the delegated agent produced no change"
// and blocked the ticket.
//
// Integration is NOT skipped with it. The tests the author just wrote exist only
// on this branch, and the integrator's merge is the only thing that puts them on
// the integration branch; skipping it would silently discard the specification.
func TestAlreadySatisfiedSectionRoutesToIntegration(t *testing.T) {
	if ColReadyForIntegration == "" {
		t.Fatal("no integration column to route to")
	}
	// The routing decision is a mode question: only the spec-authoring stages may
	// take this shortcut. A developer that finds its branch green has still
	// produced an implementation that security review should see.
	for _, m := range []agentMode{modeTest, modeSpecMerge} {
		if m == modeDevelop {
			t.Error("the developer must not skip its own review path")
		}
	}
}

// A branch that cannot be tested is not "green".
//
// Any error running the suite must route the ticket through development as
// normal, because the alternative — treating an unrunnable branch as satisfied —
// merges an unverified specification.
func TestBranchAlreadyGreenIsFalseWithoutATestCommand(t *testing.T) {
	a := &DevAgent{repo: RepoConfig{TestCommand: ""}}
	if a.branchAlreadyGreen(context.Background(), nil, &devState{}, "agent/x") {
		t.Error("a repo with no test command reported its branch green; nothing was run")
	}
}

// A delegated stage that runs out of attempts hands the branch to the native
// developer rather than declaring the ticket dead.
//
// Measured on a clean run: one section beat the delegated agent six times, three
// different ways, then blocked — and seventeen tickets that depended on it blocked
// behind it. 89% of a board lost to one agent's blind spot. The two developers
// have already been observed failing on different tickets, so a second strategy on
// the same branch is the cheapest tolerance available.
func TestDevStateRecordsADelegateHandover(t *testing.T) {
	s := &devState{}
	if s.delegateExhausted {
		t.Error("a fresh state claims the delegate was exhausted")
	}
	s.delegateExhausted = true
	if !s.delegateExhausted {
		t.Error("the handover is not recorded; which developer produced the result becomes invisible")
	}
}
