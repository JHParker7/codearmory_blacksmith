package dev

import (
	"fmt"
	"strings"
)

// MaxStaleReads is when the advice about re-reading becomes blunt.
//
// IT DOES NOT END THE ATTEMPT: only the budget does that, and a model re-reading
// is not a model doing something wrong.
const MaxStaleReads = 3

// RefundReads reports whether a read costs a turn.
//
// READING IS NOT SPENDING. A read of a file already in hand never reaches a
// sandbox — it is served from what has been read — so it costs one model call
// against a prompt that is already in the KV cache: 2.2 seconds measured,
// against ten to twenty for a verification that pushes, clones and runs the
// suite. Charging the same turn for both spent an entire 200-turn budget in
// about seven minutes on one ticket: 87 consecutive reads, three productive
// actions in it.
//
// So the budget counts WORK — writes, verifications, finishing — and reading is
// free. What bounds an agent that only ever reads is MaxTotalIterations, a
// runaway backstop rather than a budget, and MaxConsecutiveReads, which names
// the actual failure.
const RefundReads = true

// ReadResult is what one read action did.
type ReadResult struct {
	// Fresh are the paths that were not already in hand.
	Fresh []string

	// Stale is true when every path asked for was already known, so the read
	// returned the same bytes and told the agent nothing.
	Stale bool
}

// PlanRead decides which of the requested paths are worth fetching.
//
// A REPEAT IS NOT REFUSED. The refusal that used to live here was aimed at the
// wrong thing: an edit names the EXACT text it replaces, so an agent that wants
// to re-check what it is about to match is doing what the edit format requires,
// not looping. Re-reading is what a careful worker does; the failure worth
// catching is making no PROGRESS, which is a different question with its own
// bound.
//
// A repeat costs a turn and no sandbox — the content is served from what has
// already been read — so the only price is the iteration, which is the honest
// price of asking.
func (s *State) PlanRead(paths []string) ReadResult {
	var fresh []string
	for _, p := range paths {
		if _, known := s.Read[p]; known || s.Missing[p] {
			continue
		}
		fresh = append(fresh, p)
	}
	return ReadResult{Fresh: fresh, Stale: len(fresh) == 0}
}

// StaleReadNotice tells the agent that its read returned what it already had.
//
// SERVED, ALWAYS, and the refund still applies: every read is free, not only the
// ones that returned something. What the notice buys is the agent knowing the
// turn told it nothing, which it cannot otherwise tell — the contents look the
// same either way.
func (s *State) StaleReadNotice(paths []string) string {
	notice := fmt.Sprintf(
		"You already have %s — the contents above are current, and re-reading returned the same "+
			"bytes. This turn has been refunded, but it told you nothing.", strings.Join(paths, ", "))

	if s.StaleReads >= MaxStaleReads {
		notice += fmt.Sprintf(" You have now re-read %d times in a row without changing anything."+
			" Nothing new can come from asking again — write_files to make the change; the checks"+
			" run by themselves once something is different.", s.StaleReads)
	}
	return notice
}

