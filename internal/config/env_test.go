package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/code-armory-app/blacksmith/internal/model"
)

// clean gives a test an environment with nothing of this department's in it, so
// a setting on the developer's own box cannot decide what a test asserts.
//
// It also points the operator env file at an empty path: without that, Load
// would read whichever real file the machine has and the suite would pass or
// fail depending on whose laptop ran it.
func clean(t *testing.T, vars map[string]string) {
	t.Helper()
	for _, kv := range os.Environ() {
		if k, _, ok := strings.Cut(kv, "="); ok {
			if strings.HasPrefix(k, "AGENTS_") || strings.HasPrefix(k, "CODEARMORY_") {
				t.Setenv(k, "")
				os.Unsetenv(k)
			}
		}
	}
	t.Setenv("AGENTS_ENV_FILE", filepath.Join(t.TempDir(), "nothing-here"))
	for k, v := range vars {
		t.Setenv(k, v)
	}
}

// oneClass is the least a host needs to be configured at all.
func oneClass() map[string]string {
	return map[string]string{"AGENTS_LARGE_ENDPOINT": "http://127.0.0.1:8080"}
}

func TestLoadReadsAClass(t *testing.T) {
	clean(t, map[string]string{
		"AGENTS_LARGE_ENDPOINT":         "http://127.0.0.1:8080",
		"AGENTS_LARGE_MODEL":            "qwen",
		"AGENTS_LARGE_SLOTS":            "6",
		"AGENTS_LARGE_QUEUE":            "12",
		"AGENTS_LARGE_TEMPERATURE":      "0.3",
		"AGENTS_LARGE_REASONING_EFFORT": "none",
	})

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cc, ok := cfg.Classes[model.ClassLarge]
	if !ok {
		t.Fatal("the large class was not configured")
	}
	if cc.Endpoint != "http://127.0.0.1:8080/v1" {
		t.Errorf("endpoint = %q, want the version suffix appended", cc.Endpoint)
	}
	if cc.Model != "qwen" || cc.Slots != 6 || cc.QueueDepth != 12 {
		t.Errorf("class = %+v", cc)
	}
	if cc.Temperature != 0.3 || cc.ReasoningEffort != "none" {
		t.Errorf("sampling = %+v", cc)
	}
	// Absent means supported, so a host that has never heard of the setting keeps
	// the behaviour it had.
	if !cc.ToolsSupported {
		t.Error("tools were disabled on a class that says nothing about them")
	}
}

// A CLASS WITH NO ENDPOINT IS SIMPLY ABSENT. A host running one card serves one
// or two classes, and that is a supported deployment rather than an error.
func TestAClassWithNoEndpointIsAbsent(t *testing.T) {
	clean(t, oneClass())

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Serves(model.ClassTiny) || cfg.Serves(model.ClassSmall) {
		t.Errorf("classes = %v, want only the one with an endpoint", cfg.Configured())
	}
	if got := cfg.Configured(); len(got) != 1 || got[0] != model.ClassLarge {
		t.Errorf("Configured() = %v", got)
	}
}

// NO CLASS AT ALL RETURNS THE CONFIG WITH THE ERROR, not instead of it. Running
// the department without a model is a startup failure, but READING its work is
// not — and refusing to open the window would make the tool useless on exactly
// the machine where you want to see why nothing is being served.
func TestNoClassesReturnsTheConfigAlongsideTheError(t *testing.T) {
	clean(t, map[string]string{"AGENTS_HOST": "gpu-1"})

	cfg, err := Load()
	if !errors.Is(err, ErrNoModelClasses) {
		t.Fatalf("err = %v, want ErrNoModelClasses", err)
	}
	if cfg.Host != "gpu-1" {
		t.Errorf("the config was discarded with the error: %+v", cfg)
	}
	// It must name what to set, or the message is a dead end.
	for _, want := range EndpointVarNames() {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to name %s", err, want)
		}
	}
}

