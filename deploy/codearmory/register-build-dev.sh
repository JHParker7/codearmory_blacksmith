#!/usr/bin/env bash
# Register the merge-to-dev alpha build: the `build-dev` pipeline (workflows) and
# the `build-dev-on-merge` trigger (events) on the LOCAL codearmory minikube.
#
# Requires a LOCAL gatekeeper bearer token — this deployment's gatekeeper, NOT the
# (dead) exp.codearmory.app one. Provide it via env CODEARMORY_TOKEN or as $1.
# Both services are ClusterIP, so this port-forwards them for the duration.
#
#   CODEARMORY_TOKEN=<local-jwt> ./register-build-dev.sh
#
# Idempotency: creating a second pipeline/trigger with the same name may duplicate.
# Check `GET /pipelines` and `GET /triggers` first if re-running.
set -euo pipefail
DIR="$(cd "$(dirname "$0")" && pwd)"
NS=codearmory
TOK="${CODEARMORY_TOKEN:-${1:-}}"
[ -n "$TOK" ] || { echo "ERROR: set CODEARMORY_TOKEN (a LOCAL gatekeeper token) or pass it as \$1"; exit 2; }

cleanup() { [ -n "${WF_PID:-}" ] && kill "$WF_PID" 2>/dev/null || true; [ -n "${EV_PID:-}" ] && kill "$EV_PID" 2>/dev/null || true; }
trap cleanup EXIT

kubectl -n "$NS" port-forward svc/codearmory-workflows 18085:8085 >/dev/null 2>&1 & WF_PID=$!
kubectl -n "$NS" port-forward svc/codearmory-events    18093:8093 >/dev/null 2>&1 & EV_PID=$!
sleep 3

echo "== creating build-dev pipeline =="
PID=$(curl -s -X POST http://127.0.0.1:18085/pipelines \
  -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
  --data-binary @"$DIR/build-dev.workflow.json" \
  | python3 -c 'import sys,json; d=json.load(sys.stdin); print(d.get("id") or d.get("pipeline_id") or "")')
[ -n "$PID" ] || { echo "ERROR: pipeline create returned no id (token/permissions?)"; exit 3; }
echo "   pipeline_id=$PID"

echo "== creating build-dev-on-merge trigger =="
python3 - "$DIR/build-dev.trigger.json" "$PID" > /tmp/build-dev.trigger.filled.json <<'PY'
import sys, json
spec = json.load(open(sys.argv[1])); pid = sys.argv[2]
spec["actions"][0]["config"]["pipeline_id"] = pid
json.dump(spec, sys.stdout)
PY
curl -s -X POST http://127.0.0.1:18093/triggers \
  -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
  --data-binary @/tmp/build-dev.trigger.filled.json | python3 -m json.tool || true

echo "== done. A PR merged to 'dev' now builds registry-local.../<repo>:v<semver>a =="
