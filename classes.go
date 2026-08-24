package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Class is the model-class indirection: agents ask for a capability tier, never
// for a model or a host. This is the seam that keeps the inference layer
// replaceable — the stack behind a class has already changed twice (Ollama →
// vLLM → llama-server) and each change cost a config edit and nothing else.
type Class string

const (
	ClassTiny  Class = "tiny"
	ClassSmall Class = "small"
	ClassLarge Class = "large"
)

// Classes is the fixed set. It is deliberately short: a class is a capability
// tier an agent role can be assigned to, not a catalogue of every model on the
// box. Adding a fourth should require an argument about roles.
var Classes = []Class{ClassTiny, ClassSmall, ClassLarge}

// ClassConfig is one class's resolved routing entry.
type ClassConfig struct {
	// Endpoint is an OpenAI-compatible base URL, including the /v1 suffix.
	// Never a vendor-native API: everything the gateway talks to speaks /v1 so
	// a local llama-server and a frontier API are interchangeable here.
	Endpoint string
	// Model is the name passed through in the request body. llama-server
	// ignores it; vLLM and hosted APIs do not.
	Model string
	// APIKey is optional — llama-server's --api-key, or a hosted provider's key.
	APIKey string
	// Slots is the number of concurrent requests to keep in flight. Match it to
	// the serving backend's own slot count: undershooting wastes aggregate
	// throughput, overshooting measurably reduces it.
	Slots int
	// QueueDepth bounds the waiting list so a burst sheds load instead of
	// growing without limit.
	QueueDepth int
	// ToolsSupported says whether this backend's chat template can return a TOOL
	// CALL, as opposed to writing one into the content and carrying on.
	//
	// IT IS THE STOP CONDITION THAT MATTERS, not the shape of the reply. When the
	// template supports tools, the server ends generation as the call closes.
	// When it does not, the model is unconstrained: measured on qwen2.5-coder-32b,
	// every turn ran to the 8000-token ceiling, emitting write_files, then
	// run_tests, then finish, then more — an entire imagined conversation, of
	// which the loop reads only the first object. Seven thousand discarded tokens
	// and 334 seconds, per turn.
	//
	// Setting this false makes the gateway send the JSON SCHEMA instead, and a
	// grammar that permits exactly one object ends generation when that object
	// closes. Default true, because the models that do support tools are better
	// served by them.
	ToolsSupported bool
	// Temperature, when above zero, is the FLOOR this backend is sampled at.
	//
	// GREEDY DECODING IS NOT SAFE ON EVERY MODEL. Always taking the likeliest
	// token is right for a short structured action and catastrophic for a model
	// prone to repetition: measured on qwen2.5-coder-32b, a spec author produced
	// a schema-valid object whose "type" string ran
	// "..._or_is_not_a_test_file_with_the_correct_test_multiculturalism_or_is_not
	// _a_test_file_with_the_correct_test_multinationalism..." to the token
	// ceiling. A grammar bounds STRUCTURE; nothing bounds the length of a string
	// it permits. The architect hit the identical loop and 0.3 ended it.
	//
	// Zero means leave the caller's choice alone, so a well-behaved backend keeps
	// the determinism the agents were built around.
	Temperature float64
	// ReasoningEffort is passed straight through as OpenAI's reasoning_effort,
	// and "none" is how a thinking model is told to stop thinking.
	//
	// THINKING IS THE SINGLE LARGEST COST IN A RUN. Measured on r71: 91% of wall
	// clock was model generation, 6% sandbox, 0% queueing — so what the pipeline
	// spends is what it generates, and on a thinking model most of what it
	// generates is not the answer. On one representative judgement question,
	// qwen3.8 produced 1,175 characters of reasoning ahead of a 193-character
	// answer, 358 tokens in 7 seconds; with reasoning_effort "none" it produced
	// the same answer in 45 tokens and 1 second.
	//
	// Only this field works on ollama. reasoning_effort "low" still thought
	// (926 characters) and chat_template_kwargs enable_thinking=false was ignored
	// entirely, returning the byte-identical thinking reply.
	//
	// It is a per-class setting BECAUSE THE TRADE IS REAL. Reasoning is what
	// diagnosed the product manager's truncation and the unsatisfiable
	// doc-comment assertion; a stage whose failures are subtle may be worth its
	// tokens. Empty leaves the backend's default alone.
	ReasoningEffort string
}

