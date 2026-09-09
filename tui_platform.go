package main

// Portal-parity read client for the TUI. The bare-command TUI grew up as a
// run-watcher over the local sandbox plane (see platform.Local in cli.go); this
// adds READ access to the CodeArmory PLATFORM — the same data the web portal
// shows — so the terminal can browse pipelines/runs, repos/PRs and the agent
// roles without leaving for a browser.
//
// It routes through conductor exactly as the portal does: by SERVICE NAME, so
// every path carries the service's prefix (/workflows, /codearmory_git_factory,
// /blacksmith, /tickets). The base URL and credential are the platform's
// (CODEARMORY_URL / CODEARMORY_TOKEN — Config.PlatformURL/PlatformToken), NOT the
// sandbox plane's; the two are different trust domains (see the platform package
// comment). A host without a platform configured simply has empty views — Do
// reports "no platform is configured on this host" and the section shows the
// error rather than pretending there is no work.

import (
	"context"

	"github.com/code-armory-app/blacksmith/internal/transport"
)

// platformAPI is a read-only view of the platform gateway for the TUI sections.
type platformAPI struct{ http *transport.Client }

// newPlatformAPI builds the client against the platform base URL (through
// conductor) with the platform credential.
func newPlatformAPI(baseURL string, cred transport.Credential) *platformAPI {
	return &platformAPI{http: transport.New("platform", baseURL, cred)}
}

// configured reports whether a platform is reachable; the TUI uses it to show
// "no platform configured" instead of an empty list on a standalone host.
func (p *platformAPI) configured() bool { return p != nil && p.http.Configured() }

func (p *platformAPI) get(ctx context.Context, path string, out any) error {
	return p.http.Do(ctx, transport.Request{Method: "GET", Path: path, Out: out})
}

// ── Pipelines & runs (/workflows) ────────────────────────────────────────────

// Pipeline is one workflow definition (the list view omits steps).
type Pipeline struct {
	ID          string `json:"workflow_id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// Run is one pipeline execution.
type Run struct {
	ID          string `json:"run_id"`
	WorkflowID  string `json:"workflow_id"`
	Project     string `json:"project"`
	Status      string `json:"status"`
	CurrentStep int    `json:"current_step"`
	CreatedAt   string `json:"created_at"`
}

// Pipelines lists the workflow definitions.
func (p *platformAPI) Pipelines(ctx context.Context) ([]Pipeline, error) {
	var out []Pipeline
	return out, p.get(ctx, "/workflows/pipelines", &out)
}

// Runs lists recent pipeline runs, newest first (server order).
func (p *platformAPI) Runs(ctx context.Context) ([]Run, error) {
	var out []Run
	return out, p.get(ctx, "/workflows/runs", &out)
}

// Run fetches one run by id (status + current step for the progress view).
func (p *platformAPI) Run(ctx context.Context, id string) (Run, error) {
	var out Run
	return out, p.get(ctx, "/workflows/runs/"+id, &out)
}

// ── Repos & pull requests (/codearmory_git_factory) ──────────────────────────

// Repo is one git-factory repository.
type Repo struct {
	ID            string `json:"id"`
	Namespace     string `json:"namespace"`
	Name          string `json:"name"`
	Description   string `json:"description"`
	DefaultBranch string `json:"default_branch"`
	Visibility    string `json:"visibility"`
}

// Pull is one pull request on a repo.
type Pull struct {
	Number    int    `json:"number"`
	Title     string `json:"title"`
	SourceRef string `json:"source_ref"`
	TargetRef string `json:"target_ref"`
	State     string `json:"state"`
	Author    string `json:"author"`
}

// Repos lists the repositories the caller can see.
func (p *platformAPI) Repos(ctx context.Context) ([]Repo, error) {
	var out []Repo
	return out, p.get(ctx, "/codearmory_git_factory/repos", &out)
}

// Pulls lists a repo's pull requests.
func (p *platformAPI) Pulls(ctx context.Context, repoID string) ([]Pull, error) {
	var out []Pull
	return out, p.get(ctx, "/codearmory_git_factory/repos/"+repoID+"/pulls", &out)
}

// ── Agents / roles (/blacksmith) ─────────────────────────────────────────────

// Role is one agent role (the blacksmith harness's configurable stage). Fields
// mirror internal/roles.Role's JSON.
type Role struct {
	Name          string   `json:"name"`
	Class         string   `json:"class"`
	Prompt        string   `json:"prompt"`
	Check         string   `json:"check"`
	TicketKind    string   `json:"ticket_kind"`
	Tools         []string `json:"tools"`
	MaxIterations int      `json:"max_iterations"`
}

// Roles lists the agent roles.
func (p *platformAPI) Roles(ctx context.Context) ([]Role, error) {
	var out []Role
	return out, p.get(ctx, "/blacksmith/roles", &out)
}

// Role fetches one role by name (the full prompt/guard/check for the detail view).
func (p *platformAPI) Role(ctx context.Context, name string) (Role, error) {
	var out Role
	return out, p.get(ctx, "/blacksmith/roles/"+name, &out)
}
