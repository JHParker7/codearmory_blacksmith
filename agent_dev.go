package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// The developer agent: read a ticket, change code, prove it with tests, push a
// branch. First real use of the large model class, and the first agent that
// composes inference, a sandbox and git into one loop.
//
// THE LOOP IS OURS. Each iteration is one model call returning ONE action, which
// blacksmith executes and feeds back. No framework: the loop is a switch
// statement, which is both the smallest thing that works and the only version
// whose failure modes are obvious.
//
// VERIFICATION IS NOT THE AGENT'S TO DEFINE. run_tests pushes the branch and runs
// the repository's own CodeArmory pipeline — the same one a person's push runs.
// The agent never invents a test command, so there is one definition of "does
// this work" rather than two that drift.
//
// CONTAINMENT — the action surface is deliberately narrow. The model may read
// files, write files, run the pipeline, or stop. It may NOT run arbitrary shell.
//
// Note what this is and is not defending. The sandbox IS a kernel boundary
// today: the deployed forge runs the kata backend with the kata-clh
// RuntimeClass, so agent commands already execute in a microVM behind the egress
// NetworkPolicy. So the narrow surface is defence in depth, not the only wall —
// an earlier version of this comment claimed otherwise and was wrong.
//
// What it still buys, and why it stays until it is deliberately widened:
//
//   - it bounds what a prompt injection can DO, not merely where it runs. A
//     kata VM contains a compromised sandbox; it does not stop the agent pushing
//     a malicious branch, and the action list does;
//   - every path the model supplies is validated against escaping the checkout,
//     because "write a file" and "write /etc/anything" differ by two characters;
//   - and the pipeline, not the agent, decides whether a change is good.

