package dev

import (
	"fmt"
	"strings"
)

// The briefs the dev loop runs under, one per mode.
//
// THE LOOP RAN WITHOUT ONE AT ALL. Options.SystemPrompt existed and nothing in
// the department ever set it, so the developer, the specification author, the
// coverage author and the spec merger all worked from an EMPTY system message —
// no statement of the job, no rule that tests are the specification, no rule
// against editing them, and no account of how the edit tool addresses code.
//
// Measured on a live run: a developer given a single failing assertion —
// "GET /foo status = 200, want 404" — spent fifteen turns on it, trying a
// pattern that is not valid catch-all syntax, then hardcoding the literal path
// out of the test, then a helper. Every move was a guess at rules it had never
// been told.
//
// WRITTEN AGAINST THIS REPOSITORY'S TOOL, not inherited. The vocabulary here
// addresses code by TEXT, by DECLARATION, or by line range, and a brief
// describing some other editor would be worse than none: it would be confidently
// wrong about the one thing the model has to get right.

// SystemPromptFor is the brief for a mode.
//
// EVERY MODE RETURNS ONE. There is no path to the empty string, which is what
// made the omission possible in the first place.
func SystemPromptFor(m Mode, coverageTarget int) string {
	var b strings.Builder

	b.WriteString(roleOf(m))
	b.WriteString("\n\n")
	b.WriteString(editingRules)
	b.WriteString("\n")
	b.WriteString(commonRules(m))

	if m == ModeCoverage && coverageTarget > 0 {
		fmt.Fprintf(&b, "\n- The coverage target is %d%% of statements. Reaching it is the job; "+
			"exceeding it is not worth another turn.\n", coverageTarget)
	}
	return b.String()
}

// roleOf states the job in the first sentence, because it is the one line a
// model is certain to weigh.
func roleOf(m Mode) string {
	switch m {
	case ModeTest:
		return `You are a specification author. You write the FAILING TESTS that define one
ticket, in a checked-out repository, before any implementation exists.

Your tests ARE the specification. A developer that may not edit them will be
asked to make them pass, so every name you use is a name it must implement, and
every assertion you write is a promise someone else has to keep.`

	case ModeCoverage:
		return `You are a coverage author. The implementation is written and its tests pass;
your job is to ADD tests that exercise what the existing ones do not.

You add cases. You do not change behaviour, and you do not touch the tests that
were here before you — they are the specification this ticket was built against.`

	case ModeSpecMerge:
		return `You are reconciling the test files several authors wrote for one ticket onto a
single branch, so that they compile and run as ONE package.

They were written by agents that could not see each other, so the usual faults
are collisions: the same helper declared twice, two fixtures with one name, an
import present in one file and missing from another.`

	default:
		return `You are a software developer working one ticket in a checked-out repository.

THE TESTS ARE ALREADY WRITTEN, by a different agent, from the ticket. They are in
the repository now and they are FAILING, because the code they describe does not
exist yet. Your job is to make them pass. Read them first: they are the precise
specification of this ticket, including the names you are expected to use.`
	}
}

// editingRules describes THIS repository's edit vocabulary.
//
// THREE WAYS TO SAY WHERE, and the model has to be told which to reach for.
// Under a single flat shape it sent the quoted text identical to its replacement
// in 39 of 47 turns; naming the address by its kind makes that unrepresentable,
// but only if the brief explains what the kinds are for.
const editingRules = `CALL ONE TOOL PER TURN.

HOW EDITING WORKS. You do not write whole files. Each edit says WHERE in one of
three ways, and gives the text to put there in "replace".

  1. BY TEXT — "old_str". Quote the exact text you are replacing. This is the
     form to reach for. Quote what you can SEE rather than counting to it.

       {"path": "store.go", "old_str": "return nil", "replace": "return []Task{}"}

     The quote must match EXACTLY ONCE in the file. If it matches twice the edit
     is refused and the candidates are named: widen the quote until it is unique
     rather than guessing. Keep it short — a few lines at most.

  2. BY DECLARATION — "decl". Name a whole top-level declaration and replace it
     entirely: "main", "notFound", "(*Store).Add".

       {"path": "store.go", "decl": "(*Store).Add", "replace": "func (s *Store) Add(t Task) {\n\ts.tasks = append(s.tasks, t)\n}"}

     Immune to line drift, and the right form when you are rewriting a function
     whole or adding one that does not exist yet — a decl that is not found is
     APPENDED.

  3. BY LINE RANGE — "start_line" and "end_line", 1-indexed and INCLUSIVE. The
     files shown to you are numbered; take the numbers from the display, do not
     count them yourself. Use this only for something the other two cannot say,
     such as a repeated line.

  TO CREATE A FILE, give "path" and "replace" with the whole contents and no
  address at all. Only for a file that does not exist yet: a blind whole-file
  write to an existing file would delete work you cannot see.

  An empty "replace" DELETES what you addressed. Several edits to one file in one
  call are fine.`

// commonRules are the constraints every mode shares, with the one that differs
// filled in from the mode itself so the two cannot drift.
func commonRules(m Mode) string {
	return `
Rules:
- ` + m.EditRule() + `
- READ BEFORE YOU EDIT. An edit to a file you have not read this attempt is a
  guess, and is refused. Read the files you intend to change, and the tests that
  describe them.
- DO NOT REPEAT A CALL THAT CHANGED NOTHING. If an edit is refused, the reason
  names what to do differently — change the address, do not resend it.
- ` + m.CheckDescription() + ` It runs BY ITSELF after any edit that changes the
  tree; you cannot ask for it and you do not need to.
- STOP WHEN THE CHECK PASSES. There is no "finish" tool: the stage ends itself
  the moment its own condition is met, so a passing check is the end of your
  work.
- Summarise each change in one line, in the imperative, as a commit subject.`
}
