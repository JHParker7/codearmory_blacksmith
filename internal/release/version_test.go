package release

import (
	"os/exec"
	"strings"
	"testing"
)

// nextVersion RUNS THE RULE THAT SHIPS, in the shell it ships in.
//
// This is the whole reason the Go copy of the bump rule is gone. Two
// implementations held together by a test is a rule that disagrees with itself
// the first time one side is edited, and it did: r106 cut v0.0.1 where the Go
// half said v0.1.0, because the test's only first-release case carried a feat —
// the single bump where both answers agree.
func nextVersion(t *testing.T, last, subjects, bodies string) string {
	t.Helper()
	for _, bin := range []string{"sh", "awk", "grep"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s is not available; the release rule is shell", bin)
		}
	}
	cmd := exec.Command("sh", "-c",
		versionRule+"\nblacksmith_next_version \"$1\" \"$2\" \"$3\"",
		"sh", last, subjects, bodies)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("running the release rule: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestTheBumpRule(t *testing.T) {
	cases := []struct {
		name     string
		last     string
		subjects []string
		bodies   string
		want     string
	}{
		// THE r106 CASE, both halves of it. The first release is v0.1.0 whatever
		// the commits say, and the bug was that this held for a feat and not for a
		// fix — so both are pinned.
		{"first release from a fix", "", []string{"fix(store): handle a nil map"}, "", "v0.1.0"},
		{"first release from a feat", "", []string{"feat(api): add the list endpoint"}, "", "v0.1.0"},
		{"first release from a breaking change", "", []string{"feat!: replace the store"}, "", "v0.1.0"},
		{"first release from nothing", "", nil, "", "v0.1.0"},

		{"a fix is a patch", "v1.4.2", []string{"fix: handle a nil map"}, "", "v1.4.3"},
		{"a chore is a patch", "v1.4.2", []string{"chore: tidy the imports"}, "", "v1.4.3"},
		{"a feature is a minor", "v1.4.2", []string{"feat: add the list endpoint"}, "", "v1.5.0"},
		{"a scoped feature is a minor", "v1.4.2", []string{"feat(api): add the list endpoint"}, "", "v1.5.0"},
		{"a feature outranks a fix", "v1.4.2", []string{"fix: a", "feat: b", "fix: c"}, "", "v1.5.0"},

		{"a bang is a major", "v1.4.2", []string{"feat!: replace the store"}, "", "v2.0.0"},
		{"a scoped bang is a major", "v1.4.2", []string{"refactor(store)!: rename it"}, "", "v2.0.0"},
		{"a breaking footer is a major", "v1.4.2", []string{"fix: handle a nil map"},
			"fix: handle a nil map\n\nBREAKING CHANGE: the store interface moved.", "v2.0.0"},
		{"a major outranks a feature", "v1.4.2", []string{"feat: a", "fix!: b"}, "", "v2.0.0"},

		// STAYING BELOW 1.0 UNTIL SOMETHING SAYS OTHERWISE: in 0.x a breaking
		// change moves the minor, which is what semver says 0.x is for.
		{"breaking in 0.x moves the minor", "v0.3.1", []string{"feat!: replace the store"}, "", "v0.4.0"},
		{"a feature in 0.x moves the minor", "v0.3.1", []string{"feat: add it"}, "", "v0.4.0"},
		{"a fix in 0.x moves the patch", "v0.3.1", []string{"fix: mend it"}, "", "v0.3.2"},

		// A pre-release suffix is not something this cuts, but one in history must
		// not stop the next version being computed.
		{"a suffix in history still bumps", "v1.4.2-rc1", []string{"fix: mend it"}, "", "v1.4.3"},

		// Prose that is not a conventional subject must not be read as a feature.
		{"prose is a patch", "v1.4.2", []string{"Merge branch 'x'", "featuring a new store"}, "", "v1.4.3"},
		// ...including a line that mentions a feature without being one.
		{"a body mentioning a feature is a patch", "v1.4.2", []string{"docs: explain it"},
			"docs: explain it\n\nThis documents the feat: added last week.", "v1.4.3"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := nextVersion(t, c.last, strings.Join(c.subjects, "\n"), c.bodies)
			if got != c.want {
				t.Errorf("next version = %q, want %q", got, c.want)
			}
		})
	}
}

// The rule the script runs must be the rule the test ran. If TagScript ever
// stopped carrying it — inlining its own copy, say — every case above would
// still pass while nothing checked what shipped.
func TestTagScriptCarriesTheRuleThatWasTested(t *testing.T) {
	script := TagScript()
	if !strings.Contains(script, versionRule) {
		t.Fatal("TagScript does not include the version rule; the tested rule is not the one that runs")
	}
	if !strings.Contains(script, "blacksmith_next_version \"$last\"") {
		t.Error("TagScript does not call the rule it carries")
	}
	if !strings.Contains(script, Marker) {
		t.Error("TagScript prints no marker, so nothing can read the release back")
	}
	// A TAG THAT CANNOT BE PUSHED IS NOT A FAILED INTEGRATION. The merge landed;
	// losing it over the label would trade the work for the name of it.
	if !strings.Contains(script, "|| echo \"WARNING: the tag could not be pushed\"") {
		t.Error("a failed tag push is not tolerated; the merge would be lost with it")
	}
}

// The script must be valid shell. A syntax error here surfaces as an
// integration that silently cuts no tag, since the tagging block is guarded.
func TestTagScriptParses(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh is not available")
	}
	out, err := exec.Command("sh", "-n", "-c", TagScript()).CombinedOutput()
	if err != nil {
		t.Fatalf("the tag script is not valid shell: %v\n%s", err, out)
	}
}

func TestParseReadsTheBlockTheScriptPrints(t *testing.T) {
	out := "merging...\nAlready up to date.\n" + Marker + "\nv1.5.0\nfeat: add it\nfix: mend it\n"
	version, subjects, ok := Parse(out)
	if !ok {
		t.Fatal("Parse did not find the release block")
	}
	if version != "v1.5.0" {
		t.Errorf("version = %q, want v1.5.0", version)
	}
	if len(subjects) != 2 || subjects[0] != "feat: add it" || subjects[1] != "fix: mend it" {
		t.Errorf("subjects = %q", subjects)
	}
}

// ABSENT IS NOT AN ERROR. A merge that changed nothing cuts no tag, and an
// integration is not less successful for having nothing to name.
func TestParseTreatsAMissingReleaseAsNoRelease(t *testing.T) {
	for _, out := range []string{"", "Already up to date.", Marker, Marker + "\n\n"} {
		if _, _, ok := Parse(out); ok {
			t.Errorf("Parse(%q) reported a release", out)
		}
	}
}
