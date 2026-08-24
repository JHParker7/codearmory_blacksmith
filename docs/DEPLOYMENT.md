# Deploying blacksmith

How to stand up blacksmith and everything it depends on, on a fresh workstation.

Written from a working deployment on `osiris`. Every path, port and service name
below was read out of the live config rather than remembered — where something is
site-specific it is marked **[site]** and you will need your own value.

## What you are deploying

blacksmith is a **client** of CodeArmory, not a service inside it. It runs on the
workstation next to the GPU, pulls tickets through the platform, and executes agent
work in sandboxes it rents from forge. Three consequences shape the whole setup:

- Work is **pulled**. Nothing calls in, so no ingress, no TLS, no registry entry.
- **Sandboxes are forge executions.** blacksmith never runs agent code itself.
- The **model lives on the host**, and the sandbox reaches it over the network —
  which is where most first-time failures happen (§7).

Four layers, in dependency order:

```
1. model server      llama-server on the host, bound to the cluster bridge
2. cluster           minikube profile `blacksmith` running forge + friends
3. runner image      agent-tools:local — the image sandboxes boot from
4. blacksmith        systemd --user service on the host
```

## Prerequisites

| | |
|---|---|
| GPU | enough VRAM for a 27–30B model plus KV cache (32 GiB comfortable) |
| Docker | for building images |
| minikube | with the docker driver |
| Go | 1.25+ to build blacksmith |
| kubectl | matching the cluster |
| ufw | if you run a host firewall — see §7 |

---

## 1. Model server

blacksmith talks to any OpenAI-compatible endpoint. This deployment uses
llama.cpp, installed as a **prebuilt release tarball** (not a git checkout):

```bash
cd /tmp
curl -LO https://github.com/ggml-org/llama.cpp/releases/download/b10516/llama-b10516-bin-ubuntu-vulkan-x64.tar.gz
mkdir -p ~/.local/opt/llama.cpp
tar xzf llama-b10516-bin-ubuntu-vulkan-x64.tar.gz -C ~/.local/opt/llama.cpp --strip-components=1
~/.local/opt/llama.cpp/llama-server --version
```

Take the build matching your backend — `vulkan` for AMD, or the CUDA variant for
NVIDIA. The plain `ubuntu-x64` build is **CPU only** and will silently give you no
GPU at all. "ubuntu" in the filename is the build environment, not a requirement;
these binaries run fine on Arch.

**Use a current build.** Model architectures land in llama.cpp continuously, and an
older build fails to load newer GGUFs with errors like
`key qwen35.rope.dimension_sections has wrong array length`. That is a stale build,
not a corrupt model.

Create an API key and a systemd user unit:

```bash
mkdir -p ~/.config/codearmory-agents
openssl rand -hex 32 > ~/.config/codearmory-agents/llama-large.key
chmod 600 ~/.config/codearmory-agents/llama-large.key
```

`~/.config/systemd/user/llama-large.service`:

```ini
[Unit]
Description=llama.cpp server (large class)
After=network-online.target

[Service]
Type=exec
ExecStart=/bin/sh -c 'exec %h/.local/opt/llama.cpp/llama-server \
  --model %h/.local/share/llama-models/<your-model>.gguf \
  --alias <model-name-clients-will-send> \
  --host 192.168.58.1 \
  --port 8080 \
  --n-gpu-layers 999 \
  --ctx-size 65536 \
  --parallel 1 \
  --flash-attn on \
  --jinja \
  --api-key-file %h/.config/codearmory-agents/llama-large.key \
  --no-webui'
Restart=on-failure

[Install]
WantedBy=default.target
```

Then `systemctl --user daemon-reload && systemctl --user enable --now llama-large`.

**`--host 192.168.58.1` is load-bearing.** That is the minikube bridge address on
the host. Bound to loopback, the host can reach the model and nothing in the
cluster can — agents then sit at 0% GPU looking like they have hung. **[site]**
confirm your bridge IP with `ip -4 addr show | grep 192.168`.

`--alias` sets the model name the API reports, and clients must send exactly that
name. If you change the model file, change the alias and every client's model
setting together.

Verify:

```bash
curl -sf http://192.168.58.1:8080/health && echo OK
curl -sf http://192.168.58.1:8080/v1/models \
  -H "Authorization: Bearer $(cat ~/.config/codearmory-agents/llama-large.key)"
```

