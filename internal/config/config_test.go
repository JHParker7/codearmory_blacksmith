package config

import (
	"strings"
	"testing"

	"github.com/code-armory-app/blacksmith/internal/model"
)

func served(classes ...model.Class) map[model.Class]model.ClassConfig {
	out := map[model.Class]model.ClassConfig{}
	for _, c := range classes {
		out[c] = model.ClassConfig{Endpoint: "http://127.0.0.1/v1", Slots: DefaultSlots}
	}
	return out
}

// AN AGENT THAT PUSHES UNVERIFIED BRANCHES IS WORSE THAN ONE THAT DOES NOTHING,
// because it produces work that LOOKS reviewed. A repository and a way to verify
// are both required.
func TestDevelopmentNeedsARepositoryAndAWayToVerify(t *testing.T) {
	cases := []struct {
		name string
		repo Repo
		want bool
	}{
		{"nothing configured", Repo{}, false},
		{"a repository with no verification", Repo{URL: "git@host/repo"}, false},
		{"verification with no repository", Repo{TestCommand: "go test ./..."}, false},
		{"a pipeline", Repo{URL: "git@host/repo", PipelineID: "p-1"}, true},
		{"a test command", Repo{URL: "git@host/repo", TestCommand: "go test ./..."}, true},
	}
	for _, c := range cases {
		if got := (Config{Repo: c.repo}).DevEnabled(); got != c.want {
			t.Errorf("%s: DevEnabled() = %v, want %v", c.name, got, c.want)
		}
	}
}

// Which mode is in force must be answerable plainly rather than inferred from
// which variables happen to be set.
func TestTheVerificationModeIsStated(t *testing.T) {
	pipeline := Config{Repo: Repo{URL: "r", PipelineID: "p-1", TestCommand: "go test ./..."}}
	if !pipeline.VerifiesWithPipeline() {
		t.Error("a host with a pipeline reports that it does not use one")
	}
	sandbox := Config{Repo: Repo{URL: "r", TestCommand: "go test ./..."}}
	if sandbox.VerifiesWithPipeline() {
		t.Error("a host with no pipeline reports that it uses one")
	}
}

// THE TWO DEPLOYMENTS NEED DIFFERENT THINGS. Demanding a platform credential in
// standalone mode would make that mode impossible to enter.
func TestDispatchReadinessDependsOnWhichDeploymentThisIs(t *testing.T) {
	t.Run("connected", func(t *testing.T) {
		if (Config{PlatformURL: "https://p"}).DispatchReady() {
			t.Error("a connected host with no credential reported ready")
		}
		if (Config{PlatformToken: "t"}).DispatchReady() {
			t.Error("a connected host with no platform URL reported ready")
		}
		if !(Config{PlatformURL: "https://p", PlatformToken: "t"}).DispatchReady() {
			t.Error("a fully configured connected host reported not ready")
		}
	})

	t.Run("standalone", func(t *testing.T) {
		base := Config{TicketsURL: "http://plane", PlatformURL: "", PlatformToken: ""}
		if base.DispatchReady() {
			t.Error("a standalone host with no forge reported ready")
		}

		withToken := base
		withToken.ForgeURL = "http://forge"
		withToken.ForgeToken = "t"
		if !withToken.DispatchReady() {
			t.Error("a standalone host with a plane token reported not ready")
		}

		withLogin := base
		withLogin.ForgeURL = "http://forge"
		withLogin.ForgeEmail, withLogin.ForgePassword = "a@b", "pw"
		if !withLogin.DispatchReady() {
			t.Error("a standalone host that can log in reported not ready")
		}

		// A PLATFORM CREDENTIAL MUST NOT BE REQUIRED HERE.
		noPlatform := withToken
		if !noPlatform.Standalone() {
			t.Error("a host with a local ticket store is not standalone")
		}
	})
}

// THE SESSION URL IS NOT NECESSARILY THE EXECUTION URL. Through conductor they
// coincide; against a local plane they do not, and forge serves no login route —
// so a password exchanged at forge's own URL returns a 404 that reads like a
// rejected credential.
func TestTheSandboxLoginURLFallsBackToTheForge(t *testing.T) {
	if got := (Config{ForgeURL: "http://forge"}).SandboxLoginURL(); got != "http://forge" {
		t.Errorf("SandboxLoginURL() = %q, want the forge URL", got)
	}
	split := Config{ForgeURL: "http://forge", ForgeLoginURL: "http://gatekeeper"}
	if got := split.SandboxLoginURL(); got != "http://gatekeeper" {
		t.Errorf("SandboxLoginURL() = %q, want the gatekeeper", got)
	}
}

