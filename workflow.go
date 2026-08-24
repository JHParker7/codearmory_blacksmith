package main

// THE BOARD IS THE WORKFLOW.
//
// Every stage used to be selected by a MARKER left in a ticket comment — a
// triage comment meant triaged, a "pushed a branch" comment meant ready to
// review. That worked, and it was wrong in ways that cost real time:
//
//   - the state was invisible. A person looking at the board saw a column of
//     identical "open" tickets and had to read comments to learn that four were
//     waiting on review and one had been stuck for a day;
//   - selection needed the comments, and a listing does not carry them, so every
//     candidate cost a full fetch every poll — and when that hydration was
//     missed, predicates silently inverted: a stage requiring a marker selected
//     nothing forever while a stage requiring its absence re-claimed everything;
//   - and the markers were prose. Changing the wording of a comment changed the
//     routing of the pipeline.
//
// Now the STATUS is the stage. The tickets service models board columns as
// per-board status field defs, so these are real columns a person sees and can
// drag between, `?status=` filters server-side, and the listing already carries
// everything selection needs. A stage takes from one column and moves the ticket
// to the next, which is what a team does with a physical board.
//
// The markers do not disappear — they are still how a stage records WHAT it did,
// and the comment remains the audit trail. They simply stop being how work is
// routed.

