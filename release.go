package main

import (
	"fmt"
	"strconv"
	"strings"
)

// VERSIONS COME FROM THE COMMITS, because the commits are now the one thing that
// says what changed.
//
// The commit-msg hook makes every subject Conventional Commits, so the type is no
// longer a guess: feat is a minor, a "!" or a BREAKING CHANGE footer is a major,
// anything else is a patch. That is the whole of semantic-release's rule, and it
// only works because the input is enforced rather than hoped for.
//
// THE TAG IS CUT IN THE SANDBOX, in the same execution as the merge. Computing
// the version here and tagging in a second round trip would mean a clone, a push
// and a window in which the integration branch moved between the two — and this
// stage is serial precisely because that window is expensive. So the script
// decides the number and reports it back, and everything below reads what it did
// rather than deciding again.

// releaseMarker introduces the block the merge script prints: the new tag on the
// first line, then one commit subject per line.
const releaseMarker = "===BLACKSMITH-RELEASE==="

// releasedMarker identifies the comment carrying the release, so the window can
// find it the way it finds a merge.
const releasedMarker = "**Released.**"

// change is one commit, read back from its subject.
type change struct {
	kind     string
	scope    string
	breaking bool
	text     string
}

// parseSubject reads a Conventional Commits subject. A subject that is not one
// comes back with an empty kind: the hook rejects those now, but history written
// before it exists still has to render.
func parseSubject(subject string) change {
	subject = strings.TrimSpace(subject)
	head, text, ok := strings.Cut(subject, ": ")
	if !ok {
		return change{text: subject}
	}
	c := change{text: strings.TrimSpace(text)}
	if strings.HasSuffix(head, "!") {
		c.breaking = true
		head = strings.TrimSuffix(head, "!")
	}
	if i := strings.Index(head, "("); i >= 0 && strings.HasSuffix(head, ")") {
		c.scope = head[i+1 : len(head)-1]
		head = head[:i]
	}
	if !contains(conventionalTypes, head) && head != "revert" {
		return change{text: subject}
	}
	c.kind = head
	return c
}

// releaseSections is the order types appear in the notes, and the heading each
// one gets. Types not listed are collected under "Other", so a new type shows up
// rather than vanishing — the same closed-set-with-a-fallback rule the metric
// labels follow.
var releaseSections = []struct{ kind, heading string }{
	{"feat", "Features"},
	{"fix", "Fixes"},
	{"perf", "Performance"},
	{"refactor", "Refactoring"},
	{"docs", "Documentation"},
	{"test", "Tests"},
	{"build", "Build"},
	{"ci", "CI"},
	{"revert", "Reverts"},
}

