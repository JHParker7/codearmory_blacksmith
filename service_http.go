package main

// Blacksmith as a CodeArmory service: the agent harness exposed as async
// ACTIONS the workflows engine invokes. An action runs ONE role — a model call
// and its tool/edit loop, under that role's read/write guards — against the
// workflow's SHARED VOLUME, as a TEMPORARY IDENTITY scoped to the user who
// triggered the run.
//
// Blacksmith owns the roles and the harness; forge owns the volume and the
// execution; the workflow owns the flow (it clones into the volume, runs the
// checks and scanners, versions, builds, and pushes — all as forge steps). An
// action neither clones nor commits nor pushes: it reads the checkout the
// workflow's steps share, runs the role, writes the edits back to the same
// volume, and returns typed outputs the workflow routes on.
//
// DELEGATION: on each call blacksmith resolves the caller from their bearer,
// mints a run-scoped role+token bounded by that user (gatekeeper drops any
// permission the user lacks), and runs the agent as that identity — so an agent
// can never file, read, or touch anything the user could not. The temp identity
// is revoked when the action ends.

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/code-armory-app/blacksmith/internal/agents"
	"github.com/code-armory-app/blacksmith/internal/config"
	"github.com/code-armory-app/blacksmith/internal/forge"
	"github.com/code-armory-app/blacksmith/internal/gatekeeper"
	"github.com/code-armory-app/blacksmith/internal/model"
	"github.com/code-armory-app/blacksmith/internal/platform"
	"github.com/code-armory-app/blacksmith/internal/roles"
	"github.com/code-armory-app/blacksmith/internal/ticket"
	"github.com/code-armory-app/blacksmith/internal/tools"
	"github.com/code-armory-app/blacksmith/internal/transport"
)

// workspaceMount is where every action mounts the run's shared volume; the role
// operates on the checkout the workflow's steps share.
const workspaceMount = "/workspace"

// actionRequest is the async submit body. The workspace is the workflow's shared
// VOLUME, addressed by its LOGICAL HANDLE — WorkflowID (the run id every step
// shares) plus Volume (the logical name, e.g. "workspace") — not a repo/ref,
// because the clone and push are the workflow's forge steps, not the agent's,
// and not a resource name, because forge/create-volume returns none (it derives
// the resource from the handle). The workflow wires WorkflowID as ${run_id}.
type actionRequest struct {
	Role       string `json:"role"`        // which stored role to run as (architect, plan-dev, …)
	WorkflowID string `json:"workflow_id"` // the run id the shared volume belongs to
	Volume     string `json:"volume"`      // the shared volume's logical name
	Task       string `json:"task"`        // the request, the finding, the thing to do
	Check      string `json:"check"`       // optional check-command override for this role
}

// actionResult is one job's terminal state, shaped for the workflows async
// contract: `status` is polled, `outputs` feeds the next step, `stdout` carries
// the last check for a human, `error` names a failure.
type actionResult struct {
	Status  string            `json:"status"` // running | completed | failed
	Outputs map[string]string `json:"outputs"`
	Stdout  string            `json:"stdout"`
	Error   string            `json:"error"`
}

// jobStore holds running and finished actions in process. A restart forgets
// them, which is correct for a stateless harness: a lost job is re-triggered by
// the workflow and the work is idempotent.
type jobStore struct {
	mu   sync.Mutex
	jobs map[string]*actionResult
	seq  int
}

func newJobStore() *jobStore { return &jobStore{jobs: map[string]*actionResult{}} }

func (s *jobStore) create() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	id := "job-" + strconv.Itoa(s.seq)
	s.jobs[id] = &actionResult{Status: "running", Outputs: map[string]string{}}
	return id
}

func (s *jobStore) get(id string) (actionResult, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.jobs[id]
	if !ok {
		return actionResult{}, false
	}
	return *r, true
}

func (s *jobStore) finish(id string, apply func(*actionResult)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.jobs[id]; ok {
		apply(r)
	}
}

// actionServer wires the harness once — the model gateway, the gatekeeper
// client, the config — and per action mints a temp identity and acquires a
// forge sandbox AS that identity, mounting the run's volume.
type actionServer struct {
	gw   *model.Gateway
	gk   *gatekeeper.Client
	cfg  config.Config
	jobs *jobStore

	// roles is the store the agent action looks a role up in. Nil when no database
	// is configured, in which case run() falls back to the roles compiled into
	// internal/agents — so a local smoke test needs no postgres.
	roles *roles.Store
}

