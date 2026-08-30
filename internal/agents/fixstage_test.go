package agents

import (
	"testing"

	"github.com/code-armory-app/blacksmith/internal/tools"
)

// The auto-mode fix stage is a developer that may NOT edit tests — a fix that
// weakens the test proving the bug is not a fix — and MAY rewrite whole files,
// the same locked-suite bargain as the plan developer.
func TestTheFixStageCannotEditTestsButCanRewrite(t *testing.T) {
	a := maker().FixFinding(nil)

	if err := a.opts.Guard("store_test.go"); err == nil {
		t.Fatal("the fix stage may edit tests it must not weaken")
	}
	if err := a.opts.Guard("store.go"); err != nil {
		t.Fatalf("the fix stage may not write implementation: %v", err)
	}
	if a.Check() == "" || !offers(a, tools.RunCommand) {
		t.Fatal("the fix stage has no check to prove the fix keeps the tests green")
	}
	if a.opts.AttemptTimeout == 0 {
		t.Fatal("the fix stage is unbounded; a stuck fix would grind forever")
	}
}
