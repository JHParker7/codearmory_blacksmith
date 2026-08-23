package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakePlatform is an in-memory stand-in for the tickets service behind
// conductor. Tests drive the real CodeArmory client against it, so the client,
// the dispatcher and the agents are all exercised over real HTTP rather than
// against a mock interface.
type fakePlatform struct {
	mu      sync.Mutex
	tickets map[string]*Ticket
	// deps maps a ticket to the tickets it waits for.
	deps    map[string][]string
	nextCmt int
	now     time.Time

	// failNext, when non-zero, makes the next N requests return 500 so retry and
	// classification can be tested.
	failNext int
	// denyAll returns 403 for everything.
	denyAll bool
	// reads counts single-ticket GETs, so a test can prove selection stays bounded.
	reads int
	// noETag simulates an instance predating optimistic concurrency, so the
	// client's claim-by-append fallback can be exercised.
	noETag bool
	// calls records method+path for assertions.
	calls []string

	// Forge state.
	execs    map[string]*Execution
	nextExec int
	// nextLease/leaseSpecs/leasesReleased mirror the execution fields: agents now
	// require a held container, so the fake has to grant and track them.
	nextLease      int
	leasesReleased []string
	// execSteps is how many polls an execution stays non-terminal, so the poll
	// loop is actually exercised rather than short-circuited on the first read.
	execSteps int
	execPolls map[string]int
	// execFailIfScriptContains makes only the matching executions fail, so a test
	// can fail the test command without also failing the repo survey.
	execFailIfScriptContains string
	// execOutcome is the terminal status submitted executions reach.
	execOutcome  string
	execExitCode int
	execStdout   string
	// cancelled records executions the client asked forge to stop.
	cancelled []string
	// execSpecs records what was submitted, so a test can assert on the spec
	// (secret refs, image, runner class) and not merely on the outcome.
	execSpecs  []SandboxSpec
	leaseSpecs []LeaseSpec
	failing    map[string]bool

	// Workflow runs.
	runs    map[string]*Run
	nextRun int
	// runOutcome is the terminal status a triggered run reaches.
	runOutcome string
	// runSteps is how many polls a run stays non-terminal.
	runSteps  int
	runPolls  map[string]int
	runInputs []map[string]string
}

