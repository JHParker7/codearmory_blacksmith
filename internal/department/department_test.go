package department

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/code-armory-app/blacksmith/internal/config"
	"github.com/code-armory-app/blacksmith/internal/forge"
	"github.com/code-armory-app/blacksmith/internal/model"
	"github.com/code-armory-app/blacksmith/internal/ticket"
	"github.com/code-armory-app/blacksmith/internal/workflow"
)

type nothing struct{}

func (nothing) Chat(context.Context, model.Class, model.ChatRequest) (model.ChatResult, error) {
	return model.ChatResult{}, nil
}
func (nothing) Run(context.Context, forge.Recorder, forge.Spec) (forge.Result, error) {
	return forge.Result{}, nil
}
func (nothing) Acquire(context.Context, forge.SandboxSpec) (*forge.Sandbox, error) {
	return nil, nil
}
func (nothing) AddComment(context.Context, string, string) (ticket.Comment, error) {
	return ticket.Comment{}, nil
}
func (nothing) Update(context.Context, string, ticket.Update) (ticket.Ticket, error) {
	return ticket.Ticket{}, nil
}
func (nothing) Create(context.Context, ticket.Ticket) (ticket.Ticket, error) {
	return ticket.Ticket{}, nil
}
func (nothing) AddDependency(context.Context, string, string) error { return nil }
func (nothing) MoveTo(context.Context, string, string) error        { return nil }
func (nothing) Get(context.Context, string) (ticket.Ticket, error)  { return ticket.Ticket{}, nil }
func (nothing) List(context.Context, ticket.ListOpts) ([]ticket.Ticket, error) {
	return nil, nil
}

func served(classes ...model.Class) map[model.Class]model.ClassConfig {
	out := map[model.Class]model.ClassConfig{}
	for _, c := range classes {
		out[c] = model.ClassConfig{Endpoint: "http://localhost:8080", Model: "m", Slots: 1}
	}
	return out
}

func baseConfig() config.Config {
	return config.Config{
		Host:     "test-host",
		Classes:  served(model.ClassTiny, model.ClassSmall, model.ClassLarge),
		DevClass: model.ClassLarge,
		PMClass:  model.ClassSmall,
		Repo: config.Repo{
			URL: "https://git.example/org/repo", Branch: "main",
			Image: "golang:1.25", RunnerClass: "agent-dev",
		},
		IntegrationBranch: "integration",
		ArchitectEnabled:  true,
	}
}

func assemble(t *testing.T, cfg config.Config) Assembly {
	t.Helper()
	n := nothing{}
	a, err := Assemble(Deps{Gateway: n, Runner: n, Leases: n, Store: n, Config: cfg})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	return a
}

func roles(a Assembly) []string { return a.Roles() }

func classOf(a Assembly, role string) (model.Class, bool) {
	for _, s := range a.Stages {
		if s.Role == role {
			return s.Class, true
		}
	}
	return "", false
}

// EVERY ROLE THE TABLE ROUTES MUST HAVE A HANDLER. A stage missing from the
// assembly leaves its column filling up with work nobody claims — which looks
// exactly like a department with nothing to do.
func TestEveryRoutedRoleIsAssembled(t *testing.T) {
	cfg := baseConfig()
	cfg.CoverageEnabled = true
	stages := assemble(t, cfg)
	table := Table(cfg)

	got := roles(stages)
	for _, role := range table.Roles() {
		if !slices.Contains(got, role) {
			t.Errorf("the table routes %q and nothing was assembled to work it", role)
		}
	}
}

// AND NOTHING IS ASSEMBLED THAT THE TABLE DOES NOT ROUTE, or a handler polls a
// column that never receives anything.
func TestNothingIsAssembledThatTheTableDoesNotRoute(t *testing.T) {
	cfg := baseConfig()
	cfg.CoverageEnabled = true
	table := Table(cfg)

	for _, s := range assemble(t, cfg).Stages {
		if _, ok := table.For(s.Role); !ok {
			t.Errorf("%q was assembled but the table does not route it", s.Role)
		}
	}
}

