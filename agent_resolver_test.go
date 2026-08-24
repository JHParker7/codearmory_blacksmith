package main

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"
)

// The resolver's licence is "settle this disagreement", not "change the branch
// everything merges into". A resolution touching a file git did not report as
// conflicted is an unreviewed change reaching dev, so it is refused outright.
func TestResolutionMustStayInsideTheConflict(t *testing.T) {
	conflicted := map[string]string{"main.go": "<<<<<<< HEAD\na\n=======\nb\n>>>>>>> x"}

	cases := map[string]struct {
		reply   string
		wantErr string
	}{
		"touches another file": {
			`{"files":[{"path":"main.go","content":"a\nb\n"},{"path":"secrets.go","content":"x"}]}`,
			"did not report as conflicted",
		},
		"leaves markers in": {
			`{"files":[{"path":"main.go","content":"<<<<<<< HEAD\na\n=======\nb\n>>>>>>> x"}]}`,
			"still contains conflict markers",
		},
		"skips a conflicted file": {
			`{"files":[{"path":"other.go","content":"x"}]}`,
			"did not report as conflicted",
		},
		"returns nothing": {`{"files":[]}`, "no files"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := validateResolution(c.reply, conflicted)
			if err == nil {
				t.Fatalf("accepted a bad resolution (%s)", name)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("error = %v, want it to mention %q", err, c.wantErr)
			}
		})
	}

	// The good case: every conflicted file, resolved, and nothing else.
	got, err := validateResolution(`{"files":[{"path":"main.go","content":"a\nb"}]}`, conflicted)
	if err != nil {
		t.Fatalf("rejected a valid resolution: %v", err)
	}
	if got["main.go"] != "a\nb\n" {
		t.Errorf("content = %q, want it terminated with a newline", got["main.go"])
	}
}

// Nothing reaches the integration branch until the gates pass on the RESOLVED
// tree. A resolution that compiles is not a resolution that is right, and this is
// the only check between a plausible merge and a broken dev branch.
func TestResolverVerifiesBeforePushing(t *testing.T) {
	a := NewResolverAgent(nil, nil, ClassLarge, RepoConfig{
		URL: "git://host/demo.git", Branch: "main", TestCommand: "go test ./...",
	}, "dev")
	s := a.applyScript("agent/t1", map[string]string{"main.go": "package main\n"})

	write := strings.Index(s, "base64 -d")
	test := strings.Index(s, "go test ./...")
	push := strings.Index(s, "git push origin dev")
	if write < 0 || test < 0 || push < 0 {
		t.Fatalf("script is missing a step:\n%s", s)
	}
	if !(write < test && test < push) {
		t.Error("the gates do not run between writing the resolution and pushing it")
	}
	// Model-authored content must never be read by the shell.
	if strings.Contains(s, "package main\n\n") {
		t.Error("the resolution is inlined as shell text rather than base64")
	}
	// A partial resolution must not be committed with markers still in the tree.
	if !strings.Contains(s, "diff-filter=U") {
		t.Error("nothing checks that the tree is actually unconflicted before committing")
	}
	if strings.Contains(s, "--force") {
		t.Error("the resolver force-pushes")
	}
}

// One attempt. A model that got a conflict wrong has no more information the
// second time, and looping spends sandboxes to arrive somewhere worse.
func TestResolverTriesOnce(t *testing.T) {
	a := NewResolverAgent(nil, nil, ClassLarge, RepoConfig{URL: "git://host/demo.git"}, "dev")
	base := []Comment{
		{Body: branchMarker + "\n\n- **Branch:** `agent/t1`\n"},
		{Body: reviewMarker + " (automated)\n\nclean"},
		{Body: conflictMarker},
	}
	if !a.Wants(Ticket{Comments: base}) {
		t.Fatal("does not take a conflicted ticket")
	}
	for _, after := range []string{resolvedMarker, resolutionFailedMarker} {
		tk := Ticket{Comments: append(append([]Comment{}, base...), Comment{Body: after})}
		if a.Wants(tk) {
			t.Errorf("retries after %q", after)
		}
	}
	// An integration failure is not its problem — a tree that merges cleanly and
	// then fails is wrong in combination, which no amount of reconciling text
	// fixes. The COLUMN says so now: that outcome is reported as OutcomeBlocked
	// and lands where a person looks, not in the resolver's queue.
	if stages[roleResolve].Ready != ColConflicted {
		t.Errorf("the resolver reads %q, want only the conflicted column", stages[roleResolve].Ready)
	}
	if stages[roleIntegrate].Exhausted == stages[roleResolve].Ready {
		t.Error("a failed integration lands in the resolver's queue; it would try to reconcile a tree that merged fine")
	}
}

