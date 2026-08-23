package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
)

// buildProjectDispatchers wires one project's stages.
//
// Returns an error rather than exiting, because with several projects a single
// bad one must not take the host down with it — the others are unaffected and a
// department that refuses to start over one misconfigured repository is worse
// than one that reports it.
func buildProjectDispatchers(ctx context.Context, cfg Config, proj Project, api *CodeArmory,
	recorder *Recorder, gw *Gateway, tracers *agentTracers, wake *Wake) ([]*Dispatcher, error) {
	var out []*Dispatcher
	if err := api.EnsureColumns(ctx, proj.BoardID); err != nil {
		return nil, fmt.Errorf("provision columns on board %s: %w", proj.BoardID, err)
	}
	// THE ARCHITECT IS REGISTERED FIRST because it runs first: it writes the
	// project's documentation to the base branch before the request is broken
	// down, so every sandbox cut afterwards clones a tree that explains the whole
	// system. It needs a repository, so it rides on DevEnabled — main.go keeps the
	// routing table in step, leaving the product manager on the inbox when this
	// stage is not running.
	//
	// CONCURRENCY 1, deliberately: this stage pushes to the branch every other
	// agent clones, and two designs racing for that ref is the one way it could
	// lose someone else's commit.
	if cfg.ArchitectEnabled && cfg.DevEnabled() {
		architectClass := resolveArchitectClass(cfg)
		out = append(out, NewDispatcher(api, recorder,
			NewArchitectAgent(gw, api, architectClass, proj.Repo), DispatcherOpts{
				Tracers:      tracers,
				Wake:         wake,
				Host:         cfg.Host,
				BoardID:      proj.BoardID,
				Concurrency:  1,
				Poll:         cfg.Poll,
				AgentAuthors: cfg.AgentAuthors,
			}))
		slog.Info("["+proj.Name+"] architect enabled: the design is committed to the base branch before the breakdown",
			"class", string(architectClass), "branch", proj.Repo.Branch)
	}

	out = append(out, NewDispatcher(api, recorder, NewPMAgent(gw, api, cfg.PMClass), DispatcherOpts{
		Tracers:      tracers,
		Wake:         wake,
		Host:         cfg.Host,
		BoardID:      proj.BoardID,
		Concurrency:  cfg.Concurrency,
		Poll:         cfg.Poll,
		AgentAuthors: cfg.AgentAuthors,
	}))

	// The developer agent is optional: a host with no repository configured is
	// a triage-only host, which is a valid deployment rather than a mistake.
	if cfg.DevEnabled() {
		if _, ok := cfg.Classes[cfg.DevClass]; !ok {
			return nil, fmt.Errorf("developer class %q is not served by this host", cfg.DevClass)
		}
		// Declare the sandbox sizing before any developer task can claim a
		// ticket. A failure here is logged and not fatal: the class may already
		// be correct, or be managed by the operator rather than by blacksmith,
		// and refusing to start a whole host over it would trade a possible
		// mis-sizing for a certain outage.
		if err := api.EnsureRunnerClass(ctx, devRunnerClass(cfg)); err != nil {
			slog.Warn("["+proj.Name+"] could not set the developer sandbox size; forge's existing runner class applies",
				"runner_class", proj.Repo.RunnerClass, "error", err)
		} else {
			slog.Info("["+proj.Name+"] developer sandbox sized",
				"runner_class", proj.Repo.RunnerClass,
				"memory_mb", cfg.DevMemoryMB, "cpu_millicores", cfg.DevCPUMillicores)
		}
		// THE TEST AUTHOR RUNS BEFORE THE DEVELOPER and feeds its queue, so it
		// is registered first. Ordering is nothing to the dispatcher, but
		// reading main.go in pipeline order is worth something.
		//
		// It is always enabled once a repository is configured, because it is
		// now the first work stage: a host that skipped it would leave every
		// ticket sitting in ready_for_tests with nothing coming to collect it.
		// resolveTestClass falls back rather than allowing that gap.
		testClass := resolveTestClass(cfg)
		out = append(out, NewDispatcher(api, recorder,
			NewTesterAgent(gw, api, testClass, proj.Repo, cfg.SpecMaxIterations), DispatcherOpts{
				Tracers:      tracers,
				Wake:         wake,
				Host:         cfg.Host,
				BoardID:      proj.BoardID,
				Concurrency:  cfg.TestConcurrency,
				Poll:         cfg.Poll,
				AgentAuthors: cfg.AgentAuthors,
			}))
		slog.Info("["+proj.Name+"] test author enabled", "class", string(testClass),
			"independent_of_developer", testClass != cfg.DevClass)

		// The coverage stage runs AFTER the developer, on the same class as the
		// spec author. Concurrency follows the test stage: it is the same kind of
		// work, and it feeds review rather than being fed by it.
		if cfg.CoverageEnabled {
			coverageClass := resolveCoverageClass(cfg)
			out = append(out, NewDispatcher(api, recorder,
				NewCoverageAgent(gw, api, coverageClass, proj.Repo, cfg.SpecMaxIterations), DispatcherOpts{
					Tracers:      tracers,
					Wake:         wake,
					Host:         cfg.Host,
					BoardID:      proj.BoardID,
					Concurrency:  cfg.TestConcurrency,
					Poll:         cfg.Poll,
					AgentAuthors: cfg.AgentAuthors,
				}))
			slog.Info("["+proj.Name+"] coverage author enabled",
				"class", string(coverageClass), "target_pct", proj.Repo.CoverageTarget,
				"own_model", coverageClass != testClass, "command", proj.Repo.CoverageCommand)
		}

		// THE SUB-TASK AUTHOR. Same agent and class as the test author, its own
		// columns, and its output lands on the parent task's branch. Concurrency
		// follows the test author's: they are the same work.
		out = append(out, NewDispatcher(api, recorder,
			NewSpecAgent(gw, api, testClass, withIntegration(proj.Repo, proj.IntegrationBranch), cfg.SpecMaxIterations), DispatcherOpts{
				Tracers:      tracers,
				Wake:         wake,
				Host:         cfg.Host,
				BoardID:      proj.BoardID,
				Concurrency:  cfg.TestConcurrency,
				Poll:         cfg.Poll,
				AgentAuthors: cfg.AgentAuthors,
			}))

		// THE RECONCILER, between the sections and the developer. Concurrency 1:
		// it is a short stage and there is no reason to hold two sandboxes for it.
		out = append(out, NewDispatcher(api, recorder,
			NewSpecMergeAgent(gw, api, testClass, withIntegration(proj.Repo, proj.IntegrationBranch), cfg.SpecMaxIterations), DispatcherOpts{
				Tracers:      tracers,
				Wake:         wake,
				Host:         cfg.Host,
				BoardID:      proj.BoardID,
				Concurrency:  1,
				Poll:         cfg.Poll,
				AgentAuthors: cfg.AgentAuthors,
			}))

		// Each developer task holds a sandbox and a large-class slot for
		// minutes, so concurrency here is a claim on the BOX: see DevConcurrency.
		out = append(out, NewDispatcher(api, recorder,
			NewDevAgent(gw, api, cfg.DevClass, withIntegration(proj.Repo, proj.IntegrationBranch), cfg.DevMaxIterations), DispatcherOpts{
				Tracers:      tracers,
				Wake:         wake,
				Host:         cfg.Host,
				BoardID:      proj.BoardID,
				Concurrency:  cfg.DevConcurrency,
				Poll:         cfg.Poll,
				AgentAuthors: cfg.AgentAuthors,
			}))
		if _, ok := cfg.Classes[cfg.SecClass]; ok {
			// Concurrency 1: a review holds a large-class slot, and the three
			// agents share one box.
			out = append(out, NewDispatcher(api, recorder,
				NewSecAgent(gw, api, cfg.SecClass, proj.Repo).WithUpkeep(proj.AutoMode(), proj.BoardID), DispatcherOpts{
					Tracers:      tracers,
					Wake:         wake,
					Host:         cfg.Host,
					BoardID:      proj.BoardID,
					Concurrency:  1,
					Poll:         cfg.Poll,
					AgentAuthors: cfg.AgentAuthors,
				}))
			slog.Info("["+proj.Name+"] security reviewer enabled", "class", string(cfg.SecClass))

			// CONCURRENCY 1, because the whole job is "absorb my siblings": two
			// workers on sibling tickets would each try to absorb the other, and
			// whichever wrote second would produce a brief missing the first.
			//
			// Only registered when merging is asked for. The stage is otherwise a
			// dispatcher polling a column nothing is ever put into, which costs a
			// poll per interval and reads in the window as a stage that never runs.
			if onePlanTask() {
				out = append(out, NewDispatcher(api, recorder,
					NewTicketMergeAgent(api), DispatcherOpts{
						Tracers:      tracers,
						Wake:         wake,
						Host:         cfg.Host,
						BoardID:      proj.BoardID,
						Concurrency:  1,
						Poll:         cfg.Poll,
						AgentAuthors: cfg.AgentAuthors,
					}))
				slog.Info("[" + proj.Name + "] ticket merge enabled: the product manager's tasks are folded into one before specification")
			}

			// UPKEEP, when it is asked for and the repository has a linter to be
			// clean against. Registered on the same terms as ticket merge above: a
			// dispatcher on a column nothing is ever put into is a poll per interval
			// and a stage that reads as permanently idle.
			//
			// CONCURRENCY 1. There is only ever one upkeep ticket open, and its
			// agent refuses to claim while anything anyone asked for is live.
			if proj.AutoMode() && proj.Repo.LintCommand != "" {
				out = append(out, NewDispatcher(api, recorder,
					NewMaintenanceAgent(gw, api, cfg.DevClass, proj.Repo, cfg.DevMaxIterations, proj.BoardID), DispatcherOpts{
						Tracers:      tracers,
						Wake:         wake,
						Host:         cfg.Host,
						BoardID:      proj.BoardID,
						Concurrency:  1,
						Poll:         cfg.Poll,
						AgentAuthors: cfg.AgentAuthors,
					}))
				slog.Info("[" + proj.Name + "] upkeep enabled: the linter's findings are filed as low-priority work")
			}

			// CONCURRENCY 1, and not negotiable: every merge moves the branch the
			// next merge starts from, so two at once is two pushes racing for one
			// ref. The loser either force-pushes over work or fails in a way that
			// looks random. This stage calls no model, so a single worker is not a
			// throughput problem — a merge is a clone and a test run.
			out = append(out, NewDispatcher(api, recorder,
				NewIntegratorAgent(api, proj.Repo, proj.IntegrationBranch), DispatcherOpts{
					Tracers:      tracers,
					Wake:         wake,
					Host:         cfg.Host,
					BoardID:      proj.BoardID,
					Concurrency:  1,
					Poll:         cfg.Poll,
					AgentAuthors: cfg.AgentAuthors,
				}))
			slog.Info("["+proj.Name+"] integrator enabled: reviewed branches merge here and the gates re-run on the result",
				"branch", proj.IntegrationBranch, "base", proj.Repo.Branch)

			// Serial for the same reason as the integrator, and for one more: this
			// is the only agent that writes to the integration branch.
			if _, ok := cfg.Classes[cfg.DevClass]; ok {
				out = append(out, NewDispatcher(api, recorder,
					NewResolverAgent(gw, api, cfg.DevClass, proj.Repo, proj.IntegrationBranch), DispatcherOpts{
						Tracers:      tracers,
						Wake:         wake,
						Host:         cfg.Host,
						BoardID:      proj.BoardID,
						Concurrency:  1,
						Poll:         cfg.Poll,
						AgentAuthors: cfg.AgentAuthors,
					}))
				slog.Info("["+proj.Name+"] conflict resolver enabled: one attempt per conflict, gates must pass before anything is pushed",
					"class", string(cfg.DevClass), "branch", proj.IntegrationBranch)
			}
		} else {
			slog.Warn("["+proj.Name+"] security reviewer DISABLED: its model class is not served here",
				"class", string(cfg.SecClass))
		}
		slog.Info("["+proj.Name+"] developer agent enabled",
			"repo", proj.Repo.URL, "branch", proj.Repo.Branch, "image", proj.Repo.Image,
			"pipeline", proj.Repo.PipelineID, "max_iterations", cfg.DevMaxIterations,
			"credentialled", proj.Repo.SecretRef != "",
			"concurrency", cfg.DevConcurrency)
		// Which definition of "tested" is in force is worth one line at startup:
		// the two are not equivalent, and the weaker one is the silent default.
		if unpinned := cfg.unpinnedTools(); len(unpinned) > 0 {
			slog.Warn("["+proj.Name+"] a sandbox command fetches a tool at @latest: that is code chosen by its publisher, executed in the sandbox, and able to change between runs of the same ticket. Name a version",
				"settings", strings.Join(unpinned, ", "))
		}
		if cfg.VerifiesWithPipeline() {
			slog.Info("["+proj.Name+"] changes are verified by the platform pipeline", "pipeline", proj.Repo.PipelineID)
		} else {
			slog.Warn("["+proj.Name+"] changes are verified IN THE SANDBOX, not by a pipeline: this can drift from CI. Set AGENTS_REPO_PIPELINE_ID once a pipeline can reach this repository",
				"command", proj.Repo.TestCommand)
		}
	} else {
		slog.Info("[" + proj.Name + "] developer agent disabled (needs AGENTS_REPO_URL, plus AGENTS_REPO_PIPELINE_ID or AGENTS_REPO_TEST_COMMAND); this host triages only")
	}

	return out, nil
}

// withIntegration tells an agent where merged work lands. Kept as a copy rather
// than mutating the project's config, because several agents share it and only
// the developer needs to merge.
func withIntegration(repo RepoConfig, branch string) RepoConfig {
	repo.IntegrationBranch = branch
	return repo
}