// Config is the gateway's whole routing table.
type Config struct {
	Classes map[Class]ClassConfig
	// Host identifies this agent host in transcripts and ticket attribution.
	// Several agent systems may attach to one CodeArmory, so "which box made
	// this patch" is not answerable from the agent account alone.
	Host string
	// TranscriptDir is where the training corpus is appended. Capture is on by
	// default and disabled only by setting AGENTS_TRANSCRIPT_DIR to "off" —
	// opt-out rather than opt-in, because a transcript nobody enabled cannot be
	// recovered after the fact.
	TranscriptDir string

	// Platform is how this host reaches CodeArmory. blacksmith is a client, so
	// this is an ordinary account credential over the public API, not a service
	// key — the narrower credential, until scoped-role minting actually needs
	// otherwise.
	PlatformURL   string
	PlatformToken string
	// ForgeURL, when set, is a forge on THIS HOST reached directly rather than
	// through conductor — the intended deployment, since it is what puts the
	// sandbox on the same machine as the model and collapses the department's
	// egress boundary to a single outbound hole. Empty sends sandboxes to the
	// platform's forge, which works but is a stopgap.
	ForgeURL string
	// ForgeToken is the credential for the local sandbox plane, which runs its
	// OWN gatekeeper. Separate from PlatformToken on purpose — the agent host is
	// deliberately not part of the platform's trust domain.
	ForgeToken string
	// TicketsURL, when set, is a ticket store on the SANDBOX PLANE rather than the
	// platform's. This is the STANDALONE mode: the local store is authoritative,
	// nothing is synced, and the department needs no CodeArmory deployed at all.
	//
	// Leaving it empty is the CONNECTED mode that exists today — the platform is
	// authoritative and there is no local store. Those are the only two shapes;
	// what must never be built is a third where both sides accept writes to
	// mutable rows and are reconciled afterwards, because status and priority
	// would become last-write-wins across two stores and the claim protocol would
	// lose the single ordering authority it needs to stop one ticket being worked
	// twice.
	TicketsURL string
	// ForgeLoginURL is where the sandbox plane's SESSIONS come from — its
	// gatekeeper — which is not necessarily where its executions go.
	//
	// Through conductor the two coincide: one base URL routes /login to gatekeeper
	// and /forge to forge. Against a local plane they do not. forge serves no
	// /login, so exchanging a password at forge's own URL returns a bare 404 that
	// reads like a rejected credential rather than a missing route, and every
	// sandbox then fails to authenticate. Empty falls back to ForgeURL, which is
	// correct for the conductor deployment and only wrong for the direct one.
	ForgeLoginURL string
	// ForgeEmail/ForgePassword let blacksmith RENEW its sandbox-plane token
	// instead of holding a pinned one. The plane's gatekeeper issues short-lived
	// sessions, so a static token works until it silently does not.
	ForgeEmail    string
	ForgePassword string
	// BoardID scopes which tickets the department works. Empty means every board
	// the agent account can see, which is rarely what is wanted on a shared
	// instance.
	BoardID string
	// Concurrency is how many tickets this host works at once. It defaults to the
	// large class's slot count, since serving slots are the real constraint.
	Concurrency int
	// Poll is the level-trigger interval.
	Poll time.Duration
	// PMClass is the class the product-manager agent runs on. Triage is cheap and
	// a small model does it well.
	PMClass Class
	// AgentAuthors are the agent usernames whose tickets are excluded from
	// selection — the loop breaker, applied to the pull.
	AgentAuthors []string

	// DevClass is the class the developer agent runs on — the large one, since
	// this is the work that actually needs a capable model.
	DevClass Class
	// Repo is the repository the developer agent works. Empty URL means the
	// developer agent is not enabled on this host, which is a valid deployment:
	// a triage-only host needs no repository and no git credential.
	Repo RepoConfig
	// IntegrationBranch is where reviewed branches are merged. This repository is
	// the department's own workspace: agent branches land here continuously, and
	// promotion onwards is a separate decision.
	IntegrationBranch string
	// SecClass is the class the security reviewer runs on. Reading a diff for
	// vulnerabilities is not a small-model task.
	SecClass Class
	// TestClass is the class the test author runs on, and it should DIFFER from
	// DevClass wherever the host has two models to spare.
	//
	// The reason is independence, not cost. This stage's whole job is to describe
	// the ticket in a way that disagrees with a wrong implementation, and a second
	// reading drawn from the same weights is correlated with the first: it tends
	// to interpret the ticket the same way and to overlook the same cases, which
	// is most of the value gone. Different weights fail differently, and that is
	// what makes the developer's gate mean something.
	//
	// It falls back to DevClass when nothing else is configured. One model with a
	// separate stage that cannot edit the other half is still far better than one
	// agent writing both.
	TestClass Class
	// CoverageEnabled turns the coverage stage on. OFF by default.
	//
	// It works, and it is in the wrong place. Measured on a live build: the
	// developer pushed a branch in 90 seconds and the coverage stage then held the
	// ticket for over seven minutes, with five sibling tickets idle behind it
	// waiting only for the code to exist. Coverage cannot fail a ticket and its
	// output is additive, so gating a fan-out on it is pure latency.
	//
	// The fix is to run it AFTER the merge rather than before review, where its
	// duration stops mattering and a slower model on a second GPU becomes a good
	// home for it rather than a bottleneck. That is the same shape as the parked
	// janitor work — background, repo-shaped, off the critical path — so the two
	// belong together and this stays off until they land as one thing.
	CoverageEnabled bool
	// CoverageClass is the class the coverage stage runs on, separately from the
	// spec author's.
	//
	// Its own setting because the two stages carry very different consequences.
	// The spec author defines the interface the developer is held to, so a weak
	// one produces a bad implementation. The coverage author runs against
	// finished, reviewed code it may not modify, adding tests it may not use to
	// weaken the specification — so the worst it can do is add little, and since
	// falling short now carries the work on rather than blocking it, that costs
	// the ticket nothing.
	//
	// That asymmetry is what makes it reasonable to put this stage on a smaller
	// model, and on a second GPU it is otherwise the only claim on. What it is
	// NOT is an easier task: writing tests against real code means reading
	// branches and emitting code that compiles, and a model that cannot do that
	// will simply cover nothing. Falls back to TestClass.
	CoverageClass Class
	// ArchitectEnabled turns the design stage on. ON by default, because the
	// thing it removes is worse than the minute it costs: without it the product
	// manager emits documentation SUBTASKS, and a doc ticket cannot survive the
	// pipeline — the spec author that receives it may write only *_test.go while
	// the ticket asks for a README, so no permitted action ends the loop.
	//
	// Turning it off is still legitimate: it costs one model call and one push
	// per request, which is real overhead on a repository whose documentation is
	// already written and stable.
	ArchitectEnabled bool
	// ArchitectClass is the class the design stage runs on. Falls back to the
	// product manager's class, which is the closest job — read a request whole
	// and say what it means — and, like triage, it is one call rather than a
	// loop, so it is cheap to give it the best model on the host.
	ArchitectClass Class
	// TestConcurrency is how many test-authoring tickets run at once. Separate
	// from DevConcurrency because the stages are sized differently: authoring
	// tests is a smaller sandbox and a shorter loop than building the change.
	TestConcurrency int
	// SpecMaxIterations bounds the SPECIFICATION side: the author, the reconciler
	// that repairs a spec sent back, and the coverage stage.
	//
	// SEPARATE FROM THE DEVELOPER'S, because the two bound different failures. A
	// developer that cannot satisfy a specification spins — it edits, the verdict
	// does not move, and every verification zeroes the loop breaker, so only the
	// budget ends it. That argues for a tight number. The specification stages do
	// not spin; they do a job whose size is the specification's, and cutting them
	// stops work that was going to finish.
	//
	// Measured on r98, when one number served both after the developer's was cut
	// to 40: the reconciler was repairing two test files that would not compile,
	// spent 43 turns, and was stopped at the ceiling mid-repair. Its edits persist
	// on the branch, so the retry finished the job in 8 — the cut bought nothing
	// and cost a stage cycle of about three and a half minutes.
	SpecMaxIterations int
	// DevMaxIterations bounds one ticket's loop. A model that never converges
	// otherwise costs tokens indefinitely, and on a shared GPU that starves every
	// other agent.
	DevMaxIterations int

	// DevConcurrency is how many developer tickets run at once. Unlike the model
	// classes, the binding constraint here is the BOX rather than the serving
	// slots: a developer task holds a sandbox for minutes, so this host's peak
	// sandbox footprint is DevConcurrency × DevMemoryMB of RAM and DevConcurrency ×
	// DevCPUMillicores of CPU. Raise it only with those two and the node in view.
	DevConcurrency int
	// DevMemoryMB / DevCPUMillicores size the sandbox ONE developer task gets, via
	// the runner class named by Repo.RunnerClass.
	//
	// They are GBs and whole cores rather than the MBs a container tier would take,
	// because a real toolchain compiling inside a microVM pays for the guest's own
	// kernel and page cache on top of the workload. Measured on the sandbox plane:
	// forge's seeded 256 MB `standard` fails a `go test ./...` on a ONE-FILE repo
	// 3 runs in 5 — an OOM kill mid-compile, not a clean error — while 512 MB and
	// up passed every time. The default here is far above that floor on purpose:
	// the floor was measured against a trivial repo, and a real one compiles many
	// packages at once.
	DevMemoryMB      int
	DevCPUMillicores int
}

