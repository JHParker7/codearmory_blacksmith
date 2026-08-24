// Agents — the simulated department's runtime.
//
// Unlike every other service in this repo, agents is a CLIENT of CodeArmory
// rather than a component of it. It runs on a workstation beside the GPUs,
// reaches the platform through conductor exactly as a developer's tooling would,
// and is never registered as a route. That has three consequences worth knowing
// before reading further:
//
//   - There is no inbound HTTP API and no registry manifest entry. Work is
//     PULLED (conductor cannot reach an intermittent desktop behind NAT).
//   - Several agent hosts may attach to one CodeArmory at once, the way several
//     developers share one CI/CD platform. Nothing here may assume it is the
//     only one — hence the host identifier on every claim and transcript.
//   - Sandboxes are not run by this process. They are forge executions submitted
//     to a local forge; this service never reimplements isolation.
//
// This binary wires the inference gateway (model-class routing and admission
// control), transcript capture, the pull loop, and the product-manager agent.
// The developer and reviewer agents need a sandbox runtime and are not here yet.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/code-armory-app/codearmory_sdk/telemetry"
)

// secret reads NAME, preferring ${NAME}_FILE so a k8s- or systemd-mounted
// secret file works without changing the deployment. Same contract as every
// other service in the repo.
func secret(name string) string {
	if path := os.Getenv(name + "_FILE"); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			slog.Error("cannot read secret file", "var", name+"_FILE", "path", path, "error", err)
			os.Exit(1)
		}
		return strings.TrimRight(string(data), "\n")
	}
	return os.Getenv(name)
}

