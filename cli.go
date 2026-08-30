// blacksmith. Bare, it opens the terminal UI: type a request, watch it run.
//
// THE SIMPLE SHAPE TOOK THE NAME once it was measured reliable. The department
// still lives in this binary — `blacksmith service` runs it exactly as the
// systemd unit always has, and `blacksmith window` opens its board view — but
// the front door is the rebuilt pipeline: plan, tests-first, locked suite,
// reroll. A stage's input is the tree the previous stage left behind; there is
// no board and no claiming on this path, and internal/dispatch is what it
// grows back into once several hosts have to share a board.
//
// Usage:
//
//	blacksmith                     open the TUI (workspace ./workshop)
//	blacksmith -tui -repo DIR      the TUI over a chosen workspace
//	blacksmith -repo D -task "…"   one batch run, no screen
//	blacksmith service             the department daemon (systemd)
//	blacksmith window              the department board view
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
	// SUBCOMMANDS FIRST, because the department predates the flags and its
	// systemd unit says `blacksmith service` — replacing the binary must not
	// brick the daemon it replaces.
	if len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "-") {
		departmentDispatch(os.Args[1])
		return
	}
	// -h and --help get the binary's own story, then the flags. flag.Parse's
	// default usage lists options and never says what the program IS, which is
	// exactly the reader who typed --help.
	if len(os.Args) > 1 && (os.Args[1] == "-h" || os.Args[1] == "--help") {
		usage(os.Stdout)
		fmt.Fprintln(os.Stdout, "batch flags:")
		flag.CommandLine.SetOutput(os.Stdout)
	}
	flag.Usage = func() {
		usage(os.Stderr)
		fmt.Fprintln(os.Stderr, "batch flags:")
		flag.PrintDefaults()
	}

	repoDir := flag.String("repo", "", "directory the agents read and write")
	task := flag.String("task", "", "what to build; empty opens the TUI")
	only := flag.String("roles", "", "comma-separated stages to run; default is the whole pipeline")
	dryRun := flag.Bool("dry-run", false, "print the plan and the wiring, then stop")
	single := flag.Bool("single", false,
		"run ONE unrestricted agent instead of the pipeline, as a baseline to measure against")
	plan := flag.Bool("plan", false,
		"run the plan arm: an architect plans the work and the tests, a test author writes "+
			"the tests red with placeholder stubs, and a developer makes them pass")
	tui := flag.Bool("tui", false,
		"open the interactive terminal UI: type requests, watch them run, browse the results. "+
			"-repo is the workspace root; each request builds in its own directory under it")
	flag.Parse()

	// BARE MEANS THE TUI. A human typing the binary's name gets the front
	// door, not a usage dump; the workspace defaults to ./workshop and is
	// printed in the UI's own header.
	if *tui || (*task == "" && !*dryRun) {
		base := *repoDir
		if base == "" {
			base = "workshop"
		}
		if err := runTUI(base); err != nil {
			fmt.Fprintln(os.Stderr, "error: "+err.Error())
			os.Exit(1)
		}
		return
	}

	if err := runBatch(*repoDir, *task, *only, *dryRun, *single, *plan); err != nil {
		fmt.Fprintln(os.Stderr, "error: "+err.Error())
		os.Exit(1)
	}
}

