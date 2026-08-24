package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// A SCOPE IS THE SUBJECT, NOT THE FILE. store.go and store_test.go are the same
// thing seen from two sides, so they carry the same scope and a reader can see
// the pair.
func TestCommitScopeIsTheSubjectNotTheFile(t *testing.T) {
	for path, want := range map[string]string{
		"store.go":                    "store",
		"store_test.go":               "store",
		"internal/logfmt/describe.go": "describe",
		"Handlers.go":                 "handlers",
		"go.mod":                      "go",
	} {
		if got := commitScope(path); got != want {
			t.Errorf("commitScope(%q) = %q, want %q", path, got, want)
		}
	}
}

// ONE LINE, PLUS THE TRAILER THAT LINKS IT TO ITS TICKET.
//
// A body explaining one file's change, repeated once per file, says the same
// thing several times and buries the line that differs. The trailer stays: it is
// what connects a commit to the ticket that asked for it.
func TestPerFileCommitIsOneLineAndScoped(t *testing.T) {
	msg := perFileCommitMessage(Ticket{TicketID: "t-1", Title: "x"},
		"Add the in-memory store", "feat", "store.go")

	subject, rest, _ := strings.Cut(msg, "\n")
	if subject != "feat(store): add the in-memory store" {
		t.Errorf("subject = %q", subject)
	}
	if len(subject) > 72 {
		t.Errorf("subject is %d characters; the hook rejects over 72", len(subject))
	}
	if rest != "\nTicket: t-1" {
		t.Errorf("body = %q, want only the ticket trailer", rest)
	}
}

// A type the model invented is not a type. commitlint would reject it and so
// would the hook, so it becomes chore rather than failing the commit.
func TestAnInventedTypeBecomesChore(t *testing.T) {
	msg := perFileCommitMessage(Ticket{TicketID: "t-1"}, "do a thing", "implemented", "store.go")
	if !strings.HasPrefix(msg, "chore(store): ") {
		t.Errorf("msg = %q, want an invented type replaced by chore", msg)
	}
}

// ONE FILE IS ONE COMMIT ALREADY. Splitting a single change into a per-file
// commit and then a catch-all would commit it twice, or leave an empty one.
func TestASingleFileIsNotSplit(t *testing.T) {
	a := &DevAgent{}
	s := &devState{staged: map[string]string{"store.go": "package main\n"}}
	if got := a.splitCommitScript(Ticket{TicketID: "t-1"}, s); got != "" {
		t.Errorf("a one-file change produced a split script:\n%s", got)
	}
}

// SEVERAL FILES BECOME SEVERAL COMMITS, in a stable order so a run is
// reproducible and a diff of two runs is readable.
func TestSeveralFilesBecomeSeveralCommits(t *testing.T) {
	a := &DevAgent{}
	s := &devState{
		summary:    "add the ticketing system",
		commitType: "feat",
		staged: map[string]string{
			"store.go":    "x",
			"handlers.go": "y",
			"board.go":    "z",
		},
	}
	script := a.splitCommitScript(Ticket{TicketID: "t-1"}, s)

	for _, want := range []string{"'board.go'", "'handlers.go'", "'store.go'"} {
		if !strings.Contains(script, "git add -- "+want) {
			t.Errorf("the script never stages %s", want)
		}
	}
	if n := strings.Count(script, gitCommit); n != 3 {
		t.Errorf("the script makes %d commits, want one per file", n)
	}
	// Sorted, so two runs of the same change produce the same script.
	if strings.Index(script, "board.go") > strings.Index(script, "handlers.go") {
		t.Error("the files are not committed in a stable order")
	}
}

// THE HOOK IS THE ENFORCEMENT, so it is run rather than read.
//
// It is a shell script embedded in a Go string inside a heredoc; every one of
// those layers has its own escaping, and "it looks right" has never been the same
// thing as "sh accepts it".
func TestTheAngularHookAcceptsAndRejects(t *testing.T) {
	integrationTest(t)
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no shell")
	}
	dir := t.TempDir()

	// Pull the hook body out of the setup script's heredoc and write it out.
	_, rest, ok := strings.Cut(angularHookScript, "<<'BLACKSMITH_HOOK'\n")
	if !ok {
		t.Fatal("the hook is no longer written by a heredoc; this test reads it out of one")
	}
	body, _, ok := strings.Cut(rest, "\nBLACKSMITH_HOOK")
	if !ok {
		t.Fatal("the heredoc is not terminated as expected")
	}
	hook := filepath.Join(dir, "commit-msg")
	if err := os.WriteFile(hook, []byte(body+"\n"), 0o700); err != nil {
		t.Fatalf("write hook: %v", err)
	}

	// BOTH COPIES, THE SAME CASES. hooks/commit-msg holds this repository's own
	// commits to the standard the agents are held to, and two copies of a rule
	// drift the moment one is edited alone. Running them against one table is what
	// makes that a test failure rather than a surprise months later.
	hooks := map[string]string{"sandbox": hook}
	if _, err := os.Stat("hooks/commit-msg"); err == nil {
		hooks["repo"] = "hooks/commit-msg"
	} else {
		t.Error("hooks/commit-msg is missing; this repository is no longer held to the rule it enforces")
	}

	for _, tc := range []struct {
		name    string
		msg     string
		wantErr bool
	}{
		{"plain conventional", "feat: add the store", false},
		{"scoped", "feat(store): add the store", false},
		{"breaking", "feat(store)!: change the store", false},
		{"with a body", "fix(api): return 404\n\nThe route was never registered.", false},
		{"no type", "added the store", true},
		{"invented type", "implemented(store): add the store", true},
		{"no subject", "feat(store):", true},
		{"over 72", "feat(store): " + strings.Repeat("x", 70), true},
		{"body with no blank line", "fix(api): return 404\nThe route was never registered.", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := filepath.Join(dir, "msg")
			if err := os.WriteFile(f, []byte(tc.msg), 0o600); err != nil {
				t.Fatalf("write message: %v", err)
			}
			for which, path := range hooks {
				err := exec.Command("sh", path, f).Run()
				if tc.wantErr && err == nil {
					t.Errorf("the %s hook accepted %q", which, tc.msg)
				}
				if !tc.wantErr && err != nil {
					t.Errorf("the %s hook rejected %q: %v", which, tc.msg, err)
				}
			}
		})
	}
}

