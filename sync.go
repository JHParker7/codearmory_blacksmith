package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// Mirror sync: keep a local git server in step with an upstream, so the
// department works while the platform is down and so blacksmith is usable by
// someone who does not run CodeArmory at all.
//
// THE REFSPEC IS THE SECURITY CONTROL, not the local server's authentication.
//
// The untrusted party is the sandbox: it runs model-authored code and it can
// reach the local git server by design, holding a legitimate credential. So no
// amount of auth on the local side prevents a prompt-injected agent rewriting a
// branch there. What prevents that becoming an upstream compromise is bounding
// what the SYNCER is willing to push:
//
//   - local main goes to the upstream DEV branch, never to upstream main. The
//     names need not match; the mapping is a refspec (main:dev) and lives here.
//   - agent branches go to agent branches, prefix-matched.
//   - nothing else. No tags, no force-updates, no wildcards that could widen
//     later without someone noticing.
//
// Promotion to upstream main stays what it is for everyone else: a pipeline run
// and a human. Nothing in blacksmith is authorised to make that decision.
//
// Pulling DOWN is unrestricted — that direction cannot escalate.

// SyncConfig describes one mirrored repository.
type SyncConfig struct {
	// LocalURL is the bare repo on the local git server.
	LocalURL string
	// UpstreamRef is a forge credential reference for the upstream remote. The
	// syncer runs in a sandbox, so blacksmith itself never holds a git
	// credential.
	UpstreamRef string
	// UpstreamURL is the remote the local mirror syncs with — the cluster's git
	// host, GitHub, GitLab; the syncer does not care which.
	UpstreamURL string
	// LocalBranch is the branch agents work on locally. Usually "main"; it does
	// NOT have to match the upstream target.
	LocalBranch string
	// UpstreamBranch is where local work lands upstream — "dev" or "exp". It
	// must never be the upstream's main.
	UpstreamBranch string
	// BranchPrefix scopes which agent branches may be pushed.
	BranchPrefix string
	// Interval is how often to sync.
	Interval time.Duration
	// Image and RunnerClass select the sandbox the git commands run in.
	Image       string
	RunnerClass string
}

// protectedUpstream are branch names the syncer refuses to write, whatever it is
// configured with. A typo in configuration should not be able to point agent
// output at a release branch.
var protectedUpstream = []string{"main", "master", "release", "prod", "production"}

// Validate rejects a configuration that could push where it should not.
func (c SyncConfig) Validate() error {
	switch {
	case c.LocalURL == "":
		return errors.New("sync: no local repository")
	case c.UpstreamURL == "":
		return errors.New("sync: no upstream repository")
	case c.LocalBranch == "":
		return errors.New("sync: no local branch")
	case c.UpstreamBranch == "":
		return errors.New("sync: no upstream branch")
	}
	for _, p := range protectedUpstream {
		if strings.EqualFold(c.UpstreamBranch, p) {
			return fmt.Errorf("sync: refusing to target upstream %q; agent work lands on a development branch and is promoted by a person", c.UpstreamBranch)
		}
	}
	if strings.ContainsAny(c.UpstreamBranch, "*?[") || strings.ContainsAny(c.BranchPrefix, "*?[") {
		return errors.New("sync: branch names must be literal, not patterns")
	}
	return nil
}

// PushRefspecs is the complete set of things this syncer may push upstream.
//
// Enumerated rather than derived at the call site so it can be read, tested and
// reviewed on its own — the question "what can the agent plane write upstream?"
// should have an answer someone can check in one place.
func (c SyncConfig) PushRefspecs() []string {
	prefix := c.BranchPrefix
	if prefix == "" {
		prefix = "agent/"
	}
	return []string{
		// Local work → the upstream development branch. Not a force push: if
		// upstream has moved, the sync fails and a person looks, which is the
		// correct outcome for a branch other people also write to.
		"refs/heads/" + c.LocalBranch + ":refs/heads/" + c.UpstreamBranch,
		// Agent branches, scoped to the prefix.
		"refs/heads/" + prefix + "*:refs/heads/" + prefix + "*",
	}
}

// Syncer keeps one repository in step.
type Syncer struct {
	api *CodeArmory
	rec *Recorder
	cfg SyncConfig
}

func NewSyncer(api *CodeArmory, rec *Recorder, cfg SyncConfig) (*Syncer, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 5 * time.Minute
	}
	return &Syncer{api: api, rec: rec, cfg: cfg}, nil
}

// Run syncs on a timer until ctx ends.
func (s *Syncer) Run(ctx context.Context) error {
	t := time.NewTicker(s.cfg.Interval)
	defer t.Stop()
	for {
		if err := s.Sync(ctx); err != nil && !errors.Is(err, context.Canceled) {
			// A failed sync is not fatal. The local plane keeps working — that is
			// the entire point of mirroring — and the next tick retries.
			slog.WarnContext(ctx, "mirror sync failed; will retry", "repo", s.cfg.LocalURL, "error", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// Sync pulls upstream down and pushes the permitted refs up.
func (s *Syncer) Sync(ctx context.Context) error {
	spec := SandboxSpec{
		Image:       s.cfg.Image,
		Command:     []string{"sh", "-c", s.script()},
		RunnerClass: s.cfg.RunnerClass,
		TimeoutSecs: 600,
	}
	if s.cfg.UpstreamRef != "" {
		spec.SecretRefs = map[string]string{"UPSTREAM_URL": s.cfg.UpstreamRef}
	}

	res, err := s.api.RunSandbox(ctx, s.rec, spec)
	if err != nil {
		return err
	}
	if !res.OK() {
		return fmt.Errorf("sync exit %d: %s", res.ExitCode, clip(res.Stderr+res.Stdout, 1000))
	}
	return nil
}

// script builds the git commands. The push refspecs come from PushRefspecs so
// there is exactly one place that decides what may leave the local plane.
func (s *Syncer) script() string {
	var b strings.Builder
	b.WriteString(sandboxPreamble)
	fmt.Fprintf(&b, `UPSTREAM="${UPSTREAM_URL:-%s}"
git clone --mirror %q mirror >/dev/null 2>&1
cd mirror
# Pull everything down: this direction cannot escalate.
git fetch --prune "$UPSTREAM" "+refs/heads/*:refs/remotes/upstream/*" >/dev/null 2>&1
`, s.cfg.UpstreamURL, s.cfg.LocalURL)

	for _, spec := range s.cfg.PushRefspecs() {
		// No --force anywhere: a rejected non-fast-forward is a person's problem
		// to look at, not something to overwrite.
		fmt.Fprintf(&b, "git push \"$UPSTREAM\" %q\n", spec)
	}
	return b.String()
}