// devAction is the one action a model turn may request.
type devAction struct {
	Action string   `json:"action"`
	Paths  []string `json:"paths,omitempty"`
	// Edits are search/replace pairs. Whole-file writes were the previous shape
	// and are gone: replacing a file wholesale means every write is a chance to
	// silently drop the parts the ticket never mentioned, which is exactly what
	// happened — a ticket asking for one struct deleted an entire HTTP server and
	// only the reviewer noticed. An edit that names what it is replacing cannot do
	// that, and says loudly when the file is not what the model thought.
	Edits   []devEdit `json:"edits,omitempty"`
	Summary string    `json:"summary,omitempty"`
	// Type is the Conventional Commits type for the change. Requested from the
	// model because only it knows what the change was, and validated against an
	// allowlist because a commit-msg hook will reject anything else.
	Type   string `json:"type,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// conventionalTypes is the Conventional Commits allowlist. Repositories commonly
// enforce this with commitlint on the commit-msg hook, so a message outside it
// is not a style preference — it is a commit that will not land.
var conventionalTypes = []string{"feat", "fix", "chore", "docs", "refactor", "test", "perf", "build", "ci", "style"}

// The action allowlist. Anything else is refused and fed back as an error, which
// is more useful to the model than a silent no-op.
const (
	actionReadFiles  = "read_files"
	actionWriteFiles = "write_files"
	// actionUndoEdit restores a file to what it was before the last edit.
	//
	// THE MISSING ESCAPE HATCH. Once a file diverges from the model's mental image
	// of it, every addressing scheme fails together: text no longer matches and
	// line numbers no longer mean what it thinks. Recorded in this repo before it
	// had a name — a developer wrote store.go with every struct tag missing its
	// closing backtick, then spent 34 consecutive actions failing to search and
	// replace its way out of a file it had itself broken.
	//
	// Both edit formats have now failed for this same reason, which is why the
	// recovery matters more than the choice between them.
	actionUndoEdit = "undo_edit"
	actionRunTests = "run_tests"
	actionFinish   = "finish"
	actionGiveUp   = "give_up"
)

// THE MODEL CHOOSES WHAT TO CHANGE, NOT WHEN TO CHECK IT.
//
// run_tests, finish and give_up were all removed together, and the measurement
// behind that is blunt: in the last fixture pass 28 of 31 refusals were the
// harness telling the agent it could not run the tests just now. Every one of
// those exists only because asking was possible. Verification is not a decision
// the model is good at sequencing — it is a consequence of having changed
// something, so the harness runs it on every write that changes the tree and
// advances the stage the moment its own success condition is met.
//
// give_up went with them because it was never the real bound. An agent that
// cannot finish is already ended by the refusal ceiling, the read ceiling and the
// turn budget; a tool for quitting only gave a stuck model a way to convert a
// recoverable attempt into a terminal one.
var devActions = []string{actionReadFiles, actionWriteFiles, actionUndoEdit}

// actionsFor is the vocabulary one stage may use.
//
// EVERY STAGE IS ENDED BY ITS GATE, including the specification author. Giving
// the author finish back was tried, on the reasoning that a passing check means
// something different for it — its tests are supposed to fail, so "the gate
// passed" is as true after the first file as after the last, and on r89 that
// truncated an author briefed on five units of work to one file in three turns.
//
// The truncation was real and the fix was wrong. Measured on r90: with finish
// restored the same author ran 49 turns and wrote the same file 41 times, never
// finishing. That is the behaviour the tool was removed for in the first place —
// across 3,498 recorded turns the agents called finish once, and one ran to
// iteration 439 still reading — and it is not confined to the old model after
// all.
//
// The truncation is answered by the BRIEF instead: an author given one unit of
// work is finished when its first file compiles, so the gate and the job end at
// the same moment. See the ticket-merge stage, which opens one section per unit
// rather than one covering all of them.
func actionsFor(mode agentMode) []string {
	return devActions
}

// Bounds on what one iteration may move. A model that asks for the whole repo
// blows the context window and produces worse output than one that asks for four
// files, so the cap is a quality control as much as a safety one.
const (
	maxReadPaths  = 12
	maxWriteFiles = 12
	// maxUnverifiedWrites is how many write actions may pile up before the agent
	// must verify. Three is deliberately generous — a change spanning several
	// files is normal — while still catching the runaway, which does not write
	// three files, it writes one file thirty times.
	maxUnverifiedWrites = 3
	// agentResetTurns is how many turns an attempt runs before the agent's MEMORY
	// is cleared while its WORK is kept. See devState.resetAgent.
	agentResetTurns = 40
	// maxAgentResets bounds it, because a reset that has not helped four times
	// will not help a fifth. Five is a starting figure to be measured, not a
	// derived one.
	maxAgentResets = 5
	// minSpecBrokenTries is how many times the developer must CHANGE the
	// implementation and still get test-file-only compile errors before the
	// specification is judged unsatisfiable. One was far too few — see the
	// detector in verifyInSandbox.
	minSpecBrokenTries = 3
	// maxTestEditRefusals is the SECOND route to the same conclusion, and it
	// exists because the first one could not be reached.
	//
	// minSpecBrokenTries advances only when the developer CHANGES the
	// implementation and re-verifies. A developer facing a test file that does not
	// compile does not do that — it tries to fix the test file, which is refused,
	// and each refusal counts toward maxDeadRefusals instead. Measured on r57: 63
	// test-file refusals against a spec whose only fault was
	// `declared and not used: tasks`, the attempt killed at 20 refusals, four times
	// over, and the specification never once judged broken. The two bounds were in
	// direct conflict — the proof required work the refusal ceiling forbade.
	//
	// So wanting to edit the test file, repeatedly, WHILE that file is the thing
	// failing to compile, is itself the evidence. Nothing else explains it.
	maxTestEditRefusals = 3
	// maxSpecRepairs bounds how many times a section may be handed back to its
	// author for a specification that does not compile. Beyond it the ticket stops
	// for a person, because an author that cannot fix its own tests twice will not
	// fix them on the third pass either.
	maxSpecRepairs = 2
	// maxConsecutiveReads ends an attempt that has done nothing but read for this
	// many turns in a row.
	//
	// Reads no longer spend the turn budget, so something else has to stop an
	// agent that only reads — and a run of consecutive reads is a far better
	// signal than a total count, because it names the actual failure. Measured on
	// one ticket: read, write, run_tests, then 87 consecutive reads through two
	// memory resets. An agent still gathering context after a hundred looks in a
	// row is not about to start writing.
	//
	// It counts EVERY read, not just the ones that returned nothing new: an agent
	// paging through the repository one file at a time is in the same place as one
	// asking for the same file, and neither has produced any work.
	maxConsecutiveReads = 100
	maxFileBytes        = 128 * 1024
	maxFileListing      = 400
	maxTestOutput       = 8000
	maxFileForModel     = 24 * 1024
	// maxAdviceRunes bounds the linter's output, and is deliberately far smaller
	// than a file. A pedantic linter emits hundreds of findings, and advice that
	// fills the context leaves no room for the work — the model would be reading
	// style notes instead of the failure it has to fix.
	maxAdviceRunes = 3000
)

// RepoConfig is what the developer agent needs to work a repository.
type RepoConfig struct {
	// URL is the clone URL. Credentials are never held here — forge resolves
	// SecretRef at dispatch, so the agent never sees them.
	URL string
	// SecretRef is a forge credential reference, e.g. "git:https://host/org/repo".
	SecretRef string
	// Branch is the base branch to work from.
	Branch string
	// IntegrationBranch is where reviewed work lands, and the developer needs it
	// for the same reason the integrator does: A TICKET THAT WAITED FOR ITS
	// DEPENDENCY MUST ACTUALLY RECEIVE IT.
	//
	// Branches are cut at scoping time, all from the same base, before any
	// sibling has merged. Scheduling then holds a ticket until its blockers reach
	// done — correctly — but nothing rebases the branch, so the developer starts
	// on a tree that predates the very code it was waiting for. Measured: an
	// agent writing handlers.go against a branch with no store.go, compiling to
	// "undefined: Task, undefined: store, undefined: TaskFilters", then reading
	// 96 times in a row looking for where Task was defined. It is not there. The
	// reading was rational and could never terminate.
	IntegrationBranch string
	// BranchPrefix names the branches the agent creates.
	BranchPrefix string
	// Image and RunnerClass select the sandbox. The image must be on forge's
	// allowlist or every execution is refused with a bare 400.
	Image       string
	RunnerClass string
	// LeaseIdleSecs and LeaseMaxSecs bound a sandbox this agent fails to release —
	// a crash, a kill, a host shutdown. The agent releases explicitly when a ticket
	// leaves the stage, so these should never fire in normal operation.
	LeaseIdleSecs int64
	LeaseMaxSecs  int64
	// PipelineID is the CodeArmory pipeline that verifies a branch — the same one
	// a person's push runs. PREFER IT: a pipeline is a single definition of "does
	// this work", shared with every human contributor, and it cannot drift from
	// what CI does because it IS what CI does.
	PipelineID string
	// FormatCommand rewrites the code before it is committed, e.g. "gofmt -w .".
	//
	// It runs at COMMIT time rather than in verification, because the point is that
	// the formatted code is what lands — a formatter run afterwards would report on
	// a branch that is already pushed. It is also the cheapest possible feedback:
	// whitespace, import order and a missing trailing newline are things a tool
	// knows exactly and a model only guesses at, and every iteration the model
	// spends on them is one it is not spending on the actual change.
	FormatCommand string
	// DepsCommand resolves the dependency manifest before the commit, e.g.
	// "go mod tidy". See depsScript: without it an agent cannot add a dependency
	// at all, because the lock file needs hashes no amount of editing can produce.
	//
	// Alongside the formatter rather than in verification, and for the same
	// reason: what lands in the commit has to be the resolved state, or the very
	// next clone fails on a manifest that names a module the lock file has never
	// heard of.
	DepsCommand string
	// LintCommand runs before the tests, e.g. "go vet ./...".
	//
	// ADVISORY, NOT A GATE. Its findings are reported — to the developer agent as
	// something it may fix, and to the reviewer as context — but they never fail a
	// branch, and that asymmetry with the tests is deliberate.
	//
	// A linter can produce findings that this change cannot resolve: a false
	// positive, a rule needing a refactor beyond the ticket, two rules that
	// disagree. Behind a gate, each of those traps the agent — it has a fixed
	// iteration budget, it spends the budget trying, and a correct change is thrown
	// away over a lint rule. That is the same failure the security reviewer is
	// deliberately advisory to avoid: a check that can block is a check that can
	// deny the pipeline, whether the trigger is a prompt injection or a stubborn
	// rule.
	//
	// The tests stay a gate because "does this work" is binary and answerable. An
	// operator who genuinely wants lint to block already has the mechanism: put it
	// in TestCommand ("go vet ./... && go test ./..."), and say so on purpose.
	LintCommand string
	// CriticalCommand is the one analysis that DOES gate, e.g.
	// "gosec -severity=high -quiet ./..." or "govulncheck ./...".
	//
	// Severity is the line, and the operator draws it: this is a command that exits
	// non-zero ONLY for findings a change must not carry — a hardcoded credential,
	// an injection, a known-exploitable dependency. Most analysers already express
	// that as a flag (gosec -severity, semgrep --severity), so the threshold lives
	// where the tool's own vocabulary is rather than in a severity parser here that
	// would have to be rewritten per tool and would be wrong for the next one.
	//
	// It gates because the alternative is worse for exactly the findings that
	// matter most: without it a critical finding waits for the security reviewer,
	// who cannot fix anything and can only write a comment, so the fix comes back
	// to a developer agent on a LATER ticket having lost all the context of the
	// change. Blocking here returns it to the agent that still has the branch open
	// and iterations left.
	//
	// Keep the threshold high. Everything below it belongs in ScanCommand, where it
	// informs the reviewer without being able to trap the agent.
	CriticalCommand string
	// DependencyManifests are the paths whose change means this branch altered the
	// dependency tree. Empty uses defaultDependencyManifests.
	DependencyManifests []string
	// SCACommand analyses the DEPENDENCIES rather than the code, e.g.
	// "osv-scanner -r ." or "npm audit".
	//
	// Separate from ScanCommand because the two produce different work. A finding
	// in your own code is a change to make; a finding in a dependency is usually a
	// version bump, sometimes an upstream problem with no fix available, and
	// occasionally something reachable only through a call path this project never
	// takes. A reviewer that cannot tell which it is reading gives bad advice about
	// both, so they are labelled separately in the evidence.
	SCACommand string
	// ScanCommand runs a static analyser over the pushed branch before the security
	// reviewer reads it, e.g. "gosec ./...".
	//
	// Its output becomes CONTEXT for the reviewer rather than a gate. A scanner
	// finds the things scanners are good at — hardcoded credentials, unchecked
	// errors, known-dangerous calls — deterministically, repeatably, and on a CPU.
	// Handing the model that output rather than making it eyeball a diff spends the
	// GPU on the part scanners cannot do: whether a finding matters here.
	ScanCommand string
	// TestCommand verifies a branch WITHOUT the platform, by running in the same
	// sandbox the agent already uses. It is the fallback when PipelineID is empty.
	//
	// This is a real trade-off, taken deliberately. It is a second definition of
	// "does this work" and it can drift from CI. What it buys is that the
	// department runs STANDALONE: a host with inference, a git remote and a forge
	// closes the loop on its own, so a platform that is unreachable — down, or
	// simply not somewhere the repository can be reached from — degrades
	// verification rather than stopping the agent.
	//
	// Keep it as close to the pipeline's own test step as possible, and set
	// PipelineID instead as soon as a pipeline exists that can reach the repository.
	TestCommand string
	// CoverageCommand reports test coverage, e.g. "go test -cover ./...".
	//
	// Separate from TestCommand because the two answer different questions and
	// the coverage stage needs a number rather than an exit code. A repository
	// that cannot report one simply has no coverage stage.
	CoverageCommand string
	// CoverageTarget is the percentage the coverage stage works toward.
	//
	// A TARGET, NOT A GUARANTEE. It is what makes the stage finish — these agents
	// do not stop on their own — and it is a proxy: 80% of statements executed is
	// not 80% of behaviour checked, and the stage is forbidden from editing the
	// specification tests precisely because the cheapest way to move this number
	// is to weaken what is already there.
	CoverageTarget int
	// BranchInput is the pipeline input carrying the branch to check out.
	BranchInput string
	// TimeoutSecs bounds a single sandbox run.
	TimeoutSecs int64
}

// agentMode selects which half of the work a DevAgent does.
//
// ONE LOOP, TWO JOBS. Writing an implementation and writing a test against it
// are the same mechanical task — survey the repository, read what matters, write
// whole files, run the gate — and this loop took a lot of live failures to get
// right: the iteration budget, the action history, the repeat refusals, stopping
// on green. Forking it would mean keeping those lessons in two places and losing
// one copy the first time only the other got fixed.
//
// What differs between the modes is small and explicit: what the agent is told
// to do, which files it may write, and which branch it starts from.
type agentMode int

const (
	// modeDevelop writes the change and may not touch tests.
	modeDevelop agentMode = iota
	// modeTest writes the tests and may not touch anything else.
	modeTest
	// modeSpecMerge reconciles the sections several authors wrote onto one
	// branch. Same permission as modeTest — tests only — and a different job:
	// make them compile together without weakening any of them.
	modeSpecMerge
	// modeCoverage adds tests for what the finished code actually contains, and
	// may not edit the tests that were there before it.
	modeCoverage
)

// DevAgent implements Handler.
type DevAgent struct {
	// role overrides the role this agent reports, which is what selects its
	// workflow stage. Empty means the mode decides — see Role.
	role          string
	gw            *Gateway
	api           *CodeArmory
	class         Class
	repo          RepoConfig
	maxIterations int
	// delegate names an external coding agent to run instead of this loop, or is
	// empty for the native loop. See agent_delegate.go.
	delegate string
	mode     agentMode
}

func NewDevAgent(gw *Gateway, api *CodeArmory, class Class, repo RepoConfig, maxIterations int) *DevAgent {
	if maxIterations < 1 {
		maxIterations = defaultMaxIterations
	}
	return &DevAgent{gw: gw, api: api, class: class, repo: repo, maxIterations: maxIterations,
		mode: modeDevelop, delegate: strings.TrimSpace(os.Getenv("AGENTS_DEV_ENGINE"))}
}

// NewTesterAgent builds the test author: the same loop, pointed at the tests.
//
// It takes its own class so it can run on a DIFFERENT MODEL from the developer.
// That is not a performance choice. A second opinion drawn from the same weights
// on the same branch is correlated with the first — it tends to accept the same
// reading of the ticket and miss the same cases — and the entire value of this
// stage is that it disagrees when the implementation is wrong.
func NewTesterAgent(gw *Gateway, api *CodeArmory, class Class, repo RepoConfig, maxIterations int) *DevAgent {
	if maxIterations < 1 {
		maxIterations = defaultMaxIterations
	}
	return &DevAgent{gw: gw, api: api, class: class, repo: repo, maxIterations: maxIterations, mode: modeTest}
}

// NewSpecAgent builds the SUB-TASK author: the test author, pointed at one slice
// of a task's specification and routed through its own columns.
//
// Identical work, different obligation. A sub-task exists so that slice actually
// gets written — a plan in a description is a suggestion, and a request planned
// as twelve sections produced one test file. Its tests land on its TASK's
// branch, so the task reaches development with the whole specification.
func NewSpecAgent(gw *Gateway, api *CodeArmory, class Class, repo RepoConfig, maxIterations int) *DevAgent {
	a := NewTesterAgent(gw, api, class, repo, maxIterations)
	a.role = roleSpec
	return a
}

// NewSpecMergeAgent builds the reconciler: the stage between several authors
// writing onto one branch and a developer being asked to satisfy the result.
//
// It exists because the authors cannot see each other. Two sections of one task
// each declared TestConcurrentAccess on the first run that assembled specs, the
// branch stopped compiling, and the developer inherited 86 turns of nothing —
// it may not edit tests, so no move it could make would have helped.
func NewSpecMergeAgent(gw *Gateway, api *CodeArmory, class Class, repo RepoConfig, maxIterations int) *DevAgent {
	if maxIterations < 1 {
		maxIterations = defaultMaxIterations
	}
	return &DevAgent{
		gw: gw, api: api, class: class, repo: repo,
		maxIterations: maxIterations, mode: modeSpecMerge, role: roleSpecMerge,
	}
}

// NewCoverageAgent builds the second test stage: the one that has seen the code.
//
// It runs AFTER the developer precisely so it can read the implementation, which
// is the opposite of the spec author's constraint and is why they are two stages
// rather than one agent run twice. What the spec author could not know is which
// branches the code actually grew; what this one must not do is touch the spec,
// since raising coverage by weakening an existing test is the cheapest way to
// hit a number and learn nothing.
func NewCoverageAgent(gw *Gateway, api *CodeArmory, class Class, repo RepoConfig, maxIterations int) *DevAgent {
	if maxIterations < 1 {
		maxIterations = defaultMaxIterations
	}
	return &DevAgent{gw: gw, api: api, class: class, repo: repo, maxIterations: maxIterations, mode: modeCoverage}
}

func (a *DevAgent) Role() string {
	if a.role != "" {
		return a.role
	}
	switch a.mode {
	case modeTest:
		return roleTest
	case modeCoverage:
		return roleCoverage
	}
	return roleDev
}

// writesTests reports whether this mode's business is test files.
func (a *DevAgent) writesTests() bool {
	return a.mode == modeTest || a.mode == modeCoverage || a.mode == modeSpecMerge
}

func (a *DevAgent) Class() Class { return a.class }

// Wants takes tickets that have been triaged and not yet worked.
//
// Triage first, deliberately: the product manager's summary and decomposition
// are the developer's brief, and a ticket that has not survived triage is one
// nobody has decided is worth doing.
// Wants accepts everything in ready_for_dev. Scoped-and-not-yet-built is what
// that column MEANS, and the dispatcher has already checked the ticket's
// dependencies are finished.
func (a *DevAgent) Wants(Ticket) bool { return true }

// branchMarker identifies the comment left when a branch is pushed, so a ticket
// is not worked twice.
const branchMarker = "**Developer agent pushed a branch.**"

// testsWrittenMarker is the test author's equivalent, and is deliberately NOT
// branchMarker. hasBranch reads branchMarker to mean "there is finished work
// here to review and merge", and tests without an implementation are the exact
// opposite of that.
const testsWrittenMarker = "**Test author pushed the tests.**"

// alreadySatisfiedMarker records a section whose tests passed against the code
// already on the branch, so there is nothing to implement and the branch is
// ready to merge as it stands.
//
// IT IS NOT testsWrittenMarker AND MUST NOT BE. A tests-only branch is
// deliberately not mergeable — the reviewer and integrator would take unfinished
// work, and there is a test pinning exactly that. This is the opposite case: the
// implementation exists, the tests pass against it, and the only thing left is to
// merge the tests themselves.
const alreadySatisfiedMarker = "**Already satisfied.**"

// coverageMarker records the second test stage. Distinct from the first so the
// two are legible apart on a ticket: one is the specification, the other is what
// the finished code turned out to need.
const coverageMarker = "**Coverage tests added.**"

// coverageShortfallMarker records a coverage stage that ran out of turns below
// its target. Deliberately not phrased as a failure: the work is untouched and
// still passes its specification.
const coverageShortfallMarker = "**Coverage below target.**"

// A BRANCH THE INTEGRATOR MAY TAKE. The developer's push is the ordinary case;
// the other is a section whose tests already pass against the code on the branch,
// which is finished work by a different route.
//
// A TESTS-ONLY BRANCH IS STILL NOT ONE, and that distinction is the point: the
// test author's marker is deliberately absent from this list, because a branch
// carrying failing tests and no implementation must not look mergeable.
//
// Measured on r86, where the missing case deadlocked a board: a section took the
// already-satisfied route to ready_for_integration, arrived with no marker the
// integrator recognised, and sat there. Its task waited on "sections 0/1 done"
// that would never come, and the three tasks behind it never started — with every
// ticket in a legitimate column and no stage having failed.
func hasBranch(t Ticket) bool {
	for _, c := range t.Comments {
		if strings.Contains(c.Body, branchMarker) || strings.Contains(c.Body, alreadySatisfiedMarker) {
			return true
		}
	}
	return false
}

// devState is what the agent has learned so far. It is rebuilt into the prompt
// each iteration rather than kept as a growing conversation: an ephemeral,
// task-scoped agent has no session, and a transcript that replays the whole
// history every turn is how a small model runs out of context.
type devState struct {
	// delegateExhausted records that the external agent had its attempts and
	// failed, so the native loop that follows is a fallback rather than the plan.
	// Kept on the state so the reporting can say which developer produced the
	// result — two agents on one ticket is otherwise invisible afterwards.
	delegateExhausted bool

	tree   []string
	read   map[string]string
	staged map[string]string
	// baseline is each file's content AS IT WAS ON DISK, captured on the first
	// read and never overwritten by the agent's own writes. read is not a
	// substitute: it is deliberately updated with staged content so the agent
	// sees its own edits, which means by the time a second write arrives there is
	// nothing left to compare against.
	//
	// It exists to answer one question — "is this write throwing away code that
	// was already there" — and that question needs the ORIGINAL.
	baseline map[string]string
	// lastTest holds the VERIFICATION OUTPUT and nothing else. Rejections go to
	// notice. They were one field once, and every rejection overwrote the test
	// result the model needed in order to justify finishing: the first refusal
	// erased the evidence that the code passed, and the failure comment reported
	// the refusal where the test output should have been.
	lastTest  string
	testsPass bool
	notice    string
	// writes counts accepted write_files actions, and verifiedWrites records the
	// count at the last verification. Equal means the tree has not changed since,
	// so verifying again cannot say anything new.
	writes         int
	verifiedWrites int
	// history is what this agent has already done and what came of it, as a
	// RECAP IT IS SHOWN rather than a conversation it appears to have had.
	//
	// Every turn used to be a fresh two-message prompt, which made the agent a
	// stateless function: it could not remember trying something, so it re-derived
	// the same diagnosis and re-made the same edit. Read from the transcript rather
	// than inferred — turns 195 and 197 of one attempt carry byte-identical prose,
	// as do 204 and 205, and the branch shows four consecutive commits removing the
	// same duplicate methods before it deleted too much and lost Update and Delete
	// entirely. Its reasoning was correct every single time; it simply had amnesia.
	//
	// Only the ACTION and a one-line outcome are kept, never the rendered state:
	// the state carries whole file contents, and replaying that per turn would
	// exhaust the slot within a few exchanges.
	//
	// Held as rendered LINES, not as messages, because which role carries them
	// turned out to matter more than their wording. See recentHistory.
	history []string
	// historyBase is the last trail entry before any repeat marker, and
	// historyRepeats counts how many times it has arrived unchanged. Together they
	// collapse a loop into one line — see remember.
	historyBase    string
	historyRepeats int
	// failedVerifications counts verifications that came back red this attempt,
	// whatever the reason.
	//
	// SEPARATE FROM specBrokenTries ON PURPOSE. That counter only advances inside
	// the compile-error branch, which is the very case the referee exists to
	// complement — gating the referee on it meant a semantic failure, where the
	// tests compile and merely assert against unreachable state, could never reach
	// it. Shipped that way and measured: zero verdicts on the ticket it was built
	// for.
	failedVerifications int
	// ticket is what this attempt is working, kept so a check deep in verification
	// can describe the job without every function on the path growing a parameter
	// for it. See refereeOn.
	ticket Ticket
	// referee holds the verdict on whose fault the failure is, once asked. Cached
	// for the attempt because the question is about the ticket's shape rather than
	// the turn, and the answer does not change between turns.
	referee      *refereeVerdict
	refereeAsked bool
	// undoStack is the staged tree as it stood before each accepted write, newest
	// last, so undo_edit can put a file back. See actionUndoEdit for why this
	// matters more than the choice of edit format.
	undoStack []map[string]string
	// verifiedTree is a fingerprint of the staged tree AS IT WAS when a
	// verification last actually ran. It replaces comparing write counters, which
	// desynchronised nine separate times across today's runs — a no-op counted as
	// a write, a push with nothing to commit returning early, a gate rejection
	// skipping the update. Each was a different code path forgetting to keep two
	// numbers in step.
	//
	// A fingerprint cannot forget: it is derived from the tree rather than
	// maintained alongside it, so there is nothing to keep in step.
	verifiedTree string
	// refunded counts turns given back for READS — all of them, not only the ones
	// that returned nothing new. Reading does not spend the budget, so this grows
	// without bound and maxTotalIterations is what ends a runaway.
	refunded int
	// consecutiveReads counts reads taken in a row, reset by any other action.
	// Reading no longer spends the budget, so this is what ends an attempt that
	// does nothing else — see maxConsecutiveReads.
	consecutiveReads int
	// staleReads counts consecutive reads that returned nothing new. It escalates
	// ADVICE rather than refusing, because a re-read is legitimate — see doRead.
	staleReads int
	// missing records paths the repo did not return, so a file that does not
	// exist is not fetched again on every iteration.
	missing map[string]bool
	// refusals counts CONSECUTIVE actions that changed nothing. Any productive
	// action resets it. See maxRepeatRefusals.
	refusals int
	// noopEdits counts CONSECUTIVE edits that left the tree byte-identical. Kept
	// apart from refusals because the remedy is specific — run the tests, the work
	// is probably already done — and only worth escalating to once it repeats.
	noopEdits int
	// onRefusal reports each refusal to telemetry. Set at construction, nil-safe
	// so the many tests that build a devState directly need no wiring.
	onRefusal func(kind, notice string)
	// testEditRefusals counts attempts to edit a test file. The developer may not,
	// but WANTING to, over and over, while the tests are what fail to compile, is
	// evidence the specification is the broken thing — see maxTestEditRefusals.
	testEditRefusals int
	// lastCompileBroke names the test files that failed to compile at the most
	// recent verification, recorded whether or not the specBrokenTries streak has
	// advanced. Without it a refusal has nothing to corroborate itself against.
	lastCompileBroke string
	// lastTestEditTarget is the test file the developer most recently tried to
	// edit. It names the suspect when the tests FAIL rather than fail to compile,
	// where there is no compiler output to name one.
	lastTestEditTarget string
	// redIsExpected records that the last failure was ONLY "undefined:" errors —
	// the normal red state of a test-first ticket, cleared by writing the code.
	// Without it the widened evidence route fires on every healthy ticket: the
	// developer pokes at the test file, the tests are failing (as they must), and
	// a perfectly good specification gets handed back.
	redIsExpected bool
	// parseFails counts CONSECUTIVE unparseable replies, which are bounded
	// separately and more loosely — see maxParseFailures.
	parseFails int
	// lastReply is the model's most recent raw reply, kept only so a failure can
	// quote what it could not parse. Bounded at capture time — it is destined for
	// a ticket comment, not a log.
	lastReply string
	// specBroken names the test files whose COMPILE errors the developer cannot
	// fix, because it may not edit tests. Empty until the developer has TRIED and
	// failed minSpecBrokenTries times — see the detector for why a filename alone
	// is not evidence.
	specBroken string
	// specBrokenTries counts verifications that came back with test-file-only
	// compile errors AFTER the developer changed something. specBrokenWrites is
	// the write count at the last such verification, so that repeated runs
	// against an unchanged tree do not advance the streak.
	specBrokenTries  int
	specBrokenWrites int
	// resets counts how many times this attempt has been restarted, and
	// lastResetAt is the iteration the last one happened on. Bounded by
	// maxAgentResets.
	resets      int
	lastResetAt int
	// restart is the brief handed to a freshly reset agent. Separate from notice
	// because notice renders under "YOUR LAST ACTION WAS NOT ACCEPTED" and a
	// restart is not a rejection — the agent being briefed did not take the last
	// action, and telling it that it did would be the first thing it learns and
	// the first thing that is false.
	restart string
	// pushFails counts CONSECUTIVE failures to push the branch. Separate from
	// every other counter because it measures the plane rather than the agent:
	// no action the model takes can change it, so it must not feed the guards
	// that judge the model's progress.
	pushFails int
	// repairs names files the harness corrected on the agent's behalf. Reported
	// to the model, but never as a rejection: the write SUCCEEDED, and spending a
	// turn to tell it otherwise is the cost this exists to avoid.
	repairs []string
	// truncated records that the last reply hit the token limit rather than
	// finishing. A cut-off reply and a malformed one are indistinguishable
	// afterwards and need opposite advice, so the distinction is kept here.
	truncated  bool
	lastRun    string
	summary    string
	commitType string
	iteration  int
	// budget is how many iterations this attempt gets in total. It is carried on
	// the state only so the prompt can SHOW it: a model told "ITERATION 5" cannot
	// tell whether three turns remain or thirty, so it has no reason to economise,
	// and the observed result was attempts that read one file per turn and ran out
	// of turns before ever verifying. Zero means unknown, and the denominator is
	// left off rather than printed as "OF 0".
	budget int
	// trail records the actions this attempt took, in order.
	//
	// It has TWO readers, and both need it. The person reading the ticket needs it
	// because an exhausted attempt otherwise reports only its last test output, and
	// an attempt that ended on a write has no test output at all — so the ticket
	// says "stopped after 8 iterations" and nothing else, which is unusable.
	//
	// The MODEL needs it because each turn is rendered from scratch, so without it
	// the model cannot see its own past actions and has no way to notice it is
	// repeating one. Observed live: an attempt whose first two actions were a
	// byte-identical read of the same three files, then a write, then a read of a
	// file it had just written — eight turns spent, nothing verified. Everything
	// else in the state describes the REPOSITORY; this is the only part that
	// describes what the agent has already spent its turns on.
	trail []devStep
	// sb is where this ticket's commands run: one held sandbox for the whole
	// attempt when forge can give us one, a fresh container per command when it
	// cannot. Every script the agent builds assumes it is standing in the
	// repository, and the sandbox is what makes that true either way.
	sb *Sandbox
}

// devStep is one action an attempt took. The detail is what distinguishes two
// actions of the same kind — "read_files" twice tells the model nothing, while
// "read_files(main.go, go.mod)" twice tells it exactly what it wasted a turn on.
type devStep struct {
	action string
	detail string
}

func (s devStep) String() string {
	if s.detail == "" {
		return s.action
	}
	return s.action + "(" + s.detail + ")"
}

// stepDetail summarises an action's arguments for the history line. Kept short
// on purpose: the history is replayed every turn, so it is charged for on every
// one of the few turns the attempt has.
// devTemperature warms the sampler as a stuck agent accumulates evidence of it.
//
// IT TAKES BOTH COUNTERS, and keying it on refusals alone left the hole this was
// written to close. A stale re-read is REFUNDED rather than refused — reads are
// meant to be free — so it never touched the refusal count, temperature stayed at
// 0, and the model deterministically re-emitted the same read. Measured on the
// implementation fixture immediately after the first version shipped: 66 of 68
// turns were identical reads of main.go, with refusals sitting at 1.
//
// Anything that says "this agent is not progressing" has to feed the ramp, or the
// fixpoint simply moves to whichever counter was left out.
//
// The ramp is deliberately steep and capped: the first refusal is often a simple
// mis-step that the explanation fixes, but by the third the model is demonstrably
// in a fixpoint and needs to be able to produce something else at all. Above ~0.8
// a code model starts inventing identifiers, which trades one failure for a worse
// one.
func devTemperature(refusals int) float64 {
	if refusals <= 0 {
		return 0
	}
	// CAPPED LOW, and 0.8 was measured doing real damage. The reply carries both
	// code and PRECISE INTEGERS — the line range — and the integers cannot tolerate
	// heat that the prose can. Across nineteen attempts before this ramp existed,
	// syntax breaks ran 0-6 per attempt and inverted ranges were almost unknown;
	// the three attempts after it shipped at 0.8 produced 17-27 syntax breaks and
	// up to 15 ranges whose end line preceded their start.
	//
	// The job here is only to break a deterministic fixpoint, and that needs ONE
	// different token, not a different personality. 0.3 is enough to make the
	// sampler non-degenerate while leaving the structure intact.
	t := 0.1 * float64(refusals)
	if t > 0.3 {
		t = 0.3
	}
	return t
}

func stepDetail(act devAction) string {
	switch act.Action {
	case actionReadFiles:
		return clip(strings.Join(act.Paths, ", "), 160)
	case actionWriteFiles:
		// THE PATH ALONE MADE EVERY WRITE LOOK THE SAME. The trail tells the agent
		// "do not repeat an action", and for a write it recorded only the file name
		// — so twenty attempts at twenty different payloads and twenty attempts at
		// the SAME payload were indistinguishable in its own history.
		//
		// The agent has no conversation memory: each turn is a fresh two-message
		// prompt built from state, so this list is the only record it has of what it
		// already tried. A content fingerprint is what makes "I have written exactly
		// this before" visible to it. Measured on r66: 75 writes byte-identical to
		// one already accepted, none of them legible as repeats.
		parts := make([]string, 0, len(act.Edits))
		for _, e := range act.Edits {
			sum := sha256.Sum256([]byte(e.Replace))
			parts = append(parts, fmt.Sprintf("%s lines %d-%d, %dB [%x]",
				e.Path, e.StartLine, e.EndLine, len(e.Replace), sum[:3]))
		}
		return clip(strings.Join(parts, "; "), 260)
	}
	return ""
}

// noProgress records an action that cannot have changed anything, and is the
// only way refusals goes up. Routing every such case through one method is what
// keeps the loop breaker honest: a new rejection added later counts toward
// termination automatically, rather than becoming another way to spin.
// recordVerdict files what the checks decided and resets what that decision has
// made stale.
//
// A VERDICT THAT HAS NOT MOVED IS NOT PROGRESS, however much the tree has. This
// cleared all four counters on any verdict at all. That is right when the verdict
// CHANGED — the agent learned something and the counters describe a state it has
// left — and wrong when the identical failure comes back, because then the only
// thing that moved is the tree.
//
// Clearing refusals there defeats two mechanisms with one line. The loop breaker
// never accumulates toward its ceiling, so an agent alternating between two
// states runs to its whole budget; and devTemperature never warms past 0, so a
// deterministic sampler keeps re-emitting the reply that just failed. The escape
// hatch and the backstop are both keyed to this counter.
//
// r95's branch is the shape in its own commit log — "add a /nonexistent route",
// "remove the /nonexistent route handler", "add a /nonexistent route". r102 ran
// 35 turns with 19 refusals, 17 of them the identical parse rejection, and was
// still going when it was stopped.
//
// The other three clear unconditionally: a write that landed did move the tree,
// so a run of byte-identical edits and a run of stale reads are both genuinely
// over, whatever the gate then said about it.
//
// A METHOD RATHER THAN FOUR LINES IN THE LOOP, because the loop cannot be driven
// to this state by the test fake — it models one canned stdout for every command
// and has no file contents, so an agent cannot be made to land alternating edits
// through it. The decision is testable even where the path to it is not.
func (s *devState) recordVerdict(out string, pass bool) {
	if out != s.lastTest {
		s.refusals = 0
	}
	s.lastTest, s.testsPass = out, pass
	s.notice, s.staleReads, s.noopEdits = "", 0, 0
	s.forgetReadsOfFilesItDidNotWrite()
}

// forgetReadsOfFilesItDidNotWrite drops cached reads that the branch may have
// moved underneath.
//
// A verification resets the working tree to the branch, so anything the agent
// merely READ can have changed — a test file its author repaired, a file another
// stage merged. Its own staged writes are re-applied on top, so those still match
// what it holds and stay cached; re-fetching them would cost a sandbox round trip
// to be told what it just wrote.
//
// Without this an agent keeps a file for the life of an attempt: doRead skips any
// path already in the cache, deliberately, because re-reading is meant to be
// free. Free is right; permanent is not.
func (s *devState) forgetReadsOfFilesItDidNotWrite() {
	for p := range s.read {
		if _, mine := s.staged[p]; !mine {
			delete(s.read, p)
		}
	}
	for p := range s.missing {
		if _, mine := s.staged[p]; !mine {
			delete(s.missing, p)
		}
	}
}

func (s *devState) noProgress(notice string) {
	s.refusals++
	s.notice = notice + s.directive()
	// Hooking the funnel rather than the call sites is the same reasoning that
	// put every refusal through here: a rejection added later is recorded
	// automatically instead of staying invisible until someone notices the gap.
	// It was invisible once already — r56 failed four developer attempts at
	// twenty refusals each and reported a refusal count of ZERO, because the
	// metric had been wired to the specification gates and nothing else.
	kind := refusalKindOf(notice)
	if kind == RefusalTestFile {
		s.testEditRefusals++
		if f := testFileInNotice(notice); f != "" {
			s.lastTestEditTarget = f
		}
		// Concluded HERE rather than only at the next verification, because an
		// agent in this state may never reach one: it is being refused, not
		// working, and the refusal ceiling arrives first.
		//
		// A COMPILE ERROR IS NOT THE ONLY WAY A SPECIFICATION CAN BE IMPOSSIBLE,
		// and requiring one kept this shut when it was most needed. r55's spec
		// compiled perfectly and asserted its own opposite; r61's compiled and
		// simply failed, and the developer spent 36 refusals trying to edit the
		// test, 24 trying to re-run it, and 3 trying to finish — every legal move
		// refused, with no way to say what it had found.
		//
		// So failing tests count as well as uncompilable ones. This is a weaker
		// signal and it is admitted deliberately: the alternative is not a correct
		// diagnosis, it is a dead board. Sending a section back to its author costs
		// one stage and is useful even when the specification turns out to be fine;
		// stranding nineteen tickets costs the run. maxSpecRepairs still bounds it.
		// NOT WHEN THE RED IS THE EXPECTED RED. "undefined: NewStore" is what a
		// specification written before its implementation MUST produce, and it is
		// cleared by writing the code — so a developer poking at the test file while
		// that is the only failure is out of bounds, not onto something. Measured on
		// the implementation fixture: a healthy spec handed back twice.
		// A TEST THAT RUNS PROVES THE SPECIFICATION COMPILED, so a failure from that
		// point on belongs to the implementation. The weaker route this replaces —
		// "the tests fail and it keeps trying to edit them" — fired on exactly that:
		// measured on the implementation fixture, a nil-map panic in the developer's
		// own main.go was reported as "specification is unsatisfiable" and the ticket
		// was blocked for a person while the spec compiled cleanly.
		//
		// It was admitted knowingly, to catch a specification that compiles and still
		// cannot be satisfied — r55's inverted assertion. That case is real, but it is
		// caught at AUTHORING time by invertedErrorAssertions, which is where it
		// belongs: the developer cannot tell "this assertion is impossible" from "my
		// code is wrong", and guessing costs healthy tickets.
		//
		// So the evidence is now test-file COMPILE faults only.
		if s.testEditRefusals >= maxTestEditRefusals && !s.redIsExpected && s.lastCompileBroke != "" {
			s.specBroken = s.lastCompileBroke
		}
	}
	if s.onRefusal != nil {
		s.onRefusal(kind, notice)
	}
}

// refusalKindOf maps a refusal notice onto the closed label set. Anything
// unrecognised becomes "other" — see telemetry_metrics.go on why a metric label
// may never carry free text.
func refusalKindOf(notice string) string {
	n := strings.ToLower(notice)
	switch {
	// Both tenses, deliberately: the first refusal says "already contained" and
	// the escalation says "already contains". Matching one tense only would put
	// the escalated message in "other" and make the noop_edit series undercount
	// the exact failure it exists to measure.
	case strings.Contains(n, "produced no change"), strings.Contains(n, "already contain"),
		strings.Contains(n, "changed nothing"), strings.Contains(n, "not changed anything yet"):
		return RefusalNoopEdit
	case strings.Contains(n, "test file"), strings.Contains(n, "edit tests"):
		return RefusalTestFile
	case strings.Contains(n, "search text was not found"), strings.Contains(n, "nothing to search in"):
		return RefusalSearchMiss
	case strings.Contains(n, "could not read"):
		return RefusalMissingFile
	case strings.Contains(n, "cannot finish"), strings.Contains(n, "must pass before finishing"):
		return RefusalPrematureFinish
	case strings.Contains(n, "same bytes"), strings.Contains(n, "re-read"):
		return RefusalStaleRead
	case strings.Contains(n, "parse"), strings.Contains(n, `no "action" field`):
		return RefusalUnparseable
	}
	return RefusalOther
}

// directive escalates a refusal into an instruction.
//
// A refusal on its own is passive: it says what the last action could not
// achieve and leaves the model free to choose it again, which is exactly what a
// looping model does. Observed on a live ticket — the model read two files, was
// told four times that re-reading returns the same bytes, and asked a fifth
// time; the ticket ended with nothing written and nothing verified, which is the
// worst outcome available because it burns a whole attempt on no work at all.
//
// So the second and later refusals name the permitted actions instead of the
// forbidden one, and say how close the attempt is to being abandoned. The first
// refusal stays plain: a model that simply mis-stepped recovers from the
// explanation alone, and shouting at it immediately would be noise on the common
// path.
func (s *devState) directive() string {
	if s.refusals < 2 {
		return ""
	}

	// WHICH ACTION TO DEMAND DEPENDS ON WHAT IS PENDING, and getting this wrong
	// traps the agent between two guards giving opposite orders. Observed exactly
	// that: the unverified-write guard said "call run_tests now", this directive
	// said "your next action MUST be write_files", and the attempt was abandoned
	// four refusals later having been told to do two contradictory things.
	//
	// THERE IS ONLY ONE ACTION THAT CAN MAKE PROGRESS NOW. A verification used to
	// be the answer when writes were piling up unchecked; it is not something the
	// agent can choose any more, and naming it cost 28 turns on the dev fixture
	// before this was noticed. The checks follow a real edit by themselves.
	next := "write_files (make a change that is actually different)"
	// NO THREAT OF ABANDONMENT, because repetition no longer abandons anything —
	// only the budget ends an attempt. Saying otherwise was true when four
	// refusals killed a ticket and is a lie now, and a prompt that threatens a
	// consequence it cannot deliver teaches the model to discount the next one.
	// What is true, and worth saying, is that the turns are being spent.
	return fmt.Sprintf("\n\nYou have now made %d actions in a row that changed nothing,"+
		" and each one has cost a turn from your budget. Your next action should be %s.",
		s.refusals, next)
}

// maxRepeatRefusals is how many times the agent may ask to re-verify unchanged
// code before the loop ends it.
//
// Refusing a repeat verification made the mistake cheap; it did not make it
// stop. A model that has passed its tests and cannot bring itself to say
// "finish" will ask again for as many iterations as it is given, and the ticket
// fails at the cap with working, pushed, passing code sitting on the branch.
// That is a wrong answer to a solved problem. So after this many refusals the
// loop draws the conclusion the model will not: if the tests pass and files are
// staged, finish; otherwise stop, because more turns will not help.
// FOUR, not two. The bound was two when a repeated action cost a SANDBOX; it no
// longer does — a refused action is caught before any execution and costs one
// model call of about a second. So the two sides of this trade changed places:
// being too generous now wastes seconds, while being too strict kills a ticket
// outright.
//
// Two was observed doing exactly that. A model read the files it needed, asked
// for them twice more, and was stopped at turn three of eight without ever
// having written a line — the guard meant to stop waste became the reason two
// tickets reached nobody.
const maxRepeatRefusals = 4

// maxDeadRefusals ends an attempt that is refusing with nothing to show for it.
// Five times maxRepeatRefusals: far enough past the low ceiling that an agent
// recovering from a run of mistakes is never cut off, close enough that a
// deadlock costs twenty turns rather than two hundred.
const maxDeadRefusals = 20

// deadRefusalCeiling is maxDeadRefusals, overridable, and ZERO TURNS IT OFF.
//
// It is a knob because whether ending an attempt here helps is an open question
// rather than a settled one. The ceiling only began firing at all once the
// counters stopped being cleared by a write that had not landed, and since then
// every run has ended its first developer attempt on it — which bounds the waste
// but also throws away whatever context that attempt had built. r96 did the same
// work in six turns with the ceiling never reached, so "bounded waste is better
// than none" is an assumption this makes measurable instead of arguing about.
//
// Off means the turn budget is the only bound, which is what it was before.
func deadRefusalCeiling() int {
	v := strings.TrimSpace(os.Getenv("AGENTS_DEV_MAX_DEAD_REFUSALS"))
	if v == "" {
		return maxDeadRefusals
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return maxDeadRefusals
	}
	return n
}

// defaultMaxIterations is the turn budget when nothing else is configured.
//
// FIFTY, NOT EIGHT. Eight was chosen when a wasted turn was expensive and the
// loop had no guard against spending the whole allowance on one pathology. Both
// have changed: unverified writes are refused, repetition no longer ends an
// attempt, and re-reads cost a turn rather than a sandbox. Measured on the same
// ticket, eight finished nothing, twenty-four finished three, and forty-eight
// finished four — the extra tickets bought with wall-clock, which is the cheap
// resource for a department that runs in the background.
//
// A HUNDRED IS A DIAGNOSTIC CEILING, not a working allowance. A normal ticket
// finishes in single figures, so reaching this number does not mean the agent
// needed a hundred turns — it means something is wrong, and the ticket is worth
// reading rather than retrying.
//
// It can be that high because the thing that used to make a long budget harmful
// is fixed. An agent now ENDS when it is done: told "the checks passed, call
// finish", it finishes ten times out of ten, against one finish in four thousand
// turns when the prompt left that implicit. The old worry — that a roomy budget
// just buys more looping — was a symptom of the loop never being told to stop.
//
// The reserve that used to sit below this is gone. It was tried and ignored:
// every reserve turn in a live run went to read_files, not one to run_tests or
// give_up, so it changed the prompt and nothing else.
// maxReplyTokens is the output ceiling for one turn, SIZED TO THE STAGE.
//
// One number for every agent was wrong in both directions. At 4000 the developer
// truncated mid-way through creating a whole file — the reply decoded as
// "unexpected end of JSON input" and cost the entire attempt. Raised to 8000 it
// stopped truncating and started something worse: the ceiling is also what
// bounds VERBOSITY, and the spec author, whose output is one test file, simply
// wrote until it hit the limit.
//
// Measured on the dense 14B: four spec authors in lockstep, 792 tokens decoded
// with 7208 still allowed, six minutes into a single turn and nothing finished.
// On the faster MoE the same ceiling was nearly invisible, which is exactly how
// a setting like this hides.
//
// So the ceiling follows the work. The developer creates whole files and needs
// room; the spec author writes one test file and does not. It also has to leave
// space for the PROMPT — a slot holds 16384 tokens of prompt and reply together,
// and a developer prompt carrying the architecture docs and every file read has
// been measured at 8583.
func (a *DevAgent) maxReplyTokens() int {
	switch a.mode {
	case modeTest, modeCoverage, modeSpecMerge:
		return maxTestReplyTokens
	default:
		return maxDevReplyTokens
	}
}

const (
	// maxDevReplyTokens is the reply ceiling for every stage that writes a FILE,
	// and it is now the same number for all of them.
	//
	// TRUNCATION IS NOT A SMALLER ANSWER, IT IS NO ANSWER, and that is what a
	// stage-specific ceiling kept producing. Measured this run: the specification
	// author stopped at exactly 5000 on four consecutive turns and the attempt
	// ended "unparseable model output" — a whole file lost each time — while the
	// product manager stopped at exactly 3700 twice and blocked its request with
	// "decode triage: unexpected EOF".
	//
	// A smaller ceiling also frees nothing. max_tokens is a stop condition, not a
	// reservation: how many stages run at once is the class's slot count, so
	// trimming one stage's ceiling buys no capacity for another.
	//
	// The 6000 it replaces was sized against a 16384-token slot on llama-server.
	// This deployment serves 131072, so that constraint is gone; 7500 still clears
	// it with the largest prompt ever measured (8583).
	maxDevReplyTokens = 7500
	// maxTestReplyTokens is the same number, deliberately.
	//
	// It was held BELOW the developer's because at the developer's ceiling the
	// author did not stop early — measured on the dense 14B, four authors in
	// lockstep at 792 tokens with 7208 still allowed, six minutes into one turn.
	// That is a real cost and it is the smaller one: rambling spends turns, where
	// truncating loses the file and the attempt with it. On this model the
	// author truncated at 5000 on four consecutive turns before the attempt died.
	//
	// If the rambling returns on this model, it is worth measuring again — but as
	// a prompt problem, not as a ceiling that also breaks the honest case.
	maxTestReplyTokens = maxDevReplyTokens
)

// defaultMaxIterations is the turn budget for one attempt.
//
// RAISED FROM 100 once the turns started being spent on real work. At 100 the
// budget was going on harness noise — truncated replies retried verbatim, no-op
// re-reads, assertions buried under panic stacks — so more turns only bought
// more of the same. With those fixed, a run that solves the ticket at turn 65
// and one still working at turn 95 are not far apart, and the question worth
// answering is whether this model needs twenty more turns or cannot do it at all.
const defaultMaxIterations = 200

// maxStaleReads is how many consecutive no-op reads pass before the advice
// becomes blunt. It does not end the attempt: only the budget does that, and a
// model re-reading is not a model doing nothing wrong.
const maxStaleReads = 3

// maxRefundedReads is no longer a bound on refunds — EVERY read is refunded now,
// so the count is unbounded by design and maxTotalIterations is what guarantees
// the loop terminates. It survives as the point at which an attempt has clearly
// stopped doing anything but read, which is worth reporting even though nothing
// is refused for it.
const maxRefundedReads = 25

// maxParseFailures bounds CONSECUTIVE replies that are not valid actions.
//
// Looser than maxRepeatRefusals because it is a different failure. A model that
// repeats a pointless action is not going to stop; a model that emits a stray
// code fence or a truncated object usually produces a clean action next turn
// once it is handed the error, and ending the task at the second one would throw
// away recoverable work. It exists at all because the failure is otherwise
// silent and fast: a dev agent once spent its whole iteration budget, and the
// ticket's whole attempt budget, in twenty seconds without running anything.
const maxParseFailures = 4

// hasTestFile reports whether the survey found any test file.
//
// Asked before fetching anything: a branch with no tests has nothing to review,
// and finding that out should not cost a sandbox round trip.
func hasTestFile(tree []string) bool {
	for _, p := range tree {
		if isTestFile(p) {
			return true
		}
	}
	return false
}

func (a *DevAgent) Handle(ctx context.Context, t Ticket) (string, string, error) {
	if a.repo.URL == "" {
		return OutcomeFailed, "", errors.New("dev agent: no repository configured")
	}

	rec := recorderFrom(ctx)
	state := &devState{
		read: map[string]string{}, staged: map[string]string{}, missing: map[string]bool{},
		baseline: map[string]string{},
		budget:   a.maxIterations,
		ticket:   t,
	}
	if rec != nil {
		state.onRefusal = func(kind, notice string) {
			// THE COUNTERS GO IN THE RECORD, because reading the code has twice
			// failed to explain why a ceiling did not fire. A transcript that says
			// a refusal happened but not what it counted toward leaves the only
			// question worth asking unanswerable from the evidence.
			rec.Refusal(ctx, kind, a.role, fmt.Sprintf("[refusals=%d noop=%d stale=%d iter=%d writes=%d/%d lastTest=%dB] %s",
				state.refusals, state.noopEdits, state.staleReads, state.iteration,
				state.verifiedWrites, state.writes, len(state.lastTest), notice))
		}
	}

	// One sandbox for the whole ticket. A developer task runs a survey, several
	// reads and several verify passes against ONE checkout, and holding the
	// sandbox across them removes a microVM boot and a clone from every command
	// but the first — which measured as most of a task's wall time.
	//
	// It is acquired per TICKET and never reused across them: a sandbox that
	// outlived one ticket would carry its working tree, and anything a prompt
	// injection had done in it, into the next.
	sb, err := a.api.AcquireSandbox(ctx, SandboxRequest{
		Image:           a.repo.Image,
		RunnerClass:     a.repo.RunnerClass,
		TimeoutSecs:     a.repo.TimeoutSecs,
		CloneURL:        a.repo.URL,
		SecretRef:       a.repo.SecretRef,
		Branch:          a.repo.Branch,
		IdleTimeoutSecs: a.repo.LeaseIdleSecs,
		MaxLifetimeSecs: a.repo.LeaseMaxSecs,
	})
	if err != nil {
		// Reported rather than worked around. This used to fall back to a container
		// per command, which kept the ticket moving at roughly a tenth of the speed
		// and told nobody — see AcquireSandbox.
		return OutcomeFailed, "", fmt.Errorf("%s: %w", a.Role(), err)
	}
	state.sb = sb
	// Released on EVERY exit path, including the panicking one. A held sandbox is
	// a runner class's worth of memory, and the timeouts that would eventually
	// reclaim it are a backstop for a crashed agent — not a substitute for a
	// stage that simply finished.
	//
	// The release deliberately does not use ctx: by the time this runs ctx is
	// often already cancelled, and that is exactly when releasing matters most.
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), leaseReleaseTimeout)
		defer cancel()
		state.sb.Release(releaseCtx)
	}()

	// PICK UP THE TICKET'S BRANCH IF IT ALREADY EXISTS, before anything reads the
	// tree. The sandbox clones the base branch, which was right when development
	// was the first stage and is wrong now: the test author runs first and pushes
	// the tests to this ticket's branch, so a developer surveying the base would
	// not see the tests it is supposed to satisfy, and would then create the
	// branch afresh from the base and force-push the tests away.
	//
	// Doing it here rather than in the lease's CheckoutSpec is deliberate: the
	// lease checks out the BASE branch, which is the right thing to clone and the
	// wrong place to start work. This runs afterwards and is a no-op when the
	// branch does not exist yet, which is the normal first case.
	state.sb.AdoptBranch(a.branchFor(t))
	// AND ITS DEPENDENCIES. Scheduling held this ticket until its blockers
	// merged; without this the branch still predates them and the developer
	// starts against code that does not exist.
	state.sb.AlsoMerge(a.mergeIntegrationScript())

	// DELEGATE THE DEVELOPER'S INNER LOOP, if configured. Everything above this
	// line is what the stage needs either way — the lease, the branch, the merged
	// dependencies — and everything the delegated path then uses is the same
	// verification, hand-back and reporting a native attempt uses. Only the
	// "decide what to edit and write it" middle is replaced.
	if a.delegate != "" && a.mode == modeDevelop {
		status, detail, derr := a.runDelegated(ctx, rec, t, state)
		if derr != nil || status != OutcomeFailed {
			return status, detail, derr
		}
		// TWO AGENTS FAIL DIFFERENTLY, and that is the whole value of the fallback.
		//
		// Measured on a clean run: one section — "Add must assign task-0" — beat the
		// delegated agent SIX times in a row, three different ways, and never once
		// looked at the constructor. It then blocked, and seventeen tickets that
		// depended on it blocked behind it: 89% of the board destroyed by one
		// agent's blind spot. The board has no tolerance for a single dead section,
		// so the cheapest place to add tolerance is before it dies.
		//
		// The native loop is a genuinely different strategy — its own prompt, its own
		// edit tools, its own verification after every write — and the two have
		// already been observed failing on different tickets. It also inherits the
		// delegate's pushed work rather than starting over, so this is a change of
		// approach on the same branch, not a restart.
		//
		// One pass only, and only after the delegate is out of attempts: this is the
		// last thing tried before a ticket is declared dead.
		slog.InfoContext(ctx, "delegate exhausted; falling back to the native developer",
			"ticket_id", t.TicketID, "detail", detail)
		rec.Action(ctx, "delegate", "exhausted after "+strconv.Itoa(maxDelegateAttempts)+
			" attempts; handing the branch to the native developer", nil)
		a.comment(ctx, t, "**Handing over to the built-in developer.** The delegated agent could not make "+
			"these tests pass in "+strconv.Itoa(maxDelegateAttempts)+" attempts. Its work is on the branch and "+
			"the built-in developer continues from there — a different approach on the same code, rather than "+
			"a ticket declared dead.")
		state.delegateExhausted = true
	}

	tree, err := a.listRepo(ctx, rec, state)
	if err != nil {
		return OutcomeFailed, "", fmt.Errorf("dev agent: survey repo: %w", err)
	}
	state.tree = tree

	// THE SPECIFICATION IS READ BEFORE ANY CODE IS WRITTEN, and handed back if no
	// implementation could satisfy it.
	//
	// The ordinary referee needs three failed verifications first, and by then the
	// attempts are spent. Measured on a live board: a section whose tests asserted
	// on doc comments — using a parser the test itself had given no comment mode,
	// so the comments were discarded before the assertion ran — took a developer
	// that wrote correct doc comments, watched them fail, and rewrote the same
	// file. Its own reasoning said so plainly: "it DOES have doc comments". It was
	// right every time and could not win, because it may not edit the test.
	//
	// One call, no tools, no sandbox, and it cannot change the branch it judges.
	// It is deliberately biased towards silence: undefined symbols are the normal
	// state of test-first work, a false "spec" costs one of two repairs, and the
	// developer's own attempt is worth more than a maybe.
	//
	// The survey already says whether there are any tests. Asking it first means a
	// branch with none — a create, or the fake in a unit test — costs no sandbox
	// round trip at all, rather than fetching a repository to discover there is
	// nothing to review.
	if a.mode == modeDevelop && !state.delegateExhausted && hasTestFile(tree) {
		if files, ferr := a.readRepoGoFiles(ctx, rec, state); ferr == nil {
			tests := map[string]string{}
			for path, content := range files {
				if isTestFile(path) {
					tests[path] = content
				}
			}
			if v := a.askSpecPreflight(ctx, rec, t, tests); v.blames(ownerSpec) {
				state.specBroken = v.Reason
				if status, detail, done := a.endOnBrokenSpec(ctx, t, state, ""); done {
					return status, detail, nil
				}
			}
			// THE FETCH IS NOT WASTED. The developer opens on the tests every time
			// — it cannot satisfy them without reading them — so seeding the cache
			// here makes this the first read rather than an extra one, and the
			// sandbox is touched once instead of twice. Only the TESTS: seeding
			// every file would put the whole repository in every prompt.
			for path, content := range tests {
				if _, known := state.read[path]; !known {
					state.read[path] = content
					state.baseline[path] = content
				}
			}
		}
	}

	// NOTHING TO RECONCILE IS THE COMMON CASE, AND IT MUST COST NOTHING. The
	// reconciler exists for sections that collide, and most sets of sections do
	// not: on the run that produced this check, three sections compiled together
	// perfectly and the stage still spent 300 turns reading, three times over,
	// looking for a defect that was not there. A model given a job that is
	// already done does not conclude that it is done.
	//
	// So the gate is asked FIRST, before any model call. If the assembled
	// sections already compile as one package — failing on assertions and
	// undefined symbols, which is what they are supposed to do — the task moves
	// straight to development and no inference happens at all.
	// NOT WHEN THE DEVELOPER SENT IT BACK. The fast path answers "is there
	// anything to merge", and a hand-back is not that question — it is a named
	// failure this stage is the only one allowed to fix, because it may edit tests
	// and the developer may not.
	//
	// Measured on r81, and it ran until the retries were spent: a section asserted
	// with reflect.Elem on a string-kind type, which panics inside the test itself
	// whatever the implementation does. The tests COMPILED, so this returned
	// "sections already compile together" without a single model call and handed
	// the task straight back to development, which found the same panic and
	// returned it again. Three tasks cascaded behind it.
	if a.mode == modeSpecMerge && specRepairsSoFar(t) == 0 {
		// The gate is asked DIRECTLY on the branch rather than through verify,
		// which refuses when nothing is staged — and nothing is: the sections were
		// pushed by the agents that wrote them, and this stage has not edited
		// anything yet.
		out, pass, err := a.verifyTestsParse(ctx, rec, t, state, a.branchFor(t))
		if err != nil {
			return OutcomeFailed, "", fmt.Errorf("spec merge %s: %w", t.TicketID, err)
		}
		if pass {
			a.comment(ctx, t, "**Specification reconciled.**\n\nThe sections written for this task already "+
				"compile as one package, so there was nothing to merge. It goes to development as it stands.")
			return OutcomeSuccess, "sections already compile together", nil
		}
		// There IS a defect. Seed the loop with it so the first turn opens on the
		// error rather than on a blank page.
		state.lastTest, state.testsPass = out, false
		state.verifiedWrites = state.writes
	}

	// The bound includes the refunds: turns given back for reads that returned
	// nothing new extend the budget rather than being deducted from it.
	for state.iteration = 1; state.iteration <= a.maxIterations+state.refunded; state.iteration++ {
		// The prompt tells the agent how many actions remain, so it has to include
		// the refunds — otherwise it counts down to zero and keeps going, which is
		// the one thing worse than a wrong number.
		state.budget = a.maxIterations + state.refunded
		// CHECKED HERE AS WELL AS AFTER A VERIFICATION, because the flag is raised
		// during a REFUSAL and a refused agent may never verify again.
		//
		// That was the whole bug: noProgress concluded the specification was broken
		// and the only code that acted on it sat inside the post-verification path.
		// The agent could not verify — "nothing has changed since you last ran the
		// tests" — so the flag was set and never read. Measured on the dev fixture:
		// nine test-file refusals after a failing verification, the conclusion
		// reached on the third and acted on never.
		if outcome, detail, done := a.endOnBrokenSpec(ctx, t, state, state.lastTest); done {
			return outcome, detail, nil
		}
		// The turn-based restart is gone for the same reason: it treated the
		// symptom of the missing dependency, was measured changing nothing across
		// four runs, and discards a record the agent is using. resetAgent survives
		// for a caller that wants it; nothing calls it today.
		act, err := a.nextAction(ctx, t, state)
		if err != nil {
			// A malformed reply is NOT counted as a stuck loop. It is a different
			// signal: the model usually recovers when handed the parse error, and
			// treating two bad replies as terminal would throw away a task over a
			// stray code fence. It gets its own, looser bound.
			state.notice = noticeForParseFailure(state, err)
			state.parseFails++
			if state.parseFails >= maxParseFailures {
				a.comment(ctx, t, fmt.Sprintf(
					"**Developer agent stopped.** %d replies in a row could not be parsed as an action.\n\n"+
						"The last error was:\n\n```\n%s\n```\n\nand the reply it came from was:\n\n```\n%s\n```",
					state.parseFails, clip(state.notice, 600), state.lastReply))
				return OutcomeFailed, "unparseable model output", nil
			}
			continue
		}
		state.parseFails = 0
		state.trail = append(state.trail, devStep{action: act.Action, detail: stepDetail(act)})
		// ITS REASONING PLUS A PLAIN DESCRIPTION — and the split matters both ways.
		//
		// Replaying raw completions fed it bare JSON and it began imitating the shape
		// rather than choosing a tool, fabricating {"error":{"type":"llm_call_failed"}}
		// and {"tool_call_id":...} envelopes within minutes. So the SYNTAX is dropped.
		//
		// But stripping the syntax first took the prose with it, which threw away the
		// thing worth keeping: "I wrote main.go lines 31-41" does not tell the next
		// turn that it had already concluded there were duplicate NewStore
		// declarations and already tried removing them. That conclusion is what it
		// kept re-deriving, four commits running.
		//
		// Prose is affordable where the state is not. The slot holds 16384 tokens and
		// the rendered state is 4-6k of it; twenty turns of reasoning is 2-4k, while
		// twenty replayed states would exhaust the slot in three.
		turnAction := act.Action + ": " + stepDetail(act)
		if why := proseOf(state.lastReply); why != "" {
			turnAction += "\n" + why
		}

		// Carry the description forward from whichever action supplies it. The
		// commit is written during run_tests, which is BEFORE finish, so waiting
		// for finish would commit an empty description — and amending afterwards
		// would mean the commit the pipeline verified is not the commit that was
		// pushed. The model is told to describe its change when it makes it.
		if act.Summary != "" {
			state.summary = act.Summary
		}
		if act.Type != "" {
			state.commitType = act.Type
		}

		// Set by a write that actually changed the tree; drives the verification
		// below, which the model can no longer ask for.
		verifyNow := false

		switch act.Action {
		case actionReadFiles:
			a.doRead(ctx, rec, state, act)
			state.consecutiveReads++
			// READING IS NOT SPENDING. A read of a file already in hand never
			// reaches a sandbox — doRead returns from cache — so it costs one model
			// call against a prompt that is already in the KV cache: 2.2 seconds
			// measured, against ten to twenty for a verification that pushes,
			// clones and runs the suite. Charging the same turn for both spent an
			// entire 200-turn budget in about seven minutes on one ticket, 87
			// consecutive reads, with three productive actions in it.
			//
			// So the budget now counts WORK — writes, verifications, finishing —
			// and reading is free. What bounds an agent that only ever reads is
			// maxTotalIterations, which is a runaway backstop rather than a budget.
			state.refunded++

		case actionUndoEdit:
			// A REWIND IS PROGRESS, not a refusal. It costs a turn and buys a file the
			// agent can address again, which is strictly better than the alternative
			// it used to have — editing blind against a file it had broken, where
			// every subsequent old_str misses and every line number is wrong.
			if what, ok := state.undoLastEdit(); ok {
				state.notice = "Undone. Restored: " + what + ". The file is back to what it was before " +
					"your last write; read it if you are unsure what it now contains."
			} else {
				state.noProgress("Rejected: there is no edit of yours to undo — nothing has been " +
					"written this attempt.")
			}

		case actionWriteFiles:
			// LET THE TOOL RUN. This once refused a write when too many had piled up
			// unverified, on the reasoning that writing again could not tell the model
			// whether any of it worked. That reasoning was sound and the effect was
			// not: a refusal teaches nothing a result would not teach better, and
			// blocking one repeated action only moves the repetition to another —
			// measured directly, an agent barred from re-reading spent 49 of 77 turns
			// on refused verifications instead. So the guard is gone and the model sees
			// real outcomes.
			if err := applyEdits(state, act.Edits, a.mode); err != nil {
				state.noProgress("Rejected: " + err.Error())
				// A REFUSED TEST-FILE EDIT EARNS A VERDICT, because it is the one
				// refusal that means the agent believes the tests are the problem.
				//
				// Removing run_tests left a hole here: verification became a
				// consequence of a SUCCESSFUL write, so an agent that cannot land one
				// never got a verdict at all. Measured immediately — a developer facing
				// a test file it may not edit spent 38 turns and 16 refusals trying,
				// and the hand-back never fired because it waits on a compile result
				// nothing had produced. It used to ask for that verification itself.
				//
				// Narrow on purpose. Verifying unprompted at the start of every attempt
				// was the first fix and it was too blunt: on a branch that is already
				// green it ends the stage having done nothing.
				if refusalKindOf(err.Error()) == RefusalTestFile && state.lastTest == "" && !state.nothingToTest() {
					verifyNow = true
				}
			} else if !state.changedFromBaseline() {
				// A WRITE THAT CHANGES NOTHING IS NOT A WRITE, and counting it as one
				// desynchronised every guard that asks "has the tree moved since the
				// last test?".
				//
				// writes was incremented for any accepted edit, so a no-op made the
				// tree LOOK changed: verifiedWrites != writes. The re-verification
				// guard then allowed a run, the run found nothing to commit, and the
				// no-op advice — reading the same stale comparison — concluded the tree
				// was untested and said "run the tests". The agent did. Round and
				// round. Measured on r63: 80 refusals on one section, alternating
				// "that changed nothing" with "nothing has changed since you last ran
				// the tests", which cannot both be useful at once.
				//
				// Detected HERE rather than at push time, so the agent hears it on the
				// turn it happened instead of after a commit-and-clone round trip, and
				// so writes stays an honest count of real changes.
				state.noopEdits++
				verified := state.treeVerified()
				state.noProgress(noopEditNotice(state.noopEdits, verified, state.testsPass, state.lastTest))
			} else {
				state.writes++
				// THE NOTICE CLEARS, THE COUNTERS DO NOT.
				//
				// Clearing them here is a guess that the write landed, made before
				// anything has checked. changedFromBaseline compares the agent's own
				// staged map; whether git has anything to COMMIT is a different
				// question, and when the answer is no the verification below raises
				// errNothingToCommit and refuses the turn.
				//
				// So the turn cleared the counters and then earned a refusal, every
				// time, and neither the loop breaker nor devTemperature's ramp could
				// ever see more than one. Measured on r105 with the counters written
				// into the transcript: six consecutive refusals reading
				// "refusals=1 noop=1", with writes climbing 4, 5, 6, 7, 8, 9 —
				// a write counted as real on every one of them.
				//
				// The counters now clear where the answer is known: recordVerdict,
				// after a verification that actually ran and said something different
				// from last time. A write that truly landed verifies immediately —
				// verifyNow is set right below — so a healthy agent clears them on
				// the very next step, and only a write that changed nothing keeps
				// them.
				state.notice = ""
				// THE WRITE IS WHAT TRIGGERS THE CHECK. The model no longer decides
				// when to verify, which is what removes the whole family of refusals
				// that policed the decision.
				verifyNow = true
				// A REPAIR IS NOT A REFUSAL. The write landed and the turn bought
				// something, so this goes through notice directly rather than
				// noProgress — routing it through the refusal path would push a
				// successful write toward the stuck ceiling. The model is told what
				// changed so its next search text matches the file as it now stands
				// rather than as it wrote it.
				if len(state.repairs) > 0 {
					state.notice = fmt.Sprintf(
						"Your write was applied. One thing was corrected for you: in %s a struct tag was "+
							"opened with ` and never closed, and the closing ` has been added so the file "+
							"parses. The contents shown above are the corrected version — search against "+
							"those, not against what you wrote.",
						strings.Join(state.repairs, ", "))
					state.repairs = nil
				}
			}

		}

		switch {
		case verifyNow:
			// AN OPERATION WITH NO EFFECT IS REFUSED — and that is a statement of
			// fact, not a judgement about repeating.
			//
			// Verification is deterministic given the same tree: with no write
			// since the last run, the push, the clone and the test run return
			// exactly what they returned before. This was removed to measure what
			// an unobstructed agent does, and the measurement is in: with reads
			// free AND verification unbounded, one dev agent reached 606 turns
			// alternating the two — reads cost no turn so the budget never bit, and
			// the consecutive-read kill never fired because the run was broken up
			// by verifications.
			//
			// UNLIKE A READ, THIS IS NOT CHEAP. A read is served from memory; this
			// is a push, a fresh clone and a full test run. So it goes through
			// noProgress: it costs a turn and counts toward the stuck ceiling,
			// which is what bounds the loop now that reading cannot.
			// THE AGENT ASKED TO WRITE, so answer about the write. This used to
			// report that "running again cannot tell you anything new", which was
			// written when the model chose its own verifications and reads as a
			// non-sequitur now that it cannot: it wrote something and was answered
			// about test runs.
			//
			// What actually happened is that the content it just wrote is the content
			// that was already checked. Measured on the dev fixture: a spec author
			// re-sent a version that had FAILED the gate eleven times, never once
			// being shown the failure it was reproducing.
			if state.treeVerified() {
				state.noopEdits++
				state.noProgress(noopEditNotice(state.noopEdits, true, state.testsPass, state.lastTest))
				break
			}
			out, pass, err := a.verify(ctx, rec, t, state)
			// STAMPED HERE, before any branching on the outcome. A gate rejection is
			// a verdict on this tree as surely as a pass is, and the paths that
			// handle it used to return without recording that anything had run — so
			// the no-op advice believed the tree was unchecked and told a spec author
			// its file might have been written by an earlier attempt, when what had
			// actually happened was that its own edit had just failed the gate.
			//
			// The two errors below are the exception and the reason this is not
			// unconditional: neither of them ran anything.
			if !errors.Is(err, errNothingToTest) && !errors.Is(err, errPushFailed) {
				state.verifiedTree = state.treeHash()
			}
			// A PUSH THAT NEVER RAN IS NOT A VERDICT ON THE CODE. It must not
			// touch lastTest or verifiedWrites: recording it as a test result told
			// the no-op guard this tree had been verified, and the agent was then
			// refused every further verification while having no way to fix the
			// thing that was actually broken. Observed on one ticket: 118 refusals
			// and 197 turns, none of which could have helped.
			// A NO-OP EDIT IS THE AGENT'S TO FIX, not the plane's. It must not count
			// toward the push-failure ceiling, and it must not be recorded as a
			// test result — nothing ran.
			if errors.Is(err, errNothingToTest) {
				state.noProgress("Rejected: you have not changed anything yet, so there is nothing to " +
					"test. Write the file first — and if a previous attempt already wrote it, its contents " +
					"are shown above: edit those rather than creating it again.")
				break
			}
			if errors.Is(err, errNothingToCommit) {
				// SAYING "that changed nothing" IS NOT ENOUGH, and r58 measured how
				// far from enough: 103 identical refusals in one run. The message
				// described the problem and left the model to infer the remedy, so it
				// inferred "write it again".
				//
				// The remedy is specific and worth stating outright. A no-op edit
				// means the file ALREADY CONTAINS the change — which on a retry is
				// the normal case, because an earlier attempt of this same ticket
				// committed it. So the work may well be done, and the only way to
				// find out is to run the tests.
				// WHAT TO DO INSTEAD DEPENDS ON WHETHER THIS TREE HAS BEEN TESTED, and
				// the first version of this advice ignored that. It said "stop editing
				// and run the tests" unconditionally — while the guard above refuses a
				// verification when nothing has changed since the last one. Measured on
				// r59: 87 no-op refusals alongside 6 of "running again cannot tell you
				// anything new", an agent told to do the one thing it was then forbidden
				// to do. Two guards, each right alone, forming a trap together.
				state.noopEdits++
				verified := state.treeVerified()
				state.noProgress(noopEditNotice(state.noopEdits, verified, state.testsPass, state.lastTest))
				// THE TREE HAS BEEN TESTED, whatever the write counter says. Nothing to
				// commit means it is byte-identical to the branch that was last run, so
				// leaving verifiedWrites behind kept the repeat-verification guard
				// disengaged: every further run_tests reached the push, found nothing and
				// came back as another escalating no-op. Measured on the dev fixture: 6
				// writes producing 40 no-op refusals, the advice counting up to "the 20th
				// edit in a row" while the agent was not editing at all.
				//
				// Squaring them re-engages the guard, which says the useful thing: nothing
				// has changed, so write something different or finish.
				state.verifiedWrites = state.writes
				break
			}
			if errors.Is(err, errPushFailed) {
				state.pushFails++
				if state.pushFails >= maxPushFailures {
					a.comment(ctx, t, fmt.Sprintf(
						"**Developer agent stopped.** The branch could not be pushed %d times in a row, so its "+
							"work was never verified. This is a fault in the execution plane rather than in the "+
							"change, and the ticket is worth retrying.\n\n```\n%v\n```", state.pushFails, err))
					return OutcomeFailed, "could not push the branch", nil
				}
				state.notice = fmt.Sprintf(
					"The checks did NOT run: the branch could not be pushed (%v). That is a fault in the "+
						"infrastructure, not in your change. Your work is safe and the checks will be retried "+
						"on your next edit.", err)
				break
			}
			if err != nil {
				return OutcomeFailed, "", fmt.Errorf("dev agent: verify: %w", err)
			}
			state.pushFails = 0
			state.verifiedWrites = state.writes
			state.recordVerdict(out, pass)

			// STOP ON A SPECIFICATION THAT DOES NOT COMPILE. Every compile error is
			// in a test file, and this stage may not edit test files, so there is no
			// action left that can make the gate pass. Continuing spends the budget
			// to reach the same place with less information.
			//
			// It goes in front of a person rather than to another agent: a spec that
			// contradicts itself is a decision to make, not work to redo. Measured:
			// a spec dereferenced Task.Title as a pointer and used Task.Done as a
			// value, which no implementation satisfies and no amount of iterating
			// discovers.
			if outcome, detail, done := a.endOnBrokenSpec(ctx, t, state, out); done {
				return outcome, detail, nil
			}

			// GREEN IS THE END OF THE STAGE. A passing branch is finished work, and
			// the developer agent is not the last word on it — the reviewer reads it
			// next, and anything wrong comes back here as a new attempt.
			//
			// Left to decide for itself the model does not stop. Observed on a real
			// ticket: verification returned exit 0, and the agent kept editing for
			// another six turns until the budget ran out, at which point the branch
			// was reported as a failure with no passing change — a green result
			// thrown away by the turns that followed it. Polishing past a passing
			// test has no upside here and one large downside, because the only thing
			// those extra turns can do to a working branch is break it.
			// UNCONDITIONAL. Letting the author end its own stage instead was tried
			// and measured worse — see actionsFor — because a model that decides for
			// itself when a specification is complete does not decide.
			if pass {
				return a.finish(ctx, rec, t, state, state.summary,
					"Finished on the agent's behalf as soon as the checks passed; the reviewer is the next reader.")
			}

			// THE MEMORY IS NO LONGER CLEARED HERE, and the removal is the point.
			// It was added to break a read loop that turned out to be the developer
			// searching for code its branch did not contain — a missing dependency,
			// now fixed at source. With the cause gone the workaround only cost:
			// the store ticket, which merged in 4, 5, 6 and 8 turns across earlier
			// runs, reached 107 without merging while clearing its memory eleven
			// times, re-exploring ground it had already covered after every
			// verification.
		}

		// WHAT CAME OF IT, recorded before the next turn is built. A refusal is the
		// most useful thing to remember — it is the outcome the agent otherwise
		// re-earns by repeating itself — but a landed write matters too, because
		// "I already wrote that" is exactly what it kept failing to know.
		switch {
		case state.notice != "":
			// KEEP THE FAILURE, NOT THE LECTURE. A refusal is built as an
			// explanation followed by the evidence, and clipping it to a fixed head
			// kept exactly the wrong half: the no-op notice's preamble alone runs
			// ~370 characters, so a 400-rune clip ended mid-sentence at "The failure
			// was:" and dropped every line of it.
			//
			// Measured on r74: the developer on the API task was told seven times
			// that its edit changed nothing and that the checks had failed, with the
			// reason truncated to "--- t". It could not see WHICH test broke, so it
			// re-sent the same 240-byte edit every turn and would have spent all 200
			// iterations doing it — the stuck ceiling does not fire while the tests
			// are red, deliberately.
			//
			// clipEnds keeps both ends, so the head still says what was refused and
			// the tail carries the compiler or test output that says what to do
			// about it.
			state.remember(turnAction, "RESULT: "+clipEnds(state.notice, 200, 700))
		case state.lastTest != "":
			state.remember(turnAction, fmt.Sprintf("RESULT: the change landed and the checks %s.",
				passedOrFailed(state.testsPass)))
		default:
			state.remember(turnAction, "RESULT: accepted.")
		}

		// ANY action that is not a read clears the run. The counter is about doing
		// nothing but look, not about how much looking is allowed in total.
		if act.Action != actionReadFiles {
			state.consecutiveReads = 0
		}

		// NOTHING BUT READING, FOR A HUNDRED TURNS. Reads are free, so the budget
		// cannot end this; the run itself is the signal. Work already proved good
		// is banked rather than thrown away — the agent may have written and
		// verified something before it lost the thread.
		if state.consecutiveReads >= maxConsecutiveReads {
			if state.testsPass && len(state.staged) > 0 {
				return a.finish(ctx, rec, t, state, state.summary,
					fmt.Sprintf("Finished on the agent's behalf: it read %d times in a row without doing"+
						" anything else, but its change was already passing.", state.consecutiveReads))
			}
			a.comment(ctx, t, fmt.Sprintf(
				"**Developer agent stopped.** It read files %d times in a row without writing, verifying "+
					"or finishing. Reading costs it nothing, so the turn budget could not end this — a run "+
					"this long is the signal that it is not going to start.%s%s",
				state.consecutiveReads, restartSuffix(state.resets), trailSuffix(state.trail)))
			return OutcomeFailed, fmt.Sprintf("read %d times in a row without acting", state.consecutiveReads), nil
		}

		// One place decides that the agent has stopped getting anywhere, whichever
		// action it was repeating.
		if status, detail, done := a.stuck(ctx, rec, t, state); done {
			return status, detail, nil
		}
	}

	// The cap was reached. If the code nonetheless passed and is pushed, that is
	// a success the loop simply failed to announce — reporting it as a failure
	// would throw away finished work and send the ticket back for nothing.
	if state.testsPass && len(state.staged) > 0 {
		return a.finish(ctx, rec, t, state, state.summary,
			fmt.Sprintf("Finished on the agent's behalf: it reached the %d-iteration limit"+
				" with passing tests but never called finish.", a.maxIterations))
	}
	// AN AUTHOR'S TESTS NEVER PASS, so the check above can never bank its work.
	// Its output is a specification that FAILS by design, and reaching the limit
	// with tests written and staged is a specification that exists — reporting
	// that as a failure would throw it away and re-run the stage from nothing.
	//
	// This matters only because the author now ends its own stage: before, the
	// harness finished it at the first passing gate and it never reached the cap.
	if a.mode == modeTest && len(state.staged) > 0 {
		return a.finish(ctx, rec, t, state, state.summary,
			fmt.Sprintf("Finished on the author's behalf: it reached the %d-iteration limit"+
				" with tests written but never called finish.", a.maxIterations))
	}
	// THE COVERAGE STAGE REPORTS A SHORTFALL, not a failure. Its output is
	// additive: the code already satisfies the specification, so running out of
	// turns short of a target means "this much was covered", and the ticket
	// carries on to review with the figure on it. Reporting it as a failure would
	// read on the board as broken work.
	if a.mode == modeCoverage {
		reached := "no figure was reported"
		if pct, ok := parseCoverage(state.lastTest); ok {
			reached = fmt.Sprintf("%.1f%%, against a target of %d%%", pct, a.repo.CoverageTarget)
		}
		a.comment(ctx, t, fmt.Sprintf(
			"%s\n\nCoverage reached %s after %d iterations. The work is unchanged and still passes its "+
				"specification tests, so it carries on to review — this is a shortfall, not a failure.%s",
			coverageShortfallMarker, reached, a.maxIterations, trailSuffix(state.trail)))
		return OutcomeFailed, "coverage short of target", nil
	}
	a.comment(ctx, t, fmt.Sprintf(
		"**Developer agent stopped** after %d iterations without a passing change.%s%s\n\nLast test output:\n\n```\n%s\n```%s",
		a.maxIterations, restartSuffix(state.resets), trailSuffix(state.trail),
		clip(state.lastTest, 2000), noticeSuffix(state.notice)))
	return OutcomeFailed, "iteration limit reached", nil
}

// stuck ends the loop once the agent has spent maxRepeatRefusals consecutive
// turns achieving nothing, and reports done=false while there is still reason to
// continue.
//
// The judgement it makes is the one the model would not: if the code passes and
// is pushed, the task is finished and saying so is the only thing left to do; if
// it does not, more turns of the same will not help and the ticket should go to a
// person with the evidence attached.
func (a *DevAgent) stuck(ctx context.Context, rec *Recorder, t Ticket, state *devState) (string, string, bool) {
	// THE CEILING ONLY APPLIES WHEN THERE IS SOMETHING TO SALVAGE. Repetition on
	// its own no longer ends an attempt: the budget does, and nothing else.
	//
	// Cutting an agent off for repeating itself cost more than it ever saved.
	// Tickets were abandoned at turn five of forty-eight — ninety per cent of the
	// budget unspent — for asking to read a file twice, which is a thing a careful
	// worker does and which the search/replace edit format actively requires,
	// since an edit must quote the existing text exactly. Allowing the repeats
	// took a run from three merged tickets to four, and the two that still failed
	// then failed on their merits after using every turn they had.
	//
	// What survives is the OPPOSITE case: an agent that has already produced a
	// passing branch and is now spinning. Finishing on its behalf is not cutting
	// it off, it is banking work that is already done — so that one still fires.
	if state.refusals < maxRepeatRefusals {
		return "", "", false
	}
	if state.testsPass && len(state.staged) > 0 {
		status, detail, _ := a.finish(ctx, rec, t, state, state.summary,
			fmt.Sprintf("Finished on the agent's behalf: it spent %d turns without changing anything"+
				" after its tests had already passed.", state.refusals))
		return status, detail, true
	}
	// NOTHING TO BANK, AND NOTHING CHANGING. Letting this run to the budget was
	// deliberate — cutting an agent off for repeating itself abandoned real
	// tickets at turn five of forty-eight, and maxRepeatRefusals is only four —
	// but "keep going" turned out to mean "burn every remaining turn". Measured
	// twice in consecutive runs: a spec author made 45 refused actions in a row,
	// and a developer 91, each alternating a write that produced no diff with a
	// verification that could not run. Neither could reach a legal move.
	//
	// So there are TWO ceilings. The low one (maxRepeatRefusals) still only banks
	// passing work and never ends an attempt, which is what protects a careful
	// agent that repeats itself while recovering. The high one ends it: by then
	// the agent has had five times as many chances as the old ceiling gave it,
	// and every further turn is guaranteed waste.
	if ceiling := deadRefusalCeiling(); ceiling > 0 && state.refusals >= ceiling {
		a.comment(ctx, t, fmt.Sprintf(
			"**Developer agent stopped.** It made %d actions in a row that changed nothing and never "+
				"reached a passing state, so the remaining turns could only repeat that.%s\n\nLast notice:\n\n```\n%s\n```",
			state.refusals, trailSuffix(state.trail), clip(state.notice, 800)))
		return OutcomeFailed, fmt.Sprintf("%d actions in a row changed nothing", state.refusals), true
	}
	return "", "", false
}

// hasOrHasNotPassed words the state of the branch for a human reading the
// ticket, distinguishing "never verified" from "verified and failed".
func hasOrHasNotPassed(s *devState) string {
	switch {
	case s.lastTest == "":
		return "was never verified"
	case s.testsPass:
		return "passes but nothing was staged"
	default:
		return "does not pass"
	}
}

// finish ends the task successfully. Both the model's own finish and the
// loop's decision to finish for it come through here, so a ticket that the
// loop closed is indistinguishable downstream from one the model closed —
// same comment, same markers, same outcome — except for the note saying so.
//
// The branch is already pushed: run_tests pushed it, because a pipeline
// verifies a branch, not a working copy. Finishing is reporting, not
// publishing, which is exactly why the loop is entitled to do it.
func (a *DevAgent) finish(ctx context.Context, rec *Recorder, t Ticket, state *devState, summary, note string) (string, string, error) {
	body := renderStageSummary(a.mode, a.branchFor(t), summary, stagedPaths(state), state.lastRun, state.lastTest)
	if note != "" {
		body += "\n\n_" + note + "_"
	}
	a.comment(ctx, t, body)

	// A SPECIFICATION THAT IS ALREADY SATISFIED HAS NO DEVELOPMENT TO DO.
	//
	// Sections overlap: the implementation merged for one can already satisfy the
	// next one's tests. Sending that section to development spends a lease, an
	// attempt and a verification to discover the branch was green on arrival —
	// and before this was understood, it read as "the delegated agent produced no
	// change" and blocked the ticket outright.
	//
	// It goes to INTEGRATION, not to done. The tests the author just wrote exist
	// only on this branch, and the integrator's merge is the one thing that puts
	// them on the integration branch — skip that and the specification is lost,
	// which is the failure mode that costs the most and shows the least.
	//
	// Security review is skipped with development, deliberately: it reviews an
	// implementation diff, and there is no implementation diff to review.
	if a.mode == modeTest || a.mode == modeSpecMerge {
		if a.branchAlreadyGreen(ctx, rec, state, a.branchFor(t)) {
			a.comment(ctx, t, alreadySatisfiedMarker+"\n\nThe tests written for this section pass against "+
				"the code already on the branch, so there is nothing to implement. It goes straight to "+
				"integration so the tests themselves are merged.")
			if err := a.api.MoveTo(ctx, t.TicketID, ColReadyForIntegration); err != nil {
				// Fall back to the ordinary route rather than stranding it: development
				// will find the branch green and pass it along.
				slog.WarnContext(ctx, "could not route an already-satisfied section to integration",
					"ticket_id", t.TicketID, "error", err)
				return OutcomeSuccess, "pushed " + a.branchFor(t) + " (run " + state.lastRun + ")", nil
			}
			return OutcomeHandled, "specification already satisfied; straight to integration", nil
		}
	}
	return OutcomeSuccess, "pushed " + a.branchFor(t) + " (run " + state.lastRun + ")", nil
}

// branchAlreadyGreen reports whether the branch passes its own tests as it
// stands.
//
// Deliberately the WHOLE suite and not just this section's file: a section is
// only free to skip development if nothing anywhere is red, and a section whose
// own tests pass while it has broken another is exactly the case that must not
// skip anything.
//
// A failure to run is not "green". Any error, and this says no, so the ticket
// takes the ordinary route through development.
func (a *DevAgent) branchAlreadyGreen(ctx context.Context, rec *Recorder, state *devState, branch string) bool {
	if a.repo.TestCommand == "" || state.sb == nil {
		return false
	}
	res, err := state.sb.RunOnBranch(ctx, rec, branch, a.repo.TestCommand)
	if err != nil {
		return false
	}
	return res.OK()
}

// passedOrFailed words a boolean verification result.
func passedOrFailed(passed bool) string {
	if passed {
		return "passed"
	}
	return "failed"
}

// trailSuffix renders what the attempt actually did, so a failure that produced
// no test output still says something. An attempt that ends on write_files never
// verified — which is the difference between "the code is wrong" and "it ran out
// of turns before checking", and those want opposite responses.
func trailSuffix(trail []devStep) string {
	if len(trail) == 0 {
		return ""
	}
	names := make([]string, 0, len(trail))
	verified := false
	for _, s := range trail {
		names = append(names, s.String())
		if s.action == actionRunTests {
			verified = true
		}
	}
	out := "\n\nWhat it did: `" + strings.Join(names, " → ") + "`"
	last := trail[len(trail)-1]
	if last.action != actionRunTests {
		out += "\n\nIt ended on `" + last.action + "` without verifying, so the branch was never tested."
		// Only claim the budget was the problem when the attempt never got to run
		// the tests AT ALL. An attempt that verified earlier and failed had its
		// answer and did not act on it, and reporting that as "needs more turns"
		// sends the reader to the wrong fix — which this comment did until now.
		if verified {
			out += " It did verify earlier in the attempt; the last verification result above is the one to read."
		} else {
			out += " It never ran the tests once, so nothing here says whether the change works."
		}
	}
	return out
}

// noticeSuffix appends the outstanding rejection to a failure comment, so the
// reason the last turn was refused survives into the ticket.
func noticeSuffix(notice string) string {
	if notice == "" {
		return ""
	}
	return "\n\nThe last action was also rejected: " + notice
}

// nextAction asks the model for one action.
func (a *DevAgent) nextAction(ctx context.Context, t Ticket, s *devState) (devAction, error) {
	res, err := a.gw.Chat(ctx, a.class, ChatRequest{
		// ORDERED BY WHAT CHANGES, so the backend can reuse the part that does not.
		//
		// The system prompt and the world are identical on any turn that neither
		// read nor wrote a file; the history and the progress differ every turn.
		// Putting the volatile halves LAST leaves the largest block cacheable —
		// measured on this deployment at 25,791 prompt tokens, an unchanged prefix
		// costs 0.5s against 30.1s cold, and a changed preamble in front of the same
		// block saves nothing at all.
		Messages: append(append([]Message{
			{Role: "system", Content: a.systemPrompt(t)},
			{Role: "user", Content: renderDevWorld(t, s)},
		}, s.recentHistory()...),
			Message{Role: "user", Content: renderDevProgress(s)}),
		// ZERO WHILE IT IS WORKING, WARMER WHEN IT IS STUCK.
		//
		// At temperature 0 the model is a deterministic function of the prompt, so a
		// refused turn regenerates the same reply from near-identical state — which
		// is exactly what r66 did 75 times. No wording fixes that: the input barely
		// changes, so neither can the output. The escape has to come from sampling.
		//
		// Determinism is worth keeping on the happy path, where it makes runs
		// comparable, so this stays at 0 until something has actually been refused.
		Temperature: devTemperature(max(s.refusals, s.staleReads)),
		// HALF THE SLOT, not a third of it. The serving context is 65536 across four
		// slots, so a slot holds 16384 tokens of prompt AND reply together; this
		// leaves the same again for the prompt, which carries the ticket, the tree,
		// every file read and the last test output.
		//
		// It was 4000 against an 8192 slot, and that was measured failing: creating
		// a 150-line main.go inside one tool argument ran out mid-string, decoded as
		// "unexpected end of JSON input", and cost the whole attempt. The advice in
		// noticeForParseFailure still stands for the cases that exceed even this —
		// a bigger budget makes truncation rarer, not impossible.
		// THE PER-STAGE CEILING, which existed but was never applied: this read
		// MaxTokens: 8000 while maxReplyTokens sat unused, so every stage got the
		// developer's allowance whatever it was doing. It cost little on a fast
		// model and a great deal on a slow one — a spec author on the dense model
		// ran to 8000 tokens and 334 seconds on EVERY turn.
		//
		// It is a backstop, not the fix. What stops a well-behaved turn is the
		// grammar (see ClassConfig.ToolsSupported); this only bounds the damage
		// when that is unavailable.
		MaxTokens: a.maxReplyTokens(),
		Priority:  ParsePriority(t.Priority),
		// THE GRAMMAR IS THE INTERFACE, NOT THE TOOL LIST. This offered Tools and
		// kept Schema as a fallback, on the reasoning that a tool call arrives typed
		// and named and "the server validates them". Measured, that is not true of
		// this backend: the gateway drops response_format whenever tools are sent
		// (see the ResponseFormat branch in inference.go), so the ARGUMENTS are
		// generated completely unconstrained.
		//
		// What that permits, read from a live completion:
		//
		//	...Encode(task)\n\t}\n}'}], ","start_line": 89
		//
		// The model closed the "replace" string with an apostrophe and carried on
		// writing JSON. The recovered value takes `'}], ` into the Go source, which
		// comes back as "rune literal not terminated" — 14 of 23 refusals in one
		// window, plus the turns counted as unparseable.
		//
		// A grammar-constrained sampler cannot emit that: it can only produce tokens
		// that keep the JSON valid, so the mistake is unrepresentable rather than
		// repaired afterwards. That is the argument already written on ReplySchema,
		// and this path — the one carrying whole Go files through a string field —
		// is where it was most needed and least applied.
		//
		// Tools stay defined and parseDevReply still accepts a tool call, so a
		// backend that answers that way is handled; they are simply not offered.
		Schema: devActionSchema(),
	})
	if err != nil {
		return devAction{}, err
	}
	// KEPT FOR THE FAILURE MESSAGE. "unparseable model output" with nothing after
	// it is not a diagnosis — a fence, a prose preamble, a truncated object and a
	// tool call the server did not decode all produce it, and they need different
	// fixes. The architect had the same hole and naming the reply solved it in one
	// run. Truncated on the way in: this ends up on a ticket a person reads.
	s.truncated = res.FinishReason == "length"
	s.lastReply = clip(strings.TrimSpace(res.Content), 1200)
	if len(res.Calls) > 0 {
		s.lastReply = fmt.Sprintf("(tool call %q, arguments) %s",
			res.Calls[0].Name, clip(strings.TrimSpace(res.Calls[0].Arguments), 1000))
	}
	return parseDevReply(res, a.mode)
}

