package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/code-armory-app/blacksmith/internal/transport"
)

// A fake platform gateway that answers the service-prefixed paths the portal
// uses, so the client's routing and JSON decoding are pinned.
func fakePlatform(t *testing.T) *platformAPI {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/workflows/pipelines", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`[{"workflow_id":"w1","name":"agent-chain","description":"orchestrator"}]`))
	})
	mux.HandleFunc("/workflows/runs", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`[{"run_id":"r1","workflow_id":"w1","project":"ops","status":"completed","current_step":3}]`))
	})
	mux.HandleFunc("/workflows/runs/r1", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"run_id":"r1","workflow_id":"w1","status":"running","current_step":2}`))
	})
	mux.HandleFunc("/codearmory_git_factory/repos", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`[{"id":"g1","namespace":"ops","name":"task-tracker","default_branch":"main","visibility":"private"}]`))
	})
	mux.HandleFunc("/codearmory_git_factory/repos/g1/pulls", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`[{"number":1,"title":"feat: backend","source_ref":"feat/x","target_ref":"dev","state":"open","author":"u1"}]`))
	})
	mux.HandleFunc("/blacksmith/roles", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`[{"name":"developer","class":"large","check":"go test ./...","tools":["read_files","write_file"],"max_iterations":60}]`))
	})
	mux.HandleFunc("/blacksmith/roles/developer", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"name":"developer","class":"large","prompt":"You are a Go developer.","check":"go test ./..."}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return newPlatformAPI(srv.URL, transport.Static("tok"))
}

func TestPlatformAPI_Reads(t *testing.T) {
	api := fakePlatform(t)
	ctx := context.Background()

	pipes, err := api.Pipelines(ctx)
	if err != nil || len(pipes) != 1 || pipes[0].Name != "agent-chain" {
		t.Fatalf("Pipelines: %v %+v", err, pipes)
	}
	runs, err := api.Runs(ctx)
	if err != nil || len(runs) != 1 || runs[0].Status != "completed" || runs[0].CurrentStep != 3 {
		t.Fatalf("Runs: %v %+v", err, runs)
	}
	run, err := api.Run(ctx, "r1")
	if err != nil || run.Status != "running" || run.CurrentStep != 2 {
		t.Fatalf("Run: %v %+v", err, run)
	}
	repos, err := api.Repos(ctx)
	if err != nil || len(repos) != 1 || repos[0].Namespace != "ops" || repos[0].Name != "task-tracker" {
		t.Fatalf("Repos: %v %+v", err, repos)
	}
	pulls, err := api.Pulls(ctx, "g1")
	if err != nil || len(pulls) != 1 || pulls[0].Number != 1 || pulls[0].TargetRef != "dev" {
		t.Fatalf("Pulls: %v %+v", err, pulls)
	}
	roles, err := api.Roles(ctx)
	if err != nil || len(roles) != 1 || roles[0].Name != "developer" || len(roles[0].Tools) != 2 {
		t.Fatalf("Roles: %v %+v", err, roles)
	}
	role, err := api.Role(ctx, "developer")
	if err != nil || role.Prompt == "" {
		t.Fatalf("Role: %v %+v", err, role)
	}
}

func TestPlatformAPI_Unconfigured(t *testing.T) {
	api := newPlatformAPI("", transport.Static("tok"))
	if api.configured() {
		t.Fatal("empty base URL should not be configured")
	}
	if _, err := api.Pipelines(context.Background()); err == nil {
		t.Fatal("expected an error from an unconfigured platform")
	}
}
