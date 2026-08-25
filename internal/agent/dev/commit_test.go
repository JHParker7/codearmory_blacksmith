package dev

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/code-armory-app/blacksmith/internal/ticket"
)

func job2() ticket.Ticket {
	return ticket.Ticket{ID: "t-42", Title: "Add filtering to the task store"}
}

// IN BYTES, NOT RUNES, AND THE DIFFERENCE COST A WHOLE RUN.
//
// The hook counts bytes with `wc -c`, because a container's locale cannot be
// relied on to make `wc -m` mean characters. The agents write prose with em
// dashes, so a subject of seventy runes was comfortably over seventy-two bytes.
//
// r104: every specification section was rejected by the hook, which failed the
// commit, which failed the push, which blocked all five sections and the task
// under them. A cosmetic rule ended the run, and it ended it with "could not
// push the branch" — a message pointing at the network.
func TestASubjectFitsTheHooksBudgetInBytes(t *testing.T) {
	// Real agent prose: em dashes, which are three bytes each.
	summary := "add a filter — matching on done, title and owner — to the task store's List method"
	if utf8.RuneCountInString(summary) >= len(summary) {
		t.Fatal("the fixture has no multi-byte runes, so it cannot test the distinction")
	}

	for _, msg := range []string{
		CommitMessage(job2(), summary, "feat"),
		PerFileCommitMessage(job2(), summary, "feat", "store.go"),
	} {
		subject := strings.SplitN(msg, "\n", 2)[0]
		if len(subject) > MaxSubjectBytes {
			t.Errorf("the subject is %d BYTES, over the hook's %d:\n%q",
				len(subject), MaxSubjectBytes, subject)
		}
		// AND IT IS STILL VALID UTF-8: cutting mid-rune produces bytes git will
		// not take.
		if !utf8.ValidString(msg) {
			t.Errorf("the message is not valid UTF-8: %q", msg)
		}
	}
}

// NO ELLIPSIS: the marker is what pushed the line over the limit in the first
// place, and it costs three of the bytes it is apologising for.
func TestATruncatedSubjectDoesNotSpendBytesOnAnEllipsis(t *testing.T) {
	msg := CommitMessage(job2(), strings.Repeat("a", 200), "feat")
	if strings.Contains(msg, "…") || strings.Contains(msg, "...") {
		t.Errorf("the subject spends bytes on a truncation marker: %q", msg)
	}
}

func TestClipSubjectCutsOnARuneBoundary(t *testing.T) {
	// Every rune here is two bytes, so a byte budget of 5 must cut at 4.
	s := "ééééé"
	got := ClipSubject(s, 5)
	if !utf8.ValidString(got) {
		t.Errorf("ClipSubject cut mid-rune: %q", got)
	}
	if len(got) > 5 {
		t.Errorf("ClipSubject returned %d bytes for a budget of 5", len(got))
	}

	// A NON-POSITIVE BUDGET IS A CALLER'S ARITHMETIC, not a request for nothing,
	// and it must not panic: the budget is MaxSubjectBytes minus a type and a
	// separator, and a long enough type makes it negative.
	if got := ClipSubject("hello", -4); got != "" {
		t.Errorf("ClipSubject(-4) = %q", got)
	}
	if got := ClipSubject("  hello  ", 99); got != "hello" {
		t.Errorf("ClipSubject(99) = %q", got)
	}
}

// THE WHOLE LINE IS WHAT THE HOOK MEASURES, so a long scope has to come out of
// the subject's budget rather than being added on top of it.
func TestALongScopeShortensTheSubjectRatherThanTheLine(t *testing.T) {
	long := PerFileCommitMessage(job2(), strings.Repeat("a", 200), "refactor",
		"some/deep/path/a_very_long_file_name_indeed.go")
	subject := strings.SplitN(long, "\n", 2)[0]
	if len(subject) > MaxSubjectBytes {
		t.Errorf("the scope pushed the line to %d bytes:\n%q", len(subject), subject)
	}
	if !strings.Contains(subject, "a_very_long_file_name_indeed") {
		t.Errorf("the scope was lost: %q", subject)
	}
}

