# blacksmith, in Python

Start here: **the minimum needed to find out whether a model can act as an
agent at all.** A model client, seven tools, a loop, and four graded tasks that
score it. Everything else gets added once this says yes.

```bash
make -f Makefile.py venv

# Is the endpoint up, and will this model call a tool at all?  (~5s)
.venv/bin/python -m blacksmith check

# Can it do real work? Scored on the graded tasks.
.venv/bin/python -m blacksmith bench

# Against a directory of your own.
.venv/bin/python -m blacksmith solve "make the failing test pass" --workspace ./scratch
```

Any command takes `--endpoint` and `--model` to point somewhere else:

```bash
python -m blacksmith --endpoint http://192.168.58.1:11434/v1 --model qwen3-coder-38:latest bench
```

## Measured on this machine

llama-server on `:8080` was down; these ran against ollama on `:11434`.

| model | size | score | notes |
|---|---|---|---|
| `qwen3-coder-38` / `qwen3.8` | 27.3B | **4/4** | 4 turns each, 0 refusals, ~6s per task |
| `qwen3.6:27b` | 27.8B | **4/4** | 4–5 turns, 0 refusals, ~18s |
| `qwen3-coder:latest` | 30.5B | **4/4** | 4–7 turns, 0 refusals |
| `qwen2.5-coder:32b` | 32.8B | **4/4** | 0/4 until the text-protocol fallback |
| `qwen2.5-coder:14b` | 14.8B | **4/4** | 0/4 until the text-protocol fallback |
| `ministral-3:14b` | 13.9B | **4/4** | 6–10 turns, 3 refusals total |
| `granite4.1:30b` | 28.9B | 3/4 | slow: 30–112s per task |
| `llama3.1:8b` | 8B | 1/4 | burns the budget |
| `qwen2.5:1.5b-instruct` | 1.5B | 0/4 | stops emitting tool calls |

The 27B model drives the loop cleanly. That is the answer to the original
question, and it is what makes the rest of the pipeline worth building.

**`qwen3.8` and `qwen3-coder-38` are the same model.** `/api/show` reports
`parent_model: qwen3.8:27b-q4_K_M` for both, with no system prompt and the same
13-character template — `qwen3-coder-38` is a re-tag, not a coder-tuned variant.
Their scores match turn for turn and within ~10% on tokens, which is a useful
check on the instrument: identical weights should produce an identical result,
and they do. Comparing them is not a comparison.

## Serving throughput

Measured against ollama's native `/api/chat`, which returns `eval_count` and
`eval_duration` — an exact rate rather than one inferred from wall-clock, which
would fold in model load, prompt processing and HTTP.

`qwen3.8` (27.3B, Q4_K_M, 17.9GB):

| | |
|---|---|
| generation | **~65 tok/s** (64–77 across prompts; stable to ±0.2 within one prompt) |
| prompt processing | 400–1600 tok/s, rising with batch size |
| cold load | ~10s |

Generation degrades gently with context, which is what the loop actually does to
it — the history grows every turn:

| prompt tokens | gen tok/s | wait before first token |
|---|---|---|
| 80 | 77.2 | 0.3s |
| 1,924 | 74.1 | 2.3s |
| 11,385 | 69.8 | 7.2s |

**It is not running on the NVIDIA card.** The ollama unit sets
`CUDA_VISIBLE_DEVICES=` (empty) and `GGML_VK_VISIBLE_DEVICES=1`, so it serves
through **Vulkan on the AMD Radeon AI PRO R9700** — confirmed by sysfs, card0
holding 27.8GB of its 34.2GB while the RTX 4060 Ti sits at 2GB idle. Worth
knowing before tuning anything: a ROCm backend, or the idle second card, are both
unexplored.

## The baseline: a ticketing system

The task to track harness changes against. One spec, `benchtasks/ticketing/`, 37
tests across three layers:

| layer | what it asks for |
|---|---|
| store | create / get / update / list, ids, a closed status set, validation |
| API | a JSON WSGI app — list, create, fetch, patch, filter, 400s and 404s |
| GUI | an HTML page grouping tickets by status, with a create form, escaped |

**Graded by tests passed, not pass/fail.** A binary result cannot show that a
change took a model from 4 of 37 to 22 — it reports "FAIL" either way, so a real
improvement looks like no change and the next change is made blind.

```bash
python -m blacksmith bench             # all five tasks, recorded, with deltas
python -m blacksmith bench ticketing   # just the baseline task
python -m blacksmith history           # every result so far
```

No flags: the reference model is **pinned in the repository**
(`cli.REFERENCE_MODEL`), not read from the operator environment. `config.py`
describes the DEPARTMENT's serving stack, which is a thing an operator changes —
a different endpoint, a different tag, a card swapped out — and a baseline whose
model moves when someone edits `~/.config` is not a baseline. `--model` and
`--endpoint` still override it.

Every run is appended to `benchtasks/history.jsonl` with the **harness git sha**
(and whether the tree was dirty). Without that a score is not attributable to
anything: there is no way to tell whether the model improved or the rig changed
underneath it. Re-running prints the delta against that model's last run of that
task — scoped to both, because comparing ticketing against fix_return produces a
number that means nothing.

Recorded per run: tests passed/total, turns, tool calls, refusals, spins,
stop reason, prompt and completion tokens, and the cost split — **model time vs
tool time vs wall clock**. The split matters: a change that cuts turns while
doubling the time in pytest is not an improvement, and one wall-clock number
hides which happened. Generation rate is computed over model time only, so a slow
test suite cannot make a model look slow.

### The baseline — `qwen3.8:latest`, harness `f003c6e`

| task | score | turns | calls | refused | tokens | wall |
|---|---|---|---|---|---|---|
| fix_return | 2/2 | 4 | 5 | 0 | 210 | 6s |
| find_the_bug | 3/3 | 4 | 6 | 0 | 285 | 6s |
| new_file | 2/2 | 5 | 6 | 0 | 340 | 7s |
| no_cheating | 4/4 | 4 | 5 | 0 | 283 | 6s |
| **ticketing** | **37/37** | 6 | 7 | 0 | 2341 | 39s |
| **total** | **48/48** | | | | 3459 | 64s (62s model, 2s tools) at ~56 tok/s |

**The noise floor is zero.** Three consecutive runs produced identical scores,
identical turn counts and an identical 3459 completion tokens, because the client
pins `temperature=0`. That is the property that makes this worth having: a delta
of a single test is signal, not variance, so there is no threshold to argue about
before calling a change positive or negative.

Honest limit: **qwen3.8 is at the ceiling** — 48/48, one-shotting every task. For
this model the baseline detects REGRESSIONS only. Headroom lives in the weaker
models, which is where both harness improvements so far actually showed up; the
history keeps their earlier rows for that reason, and `compare` is scoped per
model so they never interfere.

Only qwen3.8 is tracked going forward. qwen3.6 is the previous generation of the
same thing and `qwen3-coder-38` is the same weights under another tag, so
measuring either adds a row and no information.

## The four small tasks

Each is a tiny workspace with a failing test. The score is whether the test
passes afterwards — never what the model said.

| task | what it isolates |
|---|---|
| `fix_return` | can it use a tool at all, and edit by quoted content? |
| `find_the_bug` | will it LOOK, when the bug is not where the test points? |
| `new_file` | can it create something, not only edit? |
| `no_cheating` | does the test-file refusal hold when editing the test is the easy way out? |

`tests/test_tasks.py` guards the tasks themselves: each must start red, be
solvable without touching a test file, and **not** be satisfied by the obvious
wrong answer. That last check earned its place — see below.

## The two rules the loop is built on

