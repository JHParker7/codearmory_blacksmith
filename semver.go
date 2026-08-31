package main

// Semantic-release versioning from the commit history.
//
// The run's journal is Conventional Commits (see conventional.go), which means
// the next version is not a guess — it is READ from the subjects since the last
// release: a breaking change bumps major, a feat bumps minor, a fix bumps patch.
// The resulting vX.Y.Z is tagged on the run tip and becomes the image's tag, so
// an image's name states exactly what changed to produce it.

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var semverTag = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)$`)

// bumpFrom reads the release level a set of commit subjects (and the collected
// bodies) implies: "major" for a breaking change, "minor" for a feature, "patch"
// for a fix, "none" when nothing releasable is present (chores, docs, marks).
func bumpFrom(subjects []string, bodies string) string {
	level := "none"
	for _, s := range subjects {
		m := ccTyped.FindStringSubmatch(strings.TrimSpace(s))
		if m == nil {
			continue
		}
		typ, breaking := m[1], m[3] == "!"
		switch {
		case breaking:
			return "major"
		case typ == "feat":
			level = "minor"
		case typ == "fix" && level == "none":
			level = "patch"
		}
	}
	if strings.Contains(bodies, "BREAKING CHANGE") {
		return "major"
	}
	return level
}

// nextVersion applies a bump to the last released version. With no prior release
// the line STARTS AT v0.0.0 — first versions are not stable, so a first feature
// is v0.1.0 and a first fix v0.0.1, not a v1.0.0 that claims stability the code
// has not earned. Empty when only chores landed, so a run that changed nothing
// worth shipping is not tagged.
func nextVersion(last, bump string) string {
	if bump == "none" {
		return ""
	}
	if last == "" {
		last = "v0.0.0"
	}
	m := semverTag.FindStringSubmatch(last)
	if m == nil {
		return "" // an unparseable prior tag: leave versioning to a person
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	patch, _ := strconv.Atoi(m[3])
	switch bump {
	case "major":
		major, minor, patch = major+1, 0, 0
	case "minor":
		minor, patch = minor+1, 0
	case "patch":
		patch++
	default:
		return "" // nothing to release on top of the last tag
	}
	return fmt.Sprintf("v%d.%d.%d", major, minor, patch)
}

// tagRelease computes the next version from the journal and tags the run tip
// with it, returning the tag (empty when nothing releasable landed, or on any
// git trouble — best-effort like the rest of the journal). The tag rides along
// on the push and names the image.
func (g *gitLog) tagRelease() string {
	if g == nil || !g.ok {
		return ""
	}
	last := g.lastVersionTag()
	rangeSpec := "HEAD"
	if last != "" {
		rangeSpec = last + "..HEAD"
	}
	subjectsOut, err := g.git("log", rangeSpec, "--format=%s")
	if err != nil {
		return ""
	}
	bodiesOut, _ := g.git("log", rangeSpec, "--format=%b")
	subjects := strings.Split(strings.TrimSpace(subjectsOut), "\n")

	version := nextVersion(last, bumpFrom(subjects, bodiesOut))
	if version == "" {
		return ""
	}
	if out, err := g.git("tag", version); err != nil {
		// A tag that already exists (a rerun of the same tip) is not a failure.
		if !strings.Contains(out, "already exists") {
			return ""
		}
	}
	return version
}

// lastVersionTag is the highest vX.Y.Z tag in the repo, or empty for a project
// that has never been released.
func (g *gitLog) lastVersionTag() string {
	out, err := g.git("tag", "--list", "v*.*.*", "--sort=-version:refname")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if semverTag.MatchString(strings.TrimSpace(line)) {
			return strings.TrimSpace(line)
		}
	}
	return ""
}
