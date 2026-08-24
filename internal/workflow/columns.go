// Package workflow is the routing table: which column each stage takes from,
// where it holds work, and where that work goes next.
//
// THE BOARD IS THE WORKFLOW.
//
// Stages used to be selected by a MARKER left in a ticket comment — a triage
// comment meant triaged, a "pushed a branch" comment meant ready to review. That
// worked, and it was wrong in three ways that cost real time:
//
//   - the state was invisible. A person saw a column of identical open tickets
//     and had to read comments to learn four were waiting on review and one had
//     been stuck for a day;
//   - selection needed the comments, and a LISTING does not carry them, so every
//     candidate cost a full fetch every poll — and when that hydration was
//     missed, predicates silently inverted: a stage requiring a marker selected
//     nothing forever while a stage requiring its absence re-claimed everything;
//   - the markers were prose, so changing the wording of a comment changed the
//     routing of the pipeline.
//
// Now the COLUMN is the stage. The platform models board columns as per-board
// status field defs, so these are real columns a person can drag between,
// filtering happens server-side, and a listing already carries everything
// selection needs. Markers still record WHAT a stage did — they simply stop
// being how work is routed.
//
// See docs/pipeline.md for why the stages sit in the order they do.
package workflow

// The columns. A column names WHICH STAGE OWNS THE WORK, which is a different
// question from the platform's own open/closed status — see internal/ticket.
const (
	// ColInbox is where a request lands. Nothing else writes here, which is what
	// makes intake safe: a request is a ticket like any other, and the only stage
	// taking from it is the first of the pipeline.
	ColInbox = "inbox"

	// ColDesigning holds a request while the architect writes the project
	// documentation; ColReadyForScoping is where it hands the request on.
	//
	// THE ARCHITECT RUNS BEFORE THE BREAKDOWN, and the order carries the whole
	// value. Its output is committed to the base branch, so it is in the tree
	// EVERY LATER AGENT CLONES — an agent otherwise sees only its own ticket and
	// has no way to learn what the other six are building. Writing the docs after
	// the code gives that context to nobody: by then everyone who needed it has
	// finished.
	//
	// It also takes documentation OUT of the pipeline, which it was never able to
	// survive: a "write the README" ticket reaching the specification author is
	// unsatisfiable by construction, since that stage may write only *_test.go.
	// Observed — one such ticket burned all 100 iterations rewriting README.md in
	// a loop, then blocked, holding a large-class slot throughout.
	ColDesigning       = "designing"
	ColReadyForScoping = "ready_for_scoping"

	// ColScoping holds a request while the product manager breaks it down.
	ColScoping = "scoping"

	// ColReadyForTicketMerge / ColMergingTickets are where planned tasks wait to
	// be folded into one.
	//
	// THE TICKETS ARE CREATED FIRST AND MERGED SECOND, deliberately. Collapsing
	// the plan before anything is written produces one generic ticket and loses
	// what the board is for: what was planned, what is done, what each piece cost.
	// So every unit of work becomes a real ticket and a stage then merges them,
	// leaving the originals on the board marked with what they were merged into.
	ColReadyForTicketMerge = "ready_for_ticket_merge"
	ColMergingTickets      = "merging_tickets"

	// ColTracking is a request that HAS been broken down. The work now lives in
	// its children; the parent stays visible so the original request can be
	// followed to the tickets that answer it.
	ColTracking = "tracking"

	// ColReadyForTests / ColWritingTests are the task-level author's queue and
	// hold.
	//
	// THEY COME BEFORE DEVELOPMENT, and the order is the whole point. An agent
	// that writes both the change and the test checking it will write the test its
	// change already passes, with no way to notice — the test then certifies the
	// implementation against itself. Running the author AFTER the developer does
	// not fix that, it moves it: an author that can read the implementation
	// describes what the code does rather than what the ticket asked for.
	//
	// Observed, and it is what prompted the split: a run merged 168 lines of
	// implementation whose test gate reported "no test files". The gate was green
	// because nothing ran.
	//
	// The consequence is that a branch is RED between these columns and a finished
	// developer, which is what test-first means. The developer's gate finally says
	// something: green is "satisfies the tests", not "nothing ran".
	ColReadyForTests = "ready_for_tests"
	ColWritingTests  = "writing_tests"

	// ColReadyForSpec / ColWritingSpec belong to a SUB-TASK: one slice of a task's
	// specification, existing only so that slice actually gets written. It ends at
	// done and is never developed — its task is.
	//
	// Why slices exist at all: a plan written into a ticket description is a
	// suggestion. Measured — a request planned as twelve sections produced ONE
	// test file, because the stage is satisfied by any file that parses, fails and
	// has no stubs. A ticket is an obligation; prose is not.
	ColReadyForSpec = "ready_for_spec"
	ColWritingSpec  = "writing_spec"

	// ColReadyForSpecMerge / ColMergingSpecs belong to the TASK, between its
	// sections finishing and its developer starting. Several authors wrote onto
	// one branch without seeing each other, and what they produced has to compile
	// as one package before anyone is asked to satisfy it.
	ColReadyForSpecMerge = "ready_for_spec_merge"
	ColMergingSpecs      = "merging_specs"

	ColReadyForDev = "ready_for_dev"
	ColInDev       = "in_dev"

	// ColReadyForMaintenance / ColMaintaining are the idle-time queue: work the
	// pipeline gives itself when nobody has asked for anything.
	//
	// A SEPARATE COLUMN SO THE GATE CAN DIFFER. Upkeep is judged by the linter AND
	// the existing tests together, which is a different bar from a feature
	// ticket's, and a dispatcher is the only place that bar can be set. Sharing
	// ColReadyForDev would mean one dispatcher and therefore one gate.
	ColReadyForMaintenance = "ready_for_maintenance"
	ColMaintaining         = "maintaining"

	// ColReadyForCoverage / ColCovering are the coverage author's queue and hold.
	//
	// A SECOND TEST STAGE, AFTER the code, and the two are not redundant. The
	// first author writes from the TICKET and has never seen an implementation —
	// that is what makes its tests a specification, and it is also why it cannot
	// know which branches the code actually grew: a nil map, an early return, an
	// error path the ticket never mentioned. Those are visible only once the code
	// exists, and they are where the bugs are.
	ColReadyForCoverage = "ready_for_coverage"
	ColCovering         = "covering"

	ColReadyForReview = "ready_for_review"
	ColInReview       = "in_review"

	ColReadyForIntegration = "ready_for_integration"
	ColIntegrating         = "integrating"

	// ColConflicted is the resolver's queue: a branch that is finished and
	// reviewed but does not merge. Separate from ColBlocked because it is
	// actionable BY AN AGENT, where blocked means a person is needed — one column
	// for both would make the resolver either take work it cannot do or leave work
	// it can.
	ColConflicted = "conflicted"

	// ColBlocked is where work goes when the department is out of options. It is
	// the only column meaning "a person is needed", which is what makes it worth
	// watching.
	ColBlocked = "blocked"

	// ColDone is merged into the integration branch.
	ColDone = "done"
)

