package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// clearClassEnv removes every routing variable so a test starts from a known
// state regardless of what the developer has exported.
func clearClassEnv(t *testing.T) {
	t.Helper()
	for _, c := range Classes {
		prefix := "AGENTS_" + strings.ToUpper(string(c))
		for _, suffix := range []string{"_ENDPOINT", "_MODEL", "_API_KEY", "_API_KEY_FILE", "_SLOTS", "_QUEUE"} {
			t.Setenv(prefix+suffix, "")
			os.Unsetenv(prefix + suffix)
		}
	}
	for _, v := range []string{"CODEARMORY_URL", "CODEARMORY_TOKEN", "CODEARMORY_TOKEN_FILE",
		"AGENTS_BOARD_ID", "AGENTS_CONCURRENCY", "AGENTS_POLL_SECONDS", "AGENTS_PM_CLASS",
		"AGENTS_DEV_CONCURRENCY", "AGENTS_DEV_MEMORY_MB", "AGENTS_DEV_CPU_MILLICORES",
		"AGENTS_REPO_RUNNER_CLASS", "AGENTS_REPO_URL", "AGENTS_REPO_TEST_COMMAND",
		"AGENTS_REPO_PIPELINE_ID", "AGENTS_FORGE_URL", "AGENTS_FORGE_GATEKEEPER_URL", "AGENTS_TICKETS_URL"} {
		t.Setenv(v, "")
		os.Unsetenv(v)
	}
	// Point the operator env file at nothing. LoadConfig reads it so a command run
	// by hand sees the host's real configuration; a test that read the developer's
	// own file would pass or fail according to what is on their machine.
	t.Setenv("AGENTS_ENV_FILE", filepath.Join(t.TempDir(), "absent"))
	t.Setenv("AGENTS_HOST", "test-host")
}

// A host with inference but no platform credential is a valid configuration —
// it can still serve models — so it must load rather than fail, and simply not
// dispatch.
func TestDispatchReadyRequiresBothURLAndToken(t *testing.T) {
	clearClassEnv(t)
	t.Setenv("AGENTS_SMALL_ENDPOINT", "http://127.0.0.1:8081")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() = %v, want a serving-only host to be valid", err)
	}
	if cfg.DispatchReady() {
		t.Error("DispatchReady() = true with no platform credentials")
	}

	t.Setenv("CODEARMORY_URL", "https://example/api")
	cfg, _ = LoadConfig()
	if cfg.DispatchReady() {
		t.Error("DispatchReady() = true with a URL but no token")
	}

	t.Setenv("CODEARMORY_TOKEN", "tok")
	cfg, _ = LoadConfig()
	if !cfg.DispatchReady() {
		t.Error("DispatchReady() = false with both URL and token set")
	}
}

// Concurrency defaults to the PM class's slot count: serving slots are the real
// constraint, and any other number starves or oversubscribes the card.
func TestConcurrencyDefaultsToPMClassSlots(t *testing.T) {
	clearClassEnv(t)
	t.Setenv("AGENTS_SMALL_ENDPOINT", "http://127.0.0.1:8081")
	t.Setenv("AGENTS_SMALL_SLOTS", "6")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() = %v", err)
	}
	if cfg.Concurrency != 6 {
		t.Errorf("Concurrency = %d, want 6 (the small class's slots)", cfg.Concurrency)
	}
	if cfg.PMClass != ClassSmall {
		t.Errorf("PMClass = %q, want small by default", cfg.PMClass)
	}
	if cfg.Poll != defaultPollInterval {
		t.Errorf("Poll = %v, want %v", cfg.Poll, defaultPollInterval)
	}
}

// The loop breaker's exclusion list must be populated by default, or the PM
// agent's own tickets feed straight back into the department.
func TestAgentAuthorsDefaultPopulated(t *testing.T) {
	clearClassEnv(t)
	t.Setenv("AGENTS_SMALL_ENDPOINT", "http://127.0.0.1:8081")
	cfg, _ := LoadConfig()
	if len(cfg.AgentAuthors) == 0 {
		t.Fatal("AgentAuthors is empty; the department would feed itself")
	}
	for _, want := range []string{"pm-agent", "dev-agent"} {
		if !slices.Contains(cfg.AgentAuthors, want) {
			t.Errorf("AgentAuthors missing %q", want)
		}
	}
}

