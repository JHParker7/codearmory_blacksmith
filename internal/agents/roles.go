package agents

import (
	"fmt"
	"strings"

	"github.com/code-armory-app/blacksmith/internal/model"
	"github.com/code-armory-app/blacksmith/internal/tools"
)

// The stage names. Constants because they are what -roles matches against and
// what appears in every log line, and a typo in a string literal on one side of
// that comparison is a stage nobody can address.
const (
	StageArchitect  = "architect"
	StagePM         = "pm"
	StageSpec       = "spec"
	StageDev        = "dev"
	StageSec        = "sec"
	StageIntegrator = "integrator"

	// The two-stage experiment's stages. Named apart from the pipeline's so a
	// log line, a -roles argument and a result table all say which arrangement
	// produced them — the whole point is comparing the two.
	StagePlanArchitect = "plan-architect"
	StagePlanDev       = "plan-dev"
)

// PlanStages are the two-stage experiment, in order.
func PlanStages() []string { return []string{StagePlanArchitect, StagePlanDev} }

// Stages are the roles in the order they run.
//
// THE ORDER IS THE PIPELINE. There is no scheduler in this rebuild: a stage's
// input is the tree the previous stage left behind, which is the simplest thing
// that can work and is enough to run the whole sequence end to end. The
// column-based dispatch in internal/dispatch is what this grows back into once
// several hosts have to share a board.
func Stages() []string {
	return []string{StageArchitect, StagePM, StageSpec, StageDev, StageSec, StageIntegrator}
}

// Stage builds one role by name.
//
// The dispatcher exists so a caller can work from a list of names — a -roles
// flag, a config file — without a switch of its own. A name that is not a stage
// is an error naming the ones that are, because the alternative is a silently
// skipped stage and a pipeline that reports success having never run the
// developer.
func (c Creator) Stage(name string, files map[string]string) (*Agent, error) {
	switch name {
	case StageArchitect:
		return c.Architect(files), nil
	case StagePM:
		return c.PM(files), nil
	case StageSpec:
		return c.Spec(files), nil
	case StageDev:
		return c.Dev(files), nil
	case StageSec:
		return c.Sec(files), nil
	case StageIntegrator:
		return c.Integrator(files), nil
	}
	return nil, fmt.Errorf("there is no stage called %q; the stages are %s",
		name, strings.Join(Stages(), ", "))
}

// Baseline is ONE unrestricted agent doing the whole job alone.
//
// NOT PART OF THE PIPELINE, and deliberately not in Stages(). It exists to be
// measured against: the pipeline's cost is six model stages, a specification
// nobody may edit and a reviewer that cannot fix what it finds, and the only
// way to know whether that cost buys anything is to run the same request
// through an agent with none of it and compare the output.
//
// So it gets what the pipeline withholds — AllowAll, every tool, no separate
// author for the tests — and the comparison is only fair if it keeps them. A
// baseline quietly given the developer's guard would be measuring a different
// question and would flatter the pipeline.
func (c Creator) Baseline(files map[string]string) *Agent {
	return c.New(files, Options{
		Name:  "baseline",
		Class: model.ClassLarge,
		Prompt: "You are a Go developer. Build what the user asks for in this repository, and " +
			"write tests for it. Put go.mod and the packages at the repository root. Run the " +
			"check as you go and fix what it reports — do not finish on a tree that does not " +
			"compile or whose tests fail.",
		Guard:         tools.AllowAll,
		Tools:         writing(tools.RunCommand),
		Check:         rootCheck,
		MaxIterations: 150,
		Temperature:   0.2,
		MaxTokens:     12000,
	})
}

