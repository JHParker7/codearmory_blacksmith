#!/usr/bin/env bash
# Deploy the pipeline batch (PR-comment timeline, semver, promote, build-dev) to the
# LOCAL codearmory minikube. Idempotent: existing pipelines are UPDATED in place by
# name->id (PUT /pipelines/{id}), so their ids — and every trigger that references
# them — stay valid. New pipelines (build-dev, promote) are created. The build-dev
# merge trigger is registered if missing.
#
# Needs a LOCAL gatekeeper token (the env CODEARMORY_TOKEN targets the DEAD exp
# cluster and will NOT work). Mint one against the local gatekeeper:
#   TOK=$(curl -s -X POST http://192.168.67.2:32612/login -H 'Content-Type: application/json' \
#           -d '{"email":"<you>","password":"<pw>"}' | python3 -c 'import sys,json;print(json.load(sys.stdin)["token"])')
#   CODEARMORY_TOKEN="$TOK" ./deploy/codearmory/deploy-batch.sh
#
# Every current pipeline is backed up under /tmp/prbatch-backup-<ts>/ before a PUT.
set -euo pipefail
DIR="$(cd "$(dirname "$0")" && pwd)"
NS=codearmory
TOK="${CODEARMORY_TOKEN:-${1:-}}"
[ -n "$TOK" ] || { echo "ERROR: set CODEARMORY_TOKEN to a LOCAL gatekeeper JWT (see header)"; exit 2; }
BK="/tmp/prbatch-backup-$(date +%Y%m%d-%H%M%S)"; mkdir -p "$BK"

cleanup() { [ -n "${WF_PID:-}" ] && kill "$WF_PID" 2>/dev/null || true; [ -n "${EV_PID:-}" ] && kill "$EV_PID" 2>/dev/null || true; }
trap cleanup EXIT
kubectl -n "$NS" port-forward svc/codearmory-workflows 18085:8085 >/dev/null 2>&1 & WF_PID=$!
kubectl -n "$NS" port-forward svc/codearmory-events    18093:8093 >/dev/null 2>&1 & EV_PID=$!
sleep 3
WF=http://127.0.0.1:18085; EV=http://127.0.0.1:18093
AUTH=(-H "Authorization: Bearer $TOK" -H 'Content-Type: application/json')

# name of the pipeline declared inside a workflow json file
pname() { python3 -c 'import sys,json;print(json.load(open(sys.argv[1]))["name"])' "$1"; }
# id of a live pipeline by its name (GET /pipelines), empty if absent
pid_of() { curl -s "${AUTH[@]}" "$WF/pipelines" | python3 -c 'import sys,json;n=sys.argv[1]
d=json.load(sys.stdin); items=d if isinstance(d,list) else (d.get("pipelines") or d.get("workflows") or d.get("items") or [])
for p in items:
    if p.get("name")==n: print(p.get("workflow_id") or p.get("id") or p.get("pipeline_id") or ""); break' "$1"; }

upsert() { # <file>  -> PUT if exists (backing up), else POST
  local f="$1" name id code
  name="$(pname "$f")"; id="$(pid_of "$name")"
  if [ -n "$id" ]; then
    curl -s "${AUTH[@]}" "$WF/pipelines/$id" > "$BK/$name.json" || true
    code=$(curl -s -o /tmp/pb.out -w '%{http_code}' -X PUT "${AUTH[@]}" "$WF/pipelines/$id" --data-binary @"$f")
    cp /tmp/pb.out "$BK/$name.response.txt" 2>/dev/null || true
    echo "  update $name ($id) -> $code"
    case "$code" in 2*) ;; *) echo "    body: $(head -c 300 /tmp/pb.out)";; esac
  else
    id=$(curl -s -X POST "${AUTH[@]}" "$WF/pipelines" --data-binary @"$f" | python3 -c 'import sys,json;d=json.load(sys.stdin);print(d.get("workflow_id") or d.get("id") or d.get("pipeline_id") or "")')
    code=201; echo "  create $name -> id=$id"
  fi
  [ -n "$id" ] || { echo "  !! $name got no id"; return 1; }
  PIDMAP_last="$id"
}

echo "== updating existing pipelines (in place) =="
for f in pr-review fix-arm-c fix-arm-web plan-arm devops; do upsert "$DIR/$f.workflow.json"; done

echo "== create/update build-dev + promote =="
upsert "$DIR/build-dev.workflow.json"; BUILD_DEV_ID="$PIDMAP_last"
upsert "$DIR/promote.workflow.json"

echo "== ensure build-dev-on-merge trigger =="
HAVE=$(curl -s "${AUTH[@]}" "$EV/triggers" | python3 -c 'import sys,json
d=json.load(sys.stdin); items=d if isinstance(d,list) else (d.get("triggers") or d.get("items") or [])
print("yes" if any(t.get("name")=="build-dev-on-merge" for t in items) else "no")' 2>/dev/null || echo no)
if [ "$HAVE" = no ]; then
  python3 - "$DIR/build-dev.trigger.json" "$BUILD_DEV_ID" > /tmp/bd.trigger.json <<'PY'
import sys, json
s=json.load(open(sys.argv[1])); s["actions"][0]["config"]["pipeline_id"]=sys.argv[2]; json.dump(s,sys.stdout)
PY
  code=$(curl -s -o /tmp/tr.out -w '%{http_code}' -X POST "${AUTH[@]}" "$EV/triggers" --data-binary @/tmp/bd.trigger.json)
  echo "  create build-dev-on-merge (pipeline $BUILD_DEV_ID) -> $code"
else
  echo "  build-dev-on-merge already registered (leaving as-is)"
fi

echo "== done. backups in $BK =="
