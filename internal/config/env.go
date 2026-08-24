package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/code-armory-app/blacksmith/internal/model"
)

// OperatorEnvFile is where the host's configuration lives.
//
// systemd reads it as the unit's EnvironmentFile; nothing exports it into an
// interactive shell, so a command run by hand saw none of it and failed on
// configuration that was plainly present. Loading it here is what makes the
// tooling work from any prompt.
const OperatorEnvFile = ".config/codearmory-agents/env"

// env reads settings and REMEMBERS THE FIRST FAILURE instead of returning one
// per call.
//
// The shape it replaces was ten repetitions of "read, check err, assign", which
// is thirty lines in which the interesting part is the variable name. One
// forgotten check there is a malformed setting silently becoming a default —
// and the failure this whole file guards against is precisely a value that looks
// set and is not.
type env struct {
	err error
}

// fail records the first error and keeps the rest, so the message names the
// setting the operator has to fix rather than whichever one happened to be read
// last.
func (e *env) fail(err error) {
	if e.err == nil {
		e.err = err
	}
}

// str reads a setting, trimmed, falling back to def when unset.
func (e *env) str(name, def string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return def
}

// secret reads NAME, preferring ${NAME}_FILE so a mounted secret file works
// without changing the deployment.
//
// A NAMED FILE THAT CANNOT BE READ IS AN ERROR, not an empty string: the
// operator asked for that file, and starting without the credential produces a
// denial at the first request, several layers away from the cause.
func (e *env) secret(name string) string {
	if path := strings.TrimSpace(os.Getenv(name + "_FILE")); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			e.fail(fmt.Errorf("%s_FILE: %w", name, err))
			return ""
		}
		return strings.TrimRight(string(data), "\n")
	}
	return os.Getenv(name)
}

// num reads a positive integer setting.
//
// IT REFUSES ANYTHING THAT IS NOT ONE, which is what catches this deployment's
// sharpest edge: systemd does not strip inline comments, so `FOO=2 # note` sets
// the literal value `2 # note`. Defaulting on a parse failure would take the
// number the operator wrote and quietly use another.
func (e *env) num(name string, def int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		e.fail(fmt.Errorf("%s: %q is not a number "+
			"(note that an EnvironmentFile keeps inline comments as part of the value)", name, raw))
		return def
	}
	if n < 1 {
		e.fail(fmt.Errorf("%s: must be at least 1 (got %d)", name, n))
		return def
	}
	return n
}

// decimal reads an optional sampling knob, treating anything unparseable as
// ABSENT rather than as an error: a malformed temperature should leave the
// backend's default in place, not stop the host from starting.
func (e *env) decimal(name string) float64 {
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

// on reports a boolean setting that is OFF unless explicitly true.
func (e *env) on(name string) bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv(name)), "true")
}

// onUnlessOff reports a boolean setting that is ON unless explicitly false, so a
// host that has never heard of it keeps the behaviour it had.
func (e *env) onUnlessOff(name string) bool {
	return !strings.EqualFold(strings.TrimSpace(os.Getenv(name)), "false")
}

