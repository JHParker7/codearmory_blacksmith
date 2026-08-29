package agents

import (
	"strings"
	"testing"

	"github.com/code-armory-app/blacksmith/internal/tools"
)

// THE REASON THIS PAIR EXISTS. The pipeline's developer may not write tests,
// because a specification author owns them. There is no specification author in
// the two-stage arrangement, so a developer held to that guard could not write
// the tests the architect just planned and the experiment would measure nothing.
func TestThePlanFollowingDeveloperCanWriteTests(t *testing.T) {
	c := maker()

	if err := c.PlanFollowingDev(nil).opts.Guard("store_test.go"); err != nil {
		t.Fatalf("the two-stage developer may not write tests: %v", err)
	}
	// And the pipeline's developer still may not, which is the rule this must
	// not have quietly relaxed.
	if err := c.Dev(nil).opts.Guard("store_test.go"); err == nil {
		t.Fatal("the pipeline developer may now write tests")
	}
}

// The architect's whole contribution is a plan the developer can read. One that
// can write code writes the code instead.
func TestThePlanningArchitectWritesOnlyMarkdown(t *testing.T) {
	a := maker().PlanningArchitect(nil)

	if err := a.opts.Guard("main.go"); err == nil {
		t.Fatal("the planning architect may write Go")
	}
	if err := a.opts.Guard("PLAN.md"); err != nil {
		t.Fatalf("the planning architect may not write markdown: %v", err)
	}
	if a.Check() != "" {
		t.Fatalf("the planning architect gates on a command: %q", a.Check())
	}
}

// The hypothesis being tested is that naming the edge cases up front is what the
// baseline was missing. If the prompt stops asking for them the experiment
// silently becomes a different one.
func TestThePlanningArchitectIsAskedForEdgeCases(t *testing.T) {
	p := maker().PlanningArchitect(nil).opts.Prompt

	for _, want := range []string{"EDGE CASES", "status code", "tests"} {
		if !strings.Contains(p, want) {
			t.Errorf("the planning architect is not asked about %q", want)
		}
	}
}

// The developer has to be told the plan is already on disk, or it will invent
// its own and the architect's turn was wasted.
func TestThePlanFollowingDeveloperIsPointedAtThePlan(t *testing.T) {
	p := maker().PlanFollowingDev(nil).opts.Prompt

	if !strings.Contains(p, "markdown") {
		t.Error("the developer is not told the plan is in markdown files")
	}
	if !strings.Contains(p, "READ THEM FIRST") {
		t.Error("the developer is not told to read the plan before building")
	}
}

// The comparison against the single-agent baseline is only meaningful if both
// arrangements are judged by the same command.
func TestTheTwoStageDeveloperGatesOnTheSameCheckAsTheBaseline(t *testing.T) {
	c := maker()

	if got, want := c.PlanFollowingDev(nil).Check(), c.Baseline(nil).Check(); got != want {
		t.Fatalf("the two-stage developer checks %q, the baseline checks %q — the runs are not "+
			"comparable", got, want)
	}
}

// Both stages must be buildable by name, or -plan cannot run them.
func TestThePlanStagesBuildByName(t *testing.T) {
	if got := PlanStages(); len(got) != 2 {
		t.Fatalf("PlanStages() = %v, want two stages", got)
	}
	for _, name := range PlanStages() {
		if name == "" {
			t.Fatal("a plan stage has no name")
		}
	}
	// The names must not collide with the pipeline's, or a result table cannot
	// say which arrangement produced a row.
	for _, p := range Stages() {
		for _, q := range PlanStages() {
			if p == q {
				t.Errorf("%q names both a pipeline stage and a plan stage", p)
			}
		}
	}
}

// A stage that cannot read cannot follow a plan someone else wrote.
func TestBothPlanStagesCanRead(t *testing.T) {
	c := maker()
	for _, a := range []*Agent{c.PlanningArchitect(nil), c.PlanFollowingDev(nil)} {
		for _, name := range []string{tools.ReadFiles, tools.ListFiles} {
			if !offers(a, name) {
				t.Errorf("%s cannot call %s", a.Name(), name)
			}
		}
	}
}
