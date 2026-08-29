// Command simple runs the rebuilt pipeline over a directory, one role at a time.
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
	only := flag.String("roles", "", "comma-separated roles to run; default is the whole pipeline")
	dryRun := flag.Bool("dry-run", false, "print the plan and the wiring, then stop")
	flag.Parse()

	if err := run(*repoDir, *task, *only, *dryRun); err != nil {
		fmt.Fprintln(os.Stderr, "error: "+err.Error())
		os.Exit(1)
	}
}

func run(repoDir, task, only string, dryRun bool) error {
	if repoDir == "" || task == "" {
		return fmt.Errorf("-repo and -task are both required")
	}

	config.LoadOperatorEnv()
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}

	roles, err := selectRoles(only)
	if err != nil {
		return err
	}

	// EVERY ROLE'S CLASS IS CHECKED BEFORE ANY OF THEM RUNS. A pipeline that gets
	// four stages in and then discovers it cannot serve the fifth has spent the
	// first four for nothing, and on a run that takes an hour that is the whole
	// hour.
	gw := model.NewGateway(cfg.Host, cfg.Classes)
	var missing []string
	for _, r := range roles {
		if r.Class != model.ClassNone && !gw.Serves(r.Class) {
			missing = append(missing, fmt.Sprintf("%s needs class %q", r.Name, r.Class))
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("this host does not serve every class the run needs: %s. Set the "+
			"endpoint for it, or drop the stage with -roles", strings.Join(missing, "; "))
	}

	files, err := readTree(repoDir)
	if err != nil {
		return fmt.Errorf("reading %s: %w", repoDir, err)
	}

	if dryRun {
		fmt.Printf("repo      %s (%d files)\n", repoDir, len(files))
		fmt.Printf("task      %s\n", task)
		fmt.Printf("forge     %s\n", orNone(cfg.ForgeURL))
		fmt.Printf("image     %s\n", orNone(cfg.Repo.Image))
		for _, r := range roles {
			fmt.Printf("role      %-11s class=%-6s check=%s\n", r.Name, r.Class, orNone(r.Check))
		}
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// THE SANDBOX IS ACQUIRED ONCE AND SHARED by every stage that has a check.
	// A lease is a runner class's worth of memory on the cluster and acquiring one
	// per stage would hold several at once for no benefit: the tree of record is
	// in memory here, so a sandbox carries nothing between stages that would be
	// lost by reusing it.
	var box tools.Sandbox
	if needsSandbox(roles) {
		sb, release, err := acquireSandbox(ctx, cfg)
		if err != nil {
			return err
		}
		defer release()
		box = sb
	}

	for _, role := range roles {
		slog.Info("stage starting", "role", role.Name, "class", string(role.Class), "files", len(files))

		ws := tools.NewWorkspace(files, role.Guard)
		set := &tools.Set{
			Workspace: ws,
			Sandbox:   box,
			Check:     checkFor(role, cfg),
			Names:     role.ToolNames,
		}
		agent := &agents.Agent{
			Role:    role,
			Tools:   set,
			Gateway: gw,
			Log:     func(line string) { slog.Info(line) },
		}

		outcome, err := agent.Run(ctx, task)
		// THE TREE IS CARRIED FORWARD EVEN WHEN THE STAGE FAILED. What a failed
		// developer wrote is most of the answer, and throwing it away means the next
		// attempt starts from nothing — which is how a run that was nearly finished
		// becomes a run that never finishes.
		files = ws.Files()
		if err != nil {
			return fmt.Errorf("stage %s: %w", role.Name, err)
		}

		slog.Info("stage finished",
			"role", role.Name, "passed", outcome.Passed, "turns", outcome.Iterations)
		if outcome.Answer != "" {
			fmt.Printf("\n--- %s ---\n%s\n", role.Name, outcome.Answer)
		}
		if !outcome.Passed {
			if err := writeTree(repoDir, files); err != nil {
				return fmt.Errorf("writing %s: %w", repoDir, err)
			}
			return fmt.Errorf("stage %s ran out of budget after %d turns and its check never "+
				"passed. What it last said:\n%s", role.Name, outcome.Iterations, outcome.LastCheck)
		}

		if err := writeTree(repoDir, files); err != nil {
			return fmt.Errorf("writing %s: %w", repoDir, err)
		}
	}

	slog.Info("pipeline finished", "repo", repoDir, "files", len(files))
	return nil
}

// checkFor resolves a role's check command, preferring what the operator
// configured for this repository over the role's own default.
//
// The default exists so the pipeline runs on a host that configured nothing; the
// override exists because "does this work" is a property of the repository, not
// of the stage looking at it.
func checkFor(r agents.Role, cfg config.Config) string {
	if r.Check == "" {
		return ""
	}
	if cfg.Repo.TestCommand != "" {
		return cfg.Repo.TestCommand
	}
	return r.Check
}

func needsSandbox(roles []agents.Role) bool {
	for _, r := range roles {
		if r.Check != "" {
			return true
		}
	}
	return false
}

func acquireSandbox(ctx context.Context, cfg config.Config) (tools.Sandbox, func(), error) {
	if cfg.ForgeURL == "" {
		return nil, nil, fmt.Errorf(
			"a stage in this run has a check and there is no sandbox to run it in. Set " +
				"AGENTS_FORGE_URL to the forge on this host")
	}
	client := forge.Local(cfg.ForgeURL, transport.Static(cfg.ForgeToken))
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
	// a substitute for a run that finished.
	return tools.ForgeSandbox{Sandbox: sb}, func() { sb.Release(context.WithoutCancel(ctx)) }, nil
}

func selectRoles(only string) ([]agents.Role, error) {
	all := agents.Pipeline()
	if only == "" {
		return all, nil
	}
	byName := map[string]agents.Role{}
	for _, r := range all {
		byName[r.Name] = r
	}
	var out []agents.Role
	for _, name := range strings.Split(only, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		r, ok := byName[name]
		if !ok {
			var names []string
			for _, r := range all {
				names = append(names, r.Name)
			}
			return nil, fmt.Errorf("there is no role called %q; the roles are %s",
				name, strings.Join(names, ", "))
		}
		out = append(out, r)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("-roles named no roles")
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
