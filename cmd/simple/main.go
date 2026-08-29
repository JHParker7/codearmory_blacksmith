// Command simple runs the rebuilt pipeline over a directory, one stage at a
// time.
//
// THE SIMPLE SHAPE, next to the full department in the root main.go rather than
// replacing it. A stage's input here is the tree the previous stage left behind,
// there is no board and no claiming, and the whole run is one process working
// one task. That is enough to exercise internal/agents and internal/tools end to
// end, which is what the rebuild needed first; internal/dispatch is what this
// grows back into once several hosts have to share a board.
//
// Usage:
//
//	simple -repo ./test_repo -task "build a task tracker"
//	simple -repo ./test_repo -task "..." -roles architect,spec,dev
package main

import (
	"context"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/code-armory-app/blacksmith/internal/agents"
	"github.com/code-armory-app/blacksmith/internal/config"
	"github.com/code-armory-app/blacksmith/internal/forge"
	"github.com/code-armory-app/blacksmith/internal/model"
	"github.com/code-armory-app/blacksmith/internal/tools"
	"github.com/code-armory-app/blacksmith/internal/transport"
)

func main() {
	repoDir := flag.String("repo", "", "directory the agents read and write (required)")
	task := flag.String("task", "", "what to build (required)")
	only := flag.String("roles", "", "comma-separated stages to run; default is the whole pipeline")
	dryRun := flag.Bool("dry-run", false, "print the plan and the wiring, then stop")
	single := flag.Bool("single", false,
		"run ONE unrestricted agent instead of the pipeline, as a baseline to measure against")
	flag.Parse()

	if err := run(*repoDir, *task, *only, *dryRun, *single); err != nil {
		fmt.Fprintln(os.Stderr, "error: "+err.Error())
		os.Exit(1)
	}
}

