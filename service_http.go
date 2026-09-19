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
	Project    string `json:"project"`     // the wiki project the architect writes to (wiki_page tool)
	// WikiBranch, when set, makes the architect's wiki_page writes land on this branch of
	// the <project>-wiki repo (auto-created off main by the wiki service) instead of main.
	// The agent-plan pipeline sets it to "plan/${run_id}" so the plan lands on a branch a
	// human reviews as a PR before the build phase reads the merged main. Blank => main.
	WikiBranch string `json:"wiki_branch"`
	// Optional per-step sandbox overrides. Blank => the process default
	// (AGENTS_REPO_IMAGE / AGENTS_REPO_RUNNER_CLASS). A Go stage leaves them blank
	// and runs in golang; a frontend stage sets Image to a node image (and, if the
	// node toolchain needs a different runtime, RunnerClass) so it builds React/TS
	// in the right toolchain instead of the process-wide Go image.
	Image       string `json:"image"`
	RunnerClass string `json:"runner_class"`
	// An optional PERSISTENT Go-cache volume, mounted alongside the workspace at
	// /gocache with GOCACHE/GOMODCACHE pointed into it. The role's per-iteration
	// `go build`/`go test` check reads a warm build+module cache instead of
	// recompiling from cold — the same trick codearmory-ci uses to collapse the
	// test step from minutes to seconds. The workflow creates it with a FIXED
	// workflow_id so it outlives the run and every stage/step re-attaches it.
	CacheWorkflowID string `json:"cache_workflow_id"`
	CacheVolume     string `json:"cache_volume"`
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

// agentTrace collects the agent's outputs and tool calls as compact lines for the
// workflow run view. The manifest names `stdout` as the action's output_field, so
// whatever lands in actionResult.Stdout is what the run view shows as the step's
// logs — an agent step used to show only its last check, which read as "nothing
// happened" through the minutes a 60-turn role actually spends working. Each line's
// content is capped (per segment) so a whole file write does not fill the log, and
// the trace keeps only the last traceMaxLines so a long run cannot bloat the poll
// response the workflow engine reads every 5s.
type agentTrace struct {
	lines []string
}

const traceMaxLines = 250

// add caps a raw log line and appends it, dropping the oldest once over the bound.
// The agent loop is the only caller and it is single-goroutine, so no lock.
func (t *agentTrace) add(line string) {
	t.lines = append(t.lines, capTraceLine(line))
	if len(t.lines) > traceMaxLines {
		t.lines = t.lines[len(t.lines)-traceMaxLines:]
	}
}

// String joins entries with a BLANK line between them: a run's turns are now
// whole (full reasoning, full check output, some multi-line), so a single "\n"
// ran them together illegibly — the blank line is the visual break between one
// turn and the next.
func (t *agentTrace) String() string { return strings.Join(t.lines, "\n\n") }

// withCheck renders the trace with the role's last check appended as a footer, the
// combination that answers "what did it do, and where did it end up".
func (t *agentTrace) withCheck(lastCheck string) string {
	s := t.String()
	if lc := strings.TrimSpace(lastCheck); lc != "" {
		if s != "" {
			s += "\n\n"
		}
		s += "── check ──\n" + truncate(lc, 2000)
	}
	return s
}

