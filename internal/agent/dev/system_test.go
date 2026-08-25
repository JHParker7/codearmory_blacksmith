package dev

import (
	"strings"
	"testing"

	"github.com/code-armory-app/blacksmith/internal/config"
	"github.com/code-armory-app/blacksmith/internal/model"
)

// NO MODE MAY RUN WITHOUT A BRIEF.
//
// Options.SystemPrompt existed and nothing in the department ever set it, so
// every stage this loop serves worked from an EMPTY system message — no
// statement of the job, no rule that tests are the specification, no rule
// against editing them, no account of how the edit tool addresses code.
//
// Measured on a live run: a developer given one failing assertion spent fifteen
// turns guessing at rules it had never been told.
func TestEveryModeHasASystemPrompt(t *testing.T) {
	for _, m := range []Mode{ModeDevelop, ModeTest, ModeCoverage, ModeSpecMerge} {
		got := SystemPromptFor(m, 80)
		if strings.TrimSpace(got) == "" {
			t.Fatalf("mode %d has no brief at all", m)
		}
		if len(got) < 600 {
			t.Errorf("mode %d's brief is %d chars; too short to carry the job, the "+
				"edit vocabulary and the rules", m, len(got))
		}
	}
}

// AND THE AGENT DERIVES ONE RATHER THAN TRUSTING ITS CALLER. A field a caller
// must remember is a field some caller will forget — which is exactly what
// happened.
func TestAnAgentBuiltWithNoPromptStillHasOne(t *testing.T) {
	for _, m := range []Mode{ModeDevelop, ModeTest, ModeCoverage, ModeSpecMerge} {
		a := New(&gw{}, nil, &board{}, model.ClassLarge, config.Repo{},
			Options{Mode: m, Role: "x"}) // no SystemPrompt, as the department gave none
		if strings.TrimSpace(a.prompt) == "" {
			t.Fatalf("mode %d was built with an empty brief", m)
		}
	}
}

// AN EXPLICIT BRIEF STILL WINS, or the field becomes a lie.
func TestAnExplicitPromptIsKept(t *testing.T) {
	a := New(&gw{}, nil, &board{}, model.ClassLarge, config.Repo{},
		Options{Mode: ModeDevelop, Role: "x", SystemPrompt: "a brief of my own"})
	if a.prompt != "a brief of my own" {
		t.Fatalf("prompt = %q, want the one supplied", a.prompt)
	}
}

// THE BRIEF DESCRIBES THIS REPOSITORY'S TOOL. A brief inherited from a different
// editor would be confidently wrong about the one thing the model must get
// right — the original's describes line-number editing, and this vocabulary
// prefers addressing by text.
func TestTheBriefDescribesTheEditVocabularyWeActuallyHave(t *testing.T) {
	got := SystemPromptFor(ModeDevelop, 0)

	for _, want := range []string{"old_str", "decl", "start_line", "replace"} {
		if !strings.Contains(got, want) {
			t.Errorf("the brief never mentions %q, which the tool requires", want)
		}
	}
	// THE UNIQUENESS RULE IS THE FAILURE MODE OF TEXT ADDRESSING, and an agent
	// that does not know it widens nothing when refused.
	if !strings.Contains(got, "ONCE") && !strings.Contains(got, "unique") {
		t.Errorf("the brief does not say an anchor must match exactly once:\n%s", got)
	}
}

// EACH MODE CARRIES ITS OWN EDIT RULE, since that is the one line that differs
// and the whole pipeline rests on it.
func TestTheBriefCarriesTheModesOwnEditRule(t *testing.T) {
	for _, m := range []Mode{ModeDevelop, ModeTest, ModeCoverage, ModeSpecMerge} {
		got := SystemPromptFor(m, 0)
		if !strings.Contains(got, m.EditRule()) {
			t.Errorf("mode %d's brief does not state its edit rule:\n%s", m, got)
		}
	}
}

// THE DEVELOPER IS TOLD IT MAY NOT EDIT TESTS. This is the rule the pipeline
// exists to enforce, and the one the model most needs stated.
func TestTheDeveloperIsToldTheTestsAreNotItsToEdit(t *testing.T) {
	got := SystemPromptFor(ModeDevelop, 0)
	if !strings.Contains(got, "_test.go") {
		t.Fatalf("the developer's brief never names test files:\n%s", got)
	}
	if !strings.Contains(got, "specification") {
		t.Errorf("the brief does not say the tests ARE the specification:\n%s", got)
	}
}

// AND IT IS TOLD THE STAGE ENDS ITSELF. There is no finish tool — across
// thousands of recorded turns the agents called one once — so an agent waiting
// to be asked waits for something that never comes.
func TestTheBriefSaysTheStageEndsItself(t *testing.T) {
	got := SystemPromptFor(ModeDevelop, 0)
	if !strings.Contains(got, "no \"finish\" tool") && !strings.Contains(got, "ends itself") {
		t.Errorf("the brief does not say how the stage ends:\n%s", got)
	}
}

// THE COVERAGE TARGET REACHES THE AUTHOR, since "add tests" without a number is
// a job with no end.
func TestTheCoverageBriefCarriesItsTarget(t *testing.T) {
	if got := SystemPromptFor(ModeCoverage, 85); !strings.Contains(got, "85") {
		t.Errorf("the coverage brief does not state its target:\n%s", got)
	}
	// And says nothing about a target when there is none to state.
	if got := SystemPromptFor(ModeCoverage, 0); strings.Contains(got, "target is 0") {
		t.Errorf("the brief invented a zero target:\n%s", got)
	}
}

// EACH MODE IS TOLD A DIFFERENT JOB. The edit rule alone is not enough: a brief
// that opens "you are a software developer" and then forbids editing anything
// but tests is asking the model to reconcile two different jobs, and the opening
// sentence is the line it weighs most.
func TestEachModeIsGivenItsOwnJob(t *testing.T) {
	briefs := map[Mode]string{}
	for _, m := range []Mode{ModeDevelop, ModeTest, ModeCoverage, ModeSpecMerge} {
		briefs[m] = roleOf(m)
	}
	seen := map[string]Mode{}
	for m, b := range briefs {
		if other, dup := seen[b]; dup {
			t.Fatalf("modes %d and %d are given the same job description", other, m)
		}
		seen[b] = m
	}
	// And each names what it is, in its own words.
	for m, want := range map[Mode]string{
		ModeTest:      "specification author",
		ModeCoverage:  "coverage author",
		ModeSpecMerge: "reconciling",
		ModeDevelop:   "software developer",
	} {
		if !strings.Contains(briefs[m], want) {
			t.Errorf("mode %d never says it is a %q:\n%s", m, want, briefs[m])
		}
	}
}
