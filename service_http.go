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
	"github.com/code-armory-app/blacksmith/internal/ticket"
	"github.com/code-armory-app/blacksmith/internal/tools"
	"github.com/code-armory-app/blacksmith/internal/transport"
)

// workspaceMount is where every action mounts the run's shared volume; the role
// operates on the checkout the workflow's steps share.
const workspaceMount = "/workspace"

// actionRequest is the async submit body. The workspace is the workflow's shared
// VOLUME (its resource name, from create-volume) — not a repo/ref, because the
// clone and push are the workflow's forge steps, not the agent's.
type actionRequest struct {
	Volume string `json:"volume"` // the run's shared volume resource name
	Task   string `json:"task"`   // the request, the finding, the thing to do
	Check  string `json:"check"`  // optional check-command override for this role
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
		Volumes:         []forge.VolumeMount{{Name: req.Volume, MountPath: workspaceMount, Workdir: true}},
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

	build := func(tree map[string]string) (*agents.Agent, error) { return stage(maker, role, tree) }
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
		{Service: "tickets", Action: "createTicket", Resource: "tickets/tickets"},
		{Service: "tickets", Action: "createComment", Resource: "tickets/tickets"},
		{Service: "forge", Action: "createExecution", Resource: "forge/executions"},
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

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/actions/", srv.handle)
	slog.Info("blacksmith action service listening", "addr", addr)
	return http.ListenAndServe(addr, mux)
}

// handle routes the async contract: POST /actions/<role> submits, GET
// /actions/<job-id> polls. A POST is authorised — the caller must hold the
// action's permission — before any agent runs.
func (a *actionServer) handle(w http.ResponseWriter, r *http.Request) {
	seg := strings.TrimPrefix(r.URL.Path, "/actions/")
	switch r.Method {
	case http.MethodPost:
		var req actionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request body", http.StatusBadRequest)
			return
		}
		if req.Volume == "" {
			http.Error(w, "volume is required", http.StatusBadRequest)
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
		id := a.submit(context.WithoutCancel(r.Context()), seg, req, who)
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

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