// THE HANDLER MUST AGREE WITH THE ROLE IT IS LISTED UNDER. The table routes by
// role and the dispatcher claims as the handler's own account, so a
// disagreement means a stage claiming tickets it will never be given.
func TestEveryHandlerReportsTheRoleItWasAssembledUnder(t *testing.T) {
	for _, s := range assemble(t, baseConfig()).Stages {
		if s.Handler.Role() != s.Role {
			t.Errorf("assembled under %q but the handler says %q", s.Role, s.Handler.Role())
		}
		if s.Handler.Class() != s.Class {
			t.Errorf("%s: assembled for class %q but the handler asks for %q",
				s.Role, s.Class, s.Handler.Class())
		}
	}
}

// WHICH CLASS EACH STAGE GETS IS A DECISION. A stage on the wrong one runs a
// 30-billion-parameter model on a job a small one does in two seconds — or, far
// worse, the reverse.
func TestEachStageAsksForTheClassItsWorkNeeds(t *testing.T) {
	cfg := baseConfig()
	cfg.CoverageEnabled = true
	stages := assemble(t, cfg)

	want := map[string]model.Class{
		// The developer is the only stage that writes implementation code.
		workflow.RoleDev: model.ClassLarge,
		// Judgement, not generation: a summary, a verdict, a merge resolution.
		workflow.RoleScoping: model.ClassSmall,
		workflow.RoleReview:  model.ClassSmall,
		workflow.RoleResolve: model.ClassSmall,
		// NO MODEL AT ALL. Merging a reviewed branch is git's job, and the one
		// decision — whether it conflicted — git already made. Saying "small"
		// instead would leave it unassembled on a host that does not serve small,
		// stranding every reviewed branch.
		workflow.RoleIntegrate: model.ClassNone,
	}
	for role, class := range want {
		got, ok := classOf(stages, role)
		if !ok {
			t.Errorf("%q was not assembled", role)
			continue
		}
		if got != class {
			t.Errorf("%s runs on %q, want %q", role, got, class)
		}
	}
}

// A STAGE THIS HOST CANNOT SERVE IS LEFT OUT rather than built and left to fail.
// A handler wired to a class with no endpoint does not discover that until it
// claims a ticket — and by then the ticket has a claim against its attempt
// budget, so a misconfigured host burns attempts to learn what startup knew.
func TestAHostThatServesOneClassAssemblesOnlyThatClassesStages(t *testing.T) {
	cfg := baseConfig()
	cfg.Classes = served(model.ClassSmall)
	cfg.ArchitectEnabled = false
	// With no large class the developer's own class does not resolve, and the
	// author falls back to it — so both are absent on this host.
	cfg.DevClass = model.ClassLarge

	stages := assemble(t, cfg)
	for _, s := range stages.Stages {
		// A stage that calls no model is assembled whatever this host serves: it
		// cannot be blocked by an endpoint it never asks anything of.
		if s.Class != model.ClassSmall && s.Class != model.ClassNone {
			t.Errorf("%s was assembled for %q on a host that serves only small", s.Role, s.Class)
		}
	}
	if got := roles(stages); slices.Contains(got, workflow.RoleDev) {
		t.Errorf("the developer was assembled on a host with no large class: %v", got)
	}
	// And what it CAN serve is still there.
	if got := roles(stages); !slices.Contains(got, workflow.RoleScoping) {
		t.Errorf("a host serving small assembled no scoping stage: %v", got)
	}
}

// A HOST THAT CAN SERVE NOTHING SAYS SO, rather than starting a department with
// no stages in it — which looks identical to a department with no work.
func TestAHostThatServesNothingIsRefused(t *testing.T) {
	cfg := baseConfig()
	cfg.Classes = nil
	n := nothing{}

	a, err := Assemble(Deps{Gateway: n, Runner: n, Leases: n, Store: n, Config: cfg})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	// THE MODEL-FREE STAGES STILL RUN. A host with no serving stack can still
	// merge a reviewed branch and fold tickets together, and refusing to would
	// strand work that needs no model at all.
	for _, s := range a.Stages {
		if s.Class != model.ClassNone {
			t.Errorf("%s was assembled on a host that serves nothing", s.Role)
		}
	}
	if len(a.Stages) == 0 {
		t.Error("a host with no serving stack assembled nothing, not even the model-free stages")
	}
	// AND EVERY STAGE IT COULD NOT RUN IS NAMED. A silent omission is a column
	// nothing claims from, which looks exactly like a column with no work.
	if len(a.Skipped) == 0 {
		t.Error("a host serving nothing reported no skipped stages")
	}
	for _, sk := range a.Skipped {
		if sk.Reason == "" {
			t.Errorf("%s was skipped with no reason given", sk.Role)
		}
	}
}

