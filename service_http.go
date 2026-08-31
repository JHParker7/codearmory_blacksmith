package main

// Blacksmith as a CodeArmory service: the agent harness exposed as async
// ACTIONS the workflows engine invokes. An action runs ONE role — a model call
// and its tool/edit loop, under that role's read/write guards — and returns the
// agent's outputs. Nothing else.
//
// THE GIT NEVER TOUCHES THIS HOST. Forge clones the repository into the sandbox
// (the same sandbox the agent already runs its checks in), the role works, and
// the change is pushed back FROM the sandbox. Orchestration — the sequence of
// roles, the gates, the reroll — is the workflow's job; it POSTs an action,
// polls the job, and ROUTES on the outputs. Blacksmith owns the roles and the
// harness; forge owns execution; the workflow owns the flow.
//
// The contract is the one forge already speaks, so workflows drives it with no
// new machinery: POST an action → {job_id}; GET the job → {status, outputs,
// stdout} until a terminal status; the next step reads ${steps.X.output.KEY}.

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

	"github.com/code-armory-app/blacksmith/internal/agents"
	"github.com/code-armory-app/blacksmith/internal/config"
	"github.com/code-armory-app/blacksmith/internal/forge"
	"github.com/code-armory-app/blacksmith/internal/model"
	"github.com/code-armory-app/blacksmith/internal/tools"
)

// actionRequest is the async submit body. The workspace is NAMED by a clone URL
// and a ref, not shipped: forge clones it into the sandbox, and only its name
// crosses the wire between steps.
type actionRequest struct {
	Repo  string `json:"repo"`  // clone URL forge checks out into the sandbox
	Ref   string `json:"ref"`   // branch the role works on and pushes back
	Task  string `json:"task"`  // the request, the finding, the thing to do
	Check string `json:"check"` // optional check-command override for this role
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
// the workflow, and the work is idempotent — it re-clones the ref and redoes it.
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
	return *r, true // a copy, so a poll never races the worker's writes
}

func (s *jobStore) finish(id string, apply func(*actionResult)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.jobs[id]; ok {
		apply(r)
	}
}

// actionServer wires the harness once — the model gateway, the board client —
// and acquires a FRESH forge sandbox per action, cloned to that action's repo
// and ref. The role and its guards come from agents.Creator.Stage; the git and
// the execution come from the sandbox.
type actionServer struct {
	gw    *model.Gateway
	forge *forge.Client
	cfg   config.Config
	jobs  *jobStore
}

func (a *actionServer) submit(ctx context.Context, role string, req actionRequest) string {
	id := a.jobs.create()
	go a.run(ctx, id, role, req)
	return id
}

func (a *actionServer) run(ctx context.Context, id, role string, req actionRequest) {
	fail := func(msg string) {
		a.jobs.finish(id, func(r *actionResult) { r.Status = "failed"; r.Error = msg })
		slog.Warn("action failed", "role", role, "job", id, "error", msg)
	}

	// FORGE CLONES THE REPO INTO THE SANDBOX. The role never sees a clone URL or
	// a credential; it sees a working tree forge stood up for it.
	sb, err := a.forge.Acquire(ctx, forge.SandboxSpec{
		Image:           a.cfg.Repo.Image,
		RunnerClass:     a.cfg.Repo.RunnerClass,
		TimeoutSecs:     a.cfg.Repo.TimeoutSecs,
		CloneURL:        req.Repo,
		SecretRef:       a.cfg.Repo.SecretRef,
		Branch:          req.Ref,
		IdleTimeoutSecs: a.cfg.Repo.LeaseIdleSecs,
		MaxLifetimeSecs: a.cfg.Repo.LeaseMaxSecs,
	})
	if err != nil {
		fail("acquire a sandbox for " + req.Repo + "@" + req.Ref + ": " + firstLineOf(err.Error()))
		return
	}
	defer sb.Release(context.WithoutCancel(ctx))

	files, err := seedFromSandbox(ctx, sb)
	if err != nil {
		fail("read the checkout forge cloned: " + firstLineOf(err.Error()))
		return
	}

	// The reviewer roles record approval as an OUTPUT rather than merging: the
	// merge is a workflow step, not the agent's. file_ticket still reaches the
	// board through the tickets client.
	approved := false
	maker := agents.Creator{
		Gateway:    a.gw,
		Sandbox:    tools.ForgeSandbox{Sandbox: sb},
		Check:      a.cfg.Repo.TestCommand,
		Log:        func(line string) { slog.Info(line) },
		FileTicket: fileFinding,
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

	// PUSH FROM INSIDE THE SANDBOX. The role's tree is laid over a fresh checkout
	// of the ref and committed and pushed in one forge command — the same
	// sandbox, git creds and all. A role that changed nothing (a reviewer)
	// reports it and pushes nothing.
	changed, err := pushFromSandbox(ctx, sb, req.Ref, tree, "chore("+role+"): "+truncate(req.Task, 60))
	if err != nil {
		fail("push " + req.Ref + " from the sandbox: " + firstLineOf(err.Error()))
		return
	}

	a.jobs.finish(id, func(r *actionResult) {
		r.Status = "completed"
		r.Stdout = truncate(out.LastCheck, 4000)
		r.Outputs = map[string]string{
			"ref":      req.Ref,
			"passed":   strconv.FormatBool(out.Passed),
			"approved": strconv.FormatBool(approved),
			"changed":  strconv.FormatBool(changed),
			"answer":   truncate(out.Answer, 2000),
		}
	})
	slog.Info("action completed", "role", role, "job", id, "passed", out.Passed, "changed", changed)
}

// seedFromSandbox reads the tree forge cloned, so the agent starts from what the
// ref actually holds. `git archive` of HEAD is the committed tree — exactly the
// ref tip — tarred and base64'd out of the sandbox and unpacked here.
func seedFromSandbox(ctx context.Context, sb *forge.Sandbox) (map[string]string, error) {
	res, err := sb.Run(ctx, nil, "git archive --format=tar HEAD | base64 | tr -d '\\n'")
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("git archive: %s", firstLineOf(strings.TrimSpace(res.Stderr)))
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(res.Stdout))
	if err != nil {
		return nil, fmt.Errorf("decode archive: %w", err)
	}
	files := map[string]string{}
	tr := tar.NewReader(bytes.NewReader(raw))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read archive: %w", err)
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		files[h.Name] = string(b)
	}
	return files, nil
}

