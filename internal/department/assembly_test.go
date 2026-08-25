package department

import (
	"strings"
	"testing"

	"github.com/code-armory-app/blacksmith/internal/agent/dev"
	"github.com/code-armory-app/blacksmith/internal/workflow"
)

// These test the DEPARTMENT'S OWN OUTPUT rather than a hand-built stage.
//
// That distinction is the whole reason they exist. Every stage this loop serves
// ran with an EMPTY system prompt for as long as it did, and the unit tests
// passed throughout — because each built its own agent through a helper that
// left the same field unset. They exercised the misconfiguration faithfully and
// agreed with it.
//
// A test that constructs the thing it is testing cannot notice that nothing
// constructs it that way in production. So these ask Assemble.

// devStages returns the assembled stages the dev loop serves, by role.
func devStages(t *testing.T, a Assembly) map[string]*dev.Agent {
	t.Helper()
	out := map[string]*dev.Agent{}
	for _, s := range a.Stages {
		if agent, ok := s.Handler.(*dev.Agent); ok {
			out[s.Role] = agent
		}
	}
	if len(out) == 0 {
		t.Fatal("the assembly contains no dev-loop stages at all")
	}
	return out
}

// NO ASSEMBLED STAGE MAY RUN WITHOUT A BRIEF.
//
// This is the test that was missing. It fails against the code as it shipped.
func TestEveryAssembledDevStageHasABrief(t *testing.T) {
	for role, agent := range devStages(t, assemble(t, baseConfig())) {
		brief := agent.Brief()
		if strings.TrimSpace(brief) == "" {
			t.Errorf("%s was assembled with an empty system prompt: it has no "+
				"statement of its job, no rule about test files and no account of "+
				"the edit tool", role)
			continue
		}
		// AND IT MUST BE THE RIGHT ONE. A brief is not a box to tick: the rule
		// that differs per stage is the rule the pipeline rests on.
		if !strings.Contains(brief, agent.ModeName().EditRule()) {
			t.Errorf("%s's brief does not carry its own edit rule", role)
		}
	}
}

// THE BRIEF DESCRIBES THE TOOL THIS STAGE IS ACTUALLY GIVEN. A brief that
// documents some other editor is worse than none — confidently wrong about the
// one thing the model has to get right.
func TestEveryAssembledBriefDescribesTheRealEditTool(t *testing.T) {
	for role, agent := range devStages(t, assemble(t, baseConfig())) {
		brief := agent.Brief()
		for _, want := range []string{"old_str", "decl", "replace"} {
			if !strings.Contains(brief, want) {
				t.Errorf("%s's brief never mentions %q, which its tool requires",
					role, want)
			}
		}
	}
}

// THE DEVELOPER IS THE ONE STAGE THAT MAY NOT EDIT TESTS, and it must be told
// so — this is the rule the whole pipeline exists to enforce.
func TestTheAssembledDeveloperIsToldNotToEditTests(t *testing.T) {
	agent, ok := devStages(t, assemble(t, baseConfig()))[workflow.RoleDev]
	if !ok {
		t.Fatal("no developer stage was assembled")
	}
	brief := agent.Brief()
	if !strings.Contains(brief, "_test.go") {
		t.Errorf("the assembled developer is never told which files it may not "+
			"edit:\n%s", brief)
	}
}

// EACH STAGE IS ASSEMBLED IN THE MODE THAT CARRIES ITS RULES.
//
// Modes are deliberately SHARED where the job is the same — the test author and
// the specification author both write tests, and maintenance is a developer
// variant — so uniqueness is the wrong invariant. What matters is that no stage
// is given another's rules, and above all that the developer is in the one mode
// that may not edit tests.
func TestEachStageIsAssembledInTheModeThatCarriesItsRules(t *testing.T) {
	stages := devStages(t, assemble(t, baseConfig()))

	want := map[string]dev.Mode{
		workflow.RoleDev:       dev.ModeDevelop,
		workflow.RoleMaintain:  dev.ModeDevelop,
		workflow.RoleTest:      dev.ModeTest,
		workflow.RoleSpec:      dev.ModeTest,
		workflow.RoleSpecMerge: dev.ModeSpecMerge,
	}
	for role, mode := range want {
		agent, ok := stages[role]
		if !ok {
			continue // not every role is served by every configuration
		}
		if agent.ModeName() != mode {
			t.Errorf("%s was assembled in mode %d, want %d — it is running under "+
				"another stage's rules", role, agent.ModeName(), mode)
		}
	}
}

// A STAGE IS EITHER ASSEMBLED OR REPORTED AS SKIPPED. Silently producing neither
// is how a pipeline dead-ends with nothing on the board to explain it.
//
// Checked against the table THIS configuration builds, not a maximal one: a
// stage the configuration switches off is absent by choice, and holding the
// assembly to roles it was never asked for tests the test's own assumptions.
func TestEveryRoleIsEitherAssembledOrExplained(t *testing.T) {
	cfg := baseConfig()
	a := assemble(t, cfg)

	got := map[string]bool{}
	for _, s := range a.Stages {
		got[s.Role] = true
	}
	for _, s := range a.Skipped {
		if got[s.Role] {
			t.Errorf("%s is both assembled and skipped", s.Role)
		}
		if strings.TrimSpace(s.Reason) == "" {
			t.Errorf("%s was skipped with no reason given", s.Role)
		}
		got[s.Role] = true
	}

	for _, role := range Table(cfg).Roles() {
		if !got[role] {
			t.Errorf("%s was neither assembled nor reported as skipped — it simply "+
				"does not happen, and nothing says so", role)
		}
	}
}