func TestLoadConfigSingleClass(t *testing.T) {
	clearClassEnv(t)
	t.Setenv("AGENTS_LARGE_ENDPOINT", "http://127.0.0.1:8080/v1")
	t.Setenv("AGENTS_LARGE_MODEL", "qwen3-coder")
	t.Setenv("AGENTS_LARGE_SLOTS", "4")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() = %v", err)
	}
	if len(cfg.Classes) != 1 {
		t.Fatalf("configured %d classes, want 1", len(cfg.Classes))
	}
	large := cfg.Classes[ClassLarge]
	if large.Endpoint != "http://127.0.0.1:8080/v1" || large.Model != "qwen3-coder" {
		t.Errorf("large = %+v", large)
	}
	if large.Slots != 4 || large.QueueDepth != defaultQueueDepth {
		t.Errorf("slots=%d queue=%d, want 4 and the default %d", large.Slots, large.QueueDepth, defaultQueueDepth)
	}
	if cfg.Host != "test-host" {
		t.Errorf("Host = %q, want the configured value: several agent hosts share one CodeArmory", cfg.Host)
	}
}

// A host serving one card serves one or two classes. That is a supported
// deployment, so absent classes must not be an error.
func TestLoadConfigPartialIsValid(t *testing.T) {
	clearClassEnv(t)
	t.Setenv("AGENTS_TINY_ENDPOINT", "http://127.0.0.1:8081")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() = %v, want a partial config to be valid", err)
	}
	if _, ok := cfg.Classes[ClassLarge]; ok {
		t.Error("large was configured without an endpoint")
	}
	if got := cfg.Classes[ClassTiny].Endpoint; got != "http://127.0.0.1:8081/v1" {
		t.Errorf("tiny endpoint = %q, want /v1 appended", got)
	}
}

// No classes at all is a misconfiguration that would otherwise fail at the first
// dispatch, long after startup.
func TestLoadConfigRejectsEmpty(t *testing.T) {
	clearClassEnv(t)
	_, err := LoadConfig()
	if err == nil {
		t.Fatal("LoadConfig() = nil error with no endpoints, want an error at startup")
	}
	if !strings.Contains(err.Error(), "AGENTS_LARGE_ENDPOINT") {
		t.Errorf("error = %v, want it to name the variables to set", err)
	}
}

func TestNormaliseEndpoint(t *testing.T) {
	cases := map[string]string{
		"http://127.0.0.1:8080":     "http://127.0.0.1:8080/v1",
		"http://127.0.0.1:8080/":    "http://127.0.0.1:8080/v1",
		"http://127.0.0.1:8080/v1":  "http://127.0.0.1:8080/v1",
		"http://127.0.0.1:8080/v1/": "http://127.0.0.1:8080/v1",
		"https://api.example/v1":    "https://api.example/v1",
	}
	for in, want := range cases {
		got, err := normaliseEndpoint(in)
		if err != nil {
			t.Errorf("normaliseEndpoint(%q) = %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("normaliseEndpoint(%q) = %q, want %q", in, got, want)
		}
	}
}

// A missing scheme produces a confusing failure at the first request rather than
// at startup, so it is rejected up front.
func TestNormaliseEndpointRejectsSchemeless(t *testing.T) {
	for _, bad := range []string{"127.0.0.1:8080", "localhost:8080/v1", "ftp://host/v1"} {
		if _, err := normaliseEndpoint(bad); err == nil {
			t.Errorf("normaliseEndpoint(%q) = nil error, want a rejection", bad)
		}
	}
}

func TestLoadConfigRejectsBadNumbers(t *testing.T) {
	for _, tc := range []struct{ name, value string }{
		{"AGENTS_LARGE_SLOTS", "many"},
		{"AGENTS_LARGE_SLOTS", "0"},
		{"AGENTS_LARGE_SLOTS", "-1"},
		{"AGENTS_LARGE_QUEUE", "lots"},
	} {
		clearClassEnv(t)
		t.Setenv("AGENTS_LARGE_ENDPOINT", "http://127.0.0.1:8080")
		t.Setenv(tc.name, tc.value)
		if _, err := LoadConfig(); err == nil {
			t.Errorf("LoadConfig() with %s=%q = nil error, want a rejection", tc.name, tc.value)
		}
	}
}

// The API key follows the repo-wide ${NAME}_FILE convention so a mounted secret
// works without changing the deployment.
func TestLoadConfigReadsAPIKeyFromFile(t *testing.T) {
	clearClassEnv(t)
	path := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, []byte("sk-from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTS_LARGE_ENDPOINT", "http://127.0.0.1:8080")
	t.Setenv("AGENTS_LARGE_API_KEY_FILE", path)

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() = %v", err)
	}
	if got := cfg.Classes[ClassLarge].APIKey; got != "sk-from-file" {
		t.Errorf("APIKey = %q, want the file contents with the newline trimmed", got)
	}
}

