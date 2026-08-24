package dev

import (
	"encoding/base64"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/code-armory-app/blacksmith/internal/forge"
)

// Bounds on what the sandbox hands back.
const (
	// MaxFileListing bounds the repository listing. A tree of ten thousand files
	// is not more useful to the model than a tree of four hundred; it is a
	// context window spent on names.
	MaxFileListing = 400

	// MaxFileForModel bounds one file's contents. A file larger than this is
	// almost always generated — a lock file, a fixture, a vendored dependency —
	// and pasting it whole leaves no room for the work.
	MaxFileForModel = 24 * 1024
)

// FileBlockMarker frames one file in a read's output.
//
// FRAMED AND BASE64-ENCODED, so content with newlines, shell metacharacters or
// invalid UTF-8 survives the round trip intact. The alternative is parsing a
// concatenation of arbitrary file contents, where any file containing the
// separator corrupts everything after it.
const FileBlockMarker = "===FILE "

// SurveyScript lists what the repository holds.
func SurveyScript() string {
	return fmt.Sprintf("git ls-files | head -n %d\n", MaxFileListing)
}

// ParseSurvey reads a listing, dropping the blank lines a `head` leaves behind.
func ParseSurvey(stdout string) []string {
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}

// ReadScript fetches several files in ONE sandbox run.
//
// ONE RUN, NOT ONE PER FILE. A sandbox round trip is ten to twenty seconds and a
// read is meant to be nearly free; asking for four files one at a time would
// spend a minute learning what one command answers.
//
// A MISSING FILE IS SIMPLY ABSENT from the output rather than an error. The
// model names paths from a listing that may be stale, or guesses a name that
// looks right, and failing the whole read because one of four paths does not
// exist would cost a turn and teach nothing.
func ReadScript(paths []string) string {
	var b strings.Builder
	for _, p := range paths {
		q := forge.Quote(p)
		fmt.Fprintf(&b, "\nif [ -f %s ]; then echo %s; base64 -w0 < %s; echo; fi",
			q, forge.Quote(FileBlockMarker+p), q)
	}
	b.WriteString("\n")
	return b.String()
}

// ParseRead decodes what a read returned.
//
// A BLOCK THAT WILL NOT DECODE IS SKIPPED, not fatal: the surrounding output is
// still good, and one unreadable file must not cost the three that read cleanly.
func ParseRead(stdout string) map[string]string {
	out := map[string]string{}
	lines := strings.Split(stdout, "\n")

	for i := 0; i < len(lines); i++ {
		name, ok := strings.CutPrefix(lines[i], FileBlockMarker)
		if !ok || i+1 >= len(lines) {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(lines[i+1]))
		if err != nil {
			continue
		}
		out[strings.TrimSpace(name)] = clipContent(string(raw), MaxFileForModel)
		i++
	}
	return out
}

// RecordRead files what a read returned, and remembers what it did NOT.
//
// A PATH THE MODEL ASKED FOR AND DID NOT GET IS RECORDED AS MISSING, which is
// what lets a later create succeed: without it the write is refused as an
// overwrite of a file the listing mentions and the sandbox does not have, and
// the agent has no way to resolve the contradiction.
func (s *State) RecordRead(asked []string, got map[string]string) {
	if s.Read == nil {
		s.Read = map[string]string{}
	}
	if s.Missing == nil {
		s.Missing = map[string]bool{}
	}
	if s.Baseline == nil {
		s.Baseline = map[string]string{}
	}

	for _, p := range asked {
		content, found := got[p]
		if !found {
			s.Missing[p] = true
			continue
		}
		delete(s.Missing, p)
		s.Read[p] = content

		// THE BASELINE IS CAPTURED ON THE FIRST READ AND NEVER OVERWRITTEN. It
		// answers "is this write throwing away code that was already there", and
		// that question needs the ORIGINAL — Read is deliberately updated with the
		// agent's own edits, so by the time a second write arrives it is no longer
		// a comparison to anything.
		if _, seen := s.Baseline[p]; !seen {
			s.Baseline[p] = content
		}
	}
}

// ReadOutcome describes a completed read for the history line.
func ReadOutcome(asked []string, got map[string]string) string {
	var missing []string
	for _, p := range asked {
		if _, found := got[p]; !found {
			missing = append(missing, p)
		}
	}
	switch {
	case len(missing) == len(asked):
		return fmt.Sprintf("none of those files exist: %s", strings.Join(missing, ", "))
	case len(missing) > 0:
		return fmt.Sprintf("read %d of %d; these do not exist: %s",
			len(asked)-len(missing), len(asked), strings.Join(missing, ", "))
	default:
		return fmt.Sprintf("read %d file(s)", len(asked))
	}
}

// clipContent bounds a file's contents WITHOUT trimming them.
//
// THE ORDINARY clip TRIMS WHITESPACE, and using it here silently changed every
// file the agent is shown. Two consequences, and the second is the dangerous
// one:
//
//   - a trailing newline is removed, so a whole-file rewrite of unchanged
//     content reads as a change against the baseline;
//   - and LEADING whitespace is removed, which shifts every line number the
//     agent is given by one. A line-addressed edit then lands one line off,
//     which is exactly the arithmetic failure the numbered listing exists to
//     prevent.
//
// A file is DATA. What the agent sees has to be what the branch holds, bounded
// only in length.
func clipContent(s string, maxBytes int) string {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "\n…(truncated)\n"
}
