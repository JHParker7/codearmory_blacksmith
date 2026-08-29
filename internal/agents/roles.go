package agents

import (
	"github.com/code-armory-app/blacksmith/internal/model"
	"github.com/code-armory-app/blacksmith/internal/tools"
)

// A Role is one stage of the pipeline: what it is told, what it may touch, and
// when it is done.
//
// DATA, NOT CODE. Every difference between the stages that matters is one of
// these fields, and each time one was expressed as a separate function instead,
// the functions drifted — a fix to the developer's refusal handling that never
// reached the reviewer's.
type Role struct {
	Name string

	// Class is the serving class this stage asks for. Which model that is, and
	// where it runs, is the gateway's business and not the stage's.
	Class model.Class

	// System is the stage's standing instruction. It says what the stage IS; the
	// task passed to Run says what this particular piece of work is.
	System string

	// Guard decides what this stage may write. The strongest statement of what a
	// stage is for, and the only one a model cannot talk its way past.
	Guard tools.Guard

	// ToolNames limits the offered tools. Empty offers all of them.
	ToolNames []string

	// Check is the command that decides whether this stage succeeded. Empty means
	// the stage ends when the model stops calling tools — right for the stages
	// whose output is prose or a document, wrong for every stage that writes code.
	Check string

	MaxIterations int
	Temperature   float64
	MaxTokens     int
}

// Reading tools every stage gets. Named once because a stage that cannot look
// around cannot do anything useful, and leaving one out of a list is a silent
// way to produce exactly that.
var readOnly = []string{tools.ReadFiles, tools.ListFiles, tools.SearchFiles}

func writing(extra ...string) []string {
	return append(append([]string{}, readOnly...), append([]string{tools.WriteFile, tools.UndoEdit}, extra...)...)
}

// Architect decides what the system looks like and writes it down.
//
// IT THINKS, and it is the stage where that pays. It is deciding a shape rather
// than transcribing a decision already made — measured on the python rebuild
// against qwen3.8, reasoning cost 358 tokens and 7s versus 45 tokens and 1s on a
// transcription job for the same answer, so the effort is worth spending here
// and not everywhere. Which is why reasoning is a property of the serving class
// this role asks for, not a global.
//
// MARKDOWN ONLY. Its output is read by every stage after it; an architect that
// can also write the code writes the code instead of the design, and then the
// stages that were meant to read a design read half an implementation.
func Architect() Role {
	return Role{
		Name:  "architect",
		Class: model.ClassLarge,
		System: "You are a systems architect. Write or update markdown files describing the " +
			"software architecture that accomplishes what the user asked for. These files are " +
			"the only thing the later stages get: a product manager will break them into work, a " +
			"specification author will write failing tests from them, and a developer will " +
			"implement against those tests. Name the packages, the types and the boundaries " +
			"between them concretely enough that someone can write a test against one without " +
			"asking you a question. Do not write source code — describe it.",
		Guard:         tools.OnlyExt(".md"),
		ToolNames:     writing(),
		MaxIterations: 20,
		Temperature:   0.3,
		MaxTokens:     8000,
	}
}

// PM breaks the architecture into work that can be done independently.
//
// NO CHECK AND NO GRAMMAR. Constraining its breakdown to a schema was tried in
// this department and measured worse: given one, the model folded dependencies
// and acceptance criteria into a single string and returned one degenerate
// subtask, where the same prompt unconstrained produced four tasks and six
// sections. The shape stays the prompt's job here.
func PM() Role {
	return Role{
		Name:  "pm",
		Class: model.ClassLarge,
		System: "You are a product manager. Read the architecture documents and break the work " +
			"into tasks that can be done independently. For each task write what it covers, what " +
			"it depends on, and how anyone would know it is finished. Write them to markdown. A " +
			"task that cannot be started until another is done must say so — the stages after " +
			"you schedule from what you write, and an unstated dependency becomes a developer " +
			"waiting on a package nobody built.",
		Guard:         tools.OnlyExt(".md"),
		ToolNames:     writing(),
		MaxIterations: 20,
		Temperature:   0.3,
		MaxTokens:     8000,
	}
}

