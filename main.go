// Command blacksmith is the agent department: it pulls work from a board and
// runs the pipeline that answers it.
//
// THIS FILE IS DELIBERATELY THIN. Every decision it looks like it makes — which
// stages exist, which class each one gets, where a ticket goes when a stage
// succeeds — is made in a package with tests, because the only way to check a
// decision made here is to run the department and watch. What is left is the
// order things are started in, and the two failures that must stop startup
// rather than be discovered later.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/code-armory-app/blacksmith/internal/config"
	"github.com/code-armory-app/blacksmith/internal/department"
	"github.com/code-armory-app/blacksmith/internal/dispatch"
	"github.com/code-armory-app/blacksmith/internal/forge"
	"github.com/code-armory-app/blacksmith/internal/model"
	"github.com/code-armory-app/blacksmith/internal/platform"
	"github.com/code-armory-app/blacksmith/internal/transcript"
	"github.com/code-armory-app/blacksmith/internal/transport"
	"github.com/code-armory-app/blacksmith/internal/wake"
	"github.com/code-armory-app/blacksmith/internal/window"
	"github.com/code-armory-app/blacksmith/internal/workflow"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	args := os.Args[1:]
	switch {
	case len(args) == 0:
		if err := runWindow(ctx); err != nil && !errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case args[0] == "service":
		if err := runService(ctx); err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("the department stopped", "error", err)
			os.Exit(1)
		}
	case args[0] == "-h", args[0] == "--help", args[0] == "help":
		usage(os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", args[0])
		usage(os.Stderr)
		os.Exit(2)
	}
}

func usage(w *os.File) {
	fmt.Fprint(w, `blacksmith — the agent department

  blacksmith            open the window onto a running department
  blacksmith service    run the department (this is what the systemd unit starts)

Configuration is read from ~/.config/codearmory-agents/env, the same file the
unit uses, so both work from any shell without exporting anything.
`)
}

// runWindow opens the read-only view.
//
// A MISSING SERVING STACK IS NOT A REASON TO REFUSE TO LOOK. The window calls no
// model and claims nothing — and "why is nothing being served" is exactly the
// question someone opens it to answer, so a configuration that would stop the
// department starting must still let the board be read.
func runWindow(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil && !errors.Is(err, config.ErrNoModelClasses) {
		return fmt.Errorf("configuration invalid: %w", err)
	}
	if !cfg.DispatchReady() {
		return errors.New("no board to read: set AGENTS_TICKETS_URL to read this host's own " +
			"store, or CODEARMORY_URL and CODEARMORY_TOKEN to read the platform's, in " +
			"~/.config/codearmory-agents/env")
	}

	// THE SAME STORE THE DEPARTMENT WORKS. Building a client here separately is
	// how the window came to read the platform while the service worked this
	// host's own plane — so it drew an empty board beside a department that was
	// busy, which is the one thing it must never do. One place decides which
	// store is authoritative and both halves follow it.
	store, _, err := clients(cfg)
	if err != nil {
		return err
	}
	where, mode := cfg.PlatformURL, "platform"
	if cfg.Standalone() {
		where, mode = cfg.TicketsURL, "standalone"
	}
	return window.Open(ctx, store, window.Options{
		Table:         department.Table(cfg),
		BoardID:       cfg.BoardID,
		TranscriptDir: cfg.TranscriptDir,
		RepoURL:       cfg.Repo.URL,
		Where:         where,
		Mode:          mode,
	})
}

