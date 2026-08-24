#!/bin/sh
# Seed the "blacksmith" board with the work remaining until changes reach the
# testing branch. One-shot: creating it twice makes two boards.
#
# Requires the platform to be reachable (armory talks to conductor).
set -eu

board_id=$(armory tickets boards create \
  --name "blacksmith" \
  --description "Agent runtime: workflow columns, dependencies, intake, and dev->testing promotion" \
  --color "#8b7bb8" -v 2>&1 | grep -oE '[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}' | head -1)

if [ -z "$board_id" ]; then
  echo "could not read the new board's id; nothing else was created" >&2
  exit 1
fi
echo "board: $board_id"

add() {
  title="$1"; priority="$2"; description="$3"
  armory tickets create --board "$board_id" --title "$title" \
    --priority "$priority" --description "$description" >/dev/null
  echo "  + $title"
}

# ── Phase 1: make what is already built actually run ──────────────────────────

add "Deploy the tickets service carrying depends_on" high \
"The depends_on relation is committed on branch tickets-dependencies but the local plane still runs the old image, so no dependency edge can be written.

Build the tickets image, load it into the blacksmith minikube profile and restart the deployment. AutoMigrate creates ticket_dependencies on startup; no migration step.

Blocks every stage that gates on dependencies."

add "Call EnsureColumns at startup" high \
"CodeArmory.EnsureColumns provisions the board's twelve workflow columns and is written but never called.

Without it every stage polls a status the tickets service rejects, because status is validated against the board's own field defs. The department would come up and silently work nothing.

Wire it into runService, after config load and before the dispatchers start."

add "Make the TUI read columns instead of comment markers" high \
"tui.go stageOf() still pattern-matches comment markers to decide a ticket's stage. Routing moved to board columns, so the window now reports the whole board as untriaged.

Replace the marker matching with the ticket status, and drop the marker helpers it no longer needs."

add "Move the existing board off the retired open status" medium \
"Roughly ten tickets sit in 'open', which under column routing is nobody's queue: no stage takes from it, so they are stranded and invisible to the department.

Decide per ticket whether it belongs in inbox or is finished, and move them. The three demo tickets from testing can be closed."

add "Build and install blacksmith, restart the service" high \
"The systemd unit runs a binary predating the column refactor. Build from workflow-columns, install to ~/.local/bin/blacksmith and restart.

Do this LAST in phase 1: an old binary against a provisioned board is a department polling statuses that no longer route."

add "Run the column pipeline end to end for the first time" critical \
"Nothing has exercised column routing against the real plane. The unit suite passes, but every genuine bug found so far came from a real run, not from tests.

File one request, watch it through inbox -> scoping -> ready_for_dev -> in_dev -> ready_for_review -> in_review -> ready_for_integration -> done, and fix what surfaces.

This is the gate for starting phase 3."

# ── Phase 2: intake ───────────────────────────────────────────────────────────

add "Submit feature and fix requests from the TUI/CLI" high \
"There is no way to file a request into the inbox column short of curl.

Add a command that takes a title and body and opens a ticket in inbox on the department's board, so the product manager picks it up and splits it. Deliberately NOT an HTTP listener on blacksmith: it stays pull-only, with no inbound port."

# ── Phase 3: dev -> testing ───────────────────────────────────────────────────

add "Batch finished work for promotion to testing" medium \
"Promotion should fire on a time window or on a number of tickets waiting in done, whichever comes first, rather than per ticket.

Per-ticket promotion would mean a testing run per change, which is both slow and a worse signal: the interesting failures are combinations."

add "Agent review of the accumulated dev..testing diff" medium \
"An agent reviews the whole batch diff rather than a single branch, and its job is different from the per-branch reviewer: it is looking for interactions between changes that each passed on their own.

Depends on the batching trigger."

add "Merge dev into testing on approval" medium \
"On approval, merge and record what went in, so a testing failure can be traced back to the batch that caused it.

Depends on the batch review."

# ── Decisions ─────────────────────────────────────────────────────────────────

add "Decide what a blocking security review does" medium \
"The reviewer is advisory by design — a check that can block is one that can be prompt-injected into blocking — so a 'blocking' verdict currently still advances to integration and merges.

Options: leave it as is (consistent with the design), send the ticket back to ready_for_dev, or send it to blocked for a person. Worth a deliberate answer either way, because the current behaviour surprises people."

add "Close parent tickets when their children finish" low \
"A request that is broken down moves to tracking and stays there forever; nothing closes it when its children reach done.

Needs a sweep that moves a tracking parent to done once every child is done."

add "Prune the seeded default board columns" low \
"EnsureColumns is deliberately additive and never deletes, so the four seeded defaults (open, in_progress, resolved, closed) sit alongside the twelve workflow columns — sixteen columns on the board.

Remove the four by hand once nothing references them."

echo
echo "done. open it with: armory tickets board --board $board_id"
