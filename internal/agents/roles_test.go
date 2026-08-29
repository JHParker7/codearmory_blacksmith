package agents

import (
	"strings"
	"testing"

	"github.com/code-armory-app/blacksmith/internal/tools"
)

// THE LOAD-BEARING RULE. A developer that can edit the specification can always
// go green without making the code work, and the run reports success. Asserted
// on the role rather than on a prompt because a prompt did not hold this line.
func TestTheDeveloperCannotWriteATestFile(t *testing.T) {
	for _, r := range []Role{Dev(), Integrator()} {
		if err := r.Guard("store_test.go"); err == nil {
			t.Errorf("%s may write a test file", r.Name)
		}
		if err := r.Guard("store.go"); err != nil {
			t.Errorf("%s may not write ordinary source: %v", r.Name, err)
		}
	}
}

// The specification author must be able to write the tests, which is exactly
// what the developer may not do. If both were guarded the same way the pipeline
// would have no way to produce a specification at all.
func TestTheSpecificationAuthorCanWriteTests(t *testing.T) {
	if err := Spec().Guard("store_test.go"); err != nil {
		t.Fatalf("the spec author may not write a test: %v", err)
	}
}

// A specification that also implements its subject cannot fail, so it never asks
// the developer for anything. Its check is what catches that, and both halves
// matter: a suite that does not compile is a broken specification, not the
// expected red.
func TestEveryCodeWritingStageHasACheck(t *testing.T) {
	for _, r := range []Role{Spec(), Dev(), Integrator()} {
		if r.Check == "" {
			t.Errorf("%s writes code and has no check", r.Name)
		}
	}
	for _, r := range []Role{Architect(), PM(), Sec()} {
		if r.Check != "" {
			t.Errorf("%s produces a document and should not gate on a command", r.Name)
		}
	}
}

// A reviewer that can edit stops reviewing and starts rewriting, and its report
// then describes code that no longer exists. Guarded twice, on purpose: the
// missing tool is what the model sees, and the guard is what happens if someone
// adds the tool back.
func TestTheReviewerCanNeitherWriteNorRun(t *testing.T) {
	r := Sec()
	if err := r.Guard("a.go"); err == nil {
		t.Fatal("the reviewer may write")
	}
	for _, name := range r.ToolNames {
		if name == tools.WriteFile || name == tools.RunCommand || name == tools.UndoEdit {
			t.Errorf("the reviewer is offered %s", name)
		}
	}
}

// The architect's output is read by every stage after it. One that can write
// code writes the code instead of the design, and the stages meant to read a
// design read half an implementation.
func TestTheArchitectWritesDocumentsAndNotCode(t *testing.T) {
	if err := Architect().Guard("main.go"); err == nil {
		t.Fatal("the architect may write Go")
	}
	if err := Architect().Guard("docs/design.md"); err != nil {
		t.Fatalf("the architect may not write markdown: %v", err)
	}
}

// A stage that cannot look around cannot do anything useful, and leaving a read
// tool out of one role's list is a silent way to produce exactly that.
func TestEveryRoleCanReadTheRepository(t *testing.T) {
	for _, r := range Pipeline() {
		var canRead, canList bool
		for _, name := range r.ToolNames {
			switch name {
			case tools.ReadFiles:
				canRead = true
			case tools.ListFiles:
				canList = true
			}
		}
		if !canRead || !canList {
			t.Errorf("%s cannot read (%v) or list (%v) the repository", r.Name, canRead, canList)
		}
	}
}

// A stage whose check it cannot run can only ever end by running out of budget.
func TestAStageWithACheckIsOfferedTheToolThatRunsIt(t *testing.T) {
	for _, r := range Pipeline() {
		if r.Check == "" {
			continue
		}
		var offered bool
		for _, name := range r.ToolNames {
			if name == tools.RunCommand {
				offered = true
			}
		}
		if !offered {
			t.Errorf("%s has a check and no way to run it", r.Name)
		}
	}
}

func TestEveryRoleIsUsable(t *testing.T) {
	seen := map[string]bool{}
	for _, r := range Pipeline() {
		if r.Name == "" {
			t.Fatal("a role has no name")
		}
		if seen[r.Name] {
			t.Fatalf("two roles are called %q; -roles could not tell them apart", r.Name)
		}
		seen[r.Name] = true

		if r.MaxIterations <= 0 {
			t.Errorf("%s has no budget and would never take a turn", r.Name)
		}
		if r.MaxTokens <= 0 {
			t.Errorf("%s has no reply budget", r.Name)
		}
		if strings.TrimSpace(r.System) == "" {
			t.Errorf("%s has no instruction", r.Name)
		}
		if r.Guard == nil {
			t.Errorf("%s has no write guard, so it may write anything", r.Name)
		}
	}
}

// The order is the pipeline: a stage's input is the tree the one before it left
// behind. Specifying before implementing is the whole design.
func TestTheSpecificationIsWrittenBeforeTheImplementation(t *testing.T) {
	var spec, dev int = -1, -1
	for i, r := range Pipeline() {
		switch r.Name {
		case "spec":
			spec = i
		case "dev":
			dev = i
		}
	}
	if spec < 0 || dev < 0 {
		t.Fatal("the pipeline is missing the spec or dev stage")
	}
	if spec > dev {
		t.Fatal("the developer runs before the specification author")
	}
}