### Sizing context and slots

`--parallel` divides one fixed KV budget: two slots of 32,768 cost the same memory
as one slot of 65,536. **Asking for more slots without lowering context does not
error — it silently gives you fewer slots.** The only place this shows is the
startup log:

```bash
journalctl --user -u llama-large | grep -oE "n_slots = [0-9]+, n_ctx_slot = [0-9]+"
```

If that says `slots = 1` when you asked for four, your context is too large for the
slot count you wanted. `AGENTS_LARGE_SLOTS` in §4 **must match** `--parallel`.

---

## 2. Cluster and CodeArmory services

```bash
minikube start -p blacksmith
kubectl --context blacksmith create namespace blacksmith
```

Create the secrets from the template, then apply the manifests:

```bash
cd ~/projects/blacksmith
cp deploy/k8s/secret.example.yaml /tmp/secret.yaml
# edit /tmp/secret.yaml — set your own passwords
kubectl --context blacksmith -n blacksmith apply -f /tmp/secret.yaml
kubectl --context blacksmith -n blacksmith apply -f deploy/k8s/forge-local.yaml
kubectl --context blacksmith -n blacksmith apply -f deploy/k8s/egress.yaml
```

What that gives you:

| component | role | NodePort |
|---|---|---|
| `forge-local` | sandbox execution — leases, executions | **30083** |
| `gatekeeper-local` | auth and RBAC | **30081** |
| `gatekeeper-redis` | permission cache | — |
| `tickets-local` | the board blacksmith pulls from | **30086** |
| `git-local` | git daemon serving the repos under test | 9418 (in-cluster) |
| `forge-postgres` | forge's state (StatefulSet, PVC) | — |
| `egress-proxy` | the sandbox's only route out | 3128 (in-cluster) |

forge runs with `RUNTIME=kubernetes` and `K8S_NAMESPACE=blacksmith`, so it creates
sandbox pods in the same namespace.

Wait for everything and note the node IP:

```bash
kubectl --context blacksmith -n blacksmith wait --for=condition=Available deploy --all --timeout=300s
minikube ip -p blacksmith     # expect 192.168.58.2 — used in the env file below
```

### The egress policy

`deploy/k8s/egress.yaml` installs a NetworkPolicy (`forge-lease-egress`) that
selects every sandbox pod and permits **only**: DNS, the model port on the host
bridge, git-local, and the proxy. Everything else is dropped.

This is deliberate — a sandbox runs model-authored code — but it means **adding any
new host service requires editing the policy**. Putting a second model on another
port and forgetting this produces an agent that cannot reach it, with no error
beyond a hang. To add a port:

```bash
kubectl --context blacksmith -n blacksmith patch networkpolicy forge-lease-egress \
  --type=json -p '[{"op":"add","path":"/spec/egress/1/ports/-",
                    "value":{"port":11434,"protocol":"TCP"}}]'
```

---

## 3. Runner image

Sandboxes boot from an image that must carry **git, Go and Node together** — git to
clone, Go to build the project under test, Node for the coding agent. No stock
image has all three.

**The recipe is `agentimg/Dockerfile`, and it is the authority.** This section
used to inline a two-line Dockerfile instead, and it was wrong: the image running
in the cluster also carried the qwen CLI, installed by npm and recorded nowhere.
Anyone rebuilding from the version documented here would have dropped it silently
and broken the delegate the moment `AGENTS_DEV_ENGINE=qwen-code` was set again.

What the image carries, and why:

| | |
|---|---|
| Go, git, Node | clone, build, and the external coding agent |
| `goimports` | `AGENTS_REPO_FORMAT_COMMAND` asks for it on every push |
| `qwen` | the delegate, when `AGENTS_DEV_ENGINE=qwen-code` |

`goimports` is the one that bites if it is missing, because nothing says so. The
format command falls through `goimports` → `go run ...goimports@v0.28.0` → `gofmt`,
and `gofmt` formats without ever adding an import, so the chain succeeds while
doing less than it was asked. Measured on r82: a specification author wrote a test
calling `url.QueryEscape` without importing `net/url`, and the task went to a
developer that may not edit tests, had no legal move, and spent its whole budget
rewriting one file. The Dockerfile now ends with a `command -v` check so a missing
tool fails the build rather than degrading quietly.