func envOrDefault(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// setupLogging wires telemetry, DEGRADING rather than failing when it is
// unavailable.
//
// The in-repo services run in a cluster with a collector and are right to treat
// its absence as a misconfiguration. blacksmith is a workstation client: no
// collector is the normal case, and exiting over it would mean the agent runtime
// refuses to start on exactly the machine it was built for. Traces are a
// diagnostic, not a dependency.
func setupLogging(ctx context.Context) (slog.Handler, func(context.Context) error, error) {
	handler, shutdown, err := telemetry.Setup(ctx, "blacksmith")
	if err != nil {
		return slog.NewTextHandler(os.Stderr, nil), func(context.Context) error { return nil }, err
	}
	return handler, shutdown, nil
}

// main dispatches between the two things this binary is.
//
// THE BARE COMMAND OPENS THE WINDOW, and running the department needs the word
// `service`. That is the right way round because of who runs each: a person types
// `blacksmith` at a prompt to see what is happening, while the department is
// started by systemd, once, from a unit file where an extra word costs nothing.
// The other arrangement made the common interactive case the one that needed
// remembering, and — worse — made the accident of typing `blacksmith` start a
// SECOND department on a host that already had one.
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cmd := ""
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	switch cmd {
	case "", "tui":
		runWindow(ctx)
	case "service":
		runService(ctx)
	case "-h", "--help", "help":
		usage(os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "blacksmith: unknown command %q\n\n", cmd)
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

// runWindow opens the read-only view. A missing serving stack is not a reason to
// refuse to LOOK: the window calls no model, and "why is nothing being served" is
// exactly the question you open it to answer.
func runWindow(ctx context.Context) {
	cfg, err := LoadConfig()
	if err != nil && !errors.Is(err, ErrNoModelClasses) {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(1)
	}
	if err := runTUI(ctx, cfg); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// runService is the department itself: inference gateway, transcript capture and
// the dispatchers.
func runService(ctx context.Context) {
	handler, shutdownTelemetry, telemetryErr := setupLogging(ctx)
	slog.SetDefault(slog.New(handler))
	if telemetryErr != nil {
		slog.Warn("telemetry unavailable; logging locally only", "error", telemetryErr)
	}
	defer func() {
		if err := shutdownTelemetry(context.WithoutCancel(ctx)); err != nil {
			slog.Error("telemetry shutdown failed", "error", err)
		}
	}()

	cfg, err := LoadConfig()
	if err != nil {
		slog.Error("configuration invalid", "error", err)
		os.Exit(1)
	}

	gw := NewGateway(cfg)

	// Transcript capture is wired before anything can run a completion.
	// Transcripts are worthless retroactively, so this is a startup failure
	// rather than a warning: a department running without capture is quietly
	// throwing away the training corpus it exists to produce.
	var recorder *Recorder
	if cfg.CaptureEnabled() {
		sink, err := NewJSONLSink(cfg.TranscriptDir)
		if err != nil {
			slog.Error("transcript capture unavailable", "dir", cfg.TranscriptDir, "error", err)
			os.Exit(1)
		}
		defer sink.Close()
		recorder = NewRecorder(sink, cfg.Host)
		gw.SetRecorder(recorder)
		slog.Info("transcript capture enabled", "dir", cfg.TranscriptDir)
	} else {
		slog.Warn("transcript capture DISABLED; runs will produce no training data")
	}

	for _, class := range cfg.Configured() {
		cc := cfg.Classes[class]
		// The endpoint and model are logged; the API key deliberately is not.
		slog.Info("model class routed",
			"class", string(class),
			"endpoint", cc.Endpoint,
			"model", cc.Model,
			"slots", cc.Slots,
			"queue_depth", cc.QueueDepth,
			"authenticated", cc.APIKey != "",
		)
	}
	slog.Info("agents host ready", "host", cfg.Host, "classes", len(cfg.Classes))

	// One service per stage, so a service graph shows five boxes rather than one
	// and a latency panel does not average a 600 ms review against a 4-minute
	// developer run. Failure is a warning: a host with no collector is the normal
	// case, and the fallback tracer keeps everything working.
	// The three-tier stages are named here too. A role missing from this list
	// still traces, but under the process-wide name — which is how architect,
	// spec and spec-merge work has been landing in the same bucket as everything
	// else since the tiering went in.
	tracers, terr := setupAgentTracers(ctx, []string{
		"architect-agent", "pm-agent", "spec-agent", "spec-merge-agent", "test-agent",
		"dev-agent", "coverage-agent", "sec-agent", "integrator", "resolver",
	})
	if terr != nil {
		slog.Warn("per-agent tracing unavailable; stages will report under one service name", "error", terr)
	}
	if tracers != nil && tracers.shutdown != nil {
		defer func() {
			if err := tracers.shutdown(context.WithoutCancel(ctx)); err != nil {
				slog.Error("flushing agent traces failed", "error", err)
			}
		}()
	}

	// Metrics, same posture as tracing: a missing collector is normal and must
	// not stop the department starting.
	if am, merr := setupAgentMetrics(ctx); merr != nil {
		slog.Warn("agent metrics unavailable; run counters will not be recorded", "error", merr)
	} else if am != nil {
		setAgentMetrics(am)
		defer func() {
			if err := am.shutdown(context.WithoutCancel(ctx)); err != nil {
				slog.Error("flushing agent metrics failed", "error", err)
			}
		}()
	}

	var dispatchers []*Dispatcher
	// ONE Wake shared by every stage. The dispatchers hand tickets to each other
	// through a board column, and without this each only learns of a hand-off on
	// its own timer — three times per ticket, since the pipeline has three of
	// them. Sharing the trigger is what makes a hand-off immediate.
	wake := NewWake()
	if !cfg.DispatchReady() {
		// Serving without dispatch is a legitimate state — the host can still be
		// exercised with the live smoke test — so this is a warning rather than a
		// failure. It is loud because a host that silently works no tickets looks
		// exactly like a host with no tickets to work.
		slog.Warn("dispatch DISABLED: set CODEARMORY_URL and CODEARMORY_TOKEN to pull work, or AGENTS_TICKETS_URL to work a local store")
	} else {
		var api *CodeArmory
		if cfg.Standalone() && cfg.PlatformURL == "" {
			// No platform at all. Everything this host needs is on its own plane.
			api = NewStandaloneCodeArmory()
		} else {
			var err error
			api, err = NewCodeArmory(cfg.PlatformURL, cfg.PlatformToken)
			if err != nil {
				slog.Error("platform client", "error", err)
				os.Exit(1)
			}
		}
		forgeToken := cfg.ForgeToken
		if cfg.ForgeEmail != "" && cfg.ForgePassword != "" {
			// A placeholder only to satisfy the "local plane needs its own
			// credential" check; the renewing credential supplies the real one.
			forgeToken = "renewing"
		}
		if err := api.UseLocalForge(cfg.ForgeURL, forgeToken); err != nil {
			slog.Error("forge endpoint invalid", "error", err)
			os.Exit(1)
		}
		if cfg.ForgeURL != "" && cfg.ForgeEmail != "" && cfg.ForgePassword != "" {
			cred, err := NewCredential("", cfg.SandboxLoginURL(), cfg.ForgeEmail, cfg.ForgePassword, nil)
			if err != nil {
				slog.Error("sandbox credential", "error", err)
				os.Exit(1)
			}
			api.UseRenewingForgeCredential(cred)
			slog.Info("sandbox credential renews on expiry",
				"email", cfg.ForgeEmail, "login_url", cfg.SandboxLoginURL())
		} else if cfg.ForgeURL != "" {
			slog.Warn("sandbox token is STATIC and will stop working when it expires; set AGENTS_FORGE_EMAIL and AGENTS_FORGE_PASSWORD to renew")
		}
		if cfg.ForgeURL != "" {
			slog.Info("sandboxes run on a LOCAL forge with its own gatekeeper", "url", cfg.ForgeURL)
		} else {
			slog.Warn("sandboxes run on the PLATFORM's forge; set AGENTS_FORGE_URL to run them on this host")
		}
		// After the plane credential is wired: the local store shares it.
		if err := api.UseLocalTickets(cfg.TicketsURL); err != nil {
			slog.Error("ticket store endpoint invalid", "error", err)
			os.Exit(1)
		}
		// Which store holds the authority decides what a claim means, so it is
		// stated rather than inferred from which variables happen to be set.
		if api.LocalTickets() {
			slog.Info("tickets come from the LOCAL store on the sandbox plane; it is AUTHORITATIVE and nothing is synced to a platform",
				"url", cfg.TicketsURL)
		} else {
			slog.Info("tickets come from the platform", "url", cfg.PlatformURL)
		}
		// THE COLUMNS ARE THE ROUTING TABLE, so they must exist before any stage
		// polls. The tickets service validates a status against the board's own
		// field defs, so a missing column is not a cosmetic gap: every list and
		// every move against it is rejected, and the department comes up looking
		// healthy while working nothing.
		//
		// A failure here is fatal rather than a warning for the same reason. There
		// is no degraded mode worth running: without the columns the department
		// cannot select work at all.
		// ONE DISPATCHER SET PER PROJECT. A project is a board and a repository,
		// and both are baked into every stage — the board it polls, the repo it
		// clones, the branch it integrates into. Sharing dispatchers across
		// projects would mean deciding per ticket which repository it belongs to,
		// which is exactly the thing the pair already answers.
		//
		// The cost is honest and worth naming: N projects run N times the pollers
		// and, if every project is busy, N times the sandboxes. Concurrency is
		// configured per project for that reason rather than globally.
		// Settled before any dispatcher reads the routing table.
		applyCoverageSetting(cfg.CoverageEnabled)
		// The architect needs a repository to commit its documentation into, so a
		// triage-only host runs without one and the product manager keeps taking
		// straight from the inbox.
		applyArchitectSetting(cfg.ArchitectEnabled && cfg.DevEnabled())

		projects, perr := LoadProjects(cfg)
		if perr != nil {
			slog.Error("could not read the project list", "error", perr)
			os.Exit(1)
		}
		if _, ok := cfg.Classes[cfg.PMClass]; !ok {
			slog.Error("the product-manager agent's model class is not served by this host",
				"class", string(cfg.PMClass), "configured", cfg.Configured())
			os.Exit(1)
		}
		var active int
		for _, proj := range projects {
			if !proj.Active() {
				slog.Info("project disabled; skipping", "project", proj.Name)
				continue
			}
			ds, derr := buildProjectDispatchers(ctx, cfg, proj, api, recorder, gw, tracers, wake)
			if derr != nil {
				slog.Error("could not start a project", "project", proj.Name, "error", derr)
				os.Exit(1)
			}
			dispatchers = append(dispatchers, ds...)
			active++
		}
		if active == 0 {
			slog.Error("no project is enabled; the department would run nothing")
			os.Exit(1)
		}
		slog.Info("projects running", "count", active, "of", len(projects))

		for _, d := range dispatchers {
			go func() {
				if err := d.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
					slog.Error("dispatch stopped", "error", err)
				}
			}()
		}
		// Report the store work actually comes FROM. Logging the platform URL here
		// while reading a local store says the opposite of what is happening, and
		// this line is the one someone reads to find out where the tickets are.
		source := cfg.PlatformURL
		if cfg.Standalone() {
			source = cfg.TicketsURL
		}
		slog.Info("dispatch running",
			"tickets_from", source, "standalone", cfg.Standalone(),
			"dispatchers", len(dispatchers), "concurrency", cfg.Concurrency,
			"poll", cfg.Poll.String(), "pm_class", string(cfg.PMClass))
	}

	<-ctx.Done()
	// A non-zero drop count means the corpus has holes in it, so it is reported
	// at shutdown rather than left to be discovered during training.
	dropped := int64(0)
	if recorder != nil {
		dropped = recorder.Dropped()
	}
	tickets := 0
	for _, d := range dispatchers {
		tickets += d.InFlight()
	}
	slog.Info("shutting down",
		"tickets_in_flight", tickets,
		"completions_in_flight", inFlightTotal(gw, cfg),
		"transcript_records_dropped", dropped)
}

// inFlightTotal sums live requests across classes, so the shutdown line says
// whether anything was actually interrupted.
func inFlightTotal(gw *Gateway, cfg Config) int {
	total := 0
	for _, class := range cfg.Configured() {
		if q := gw.Queue(class); q != nil {
			total += q.InFlight()
		}
	}
	return total
}
