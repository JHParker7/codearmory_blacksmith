# Verification

The gate is the only thing that decides whether work is done. Everything here is
about making its verdict mean what it says.

## It runs on the branch, not the working tree

The gate fetches, checks out and hard-resets to the pushed branch before running
anything. That distinction is the whole point: the verdict is about what was
**published**, not about what the sandbox happens to be holding.

## Build before test

`go build ./... && go test ./...`, in that order.

`go test` compiles a main package **for testing**, and a test binary brings its
own entry point — so a `package main` with no `func main` passes the tests and
cannot be built. The pipeline shipped exactly that three times (r94, r100, r103),
each with every stage reporting success and the black-box suite scoring zero:
`function main is undeclared in the main package`.

Build first for the same reason lint goes first: a repository that does not
compile should not pay for the test run, and the error lands at the top of what
the agent reads.

**Capture the whole chain.** A redirect binds to the last element of an `&&`
chain, so composing the gate this way left the build's output uncaptured — and
when the build failed, the test never ran and never created the file the script
then read. The agent was handed `cat: /tmp/.gate-out: No such file or directory`
above its real error, on every failing turn. Group the command.

## Quote the source of a failure

A failing assertion names a file and a line and nothing else about it, so the
agent's next move is always the same: spend a turn reading the file to see what
line 155 asserts. That is a whole model round trip to fetch five lines the sandbox
is standing next to.

The gate prints them. Bounded — a handful of distinct locations, a few lines
each — because a run with forty failures must not paste the suite into the prompt.

The test file is the specification and the developer may not change it, so it is
the one thing it most needs to see and the one thing it cannot edit.

## Expected red is not a defect

`undefined: NewStore` is what a specification written before its implementation
**must** produce. The implementation is missing, so every symbol it should provide
is undefined, and writing the code clears it.

But the exemption is narrow, and both ways of widening it have cost a run:

- **A dotted name is not expected red.** `undefined: sync.atomic` is a package
  selector, and no code written in this package can define it — the test meant to
  import `sync/atomic`. Treating it as expected red hid a real specification
  fault: the developer wrote a correct 138-line implementation, this was the only
  remaining error, and the hand-back was suppressed because the line began with
  `undefined:`.
- **Neither is a bare package name.** An unimported package is reported as
  `undefined: url`, with no dot to tell it from a symbol the implementation has
  yet to write. Measured on r82: a test called `url.QueryEscape` without importing
  `net/url`, the filter read it as expected red, and the developer spent its whole
  budget trying to declare a package.

`too many errors` is also not an error. It is what the Go compiler appends when it
stops after ten, and the file and line on it belong to whatever error happened to
be tenth.

## Green from an author means one of two things

An author is supposed to write red. A green suite therefore needs splitting, and
calling both cases the same was wrong:

- **Vacuous** — the tests assert nothing.
- **Already implemented** — the behaviour is genuinely built. Sections of one
  task *share* a branch, so whichever lands first leaves its code there for the
  rest, which makes this routine rather than exotic.

Measured on r67: an author speccing a store a sibling section had already built
found everything green, diagnosed it correctly, said so every turn — *"the tests
are already written but they're passing against the existing implementation"* —
and had no legal move, because the only way to go red was to write a test it knew
to be wrong.

A section whose tests already pass goes **straight to integration**, not to
development and not to done. The tests exist only on that branch, and the merge is
the one thing that puts them on the integration branch — skipping it loses the
specification, which is the failure that costs the most and shows the least.

## Distinguish "did not run" from "ran and failed"

They need different responses and must never share a message.

- **Nothing to test** — the agent has not changed anything yet.
- **Nothing to commit** — the write left the file byte-identical.
- **Push failed** — a plane fault, not a code fault. Bounded separately, and the
  agent is told its work is safe.
- **The gate errored** — could not fetch, could not check out. This must not come
  back as a verdict. A gate that never ran reporting a *failure* is a verdict on
  code that was never examined, and it resets the loop counters that would
  otherwise end the attempt.

Recording a push failure as a test result once told the no-op guard the tree had
been verified, and the agent was then refused every further verification while
having no way to fix the thing that was actually broken — **118 refusals and 197
turns, none of which could have helped.**

## Verification is triggered by a write, not requested

The model has no action for "run the tests". A write that changes the tree
triggers the gate; nothing else does.

That removes the family of refusals that policed the decision — but it means an
agent whose writes stop changing anything can no longer refresh its verdict. See
[agent-loop.md](agent-loop.md) on why the counters must then accumulate.
