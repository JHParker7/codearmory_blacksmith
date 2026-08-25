package edit

import (
	"strings"
	"testing"
)

// THE MISSING CLOSING BACKTICK ON A STRUCT TAG. Observed on a live ticket, eight
// fields in one struct, all identical. It is the model's own output — the same
// model through the same code path emitted the backtick correctly on an earlier
// run, and nothing here touches backticks.
func TestTheMissingStructTagBacktickIsRepaired(t *testing.T) {
	broken := "package main\n\ntype Task struct {\n" +
		"\tID    string `json:\"id\"\n" +
		"\tName  string `json:\"name\"\n" +
		"\tDone  bool   `json:\"done\"\n" +
		"}\n"

	got, fixed, err := RepairGoSource("task.go", broken)
	if err != nil {
		t.Fatalf("RepairGoSource: %v", err)
	}
	if !fixed {
		t.Fatal("the file was not reported as repaired")
	}
	for _, want := range []string{"`json:\"id\"`", "`json:\"name\"`", "`json:\"done\"`"} {
		if !strings.Contains(got, want) {
			t.Errorf("the repair missed %s:\n%s", want, got)
		}
	}
}

// REPAIRING IS SAFE BECAUSE THE RESULT IS VERIFIED. A Go raw string may
// legitimately span lines, so a lone backtick is not always a missing
// terminator — the repair is a GUESS, accepted only if the file parses
// afterwards.
func TestARepairThatDoesNotParseIsDiscarded(t *testing.T) {
	// A genuine multi-line raw string: adding a backtick to the opening line
	// would not make this parse, so the repair must be thrown away.
	broken := "package main\n\nfunc main() {\n\ts := `line one\n\tline two`\n\tif s {\n"

	got, fixed, err := RepairGoSource("a.go", broken)
	if err == nil {
		t.Fatal("a file that cannot be made to parse was accepted")
	}
	if fixed {
		t.Error("a rejected guess was reported as a repair")
	}
	if got != broken {
		t.Error("a rejected guess changed the content")
	}
	// THE ORIGINAL ERROR IS REPORTED, because an error quoting a line the model
	// never wrote is a worse clue than one quoting its own.
	if strings.Contains(err.Error(), "line one`") {
		t.Errorf("the error quotes the repaired text rather than the model's: %v", err)
	}
}

func TestValidSourceIsLeftExactlyAlone(t *testing.T) {
	src := "package main\n\nfunc main() {}\n"
	got, fixed, err := RepairGoSource("a.go", src)
	if err != nil || fixed || got != src {
		t.Errorf("valid source was touched: fixed=%v err=%v", fixed, err)
	}

	// A LEGITIMATE MULTI-LINE RAW STRING must survive: it has an odd backtick
	// count on two lines and is perfectly valid.
	raw := "package main\n\nvar q = `SELECT *\nFROM tasks`\n\nfunc main() {}\n"
	got, fixed, err = RepairGoSource("a.go", raw)
	if err != nil || fixed || got != raw {
		t.Errorf("a multi-line raw string was mangled: fixed=%v err=%v\n%s", fixed, err, got)
	}
}

// ONLY AN ODD BACKTICK COUNT IS A CANDIDATE. A line whose backticks balance is
// correct whatever else is wrong with the file, and appending to it turns a
// repairable file into one that cannot be repaired at all.
func TestALineWhoseBackticksBalanceIsLeftAlone(t *testing.T) {
	broken := "package main\n\n" +
		"var q = `SELECT 1`\n\n" +
		"type Task struct {\n\tID string `json:\"id\"\n}\n"

	got, fixed, err := RepairGoSource("task.go", broken)
	if err != nil {
		t.Fatalf("a repairable file was refused: %v", err)
	}
	if !fixed {
		t.Fatal("the tag was not repaired")
	}
	if !strings.Contains(got, "var q = `SELECT 1`\n") {
		t.Errorf("a balanced line was extended:\n%s", got)
	}
	if !strings.Contains(got, "`json:\"id\"`") {
		t.Errorf("the tag was not closed:\n%s", got)
	}
}

// A FILE THAT IS NOT GO IS NOT THIS CHECK'S BUSINESS.
func TestNonGoFilesArePassedThroughUntouched(t *testing.T) {
	for _, p := range []string{"README.md", "Makefile", "config.yaml"} {
		src := "this is not go ` at all\n"
		got, fixed, err := RepairGoSource(p, src)
		if err != nil || fixed || got != src {
			t.Errorf("%s was parsed as Go: fixed=%v err=%v", p, fixed, err)
		}
	}
}

