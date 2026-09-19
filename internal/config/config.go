// Package config is the host's whole routing table: which models it serves,
// which repository it works, and which half of the deployment it is.
//
// Everything here comes from the environment, because the unit reads an
// EnvironmentFile and that is what an operator edits. The one rule that file
// imposes on this package: systemd does not strip inline comments, so
// `FOO=2 # note` sets the value `2 # note` — every numeric setting has to fail
// loudly on that rather than silently defaulting.
package config

import (
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/code-armory-app/blacksmith/internal/model"
)

// Defaults. Each of these was measured or argued for; the reasoning lives beside
// the field it fills in.
const (
	DefaultSlots      = 4
	DefaultQueueDepth = 32

	// DefaultDevConcurrency is how many developer sandboxes may run at once, and
	// DefaultTestConcurrency matches it: the author is the stage that FEEDS
	// development, so a narrower one would move the queue one column left and
	// leave developer slots idle waiting for specifications.
	DefaultDevConcurrency  = 4
	DefaultTestConcurrency = 4

	// DefaultCoverageTarget is conventional rather than derived: high enough that
	// the error paths have to be exercised, low enough not to demand tests for
	// generated code and trivial accessors.
	DefaultCoverageTarget = 80

	// DefaultDevMemoryMB and DefaultDevCPUMillicores size ONE developer sandbox.
	//
	// GBs and whole cores rather than the MBs a container tier would take, because
	// a real toolchain compiling inside a microVM pays for the guest's own kernel
	// and page cache on top of the workload. Measured on the sandbox plane: a
	// seeded 256 MB class fails `go test ./...` on a ONE-FILE repository three
	// runs in five — an OOM kill mid-compile, not a clean error — while 512 MB and
	// up passed every time. This is far above that floor on purpose, because the
	// floor was measured against a trivial repository.
	DefaultDevMemoryMB      = 8192
	DefaultDevCPUMillicores = 4000

	// DefaultDevRunnerClass is a DEDICATED class rather than forge's shared one:
	// the sizing above is specific to agent work, and writing it onto a shared
	// tier would silently re-size every other tenant's executions too.
	DefaultDevRunnerClass = "agent-dev"

	// DefaultDevPidsLimit and DefaultDevDiskGB round out the class. A parallel
	// build forks a compiler per core, so the pid ceiling is generous relative to
	// the core count rather than tight against it.
	DefaultDevPidsLimit = 512
	DefaultDevDiskGB    = 40

	// DefaultDevTmpfsMB sizes /tmp, and /tmp is where EVERYTHING the toolchain
	// writes lives: SandboxEnv puts HOME, GOPATH, the Go build cache and the
	// module cache all under it. A class that declares no tmpfs at all is
	// rejected by forge — "tmpfs_mb must be at least 16" — which is the only
	// reason an earlier version of this declaration did not SHRINK the working
	// class to nothing on startup. Set to what the live class was measured
	// carrying, rather than to a guess.
	DefaultDevTmpfsMB = 2048

	// DefaultIntegrationBranch is where reviewed work lands. NOT the base branch:
	// the base is what agents cut from and what stays known-good, and merging into
	// it directly would mean a broken integration takes the branch every
	// subsequent agent starts from.
	DefaultIntegrationBranch = "dev"

	// DefaultLeaseIdleSecs and DefaultLeaseMaxSecs bound a sandbox the agent fails
	// to release. Both are generous relative to a task because they exist to catch
	// a CRASHED agent rather than to end a healthy one: a developer task that hits
	// either is one something went wrong in, and cutting it short while it is
	// still working turns a slow ticket into a failed one.
	DefaultLeaseIdleSecs = 600
	DefaultLeaseMaxSecs  = 3600

	// DefaultTranscriptDir keeps the corpus beside the service's other state
	// rather than in /tmp, which is cleared on reboot.
	DefaultTranscriptDir = "/var/lib/codearmory-agents/transcripts"

	// TranscriptOff is the single value that disables capture.
	TranscriptOff = "off"

	// DefaultMaxIterations bounds one ticket's loop.
	DefaultMaxIterations = 200

	// DefaultPoll is the level-trigger interval.
	DefaultPoll = 15 * time.Second

	// DefaultTimeoutSecs bounds a single sandbox run.
	DefaultTimeoutSecs = 900
)

