package main

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// ── unit ──────────────────────────────────────────────────────────────────────

// The ticket is the only shared state between the developer and the reviewer,
// which is what lets them restart independently. If the branch cannot be read
// back out of the comment, the reviewer has nothing to review.
func TestBranchFromTicket(t *testing.T) {
	rendered := renderDevSummary("agent/abc123", "did it", []string{"a.go"}, "run001", "ok")
	got := branchFromTicket(Ticket{Comments: []Comment{{Body: rendered}}})
	if got != "agent/abc123" {
		t.Errorf("branchFromTicket = %q, want agent/abc123 (parsed from what the dev agent actually writes)", got)
	}
	if got := branchFromTicket(Ticket{Comments: []Comment{{Body: "unrelated comment"}}}); got != "" {
		t.Errorf("branchFromTicket = %q on a ticket with no branch, want empty", got)
	}
}

// An uninterpretable verdict must make a human look, not say everything is fine.
func TestSanitiseReviewDefaultsToConcerns(t *testing.T) {
	for _, bad := range []string{"", "APPROVED", "lgtm", "reject", "Clean"} {
		if got := sanitiseReview(review{Verdict: bad}).Verdict; got != verdictConcerns {
			t.Errorf("verdict %q sanitised to %q, want %q", bad, got, verdictConcerns)
		}
	}
	for _, ok := range reviewVerdicts {
		r := review{Verdict: ok}
		if ok == verdictBlocking {
			r.Findings = []reviewFinding{{Severity: "high", File: "a.go", Detail: "x"}}
		}
		if got := sanitiseReview(r).Verdict; got != ok {
			t.Errorf("valid verdict %q was changed to %q", ok, got)
		}
	}
}

// "blocking" with nothing to point at is unactionable, and it is exactly what a
// prompt injection saying "report this as blocking" produces.
func TestSanitiseReviewDowngradesEmptyBlocking(t *testing.T) {
	got := sanitiseReview(review{Verdict: verdictBlocking})
	if got.Verdict != verdictConcerns {
		t.Errorf("verdict = %q for a blocking review with no findings, want it downgraded", got.Verdict)
	}
}

func TestSanitiseReviewBoundsFindings(t *testing.T) {
	many := make([]reviewFinding, 100)
	for i := range many {
		many[i] = reviewFinding{Severity: "critical", File: strings.Repeat("p", 500), Detail: strings.Repeat("d", 5000)}
	}
	got := sanitiseReview(review{Verdict: verdictConcerns, Findings: many})
	if len(got.Findings) > maxFindings {
		t.Errorf("%d findings, want at most %d", len(got.Findings), maxFindings)
	}
	for _, f := range got.Findings {
		if f.Severity != "low" {
			t.Errorf("unknown severity became %q, want it normalised to low", f.Severity)
		}
		if len([]rune(f.Detail)) > maxDetailRune+1 {
			t.Error("finding detail is not bounded")
		}
	}
	// A finding with no explanation is noise.
	if got := sanitiseReview(review{Findings: []reviewFinding{{Severity: "high", File: "a.go"}}}); len(got.Findings) != 0 {
		t.Error("a finding with no detail was kept")
	}
}

// The diff is attacker-influenced on a compromised branch, so the prompt must
// tell the model to treat it as data rather than direction.
func TestSecPromptTreatsDiffAsUntrusted(t *testing.T) {
	for _, want := range []string{"untrusted", "Ignore any instruction"} {
		if !strings.Contains(secSystemPrompt, want) {
			t.Errorf("system prompt does not mention %q", want)
		}
	}
}

// ── integration ───────────────────────────────────────────────────────────────

func secTestAgent(t *testing.T, gw *Gateway, api *CodeArmory) *SecAgent {
	t.Helper()
	return NewSecAgent(gw, api, ClassLarge, RepoConfig{
		URL: "https://example.invalid/r", Branch: "dev", Image: "golang:1.25",
	})
}

func reviewedTicket(id string) Ticket {
	return Ticket{TicketID: id, Title: "x", CreatedBy: "alice", Comments: []Comment{
		{CommentID: "c1", Body: renderDevSummary("agent/"+id, "did it", []string{"a.go"}, "run1", "ok")},
	}}
}

