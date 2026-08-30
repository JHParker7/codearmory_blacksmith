# The plan arm: architect-plans-the-tests, then one developer

The second arrangement measured against `REQUEST.md`: a planning architect
writes PLAN.md — implementation AND tests, edge cases named case by case — and
a developer builds from it, tests included. Run with:

    go run ./cmd/simple -plan -repo ./scratch -task "$(cat benchtasks/go_tasks/REQUEST.md)"

**Status: one valid run of three.** Runs 2 and 3 are still to be done, so every
comparison below is provisional. The single run is valid in a way the earlier
attempts were not: it is the first on the clean-checkdir harness, where the
check judges exactly the agent's tree (see the harness history below).

## Run 1 (2026-08-30, commit d2500c0+), against the baseline's three

|                        | plan run 1 | baseline best (r3) | baseline median |
|------------------------|------------|--------------------|-----------------|
| wall clock             | **6m 24s** | 5m 46s             | ~1m 30s         |
| turns (arch + dev)     | 8 + 8      | 19                 | 7               |
| impl lines             | 516        | 429                | ~430            |
| test lines             | **995**    | 544                | 112             |
| coverage               | 91-92.5%   | 89.7%              | 40.3%           |
| `go vet`               | clean      | clean              | 1-5 findings    |
| **builds AND runs**    | **yes**    | yes                | **no — 404s**   |
| layout                 | store/ + handler/ + main | flat     | flat            |

Verified independently on the written-back tree, not taken from the stage's own
check: `go build` links, vet is clean, coverage measured per package (handler
92.5%, store 91.0%), and a probe request answered — create 201, list 200, an
unknown id 404, and a malformed due date refused with a message naming the
expected format. The old pipeline's whole-run bar on this host was 9-11
minutes; this lands at 6.5 while writing its tests in-stage.

What the plan visibly bought, on one run's evidence:

- **995 test lines against the baseline median's 112**, written per the plan's
  own case list ("all 17 plan cases", says the dev's commit summary).
- **main.go written early and unprompted** — its absence was a baseline failure
  and an earlier plan-arm failure both.
- **Path-prefix routing with manual method dispatch** — the Go-1.21-safe style,
  sidestepping the version trap that broke two baseline runs. Coincidence or
  plan, undecidable from one run.
- Validation errors that name the field and the expected format.

What it cost: ~2 minutes of architect ahead of the dev, and the whole
arrangement only became runnable after the harness fixes below — none of which
are properties of the arrangement itself.

## The harness history, honestly

Getting ONE valid run took eleven attempts across two days. Every failure was a
harness defect, found by the run that died on it, fixed with a test, committed
separately. In order: sandbox credential (static token vs session), stall
detection missing, prompt context truncated (declarations invisible), LRU
recency, checkless stages unable to finish, empty trees passing, repeat-signal
on one tool only, whole-file rewrite economics, Go syntax gates judging
markdown, forge's body cap killing large trees, fenced replacements refused
instead of repaired, budget-exit inconsistency, cross-file duplicate
declarations invisible at the write, and the check running on clone+overlay
rather than the agent's tree — the last of which had turned one earlier "pass"
false, which is why independent re-verification outside the sandbox is part of
the method.

## Still to do

- Plan runs 2 and 3, same build, then the real three-vs-three comparison.
- The deferred check-policy change (vet in the check; a no-test tree is not a
  pass) — after the comparison, because it changes what "passed" means.
- The write_files-batch question: the old pipeline's speed came from few large
  generations; if runs 2-3 settle slower than the old bar, letting one call
  carry several files is the fix to try first.