func (a *DevAgent) doRead(ctx context.Context, rec *Recorder, s *devState, act devAction) {
	paths, err := validatePaths(act.Paths, maxReadPaths)
	if err != nil {
		s.noProgress("Rejected: " + err.Error())
		return
	}

	// RE-READING IS ALLOWED. It used to be refused — the contents are already in
	// the prompt, so a re-read returns bytes the model can see — and the refusal
	// counted toward the stuck ceiling, which killed tickets four turns after they
	// started. Measured: two tickets abandoned at turn five of forty-eight, having
	// spent 10% of their budget and produced nothing, purely because they asked
	// for a file twice.
	//
	// The refusal was also aimed at the wrong thing. An edit names the EXACT text
	// it replaces, so an agent that wants to re-check what it is about to match is
	// doing the thing the edit format requires, not looping. Re-reading is what a
	// careful worker does; the failure worth catching is making no PROGRESS, which
	// is a different question and is answered below.
	//
	// A repeat costs a turn and no sandbox: the content is served from what has
	// already been read, so the only price is the iteration, which is the honest
	// price of asking.
	var fresh []string
	for _, p := range paths {
		if _, known := s.read[p]; known || s.missing[p] {
			continue
		}
		fresh = append(fresh, p)
	}
	if len(fresh) == 0 {
		s.staleReads++
		// SERVED, ALWAYS, and the refund is applied by the LOOP rather than here:
		// every read is free now, not only the ones that returned nothing.
		s.notice = fmt.Sprintf(
			"You already have %s — the contents above are current, and re-reading returned the same bytes."+
				" This turn has been refunded, but it told you nothing.", strings.Join(paths, ", "))
		if s.staleReads >= maxStaleReads {
			s.notice += fmt.Sprintf(" You have now re-read %d times in a row without changing anything."+
				" Nothing new can come from asking again — write_files to make the change; the checks"+
				" run by themselves once something is different.", s.staleReads)
		}
		return
	}
	s.staleReads = 0

	contents, err := a.readFiles(ctx, rec, s, fresh)
	if err != nil {
		s.noProgress("Could not read files: " + err.Error())
		return
	}
	for p, c := range contents {
		s.read[p] = c
		if _, seen := s.baseline[p]; !seen {
			s.baseline[p] = c
		}
	}
	// A path the repo did not return does not exist. Recording that is what stops
	// a model asking for a file that was never there once per iteration.
	for _, p := range fresh {
		if _, ok := contents[p]; !ok {
			s.missing[p] = true
		}
	}
	s.notice, s.refusals, s.staleReads, s.noopEdits = "", 0, 0, 0
}

