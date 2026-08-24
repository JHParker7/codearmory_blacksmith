package workflow

import "sort"

// The role names. Constants because a role is the key into the routing table, it
// names the traced service, and it appears in the claim record — three places a
// typo would be silent in a different way.
const (
	RoleArchitect   = "architect-agent"
	RoleScoping     = "pm-agent"
	RoleTicketMerge = "ticket-merge-agent"
	RoleTest        = "test-agent"
	RoleSpec        = "spec-agent"
	RoleSpecMerge   = "spec-merge-agent"
	RoleDev         = "dev-agent"
	RoleMaintain    = "maintenance-agent"
	RoleCoverage    = "coverage-agent"
	RoleReview      = "sec-agent"
	RoleIntegrate   = "integrator"
	RoleResolve     = "resolver"
)

// Stage binds one role to the columns it moves a ticket through.
//
// A stage is a FUNCTION FROM COLUMN TO COLUMN, and writing that down as data
// rather than as conditionals inside each agent is what makes the pipeline
// legible: the whole routing table is one value, and an agent that forgets to
// move a ticket is a missing field rather than a ticket that silently stops.
type Stage struct {
	Role string

	// Ready is the column this stage takes from — its queue.
	Ready string

	// Working is where the ticket sits while the stage holds it.
	//
	// It makes work-in-progress visible, and it is also THE CLAIM: moving a
	// ticket out of Ready with a compare-and-set is how two hosts racing for the
	// same work are resolved, since only one can win the version check. See
	// docs/claiming.md.
	Working string

	// Success and Exhausted are where the ticket goes when the stage finishes and
	// when it has failed too many times.
	Success   string
	Exhausted string

	// Returns is where this stage sends work BACKWARDS: the queue of an earlier
	// stage that has to look at the ticket again. It is the only edge in this
	// table that does not move a ticket forward, and every user of it is asking
	// the same question — "this is not mine to finish, hand it back".
	//
	// Empty means the stage cannot return work, and an OutcomeReturned from such a
	// stage is a routing bug rather than a silent no-op. See Destination.
	Returns string

	// RequiresDependencies makes the stage wait until every ticket in depends_on
	// has reached ColDone.
	//
	// Set on the stages that START work: there is no point writing code against a
	// prerequisite that does not exist, and a reviewer looking at that branch is
	// reviewing the wrong thing.
	//
	// THE AUTHOR WAITS TOO, and that is a reversal with a price. Not waiting was
	// argued for because tests are written from the TICKET rather than from any
	// code, so an unfinished prerequisite tells the author nothing — and waiting
	// costs the pipeline its only parallel stage, with every sibling of a slow
	// foundation idle when all of them could have been specified at once. That is
	// still true and still the price.
	//
	// What it missed is that a specification NAMES THINGS. A ticket for the API
	// writes tests against the types the foundation ticket creates. An author that
	// cannot see them invents their shape, and the developer — which does wait,
	// and does merge — then has to satisfy an invented interface against the real
	// one already on the integration branch. That is not a failing test it can
	// fix; it is a contradiction, and it surfaced as "cannot use ... as ..."
	// errors no implementation resolves.
	//
	// Waiting alone is not enough: the branch is cut at scoping time, so a stage
	// that waits must also MERGE what it waited for.
	RequiresDependencies bool
}

// Options is the shape of the pipeline on this host. Two stages are optional,
// and each absence rewires the neighbour that would have fed it.
type Options struct {
	// Architect puts a documentation stage in front of the breakdown and moves
	// the product manager's queue behind it.
	//
	// It is optional because the architect needs a REPOSITORY to commit into, so
	// a triage-only host cannot run one. If the product manager's queue were
	// hard-wired behind it, such a host would leave every request sitting in the
	// inbox with nothing coming to collect it — a department that looks up and
	// does nothing, which is the worst way for this to fail.
	Architect bool

	// Coverage puts the second test author between development and review.
	Coverage bool
}

// Table is a routing table: one stage per role, fixed once it is built.
//
// A VALUE, NOT A PACKAGE-LEVEL MAP, and that is the one structural change from
// the shape this replaces. The old table was a global that startup mutated to
// apply these options, which had two failure modes worth naming: a table changed
// under a running pipeline moves tickets into a column nothing is polling, and a
// disabled stage left inert in the map gives "which stage works this column" two
// answers — decided by map iteration order, so a test asserting the inbox
// belongs to the product manager passed or failed by luck.
//
// Building the table instead means a disabled stage is ABSENT rather than inert,
// nothing can rewire it later, and two configurations can be examined side by
// side in one test.
type Table struct {
	stages map[string]Stage
}

