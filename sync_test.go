package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

func syncCfg() SyncConfig {
	return SyncConfig{
		LocalURL: "http://git-local/repo.git", UpstreamURL: "https://upstream/org/repo.git",
		LocalBranch: "main", UpstreamBranch: "dev", BranchPrefix: "agent/",
		Image: "alpine:3.19",
	}
}

// ── unit: the security control ────────────────────────────────────────────────

// The refspecs are the whole boundary. "What can the agent plane write
// upstream?" must have an answer that can be read in one place.
func TestPushRefspecsAreBounded(t *testing.T) {
	got := syncCfg().PushRefspecs()
	want := []string{
		"refs/heads/main:refs/heads/dev",
		"refs/heads/agent/*:refs/heads/agent/*",
	}
	if len(got) != len(want) {
		t.Fatalf("PushRefspecs() = %v, want exactly %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("refspec %d = %q, want %q", i, got[i], want[i])
		}
	}
	// The failure that matters: nothing may target upstream main, and nothing
	// may push tags.
	for _, spec := range got {
		_, dst, _ := strings.Cut(spec, ":")
		if dst == "refs/heads/main" || dst == "refs/heads/master" {
			t.Errorf("refspec %q targets upstream main", spec)
		}
		if strings.Contains(spec, "refs/tags") {
			t.Errorf("refspec %q pushes tags", spec)
		}
	}
}

// Local and upstream branch names are independent — the mapping is a refspec,
// not a naming constraint.
func TestPushRefspecsMapAcrossDifferentNames(t *testing.T) {
	c := syncCfg()
	c.LocalBranch, c.UpstreamBranch = "main", "exp"
	if got := c.PushRefspecs()[0]; got != "refs/heads/main:refs/heads/exp" {
		t.Errorf("refspec = %q, want main mapped onto exp", got)
	}
}

// A typo in configuration must not be able to point agent output at a release
// branch.
func TestValidateRefusesProtectedUpstreamBranches(t *testing.T) {
	for _, bad := range []string{"main", "Main", "MASTER", "release", "prod", "production"} {
		c := syncCfg()
		c.UpstreamBranch = bad
		if err := c.Validate(); err == nil {
			t.Errorf("Validate() accepted upstream branch %q", bad)
		}
	}
	if err := syncCfg().Validate(); err != nil {
		t.Errorf("Validate() rejected a valid config: %v", err)
	}
}

// A pattern where a literal branch is expected could widen the push silently.
func TestValidateRefusesPatterns(t *testing.T) {
	for _, c := range []SyncConfig{
		func() SyncConfig { x := syncCfg(); x.UpstreamBranch = "dev*"; return x }(),
		func() SyncConfig { x := syncCfg(); x.BranchPrefix = "*"; return x }(),
	} {
		if err := c.Validate(); err == nil {
			t.Error("Validate() accepted a wildcard where a literal branch is required")
		}
	}
}

func TestValidateRequiresBothEnds(t *testing.T) {
	for name, mut := range map[string]func(*SyncConfig){
		"no local":    func(c *SyncConfig) { c.LocalURL = "" },
		"no upstream": func(c *SyncConfig) { c.UpstreamURL = "" },
		"no local br": func(c *SyncConfig) { c.LocalBranch = "" },
		"no up br":    func(c *SyncConfig) { c.UpstreamBranch = "" },
	} {
		c := syncCfg()
		mut(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("Validate() accepted a config with %s", name)
		}
	}
}

// A force push would let a compromised local plane rewrite upstream history;
// a rejected non-fast-forward is a person's problem to look at.
func TestSyncScriptNeverForcePushes(t *testing.T) {
	s, err := NewSyncer(nil, nil, syncCfg())
	if err != nil {
		t.Fatal(err)
	}
	script := s.script()
	for _, forbidden := range []string{"--force", "-f ", "+refs/heads/main"} {
		if strings.Contains(script, forbidden) {
			t.Errorf("sync script contains %q:\n%s", forbidden, script)
		}
	}
	// Fetching down IS allowed to be forced; that direction cannot escalate.
	if !strings.Contains(script, "+refs/heads/*:refs/remotes/upstream/*") {
		t.Error("sync script does not fetch upstream refs")
	}
	if !strings.Contains(script, "refs/heads/main:refs/heads/dev") {
		t.Error("sync script does not push local main to upstream dev")
	}
}

func TestNewSyncerRejectsBadConfig(t *testing.T) {
	c := syncCfg()
	c.UpstreamBranch = "main"
	if _, err := NewSyncer(nil, nil, c); err == nil {
		t.Error("NewSyncer accepted a config targeting upstream main")
	}
}

// ── integration ───────────────────────────────────────────────────────────────

func TestSyncRunsInASandboxWithTheCredentialAsASecretRef(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	rec, _ := newTestRecorder(t)
	ctx := rec.Start(context.Background(), "tr", "sync", "sync")

	c := syncCfg()
	c.UpstreamRef = "git:https://upstream/org/repo.git"
	s, err := NewSyncer(api, rec, c)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Sync(ctx); err != nil {
		t.Fatalf("Sync() = %v", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.execSpecs) != 1 {
		t.Fatalf("submitted %d executions, want 1", len(f.execSpecs))
	}
	spec := f.execSpecs[0]
	// blacksmith never holds a git credential; forge resolves the reference.
	if spec.SecretRefs["UPSTREAM_URL"] != c.UpstreamRef {
		t.Errorf("secret_refs = %v, want the upstream reference passed through", spec.SecretRefs)
	}
	joined := strings.Join(spec.Command, " ")
	if strings.Contains(joined, "--force") {
		t.Error("the sync command force-pushes")
	}
	if !strings.Contains(joined, "refs/heads/main:refs/heads/dev") {
		t.Errorf("the sync command does not push main to dev:\n%s", joined)
	}
}

// A failed sync must not stop the loop: the local plane keeps working, which is
// the entire point of mirroring.
func TestSyncFailureIsNotFatal(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.execExitCode = 1
	f.execStdout = "! [rejected] main -> dev (non-fast-forward)"

	rec, _ := newTestRecorder(t)
	s, err := NewSyncer(api, rec, syncCfg())
	if err != nil {
		t.Fatal(err)
	}
	ctx := rec.Start(context.Background(), "tr", "sync", "sync")

	if err := s.Sync(ctx); err == nil {
		t.Fatal("Sync() = nil error on a rejected push")
	}

	// Run() absorbs it and keeps going.
	s.cfg.Interval = 10 * time.Millisecond
	runCtx, cancel := context.WithTimeout(ctx, 120*time.Millisecond)
	defer cancel()
	if err := s.Run(runCtx); err == nil {
		t.Error("Run() returned without a context error")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.execSpecs) < 2 {
		t.Errorf("attempted %d syncs, want the loop to have retried after a failure", len(f.execSpecs))
	}
}
