package release

import (
	"fmt"
	"strings"

	"github.com/code-armory-app/blacksmith/internal/ticket"
)

// Change is one commit, read back from its subject.
type Change struct {
	Kind     string
	Scope    string
	Breaking bool
	Text     string
}

// types are the Conventional Commits types this department writes. Anything
// else in a subject means the subject is not one — the hook rejects those now,
// but history written before it exists still has to render.
var types = []string{
	"feat", "fix", "docs", "style", "refactor", "perf",
	"test", "build", "ci", "chore", "revert",
}

// ParseSubject reads a Conventional Commits subject. A subject that is not one
// comes back with an empty Kind and its whole text preserved — never dropped,
// because the point of the notes is to say what changed.
func ParseSubject(subject string) Change {
	subject = strings.TrimSpace(subject)

	head, text, ok := strings.Cut(subject, ": ")
	if !ok {
		return Change{Text: subject}
	}

	c := Change{Text: strings.TrimSpace(text)}
	if strings.HasSuffix(head, "!") {
		c.Breaking = true
		head = strings.TrimSuffix(head, "!")
	}
	if i := strings.Index(head, "("); i >= 0 && strings.HasSuffix(head, ")") {
		c.Scope = head[i+1 : len(head)-1]
		head = head[:i]
	}
	if !known(head) {
		// NOT A CONVENTIONAL SUBJECT AFTER ALL. Returning the fragments would put
		// half a sentence in the notes — "add an index" from "store: add an index"
		// loses the only word saying where — so the whole subject is kept as text.
		return Change{Text: subject}
	}
	c.Kind = head
	return c
}

func known(kind string) bool {
	for _, t := range types {
		if t == kind {
			return true
		}
	}
	return false
}

// sections are the order types appear in the notes, and the heading each gets.
//
// A CLOSED SET WITH A FALLBACK, the same rule the metric labels follow: a type
// not listed is collected under "Other", so a new one SHOWS UP rather than
// vanishing. Notes that quietly omit a commit are worse than notes with an
// untidy heading.
var sections = []struct{ Kind, Heading string }{
	{"feat", "Features"},
	{"fix", "Fixes"},
	{"perf", "Performance"},
	{"refactor", "Refactoring"},
	{"docs", "Documentation"},
	{"test", "Tests"},
	{"build", "Build"},
	{"ci", "CI"},
	{"revert", "Reverts"},
}

// Notes renders one release: what changed, grouped, with the breaking changes
// FIRST because they are the ones that need reading.
func Notes(version string, subjects []string) string {
	var changes []Change
	for _, s := range subjects {
		if strings.TrimSpace(s) == "" {
			continue
		}
		changes = append(changes, ParseSubject(s))
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s %s\n", ReleasedMarker, version)
	if len(changes) == 0 {
		b.WriteString("\nNo commits since the last release.")
		return b.String()
	}

	writeSection(&b, "Breaking", filter(changes, func(c Change) bool { return c.Breaking }))
	for _, sec := range sections {
		writeSection(&b, sec.Heading, filter(changes, func(c Change) bool { return c.Kind == sec.Kind }))
	}
	// EVERY COMMIT APPEARS SOMEWHERE. A change with no kind, or with one no
	// section claims, lands here rather than being dropped — which is what makes
	// the closed set above safe to keep closed.
	writeSection(&b, "Other", filter(changes, func(c Change) bool { return !sectioned(c.Kind) }))

	return strings.TrimRight(b.String(), "\n")
}

func writeSection(b *strings.Builder, heading string, in []Change) {
	if len(in) == 0 {
		return
	}
	fmt.Fprintf(b, "\n**%s**\n", heading)
	for _, c := range in {
		b.WriteString("- " + line(c) + "\n")
	}
}

func filter(changes []Change, keep func(Change) bool) []Change {
	var out []Change
	for _, c := range changes {
		if keep(c) {
			out = append(out, c)
		}
	}
	return out
}

func sectioned(kind string) bool {
	for _, sec := range sections {
		if sec.Kind == kind {
			return true
		}
	}
	return false
}

// line renders one entry: the SCOPE LEADS, because "store: add an index" says
// where before it says what.
func line(c Change) string {
	if c.Scope == "" {
		return c.Text
	}
	return "`" + c.Scope + "` " + c.Text
}

// Of reads a release back off a ticket, the way the window reads a merge.
//
// THE LAST ONE WINS. A ticket sent back and integrated twice carries two, and
// the one describing the code now is the later.
func Of(t ticket.Ticket) (version, notes string) {
	for _, c := range t.Comments {
		body := strings.TrimSpace(c.Body)
		if !strings.HasPrefix(body, ReleasedMarker) {
			continue
		}
		first, rest, _ := strings.Cut(body, "\n")
		version = strings.TrimSpace(strings.TrimPrefix(first, ReleasedMarker))
		notes = strings.TrimSpace(rest)
	}
	return version, notes
}
