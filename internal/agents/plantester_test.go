package agents

import (
	"strings"
	"testing"

	"github.com/code-armory-app/blacksmith/internal/tools"
)

// THE STAGE RUN 3 PROVED NECESSARY: the plan named its cases one by one and the
// developer skipped all of them and passed. The test author makes the tests
// exist before the developer starts, which changes what "done" means for it.

// The test author writes tests AND the stubs they compile against — both are
// .go, so its guard must allow both.
func TestTheTestAuthorMayWriteTestsAndStubs(t *testing.T) {
	a := maker().PlanTester(nil)

	for _, p := range []string{"store_test.go", "store.go", "go.mod"} {
		if err := a.opts.Guard(p); err != nil {
			t.Errorf("the test author may not write %s: %v", p, err)
		}
	}
	if err := a.opts.Guard("PLAN.md"); err == nil {
		t.Error("the test author may rewrite the plan it is meant to follow")
	}
}

// Its gate points the expected-red way: green when the tree builds and the
// tests FAIL — with go vet as the piece that typechecks the test files, so a
// suite that does not compile cannot masquerade as the expected red.
func TestTheTestAuthorsCheckExpectsRed(t *testing.T) {
	a := maker().PlanTester(nil)

	for _, want := range []string{"! go test", "go vet", "go build"} {
		if !strings.Contains(a.Check(), want) {
			t.Errorf("the test author's check is missing %q: %q", want, a.Check())
		}
	}
	// And the operator's green-check override must not re-invert it.
	overridden := Creator{Gateway: &fakeGateway{}, Check: "make verify"}.PlanTester(nil)
	if overridden.Check() != a.Check() {
		t.Fatalf("the override replaced the expected-red check: %q", overridden.Check())
	}
}

// The order is the arrangement: plan, then red tests, then green.
func TestThePlanArmRunsPlanThenTestsThenDev(t *testing.T) {
	got := PlanStages()
	want := []string{StagePlanArchitect, StagePlanTest, StagePlanDev}
	if len(got) != len(want) {
		t.Fatalf("PlanStages() = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("PlanStages()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// The developer is told the tests exist and that they are not its to change —
// and both developers, plan-arm and pipeline, now hold the same locked-suite
// rule.
func TestThePlanDevIsToldTheTestsExistAndAreLocked(t *testing.T) {
	dev := maker().PlanFollowingDev(nil)

	if !strings.Contains(dev.opts.Prompt, "FAILING TESTS") {
		t.Error("the developer is not told the tests already exist")
	}
	if !strings.Contains(dev.opts.Prompt, "NOT YOURS TO CHANGE") {
		t.Error("the developer is not told the tests are locked")
	}
	for _, a := range []*Agent{dev, maker().Dev(nil)} {
		if err := a.opts.Guard("store_test.go"); err == nil {
			t.Errorf("%s may write a test file", a.Name())
		}
	}
}

// The test author is asked for stubs that make tests FAIL, not error — the
// distinction the whole stage exists to hold.
func TestTheTestAuthorIsAskedForCompilingStubs(t *testing.T) {
	p := maker().PlanTester(nil).opts.Prompt

	for _, want := range []string{"COMPILE", "zero-value", "FAIL"} {
		if !strings.Contains(p, want) {
			t.Errorf("the test author's prompt is missing %q", want)
		}
	}
}

// Every plan stage builds by name through the driver's dispatcher.
func TestThePlanTesterBuildsByItsStageName(t *testing.T) {
	a := maker().PlanTester(map[string]string{})
	if a.Name() != StagePlanTest {
		t.Fatalf("PlanTester built a stage called %q", a.Name())
	}
	if !offers(a, tools.RunCommand) {
		t.Fatal("the test author has a check and no way to run it")
	}
}