// serveWorkflows implements enough of the workflows API to drive verification.
func (f *fakePlatform) serveWorkflows(w http.ResponseWriter, r *http.Request, parts []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.runs == nil {
		f.runs = map[string]*Run{}
		f.runPolls = map[string]int{}
	}

	switch {
	case len(parts) == 3 && parts[0] == "pipelines" && parts[2] == "runs" && r.Method == http.MethodPost:
		var body struct {
			Inputs map[string]string `json:"inputs"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.runInputs = append(f.runInputs, body.Inputs)
		f.nextRun++
		id := fmt.Sprintf("run%03d", f.nextRun)
		run := &Run{RunID: id, WorkflowID: parts[1], Status: RunPending, Inputs: body.Inputs}
		f.runs[id] = run
		_ = json.NewEncoder(w).Encode(*run)

	case len(parts) == 2 && parts[0] == "runs" && r.Method == http.MethodGet:
		run, ok := f.runs[parts[1]]
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		f.runPolls[run.RunID]++
		if f.runPolls[run.RunID] > f.runSteps && !isTerminalRun(run.Status) {
			run.Status = f.runOutcome
			if run.Status == "" {
				run.Status = RunCompleted
			}
		} else if run.Status == RunPending {
			run.Status = RunRunning
		}
		_ = json.NewEncoder(w).Encode(*run)

	default:
		http.Error(w, "no workflows route", http.StatusNotFound)
	}
}

// serveForge implements enough of forge's API to drive the sandbox client.
func (f *fakePlatform) serveForge(w http.ResponseWriter, r *http.Request, parts []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.execs == nil {
		f.execs = map[string]*Execution{}
		f.execPolls = map[string]int{}
		f.failing = map[string]bool{}
	}

	switch {
	case len(parts) == 1 && parts[0] == "images" && r.Method == http.MethodGet:
		_ = json.NewEncoder(w).Encode([]string{"alpine:3.19", "golang:1.25"})

	// Leases. The fake grants them because the real forge does: an agent that
	// cannot hold a container now fails rather than quietly running one container
	// per command, so a fake without this would fail every agent test with an
	// error about the SERVER rather than about the code under test.
	case len(parts) == 1 && parts[0] == "leases" && r.Method == http.MethodPost:
		var spec LeaseSpec
		if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		f.leaseSpecs = append(f.leaseSpecs, spec)
		f.nextLease++
		id := fmt.Sprintf("lease%03d", f.nextLease)
		_ = json.NewEncoder(w).Encode(Lease{LeaseID: id, Status: LeaseReady})

	case len(parts) == 2 && parts[0] == "leases" && r.Method == http.MethodGet:
		_ = json.NewEncoder(w).Encode(Lease{LeaseID: parts[1], Status: LeaseReady})

	case len(parts) == 2 && parts[0] == "leases" && r.Method == http.MethodDelete:
		f.leasesReleased = append(f.leasesReleased, parts[1])
		w.WriteHeader(http.StatusNoContent)

	case len(parts) == 1 && parts[0] == "executions" && r.Method == http.MethodPost:
		var spec SandboxSpec
		if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		f.execSpecs = append(f.execSpecs, spec)
		f.nextExec++
		id := fmt.Sprintf("exec%03d", f.nextExec)
		e := &Execution{ExecutionID: id, Status: ExecPending, RunnerClass: spec.RunnerClass}
		if f.execFailIfScriptContains != "" &&
			strings.Contains(strings.Join(spec.Command, " "), f.execFailIfScriptContains) {
			f.failing[id] = true
		}
		f.execs[id] = e
		_ = json.NewEncoder(w).Encode(*e)

	case len(parts) == 2 && parts[0] == "executions" && r.Method == http.MethodGet:
		e, ok := f.execs[parts[1]]
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		f.execPolls[e.ExecutionID]++
		if f.execPolls[e.ExecutionID] > f.execSteps && !isTerminalExec(e.Status) {
			e.Status = f.execOutcome
			if e.Status == "" {
				e.Status = ExecCompleted
			}
			code := f.execExitCode
			if f.failing[e.ExecutionID] {
				code = 1
			}
			out := f.execStdout
			e.ExitCode, e.Stdout = &code, &out
			e.Outputs = map[string]string{"RESULT": "ok"}
		} else if e.Status == ExecPending {
			e.Status = ExecRunning
		}
		_ = json.NewEncoder(w).Encode(*e)

	case len(parts) == 2 && parts[0] == "executions" && r.Method == http.MethodDelete:
		e, ok := f.execs[parts[1]]
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		e.Status = ExecCancelled
		f.cancelled = append(f.cancelled, e.ExecutionID)
		w.WriteHeader(http.StatusNoContent)

	default:
		http.Error(w, "no forge route", http.StatusNotFound)
	}
}

func newFakePlatform(t *testing.T) (*fakePlatform, *CodeArmory) {
	t.Helper()
	f := &fakePlatform{
		tickets: map[string]*Ticket{},
		// ANCHORED TO NOW, not to a fixed date. The clock exists to give comments
		// distinct, ordered timestamps; the particular date was incidental, and a
		// fixed past one makes every claim it writes look abandoned to the liveness
		// check in pruneStaleClaims.
		now:     time.Now().Add(-time.Minute),
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)

	api, err := NewCodeArmory(srv.URL, "test-token")
	if err != nil {
		t.Fatal(err)
	}
	api.backoff = time.Millisecond // keep retry tests fast
	api.pollMin, api.pollMax = time.Millisecond, 5*time.Millisecond
	return f, api
}

// tick advances the fake clock so comments get distinct, ordered timestamps.
func (f *fakePlatform) tick() time.Time {
	f.now = f.now.Add(time.Second)
	return f.now
}

func (f *fakePlatform) addTicket(t *testing.T, tk Ticket) *Ticket {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if tk.Status == "" {
		// The INBOX is where an unrouted ticket starts, matching the real board.
		// Defaulting to a status no stage takes from would make every dispatcher
		// test silently select nothing.
		tk.Status = ColInbox
	}
	if tk.CreatedAt.IsZero() {
		tk.CreatedAt = f.tick()
	}
	cp := tk
	f.tickets[tk.TicketID] = &cp
	return &cp
}

// addClaim injects a claim comment as though a peer host had written it.
// seedComment appends a plain comment, for setting up a ticket that a pipeline
// stage has already acted on.
func (f *fakePlatform) seedComment(t *testing.T, ticketID, body string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	tk := f.tickets[ticketID]
	if tk == nil {
		t.Fatalf("no such ticket %s", ticketID)
	}
	f.nextCmt++
	tk.Comments = append(tk.Comments, Comment{
		CommentID: fmt.Sprintf("c%03d", f.nextCmt),
		TicketID:  ticketID,
		Body:      body,
	})
}

func (f *fakePlatform) addClaim(t *testing.T, ticketID, host, role string, at time.Time) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	tk := f.tickets[ticketID]
	if tk == nil {
		t.Fatalf("no such ticket %s", ticketID)
	}
	payload, _ := json.Marshal(ClaimToken{Host: host, Role: role})
	f.nextCmt++
	tk.Comments = append(tk.Comments, Comment{
		CommentID: fmt.Sprintf("c%03d", f.nextCmt),
		TicketID:  ticketID,
		AuthorID:  role,
		Body:      claimMarker + string(payload) + " -->",
		CreatedAt: at,
	})
}

func (f *fakePlatform) get(id string) Ticket {
	f.mu.Lock()
	defer f.mu.Unlock()
	return *f.tickets[id]
}

func (f *fakePlatform) commentBodies(id string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.tickets[id].Comments {
		if !strings.HasPrefix(c.Body, claimMarker) {
			out = append(out, c.Body)
		}
	}
	return out
}

func (f *fakePlatform) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	if f.denyAll {
		f.mu.Unlock()
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if f.failNext > 0 {
		f.failNext--
		f.mu.Unlock()
		http.Error(w, "upstream unavailable", http.StatusInternalServerError)
		return
	}
	f.mu.Unlock()

	if r.Header.Get("Authorization") != "Bearer test-token" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	path := strings.Trim(r.URL.Path, "/")
	parts := strings.Split(path, "/")
	w.Header().Set("Content-Type", "application/json")

	// Conductor's real routing: /<service>/<path>. The fake mirrors it exactly,
	// including the doubled "tickets", because a fake that serves a tidier shape
	// than production quietly validates the wrong client.
	// A DIRECT forge connection carries no service prefix, because nothing is
	// routing by service name. The fake serves both shapes because both are real
	// deployments — sandboxes on a local forge, everything else through conductor.
	if len(parts) > 0 && (parts[0] == "executions" || parts[0] == "images") {
		f.serveForge(w, r, parts)
		return
	}
	if len(parts) < 2 {
		http.Error(w, "no route", http.StatusNotFound)
		return
	}
	if parts[0] == "forge" {
		f.serveForge(w, r, parts[1:])
		return
	}
	if parts[0] == "workflows" {
		f.serveWorkflows(w, r, parts[1:])
		return
	}
	if parts[0] != "tickets" {
		http.Error(w, "no route", http.StatusNotFound)
		return
	}
	parts = parts[1:]

	switch {
	case len(parts) == 1 && parts[0] == "tickets" && r.Method == http.MethodGet:
		f.listTickets(w, r)
	case len(parts) == 2 && parts[0] == "tickets" && r.Method == http.MethodGet:
		f.getTicket(w, parts[1])
	case len(parts) == 2 && parts[0] == "tickets" && r.Method == http.MethodPut:
		f.updateTicket(w, r, parts[1])
	case len(parts) == 2 && parts[0] == "tickets" && r.Method == http.MethodDelete:
		f.deleteTicket(w, parts[1])
	// CREATE is POST /tickets/tickets — one path segment after the service
	// prefix. The route used to require two, so every create 404'd against this
	// fake and nothing noticed until the product manager began opening tickets.
	case len(parts) == 1 && parts[0] == "tickets" && r.Method == http.MethodPost:
		f.createTicket(w, r)
	case len(parts) == 3 && parts[0] == "tickets" && parts[2] == "dependencies" && r.Method == http.MethodPost:
		f.addDependency(w, r, parts[1])
	case len(parts) == 3 && parts[0] == "tickets" && parts[2] == "comments" && r.Method == http.MethodPost:
		f.addComment(w, r, parts[1])
	default:
		http.Error(w, "no route", http.StatusNotFound)
	}
}

func (f *fakePlatform) createTicket(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var in Ticket
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	f.nextCmt++
	in.TicketID = fmt.Sprintf("gen%03d", f.nextCmt)
	// The REQUESTED column is honoured. The fake used to force every new ticket
	// to one status, which would hide the product manager opening its children
	// straight into ready_for_dev — the behaviour most worth testing.
	if in.Status == "" {
		in.Status = ColInbox
	}
	in.CreatedAt = f.tick()
	cp := in
	f.tickets[in.TicketID] = &cp
	_ = json.NewEncoder(w).Encode(cp)
}

// addDependency records an edge. The fake mirrors the real service's shape:
// the edge is stored, and the blocker's STATUS is filled in on read — which is
// what lets a test prove a stage waits for unfinished work.
func (f *fakePlatform) addDependency(w http.ResponseWriter, r *http.Request, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var body struct {
		DependsOn string `json:"depends_on"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if f.deps == nil {
		f.deps = map[string][]string{}
	}
	f.deps[id] = append(f.deps[id], body.DependsOn)
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"ticket_id": id})
}