// releaseNotes renders one release: what changed, grouped, with the breaking
// changes first because they are the ones that need reading.
func releaseNotes(version string, subjects []string) string {
	changes := make([]change, 0, len(subjects))
	for _, s := range subjects {
		if strings.TrimSpace(s) == "" {
			continue
		}
		changes = append(changes, parseSubject(s))
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s %s\n", releasedMarker, version)
	if len(changes) == 0 {
		b.WriteString("\nNo commits since the last release.")
		return b.String()
	}

	var breaking []change
	for _, c := range changes {
		if c.breaking {
			breaking = append(breaking, c)
		}
	}
	if len(breaking) > 0 {
		b.WriteString("\n**Breaking**\n")
		for _, c := range breaking {
			b.WriteString("- " + withScope(c) + "\n")
		}
	}

	seen := map[string]bool{}
	for _, sec := range releaseSections {
		var in []change
		for _, c := range changes {
			if c.kind == sec.kind {
				in = append(in, c)
			}
		}
		if len(in) == 0 {
			continue
		}
		seen[sec.kind] = true
		fmt.Fprintf(&b, "\n**%s**\n", sec.heading)
		for _, c := range in {
			b.WriteString("- " + withScope(c) + "\n")
		}
	}

	var other []change
	for _, c := range changes {
		if c.kind == "" || (!seen[c.kind] && !isSectioned(c.kind)) {
			other = append(other, c)
		}
	}
	if len(other) > 0 {
		b.WriteString("\n**Other**\n")
		for _, c := range other {
			b.WriteString("- " + withScope(c) + "\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func isSectioned(kind string) bool {
	for _, sec := range releaseSections {
		if sec.kind == kind {
			return true
		}
	}
	return false
}

// withScope renders one line: the scope leads, because "store: add an index" says
// where before it says what.
func withScope(c change) string {
	if c.scope == "" {
		return c.text
	}
	return "`" + c.scope + "` " + c.text
}

// parseRelease reads the block the merge script printed. Absent is not an error:
// a merge that changed nothing cuts no tag.
func parseRelease(out string) (version string, subjects []string, ok bool) {
	_, rest, found := strings.Cut(out, releaseMarker)
	if !found {
		return "", nil, false
	}
	lines := strings.Split(strings.TrimSpace(rest), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) == "" {
		return "", nil, false
	}
	version = strings.TrimSpace(lines[0])
	for _, l := range lines[1:] {
		if s := strings.TrimSpace(l); s != "" {
			subjects = append(subjects, s)
		}
	}
	return version, subjects, true
}

// bumpFor says which part of the version the commits move.
//
// Here as well as in the script because the script is where it RUNS and this is
// where it can be tested: shell that only executes inside a sandbox is shell
// nobody checks. TestTheBumpRuleMatchesTheScript holds the two together.
func bumpFor(subjects []string) string {
	bump := "patch"
	for _, s := range subjects {
		c := parseSubject(s)
		if c.breaking || strings.Contains(s, "BREAKING CHANGE") {
			return "major"
		}
		if c.kind == "feat" {
			bump = "minor"
		}
	}
	return bump
}

// nextVersion applies a bump to a tag like "v1.4.2". An unreadable or absent
// previous tag starts at v0.1.0: a first release is not v1, and pretending
// otherwise would claim a stability nothing has earned yet.
func nextVersion(prev, bump string) string {
	major, minor, patch, ok := parseVersion(prev)
	if !ok {
		return "v0.1.0"
	}
	switch bump {
	case "major":
		// STAYING BELOW 1.0 UNTIL SOMETHING SAYS OTHERWISE. A breaking change in
		// 0.x moves the minor, which is what the semver spec says 0.x is for.
		if major == 0 {
			return fmt.Sprintf("v0.%d.0", minor+1)
		}
		return fmt.Sprintf("v%d.0.0", major+1)
	case "minor":
		return fmt.Sprintf("v%d.%d.0", major, minor+1)
	default:
		return fmt.Sprintf("v%d.%d.%d", major, minor, patch+1)
	}
}

func parseVersion(tag string) (major, minor, patch int, ok bool) {
	t := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(tag), "v"))
	parts := strings.SplitN(t, ".", 3)
	if len(parts) != 3 {
		return 0, 0, 0, false
	}
	out := make([]int, 3)
	for i, p := range parts {
		// A pre-release or build suffix is not something this cuts, but one in
		// history must not stop the next version being computed.
		if j := strings.IndexAny(p, "-+"); j >= 0 {
			p = p[:j]
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return 0, 0, 0, false
		}
		out[i] = n
	}
	return out[0], out[1], out[2], true
}

// releaseTagScript cuts and pushes the tag, then prints what it did.
//
// IT RUNS AFTER THE GATES AND THE PUSH, so a tag never names a tree that failed
// to build. A tag that cannot be pushed is not a failed integration: the merge
// landed, and losing the merge over a tag would trade the work for the label on
// it.
func releaseTagScript() string {
	return fmt.Sprintf(`
last=$(git tag --list 'v*' --sort=-v:refname | head -n 1)
if [ -z "$last" ]; then range=""; else range="$last..HEAD"; fi
subjects=$(git log --no-merges --format='%%s' $range)
bump=patch
if printf '%%s\n' "$subjects" | grep -Eq '^[a-z]+(\([^)]*\))?!:'; then bump=major; fi
if git log --format='%%B' $range | grep -q 'BREAKING CHANGE'; then bump=major; fi
if [ "$bump" = patch ] && printf '%%s\n' "$subjects" | grep -Eq '^feat(\([^)]*\))?:'; then bump=minor; fi
# THE FIRST RELEASE IS v0.1.0, WHATEVER THE COMMITS SAY.
#
# Not v0.0.1. A project's first tagged release is 0.1.0 by convention, and
# applying the bump to a notional v0.0.0 makes the number depend on whether the
# first commits happened to include a feature -- v0.1.0 from a feat, v0.0.1 from
# a fix. nextVersion in Go has always returned v0.1.0 here; this is the half
# that disagreed.
#
# Measured on r106, which cut v0.0.1 for its first release while Go would have
# said v0.1.0. TestTheBumpRuleMatchesTheScript existed to catch exactly this and
# did not, because its only first-release case carried a feat -- the one bump
# where the two agreed.
if [ -z "$last" ]; then
  next=v0.1.0
else
next=$(printf '%%s' "$last" | awk -v bump="$bump" '
  { sub(/^v/, ""); split($0, p, "."); maj=p[1]+0; min=p[2]+0; pat=p[3]+0
    if (bump == "major") { if (maj == 0) { min += 1; pat = 0 } else { maj += 1; min = 0; pat = 0 } }
    else if (bump == "minor") { min += 1; pat = 0 }
    else { pat += 1 }
    printf "v%%d.%%d.%%d", maj, min, pat }')
fi
if [ -n "$next" ] && git tag -a "$next" -m "$next" >/dev/null 2>&1; then
  git push origin "$next" >/dev/null 2>&1 || echo "WARNING: the tag could not be pushed"
  echo '%s'
  echo "$next"
  printf '%%s\n' "$subjects"
fi
`, releaseMarker)
}

// releaseOf reads a release back off a ticket, the way the window reads a merge.
//
// THE LAST ONE WINS. A ticket sent back and integrated twice carries two, and the
// one that describes the code now is the later.
func releaseOf(t Ticket) (version, notes string) {
	for _, c := range t.Comments {
		body := strings.TrimSpace(c.Body)
		if !strings.HasPrefix(body, releasedMarker) {
			continue
		}
		first, rest, _ := strings.Cut(body, "\n")
		version = strings.TrimSpace(strings.TrimPrefix(first, releasedMarker))
		notes = strings.TrimSpace(rest)
	}
	return version, notes
}