// The columns, in board order. Values are what the tickets service stores;
// labels are what a person reads.
const (
	// ColInbox is where a request lands. Nothing else writes here, which is what
	// makes intake safe: a request is a ticket like any other, and the only stage
	// that takes from it is the first one of the pipeline.
	ColInbox = "inbox"
	// ColDesigning is the architect holding a request while it writes the project
	// documentation, and ColReadyForScoping is where it hands the request on.
	//
	// THE ARCHITECT RUNS FIRST, BEFORE THE BREAKDOWN, and the order carries the
	// whole value. Its output is committed to the base branch, so it is present in
	// the tree EVERY LATER AGENT CLONES — an agent otherwise sees only its own
	// ticket and has no way to learn what the other six are building, or what the
	// thing as a whole is meant to be. Writing the docs after the code, or as a
	// ticket alongside it, gives that context to nobody: by then every agent that
	// needed it has already finished.
	//
	// It also removes documentation from the pipeline, which it was never able to
	// survive. A "write the README" ticket reaching the spec author is
	// unsatisfiable by construction — that stage may write only *_test.go, and the
	// ticket asks for a README — so the agent cannot reach its finish condition by
	// any permitted action. Observed: one such ticket burned all 100 iterations
	// rewriting README.md in a loop, then blocked, having held a large-class slot
	// the whole time.
	ColDesigning       = "designing"
	ColReadyForScoping = "ready_for_scoping"
	// ColScoping is the product manager holding a request while it breaks it down.
	ColScoping = "scoping"
	// ColReadyForSpec and ColWritingSpec belong to a SUB-TASK: a slice of one
	// task, existing only so that slice of the specification actually gets
	// written. It ends at done and is never developed — its task is.
	ColReadyForSpec = "ready_for_spec"
	ColWritingSpec  = "writing_spec"
	// ColReadyForSpecMerge and ColMergingSpecs belong to the TASK, between its
	// sections finishing and its developer starting: several authors wrote onto
	// one branch without seeing each other, and what they produced has to compile
	// as one package before anyone is asked to satisfy it.
	ColReadyForSpecMerge = "ready_for_spec_merge"
	ColMergingSpecs      = "merging_specs"

	// ColReadyForTicketMerge / ColMergingTickets are where the product manager's
	// tasks wait to be folded into one.
	//
	// THE TICKETS ARE CREATED FIRST AND MERGED SECOND, deliberately. Collapsing the
	// plan before anything is written produces one generic ticket and loses what
	// the board is for: what was planned, what is done, and what each piece cost.
	// So every unit of work the product manager found becomes a real ticket, and a
	// stage then merges them — leaving the originals on the board, marked with
	// what they were merged into.
	ColReadyForTicketMerge = "ready_for_ticket_merge"
	ColMergingTickets      = "merging_tickets"
	// ColTracking is a request that HAS been broken down. The work now lives in
	// its children; the parent stays visible so the original request can be
	// followed to the tickets that answer it.
	ColTracking = "tracking"

	ColReadyForDev = "ready_for_dev"
	ColInDev       = "in_dev"

	// ColReadyForMaintenance / ColMaintaining are the idle-time queue: work the
	// pipeline gives itself when no one has asked for anything.
	//
	// IT IS A SEPARATE COLUMN SO THE GATE CAN DIFFER. A maintenance ticket is
	// judged by the linter AND the existing tests together (see withMaintenance),
	// which is a different bar from a feature ticket's, and a dispatcher is the
	// only place that bar can be set. Sharing ColReadyForDev would mean one
	// dispatcher and therefore one gate.
	ColReadyForMaintenance = "ready_for_maintenance"
	ColMaintaining         = "maintaining"

	// ColReadyForTests / ColWritingTests are the test author's queue and hold.
	//
	// THEY COME BEFORE DEVELOPMENT, and the order is the whole point. An agent
	// that writes both the change and the test that checks it will write the test
	// its change already passes, with no way to notice it has done so — the test
	// then certifies the implementation against itself. Running the author AFTER
	// the developer does not fix that, it only moves it: an author that can read
	// the implementation describes what the code does rather than what the ticket
	// asked for. Only an author that has never seen the code can write a test that
	// is a specification instead of a description.
	//
	// Observed directly, and it is what prompted this: a run merged 168 lines of
	// implementation whose test gate reported "no test files". The gate was green
	// because there was nothing to run.
	//
	// The consequence is that a branch is RED between these columns and a finished
	// developer, which is what test-driven development is. The developer's gate
	// finally means something: green is "satisfies the tests", not "nothing ran".
	//
	// The split is enforced by what each stage may WRITE, not by asking nicely:
	// the developer cannot write *_test.go and the author can write nothing else.
	// Neither can move the other's half to make its own pass.
	ColReadyForTests = "ready_for_tests"
	ColWritingTests  = "writing_tests"

	// ColReadyForCoverage / ColCovering are the coverage author's queue and hold.
	//
	// A SECOND TEST STAGE, AFTER the code, and the two are not redundant. The
	// first author writes from the TICKET and has never seen an implementation —
	// that is what makes its tests a specification. It therefore cannot know which
	// branches the implementation actually grew: a nil map, an early return, an
	// error path the ticket never mentioned. Those are visible only once the code
	// exists, and they are where the bugs are.
	//
	// So the spec comes first and binds the developer, and the coverage pass comes
	// after and fills in what the code turned out to contain. Running only the
	// second would give tests that describe the implementation; running only the
	// first leaves its edge cases untested.
	ColReadyForCoverage = "ready_for_coverage"
	ColCovering         = "covering"

	ColReadyForReview = "ready_for_review"
	ColInReview       = "in_review"

	ColReadyForIntegration = "ready_for_integration"
	ColIntegrating         = "integrating"

	// ColConflicted is the resolver's queue: a branch that is finished and
	// reviewed but does not merge. Separate from ColBlocked because it is
	// actionable by an agent, where blocked means a person is needed — and one
	// column for both would make the resolver either take work it cannot do or
	// leave work it can.
	ColConflicted = "conflicted"
	// ColBlocked is where work goes when the department is out of options: a
	// stage that failed its attempt ceiling, or a conflict no agent could settle.
	// It is the only column that means "a person is needed", which is what makes
	// it worth watching.
	ColBlocked = "blocked"
	// ColDone is merged into the integration branch.
	ColDone = "done"
)

// Column is one board column, as the tickets service's status field defs model
// them.
type Column struct {
	Value string
	Label string
	Color string
}

// workflowColumns is the board this department works, in order. Position is the
// index, so reordering this slice reorders the board.
var workflowColumns = []Column{
	{ColInbox, "Inbox", "#6b7280"},
	{ColDesigning, "Designing", "#9d7bb8"},
	{ColReadyForScoping, "Ready for scoping", "#9483bd"},
	{ColScoping, "Scoping", "#8b7bb8"},
	{ColReadyForTicketMerge, "Ready for ticket merge", "#5b6b8c"},
	{ColMergingTickets, "Merging tickets", "#5b6b8c"},
	{ColTracking, "Tracking", "#5b6b8c"},
	{ColReadyForTests, "Ready for tests", "#7a9bc4"},
	{ColWritingTests, "Writing tests", "#6289b8"},
	{ColReadyForDev, "Ready for dev", "#4a90d9"},
	{ColInDev, "In dev", "#3b7dc4"},
	{ColReadyForMaintenance, "Ready for upkeep", "#6f7f8f"},
	{ColMaintaining, "Upkeep", "#5f6f7f"},
	{ColReadyForCoverage, "Ready for coverage", "#8fb08a"},
	{ColCovering, "Covering", "#7aa375"},
	{ColReadyForReview, "Ready for review", "#c9b060"},
	{ColInReview, "In review", "#b89f4a"},
	{ColReadyForIntegration, "Ready to integrate", "#5fa88a"},
	{ColIntegrating, "Integrating", "#4d9276"},
	{ColConflicted, "Conflicted", "#d4874f"},
	{ColReadyForSpec, "Ready for spec", "#7fa8c9"},
	{ColWritingSpec, "Writing spec", "#6f9dc2"},
	{ColReadyForSpecMerge, "Ready for spec merge", "#5f92bb"},
	{ColMergingSpecs, "Merging specs", "#4f87b4"},
	{ColBlocked, "Blocked", "#d46b55"},
	{ColDone, "Done", "#4a9d5f"},
}

