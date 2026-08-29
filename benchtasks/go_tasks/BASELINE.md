# Baseline: one unrestricted agent, three runs

What ONE agent produces from `REQUEST.md` with none of the pipeline's
machinery — no separate specification author, no write guard, no reviewer. This
is what the six-stage pipeline has to beat to justify costing six times as much.

    go run ./cmd/simple -single -repo ./scratch -task "$(cat benchtasks/go_tasks/REQUEST.md)"

Model qwen3.8:27b on the `large` class, sandbox on the local forge, run
2026-08-29. Three runs, sequentially — the host serves one request at a time.
The agent had `AllowAll`, every tool, a 150-turn budget, and this check:

    go build ./... && go test ./...

## What came out

|                        | run 1        | run 2        | run 3     |
|------------------------|--------------|--------------|-----------|
| turns                  | 6            | 7            | 19        |
| check passed           | yes          | yes          | yes       |
| source lines           | 368          | 431          | 426       |
| test lines             | 112          | **0**        | 544       |
| test functions         | 9            | **0**        | 34        |
| statement coverage     | 40.3%        | **0.0%**     | 89.7%     |
| `go vet`               | 1 finding    | 5 findings   | clean     |
| **the API responds**   | **no — 404** | **no — 404** | yes       |
| logging calls          | 2            | 2            | 2         |
| layout                 | flat         | store/+handlers/ | flat  |

Every run reported success. Two of the three produce an API that answers 404 to
every request.

## The headline: a green check is not working software

Runs 1 and 2 register Go 1.22 method-and-wildcard routes —
`mux.HandleFunc("GET /tasks/{id}", ...)` — under a `go.mod` that declares
`go 1.21`. Those patterns are gated on the module's go directive, so under 1.21
they are LITERAL paths and match nothing. Measured, not inferred: a request to
each run's own mux comes back `404 page not found`.

`go build ./... && go test ./...` passes anyway. Nothing in it is a compile
error, and neither run has a test that sends a request. `go vet` catches it —
one finding in run 1, five in run 2 — and vet is not in the check.

Two lessons, and they are separate:

- **The check the agent gates on decides what it builds.** It will stop at the
  first thing that satisfies the check, so anything the check cannot see is
  optional. `go vet` belongs in it.
- **`go test ./...` exits 0 on a tree with no tests at all.** Run 2 passed its
  check having written none. "The check passed" and "this is tested" are
  unrelated statements, and only one of them is being measured.

## Readability — consistently good, and the least variable axis

All three are code you would accept in review. Doc comments on every exported
type and function, in the Go convention, starting with the name. Sensible
decomposition — run 2 split `store/` and `handlers/` unprompted. `Status` and
`Priority` are named string types with constants, never bare strings. JSON tags
throughout. Small helpers (`writeJSON`, `writeError`, `parseID`) rather than
repetition.

The blemishes are small:

- run 2 ends `handlers.go` with `var _ = errors.New // keep errors import if
  unused in future` — silencing an unused import instead of removing it;
- the runs disagree on the vocabulary the request left open: run 1 and 3 have
  `PriorityCritical`, run 2 has `PriorityUrgent`. Nothing is wrong with either.
  It is what an unstated requirement looks like downstream.

Readability is where the model needs the least help. None of the pipeline's
machinery is aimed at it.

## Testing — the widest variance, from nothing to thorough

0%, 40%, 90% from one prompt and one model. This is the axis worth building
machinery for, and the numbers say so.

Run 3 wrote 34 tests including 21 against the HTTP layer, and those API tests
are why it is the one run whose routes work — it could not have passed them
otherwise. Run 1 wrote 9, all against the store, and left `server.go` — 155
lines, the entire HTTP surface — untested; that is exactly the file with the
404 bug. Run 2 wrote none.

**What is tested is what works.** The correlation across three runs is perfect
and it is not a coincidence: the untested layer is the broken layer in every
case.

Where tests exist they are decent — defaults, empty title, invalid status and
priority, not-found, delete-twice. What is missing everywhere is concurrency:
all three ship a `sync.RWMutex` store and none tests it, so `go test -race`
proves nothing about the one thing the mutex is for.

## Error handling — good at the edge, thin in the core

The HTTP boundary is handled well and consistently: a JSON error body, 400 for
malformed input and a bad id, 404 for an unknown task, 201 on create, 204 on
delete. Run 1 uses a sentinel `ErrNotFound` with `errors.Is`, which is the right
shape.

The core is weaker, and two defects in run 1 were confirmed by running tests
against it rather than by reading:

1. **A rejected update corrupts the stored task.** `Store.Update` mutates the
   task in place and calls `Validate()` afterwards. An update carrying
   `status: "nonsense"` returns 400 to the caller and leaves that invalid status
   in the store. The one thing `Validate` exists to prevent is what happens.
2. **`Store.Get` hands back the pointer held in the map**, so any caller can
   rewrite the store without the mutex — a data race in a store that is
   otherwise careful with its `RWMutex`.

Run 3 has neither: it validates before mutating and returns `t.Clone()` from
`Get`, `List`, `Create` and `Update`. So the model can get this right; it does
not do so reliably.

Run 2 has no validation in the store at all — every check lives in the handlers,
so any non-HTTP caller writes what it likes.

## Logging — absent, unanimously

Two calls in every run, both in `main.go`: one startup line and `log.Fatal`.
Zero request logging, zero error logging, no levels, no `slog`, no request id.
An operator running any of these has no way to answer "what happened" beyond the
status code the caller already saw.

This is the most consistent result in the whole exercise — three for three,
nobody close. Unlike the test coverage there is no variance to exploit: it is
not that the model sometimes forgets, it is that nothing asked. Logging will
only appear if the request or a stage's instruction asks for it.

## What this predicts about the pipeline

The baseline is strong on readability, unreliable on testing, and unobservable
in production. So:

- The **specification author** is aimed at the one axis with 90-point variance,
  and takes it out of the developer's hands. Run 2 could not have happened.
- The **write guard** on tests matters because run 3 shows the model will write
  tests that pass rather than tests that bind, if it owns both sides.
- The **reviewer** is aimed at exactly the defects that reading finds and the
  check does not — the rejected-update corruption is a review finding, not a
  test failure.
- Nothing in the pipeline currently addresses **logging**, and the baseline says
  nothing will unless something asks.

## Fix the check before comparing

The pipeline stages currently gate on the same command as this baseline, so they
inherit the same blind spot. Before the comparison means anything:

    go vet ./... && go build ./... && go test ./...

That alone turns two of these three green runs red, which is the correct answer.