// PlanningArchitect and PlanFollowingDev are the TWO-STAGE EXPERIMENT: plan the
// work and the tests, then build from that plan.
//
// A separate pair rather than the pipeline's Architect and Dev, for one reason
// that is not cosmetic: the pipeline's developer MAY NOT WRITE TESTS, because a
// specification author owns them. There is no specification author here, so a
// developer held to that guard could not write the tests the architect just
// planned and the experiment would measure nothing. Reusing Dev() and quietly
// dropping its guard would be worse — the guard is the pipeline's load-bearing
// rule and it should not be something a caller can turn off.
//
// The architect plans the TESTS as well as the code, and is pushed at edge cases
// specifically. That is the whole hypothesis: the baseline's failure was not
// that it wrote bad code but that it did not think about what could go wrong
// before it started, and what it did not think of, it did not test.
func (c Creator) PlanningArchitect(files map[string]string) *Agent {
	return c.New(files, Options{
		Name:  StagePlanArchitect,
		Class: model.ClassLarge,
		Prompt: "You are a systems architect. Read the request and write markdown files that plan " +
			"BOTH the implementation and the tests. A developer will read these files as its plan " +
			"and write the code and the tests from them, so anything you leave out is something " +
			"nobody builds and nobody checks.\n\n" +
			"Plan the implementation: name the packages, the types, their fields and the " +
			"functions, concretely enough that someone can write a test against one without " +
			"asking you a question. Put the Go module at the repository ROOT, not in a " +
			"subdirectory.\n\n" +
			"Then plan the tests, and spend most of your effort on the EDGE CASES. List them " +
			"case by case: what happens on an empty or missing field, on a value outside the " +
			"allowed set, on an id that does not exist, on a malformed body, on a boundary, on " +
			"the same operation done twice. For anything served over HTTP, say which cases must " +
			"be answered with which status code, and include a case that proves the routes " +
			"actually match a request. A case you do not name is a case nobody tests.\n\n" +
			"Do not write source code. Describe it.",
		Guard:         tools.OnlyExt(".md"),
		Tools:         writing(),
		MaxIterations: 40,
		Temperature:   0.3,
		MaxTokens:     12000,
	})
}

// PlanFollowingDev builds what PlanningArchitect planned, tests included.
func (c Creator) PlanFollowingDev(files map[string]string) *Agent {
	return c.New(files, Options{
		Name:  StagePlanDev,
		Class: model.ClassLarge,
		Prompt: "You are a Go developer. The markdown files already in this repository are your " +
			"plan: an architect wrote them for you. READ THEM FIRST, then build what they " +
			"describe.\n\n" +
			"Write the tests the plan names, including every edge case it lists — those cases " +
			"are the part of the plan most easily skipped and the part most worth having. Put " +
			"the Go module at the repository root. Run the check as you go and fix what it " +
			"reports; do not finish on a tree that does not compile or whose tests fail.\n\n" +
			"If the plan is wrong or cannot be built as written, say so plainly and build the " +
			"nearest thing that works, rather than following it off a cliff.",
		// NO NoTests GUARD. This developer writes the tests, because nothing else
		// in this two-stage arrangement does.
		Guard:         tools.OnlyExt(".go"),
		Tools:         writing(tools.RunCommand),
		Check:         rootCheck,
		MaxIterations: 150,
		Temperature:   0.2,
		MaxTokens:     12000,
	})
}

// Reading tools every stage gets. Named once because a stage that cannot look
// around cannot do anything useful, and leaving one out of a list is a silent
// way to produce exactly that.
var readOnly = []string{tools.ReadFiles, tools.ListFiles, tools.SearchFiles}

// writing is the reading tools plus the editing ones, and whatever else a stage
// needs on top.
func writing(extra ...string) []string {
	out := append([]string{}, readOnly...)
	out = append(out, tools.WriteFile, tools.UndoEdit)
	return append(out, extra...)
}

// The check a stage that writes Go gates on.
//
// A DEFAULT, NOT A POLICY. It assumes a tree whose module lives in src/, which
// is what the architect is told to lay down; an operator whose tree is arranged
// otherwise sets Creator.Check and this is never consulted.
const goCheck = "cd src && go build ./... && go test ./..."