// listRepo surveys the repository once, at the start.
//
// The tree is fetched up front and reused rather than a file at a time: even in
// a held sandbox a round trip is not free.
// listRepo surveys the repository at the START OF AN ATTEMPT, and syncs the
// working tree to the branch while it is there.
//
// ON THE BRANCH, not on whatever the sandbox happens to be holding. The sandbox
// is acquired per TICKET and outlives every attempt in it, while a.run executes
// against its working tree with no fetch — so a second attempt surveys and reads
// the tree its FIRST attempt cloned, however long ago that was and whatever has
// happened to the branch since.
//
// Measured on r111. The developer correctly handed back a specification whose
// ticket_test.go shadowed *testing.T; the reconciler repaired it and pushed at
// 15:15:48; every later attempt still read the broken file, because the tree had
// been cloned at 15:13 and nothing re-fetched it. The agent diagnosed the fault
// exactly right, wrote the same fix the reconciler had already made, was refused
// for editing a test file, re-read it, saw the broken version again, and repeated
// until its ceiling. Three actors each behaving correctly against a file that no
// longer existed.
func (a *DevAgent) listRepo(ctx context.Context, rec *Recorder, s *devState) ([]string, error) {
	// TOLERANT OF A BRANCH THAT DOES NOT EXIST YET, which is every first attempt.
	// RunOnBranch fetches strictly and the prelude sets -e, so on attempt one it
	// aborts the survey with "couldn't find remote ref" and blocks the ticket —
	// measured on r112, which blocked all five sections that way. This is the same
	// tolerant form pushScript uses: sync to the branch when it is there, and stay
	// on the base clone when it is not.
	branch := a.branchFor(s.ticket)
	script := fmt.Sprintf("git fetch -q origin %q 2>/dev/null && git checkout -q -B %q FETCH_HEAD 2>/dev/null || true\n",
		branch, branch) +
		fmt.Sprintf("git ls-files | head -n %d\n", maxFileListing)
	res, err := a.run(ctx, rec, s, script)
	if err != nil {
		return nil, err
	}
	if !res.OK() {
		return nil, fmt.Errorf("clone failed (exit %d): %s", res.ExitCode, clip(res.Stderr+res.Stdout, 1000))
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(res.Stdout), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out, nil
}

// readFiles fetches file contents in ONE sandbox run, base64-framed so content
// with newlines or shell metacharacters survives the round trip intact.
func (a *DevAgent) readFiles(ctx context.Context, rec *Recorder, s *devState, paths []string) (map[string]string, error) {
	var b strings.Builder
	for _, p := range paths {
		fmt.Fprintf(&b, "\nif [ -f %q ]; then echo \"===FILE %s\"; base64 -w0 < %q; echo; fi", p, p, p)
	}
	res, err := a.run(ctx, rec, s, b.String())
	if err != nil {
		return nil, err
	}
	if !res.OK() {
		return nil, fmt.Errorf("exit %d: %s", res.ExitCode, clip(res.Stderr, 500))
	}

	return decodeFileBlocks(res.Stdout), nil
}

// decodeFileBlocks reads the "===FILE name" + base64 protocol that every
// sandbox-side read emits. Shared, because the referee fetches files the same way
// and two copies of a wire format drift.
func decodeFileBlocks(stdout string) map[string]string {
	out := map[string]string{}
	lines := strings.Split(stdout, "\n")
	for i := 0; i < len(lines); i++ {
		name, ok := strings.CutPrefix(lines[i], "===FILE ")
		if !ok || i+1 >= len(lines) {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(lines[i+1]))
		if err != nil {
			continue
		}
		out[strings.TrimSpace(name)] = clip(string(raw), maxFileForModel)
		i++
	}
	return out
}

// noticeForParseFailure turns a rejected reply into advice the model can act on.
//
// TRUNCATION IS NOT A FORMAT MISTAKE, and telling a model its reply was
// "rejected" when it was actually CUT OFF makes it try the same thing again —
// which is exactly what four identical whole-file writes in a row looked like.
// Measured: the model was creating a 150-line main.go inside one tool argument,
// ran out of output tokens mid-string, and the result decoded as "unexpected end
// of JSON input". The advice has to change the SIZE of the next attempt, not its
// shape.
func noticeForParseFailure(s *devState, err error) string {
	if s.truncated {
		return "Your previous reply was CUT OFF at the token limit — it was not malformed, it was too long. " +
			"Do not send it again unchanged. Make a SMALLER edit: write one function or one section per turn, " +
			"and use a search/replace edit against text already in the file rather than sending a whole file at once."
	}
	return "Your previous reply was rejected: " + err.Error()
}

// isStdlibFrame reports a stack frame from Go's own tree, which is never where
// the fault is. Anchored on the GOROOT path so a repository with a directory
// called "testing" is not mistaken for one.
func isStdlibFrame(l string) bool {
	t := strings.TrimSpace(l)
	if strings.HasPrefix(t, "/usr/local/go/src/") || strings.HasPrefix(t, "/usr/lib/go/src/") {
		return true
	}
	for _, p := range []string{"testing.tRunner", "testing.(*T).Run", "runtime.gopanic", "runtime.goexit", "created by testing."} {
		if strings.HasPrefix(t, p) {
			return true
		}
	}
	return false
}

// failedGate reads the marker the script leaves. "checks" is the honest answer
// when nothing was marked — a sandbox that died before any gate ran.
func failedGate(output string) string {
	i := strings.LastIndex(output, gateMarker)
	if i < 0 {
		return "checks"
	}
	rest := output[i+len(gateMarker):]
	if j := strings.IndexAny(rest, " \n\r"); j >= 0 {
		rest = rest[:j]
	}
	if rest == "" {
		return "checks"
	}
	return rest
}

// defaultDependencyManifests covers the common ecosystems. A path matching one of
// these is how the branch is judged to have touched the dependency tree.
var defaultDependencyManifests = []string{
	"go.mod", "go.sum", "go.work", "go.work.sum",
	"package.json", "package-lock.json", "yarn.lock", "pnpm-lock.yaml",
	"requirements.txt", "poetry.lock", "Pipfile.lock", "pyproject.toml",
	"Cargo.toml", "Cargo.lock", "Gemfile", "Gemfile.lock",
	"composer.json", "composer.lock", "pom.xml", "build.gradle", "build.gradle.kts",
}

// manifestPattern builds the expression matched against the branch's changed
// paths. Anchored at a path separator so a nested module's go.mod counts and a
// file merely named like one (vendor/notes-about-go.mod.txt) does not.
func manifestPattern(manifests []string) string {
	if len(manifests) == 0 {
		manifests = defaultDependencyManifests
	}
	escaped := make([]string, 0, len(manifests))
	for _, m := range manifests {
		escaped = append(escaped, strings.ReplaceAll(m, ".", `\.`))
	}
	return "(^|/)(" + strings.Join(escaped, "|") + ")$"
}

// dependencyRegressionScript gates the dependency scan on whether THIS BRANCH
// changed the dependency tree.
//
// The distinction is the whole point. An SCA finding is normally a property of a
// tree that predates the ticket: blocking on it would stop an agent adding a
// function because some transitive package has an advisory, which is outside its
// brief, may have no upstream fix at all, and invites a version bump smuggled
// into a feature branch where nobody is reviewing it. Left advisory, the reviewer
// sees it and a human decides.
//
// A dependency this branch ADDED is the opposite case: it is in scope, this
// change caused it, and the fix is usually one version bump the agent can make
// while it still has the branch open. That is worth blocking on.
//
// FAILS OPEN. If the base cannot be fetched — a shallow remote, a renamed
// default branch — the comparison is unknown, and an unknown must not block: a
// gate that fires when it cannot tell traps the agent for a reason nothing in its
// output explains.
func (a *DevAgent) dependencyRegressionScript() string {
	if a.repo.SCACommand == "" {
		return ""
	}
	base := a.repo.Branch
	if base == "" {
		base = "main"
	}
	return fmt.Sprintf(`
echo '--- dependency regression check ---'
if git fetch -q origin %q 2>/dev/null && git diff --name-only origin/%s...HEAD 2>/dev/null | grep -qE %q; then
  echo 'this branch changes the dependency tree, so its findings are this change to answer for'
  %s || { echo '%s%s'; exit 1; }
else
  echo 'dependencies unchanged by this branch; any findings are pre-existing and advisory'
fi
`, base, base, manifestPattern(a.repo.DependencyManifests), a.repo.SCACommand, gateMarker, "dependency-regression")
}

// The advisory section's delimiters. Marked rather than merely printed so the
// linter's output can be separated from the gate's — mixing them is how a passing
// branch comes back looking like a failing one.
const (
	adviceOpen  = "===ADVISORY-OPEN==="
	adviceClose = "===ADVISORY-CLOSE==="
)

// splitAdvice separates the linter's advisory output from the rest.
func splitAdvice(out string) (advice, rest string) {
	i := strings.Index(out, adviceOpen)
	if i < 0 {
		return "", out
	}
	j := strings.Index(out, adviceClose)
	if j < 0 || j < i {
		return "", out
	}
	advice = strings.TrimSpace(out[i+len(adviceOpen) : j])
	rest = out[:i] + out[j+len(adviceClose):]
	return advice, rest
}

// push commits the staged edits onto the branch, creating or updating it.
//
// Force-pushed on purpose: the branch belongs to this ticket's attempt, the
// agent is its only writer, and each iteration replaces the previous attempt
// rather than accumulating "fix the fix" commits nobody wants to read.
//
// THE REPOSITORY'S OWN HOOKS RUN. Same principle as verification: the project
// already decides what a commit must satisfy — conventional message, no secrets,
// no oversized files — and an agent that passes --no-verify is an agent held to
// a lower standard than the people it works alongside. It also moves those
// failures minutes earlier than the pipeline would.
func (a *DevAgent) push(ctx context.Context, rec *Recorder, t Ticket, s *devState, branch string) error {
	// START FROM THE REMOTE BRANCH IF THERE IS ONE. This is belt and braces
	// beside Sandbox.AdoptBranch, and it is the half that actually protects
	// earlier work: a bare `checkout -B` from whatever HEAD happens to be, followed
	// by a force-push, recreates the branch from the base and destroys whatever
	// another stage pushed to it. That is not a hypothetical — it silently erased
	// every specification test on a live run, leaving the developer's gate with
	// nothing to satisfy and reporting success for it.
	script := a.pushScript(t, branch, s)

	// A SUBMIT FAILURE IS THE PLANE, NOT THE WORK. forge refuses a second
	// execution against a lease that is already running one — "POST /executions:
	// already claimed" — and that is a moment in time rather than a fault in the
	// branch. Measured 555 times today, invisible until the read loops stopped
	// hiding it. A short retry clears it; what must never happen is the failure
	// reaching the model as a test result, which is handled by errPushFailed.
	var err error
	var res SandboxResult
	for attempt := range pushAttempts {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			// EXPONENTIAL, not linear. attempt*delay gave 0,2,4,6,8 — twenty seconds
			// across five tries, which still expires inside a single cold-sandbox
			// verification. Doubling gives 2,4,8,16 and covers about half a minute
			// per push, with maxPushFailures multiplying that again.
			case <-time.After((1 << (attempt - 1)) * pushRetryDelay):
			}
		}
		res, err = a.run(ctx, rec, s, script)
		if err == nil {
			break
		}
		if !isTransientSubmit(err) {
			return err
		}
		slog.WarnContext(ctx, "the sandbox refused a push; retrying",
			"ticket_id", t.TicketID, "attempt", attempt+1, "error", err)
	}
	if err != nil {
		return err
	}
	if !res.OK() {
		// "NOTHING TO COMMIT" IS NOT A FAILURE OF THE PLANE. It means the agent's
		// edits produced no diff against the branch — it rewrote what was already
		// there. Treating it as a push failure killed a ticket after three of
		// them; what the model needs is to be told its change was a no-op, which
		// is something it can act on.
		out := res.Stderr + res.Stdout
		if strings.Contains(out, "nothing to commit") {
			return errNothingToCommit
		}
		return fmt.Errorf("exit %d: %s", res.ExitCode, clip(out, 1500))
	}
	return nil
}

