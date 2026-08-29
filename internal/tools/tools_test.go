package tools

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeSandbox stands in for forge, so this package's tests need no cluster.
type fakeSandbox struct {
	out  Output
	err  error
	last struct {
		files   map[string]string
		command string
	}
}

func (f *fakeSandbox) Run(_ context.Context, files map[string]string, command string) (Output, error) {
	f.last.files, f.last.command = files, command
	return f.out, f.err
}

func newSet(files map[string]string, g Guard, box Sandbox, names ...string) *Set {
	return &Set{
		Workspace: NewWorkspace(files, g),
		Sandbox:   box,
		Check:     "go test ./...",
		Names:     names,
	}
}

func TestTheDeveloperIsRefusedAtTheWriteWhenItEditsATest(t *testing.T) {
	s := newSet(map[string]string{"a_test.go": "package p\n"}, NoTests, nil)

	got, err := s.Invoke(context.Background(), WriteFile, `{
		"path":"a_test.go","replace":"package q\n","summary":"x","type":"fix"}`)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if !strings.Contains(got, "specification is not yours to change") {
		t.Fatalf("the refusal does not name the cause: %q", got)
	}
}

// The module manifest has to stay writable alongside .go. Measured on the python
// rebuild: without it the developer could not add a require line, could not
// fetch a module either, and reimplemented three libraries from the standard
// library instead.
func TestTheGoGuardStillPermitsTheModuleManifest(t *testing.T) {
	g := OnlyExt(".go")
	for _, p := range []string{"go.mod", "go.sum", "src/go.mod"} {
		if err := g(p); err != nil {
			t.Errorf("%s was refused: %v", p, err)
		}
	}
	if err := g("README.md"); err == nil {
		t.Error("a markdown file was writable at a Go stage")
	}
}

func TestAReviewerCannotWriteAnything(t *testing.T) {
	if err := DenyAll("a.go"); err == nil {
		t.Fatal("the review guard permitted a write")
	}
}

func TestBothGuardsReportTheFirstRefusal(t *testing.T) {
	g := Both(NoTests, OnlyExt(".go"))
	if err := g("a_test.go"); err == nil || !strings.Contains(err.Error(), "specification") {
		t.Fatalf("the test-file rule did not come first: %v", err)
	}
	if err := g("notes.md"); err == nil {
		t.Fatal("a markdown file passed a Go-only guard")
	}
	if err := g("a.go"); err != nil {
		t.Fatalf("an ordinary Go file was refused: %v", err)
	}
}

func TestReadFilesNumbersTheLinesTheRefusalsTellTheModelToCopyFrom(t *testing.T) {
	s := newSet(map[string]string{"a.go": "package p\nfunc F() {}\n"}, nil, nil)

	got, err := s.Invoke(context.Background(), ReadFiles, `{"paths":["a.go"]}`)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if !strings.Contains(got, "1\tpackage p") || !strings.Contains(got, "2\tfunc F() {}") {
		t.Fatalf("the contents are not numbered: %q", got)
	}
}

// A model told only "not found" retries the same wrong path. Shown what is
// actually in the directory, it corrects on the next turn.
func TestAMissingFileNamesTheOnesThatDoExist(t *testing.T) {
	s := newSet(map[string]string{"src/main.go": "", "src/store.go": ""}, nil, nil)

	got, err := s.Invoke(context.Background(), ReadFiles, `{"paths":["src/mian.go"]}`)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if !strings.Contains(got, "store.go") || !strings.Contains(got, "main.go") {
		t.Fatalf("the neighbours were not named: %q", got)
	}
}

func TestSearchReportsPathAndLineNumber(t *testing.T) {
	s := newSet(map[string]string{"a.go": "package p\nfunc Wanted() {}\n"}, nil, nil)

	got, err := s.Invoke(context.Background(), SearchFiles, `{"pattern":"Wanted"}`)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if !strings.Contains(got, "a.go:2:") {
		t.Fatalf("the hit is not addressable: %q", got)
	}
}