// runService starts the department.
func runService(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("configuration invalid: %w", err)
	}
	slog.Info("agents host starting", "host", cfg.Host, "classes", len(cfg.Classes))

	// TRANSCRIPT CAPTURE IS WIRED BEFORE ANYTHING CAN RUN A COMPLETION, and a
	// failure here STOPS STARTUP rather than warning.
	//
	// A transcript is worthless retroactively: what was not captured at the time
	// cannot be recovered afterwards, so a department running without capture is
	// quietly throwing away the training corpus it exists to produce. That is a
	// thing to fail on, not a thing to log.
	var recorder *transcript.Recorder
	if cfg.CaptureEnabled() {
		sink, err := transcript.NewJSONLSink(cfg.TranscriptDir)
		if err != nil {
			return fmt.Errorf("transcript capture unavailable at %s: %w", cfg.TranscriptDir, err)
		}
		defer sink.Close()
		recorder = transcript.New(sink, cfg.Host)
		slog.Info("transcript capture enabled", "dir", cfg.TranscriptDir)
	} else {
		slog.Warn("transcript capture DISABLED; runs will produce no training data")
	}

	gw := model.NewGateway(cfg.Host, cfg.Classes)
	gw.SetRecorder(recorder)
	for _, class := range cfg.Configured() {
		cc := cfg.Classes[class]
		// The endpoint and model are logged; THE API KEY DELIBERATELY IS NOT.
		slog.Info("model class routed", "class", string(class), "endpoint", cc.Endpoint,
			"model", cc.Model, "slots", cc.Slots, "authenticated", cc.APIKey != "")
	}

	if !cfg.DispatchReady() {
		// Serving without dispatch is a legitimate state, so this is a warning
		// rather than a failure. It is LOUD because a host that silently works no
		// tickets looks exactly like a host with no tickets to work.
		slog.Warn("dispatch DISABLED: set CODEARMORY_URL and CODEARMORY_TOKEN to pull work, " +
			"or AGENTS_TICKETS_URL to work a local store")
		<-ctx.Done()
		return ctx.Err()
	}

	store, runner, err := clients(cfg)
	if err != nil {
		return err
	}

	assembly, err := department.Assemble(department.Deps{
		Gateway: gw, Runner: runner, Leases: runner,
		Store: store, Config: cfg,
	})
	if err != nil {
		return err
	}
	// WHAT WILL NOT RUN IS SAID OUT LOUD. A stage dropped in silence is a column
	// nothing claims from, which is indistinguishable from a column with no work.
	for _, sk := range assembly.Skipped {
		slog.Warn("stage NOT running; its column will not be worked",
			"role", sk.Role, "class", string(sk.Class), "reason", sk.Reason)
	}

	return run(ctx, cfg, assembly, store, recorder)
}