1. **Nothing a model can emit ends the run.** Invalid JSON arguments, an invented
   tool, a missing field, prose where a call belongs — each becomes a message the
   model can act on and costs one turn. A harness that raises on those cannot
   measure whether a model is any good; it only establishes that it is not
   perfect, which was never in question. On a 27B local model most turns contain
   a mistake, so this is the measurement apparatus, not defensive programming.

2. **The model's claim to have finished is not evidence.** `finish` is refused
   until the tests actually pass, and the score comes from running them. Small
   models declare victory after an edit without ever running anything.

## The tools

Seven, and the seventh had to earn its place — every extra tool is another thing
a small model can pick wrongly, so the bar is evidence that its ABSENCE cost
something (see *Read the reasoning* below). Their shapes are chosen to make a
wrong move impossible rather than to explain it afterwards:

- `edit_file` addresses by **quoted content** and refuses a match that is absent,
  ambiguous, or identical — so the wrong one of three similar lines cannot be
  silently patched.
- `write_file` **refuses to overwrite**, so changing a file means quoting the part
  being changed rather than reconstructing it from memory and dropping what was
  forgotten.
- **Test files are not writable at all.** An agent that can edit the test can make
  it pass without making the code work, and it will.
- Paths are resolved before the boundary check, so a symlink out of the workspace
  is refused too. These run on the workstation with no sandbox.

## The harder test: a web GUI, against the real codebase

A scratch copy of this package plus one failing spec (`test_web.py`, 12 tests)
asking for a board view: every column in order, tickets under their own column,
titles escaped, served as a WSGI app.

`qwen3-coder-38` **passes it**, and the result is genuinely good code — it reads
`workflow.py` and `tickets.py`, uses the real `WORKFLOW_COLUMNS`, escapes titles,
handles a ticket in an unknown column, and picks up the codebase's comment voice.
Served for real it returns 200, all 22 columns, all tickets, and 404s elsewhere.

| | turns | spins |
|---|---|---|
| before search_files / read ranges | 14 | 8 |
| after | **7** | **0** |

### The graded bench does not predict the hard task

Six models score 4/4 on the four small tasks. On the real codebase they split
hard, which is the strongest argument for keeping a task of this size around:

| model | result | turns |
|---|---|---|
| `qwen3.8` / `qwen3-coder-38` | **PASS** | 7 |
| `qwen3.6:27b` | **PASS** | 7 |
| `qwen3-coder:latest` (30.5B) | PASS | 12 |
| `ministral-3:14b` | FAIL — ran out the 25-turn budget, tests still red | 25 |
| `qwen2.5-coder:14b` | FAIL — stopped calling tools | 13 |
| `qwen2.5-coder:32b` | FAIL — wrote a 0-byte file, then ran out the 9-minute clock | — |

### And passing is not the same as good

Of the three that passed, the output differs by more than style. Only the tests
were graded; these were found by reading the code afterwards:

| | qwen3.8 | qwen3.6:27b | qwen3-coder:latest |
|---|---|---|---|
| lines | 118 | 75 | 110 |
| found and used `Column.color` | **yes** | no | no |
| shipped CSS — an actual board | **yes** | no | no |
| escapes the ticket **id** | yes | yes | **no — XSS** |
| dead code / trailing whitespace | no | no | yes |

`qwen3-coder:latest` interpolates `ticket_id` into an `id="..."` attribute
unescaped, so a hostile id closes the attribute and injects script. The graded
spec only asserted that TITLES are escaped, so all 12 tests passed. A score is
not a code review.

**qwen3.8 produced the best output** — the only one that went looking for
`Column.color` and used it, the only one that rendered a laid-out board rather
than a list of headings, and the only one whose docstrings say *why* rather than
*what*.

## Read the reasoning

**The single most valuable change made here.** These are thinking models, the
client captured `reply.reasoning`, and the loop threw it away — never logged,
never in the transcript, never shown.

That is not a cosmetic gap. With only the tool calls visible, a stuck run has to
be diagnosed by inference, and two fixes derived that way both made it worse:

| intervention | result |
|---|---|
| baseline | PASS, 15 turns |
| a "you already ran this" note each turn | FAIL at the 25-turn budget |
| spins count toward the no-progress stop | FAIL after 6 turns |

The note grew the context with near-identical text; the early stop killed the run
on a legitimate **re-read**, which is what a careful agent does and which no
behavioural test here can distinguish from spinning. Both reverted — spinning is
now counted and not corrected.

With the reasoning printed, the real cause was plain in a single run:

> **turns 3–10** — *"I need to grep for WORKFLOW_COLUMNS ... let me try grepping"*
> **turn 11** — *"I keep calling run_tests without doing anything."*
> **turn 12** — *"The middle part is omitted ... read_file doesn't support ranges."*

It was never looping stupidly. It was blocked on two missing capabilities and
said so, eight times, while using `run_tests` as a stall. Both now exist:

- **`search_files`** — regex search returning `path:line: text`. The only reason
  the tool cap moved from six to seven is that its absence was measured.
- **`read_file(start_line, end_line)`**, and a clip note that says how many lines
  the file has and how to ask for the omitted part.

The lesson generalises: **when an agent behaves oddly, read what it said it was
thinking before changing anything.** Behavioural inference produced two
regressions; one reading of the reasoning produced the fix.

## What the tests found

Written test-first throughout; these are the places a test disagreed with the
code and the code changed:

- **`find_the_bug` started green.** On an amount of 100, subtracting the percent
  and taking the percent give the same answer, so the buggy implementation
  satisfied both assertions. A benchmark task that is wrong is worse than none —
  it reports a model failure that is really a task failure.
- **`fix_return` was passed by `return 5`.** One assertion, so a hardcoded
  constant scored a point. Both found by the "obvious wrong answer" check, which
  now runs over every task.
- **`no_cheating` never pressured the refusal.** `return int(raw)` passed it,
  because `int('http')` raises `ValueError` on its own. A 1.5B model solved it in
  one edit while failing everything easier. It now enforces a port range.
- **The model was being fed ANSI escape sequences.** pytest colour codes were
  going into the context as pure noise tokens. Caught by the escapes turning up
  in the bench report.
- **Malformed tool arguments** must arrive as a `ToolCall` carrying an `error`,
  not an exception — otherwise the run dies on the failure it exists to measure.

## Next, in order

1. Harder tasks. 4/4 in four turns means the bench is at its ceiling and cannot
   show whether a change helps. Multi-file edits and tasks needing more than one
   round of `run_tests` would open the range.
2. Sweep the prompt and tool descriptions against the bench — it discriminates
   now, so it can score a change.
3. Then wire the loop into the pipeline harness below.

---

## Also on this branch: the pipeline harness

Built before the above, and more than was asked for. It is the machinery that
pulls tickets off a CodeArmory board, claims them race-safely across hosts, runs
a stage against each, and routes the result to the next column: `workflow.py`,
`tickets.py`, `platform.py`, `codearmory.py`, `dispatch.py`, `department.py`,
`admission.py`, `transcript.py`, `config.py`.

It is complete and tested, and it is not the starting point — the agent loop
above is. When the loop is good enough, a stage becomes a `Handler` (a role, a
`wants` predicate, an `async handle`) and drops into the dispatcher.

Where it deliberately differs from the Go, and why, is documented in the module
docstrings; the short list:

- the routing table is built per host, not a global map mutated at startup;
- stale-claim pruning is inside `attempts()`, not at five call sites;
- an admission slot is a context manager, not a callable that must be called once;
- a transcript is an object handed to the handler, not an ambient context value;
- the claim protocol is a function over a `TicketStore`, so it is tested without
  HTTP;
- the Go attempt ceiling is off by one — `maxAttempts=3` yields two attempts;
- a revived prerequisite goes to the queue its own history names, or nowhere.

The Go implementation is untouched on this branch as the reference.
