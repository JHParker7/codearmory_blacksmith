package dev

import "fmt"

// The bounds that end an attempt.
//
// THEY ARE NOT ONE BOUND WITH SEVERAL NAMES. Each answers a different question —
// how long, how stuck, how confused, how many times has this been tried — and
// the two occasions they were conflated both cost whole runs: a refusal ceiling
// that made proving a specification broken impossible, and a repeat ceiling that
// killed tickets before they had written a line.
const (
	// DefaultMaxIterations is the turn budget when nothing else is configured.
	//
	// FIFTY, NOT EIGHT. Eight was chosen when a wasted turn was expensive and the
	// loop had no guard against spending the whole allowance on one pathology.
	// Both have changed. Measured on the same ticket: eight finished nothing,
	// twenty-four finished three, and forty-eight finished four — the extra
	// tickets bought with wall-clock, which is the cheap resource for a
	// department that runs in the background.
	DefaultMaxIterations = 50

	// MaxTotalIterations is A DIAGNOSTIC CEILING, NOT A WORKING ALLOWANCE. Reads
	// refund their turn, so an agent that only reads does not spend the budget at
	// all, and something has to stop a runaway. A normal ticket finishes in
	// single figures.
	MaxTotalIterations = 200

	// MaxRepeatRefusals is how many times the agent may repeat a refused action
	// before the loop draws the conclusion the model will not.
	//
	// FOUR, NOT TWO. The bound was two when a repeated action cost a SANDBOX; it
	// no longer does — a refused action is caught before any execution and costs
	// one model call of about a second. So the two sides of the trade changed
	// places: being too generous now wastes seconds, while being too strict kills
	// a ticket outright. Two was observed doing exactly that — a model read the
	// files it needed, asked for them twice more, and was stopped at turn three of
	// eight without ever having written a line. The guard meant to stop waste
	// became the reason two tickets reached nobody.
	MaxRepeatRefusals = 4

	// MaxDeadRefusals ends an attempt that is refusing with nothing to show for
	// it. FIVE TIMES MaxRepeatRefusals: far enough past the low ceiling that an
	// agent recovering from a run of mistakes is never cut off, close enough that
	// a deadlock costs twenty turns rather than two hundred.
	MaxDeadRefusals = 20

	// MaxConsecutiveReads ends an attempt that has done nothing but read.
	//
	// Reads no longer spend the turn budget, so something else has to stop an
	// agent that only reads — and a RUN of consecutive reads is a far better
	// signal than a total count, because it names the actual failure. Measured on
	// one ticket: read, write, verify, then 87 consecutive reads through two
	// memory resets. An agent still gathering context after a hundred looks in a
	// row is not about to start writing.
	//
	// It counts EVERY read, not only the ones that returned nothing new: an agent
	// paging through the repository one file at a time is in the same place as one
	// asking for the same file, and neither has produced any work.
	MaxConsecutiveReads = 100

	// AgentResetTurns is how many turns an attempt runs before the agent's MEMORY
	// is cleared while its WORK is kept. See State.ResetAgent.
	AgentResetTurns = 40

	// MaxAgentResets bounds it, because a reset that has not helped four times
	// will not help a fifth. A starting figure to be measured, not a derived one.
	MaxAgentResets = 5

	// MinSpecBrokenTries is how many times the developer must CHANGE the
	// implementation and still get test-file-only compile errors before the
	// specification is judged unsatisfiable. One was far too few.
	MinSpecBrokenTries = 3

	// MaxTestEditRefusals is the SECOND route to the same conclusion, and it
	// exists because the first one could not be reached.
	//
	// MinSpecBrokenTries advances only when the developer CHANGES the
	// implementation and re-verifies. A developer facing a test file that does not
	// compile does not do that — it tries to fix the test file, which is refused,
	// and each refusal counts toward MaxDeadRefusals instead. Measured on r57: 63
	// test-file refusals against a spec whose only fault was `declared and not
	// used: tasks`, the attempt killed at 20 refusals, FOUR TIMES OVER, and the
	// specification never once judged broken. THE TWO BOUNDS WERE IN DIRECT
	// CONFLICT — the proof required work the refusal ceiling forbade.
	//
	// So wanting to edit the test file, repeatedly, WHILE that file is the thing
	// failing to compile, is itself the evidence. Nothing else explains it.
	MaxTestEditRefusals = 3

	// MaxSpecRepairs bounds how many times a section may be handed back to its
	// author. Beyond it the ticket stops for a person, because an author that
	// cannot fix its own tests twice will not fix them on the third pass.
	MaxSpecRepairs = 2
)