Build it **into the cluster's** image store, not the host's:

```bash
docker build -t agent-tools:local agentimg/
minikube -p blacksmith image load agent-tools:local
```

**Do not use `minikube image build` on this cluster.** It exits 0, prints nothing
and writes no image — the only way to notice is that the digest in
`ctr -n k8s.io images ls` has not changed. `eval $(minikube docker-env)` does not
work either, because minikube runs containerd rather than docker. Build on the
host and load, as above.

Check the digest changed after loading:

```bash
minikube -p blacksmith ssh -- 'sudo ctr -n k8s.io images ls | grep agent-tools'
```

**The image must be listed in forge's `ALLOWED_IMAGES`** (see the env block in
`deploy/k8s/forge-local.yaml`). Miss it and every sandbox is refused with a bare
`400: image not allowed`.

Two things the image cannot fix, which your agent command must set itself:

- `sh -lc` rebuilds `PATH` from `/etc/profile`, so Go is "not found" despite being
  installed. Export `PATH=/usr/local/go/bin:$PATH`.
- `HOME` defaults to `/` and is not writable. Set `HOME=/tmp` and point
  `GOCACHE`, `GOMODCACHE` and `npm_config_prefix` under `/tmp`.

---

## 4. blacksmith

```bash
cd ~/projects/blacksmith
make test          # 600+ subtests; do not skip
make build
mv blacksmith ~/.local/bin/blacksmith
```

Use `mv`, not `cp`. A running blacksmith TUI holds the binary open and `cp` fails
with `Text file busy`; `mv` replaces the directory entry atomically and leaves the
running process alone.

### Configuration

`~/.config/codearmory-agents/env` — read by the systemd unit as an
`EnvironmentFile`. **Comments must be on their own lines.** systemd does not strip
trailing comments, so `FOO=2 # note` sets the literal value `2 # note`, and
blacksmith crash-loops with no useful message.

```bash
# --- model ---
AGENTS_LARGE_ENDPOINT=http://192.168.58.1:8080/v1
AGENTS_LARGE_MODEL=<must equal llama-server --alias>
AGENTS_LARGE_SLOTS=1
AGENTS_LARGE_API_KEY_FILE=/home/<you>/.config/codearmory-agents/llama-large.key
AGENTS_LARGE_TEMPERATURE=0
AGENTS_LARGE_TOOLS=true

# --- platform (NodePorts from §2; 192.168.58.2 is the minikube node IP) ---
AGENTS_FORGE_URL=http://192.168.58.2:30083
AGENTS_FORGE_GATEKEEPER_URL=http://192.168.58.2:30081
AGENTS_TICKETS_URL=http://192.168.58.2:30086
AGENTS_FORGE_EMAIL=admin@blacksmith.invalid
AGENTS_FORGE_PASSWORD=<your value>
AGENTS_BOARD_ID=<board uuid the department works>

# --- the repo under test ---
AGENTS_REPO_URL=git://git-local:9418/demo.git
AGENTS_REPO_BRANCH=dev
AGENTS_REPO_IMAGE=agent-tools:local
AGENTS_REPO_TEST_COMMAND=go test ./...
AGENTS_REPO_LINT_COMMAND=go vet ./...
AGENTS_REPO_TIMEOUT_SECONDS=1650

# --- runtime ---
AGENTS_HOST=<hostname>
AGENTS_SANDBOX_PROXY=http://egress-proxy:3128
AGENTS_TRANSCRIPT_DIR=/home/<you>/.local/share/codearmory-agents/transcripts
AGENTS_DEV_ENGINE=
```

`AGENTS_DEV_ENGINE` empty runs blacksmith's built-in developer loop. Set it to
`qwen-code` to delegate the developer's inner loop to an external coding agent
inside the sandbox instead.

The full variable list is in `classes.go` and `settings.go` — everything above is
the minimum to start.

### The service

`~/.config/systemd/user/blacksmith.service`:

```ini
[Unit]
Description=blacksmith — CodeArmory agent runtime
After=network-online.target llama-large.service
Wants=llama-large.service

[Service]
Type=exec
ExecStart=%h/.local/bin/blacksmith service
EnvironmentFile=%h/.config/codearmory-agents/env
Restart=on-failure
RestartSec=10
KillSignal=SIGINT
TimeoutStopSec=120

[Install]
WantedBy=default.target
```

