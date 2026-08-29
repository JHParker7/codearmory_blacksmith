# The smoke-test request

The smallest task that still exercises every stage. Deliberately pure logic —
no HTTP server, no database, no dependencies to resolve — so that a run which
fails tells you the HARNESS is broken rather than that the model found the work
hard. Use `benchtasks/go_ticketing/` when you want to measure quality instead.

Run it with:

```bash
go run ./cmd/simple -repo ./scratch -task "$(cat benchtasks/go_lru/REQUEST.md)"
go run ./cmd/simple -repo ./scratch -task "..." -roles spec,dev   # just the loop
```

`-roles spec,dev` is the short version: it is the pair that proves test-first
works, and it skips the two document stages that cost the most tokens for the
least signal about whether the machinery runs.

**Check where the check runs first.** This host sets

    AGENTS_REPO_TEST_COMMAND=go build ./... && go test ./...

which runs at the workspace ROOT, while `agents.Creator.Architect` tells the
architect to put the module in `src/`. Those disagree, and the way they fail is
not obvious: every stage with a check runs a command that finds no `go.mod`, so
the developer burns its whole budget on a tree that was never the problem. The
request below puts the module at the root to match the command as configured.
Reconcile them properly by setting `AGENTS_REPO_TEST_COMMAND` to
`cd src && go build ./... && go test ./...`, or by dropping `src` from the
architect's prompt — but do it deliberately, in one place, rather than per task.

---

## The request

Build an LRU cache in this repository, as one package. Put `go.mod` and the
package at the repository ROOT, not in a subdirectory.

`New(capacity int)` returns a cache holding at most `capacity` entries. Keys are
strings and values are integers.

- `Get(key)` returns the value and whether it was present, and counts as a use.
- `Put(key, value)` inserts or overwrites, and counts as a use.
- `Len()` is how many entries are held.

When a `Put` would exceed the capacity, the LEAST RECENTLY USED entry is
evicted — where "used" means read or written, so a `Get` rescues an entry that
was next in line. Overwriting an existing key updates it in place and does not
grow the cache.

Handle the edges as well as the happy path: a capacity of zero or less, a get on
an empty cache, a key that was evicted and then re-inserted, and a repeated
`Put` of the same key.