// Stage binds one role to the columns it moves a ticket through.
//
// A stage is a FUNCTION FROM COLUMN TO COLUMN, and writing it down as data
// rather than as conditionals inside each agent is what makes the pipeline
// legible: the whole routing table is this one variable, and an agent that
// forgets to move a ticket is a missing field rather than a ticket that silently
// stops.
type Stage struct {
	Role string
	// Ready is the column this stage takes from — the queue.
	Ready string
	// Working is where the ticket sits while the stage holds it. It is what makes
	// work-in-progress visible, and it is also the CLAIM: moving a ticket out of
	// the Ready column with a compare-and-set is how two hosts racing for the
	// same work are resolved, since only one of them can win the version check.
	Working string
	// Success and Exhausted are where the ticket goes when the stage finishes and
	// when it has failed too many times.
	Success   string
	Exhausted string
	// Returns is where this stage sends work BACKWARDS: the queue of an earlier
	// stage that has to look at the ticket again. It is the only edge in this
	// table that does not move a ticket forward, and both users of it are asking
	// the same question — "this is not mine to finish, hand it back":
	//
	//   - the reviewer rejecting a change, which returns it to the developer;
	//   - the resolver finding a conflict has evaporated, which returns it to the
	//     integrator to merge normally.
	//
	// Empty means the stage cannot return work, and an OutcomeReturned from a
	// stage with no Returns column is a routing bug rather than a silent no-op —
	// see destination.
	Returns string
	// RequiresDependencies makes the stage wait until every ticket in depends_on
	// has reached ColDone. Set on the stages that START work: there is no point
	// writing code against a prerequisite that does not exist yet, and a reviewer
	// looking at that branch would be reviewing the wrong thing.
	RequiresDependencies bool
}

// stages is the routing table. One entry per role.
//
// Read it as a pipeline: inbox → scoping → ready_for_dev → in_dev →
// ready_for_review → in_review → ready_for_integration → integrating → done,
// with conflicted branching off integration for the resolver and blocked as the
// terminus for anything the department cannot finish itself.
//
// NOTE the scoping stage's Success: a request that has been broken down goes to
// tracking, NOT to ready_for_dev. The parent is not work; its children are. A
// stage that pushed the parent forward would put a dev agent to work on "add
// rate limiting", the whole request, rather than on the pieces it just defined.
// applyCoverageSetting rewires the developer's hand-off for a host that is not
// running the coverage stage.
//
// The routing table is otherwise static, and this is the one thing that changes
// it — done ONCE at startup, before any dispatcher reads it, because a table
// that changed under a running pipeline would move tickets to a column nothing
// was polling. The alternative was a permanent extra column that some hosts
// simply never used, which reads on a board as a stage that is silently broken.
func applyCoverageSetting(enabled bool) {
	dev := stages[roleDev]
	if enabled {
		dev.Success = ColReadyForCoverage
	} else {
		dev.Success = ColReadyForReview
	}
	stages[roleDev] = dev
}