const (
	defaultSlots      = 4
	defaultQueueDepth = 32
	// defaultDevConcurrency is how many developer sandboxes may run at once.
	defaultDevConcurrency = 4
	// defaultTestConcurrency matches it: the test author is the stage that FEEDS
	// development, so a narrower one would simply move the queue one column left
	// and leave developer slots idle waiting for specs.
	defaultTestConcurrency = 4
	// defaultCoverageTarget is the percentage the coverage stage works toward.
	// Eighty is conventional rather than derived: high enough that the error paths
	// have to be exercised, low enough not to demand tests for generated code and
	// trivial accessors.
	defaultCoverageTarget = 80
	// defaultDevMemoryMB / defaultDevCPUMillicores size one developer sandbox:
	// 8 GB and 4 cores. At defaultDevConcurrency that is a 32 GB / 16-core peak,
	// which is what the sandbox plane's node is sized for.
	defaultDevMemoryMB      = 8192
	defaultDevCPUMillicores = 4000
	// defaultDevRunnerClass is the forge runner class the developer agent's
	// sandboxes run in. It is a DEDICATED class rather than forge's shared
	// `standard`: the sizing above is specific to agent work, and writing it onto
	// a shared tier would silently re-size every other tenant's executions too.
	defaultDevRunnerClass = "agent-dev"
	// defaultIntegrationBranch is where reviewed work lands. Not the base branch:
	// the base is what agents cut from and what stays known-good, and merging into
	// it directly would mean a broken integration takes the branch every subsequent
	// agent starts from.
	defaultIntegrationBranch = "dev"
	// defaultDevPidsLimit / defaultDevDiskGB round out the class. A parallel build
	// forks a compiler per core, so the pid ceiling is generous relative to the
	// core count rather than tight against it.
	defaultDevPidsLimit = 512
	defaultDevDiskGB    = 40
	// defaultLeaseIdleSecs / defaultLeaseMaxSecs bound a sandbox the agent fails
	// to release. Both are generous relative to a task, because they exist to
	// catch a crashed agent rather than to end a healthy one: a developer task
	// that hits either is a task something went wrong in, and cutting it short
	// while it is still working would turn a slow ticket into a failed one. The
	// idle bound is the tighter of the two, since a sandbox with no command in it
	// for ten minutes is one nobody is coming back to.
	defaultLeaseIdleSecs = 600
	defaultLeaseMaxSecs  = 3600
	// defaultTranscriptDir keeps the corpus beside the service's other state
	// rather than in /tmp, which is cleared on reboot.
	defaultTranscriptDir = "/var/lib/codearmory-agents/transcripts"
	// transcriptOff is the single value that disables capture.
	transcriptOff = "off"
)