// New builds the routing table for a host running these options.
func New(opts Options) Table {
	st := map[string]Stage{
		// A request that has been broken down goes to TRACKING, not onward: the
		// parent is not work, its children are. A stage that pushed the parent
		// forward would put a developer to work on "add rate limiting" — the whole
		// request — rather than on the pieces just defined.
		RoleScoping: {
			Role: RoleScoping, Ready: ColInbox, Working: ColScoping,
			Success: ColTracking, Exhausted: ColBlocked,
		},

		// ONE TASK OUT OF MANY, mechanically. This stage calls no model: merging
		// tickets is concatenation, and a model asked to concatenate can
		// paraphrase, reorder or drop.
		//
		// It exists because the pipeline is strictly serial. Tasks are chained and
		// the model serves one request at a time whatever the slot count says, so
		// every split buys parallelism nothing can deliver while costing a spec
		// merge, a security pass, an integrator run and a developer's startup.
		// Measured at the section level: 3.6 minutes per task down to 2.3, on a
		// bigger board.
		//
		// Out to the ordinary task entry, so everything downstream is unchanged:
		// the merged ticket is a task like any other, with one section under it.
		RoleTicketMerge: {
			Role: RoleTicketMerge, Ready: ColReadyForTicketMerge, Working: ColMergingTickets,
			Success: ColReadyForSpecMerge, Exhausted: ColBlocked,
		},

		// THE TASK-LEVEL AUTHOR. It takes a request that was one unit of work and
		// hands the developer an executable specification.
		//
		// No Returns column: there is no stage before it to hand work back to, and
		// a ticket it cannot write tests for is one a person should read.
		RoleTest: {
			Role: RoleTest, Ready: ColReadyForTests, Working: ColWritingTests,
			Success: ColReadyForDev, Exhausted: ColBlocked,
			RequiresDependencies: true,
		},

		// THE SECTION AUTHOR. Same agent as RoleTest, different columns and a
		// different ending: its success is DONE, because a section is a slice of a
		// specification rather than a unit of work. The tests it writes land on its
		// TASK's branch, so the task arrives at development with every slice of its
		// specification already assembled.
		//
		// This reverses a split made for a weaker model, and the reversal is worth
		// the detail. Per-section DEVELOPERS were introduced because the assembled
		// task was the size that model could not manage: section specs took 3, 5
		// and 11 turns while the assembled task took 10 once and then failed at 49
		// and 80. That ceiling moved — measured on the current model against the
		// same shape of work, an in-memory store, a JSON API and an HTML page with
		// 37 tests, the whole thing was built in five turns.
		//
		// What the split BOUGHT is kept: each slice still gets its own author, with
		// only its own criteria in front of it, so the specification stays as
		// accurate as it was. What it COST is dropped — a branch, a developer, a
		// merge and a place in the dependency queue per slice, which is most of a
		// run's wall time and every one of its merge conflicts.
		RoleSpec: {
			Role: RoleSpec, Ready: ColReadyForSpec, Working: ColWritingSpec,
			Success: ColDone, Exhausted: ColBlocked,
			RequiresDependencies: true,
		},

		// THE RECONCILER. Sections are written concurrently by authors that cannot
		// see each other, into one Go package, so two of them naming a test the
		// same way is not a rare accident — it happened on the first run that
		// assembled specs, and the DEVELOPER inherited a branch it had no legal
		// move to fix, since it may not edit tests.
		//
		// This stage may edit tests, and ONLY tests. Its job is to make the
		// assembled sections compile as one package — rename a duplicate, fold two
		// identical helpers into one — WITHOUT weakening an assertion. Then it
		// hands the task to one developer, which is the only place a developer runs
		// for this branch.
		RoleSpecMerge: {
			Role: RoleSpecMerge, Ready: ColReadyForSpecMerge, Working: ColMergingSpecs,
			Success: ColReadyForDev, Exhausted: ColBlocked,
			RequiresDependencies: true,
		},

		RoleDev: {
			Role: RoleDev, Ready: ColReadyForDev, Working: ColInDev,
			Success: ColReadyForReview, Exhausted: ColBlocked,
			// BACK TO THE RECONCILER, the stage that may edit this task's tests. The
			// developer may not, so a specification it cannot satisfy is the one
			// failure it can neither fix nor route around; without this column it had
			// nowhere to send the work and stopped the board instead.
			//
			// NOT ready_for_spec, now that a developer works on tasks rather than
			// sections. A task in the SECTION author's queue is a failure already
			// paid for once: a blocked task revived into ready_for_spec was claimed
			// by the section author, read a hundred times and killed. A task belongs
			// to the stage that assembles it.
			Returns:              ColReadyForSpecMerge,
			RequiresDependencies: true,
		},

		// STRAIGHT TO INTEGRATION, past the reviewer and the coverage stage.
		//
		// There is no specification author either, and that is the shape of this
		// stage rather than an omission. An upkeep ticket's acceptance criterion is
		// that the linter stops reporting the thing, and its regression net is the
		// suite already in the repository — the gate runs both, so an author
		// writing a failing test first would be restating a bar already enforced.
		//
		// The reviewer is skipped because it reviews a diff for security and its
		// verdict is advisory anyway; coverage is skipped because upkeep adds no
		// behaviour to cover.
		RoleMaintain: {
			Role: RoleMaintain, Ready: ColReadyForMaintenance, Working: ColMaintaining,
			Success: ColReadyForIntegration, Exhausted: ColBlocked,
		},

		RoleReview: {
			Role: RoleReview, Ready: ColReadyForReview, Working: ColInReview,
			Success: ColReadyForIntegration, Exhausted: ColBlocked,
			Returns: ColReadyForDev,
		},

		RoleIntegrate: {
			Role: RoleIntegrate, Ready: ColReadyForIntegration, Working: ColIntegrating,
			Success: ColDone, Exhausted: ColBlocked,
		},

		// The resolver takes a merge two changes genuinely disagree about. Its
		// Returns column is for the opposite discovery: a conflict that has
		// evaporated goes back to the integrator to be merged normally.
		RoleResolve: {
			Role: RoleResolve, Ready: ColConflicted, Working: ColIntegrating,
			Success: ColDone, Exhausted: ColBlocked,
			Returns: ColReadyForIntegration,
		},
	}

	if opts.Architect {
		// EXHAUSTION MOVES THE REQUEST FORWARD to scoping rather than blocking it:
		// documentation is context, not a gate, so a request that could not be
		// designed is still a request worth breaking down. Blocking here would make
		// a weak or unreachable architect fatal to work that never needed one.
		st[RoleArchitect] = Stage{
			Role: RoleArchitect, Ready: ColInbox, Working: ColDesigning,
			Success: ColReadyForScoping, Exhausted: ColReadyForScoping,
		}
		pm := st[RoleScoping]
		pm.Ready = ColReadyForScoping
		st[RoleScoping] = pm
	}

	if opts.Coverage {
		// The coverage author returns to the DEVELOPER, not to the specification
		// author: a branch it cannot cover is usually one the code makes
		// unreachable, and that is the developer's to answer.
		//
		// EXHAUSTION MOVES IT FORWARD, the one stage where that is right. Coverage
		// is a quality goal, not a correctness gate: by the time work arrives here
		// it has satisfied the specification the developer was held to, and blocking
		// it for falling short of a percentage would blackhole finished, working
		// code over a target that is itself only a proxy. The shortfall is recorded
		// on the ticket, where the reviewer and a person reading the board see it.
		//
		// It also decides what a weak model in this stage costs. Blocking would make
		// one that cannot write tests actively harmful; passing through makes it
		// merely unhelpful, which is what allows a smaller model to be tried here.
		st[RoleCoverage] = Stage{
			Role: RoleCoverage, Ready: ColReadyForCoverage, Working: ColCovering,
			Success: ColReadyForReview, Exhausted: ColReadyForReview,
			Returns: ColReadyForDev,
		}
		dev := st[RoleDev]
		dev.Success = ColReadyForCoverage
		st[RoleDev] = dev
	}

	return Table{stages: st}
}