// withDependencies fills DependsOn the way the real service does — on LISTINGS
// as well as reads, which is the difference that lets a dispatcher judge
// readiness from one poll.
func (f *fakePlatform) withDependencies(t Ticket) Ticket {
	t.DependsOn = []TicketDependency{}
	for _, id := range f.deps[t.TicketID] {
		d := TicketDependency{TicketID: id}
		if blocker, ok := f.tickets[id]; ok {
			d.Title, d.Status = blocker.Title, blocker.Status
		}
		t.DependsOn = append(t.DependsOn, d)
	}
	return t
}

func (f *fakePlatform) deleteTicket(w http.ResponseWriter, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.tickets[id]; !ok {
		http.Error(w, "ticket not found", http.StatusNotFound)
		return
	}
	delete(f.tickets, id)
	w.WriteHeader(http.StatusNoContent)
}

func (f *fakePlatform) listTickets(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	status := r.URL.Query().Get("status")
	out := []Ticket{}
	for _, t := range f.tickets {
		if status != "" && t.Status != status {
			continue
		}
		// A LISTING CARRIES NO COMMENTS, matching the real service, which returns
		// an empty array for them. Serving them here would confirm the client's
		// assumption instead of testing it: every pipeline marker is a comment, so
		// a fake that includes them hides the fact that Wants and the attempt
		// ceiling see nothing at selection time.
		listed := *t
		listed.Comments = []Comment{}
		// DEPENDENCIES ARE PRESENT, though comments are not — the same asymmetry
		// the real service has, and the reason a stage can decide readiness from
		// one listing instead of a fetch per ticket.
		listed = f.withDependencies(listed)
		out = append(out, listed)
	}
	_ = json.NewEncoder(w).Encode(out)
}