// defaultAgentAuthors is the loop breaker's exclusion list: the agent accounts
// seeded by gatekeeper. A ticket one of them authored is never selected, or the
// product-manager agent's own output feeds straight back into the department.
var defaultAgentAuthors = []string{"pm-agent", "dev-agent", "sec-agent", "qa-agent"}

// CaptureEnabled reports whether transcripts are being written.
func (c Config) CaptureEnabled() bool { return c.TranscriptDir != transcriptOff }

// devRunnerClass is the forge runner class the developer agent's sandboxes run
// in, derived from the configured sizing.
//
// TmpfsMB is a QUARTER of the memory rather than a separate knob: /tmp in a
// sandbox is memory-backed and so is spent from the same budget, and it is where
// a toolchain puts its caches (GOCACHE, GOMODCACHE, the checkout). Sizing it
// independently of memory is how you get either a build that runs out of /tmp
// with gigabytes of RAM free, or a tmpfs that can eat the whole limit and take
// the compile with it.
func devRunnerClass(cfg Config) RunnerClassSpec {
	return RunnerClassSpec{
		Name:          cfg.Repo.RunnerClass,
		MemoryMB:      int64(cfg.DevMemoryMB),
		CPUMillicores: int64(cfg.DevCPUMillicores),
		TmpfsMB:       int64(cfg.DevMemoryMB / 4),
		PidsLimit:     defaultDevPidsLimit,
		DiskGB:        defaultDevDiskGB,
		Backend:       "default",
		Enabled:       true,
		// Not privileged. The developer agent clones, builds and tests; none of
		// that needs root, and forge only honours the flag on a kernel-isolated
		// backend anyway. Asking for it would widen what a prompt injection can
		// reach for no capability the agent actually uses.
		Privileged: false,
	}
}