func TestABadPatternIsExplainedRatherThanCrashing(t *testing.T) {
	s := newSet(map[string]string{"a.go": ""}, nil, nil)

	got, err := s.Invoke(context.Background(), SearchFiles, `{"pattern":"("}`)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if !strings.Contains(got, "not a valid regular expression") {
		t.Fatalf("a bad pattern was not explained: %q", got)
	}
}

// A tool offered but not accepted is a trap. The set is filtered in one place so
// the offered list and the accepted one cannot drift apart.
func TestAToolOutsideTheSetIsRefusedAndTheRefusalSaysSo(t *testing.T) {
	s := newSet(map[string]string{}, nil, nil, ReadFiles, ListFiles)

	if len(s.Definitions()) != 2 {
		t.Fatalf("the set offered %d tools, want 2", len(s.Definitions()))
	}
	got, err := s.Invoke(context.Background(), WriteFile, `{}`)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if !strings.Contains(got, "no tool called") {
		t.Fatalf("an unoffered tool was not refused: %q", got)
	}
}

func TestRunCommandReportsTheExitStatusTheLoopReadsBack(t *testing.T) {
	box := &fakeSandbox{out: Output{ExitCode: 1, Stdout: "FAIL\n"}}
	s := newSet(map[string]string{"a.go": "package p\n"}, nil, box)

	got, err := s.Invoke(context.Background(), RunCommand, `{}`)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if !strings.Contains(got, "exit 1") || !strings.Contains(got, "FAIL") {
		t.Fatalf("the check output is not reported: %q", got)
	}
	if box.last.command != "go test ./..." {
		t.Fatalf("the fixed check was not the command run: %q", box.last.command)
	}
	if _, ok := box.last.files["a.go"]; !ok {
		t.Fatal("the tree was not handed to the sandbox")
	}
}

// A red suite and an unreachable cluster want opposite responses, so only one of
// them comes back as an error.
func TestAnUnreachableSandboxIsAnErrorAndNotARefusal(t *testing.T) {
	box := &fakeSandbox{err: errors.New("no route to host")}
	s := newSet(map[string]string{}, nil, box)

	if _, err := s.Invoke(context.Background(), RunCommand, `{}`); err == nil {
		t.Fatal("an unreachable sandbox was handed back as a refusal")
	}
}

func TestAnEditWithAnInvalidCommitTypeIsRefusedWithTheList(t *testing.T) {
	s := newSet(map[string]string{}, nil, nil)

	got, err := s.Invoke(context.Background(), WriteFile,
		`{"path":"a.md","replace":"hi\n","summary":"x","type":"wip"}`)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if !strings.Contains(got, "feat") || !strings.Contains(got, "chore") {
		t.Fatalf("the refusal does not list the accepted types: %q", got)
	}
}

func TestMalformedArgumentsAreExplainedRatherThanEndingTheRun(t *testing.T) {
	s := newSet(map[string]string{}, nil, nil)

	got, err := s.Invoke(context.Background(), ReadFiles, `{"paths": [`)
	if err != nil {
		t.Fatalf("bad JSON ended the run: %v", err)
	}
	if !strings.Contains(got, "not valid JSON") {
		t.Fatalf("the refusal does not name the cause: %q", got)
	}
}

// Every tool's parameters must be a closed object: an open one lets a model
// invent a field, have it accepted, and never learn it did nothing.
func TestEveryToolClosesItsParameterObject(t *testing.T) {
	s := newSet(map[string]string{}, nil, nil)

	for _, tool := range s.Definitions() {
		if tool.Parameters["additionalProperties"] != false {
			t.Errorf("%s does not close additionalProperties", tool.Name)
		}
		if _, ok := tool.Parameters["required"]; !ok {
			t.Errorf("%s declares no required list", tool.Name)
		}
	}
}