// THE AGENT IS NOT GETTING THE CODE WRONG, IT IS GETTING THE ARITHMETIC WRONG.
// Across r69's 77 refusals the diagnosis was correct and unchanging; 41 were a
// range that cut across a function so the replacement landed at file scope.
// "Give a range that covers whole declarations" only restates the requirement it
// is already failing to meet; real numbers end the arithmetic.
func TestTheDeclarationARangeCutsIsNamedWithItsOwnRange(t *testing.T) {
	src := `package main

import "fmt"

type Store struct {
	tasks []string
}

func (s *Store) Add(t string) {
	s.tasks = append(s.tasks, t)
}

func main() {
	fmt.Println("x")
}
`
	cases := []struct {
		name             string
		from, to         int
		wantName         string
		wantFrom, wantTo int
	}{
		{"inside a method", 10, 10, "method Add", 9, 11},
		{"inside a function", 14, 14, "func main", 13, 15},
		{"a type declaration", 6, 6, "type declaration", 5, 7},
		{"the import", 3, 3, "import declaration", 3, 3},
		// OVERLAPPING IS THE CASE THAT MATTERS, not containment: a range that
		// starts inside one declaration and ends inside the next is precisely the
		// edit that leaves statements stranded between them.
		{"across two declarations", 10, 14, "method Add", 9, 11},
	}
	for _, c := range cases {
		name, from, to, ok := EnclosingDecl(src, c.from, c.to)
		if !ok {
			t.Errorf("%s: no declaration found for %d-%d", c.name, c.from, c.to)
			continue
		}
		if name != c.wantName || from != c.wantFrom || to != c.wantTo {
			t.Errorf("%s: got %q %d-%d, want %q %d-%d",
				c.name, name, from, to, c.wantName, c.wantFrom, c.wantTo)
		}
	}

	// A range touching nothing reports nothing rather than guessing.
	if _, _, _, ok := EnclosingDecl(src, 2, 2); ok {
		t.Error("a blank line was attributed to a declaration")
	}
	// PRE-EDIT SOURCE THAT IS ALREADY BROKEN IS NOT THIS CHECK'S BUSINESS.
	if _, _, _, ok := EnclosingDecl("package main\n\nfunc main() {\n", 3, 3); ok {
		t.Error("a declaration was named in source that does not parse")
	}
}

// THE PARSER'S LINE NUMBER REFERS TO A FILE THE AGENT HAS NEVER SEEN: the
// post-edit one. Naming a line it cannot look at is the same mistake as pointing
// at a tool it does not have.
func TestTheWindowShowsTheEditsDamageAroundTheBreak(t *testing.T) {
	var lines []string
	for i := 1; i <= 30; i++ {
		lines = append(lines, "line "+string(rune('a'+i%26)))
	}
	content := strings.Join(lines, "\n") + "\n"

	got := WindowAround(content, "a.go:20:3: expected declaration")
	if !strings.Contains(got, ">> 20\t") {
		t.Errorf("the break is not marked:\n%s", got)
	}
	// A window, not the file: eight lines before and four after.
	if n := len(strings.Split(got, "\n")); n > 13 {
		t.Errorf("the window is %d lines; it should be a window", n)
	}
	if strings.Contains(got, "  1\t") {
		t.Errorf("the window starts at the top of the file:\n%s", got)
	}
}

// CLAMP TO THE FILE BEFORE WINDOWING. A parse error can name a line past the end
// — "expected declaration" where a truncated file simply stops — and an
// unclamped window then starts after it ends and renders nothing, which is worse
// than the message it replaced.
func TestTheWindowSurvivesALineNumberPastTheEndOfTheFile(t *testing.T) {
	content := "package main\n\nfunc main() {\n"

	for _, errText := range []string{
		"a.go:99:1: expected declaration",  // past the end
		"a.go: something with no position", // no line at all
		"",
	} {
		got := WindowAround(content, errText)
		if strings.TrimSpace(got) == "" {
			t.Errorf("the window rendered nothing for %q", errText)
		}
		if !strings.Contains(got, "func main") {
			t.Errorf("the window for %q does not show the file:\n%s", errText, got)
		}
	}

	// A one-line file is a one-line window, not a panic.
	if got := WindowAround("package main\n", "a.go:1:1: x"); !strings.Contains(got, ">> 1\t") {
		t.Errorf("a single-line file rendered %q", got)
	}
}