// AN UNRECOGNISED TYPE FALLS BACK TO "chore": wrong-but-valid beats confidently
// mislabelled, and a message without a recognised type is REJECTED by the hook —
// so an agent that writes plain subjects produces branches that cannot be
// committed at all.
func TestAnUnrecognisedTypeBecomesOneTheHookAccepts(t *testing.T) {
	for _, bad := range []string{"", "improvement", "FEAT", "feature", "wip"} {
		msg := CommitMessage(job2(), "do the thing", bad)
		got, _, _ := strings.Cut(msg, ":")
		if !contains(ConventionalTypes, got) {
			t.Errorf("type %q produced %q, which the hook rejects", bad, got)
		}
		if got != "chore" {
			t.Errorf("type %q became %q, want chore", bad, got)
		}
	}
	// A recognised one is kept.
	if !strings.HasPrefix(CommitMessage(job2(), "x", "fix"), "fix: ") {
		t.Error("a valid type was replaced")
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// commitlint's default config enforces a lower-case, unpunctuated subject.
func TestTheSubjectIsShapedTheWayTheHookWantsIt(t *testing.T) {
	msg := CommitMessage(job2(), "Added the filter to the store.", "feat")
	subject := strings.SplitN(msg, "\n", 2)[0]

	if !strings.HasPrefix(subject, "feat: added") {
		t.Errorf("the subject was not lower-cased: %q", subject)
	}
	if strings.HasSuffix(subject, ".") {
		t.Errorf("the subject keeps its full stop: %q", subject)
	}
}

// THE FIRST RUNE, NOT THE FIRST BYTE. Lowercasing half of a two-byte rune leaves
// invalid UTF-8 that git will not take.
func TestLoweringTheSubjectDoesNotCorruptAMultiByteFirstRune(t *testing.T) {
	for _, summary := range []string{"Ärger with the store", "École handling", "日本語 support"} {
		msg := CommitMessage(job2(), summary, "fix")
		if !utf8.ValidString(msg) {
			t.Errorf("%q produced invalid UTF-8: %q", summary, msg)
		}
	}
}

// AN EMPTY SUMMARY FALLS BACK TO THE TICKET, because a commit with no subject is
// rejected outright and the ticket title is the one thing always present.
func TestAnEmptySummaryFallsBackToTheTicketTitle(t *testing.T) {
	for _, summary := range []string{"", "   ", "\n"} {
		msg := CommitMessage(job2(), summary, "feat")
		if !strings.Contains(msg, "add filtering to the task store") {
			t.Errorf("summary %q did not fall back to the title: %q", summary, msg)
		}
	}
}

// THE TRAILER IS WHAT CONNECTS A COMMIT TO THE TICKET THAT ASKED FOR IT.
func TestEveryCommitCarriesItsTicket(t *testing.T) {
	for _, msg := range []string{
		CommitMessage(job2(), "x", "feat"),
		PerFileCommitMessage(job2(), "x", "feat", "store.go"),
	} {
		if !strings.HasSuffix(msg, "\n\nTicket: t-42") {
			t.Errorf("the trailer is missing or malformed: %q", msg)
		}
	}
}

// store_test.go AND store.go ARE THE SAME SCOPE, because they are the same
// subject seen from two sides.
func TestTheScopeIsTheSubjectNotTheFile(t *testing.T) {
	for in, want := range map[string]string{
		"store.go":         "store",
		"store_test.go":    "store",
		"a/b/handlers.go":  "handlers",
		"HANDLERS.go":      "handlers",
		"handlers.read.go": "handlers",
		"api-v2.go":        "api-v2",
		"weird name!.go":   "weirdname",
		"":                 "",
		// A LEADING DOT IS PART OF THE NAME, not an extension separator, so a
		// dotfile keeps its own name as the scope.
		".gitignore": "gitignore",
		"Makefile":   "makefile",
	} {
		if got := CommitScope(in); got != want {
			t.Errorf("CommitScope(%q) = %q, want %q", in, got, want)
		}
	}

	// A file whose name survives nothing gets no scope rather than an empty one,
	// which would render as "feat(): ..." and be rejected.
	msg := PerFileCommitMessage(job2(), "x", "feat", "###.go")
	if strings.Contains(msg, "()") {
		t.Errorf("an empty scope reached the subject: %q", msg)
	}
}

// ONE COMMIT PER FILE WHEN THERE IS MORE THAN ONE. A single commit carrying the
// store, the handlers and the board page is three changes a reviewer has to
// separate by hand, and its subject can only describe one of them.
func TestAChangeIsSplitOnlyWhenThereIsSomethingToSplit(t *testing.T) {
	if SplitCommits(nil) || SplitCommits(map[string]string{"a.go": "x"}) {
		t.Error("a single-file change was split into per-file commits")
	}
	if !SplitCommits(map[string]string{"a.go": "x", "b.go": "y"}) {
		t.Error("a multi-file change was committed as one")
	}
}

// PER-FILE SUBJECTS ARE DISTINGUISHABLE, which is the whole point: one subject
// describing three files can only be true of one of them.
func TestPerFileCommitsDifferByTheirScope(t *testing.T) {
	a := PerFileCommitMessage(job2(), "add filtering", "feat", "store.go")
	b := PerFileCommitMessage(job2(), "add filtering", "feat", "handlers.go")
	if a == b {
		t.Errorf("two files produced the same commit subject: %q", a)
	}
	if !strings.Contains(a, "feat(store):") || !strings.Contains(b, "feat(handlers):") {
		t.Errorf("the scopes are not in the subjects:\n%q\n%q", a, b)
	}
}