// caller is who a request is for and the bearer to act as, resolved from the
// inbound token.
type caller struct {
	sub    gatekeeper.Subject
	bearer string
}

func (a *actionServer) submit(ctx context.Context, role string, req actionRequest, who caller) string {
	id := a.jobs.create()
	go a.run(ctx, id, role, req, who)
	return id
}

func (a *actionServer) run(ctx context.Context, id, role string, req actionRequest, who caller) {
	fail := func(msg string) {
		a.jobs.finish(id, func(r *actionResult) { r.Status = "failed"; r.Error = msg })
		slog.Warn("action failed", "role", role, "job", id, "error", msg)
	}

	// MINT THE AGENT'S TEMP IDENTITY, scoped to the caller. gatekeeper keeps only
	// the permissions the user already holds, so the agent is bounded by them. No
	// gatekeeper configured (a local smoke test) means the agent acts as the
	// caller's own bearer — bounded, just not attenuated.
	agentBearer := who.bearer
	var roleID, sessionID string
	if a.gk != nil && who.sub.UserID != "" {
		rid, err := a.gk.MintRole(ctx, id, who.sub.UserID, who.sub.OrgID, agentPermissions())
		if err != nil {
			fail("mint the agent's scoped role: " + firstLineOf(err.Error()))
			return
		}
		tok, sid, err := a.gk.MintRunToken(ctx, who.sub.UserID, rid)
		if err != nil {
			fail("mint the agent's run token: " + firstLineOf(err.Error()))
			return
		}
		agentBearer, roleID, sessionID = tok, rid, sid
		defer a.gk.Revoke(context.WithoutCancel(ctx), sessionID, roleID)
	}

	// ACQUIRE THE SANDBOX AS THE AGENT, mounting the run's shared volume as the
	// working directory. No CloneURL: the checkout is already in the volume, put
	// there by the workflow's clone step; the role operates on what the steps
	// share.
	fc := forge.Local(a.cfg.ForgeURL, tokenCredential(agentBearer, a.cfg))
	sb, err := fc.Acquire(ctx, forge.SandboxSpec{
		Image:           a.cfg.Repo.Image,
		RunnerClass:     a.cfg.Repo.RunnerClass,
		TimeoutSecs:     a.cfg.Repo.TimeoutSecs,
		IdleTimeoutSecs: a.cfg.Repo.LeaseIdleSecs,
		MaxLifetimeSecs: a.cfg.Repo.LeaseMaxSecs,
		Volumes:         []forge.VolumeMount{{WorkflowID: req.WorkflowID, Name: req.Volume, MountPath: workspaceMount, Workdir: true}},
	})
	if err != nil {
		fail("acquire a sandbox on volume " + req.Volume + ": " + firstLineOf(err.Error()))
		return
	}
	defer sb.Release(context.WithoutCancel(ctx))

	files, err := seedFromWorkspace(ctx, sb)
	if err != nil {
		fail("read the shared checkout: " + firstLineOf(err.Error()))
		return
	}

	// The agent files findings AS THE USER through a tickets client on the agent's
	// token, and a reviewer records approval as an output rather than merging.
	approved := false
	maker := agents.Creator{
		Gateway:    a.gw,
		Sandbox:    tools.ForgeSandbox{Sandbox: sb},
		Check:      a.cfg.Repo.TestCommand,
		Log:        func(line string) { slog.Info(line) },
		FileTicket: a.ticketFiler(agentBearer),
		MergeFix:   func(string) (string, error) { approved = true; return "approved", nil },
	}
	if req.Check != "" {
		maker.Check = req.Check
	}

	// BUILD THE AGENT FROM THE STORED ROLE. The role is data now: a row an operator
	// edits in the portal, picked by name on the one blacksmith/agent action. With
	// no database configured, fall back to the roles compiled into internal/agents
	// so the local CLI/TUI and a smoke test still work.
	build := func(tree map[string]string) (*agents.Agent, error) {
		if a.roles != nil {
			row, err := a.roles.Get(ctx, role)
			if err != nil {
				return nil, fmt.Errorf("role %q: %w", role, err)
			}
			return maker.New(tree, roleToOptions(row)), nil
		}
		return stage(maker, role, tree)
	}
	out, tree, runErr := runStage(ctx, build, files, req.Task)
	if runErr != nil {
		fail("role " + role + ": " + firstLineOf(runErr.Error()))
		return
	}

	// WRITE THE EDITS BACK TO THE VOLUME — no git. The next workflow step (a check,
	// a scanner, the push) mounts the same volume and sees them.
	changed, err := writeToWorkspace(ctx, sb, tree)
	if err != nil {
		fail("write the role's edits back to the volume: " + firstLineOf(err.Error()))
		return
	}

	a.jobs.finish(id, func(r *actionResult) {
		r.Status = "completed"
		r.Stdout = truncate(out.LastCheck, 4000)
		r.Outputs = map[string]string{
			"passed":   strconv.FormatBool(out.Passed),
			"approved": strconv.FormatBool(approved),
			"changed":  strconv.FormatBool(changed),
			"answer":   truncate(out.Answer, 2000),
		}
	})
	slog.Info("action completed", "role", role, "job", id, "passed", out.Passed, "changed", changed)
}