// capTraceLine caps ONLY a write_file's call content, and leaves everything else
// WHOLE. The loop logs lines as "<role>: <body>", body often "<call> -> <result>".
// The one segment big enough to swamp the log is a write_file's call, which carries
// the entire file body; the reasoning, the command output, and every result are what
// an operator actually reads in the run view, so they are kept in full — a capped
// check output or a stubbed thought was the whole complaint. Only write_file's call
// is flattened and clipped; its short "Edited: …" result and all other lines pass
// through untouched (including their newlines).
func capTraceLine(line string) string {
	const writeContentCap = 200
	name, body, ok := strings.Cut(line, ": ")
	if !ok {
		return line
	}
	if call, res, ok := strings.Cut(body, " -> "); ok {
		if strings.HasPrefix(call, "write_file(") {
			call = truncate(call, writeContentCap)
		}
		return name + ": " + call + " -> " + res
	}
	return line
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
	tr := &agentTrace{}
	fail := func(msg string) {
		a.jobs.finish(id, func(r *actionResult) {
			r.Status = "failed"
			r.Error = msg
			// Show how far the agent got before it failed — the trace is the only
			// window into a role that ran for minutes and then broke.
			if s := tr.String(); s != "" {
				r.Stdout = s
			}
		})
		slog.Warn("action failed", "role", role, "job", id, "error", msg)
	}

	// MINT THE AGENT'S TEMP IDENTITY, scoped to the caller. gatekeeper keeps only
	// the permissions the user already holds, so the agent is bounded by them. No
	// gatekeeper configured (a local smoke test) means the agent acts as the
	// caller's own bearer — bounded, just not attenuated.
	agentBearer := who.bearer
	var roleID, sessionID string
	if a.gk != nil && who.sub.UserID != "" {
		rid, err := a.gk.MintRole(ctx, id, who.sub.UserID, who.sub.OrgID, agentPermissions(req.Project))
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
	// Mount the run's shared volume, and — when the workflow supplies one — a
	// PERSISTENT Go-cache volume at /gocache, so the role's checks compile against a
	// warm cache. sandboxEnv carries GOCACHE/GOMODCACHE onto every command the agent
	// runs (see ForgeSandbox.Env); empty when no cache is attached.
	volumes := []forge.VolumeMount{{WorkflowID: req.WorkflowID, Name: req.Volume, MountPath: workspaceMount, Workdir: true}}
	var sandboxEnv map[string]string
	if req.CacheWorkflowID != "" && req.CacheVolume != "" {
		volumes = append(volumes, forge.VolumeMount{WorkflowID: req.CacheWorkflowID, Name: req.CacheVolume, MountPath: "/gocache"})
		sandboxEnv = map[string]string{"GOCACHE": "/gocache/build", "GOMODCACHE": "/gocache/mod"}
	}
	// Per-step sandbox toolchain: a stage may override the image/runner class (a
	// frontend stage runs node, a Go stage the default golang). Blank keeps the
	// process default so every existing pipeline is unchanged.
	sandboxImage := a.cfg.Repo.Image
	if req.Image != "" {
		sandboxImage = req.Image
	}
	sandboxClass := a.cfg.Repo.RunnerClass
	if req.RunnerClass != "" {
		sandboxClass = req.RunnerClass
	}
	fc := forge.Local(a.cfg.ForgeURL, tokenCredential(agentBearer, a.cfg))
	sb, err := fc.Acquire(ctx, forge.SandboxSpec{
		Image:           sandboxImage,
		RunnerClass:     sandboxClass,
		TimeoutSecs:     a.cfg.Repo.TimeoutSecs,
		IdleTimeoutSecs: a.cfg.Repo.LeaseIdleSecs,
		MaxLifetimeSecs: a.cfg.Repo.LeaseMaxSecs,
		Volumes:         volumes,
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
	// Capture each landed write in order — path, content, deleted, and the
	// "type: summary" the edit declared — so the stage can commit its own work
	// into the volume as one conventional commit per write (commitJournal). The
	// service arm used to write a final tree with no git; the stages journal
	// their history now, and publish reads the commit types to name the branch by
	// impact and just pushes what the stages committed.
	var journal []recordedWrite
	maker := agents.Creator{
		Gateway: a.gw,
		// RepoDir points the check at the mounted volume's git, so a role's check runs in
		// a worktree of the pre-stage base (origin/<ref> resolvable) yet sees only the
		// agent's files — the base a coverage-vs-base or regression-proof check needs.
		Sandbox: tools.ForgeSandbox{Sandbox: sb, Env: sandboxEnv, RepoDir: workspaceMount},
		Check:   a.cfg.Repo.TestCommand,
		// STREAM THE TRACE LIVE. Appending to tr is not enough — the running job's
		// Stdout is what a poll returns, and it was only written on finish, so the run
		// view showed "running" with no output for the whole stage. Push the trace into
		// the running job on every line (jobStore.finish just applies a mutation under
		// lock; Status stays "running") so the workflow run view streams the agent's
		// reasoning and tool calls as they happen, not only after the role finishes.
		Log: func(line string) {
			slog.Info(line)
			tr.add(line)
			a.jobs.finish(id, func(r *actionResult) { r.Stdout = tr.String() })
		},
		FileTicket:  a.ticketFiler(agentBearer, req.Project),
		WritePage:   a.wikiWriter(agentBearer, req.Project, req.WikiBranch),
		// Central cross-repo docs wiki: architecture diagrams mirror here (main branch,
		// never a plan branch) in addition to the project wiki. Nil unless
		// AGENTS_DOCS_PROJECT names a project; wikiWriter returns nil for an empty one.
		WriteDocsPage: a.wikiWriter(agentBearer, a.cfg.DocsProject, ""),
		Project:       req.Project,
		ReadWiki:      a.wikiReader(agentBearer, req.Project),
		ReadTickets: a.ticketsReader(agentBearer, req.Project),
		MergeFix:    func(string) (string, error) { approved = true; return "approved", nil },
		OnWrite: func(path, content string, deleted bool, message string) {
			journal = append(journal, recordedWrite{path: path, content: content, deleted: deleted, message: message})
		},
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
	out, _, runErr := runStage(ctx, build, files, req.Task)
	if runErr != nil {
		fail("role " + role + ": " + firstLineOf(runErr.Error()))
		return
	}

	// COMMIT THE EDITS INTO THE VOLUME — one conventional commit per landed write,
	// on top of the .git the workflow's clone step laid down. The next steps (a
	// check, a scanner, the push) mount the same volume; publish reads these
	// commits' types to name the branch by impact and pushes them as the dev PR.
	changed, err := commitJournal(ctx, sb, journal, role)
	if err != nil {
		fail("commit the role's edits into the volume: " + firstLineOf(err.Error()))
		return
	}

	a.jobs.finish(id, func(r *actionResult) {
		r.Status = "completed"
		r.Stdout = tr.withCheck(out.LastCheck)
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
func agentPermissions(project string) []gatekeeper.Permission {
	perms := []gatekeeper.Permission{
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
		// tree — so an agent can only file where the user could. listTicket/getTicket
		// let a builder READ the task breakdown the PM filed (the read_tickets tool);
		// the tickets service scopes a listing to created_by/org, so this only ever
		// surfaces the pipeline owner's own tickets, not the whole board.
		{Service: "tickets", Action: "createTicket", Resource: "tickets/tickets"},
		{Service: "tickets", Action: "createComment", Resource: "tickets/tickets/*"},
		{Service: "tickets", Action: "listTicket", Resource: "tickets/tickets"},
		{Service: "tickets", Action: "getTicket", Resource: "tickets/tickets/*"},
	}
	// The architect writes the project's wiki via the wiki_page tool. The wiki now
	// authorizes on the reserved project namespace "project/<slug>/wiki/pages[/id]"
	// (the same top-level scope every service uses), so the scoped role carries that
	// shape. Attenuated against the user: a project MEMBER holds "project/<slug>/*"
	// via the project's tier role, which covers these — no per-user wiki grant needed.
	if project != "" {
		perms = append(perms,
			gatekeeper.Permission{Service: "wiki", Action: "writePage", Resource: "project/" + project + "/wiki/pages/*"},
			gatekeeper.Permission{Service: "wiki", Action: "getPage", Resource: "project/" + project + "/wiki/pages/*"},
			gatekeeper.Permission{Service: "wiki", Action: "listPage", Resource: "project/" + project + "/wiki/pages"},
		)
	}
	return perms
}

// ticketFiler files a finding on the board AS THE AGENT'S IDENTITY, so a finding
// can only land where the user could file one. No board configured is a no-op,
// like the standalone harness.
func (a *actionServer) ticketFiler(bearer, project string) func(kind, title, body, severity string) (string, error) {
	if a.cfg.TicketsURL == "" {
		return nil
	}
	store, err := platform.Local(a.cfg.TicketsURL, transport.Static(bearer))
	if err != nil {
		slog.Warn("action: could not build a tickets client for the agent", "error", err)
		return nil
	}
	return func(kind, title, body, severity string) (string, error) {
		// The kind prefix labels a REVIEWER's finding ("security: ..."). A PM filing
		// its task breakdown carries no kind, and prefixing ": title" there is just
		// noise — so only prefix when a stage actually set one.
		full := title
		if strings.TrimSpace(kind) != "" {
			full = kind + ": " + title
		}
		t, err := store.Create(context.Background(), ticket.Ticket{
			Title:       full,
			Description: body,
			Status:      ticket.StatusOpen,
			Priority:    severity,
			// Tag the project so the ticket appears under it in the portal and in a
			// project-scoped listing (the read_tickets tool a builder uses). Empty
			// for a project-less run, which files an unfiled ticket exactly as before.
			Project: project,
		})
		if err != nil {
			return "", err
		}
		return t.ID, nil
	}
}

// ticketsReader builds the read_tickets sink: it lists the project's OPEN board
// tickets as one document, so a builder (dev/frontend/backend) picks up the task
// breakdown the PM filed rather than reading task pages out of the wiki. It reads
// AS THE AGENT'S IDENTITY, so it only sees what the user could — which, since the
// PM and the builder run under the same pipeline owner, is the tasks the PM just
// filed. No board configured is a no-op, like the standalone harness.
func (a *actionServer) ticketsReader(bearer, project string) func() (string, error) {
	if a.cfg.TicketsURL == "" {
		return nil
	}
	store, err := platform.Local(a.cfg.TicketsURL, transport.Static(bearer))
	if err != nil {
		slog.Warn("action: could not build a tickets client for the agent", "error", err)
		return nil
	}
	return func() (string, error) {
		ts, err := store.List(context.Background(), ticket.ListOpts{Project: project, Status: ticket.StatusOpen})
		if err != nil {
			return "", err
		}
		if len(ts) == 0 {
			return "No open tickets are filed for this project. Ground your work in the wiki (read_wiki) and the task you were given.", nil
		}
		var b strings.Builder
		fmt.Fprintf(&b, "# Open tickets for project %q (%d)\n\n", project, len(ts))
		for _, t := range ts {
			pr := t.Priority
			if pr == "" {
				pr = "unset"
			}
			fmt.Fprintf(&b, "## [%s] %s\n(id %s)\n\n%s\n\n", pr, t.Title, t.ID, strings.TrimSpace(t.Description))
		}
		return b.String(), nil
	}
}

// wikiWriter builds the architect's WritePage sink: a closure that PUTs one page to the
// wiki service for `project`, authenticating as the agent's own run token (so the wiki's
// own writePage permission gates it). Nil when no wiki is configured or no project was
// given — the tool then tells the model it is not wired rather than pretending.
func (a *actionServer) wikiWriter(bearer, project, branch string) func(id, pageType, stack, format, title, content string) (string, error) {
	if a.cfg.WikiURL == "" || project == "" || bearer == "" {
		return nil
	}
	base := strings.TrimRight(a.cfg.WikiURL, "/")
	return func(id, pageType, stack, format, title, content string) (string, error) {
		fields := map[string]string{
			"type": pageType, "stack": stack, "format": format, "title": title, "content": content,
		}
		// When a plan branch is set, every page lands there (the wiki service creates it
		// off main on first write) so the whole plan can be reviewed as one PR.
		if branch != "" {
			fields["branch"] = branch
		}
		body, _ := json.Marshal(fields)
		req, err := http.NewRequest(http.MethodPut, base+"/projects/"+project+"/pages/"+id, strings.NewReader(string(body)))
		if err != nil {
			return "", err
		}
		req.Header.Set("Authorization", "Bearer "+bearer)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
			return "", fmt.Errorf("wiki %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
		}
		return "published to " + project, nil
	}
}

// wikiReader builds the ReadWiki sink: a closure that fetches the WHOLE project
// wiki — the manifest then each page's content — and returns it as one document,
// so a pm/dev/frontend stage reads the source of truth the architect wrote. Reads
// as the agent's own run token (the wiki's getPage/listPage permission gates it).
// Nil when no wiki is configured or no project was given.
func (a *actionServer) wikiReader(bearer, project string) func() (string, error) {
	if a.cfg.WikiURL == "" || project == "" || bearer == "" {
		return nil
	}
	base := strings.TrimRight(a.cfg.WikiURL, "/")
	get := func(path string) ([]byte, error) {
		req, err := http.NewRequest(http.MethodGet, base+path, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+bearer)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, fmt.Errorf("wiki %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
		}
		return raw, nil
	}
	return func() (string, error) {
		raw, err := get("/projects/" + project + "/pages")
		if err != nil {
			return "", err
		}
		var manifest struct {
			Pages []struct {
				ID, Title, Type, Stack, Format string
			} `json:"pages"`
		}
		if err := json.Unmarshal(raw, &manifest); err != nil {
			return "", err
		}
		if len(manifest.Pages) == 0 {
			return "The project wiki has no pages yet.", nil
		}
		var b strings.Builder
		fmt.Fprintf(&b, "# Project wiki: %s (%d pages)\n\n", project, len(manifest.Pages))
		for _, p := range manifest.Pages {
			pageRaw, perr := get("/projects/" + project + "/pages/" + p.ID)
			content := ""
			if perr == nil {
				var page struct {
					Content string `json:"content"`
				}
				_ = json.Unmarshal(pageRaw, &page)
				content = page.Content
			}
			// A diagram page is a rendered HTML doc, not prose — name it, don't dump it.
			if p.Format == "html" {
				content = "(rendered diagram — see the wiki UI)"
			}
			fmt.Fprintf(&b, "## %s  [id=%s type=%s stack=%s]\n\n%s\n\n---\n\n", p.Title, p.ID, p.Type, p.Stack, content)
		}
		return b.String(), nil
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

// recordedWrite is one landed edit captured from the agent's OnWrite: the file
// it touched, the post-edit content (empty when deleted), and the "type: summary"
// the edit declared, ready to become a conventional-commit subject.
type recordedWrite struct {
	path, content string
	deleted       bool
	message       string
}

// commitJournal replays the writes a role made — captured in order via the
// Creator's OnWrite — into the shared volume as ONE conventional commit per
// landed write, on top of the .git the workflow's clone step laid down. It
// reuses the same ccScope/ccWithScope/ccNormalize the CLI arm's journal uses
// (gitlog.go), so a run's history reads identically whichever arm produced it,
// and semantic versioning accepts every subject.
//
// This replaces writeToWorkspace, which laid the final tree with no git
// ("committing is the workflow's"). The stages own their history now: the
// publish step reads these commits' types to name the branch by impact
// (chore < fix < feat) and just pushes what the stages committed. A read-only
// role (a reviewer) journals nothing and commits nothing. Returns whether
// anything was written back.
func commitJournal(ctx context.Context, sb *forge.Sandbox, journal []recordedWrite, role string) (bool, error) {
	if len(journal) == 0 {
		return false, nil
	}
	// AUTHOR THE COMMITS AS THE AGENT ROLE, under an @blacksmith.agent identity. This is
	// how a reader (and the PR view) tells an agent's commit from a human's — by the
	// authored identity, not a parsed name. The local part is the role (fix-dev, req-dev,
	// …); the .agent domain suffix marks it automated (forge CI/CD steps use .cicd).
	author := "blacksmith"
	if role != "" {
		author = role
	}
	email := author + "@blacksmith.agent"
	var b strings.Builder
	b.WriteString("cd " + workspaceMount + "\n")
	b.WriteString("export HOME=/tmp\n")
	// The forge container has no git identity or ownership config; set both so
	// the commits land and go tooling in later steps trusts the .git.
	b.WriteString("git config --global --add safe.directory '*'\n")
	for _, w := range journal {
		if w.deleted {
			fmt.Fprintf(&b, "rm -f %s\n", forge.Quote(w.path))
		} else {
			b.WriteString(tools.WriteTreeScript(map[string]string{w.path: w.content}))
		}
		// A no-op write (identical content) stages nothing; `|| true` lets the
		// replay continue past a commit git declines for lack of changes.
		msg := ccNormalize(ccWithScope(w.message, ccScope(w.path)))
		fmt.Fprintf(&b, "git add -A && git -c user.email=%s -c user.name=%s commit -q -m %s || true\n", forge.Quote(email), forge.Quote(author), forge.Quote(msg))
	}
	// PACK a large replay, exactly as the check path does. A converging fix can make
	// dozens of writes (measured: 33 over 20 turns, incl. whole-file test rewrites),
	// and the raw replay script then overflows forge's ~64KB execution-body cap — a
	// bare "400 invalid request body" that discarded a whole green stage's work. Source
	// gzips ~4-5x, so the packed form fits.
	script := b.String()
	if len(script) > tools.PackThreshold {
		script = tools.Pack(script)
	}
	if len(script) > tools.MaxScriptBytes {
		return false, fmt.Errorf(
			"the role's edits are too large to commit in one forge execution (%d bytes even packed; the cap is ~64KB) — the fix changed too much at once", len(script))
	}
	res, err := sb.Run(ctx, nil, script)
	if err != nil {
		return false, err
	}
	if res.ExitCode != 0 {
		return false, fmt.Errorf("%s", firstLineOf(strings.TrimSpace(res.Stderr)))
	}
	return true, nil
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
