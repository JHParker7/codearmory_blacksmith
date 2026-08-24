# Sandboxes

Every command an agent causes runs in a forge execution. **This process never runs
user code**, and that boundary is not negotiable.

## One sandbox per ticket

A developer task runs a survey, several reads and several verify passes against
one checkout, and holding the sandbox across them removes a microVM boot and a
clone from every command but the first — which measured as most of a task's wall
time.

It is acquired per **ticket** and never reused across them: a sandbox that
outlived one ticket would carry its working tree — and anything a prompt injection
had done in it — into the next.

**The consequence to design for:** the sandbox outlives each *attempt*. A second
attempt inherits the working tree the first one cloned, which may be arbitrarily
old. Anything reading the repository must sync to the branch first, or it reads
history. That cost r111 an entire ticket: a specification the reconciler had
repaired still looked broken to every later attempt.

## Two ways to run a command

- **Plain** — execute against the working tree as it stands. Cheap; correct only
  when the tree is known to be current.
- **On a branch** — `fetch`, `checkout -B`, `reset --hard`, `clean -xfd`, then the
  script. This is the one that makes a verdict mean something, and it **destroys
  anything uncommitted**.

Because of that reset, anything an agent writes must be committed and pushed in
the same command. There is no "later".

**Tolerate a branch that does not exist.** A strict fetch under `set -e` aborts
the whole script, and on a first attempt the branch has never been pushed. That
mistake blocked all five sections of a run within ninety seconds.

## The plane's limits are the plane's

forge caps what it is asked for. Idle timeout and max lifetime are requested but
**capped server-side** — 300s idle, 3600s lifetime — so a lease is never as
long-lived as the caller believes.

It reports a finished execution as **`completed`**, not `succeeded`. Polling for
the wrong word spins until the lease is reaped.

A held lease is a claim on the box, so the number held at once is bounded, and the
bound is respected by queueing rather than discovered by failing: a cap the plane
enforces is one the caller should respect.

## Polling

An agent's command is usually seconds to minutes, so the first few polls are quick
and then back off — a tight poll for a long build is load on the control plane for
no information.

**The cap is what the pipeline waits on.** At 5s it was tuned for a workload this
department does not have. Because the interval doubles from the minimum, the only
instants anyone can learn an execution finished are 0.5s, 1.5s, 3.5s, 7.5s, 12.5s,
17.5s — and **every sandbox duration recorded on r77 was one of exactly those six
numbers, 69 of them, with nothing in between**. They are not measurements of work;
they are the moments someone looked.

The waste is half the final interval per call.

## Model-authored bytes never reach a command line

File contents are base64-encoded on the way into the sandbox. This is the only
place model output becomes part of a script, and encoding it is what keeps "write
a file" from becoming "run a command".

The same applies to commit messages: they are written to a file and passed with
`-F`, not formatted into `commit -m`. Go quoting is not shell quoting, and a
summary containing `$(...)` was substituted by the shell — model output becoming a
command, on the one path carrying whole source files through a string field.

## The gate script and the agent are different readers

Output that helps a human debug and output that helps an agent act are not the
same text. The gate quotes the source of a failing assertion because that saves
the agent a round trip; it does not paste the whole suite, because a run with
forty failures would bury the one line that matters.
