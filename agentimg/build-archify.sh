#!/usr/bin/env bash
# Build the archify sandbox image and make it available to forge on the LOCAL
# minikube: push it to the in-cluster registry AND load it into the node's
# containerd (belt and suspenders — forge pulls from the registry, the containerd
# copy avoids a cold pull that can blow forge's 300s sandbox-readiness timeout).
#
# The architect's archify_diagram tool runs `node /opt/archify/bin/archify.mjs` in
# its sandbox, so the agent-plan architect step must run THIS image (see
# deploy/codearmory/agent-plan.workflow.json) and it must be in forge ALLOWED_IMAGES
# (infra/helm/codearmory values / the forge deployment env).
#
#   ./agentimg/build-archify.sh            # build + push + load :1
#   TAG=2 ./agentimg/build-archify.sh      # a new tag
set -euo pipefail
DIR="$(cd "$(dirname "$0")" && pwd)"
NS="${NS:-codearmory}"
TAG="${TAG:-1}"
REGHOST=codearmory-registry-mirror:5000
IMG="$REGHOST/agent-tools/archify:$TAG"

echo "== build $IMG (FROM node:22 — small new layer so the first sandbox pull is fast) =="
docker build -f "$DIR/archify.Dockerfile" -t "localhost:5000/agent-tools/archify:$TAG" "$DIR"
docker tag "localhost:5000/agent-tools/archify:$TAG" "$IMG"

echo "== push to the in-cluster registry (via port-forward) =="
kubectl -n "$NS" port-forward svc/codearmory-registry-mirror 5000:5000 >/dev/null 2>&1 & PF=$!
trap '[ -n "${PF:-}" ] && kill "$PF" 2>/dev/null || true' EXIT
sleep 3
docker push "localhost:5000/agent-tools/archify:$TAG"

echo "== load into the node containerd under the registry name =="
minikube -p "$NS" image load "$IMG"

echo "== done: $IMG"
echo "   ensure it is in forge ALLOWED_IMAGES and on the agent-plan architect step."
