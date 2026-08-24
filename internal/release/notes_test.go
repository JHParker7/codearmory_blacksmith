package release

import (
	"strings"
	"testing"
	"time"

	"github.com/code-armory-app/blacksmith/internal/ticket"
)

func TestParseSubjectReadsAConventionalSubject(t *testing.T) {
	cases := map[string]Change{
		"feat: add the list endpoint":         {Kind: "feat", Text: "add the list endpoint"},
		"feat(api): add the list endpoint":    {Kind: "feat", Scope: "api", Text: "add the list endpoint"},
		"fix(store)!: rename the field":       {Kind: "fix", Scope: "store", Breaking: true, Text: "rename the field"},
		"refactor!: replace the store":        {Kind: "refactor", Breaking: true, Text: "replace the store"},
		"revert: feat(api): add the endpoint": {Kind: "revert", Text: "feat(api): add the endpoint"},
	}
	for subject, want := range cases {
		if got := ParseSubject(subject); got != want {
			t.Errorf("ParseSubject(%q) = %+v, want %+v", subject, got, want)
		}
	}
}

// A SUBJECT THAT IS NOT CONVENTIONAL KEEPS ALL OF ITSELF. The hook rejects these
// now, but history written before it exists still has to render — and half a
// sentence in the notes is worse than an unsorted one: "add an index" from
// "store: add an index" has lost the only word saying where.
func TestParseSubjectKeepsProseWhole(t *testing.T) {
	for _, subject := range []string{
		"Merge branch 'main'",
		"store: add an index",    // a colon, but not a known type
		"Feat: add the endpoint", // conventional types are lower case
		"updated the readme",     // no colon at all
		"chore add the endpoint", // no colon after the type
	} {
		got := ParseSubject(subject)
		if got.Kind != "" {
			t.Errorf("ParseSubject(%q) claimed kind %q", subject, got.Kind)
		}
		if got.Text != strings.TrimSpace(subject) {
			t.Errorf("ParseSubject(%q).Text = %q; the whole subject must survive", subject, got.Text)
		}
	}
}

func TestNotesGroupChangesUnderHeadings(t *testing.T) {
	notes := Notes("v0.2.0", []string{
		"feat(api): add the list endpoint",
		"fix(store): handle a nil map",
		"docs: explain the pipeline",
		"feat: add pagination",
	})

	if !strings.HasPrefix(notes, ReleasedMarker+" v0.2.0") {
		t.Fatalf("notes do not open with the marker and version:\n%s", notes)
	}
	for _, want := range []string{"**Features**", "**Fixes**", "**Documentation**"} {
		if !strings.Contains(notes, want) {
			t.Errorf("notes have no %s section:\n%s", want, notes)
		}
	}
	// THE SCOPE LEADS, because "store: handle a nil map" says where before what.
	if !strings.Contains(notes, "- `store` handle a nil map") {
		t.Errorf("the scope does not lead the line:\n%s", notes)
	}
	if strings.Index(notes, "**Features**") > strings.Index(notes, "**Fixes**") {
		t.Errorf("features come after fixes:\n%s", notes)
	}
}

// BREAKING CHANGES COME FIRST because they are the ones that need reading.
func TestBreakingChangesLeadTheNotes(t *testing.T) {
	notes := Notes("v2.0.0", []string{
		"fix: handle a nil map",
		"feat(store)!: replace the interface",
	})
	if !strings.Contains(notes, "**Breaking**") {
		t.Fatalf("no breaking section:\n%s", notes)
	}
	if strings.Index(notes, "**Breaking**") > strings.Index(notes, "**Fixes**") {
		t.Errorf("the breaking change is not first:\n%s", notes)
	}
	// It still appears under its own type: a person scanning Features should see
	// the change that landed there.
	if !strings.Contains(notes, "**Features**") {
		t.Errorf("the breaking feature vanished from its own section:\n%s", notes)
	}
}

// EVERY COMMIT APPEARS SOMEWHERE. A closed set of headings is only safe because
// what it does not claim lands under Other — notes that quietly omit a commit
// are worse than notes with an untidy heading.
func TestNothingIsDroppedFromTheNotes(t *testing.T) {
	subjects := []string{
		"feat: add it",
		"chore: tidy the imports", // a known type with no section
		"style: gofmt",            // ditto
		"Merge branch 'main'",     // not conventional at all
		"wip: something",          // an unknown type
	}
	notes := Notes("v0.2.0", subjects)
	for _, s := range subjects {
		text := ParseSubject(s).Text
		if !strings.Contains(notes, text) {
			t.Errorf("%q is in no section of the notes:\n%s", s, notes)
		}
	}
	if !strings.Contains(notes, "**Other**") {
		t.Errorf("nothing was collected under Other:\n%s", notes)
	}
}

// A release with nothing in it must say so rather than rendering an empty page —
// a merge that changed nothing is a real outcome, not a rendering failure.
func TestAnEmptyReleaseSaysSo(t *testing.T) {
	for _, subjects := range [][]string{nil, {}, {"", "   "}} {
		notes := Notes("v0.1.0", subjects)
		if !strings.Contains(notes, "No commits since the last release.") {
			t.Errorf("notes for %q do not say the release is empty:\n%s", subjects, notes)
		}
	}
}

func TestNotesEndWithoutTrailingBlankLines(t *testing.T) {
	notes := Notes("v0.2.0", []string{"feat: add it"})
	if strings.HasSuffix(notes, "\n") {
		t.Errorf("notes end with a blank line: %q", notes)
	}
}

// THE LAST RELEASE WINS. A ticket sent back and integrated twice carries two,
// and the one describing the code now is the later.
func TestOfReadsTheLatestReleaseOffATicket(t *testing.T) {
	at := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	tk := ticket.Ticket{Comments: []ticket.Comment{
		{ID: "c-1", Body: "an ordinary comment", CreatedAt: at},
		{ID: "c-2", Body: ReleasedMarker + " v0.1.0\n\n**Features**\n- add it", CreatedAt: at.Add(time.Minute)},
		{ID: "c-3", Body: "rejected by review", CreatedAt: at.Add(2 * time.Minute)},
		{ID: "c-4", Body: ReleasedMarker + " v0.2.0\n\n**Fixes**\n- mend it", CreatedAt: at.Add(3 * time.Minute)},
	}}

	version, notes := Of(tk)
	if version != "v0.2.0" {
		t.Errorf("version = %q, want the later release v0.2.0", version)
	}
	if !strings.Contains(notes, "mend it") {
		t.Errorf("notes = %q, want the later release's", notes)
	}
}

func TestOfReportsNothingWhenNothingWasReleased(t *testing.T) {
	tk := ticket.Ticket{Comments: []ticket.Comment{{ID: "c-1", Body: "merged cleanly"}}}
	if v, n := Of(tk); v != "" || n != "" {
		t.Errorf("Of() = (%q, %q) on a ticket with no release", v, n)
	}
}

// The notes a stage writes must be the notes the window reads back. These two
// halves are the only place the marker is agreed on, and a mismatch shows up as
// a release that is never displayed.
func TestWhatNotesWritesIsWhatOfReads(t *testing.T) {
	written := Notes("v1.2.3", []string{"feat(api): add it"})
	tk := ticket.Ticket{Comments: []ticket.Comment{{ID: "c-1", Body: written}}}

	version, notes := Of(tk)
	if version != "v1.2.3" {
		t.Errorf("version = %q, want v1.2.3", version)
	}
	if !strings.Contains(notes, "`api` add it") {
		t.Errorf("notes = %q, want the rendered entry", notes)
	}
}