// rootCheck is the same check for a module at the repository ROOT.
//
// Used by the arrangements whose instructions put it there — the baseline and
// the two-stage experiment — and it must stay the SAME STRING for both, because
// the whole point of running them is comparing their results. Two arrangements
// judged by different commands are not comparable, and the difference is easy to
// introduce and invisible afterwards.
const rootCheck = "go build ./... && go test ./..."

// Architect decides what the system looks like and writes it down.
//
// IT THINKS, and this is the stage where that pays. It is deciding a shape
// rather than transcribing a decision already made — measured on the python
// rebuild against qwen3.8, reasoning cost 358 tokens and 7s versus 45 tokens and
// 1s on a transcription job for the same answer. That is why the effort is a
// property of the serving class a stage asks for and not a global.
//
// MARKDOWN ONLY. Its output is read by every stage after it; an architect that
// can also write the code writes the code instead of the design, and then the
// stages meant to read a design read half an implementation.
func (c Creator) Architect(files map[string]string) *Agent {
	return c.New(files, Options{
		Name:  StageArchitect,
		Class: model.ClassLarge,
		Prompt: "You are a systems architect. Write or update markdown files describing the " +
			"software architecture that accomplishes what the user asked for. These files are " +
			"the only thing the later stages get: a product manager will break them into work, a " +
			"specification author will write failing tests from them, and a developer will " +
			"implement against those tests. Put the Go module in a directory called src. Name " +
			"the packages, the types and the boundaries between them concretely enough that " +
			"someone can write a test against one without asking you a question. Do not write " +
			"source code — describe it.",
		Guard:         tools.OnlyExt(".md"),
		Tools:         writing(),
		MaxIterations: 20,
		Temperature:   0.3,
		MaxTokens:     8000,
	})
}

// PM breaks the architecture into work that can be done independently.
//
// NO GRAMMAR AND NO CHECK. Constraining its breakdown to a schema was tried in
// this department and measured worse: given one, the model folded dependencies
// and acceptance criteria into a single string and returned one degenerate
// subtask, where the same prompt unconstrained produced four tasks and six
// sections. Here the shape stays the prompt's job.
func (c Creator) PM(files map[string]string) *Agent {
	return c.New(files, Options{
		Name:  StagePM,
		Class: model.ClassLarge,
		Prompt: "You are a product manager. Read the architecture documents and break the work " +
			"into tasks that can be done independently. For each task write what it covers, what " +
			"it depends on, and how anyone would know it is finished. Write them to markdown. A " +
			"task that cannot be started until another is done must say so — the stages after " +
			"you work from what you write, and an unstated dependency becomes a developer " +
			"waiting on a package nobody built.",
		Guard:         tools.OnlyExt(".md"),
		Tools:         writing(),
		MaxIterations: 20,
		Temperature:   0.3,
		MaxTokens:     8000,
	})
}

// Spec writes the failing tests. The stage the whole pipeline is arranged
// around.
//
// ITS CHECK IS THAT THE TREE BUILDS AND THE TESTS FAIL, and both halves were
// learned separately. A suite that does not compile is not the expected red — it
// is a broken specification, and a developer sent at it spends its budget on the
// author's syntax errors. A suite that passes against an empty implementation is
// not a specification at all but a tautology, and nothing downstream ever
// notices.
//
// It may write tests and the declarations they need in order to compile, and
// nothing else: a specification that implements its own subject cannot fail, so
// it never asks the developer for anything.
func (c Creator) Spec(files map[string]string) *Agent {
	return c.New(files, Options{
		Name:  StageSpec,
		Class: model.ClassLarge,
		Prompt: "You are a specification author and you work test-first. Read the architecture " +
			"and the task, then write Go tests that FAIL against the code as it stands, plus the " +
			"minimum declarations — types, function signatures with empty or panicking bodies — " +
			"that the tests need in order to COMPILE. The tests are the contract: a developer " +
			"who cannot read your test cannot build the right thing, and a developer whose tests " +
			"pass before writing anything has been told nothing. Never implement the behaviour " +
			"you are specifying. You are done when the tree compiles and the tests fail for the " +
			"reason you intended.",
		Guard:         tools.OnlyExt(".go"),
		Tools:         writing(tools.RunCommand),
		Check:         goCheck,
		MaxIterations: 60,
		Temperature:   0.2,
		MaxTokens:     12000,
	})
}

