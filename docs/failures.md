# The failure catalogue

What broke, what it cost, what fixed it. Ordered by how much it teaches.

The single most useful entry in this file is the pattern at the end.

## Work that never left the sandbox

**r110.** The developer wrote `ticket.go`, `store.go`, `main.go` and
`handlers.go`, all four correct. The branch received only `ticket.go`.

A per-file commit split committed each written file individually, leaving nothing
for the commit that followed. It failed with "nothing to commit", the retry failed
the same way, and `set -e` aborted the script **before `git push`**. `ticket.go`
survived only because it was written while a single staged file meant the split
did not run.

For twenty turns afterwards the agent was shown its own correct `main.go` in the
prompt and told by the gate that `func main` was undeclared — because the gate
builds the branch. It was right every turn and told it was wrong every turn.

*Rule:* a commit that may legitimately find nothing to commit must not abort the
push.

## A tree that was never re-read

**r111.** The developer correctly handed back a specification whose test file
shadowed its own `*testing.T` parameter. The reconciler repaired it and pushed.
Every later attempt still read the broken file.

The sandbox is held per **ticket** and outlives each attempt; the survey ran
against its working tree with no fetch, so attempt two read the tree attempt one
had cloned. The agent diagnosed the fault exactly, wrote the same fix the
reconciler had already made, was refused for editing a test file, re-read the
cached copy, and repeated until its ceiling.

Three actors each behaving correctly against a file that no longer existed.

## Counters cleared before anything was checked

**r105.** Thirty-eight refusals with a twenty-refusal ceiling that never fired.

The write branch cleared `refusals`, `noopEdits` and `staleReads` on
`changedFromBaseline` — which compares the agent's *own staged map*. Whether git
has anything to **commit** is a different question, and when the answer was no the
verification refused the same turn. Every turn cleared the counters and then
earned a refusal: six consecutive refusals reading `refusals=1 noop=1` while
writes climbed 4, 5, 6, 7, 8, 9.

Neither the loop breaker nor the temperature ramp could see more than one of
anything.

**Related:** any verification that returned a verdict also cleared them, including
an identical failure. A verdict that has not moved is not progress, however much
the tree has.

*Rule:* a counter may only be cleared by evidence that something changed — never
by an intention to change it.

## Developers pointed at the wrong branch

**r73.** Eight sections wrote their tests to four task branches. All four
developers were sent to `agent/<request>`, where nothing had been written. Each
finished in seven records against an empty tree, security reported "empty diff",
and the integrator merged it.

**The whole board reported success in 10.3 minutes having delivered one struct.**

## A gate that was green because nothing ran

A run merged 168 lines of implementation whose test gate reported "no test files".
This is what produced the test-first split.

**Its modern form:** `go test` compiles a `main` package for testing and a test
binary brings its own entry point, so a package with no `func main` passes the
tests and cannot be built. Shipped three times (r94, r100, r103), every stage
green, black-box score zero. Fixed by building before testing.

## An author with no legal move

**r67.** An author speccing a store that a sibling section had already built and
merged found everything green. It diagnosed the situation correctly and said so
every turn — *"the tests are already written but they're passing against the
existing implementation"* — and had no move, because the only way to go red was to
write a test it knew to be wrong.

*Rule:* "green from an author" splits into vacuous and already-implemented, and
they need different answers.

## Expected red that was not

**r82.** A test called `url.QueryEscape` without importing `net/url`. The
compiler says `undefined: url`, the expected-red filter read it as the normal red
of test-first, and the developer spent its whole budget trying to declare a
package.

The mirror image: `undefined: sync.atomic` — a dotted name no code in the package
can define — was also treated as expected red, hiding a real specification fault
behind a correct 138-line implementation.

## A copy the sampler could not resist

Under a flat edit schema the model sent `old_str` identical to `replace` in **39
of 47 turns**, and the no-progress ceiling killed the ticket — twice, taking the
board with it. A message naming the mistake did not move it; neither did a length
cap. Branching the schema by addressing mode removed the opportunity.

## A ceiling that was too strict

Two tickets abandoned at turn five of forty-eight — 10% of budget spent, nothing
produced — for asking to read a file twice. Re-reading is what the edit format
requires, and the guard meant to stop waste became the reason two tickets reached
nobody.

*Rule:* the generous ceiling never ends an attempt. Only the high one does.

## Idle work that competed with real work

**r99.** A scout opened "make the linter clean" eleven seconds after startup — the
board was empty, so it was correct by its own rule — and then a request arrived.
Ten minutes later the run had spent 94 model turns, 56 of them (59%) on upkeep, 41
of those refusals, on a repository holding a `go.mod` and a README with nothing to
lint.

Fixed by harvesting findings from work that already happened, and gating at the
**claim** rather than at the opening: a board empty at that instant says nothing
about the next ten minutes.

## A cosmetic rule that bricked a run

**r104.** A commit-msg hook counted subject length in **bytes** while the message
was clipped by **runes**, and the clip appended a three-byte ellipsis. Agents
write prose with em dashes, so seventy runes came out over seventy-two bytes. The
hook rejected the commit, the commit failure failed the push, and all five
specification sections blocked with **"could not push the branch"** — a message
pointing squarely at the network.

## A reply thrown away for one character

`escapeControlCharsInStrings` exists so a reply carrying a good file is not
discarded over one character, and it made an exception for the only escape it did
not check. Any string containing `\users`, `\usr` or a Windows path failed to
decode and the edit went with it.

Found by reading the function against its own stated principle, not by a run
failing — which is the only entry here discovered that way, and the argument for
doing it deliberately.

---

## The pattern

**Every one of these was in the harness. None was the model.**

The diagnosis went wrong the same way each time: counting refusals, or reading the
board, instead of reading what the agent actually said. Its `completion` carries
its reasoning, and in every case above it names the fault correctly while the
harness contradicts it.

Before proposing a cause:

1. Dump the agent's completions and **read them**.
2. Compare what it believes about the tree against what is actually on the
   branch. A mismatch there is a harness bug, every time.
3. Only then is "the model struggled" a claim worth making.

The corollary, learned expensively: **a test that passes for the wrong reason is
worse than no test.** Three fixes this week were declared verified by runs that
never exercised the code path in question. If a fix is meant to change behaviour,
show the test failing without it.
