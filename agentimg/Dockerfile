# The runner image every sandbox boots from.
#
# IT WAS BUILT BY HAND AND THE RECIPE LIVED ONLY IN PROSE. docs/DEPLOYMENT.md
# described a two-line Dockerfile; the image actually running carried the qwen
# CLI as well, installed by npm and recorded nowhere. Rebuilding from the
# documented version would have dropped it silently and broken the delegate the
# moment anyone turned AGENTS_DEV_ENGINE back on. This file is the whole of what
# the image contains, so the next rebuild is a rebuild and not a guess.
#
# Build it into the CLUSTER's image store, not the host's — minikube runs
# containerd, so `eval $(minikube docker-env)` does not apply:
#
#   minikube -p blacksmith image build -t agent-tools:local -f agentimg/Dockerfile agentimg/
#
# The tag must also appear in forge's ALLOWED_IMAGES (deploy/k8s/forge-local.yaml)
# or every sandbox is refused with a bare "400: image not allowed".
FROM golang:1.25

RUN apt-get update -qq \
    && apt-get install -y -qq nodejs npm git >/dev/null \
    && rm -rf /var/lib/apt/lists/*

# GOIMPORTS, AND IT IS NOT A CONVENIENCE. AGENTS_REPO_FORMAT_COMMAND asks for it
# on every push and the image did not have it, so the chain fell through to
# `go run ...goimports@v0.28.0` — which has to fetch and compile the tool through
# the sandbox proxy on every single push — and then to plain gofmt, which formats
# but never adds an import. The degradation was silent: formatScript only warns
# when the WHOLE chain fails, and gofmt always succeeds.
#
# What that cost, measured on r82: a specification author wrote a test calling
# url.QueryEscape without importing net/url. goimports would have added the
# import at the moment it was pushed. Instead the task went to a developer that
# may not edit tests, could not resolve a package qualifier by any legal move,
# and spent its entire budget rewriting the same file.
#
# It also cannot weaken a test, which is why it is the right tool for this and a
# model reviewing test edits is not: it rewrites the import block and leaves
# every assertion untouched.
#
# Pinned to the version the format command falls back to, so the tool that runs
# is the tool that was asked for.
RUN go install golang.org/x/tools/cmd/goimports@v0.28.0

# The external coding agent the developer stage can be delegated to, when
# AGENTS_DEV_ENGINE=qwen-code. Pinned to the version the working image carried.
RUN npm install -g --silent @qwen-code/qwen-code@0.21.13

# ARCHIFY renders the architect's typed JSON IR into a self-contained diagram
# (HTML, pure JS — no headless browser). The archify_diagram tool runs
# `node /opt/archify/bin/archify.mjs deliver <type> ir.json out.html` in the
# sandbox, so the architect emits an IR and the diagram is compiled
# deterministically rather than hand-drawn as ASCII.
#
# VENDORED, not cloned at build time: archify has ZERO npm dependencies (pure JS,
# node >=18, which is already installed above), so the whole tool is just its
# source. It is copied from agentimg/archify/ (the tt-a1i/archify repo's inner
# archify/ code dir) rather than git-cloned — a clone in the build sandbox failed
# with no network, and baking it in matches this image's rule that nothing the
# pipeline needs is fetched per run.
COPY archify /opt/archify

# THE SECURITY SCANNERS, pinned like everything else. gosec is the SAST pass
# and govulncheck the SCA pass; both run in the sandbox between the developer
# and the security reviewer, and their output lands under scan/ as LEADS the
# reviewer verifies. Installed here rather than fetched per run, because the
# goimports lesson above is exactly what `go run tool@latest` on every scan
# would repeat.
RUN go install github.com/securego/gosec/v2/cmd/gosec@v2.21.4
RUN go install golang.org/x/vuln/cmd/govulncheck@v1.1.3

# staticcheck is the LINT pass, feeding the CODE reviewer the way gosec and
# govulncheck feed the security one. Advisory by the same rule the department
# wrote on LintCommand: a linter can produce findings a change cannot resolve,
# and behind a gate each of those traps the agent.
RUN go install honnef.co/go/tools/cmd/staticcheck@2025.1

# Fail the build rather than ship an image missing what the pipeline asks for.
# The whole reason this file exists is that a missing tool degraded quietly.
RUN command -v goimports && command -v qwen && command -v git && command -v node \
    && command -v gosec && command -v govulncheck && command -v staticcheck \
    && test -f /opt/archify/bin/archify.mjs