// NoProgress records an action that cannot have changed anything, and is THE
// ONLY WAY the refusal count goes up.
//
// Routing every such case through one method is what keeps the loop breaker
// honest: a rejection added later counts toward termination automatically rather
// than becoming another way to spin. It was invisible once already — r56 failed
// four developer attempts at twenty refusals each and reported a refusal count
// of ZERO, because the metric had been wired to the specification gates and
// nothing else.
func (s *State) NoProgress(notice string) {
	s.Refusals++
	s.Notice = notice + s.Directive()
}

// Progress clears the refusal run. An action that changed something ends the
// deadlock whatever came before it.
func (s *State) Progress() {
	s.Refusals = 0
	s.Notice = ""
}

// Directive tells a repeating agent what is actually happening to it.
//
// WHICH ACTION TO DEMAND DEPENDS ON WHAT IS PENDING, and getting it wrong traps
// the agent between two guards giving opposite orders. Observed exactly that:
// the unverified-write guard said "call run_tests now", this directive said
// "your next action MUST be write_files", and the attempt was abandoned four
// refusals later having been told to do two contradictory things.
//
// THERE IS ONLY ONE ACTION THAT CAN MAKE PROGRESS NOW. A verification used to be
// the answer when writes were piling up unchecked; it is not something the agent
// can choose any more, and naming it cost 28 turns on the dev fixture before
// this was noticed. The checks follow a real edit by themselves.
//
// NO THREAT OF ABANDONMENT, because repetition no longer abandons anything on
// its own. Saying otherwise was true when four refusals killed a ticket and is a
// lie now — and a prompt that threatens a consequence it cannot deliver teaches
// the model to discount the next one. What IS true, and worth saying, is that
// the turns are being spent.
func (s *State) Directive() string {
	if s.Refusals < 2 {
		return ""
	}
	return fmt.Sprintf("\n\nYou have now made %d actions in a row that changed nothing,"+
		" and each one has cost a turn from your budget. Your next action should be "+
		"write_files (make a change that is actually different).", s.Refusals)
}

// Exhausted reports whether the attempt should stop, and why.
//
// ONE PLACE, so a bound added later is checked rather than merely defined. The
// order is the order of certainty: a hard runaway first, then the budget, then
// the softer behavioural signals.
func (s *State) Exhausted(deadCeiling int) (why string, done bool) {
	switch {
	// STRICTLY GREATER, because the check runs at the TOP of a turn: at the start
	// of turn N the agent has taken N-1 actions, so ">=" would give a budget of N
	// exactly N-1 turns — and a budget of one would give none at all.
	case s.Iteration > MaxTotalIterations:
		return fmt.Sprintf("ran to the diagnostic ceiling of %d turns", MaxTotalIterations), true

	case s.Budget > 0 && s.Iteration > s.Budget:
		return fmt.Sprintf("spent its budget of %d turns", s.Budget), true

	case s.ConsecutiveReads >= MaxConsecutiveReads:
		return fmt.Sprintf("read %d times in a row without writing anything",
			s.ConsecutiveReads), true

	// ZERO TURNS THE REFUSAL CEILING OFF, which is what it was before it existed.
	//
	// It is a knob because whether ending an attempt here HELPS is an open
	// question rather than a settled one. The ceiling only began firing at all
	// once the counters stopped being cleared by a write that had not landed, and
	// since then every run has ended its first developer attempt on it — which
	// bounds the waste but also throws away whatever context that attempt had
	// built. r96 did the same work in six turns with the ceiling never reached, so
	// "bounded waste is better than none" is an assumption this makes measurable
	// instead of arguing about.
	case deadCeiling > 0 && s.Refusals >= deadCeiling:
		return fmt.Sprintf("made %d actions in a row that changed nothing", s.Refusals), true
	}
	return "", false
}

// DueForReset reports whether the agent's memory should be cleared while its
// work is kept.
func (s *State) DueForReset() bool {
	return s.Resets < MaxAgentResets && s.Iteration-s.LastResetAt >= AgentResetTurns
}