// ErrNoModelClasses means no serving endpoint is configured. Fatal for the
// department, ignorable for anything that only reads.
var ErrNoModelClasses = errors.New("no model classes configured")

// operatorEnvFile is where the host's configuration lives. systemd reads it as
// the unit's EnvironmentFile; nothing exports it into an interactive shell, so a
// command run by hand saw none of it and failed on config that was plainly
// present. Loading it here is what makes `blacksmith tui` work from any prompt.
const operatorEnvFile = ".config/codearmory-agents/env"

// loadOperatorEnv reads KEY=VALUE lines into the environment, WITHOUT overriding
// anything already set: an explicit export on the command line is a deliberate
// override and must win over the file. Absent or unreadable is not an error —
// plenty of hosts configure the process some other way.
func loadOperatorEnv() {
	path := strings.TrimSpace(os.Getenv("AGENTS_ENV_FILE"))
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return
		}
		path = filepath.Join(home, operatorEnvFile)
	}
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" || os.Getenv(key) != "" {
			continue
		}
		// systemd's EnvironmentFile takes the value literally apart from optional
		// surrounding quotes, so this must too — a shell-style unquote would turn
		// a password containing $ into something else.
		value = strings.TrimSpace(value)
		if len(value) >= 2 && (value[0] == '"' && value[len(value)-1] == '"' ||
			value[0] == '\'' && value[len(value)-1] == '\'') {
			value = value[1 : len(value)-1]
		}
		os.Setenv(key, value)
	}
}

