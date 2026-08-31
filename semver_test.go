package main

import "testing"

func TestBumpFromReadsConventionalCommits(t *testing.T) {
	cases := []struct {
		name     string
		subjects []string
		bodies   string
		want     string
	}{
		{"a feature is a minor", []string{"fix(a): x", "feat(b): y"}, "", "minor"},
		{"a fix alone is a patch", []string{"fix(a): x", "chore: mark"}, "", "patch"},
		{"only chores release nothing", []string{"chore: run passed", "docs: note"}, "", "none"},
		{"a bang is a major", []string{"feat(a)!: drop v1"}, "", "major"},
		{"BREAKING CHANGE in body is a major", []string{"feat(a): x"}, "BREAKING CHANGE: renamed", "major"},
		{"feat outranks fix", []string{"fix: a", "feat: b", "fix: c"}, "", "minor"},
	}
	for _, c := range cases {
		if got := bumpFrom(c.subjects, c.bodies); got != c.want {
			t.Errorf("%s: bumpFrom = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestNextVersionAppliesTheBump(t *testing.T) {
	cases := []struct{ last, bump, want string }{
		{"", "minor", "v1.0.0"}, // first release is always 1.0.0
		{"", "major", "v1.0.0"}, // even a breaking first release
		{"", "none", ""},        // nothing releasable, no tag
		{"v1.2.3", "major", "v2.0.0"},
		{"v1.2.3", "minor", "v1.3.0"},
		{"v1.2.3", "patch", "v1.2.4"},
		{"v1.2.3", "none", ""}, // nothing new on top of the last tag
		{"garbage", "patch", ""},
	}
	for _, c := range cases {
		if got := nextVersion(c.last, c.bump); got != c.want {
			t.Errorf("nextVersion(%q,%q) = %q, want %q", c.last, c.bump, got, c.want)
		}
	}
}

func TestConventionalScopeAndNormalize(t *testing.T) {
	if got := ccScope("store.go"); got != "store" {
		t.Errorf("ccScope = %q, want store", got)
	}
	if got := ccScope("internal/api/handlers.go"); got != "handlers" {
		t.Errorf("ccScope nested = %q, want handlers", got)
	}
	// Scope injected into an unscoped typed message.
	if got := ccWithScope("fix: cap the body", "handlers"); got != "fix(handlers): cap the body" {
		t.Errorf("ccWithScope = %q", got)
	}
	// An already-scoped message is left alone.
	if got := ccWithScope("feat(store): add owner", "x"); got != "feat(store): add owner" {
		t.Errorf("ccWithScope should not double-scope: %q", got)
	}
	// A boundary mark becomes a valid chore.
	if got := ccNormalize("run: passed"); got != "chore: run: passed" {
		t.Errorf("ccNormalize mark = %q", got)
	}
	// A conventional subject passes through, collapsed to one line.
	if got := ccNormalize("feat(a): do the thing"); got != "feat(a): do the thing" {
		t.Errorf("ccNormalize valid = %q", got)
	}
	// Everything ccNormalize returns must satisfy the hook's own pattern.
	for _, m := range []string{"run: passed", "stage(plan-sec): passed", "request: build an api", "random text"} {
		if !ccSubject.MatchString(ccNormalize(m)) {
			t.Errorf("ccNormalize(%q) is not a valid subject: %q", m, ccNormalize(m))
		}
	}
}