// clients builds the ticket store and the sandbox runner this host talks to.
//
// TWO SHAPES, AND WHICH ONE IS IN USE IS STATED RATHER THAN INFERRED from
// whichever variables happen to be set. STANDALONE means the local plane is the
// AUTHORITY: the store on this host holds the tickets, nothing is synced, and
// the platform is not consulted at all. CONNECTED means the platform holds them
// and this host pulls work through conductor.
//
// They are the only two shapes. A third — both stores writable and reconciled —
// is master-master on mutable rows, where status and priority become
// last-write-wins and the claim protocol loses the single ordering authority it
// relies on to stop two agents working one ticket.
func clients(cfg config.Config) (*platform.Store, *forge.Client, error) {
	// THE PLANE HAS ITS OWN CREDENTIAL, and it is a SESSION rather than a fixed
	// token: the gatekeeper issues short ones, so a department that cannot renew
	// stops working when the first expires — every stage then reports
	// "unauthorized" against a plane that is perfectly healthy, which reads as a
	// broken deployment rather than an expired session.
	//
	// One credential for the whole plane, because the store and the forge
	// authenticate against the same gatekeeper: issuing two would mean two
	// sessions for one trust domain, drifting out of step.
	var plane transport.Credential
	if cfg.ForgeEmail != "" && cfg.ForgePassword != "" {
		session, err := transport.Login(cfg.SandboxLoginURL(), cfg.ForgeEmail, cfg.ForgePassword, nil)
		if err != nil {
			return nil, nil, fmt.Errorf("sandbox credential: %w", err)
		}
		plane = session
		slog.Info("sandbox session renews on expiry",
			"login_url", cfg.SandboxLoginURL(), "email", cfg.ForgeEmail)
	} else {
		plane = transport.Static(cfg.ForgeToken)
		if cfg.ForgeToken == "" {
			slog.Warn("no plane credential: set AGENTS_FORGE_EMAIL and AGENTS_FORGE_PASSWORD, " +
				"or AGENTS_FORGE_TOKEN")
		} else {
			slog.Warn("the sandbox token is STATIC and will stop working when it expires; " +
				"set AGENTS_FORGE_EMAIL and AGENTS_FORGE_PASSWORD to renew")
		}
	}

	runner := forge.Routed(cfg.PlatformURL, transport.Static(cfg.PlatformToken))
	if cfg.ForgeURL != "" {
		// SANDBOXES ON THE LOCAL FORGE is the intended deployment: it puts the
		// sandbox on the same machine as the model.
		runner = forge.Local(cfg.ForgeURL, plane)
		slog.Info("sandboxes run on a LOCAL forge with its own gatekeeper", "url", cfg.ForgeURL)
	} else {
		slog.Warn("sandboxes run on the PLATFORM's forge; set AGENTS_FORGE_URL to run them here")
	}

	if cfg.Standalone() {
		store, err := platform.Local(cfg.TicketsURL, plane)
		if err != nil {
			return nil, nil, fmt.Errorf("ticket store: %w", err)
		}
		slog.Info("STANDALONE: this host's own store holds the tickets and the platform "+
			"is not consulted", "url", cfg.TicketsURL)
		return store, runner, nil
	}

	store, err := platform.Routed(cfg.PlatformURL, transport.Static(cfg.PlatformToken))
	if err != nil {
		return nil, nil, fmt.Errorf("platform client: %w", err)
	}
	slog.Info("CONNECTED: tickets are pulled from the platform", "url", cfg.PlatformURL)
	return store, runner, nil
}

// run starts one dispatcher per stage and waits for the context to end.
func run(
	ctx context.Context, cfg config.Config, assembly department.Assembly,
	store dispatch.Store, recorder *transcript.Recorder,
) error {
	table := department.Table(cfg)

	// ONE Wake SHARED BY EVERY STAGE. The dispatchers hand tickets to each other
	// through a board column, and without this each learns of a hand-off only on
	// its own timer — several times per ticket, since the pipeline has several of
	// them. Sharing the trigger is what makes a hand-off immediate.
	trigger := wake.New()

	var wg sync.WaitGroup
	for _, stage := range assembly.Stages {
		d, err := dispatch.New(store, stage.Handler, table, dispatch.Options{
			Host:        cfg.Host,
			BoardID:     cfg.BoardID,
			Concurrency: concurrencyFor(cfg, stage.Role),
			Poll:        cfg.Poll,
			// Left at the dispatcher's own default: the loop breaker's ceiling is a
			// property of the protocol, not of a host.
			MaxAttempts: dispatch.DefaultMaxAttempts,
			Wake:        trigger,
			Recorder:    recorder,
		})
		if err != nil {
			return fmt.Errorf("dispatcher for %s: %w", stage.Role, err)
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := d.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
				slog.Error("stage stopped", "role", stage.Role, "error", err)
			}
		}()
		slog.Info("stage running", "role", stage.Role, "class", string(stage.Class))
	}

	slog.Info("department ready", "host", cfg.Host, "stages", len(assembly.Stages))
	<-ctx.Done()
	slog.Info("stopping; waiting for in-flight work to finish")
	wg.Wait()
	return nil
}

// concurrencyFor is how many tickets a stage works at once.
//
// THE DEVELOPER GETS ITS OWN NUMBER because its work is not like the others': a
// review is one model call and a developer is dozens, against a sandbox it holds
// throughout. Running as many developers as reviewers fills the serving class's
// queue with the one stage that cannot be quick.
func concurrencyFor(cfg config.Config, role string) int {
	if role == workflow.RoleDev && cfg.DevConcurrency > 0 {
		return cfg.DevConcurrency
	}
	return cfg.Concurrency
}
