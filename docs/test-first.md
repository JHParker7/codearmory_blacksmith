# Test-first

This is the load-bearing constraint. Most of the machinery in the pipeline exists
to serve it, and a rebuild that weakens it will pass its own tests and ship code
nothing checked.

## The rule

**The specification author writes failing tests. The developer may never edit
`*_test.go`.**

## Why the order is what it is

An agent that writes both the change and the test that checks it will write the
test its change already passes, with no way to notice it has done so — the test
then certifies the implementation against itself.

Running the author *after* the developer does not fix that, it only moves it: an
author that can read the implementation describes what the code does rather than
what the ticket asked for. **Only an author that has never seen the code can
write a test that is a specification instead of a description.**

Observed directly, and it is what prompted the split: a run merged 168 lines of
implementation whose test gate reported "no test files". The gate was green
because there was nothing to run.

The consequence is that a branch is **red** between the authoring columns and a
finished developer, which is what test-driven development is. It is also what
makes the developer's gate mean something: green is "satisfies the tests", not
"nothing ran".

## How it is enforced

**By what each stage may write, not by asking nicely.** The developer cannot
write `*_test.go`; the author can write nothing else. Neither can move the
other's half to make its own pass.

Enforcement belongs in the schema and the filesystem, not in the prompt — see
[edit-shapes.md](edit-shapes.md). A message telling a model not to do something
has repeatedly failed to stop it where a constraint did.

## The consequences that must be rebuilt with it

Because the developer cannot edit the tests, a specification it cannot satisfy is
a dead end with no legal move. Three mechanisms exist for that, and all three are
required:

### 1. The hand-back

When every compile error is in a test file, no change the developer is allowed to
make can turn the gate green. Continuing spends the budget to reach the same
place with less information, so the attempt ends and the ticket returns to its
author.

It goes **back to the author first**, not to a person. This once stopped dead, on
the reasoning that a specification contradicting itself is a decision to make
rather than work to redo. That holds for a contradiction; it does not hold for
what actually arrives, which is a mechanical defect. **r57 stranded an entire
nineteen-ticket board on `declared and not used: tasks`, and the one agent
allowed to fix it was never asked.**

Bounded, because an author that cannot fix its own tests twice will not manage it
on a third pass — and then a person really is the right answer.

### 2. The referee

For the ambiguous middle, an LLM decides whether a failure is the specification's
fault, the developer's, or the expected red of test-first. It acts only on high
confidence, and only after several failed verifications.

### 3. Expected red is not a defect

`undefined: NewStore` is what a specification written before its implementation
**must** produce. A gate that treats it as a fault sends the developer to fix
something that is not broken. Distinguishing expected red from a real defect is a
requirement, not a refinement — see [verification.md](verification.md).

## What breaks when this is got wrong

Both directions have been measured:

- **Too strict**: a developer handed a specification whose test file shadowed its
  own `*testing.T` parameter correctly diagnosed the fault, wrote the right fix,
  and was refused for editing a test file — twenty times, until its ceiling. It
  was right every turn. (r111)
- **Too loose**: the 168-line merge above, and every run where the gate was green
  because nothing ran.

The rule is not negotiable, but the machinery around it — hand-back, referee,
expected-red detection — is what makes it survivable.