// ResetAgent clears what the AGENT has accumulated and keeps what it has BUILT.
//
// THE WORK IS NOT THE PROBLEM; THE MEMORY IS. An attempt that has run forty
// turns has usually written real code and then lost the thread — measured on one
// ticket, 77 turns of which 46 were repeat verifications, with the action trail
// showing 46 consecutive identical entries under a heading telling it not to
// repeat itself. A model reads that list and continues the pattern it can see.
// Throwing the ticket away would discard working code because the agent got
// confused; throwing the CONFUSION away keeps both.
//
// WHAT SURVIVES IS THE WORLD. Staged has to: the push re-applies it on top of
// the fetched branch, so an attempt whose writes were never verified would lose
// them outright if this cleared it. The files it wrote are folded into Baseline
// instead, which is what makes this a reset rather than an undo — to the agent
// that continues, its own earlier work is simply the code that was already
// there, exactly as the specification tests are.
//
// THE LAST VERIFICATION SURVIVES TOO. "Fix the remaining failures" needs the
// failures; a fresh agent without them would spend its first turn rediscovering
// what the previous one already knew, which is the cost this is meant to avoid.
func (s *State) ResetAgent(brief string) {
	s.Resets++
	s.LastResetAt = s.Iteration

	// The agent's own writes become part of the ground truth. Baseline exists to
	// answer "is this write discarding code that was already there", and after a
	// reset the honest answer includes the code the previous turns wrote.
	if s.Baseline == nil {
		s.Baseline = map[string]string{}
	}
	for p, c := range s.Staged {
		s.Baseline[p] = c
	}

	// Everything below is memory of HOW it got here, which is the thing being
	// discarded.
	s.Trail = nil
	// The repeat-collapse counters need no clearing: Remember only collapses when
	// History is non-empty, so emptying it is what makes them meaningless. A line
	// zeroing them would be unobservable, which is a line nothing can hold honest.
	s.History = nil
	s.Notice = ""
	s.Refusals = 0
	s.NoopEdits = 0
	s.UndoStack = nil

	// THE READ RUN DELIBERATELY SURVIVES. It is not memory of how the agent got
	// here, it is evidence about the agent itself — and clearing it every forty
	// turns means an agent that only ever reads can never reach the hundred-read
	// ceiling. It then runs to the diagnostic ceiling instead and the ticket
	// reports "ran to 200 turns", which is exactly the unusable message the read
	// bound exists to replace. Caught by a test, not by reading the code.

	// The brief goes last and on its own, because the agent being briefed did not
	// take the action that came before — it is not a rejection.
	s.Restart = brief
}

// RestartBrief is what a reset agent is told.
//
// IT SAYS THE WORK IS KEPT, first and plainly. An agent told only that its
// memory was cleared has every reason to start over, which is the one outcome
// this exists to prevent.
func RestartBrief(mode Mode, staged []string, resets int) string {
	if len(staged) == 0 {
		return fmt.Sprintf("YOU ARE PICKING UP THIS TICKET FRESH (restart %d). Nothing has been "+
			"written yet. Read what you need in ONE call, then make the change.\n\n%s",
			resets, mode.RestartJob())
	}
	return fmt.Sprintf("YOU ARE PICKING UP THIS TICKET PART-WAY THROUGH (restart %d). The files "+
		"below are ALREADY WRITTEN and are on the branch — they are the code that exists now, "+
		"not a draft to redo:\n\n  %s\n\nRead them before changing them, and continue from "+
		"there rather than starting over.\n\n%s",
		resets, joinLines(staged), mode.RestartJob())
}

// RestartJob is what the fresh agent is told its job is.
//
// THE BRIEF MUST MATCH THE JOB. One loop serves three stages, and the first
// version of this told all of them to "make the remaining test failures pass" —
// which is the developer's job and the EXACT INVERSE of the specification
// author's, whose tests are supposed to fail. Observed on a live ticket: an
// author was restarted twice and instructed, in its own prompt, to do the one
// thing its own gate refuses.
func (m Mode) RestartJob() string {
	switch m {
	case ModeTest:
		return "Your job is unchanged: finish the SPECIFICATION for this ticket. The tests " +
			"already written are shown above — keep what is right, add what the ticket asks for " +
			"and is still missing. They must FAIL against the current code; that is what makes " +
			"them a specification, and it is what the gate checks."
	case ModeSpecMerge:
		return "Your job is unchanged: make the sections on this branch compile as ONE " +
			"package. Rename a duplicate, fold two identical helpers into one — and weaken " +
			"nothing: every assertion that was here must still be here when you finish."
	case ModeCoverage:
		return "Your job is unchanged: ADD tests to raise coverage. The tests that were here " +
			"before you are the specification the developer was held to and must not be edited."
	default:
		return "Your job is narrow: make the remaining test failures pass. Read the verification " +
			"output, change what is wrong, and let the checks run. If the existing approach is " +
			"wrong, replace it."
	}
}

func joinLines(in []string) string {
	out := ""
	for i, s := range in {
		if i > 0 {
			out += "\n  "
		}
		out += s
	}
	return out
}