// agentPermissions is the set an agent run requests. gatekeeper intersects it
// with the user's own grants, so this is a ceiling, not a grant: the agent may
// file findings and run in its sandbox, and only against what the user can.
func agentPermissions() []gatekeeper.Permission {
	return []gatekeeper.Permission{
		// The role runs its ENTIRE tool loop inside ONE forge lease: acquire it
		// (createLease), wait for its sandbox to boot (getLease), run every command
		// in it — the seed, the edits, each check — as executions bound to the lease
		// (createExecution to submit, getExecution to poll each to a terminal state),
		// and release it at the end (deleteLease). Requesting only createExecution —
		// as this first did — got the mint past gatekeeper but 403'd at POST /leases,
		// because a lease is its own resource, not an execution. Item-scoped reads and
		// the release use the "/*" child space the owner's forge grant covers.
		// Forge checks getLease and deleteLease on the BARE forge/leases collection
		// (not the item space) — measured: requesting them on forge/leases/* minted
		// fine but 403'd the WaitReady poll and the release. getExecution, in
		// contrast, forge checks on the item forge/executions/{id}, so that one is
		// the "/*" child space.
		{Service: "forge", Action: "createLease", Resource: "forge/leases"},
		{Service: "forge", Action: "getLease", Resource: "forge/leases"},
		{Service: "forge", Action: "deleteLease", Resource: "forge/leases"},
		{Service: "forge", Action: "createExecution", Resource: "forge/executions"},
		{Service: "forge", Action: "getExecution", Resource: "forge/executions/*"},
		// A reviewer files findings on the board AS THE USER — never committed to the
		// tree — so an agent can only file where the user could.
		{Service: "tickets", Action: "createTicket", Resource: "tickets/tickets"},
		{Service: "tickets", Action: "createComment", Resource: "tickets/tickets/*"},
	}
}

// ticketFiler files a finding on the board AS THE AGENT'S IDENTITY, so a finding
// can only land where the user could file one. No board configured is a no-op,
// like the standalone harness.
func (a *actionServer) ticketFiler(bearer string) func(kind, title, body, severity string) (string, error) {
	if a.cfg.TicketsURL == "" {
		return nil
	}
	store, err := platform.Local(a.cfg.TicketsURL, transport.Static(bearer))
	if err != nil {
		slog.Warn("action: could not build a tickets client for the agent", "error", err)
		return nil
	}
	return func(kind, title, body, severity string) (string, error) {
		t, err := store.Create(context.Background(), ticket.Ticket{
			Title:       kind + ": " + title,
			Description: body,
			Status:      ticket.StatusOpen,
			Priority:    severity,
		})
		if err != nil {
			return "", err
		}
		return t.ID, nil
	}
}

// tokenCredential turns a bearer into a forge credential. A minted run token is
// static for the life of an action; falling back to the operator credential is
// only for a local smoke test where no caller identity is present.
func tokenCredential(bearer string, cfg config.Config) transport.Credential {
	if bearer != "" {
		return transport.Static(bearer)
	}
	return planeCredential(cfg)
}