const (
	// pushAttempts and pushRetryDelay bound the retry of a transient submit
	// refusal. Small on purpose: this is a lease briefly busy, not an outage.
	// THE WINDOW WAS TOO SHORT FOR THE THING IT RETRIES. "already claimed" means
	// the forge lease is busy, and the old comment called that "briefly busy" — 3
	// attempts, 2s apart, six seconds of tolerance. Measured: nine consecutive
	// refusals across 55 seconds, and the attempt died having never pushed once.
	//
	// A verification is a push, a clone, a formatter and a full test run, and the
	// formatter downloads and compiles itself on a cold sandbox. Tens of seconds is
	// the NORMAL duration of a held lease, not an outage — so the retry has to
	// outlast it. Backoff doubles from 2s: 2+4+8+16 is about half a minute per
	// push, and maxPushFailures allows three of those.
	pushAttempts    = 5
	pushRetryDelay  = 2 * time.Second
	maxPushFailures = 3
)

// errPushFailed marks a verification that never ran because the branch could not
// be pushed.
//
// THIS IS NOT A TEST RESULT AND MUST NOT BE RECORDED AS ONE. It was: the message
// went into lastTest and verifiedWrites was set to writes, which told the no-op
// guard the tree had been verified. The agent was then refused every further
// verification — "nothing has changed since you last ran the tests" — 118 times
// on one ticket, unable to leave a state it had no way to fix, because the thing
// that was broken was not something writing code could mend.
var errPushFailed = errors.New("could not push the branch")