// Column is one board column as the platform's status field defs model them.
type Column struct {
	Value string
	Label string
	Color string
}

// Columns is the board this department works, in order. Position is the index,
// so reordering this slice reorders the board.
//
// A COPY, because the caller creates a board from it, and a caller that sorted
// or truncated the package's own slice would reorder every later board on the
// host.
func Columns() []Column {
	out := make([]Column, len(columns))
	copy(out, columns)
	return out
}

var columns = []Column{
	{ColInbox, "Inbox", "#6b7280"},
	{ColDesigning, "Designing", "#9d7bb8"},
	{ColReadyForScoping, "Ready for scoping", "#9483bd"},
	{ColScoping, "Scoping", "#8b7bb8"},
	{ColReadyForTicketMerge, "Ready for ticket merge", "#5b6b8c"},
	{ColMergingTickets, "Merging tickets", "#5b6b8c"},
	{ColTracking, "Tracking", "#5b6b8c"},
	{ColReadyForTests, "Ready for tests", "#7a9bc4"},
	{ColWritingTests, "Writing tests", "#6289b8"},
	{ColReadyForSpec, "Ready for spec", "#7fa8c9"},
	{ColWritingSpec, "Writing spec", "#6f9dc2"},
	{ColReadyForSpecMerge, "Ready for spec merge", "#5f92bb"},
	{ColMergingSpecs, "Merging specs", "#4f87b4"},
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
	{ColBlocked, "Blocked", "#d46b55"},
	{ColDone, "Done", "#4a9d5f"},
}

// Terminal reports whether work has stopped in this column. Nothing takes from
// these two, and they are the pair a readiness check must tell apart: done is
// finished, blocked is finished WITHOUT being done.
func Terminal(column string) bool {
	return column == ColDone || column == ColBlocked
}