// THE SHARPEST EDGE IN THIS DEPLOYMENT: systemd does not strip inline comments,
// so `FOO=2 # note` sets the literal value `2 # note`. Defaulting on a parse
// failure would take the number the operator wrote and quietly use another.
func TestANumberWithAnInlineCommentIsRefused(t *testing.T) {
	env := oneClass()
	env["AGENTS_DEV_CONCURRENCY"] = "2 # two at a time"
	clean(t, env)

	_, err := Load()
	if err == nil {
		t.Fatal("a setting with an inline comment was accepted")
	}
	if !strings.Contains(err.Error(), "AGENTS_DEV_CONCURRENCY") {
		t.Errorf("err = %v, want it to name the setting", err)
	}
	// The message must explain the trap, because the value looks correct.
	if !strings.Contains(err.Error(), "inline comment") {
		t.Errorf("err = %v, want it to explain why the value is not a number", err)
	}
}

func TestANonPositiveNumberIsRefused(t *testing.T) {
	for _, value := range []string{"0", "-1"} {
		env := oneClass()
		env["AGENTS_DEV_CONCURRENCY"] = value
		clean(t, env)

		if _, err := Load(); err == nil {
			t.Errorf("AGENTS_DEV_CONCURRENCY=%q was accepted", value)
		}
	}
}

// THE FIRST FAILURE IS THE ONE REPORTED, so the message names a setting the
// operator has to fix rather than whichever happened to be read last.
func TestTheFirstBadSettingIsTheOneReported(t *testing.T) {
	env := oneClass()
	env["AGENTS_DEV_CONCURRENCY"] = "not a number"
	env["AGENTS_TEST_CONCURRENCY"] = "also not a number"
	clean(t, env)

	_, err := Load()
	if err == nil {
		t.Fatal("two malformed settings were accepted")
	}
	if !strings.Contains(err.Error(), "AGENTS_DEV_CONCURRENCY") {
		t.Errorf("err = %v, want the first malformed setting", err)
	}
}

// A malformed sampling knob leaves the backend's default in place rather than
// stopping the host: it is a preference, not a routing decision.
func TestAMalformedTemperatureIsIgnoredRatherThanFatal(t *testing.T) {
	env := oneClass()
	env["AGENTS_LARGE_TEMPERATURE"] = "warm"
	clean(t, env)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("a malformed temperature stopped the host: %v", err)
	}
	if got := cfg.Classes[model.ClassLarge].Temperature; got != 0 {
		t.Errorf("temperature = %v, want the backend's own default", got)
	}
}

// AN ENDPOINT WITHOUT A SCHEME OR A VERSION produces a confusing 404 at the
// first request. Both are caught at startup instead.
func TestEndpointsAreNormalisedAndBadOnesRefused(t *testing.T) {
	cases := map[string]string{
		"http://host:8080":     "http://host:8080/v1",
		"http://host:8080/":    "http://host:8080/v1",
		"http://host:8080/v1":  "http://host:8080/v1",
		"http://host:8080/v1/": "http://host:8080/v1",
		"https://api.example":  "https://api.example/v1",
	}
	for in, want := range cases {
		got, err := NormaliseEndpoint(in)
		if err != nil {
			t.Errorf("NormaliseEndpoint(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("NormaliseEndpoint(%q) = %q, want %q", in, got, want)
		}
	}
	for _, bad := range []string{"host:8080", "ftp://host", "", "  "} {
		if _, err := NormaliseEndpoint(bad); err == nil {
			t.Errorf("NormaliseEndpoint(%q) was accepted", bad)
		}
	}
}

func TestABadEndpointNamesTheSetting(t *testing.T) {
	clean(t, map[string]string{"AGENTS_LARGE_ENDPOINT": "127.0.0.1:8080"})

	_, err := Load()
	if err == nil {
		t.Fatal("an endpoint with no scheme was accepted")
	}
	if !strings.Contains(err.Error(), "AGENTS_LARGE_ENDPOINT") {
		t.Errorf("err = %v, want it to name the setting", err)
	}
}

// A SECRET FILE THAT CANNOT BE READ IS AN ERROR. The operator asked for that
// file, and starting without the credential produces a denial at the first
// request, several layers from the cause.
func TestAMissingSecretFileIsReportedAtStartup(t *testing.T) {
	env := oneClass()
	env["CODEARMORY_TOKEN_FILE"] = filepath.Join(t.TempDir(), "absent")
	clean(t, env)

	_, err := Load()
	if err == nil {
		t.Fatal("a named secret file that does not exist was accepted")
	}
	if !strings.Contains(err.Error(), "CODEARMORY_TOKEN_FILE") {
		t.Errorf("err = %v, want it to name the setting", err)
	}
}

func TestASecretFileIsReadAndTrimmed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := oneClass()
	env["CODEARMORY_TOKEN_FILE"] = path
	clean(t, env)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.PlatformToken != "s3cret" {
		t.Errorf("token = %q, want the file's contents without the trailing newline", cfg.PlatformToken)
	}
}

