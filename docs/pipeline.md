# The pipeline

A ticket moves across board columns, and each column is one stage's input. The
routing lives in a table, not in the stages: a stage names the column it takes
from, the column it holds work in, where success goes, where exhaustion goes, and
— for the few that can — where it sends work *backwards*.

## The stages

| role | takes from | on success |
|---|---|---|
| `architect-agent` | inbox | scoping |
| `pm-agent` | scoping | the task columns |
| `spec-agent` | ready for spec | spec merge |
| `spec-merge-agent` | ready for spec merge | ready for dev |
| `dev-agent` | ready for dev | review |
| `sec-agent` | ready for review | ready to integrate |
| `integrator` | ready to integrate | done |
| `resolver` | conflicted | done |
| `coverage-agent` | after integration | done |
| `maintenance-agent` | upkeep queue | ready to integrate |

The working column is not decoration: moving a ticket out of its ready column
**is** the claim (see [claiming.md](claiming.md)), and it is what makes
work-in-progress visible.

## Why the architect is not a ticket, and not the product manager

Every later agent works one ticket in a sandbox of its own, and none of them can
see the others. The design document is the only shared picture of the system, so
it has to exist before anything is broken down.

It was a subtask once, which meant it competed for attempts with the work it was
supposed to inform, and a run that spent its budget elsewhere left every developer
working without it.

Triage and design are different jobs. The product manager decides *what units of
work exist*; the architect decides *what the system looks like*. Merging them
produced a plan shaped by whatever the design happened to say first.

## The reconciler has two jobs and the brief must say which

`spec-merge-agent` both (a) reconciles collisions between sections that share a
branch and (b) repairs a specification the developer has handed back. These need
different instructions, and a brief that does not say which is happening produces
an agent doing neither well.

## Shape: what was actually measured

Three arrangements were run against the same seed request and scored by a
black-box suite the pipeline cannot see:

| shape | time | score |
|---|---|---|
| one task, one author writing every slice | 10.6 min | 25/27 |
| tasks merged into one, one author per unit, one developer | 9m45s | **25/27** |
| N tasks kept separate, one author and one developer each | 14.4 min | **0 — unrunnable** |

The split shape lost on both axes. It pays the fixed cost of every stage N times —
security, integration and spec-merge alone were 34% of that run against 8% for the
merged one — and, more importantly, **nobody owned the whole**: the task that was
supposed to wire the main package wrote a library to match its siblings, and the
delivered repository had no entry point.

The merged shape is the default for that reason, not for speed.

**Moving work between the specification and the developer does not change the
total.** A thin specification gives a fast author and a slow developer; a complete
one reverses it. Measured repeatedly, the sum stayed within seconds.

## Sizing the brief, not the stopping rule

An author is finished when its gate passes, and its gate passes as soon as its
first file compiles and fails correctly — which is as true after one unit of work
as after five. An author briefed on five units wrote one test file in three turns
and every stage after it reported success on a repository that did not compile.

The answer is one unit of work per author, so the gate and the job end at the same
moment. Giving the author a way to end its own stage was tried instead and
measured worse — see [agent-loop.md](agent-loop.md).

## Sections wait for each other

They share a branch, so they are chained rather than concurrent, and the order is
sorted rather than assumed. See [branches.md](branches.md).

## Merging tickets is mechanical

The stage that folds a plan's tasks into one **calls no model**. Merging is
concatenation: every unit of work is carried into one brief with its criteria
intact, quoted whole rather than summarised, because a summary is where a
requirement goes missing and there is no reader downstream who could notice it
had.

Each unit then gets its own section, and each section's brief **names the test
file it owns**. Omitting that let one author append its tests into a sibling's
file, fight the edit interface for three turns, and leave the delivered repository
with no test file for its own unit.

## Registering a stage costs a poll

A dispatcher on a column nothing is ever put into is a poll per interval and a
stage that reads as permanently idle in the window. Stages that are conditional —
ticket merge, upkeep — are registered only when they are switched on.
