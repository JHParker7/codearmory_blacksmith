package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func envFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "env")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTS_ENV_FILE", path)
	return path
}

// THE FILE HOLDS SECRETS, and a save must not disturb anything it does not
// manage. This is the property that makes editing settings from a window safe at
// all: comments, credentials and unknown keys are copied through untouched.
func TestSavingSettingsPreservesEverythingElse(t *testing.T) {
	path := envFile(t, `# how this host reaches the plane
AGENTS_FORGE_PASSWORD=hunter2
CODEARMORY_TOKEN=secret-token

# the budget, and why
AGENTS_DEV_MAX_ITERATIONS=55
AGENTS_SOMETHING_FUTURE=keep-me
`)
	if err := WriteSettings(map[string]string{"AGENTS_DEV_MAX_ITERATIONS": "100"}); err != nil {
		t.Fatalf("WriteSettings() = %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)

	for _, want := range []string{
		"AGENTS_FORGE_PASSWORD=hunter2",
		"CODEARMORY_TOKEN=secret-token",
		"AGENTS_SOMETHING_FUTURE=keep-me",
		"# how this host reaches the plane",
		"# the budget, and why",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("saving lost %q:\n%s", want, s)
		}
	}
	if !strings.Contains(s, "AGENTS_DEV_MAX_ITERATIONS=100") {
		t.Errorf("the edit was not applied:\n%s", s)
	}
	if strings.Contains(s, "AGENTS_DEV_MAX_ITERATIONS=55") {
		t.Errorf("the old value survived:\n%s", s)
	}
	// Rewritten IN PLACE, so it stays under the comment explaining it.
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, "AGENTS_DEV_MAX_ITERATIONS=") && i > 0 {
			if !strings.HasPrefix(lines[i-1], "# the budget") {
				t.Errorf("the setting moved away from its comment:\n%s", s)
			}
		}
	}
}

// Clearing a field means "unset", so the built-in default takes over. That is
// what a person means by emptying a box, and it is the only way back to a
// default once one has been overridden.
func TestClearingASettingRemovesTheLine(t *testing.T) {
	path := envFile(t, "AGENTS_DEV_MAX_ITERATIONS=55\nAGENTS_FORGE_PASSWORD=keep\n")
	if err := WriteSettings(map[string]string{"AGENTS_DEV_MAX_ITERATIONS": ""}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if strings.Contains(string(got), "AGENTS_DEV_MAX_ITERATIONS") {
		t.Errorf("clearing left the key behind:\n%s", got)
	}
	if !strings.Contains(string(got), "AGENTS_FORGE_PASSWORD=keep") {
		t.Errorf("clearing one setting disturbed another:\n%s", got)
	}
}

// A setting the file has never carried is appended rather than dropped.
func TestANewSettingIsAppended(t *testing.T) {
	path := envFile(t, "AGENTS_FORGE_PASSWORD=keep\n")
	if err := WriteSettings(map[string]string{"AGENTS_POLL_SECONDS": "3"}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), "AGENTS_POLL_SECONDS=3") {
		t.Errorf("a new setting was not written:\n%s", got)
	}
}

// A bad value is refused BEFORE the write, so the running configuration survives
// a typo rather than the next restart failing on it.
func TestAnInvalidSettingIsRefusedAndChangesNothing(t *testing.T) {
	path := envFile(t, "AGENTS_DEV_MAX_ITERATIONS=55\n")
	before, _ := os.ReadFile(path)

	for name, change := range map[string]map[string]string{
		"not a number":  {"AGENTS_DEV_MAX_ITERATIONS": "loads"},
		"zero turns":    {"AGENTS_DEV_CONCURRENCY": "0"},
		"unknown class": {"AGENTS_DEV_CLASS": "enormous"},
	} {
		if err := WriteSettings(change); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(before) {
		t.Error("a refused save still modified the file")
	}
}

// Only NAMED settings are editable. A generic editor over this file would put
// credentials on screen and one keystroke from being changed.
func TestSecretsAreNotEditableSettings(t *testing.T) {
	for _, s := range editableSettings {
		for _, forbidden := range []string{"TOKEN", "PASSWORD", "KEY", "SECRET"} {
			if strings.Contains(strings.ToUpper(s.Key), forbidden) {
				t.Errorf("%s is editable from the window; credentials must not be", s.Key)
			}
		}
		if s.Name == "" || s.Help == "" {
			t.Errorf("%s has no name or explanation", s.Key)
		}
	}
}

// Reading comes from the FILE, not the environment: a process started before an
// edit carries the old value, and showing that would make the form disagree with
// what saving overwrites.
func TestSettingsAreReadFromTheFileNotTheEnvironment(t *testing.T) {
	envFile(t, "AGENTS_DEV_MAX_ITERATIONS=42\n")
	t.Setenv("AGENTS_DEV_MAX_ITERATIONS", "999")
	got, err := ReadSettings()
	if err != nil {
		t.Fatal(err)
	}
	if got["AGENTS_DEV_MAX_ITERATIONS"] != "42" {
		t.Errorf("read %q, want the file's value not the environment's", got["AGENTS_DEV_MAX_ITERATIONS"])
	}
}

// The file keeps its mode: it names credentials whether or not this editor
// touches them.
func TestSavingKeepsTheFilePrivate(t *testing.T) {
	path := envFile(t, "AGENTS_DEV_MAX_ITERATIONS=55\n")
	if err := WriteSettings(map[string]string{"AGENTS_DEV_MAX_ITERATIONS": "60"}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("settings file mode = %o, want 600", perm)
	}
}