// errNothingToCommit marks a push whose tree was identical to the branch: the
// agent wrote, but wrote what was already there.
var errNothingToCommit = errors.New("the edits produced no change")

// errNothingToTest marks a verification asked for before anything was written.
// It is the agent's to fix by writing something, and it is not a test result.
var errNothingToTest = errors.New("nothing has been changed yet")

// isTransientSubmit reports whether a sandbox refused the work for a reason that
// will pass on its own.
func isTransientSubmit(err error) bool {
	return err != nil && strings.Contains(err.Error(), "already claimed")
}

// commitMessageScript writes the commit message to a file for `git commit -F`,
// base64-encoded on the way.
//
// NOT `commit -m` with the message formatted into the script. It was written
// that way with %q, which is GO quoting, not shell quoting, and the two differ in
// exactly the ways that matter here:
//
//   - Go escapes a newline as the two characters \n. Inside shell double quotes
//     those stay two characters, so the Conventional Commits body — the blank
//     line and the Ticket trailer — arrived as literal backslash-n on one line.
//     A repository running commitlint rejects that (body-leading-blank), and the
//     agent's commit fails for a reason nothing in its output explains.
//   - Go does NOT escape $ or backticks. The subject is MODEL-AUTHORED, so a
//     summary containing $(...) was substituted by the shell: model output
//     becoming a command. That is precisely what base64-encoding file content
//     exists to prevent on the write path, and the commit path went around it.
//
// Encoding both closes it: the bytes reach git without the shell reading them.
func commitMessageScript(msg string) string {
	// Single quotes are safe without escaping: base64 output is alphanumerics plus
	// "+/=", so it can contain neither a quote nor anything the shell expands.
	return fmt.Sprintf("printf %%s '%s' | base64 -d > \"$COMMIT_MSG_FILE\"\n",
		base64.StdEncoding.EncodeToString([]byte(msg)))
}

// gitCommit commits what is staged, taking the message from the file written by
// commitMessageScript.
const gitCommit = "git -c user.name=dev-agent -c user.email=dev-agent@platform.invalid commit -F \"$COMMIT_MSG_FILE\""

// formatScript rewrites the working tree with the project's formatter before the
// commit is made, so what lands is formatted.
//
// BEST EFFORT, and loudly so. A repository may not have the tool, and a formatter
// that cannot run must not stop a change that is otherwise fine — but silently
// skipping one the project DOES define would let the agent commit to a standard
// its human contributors are held to. Empty is a no-op: an operator who has not
// named a formatter has not asked for one.
func (a *DevAgent) formatScript() string {
	if a.repo.FormatCommand == "" {
		return ""
	}
	return fmt.Sprintf(`
%s || echo "WARNING: the formatter failed; committing unformatted"
`, a.repo.FormatCommand)
}

// depsScript resolves the dependency manifest after the writes and before the
// commit, so a lock file lands in the same commit as the import that needs it.
//
// WITHOUT THIS AN AGENT CANNOT ADD A DEPENDENCY AT ALL, and the way it fails is
// vicious. It writes `require github.com/x/y` into go.mod correctly, and then
// every build fails with "missing go.sum entry" — a file of CRYPTOGRAPHIC HASHES
// that cannot be produced by editing text. The agent's only execution tool is
// run_tests, which runs the test command and nothing else, so the one action
// that would fix it is unavailable. Measured on a live ticket: six commits and
// 112 model turns spent circling `go.mod`, while the actual compile errors sat
// untouched behind it. Adding `go mod tidy` cleared the dependency wall in one
// step and left three ordinary errors the agent can fix.
//
// Best effort, like the formatter: a repository without the tool, or offline,
// must not lose an otherwise good change. The warning is printed rather than
// swallowed, because a dependency that silently failed to resolve becomes a test
// failure with a confusing cause.
//
// WHAT THIS WIDENS, stated plainly: the model now chooses what gets downloaded
// into the sandbox, and `go test` then runs that code. That is inherent in
// allowing third-party dependencies at all — the sandbox is the boundary, not
// this command — but it is a real increase in what a prompt-injected spec can
// reach, and it is the reason this is one named command rather than a general
// "run whatever you need" tool.
func (a *DevAgent) depsScript() string {
	if a.repo.DepsCommand == "" {
		return ""
	}
	return fmt.Sprintf(`
%s || echo "WARNING: dependencies could not be resolved; the build may fail on a missing lock entry"
`, a.repo.DepsCommand)
}

// hookSetupScript installs the repository's pre-commit hooks when it has a
// configuration, and does nothing when it does not.
//
// Best-effort by design: a repository that does not use pre-commit, or a sandbox
// image without pip, must not fail the commit over a hook that was never going
// to run. What must NOT happen is silently skipping hooks a repository DOES
// define, which is why the install failure is printed rather than swallowed.
const hookSetupScript = `
if [ -f .pre-commit-config.yaml ]; then
  if command -v pre-commit >/dev/null 2>&1 || pip install --quiet pre-commit 2>/dev/null; then
    pre-commit install --hook-type pre-commit --hook-type commit-msg >/dev/null 2>&1 \
      || echo "WARNING: pre-commit install failed; hooks will not run"
  else
    echo "WARNING: this repository defines pre-commit hooks but pre-commit could not be installed"
  fi
fi
` + angularHookScript

// angularHookScript installs a commit-msg hook that rejects a message which is
// not Conventional Commits.
//
// A HOOK RATHER THAN A PROMPT, which is this repository's usual answer. The
// message is already built in Go by commitMessage, so an agent cannot normally
// get it wrong — but the delegate engine commits for itself, a repository may
// carry its own tooling, and a rule that is only enforced on one path is a rule
// that stops being true the moment a second path appears. The hook holds for
// every commit made in the sandbox however it was made.
//
// ONLY IF NOTHING ELSE CLAIMED THE HOOK. pre-commit installs its own commit-msg
// above and a repository that runs commitlint has said what it wants; overwriting
// that would replace a project's rules with ours.
const angularHookScript = `
if [ ! -e .git/hooks/commit-msg ]; then
  mkdir -p .git/hooks
  cat > .git/hooks/commit-msg <<'BLACKSMITH_HOOK'
#!/bin/sh
# Conventional Commits, as the Angular project defines them.
subject=$(head -n 1 "$1")
case "$subject" in
  '#'*|'') exit 0 ;;
esac
if ! printf '%s' "$subject" | grep -Eq '^(feat|fix|chore|docs|refactor|test|perf|build|ci|style|revert)(\([a-z0-9._/-]+\))?!?: .+'; then
  echo "commit-msg: not Conventional Commits." >&2
  echo "  got:  $subject" >&2
  echo "  want: type(scope): subject   e.g. feat(store): add the in-memory store" >&2
  echo "  type is one of feat fix chore docs refactor test perf build ci style revert" >&2
  exit 1
fi
if [ "$(printf '%s' "$subject" | wc -c)" -gt 72 ]; then
  echo "commit-msg: subject is longer than 72 characters." >&2
  exit 1
fi
second=$(sed -n '2p' "$1")
if [ -n "$second" ]; then
  echo "commit-msg: leave a blank line between the subject and the body." >&2
  exit 1
fi
BLACKSMITH_HOOK
  chmod +x .git/hooks/commit-msg
fi
`

// branchFor names the branch this agent works on.
//
// A SUB-TASK WRITES ONTO ITS TASK'S BRANCH. That is the whole assembly
// mechanism: three sub-tasks each write one slice of the specification, all onto
// agent/<task>, and the task then reaches development with the complete thing.
// Giving each sub-task its own branch would leave three specifications in three
// places and nothing to develop against.
func (a *DevAgent) branchFor(t Ticket) string {
	// A SECTION WORKS ITS TASK'S BRANCH. That is the assembly: every section of a
	// task writes one tree, so the task's branch ends up holding the whole
	// specification rather than one slice of it per branch.
	//
	// ONLY A SECTION. The parent redirect used to apply to development too, and
	// because a TASK also has a parent — the request the product manager broke
	// down — every developer was sent to the REQUEST's branch, which is not where
	// anything was written.
	//
	// Measured on r73, and it is silent: the eight sections wrote their tests to
	// the four task branches, all four developers were pointed at agent/<request>
	// where there were none, each finished in seven records against an empty tree,
	// the security stage reported "empty diff" and the integrator merged it. The
	// whole board reported success in 10.3 minutes having delivered one struct,
	// while the specification sat on four branches nobody merged. All four
	// developers also shared that one branch, so they overwrote each other.
	//
	// The role is the discriminator because it is the only one there is: a task
	// and a section both carry a ParentID and are otherwise identical here. The
	// specification author is the stage that handles sections; development and
	// spec-merge handle tasks, and a task assembles onto ITS OWN branch — which is
	// exactly where its sections were told to write.
	//
	// A ticket with no parent — a request scoped as one unit of work — keeps its
	// own branch, which is the ordinary case this started as.
	if t.ParentID != nil && *t.ParentID != "" && a.role == roleSpec {
		return a.repo.BranchPrefix + shortID(*t.ParentID)
	}
	return a.repo.BranchPrefix + shortID(t.TicketID)
}