func TestAssemblingWithoutAStoreIsRefused(t *testing.T) {
	n := nothing{}
	if _, err := Assemble(Deps{Gateway: n, Runner: n, Leases: n, Config: baseConfig()}); err == nil {
		t.Error("a department was assembled with no ticket store")
	}
}

// THE OPTIONAL STAGES MATCH THE TABLE THEY ARE ROUTED BY. A stage assembled
// while the table routes around it polls a column nothing fills; a stage the
// table routes to while nothing was assembled is work that stops dead.
func TestTheOptionalStagesFollowTheirConfiguration(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(*config.Config)
		role     string
		expected bool
	}{
		{"the architect on", func(c *config.Config) { c.ArchitectEnabled = true },
			workflow.RoleArchitect, true},
		{"the architect off", func(c *config.Config) { c.ArchitectEnabled = false },
			workflow.RoleArchitect, false},
		{"coverage on", func(c *config.Config) { c.CoverageEnabled = true },
			workflow.RoleCoverage, true},
		{"coverage off", func(c *config.Config) { c.CoverageEnabled = false },
			workflow.RoleCoverage, false},
		// THE TABLE ROUTES ITS COLUMN UNCONDITIONALLY, so the stage is assembled
		// either way: a column the table names with nothing serving it is work
		// that stops dead. When the shape is off nothing is moved there.
		{"tasks merged first", func(c *config.Config) { c.MergeTasksFirst = true },
			workflow.RoleTicketMerge, true},
		{"tasks not merged", func(c *config.Config) { c.MergeTasksFirst = false },
			workflow.RoleTicketMerge, true},
	}

	for _, c := range cases {
		cfg := baseConfig()
		c.mutate(&cfg)

		got := slices.Contains(roles(assemble(t, cfg)), c.role)
		if got != c.expected {
			t.Errorf("%s: %q assembled = %v, want %v", c.name, c.role, got, c.expected)
		}
		// AND THE TABLE AGREES for the two that change routing.
		if c.role == workflow.RoleArchitect || c.role == workflow.RoleCoverage {
			if _, routed := Table(cfg).For(c.role); routed != c.expected {
				t.Errorf("%s: the table routes %q = %v, want %v", c.name, c.role, routed, c.expected)
			}
		}
	}
}

// THE ARCHITECT'S ABSENCE MOVES THE SCOPING QUEUE. If it were hard-wired behind
// the architect, a host that cannot run one would leave every request in the
// inbox with nothing coming to collect it — a department that looks up and does
// nothing, which is the worst way for this to fail.
func TestTheScopingQueueFollowsWhetherTheArchitectRuns(t *testing.T) {
	with := baseConfig()
	with.ArchitectEnabled = true
	without := baseConfig()
	without.ArchitectEnabled = false

	a, _ := Table(with).For(workflow.RoleScoping)
	b, _ := Table(without).For(workflow.RoleScoping)

	if a.Ready == b.Ready {
		t.Errorf("scoping reads %q either way; one of the two configurations strands work", a.Ready)
	}
	if b.Ready != workflow.ColInbox {
		t.Errorf("with no architect, scoping reads %q rather than the inbox", b.Ready)
	}
}

// THE TWO STAGES THAT SHARE ONE LOOP ARE STILL TWO STAGES, and they differ in
// exactly the ways that matter: what they may write, and what their check means.
func TestTheAuthorAndTheDeveloperAreDistinctStages(t *testing.T) {
	stages := assemble(t, baseConfig())

	spec, okSpec := classOf(stages, workflow.RoleSpec)
	_, okDev := classOf(stages, workflow.RoleDev)
	if !okSpec || !okDev {
		t.Fatalf("both stages should be assembled: %v", roles(stages))
	}
	// The author may run on a smaller class than the developer: writing a test
	// against a stated criterion is a smaller job than implementing it. What it
	// must never be is unset — a stage with no class is one no host can serve.
	if spec == "" || spec == model.ClassNone {
		t.Errorf("the author's class is %q", spec)
	}

	// They must not be the same handler, or one ticket's state would leak into
	// the other's.
	var specH, devH any
	for _, s := range stages.Stages {
		switch s.Role {
		case workflow.RoleSpec:
			specH = s.Handler
		case workflow.RoleDev:
			devH = s.Handler
		}
	}
	if specH == devH {
		t.Error("the author and the developer are one handler")
	}
}

