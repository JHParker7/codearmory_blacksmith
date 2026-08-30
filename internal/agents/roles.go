package agents

import (
	"fmt"
	"strings"
	"time"

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
	StagePlanTest      = "plan-test"
	StagePlanDev       = "plan-dev"
	StagePlanSec       = "plan-sec"
	StagePlanReview    = "plan-review"

	// The auto-mode fix stage, run against a project cloned from the board, and
	// the review stage that merges its approved work.
	StageFix    = "fix"
	StageReview = "review"
)

// PlanStages are the plan arm's stages, in order: plan the work and the tests,
// write the tests red, make them green, then read the result with an
// attacker's eyes.
func PlanStages() []string {
	return []string{StagePlanArchitect, StagePlanTest, StagePlanDev, StagePlanSec, StagePlanReview}
}

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
	// ONE DRAFT, NOT A REVISION LOOP, and the budget is what enforces it. The
	// department's original architect was a single model call and took a minute;
	// this one, given forty turns of tools, spent sixteen writes and fifteen
	// searches polishing a plan it had substantially finished on the first write
	// — ten minutes of GPU for wording changes. A planning document's value is
	// in existing, not in its fifth revision, and the developer reads it once.
	// The budget leaves room to look around, draft once, patch once, and stop.
	return c.New(files, Options{
		Name:  StagePlanArchitect,
		Class: model.ClassLarge,
		Prompt: "You are a systems architect. Write PLAN.md — ONE file, in ONE write_file call — " +
			"planning BOTH the implementation and the tests for what the user asked. A developer " +
			"will read it as its plan and write the code and the tests from it, so anything you " +
			"leave out is something nobody builds and nobody checks.\n\n" +
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
			"Do not write source code — describe it. Draft the whole plan in your head, write " +
			"it ONCE, and stop: the plan is read once by one developer, and its value is in " +
			"existing, not in being polished. When it is written, say so and finish.",
		Guard:         tools.OnlyExt(".md"),
		Tools:         writing(),
		MaxIterations: 8,
		Temperature:   0.3,
		MaxTokens:     12000,
	})
}

// rootSpecCheck is the expected-red check for a module at the repository ROOT:
// green when the stubs build, the tests typecheck, and the tests FAIL. The
// same three pieces as specCheck, for the same three reasons.
const rootSpecCheck = "go build ./... && go vet ./... && ! go test ./..."

// routeGate refuses a suite that never touches the HTTP layer the tree serves.
//
// THE GAP THE FIRST LOCKED-SUITE RUN SHIPPED THROUGH: the test author wrote 349
// lines of store tests and not one handler test, the developer implemented
// exactly what the locked suite forced and nothing more, and a green 4-minute
// run produced an API answering 501 to every request. With the tests locked,
// the test author's coverage IS the product specification — so the author's own
// gate has to see the layer it skipped, or the whole arm inherits the gap.
//
// CONDITIONAL, so a task with no HTTP in it cannot be dead-ended by a gate
// about routes: it bites only when some non-test source imports net/http.
// httptest presence is a proxy for "the routes are exercised" — a mechanical,
// checkable one, in the spirit of every other gate here — and the prompt still
// carries the real demand of one case per route and status code.
const routeGate = `{ ! grep -rq --include='*.go' --exclude='*_test.go' '"net/http"' . || ` +
	`grep -rq --include='*_test.go' 'httptest\.' . || ` +
	`{ echo 'the tree serves HTTP but the suite never touches it: no test uses ` +
	`net/http/httptest. The routes are part of the specification - add handler tests ` +
	`that call each route through the mux and assert the status codes the plan names.'; ` +
	`exit 1; }; }`

