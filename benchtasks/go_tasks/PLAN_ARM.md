# The plan arm: architect-plans-the-tests, then one developer

The second arrangement measured against `REQUEST.md`: a planning architect
writes PLAN.md — implementation AND tests, edge cases named case by case — and
a developer builds from it, tests included. Run with:

    go run ./cmd/simple -plan -repo ./scratch -task "$(cat benchtasks/go_tasks/REQUEST.md)"

**Complete: three runs of three**, all on one build (commit be798f9+, the
clean-checkdir harness), model qwen3.8:27b, 2026-08-30. Every number below was
verified independently on the written-back tree — build, vet, coverage, and a
live probe request — because one earlier "pass" turned out to be an artifact of
the check running on clone-plus-overlay.

## The three runs

|                     | run 1        | run 2        | run 3          |
|---------------------|--------------|--------------|----------------|
| wall clock          | 6m 24s       | 6m 25s       | **2m 41s**     |
| turns (arch + dev)  | 8 + 8        | 8 + 9        | 7 + 8          |
| impl lines          | 516          | 473          | 518            |
| test lines          | 995          | 699          | **0**          |
| statement coverage  | 91-92.5%     | 83.4%        | **0%**         |
| `go vet`            | clean        | clean        | clean          |
| routes answer       | yes          | yes          | yes            |
| create works        | yes          | yes          | only RFC3339 dates, error says "invalid JSON body" |
| layout              | store/ handler/ | flat      | model/ store/ server/ |

And against the single unrestricted agent (BASELINE.md; its three runs were on
the pre-review harness — none of the interim fixes would have changed its
outcomes, checked against its logs):

|                          | baseline (3 runs)   | plan arm (3 runs)  |
|--------------------------|---------------------|--------------------|
| wall clock               | 1m25s / 1m10s / 5m46s | 6m24s / 6m25s / 2m41s |
| test lines               | 112 / 0 / 544       | 995 / 699 / 0      |
| coverage                 | 40% / 0% / 90%      | 92% / 83% / 0%     |
| `go vet` findings        | 1 / 5 / 0           | 0 / 0 / 0          |
| **APIs that answer**     | **1 of 3**          | **3 of 3**         |
| main package present     | 3 of 3              | 3 of 3             |

## What the plan bought, on three runs' evidence

**Every plan-arm API answers requests, and every tree is vet-clean.** The
baseline shipped two dead APIs out of three — Go 1.22 route patterns under a
go 1.21 directive — and 1-5 vet findings in two of three trees. The plan arm
went three for three on both axes: two runs declared go 1.22 and used wildcard
routes legally, one used path-prefix routing that works everywhere. The
architect's plan names the module layout and the routes before the developer
touches them, and the difference shows exactly there.

**The median improved; the floor did not move.** Test lines 995/699/0 against
the baseline's 544/112/0: the plan more than doubles the middle of the
distribution, and run 3 still skipped tests WHOLESALE — a 200-line plan with
the cases named one by one, and the developer wrote none of them, finished in
2m41s, and passed, because `go test ./...` exits 0 on a tree with no tests.
A plan nobody enforces is advice. The floor belongs to the CHECK, and this was
known before the arm ran (see BASELINE.md's closing section); the arm confirms
it survives an explicit plan.

**What is tested is what works — six runs, no exceptions.** The one plan run
without tests is the one whose create path rejects plain dates with a message
("invalid JSON body") that names neither the field nor the format; runs 1 and
2, whose handler tests exercise the routes, both answer probes cleanly with
field-naming errors. Same correlation as the baseline's three.

**Cost:** ~1-2.5 minutes of architect ahead of the dev, and total wall clock of
2.5-6.5 minutes per run against the old department's 9-11 minutes for its full
six-stage pipeline on this host — while writing tests in-stage, which the old
pipeline's dev never did.

## What follows

The deferred check-policy change is now unblocked, and the arm's own data
argues for it: put `go vet` in the check (turns two baseline greens red for the
right reason, costs the plan arm nothing — it is already clean), and make a
tree with no test files fail the checked stages whose prompt demands tests
(turns run 3 red for the right reason). After that, the interesting experiment
is whether the plan arm keeps its 3-of-3 liveness with the floor enforced —
and whether run-3-shaped runs then spend their saved minutes writing the tests
they skipped.

## The harness history, honestly

Getting the first valid run took eleven attempts across two days, every failure
a harness defect, each fixed with a regression test in its own commit: sandbox
credential, stall detection, context truncation, LRU recency, checkless
completion (three variants), repeat signals on one tool only, whole-file
rewrite economics, Go syntax gates judging markdown, forge's execution body
cap, fenced replacements refused instead of repaired, cross-file duplicate
declarations, and the clone-overlay check. Runs 2 and 3 then completed first
try, back to back, in under 10 minutes combined — which is what the fixes were
for.

# The third arm: plan, tests-first, locked suite

Added after the two-stage comparison: a test author between the architect and
the developer, writing the tests RED with compiling stubs (expected-red gate,
plus a route gate refusing a suite that never touches the HTTP the tree
serves), and a developer whose tests are locked — the department's rule,
restored once its precondition existed — with whole-file rewrites freed in
exchange, an auto-check after every writing turn, and an 8-minute wall-clock
per dev attempt with two fresh-start respins that keep the tree and discard
the trail. Run with the same -plan flag; runs of 2026-08-30, one build.

|                     | run 1     | run 2     | run 3 (reran, 15m timeout) |
|---------------------|-----------|-----------|----------------------------|
| wall clock          | 7m 24s    | 8m 51s    | 10m 11s                    |
| outcome             | passed    | passed    | passed, dev in ONE attempt |
| test lines          | 949       | 1,258     | 677                        |
| coverage            | 89 / 96%  | 88.8%     | 80.2%                      |
| `go vet`            | clean     | clean     | clean                      |
| routes answer       | yes       | yes       | yes                        |
| handler tests       | 629 lines | 806 lines | present (httptest)         |

Run 3's FIRST seed, under the original 8-minute timeout, failed: three
attempts, ~23 minutes, the last tree one failing subtest from green across
1,521 lines. The carry was checked and was never broken — the killed attempts'
writes reached each fresh dev, proven by test and by the log — but each respin
paid ~4.5 minutes of re-orientation, over half an 8-minute budget, so three
short attempts lost to what one long one would have done. The timeout was
raised to fifteen minutes on the operator's calibration (a healthy dev is
5-6 minutes; the bound exists to stop the wedge, not to race the median) and
the rerun passed with the dev needing 4m40s of its window.