// A failing resolver must still record the attempt, and the reason is not
// politeness. Wants() is `hasConflict && !hasResolutionAttempt`, and the attempt
// is recorded by the COMMENT — so a failure that posts nothing leaves the ticket
// eligible, and the stage is picked up again on the next poll, fails the same
// way, and spends a sandbox each time. Observed as a resolver that claimed a
// ticket and left no trace.
func TestResolverFailureIsVisibleAndNotRetriedForever(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.addTicket(t, Ticket{TicketID: "t1", Title: "x", CreatedBy: "alice"})

	// No branch on the ticket: the earliest failure, and the one that used to
	// return silently before any sandbox ran.
	a := NewResolverAgent(nil, api, ClassLarge, RepoConfig{URL: "git://host/demo.git"}, "dev")
	rec, _ := newTestRecorder(t)
	ctx := rec.Start(context.Background(), "tr", "t1", "resolver")

	status, _, err := a.Handle(ctx, Ticket{TicketID: "t1", Comments: []Comment{{Body: conflictMarker}}})
	if status != OutcomeFailed || err == nil {
		t.Fatalf("status=%q err=%v, want a reported failure", status, err)
	}

	bodies := f.commentBodies("t1")
	if len(bodies) != 1 {
		t.Fatalf("comments = %v, want exactly one explaining the failure", bodies)
	}
	if !strings.Contains(bodies[0], "branch") {
		t.Errorf("the comment does not say what was wrong: %q", bodies[0])
	}

	// And the marker must make the stage ineligible, or it runs again forever.
	after := Ticket{Comments: []Comment{{Body: conflictMarker}, {Body: bodies[0]}}}
	if a.Wants(after) {
		t.Error("still eligible after a failure; this is the retry loop that burned a sandbox per poll")
	}
}

// The preamble sets -e, so every command in these scripts that is EXPECTED to
// fail must say so. A bare `git merge` aborts the script at the conflict it was
// sent to read: no output, and an unresolved conflict reported as none — which is
// how a stuck ticket ends up looking finished.
func TestScriptsGuardCommandsThatAreMeantToFail(t *testing.T) {
	if !strings.Contains(sandboxPreamble, "set -e") {
		t.Skip("the preamble no longer aborts on error; this guard is moot")
	}
	a := NewResolverAgent(nil, nil, ClassLarge, RepoConfig{
		URL: "git://host/demo.git", Branch: "main", TestCommand: "go test ./...",
	}, "dev")

	for name, script := range map[string]string{
		"readConflict": a.conflictScript("agent/t1"),
		"applyScript":  a.applyScript("agent/t1", map[string]string{"main.go": "x"}),
	} {
		i := strings.Index(script, "git merge")
		if i < 0 {
			t.Fatalf("%s: no merge in the script", name)
		}
		line := script[i:]
		if end := strings.IndexByte(line, '\n'); end >= 0 {
			line = line[:end]
		}
		if !strings.Contains(line, "||") {
			t.Errorf("%s: the merge is unguarded under `set -e`, so a conflict ends the script:\n  %s", name, line)
		}
	}
}

// THE REGRESSION. Every failure path here used to report OutcomeSuccess, and the
// resolver's Success column is ColDone — so a conflict no agent could settle was
// filed as finished work. A whole run reported six tickets done against an
// integration branch that had not moved, and nothing on the board disagreed.
//
// The failure has to land in front of a person. "Do not retry" is OutcomeBlocked,
// not OutcomeSuccess; the two differ by exactly this bug.
func TestResolverFailureIsNotReportedAsDone(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.addTicket(t, Ticket{TicketID: "t1", Title: "x", CreatedBy: "alice"})
	// A real conflict comes back from the sandbox...
	f.execStdout = "===FILE main.go\n" +
		base64.StdEncoding.EncodeToString([]byte("<<<<<<< HEAD\na\n=======\nb\n>>>>>>> x\n")) + "\n"
	// ...and the model fails to produce anything usable for it.
	gw, _ := scriptedModel(t, "not json at all")

	a := NewResolverAgent(gw, api, ClassLarge, RepoConfig{URL: "git://host/demo.git", Branch: "main", Image: "golang:1.25"}, "dev")
	rec, _ := newTestRecorder(t)
	ctx := rec.Start(context.Background(), "tr", "t1", "resolver")

	tk := Ticket{TicketID: "t1", Comments: []Comment{
		{Body: branchMarker + "\n\n- **Branch:** `agent/t1`\n"},
		{Body: conflictMarker},
	}}
	status, _, err := a.Handle(ctx, tk)
	if err != nil {
		t.Fatalf("Handle() = %v", err)
	}
	if status == OutcomeSuccess {
		t.Fatal("an unresolvable conflict reported success; the resolver's success column is done, so the ticket would be filed as finished without ever merging")
	}
	if status != OutcomeBlocked {
		t.Errorf("status = %q, want %q", status, OutcomeBlocked)
	}

	// And routing must actually put that in front of a person.
	d := NewDispatcher(nil, nil, &stubHandler{role: roleResolve}, DispatcherOpts{Host: "h"})
	if got := d.destination(tk, status); got == ColDone {
		t.Error("a failed resolution still routes to done")
	} else if got != ColBlocked {
		t.Errorf("a failed resolution routes to %q, want %q", got, ColBlocked)
	}
}