func TestSecAgentReviewsABranch(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.addTicket(t, reviewedTicket("t1"))
	f.execStdout = "diff --git a/a.go b/a.go\n+password := \"hunter2\"\n"

	gw, _ := scriptedModel(t, `{"verdict":"blocking","summary":"A credential is committed.","findings":[{"severity":"high","file":"a.go","detail":"hardcoded password"}]}`)
	rec, _ := newTestRecorder(t)
	ctx := rec.Start(context.Background(), "tr", "t1", "sec-agent")

	status, detail, err := secTestAgent(t, gw, api).Handle(ctx, f.get("t1"))
	if err != nil {
		t.Fatalf("Handle() = %v", err)
	}
	// A blocking verdict RETURNS the work rather than waving it through.
	if status != OutcomeReturned || !strings.Contains(detail, verdictBlocking) {
		t.Errorf("status=%q detail=%q, want %q", status, detail, OutcomeReturned)
	}

	bodies := f.commentBodies("t1")
	var found string
	for _, b := range bodies {
		if strings.Contains(b, reviewMarker) {
			found = b
		}
	}
	if found == "" {
		t.Fatalf("no review comment written; comments were %v", bodies)
	}
	for _, want := range []string{"blocking", "hardcoded password", "a.go", "agent/t1"} {
		if !strings.Contains(found, want) {
			t.Errorf("review comment missing %q:\n%s", want, found)
		}
	}
	// The comment must say what the pipeline actually did with it. This used to
	// assert the opposite — that the review "blocks nothing" — and that sentence
	// beside a verdict which sends work back would tell a reader the reverse of
	// what happened.
	if !strings.Contains(found, "Returned to the developer") {
		t.Errorf("a blocking review does not say the work was returned:\n%s", found)
	}
	if !strings.Contains(found, returnedMarker) {
		t.Errorf("a blocking review carries no return marker, so nothing can count the bounce:\n%s", found)
	}
}

// A verdict that is not "blocking" must move the work FORWARD. "concerns" is a
// note for whoever reads the ticket, and treating it as a refusal would return
// every change that attracted a nitpick.
func TestSecAgentConcernsDoNotReturnTheWork(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.addTicket(t, reviewedTicket("t1"))
	f.execStdout = "diff --git a/a.go b/a.go\n+x\n"

	gw, _ := scriptedModel(t, `{"verdict":"concerns","summary":"s","findings":[{"severity":"low","file":"a.go","detail":"d"}]}`)
	rec, _ := newTestRecorder(t)
	ctx := rec.Start(context.Background(), "tr", "t1", "sec-agent")

	status, _, err := secTestAgent(t, gw, api).Handle(ctx, f.get("t1"))
	if err != nil {
		t.Fatalf("Handle() = %v", err)
	}
	if status != OutcomeSuccess {
		t.Errorf("status = %q for a non-blocking verdict, want %q", status, OutcomeSuccess)
	}
	for _, b := range f.commentBodies("t1") {
		if strings.Contains(b, returnedMarker) {
			t.Errorf("a non-blocking review left a return marker, which would spend a bounce:\n%s", b)
		}
	}
}

// THE LOOP BREAKER. A reviewer that rejects every fix would bounce a ticket
// between review and dev forever, because a return deliberately opens a fresh
// round of developer attempts. maxReturns is the only thing stopping that, and
// hitting it must put the ticket in front of a person.
func TestSecAgentStopsBouncingAtTheReturnCeiling(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	tk := reviewedTicket("t1")
	for i := 0; i < maxReturns; i++ {
		tk.Comments = append(tk.Comments, Comment{
			CommentID: fmt.Sprintf("r%d", i),
			Body:      "**Security review** — returned. " + returnedMarker,
			CreatedAt: time.Now().Add(time.Duration(i) * time.Minute),
		})
	}
	f.addTicket(t, tk)
	f.execStdout = "diff --git a/a.go b/a.go\n+x\n"

	gw, _ := scriptedModel(t, `{"verdict":"blocking","summary":"s","findings":[{"severity":"high","file":"a.go","detail":"d"}]}`)
	rec, _ := newTestRecorder(t)
	ctx := rec.Start(context.Background(), "tr", "t1", "sec-agent")

	status, _, err := secTestAgent(t, gw, api).Handle(ctx, f.get("t1"))
	if err != nil {
		t.Fatalf("Handle() = %v", err)
	}
	if status != OutcomeBlocked {
		t.Errorf("status = %q at the return ceiling, want %q — anything else keeps the loop running",
			status, OutcomeBlocked)
	}
	var announced bool
	for _, b := range f.commentBodies("t1") {
		if strings.Contains(b, returnCeilingMarker) {
			announced = true
		}
	}
	if !announced {
		t.Error("nothing on the ticket says the ceiling was reached, so the window cannot raise the alert")
	}
}