func run(repoDir, task, only string, dryRun, single bool) error {
	if repoDir == "" || task == "" {
		return fmt.Errorf("-repo and -task are both required")
	}

	config.LoadOperatorEnv()
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}

	stages, err := selectStages(only)
	if err != nil {
		return err
	}
	// THE BASELINE IS ONE STAGE THAT IS NOT ONE OF THE STAGES. Handled by
	// swapping the list rather than by a second code path, so the preflight, the
	// sandbox and the write-back below are the same ones the pipeline uses — the
	// two things being compared must not run through different plumbing.
	if single {
		if only != "" {
			return fmt.Errorf("-single runs one agent and -roles selects several; use one or the other")
		}
		stages = []string{stageBaseline}
	}

	files, err := readTree(repoDir)
	if err != nil {
		return fmt.Errorf("reading %s: %w", repoDir, err)
	}

	// The creator holds what every stage shares. Building one before the sandbox
	// exists is deliberate: the preflight below needs to ask each stage what it
	// wants, and acquiring a lease to answer that would be backwards.
	gw := model.NewGateway(cfg.Host, cfg.Classes)
	maker := agents.Creator{
		Gateway: gw,
		Check:   cfg.Repo.TestCommand,
		Log:     func(line string) { slog.Info(line) },
	}

	// EVERY STAGE IS INSPECTED BEFORE ANY OF THEM RUNS. A pipeline that gets four
	// stages in and then finds it cannot serve the fifth has spent the first four
	// for nothing, and on a run that takes an hour that is the whole hour.
	var missing []string
	var wantsSandbox bool
	for _, name := range stages {
		a, err := stage(maker, name, nil)
		if err != nil {
			return err
		}
		if a.Class() != model.ClassNone && !gw.Serves(a.Class()) {
			missing = append(missing, fmt.Sprintf("%s needs class %q", a.Name(), a.Class()))
		}
		if a.Check() != "" {
			wantsSandbox = true
		}
		if dryRun {
			fmt.Printf("stage     %-11s class=%-6s check=%s\n", a.Name(), a.Class(), orNone(a.Check()))
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("this host does not serve every class the run needs: %s. Set the "+
			"endpoint for it, or drop the stage with -roles", strings.Join(missing, "; "))
	}

	if dryRun {
		fmt.Printf("repo      %s (%d files)\n", repoDir, len(files))
		fmt.Printf("task      %s\n", task)
		fmt.Printf("forge     %s\n", orNone(cfg.ForgeURL))
		fmt.Printf("image     %s\n", orNone(cfg.Repo.Image))
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// THE SANDBOX IS ACQUIRED ONCE AND SHARED by every stage that has a check. A
	// lease is a runner class's worth of memory on the cluster, and acquiring one
	// per stage would hold several at once for no benefit: the tree of record is
	// in memory here, so a sandbox carries nothing between stages that reusing it
	// would lose.
	if wantsSandbox {
		box, release, err := acquireSandbox(ctx, cfg)
		if err != nil {
			return err
		}
		defer release()
		maker.Sandbox = box
	}

	for _, name := range stages {
		agent, err := stage(maker, name, files)
		if err != nil {
			return err
		}
		slog.Info("stage starting",
			"stage", agent.Name(), "class", string(agent.Class()), "files", len(files))

		outcome, runErr := agent.Run(ctx, task)

		// THE TREE IS CARRIED FORWARD EVEN WHEN THE STAGE FAILED, and written out
		// before anything is reported. What a failed developer wrote is most of the
		// answer, and throwing it away means the next attempt starts from nothing —
		// which is how a run that was nearly finished becomes one that never
		// finishes.
		files = agent.Files()
		if err := writeTree(repoDir, files); err != nil {
			return fmt.Errorf("writing %s: %w", repoDir, err)
		}
		if runErr != nil {
			return fmt.Errorf("stage %s: %w", agent.Name(), runErr)
		}

		slog.Info("stage finished",
			"stage", agent.Name(), "passed", outcome.Passed, "turns", outcome.Iterations)
		if outcome.Answer != "" {
			fmt.Printf("\n--- %s ---\n%s\n", agent.Name(), outcome.Answer)
		}
		if !outcome.Passed {
			// TOLD APART, because they need opposite responses: a stage that spent
			// its budget working may deserve a larger one, while a stage that stopped
			// moving would do the same thing with twice as many turns.
			why := fmt.Sprintf("ran out of budget after %d turns", outcome.Iterations)
			if outcome.Stalled {
				why = fmt.Sprintf("stopped after %d turns having changed nothing in the last %d",
					outcome.Iterations, agents.MaxIdleTurns)
			}
			return fmt.Errorf("stage %s %s and its check never passed. What it last said:\n%s",
				agent.Name(), why, orNone(outcome.LastCheck))
		}
	}

	slog.Info("pipeline finished", "repo", repoDir, "files", len(files))
	return nil
}

// planeCredential is how this host authenticates to its own sandbox plane.
//
// A SESSION RATHER THAN A FIXED TOKEN wherever a password is configured, because
// the local gatekeeper issues SHORT ones. A client that cannot renew works until
// the first expiry and then reports "unauthorized" against a plane that is
// perfectly healthy — which reads as a broken deployment or a wrong URL, and
// sends you to look at the cluster. This cost the first live run of the rebuilt
// pipeline: cmd/simple used the static token alone and forge refused every lease
// with `POST /leases: denied: unauthorized` while the plane was up and serving.
//
// Deliberately the same rule as clients() in the root main.go. Two ways of
// authenticating to one plane is how they drift.
func planeCredential(cfg config.Config) transport.Credential {
	if cfg.ForgeEmail != "" && cfg.ForgePassword != "" {
		session, err := transport.Login(cfg.SandboxLoginURL(), cfg.ForgeEmail, cfg.ForgePassword, nil)
		if err == nil {
			slog.Info("sandbox session renews on expiry",
				"login_url", cfg.SandboxLoginURL(), "email", cfg.ForgeEmail)
			return session
		}
		slog.Warn("could not build a renewing sandbox session; falling back to the static token",
			"error", err)
	}
	if cfg.ForgeToken == "" {
		slog.Warn("no plane credential: set AGENTS_FORGE_EMAIL and AGENTS_FORGE_PASSWORD, " +
			"or AGENTS_FORGE_TOKEN")
	}
	return transport.Static(cfg.ForgeToken)
}

func acquireSandbox(ctx context.Context, cfg config.Config) (tools.Sandbox, func(), error) {
	if cfg.ForgeURL == "" {
		return nil, nil, fmt.Errorf(
			"a stage in this run has a check and there is no sandbox to run it in. Set " +
				"AGENTS_FORGE_URL to the forge on this host")
	}
	client := forge.Local(cfg.ForgeURL, planeCredential(cfg))
	sb, err := client.Acquire(ctx, forge.SandboxSpec{
		Image:           cfg.Repo.Image,
		RunnerClass:     cfg.Repo.RunnerClass,
		TimeoutSecs:     cfg.Repo.TimeoutSecs,
		CloneURL:        cfg.Repo.URL,
		SecretRef:       cfg.Repo.SecretRef,
		Branch:          cfg.Repo.Branch,
		IdleTimeoutSecs: cfg.Repo.LeaseIdleSecs,
		MaxLifetimeSecs: cfg.Repo.LeaseMaxSecs,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("acquiring a sandbox: %w", err)
	}
	// RELEASED ON EVERY EXIT PATH. A held lease is memory on the cluster, and the
	// idle timeout that would reclaim it is a backstop for a crashed process, not
	// a substitute for a run that finished. WithoutCancel because the release must
	// still go out when the run ended on an interrupt.
	return tools.ForgeSandbox{Sandbox: sb}, func() { sb.Release(context.WithoutCancel(ctx)) }, nil
}

// stageBaseline names the unrestricted single agent. Not in agents.Stages(),
// because it is what the pipeline is measured AGAINST rather than part of it.
const stageBaseline = "baseline"

// stage resolves a name to a built agent, including the baseline that the
// pipeline's own dispatcher does not know about.
func stage(c agents.Creator, name string, files map[string]string) (*agents.Agent, error) {
	if name == stageBaseline {
		return c.Baseline(files), nil
	}
	return c.Stage(name, files)
}

func selectStages(only string) ([]string, error) {
	if only == "" {
		return agents.Stages(), nil
	}
	known := map[string]bool{}
	for _, name := range agents.Stages() {
		known[name] = true
	}
	var out []string
	for _, name := range strings.Split(only, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if !known[name] {
			return nil, fmt.Errorf("there is no stage called %q; the stages are %s",
				name, strings.Join(agents.Stages(), ", "))
		}
		out = append(out, name)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("-roles named no stages")
	}
	return out, nil
}

// MaxFileBytes bounds one file read into the workspace.
//
// A tree is held in memory and rendered into prompts, so a single large file is
// not a slow read but a stage that cannot run at all. Refusing it by name beats
// discovering it as a context-length error from the model, which names nothing.
const MaxFileBytes = 1 << 20

func readTree(dir string) (map[string]string, error) {
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			return relErr
		}
		if d.IsDir() {
			// Build output and version control are not source. Walked in, a single
			// search returns thousands of cache files as context.
			switch d.Name() {
			case ".git", "node_modules", "vendor", ".gocache":
				if rel != "." {
					return fs.SkipDir
				}
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Size() > MaxFileBytes {
			slog.Warn("skipping a file too large to hold in a prompt",
				"path", rel, "bytes", info.Size(), "limit", MaxFileBytes)
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = string(b)
		return nil
	})
	return files, err
}

func writeTree(dir string, files map[string]string) error {
	for rel, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}