func (a *DevAgent) branchInput() string {
	if a.repo.BranchInput != "" {
		return a.repo.BranchInput
	}
	return "branch"
}

func stagedPaths(s *devState) []string {
	out := make([]string, 0, len(s.staged))
	for p := range s.staged {
		out = append(out, p)
	}
	return out
}

func (a *DevAgent) comment(ctx context.Context, t Ticket, body string) {
	if a.api == nil {
		return
	}
	if _, err := a.api.AddComment(ctx, t.TicketID, body); err != nil {
		// LOUD, not merely recorded. A stage's whole visible output is its comment:
		// if the write fails, the work happened and left no trace anywhere a person
		// looks, and the ticket reads as though the stage never ran. That is the
		// hardest failure to diagnose, because there is nothing to diagnose from.
		recorderFrom(ctx).Action(ctx, "ticket-comment", t.TicketID, err)
		slog.ErrorContext(ctx, "could not write the stage's comment; its work is invisible on the ticket",
			"ticket_id", t.TicketID, "error", err)
	}
}

// run executes a script in this attempt's sandbox.
//
// The script is written as though the repository is already checked out and
// current, because the Sandbox guarantees exactly that: the lease cloned it once
// at boot. Credentials are forge's business — blacksmith never holds one, and it
// never reaches the model.
func (a *DevAgent) run(ctx context.Context, rec *Recorder, s *devState, script string) (SandboxResult, error) {
	return s.sb.Run(ctx, rec, script)
}

// applyScript writes the staged files, base64-encoded so arbitrary content
// cannot break out of the shell script that carries it. This is the only place
// model-authored bytes reach a command line, and encoding them is what keeps
// "write a file" from becoming "run a command".
func (a *DevAgent) applyScript(s *devState) string {
	var b strings.Builder
	for p, content := range s.staged {
		fmt.Fprintf(&b, "\nmkdir -p \"$(dirname %q)\"\necho %q | base64 -d > %q",
			p, base64.StdEncoding.EncodeToString([]byte(content)), p)
	}
	b.WriteString("\n")
	return b.String()
}

// changedFromBaseline reports whether anything staged actually differs from the
// branch. baseline is the on-disk content captured on first read and never
// overwritten by the agent's own writes, which is what makes it a valid
// comparison — read is deliberately updated with staged content so the agent sees
// its own edits, and is therefore useless for this.
func (s *devState) changedFromBaseline() bool {
	for p, c := range s.staged {
		if b, ok := s.baseline[p]; !ok || b != c {
			return true
		}
	}
	return false
}

// treeHash fingerprints the staged tree. Paths are sorted so the same tree always
// yields the same value.
func (s *devState) treeHash() string {
	h := sha256.New()
	for _, p := range sortedKeys(s.staged) {
		fmt.Fprintf(h, "%s\x00%s\x00", p, s.staged[p])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// treeVerified reports whether the tree as it stands right now is the one the
// last verification ran against.
func (s *devState) treeVerified() bool {
	return s.lastTest != "" && s.verifiedTree == s.treeHash()
}

// proseOf returns what the model SAID around its action, with the JSON stripped.
//
// The syntax is what it imitates; the prose is what it reasoned. Keeping the
// second without the first is the whole point of carrying a history at all.
func proseOf(reply string) string {
	if reply == "" {
		return ""
	}
	out := jsonBlob.ReplaceAllString(reply, " ")
	out = strings.TrimSpace(strings.Join(strings.Fields(out), " "))
	return clip(out, 400)
}

// jsonBlob matches a JSON object spanning the reply, including the fenced and
// tagged wrappers models add unprompted.
var jsonBlob = regexp.MustCompile(`(?s)` + "```" + `(?:json)?.*?` + "```" + `|<[a-z_]+>.*?</[a-z_]+>|\{.*\}`)

// maxHistoryTurns bounds the record carried between turns.
//
// Enough to remember a line of attack and its consequences, not so much that the
// slot fills with old actions. Each entry is one action and one outcome line, so
// twenty of them cost far less than a single rendered state.
const maxHistoryTurns = 20

// remember records what the agent just did and what came of it, so the next turn
// can see it rather than re-deriving it.
func (s *devState) remember(action, outcome string) {
	if action == "" {
		return
	}
	entry := strings.TrimSpace(action) + "\n" + strings.TrimSpace(outcome)

	// AN IDENTICAL STEP REPEATED IS ONE FACT, NOT SEVERAL, and writing it out
	// several times costs twice: it spends the twenty-step window on one mistake,
	// evicting the reads and edits that explain how the agent got there, and it
	// spends prompt on redundancy — r74's looping developer reached 12,565 prompt
	// tokens to emit a 200-token repeat, most of it seven copies of one refusal.
	//
	// Collapsing it also makes the loop LEGIBLE. The agent could already be told
	// not to repeat itself and did anyway; a line saying it has now done the same
	// thing seven times running is a fact about its own behaviour, which is a
	// different thing to read than an instruction.
	if n := len(s.history); n > 0 && s.historyBase == entry {
		s.historyRepeats++
		s.history[n-1] = fmt.Sprintf("%s\n(the same action, the same result, %d times in a row)",
			entry, s.historyRepeats+1)
		return
	}
	s.historyBase, s.historyRepeats = entry, 0

	s.history = append(s.history, entry)
	if n := len(s.history); n > maxHistoryTurns {
		s.history = append([]string(nil), s.history[n-maxHistoryTurns:]...)
	}
}

// recentHistory is the record to show before the current state, as ONE user
// message and never as assistant turns.
//
// THE ASSISTANT CHANNEL IS A FORMAT EXAMPLE, whatever it is used for. Replaying
// raw completions there taught the model to imitate the wire format, so the
// replay became descriptions instead — and it then imitated the descriptions,
// emitting `write_files: store_concurrent_test.go lines 169-169, 1B [169]` as a
// literal reply. Off-schema output is unconstrained output, and the collapse
// followed: nine of one attempt's turns ran to 9,000-17,000 characters of
// `}(i)}(i)}(i)` before the sampler was cut off.
//
// The lesson generalises past both fixes. Anything placed in the assistant role
// is a demonstration of what to produce, so the only safe content there is a
// real reply — and a description of a reply is not one. Putting the record in
// the user channel leaves the system prompt's worked example as the single
// precedent for what an answer looks like.
func (s *devState) recentHistory() []Message {
	if len(s.history) == 0 {
		return nil
	}
	var b strings.Builder
	b.WriteString("WHAT YOU HAVE ALREADY TRIED on this ticket, oldest first, with " +
		"what came of each. Do not repeat an action that was refused, and do not " +
		"re-make an edit that already landed.")
	for i, h := range s.history {
		fmt.Fprintf(&b, "\n\n%d. %s", i+1, h)
	}
	return []Message{{Role: "user", Content: b.String()}}
}

// nothingToTest reports whether a verification asked for with nothing staged is
// genuinely without a subject.
//
// It is not, when a retry has inherited a branch: staged is empty at the start of
// every attempt after the first, while the branch already carries the earlier
// attempts' commits. Testing that is a real verdict on real code. It IS without a
// subject when this attempt already has a verdict, or when there is no branch
// content at all.
func (s *devState) nothingToTest() bool {
	return s.lastTest != "" || len(s.tree) == 0
}

// ordinalSuffix returns "st", "nd", "rd" or "th" for n.
func ordinalSuffix(n int) string {
	if n%100 >= 11 && n%100 <= 13 {
		return "th"
	}
	switch n % 10 {
	case 1:
		return "st"
	case 2:
		return "nd"
	case 3:
		return "rd"
	}
	return "th"
}

// isDocFile reports whether a path is project documentation.
//
// Markdown is the whole rule, and that is on purpose: the architect writes
// markdown and nothing else (sanitiseDesign refuses anything that is not .md),
// so the two halves of the boundary are defined the same way and cannot drift
// apart. A repository documenting itself in some other format would need this to
// be configuration; today it would be configuration with one possible value.
func isDocFile(p string) bool { return strings.EqualFold(path.Ext(path.Clean(p)), ".md") }

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// commitMessage builds a Conventional Commits message.
//
// The type prefix is not cosmetic. Repositories routinely run commitlint on the
// commit-msg hook, and a message without a recognised type is rejected — so an
// agent that writes plain subjects produces branches that cannot be committed at
// all. An unrecognised or missing type falls back to "chore", which is the
// honest choice: wrong-but-valid beats confidently mislabelled.
func commitMessage(t Ticket, summary, commitType string) string {
	if !contains(conventionalTypes, commitType) {
		commitType = "chore"
	}
	subject := strings.TrimSpace(summary)
	if subject == "" {
		subject = t.Title
	}
	// Conventional Commits subjects are lower-case and unpunctuated by
	// convention, and commitlint's default config enforces both.
	subject = strings.TrimRight(subject, ".")
	// THE FIRST RUNE, NOT THE FIRST BYTE. subject[:1] takes one byte, and
	// lowercasing half of a two-byte rune leaves invalid UTF-8 that git will not
	// take. Found by the test that drives real agent prose through the hook.
	if subject != "" {
		r := []rune(subject)
		r[0] = unicode.ToLower(r[0])
		subject = string(r)
	}
	// The WHOLE line is what the hook measures, so the budget is what is left of
	// it after the type and the separator.
	subject = clipSubject(subject, maxSubjectBytes-len(commitType)-2)
	return commitType + ": " + subject + "\n\nTicket: " + t.TicketID
}

// splitCommitScript commits each written file on its own, ahead of the catch-all
// commit that follows it.
//
// ONE COMMIT PER FILE WHEN THERE IS MORE THAN ONE. A single commit carrying the
// store, the handlers and the board page is three changes a reviewer has to
// separate by hand, and its subject can only describe one of them — the agents'
// history is full of "test: added handlers.go (TicketHandler with...)" truncated
// mid-thought. Per file, each subject is short enough to be true.
//
// GENERATED FROM s.staged, not discovered in the shell. These are the paths this
// agent wrote this attempt, known here, so the messages are built in Go with the
// same rules as any other commit rather than assembled by string-mashing inside a
// script. Anything the loop does not name — a formatter's rewrite, a lock file
// the dependency step resolved — is still caught by the `git add -A` after it,
// which is why that commit stays.
//
// A file that fails to commit is not an error: it may have been staged and
// committed by an earlier attempt, or reverted by a formatter. The catch-all
// behind it is the backstop, so each of these is allowed to be a no-op.
func (a *DevAgent) splitCommitScript(t Ticket, s *devState) string {
	if len(s.staged) < 2 {
		return ""
	}
	paths := make([]string, 0, len(s.staged))
	for p := range s.staged {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	var b strings.Builder
	b.WriteString("\n")
	for _, p := range paths {
		fmt.Fprintf(&b, "git add -- %s 2>/dev/null || true\n", shellSingleQuote(p))
		// Nothing staged for this path means nothing to commit for it.
		b.WriteString("if ! git diff --cached --quiet; then\n")
		b.WriteString("  " + commitMessageScript(perFileCommitMessage(t, s.summary, s.commitType, p)))
		b.WriteString("  " + gitCommit + " >/dev/null || true\n")
		b.WriteString("fi\n")
	}
	return b.String()
}

// maxSubjectBytes is the longest a commit subject may be, measured the way the
// commit-msg hook measures it.
//
// IN BYTES, NOT RUNES, and the difference cost a whole run. clip trims to a rune
// count and appends a three-byte ellipsis; the hook counts bytes with `wc -c`,
// because a container's locale cannot be relied on to make `wc -m` mean
// characters. The agents write prose with em dashes, so a subject of seventy
// runes was comfortably over seventy-two bytes.
//
// Measured on r104: every specification section was rejected by the hook, which
// failed the commit, which failed the push, which blocked all five of them and
// the task under them. A cosmetic rule ended the run, and it ended it with
// "could not push the branch" — a message pointing at the network.
const maxSubjectBytes = 72

// clipSubject trims a subject to fit maxSubjectBytes, cutting on a rune boundary
// so the result is still valid UTF-8.
//
// No ellipsis: the marker is what pushed the line over the limit in the first
// place, and a subject is a summary — a truncated one reads no worse for ending
// bluntly than for ending in a character that costs three of the bytes it is
// apologising for.
func clipSubject(s string, maxBytes int) string {
	s = strings.TrimSpace(s)
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	// Back up off a partial rune.
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return strings.TrimSpace(s[:cut])
}

// commitScope turns a path into a Conventional Commits scope.
//
// The file's own name, without its extension and without the _test that marks it
// as the tests for something: store_test.go and store.go are the same scope,
// because they are the same subject seen from two sides.
func commitScope(path string) string {
	base := path
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	if i := strings.Index(base, "."); i > 0 {
		base = base[:i]
	}
	base = strings.TrimSuffix(base, "_test")
	// A scope has to be safe in a subject line and lower-case by convention.
	base = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		case r >= 'A' && r <= 'Z':
			return r + 32
		}
		return -1
	}, base)
	return base
}

// perFileCommitMessage is one file's commit: a single subject line and nothing
// else but the trailer that links it to its ticket.
//
// SINGLE LINE BECAUSE THE COMMIT IS SMALL. A body explaining one file's change,
// repeated once per file, says the same thing several times and buries the one
// line that differs. The trailer stays: it is what connects a commit to the
// ticket that asked for it, and losing that to save two lines would trade
// something load-bearing for something cosmetic.
func perFileCommitMessage(t Ticket, summary, commitType, path string) string {
	if !contains(conventionalTypes, commitType) {
		commitType = "chore"
	}
	scope := commitScope(path)
	head := commitType
	if scope != "" {
		head = commitType + "(" + scope + ")"
	}
	subject := strings.TrimSpace(summary)
	if subject == "" {
		subject = t.Title
	}
	subject = strings.TrimRight(subject, ".")
	// THE FIRST RUNE, NOT THE FIRST BYTE. subject[:1] takes one byte, and
	// lowercasing half of a two-byte rune leaves invalid UTF-8 that git will not
	// take. Found by the test that drives real agent prose through the hook.
	if subject != "" {
		r := []rune(subject)
		r[0] = unicode.ToLower(r[0])
		subject = string(r)
	}
	subject = clipSubject(subject, maxSubjectBytes-len(head)-2)
	return head + ": " + subject + "\n\nTicket: " + t.TicketID
}

// assertIntent is what an assertion's own failure message says it wants to see.
type assertIntent int

const (
	intentUnknown assertIntent = iota
	intentWantsError
	intentWantsSuccess
)

// wantsSuccessPhrases are checked BEFORE wantsErrorPhrases and the order is not
// cosmetic: "should not return an error" contains "return an error", so testing
// for the error phrasing first would classify every negated message backwards —
// the exact mistake this gate exists to catch.
var wantsSuccessPhrases = []string{
	"should not fail", "should not return", "should not error", "should not have",
	"must not fail", "must not return", "unexpected error", "should succeed",
	"expected no error", "want no error", "did not expect", "no error expected",
	"should work", "should have succeeded",
}

var wantsErrorPhrases = []string{
	"should return error", "should return an error", "should fail", "should error",
	"expected error", "expected an error", "want error", "wanted error",
	"must fail", "must return error", "must return an error", "expected failure",
	"should have failed", "should have returned an error", "expects an error",
	"should reject", "must reject",
}

// intentOf reads what a failure message claims to be asserting. Unknown unless
// the wording is unambiguous — a gate that guesses at prose would reject correct
// specifications, which is far more expensive than missing an inverted one.
func intentOf(msg string) assertIntent {
	m := strings.ToLower(msg)
	for _, p := range wantsSuccessPhrases {
		if strings.Contains(m, p) {
			return intentWantsSuccess
		}
	}
	for _, p := range wantsErrorPhrases {
		if strings.Contains(m, p) {
			return intentWantsError
		}
	}
	return intentUnknown
}

// errNilCompare reports whether cond compares something error-shaped against
// nil, and whether the branch is taken when that value is NON-nil.
func errNilCompare(cond ast.Expr) (branchOnNonNil bool, ok bool) {
	be, isBinary := cond.(*ast.BinaryExpr)
	if !isBinary || (be.Op != token.NEQ && be.Op != token.EQL) {
		return false, false
	}
	isNil := func(e ast.Expr) bool {
		id, isIdent := e.(*ast.Ident)
		return isIdent && id.Name == "nil"
	}
	isErrLike := func(e ast.Expr) bool {
		id, isIdent := e.(*ast.Ident)
		if !isIdent {
			return false
		}
		n := strings.ToLower(id.Name)
		return n == "err" || strings.HasPrefix(n, "err") || strings.HasSuffix(n, "err") ||
			strings.HasSuffix(n, "error")
	}
	switch {
	case isNil(be.Y) && isErrLike(be.X):
	case isNil(be.X) && isErrLike(be.Y):
	default:
		return false, false
	}
	return be.Op == token.NEQ, true
}

// failureMessage returns the literal message of the first t.Error/Errorf/Fatal/
// Fatalf call in body, and whether one was found.
func failureMessage(body *ast.BlockStmt) (string, bool) {
	var msg string
	var found bool
	ast.Inspect(body, func(n ast.Node) bool {
		if found {
			return false
		}
		call, isCall := n.(*ast.CallExpr)
		if !isCall {
			return true
		}
		sel, isSel := call.Fun.(*ast.SelectorExpr)
		if !isSel {
			return true
		}
		switch sel.Sel.Name {
		case "Error", "Errorf", "Fatal", "Fatalf":
		default:
			return true
		}
		for _, arg := range call.Args {
			lit, isLit := arg.(*ast.BasicLit)
			if !isLit || lit.Kind != token.STRING {
				continue
			}
			if s, err := strconv.Unquote(lit.Value); err == nil && strings.TrimSpace(s) != "" {
				msg, found = s, true
				return false
			}
		}
		return true
	})
	return msg, found
}

// invertedErrorAssertions names assertions whose CONDITION contradicts their own
// MESSAGE — the check fires in the case the message says is correct.
//
// THIS IS THE MOST EXPENSIVE SPECIFICATION DEFECT THERE IS, because it is the
// only one the developer cannot escape. Measured on r55: a section's spec wrote
//
//	if err := store.Update("non-existent", task); err != nil {
//	    t.Errorf("Update should return error for non-existent task, got: %v", err)
//	}
//
// which demands that Update NOT return an error while saying the opposite, and
// got Get right three lines above. The implementation was correct — it returned
// "task not found" — and failed BECAUSE it was correct. The developer may not
// edit test files, so it cannot fix the assertion; it thrashed between the two
// readings across three commits, hit the dead-action ceiling, was killed, and
// did it three more times. The task then blocked and the ten sections behind it
// stranded: one inverted operator cost the whole run.
//
// Caught here, at authoring time, because this is the last point at which the
// agent that CAN fix it is still holding it.
func invertedErrorAssertions(staged map[string]string) []string {
	var bad []string
	for _, p := range sortedKeys(staged) {
		if !isTestFile(p) {
			continue
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, p, staged[p], parser.SkipObjectResolution)
		if err != nil {
			continue
		}
		ast.Inspect(f, func(n ast.Node) bool {
			ifs, isIf := n.(*ast.IfStmt)
			if !isIf {
				return true
			}
			branchOnNonNil, ok := errNilCompare(ifs.Cond)
			if !ok {
				return true
			}
			msg, found := failureMessage(ifs.Body)
			if !found {
				return true
			}
			intent := intentOf(msg)
			if intent == intentUnknown {
				return true
			}
			// Branch taken when the error IS present, but the message says an
			// error is what the test wanted: the assertion fails on success.
			inverted := (branchOnNonNil && intent == intentWantsError) ||
				(!branchOnNonNil && intent == intentWantsSuccess)
			if !inverted {
				return true
			}
			want, got := "err == nil", "err != nil"
			if !branchOnNonNil {
				want, got = "err != nil", "err == nil"
			}
			bad = append(bad, fmt.Sprintf("%s:%d — %q is guarded by `%s` and should be `%s`",
				p, fset.Position(ifs.Pos()).Line, clip(msg, 80), got, want))
			return true
		})
	}
	return bad
}

