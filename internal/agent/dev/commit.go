package dev

import (
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/code-armory-app/blacksmith/internal/ticket"
)

// MaxSubjectBytes is the longest a commit subject may be, measured THE WAY THE
// COMMIT-MSG HOOK MEASURES IT.
//
// IN BYTES, NOT RUNES, and the difference cost a whole run. A rune-based clip
// trims to a character count and appends a three-byte ellipsis; the hook counts
// bytes with `wc -c`, because a container's locale cannot be relied on to make
// `wc -m` mean characters. The agents write prose with em dashes, so a subject
// of seventy runes was comfortably over seventy-two bytes.
//
// Measured on r104: every specification section was rejected by the hook, which
// failed the commit, which failed the push, which blocked all five of them and
// the task under them. A COSMETIC RULE ENDED THE RUN — and it ended it with
// "could not push the branch", a message pointing at the network.
const MaxSubjectBytes = 72

// CommitMessage builds a Conventional Commits message for the whole change.
//
// THE TYPE PREFIX IS NOT COSMETIC. Repositories routinely run commitlint on the
// commit-msg hook, and a message without a recognised type is rejected — so an
// agent that writes plain subjects produces branches that cannot be committed at
// all. An unrecognised or missing type falls back to "chore", which is the
// honest choice: WRONG-BUT-VALID BEATS CONFIDENTLY MISLABELLED.
func CommitMessage(t ticket.Ticket, summary, commitType string) string {
	head := normaliseType(commitType)
	return head + ": " + subjectFor(t, summary, head) + "\n\nTicket: " + t.ID
}

// PerFileCommitMessage is one file's commit: a single subject line and nothing
// else but the trailer that links it to its ticket.
//
// SINGLE LINE BECAUSE THE COMMIT IS SMALL. A body explaining one file's change,
// repeated once per file, says the same thing several times and buries the one
// line that differs. The trailer stays: it is what connects a commit to the
// ticket that asked for it, and losing that to save two lines would trade
// something load-bearing for something cosmetic.
func PerFileCommitMessage(t ticket.Ticket, summary, commitType, path string) string {
	head := normaliseType(commitType)
	if scope := CommitScope(path); scope != "" {
		head += "(" + scope + ")"
	}
	return head + ": " + subjectFor(t, summary, head) + "\n\nTicket: " + t.ID
}

func normaliseType(commitType string) string {
	if !slices.Contains(ConventionalTypes, commitType) {
		return "chore"
	}
	return commitType
}

// subjectFor writes the subject line, in the shape commitlint's default config
// enforces: lower-case, unpunctuated, and inside the byte budget left over after
// the type and its separator.
func subjectFor(t ticket.Ticket, summary, head string) string {
	subject := strings.TrimSpace(summary)
	if subject == "" {
		subject = t.Title
	}
	subject = strings.TrimRight(subject, ".")

	// THE FIRST RUNE, NOT THE FIRST BYTE. Taking one byte and lowercasing half of
	// a two-byte rune leaves invalid UTF-8 that git will not take. Found by the
	// test that drives real agent prose through the hook.
	if subject != "" {
		r := []rune(subject)
		r[0] = unicode.ToLower(r[0])
		subject = string(r)
	}

	// THE WHOLE LINE IS WHAT THE HOOK MEASURES, so the budget is what is left of
	// it after the type and the ": ".
	return ClipSubject(subject, MaxSubjectBytes-len(head)-2)
}

// ClipSubject trims a subject to fit a byte budget, cutting on a rune boundary
// so the result is still valid UTF-8.
//
// NO ELLIPSIS: the marker is what pushed the line over the limit in the first
// place, and a subject is a summary — a truncated one reads no worse for ending
// bluntly than for ending in a character costing three of the bytes it is
// apologising for.
func ClipSubject(s string, maxBytes int) string {
	s = strings.TrimSpace(s)
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return strings.TrimSpace(s[:cut])
}

// CommitScope turns a path into a Conventional Commits scope.
//
// The file's own name, without its extension and without the _test that marks it
// as the tests FOR something: store_test.go and store.go are the same scope,
// because they are the same subject seen from two sides.
func CommitScope(path string) string {
	base := path
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	if i := strings.Index(base, "."); i > 0 {
		base = base[:i]
	}
	base = strings.TrimSuffix(base, "_test")

	// A scope has to be safe in a subject line and lower-case by convention.
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		case r >= 'A' && r <= 'Z':
			return r + 32
		}
		return -1
	}, base)
}

// SplitCommits reports whether this change is committed file by file.
//
// ONE COMMIT PER FILE WHEN THERE IS MORE THAN ONE. A single commit carrying the
// store, the handlers and the board page is three changes a reviewer has to
// separate by hand, and its subject can only describe one of them — the agents'
// history is full of "test: added handlers.go (TicketHandler with..." truncated
// mid-thought. Per file, each subject is short enough to be true.
func SplitCommits(staged map[string]string) bool { return len(staged) >= 2 }