// PlanTester turns the plan's named cases into failing tests, plus the stubs
// they need to compile.
//
// THE STAGE RUN 3 PROVED NECESSARY. The plan named its test cases one by one
// and the developer skipped every one of them, finished in 2m41s, and passed —
// a plan nobody enforces is advice. Making the tests EXIST before the developer
// starts changes what "done" means for it: the tree it inherits is already red,
// and its own check cannot go green without answering the cases.
//
// The stubs are the "fail but not error" half: placeholder types and functions
// with zero-value bodies, so the tests COMPILE and fail on assertions. A suite
// that does not compile is a broken spec, not the expected red, and go vet in
// the check is what tells those apart.
func (c Creator) PlanTester(files map[string]string) *Agent {
	return c.New(files, Options{
		Name:  StagePlanTest,
		Class: model.ClassLarge,
		Prompt: "You are a test author and you work test-first. PLAN.md in this repository names " +
			"the test cases, one by one; an architect wrote it and a developer will make your " +
			"tests pass. READ IT FIRST, then write Go unit tests for EVERY case it names — a " +
			"case you skip is a case nobody will ever check.\n\n" +
			"Also write the placeholder declarations the tests need in order to COMPILE: the " +
			"types with their fields, and the functions with zero-value bodies — return nil, " +
			"zero, false, or an empty struct. The placeholders exist so your tests FAIL on " +
			"assertions rather than fail to build; do not implement any real behaviour, because " +
			"a test that passes before the developer starts has been told nothing. Put go.mod " +
			"and the packages at the repository ROOT.\n\n" +
			"You are done when the tree compiles and the tests FAIL — that is what your check " +
			"verifies, and it is the only green this stage has. If the plan serves HTTP, the " +
			"check also refuses a suite that never exercises the routes: write handler tests " +
			"with net/http/httptest that call each route through the mux, one case per status " +
			"code the plan names. A store tested to perfection behind untested routes is a " +
			"product that does not exist.",
		Guard:         tools.OnlyExt(".go"),
		Tools:         writing(tools.RunCommand),
		Check:         rootSpecCheck + " && " + routeGate,
		OwnCheck:      true,
		MaxIterations: 60,
		Temperature:   0.2,
		MaxTokens:     12000,
	})
}

// PlanFollowingDev makes the tests pass, and may not touch them.
//
// THE TESTS ARE LOCKED — the department's rule, restored here once the test
// author existed to justify it. The softer version (dev may edit tests) was
// tried first and belonged to the arrangement where the dev also WROTE the
// tests; now they arrive written against the plan with compiling stubs and
// proven red-not-broken by an expected-red gate, so "make them pass" is a
// closed goal. Not a guarantee — a genuinely wrong test now dead-ends this
// stage at its budget, and this arm has no referee — but that trade was taken
// deliberately, after watching the softer version, and this comment is where
// the decision lives.
//
// IN EXCHANGE IT MAY REWRITE IMPLEMENTATION FILES WHOLE, drop check and all.
// That check exists for stages whose suite cannot speak for them; this one's
// suite is locked against it, so a dropped function fails the very next
// auto-check BY NAME — a better refusal than the write gate can compose,
// because the compiler writes it against the real tree.
func (c Creator) PlanFollowingDev(files map[string]string) *Agent {
	return c.New(files, Options{
		Name:  StagePlanDev,
		Class: model.ClassLarge,
		Prompt: "You are a Go developer. This repository holds a plan (the markdown files, " +
			"written by an architect) and FAILING TESTS with placeholder stubs (written by a " +
			"test author from that plan). READ THEM FIRST. Your job is to replace the " +
			"placeholder bodies with real implementations until the tests pass.\n\n" +
			"The tests are the specification and they are NOT YOURS TO CHANGE — they were " +
			"written to be passable, and the check runs them for you after every edit. You may " +
			"rewrite an implementation file whole when that is simpler than editing it: if you " +
			"drop something the code needed, the tests will name it. If a test truly cannot be " +
			"satisfied — it contradicts another, or the declared types cannot express what it " +
			"asks — say so plainly and stop, naming the test; that is a real answer, and quietly " +
			"working around it is not. Do not finish on a tree that does not compile or whose " +
			"tests fail.",
		Guard:        tools.Both(tools.NoTests, tools.OnlyExt(".go")),
		Tools:        writing(tools.RunCommand),
		Check:        rootCheck,
		RewriteWhole: true,
		// EIGHT MINUTES, NO RESPINS: a slow dev FAILS THE RUN, and the
		// whole-run reroll draws everything fresh. The respin era (15m ×3,
		// fresh dev on the same tree) taught its own limit on the nine-run
		// batch — the one failing seed's curse was the TEST SUITE, an envelope
		// its own helper could not decode, so every fresh dev inherited the
		// trap and died in it. A dev respin keeps the cursed suite; the run
		// reroll replaces plan, suite and dev together, and a fresh draw is a
		// measured 8-in-9 fast pass.
		//
		// Eight, because the batch's dev times were bimodal: seven of eight
		// passes finished under 6m39s, then nothing until 13m27s, and past
		// eight minutes the conditional pass rate fell to a coin flip while
		// the remaining cost ballooned. The one slow-honest pass this cuts
		// (19m30s total) redraws in an expected ~10m — the case that "loses"
		// roughly breaks even, and the 45-minute tail is gone entirely.
		AttemptTimeout: 8 * time.Minute,
		Respins:        0,
		MaxIterations:  150,
		Temperature:    0.2,
		MaxTokens:      12000,
	})
}

