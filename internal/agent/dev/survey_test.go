package dev

import (
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/code-armory-app/blacksmith/internal/edit"
)

// runInDir executes a script under `set -e` in a real directory, which is what
// the sandbox preamble does.
func runInDir(t *testing.T, dir, script string) string {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh is not available")
	}
	cmd := exec.Command("sh", "-c", "set -e\n"+script)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the script failed: %v\n%s", err, out)
	}
	return string(out)
}

// FRAMED AND BASE64-ENCODED, so content with newlines, shell metacharacters or
// invalid UTF-8 survives intact. The alternative is parsing a concatenation of
// arbitrary file contents, where any file containing the separator corrupts
// everything after it.
func TestAReadSurvivesWhateverIsInTheFiles(t *testing.T) {
	if _, err := exec.LookPath("base64"); err != nil {
		t.Skip("base64 is not available")
	}
	dir := t.TempDir()

	files := map[string]string{
		"store.go":  "package main\n\ntype Task struct {\n\tID string `json:\"id\"`\n}\n",
		"weird.txt": "$(rm -rf /) && `whoami` 'quoted'\nsecond line\n",
		// A file whose CONTENT contains the frame marker. Without base64 this
		// would be read as the start of another file.
		"nasty.txt": FileBlockMarker + "not-a-real-file\nZm9v\n",
		"empty.txt": "",
	}
	var paths []string
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, name)
	}

	got := ParseRead(runInDir(t, dir, ReadScript(paths)))

	for name, want := range files {
		if got[name] != want {
			t.Errorf("%s did not survive:\n got %q\nwant %q", name, got[name], want)
		}
	}
	if len(got) != len(files) {
		t.Errorf("read %d files, want %d: %v", len(got), len(files), got)
	}
}

