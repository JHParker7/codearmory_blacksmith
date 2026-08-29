package agents

import (
	"strings"
	"testing"

	"github.com/code-armory-app/blacksmith/internal/model"
	"github.com/code-armory-app/blacksmith/internal/tools"
)

// maker builds stages with a gateway that is never called. Every test here is
// about how a stage is WIRED, which is settled at construction.
func maker() Creator {
	return Creator{Gateway: &fakeGateway{}, Sandbox: fakeSandbox{}}
}

// built returns every stage in the pipeline, constructed.
func built(t *testing.T) []*Agent {
	t.Helper()
	c := maker()
	var out []*Agent
	for _, name := range Stages() {
		a, err := c.Stage(name, map[string]string{})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		out = append(out, a)
	}
	return out
}

// offers reports whether a built stage may call a tool.
func offers(a *Agent, name string) bool {
	for _, tool := range a.Offers() {
		if tool.Name == name {
			return true
		}
	}
	return false
}

// writes reports whether a stage's guard permits a path.
func writes(a *Agent, path string) bool {
	return a.opts.Guard(path) == nil
}

func TestEveryStageCanBeBuiltByName(t *testing.T) {
	c := maker()
	for _, name := range Stages() {
		a, err := c.Stage(name, map[string]string{})
		if err != nil {
			t.Fatalf("%s could not be built: %v", name, err)
		}
		if a.Name() != name {
			t.Errorf("%s built a stage called %q", name, a.Name())
		}
	}
}

// A name that is not a stage must be an error naming the ones that are. The
// alternative is a silently skipped stage and a pipeline reporting success
// having never run the developer.
func TestAnUnknownStageIsRefusedWithTheList(t *testing.T) {
	_, err := maker().Stage("architekt", map[string]string{})
	if err == nil {
		t.Fatal("an unknown stage was built")
	}
	for _, name := range Stages() {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the refusal does not name %q: %v", name, err)
		}
	}
}

// THE LOAD-BEARING RULE. A developer that can edit the specification can always
// go green without making the code work, and the run reports success. Asserted
// on the built stage rather than on a prompt because a prompt did not hold it.
func TestTheDeveloperCannotWriteATestFile(t *testing.T) {
	c := maker()
	for _, a := range []*Agent{c.Dev(nil), c.Integrator(nil)} {
		if writes(a, "store_test.go") {
			t.Errorf("%s may write a test file", a.Name())
		}
		if !writes(a, "store.go") {
			t.Errorf("%s may not write ordinary source", a.Name())
		}
	}
}

// The specification author must be able to write the tests, which is exactly
// what the developer may not do. If both were guarded the same way the pipeline
// would have no way to produce a specification at all.
func TestTheSpecificationAuthorCanWriteTests(t *testing.T) {
	if !writes(maker().Spec(nil), "store_test.go") {
		t.Fatal("the spec author may not write a test")
	}
}

// A specification that also implements its subject cannot fail, so it never asks
// the developer for anything. Its check is what catches that.
func TestEveryCodeWritingStageHasACheckAndCanRunIt(t *testing.T) {
	c := maker()
	for _, a := range []*Agent{c.Spec(nil), c.Dev(nil), c.Integrator(nil)} {
		if a.Check() == "" {
			t.Errorf("%s writes code and has no check", a.Name())
		}
		if !offers(a, tools.RunCommand) {
			t.Errorf("%s has a check and no way to run it", a.Name())
		}
	}
	for _, a := range []*Agent{c.Architect(nil), c.PM(nil), c.Sec(nil)} {
		if a.Check() != "" {
			t.Errorf("%s produces a document and should not gate on a command", a.Name())
		}
	}
}

// A reviewer that can edit stops reviewing and starts rewriting, and its report
// then describes code that no longer exists. Guarded twice on purpose: the
// missing tool is what the model sees, the guard is what happens if someone adds
// the tool back.
func TestTheReviewerCanNeitherWriteNorRun(t *testing.T) {
	a := maker().Sec(nil)

	if writes(a, "a.go") {
		t.Fatal("the reviewer may write")
	}
	for _, name := range []string{tools.WriteFile, tools.UndoEdit, tools.RunCommand} {
		if offers(a, name) {
			t.Errorf("the reviewer is offered %s", name)
		}
	}
}

// The architect's output is read by every stage after it. One that can write
// code writes the code instead of the design, and the stages meant to read a
// design read half an implementation.
func TestTheArchitectWritesDocumentsAndNotCode(t *testing.T) {
	a := maker().Architect(nil)

	if writes(a, "main.go") {
		t.Fatal("the architect may write Go")
	}
	if !writes(a, "docs/design.md") {
		t.Fatal("the architect may not write markdown")
	}
}

// A stage that cannot look around cannot do anything useful, and leaving a read
// tool out of one constructor is a silent way to produce exactly that.
func TestEveryStageCanReadTheRepository(t *testing.T) {
	for _, a := range built(t) {
		for _, name := range []string{tools.ReadFiles, tools.ListFiles, tools.SearchFiles} {
			if !offers(a, name) {
				t.Errorf("%s cannot call %s", a.Name(), name)
			}
		}
	}
}

func TestEveryStageIsUsable(t *testing.T) {
	seen := map[string]bool{}
	for _, a := range built(t) {
		if a.Name() == "" {
			t.Fatal("a stage has no name")
		}
		if seen[a.Name()] {
			t.Fatalf("two stages are called %q; -roles could not tell them apart", a.Name())
		}
		seen[a.Name()] = true

		if a.opts.MaxIterations <= 0 {
			t.Errorf("%s has no budget and would never take a turn", a.Name())
		}
		if a.opts.MaxTokens <= 0 {
			t.Errorf("%s has no reply budget", a.Name())
		}
		if strings.TrimSpace(a.opts.Prompt) == "" {
			t.Errorf("%s has no instruction", a.Name())
		}
		if a.Class() == "" {
			t.Errorf("%s asks for no serving class", a.Name())
		}
		if a.opts.Guard == nil {
			t.Errorf("%s has no write guard", a.Name())
		}
	}
}

// A stage that asked for the none class would be one that calls no model, and
// every stage here calls one.
func TestEveryStageAsksForAServingClass(t *testing.T) {
	for _, a := range built(t) {
		if a.Class() == model.ClassNone {
			t.Errorf("%s asks for the none class but has a prompt", a.Name())
		}
	}
}

// The order is the pipeline: a stage's input is the tree the one before it left
// behind. Specifying before implementing is the whole design.
func TestTheSpecificationIsWrittenBeforeTheImplementation(t *testing.T) {
	spec, dev := -1, -1
	for i, name := range Stages() {
		switch name {
		case StageSpec:
			spec = i
		case StageDev:
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