// The default repository commands.
//
// FormatCommand pins a version rather than taking @latest: `go run tool@latest`
// downloads and executes code chosen by whoever published it most recently,
// inside the sandbox, on every run.
const (
	DefaultFormatCommand   = "go run golang.org/x/tools/cmd/goimports@v0.28.0 -w . || gofmt -w ."
	DefaultDepsCommand     = "go mod tidy"
	DefaultCoverageCommand = "go test -coverprofile=coverage.out ./... && go tool cover -func=coverage.out | tail -1"
)

// DefaultAgentAuthors is the loop breaker's exclusion list: the agent accounts
// the platform seeds. A ticket one of them authored is never selected at intake,
// or the product manager's own output feeds straight back into the department.
var DefaultAgentAuthors = []string{"pm-agent", "dev-agent", "sec-agent", "qa-agent"}

// DefaultDependencyManifests covers the common ecosystems. A changed path
// matching one of these is how a branch is judged to have altered the dependency
// tree.
var DefaultDependencyManifests = []string{
	"go.mod", "go.sum", "go.work", "go.work.sum",
	"package.json", "package-lock.json", "yarn.lock", "pnpm-lock.yaml",
	"requirements.txt", "poetry.lock", "Pipfile.lock", "pyproject.toml",
	"Cargo.toml", "Cargo.lock", "Gemfile", "Gemfile.lock",
	"composer.json", "composer.lock", "pom.xml", "build.gradle", "build.gradle.kts",
}

// ErrNoModelClasses means no serving endpoint is configured. Fatal for the
// department, ignorable for anything that only reads.
var ErrNoModelClasses = errors.New("no model classes configured")

// Repo is the repository the developer works.
type Repo struct {
	// URL is the clone URL. CREDENTIALS ARE NEVER HELD HERE — forge resolves
	// SecretRef at dispatch, so the agent never sees them.
	URL       string
	SecretRef string

	// Branch is the base to work from; IntegrationBranch is where reviewed work
	// lands.
	//
	// The developer needs the integration branch for the same reason the
	// integrator does: A TICKET THAT WAITED FOR ITS DEPENDENCY MUST ACTUALLY
	// RECEIVE IT. Branches are cut at scoping time, all from the same base, before
	// any sibling has merged; scheduling then holds a ticket until its blockers
	// finish — correctly — but nothing rebases, so the developer starts on a tree
	// that predates the code it was waiting for. Measured: an agent writing
	// handlers against a branch with no store, compiling to "undefined: Task", then
	// reading 96 times in a row looking for where Task was defined. It was not
	// there. The reading was rational and could never terminate.
	Branch            string
	IntegrationBranch string
	BranchPrefix      string

	// Image and RunnerClass select the sandbox. The image MUST be on forge's
	// allowlist or every execution is refused with a bare 400.
	Image       string
	RunnerClass string

	LeaseIdleSecs int64
	LeaseMaxSecs  int64

	// PipelineID is the platform pipeline that verifies a branch — the same one a
	// person's push runs. PREFER IT: a pipeline is a single definition of "does
	// this work", shared with every human contributor, and it cannot drift from
	// what CI does because it IS what CI does.
	PipelineID  string
	BranchInput string

	// FormatCommand and DepsCommand run at COMMIT time rather than in
	// verification, because what lands has to be the formatted, resolved state. A
	// formatter run afterwards reports on a branch that is already pushed, and a
	// manifest that names a module the lock file has never heard of fails the very
	// next clone.
	FormatCommand string
	DepsCommand   string

	// LintCommand is ADVISORY, NOT A GATE, and that asymmetry with the tests is
	// deliberate. A linter can produce findings this change cannot resolve — a
	// false positive, a rule needing a refactor beyond the ticket, two rules that
	// disagree — and behind a gate each of those traps the agent: fixed budget,
	// spent trying, correct change thrown away over a lint rule. An operator who
	// genuinely wants lint to block puts it in TestCommand and says so on purpose.
	LintCommand string

	// CriticalCommand is the one analysis that DOES gate. Severity is the line and
	// the operator draws it: a command exiting non-zero only for findings a change
	// must not carry. It gates because the alternative is worse for exactly the
	// findings that matter most — without it a critical finding waits for the
	// reviewer, who cannot fix anything, so the fix returns to a developer on a
	// LATER ticket having lost the context of the change.
	CriticalCommand string

	// ScanCommand and SCACommand are CONTEXT for the reviewer rather than gates,
	// and they are separate because the two produce different work: a finding in
	// your own code is a change to make, while one in a dependency is usually a
	// version bump and occasionally unreachable from any path this project takes.
	ScanCommand string
	SCACommand  string

	DependencyManifests []string

	// TestCommand verifies a branch WITHOUT the platform, in the sandbox the agent
	// already holds. A real trade taken deliberately: it is a second definition of
	// "does this work" and it can drift from CI. What it buys is that the
	// department runs STANDALONE.
	TestCommand string

	// CoverageCommand reports a number rather than an exit code, which is why it
	// is separate from TestCommand. CoverageTarget is A TARGET, NOT A GUARANTEE:
	// it is what makes the stage finish, and it is a proxy — 80% of statements
	// executed is not 80% of behaviour checked.
	CoverageCommand string
	CoverageTarget  int

	TimeoutSecs int64
}