// CONCURRENCY DEFAULTS TO THE SERVING SLOTS. They are the real constraint, so
// any other number either starves the card or oversubscribes it.
func TestConcurrencyDefaultsToTheIntakeClassSlots(t *testing.T) {
	clean(t, map[string]string{
		"AGENTS_SMALL_ENDPOINT": "http://127.0.0.1:8081",
		"AGENTS_SMALL_SLOTS":    "7",
	})

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Concurrency != 7 {
		t.Errorf("concurrency = %d, want the intake class's slot count", cfg.Concurrency)
	}
}

func TestConcurrencyFallsBackWhenTheIntakeClassIsNotServed(t *testing.T) {
	clean(t, oneClass()) // large only; intake defaults to small
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Concurrency != DefaultSlots {
		t.Errorf("concurrency = %d, want the default when the intake class is absent", cfg.Concurrency)
	}
}

// CAPTURE IS OPT-OUT. A transcript nobody enabled cannot be recovered after the
// fact, so only the one exact value turns it off.
func TestCaptureIsOnUnlessExplicitlyTurnedOff(t *testing.T) {
	clean(t, oneClass())
	cfg, _ := Load()
	if !cfg.CaptureEnabled() || cfg.TranscriptDir != DefaultTranscriptDir {
		t.Errorf("capture is off by default: %q", cfg.TranscriptDir)
	}

	env := oneClass()
	env["AGENTS_TRANSCRIPT_DIR"] = TranscriptOff
	clean(t, env)
	cfg, _ = Load()
	if cfg.CaptureEnabled() {
		t.Error("capture stayed on when it was turned off")
	}

	// Any other value is a directory, not a switch.
	env["AGENTS_TRANSCRIPT_DIR"] = "/tmp/elsewhere"
	clean(t, env)
	cfg, _ = Load()
	if !cfg.CaptureEnabled() {
		t.Error("a directory was read as a request to disable capture")
	}
}

// The architect is ON unless explicitly false; coverage is OFF unless explicitly
// true. Each default is the safe direction for its own failure.
func TestTheTwoOptionalStagesDefaultOppositeWays(t *testing.T) {
	clean(t, oneClass())
	cfg, _ := Load()
	if !cfg.ArchitectEnabled {
		t.Error("the architect is off by default; the product manager would emit doc subtasks")
	}
	if cfg.CoverageEnabled {
		t.Error("coverage is on by default; it gates a fan-out on a stage that cannot fail a ticket")
	}

	env := oneClass()
	env["AGENTS_ARCHITECT_ENABLED"] = "false"
	env["AGENTS_COVERAGE_ENABLED"] = "true"
	clean(t, env)
	cfg, _ = Load()
	if cfg.ArchitectEnabled || !cfg.CoverageEnabled {
		t.Errorf("explicit settings were ignored: architect=%v coverage=%v",
			cfg.ArchitectEnabled, cfg.CoverageEnabled)
	}

	// Anything that is not the magic word leaves the default alone.
	env["AGENTS_ARCHITECT_ENABLED"] = "no"
	env["AGENTS_COVERAGE_ENABLED"] = "yes"
	clean(t, env)
	cfg, _ = Load()
	if !cfg.ArchitectEnabled || cfg.CoverageEnabled {
		t.Errorf("an unrecognised value changed a default: architect=%v coverage=%v",
			cfg.ArchitectEnabled, cfg.CoverageEnabled)
	}
}