func TestConfiguredIsSorted(t *testing.T) {
	cfg := Config{Classes: map[Class]ClassConfig{
		ClassTiny:  {},
		ClassLarge: {},
		ClassSmall: {},
	}}
	got := cfg.Configured()
	want := []Class{ClassLarge, ClassSmall, ClassTiny} // alphabetical, so logs are diffable
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Configured() = %v, want %v", got, want)
		}
	}
}

// Capture is opt-OUT, not opt-in: a transcript nobody enabled cannot be
// recovered after the fact, so the default must write.
func TestTranscriptCaptureDefaultsOn(t *testing.T) {
	clearClassEnv(t)
	os.Unsetenv("AGENTS_TRANSCRIPT_DIR")
	t.Setenv("AGENTS_LARGE_ENDPOINT", "http://127.0.0.1:8080")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() = %v", err)
	}
	if !cfg.CaptureEnabled() {
		t.Error("CaptureEnabled() = false by default; capture must be opt-out")
	}
	if cfg.TranscriptDir != defaultTranscriptDir {
		t.Errorf("TranscriptDir = %q, want %q", cfg.TranscriptDir, defaultTranscriptDir)
	}
	if strings.HasPrefix(cfg.TranscriptDir, "/tmp") {
		t.Error("the default transcript dir is under /tmp, which is cleared on reboot")
	}
}

func TestTranscriptCaptureCanBeDisabled(t *testing.T) {
	clearClassEnv(t)
	t.Setenv("AGENTS_LARGE_ENDPOINT", "http://127.0.0.1:8080")
	t.Setenv("AGENTS_TRANSCRIPT_DIR", "off")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() = %v", err)
	}
	if cfg.CaptureEnabled() {
		t.Error("CaptureEnabled() = true with AGENTS_TRANSCRIPT_DIR=off")
	}
}

// The developer sandbox defaults are the whole point of the runner class: a
// container-sized default is what gets a real toolchain OOM-killed mid-compile,
// so a silent regression to MBs here is a regression in the agent's ability to
// build anything at all.
func TestDevSandboxDefaults(t *testing.T) {
	clearClassEnv(t)
	t.Setenv("AGENTS_LARGE_ENDPOINT", "http://127.0.0.1:8080")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() = %v", err)
	}
	if cfg.DevMemoryMB != 8192 {
		t.Errorf("DevMemoryMB = %d, want 8192 (8 GB)", cfg.DevMemoryMB)
	}
	if cfg.DevCPUMillicores != 4000 {
		t.Errorf("DevCPUMillicores = %d, want 4000 (4 cores)", cfg.DevCPUMillicores)
	}
	if cfg.DevConcurrency != 4 {
		t.Errorf("DevConcurrency = %d, want 4", cfg.DevConcurrency)
	}
	if cfg.Repo.RunnerClass != defaultDevRunnerClass {
		t.Errorf("Repo.RunnerClass = %q, want %q", cfg.Repo.RunnerClass, defaultDevRunnerClass)
	}
}

// Every sizing knob must be settable: a host with a smaller box than the one
// these defaults assume has to be able to say so without a rebuild.
func TestDevSandboxOverrides(t *testing.T) {
	clearClassEnv(t)
	t.Setenv("AGENTS_LARGE_ENDPOINT", "http://127.0.0.1:8080")
	t.Setenv("AGENTS_DEV_MEMORY_MB", "2048")
	t.Setenv("AGENTS_DEV_CPU_MILLICORES", "1000")
	t.Setenv("AGENTS_DEV_CONCURRENCY", "2")
	t.Setenv("AGENTS_REPO_RUNNER_CLASS", "custom-dev")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() = %v", err)
	}
	if cfg.DevMemoryMB != 2048 || cfg.DevCPUMillicores != 1000 || cfg.DevConcurrency != 2 {
		t.Errorf("overrides not applied: memory=%d cpu=%d concurrency=%d",
			cfg.DevMemoryMB, cfg.DevCPUMillicores, cfg.DevConcurrency)
	}
	if cfg.Repo.RunnerClass != "custom-dev" {
		t.Errorf("Repo.RunnerClass = %q, want %q", cfg.Repo.RunnerClass, "custom-dev")
	}
}