// Config is everything this host was told.
type Config struct {
	Classes map[model.Class]model.ClassConfig

	// Host identifies this agent host in transcripts and attribution. Several
	// agent systems may attach to one platform, so "which box made this patch" is
	// not answerable from the agent account alone.
	Host string

	// TranscriptDir is where the corpus is appended. Capture is ON by default and
	// disabled only by the exact value "off" — OPT-OUT rather than opt-in, because
	// a transcript nobody enabled cannot be recovered after the fact.
	TranscriptDir string

	// Platform is how this host reaches the platform. An ordinary ACCOUNT
	// credential over the public API, not a service key.
	PlatformURL   string
	PlatformToken string

	// ForgeURL, when set, is a forge on THIS HOST reached directly — the intended
	// deployment, since it puts the sandbox on the same machine as the model.
	// ForgeToken is its own credential: the agent host is deliberately not part of
	// the platform's trust domain.
	ForgeURL   string
	ForgeToken string

	// ForgeLoginURL is where the plane's SESSIONS come from, which is not
	// necessarily where its executions go. Through conductor the two coincide;
	// against a local plane they do not, and forge serves no login route — so
	// exchanging a password at forge's own URL returns a bare 404 that reads like
	// a rejected credential rather than a missing route.
	ForgeLoginURL string
	ForgeEmail    string
	ForgePassword string

	// TicketsURL, when set, is a ticket store on the SANDBOX PLANE. This is the
	// standalone mode: the local store is authoritative, nothing is synced, and
	// the department needs no platform at all. Empty is the connected mode. Those
	// are the only two shapes — a third where both sides accept writes and are
	// reconciled would make status last-write-wins across two stores and cost the
	// claim protocol the single ordering authority it needs.
	TicketsURL string

	// WikiURL, when set, is the project wiki service the architect's wiki_page tool
	// writes to (via conductor/in-cluster DNS). Empty disables the tool.
	WikiURL string

	// DocsProject, when set, names a CENTRAL docs-wiki project that architecture
	// diagrams are mirrored into (in addition to the run's own project wiki), so
	// diagrams from every repo aggregate in one cross-repo docs wiki. Empty disables
	// the mirror. The wiki-bot identity must hold a write grant on this project.
	DocsProject string

	// GatekeeperURL and ServiceKey are how the ACTION SERVICE authenticates as a
	// CodeArmory service: GatekeeperURL is gatekeeper's base URL, ServiceKey is
	// blacksmith's east-west key (rotated), used to validate a caller's bearer and
	// to mint each agent run a temp identity scoped to that caller. Empty leaves
	// the service unauthenticated — fine for a local smoke test, not for the plane.
	GatekeeperURL string
	ServiceKey    string

	// DatabaseURL is the postgres DSN the action service reads its ROLES from —
	// the editable role definitions a workflow picks by name on the blacksmith/agent
	// action. Empty means no store: the service falls back to the roles compiled
	// into internal/agents, so a local smoke test needs no database.
	DatabaseURL string

	BoardID     string
	Concurrency int
	Poll        time.Duration

	AgentAuthors []string

	// The class each stage runs on. A stage assigned to a class this host does not
	// serve fails loudly at dispatch rather than silently falling back to a weaker
	// model, which would be an invisible quality regression.
	PMClass        model.Class
	DevClass       model.Class
	SecClass       model.Class
	TestClass      model.Class
	CoverageClass  model.Class
	ArchitectClass model.Class

	// TestClass should DIFFER from DevClass wherever the host has two models to
	// spare, and the reason is independence rather than cost: this stage's whole
	// job is to describe the ticket in a way that disagrees with a wrong
	// implementation, and a second reading drawn from the same weights tends to
	// interpret the ticket the same way and overlook the same cases.

	CoverageEnabled  bool
	ArchitectEnabled bool

	// MergeTasksFirst folds a whole plan into one task before it is specified,
	// and OneSpecAuthorPerTask has one agent write a task's whole specification
	// instead of one per slice.
	//
	// BOTH ARE MEASUREMENTS IN PROGRESS rather than settled design: each keeps
	// the other shape runnable on the same seed, so the answer is a number
	// instead of an argument. See internal/agent/scope.Options, which is where
	// what they mean is written down.
	MergeTasksFirst      bool
	OneSpecAuthorPerTask bool

	Repo              Repo
	IntegrationBranch string

	TestConcurrency int
	DevConcurrency  int

	// SpecMaxIterations and DevMaxIterations bound DIFFERENT failures. A developer
	// that cannot satisfy a specification spins — it edits, the verdict does not
	// move — which argues for a tight number. The specification stages do not
	// spin; they do a job whose size is the specification's, and cutting them
	// stops work that was going to finish. Measured when one number served both:
	// a reconciler repairing two test files was stopped at 43 turns mid-repair,
	// and the retry finished the job in 8.
	SpecMaxIterations int
	DevMaxIterations  int

	DevMemoryMB      int
	DevCPUMillicores int
}