// seedFromWorkspace reads the shared checkout the workflow's steps populated —
// the whole working tree, INCLUDING uncommitted files a forge step wrote (the
// scanner reports a reviewer must see), tarred out of the volume minus .git.
func seedFromWorkspace(ctx context.Context, sb *forge.Sandbox) (map[string]string, error) {
	res, err := sb.Run(ctx, nil, "cd "+workspaceMount+" && tar --exclude=./.git -cf - . | base64 | tr -d '\\n'")
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("tar workspace: %s", firstLineOf(strings.TrimSpace(res.Stderr)))
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(res.Stdout))
	if err != nil {
		return nil, fmt.Errorf("decode workspace: %w", err)
	}
	files := map[string]string{}
	tr := tar.NewReader(bytes.NewReader(raw))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read workspace: %w", err)
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		files[strings.TrimPrefix(h.Name, "./")] = string(b)
	}
	return files, nil
}

// writeToWorkspace makes the shared checkout EXACTLY the role's tree — clears
// the working files (keeping .git and the volume itself), lays the tree down —
// so the next step sees the edits. No git: committing and pushing are the
// workflow's. Returns whether the tree differs from what was there.
func writeToWorkspace(ctx context.Context, sb *forge.Sandbox, files map[string]string) (bool, error) {
	script := "cd " + workspaceMount + "\n" +
		"before=$(find . -path ./.git -prune -o -type f -print | sort | xargs -r sha1sum | sha1sum)\n" +
		"find . -mindepth 1 -path ./.git -prune -o -exec rm -rf {} + 2>/dev/null || true\n" +
		tools.WriteTreeScript(files) +
		"after=$(find . -path ./.git -prune -o -type f -print | sort | xargs -r sha1sum | sha1sum)\n" +
		"[ \"$before\" = \"$after\" ] && echo __SAME__ || echo __CHANGED__\n"
	res, err := sb.Run(ctx, nil, script)
	if err != nil {
		return false, err
	}
	if res.ExitCode != 0 {
		return false, fmt.Errorf("%s", firstLineOf(strings.TrimSpace(res.Stderr)))
	}
	return strings.Contains(res.Stdout, "__CHANGED__"), nil
}