// list parses a comma-separated setting, dropping blanks so a trailing comma
// does not become an empty entry that matches nothing.
func (e *env) list(name string) []string {
	var out []string
	for _, p := range strings.Split(os.Getenv(name), ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Load reads the whole configuration from the environment.
//
// A CLASS WITH NO ENDPOINT IS SIMPLY ABSENT — a supported deployment, not an
// error. A host running one card serves one or two classes, and a role assigned
// to a missing class fails loudly at dispatch rather than silently falling back
// to a weaker model.
func Load() (Config, error) {
	LoadOperatorEnv()

	e := &env{}
	cfg := Config{
		Classes:       map[model.Class]model.ClassConfig{},
		Host:          e.str("AGENTS_HOST", hostname()),
		TranscriptDir: e.str("AGENTS_TRANSCRIPT_DIR", DefaultTranscriptDir),

		PlatformURL:   e.str("CODEARMORY_URL", ""),
		PlatformToken: e.secret("CODEARMORY_TOKEN"),

		ForgeURL:      e.str("AGENTS_FORGE_URL", ""),
		ForgeLoginURL: e.str("AGENTS_FORGE_GATEKEEPER_URL", ""),
		ForgeToken:    e.secret("AGENTS_FORGE_TOKEN"),
		ForgeEmail:    e.str("AGENTS_FORGE_EMAIL", ""),
		ForgePassword: e.secret("AGENTS_FORGE_PASSWORD"),

		TicketsURL: e.str("AGENTS_TICKETS_URL", ""),
		BoardID:    e.str("AGENTS_BOARD_ID", ""),

		AgentAuthors: DefaultAgentAuthors,

		PMClass:  model.Class(e.str("AGENTS_PM_CLASS", string(model.ClassSmall))),
		DevClass: model.Class(e.str("AGENTS_DEV_CLASS", string(model.ClassLarge))),
		SecClass: model.Class(e.str("AGENTS_SEC_CLASS", string(model.ClassLarge))),
		// Defaults to the small class so that out of the box the author and the
		// developer are DIFFERENT MODELS, which is the point of the stage. A host
		// with only one class configured gets the fallback rather than a stage that
		// silently never runs.
		TestClass:      model.Class(e.str("AGENTS_TEST_CLASS", string(model.ClassSmall))),
		CoverageClass:  model.Class(e.str("AGENTS_COVERAGE_CLASS", "")),
		ArchitectClass: model.Class(e.str("AGENTS_ARCHITECT_CLASS", "")),

		// Coverage is OFF by default: it works, and it is in the wrong place.
		// Measured on a live build — the developer pushed in 90 seconds and the
		// coverage stage then held the ticket for over seven minutes with five
		// siblings idle behind it. It cannot fail a ticket and its output is
		// additive, so gating a fan-out on it is pure latency.
		CoverageEnabled: e.on("AGENTS_COVERAGE_ENABLED"),

		// The architect is ON by default, because what it removes is worse than the
		// minute it costs: without it the product manager emits documentation
		// subtasks, and a doc ticket cannot survive the pipeline — the author that
		// receives it may write only test files while the ticket asks for a README,
		// so no permitted action ends the loop.
		ArchitectEnabled: e.onUnlessOff("AGENTS_ARCHITECT_ENABLED"),

		// Both OFF by default, which is the shape every run so far has used.
		MergeTasksFirst:      e.on("AGENTS_PM_ONE_TASK"),
		OneSpecAuthorPerTask: e.on("AGENTS_SPEC_ONE_AUTHOR"),

		IntegrationBranch: e.str("AGENTS_REPO_INTEGRATION_BRANCH", DefaultIntegrationBranch),
	}

	cfg.Concurrency = e.num("AGENTS_CONCURRENCY", 0)
	cfg.Poll = time.Duration(e.num("AGENTS_POLL_SECONDS", int(DefaultPoll/time.Second))) * time.Second
	cfg.DevMaxIterations = e.num("AGENTS_DEV_MAX_ITERATIONS", DefaultMaxIterations)
	// Defaults to the shared value rather than to the developer's, so a host that
	// tightens the developer does not silently tighten the author with it.
	cfg.SpecMaxIterations = e.num("AGENTS_SPEC_MAX_ITERATIONS", DefaultMaxIterations)
	cfg.DevConcurrency = e.num("AGENTS_DEV_CONCURRENCY", DefaultDevConcurrency)
	cfg.TestConcurrency = e.num("AGENTS_TEST_CONCURRENCY", DefaultTestConcurrency)
	cfg.DevMemoryMB = e.num("AGENTS_DEV_MEMORY_MB", DefaultDevMemoryMB)
	cfg.DevCPUMillicores = e.num("AGENTS_DEV_CPU_MILLICORES", DefaultDevCPUMillicores)

	cfg.Repo = Repo{
		URL:               e.str("AGENTS_REPO_URL", ""),
		SecretRef:         e.str("AGENTS_REPO_SECRET_REF", ""),
		Branch:            e.str("AGENTS_REPO_BRANCH", "main"),
		IntegrationBranch: cfg.IntegrationBranch,
		BranchPrefix:      e.str("AGENTS_REPO_BRANCH_PREFIX", "agent/"),
		Image:             e.str("AGENTS_REPO_IMAGE", "golang:1.25"),
		RunnerClass:       e.str("AGENTS_REPO_RUNNER_CLASS", DefaultDevRunnerClass),
		PipelineID:        e.str("AGENTS_REPO_PIPELINE_ID", ""),
		BranchInput:       e.str("AGENTS_REPO_BRANCH_INPUT", "branch"),
		TestCommand:       e.str("AGENTS_REPO_TEST_COMMAND", ""),
		FormatCommand:     e.str("AGENTS_REPO_FORMAT_COMMAND", DefaultFormatCommand),
		// Defaulted rather than left empty: an agent that cannot resolve a
		// dependency cannot ADD one, and it fails in a way that looks like a broken
		// model rather than a missing tool. A repository that is not Go overrides
		// it; one that wants no resolution sets it to "true".
		DepsCommand:         e.str("AGENTS_REPO_DEPS_COMMAND", DefaultDepsCommand),
		LintCommand:         e.str("AGENTS_REPO_LINT_COMMAND", ""),
		ScanCommand:         e.str("AGENTS_REPO_SCAN_COMMAND", ""),
		CriticalCommand:     e.str("AGENTS_REPO_CRITICAL_COMMAND", ""),
		SCACommand:          e.str("AGENTS_REPO_SCA_COMMAND", ""),
		DependencyManifests: e.list("AGENTS_REPO_DEPENDENCY_MANIFESTS"),
		CoverageCommand:     e.str("AGENTS_REPO_COVERAGE_COMMAND", DefaultCoverageCommand),
		CoverageTarget:      e.num("AGENTS_REPO_COVERAGE_TARGET", DefaultCoverageTarget),
		TimeoutSecs:         int64(e.num("AGENTS_REPO_TIMEOUT_SECONDS", DefaultTimeoutSecs)),
		LeaseIdleSecs:       DefaultLeaseIdleSecs,
		LeaseMaxSecs:        DefaultLeaseMaxSecs,
	}

	for _, class := range model.Classes {
		prefix := "AGENTS_" + strings.ToUpper(string(class))
		endpoint := strings.TrimSpace(os.Getenv(prefix + "_ENDPOINT"))
		if endpoint == "" {
			continue
		}
		normalised, err := NormaliseEndpoint(endpoint)
		if err != nil {
			e.fail(fmt.Errorf("%s_ENDPOINT: %w", prefix, err))
			continue
		}
		cfg.Classes[class] = model.ClassConfig{
			Endpoint:   normalised,
			Model:      e.str(prefix+"_MODEL", ""),
			APIKey:     e.secret(prefix + "_API_KEY"),
			Slots:      e.num(prefix+"_SLOTS", DefaultSlots),
			QueueDepth: e.num(prefix+"_QUEUE", DefaultQueueDepth),
			// Absent means supported: only an explicit "false" turns it off.
			ToolsSupported:  e.onUnlessOff(prefix + "_TOOLS"),
			Temperature:     e.decimal(prefix + "_TEMPERATURE"),
			ReasoningEffort: e.str(prefix+"_REASONING_EFFORT", ""),
		}
	}

	if e.err != nil {
		return Config{}, e.err
	}

	if len(cfg.Classes) == 0 {
		// RETURNED WITH THE CONFIG, not instead of it. Running the department
		// without a model is a startup failure, but READING its work is not: the
		// window calls no model at all, and refusing to open because none is
		// configured would make the tool useless on exactly the machine where you
		// want to see why nothing is being served.
		return cfg, fmt.Errorf("%w: set at least one of %s",
			ErrNoModelClasses, strings.Join(EndpointVarNames(), ", "))
	}

	// Default concurrency to the intake class's slot count. SERVING SLOTS ARE THE
	// REAL CONSTRAINT, so any other number either starves the card or
	// oversubscribes it — both measurably worse than matching.
	if cfg.Concurrency == 0 {
		if cc, ok := cfg.Classes[cfg.PMClass]; ok {
			cfg.Concurrency = cc.Slots
		} else {
			cfg.Concurrency = DefaultSlots
		}
	}
	return cfg, nil
}

// LoadOperatorEnv reads KEY=VALUE lines into the environment WITHOUT overriding
// anything already set: an explicit export on the command line is a deliberate
// override and must win over the file.
//
// Absent or unreadable is NOT an error — plenty of hosts configure the process
// some other way.
func LoadOperatorEnv() {
	path := strings.TrimSpace(os.Getenv("AGENTS_ENV_FILE"))
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return
		}
		path = filepath.Join(home, OperatorEnvFile)
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
		// systemd takes the value LITERALLY apart from optional surrounding quotes,
		// so this must too — a shell-style unquote would turn a password containing
		// a dollar sign into something else.
		value = strings.TrimSpace(value)
		if len(value) >= 2 &&
			(value[0] == '"' && value[len(value)-1] == '"' ||
				value[0] == '\'' && value[len(value)-1] == '\'') {
			value = value[1 : len(value)-1]
		}
		os.Setenv(key, value)
	}
}

// NormaliseEndpoint rejects the two mistakes that produce a confusing 404 at the
// first request rather than a clear error at startup: a missing scheme, and a
// path that stops short of the API version.
func NormaliseEndpoint(raw string) (string, error) {
	if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
		return "", fmt.Errorf("must start with http:// or https:// (got %q)", raw)
	}
	trimmed := strings.TrimRight(raw, "/")
	if !strings.HasSuffix(trimmed, "/v1") {
		trimmed += "/v1"
	}
	return trimmed, nil
}

// EndpointVarNames lists the settings that would configure a class, for the
// error that says none is set.
func EndpointVarNames() []string {
	names := make([]string, 0, len(model.Classes))
	for _, c := range model.Classes {
		names = append(names, "AGENTS_"+strings.ToUpper(string(c))+"_ENDPOINT")
	}
	return names
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "unknown"
	}
	return h
}