// pushFromSandbox makes the ref's checkout EXACTLY the role's tree and pushes it
// — all inside the sandbox forge already leased. RunOnBranch resets to the ref
// tip first; the script then clears the tracked files (keeping .git), lays the
// tree down, and commits+pushes only if something actually changed. Returns
// whether anything was pushed.
func pushFromSandbox(ctx context.Context, sb *forge.Sandbox, ref string, files map[string]string, message string) (bool, error) {
	script := "find . -mindepth 1 -path ./.git -prune -o -exec rm -rf {} + 2>/dev/null || true\n" +
		tools.WriteTreeScript(files) +
		"git add -A\n" +
		"if git diff --cached --quiet; then echo __NOCHANGE__; exit 0; fi\n" +
		"git -c user.name=blacksmith -c user.email=blacksmith@workshop.invalid commit -q -m " + forge.Quote(message) + "\n" +
		"git push -q origin HEAD:refs/heads/" + forge.Quote(ref) + " && echo __PUSHED__\n"
	res, err := sb.RunOnBranch(ctx, nil, ref, script)
	if err != nil {
		return false, err
	}
	if strings.Contains(res.Stdout, "__NOCHANGE__") {
		return false, nil
	}
	if res.ExitCode != 0 || !strings.Contains(res.Stdout, "__PUSHED__") {
		return false, fmt.Errorf("%s", firstLineOf(strings.TrimSpace(res.Stderr+res.Stdout)))
	}
	return true, nil
}

// serveActions is the service entrypoint: wire the harness, expose the async
// action endpoints, serve until interrupted. Registration with gatekeeper and
// the git-factory clone-token are the next phase; this proves the harness
// answers the workflows contract with all git kept in forge.
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
		gw:    model.NewGateway(cfg.Host, cfg.Classes),
		forge: forge.Local(cfg.ForgeURL, planeCredential(cfg)),
		cfg:   cfg,
		jobs:  newJobStore(),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/actions/", srv.handle)
	slog.Info("blacksmith action service listening", "addr", addr)
	return http.ListenAndServe(addr, mux)
}

// handle routes the async contract: POST /actions/<role> submits, GET
// /actions/<job-id> polls. One path prefix, told apart by method.
func (a *actionServer) handle(w http.ResponseWriter, r *http.Request) {
	seg := strings.TrimPrefix(r.URL.Path, "/actions/")
	switch r.Method {
	case http.MethodPost:
		var req actionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request body", http.StatusBadRequest)
			return
		}
		if req.Repo == "" || req.Ref == "" {
			http.Error(w, "repo and ref are required", http.StatusBadRequest)
			return
		}
		id := a.submit(context.WithoutCancel(r.Context()), seg, req)
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
