# The developer loop

One stage, one ticket, one sandbox, many turns. Each turn is a fresh two-message
prompt built from state — **the agent has no conversation memory**, so anything it
needs to know has to be in the state that builds the prompt.

Every defect this project has shipped in the last week lived here.

## The turn

1. Build the prompt from state.
2. Ask the model for one action under the grammar (see
   [edit-shapes.md](edit-shapes.md)).
3. Apply it: read, write, or undo.
4. A write that changes the tree triggers a verification.
5. Decide whether the attempt is over.

The model does **not** decide when to verify. Removing that decision removed the
entire family of refusals that policed it.

## What a turn costs, and what it buys

**Reading is free.** A read of a file already in hand never reaches a sandbox, so
it costs one model call against a prompt already in the KV cache — 2.2 seconds
measured, against ten to twenty for a verification that pushes, clones and runs
the suite.

Charging the same turn for both spent an entire 200-turn budget in about seven
minutes on one ticket: **87 consecutive reads, three productive actions in it.**

So the budget counts **work** — writes, verifications, finishing — and reads are
refunded.

**Re-reading is allowed.** It used to be refused, and the refusal counted toward
the stuck ceiling, which killed tickets four turns after they started: two
abandoned at turn five of forty-eight, 10% of budget spent, nothing produced,
purely for asking for a file twice. An edit names the exact text it replaces, so
an agent re-checking what it is about to match is doing what the format requires.

## The bounds, and what each is for

| bound | value | catches |
|---|---|---|
| turn budget | 200 default | work, not reads |
| `maxConsecutiveReads` | 100 | an agent that only ever reads |
| `maxRepeatRefusals` | 4 | **banks passing work only — never ends an attempt** |
| `maxDeadRefusals` | 20 | an attempt achieving nothing |
| `maxParseFailures` | 4 | replies that are not actions |
| `maxPushFailures` | 3 | a plane fault, not a code fault |
| `maxTestEditRefusals` | 3 | evidence the spec may be at fault |
| `maxSpecRepairs` | 2 | an author that cannot fix its own tests |

**Two ceilings, deliberately.** The low one never ends an attempt: cutting an
agent off for repeating itself abandoned real tickets at turn five of forty-eight,
and repetition is what a careful worker does while recovering. It only *banks*
work already passing — an agent spinning on top of a green, pushed branch has
finished, and saying so is better than failing it.

The high one ends the attempt. "Keep going" turned out to mean "burn every
remaining turn": measured twice in consecutive runs, a spec author made 45 refused
actions in a row and a developer 91, each alternating a write that produced no
diff with a verification that could not run. Neither could reach a legal move.

`maxConsecutiveReads` counts **every** read, not only the ones returning nothing
new: an agent paging through the repository one file at a time is in the same
place as one asking for the same file twice, and neither has produced work.

## Temperature is a ramp, not a setting

At temperature 0 the sampler is deterministic, so a near-identical prompt produces
an identical reply — which is the loop, not a symptom of it.

The ramp warms as evidence of being stuck accumulates: 0 → 0.1 → 0.2, **capped at
0.3**.

- Greedy decoding is unsafe on a repetition-prone model. Measured on
  qwen2.5-coder-32b: a spec author produced a schema-valid object whose `type`
  string ran `..._or_is_not_a_test_file_with_the_correct_test_multiculturalism...`
  to the token ceiling. The architect hit the identical loop and 0.3 ended it.
- The cap is low because the reply carries **code and precise integers** — the
  line range — and the integers cannot tolerate heat the prose can. Across
  nineteen attempts before the ramp existed, syntax breaks ran 0–6 per attempt and
  inverted ranges were almost unknown; three attempts at 0.8 produced **17–27
  syntax breaks and up to 15 ranges whose end line preceded their start**.

**It must be fed by every counter that means "not progressing".** Keying it on
refusals alone left a hole: a stale re-read is refunded rather than refused, so it
never touched the refusal count, temperature stayed at 0, and the model
deterministically re-emitted the same read — 66 of 68 turns identical reads of
`main.go`, with refusals sitting at 1.

## The counters must actually count

This is where the week's defects were, and both are the same mistake: **clearing
a counter before anything had checked whether the work landed.**

1. A write was counted as real on `changedFromBaseline`, which compares the
   agent's own staged map. Whether git has anything to *commit* is a different
   question, and when the answer was no the verification refused the same turn.
   Every turn cleared the counters and then earned a refusal, so nothing ever
   accumulated: six consecutive refusals reading `refusals=1 noop=1` while writes
   climbed 4, 5, 6, 7, 8, 9. No ceiling could fire and the ramp never warmed.

2. Any verification that returned a verdict cleared them, including an identical
   failure. A verdict that has not moved is not progress, however much the tree
   has — an agent alternating between two states never accumulated toward
   anything. r95's branch says it in its own commit log: *add a /nonexistent
   route*, *remove the /nonexistent route handler*, *add a /nonexistent route*.

**Rule for the rebuild:** a counter may only be cleared by evidence that something
actually changed — a verdict that differs from the last one. Not by an intention
to change something.

## The trail

The agent has no conversation memory, so the action trail is the only record it
has of what it already tried. A trail entry naming only the file made twenty
attempts at twenty different payloads indistinguishable from twenty attempts at
the same one — 75 writes byte-identical to one already accepted, none of them
legible as repeats. **A content fingerprint is what makes "I have written exactly
this before" visible to the agent.**

## Finishing

**Green ends the stage, and the loop ends it — not the model.** Left to decide for
itself the model does not stop: verification returned exit 0 and the agent kept
editing for six more turns until the budget ran out, at which point a passing
branch was reported as a failure.

Giving the author a `finish` tool was tried and measured worse: across 3,498
recorded turns the agents called it once, and one ran to iteration 439 still
reading. With it restored, an author ran 49 turns and wrote the same file 41
times.

The truncation problem it was meant to solve — an author stopping after the first
of five units because its gate passes as soon as one file compiles — is answered
by **sizing the brief**, not the stopping rule. One unit of work per author means
the gate and the job end at the same moment.