// applyArchitectSetting moves the product manager's queue to sit BEHIND the
// architect when one is running, and back onto the inbox when there is not.
//
// The same startup-only rule as applyCoverageSetting, for the same reason, and
// one more that is specific to this stage: the architect needs a repository to
// commit into, so a triage-only host cannot run one. If the PM's queue were
// hard-wired to ready_for_scoping, such a host would leave every request sitting
// in inbox with nothing coming to collect it — a department that looks up and
// does nothing, which is the worst way for this to fail.
// It ADDS AND REMOVES the architect's entry rather than leaving it in place.
// The routing table is read as "the stage that works this column" — by the
// window, and by anything asking what happens to a ticket next. An inert entry
// still claiming the inbox gives that question two answers, and since the table
// is a map, which one you get depends on iteration order: a test asserting the
// inbox belongs to the product manager passed or failed by luck. A stage that is
// not running should not be in the table at all.
func applyArchitectSetting(enabled bool) {
	pm := stages[roleScoping]
	if enabled {
		stages[roleArchitect] = architectStage
		pm.Ready = ColReadyForScoping
	} else {
		delete(stages, roleArchitect)
		pm.Ready = ColInbox
	}
	stages[roleScoping] = pm
}

// architectStage is held apart from the table so applyArchitectSetting can put
// it back. Exhaustion moves the request FORWARD to scoping rather than blocking
// it: documentation is context, not a gate, so a request that could not be
// designed is still a request worth breaking down. Blocking here would make a
// weak or unreachable architect fatal to work that never needed one.
var architectStage = Stage{
	Role: roleArchitect, Ready: ColInbox, Working: ColDesigning,
	Success: ColReadyForScoping, Exhausted: ColReadyForScoping,
}

