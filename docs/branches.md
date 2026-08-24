# Branches

Who owns which branch, how work reaches it, and the ways it has silently gone
missing.

## One branch per task

`agent/<task-id>`. **A section writes onto its task's branch**, not its own.

That is the whole assembly mechanism: several sections each write one slice of the
specification, all onto the same branch, and the task then reaches development
with the complete thing. Giving each section its own branch would leave several
specifications in several places and nothing to develop against.

**Only a section redirects to its parent.** The redirect once applied to
development too, and because a task *also* has a parent — the request the product
manager broke down — every developer was sent to the request's branch, where
nothing had been written.

Measured on r73, and it is silent in the way that costs most: eight sections wrote
their tests to four task branches, all four developers were pointed at
`agent/<request>` where there were none, each finished in seven records against an
empty tree, the security stage reported "empty diff", and the integrator merged
it. **The whole board reported success in 10.3 minutes having delivered one
struct**, while the specification sat on four branches nobody merged.

## Sections are chained, not concurrent

Sections of one task share its branch, and a sandbox begins by resetting hard to
that branch. Two authors writing at once therefore wipe each other's work — and
**neither can tell**: the loser's file is still in its own tree, so its next edit
reads as a no-op, while the gate it runs against reports "no test files were
written".

Measured on r91: five sections claimed at once, four of them looping on that exact
pair of messages until the run was stopped.

So each section depends on the one before it. The order is **sorted**, not
assumed — leaving it to whatever the listing returned made both the brief and the
write order vary run to run.

## Everything the agent writes must reach the branch

The gate builds the **branch**, not the sandbox's working tree. Work that stays in
the sandbox is work that does not exist.

This has failed twice, both silently:

- **The push was never reached.** A per-file commit split committed every file
  individually, leaving nothing for the commit that followed; it failed with
  "nothing to commit", the retry failed the same way, and `set -e` aborted the
  script *before* `git push`. Only the first file — written while a single staged
  file meant the split did not run — ever reached the branch. For twenty turns
  afterwards the agent was shown its own correct `main.go` and told by the gate
  that `func main` was undeclared. (r110)
- **The tree was never re-read.** The sandbox is held per *ticket* and outlives
  each attempt, and the survey ran against its working tree with no fetch. A
  specification the reconciler had repaired and pushed still looked broken to
  every later attempt, which read the tree the first attempt had cloned. (r111)

**Rules for the rebuild:** a commit step that may legitimately find nothing to
commit must not abort the push; and anything that reads the repository must sync
to the branch first — tolerantly, because on a first attempt the branch does not
exist yet.

## Integration rebases before it merges

An agent branch is cut from the integration branch when its sandbox cloned, and
integration is serial, so by the time a branch arrives the integration branch has
usually moved on beneath it. Merging then compares the work against a snapshot
that no longer exists, and **every ticket touching the same file conflicts by
construction** rather than because two changes disagree.

Measured: three of four unfinished tickets in a run were correct, reviewed
branches lost this way.

Replaying the commits onto the current tip removes the ordering conflicts and
leaves only the genuine overlaps — which are the ones worth sending to the
resolver, and the ones a person would actually have to think about.

## RunOnBranch resets the working tree

`fetch`, `checkout -B`, `reset --hard`, `clean -xfd`. **Anything an agent wrote
and did not commit is gone.** Anything it needs must be committed and pushed in
the same command.

## The branch is read off the ticket, and not every stage marks it the same way

Every writing stage publishes a branch, and they do not share a marker: a
developer's comment carries one, an author's another, a coverage run a third. All
of them render the same `**Branch:**` line.

Reading only the developer's marker meant a ticket could be *admitted* to a stage
that then could not find its branch — the already-satisfied route sends a section
straight to integration, admission was widened for it, extraction was not, and the
integrator failed with "no branch recorded". It retried until its budget was gone
and took the other five tickets on the board to blocked with it.

**Extraction is not admission.** Answering "which branch is this" for a tests-only
ticket is safe and necessary; deciding a tests-only branch is *mergeable* is a
different question and belongs elsewhere — a branch carrying failing tests and no
implementation must never look like finished work.