// A MISSING FILE IS SIMPLY ABSENT rather than an error. The model names paths
// from a listing that may be stale, and failing the whole read because one of
// four does not exist would cost a turn and teach nothing.
func TestAReadOfAFileThatIsNotThereStillReturnsTheRest(t *testing.T) {
	if _, err := exec.LookPath("base64"); err != nil {
		t.Skip("base64 is not available")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "here.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := ParseRead(runInDir(t, dir, ReadScript([]string{"here.go", "gone.go"})))
	if got["here.go"] != "package main\n" {
		t.Errorf("the file that exists was lost: %v", got)
	}
	if _, found := got["gone.go"]; found {
		t.Errorf("a file that does not exist was returned: %v", got)
	}
}

// A PATH WITH AN APOSTROPHE IN IT closed a quote early elsewhere and made a
// whole script exit on a syntax error.
func TestAReadQuotesEveryPathItIsGiven(t *testing.T) {
	if _, err := exec.LookPath("base64"); err != nil {
		t.Skip("base64 is not available")
	}
	dir := t.TempDir()
	name := "it's a file.go"
	if err := os.WriteFile(filepath.Join(dir, name), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := ParseRead(runInDir(t, dir, ReadScript([]string{name, "plain.go"})))
	if got[name] != "package main\n" {
		t.Errorf("a path with an apostrophe was not read: %v", got)
	}
}

// A BLOCK THAT WILL NOT DECODE IS SKIPPED, not fatal: the surrounding output is
// still good, and one unreadable file must not cost the three that read cleanly.
func TestAMalformedBlockDoesNotLoseTheGoodOnes(t *testing.T) {
	good := base64.StdEncoding.EncodeToString([]byte("package main\n"))
	out := FileBlockMarker + "a.go\n" + good + "\n" +
		FileBlockMarker + "b.go\n!!!not base64!!!\n" +
		FileBlockMarker + "c.go\n" + good + "\n" +
		// A marker at the very end with nothing after it.
		FileBlockMarker + "d.go"

	got := ParseRead(out)
	if got["a.go"] == "" || got["c.go"] == "" {
		t.Errorf("a good block was lost: %v", got)
	}
	if _, found := got["b.go"]; found {
		t.Error("an undecodable block was returned")
	}
	if _, found := got["d.go"]; found {
		t.Error("a truncated block was returned")
	}
}

// A FILE LARGER THAN THE BOUND IS ALMOST ALWAYS GENERATED, and pasting it whole
// leaves no room for the work.
func TestAVeryLargeFileIsClippedRatherThanFillingTheWindow(t *testing.T) {
	huge := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", MaxFileForModel*2)))
	got := ParseRead(FileBlockMarker + "big.txt\n" + huge + "\n")

	// The bound is in BYTES, and the marker below is the only thing added.
	if len(got["big.txt"]) > MaxFileForModel+32 {
		t.Errorf("a %d-byte file reached the prompt at %d bytes",
			MaxFileForModel*2, len(got["big.txt"]))
	}
	if got["big.txt"] == "" {
		t.Error("the file was dropped rather than clipped")
	}
	// AND IT SAYS IT WAS CUT. A truncated file the agent does not know is
	// truncated is worse than a short one: it quotes text near what it believes
	// is the end, and the anchor never matches.
	if !strings.Contains(got["big.txt"], "truncated") {
		t.Error("a clipped file does not say it was clipped")
	}
}

// A FILE IS DATA: what the agent sees has to be what the branch holds, bounded
// only in length.
//
// Using the ordinary whitespace-trimming clip here silently changed every file.
// The trailing newline went, so a whole-file rewrite of unchanged content read
// as a change; and LEADING whitespace went, which shifts every line number the
// agent is given by one — exactly the arithmetic failure the numbered listing
// exists to prevent.
func TestAFileIsShownExactlyAsTheBranchHoldsIt(t *testing.T) {
	cases := map[string]string{
		"a trailing newline":   "package main\n",
		"no trailing newline":  "package main",
		"a leading blank line": "\npackage main\n",
		"leading indentation":  "\t// generated\npackage main\n",
		"trailing blank lines": "package main\n\n\n",
		"nothing at all":       "",
		"only whitespace":      "\n\t \n",
	}
	for name, content := range cases {
		enc := base64.StdEncoding.EncodeToString([]byte(content))
		got := ParseRead(FileBlockMarker + "f.go\n" + enc + "\n")
		if got["f.go"] != content {
			t.Errorf("%s: the file was altered:\n got %q\nwant %q", name, got["f.go"], content)
		}
	}

	// And the line numbers the agent is shown then line up with the file.
	numbered := NumberLines("\npackage main\n")
	if !strings.HasPrefix(numbered, "1\t\n2\tpackage main") {
		t.Errorf("a leading blank line shifted the numbering: %q", numbered)
	}
}

// A TREE OF TEN THOUSAND FILES IS NOT MORE USEFUL than one of four hundred; it
// is a context window spent on names.
func TestTheSurveyIsBounded(t *testing.T) {
	if !strings.Contains(SurveyScript(), "head -n 400") {
		t.Errorf("the survey is unbounded: %s", SurveyScript())
	}
	got := ParseSurvey("store.go\n\n  server.go  \n\n")
	if len(got) != 2 || got[0] != "store.go" || got[1] != "server.go" {
		t.Errorf("the listing was not cleaned: %q", got)
	}
	if got := ParseSurvey("   \n\n"); len(got) != 0 {
		t.Errorf("an empty listing produced %q", got)
	}
}

// A PATH THE MODEL ASKED FOR AND DID NOT GET IS RECORDED AS MISSING, which is
// what lets a later create succeed: without it the write is refused as an
// overwrite of a file the listing mentions and the sandbox does not have, and
// the agent has no way to resolve the contradiction.
func TestAPathThatCameBackEmptyIsRememberedAsMissing(t *testing.T) {
	s := &State{Tree: []string{"store.go", "ghost.go"}}
	s.RecordRead([]string{"store.go", "ghost.go"}, map[string]string{"store.go": storeGo})

	if s.Read["store.go"] != storeGo {
		t.Error("the file that was read was not recorded")
	}
	if !s.Missing["ghost.go"] {
		t.Error("a path that came back empty was not remembered as missing")
	}
	// And that is what makes the create legal.
	if err := Apply(s, []edit.Edit{{Path: "ghost.go", Replace: "package main\n"}}, ModeDevelop); err != nil {
		t.Errorf("a file the sandbox does not have could not be created: %v", err)
	}
}

// A FILE THAT APPEARS LATER STOPS BEING MISSING, or a stale entry lets a blind
// overwrite through on a file that now exists.
func TestAMissingPathThatLaterAppearsIsNoLongerMissing(t *testing.T) {
	s := &State{}
	s.RecordRead([]string{"late.go"}, nil)
	if !s.Missing["late.go"] {
		t.Fatal("the path was not recorded as missing")
	}

	s.RecordRead([]string{"late.go"}, map[string]string{"late.go": "package main\n"})
	if s.Missing["late.go"] {
		t.Error("a path that now exists is still marked missing")
	}
}

// THE BASELINE IS CAPTURED ON THE FIRST READ AND NEVER OVERWRITTEN. It answers
// "is this write throwing away code that was already there", and that needs the
// ORIGINAL — Read is deliberately updated with the agent's own edits.
func TestTheBaselineKeepsWhatTheBranchHadRatherThanWhatTheAgentWrote(t *testing.T) {
	s := &State{Staged: map[string]string{}}
	s.RecordRead([]string{"store.go"}, map[string]string{"store.go": storeGo})

	// The agent edits it, which updates Read.
	if err := Apply(s, []edit.Edit{
		{Path: "store.go", OldStr: "return nil", Replace: "return []Task{}"},
	}, ModeDevelop); err != nil {
		t.Fatal(err)
	}
	if s.Read["store.go"] == storeGo {
		t.Fatal("the edit did not reach what the agent sees")
	}

	// A SECOND READ MUST NOT MOVE THE BASELINE to the edited content, or the
	// comparison silently becomes "is this write the same as my last one".
	s.RecordRead([]string{"store.go"}, map[string]string{"store.go": s.Read["store.go"]})
	if s.Baseline["store.go"] != storeGo {
		t.Errorf("the baseline followed the agent's own edit:\n%s", s.Baseline["store.go"])
	}
	if !s.ChangedFromBaseline() {
		t.Error("a real change reads as a no-op against a moved baseline")
	}
}

// THE HISTORY LINE SAYS WHAT THE READ ACTUALLY RETURNED, because "read 4 files"
// when three did not exist sends the agent looking for content it never got.
func TestAReadSaysWhichPathsDidNotExist(t *testing.T) {
	asked := []string{"a.go", "b.go", "c.go"}

	all := ReadOutcome(asked, map[string]string{"a.go": "x", "b.go": "y", "c.go": "z"})
	if !strings.Contains(all, "read 3 file") {
		t.Errorf("a complete read reported %q", all)
	}

	some := ReadOutcome(asked, map[string]string{"a.go": "x"})
	if !strings.Contains(some, "b.go") || !strings.Contains(some, "c.go") {
		t.Errorf("a partial read does not name what was missing: %q", some)
	}
	if !strings.Contains(some, "read 1 of 3") {
		t.Errorf("a partial read does not say how much it got: %q", some)
	}

	none := ReadOutcome(asked, nil)
	if !strings.Contains(none, "none of those files exist") {
		t.Errorf("an empty read reported %q", none)
	}
}

// A FILE CUT MID-RUNE IS NOT VALID UTF-8, and what the agent is shown then ends
// in a replacement character it will happily quote back in an anchor that can
// never match. The offset is chosen so the byte bound lands INSIDE a rune.
func TestAClippedFileIsStillValidUTF8(t *testing.T) {
	// One ASCII byte then three-byte runes, so byte MaxFileForModel is mid-rune.
	content := "x" + strings.Repeat("…", MaxFileForModel)
	if utf8.RuneStart(content[MaxFileForModel]) {
		t.Fatalf("the fixture does not cut mid-rune; byte %d starts a rune", MaxFileForModel)
	}

	enc := base64.StdEncoding.EncodeToString([]byte(content))
	got := ParseRead(FileBlockMarker + "big.txt\n" + enc + "\n")["big.txt"]

	if got == "" {
		t.Fatal("the file was dropped")
	}
	if !utf8.ValidString(got) {
		t.Errorf("the clipped file is not valid UTF-8; it ends %q", got[max(len(got)-8, 0):])
	}
}