// stages describes a host running NO architect — the simplest pipeline, and the
// one every reader should meet first. applyArchitectSetting inserts the design
// stage and moves the product manager behind it; see architectStage.
var stages = map[string]Stage{
	roleScoping: {
		Role: roleScoping, Ready: ColInbox, Working: ColScoping,
		Success: ColTracking, Exhausted: ColBlocked,
	},
	// The test author goes FIRST and hands the developer an executable spec. It
	// needs no Returns column: there is no stage before it to hand work back to,
	// and a ticket it cannot write tests for is one a person should read.
	//
	// IT WAITS FOR DEPENDENCIES, and this is a reversal with a cost attached.
	//
	// The argument for not waiting was that tests are written from the TICKET
	// rather than from any code, so an unfinished prerequisite tells the author
	// nothing — and waiting cost the pipeline its only parallel stage, with every
	// sibling of a slow foundation sitting idle when all of them could have been
	// specified at once. That is still true and still the price.
	//
	// What it missed is that a specification NAMES THINGS. A ticket for the API
	// writes tests against Task and Store, which the foundation ticket creates. An
	// author that cannot see them invents their shape, and the developer — which
	// does wait, and does merge — then has to satisfy an invented interface
	// against the real one already on the integration branch. That is not a
	// failing test it can fix; it is a contradiction, and it surfaced as
	// "cannot use ... as ..." errors that no implementation resolves.
	//
	// Waiting alone is not enough either: the branch is cut at scoping time, so
	// the author must also MERGE what it waited for. See mergeIntegrationScript.
	roleTest: {
		Role: roleTest, Ready: ColReadyForTests, Working: ColWritingTests,
		Success: ColReadyForDev, Exhausted: ColBlocked,
		RequiresDependencies: true,
	},
	// THE SUB-TASK STAGE. Same agent as roleTest, different columns and a
	// different ending: its success is DONE, because a sub-task is a slice of a
	// specification rather than a unit of work. The tests it writes land on its
	// TASK's branch, so the task arrives at development with every slice of its
	// specification already assembled.
	//
	// Why this exists at all: a plan written into a ticket description is a
	// suggestion. Measured — a request planned as twelve sections produced ONE
	// test file, because the spec stage is satisfied by any file that parses,
	// fails and has no stubs. A ticket is an obligation; prose is not.
	roleSpec: {
		Role: roleSpec, Ready: ColReadyForSpec, Working: ColWritingSpec,
		// A SECTION ENDS WHEN ITS SPECIFICATION IS WRITTEN. It is a slice of a
		// task's tests, not a unit of implementation, so nothing develops it on
		// its own — its TASK is developed once, against every slice assembled.
		//
		// This reverses a change made for a weaker model, and the reversal is
		// worth the detail. Per-section developers were introduced because the
		// assembled task was the size that model could not manage: section SPECS
		// took 3, 5 and 11 turns while the assembled task took 10 once and failed
		// at 49 and 80 twice.
		//
		// That ceiling moved. Measured on the current model against the same shape
		// of work — an in-memory store, a JSON API and an HTML page, 37 tests —
		// the whole thing was built in five turns, which is the assembled size the
		// old model could not finish at all.
		//
		// What the split BOUGHT is kept: each slice still gets its own author,
		// with only its own criteria in front of it, so the specification stays as
		// accurate as it was. What it COST is dropped — a branch, a developer, a
		// merge and a place in the dependency queue for every slice, which is most
		// of a run's wall time and every one of its merge conflicts.
		Success: ColDone, Exhausted: ColBlocked,
		RequiresDependencies: true,
	},
	// THE RECONCILER. Sections are written concurrently by authors that cannot see
	// each other, in one Go package, so two of them naming a test the same way is
	// not a rare accident — it happened on the first run that assembled specs,
	// and the DEVELOPER inherited a branch it had no legal move to fix, since it
	// may not edit tests.
	//
	// This stage may edit tests, and only tests. Its job is to make the assembled
	// sections compile as one package — rename a duplicate, fold two identical
	// helpers into one — WITHOUT weakening an assertion. It waits for the
	// sections, and hands the task to development.
	// ONE TASK OUT OF MANY, mechanically. This stage calls no model: merging is
	// concatenation, and a model asked to concatenate can paraphrase, reorder or
	// drop — which is the one failure the coverage rule upstream exists to stop.
	//
	// It exists because the pipeline is strictly serial. Tasks are chained and the
	// model serves one request at a time whatever the slot count says, so every
	// split buys parallelism nothing can deliver while costing a spec-merge, a
	// security pass, an integrator run and a developer's startup. Measured on r85
	// at the section level: 3.6 minutes per task down to 2.3, on a bigger board.
	roleTicketMerge: {
		Role: roleTicketMerge, Ready: ColReadyForTicketMerge, Working: ColMergingTickets,
		// Out to the ordinary task entry, so everything downstream is unchanged: the
		// merged ticket is a task like any other, with one section under it.
		Success: ColReadyForSpecMerge, Exhausted: ColBlocked,
	},
	roleSpecMerge: {
		Role: roleSpecMerge, Ready: ColReadyForSpecMerge, Working: ColMergingSpecs,
		// AND THEN THE TASK IS DEVELOPED. This stage assembles the slices its
		// sections wrote and hands the task to one developer, which is the only
		// place a developer now runs for this branch.
		//
		// It is also the last point at which the assembled specification is
		// checked to compile as one package — several authors wrote onto one
		// branch without seeing each other, and a duplicate helper or a clashing
		// test name is a defect the developer inherits with no legal move, since
		// it may not edit tests.
		Success: ColReadyForDev, Exhausted: ColBlocked,
		RequiresDependencies: true,
	},
	roleDev: {
		Role: roleDev, Ready: ColReadyForDev, Working: ColInDev,
		Success: ColReadyForCoverage, Exhausted: ColBlocked,
		// Returns sends a TASK back to the reconciler, which is the stage that may
		// edit this task's tests. The developer may not edit test files, so a
		// specification it cannot satisfy is the one failure it can neither fix
		// nor route around; without this column it had nowhere to send the work
		// and stopped the board instead.
		//
		// NOT ready_for_spec, now that a developer works on tasks rather than
		// sections. A task in the section author's queue is the failure this
		// repository has already paid for once: a blocked task revived into
		// ready_for_spec was claimed by the section author, read a hundred times
		// and killed. A task belongs to the stage that assembles it.
		Returns:              ColReadyForSpecMerge,
		RequiresDependencies: true,
	},
	// The coverage author returns to the DEVELOPER, not to the spec author: a
	// branch it cannot cover is usually one the code makes unreachable, and that
	// is the developer's to answer.
	//
	// EXHAUSTION MOVES IT FORWARD, which is the one stage where that is right.
	// Coverage is a quality goal, not a correctness gate: by the time work
	// reaches here it has satisfied the specification the developer was held to,
	// and blocking it for falling short of a percentage would blackhole finished,
	// working code over a target that is itself only a proxy. The shortfall is
	// recorded on the ticket and the reviewer — and the person reading the board —
	// sees the figure it reached.
	//
	// It also decides what a weak model in this stage costs. Blocking would make
	// one that cannot write tests actively harmful; passing through makes it
	// merely unhelpful, which is what allows a smaller model to be tried here at
	// all.
	roleCoverage: {
		Role: roleCoverage, Ready: ColReadyForCoverage, Working: ColCovering,
		Success: ColReadyForReview, Exhausted: ColReadyForReview,
		Returns: ColReadyForDev,
	},
	roleReview: {
		Role: roleReview, Ready: ColReadyForReview, Working: ColInReview,
		Success: ColReadyForIntegration, Exhausted: ColBlocked,
		Returns: ColReadyForDev,
	},
	roleIntegrate: {
		Role: roleIntegrate, Ready: ColReadyForIntegration, Working: ColIntegrating,
		Success: ColDone, Exhausted: ColBlocked,
	},
	roleMaintain: {
		// STRAIGHT TO INTEGRATION, past the reviewer and the coverage stage.
		//
		// There is no specification author either, and that is the whole shape of
		// this stage rather than an omission. A maintenance ticket's acceptance
		// criterion is that the linter stops reporting the thing, and its
		// regression net is the test suite already in the repository — both are
		// already run by the gate, so an author writing a failing test first would
		// be writing a test for a bar the gate already enforces.
		//
		// The reviewer is skipped because it reviews a diff for security, and its
		// verdict is advisory anyway; the coverage stage is skipped because
		// maintenance adds no behaviour to cover.
		Role: roleMaintain, Ready: ColReadyForMaintenance, Working: ColMaintaining,
		Success: ColReadyForIntegration, Exhausted: ColBlocked,
	},
	roleResolve: {
		Role: roleResolve, Ready: ColConflicted, Working: ColIntegrating,
		Success: ColDone, Exhausted: ColBlocked,
		Returns: ColReadyForIntegration,
	},
}