// PlanSec reads the finished run with an attacker's eyes and files every
// real finding as a TICKET on the board. Never into the tree: a findings file
// committed to the repository is a curated vulnerability list handed to
// anyone with clone access, while the board lives behind the gatekeeper —
// the operator drew that line and it is the right one. The stage's prose
// answer is its verdict; the tickets are its work.
//
// THE SCANNER SEAM IS THE TREE, designed now, fed later: SAST and SCA
// scanners will run in the sandbox before this stage and leave their reports
// under scan/, where the reviewer reads them like any other file. Reports are
// LEADS TO VERIFY, not verdicts to copy — a scanner finding in your own code
// is usually a change to make, one in a dependency usually a version bump,
// and an unverified copy of either costs a person a day. Advisory, not a
// gate, matching the department's split between ScanCommand context and
// CriticalCommand gates.
func (c Creator) PlanSec(files map[string]string) *Agent {
	return c.New(files, Options{
		Name:  StagePlanSec,
		Class: model.ClassLarge,
		Prompt: "You are a security reviewer reading a finished change. Read the implementation " +
			"and the tests, then file each REAL finding as a ticket with file_ticket: injection " +
			"through unvalidated input, secrets on disk or in logs, authorisation checks missing " +
			"rather than wrong, resource limits nobody set, error text that leaks internals. One " +
			"ticket per finding; in the body name the FILE and LINE, say concretely what an " +
			"attacker gets, and the fix to make.\n\n" +
			"NEVER write findings into the repository — a committed list of vulnerabilities is " +
			"a gift to anyone who clones it. The board is where findings go.\n\n" +
			"If scanner reports exist under scan/ — SAST or dependency audits — read them " +
			"FIRST, as leads: verify each against the code, file the real ones, and name the " +
			"false positives in your answer, because an unverified copy of a scanner line " +
			"costs a person a day. If you find nothing real, file nothing and say so — an " +
			"invented finding teaches people to skip your tickets. Finish with a one-line " +
			"verdict: ship, ship with fixes, or stop.",
		Guard:         tools.DenyAll,
		Tools:         append(append([]string{}, readOnly...), tools.FileTicket),
		TicketKind:    "security",
		MaxIterations: 25,
		Temperature:   0.2,
		MaxTokens:     8000,
	})
}

// PlanReview is the CODE reviewer: it reads the finished change for quality —
// correctness bugs the tests missed, error handling that swallows failures,
// duplication, dead code, unclear names, missing docs on exported symbols —
// and files each as a QUALITY ticket. Same shape as the security reviewer and
// for the same reasons: it writes nothing into the tree, findings go to the
// board, and staticcheck's report under scan/ is a lead to verify, not a
// verdict to copy.
//
// LOWER PRIORITY THAN SECURITY BY CONSTRUCTION: its tickets are kinded
// "quality", and auto-mode works security kinds first. A quality nit is worth
// filing and worth fixing when nothing more dangerous is waiting, which is
// exactly the ordering the kind encodes.
func (c Creator) PlanReview(files map[string]string) *Agent {
	return c.New(files, Options{
		Name:  StagePlanReview,
		Class: model.ClassLarge,
		Prompt: "You are a code reviewer reading a finished change for QUALITY, not security " +
			"— a separate reviewer already covered security. File each real issue as a ticket " +
			"with file_ticket: a correctness bug the tests do not catch, error handling that " +
			"swallows or mislabels a failure, duplicated logic, dead code, a misleading name, a " +
			"missing doc comment on an exported symbol, a resource left unclosed. One ticket per " +
			"issue; in the body name the FILE and LINE, say what is wrong and the change to make, " +
			"and rate it high, medium or low.\n\n" +
			"NEVER write into the repository — you review, you do not fix; the tickets are the " +
			"work. If staticcheck's report exists under scan/lint.txt, read it FIRST as leads: " +
			"verify each against the code, file the real ones, name the false positives in your " +
			"answer. Findings already on the board are listed in your task; do NOT refile them. " +
			"If you find nothing worth a person's time, file nothing and say so. Finish with a " +
			"one-line verdict on the change's quality.",
		Guard:         tools.DenyAll,
		Tools:         append(append([]string{}, readOnly...), tools.FileTicket),
		TicketKind:    "quality",
		MaxIterations: 25,
		Temperature:   0.2,
		MaxTokens:     8000,
	})
}

