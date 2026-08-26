package dev

import (
	"fmt"
	"strings"

	"github.com/code-armory-app/blacksmith/internal/ticket"
)

// Bounds on what a turn quotes.
const (
	MaxTicketRunes = 4000
	MaxReasonRunes = 2000
)

// NumberLines prefixes each line with its 1-indexed number, which is what makes
// the line-addressed edit shape usable: the model quotes a number it can see
// rather than counting to one it cannot.
func NumberLines(content string) string {
	lines := strings.Split(strings.TrimSuffix(content, "\n"), "\n")
	var b strings.Builder
	for i, l := range lines {
		fmt.Fprintf(&b, "%d\t%s\n", i+1, l)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// Reasons is why this ticket came back, if it did.
//
// Two directions of two round trips, and BOTH ARE NEEDED FOR THE SAME REASON:
// the prompt is rebuilt from the ticket every attempt and carries no comment
// history, so a returned ticket arrives looking exactly like a fresh one. The
// agent writes the same change again and is rejected again until a ceiling stops
// the loop. The findings are the only thing that makes the second attempt
// different from the first.
type Reasons struct {
	// Returned is what the reviewer found.
	Returned string

	// SpecRepair is the developer's hand-back to the specification author.
	//
	// THE HAND-BACK WAS WRITE-ONLY FOR A LONG TIME. A comment was posted naming
	// the file, the fault and the failing output, and a counter bounded the round
	// trips — so the mechanism looked complete. Nothing ever rendered the BODY
	// into a prompt. Measured on r68: a section panicked in its own fixture, the
	// developer handed it back twice, and both times the author opened with "I'll
	// write the unit tests for the status parameter filtering functionality" — it
	// had no idea it was a repair. It rewrote the whole file from scratch and
	// reproduced the identical bug, twice, until the ceiling stopped it. The round
	// trip cannot converge while the reason for it is unreadable.
	SpecRepair string
}

// Render is the whole turn state as one string, for callers and tests that want
// it undivided. The loop sends the two halves separately — see RenderWorld.
func Render(t ticket.Ticket, why Reasons, s *State) string {
	return RenderWorld(t, why, s) + RenderChanges(s) + RenderProgress(s)
}

// RenderWorld is the half of the prompt that does NOT change from turn to turn:
// the ticket, why it came back, and what the repository holds.
//
// THE SPLIT IS ABOUT PREFILL, NOT TIDINESS. 93% of every token this pipeline
// moves is prompt rather than answer — measured on r75, the developer read
// 387,419 tokens to write 24,557 — so what a turn costs is mostly the cost of
// re-reading its own context.
//
// The serving backend caches a prompt PREFIX and reuses it, and the effect is
// not marginal. Measured against this deployment at 25,791 prompt tokens: 30.1s
// cold, 0.5s for the identical prompt again, 1.3s for the same prefix with a
// different question appended — and 31.3s, NO SAVING WHATEVER, when a few
// hundred characters change in FRONT of the same block.
//
// That last case was the shape this prompt had. The iteration counter and the
// action trail — both different on every single turn — sat above the repository
// contents, so the largest and most stable part of the prompt was re-processed
// from scratch every time. Ordering by VOLATILITY instead of by topic is what
// makes the cache reachable at all.
func RenderWorld(t ticket.Ticket, why Reasons, s *State) string {
	var b strings.Builder

	fmt.Fprintf(&b, "TICKET %s\nTitle: %s\nPriority: %s\n\n%s\n\n",
		t.ID, t.Title, t.Priority, clip(t.Description, MaxTicketRunes))

	// WHY IT CAME BACK goes ABOVE the repository state and is framed as the task,
	// because a rejection is not context for the original ticket — it IS the work
	// now.
	if r := strings.TrimSpace(why.Returned); r != "" {
		b.WriteString("THIS TICKET WAS SENT BACK BY THE REVIEWER. Your job this time is to fix what it found.\n")
		b.WriteString("The change is already written and pushed; do not start over, correct it.\n\n")
		b.WriteString(clip(r, MaxReasonRunes) + "\n\n")
	}

	// The other direction of the round trip. "DO NOT START OVER" is the operative
	// instruction — the r68 author replaced all 215 lines both times rather than
	// fixing one of them.
	if r := strings.TrimSpace(why.SpecRepair); r != "" {
		b.WriteString("THE DEVELOPER SENT THIS SPECIFICATION BACK. Your job this time is to CORRECT the\n")
		b.WriteString("tests you already wrote — they are on the branch and the developer may not edit\n")
		b.WriteString("them. Fix the fault named below and change nothing else; do not rewrite the file\n")
		b.WriteString("from scratch, and do not weaken an assertion to make the fault go away.\n\n")
		b.WriteString(clip(r, MaxReasonRunes) + "\n\n")
	}

	b.WriteString("REPOSITORY FILES:\n")
	for _, f := range s.Tree {
		fmt.Fprintf(&b, "  %s\n", f)
	}

	// AS READ, IN THE ORDER THEY WERE READ, AND NEVER REWRITTEN. This is the
	// expensive half of the prompt and the half a backend caches, so it has to be
	// APPEND-ONLY: a new read costs the tail and nothing before it, and an edit
	// costs nothing here at all. What the agent has since changed is shown late,
	// by RenderChanges.
	//
	// Sorting these would be worse than it looks. A file read later can sort
	// before one read earlier, which inserts text into the middle of the cached
	// prefix and invalidates everything after it — the same fault as rewriting,
	// arriving on reads instead of writes.
	if len(s.ReadOrder) > 0 {
		b.WriteString("\nFILES YOU HAVE READ:\n")
		for _, p := range s.ReadOrder {
			fmt.Fprintf(&b, "\n--- %s ---\n%s\n", p, NumberLines(s.AsRead[p]))
		}
	}

	return b.String()
}

// RenderChanges is what the agent has done to those files since, and it is
// rendered LATE for the reason RenderWorld is rendered early: this is the part
// that changes every time an edit lands, and putting it in the cached prefix is
// what made the developer reprocess a 25,000-token prompt on every productive
// turn.
//
// ONLY WHAT ACTUALLY DIFFERS. A file read and not edited is already above in
// full; repeating it would pay the tokens twice for nothing. A file the agent
// CREATED was never read, so it has no entry above and appears here whole, which
// is correct — it is entirely the agent's own work.
func RenderChanges(s *State) string {
	var changed []string
	for _, p := range sortedKeys(s.Read) {
		if was, seen := s.AsRead[p]; !seen || was != s.Read[p] {
			changed = append(changed, p)
		}
	}
	if len(changed) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("FILES AS YOU HAVE CHANGED THEM (staged, not yet committed).\n")
	b.WriteString("These supersede the copies above — this is what is on disk now.\n")
	for _, p := range changed {
		fmt.Fprintf(&b, "\n--- %s ---\n%s\n", p, NumberLines(s.Read[p]))
	}
	// Reading a staged file back is the specific waste this prevents: the contents
	// here are ALREADY the staged version, so a read to "check the write landed"
	// returns exactly what the model just wrote and teaches it nothing, while
	// costing one of very few turns.
	b.WriteString("\nReading any of these again returns your own writes.\n")
	return b.String()
}

// RenderProgress is the half that changes every turn: where the agent is in its
// budget, what it has already tried, and what came of the last action.
//
// It goes LAST so that everything before it can be cached, and because the model
// reads the end of the prompt most closely — the rejection and the call to act
// are the two things it must not miss. The trail moved down here from above the
// repository listing; it was placed there to describe the agent before the
// world, which is a real distinction but not one worth re-processing the whole
// tree for on every turn.
func RenderProgress(s *State) string {
	var b strings.Builder

	// SHOW THE BUDGET, NOT JUST THE COUNTER. Without a denominator the model has
	// no way to know turns are scarce, and the failure that produced is specific
	// and repeatable: read one file, read another, read another, until the
	// attempt ends having never run the tests. Naming the remainder every turn is
	// what makes "read everything at once" the obviously correct move.
	//
	// NO RESERVE. It was tried and the model ignored it: every reserve turn in a
	// live run went to read_files, not one to a verification, so the notice
	// changed the prompt and nothing else. Advice arriving at turn fifty-one does
	// not rescue an agent that has spent fifty turns not converging.
	if s.Budget > 0 {
		left := max(s.Budget-s.Iteration, 0)
		fmt.Fprintf(&b, "ITERATION %d OF %d — %d action(s) left after this one, "+
			"then the ticket is abandoned.\n\n", s.Iteration, s.Budget, left)
	} else {
		fmt.Fprintf(&b, "ITERATION %d\n\n", s.Iteration)
	}

	// What the agent has already done.
	//
	// Every other section describes the WORLD; this is the only one that
	// describes the agent. Without it each turn is the model's first turn as far
	// as it can tell, and the observed consequence was an attempt that opened
	// with the same three-file read twice in a row and then re-read a file it had
	// itself just written.
	if len(s.Trail) > 0 {
		b.WriteString("ACTIONS YOU HAVE ALREADY TAKEN " +
			"(do not repeat one — it will cost a turn and change nothing):\n")
		for i, step := range s.Trail {
			fmt.Fprintf(&b, "  %d. %s\n", i+1, step)
		}
		b.WriteString("\n")
	}

	// The verification result and the rejection are rendered SEPARATELY. They
	// shared a field once, so a rejection erased the passing test output and the
	// model lost the evidence it needed to justify finishing.
	if s.LastTest != "" {
		status := "FAILED"
		if s.TestsPass {
			status = "PASSED"
		}
		fmt.Fprintf(&b, "\nLAST VERIFICATION (%s):\n%s\n", status, s.LastTest)
	}

	// THE RESTART BRIEF GOES LAST AND ON ITS OWN. It is the most important thing
	// on the page for the agent reading it, and it is NOT a rejection — the agent
	// being briefed did not take the action that came before.
	if s.Restart != "" {
		fmt.Fprintf(&b, "\n%s\n", s.Restart)
	}

	// THE SECOND OPINION IS NOT A REJECTION and must not be framed as one. It is
	// an analysis of the failure the agent is looking at, from a model that read
	// the same evidence with fresh eyes, so it goes beside the verification
	// rather than under the heading for a refused action.
	if s.Hint != "" {
		fmt.Fprintf(&b, "\nA SECOND OPINION ON THIS FAILURE (advice, not a refusal):\n%s\n",
			s.Hint)
	}

	if s.Notice != "" {
		fmt.Fprintf(&b, "\nYOUR LAST ACTION WAS NOT ACCEPTED:\n%s\n", s.Notice)
	}

	b.WriteString("\nReply with one JSON action.")
	return b.String()
}