// A sizing knob that rejects nothing is a knob that lets a host be configured
// into a guaranteed OOM.
func TestDevSandboxRejectsNonPositive(t *testing.T) {
	for _, v := range []string{"AGENTS_DEV_MEMORY_MB", "AGENTS_DEV_CPU_MILLICORES", "AGENTS_DEV_CONCURRENCY"} {
		t.Run(v, func(t *testing.T) {
			clearClassEnv(t)
			t.Setenv("AGENTS_LARGE_ENDPOINT", "http://127.0.0.1:8080")
			t.Setenv(v, "0")
			if _, err := LoadConfig(); err == nil {
				t.Errorf("LoadConfig() with %s=0 = nil, want an error", v)
			}
		})
	}
}

// /tmp is memory-backed, so it is spent from the same budget as the build. If it
// stopped tracking the memory size, a bigger sandbox would silently keep a tmpfs
// sized for the old one and builds would fail on disk space with RAM to spare.
func TestDevRunnerClassTracksMemory(t *testing.T) {
	cfg := Config{
		DevMemoryMB:      8192,
		DevCPUMillicores: 4000,
		Repo:             RepoConfig{RunnerClass: "agent-dev"},
	}
	rc := devRunnerClass(cfg)

	if rc.Name != "agent-dev" {
		t.Errorf("Name = %q, want %q", rc.Name, "agent-dev")
	}
	if rc.MemoryMB != 8192 || rc.CPUMillicores != 4000 {
		t.Errorf("sizing = %d MB / %d millicores, want 8192 / 4000", rc.MemoryMB, rc.CPUMillicores)
	}
	if rc.TmpfsMB != 2048 {
		t.Errorf("TmpfsMB = %d, want a quarter of memory (2048)", rc.TmpfsMB)
	}
	if !rc.Enabled {
		t.Error("Enabled = false; forge refuses to schedule a disabled class")
	}
	if rc.Privileged {
		t.Error("Privileged = true; the developer agent clones and builds, which needs no root")
	}
}

// A repository with no reachable pipeline must still be workable, or a host that
// cannot see the platform's CI is reduced to triage for no good reason.
func TestDevEnabledAcceptsEitherVerificationMode(t *testing.T) {
	repo := func(pipeline, cmd string) Config {
		return Config{Repo: RepoConfig{
			URL: "git://git-local:9418/demo.git", PipelineID: pipeline, TestCommand: cmd,
		}}
	}
	if !repo("pipe-1", "").DevEnabled() {
		t.Error("a pipeline alone should enable the developer agent")
	}
	if !repo("", "go test ./...").DevEnabled() {
		t.Error("a test command alone should enable the developer agent (standalone host)")
	}
	if repo("", "").DevEnabled() {
		t.Error("no verification at all must NOT enable it: unverified pushes look reviewed")
	}
	if (Config{Repo: RepoConfig{PipelineID: "pipe-1"}}).DevEnabled() {
		t.Error("a pipeline with no repository must not enable it")
	}

	// The pipeline wins when both are set: it is the definition CI shares.
	if !repo("pipe-1", "go test ./...").VerifiesWithPipeline() {
		t.Error("VerifiesWithPipeline() = false with a pipeline set")
	}
	if repo("", "go test ./...").VerifiesWithPipeline() {
		t.Error("VerifiesWithPipeline() = true with no pipeline")
	}
}

// The standalone command is read from the environment like every other knob.
func TestLoadConfigReadsTestCommand(t *testing.T) {
	clearClassEnv(t)
	t.Setenv("AGENTS_LARGE_ENDPOINT", "http://127.0.0.1:8080")
	t.Setenv("AGENTS_REPO_URL", "git://git-local:9418/demo.git")
	t.Setenv("AGENTS_REPO_TEST_COMMAND", "go test ./...")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() = %v", err)
	}
	if cfg.Repo.TestCommand != "go test ./..." {
		t.Errorf("TestCommand = %q, want %q", cfg.Repo.TestCommand, "go test ./...")
	}
	if !cfg.DevEnabled() {
		t.Error("DevEnabled() = false with a repo and a test command")
	}
}