// The role names. Constants because they are the key into the routing table,
// they name the traced service, and they appear in the claim record — three
// places a typo would be silent in a different way.
const (
	roleArchitect   = "architect-agent"
	roleScoping     = "pm-agent"
	roleDev         = "dev-agent"
	roleTest        = "test-agent"
	roleSpec        = "spec-agent"
	roleSpecMerge   = "spec-merge-agent"
	roleTicketMerge = "ticket-merge-agent"
	roleCoverage    = "coverage-agent"
	roleReview      = "sec-agent"
	roleMaintain    = "maintenance-agent"
	roleIntegrate   = "integrator"
	roleResolve     = "resolver"
)

// stageFor returns the routing entry for a role.
func stageFor(role string) (Stage, bool) {
	s, ok := stages[role]
	return s, ok
}

// dependenciesMet reports whether every ticket this one waits for has reached
// ColDone.
//
// The tickets service deliberately returns each blocker's raw STATUS rather than
// a "satisfied" flag, because boards define their own columns and only the
// consumer knows which of its own means finished. This is that decision, made
// here, once.
//
// A blocker the caller cannot see comes back with an empty status, and is
// treated as NOT met. That is the safe direction: working a ticket whose
// prerequisite might be unfinished produces a branch built on something that
// does not exist, while waiting merely means a person has to look.
func dependenciesMet(t Ticket) bool {
	for _, d := range t.DependsOn {
		if d.Status != ColDone {
			return false
		}
	}
	return true
}

// dependencyDeadlocked reports whether this ticket waits on a prerequisite that
// can never finish, and names the first such blocker.
//
// dependenciesMet requires every blocker to reach ColDone, and ColBlocked is
// terminal WITHOUT being done — so a ticket behind a blocked one is not waiting,
// it is stranded. Observed on a real batch: a chain of six tickets lost its
// second, and the remaining four sat in ready_for_dev indefinitely, looking
// queued and consuming nothing. Nothing swept them and nothing said why, which
// is the worst shape a stall can take — a queue that is not moving reads exactly
// like a queue that is busy.
//
// Only ColBlocked counts. ColConflicted is deliberately excluded: that is the
// resolver's queue, so a conflicted blocker is still on its way to done.
// It returns the DEPENDENCY, not just a label. An earlier version returned the
// blocker's title, which reads well in a comment and is useless to a caller that
// needs to act on the blocker — the revival path passed that title to GetTicket
// as an id, failed on every ticket, and logged a warning nobody read while the
// board sat deadlocked.
func dependencyDeadlocked(t Ticket) (TicketDependency, bool) {
	for _, d := range t.DependsOn {
		if d.Status == ColBlocked {
			return d, true
		}
	}
	return TicketDependency{}, false
}

// blockerName is how a dependency is named to a person.
func blockerName(d TicketDependency) string {
	if d.Title != "" {
		return d.Title
	}
	return d.TicketID
}

// blockedBy lists the unfinished prerequisites, for a message a person can act
// on. Empty when the ticket is ready.
func blockedBy(t Ticket) []TicketDependency {
	var out []TicketDependency
	for _, d := range t.DependsOn {
		if d.Status != ColDone {
			out = append(out, d)
		}
	}
	return out
}