// ticketReads reports how many single-ticket GETs have been served.
func (f *fakePlatform) ticketReads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}

func (f *fakePlatform) getTicket(w http.ResponseWriter, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	t, ok := f.tickets[id]
	if !ok {
		http.Error(w, "ticket not found", http.StatusNotFound)
		return
	}
	if !f.noETag {
		w.Header().Set("ETag", `"`+strconv.FormatInt(t.Version, 10)+`"`)
	}
	_ = json.NewEncoder(w).Encode(f.withDependencies(*t))
}

func (f *fakePlatform) updateTicket(w http.ResponseWriter, r *http.Request, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tickets[id]
	if !ok {
		http.Error(w, "ticket not found", http.StatusNotFound)
		return
	}
	var up TicketUpdate
	if err := json.NewDecoder(r.Body).Decode(&up); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	// Optimistic concurrency, mirroring the tickets service.
	if im := strings.TrimSpace(r.Header.Get("If-Match")); im != "" && im != "*" {
		want, err := strconv.ParseInt(strings.Trim(im, `"`), 10, 64)
		if err != nil {
			http.Error(w, "bad If-Match", http.StatusBadRequest)
			return
		}
		if want != t.Version {
			w.Header().Set("ETag", `"`+strconv.FormatInt(t.Version, 10)+`"`)
			http.Error(w, "version conflict", http.StatusPreconditionFailed)
			return
		}
	}
	t.Version++
	if up.Status != "" {
		t.Status = up.Status
	}
	if up.Priority != "" {
		t.Priority = up.Priority
	}
	if up.Title != "" {
		t.Title = up.Title
	}
	if up.Description != "" {
		t.Description = up.Description
	}
	t.UpdatedAt = f.tick()
	if !f.noETag {
		w.Header().Set("ETag", `"`+strconv.FormatInt(t.Version, 10)+`"`)
	}
	_ = json.NewEncoder(w).Encode(*t)
}

func (f *fakePlatform) addComment(w http.ResponseWriter, r *http.Request, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tickets[id]
	if !ok {
		http.Error(w, "ticket not found", http.StatusNotFound)
		return
	}
	var body struct {
		Body string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	f.nextCmt++
	c := Comment{
		CommentID: fmt.Sprintf("c%03d", f.nextCmt),
		TicketID:  id,
		AuthorID:  "pm-agent",
		Body:      body.Body,
		CreatedAt: f.tick(),
	}
	t.Comments = append(t.Comments, c)
	_ = json.NewEncoder(w).Encode(c)
}
