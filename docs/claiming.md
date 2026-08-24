# Claiming work

Several hosts may attach to one CodeArmory, so taking a ticket is a race between
machines that cannot see each other. The ticket store is the **only** arbiter.

## Two paths, one guarantee

**Preferred — conditional write.** The tickets service serves a version as an
ETag and accepts `If-Match` on `PUT`, so moving a ticket out of its ready column
is a compare-and-set: the version is in the `WHERE` clause of the `UPDATE`, so
there is no window between checking and writing and exactly one racer can win.

**Fallback — claim by append.** Older instances have no `If-Match`, and there a
conditional write degrades to last-write-wins, which would let two hosts both
believe they won. It is detected by the *absence* of an ETag on the read.

The fallback exploits comments being append-only with server-assigned ordering:
every host appends a claim, reads back, and yields unless the **oldest** claim is
its own. Both hosts apply the same deterministic rule to the same server-ordered
list, so they agree even when one reads before the other has written.

## The claim comment is written either way

It is not just a lock. It is also:

- **the audit trail** — which host and which role took the work, something the
  agent account alone cannot express when one role runs on several machines;
- **the attempt counter** — how many times this ticket has been tried by this
  role.

## Moving out of the ready column *is* the claim

The working column is what makes work-in-progress visible, and it is the same act
as the compare-and-set. There is no separate lock to leak.

## A claim is written when work starts

So a killed process leaves one behind, and it counts against the attempt ceiling.
Stale claims must be pruned, or a host that crashed once slowly consumes every
ticket's attempts.

## Roles, columns and attempts

Each stage takes from one column and holds the ticket in another. A ticket is
refused if its dependencies are unfinished or if this role has already used its
attempts.

`maxAttempts` is 3. A ticket that fails three times has failed on its merits and
belongs in front of a person.

## What a stage may not assume

- **That it is the only host.** Every listing may be stale by the time it is
  acted on; the compare-and-set is what makes that safe.
- **That a ticket it can see is a ticket it may take.** Dependencies, attempts and
  the handler's own `Wants` all gate the claim, and the board read behind `Wants`
  may fail — in which case the answer is **no**. Proceeding on a guess is the
  failure that gating exists to prevent.