// Sessions and executions come from different services on a local plane. Sending
// a login to forge returns a 404 that reads like a rejected password, and every
// sandbox then fails to authenticate — so the fallback must not be the default
// when a gatekeeper URL is given.
func TestSandboxLoginURLPrefersTheGatekeeper(t *testing.T) {
	both := Config{ForgeURL: "http://node:30083", ForgeLoginURL: "http://node:30081"}
	if got := both.SandboxLoginURL(); got != "http://node:30081" {
		t.Errorf("SandboxLoginURL() = %q, want the gatekeeper URL", got)
	}
	// Through conductor one base URL serves both, so the fallback is correct there.
	only := Config{ForgeURL: "https://platform/api"}
	if got := only.SandboxLoginURL(); got != "https://platform/api" {
		t.Errorf("SandboxLoginURL() = %q, want the forge URL as fallback", got)
	}
}

func TestLoadConfigReadsSandboxGatekeeperURL(t *testing.T) {
	clearClassEnv(t)
	t.Setenv("AGENTS_LARGE_ENDPOINT", "http://127.0.0.1:8080")
	t.Setenv("AGENTS_FORGE_URL", "http://node:30083")
	t.Setenv("AGENTS_FORGE_GATEKEEPER_URL", "http://node:30081")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() = %v", err)
	}
	if cfg.SandboxLoginURL() != "http://node:30081" {
		t.Errorf("SandboxLoginURL() = %q", cfg.SandboxLoginURL())
	}
}

// Standalone must be enterable without any platform credential — demanding one
// would make the mode impossible to reach, which is the whole point of it.
func TestDispatchReadyStandalone(t *testing.T) {
	plane := Config{
		TicketsURL: "http://node:30086",
		ForgeURL:   "http://node:30083",
		ForgeEmail: "admin@blacksmith.invalid", ForgePassword: "pw",
	}
	if !plane.DispatchReady() {
		t.Error("DispatchReady() = false for a plane with tickets, forge and a credential")
	}
	if !plane.Standalone() {
		t.Error("Standalone() = false with a local ticket store")
	}

	// A local store with no plane behind it has no credential it accepts.
	noPlane := Config{TicketsURL: "http://node:30086"}
	if noPlane.DispatchReady() {
		t.Error("DispatchReady() = true with a ticket store but no forge or credential")
	}

	// The connected host is unchanged.
	connected := Config{PlatformURL: "https://example/api", PlatformToken: "tok"}
	if !connected.DispatchReady() || connected.Standalone() {
		t.Error("a platform host must stay ready and must not be standalone")
	}
}

// `go run some/tool@latest` downloads and executes code chosen by whoever
// published it most recently, in the sandbox, on every run — a supply-chain
// dependency acquired by accident, unreviewed and unpinned, able to change
// between two runs of the same ticket.
func TestUnpinnedToolsAreReported(t *testing.T) {
	cfg := Config{Repo: RepoConfig{
		ScanCommand:     "go run golang.org/x/vuln/cmd/govulncheck@latest ./...",
		CriticalCommand: "go run github.com/securego/gosec/v2/cmd/gosec@latest ./...",
		LintCommand:     "go vet ./...",
		TestCommand:     "go test ./...",
	}}
	got := cfg.unpinnedTools()
	if len(got) != 2 {
		t.Fatalf("unpinnedTools() = %v, want the two @latest settings", got)
	}
	if got[0] != "AGENTS_REPO_CRITICAL_COMMAND" || got[1] != "AGENTS_REPO_SCAN_COMMAND" {
		t.Errorf("unpinnedTools() = %v, want them named and sorted", got)
	}
	// A pinned version is the point, and must not be reported.
	pinned := Config{Repo: RepoConfig{
		ScanCommand: "go run golang.org/x/vuln/cmd/govulncheck@v1.6.0 ./...",
		TestCommand: "go test ./...",
	}}
	if got := pinned.unpinnedTools(); len(got) != 0 {
		t.Errorf("unpinnedTools() = %v on pinned commands, want none", got)
	}
}