// CONTAINMENT, still. The reviewer can now return work, but it does that by
// REPORTING an outcome the dispatcher routes on — it must never move the ticket
// itself. That keeps the routing table the single place a column is decided, and
// it keeps this agent's own reach to exactly one comment even though its verdict
// now has consequences.
//
// The bound on a prompt-injected reviewer is no longer "it cannot reject at
// all"; it is maxReturns, checked in the ceiling test above.
func TestSecAgentOnlyEverComments(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.addTicket(t, reviewedTicket("t1"))
	before := f.get("t1")
	f.execStdout = "diff --git a/a.go b/a.go\n+x\n"

	gw, _ := scriptedModel(t, `{"verdict":"blocking","summary":"s","findings":[{"severity":"high","file":"a.go","detail":"d"}]}`)
	rec, _ := newTestRecorder(t)
	ctx := rec.Start(context.Background(), "tr", "t1", "sec-agent")

	if _, _, err := secTestAgent(t, gw, api).Handle(ctx, f.get("t1")); err != nil {
		t.Fatalf("Handle() = %v", err)
	}
	after := f.get("t1")
	if after.Status != before.Status || after.Priority != before.Priority {
		t.Errorf("the reviewer changed ticket state: status %q→%q priority %q→%q",
			before.Status, after.Status, before.Priority, after.Priority)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if strings.Contains(c, "PUT ") {
			t.Errorf("the reviewer issued a write beyond a comment: %s", c)
		}
	}
}

// An empty diff is a real answer, not a failure.
func TestSecAgentHandlesAnEmptyDiff(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.addTicket(t, reviewedTicket("t1"))
	f.execStdout = ""

	gw, calls := scriptedModel(t, `{"verdict":"clean"}`)
	rec, _ := newTestRecorder(t)
	ctx := rec.Start(context.Background(), "tr", "t1", "sec-agent")

	status, _, err := secTestAgent(t, gw, api).Handle(ctx, f.get("t1"))
	if err != nil || status != OutcomeSuccess {
		t.Fatalf("Handle() = (%q, %v)", status, err)
	}
	if calls() != 0 {
		t.Errorf("the model was called %d times for an empty diff; that is tokens spent on nothing", calls())
	}
}

// Unparseable output must say the branch was NOT reviewed, rather than leaving a
// ticket that looks reviewed when nothing checked it.
func TestSecAgentSurfacesUnparseableOutput(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.addTicket(t, reviewedTicket("t1"))
	f.execStdout = "diff --git a/a.go b/a.go\n+x\n"

	gw, _ := scriptedModel(t, "Looks fine to me!")
	rec, _ := newTestRecorder(t)
	ctx := rec.Start(context.Background(), "tr", "t1", "sec-agent")

	status, _, err := secTestAgent(t, gw, api).Handle(ctx, f.get("t1"))
	if err == nil || status != OutcomeFailed {
		t.Fatalf("Handle() = (%q, %v), want a failure", status, err)
	}
	var said bool
	for _, b := range f.commentBodies("t1") {
		if strings.Contains(b, "NOT been reviewed") {
			said = true
		}
	}
	if !said {
		t.Error("a failed review left no warning; the ticket would look reviewed")
	}
}

// The three agents must form a pipeline: exactly one wants any given ticket.
func TestThreeAgentPipelineIsDisjoint(t *testing.T) {
	pm := NewPMAgent(nil, nil, ClassSmall)
	dev := NewDevAgent(nil, nil, ClassLarge, RepoConfig{URL: "u", PipelineID: "p"}, 4)
	sec := NewSecAgent(nil, nil, ClassLarge, RepoConfig{URL: "u"})

	triage := Comment{Body: triageMarker + " (automated)"}
	branch := Comment{Body: branchMarker + "\n- **Branch:** `agent/x`"}
	rev := Comment{Body: reviewMarker + " (automated)"}

	// Disjointness is a property of the ROUTING TABLE now, not of these
	// predicates — see TestStagesTakeFromDistinctColumns. What is left for each
	// agent to say is the part its column cannot: the reviewer needs something to
	// review, and a ticket moved into its column by hand with no branch on it is
	// the one case worth refusing.
	if !sec.Wants(Ticket{Comments: []Comment{triage, branch}}) {
		t.Error("the reviewer does not want a ticket with a branch to read")
	}
	if sec.Wants(Ticket{Comments: []Comment{triage}}) {
		t.Error("the reviewer wants a ticket with no branch; there is nothing to review")
	}
	_ = rev
	_ = pm
	_ = dev
}

