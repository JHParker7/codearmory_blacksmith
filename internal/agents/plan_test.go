package agents

import (
	"strings"
	"testing"

	"github.com/code-armory-app/blacksmith/internal/tools"
)

// THE RULE CAME BACK WITH THE TEST AUTHOR. In the two-stage arrangement the
// developer had to write the tests because nothing else did; now PlanTester
// owns them, and a developer that can edit the tests can always go green
// without making the code work — the department's rule, restored the moment
// its precondition existed.
func TestThePlanFollowingDeveloperMayNotTouchTests(t *testing.T) {
	c := maker()

	if err := c.PlanFollowingDev(nil).opts.Guard("store_test.go"); err == nil {
		t.Fatal("the plan-arm developer may edit the tests it is judged by")
	}
	if err := c.PlanFollowingDev(nil).opts.Guard("store.go"); err != nil {
		t.Fatalf("the plan-arm developer may not write implementation: %v", err)
	}
}

// The compensating freedom: with the suite locked against it, the developer may
// rewrite implementation files WHOLE — a dropped function fails the next
// auto-check by name, which is a better refusal than the write gate's.
func TestThePlanFollowingDeveloperMayRewriteFilesWhole(t *testing.T) {
	a := maker().PlanFollowingDev(map[string]string{
		"store.go": "package main\n\nfunc NewStore() int { return 0 }\n\nfunc helper() {}\n",
	})

	// This rewrite DROPS helper — the strict rule would refuse it; here the
	// tests are the guard and the write must land.
	got, err := a.tools.Invoke(t.Context(), tools.WriteFile,
		`{"path":"store.go","replace":"package main\n\nfunc NewStore() int { return 42 }\n","summary":"implement","type":"feat"}`)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if strings.Contains(got, "Error") {
		t.Fatalf("a whole-file rewrite was refused for the locked-suite developer: %q", got)
	}
	// The baseline keeps the strict rule: its tests are its own to write, so
	// nothing else would catch the drop.
	b := maker().Baseline(map[string]string{
		"store.go": "package main\n\nfunc NewStore() int { return 0 }\n\nfunc helper() {}\n",
	})
	got, err = b.tools.Invoke(t.Context(), tools.WriteFile,
		`{"path":"store.go","replace":"package main\n\nfunc NewStore() int { return 42 }\n","summary":"implement","type":"feat"}`)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if !strings.Contains(got, "DROPS") {
		t.Fatalf("the baseline lost its drop protection: %q", got)
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
	if got := PlanStages(); len(got) != 3 {
		t.Fatalf("PlanStages() = %v, want three stages", got)
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