// The test author is the FIRST work stage, so a class that does not resolve must
// fall back rather than leave the stage unstaffed: tickets would pile up in
// ready_for_tests with nothing coming to collect them, and a department that
// stalls silently is worse than one that runs with less independence.
func TestResolveTestClassFallsBackRatherThanStallingThePipeline(t *testing.T) {
	both := Config{
		Classes:   map[Class]ClassConfig{ClassSmall: {}, ClassLarge: {}},
		TestClass: ClassSmall, DevClass: ClassLarge,
	}
	if got := resolveTestClass(both); got != ClassSmall {
		t.Errorf("resolveTestClass = %q, want %q — the configured class resolves", got, ClassSmall)
	}
	if resolveTestClass(both) == both.DevClass {
		t.Error("the test author shares the developer's model when a separate one is available")
	}

	onlyLarge := Config{
		Classes:   map[Class]ClassConfig{ClassLarge: {}},
		TestClass: ClassSmall, DevClass: ClassLarge,
	}
	if got := resolveTestClass(onlyLarge); got != ClassLarge {
		t.Errorf("resolveTestClass = %q with no small class, want the developer's %q", got, ClassLarge)
	}
}

// The coverage stage may run on its own class — a smaller model, on a second
// GPU — because its blast radius is smaller: it cannot touch the implementation
// or the specification, and falling short now carries the work on rather than
// blocking it.
func TestCoverageClassCanDifferFromTheTestAuthors(t *testing.T) {
	both := Config{
		Classes:   map[Class]ClassConfig{ClassSmall: {}, ClassLarge: {}},
		TestClass: ClassLarge, DevClass: ClassLarge, CoverageClass: ClassSmall,
	}
	if got := resolveCoverageClass(both); got != ClassSmall {
		t.Errorf("resolveCoverageClass = %q, want the explicitly configured %q", got, ClassSmall)
	}

	// Unset, it follows the test author: same kind of work, and that is where the
	// operator has already expressed a preference.
	unset := Config{
		Classes:   map[Class]ClassConfig{ClassSmall: {}, ClassLarge: {}},
		TestClass: ClassLarge, DevClass: ClassLarge,
	}
	if got := resolveCoverageClass(unset); got != ClassLarge {
		t.Errorf("resolveCoverageClass = %q with none set, want the test author's %q", got, ClassLarge)
	}

	// A class that is not served falls back rather than leaving the stage
	// unstaffed — tickets would pile up in ready_for_coverage with nothing coming.
	missing := Config{
		Classes:   map[Class]ClassConfig{ClassLarge: {}},
		TestClass: ClassLarge, DevClass: ClassLarge, CoverageClass: ClassTiny,
	}
	if got := resolveCoverageClass(missing); got != ClassLarge {
		t.Errorf("resolveCoverageClass = %q for an unserved class, want the fallback", got)
	}
}

// TIGHTENING THE DEVELOPER MUST NOT TIGHTEN THE AUTHOR.
//
// The two bound different failures. A developer that cannot satisfy a
// specification spins, and only the budget ends it, so its number is deliberately
// small. The specification stages do not spin: their job is as big as the
// specification is, and cutting them stops work that was going to finish.
//
// Measured on r98, when one number served both. The developer's had been cut to
// 40 to bound its loop; the reconciler inherited it, was repairing two test files
// that would not compile, and was stopped at 43 turns mid-repair. Its edits
// persist on the branch, so the retry finished in 8 — the cut bought nothing and
// cost a stage cycle.
func TestTheSpecBudgetIsNotTheDeveloperBudget(t *testing.T) {
	clearClassEnv(t)
	t.Setenv("AGENTS_SMALL_ENDPOINT", "http://127.0.0.1:8081")
	t.Setenv("AGENTS_SPEC_MAX_ITERATIONS", "")
	os.Unsetenv("AGENTS_SPEC_MAX_ITERATIONS")
	t.Setenv("AGENTS_DEV_MAX_ITERATIONS", "40")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() = %v", err)
	}
	if cfg.DevMaxIterations != 40 {
		t.Errorf("dev budget = %d, want the 40 that was asked for", cfg.DevMaxIterations)
	}
	if cfg.SpecMaxIterations != defaultMaxIterations {
		t.Errorf("spec budget = %d, want %d — cutting the developer must not cut the author",
			cfg.SpecMaxIterations, defaultMaxIterations)
	}
}

// And it is still settable on its own, for a host that wants both tightened.
func TestTheSpecBudgetCanBeSetDirectly(t *testing.T) {
	clearClassEnv(t)
	t.Setenv("AGENTS_SMALL_ENDPOINT", "http://127.0.0.1:8081")
	t.Setenv("AGENTS_SPEC_MAX_ITERATIONS", "75")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() = %v", err)
	}
	if cfg.SpecMaxIterations != 75 {
		t.Errorf("spec budget = %d, want 75", cfg.SpecMaxIterations)
	}
}
