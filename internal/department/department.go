// Package department assembles the stages into a running pipeline.
//
// THE ASSEMBLY IS THE PIPELINE. Which stages exist, which model class each one
// gets, and which columns it moves work between are decisions — not plumbing —
// and every one of them has been got wrong at least once in a way that was
// silent. A stage wired to the wrong class runs a 30-billion-parameter model on
// a job a small one does in two seconds; a stage missing from the table leaves
// its column filling up with work nobody claims.
//
// So this lives in a package with tests rather than in main, where the only way
// to check it is to run the department and watch.
package department

import (
	"context"
	"fmt"
	"strings"

	"github.com/code-armory-app/blacksmith/internal/agent/architect"
	"github.com/code-armory-app/blacksmith/internal/agent/dev"
	"github.com/code-armory-app/blacksmith/internal/agent/integrate"
	"github.com/code-armory-app/blacksmith/internal/agent/referee"
	"github.com/code-armory-app/blacksmith/internal/agent/resolve"
	"github.com/code-armory-app/blacksmith/internal/agent/review"
	"github.com/code-armory-app/blacksmith/internal/agent/scope"
	"github.com/code-armory-app/blacksmith/internal/agent/ticketmerge"
	"github.com/code-armory-app/blacksmith/internal/config"
	"github.com/code-armory-app/blacksmith/internal/dispatch"
	"github.com/code-armory-app/blacksmith/internal/forge"
	"github.com/code-armory-app/blacksmith/internal/model"
	"github.com/code-armory-app/blacksmith/internal/workflow"
)

// Deps are what the stages are built from.
type Deps struct {
	Gateway dev.Gateway

	// Runner runs ONE-SHOT commands, and Leases hands out a HELD sandbox.
	//
	// TWO SHAPES BECAUSE THERE ARE TWO KINDS OF STAGE. A review or an integration
	// runs a single script and is done, where a developer runs a survey, several
	// reads and several verifications against ONE checkout — and holding the
	// container across those removes a microVM boot and a clone from every
	// command but the first, which measured as most of a task's wall time.
	Runner Runner
	Leases Leases

	Store  Store
	Config config.Config
}

// Runner runs a one-shot command in a fresh container.
type Runner interface {
	Run(ctx context.Context, rec forge.Recorder, spec forge.Spec) (forge.Result, error)
}

// Leases hands out a sandbox held for the length of one ticket.
type Leases interface {
	Acquire(ctx context.Context, spec forge.SandboxSpec) (*forge.Sandbox, error)
}

// heldSandboxes adapts a lease client to what the developer's loop asks for.
//
// The loop takes an INTERFACE so it can be driven without a network; forge hands
// back a concrete sandbox. This is the one line that joins them, and it lives
// here rather than in either package so neither has to know about the other's
// testing arrangements.
type heldSandboxes struct{ Leases }

func (h heldSandboxes) Acquire(ctx context.Context, spec forge.SandboxSpec) (dev.Box, error) {
	return h.Leases.Acquire(ctx, spec)
}

// Store is every board operation any stage needs, in one interface.
//
// ONE INTERFACE HERE AND NARROW ONES AT THE STAGES. Each stage declares only
// what it uses, which is what lets them be tested against three-line fakes; this
// is the one place that has to satisfy all of them at once, and it is satisfied
// by the real client.
type Store interface {
	scope.Store
	architect.Store
	review.Store
	integrate.Store
	resolve.Store
	ticketmerge.Store
	dev.Store
}

// Assembly is what this host will run, and what it will not.
//
// THE SKIPS ARE RETURNED RATHER THAN SWALLOWED. A stage dropped because its
// class does not resolve is a column nothing will ever claim from, and a host
// that silently works no tickets looks exactly like a host with no tickets to
// work — which is the most expensive way for this to fail, because nothing
// points at the cause.
type Assembly struct {
	Stages  []Stage
	Skipped []Skip
}

// Skip is a stage this host cannot run, and why.
type Skip struct {
	Role   string
	Class  model.Class
	Reason string
}

// Roles names what will run.
func (a Assembly) Roles() []string {
	out := make([]string, 0, len(a.Stages))
	for _, s := range a.Stages {
		out = append(out, s.Role)
	}
	return out
}