// LoadConfig reads the routing table from the environment. Each class takes
// AGENTS_<CLASS>_ENDPOINT / _MODEL / _API_KEY / _SLOTS / _QUEUE.
//
// A class with no endpoint is simply absent — that is a supported deployment,
// not an error. A host running one card serves one or two classes, and an agent
// role assigned to a missing class fails loudly at dispatch rather than silently
// falling back to a weaker model, which would be an invisible quality
// regression.
func LoadConfig() (Config, error) {
	loadOperatorEnv()
	cfg := Config{
		Classes:       map[Class]ClassConfig{},
		Host:          envOrDefault("AGENTS_HOST", defaultHostname()),
		TranscriptDir: envOrDefault("AGENTS_TRANSCRIPT_DIR", defaultTranscriptDir),
		PlatformURL:   strings.TrimSpace(os.Getenv("CODEARMORY_URL")),
		PlatformToken: secret("CODEARMORY_TOKEN"),
		BoardID:       strings.TrimSpace(os.Getenv("AGENTS_BOARD_ID")),
		ForgeURL:      strings.TrimSpace(os.Getenv("AGENTS_FORGE_URL")),
		ForgeLoginURL: strings.TrimSpace(os.Getenv("AGENTS_FORGE_GATEKEEPER_URL")),
		TicketsURL:    strings.TrimSpace(os.Getenv("AGENTS_TICKETS_URL")),
		ForgeToken:    secret("AGENTS_FORGE_TOKEN"),
		ForgeEmail:    strings.TrimSpace(os.Getenv("AGENTS_FORGE_EMAIL")),
		ForgePassword: secret("AGENTS_FORGE_PASSWORD"),
		PMClass:       Class(envOrDefault("AGENTS_PM_CLASS", string(ClassSmall))),
		DevClass:      Class(envOrDefault("AGENTS_DEV_CLASS", string(ClassLarge))),
		SecClass:      Class(envOrDefault("AGENTS_SEC_CLASS", string(ClassLarge))),
		// Defaults to the SMALL class so that out of the box the test author and
		// the developer are different models, which is the point of the stage. An
		// operator with only one class configured gets the fallback in
		// resolveTestClass instead of a stage that silently never runs.
		TestClass:       Class(envOrDefault("AGENTS_TEST_CLASS", string(ClassSmall))),
		CoverageClass:   Class(strings.TrimSpace(os.Getenv("AGENTS_COVERAGE_CLASS"))),
		CoverageEnabled: strings.EqualFold(strings.TrimSpace(os.Getenv("AGENTS_COVERAGE_ENABLED")), "true"),
		ArchitectClass:  Class(strings.TrimSpace(os.Getenv("AGENTS_ARCHITECT_CLASS"))),
		// Defaults ON: absent means enabled, and only an explicit "false" turns it
		// off. The opposite default would quietly restore doc subtasks, which is
		// the failure this stage exists to remove.
		ArchitectEnabled:  !strings.EqualFold(strings.TrimSpace(os.Getenv("AGENTS_ARCHITECT_ENABLED")), "false"),
		IntegrationBranch: envOrDefault("AGENTS_REPO_INTEGRATION_BRANCH", defaultIntegrationBranch),
		AgentAuthors:      defaultAgentAuthors,
		Repo: RepoConfig{
			URL:           strings.TrimSpace(os.Getenv("AGENTS_REPO_URL")),
			SecretRef:     strings.TrimSpace(os.Getenv("AGENTS_REPO_SECRET_REF")),
			Branch:        envOrDefault("AGENTS_REPO_BRANCH", "main"),
			BranchPrefix:  envOrDefault("AGENTS_REPO_BRANCH_PREFIX", "agent/"),
			Image:         envOrDefault("AGENTS_REPO_IMAGE", "golang:1.25"),
			RunnerClass:   envOrDefault("AGENTS_REPO_RUNNER_CLASS", defaultDevRunnerClass),
			PipelineID:    strings.TrimSpace(os.Getenv("AGENTS_REPO_PIPELINE_ID")),
			TestCommand:   strings.TrimSpace(os.Getenv("AGENTS_REPO_TEST_COMMAND")),
			FormatCommand: envOrDefault("AGENTS_REPO_FORMAT_COMMAND", defaultFormatCommand),
			// Defaulted rather than left empty: an agent that cannot resolve a
			// dependency cannot add one, and it fails in a way that looks like a
			// broken model rather than a missing tool. A repository that is not Go
			// overrides it; one that wants no resolution sets it to "true".
			DepsCommand:         envOrDefault("AGENTS_REPO_DEPS_COMMAND", defaultDepsCommand),
			LintCommand:         strings.TrimSpace(os.Getenv("AGENTS_REPO_LINT_COMMAND")),
			ScanCommand:         strings.TrimSpace(os.Getenv("AGENTS_REPO_SCAN_COMMAND")),
			CriticalCommand:     strings.TrimSpace(os.Getenv("AGENTS_REPO_CRITICAL_COMMAND")),
			SCACommand:          strings.TrimSpace(os.Getenv("AGENTS_REPO_SCA_COMMAND")),
			DependencyManifests: splitList(os.Getenv("AGENTS_REPO_DEPENDENCY_MANIFESTS")),
			CoverageCommand:     envOrDefault("AGENTS_REPO_COVERAGE_COMMAND", defaultCoverageCommand),
			BranchInput:         envOrDefault("AGENTS_REPO_BRANCH_INPUT", "branch"),
			LeaseIdleSecs:       defaultLeaseIdleSecs,
			LeaseMaxSecs:        defaultLeaseMaxSecs,
		},
	}

	concurrency, err := intEnv("AGENTS_CONCURRENCY", 0)
	if err != nil {
		return Config{}, err
	}
	cfg.Concurrency = concurrency

	pollSecs, err := intEnv("AGENTS_POLL_SECONDS", int(defaultPollInterval/time.Second))
	if err != nil {
		return Config{}, err
	}
	cfg.Poll = time.Duration(pollSecs) * time.Second

	iterations, err := intEnv("AGENTS_DEV_MAX_ITERATIONS", defaultMaxIterations)
	if err != nil {
		return Config{}, err
	}
	cfg.DevMaxIterations = iterations

	// Defaults to the old shared value rather than to the developer's, so a host
	// that tightens the developer does not silently tighten the author with it.
	specIterations, err := intEnv("AGENTS_SPEC_MAX_ITERATIONS", defaultMaxIterations)
	if err != nil {
		return Config{}, err
	}
	cfg.SpecMaxIterations = specIterations

	coverage, err := intEnv("AGENTS_REPO_COVERAGE_TARGET", defaultCoverageTarget)
	if err != nil {
		return Config{}, err
	}
	cfg.Repo.CoverageTarget = coverage

	devConcurrency, err := intEnv("AGENTS_DEV_CONCURRENCY", defaultDevConcurrency)
	if err != nil {
		return Config{}, err
	}
	cfg.DevConcurrency = devConcurrency

	testConcurrency, err := intEnv("AGENTS_TEST_CONCURRENCY", defaultTestConcurrency)
	if err != nil {
		return Config{}, err
	}
	cfg.TestConcurrency = testConcurrency

	devMemory, err := intEnv("AGENTS_DEV_MEMORY_MB", defaultDevMemoryMB)
	if err != nil {
		return Config{}, err
	}
	cfg.DevMemoryMB = devMemory

	devCPU, err := intEnv("AGENTS_DEV_CPU_MILLICORES", defaultDevCPUMillicores)
	if err != nil {
		return Config{}, err
	}
	cfg.DevCPUMillicores = devCPU

	timeout, err := intEnv("AGENTS_REPO_TIMEOUT_SECONDS", 900)
	if err != nil {
		return Config{}, err
	}
	cfg.Repo.TimeoutSecs = int64(timeout)

	for _, c := range Classes {
		prefix := "AGENTS_" + strings.ToUpper(string(c))
		endpoint := strings.TrimSpace(os.Getenv(prefix + "_ENDPOINT"))
		if endpoint == "" {
			continue
		}
		normalised, err := normaliseEndpoint(endpoint)
		if err != nil {
			return Config{}, fmt.Errorf("%s_ENDPOINT: %w", prefix, err)
		}

		slots, err := intEnv(prefix+"_SLOTS", defaultSlots)
		if err != nil {
			return Config{}, err
		}
		depth, err := intEnv(prefix+"_QUEUE", defaultQueueDepth)
		if err != nil {
			return Config{}, err
		}

		cfg.Classes[c] = ClassConfig{
			Endpoint:   normalised,
			Model:      strings.TrimSpace(os.Getenv(prefix + "_MODEL")),
			APIKey:     secret(prefix + "_API_KEY"),
			Slots:      slots,
			QueueDepth: depth,
			// Absent means supported: only an explicit "false" turns it off, so a
			// host that has never heard of this keeps the behaviour it had.
			ToolsSupported:  !strings.EqualFold(strings.TrimSpace(os.Getenv(prefix+"_TOOLS")), "false"),
			Temperature:     floatEnv(prefix + "_TEMPERATURE"),
			ReasoningEffort: strings.TrimSpace(os.Getenv(prefix + "_REASONING_EFFORT")),
		}
	}

	if len(cfg.Classes) == 0 {
		// Returned WITH the config, not instead of it. Running the department
		// without a model is a startup failure, but reading its work is not: the
		// window onto it calls no model at all, and refusing to open because none
		// is configured would make the tool useless on exactly the machine where
		// you want to see why nothing is being served.
		return cfg, fmt.Errorf("%w: set at least one of %s", ErrNoModelClasses, strings.Join(endpointVarNames(), ", "))
	}

	// Default concurrency to the PM class's slot count. Serving slots are the
	// real constraint, so picking any other number either starves the card or
	// oversubscribes it — both measurably worse than matching.
	if cfg.Concurrency == 0 {
		if cc, ok := cfg.Classes[cfg.PMClass]; ok {
			cfg.Concurrency = cc.Slots
		} else {
			cfg.Concurrency = defaultSlots
		}
	}
	return cfg, nil
}