Run 3's final tree builds and fails ONE subtest — "expected 2 comments, got 3"
— across 1,521 lines: the third dev was minutes from green when its clock ran
out. The respin harness did its job either way: three bounded attempts, ~23
minutes, against the 80-minute unbounded grind the mechanism replaced. The
first wedge it was built for evaded every other bound at once — re-running the
check reset the idle counter, kept the temperature at base, and never tripped
the repeat notice because go test prints timings.

## The whole experiment, one table

|                      | lone agent ×3    | plan-only ×3      | plan+tests-first ×3 |
|----------------------|------------------|-------------------|---------------------|
| passed               | 3 (2 falsely)    | 3                 | 3                   |
| APIs that answer     | 1 of 3           | 3 of 3            | 3 of 3              |
| handler tests        | 1 of 3           | 2 of 3            | 3 of 3              |
| test lines           | 112 / 0 / 544    | 995 / 699 / 0     | 949 / 1,258 / 677   |
| coverage             | 40 / 0 / 90%     | 92 / 83 / 0%      | 89-96 / 89 / 80%    |
| wall clock           | 1-6m             | 2m41s-6m25s       | 7m24s-10m11s        |
| the failure mode     | dead routes,     | tests skipped     | time, once, under a |
|                      | reported green   | wholesale         | mis-set 8m timeout  |

The progression reads cleanly: each arrangement closed the previous one's
failure mode and exposed its own. The lone agent ships broken code as green;
the plan fixes liveness but cannot make anyone write tests; the gated test
author makes the tests exist and the locked suite makes them binding — at
which point the failure mode left is TIME, which is the honest one: run 3 did
not ship anything false, it ran out of clock one assertion short, visibly.

The floor moved where it was pushed, every time. What enforced it was never
the prompt — it was the gate: the route gate produced handler tests three for
three where the prompt alone went one for three; the locked suite produced
handler implementations where advice produced stubs. A plan nobody enforces is
advice, and the corollary held at every stage it was tested.

## Reliability: nine runs of the tests-first arm

Six more runs (tdd4-tdd9) on the same build and input, verified the same way,
to measure the arrangement's variance before building on it.

| run | outcome | wall clock | dev turns | test lines | coverage |
|-----|---------|-----------|-----------|------------|----------|
| 1   | pass    | 7m 24s    | 8         | 949        | 89-96%   |
| 2   | pass    | 8m 51s    | 9         | 1,258      | 88.8%    |
| 3   | pass    | 10m 11s   | 9         | 677        | 80.2%    |
| 4   | pass    | 9m 40s    | 4         | 1,028      | 93.6%    |
| 5   | pass    | 12m 14s   | 16        | 1,144      | 86.9%    |
| 6   | **FAIL**| ~45m bound| 3 timeouts| —          | —        |
| 7   | pass    | 7m 37s    | 5         | 966        | 98.6%    |
| 8   | pass    | 19m 30s   | 29        | 1,394      | 90.0%    |
| 9   | pass    | 11m 00s   | 10        | 1,510      | 87.8%    |

**Pass rate 8 of 9 (0.89).** Every pass: build clean, vet clean, ≥80%
coverage, handler tests present. Times 7m24s-19m30s; the dev is the whole
spread (1m28s to 13m27s), and run 8's 13m27s dev — 29 turns of honest work,
90 seconds inside its window — is what the 15-minute timeout exists to permit.

The one failure is diagnosed to the line: the test author demanded an
`{"tasks": [...]}` envelope its own helper could not decode from a bare
array, the developer returned bare arrays through eight rewrites of
`listTasks` and never made the leap — the fix was one wrapping line. The
accepted-but-futile rewrite loop lives inside the harness's definition of
progress (byte-changing writes reset the stall counter), which only the
wall-clock timeout can see; that design note stands open.

**The whole-run reroll follows from these numbers.** At a measured 1-in-9
failure, reverting to the original tree and redrawing (at most three
attempts, now in the harness) projects to (1/9)^3 ≈ 0.14% — three nines of
reliability for triple time on bad seeds only. The revert is surgical:
files the failed attempt created are removed, files it changed are restored,
nothing the harness did not produce is touched. Runs 4-9 above were measured
WITHOUT it, deliberately: they are the samples the arithmetic is built on.