```bash
systemctl --user daemon-reload
systemctl --user enable --now blacksmith
blacksmith          # the TUI, in a separate terminal
```

---

## 5. Verification

Work up the stack; each step depends on the one before.

```bash
# 1. model answers, and reports the alias clients must send
curl -sf http://192.168.58.1:8080/v1/models \
  -H "Authorization: Bearer $(cat ~/.config/codearmory-agents/llama-large.key)"

# 2. slot geometry is what you asked for
journalctl --user -u llama-large | grep -oE "n_slots = [0-9]+, n_ctx_slot = [0-9]+" | tail -1

# 3. cluster is up
kubectl --context blacksmith -n blacksmith get pods

# 4. the platform answers on its NodePorts
curl -sf http://192.168.58.2:30081/health && curl -sf http://192.168.58.2:30083/health

# 5. blacksmith is running and not crash-looping
systemctl --user status blacksmith --no-pager | head -5

# 6. it is doing something — records appear as agents work
ls -la ~/.local/share/codearmory-agents/transcripts/
```

The real smoke test is a sandbox reaching the model. Put a ticket on the board and
watch for a `forge-lease-*` pod plus GPU activity:

```bash
kubectl --context blacksmith -n blacksmith get pods | grep forge-lease
journalctl --user -u llama-large --since '2 min ago' | grep -c "eval time"
```

Zero decode events with a lease pod running means the sandbox cannot reach the
model — go to §7.

---

## 6. Updating a service image

Change the manifest **and** the live deployment. Editing only the deployment works
until the next `apply` silently reverts it:

```bash
minikube -p blacksmith image build -t ghcr.io/code-armory-app/forge:mytag -f forge/Dockerfile src/systems
sed -i 's|forge:oldtag|forge:mytag|' deploy/k8s/forge-local.yaml
kubectl --context blacksmith -n blacksmith set image deploy/forge-local forge=ghcr.io/code-armory-app/forge:mytag
kubectl --context blacksmith -n blacksmith rollout status deploy/forge-local
```

---

## 7. Traps

Each of these cost real debugging time. They fail quietly, which is why they are
listed.

**`kubectl apply` of `forge-local.yaml` resets the live deployment** to whatever
the manifest says. It has reverted a forge image and wiped runtime config before.
Change the manifest, not just the deployment.

**forge caps lease timeouts** — idle 300s, max lifetime 3600s — whatever you ask
for, and reports execution status as **`completed`**, never `succeeded`. Polling
for the wrong word spins until the lease is reaped and the next call 409s.

**The model must bind the bridge address, and the firewall must allow it.** Both
halves are required:

```bash
sudo ufw allow from 192.168.58.0/24 to 192.168.58.1 port 8080 proto tcp
```

**The egress NetworkPolicy is an allowlist.** Any new host port needs both a policy
patch and a ufw rule. Symptom: the agent hangs, GPU at 0%, no error anywhere.

**systemd does not strip inline comments** from `EnvironmentFile`. Comments go on
their own lines or blacksmith crash-loops silently.

**`AGENTS_LARGE_SLOTS` must equal llama-server's `--parallel`.** Slots and context
share one KV budget, and over-asking is silently downgraded — check the startup log.

**Install with `mv`, not `cp`** — a running TUI holds the binary open.

**Do not run `go work sync`** in the CodeArmory tree. It rewrites each module's
`go.mod` to the workspace build list but leaves hashes in `go.work.sum`; workspace
builds keep working while Docker builds (`GOWORK=off`) fail with
`missing go.sum entry`. Change the dependency in the module and run
`GOWORK=off go mod tidy` there instead.

**Model name must match the alias exactly.** `AGENTS_LARGE_MODEL`, the client's
model field and llama-server's `--alias` are one value in three places; a mismatch
is rejected at the API, not at startup.

**Prompt processing degrades sharply with context.** Long agent conversations cost
far more per token than short ones. If turns start taking minutes, check prompt
eval rate before blaming the model:

```bash
journalctl --user -u llama-large | grep -oE "prompt eval time.*tokens per second" | tail -3
```