// implementationInTests names the exported top-level declarations a test file
// makes — the ones that belong to the code under test rather than to the test.
//
// Test entry points are not declarations of this kind and are skipped by name:
// TestX, BenchmarkX, FuzzX and ExampleX are exported by the convention that makes
// the runner find them, which says nothing about what they own.
func implementationInTests(staged map[string]string) []string {
	var own []string
	for _, p := range sortedKeys(staged) {
		if !isTestFile(p) {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), p, staged[p], parser.SkipObjectResolution)
		if err != nil {
			continue
		}
		for _, d := range f.Decls {
			switch decl := d.(type) {
			case *ast.FuncDecl:
				if decl.Recv != nil || isTestEntryPoint(decl.Name.Name) || !decl.Name.IsExported() {
					continue
				}
				own = append(own, "func "+decl.Name.Name)
			case *ast.GenDecl:
				for _, spec := range decl.Specs {
					switch sp := spec.(type) {
					case *ast.TypeSpec:
						if sp.Name.IsExported() {
							own = append(own, "type "+sp.Name.Name)
						}
					case *ast.ValueSpec:
						for _, n := range sp.Names {
							if n.IsExported() {
								own = append(own, "var "+n.Name)
							}
						}
					}
				}
			}
		}
	}
	return own
}

// isTestEntryPoint reports whether a name is one the go test runner discovers.
func isTestEntryPoint(name string) bool {
	for _, prefix := range []string{"Test", "Benchmark", "Fuzz", "Example"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// emptySubtests names the t.Run subtests in one test function whose closures
// contain no statements.
//
// THE STUB MOVED DOWN A LEVEL. Blocking empty test functions did not stop the
// pattern, it relocated it: an author wrote TestGetTasksFiltering containing
// four t.Run calls, every one of them a closure holding a single "// TODO:
// Implement test for ..." comment. The outer function has statements — the
// t.Run calls themselves — so the top-level check saw nothing wrong, and the
// file compiled, ran, and reported four subtests passing.
//
// It was still caught, because a suite that asserts nothing passes and the red
// gate refuses a passing spec. But "your tests pass" is a conclusion the author
// has to work backwards from, and it spent thirty-seven turns doing that. Naming
// the four closures says the same thing on the first turn.
func emptySubtests(fn *ast.FuncDecl) []string {
	var empty []string
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		// t.Run(name, func(t *testing.T) { ... }) — matched on the method name
		// rather than the receiver's type, which is not resolved here. A call to
		// something else named Run taking a closure is rare, and the cost of
		// treating one as a subtest is a message naming a closure that is
		// deliberately empty.
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Run" {
			return true
		}
		lit, ok := call.Args[len(call.Args)-1].(*ast.FuncLit)
		if !ok || lit.Body == nil || !isStubBody(lit.Body) {
			return true
		}
		empty = append(empty, fn.Name.Name+"/"+subtestName(call.Args[0]))
		return true
	})
	return empty
}

// isStubBody reports whether a test body asserts nothing — either because it is
// empty, or because it does nothing but skip.
//
// t.Skip IS THE SAME LIE AS AN EMPTY BODY, and it slipped past every gate built
// for the empty one. Merged on a live run: store_test.go with four functions,
// each holding a comment and t.Skip("Not implemented"). The bodies were not
// empty, there were no empty subtests, nothing declared the implementation, and
// the file parsed — so the stub detector saw nothing wrong. The red check did
// not save it either: with no implementation the file failed to COMPILE, which
// reads as red and is accepted; once the developer wrote the code the suite
// compiled, every test skipped, and `go test` exited 0. A specification of four
// skips passed as finished work.
//
// A CONDITIONAL SKIP IS LEGITIMATE and must survive: "if testing.Short() {
// t.Skip() }" is how a real suite opts out of slow cases. Only an UNCONDITIONAL
// skip at the top level of the body is a stub, which is why this looks at
// statements rather than searching the whole subtree.
func isStubBody(body *ast.BlockStmt) bool {
	if body == nil || len(body.List) == 0 {
		return true
	}
	for _, stmt := range body.List {
		expr, ok := stmt.(*ast.ExprStmt)
		if !ok {
			continue
		}
		call, ok := expr.X.(*ast.CallExpr)
		if !ok {
			continue
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			continue
		}
		switch sel.Sel.Name {
		case "Skip", "Skipf", "SkipNow":
			return true
		}
	}
	return false
}

// subtestName recovers the label from t.Run's first argument when it is a plain
// string, so the complaint can point at the subtest the author wrote rather than
// at a position in a file.
func subtestName(arg ast.Expr) string {
	lit, ok := arg.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "?"
	}
	if s, err := strconv.Unquote(lit.Value); err == nil {
		return s
	}
	return "?"
}

// autoFinishOnGreen reports whether the loop ends the stage itself once the
// checks pass.
//
// ON BY DEFAULT, and it should stay that way in normal operation: left to decide
// for itself a model does not stop. Observed on a real ticket — verification
// returned exit 0 and the agent kept editing for another six turns until the
// budget ran out, at which point a green branch was reported as a failure with
// no passing change. Polishing past a passing test has no upside and one large
// downside.
//
// It exists as a switch because it also HIDES something. While the loop finishes
// on the agent's behalf, the agent never sees a passing state to react to, so
// "does it recognise it is done?" cannot be answered — the harness always answers
// first. Turning this off is how that question gets asked, and it is an
// experiment rather than a mode: a run with it off will not stop on its own.
func autoFinishOnGreen() bool {
	return strings.TrimSpace(os.Getenv("AGENTS_DEV_AUTOFINISH")) != "false"
}

// coverageScript runs the suite with coverage reporting.
func (a *DevAgent) coverageScript() string {
	cmd := a.repo.CoverageCommand
	if cmd == "" {
		cmd = defaultCoverageCommand
	}
	return sandboxPreamble + cmd + "\n"
}

// defaultCoverageCommand reports WEIGHTED TOTAL statement coverage.
//
// -coverprofile over ./... writes one merged profile across every package, and
// `go tool cover -func` ends with a "total:" line computed as statements covered
// over statements total. That is the real figure, and it is worth the second
// command because the alternatives are all wrong in the same direction:
//
//   - the highest per-package number flatters outright, and one tiny fully
//     covered package would satisfy any target;
//   - averaging the per-package numbers weights every package equally regardless
//     of size, so three statements at 100% offset a thousand at 20%.
//
// The profile is written inside the sandbox's checkout and goes nowhere: the
// stage reads the number off stdout and the sandbox is discarded.
const defaultCoverageCommand = "go test -coverprofile=coverage.out ./... && go tool cover -func=coverage.out | tail -1"

// defaultDepsCommand resolves Go's manifest and lock file together.
//
// `go mod tidy` rather than `go mod download`: download fetches what go.mod
// already names and leaves go.sum alone, which is exactly the state an agent
// gets stuck in. tidy writes the sum entries AND removes requires nothing
// imports any more, so the manifest that lands matches the code that landed.
const defaultDepsCommand = "go mod tidy"

// defaultFormatCommand is goimports, NOT gofmt, and the difference decides
// whether a whole class of ticket can be finished at all.
//
// gofmt formats; goimports also FIXES THE IMPORT BLOCK — it removes imports
// nothing uses and adds the standard-library ones the code refers to. Those two
// mistakes are not syntax errors, so the specification author's gate (which
// checks parsing, deliberately, because the spec refers to types that do not
// exist yet) passes them straight through. They surface later as compile errors
// in a TEST FILE — which the developer is forbidden to edit.
//
// That combination is unwinnable, and it was measured: a ticket whose only two
// faults were `"time" imported and not used` and `undefined: fmt` could not be
// completed by any agent in the pipeline. Running goimports over the same branch
// removed one line, added another, and `go test ./...` passed.
//
// Pinned rather than @latest: this runs publisher-chosen code inside the sandbox
// on every commit, and unpinnedTools exists to complain about exactly that. The
// gofmt fallback keeps a host that cannot fetch the tool formatting as before
// rather than not at all.
const defaultFormatCommand = "go run golang.org/x/tools/cmd/goimports@v0.28.0 -w . || gofmt -w ."

// coveragePattern reads a percentage out of tool output.
//
// Deliberately loose: it matches "coverage: 82.4% of statements" and a bare
// "82.4%", because pinning it to one tool's exact wording would break the stage
// the first time someone set a different coverage command.
var coveragePattern = regexp.MustCompile(`(\d+(?:\.\d+)?)%`)

// parseCoverage reads the coverage figure, preferring a stated TOTAL.
//
// A "total:" line is the weighted figure — statements covered over statements
// total — and is the only one that means what a coverage target is supposed to
// mean. `go tool cover -func` emits it last, which is what the default command
// asks for.
//
// Without one, this falls back to the MEAN of the per-package figures. That is
// wrong for packages of unequal size, and it is chosen anyway because the
// alternative fallback is worse: taking the highest lets a single tiny fully
// covered package satisfy any target, which is not a measurement so much as an
// invitation. The mean at least moves in the right direction when a real package
// is untested. An operator who sets a custom coverage command should make it
// report a total.
func parseCoverage(out string) (float64, bool) {
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(strings.ToLower(line), "total:") {
			continue
		}
		if m := coveragePattern.FindStringSubmatch(line); m != nil {
			if v, err := strconv.ParseFloat(m[1], 64); err == nil {
				return v, true
			}
		}
	}

	var sum float64
	var n int
	for _, m := range coveragePattern.FindAllStringSubmatch(out, -1) {
		v, err := strconv.ParseFloat(m[1], 64)
		if err != nil {
			continue
		}
		sum += v
		n++
	}
	if n == 0 {
		return 0, false
	}
	return sum / float64(n), true
}

// pushScript builds the commit-and-push for a branch. Extracted so its SHAPE can
// be asserted: the bug it guards against passed every behavioural test in the
// suite while silently destroying another stage's work.
func (a *DevAgent) pushScript(t Ticket, branch string, s *devState) string {
	return fmt.Sprintf("git fetch -q origin %q 2>/dev/null && git checkout -q -B %q FETCH_HEAD 2>/dev/null || git checkout -q -B %q\n",
		branch, branch, branch) +
		// BEFORE the edits land, so the verification runs against the tree that
		// includes this ticket's dependencies. The checkout above resets to the
		// remote branch, so a merge done only in the sandbox would be discarded
		// here; doing it again is what gets it committed and pushed.
		a.mergeIntegrationScript() +
		a.applyScript(s) +
		a.depsScript() +
		a.formatScript() +
		hookSetupScript +
		a.commitAndPushScript(t, s, branch)
}

// commitAndPushScript commits what the agent wrote and pushes it.
//
// THE LAST COMMIT IS GUARDED, and leaving it unguarded destroyed a run. The
// per-file split above commits each written file on its own; when it has taken
// all of them there is nothing left for this one, `git commit` fails with
// "nothing to commit", the retry fails the same way, and the preamble's `set -e`
// aborts the script BEFORE the push. The work is committed inside the sandbox
// and thrown away with it.
//
// Measured on r110, and it is silent in exactly the way that costs most: the
// developer wrote ticket.go, store.go, main.go and handlers.go, all correct, and
// the branch ended up holding only ticket.go — the one write made while a single
// staged file meant the split did not run. Every turn after that the agent was
// shown its own correct main.go and told by the gate that func main was
// undeclared, because the gate builds the BRANCH and the branch never got it.
// Twenty turns of an agent being right and being told it was wrong.
//
// So the final commit runs only when something is staged for it, and the push
// runs either way.
func (a *DevAgent) commitAndPushScript(t Ticket, s *devState, branch string) string {
	return a.splitCommitScript(t, s) +
		"\ngit add -A\n" +
		"if ! git diff --cached --quiet; then\n" +
		"  " + commitMessageScript(commitMessage(t, s.summary, s.commitType)) +
		"  " + gitCommit + " || {\n" +
		// Formatters and linters that rewrite files fail the commit AND leave the
		// fixes in the working tree. Re-staging and retrying once turns that from
		// an unexplained failure into the no-op it should be; a second failure is
		// a real rejection and is reported.
		"    echo '--- hooks modified files or rejected the commit; retrying once ---'\n" +
		"    git add -A\n" +
		"    " + gitCommit + "\n" +
		"  }\n" +
		"fi\n" +
		fmt.Sprintf("git push --force origin %q\n", branch)
}

// unterminatedTag matches a struct-tag literal that opened with a backtick and
// reached the end of the line without closing.
//
// It is deliberately narrow. The tag body must look like Go struct tags —
// key:"value" pairs and nothing else — so an ordinary raw string that happens to
// run past a newline does not match, and neither does a line with a backtick in
// a comment or an expression.
var unterminatedTag = regexp.MustCompile("`((?:[A-Za-z_][A-Za-z0-9_.-]*:\"[^\"`]*\"[ \t]*)+)$")

// resetAgent clears what the AGENT has accumulated and keeps what it has BUILT.
//
// THE WORK IS NOT THE PROBLEM; THE MEMORY IS. An attempt that has run forty
// turns has usually written real code and then lost the thread — measured on one
// ticket, 77 turns of which 46 were repeat verifications, with the action trail
// showing 46 consecutive identical entries under a heading telling it not to
// repeat itself. A model reads that list and continues the pattern it can see.
// Throwing the ticket away would discard working code because the agent got
// confused; throwing the CONFUSION away keeps both.
//
// WHAT SURVIVES IS THE WORLD. staged has to: pushScript re-applies it on top of
// the fetched branch, so an attempt whose writes were never verified would lose
// them outright if this cleared it. The files it wrote are folded into baseline
// instead, which is what makes this a reset rather than an undo — to the agent
// that continues, its own earlier work is simply the code that was already
// there, exactly as the specification tests are.
//
// The last verification survives too. "Fix the remaining failures" needs the
// failures; a fresh agent without them would spend its first turn rediscovering
// what the previous one already knew, which is the cost this is meant to avoid.
func (s *devState) resetAgent(mode agentMode) {
	s.resets++
	s.lastResetAt = s.iteration

	// The agent's own writes become part of the ground truth. baseline exists to
	// answer "is this write discarding code that was already there", and after a
	// reset the honest answer includes the code the previous turns wrote.
	for p, c := range s.staged {
		s.baseline[p] = c
	}

	// Everything below is memory of HOW it got here, which is the thing being
	// discarded.
	s.trail = nil
	s.notice = ""
	s.lastReply = ""
	s.truncated = false
	s.refusals = 0
	s.noopEdits = 0
	s.staleReads = 0
	s.parseFails = 0
	s.summary = ""
	s.missing = map[string]bool{}
	// The write counters are memory too: they exist to describe this agent's
	// activity, and a fresh agent has not written anything yet.
	s.writes = 0
	s.verifiedWrites = 0

	// THE BRIEF MUST MATCH THE JOB. DevAgent is three stages sharing one loop, and
	// the first version of this told all of them to "make the remaining test
	// failures pass" — which is the developer's job and the exact inverse of the
	// spec author's, whose tests are SUPPOSED to fail. Observed on a live ticket:
	// a test author was restarted twice and instructed, in its own prompt, to do
	// the one thing its own gate refuses.
	job := "Your job is narrow: make the remaining test failures pass. Read the verification " +
		"output, change what is wrong, run the tests. If the existing approach is wrong, replace it."
	switch mode {
	case modeTest:
		job = "Your job is unchanged: finish the SPECIFICATION for this ticket. The tests already " +
			"written are shown above — keep what is right, add what the ticket asks for and is " +
			"still missing. They must FAIL against the current code; that is what makes them a " +
			"specification, and it is what the gate checks."
	case modeSpecMerge:
		job = "Your job is unchanged: make the sections on this branch compile as ONE package. " +
			"Rename a duplicate, fold two identical helpers into one — and weaken nothing: every " +
			"assertion that was here must still be here when you finish."
	case modeCoverage:
		job = "Your job is unchanged: add tests for the branches the specification did not cover. " +
			"The suite passes and must keep passing, and the tests that were here before you are " +
			"not yours to edit."
	}
	s.restart = fmt.Sprintf(
		"YOU ARE PICKING UP THIS TICKET PART-WAY THROUGH. %d turns of work came before you and it "+
			"is unfinished. That work is in the repository and is shown above as the current "+
			"contents — treat it as what is already there, not as a draft to defend, and read the "+
			"verification result above for where it stands.\n\n%s\n\nThis is restart %d.",
		s.iteration, job, s.resets)
}

// restartSuffix reports how many times the agent was reset during an attempt, so
// the figure is visible on the ticket rather than only in the logs. Silent at
// zero: most attempts never reach the first reset, and a line saying so on every
// one of them would be noise.
func restartSuffix(resets int) string {
	if resets == 0 {
		return ""
	}
	if resets == 1 {
		return " It was restarted once along the way, keeping its work."
	}
	return fmt.Sprintf(" It was restarted %d times along the way, keeping its work each time.", resets)
}

// mergeIntegrationScript brings the integration branch into the working branch.
//
// IDEMPOTENT AND NON-FATAL. Already merged is a no-op; a CONFLICT aborts and
// leaves the tree exactly as it was rather than half-merged, because a developer
// facing a conflicted working tree can do nothing useful with it — resolving
// belongs to the integrator, which has a stage and a model for it. A ticket that
// cannot take its dependency cleanly is better off failing on honest compile
// errors than on merge markers.
func (a *DevAgent) mergeIntegrationScript() string {
	// BOTH STAGES THAT WORK AGAINST EXISTING CODE TAKE IT. The developer needs the
	// dependency to compile against; the spec author needs it to write tests that
	// NAME the right things — Task and Store belong to the foundation ticket, and
	// an author that cannot see them invents their shape for the developer to
	// contradict.
	//
	// This does not weaken the red gate. The integration branch holds OTHER
	// tickets' work, never this ticket's subject, so a specification of something
	// unbuilt still fails. If it somehow passes, that is a vacuous spec and the
	// red check is right to refuse it.
	if a.repo.IntegrationBranch == "" || (a.mode != modeDevelop && a.mode != modeTest) {
		return ""
	}
	q := shellSingleQuote(a.repo.IntegrationBranch)
	return "git fetch -q origin " + q + " 2>/dev/null && " +
		"git -c user.email=agent@blacksmith -c user.name=blacksmith " +
		"merge -q --no-edit FETCH_HEAD 2>/dev/null || git merge --abort 2>/dev/null || true\n"
}

// undoLastEdit puts the staged tree back to before the most recent write.
func (s *devState) undoLastEdit() (string, bool) {
	if len(s.undoStack) == 0 {
		return "", false
	}
	prev := s.undoStack[len(s.undoStack)-1]
	s.undoStack = s.undoStack[:len(s.undoStack)-1]

	var restored []string
	for p, was := range prev {
		if s.staged[p] != was {
			restored = append(restored, p)
		}
		s.staged[p] = was
		s.read[p] = was
	}
	// A file created by the undone edit has no previous state and must go.
	for p := range s.staged {
		if _, existed := prev[p]; !existed {
			delete(s.staged, p)
			delete(s.read, p)
			restored = append(restored, p+" (removed; the edit created it)")
		}
	}
	sort.Strings(restored)
	if len(restored) == 0 {
		return "nothing differed", true
	}
	return strings.Join(restored, ", "), true
}

// minTriesBeforeReferee is how hard the developer must have tried before the
// question is worth asking.
//
// Not the first failure: test-first work STARTS red, and a referee asked at turn
// one would be judging a ticket whose implementation does not exist yet. By the
// third failed verification the developer has written real code and the failure
// is about that code rather than its absence.
const minTriesBeforeReferee = 3

// refereeThreshold is minTriesBeforeReferee, unless the stage cannot reach it.
//
// A DELEGATED STAGE HAS FEWER TRIES THAN THAT. maxDelegateAttempts is 2, and
// failedVerifications counts within one stage run, so a fixed 3 made the referee
// unreachable in delegate mode — measured on a clean run, six consecutive dev
// failures on one ticket with no verdict ever asked for. The threshold has to
// follow the ceiling it is measured against, or it silently switches the referee
// off whenever that ceiling moves.
func (a *DevAgent) refereeThreshold() int {
	if a.delegate != "" && maxDelegateAttempts < minTriesBeforeReferee {
		return maxDelegateAttempts
	}
	return minTriesBeforeReferee
}

// refereeOn asks the referee at most once per attempt, and only when the
// developer has genuinely been trying.
//
// BOUNDED ON PURPOSE. It is one extra model call per stuck attempt, not per turn:
// the verdict is about the ticket's shape, which does not change between turns,
// so asking again would spend a slot to hear the same answer. It also needs no
// sandbox, so it costs a call and nothing else.
func (a *DevAgent) refereeOn(ctx context.Context, rec *Recorder, s *devState, out string) *refereeVerdict {
	if a.mode != modeDevelop || s.refereeAsked || s.failedVerifications < a.refereeThreshold() {
		return s.referee
	}
	s.refereeAsked = true
	s.referee = a.askReferee(ctx, rec, s.ticket, s, out)
	if s.referee != nil {
		recordRefereeVerdict(ctx, s.referee.Owner, s.referee.Confidence)
	}
	return s.referee
}
