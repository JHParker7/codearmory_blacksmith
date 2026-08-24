// Package release turns the commits an integration produced into a version and
// a set of notes.
//
// VERSIONS COME FROM THE COMMITS, because the commits are the one thing that
// says what changed. The commit-msg hook makes every subject Conventional
// Commits, so the type is not a guess: feat is a minor, a "!" or a BREAKING
// CHANGE footer is a major, anything else is a patch. That is the whole of
// semantic-release's rule, and it only works because the input is ENFORCED
// rather than hoped for.
//
// THE TAG IS CUT IN THE SANDBOX, in the same execution as the merge. Computing
// the version here and tagging in a second round trip would mean a clone, a push
// and a window in which the integration branch moved between the two — and this
// stage is serial precisely because that window is expensive. So the script
// decides the number and everything here reads what it did rather than deciding
// again.
package release

import "strings"

// Marker introduces the block the merge script prints: the new tag on the first
// line, then one commit subject per line.
const Marker = "===BLACKSMITH-RELEASE==="

// ReleasedMarker identifies the comment carrying the release, so the window can
// find it the way it finds a merge.
const ReleasedMarker = "**Released.**"

// versionRule is the bump rule, and THE ONLY COPY OF IT.
//
// The previous shape had this twice — once in shell because that is where it
// runs, once in Go because that is where it could be tested — with a test
// holding the two together. It did not hold them: the first-release case was the
// one place they disagreed, the test's only first-release example carried a feat,
// and that is the single bump where both answers are the same. r106 cut v0.0.1
// where the Go half said v0.1.0.
//
// So there is now one implementation, in the language it executes in, and the
// test RUNS IT — see version_test.go. A rule that only ever executes inside a
// sandbox is a rule nobody checks; a rule with two implementations is a rule
// that will disagree with itself.
//
// It takes its inputs as arguments rather than reading git, so the part that
// decides is separable from the part that needs a repository:
//
//	$1  the previous tag, empty when there is none
//	$2  the commit subjects, one per line
//	$3  the full commit messages, for the BREAKING CHANGE footer
const versionRule = `
blacksmith_next_version() {
  last=$1
  subjects=$2
  bodies=$3

  bump=patch
  if printf '%s\n' "$subjects" | grep -Eq '^[a-z]+(\([^)]*\))?!:'; then bump=major; fi
  if printf '%s\n' "$bodies" | grep -q 'BREAKING CHANGE'; then bump=major; fi
  if [ "$bump" = patch ] && printf '%s\n' "$subjects" | grep -Eq '^feat(\([^)]*\))?:'; then bump=minor; fi

  # THE FIRST RELEASE IS v0.1.0, WHATEVER THE COMMITS SAY.
  #
  # Not v0.0.1. A project's first tagged release is 0.1.0 by convention, and
  # applying the bump to a notional v0.0.0 makes the number depend on whether the
  # first commits happened to include a feature -- v0.1.0 from a feat, v0.0.1
  # from a fix.
  if [ -z "$last" ]; then printf 'v0.1.0'; return; fi

  printf '%s' "$last" | awk -v bump="$bump" '
    { sub(/^v/, ""); split($0, p, "."); maj=p[1]+0; min=p[2]+0; pat=p[3]+0
      # STAYING BELOW 1.0 UNTIL SOMETHING SAYS OTHERWISE. A breaking change in
      # 0.x moves the minor, which is what semver says 0.x is for.
      if (bump == "major") { if (maj == 0) { min += 1; pat = 0 } else { maj += 1; min = 0; pat = 0 } }
      else if (bump == "minor") { min += 1; pat = 0 }
      else { pat += 1 }
      printf "v%d.%d.%d", maj, min, pat }'
}
`

// TagScript cuts and pushes the tag, then prints what it did.
//
// IT RUNS AFTER THE GATES AND THE PUSH, so a tag never names a tree that failed
// to build. A tag that cannot be pushed is NOT a failed integration: the merge
// landed, and losing the merge over a tag would trade the work for the label on
// it.
func TagScript() string {
	return versionRule + `
last=$(git tag --list 'v*' --sort=-v:refname | head -n 1)
if [ -z "$last" ]; then range=""; else range="$last..HEAD"; fi
subjects=$(git log --no-merges --format='%s' $range)
bodies=$(git log --format='%B' $range)
next=$(blacksmith_next_version "$last" "$subjects" "$bodies")
if [ -n "$next" ] && git tag -a "$next" -m "$next" >/dev/null 2>&1; then
  git push origin "$next" >/dev/null 2>&1 || echo "WARNING: the tag could not be pushed"
  echo '` + Marker + `'
  echo "$next"
  printf '%s\n' "$subjects"
fi
`
}

// Parse reads the block the merge script printed.
//
// ABSENT IS NOT AN ERROR: a merge that changed nothing cuts no tag, and an
// integration is not less successful for having nothing to name.
func Parse(out string) (version string, subjects []string, ok bool) {
	_, rest, found := strings.Cut(out, Marker)
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