// CaptureEnabled reports whether transcripts are being written.
func (c Config) CaptureEnabled() bool { return c.TranscriptDir != TranscriptOff }

// DevEnabled reports whether the developer can run here.
//
// A repository AND A WAY TO VERIFY are both required. The verification may be a
// platform pipeline or a sandbox test command, but one must exist: an agent that
// pushes unverified branches is worse than an agent that does nothing, because
// it produces work that LOOKS reviewed.
func (c Config) DevEnabled() bool {
	return c.Repo.URL != "" && (c.Repo.PipelineID != "" || c.Repo.TestCommand != "")
}

// VerifiesWithPipeline reports which of the two verification modes is in force,
// so startup can say so plainly rather than leaving it to be inferred from which
// variables happen to be set.
func (c Config) VerifiesWithPipeline() bool { return c.Repo.PipelineID != "" }

// Standalone reports whether this host takes its work from its own plane.
func (c Config) Standalone() bool { return c.TicketsURL != "" }

// SandboxLoginURL is where the plane's gatekeeper issues sessions, defaulting to
// the forge URL for the conductor deployment where one base URL serves both.
func (c Config) SandboxLoginURL() string {
	if c.ForgeLoginURL != "" {
		return c.ForgeLoginURL
	}
	return c.ForgeURL
}