// Stage is one assembled handler, and the role and class it runs as.
//
// Both are recorded so the assembly can be CHECKED without running it: which
// roles a configuration produces, and what each one will ask the gateway for.
type Stage struct {
	Handler dispatch.Handler
	Role    string
	Class   model.Class
}

// Assemble builds every stage this host is configured to run.
//
// A STAGE THIS HOST CANNOT SERVE IS LEFT OUT rather than built and left to fail.
// A handler wired to a class with no endpoint does not discover that until it
// claims a ticket — and by then the ticket has a claim against its attempt
// budget, so a misconfigured host burns three attempts per ticket to learn what
// startup already knew.
func Assemble(d Deps) (Assembly, error) {
	cfg := d.Config
	if d.Store == nil {
		return Assembly{}, fmt.Errorf("department: no ticket store")
	}

	var out Assembly
	var problems []string

	add := func(role string, class model.Class, h dispatch.Handler) {
		// THE HANDLER MUST AGREE WITH THE ROLE IT IS LISTED UNDER. The table routes
		// by role and the dispatcher claims as the handler's own account, so a
		// disagreement means a stage that claims tickets it will never be given —
		// and the column it was meant to serve fills up with work nobody collects.
		if h.Role() != role {
			problems = append(problems, fmt.Sprintf(
				"%s is assembled under %q but reports itself as %q", class, role, h.Role()))
			return
		}
		// A STAGE THIS HOST CANNOT SERVE IS LEFT OUT rather than built and left to
		// fail on its first ticket — EXCEPT one that calls no model, which cannot
		// be blocked by a missing endpoint. Gating those on a class it never uses
		// leaves the integrator unassembled on a host serving only large, and
		// every reviewed branch then sits in ready_for_integration forever.
		if class == model.ClassNone {
			out.Stages = append(out.Stages, Stage{Handler: h, Role: role, Class: class})
			return
		}
		if class == "" {
			// A CLASS THAT RESOLVED TO NOTHING is a configuration hole, not a
			// choice: some resolver fell through to a field nobody set.
			out.Skipped = append(out.Skipped, Skip{role, class,
				"no model class resolved for this stage"})
			return
		}
		if !cfg.Serves(class) {
			out.Skipped = append(out.Skipped, Skip{role, class,
				"this host does not serve the " + string(class) + " class"})
			return
		}
		out.Stages = append(out.Stages, Stage{Handler: h, Role: role, Class: class})
	}

	// THE ARCHITECT IS OPTIONAL AND ON BY DEFAULT. What it removes is worse than
	// the minute it costs: without it the breakdown emits documentation subtasks,
	// and a doc ticket cannot survive the pipeline — the author that receives it
	// may write only test files while the ticket asks for a README, so no
	// permitted action ends the loop.
	if cfg.ArchitectEnabled {
		class := cfg.ResolveArchitectClass()
		add(workflow.RoleArchitect, class,
			architect.New(d.Gateway, d.Runner, d.Store, class, cfg.Repo))
	}

	add(workflow.RoleScoping, model.ClassSmall,
		scope.New(d.Gateway, d.Store, model.ClassSmall, scope.Options{
			MergeTasksFirst:      cfg.MergeTasksFirst,
			OneSpecAuthorPerTask: cfg.OneSpecAuthorPerTask,
		}))

	// THE MERGE STAGE CALLS NO MODEL: merging tickets is concatenation, and a
	// model asked to concatenate can paraphrase, reorder or drop.
	//
	// ASSEMBLED WHATEVER THE SHAPE, because the table routes its column
	// unconditionally — and a column the table names with nothing serving it is
	// work that stops dead. When the shape is off nothing is ever moved there, so
	// the stage simply never claims anything.
	add(workflow.RoleTicketMerge, model.ClassNone, ticketmerge.New(d.Store))

	// FOUR STAGES SHARE ONE LOOP, differing in what they may write and what their
	// check means. See dev.Mode.
	specClass := cfg.ResolveTestClass()
	writesTests := func(role string, mode dev.Mode) {
		add(role, specClass, dev.New(d.Gateway, heldSandboxes{d.Leases}, d.Store, specClass, cfg.Repo,
			dev.Options{
				Mode: mode, Role: role,
				MaxTurns: cfg.SpecMaxIterations, Tools: cfg.Classes[specClass].ToolsSupported,
			}))
	}

	// THE TEST AUTHOR AND THE SECTION AUTHOR ARE THE SAME JOB ON DIFFERENT
	// QUEUES. A request that is ONE unit of work goes straight to the test
	// author; a request broken into several has a section author per slice, all
	// writing onto the task's branch. Both write the failing tests the developer
	// is then held to.
	writesTests(workflow.RoleTest, dev.ModeTest)
	writesTests(workflow.RoleSpec, dev.ModeTest)

	// AND THE RECONCILER, because the sections are written by agents that cannot
	// see each other: two declare the same helper and the package does not build.
	// Without this the developer is handed a branch that does not compile as
	// though it were its own fault.
	writesTests(workflow.RoleSpecMerge, dev.ModeSpecMerge)

	// THE REFEREE IS THE DEVELOPER'S ONLY WAY TO SPOT A SPECIFICATION THAT
	// COMPILES AND STILL CANNOT BE SATISFIED. The mechanical routes read the
	// compiler; this reads the tests. It runs once, before the first turn, and
	// only for the developer — every other stage may edit the tests it is given.
	add(workflow.RoleDev, model.ClassLarge, dev.New(d.Gateway, heldSandboxes{d.Leases}, d.Store, model.ClassLarge, cfg.Repo,
		dev.Options{
			Mode: dev.ModeDevelop, Role: workflow.RoleDev,
			MaxTurns: cfg.DevMaxIterations, Tools: cfg.Classes[model.ClassLarge].ToolsSupported,
			Referee: referee.New(d.Gateway, model.ClassLarge),
		}))

	// COVERAGE IS OFF BY DEFAULT: it works, and it is in the wrong place.
	// Measured on a live build — the developer pushed in 90 seconds and the
	// coverage stage then held the ticket for over seven minutes with five
	// siblings idle behind it. It cannot fail a ticket and its output is
	// additive, so gating a fan-out on it is pure latency.
	if cfg.CoverageEnabled {
		class := cfg.ResolveCoverageClass()
		add(workflow.RoleCoverage, class, dev.New(d.Gateway, heldSandboxes{d.Leases}, d.Store, class, cfg.Repo,
			dev.Options{
				Mode: dev.ModeCoverage, Role: workflow.RoleCoverage,
				MaxTurns: cfg.DevMaxIterations, Tools: cfg.Classes[class].ToolsSupported,
			}))
	}

	// THE MAINTENANCE STAGE fixes what the linter found, on a branch that has
	// already been reviewed. It is the developer's loop on a smaller job, so it
	// runs on the smaller class.
	add(workflow.RoleMaintain, specClass,
		dev.New(d.Gateway, heldSandboxes{d.Leases}, d.Store, specClass, cfg.Repo,
			dev.Options{
				Mode: dev.ModeDevelop, Role: workflow.RoleMaintain,
				MaxTurns: cfg.DevMaxIterations, Tools: cfg.Classes[specClass].ToolsSupported,
			}))

	add(workflow.RoleReview, model.ClassSmall,
		review.New(d.Gateway, d.Runner, d.Store, model.ClassSmall, cfg.Repo))

	// THE INTEGRATOR CALLS NO MODEL EITHER. Merging a reviewed branch is git's
	// job, and the one decision — whether it conflicted — git already made.
	add(workflow.RoleIntegrate, model.ClassNone,
		integrate.New(d.Runner, d.Store, cfg.Repo, cfg.IntegrationBranch))

	add(workflow.RoleResolve, model.ClassSmall,
		resolve.New(d.Gateway, d.Runner, d.Store, model.ClassSmall,
			cfg.Repo, cfg.IntegrationBranch))

	if len(problems) > 0 {
		return Assembly{}, fmt.Errorf("department: %s", strings.Join(problems, "; "))
	}
	if len(out.Stages) == 0 {
		return Assembly{}, fmt.Errorf("department: no stage can run at all on this host")
	}
	return out, nil
}

// Table is the routing every dispatcher shares.
//
// ONE TABLE, BUILT ONCE. Two dispatchers holding different ideas of where
// success goes is a hand-off that silently stops: the producing stage moves a
// ticket to a column the consuming stage is not watching.
func Table(cfg config.Config) workflow.Table {
	return workflow.New(workflow.Options{
		Architect: cfg.ArchitectEnabled,
		Coverage:  cfg.CoverageEnabled,
	})
}
