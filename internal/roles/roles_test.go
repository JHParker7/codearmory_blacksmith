package roles

import "testing"

// A guard rebuilt from its config must refuse exactly what the compiled guard
// refused — this is the whole safety of making guards data, so it is the test
// that matters most.
func TestGuardConfigRebuildsTheRightRefusals(t *testing.T) {
	cases := []struct {
		name   string
		cfg    *GuardConfig
		writes map[string]bool // path -> should be allowed
	}{
		{"nil denies all", nil, map[string]bool{"a.go": false, "PLAN.md": false}},
		{"deny_all", denyAll(), map[string]bool{"a.go": false, "PLAN.md": false}},
		{"allow_all", &GuardConfig{Kind: GuardAllowAll}, map[string]bool{"a.go": true, "x_test.go": true}},
		{"only_ext md", onlyExt(".md"), map[string]bool{"PLAN.md": true, "main.go": false}},
		{"only_ext go allows go.mod", onlyExt(".go"), map[string]bool{"main.go": true, "go.mod": true, "PLAN.md": false}},
		{"basenames", onlyBasenames("Dockerfile", ".dockerignore"), map[string]bool{"Dockerfile": true, "sub/Dockerfile": true, "main.go": false}},
		{"no_tests+go", noTestsGo(), map[string]bool{"main.go": true, "main_test.go": false, "PLAN.md": false}},
	}
	for _, c := range cases {
		g := c.cfg.Build()
		for path, want := range c.writes {
			allowed := g(path) == nil
			if allowed != want {
				t.Errorf("%s: guard on %q allowed=%v, want %v", c.name, path, allowed, want)
			}
		}
	}
}

// The seed must cover every role the workflows reference and be internally sound
// — a role with no prompt or no guard is one that cannot run.
func TestDefaultsAreCompleteAndSound(t *testing.T) {
	defs := Defaults()
	want := []string{"plan-architect", "plan-test", "plan-dev", "plan-sec", "plan-review", "fix", "review", "devops"}
	byName := map[string]Role{}
	for _, r := range defs {
		byName[r.Name] = r
	}
	for _, name := range want {
		r, ok := byName[name]
		if !ok {
			t.Errorf("default role %q is missing", name)
			continue
		}
		if r.Prompt == "" {
			t.Errorf("role %q has no prompt", name)
		}
		if r.Guard == nil {
			t.Errorf("role %q has no guard", name)
		}
		if len(r.Tools) == 0 {
			t.Errorf("role %q offers no tools", name)
		}
		if r.Class == "" {
			t.Errorf("role %q has no class", name)
		}
	}
}
