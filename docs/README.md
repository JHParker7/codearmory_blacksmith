# blacksmith — the reasoning

This directory is the specification the rebuild is written against.

The code it replaces carried its reasoning inline, in comments citing the run and
the number that bought each decision — "measured on r63: 80 refusals on one
section". That corpus is the most valuable thing in the project and the easiest
to lose in a rewrite, because a rewrite re-derives every decision from scratch
and gets the subtle ones wrong. So it is written down here first, as *what each
subsystem must do and why*, before any of it is built again.

**Read this as requirements, not as history.** Each rule below cost a failed run.
A rebuild that satisfies the code but not these rules will reproduce the failure
that produced them.

## What blacksmith is

A **client** of CodeArmory, not a component of it. It runs on a workstation
beside the GPUs and pulls work through conductor. It has no inbound HTTP API and
no registry entry. Three consequences shape everything:

- Work is **pulled**, because conductor cannot reach a workstation behind NAT.
- **Several hosts may attach to one CodeArmory**, so claiming a ticket is a
  race-safe append protocol and every transcript record carries a host.
- **Sandboxes are forge executions**, not local containers. This process never
  runs user code.

## The documents

| document | what it specifies |
|---|---|
| [pipeline.md](pipeline.md) | the stages, the columns, and why the order is what it is |
| [test-first.md](test-first.md) | the load-bearing constraint and the machinery serving it |
| [agent-loop.md](agent-loop.md) | the developer loop: turns, refusals, ceilings, temperature |
| [edit-shapes.md](edit-shapes.md) | why the write schema branches, and what a flat one costs |
| [verification.md](verification.md) | gates, expected red, and handing a broken spec back |
| [branches.md](branches.md) | who owns which branch, and how work reaches it |
| [claiming.md](claiming.md) | the race-safe claim protocol across hosts |
| [sandboxes.md](sandboxes.md) | leases, working trees, and what resets when |
| [model-gateway.md](model-gateway.md) | serving classes, prefill, temperature, reasoning effort |
| [failures.md](failures.md) | the catalogue: what broke, what it cost, what fixed it |

## The conventions that survive the rewrite

**Comments explain WHY, with the evidence.** When you change a decision, say what
it cost. Do not delete that history to tidy up.

**Prefer making a mistake unrepresentable over explaining it.** Repeatedly, a
message telling the model not to do something did not stop it and a schema or a
filesystem constraint did. The edit tool has one shape per way of addressing
code so a quote cannot be copied into its own replacement; `*_test.go` is
`chmod a-w` in the sandbox rather than defended by a refusal.

**Every refusal must be actionable.** A message that names the symptom and not
the cause sends a correct agent to the wrong place — the most expensive single
failure class in this project's history. Name the cause, and give the numbers or
the symbol the agent needs.

**Closed sets for metric labels.** Reason codes are enumerated and anything
unknown becomes `other`; a growing `other` means add a code. An unbounded label
is a cardinality bug.

**Read the agent's own output before diagnosing.** Counting refusals tells you
that something is stuck, never what. Every time this project blamed the model,
the cause was in the harness — see [failures.md](failures.md).