// NoopEditNotice is what an edit that changed nothing is told.
//
// FOUR DIFFERENT SITUATIONS PRODUCE THE SAME SYMPTOM and they need different
// answers. "Your edits changed nothing" is true in all four and actionable in
// none: the agent cannot tell from it whether it has finished, whether the tree
// it wrote has already failed, or whether it is simply repeating itself.
func NoopEditNotice(noopEdits int, verified, testsPass bool, lastTest string) string {
	switch {
	case verified && testsPass:
		// DONE AND PROVED. The stage ends on its own; nothing is left to write.
		return "Rejected: your edits changed nothing — the file already contains exactly what you " +
			"wrote, and the checks have already PASSED against this tree. There is nothing left to " +
			"do; this ticket is finished and will be handed on."

	case verified:
		// TESTED AND FAILING. Rewriting the same text cannot change that, and the
		// failure is what the agent needs in front of it to write something else.
		return fmt.Sprintf(
			"Rejected: your edits changed nothing — the file already contains exactly what you wrote, "+
				"and the checks have already been run against this exact tree: they FAILED.\n\nWriting "+
				"the same text again cannot help. You must change something DIFFERENT — a different "+
				"line range, or different content. The failure was:\n\n%s", clip(lastTest, 1200))

	case noopEdits >= 2:
		return fmt.Sprintf(
			"Rejected: that is the %s edit in a row that changed nothing — the file already contains "+
				"exactly what you wrote, so nothing was checked and nothing can be.\n\nThe checks run "+
				"BY THEMSELVES on any edit that actually changes the file. Make a real change: read "+
				"the numbered contents again, pick the line range you actually mean, and write "+
				"something different from what is already there.", ordinal(noopEdits))

	default:
		// THE FIRST ONE IS OFTEN NOT THE AGENT'S FAULT: a retry inherits a branch
		// an earlier attempt already wrote to, so the file genuinely does contain
		// what it was about to write.
		return "Rejected: your edits produced no change — the file already contained exactly what " +
			"you wrote. An earlier attempt at this ticket may already have written it. The checks " +
			"run by themselves on any edit that actually changes the file, so make a real change " +
			"or leave it alone."
	}
}

// ordinal renders 2 as "2nd", which reads as a count rather than an index.
func ordinal(n int) string {
	suffix := "th"
	switch {
	case n%100 >= 11 && n%100 <= 13:
	case n%10 == 1:
		suffix = "st"
	case n%10 == 2:
		suffix = "nd"
	case n%10 == 3:
		suffix = "rd"
	}
	return fmt.Sprintf("%d%s", n, suffix)
}

// Temperature warms the sampler as a stuck agent accumulates evidence of it.
//
// GREEDY DECODING IS RIGHT UNTIL IT IS NOT. Temperature 0 is correct for a task
// with one right answer, and it is exactly what makes a loop inescapable: the
// same prompt yields the same token, so an agent that has just been refused
// re-derives the identical action.
//
// IT TAKES BOTH COUNTERS, and keying it on refusals alone left the hole this was
// written to close. A stale re-read is REFUNDED rather than refused — reads are
// meant to be free — so it never touched the refusal count, the temperature
// stayed at 0, and the model deterministically re-emitted the same read.
// Measured on the implementation fixture immediately after the first version
// shipped: 66 of 68 turns were identical reads of main.go, with refusals sitting
// at 1. Anything that says "this agent is not progressing" has to feed the ramp,
// or the fixpoint simply moves to whichever counter was left out.
//
// CAPPED LOW, AND 0.8 WAS MEASURED DOING REAL DAMAGE. The reply carries both
// code and PRECISE INTEGERS — the line range — and the integers cannot tolerate
// heat that the prose can. Across nineteen attempts before this ramp existed,
// syntax breaks ran 0-6 per attempt and inverted ranges were almost unknown; the
// three attempts after it shipped at 0.8 produced 17-27 syntax breaks and up to
// 15 ranges whose end line preceded their start.
//
// The job is only to break a deterministic fixpoint, and that needs ONE
// different token, not a different personality. 0.3 is enough to make the
// sampler non-degenerate while leaving the structure intact.
func Temperature(stuck int) float64 {
	// No guard for zero: 0.1 * 0 is already 0, and a line that cannot change the
	// answer is a line nothing can hold honest.
	return min(0.1*float64(max(stuck, 0)), MaxTemperature)
}

// MaxTemperature is the cap. See Temperature for what 0.8 cost.
const MaxTemperature = 0.3

// Stuck is the evidence that this agent is not progressing, from EVERY counter
// that says so. See Temperature for what leaving one out did.
func (s *State) Stuck() int { return max(s.Refusals, s.StaleReads, s.NoopEdits) }
