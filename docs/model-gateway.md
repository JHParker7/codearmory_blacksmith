# The model gateway

Serving classes, what a turn actually costs, and the settings that were bought
with a failed run.

## Measured rates

On the reference deployment (qwen3.8 on one card, ollama, one slot):

| | |
|---|---|
| prefill | **~920 tok/s** at scale (5,520 tokens in 6.0s) |
| decode | **~86 tok/s** steady (two 1,200-token generations agreeing to 2%) |

A typical run spends most of its wall clock in **decode**, not prefill: one
measured run moved 127k prompt tokens (~138s) against 23.3k completion tokens
(~272s). 73% of wall clock was model time, with **zero** queueing — there is no
scheduling waste left to reclaim, so the levers are "generate less" and "re-read
less".

## Order the prompt by volatility

**93% of every token this pipeline moves is prompt rather than answer** — measured
on r75, the developer read 387,419 tokens to write 24,557. What a turn costs is
mostly the cost of re-reading its own context.

The backend caches a prompt **prefix**. Measured at 25,791 prompt tokens:

| | |
|---|---|
| cold | 30.1s |
| identical prompt again | **0.5s** |
| same prefix, different question appended | 1.3s |
| a few hundred characters changed **in front** of the same block | **31.3s — no saving at all** |

That last case was the shape the prompt had: the iteration counter and the action
trail, both different every turn, sat above the repository contents, so the
largest and most stable part was reprocessed from scratch every time.

**Rule:** identical system prompt and schema first, then repo-invariant context,
then the ticket brief, then the volatile trail and last verdict. One volatile
token early invalidates everything after it. Anything varying per section — "slice
2 of 5" — must not appear near the front.

## Serving classes

Three: `tiny`, `small`, `large`. A class exists only if its endpoint is
configured; an unconfigured class **falls back** rather than failing, because a
host serving one model must still get a working stage rather than a silent one.

The fallback matters more than the preference. The test author is the first work
stage, so a host where its class does not resolve would leave every ticket in its
ready column with nothing coming to collect it — a silently stalled department,
which is the worst available failure. The log says which happened.

Slots must match what the server actually serves. Asking for more silently gives
fewer, and oversubscribing measures **worse** than matching: 8 requests against 4
slots measured 372 tok/s aggregate against 384 at 4.

## Thinking off

`reasoning_effort: "none"` is the only knob that works on this backend — `"low"`
still thought. Measured on qwen3.8: **358 tokens and 7s with reasoning, 45 tokens
and 1s without, for the same answer.** One run spent 91% of wall clock generating,
and most of what a thinking model generates is not the answer.

## Temperature is a floor, not a value

The class setting is a **floor** the backend is sampled at, because greedy decoding
is not safe on every model — see [agent-loop.md](agent-loop.md) for the ramp and
why it caps at 0.3. Zero means "leave the caller's choice alone", so a well-behaved
backend keeps the determinism the agents were built around.

## The grammar, not the tool list

The gateway drops `response_format` whenever tools are sent, so offering tools
means the arguments generate unconstrained. See
[edit-shapes.md](edit-shapes.md) — this is the single most expensive interaction
in the system.

## Bound the reply

`MaxTokens` is a backstop, not the mechanism. What stops a well-behaved turn is
the grammar; this only bounds the damage when a grammar is unavailable.

## What does not help

**Parallelism.** One card serves one request at a time whatever the slot count
says, and the pipeline is strictly serial by construction — one measured run had
**1 second** of dispatch idle in 619. Running agents concurrently against one card
made things measurably worse: five specification authors claimed at once produced
four of them wiping each other's work.