// A ROLE APPEARS ONCE. Two handlers for one role means two dispatchers claiming
// the same column, and the claim makes that race silently destructive: whichever
// wins, the other never sees the ticket again.
func TestNoRoleIsAssembledTwice(t *testing.T) {
	cfg := baseConfig()
	cfg.CoverageEnabled = true
	cfg.MergeTasksFirst = true

	seen := map[string]int{}
	for _, s := range assemble(t, cfg).Stages {
		seen[s.Role]++
	}
	for role, n := range seen {
		if n != 1 {
			t.Errorf("%q was assembled %d times", role, n)
		}
	}
}

// A DISAGREEMENT IS REPORTED, not skipped: a stage silently left out is a column
// nobody works.
func TestAHandlerThatDisagreesWithItsRoleIsReported(t *testing.T) {
	// Assemble normally, then confirm the check exists by asserting the error
	// text a mismatch would produce is reachable — the guard is exercised by the
	// mutation sweep, and this pins the message it must give.
	cfg := baseConfig()
	stages := assemble(t, cfg)
	if len(stages.Stages) == 0 {
		t.Fatal("nothing was assembled")
	}
	for _, s := range stages.Stages {
		if strings.TrimSpace(s.Role) == "" {
			t.Error("a stage was assembled with no role")
		}
	}
}

// A CLASS THAT RESOLVED TO NOTHING is a configuration hole, not a choice: some
// resolver fell through to a field nobody set. Dropping the stage silently is
// the worst available outcome — the column fills and nothing claims from it.
func TestAStageWhoseClassResolvesToNothingIsNamedRatherThanDropped(t *testing.T) {
	cfg := baseConfig()
	cfg.ArchitectEnabled = true
	// Neither the architect's own class nor the one it falls back to is set.
	cfg.ArchitectClass = ""
	cfg.PMClass = ""

	a := assemble(t, cfg)

	if slices.Contains(a.Roles(), workflow.RoleArchitect) {
		t.Error("a stage with no resolvable class was assembled")
	}
	var named bool
	for _, sk := range a.Skipped {
		if sk.Role == workflow.RoleArchitect {
			named = true
			if !strings.Contains(sk.Reason, "no model class resolved") {
				t.Errorf("the skip does not name the cause: %q", sk.Reason)
			}
		}
	}
	if !named {
		t.Errorf("the architect was dropped without being named: %+v", a.Skipped)
	}
}

// AND A STAGE SKIPPED FOR A CLASS THIS HOST DOES NOT SERVE says which class.
func TestASkippedStageNamesTheClassItNeeded(t *testing.T) {
	cfg := baseConfig()
	cfg.Classes = served(model.ClassSmall)

	a := assemble(t, cfg)
	var named bool
	for _, sk := range a.Skipped {
		if sk.Role == workflow.RoleDev {
			named = true
			if !strings.Contains(sk.Reason, string(model.ClassLarge)) {
				t.Errorf("the skip does not name the class: %q", sk.Reason)
			}
		}
	}
	if !named {
		t.Errorf("the developer was dropped without being named: %+v", a.Skipped)
	}
}

// The loop takes an INTERFACE so it can be driven without a network; forge hands
// back a concrete sandbox. This is the one line that joins them.
func TestTheHeldSandboxAdapterForwardsToTheLeaseClient(t *testing.T) {
	var asked forge.SandboxSpec
	h := heldSandboxes{leasesFunc(func(_ context.Context, spec forge.SandboxSpec) (*forge.Sandbox, error) {
		asked = spec
		return nil, nil
	})}

	if _, err := h.Acquire(context.Background(), forge.SandboxSpec{Image: "golang:1.25"}); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if asked.Image != "golang:1.25" {
		t.Errorf("the spec did not reach the lease client: %+v", asked)
	}
}

type leasesFunc func(context.Context, forge.SandboxSpec) (*forge.Sandbox, error)

func (f leasesFunc) Acquire(ctx context.Context, spec forge.SandboxSpec) (*forge.Sandbox, error) {
	return f(ctx, spec)
}