// Dev makes the tests pass, and may not change them.
//
// THE GUARD IS THE POINT. A developer that can edit the specification can always
// go green without making the code work, and that failure is invisible — the run
// reports success. Every attempt to hold this line with a sentence in the prompt
// failed; the refusal at the write holds it.
//
// Its budget is the largest of any stage because it is the only one whose work
// is bounded by a compiler rather than by its own judgement about being
// finished.
func (c Creator) Dev(files map[string]string) *Agent {
	return c.New(files, Options{
		Name:  StageDev,
		Class: model.ClassLarge,
		Prompt: "You are a Go developer. Failing tests describe what the code must do; make them " +
			"pass. You may not edit test files — they are the specification and they are not " +
			"yours to change. Read the tests first, then write the implementation. Run the check " +
			"as you go and fix what it reports; do not finish on a tree that does not compile. " +
			"If a test cannot be satisfied by any implementation — it contradicts another, or " +
			"asks for something the declared types cannot express — say so plainly and stop, " +
			"naming the test. That is a real answer and hands the work back to its author; " +
			"quietly working around it is not.",
		Guard:         tools.Both(tools.NoTests, tools.OnlyExt(".go")),
		Tools:         writing(tools.RunCommand),
		Check:         goCheck,
		MaxIterations: 120,
		Temperature:   0.2,
		MaxTokens:     12000,
	})
}

// Sec reviews the finished code and reports. It does not fix.
//
// READ-ONLY DELIBERATELY, and guarded twice. A reviewer that can edit stops
// reviewing and starts rewriting, and the report — the thing the stage exists to
// produce — then describes code that no longer exists. The missing tool is what
// the model sees; the guard is what happens if someone adds the tool back.
func (c Creator) Sec(files map[string]string) *Agent {
	return c.New(files, Options{
		Name:  StageSec,
		Class: model.ClassLarge,
		Prompt: "You are a security reviewer. Read the implementation and report what an " +
			"attacker could do with it: injection through unvalidated input, secrets on disk or " +
			"in logs, authorisation checks that are missing rather than wrong, resource limits " +
			"nobody set. For each finding name the file and the line and say what an attacker " +
			"gets, concretely. You cannot change the code — say what is wrong and let the stage " +
			"that owns it fix it. If you find nothing real, say that; an invented finding costs " +
			"someone a day and teaches them to skip your reports.",
		Guard:         tools.DenyAll,
		Tools:         readOnly,
		MaxIterations: 20,
		Temperature:   0.2,
		MaxTokens:     8000,
	})
}

// Integrator is the last gate before the work is called done.
//
// ITS CHECK IS THE WHOLE SUITE, not one task's tests. The failure it catches is
// the one no earlier stage can: two pieces of work that each passed alone and do
// not compile together.
func (c Creator) Integrator(files map[string]string) *Agent {
	return c.New(files, Options{
		Name:  StageIntegrator,
		Class: model.ClassLarge,
		Prompt: "You are integrating finished work. Run the full check across the whole tree and " +
			"resolve what only shows up once the parts are together: duplicate declarations, " +
			"packages that drifted apart on a shared type, imports that no longer resolve. Fix " +
			"the integration, not the design — if two pieces disagree about what a type should " +
			"be, make them agree the way the architecture says, and if the architecture does not " +
			"say, pick the one with more callers and note it. You may not edit tests.",
		Guard:         tools.Both(tools.NoTests, tools.OnlyExt(".go")),
		Tools:         writing(tools.RunCommand),
		Check:         goCheck,
		MaxIterations: 60,
		Temperature:   0.2,
		MaxTokens:     12000,
	})
}