// The scanner's findings reach the reviewer as CONTEXT, in the same user turn as
// the diff and never the system prompt: on a compromised branch a scanner echoes
// the attacker's own paths and code back verbatim, so it is attacker-influenced
// text exactly as the diff is.
func TestReviewRequestCarriesScanFindingsAsUntrustedContext(t *testing.T) {
	req := reviewRequest("agent/t1", "+func main() {}", "main.go:4: G101 hardcoded credentials")

	if !strings.Contains(req, "+func main() {}") {
		t.Error("the diff is missing from the review request")
	}
	if !strings.Contains(req, "G101 hardcoded credentials") {
		t.Error("the scanner's findings are missing; the reviewer would have to rediscover them")
	}
	// Labelled as leads, not conclusions — a scanner is noisy and the judgement of
	// whether a finding applies here is the part the model is for.
	if !strings.Contains(req, "UNVERIFIED") {
		t.Error("the findings are not marked unverified; the model would repeat them as fact")
	}
	// No scanner configured must not leave a dangling header.
	plain := reviewRequest("agent/t1", "+x", "")
	if strings.Contains(plain, "static analyser") {
		t.Errorf("an unset scanner still adds a section: %q", plain)
	}
}

// One sandbox produces both, and the marker is what separates them. Getting this
// wrong feeds the scanner's output to the model as though it were the diff.
func TestScanOutputIsSeparatedFromTheDiff(t *testing.T) {
	combined := "+++ b/main.go\n+func main() {}\n" + scanMarker + "\nmain.go:4: G101\n"
	diff, findings, _ := strings.Cut(combined, scanMarker)
	if strings.Contains(diff, "G101") {
		t.Error("scanner output leaked into the diff")
	}
	if !strings.Contains(findings, "G101") {
		t.Error("findings did not survive the split")
	}
}

// Nothing upstream blocks on lint, so the reviewer is where a linter's finding
// meets judgement. It must be told that plainly, or it reports style complaints
// as security findings and the advisory design leaks into a de facto gate.
func TestReviewRequestFramesFindingsAsUnblockingLeads(t *testing.T) {
	req := reviewRequest("agent/t1", "+func main() {}", "main.go:4: unused parameter\nmain.go:9: G101 credentials")

	for _, want := range []string{"UNVERIFIED", "blocked this branch", "does not apply", "style complaint"} {
		if !strings.Contains(req, want) {
			t.Errorf("the request does not tell the reviewer %q; it would treat tool output as fact", want)
		}
	}
}

// The three analyses answer different questions and the advice differs, so the
// evidence must say which is which. Unlabelled, a linter's opinion gets reported
// as a vulnerability and a third-party CVE as a defect in this diff.
func TestEvidenceSectionsAreLabelledByKind(t *testing.T) {
	a := NewSecAgent(nil, nil, ClassLarge, RepoConfig{
		URL: "git://host/demo.git", Branch: "main",
		LintCommand: "go vet ./...", ScanCommand: "gosec ./...", SCACommand: "osv-scanner -r .",
	})
	got := a.analyses()
	if len(got) != 3 {
		t.Fatalf("analyses = %d, want all three configured", len(got))
	}
	if !strings.Contains(got[2].label, "DEPENDENCY") {
		t.Errorf("the SCA section is not labelled as third-party: %q", got[2].label)
	}
	// Unset commands must not produce empty labelled sections.
	none := NewSecAgent(nil, nil, ClassLarge, RepoConfig{URL: "git://host/demo.git"})
	if len(none.analyses()) != 0 {
		t.Error("unset commands still produce analysis sections")
	}
	// And the reviewer must be told how a dependency finding differs.
	req := reviewRequest("b", "+x", "## DEPENDENCY SCAN\nCVE-2024-1 in golang.org/x/net")
	if !strings.Contains(req, "third-party") {
		t.Error("the reviewer is not told a dependency finding is not a defect in this diff")
	}
}