// serveActions is the service entrypoint: register with gatekeeper, wire the
// harness, expose the async action endpoints, serve until interrupted.
func serveActions(addr string) error {
	config.LoadOperatorEnv()
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	if cfg.ForgeURL == "" {
		return fmt.Errorf("the action service needs a forge to run in; set AGENTS_FORGE_URL")
	}
	wireTickets(cfg)

	srv := &actionServer{
		gw:   model.NewGateway(cfg.Host, cfg.Classes),
		cfg:  cfg,
		jobs: newJobStore(),
	}
	// Register as a service: the rotating east-west key authenticates blacksmith
	// to gatekeeper for validating callers and minting temp identities. Without a
	// gatekeeper the service still serves, unauthenticated — a local smoke test.
	if cfg.GatekeeperURL != "" && cfg.ServiceKey != "" {
		key := gatekeeper.StartKeyRotation(context.Background(), cfg.GatekeeperURL, "blacksmith", cfg.ServiceKey, 25*time.Minute)
		srv.gk = &gatekeeper.Client{URL: cfg.GatekeeperURL, Service: "blacksmith", Key: key}
		slog.Info("registered with gatekeeper", "url", cfg.GatekeeperURL)
	}

	// OPEN THE ROLE STORE. It migrates the table and seeds the default roles on a
	// fresh database. Without AGENTS_DATABASE_URL there is no store and the agent
	// action falls back to the compiled-in roles — enough for a local smoke test,
	// but the portal's role editing and role CRUD need the database.
	if cfg.DatabaseURL != "" {
		store, err := roles.Open(context.Background(), cfg.DatabaseURL)
		if err != nil {
			return fmt.Errorf("roles store: %w", err)
		}
		srv.roles = store
		slog.Info("role store ready (postgres)")
	} else {
		slog.Warn("no AGENTS_DATABASE_URL: agent action uses compiled-in roles; role editing is off")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/actions/", srv.handle)
	mux.HandleFunc("/roles", srv.handleRoles)
	mux.HandleFunc("/roles/", srv.handleRole)
	slog.Info("blacksmith action service listening", "addr", addr)
	return http.ListenAndServe(addr, mux)
}

// handle routes the async contract of the ONE general agent action: POST
// /actions/agent submits (the role is named in the body), GET /actions/<job-id>
// polls. A POST is authorised — the caller must hold runAction — before any
// agent runs.
func (a *actionServer) handle(w http.ResponseWriter, r *http.Request) {
	seg := strings.TrimPrefix(r.URL.Path, "/actions/")
	switch r.Method {
	case http.MethodPost:
		var req actionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request body", http.StatusBadRequest)
			return
		}
		if req.Volume == "" || req.WorkflowID == "" {
			http.Error(w, "workflow_id and volume are required to address the shared volume", http.StatusBadRequest)
			return
		}
		if req.Role == "" {
			http.Error(w, "role is required: name the role this agent runs as (e.g. plan-architect)", http.StatusBadRequest)
			return
		}
		who := caller{bearer: strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")}
		if a.gk != nil {
			sub, ok, err := a.gk.Check(r.Context(), who.bearer, "runAction", "blacksmith/actions")
			if err != nil {
				http.Error(w, "gatekeeper unavailable", http.StatusBadGateway)
				return
			}
			if !ok {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			who.sub = sub
		}
		id := a.submit(context.WithoutCancel(r.Context()), req.Role, req, who)
		writeJSON(w, http.StatusAccepted, map[string]string{"job_id": id})
	case http.MethodGet:
		res, ok := a.jobs.get(seg)
		if !ok {
			http.Error(w, "no such job", http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, res)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// rolesAuthorized gates a role-CRUD call. Conductor already enforces the
// endpoint's permission before it proxies here, so this is defense in depth: it
// re-checks the caller's bearer against the action on blacksmith/roles. With no
// gatekeeper configured (a local run) it allows the call, matching how the agent
// action behaves unauthenticated.
func (a *actionServer) rolesAuthorized(w http.ResponseWriter, r *http.Request, action string) bool {
	if a.gk == nil {
		return true
	}
	bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	_, ok, err := a.gk.Check(r.Context(), bearer, action, "blacksmith/roles")
	if err != nil {
		http.Error(w, "gatekeeper unavailable", http.StatusBadGateway)
		return false
	}
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return false
	}
	return true
}

// handleRoles serves the collection: GET /roles lists every stored role. A role
// is created by PUT /roles/{name} (handleRole), so there is no POST here.
func (a *actionServer) handleRoles(w http.ResponseWriter, r *http.Request) {
	if a.roles == nil {
		http.Error(w, "role store not configured; set AGENTS_DATABASE_URL", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !a.rolesAuthorized(w, r, "listRole") {
		return
	}
	list, err := a.roles.List(r.Context())
	if err != nil {
		http.Error(w, "list roles: "+firstLineOf(err.Error()), http.StatusInternalServerError)
		return
	}
	if list == nil {
		list = []roles.Role{}
	}
	writeJSON(w, http.StatusOK, list)
}

// handleRole serves one role: GET reads it, PUT creates or replaces it, DELETE
// removes it. The name is the last path segment.
func (a *actionServer) handleRole(w http.ResponseWriter, r *http.Request) {
	if a.roles == nil {
		http.Error(w, "role store not configured; set AGENTS_DATABASE_URL", http.StatusServiceUnavailable)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/roles/")
	if name == "" || strings.Contains(name, "/") {
		http.Error(w, "role name required", http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodGet:
		if !a.rolesAuthorized(w, r, "getRole") {
			return
		}
		role, err := a.roles.Get(r.Context(), name)
		if errors.Is(err, roles.ErrNotFound) {
			http.Error(w, "no such role", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, "get role: "+firstLineOf(err.Error()), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, role)
	case http.MethodPut:
		if !a.rolesAuthorized(w, r, "updateRole") {
			return
		}
		var role roles.Role
		if err := json.NewDecoder(r.Body).Decode(&role); err != nil {
			http.Error(w, "bad request body", http.StatusBadRequest)
			return
		}
		role.Name = name // the URL is authoritative, not the body
		if err := a.roles.Put(r.Context(), role); err != nil {
			http.Error(w, "save role: "+firstLineOf(err.Error()), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, role)
	case http.MethodDelete:
		if !a.rolesAuthorized(w, r, "deleteRole") {
			return
		}
		if err := a.roles.Delete(r.Context(), name); err != nil {
			http.Error(w, "delete role: "+firstLineOf(err.Error()), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
