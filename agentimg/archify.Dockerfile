# A minimal runner image for the architect's archify_diagram tool: just Node and
# the vendored archify CLI. The architect never builds Go in the sandbox (its only
# sandbox use is `node /opt/archify/bin/archify.mjs`), so a node base is both
# correct and far smaller than a golang+apt-node image.
#
# WHY node:22 (not golang:1.25 + apt nodejs): the cluster already caches node:22
# layers (scan-web and the frontend stages run on it), so the ONLY new layer a
# forge runner must pull is the tiny archify COPY below. The golang+apt-node
# variant added ~hundreds of MB of new layers whose first pull blew forge's 300s
# sandbox-readiness timeout ("lease sandbox did not become ready within 300s").
FROM node:22

# archify: zero npm deps, pure JS, run by node. Vendored under agentimg/archify/.
COPY archify /opt/archify

# Fail the build rather than ship an image missing what archify_diagram needs.
RUN command -v node && test -f /opt/archify/bin/archify.mjs