// unpinnedTools reports configured commands that fetch a tool at @latest.
//
// A sandbox command like `go run some/tool@latest` downloads and EXECUTES code
// chosen by whoever published it most recently, inside the sandbox, on every run.
// That is a supply-chain dependency acquired by accident: nobody reviewed it,
// nothing pins it, and it can change between two runs of the same ticket. The
// microVM bounds what it can reach, but the right answer is to name a version.
//
// Reported rather than refused: a host mid-experiment should not be blocked from
// starting, and an operator who means it can leave it.
func (c Config) unpinnedTools() []string {
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

// DevEnabled reports whether the developer agent can run here. A host with no
// repository configured is a triage-only host, which is a valid deployment
// rather than a misconfiguration.
//
// A repository AND a way to verify are both required. The verification may be a
// platform pipeline (preferred) or a sandbox test command (standalone), but one
// of them must exist: an agent that pushes unverified branches is worse than an
// agent that does nothing, because it produces work that LOOKS reviewed.
func (c Config) DevEnabled() bool {
	return c.Repo.URL != "" && (c.Repo.PipelineID != "" || c.Repo.TestCommand != "")
}

// SandboxLoginURL is where the sandbox plane's gatekeeper issues sessions,
// defaulting to the forge URL for the conductor deployment where one base URL
// serves both.
func (c Config) SandboxLoginURL() string {
	if c.ForgeLoginURL != "" {
		return c.ForgeLoginURL
	}
	return c.ForgeURL
}

// VerifiesWithPipeline reports which of the two verification modes is in force,
// so startup can say so plainly rather than leaving it to be inferred from which
// variables happen to be set.
func (c Config) VerifiesWithPipeline() bool { return c.Repo.PipelineID != "" }

// DispatchReady reports whether this host has everything it needs to pull work.
// A host with inference but no platform credential is a valid configuration —
// it can serve models and run the live smoke test — so this is a check, not a
// startup error.
func (c Config) DispatchReady() bool {
	if c.Standalone() {
		// Work comes from the plane, so it is the plane that must be reachable.
		// No platform credential is wanted here, and demanding one would make the
		// standalone mode impossible to enter.
		return c.ForgeURL != "" &&
			(c.ForgeToken != "" || (c.ForgeEmail != "" && c.ForgePassword != ""))
	}
	return c.PlatformURL != "" && c.PlatformToken != ""
}

// Standalone reports whether this host takes its work from its own plane rather
// than from a platform. See Config.TicketsURL for why the two are exclusive.
func (c Config) Standalone() bool { return c.TicketsURL != "" }

// Configured lists the classes this host serves, in a stable order so startup
// logs are diffable.
func (c Config) Configured() []Class {
	out := make([]Class, 0, len(c.Classes))
	for class := range c.Classes {
		out = append(out, class)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// normaliseEndpoint rejects the two mistakes that produce a confusing 404 at the
// first request rather than a clear error at startup: a missing scheme, and a
// path that stops short of /v1.
func normaliseEndpoint(raw string) (string, error) {
	if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
		return "", fmt.Errorf("must start with http:// or https:// (got %q)", raw)
	}
	trimmed := strings.TrimRight(raw, "/")
	if !strings.HasSuffix(trimmed, "/v1") {
		trimmed += "/v1"
	}
	return trimmed, nil
}

func intEnv(name string, def int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not a number", name, raw)
	}
	if n < 1 {
		return 0, fmt.Errorf("%s: must be at least 1 (got %d)", name, n)
	}
	return n, nil
}

func endpointVarNames() []string {
	names := make([]string, 0, len(Classes))
	for _, c := range Classes {
		names = append(names, "AGENTS_"+strings.ToUpper(string(c))+"_ENDPOINT")
	}
	return names
}

func defaultHostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "unknown"
	}
	return h
}

