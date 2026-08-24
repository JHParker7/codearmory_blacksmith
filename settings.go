package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Editing the host's own settings, from the window.
//
// THE FILE HOLDS SECRETS. It carries the agent account's password and the
// platform token beside the ordinary knobs, which decides almost everything
// about how this is written:
//
//   - only NAMED settings are editable, and the list is here rather than derived
//     from whatever the file happens to contain. A generic key/value editor over
//     this file would put credentials on screen and one keystroke from being
//     changed;
//   - the rewrite is line-preserving. Every line this does not manage — comments,
//     secrets, settings added by hand or by a future version — is copied through
//     byte for byte, so saving cannot lose something the editor did not know
//     about;
//   - and the file keeps its mode.
//
// The alternative was regenerating the file from a struct, which is tidier and
// would silently delete every comment in it. The comments in that file are where
// the reasoning lives.

// setting is one editable knob.
type setting struct {
	Key  string
	Name string
	Help string
	// Kind decides validation. Deliberately coarse: the point is to catch "four"
	// where a number belongs, not to reimplement the config loader.
	Kind settingKind
	// Choices, when set, is the allowed set — shown, and enforced.
	Choices []string
}

type settingKind int

const (
	settingInt settingKind = iota
	settingChoice
	settingText
)

// editableSettings is what the window may change.
//
// Chosen for being the things a person actually retunes while running the
// department, rather than everything the loader understands. Endpoints, model
// keys and credentials are deliberately absent: changing those is provisioning,
// not operation, and they are the lines worth protecting from a stray keystroke.
var editableSettings = []setting{
	{
		Key: "AGENTS_DEV_MAX_ITERATIONS", Name: "Turn budget", Kind: settingInt,
		Help: "turns an agent gets per ticket; reaching it means something is wrong, not that it needed them",
	},
	{
		Key: "AGENTS_SPEC_MAX_ITERATIONS", Name: "Spec turn budget", Kind: settingInt,
		Help: "turns the author and the reconciler get; they do not spin, so this is sized by the job",
	},
	{
		Key: "AGENTS_DEV_CONCURRENCY", Name: "Dev concurrency", Kind: settingInt,
		Help: "developer tickets at once; each holds a sandbox, so this is a claim on the box",
	},
	{
		Key: "AGENTS_TEST_CONCURRENCY", Name: "Test concurrency", Kind: settingInt,
		Help: "test-authoring tickets at once; this stage feeds the developers",
	},
	{
		Key: "AGENTS_POLL_SECONDS", Name: "Poll interval", Kind: settingInt,
		Help: "seconds between level-triggered sweeps; hand-offs are immediate regardless",
	},
	{
		Key: "AGENTS_PM_CLASS", Name: "Product manager", Kind: settingChoice,
		Choices: []string{"tiny", "small", "large"},
		Help:    "model class that breaks a request into tickets",
	},
	{
		Key: "AGENTS_TEST_CLASS", Name: "Test author", Kind: settingChoice,
		Choices: []string{"tiny", "small", "large"},
		Help:    "model class that writes the specs; a DIFFERENT model from the developer is the point",
	},
	{
		Key: "AGENTS_DEV_CLASS", Name: "Developer", Kind: settingChoice,
		Choices: []string{"tiny", "small", "large"},
		Help:    "model class that writes the code",
	},
	{
		Key: "AGENTS_SEC_CLASS", Name: "Reviewer", Kind: settingChoice,
		Choices: []string{"tiny", "small", "large"},
		Help:    "model class that reviews a diff and can send it back",
	},
	{
		Key: "AGENTS_REPO_BRANCH", Name: "Base branch", Kind: settingText,
		Help: "default branch projects cut from; a project may override it",
	},
	{
		Key: "AGENTS_REPO_INTEGRATION_BRANCH", Name: "Integration branch", Kind: settingText,
		Help: "default branch reviewed work merges into",
	},
}

// Validate reports why a value is unusable, or nil.
func (s setting) Validate(v string) error {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil // blank means "unset", which restores the built-in default
	}
	switch s.Kind {
	case settingInt:
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("%s must be a number", s.Name)
		}
		if n < 1 {
			return fmt.Errorf("%s must be at least 1", s.Name)
		}
	case settingChoice:
		if !contains(s.Choices, v) {
			return fmt.Errorf("%s must be one of %s", s.Name, strings.Join(s.Choices, ", "))
		}
	}
	return nil
}

// envPath is the settings file this host reads.
func envPath() string {
	if p := strings.TrimSpace(os.Getenv("AGENTS_ENV_FILE")); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "env"
	}
	return filepath.Join(home, ".config", "codearmory-agents", "env")
}

// ReadSettings returns the current value of each editable setting.
//
// Read from the FILE rather than the environment. The window is editing the file,
// and a process started before an edit carries the old value in its environment —
// showing that would mean the form disagreed with what saving would overwrite.
func ReadSettings() (map[string]string, error) {
	out := map[string]string{}
	f, err := os.Open(envPath())
	if os.IsNotExist(err) {
		return out, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", envPath(), err)
	}
	defer f.Close()

	managed := map[string]bool{}
	for _, s := range editableSettings {
		managed[s.Key] = true
	}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := splitEnvLine(sc.Text())
		if ok && managed[k] {
			out[k] = v
		}
	}
	return out, sc.Err()
}

// splitEnvLine parses one KEY=value line, ignoring comments and blanks.
func splitEnvLine(line string) (key, value string, ok bool) {
	t := strings.TrimSpace(line)
	if t == "" || strings.HasPrefix(t, "#") {
		return "", "", false
	}
	k, v, found := strings.Cut(t, "=")
	if !found {
		return "", "", false
	}
	return strings.TrimSpace(k), strings.TrimSpace(v), true
}

// WriteSettings applies changes, preserving every line it does not manage.
//
// A key already present is rewritten in place, which keeps it under whatever
// comment explains it. A new one is appended. A key set to blank is REMOVED, so
// "unset this" and "set it to empty" are the same gesture and the built-in
// default takes over — that is what a person means by clearing a field.
func WriteSettings(changes map[string]string) error {
	for _, s := range editableSettings {
		if v, ok := changes[s.Key]; ok {
			if err := s.Validate(v); err != nil {
				return err
			}
		}
	}

	path := envPath()
	original, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", path, err)
	}
	mode := os.FileMode(0o600)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}

	var out []string
	seen := map[string]bool{}
	for _, line := range strings.Split(string(original), "\n") {
		k, _, ok := splitEnvLine(line)
		if !ok {
			out = append(out, line)
			continue
		}
		v, managed := changes[k]
		if !managed {
			out = append(out, line)
			continue
		}
		seen[k] = true
		if strings.TrimSpace(v) == "" {
			continue // removed: the built-in default applies
		}
		out = append(out, k+"="+v)
	}
	for _, s := range editableSettings {
		v, ok := changes[s.Key]
		if !ok || seen[s.Key] || strings.TrimSpace(v) == "" {
			continue
		}
		out = append(out, s.Key+"="+v)
	}

	body := strings.Join(out, "\n")
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	return atomicWrite(path, []byte(body), mode)
}

// atomicWrite replaces a file without ever leaving it half-written.
func atomicWrite(path string, body []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".settings-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return fmt.Errorf("secure temp file: %w", err)
	}
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return fmt.Errorf("write settings: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("flush settings: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}