// THE FALLBACK MATTERS MORE THAN THE PREFERENCE. The author is the first work
// stage, so a host where its class does not resolve would leave every ticket in
// its queue with nothing coming to collect it.
func TestTheStageClassesFallBackRatherThanVanish(t *testing.T) {
	t.Run("the author prefers a different model", func(t *testing.T) {
		cfg := Config{
			Classes:   served(model.ClassLarge, model.ClassSmall),
			DevClass:  model.ClassLarge,
			TestClass: model.ClassSmall,
		}
		if got := cfg.ResolveTestClass(); got != model.ClassSmall {
			t.Errorf("ResolveTestClass() = %q, want the configured one", got)
		}
	})

	t.Run("one model on the host", func(t *testing.T) {
		cfg := Config{
			Classes:   served(model.ClassLarge),
			DevClass:  model.ClassLarge,
			TestClass: model.ClassSmall, // not served
		}
		if got := cfg.ResolveTestClass(); got != model.ClassLarge {
			t.Errorf("ResolveTestClass() = %q; the first work stage would never run", got)
		}
	})

	t.Run("coverage follows the author", func(t *testing.T) {
		cfg := Config{
			Classes:   served(model.ClassLarge, model.ClassSmall),
			DevClass:  model.ClassLarge,
			TestClass: model.ClassSmall,
		}
		if got := cfg.ResolveCoverageClass(); got != model.ClassSmall {
			t.Errorf("ResolveCoverageClass() = %q, want the author's", got)
		}

		cfg.CoverageClass = model.ClassTiny // not served
		if got := cfg.ResolveCoverageClass(); got != model.ClassSmall {
			t.Errorf("ResolveCoverageClass() = %q, want the author's when its own is absent", got)
		}

		cfg.Classes = served(model.ClassLarge, model.ClassSmall, model.ClassTiny)
		if got := cfg.ResolveCoverageClass(); got != model.ClassTiny {
			t.Errorf("ResolveCoverageClass() = %q, want the configured one once it resolves", got)
		}
	})

	t.Run("the architect follows intake", func(t *testing.T) {
		cfg := Config{Classes: served(model.ClassSmall), PMClass: model.ClassSmall}
		if got := cfg.ResolveArchitectClass(); got != model.ClassSmall {
			t.Errorf("ResolveArchitectClass() = %q, want intake's", got)
		}
		cfg.Classes = served(model.ClassSmall, model.ClassLarge)
		cfg.ArchitectClass = model.ClassLarge
		if got := cfg.ResolveArchitectClass(); got != model.ClassLarge {
			t.Errorf("ResolveArchitectClass() = %q, want the configured one", got)
		}
	})
}

// A COMMAND THAT FETCHES @latest DOWNLOADS AND EXECUTES CODE chosen by whoever
// published it most recently, on every run. Reported rather than refused: a host
// mid-experiment should not be blocked from starting.
func TestUnpinnedToolsAreReportedNotRefused(t *testing.T) {
	cfg := Config{Repo: Repo{
		FormatCommand: DefaultFormatCommand,
		LintCommand:   "go run honnef.co/go/tools/cmd/staticcheck@latest ./...",
		ScanCommand:   "go run github.com/securego/gosec/v2/cmd/gosec@latest ./...",
		TestCommand:   "go test ./...",
	}}

	got := cfg.UnpinnedTools()
	if len(got) != 2 {
		t.Fatalf("UnpinnedTools() = %v, want the two that are unpinned", got)
	}
	// Sorted, so the report is stable between runs.
	if got[0] > got[1] {
		t.Errorf("UnpinnedTools() = %v, want them ordered", got)
	}
	for _, name := range got {
		if !strings.HasPrefix(name, "AGENTS_REPO_") {
			t.Errorf("UnpinnedTools() named %q, which is not a setting", name)
		}
	}

	// THE DEFAULT MUST NOT BE ON THAT LIST. It is the one command every host runs
	// whether or not anybody configured it.
	if strings.Contains(DefaultFormatCommand, "@latest") {
		t.Error("the default format command fetches @latest on every run")
	}
	if len((Config{}).UnpinnedTools()) != 0 {
		t.Error("a host with nothing configured reported unpinned tools")
	}
}

// The default format command has to be able to fix an import block, which is
// most of what a model gets wrong about Go source.
func TestTheDefaultFormatCommandCanFixImports(t *testing.T) {
	if !strings.Contains(DefaultFormatCommand, "goimports") {
		t.Errorf("the default format command is %q; gofmt alone cannot add or remove an import",
			DefaultFormatCommand)
	}
	// ...and it must still work where that tool cannot be fetched.
	if !strings.Contains(DefaultFormatCommand, "gofmt") {
		t.Error("the default format command has no fallback when the tool cannot be fetched")
	}
}

func TestConfiguredIsStableForDiffableLogs(t *testing.T) {
	cfg := Config{Classes: served(model.ClassLarge, model.ClassTiny, model.ClassSmall)}
	first := cfg.Configured()
	for i := 0; i < 5; i++ {
		got := cfg.Configured()
		for j := range got {
			if got[j] != first[j] {
				t.Fatalf("Configured() varies between calls: %v then %v", first, got)
			}
		}
	}
	if len(first) != 3 {
		t.Errorf("Configured() = %v", first)
	}
}
