#!/usr/bin/env bash
# Provision the dedicated "Delivery" lifecycle board + its status columns on the
# CodeArmory tickets service. The agent-chain / build / pr-review / build-dev /
# integration pipelines reference this board's id to advance an epic ticket across
# the delivery lifecycle (todo -> plan -> triage -> in_progress -> in_review ->
# built -> testing -> done).
#
# The board id is a runtime resource; the pipelines hardcode it the same way they
# hardcode pipeline UUIDs. If you reprovision (fresh tickets DB), re-run this and
# update BOARD_ID in agent-chain.workflow.json (and the other lifecycle steps) to
# the new id it prints.
#
# The terminal column uses value "closed" (label "Done") because the tickets
# service's terminal detection is hardcoded to resolved/closed — a column named
# "done" would not be counted as finished in board tallies.
#
# Usage: CODEARMORY_TOKEN=<local-gatekeeper-jwt> ./provision-delivery-board.sh [project] [board-name]
set -uo pipefail
NS=codearmory
PROJECT="${1:-demo}"
BOARD_NAME="${2:-Delivery}"
TOK="${CODEARMORY_TOKEN:-}"
[ -n "$TOK" ] || { echo "ERROR: set CODEARMORY_TOKEN to a LOCAL gatekeeper JWT"; exit 2; }
TMP="$(mktemp -d)"
cleanup(){ [ -n "${PF:-}" ] && kill "$PF" 2>/dev/null; rm -rf "$TMP"; }
trap cleanup EXIT
kubectl -n "$NS" port-forward svc/codearmory-tickets 18086:8086 >/dev/null 2>&1 & PF=$!
sleep 3
T=http://127.0.0.1:18086
AUTH=(-H "Authorization: Bearer $TOK" -H 'content-type: application/json')

curl -s "${AUTH[@]}" "$T/boards" -o "$TMP/boards.json"
BID=$(python3 -c "
import json
d=json.load(open('$TMP/boards.json'))
items=d if isinstance(d,list) else (d.get('boards') or [])
print(next((b.get('board_id') for b in items if b.get('name')=='$BOARD_NAME' and b.get('project')=='$PROJECT'),''))
")
if [ -z "$BID" ]; then
  curl -s "${AUTH[@]}" -X POST "$T/boards" \
    -d "{\"name\":\"$BOARD_NAME\",\"description\":\"Delivery lifecycle board (auto-managed by the agent pipelines)\",\"project\":\"$PROJECT\",\"color\":\"#6366f1\"}" \
    -o "$TMP/new.json"
  BID=$(python3 -c "import json;print(json.load(open('$TMP/new.json')).get('board_id',''))")
  echo "created board $BOARD_NAME ($BID) in $PROJECT"
else
  echo "board $BOARD_NAME exists ($BID) in $PROJECT"
fi
[ -n "$BID" ] || { echo "FAILED"; cat "$TMP/new.json" 2>/dev/null; exit 1; }

# reset status columns to the lifecycle set
curl -s "${AUTH[@]}" "$T/field-defs?kind=status&board_id=$BID" -o "$TMP/fd.json"
python3 -c "
import json
d=json.load(open('$TMP/fd.json'))
items=d if isinstance(d,list) else (d.get('field_defs') or d.get('fieldDefs') or [])
print('\n'.join(f.get('field_def_id','') for f in items if f.get('field_def_id')))
" | while read -r fid; do
  [ -n "$fid" ] && curl -s -o /dev/null -X DELETE "${AUTH[@]}" "$T/field-defs/$fid"
done
col(){ curl -s -o /dev/null -w "  + $1 -> %{http_code}\n" "${AUTH[@]}" -X POST "$T/field-defs" \
  -d "{\"kind\":\"status\",\"value\":\"$1\",\"label\":\"$2\",\"position\":$3,\"board_id\":\"$BID\",\"color\":\"$4\"}"; }
col todo        "To Do"       0 "#94a3b8"
col plan        "Plan"        1 "#818cf8"
col triage      "Triage"      2 "#a78bfa"
col in_progress "In Progress" 3 "#38bdf8"
col in_review   "In Review"   4 "#fbbf24"
col built       "Built"       5 "#34d399"
col testing     "Testing"     6 "#22d3ee"
col closed      "Done"        7 "#10b981"
echo "BOARD_ID=$BID"