// For returns the stage a role runs, and whether this host runs it at all.
//
// A MISSING ROLE IS A WIRING QUESTION, not a runtime one: either the option that
// enables the stage is off, or a dispatcher was built for something the table
// does not route. Callers wiring stages at startup should treat it as fatal
// there, where the message can name the role, rather than discovering it on the
// first poll.
func (tb Table) For(role string) (Stage, bool) {
	s, ok := tb.stages[role]
	return s, ok
}

// Roles returns every role this table routes, in BOARD ORDER — the order the
// work flows in, rather than the map's, so anything that lists stages lists them
// the same way twice running.
func (tb Table) Roles() []string {
	out := make([]string, 0, len(tb.stages))
	for role := range tb.stages {
		out = append(out, role)
	}
	sort.Slice(out, func(i, j int) bool {
		pi, pj := columnIndex(tb.stages[out[i]].Ready), columnIndex(tb.stages[out[j]].Ready)
		if pi != pj {
			return pi < pj
		}
		return out[i] < out[j]
	})
	return out
}

// IsWorking reports whether a column is one a stage parks HELD work in.
//
// DERIVED FROM THE TABLE rather than listed separately, so a stage added above
// cannot be forgotten here. What it answers is "is this ticket claimed": comments
// are append-only and nothing deletes them, so a ticket carries every claim ever
// made against it, and reading "claimed" off that history means a ticket looks
// claimed forever after its first stage — which locks each stage out of the work
// the one before it just handed over. The column says the same thing without the
// history, and says it to a person looking at the board as well.
func (tb Table) IsWorking(column string) bool {
	if column == "" {
		return false
	}
	for _, st := range tb.stages {
		if st.Working == column {
			return true
		}
	}
	return false
}

// Stages returns the routed stages, in the same order as Roles.
func (tb Table) Stages() []Stage {
	roles := tb.Roles()
	out := make([]Stage, 0, len(roles))
	for _, r := range roles {
		out = append(out, tb.stages[r])
	}
	return out
}

// columnIndex is a column's position on the board, and len(columns) for anything
// not on it — an unknown column sorts last rather than first, so a typo shows up
// at the end of a listing instead of quietly claiming to be the first stage.
func columnIndex(value string) int {
	for i, c := range columns {
		if c.Value == value {
			return i
		}
	}
	return len(columns)
}