// WHAT THE AGENTS ACTUALLY COMMIT MUST PASS THE HOOK THEY ARE HELD TO.
//
// This is the test that was missing, and r104 is what it cost. The hook counts
// BYTES, because a container's locale cannot be relied on to make `wc -m` mean
// characters; clip counted RUNES and appended a three-byte ellipsis. The agents
// write prose with em dashes, so seventy runes came out well over seventy-two
// bytes, the hook rejected the commit, the push failed, and all five
// specification sections blocked with "could not push the branch" — a message
// pointing squarely at the network.
//
// The earlier hook test passed because every message in it was ASCII and short.
func TestAgentCommitsPassTheHookTheyAreHeldTo(t *testing.T) {
	integrationTest(t)
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no shell")
	}
	dir := t.TempDir()
	_, rest, ok := strings.Cut(angularHookScript, "<<'BLACKSMITH_HOOK'\n")
	if !ok {
		t.Fatal("the hook is no longer written by a heredoc")
	}
	body, _, ok := strings.Cut(rest, "\nBLACKSMITH_HOOK")
	if !ok {
		t.Fatal("the heredoc is not terminated as expected")
	}
	hook := filepath.Join(dir, "commit-msg")
	if err := os.WriteFile(hook, []byte(body+"\n"), 0o700); err != nil {
		t.Fatalf("write hook: %v", err)
	}

	ticket := Ticket{TicketID: "bc928d57-1234-5678-9abc-def012345678", Title: "a ticket"}
	// Real agent prose: long, and full of the punctuation a model reaches for.
	summaries := []string{
		"write the tests for the ticket type — status constants, validation and the zero value",
		"добавить хранилище — проверка пустого заголовка и повторяющихся идентификаторов",
		"add the in-memory store with unique ids, empty-title rejection and status validation, plus tests",
		"fix",
		strings.Repeat("é", 200),
		"",
	}

	check := func(name, msg string) {
		t.Helper()
		subject, _, _ := strings.Cut(msg, "\n")
		if len(subject) > 72 {
			t.Errorf("%s: subject is %d BYTES, over the 72 the hook enforces: %q", name, len(subject), subject)
		}
		if !utf8.ValidString(subject) {
			t.Errorf("%s: subject was cut mid-rune: %q", name, subject)
		}
		f := filepath.Join(dir, "msg")
		if err := os.WriteFile(f, []byte(msg), 0o600); err != nil {
			t.Fatalf("write message: %v", err)
		}
		if err := exec.Command("sh", hook, f).Run(); err != nil {
			t.Errorf("%s: the hook rejected what the agent commits: %q", name, subject)
		}
	}

	for _, summary := range summaries {
		for _, kind := range []string{"feat", "fix", "test", "refactor"} {
			check("commitMessage", commitMessage(ticket, summary, kind))
			for _, path := range []string{"store.go", "internal/logfmt/describe.go", "handlers_test.go"} {
				check("perFileCommitMessage", perFileCommitMessage(ticket, summary, kind, path))
			}
		}
	}
}

// A subject is cut on a rune boundary, or it is not valid UTF-8 and git says so.
func TestClipSubjectCutsOnARuneBoundary(t *testing.T) {
	for _, max := range []int{1, 2, 3, 4, 5, 10, 71, 72} {
		got := clipSubject(strings.Repeat("é", 50), max)
		if len(got) > max {
			t.Errorf("clipSubject(max=%d) returned %d bytes", max, len(got))
		}
		if !utf8.ValidString(got) {
			t.Errorf("clipSubject(max=%d) cut mid-rune: %q", max, got)
		}
	}
	if got := clipSubject("short", 72); got != "short" {
		t.Errorf("clipSubject left a short subject as %q", got)
	}
}