// Spec writes the failing tests. The stage the whole pipeline is arranged
// around.
//
// ITS CHECK IS THAT THE TREE BUILDS AND THE TESTS FAIL. Both halves matter and
// each was learned separately: a suite that does not compile is not the expected
// red — it is a broken specification, and a developer sent at it spends its
// budget on the author's syntax errors. A suite that passes on an empty
// implementation is not a specification at all, it is a tautology, and nothing
// downstream will ever notice.
//
// It may write tests and the declarations they need to compile against, and
// nothing else. A specification that also implements its subject cannot fail,
// so it never asks the developer for anything.
func Spec() Role {
	return Role{
		Name:  "spec",
		Class: model.ClassLarge,
		System: "You are a specification author and you work test-first. Read the architecture " +
			"and the task, then write Go tests that FAIL against the code as it stands, plus the " +
			"minimum declarations — types, function signatures with empty or panicking bodies — " +
			"that the tests need in order to COMPILE. The tests are the contract: a developer who " +
			"cannot read your test cannot build the right thing, and a developer whose tests pass " +
			"before writing anything has been told nothing. Never implement the behaviour you are " +
			"specifying. Your work is done when the tree compiles and the tests fail for the " +
			"reason you intended.",
		Guard:         tools.AllowAll,
		ToolNames:     writing(tools.RunCommand),
		Check:         "cd src && go build ./... && go test ./...",
		MaxIterations: 60,
		Temperature:   0.2,
		MaxTokens:     12000,
	}
}

// Dev makes the tests pass, and may not change them.
//
// THE GUARD IS THE POINT. A developer that can edit the specification can always
// go green without making the code work, and that failure is invisible — the run
// reports success. Every attempt to hold this line with a sentence in the prompt
// failed; the refusal at the write holds it.
//
// The budget is the largest of any stage because it is the only one whose work
// is bounded by a compiler rather than by its own judgement about when it is
// finished.
func Dev() Role {
	return Role{
		Name:  "dev",
		Class: model.ClassLarge,
		System: "You are a Go developer. Failing tests describe what the code must do; make them " +
			"pass. You may not edit test files — they are the specification and they are not " +
			"yours to change. Read the tests first, then write the implementation. Run the check " +
			"as you go and fix what it reports; do not finish on a tree that does not compile. If " +
			"a test cannot be satisfied by any implementation — it contradicts another, or asks " +
			"for something the declared types cannot express — say so plainly and stop, naming " +
			"the test. That is a real answer and hands the work back to its author; quietly " +
			"working around it is not.",
		Guard:         tools.Both(tools.NoTests, tools.OnlyExt(".go")),
		ToolNames:     writing(tools.RunCommand),
		Check:         "cd src && go build ./... && go test ./...",
		MaxIterations: 120,
		Temperature:   0.2,
		MaxTokens:     12000,
	}
}

// Sec reviews the finished code and reports. It does not fix.
//
// READ-ONLY DELIBERATELY. A reviewer that can edit stops reviewing and starts
// rewriting, and the report — the thing the stage exists to produce — comes back
// describing code that no longer exists.
func Sec() Role {
	return Role{
		Name:  "sec",
		Class: model.ClassLarge,
		System: "You are a security reviewer. Read the implementation and report what an attacker " +
			"could do with it: injection through unvalidated input, secrets on disk or in logs, " +
			"authorisation checks that are missing rather than wrong, resource limits nobody set. " +
			"For each finding name the file and the line and say what an attacker gets, " +
			"concretely. You cannot change the code — say what is wrong and let the stage that " +
			"owns it fix it. If you find nothing real, say that; an invented finding costs " +
			"someone a day and teaches them to skip your reports.",
		Guard:         tools.DenyAll,
		ToolNames:     readOnly,
		MaxIterations: 20,
		Temperature:   0.2,
		MaxTokens:     8000,
	}
}

// Integrator is the last gate before the work is called done.
//
// ITS CHECK IS THE WHOLE SUITE, not the one task's tests. The failure it catches
// is the one no earlier stage can: two tasks that each passed alone and do not
// compile together.
func Integrator() Role {
	return Role{
		Name:  "integrator",
		Class: model.ClassLarge,
		System: "You are integrating finished work. Run the full check across the whole tree and " +
			"resolve what only shows up once the parts are together: duplicate declarations, " +
			"packages that drifted apart on a shared type, imports that no longer resolve. Fix " +
			"the integration, not the design — if two pieces disagree about what a type should " +
			"be, make them agree the way the architecture says, and if the architecture does not " +
			"say, pick the one with more callers and note it. You may not edit tests.",
		Guard:         tools.Both(tools.NoTests, tools.OnlyExt(".go")),
		ToolNames:     writing(tools.RunCommand),
		Check:         "cd src && go build ./... && go test ./...",
		MaxIterations: 60,
		Temperature:   0.2,
		MaxTokens:     12000,
	}
}

// Pipeline is the roles in the order they run.
//
// THE ORDER IS THE PIPELINE. There is no scheduler in this rebuild: a stage's
// input is the tree the previous stage left behind, which is the simplest thing
// that can work and is enough to run the whole sequence end to end. The
// column-based dispatch in internal/dispatch is what this grows back into when
// several hosts have to share a board.
func Pipeline() []Role {
	return []Role{Architect(), PM(), Spec(), Dev(), Sec(), Integrator()}
}
