package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// THE TYPE DECIDES THE NUMBER, which only works because the commit-msg hook makes
// the type real rather than hoped for.
func TestTheBumpFollowsTheCommitTypes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		subjects []string
		want     string
	}{
		{"only fixes", []string{"fix(store): handle an empty title"}, "patch"},
		{"a feature", []string{"fix(a): x", "feat(store): add an index"}, "minor"},
		{"a breaking marker", []string{"feat(api)!: rename the status field"}, "major"},
		{"a breaking footer", []string{"refactor(api): move things\n\nBREAKING CHANGE: renamed"}, "major"},
		{"chores only", []string{"chore: tidy up", "docs: explain it"}, "patch"},
		{"nothing", nil, "patch"},
	} {
		if got := bumpFor(tc.subjects); got != tc.want {
			t.Errorf("%s: bumpFor = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// 0.x IS NOT 1.x. A breaking change before 1.0 moves the minor, which is what the
// spec says 0.x is for, and cutting v1.0.0 the first time an agent writes a "!"
// would claim a stability nothing has earned.
func TestVersionsBumpTheRightPart(t *testing.T) {
	for _, tc := range []struct{ prev, bump, want string }{
		{"v0.1.0", "patch", "v0.1.1"},
		{"v0.1.0", "minor", "v0.2.0"},
		{"v0.1.0", "major", "v0.2.0"},
		{"v1.4.2", "patch", "v1.4.3"},
		{"v1.4.2", "minor", "v1.5.0"},
		{"v1.4.2", "major", "v2.0.0"},
		{"", "minor", "v0.1.0"},
		{"not-a-version", "major", "v0.1.0"},
	} {
		if got := nextVersion(tc.prev, tc.bump); got != tc.want {
			t.Errorf("nextVersion(%q, %q) = %q, want %q", tc.prev, tc.bump, got, tc.want)
		}
	}
}

// THE NOTES ARE WHAT A PERSON READS, so they are grouped and the breaking ones
// come first.
func TestReleaseNotesGroupTheChanges(t *testing.T) {
	notes := releaseNotes("v0.2.0", []string{
		"feat(store): add the in-memory store",
		"fix(api): return 404 for an unknown id",
		"feat(board)!: drop the legacy column",
		"chore: tidy imports",
	})

	if !strings.HasPrefix(notes, releasedMarker+" v0.2.0") {
		t.Errorf("the notes do not open with the marker and version:\n%s", notes)
	}
	bi := strings.Index(notes, "**Breaking**")
	fi := strings.Index(notes, "**Features**")
	if bi < 0 || fi < 0 || bi > fi {
		t.Errorf("breaking changes are not first:\n%s", notes)
	}
	for _, want := range []string{"`store` add the in-memory store", "`api` return 404 for an unknown id"} {
		if !strings.Contains(notes, want) {
			t.Errorf("the notes are missing %q:\n%s", want, notes)
		}
	}
	// An unlisted type is collected rather than dropped.
	if !strings.Contains(notes, "tidy imports") {
		t.Errorf("a chore vanished from the notes:\n%s", notes)
	}
}

// The window reads the version and notes back off the ticket the same way it
// reads a merge.
func TestAReleaseIsReadableBackOffTheTicket(t *testing.T) {
	notes := releaseNotes("v0.3.0", []string{"feat(store): add an index"})
	tk := Ticket{Comments: []Comment{
		{Body: "some earlier note"},
		{Body: notes},
	}}

	v, body := releaseOf(tk)
	if v != "v0.3.0" {
		t.Errorf("version = %q, want v0.3.0", v)
	}
	if !strings.Contains(body, "add an index") {
		t.Errorf("notes = %q", body)
	}
}

// Two releases on one ticket means it was sent back and integrated again; the
// later one describes the code now.
func TestTheLatestReleaseWins(t *testing.T) {
	tk := Ticket{Comments: []Comment{
		{Body: releaseNotes("v0.1.0", []string{"feat(a): one"})},
		{Body: releaseNotes("v0.2.0", []string{"feat(b): two"})},
	}}
	if v, _ := releaseOf(tk); v != "v0.2.0" {
		t.Errorf("version = %q, want the later v0.2.0", v)
	}
}

// A merge that cut no tag is not a release, and must not render as one.
func TestNoTagMeansNoRelease(t *testing.T) {
	if _, _, ok := parseRelease("merged cleanly, gates passed"); ok {
		t.Error("output with no release block was read as a release")
	}
	if v, _ := releaseOf(Ticket{Comments: []Comment{{Body: "**Merged.**"}}}); v != "" {
		t.Errorf("version = %q on a ticket with no release", v)
	}
}

// THE SCRIPT IS WHERE THE RULE RUNS, and Go is where it can be tested. If the two
// disagree, the tag says one thing and everything reading it says another — so
// they are run against the same cases.
//
// The script is shell inside a Go string with awk inside that; "it looks right"
// has never been the same thing as "sh accepts it".
func TestTheBumpRuleMatchesTheScript(t *testing.T) {
	integrationTest(t)
	for _, bin := range []string{"sh", "git", "awk"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("no %s", bin)
		}
	}

	for _, tc := range []struct {
		name     string
		prev     string
		subjects []string
	}{
		// EVERY BUMP, not just the one. The first version of this test carried a
		// feat here and nothing else, which is the single bump where the two halves
		// agreed — so r106 cut v0.0.1 from a patch-only first release while Go
		// would have said v0.1.0, and the test written to prevent exactly that
		// passed.
		{"first release from a fix", "", []string{"fix(store): handle empty"}},
		{"first release from a chore", "", []string{"chore: set up the module"}},
		{"first release from a breaking change", "", []string{"feat(api)!: rename it"}},
		{"first release", "", []string{"feat(store): add the store"}},
		{"a patch", "v0.1.0", []string{"fix(store): handle empty"}},
		{"a feature", "v0.1.0", []string{"feat(store): add an index"}},
		{"breaking below one", "v0.4.2", []string{"feat(api)!: rename it"}},
		{"breaking above one", "v1.4.2", []string{"feat(api)!: rename it"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			run := func(args ...string) {
				t.Helper()
				cmd := exec.Command(args[0], args[1:]...)
				cmd.Dir = dir
				cmd.Env = append(os.Environ(),
					"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("%v: %v\n%s", args, err, out)
				}
			}
			run("git", "init", "-q", "-b", "main")
			if err := os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
			run("git", "add", "-A")
			run("git", "commit", "-q", "-m", "chore: base")
			if tc.prev != "" {
				run("git", "tag", "-a", tc.prev, "-m", tc.prev)
			}
			for i, s := range tc.subjects {
				if err := os.WriteFile(filepath.Join(dir, "f"), []byte(strings.Repeat("x", i+2)), 0o600); err != nil {
					t.Fatal(err)
				}
				run("git", "add", "-A")
				run("git", "commit", "-q", "-m", s)
			}

			// The script pushes; there is no remote here, and it is written to
			// tolerate that.
			cmd := exec.Command("sh", "-c", releaseTagScript())
			cmd.Dir = dir
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("script failed: %v\n%s", err, out)
			}
			gotVersion, gotSubjects, ok := parseRelease(string(out))
			if !ok {
				t.Fatalf("the script printed no release block:\n%s", out)
			}

			want := nextVersion(tc.prev, bumpFor(tc.subjects))
			if gotVersion != want {
				t.Errorf("script cut %s, Go would have cut %s", gotVersion, want)
			}
			if len(gotSubjects) == 0 {
				t.Error("the script reported no commit subjects, so the notes would be empty")
			}
		})
	}
}