// DispatchReady reports whether this host has what it needs to pull work.
//
// A host with inference but no platform credential is a VALID configuration — it
// can serve models and run a live smoke test — so this is a check, not a startup
// error.
func (c Config) DispatchReady() bool {
	if c.Standalone() {
		// Work comes from the plane, so it is the plane that must be reachable. No
		// platform credential is wanted here, and demanding one would make the
		// standalone mode impossible to enter.
		return c.ForgeURL != "" &&
			(c.ForgeToken != "" || (c.ForgeEmail != "" && c.ForgePassword != ""))
	}
	return c.PlatformURL != "" && c.PlatformToken != ""
}

// Configured lists the classes this host serves, in a stable order so startup
// logs are diffable.
func (c Config) Configured() []model.Class {
	out := make([]model.Class, 0, len(c.Classes))
	for class := range c.Classes {
		out = append(out, class)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Serves reports whether a class resolves on this host.
func (c Config) Serves(class model.Class) bool {
	_, ok := c.Classes[class]
	return ok
}

// ResolveTestClass picks the class the author runs on, preferring a model that
// is NOT the developer's.
//
// THE FALLBACK MATTERS MORE THAN THE PREFERENCE. The author is the first work
// stage, so a host where its configured class does not resolve would leave every
// ticket in its queue with nothing coming to collect it — a silently stalled
// department, the worst of the available failures. Falling back costs the
// independence this stage was built for and keeps the pipeline moving.
func (c Config) ResolveTestClass() model.Class {
	if c.Serves(c.TestClass) {
		return c.TestClass
	}
	return c.DevClass
}

// ResolveCoverageClass falls back to the author's rather than the developer's:
// it is the same kind of work, and an operator who has already chosen a class
// for writing tests has expressed the preference that matters.
func (c Config) ResolveCoverageClass() model.Class {
	if c.Serves(c.CoverageClass) {
		return c.CoverageClass
	}
	return c.ResolveTestClass()
}

// ResolveArchitectClass falls back to the product manager's: the two do the same
// kind of reading, and a host serving one class must still get a working stage
// rather than a silent one.
func (c Config) ResolveArchitectClass() model.Class {
	if c.Serves(c.ArchitectClass) {
		return c.ArchitectClass
	}
	return c.PMClass
}

// UnpinnedTools reports configured commands that fetch a tool at @latest.
//
// A sandbox command like `go run some/tool@latest` downloads and EXECUTES code
// chosen by whoever published it most recently, inside the sandbox, on every
// run. That is a supply-chain dependency acquired by accident: nobody reviewed
// it, nothing pins it, and it can change between two runs of the same ticket.
//
// REPORTED RATHER THAN REFUSED: a host mid-experiment should not be blocked from
// starting, and an operator who means it can leave it.
func (c Config) UnpinnedTools() []string {
	var out []string
	for name, cmd := range map[string]string{
		"AGENTS_REPO_FORMAT_COMMAND":   c.Repo.FormatCommand,
		"AGENTS_REPO_DEPS_COMMAND":     c.Repo.DepsCommand,
		"AGENTS_REPO_LINT_COMMAND":     c.Repo.LintCommand,
		"AGENTS_REPO_TEST_COMMAND":     c.Repo.TestCommand,
		"AGENTS_REPO_SCAN_COMMAND":     c.Repo.ScanCommand,
		"AGENTS_REPO_SCA_COMMAND":      c.Repo.SCACommand,
		"AGENTS_REPO_CRITICAL_COMMAND": c.Repo.CriticalCommand,
	} {
		if strings.Contains(cmd, "@latest") {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// Manifests is the dependency manifest list, falling back to the built-in set.
func (r Repo) Manifests() []string {
	if len(r.DependencyManifests) > 0 {
		return r.DependencyManifests
	}
	return DefaultDependencyManifests
}