// A conflict that has already gone away is not a failed attempt. Recording it as
// one set hasResolutionAttempt, and Wants() is
// `hasConflict && !hasResolutionAttempt` — so the ticket was barred from this
// stage permanently, and a branch that conflicted again would never be looked at
// a second time.
func TestVanishedConflictIsReturnedNotRecordedAsAFailure(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.addTicket(t, Ticket{TicketID: "t1", Title: "x", CreatedBy: "alice"})
	f.execStdout = "" // the merge is clean now: no ===FILE stanzas

	a := NewResolverAgent(nil, api, ClassLarge, RepoConfig{URL: "git://host/demo.git", Branch: "main", Image: "golang:1.25"}, "dev")
	rec, _ := newTestRecorder(t)
	ctx := rec.Start(context.Background(), "tr", "t1", "resolver")

	status, _, err := a.Handle(ctx, Ticket{TicketID: "t1", Comments: []Comment{
		{Body: branchMarker + "\n\n- **Branch:** `agent/t1`\n"},
		{Body: conflictMarker},
	}})
	if err != nil {
		t.Fatalf("Handle() = %v", err)
	}
	if status != OutcomeReturned {
		t.Errorf("status = %q, want %q — the integrator should merge it normally", status, OutcomeReturned)
	}

	bodies := f.commentBodies("t1")
	if len(bodies) != 1 {
		t.Fatalf("comments = %v, want one", bodies)
	}
	if strings.Contains(bodies[0], resolutionFailedMarker) {
		t.Error("a vanished conflict was recorded as a failed resolution; the resolver is now barred from this ticket for good")
	}
	if !strings.Contains(bodies[0], noConflictMarker) {
		t.Errorf("nothing says why the ticket moved: %q", bodies[0])
	}

	// The stage must still be willing to take it if it conflicts again.
	again := Ticket{Comments: []Comment{{Body: conflictMarker}, {Body: bodies[0]}}}
	if !a.Wants(again) {
		t.Error("the resolver refuses a ticket whose conflict merely went away once; a later conflict would never be resolved")
	}

	// And it goes back to the integrator, not onward to done.
	d := NewDispatcher(nil, nil, &stubHandler{role: roleResolve}, DispatcherOpts{Host: "h"})
	if got := d.destination(Ticket{}, status); got != ColReadyForIntegration {
		t.Errorf("a vanished conflict routes to %q, want %q", got, ColReadyForIntegration)
	}
}

// THE REPLY CARRIES WHOLE SOURCE FILES, which are made of exactly the characters
// JSON cares about: quotes, backslashes, newlines, backticks. Asking a model to
// escape all of them inside a string it is also authoring produced the failure
// that blocked a finished ticket — "invalid character '\"' after object
// key:value pair" — after its tests AND its security review had passed.
//
// A tool call moves the encoding to the server: the model fills typed fields.
func TestResolverAsksForATypedToolCall(t *testing.T) {
	tools := resolverTools()
	if len(tools) != 1 {
		t.Fatalf("got %d tools, want exactly one", len(tools))
	}
	if tools[0].Name != "resolve_conflicts" {
		t.Errorf("tool name = %q", tools[0].Name)
	}
	// The tool and the structured-output fallback must not describe different
	// shapes, or the two paths accept different replies.
	props, _ := tools[0].Parameters["properties"].(map[string]any)
	if _, ok := props["files"]; !ok {
		t.Fatalf("the tool does not take files: %+v", tools[0].Parameters)
	}
	sp, _ := resolutionSchema()["properties"].(map[string]any)
	if _, ok := sp["files"]; !ok {
		t.Error("the schema and the tool disagree about the shape")
	}
	// The prompt must no longer ask for a hand-written envelope.
	if strings.Contains(resolverSystemPrompt, `{"files":[{"path"`) {
		t.Error("the prompt still asks the model to author JSON itself")
	}
	if !strings.Contains(resolverSystemPrompt, "resolve_conflicts") {
		t.Error("the prompt does not name the tool it must call")
	}
}

// Whichever path the reply arrives on, the SAME validation applies — a
// resolution may only rewrite files git reported as conflicted.
func TestToolArgumentsGoThroughTheSameValidation(t *testing.T) {
	conflicted := map[string]string{"main.go": "<<<<<<< ours\na\n=======\nb\n>>>>>>> theirs\n"}

	args := `{"files":[{"path":"main.go","content":"package main\n\nfunc main() {}\n"}]}`
	got, err := validateResolution(args, conflicted)
	if err != nil {
		t.Fatalf("a valid tool-call resolution was rejected: %v", err)
	}
	if !strings.Contains(got["main.go"], "func main()") {
		t.Errorf("resolution lost its content: %q", got["main.go"])
	}

	// And the boundary still holds on this path.
	if _, err := validateResolution(`{"files":[{"path":"other.go","content":"x"}]}`, conflicted); err == nil {
		t.Error("a resolution touching an unconflicted file was accepted")
	}
	if _, err := validateResolution(`{"files":[{"path":"main.go","content":"<<<<<<< ours\nx\n"}]}`, conflicted); err == nil {
		t.Error("a resolution still carrying conflict markers was accepted")
	}
}