// splitList parses a comma-separated setting, dropping blanks so a trailing comma
// does not become an empty entry that matches nothing.
func splitList(raw string) []string {
	var out []string
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// resolveTestClass picks the class the test author runs on, preferring a model
// that is NOT the developer's.
//
// The fallback matters more than the preference. The test author is the first
// work stage, so a host where its configured class does not resolve would leave
// every ticket in ready_for_tests with nothing coming to collect it — a silently
// stalled department, which is the worst of the available failures. Falling back
// to the developer's class costs the independence this stage was built for and
// keeps the pipeline moving; the log says which one happened.
// resolveCoverageClass picks the class the coverage stage runs on.
//
// Falls back to the test author's rather than the developer's: it is the same
// kind of work, and an operator who has already chosen a class for writing tests
// has expressed the preference that matters. Setting it explicitly is how the
// stage gets moved onto a second GPU.
func resolveCoverageClass(cfg Config) Class {
	if _, ok := cfg.Classes[cfg.CoverageClass]; ok {
		return cfg.CoverageClass
	}
	return resolveTestClass(cfg)
}

// resolveArchitectClass picks the class the design stage runs on, falling back
// to the product manager's: the two do the same kind of reading, and a host that
// serves only one class must still get a working stage rather than a silent one.
func resolveArchitectClass(cfg Config) Class {
	if _, ok := cfg.Classes[cfg.ArchitectClass]; ok {
		return cfg.ArchitectClass
	}
	return cfg.PMClass
}

func resolveTestClass(cfg Config) Class {
	if _, ok := cfg.Classes[cfg.TestClass]; ok {
		return cfg.TestClass
	}
	return cfg.DevClass
}

// floatEnv reads an optional decimal setting, treating anything unparseable as
// absent: a malformed sampling knob should leave the default in place rather
// than fail the host's startup.
func floatEnv(name string) float64 {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return 0
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || v < 0 {
		return 0
	}
	return v
}
