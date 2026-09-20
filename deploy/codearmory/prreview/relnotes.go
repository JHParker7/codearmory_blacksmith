// Command relnotes turns a git-log dump into a semantic-release-style changelog.
//
// It reads a dump file whose records are `%H \x1f %s \x1f %b` (hash, subject, body)
// separated by \x1e — written shell-safely by the release step (never interpolated)
// so arbitrary commit text survives. Given that dump and a version, it prints:
//
//	## <version> (<YYYY-MM-DD>)
//
//	### Features
//	* **scope:** subject (<shorthash>), closes #12
//	...
//	### Bug Fixes
//	...
//	### BREAKING CHANGES
//	* subject
//
// Only user-facing conventional-commit types appear (feat/fix/perf/revert, plus a
// BREAKING section from a `type!:` bang or a `BREAKING CHANGE:` body trailer). Every
// other type — chore/docs/test/ci/style/build/refactor and the pipeline's own
// review/audit housekeeping — is dropped, so the notes read like a product changelog
// and not a raw commit log.
//
// The pr-review release step embeds this file base64-encoded and `go run`s it, so the
// source lives here for maintenance — regenerate the embedded copy with:
//
//	base64 -w0 deploy/codearmory/prreview/relnotes.go
//
// Args: <dumpFile> <version>.
//go:build ignore

package main

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

// header matches a conventional-commit subject: type(scope)?!?: description.
var header = regexp.MustCompile(`^([a-zA-Z]+)(?:\(([^)]*)\))?(!)?:\s*(.+)$`)

// closesRef matches an issue-closing reference anywhere in the subject or body.
var closesRef = regexp.MustCompile(`(?i)\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\s+#(\d+)`)

type entry struct {
	scope, subject, hash string
	closes               []string
	breaking             string // non-empty when this commit is a breaking change
}

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "relnotes: usage: relnotes <dumpFile> <version>")
		os.Exit(1)
	}
	raw, _ := os.ReadFile(os.Args[1])
	version := strings.TrimPrefix(strings.TrimSpace(os.Args[2]), "v")

	// Group name -> entries, in the order sections should render.
	groups := map[string][]entry{}
	var breaking []entry

	for _, rec := range strings.Split(string(raw), "\x1e") {
		rec = strings.TrimSpace(rec)
		if rec == "" {
			continue
		}
		parts := strings.SplitN(rec, "\x1f", 3)
		if len(parts) < 2 {
			continue
		}
		hash, subject := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		body := ""
		if len(parts) == 3 {
			body = parts[2]
		}
		m := header.FindStringSubmatch(subject)
		if m == nil {
			continue // not a conventional commit — drop
		}
		typ, scope, bang, desc := strings.ToLower(m[1]), m[2], m[3] != "", m[4]

		e := entry{scope: scope, subject: strings.TrimSpace(desc), hash: shortHash(hash), closes: refs(subject + "\n" + body)}

		// Breaking: a `!` in the type, or a `BREAKING CHANGE:` body trailer.
		if bang || breakingTrailer(body) != "" {
			b := breakingTrailer(body)
			if b == "" {
				b = e.subject
			}
			breaking = append(breaking, entry{scope: scope, subject: b, hash: e.hash})
		}

		switch typ {
		case "feat":
			groups["Features"] = append(groups["Features"], e)
		case "fix":
			groups["Bug Fixes"] = append(groups["Bug Fixes"], e)
		case "perf":
			groups["Performance Improvements"] = append(groups["Performance Improvements"], e)
		case "revert":
			groups["Reverts"] = append(groups["Reverts"], e)
		default:
			// chore/docs/test/ci/style/build/refactor + housekeeping: dropped.
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "## %s (%s)\n", version, time.Now().UTC().Format("2006-01-02"))

	for _, name := range []string{"Features", "Bug Fixes", "Performance Improvements", "Reverts"} {
		writeSection(&b, name, groups[name])
	}
	if len(breaking) > 0 {
		b.WriteString("\n### BREAKING CHANGES\n\n")
		for _, e := range breaking {
			b.WriteString("* " + line(e) + "\n")
		}
	}

	if len(groups) == 0 && len(breaking) == 0 {
		b.WriteString("\n_No user-facing changes._\n")
	}
	fmt.Print(b.String())
}

func writeSection(b *strings.Builder, name string, es []entry) {
	if len(es) == 0 {
		return
	}
	// Stable order: by scope then subject, so the list doesn't reshuffle run to run.
	sort.SliceStable(es, func(i, j int) bool {
		if es[i].scope != es[j].scope {
			return es[i].scope < es[j].scope
		}
		return es[i].subject < es[j].subject
	})
	fmt.Fprintf(b, "\n### %s\n\n", name)
	for _, e := range es {
		b.WriteString("* " + line(e) + "\n")
	}
}

// line renders one changelog entry: "**scope:** subject (hash), closes #N".
func line(e entry) string {
	var s strings.Builder
	if e.scope != "" {
		s.WriteString("**" + e.scope + ":** ")
	}
	s.WriteString(e.subject)
	if e.hash != "" {
		s.WriteString(" (" + e.hash + ")")
	}
	if len(e.closes) > 0 {
		refs := make([]string, len(e.closes))
		for i, n := range e.closes {
			refs[i] = "#" + n
		}
		s.WriteString(", closes " + strings.Join(refs, ", "))
	}
	return s.String()
}

func shortHash(h string) string {
	if len(h) > 7 {
		return h[:7]
	}
	return h
}

// refs returns the distinct issue numbers a text closes, in first-seen order.
func refs(text string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range closesRef.FindAllStringSubmatch(text, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	return out
}

// breakingTrailer returns the description after a `BREAKING CHANGE:` / `BREAKING-CHANGE:`
// trailer in the body, or "" when there is none.
func breakingTrailer(body string) string {
	for _, ln := range strings.Split(body, "\n") {
		t := strings.TrimSpace(ln)
		for _, kw := range []string{"BREAKING CHANGE:", "BREAKING-CHANGE:"} {
			if strings.HasPrefix(t, kw) {
				return strings.TrimSpace(strings.TrimPrefix(t, kw))
			}
		}
	}
	return ""
}