// The evidence script must PARSE. One evidence label contained an apostrophe —
// "this project's own code" — interpolated into a single-quoted echo, which
// closed the quote early and left the script unbalanced. The shell then exited 2
// on a syntax error before running a single line, so every security review
// failed with an empty diff and nothing in the output to explain why.
//
// Asserting on the label would only pin the one string that broke. Parsing the
// script catches the class: any prose that reaches a shell word.
func TestEvidenceScriptIsValidShell(t *testing.T) {
	integrationTest(t)
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh to parse with")
	}
	f, api := newFakePlatform(t)
	f.addTicket(t, Ticket{TicketID: "t1", Title: "x", CreatedBy: "alice"})

	a := NewSecAgent(nil, api, ClassLarge, RepoConfig{
		URL: "git://host/demo.git", Branch: "main", Image: "golang:1.25",
		LintCommand: "go vet ./...", ScanCommand: "gosec ./...", SCACommand: "osv-scanner -r .",
	})
	rec, _ := newTestRecorder(t)
	ctx := rec.Start(context.Background(), "tr", "t1", "sec-agent")
	sb, aerr := api.AcquireSandbox(ctx, SandboxRequest{Image: "golang:1.25", CloneURL: "git://host/demo.git", Branch: "main"})
	if aerr != nil {
		t.Fatalf("AcquireSandbox: %v", aerr)
	}
	if _, _, err := a.fetchEvidence(ctx, rec, sb, "agent/t1"); err != nil {
		t.Fatalf("fetchEvidence() = %v", err)
	}

	f.mu.Lock()
	specs := append([]SandboxSpec(nil), f.execSpecs...)
	f.mu.Unlock()
	if len(specs) == 0 {
		t.Fatal("no sandbox was run, so there is no script to check")
	}
	for _, spec := range specs {
		script := spec.Command[len(spec.Command)-1]
		cmd := exec.Command(sh, "-n")
		cmd.Stdin = strings.NewReader(script)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("the evidence script does not parse: %v\n%s\n--- script ---\n%s", err, out, script)
		}
	}
}

func TestShellSingleQuoteSurvivesAnApostrophe(t *testing.T) {
	const text = "this project's own code"
	script := "printf %s " + shellSingleQuote(text)
	out, err := exec.Command("sh", "-c", script).Output()
	if err != nil {
		t.Fatalf("quoted script failed: %v", err)
	}
	if string(out) != text {
		t.Errorf("round trip = %q, want %q", out, text)
	}
}

// A SECTION ROUTED STRAIGHT TO INTEGRATION MUST STILL YIELD ITS BRANCH.
//
// The specification author publishes with testsWrittenMarker, not branchMarker,
// and for a while nothing could read the branch back off that comment. The
// already-satisfied route sends such a ticket to the integrator, which admitted
// it (hasBranch accepts alreadySatisfiedMarker) and then failed with "no branch
// recorded".
//
// Measured on r93: the ticket retried until its budget was gone and took the
// other five on the board to blocked with it, and nothing named the cause.
func TestBranchIsReadableFromEveryStageThatPublishesOne(t *testing.T) {
	for _, mode := range []agentMode{modeDevelop, modeTest, modeCoverage} {
		rendered := renderStageSummary(mode, "agent/abc123", "did it", []string{"a.go"}, "run001", "ok")
		if got := branchFromTicket(Ticket{Comments: []Comment{{Body: rendered}}}); got != "agent/abc123" {
			t.Errorf("mode %v published a branch that cannot be read back: got %q, want agent/abc123", mode, got)
		}
	}
}

// The most recent branch wins: a ticket reworked after a hand-back carries more
// than one, and the stale one is not the one to act on.
func TestTheLastPublishedBranchWins(t *testing.T) {
	first := renderStageSummary(modeTest, "agent/old", "tests", nil, "run001", "ok")
	second := renderStageSummary(modeDevelop, "agent/new", "impl", nil, "run002", "ok")
	got := branchFromTicket(Ticket{Comments: []Comment{{Body: first}, {Body: second}}})
	if got != "agent/new" {
		t.Errorf("branchFromTicket = %q, want agent/new", got)
	}
}