// FixFinding is auto-mode's developer: given a finding ticket and the code it
// is about, it makes the change and keeps the tests green. It is the plan-arm
// developer pointed at a different task — a filed finding rather than a fresh
// suite — with the same rules that make a developer trustworthy: it MAY NOT
// edit tests, because a fix that weakens the test proving the bug is not a
// fix, and the existing suite is what proves the fix did not break anything
// else. Whole-file rewrites stay allowed, for the same locked-suite reason.
//
// No respins, an 8-minute bound: a fix that has not converged in eight minutes
// is one a person should see, and auto-mode moves on to the next finding
// rather than grinding.
func (c Creator) FixFinding(files map[string]string) *Agent {
	return c.New(files, Options{
		Name:  StageFix,
		Class: model.ClassLarge,
		Prompt: "You are a Go developer fixing ONE reported issue in an existing project. Your " +
			"task names the finding — a security or quality problem, with the file and line and " +
			"the change to make. READ the named code first, then make the smallest change that " +
			"resolves the finding.\n\n" +
			"You may not edit test files: a fix that weakens the test proving the bug is not a " +
			"fix, and the suite is what proves your change broke nothing else. Run the check as " +
			"you go; finish only on a tree that compiles and whose tests pass. If the finding is " +
			"wrong or cannot be fixed without changing behaviour the tests require, say so " +
			"plainly and stop — that is a real answer a person needs to see.",
		Guard:          tools.Both(tools.NoTests, tools.OnlyExt(".go")),
		Tools:          writing(tools.RunCommand),
		Check:          rootCheck,
		RewriteWhole:   true,
		AttemptTimeout: 8 * time.Minute,
		Respins:        0,
		MaxIterations:  150,
		Temperature:    0.2,
		MaxTokens:      12000,
	})
}

// MergeReviewer is the gate on auto-mode's fixes: it reads the FIX DIFF against
// the finding it claims to resolve and either APPROVES — merging into dev via
// merge_fix — or rejects in prose. Nothing merges to dev without passing here,
// because an auto-fix is a machine's proposal and dev is the line humans and
// the next request build on.
//
// It reviews, it does not rewrite: no write tools, no check. The fix already
// passed its own tests; this stage judges whether the change is the RIGHT one
// — does it resolve the finding, does it keep the tests meaningful rather than
// gaming them, is it safe — the judgement a person makes at a pull request,
// made here so the merge can be autonomous and still trustworthy.
func (c Creator) MergeReviewer(files map[string]string) *Agent {
	return c.New(files, Options{
		Name:  StageReview,
		Class: model.ClassLarge,
		Prompt: "You are reviewing a proposed fix before it merges to dev. Your task gives the " +
			"finding and the DIFF of the fix. Read the diff against the current code and judge " +
			"it: does it actually resolve the finding, does it keep the tests meaningful rather " +
			"than weakening them to pass, does it introduce a new problem, is it safe to ship.\n\n" +
			"If it is right, call merge_fix with a one-line reason and it merges to dev. If it is " +
			"wrong, incomplete, or you are unsure — do NOT merge; say plainly what is wrong, and " +
			"the fix waits on its branch for a person. Approve only what you would merge " +
			"yourself; a bad merge to dev costs more than a fix left waiting.",
		Guard:         tools.DenyAll,
		Tools:         append(append([]string{}, readOnly...), tools.MergeFix),
		MaxIterations: 15,
		Temperature:   0.2,
		MaxTokens:     6000,
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

// specCheck is the EXPECTED-RED check: green when the tree compiles and the
// tests FAIL.
//
// The specification stage's success is the opposite of everyone else's, and its
// check was goCheck — green on passing tests — so a correct specification
// (failing tests, per its own prompt) could never pass, and a tautological or
// self-implemented one always did. The stage's contract was inverted by its own
// gate.
//
// The pieces, in order, because each rules out a different wrong kind of red:
//   - `go build` — the non-test tree compiles; a spec whose stubs do not build
//     is a broken spec, not the expected red;
//   - `go vet` — this is what TYPECHECKS THE TEST FILES, which `go build` does
//     not touch. Without it, a test file full of syntax errors makes `go test`
//     exit non-zero and reads as the expected red;
//   - `! go test` — the tests, which now provably compile, fail.
//
// internal/gate solves this properly (SpecScript knows a compile failure from a
// red suite); this is the simple shape's honest approximation of it.
const specCheck = "cd src && go build ./... && go vet ./... && ! go test ./..."

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
		Check:         specCheck,
		OwnCheck:      true,
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