func TestAListDropsBlanks(t *testing.T) {
	env := oneClass()
	env["AGENTS_REPO_DEPENDENCY_MANIFESTS"] = "go.mod, go.sum ,,"
	clean(t, env)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := cfg.Repo.DependencyManifests
	if len(got) != 2 || got[0] != "go.mod" || got[1] != "go.sum" {
		t.Errorf("manifests = %q; a trailing comma became an entry that matches nothing", got)
	}
}

func TestManifestsFallBackToTheBuiltInSet(t *testing.T) {
	if got := (Repo{}).Manifests(); len(got) == 0 {
		t.Error("a repository with no manifests configured matches nothing")
	}
	custom := Repo{DependencyManifests: []string{"deps.txt"}}
	if got := custom.Manifests(); len(got) != 1 || got[0] != "deps.txt" {
		t.Errorf("Manifests() = %q, want the configured list", got)
	}
}

// THE OPERATOR FILE IS READ, and an explicit export WINS over it: a variable set
// on the command line is a deliberate override.
func TestTheOperatorFileFillsInWhatIsNotAlreadySet(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "env")
	body := "# a comment\n\n" +
		"AGENTS_LARGE_ENDPOINT=http://from-file:8080\n" +
		"AGENTS_HOST=from-file\n" +
		"AGENTS_BOARD_ID=\"quoted-board\"\n" +
		"AGENTS_FORGE_EMAIL='single-quoted'\n" +
		"not a setting\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	clean(t, nil)
	t.Setenv("AGENTS_ENV_FILE", path)
	t.Setenv("AGENTS_HOST", "from-the-command-line")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Host != "from-the-command-line" {
		t.Errorf("host = %q; an explicit export must win over the file", cfg.Host)
	}
	if !cfg.Serves(model.ClassLarge) {
		t.Error("the file's endpoint was not read")
	}
	// systemd strips surrounding quotes and nothing else.
	if cfg.BoardID != "quoted-board" {
		t.Errorf("board = %q, want the quotes stripped", cfg.BoardID)
	}
	if cfg.ForgeEmail != "single-quoted" {
		t.Errorf("email = %q", cfg.ForgeEmail)
	}
}

// A value containing a dollar sign must survive: this file is read literally,
// and a shell-style unquote would turn a password into something else.
func TestTheOperatorFileDoesNotExpandValues(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "env")
	body := "AGENTS_LARGE_ENDPOINT=http://127.0.0.1:8080\n" +
		"AGENTS_FORGE_PASSWORD=p$$w0rd${HOME}\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	clean(t, nil)
	t.Setenv("AGENTS_ENV_FILE", path)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ForgePassword != "p$$w0rd${HOME}" {
		t.Errorf("password = %q, want it taken literally", cfg.ForgePassword)
	}
}

// An absent file is not an error: plenty of hosts configure the process some
// other way.
func TestAMissingOperatorFileIsNotAnError(t *testing.T) {
	clean(t, oneClass())
	t.Setenv("AGENTS_ENV_FILE", filepath.Join(t.TempDir(), "absent"))

	if _, err := Load(); err != nil {
		t.Errorf("a missing operator file failed the load: %v", err)
	}
}

func TestTheHostFallsBackToSomethingNameable(t *testing.T) {
	clean(t, oneClass())
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Host == "" {
		t.Error("the host is unnamed; a transcript could not say which box made a patch")
	}
}