func runBatch(repoDir, task, only string, dryRun, single, plan bool) error {
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
	if single && plan {
		return fmt.Errorf("-single and -plan are two different arrangements; run one at a time")
	}
	if (single || plan) && only != "" {
		return fmt.Errorf("-roles selects stages from the pipeline; it cannot be combined with " +
			"-single or -plan, which are their own arrangements")
	}
	switch {
	case single:
		stages = []string{stageBaseline}
	case plan:
		stages = agents.PlanStages()
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
		OnWrite: func(path, content string, deleted bool, message string) {
			curGit.write(path, content, deleted, message)
		},
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

	// THE WHOLE RUN RETRIES ON FAILURE, from the ORIGINAL tree. The seeds vary
	// wildly on the same input — measured on this arrangement, roughly one run
	// in six draws a suite its developer cannot converge on — so rerolling the
	// whole run converts a ~15% per-run failure into (0.15)^3 at the cost of
	// time on the bad seeds only. The revert matters as much as the retry: the
	// failed attempt's tree is the thing the next attempt must NOT inherit,
	// because the reroll's whole value is a fresh draw.
	return runWithReroll(ctx, maker, stages, files, repoDir, task)
}

// runWithReroll gives the whole run MaxRunAttempts fresh draws.
func runWithReroll(
	ctx context.Context, maker agents.Creator, stages []string,
	files map[string]string, repoDir, task string,
) error {
	original := copyTree(files)

	// The run's history opens with the request and closes with the verdict.
	// curGit is what the write hook reaches; one run holds the GPU at a time.
	curGit = newGitLog(repoDir, task)
	defer func() { curGit = nil }()
	// THE SEED GOES TO DISK BEFORE THE SNAPSHOT. The tree lives in memory and
	// used to reach disk only at stage ends, so the snapshot here committed an
	// empty directory and the first agent write's add -A silently swept the
	// whole project in under that write's message — measured on the first real
	// project run: "feat: Add TaskCount method", carrying 2,044 seed lines.
	// The story a history tells is only as honest as its first commit.
	if err := writeTree(repoDir, files); err != nil {
		return fmt.Errorf("writing %s: %w", repoDir, err)
	}
	curGit.snapshot("chore: the tree as the request found it")

	var lastErr error
	for attempt := 1; attempt <= MaxRunAttempts; attempt++ {
		announce("draw", "", attempt)
		if attempt > 1 {
			slog.Warn("the run failed; reverting to the original tree for a fresh attempt",
				"attempt", attempt, "of", MaxRunAttempts, "error", lastErr.Error())
			if err := revertTree(repoDir, original, files); err != nil {
				return fmt.Errorf("reverting %s: %w", repoDir, err)
			}
			files = copyTree(original)
			curGit.snapshot(fmtDraw(attempt))
		}
		files, lastErr = executeRun(ctx, maker, stages, files, repoDir, task)
		if lastErr == nil {
			curGit.mark("run: passed")
			curGit.pushRun(os.Getenv(gitEnvURL), os.Getenv(gitEnvMkrepo), repoDir)
			return nil
		}
		// An operator's ctrl-C is not a bad seed.
		if ctx.Err() != nil {
			return lastErr
		}
	}
	// A FAILED run pushes too. The history of how three draws died is exactly
	// what a person debugging the seed wants on a VM, and it is the record the
	// reroll's revert would otherwise silently destroy.
	curGit.mark(fmt.Sprintf("run: failed after %d attempts: %s", MaxRunAttempts, firstLineOf(lastErr.Error())))
	curGit.pushRun(os.Getenv(gitEnvURL), os.Getenv(gitEnvMkrepo), repoDir)
	return fmt.Errorf("after %d attempts: %w", MaxRunAttempts, lastErr)
}

func firstLineOf(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// MaxRunAttempts bounds the whole-run reroll.
const MaxRunAttempts = 3

// inTUI is set for the screen's lifetime so nothing else writes to a terminal
// the alternate buffer owns.
var inTUI bool

// announce is the seam the TUI listens through. A package variable rather
// than a parameter because runStage and executeRun are load-bearing, tested
// signatures and the batch CLI has no listener; the default is silence.
var announce = func(kind, stage string, n int) {}

// executeRun works the stages in order over the tree and returns the tree as
// the last stage left it, finished or not.
func executeRun(
	ctx context.Context, maker agents.Creator, stages []string,
	files map[string]string, repoDir, task string,
) (map[string]string, error) {
	for _, name := range stages {
		build := func(tree map[string]string) (*agents.Agent, error) {
			return stage(maker, name, tree)
		}
		outcome, tree, runErr := runStage(ctx, build, files, task)

		// THE TREE IS CARRIED FORWARD EVEN WHEN THE STAGE FAILED, and written out
		// before anything is reported. What a failed developer wrote is most of the
		// answer, and throwing it away means the next attempt starts from nothing —
		// which is how a run that was nearly finished becomes one that never
		// finishes.
		files = tree
		if err := writeTree(repoDir, files); err != nil {
			return files, fmt.Errorf("writing %s: %w", repoDir, err)
		}
		if runErr != nil {
			announce("stage-fail", name, 0)
			curGit.mark("stage(" + name + "): failed")
			return files, fmt.Errorf("stage %s: %w", name, runErr)
		}

		announce("stage-pass", name, 0)
		curGit.mark("stage(" + name + "): passed")
		slog.Info("stage finished",
			"stage", name, "passed", outcome.Passed, "turns", outcome.Iterations)
		if outcome.Answer != "" {
			// NOT STDOUT UNDER THE TUI. The screen is an alternate buffer and a
			// Printf lands underneath it, bleeding through the frame — seen live
			// on the first real session, the architect's prose stamped across the
			// footer. The log keeps it either way; batch keeps its stdout.
			if inTUI {
				slog.Info("stage answer", "stage", name, "answer", outcome.Answer)
			} else {
				fmt.Printf("\n--- %s ---\n%s\n", name, outcome.Answer)
			}
		}
		if !outcome.Passed {
			announce("stage-fail", name, 0)
			// TOLD APART, because they need opposite responses: a stage that spent
			// its budget working may deserve a larger one, while a stage that stopped
			// moving would do the same thing with twice as many turns.
			why := fmt.Sprintf("ran out of budget after %d turns", outcome.Iterations)
			if outcome.Stalled {
				why = fmt.Sprintf("stopped after %d turns having changed nothing in the last %d",
					outcome.Iterations, agents.MaxIdleTurns)
			}
			return files, fmt.Errorf("stage %s %s and its check never passed. What it last said:\n%s",
				name, why, orNone(outcome.LastCheck))
		}
	}

	slog.Info("pipeline finished", "repo", repoDir, "files", len(files))
	return files, nil
}

func copyTree(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// revertTree puts the directory back to the original snapshot: files the
// failed attempt created are removed, files it changed are rewritten. Only
// paths the attempt is known to have produced are touched — this must never
// become a general rm -rf over a directory that can be a real checkout.
func revertTree(dir string, original, dirty map[string]string) error {
	for rel := range dirty {
		if _, kept := original[rel]; kept {
			continue
		}
		if err := os.Remove(filepath.Join(dir, filepath.FromSlash(rel))); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return writeTree(dir, original)
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

// runStage runs one stage to completion, RESPINNING it on a wall-clock
// timeout: the wedged attempt is cancelled, its TREE is kept, its TRAIL is
// thrown away, and a fresh agent of the same stage takes over the files.
//
// The trail is discarded on purpose — it is where the wedge lives. A context
// full of identical failed checks makes the next identical check the most
// probable continuation, while the tree holds all of the actual work. Measured
// on the run that bought this: a dev re-ran an unchanging check for ten
// minutes, immune to the stall bound (test timings kept the output from ever
// being byte-identical) and to the heat (each check reset idle).
//
// A stage with no AttemptTimeout runs exactly once, unbounded, as before.
func runStage(
	ctx context.Context,
	build func(files map[string]string) (*agents.Agent, error),
	files map[string]string, task string,
) (agents.Outcome, map[string]string, error) {
	agent, err := build(files)
	if err != nil {
		return agents.Outcome{}, files, err
	}
	attempts := 1 + agent.Respins()

	for attempt := 1; ; attempt++ {
		announce("stage-start", agent.Name(), attempt)
		slog.Info("stage starting", "stage", agent.Name(), "class", string(agent.Class()),
			"files", len(files), "attempt", attempt)

		actx, cancel := ctx, context.CancelFunc(func() {})
		if t := agent.AttemptTimeout(); t > 0 {
			actx, cancel = context.WithTimeout(ctx, t)
		}
		outcome, runErr := agent.Run(actx, task)
		cancel()

		// The tree survives every exit, including the timeout: what the killed
		// attempt wrote is most of the answer.
		files = agent.Files()

		timedOut := runErr != nil && actx.Err() == context.DeadlineExceeded && ctx.Err() == nil
		if !timedOut || attempt >= attempts {
			return outcome, files, runErr
		}

		slog.Warn("stage attempt timed out; a fresh agent takes over the tree",
			"stage", agent.Name(), "attempt", attempt, "of", attempts,
			"timeout", agent.AttemptTimeout().String())
		agent, err = build(files)
		if err != nil {
			return outcome, files, err
		}
	}
}

// stageBaseline names the unrestricted single agent. Not in agents.Stages(),
// because it is what the pipeline is measured AGAINST rather than part of it.
const stageBaseline = "baseline"

// stage resolves a name to a built agent, including the baseline that the
// pipeline's own dispatcher does not know about.
func stage(c agents.Creator, name string, files map[string]string) (*agents.Agent, error) {
	switch name {
	case stageBaseline:
		return c.Baseline(files), nil
	case agents.StagePlanArchitect:
		return c.PlanningArchitect(files), nil
	case agents.StagePlanTest:
		return c.PlanTester(files), nil
	case agents.StagePlanDev:
		return c.PlanFollowingDev(files), nil
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
