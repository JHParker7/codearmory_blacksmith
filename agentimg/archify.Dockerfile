# A minimal runner image: the golang base the sandbox already boots from, plus
# Node and the vendored archify CLI, so the architect's archify_diagram tool can
# render diagrams in the sandbox.
#
# WHY MINIMAL, not the full agentimg/Dockerfile: that image's scanner installs
# (gosec/govulncheck/staticcheck) no longer build — gosec's x/tools dependency
# fails to compile on this Go toolchain — and none of those tools are present in
# the sandbox image actually deployed (plain golang:1.25) anyway. So this is a
# clean superset of the running sandbox: golang:1.25 + node + git + archify, and
# nothing that can break the build.
FROM golang:1.25

RUN apt-get update -qq \
    && apt-get install -y -qq nodejs npm git >/dev/null \
    && rm -rf /var/lib/apt/lists/*

# archify: zero npm deps, pure JS, run by node. Vendored under agentimg/archify/.
COPY archify /opt/archify

# Fail the build rather than ship an image missing what archify_diagram needs.
RUN command -v node && command -v git && test -f /opt/archify/bin/archify.mjs
