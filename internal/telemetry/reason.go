// Package telemetry counts what the department does, in labels that cannot grow
// without bound.
//
// WHY IT EXISTS. Every question asked of a run used to be answered by grepping
// transcripts after the fact: how many attempts died on dead actions, which
// stage lost the time, whether a fix moved anything. That worked while the
// answer was one number and stopped working the moment two runs had to be
// compared — a model comparison was decided on figures reconstructed by hand,
// with no way to watch a rate change while a run was still going.
//
// CARDINALITY IS THE WHOLE DESIGN PROBLEM. Outcome details are free text written
// by a model — "20 actions in a row changed nothing" — and a label taking model
// prose as its value is unbounded: one time series per phrasing, until the
// metrics backend falls over. So details map onto a CLOSED set of reason codes,
// and anything unrecognised becomes "other". A growing "other" is the signal to
// add a code, and is cheap; an unbounded label is not.
package telemetry

import "regexp"

// ReasonOther is what an unrecognised detail becomes.
const ReasonOther = "other"

// reasons is the failure taxonomy this runtime actually has, taken from the
// strings the agents emit.
//
// KEEP THEM IN SYNC WITH THOSE STRINGS. A code that stops matching does not
// error — it quietly becomes "other" — so the "other" series is the thing to
// watch after changing any failure message.
var reasons = []struct {
	code string
	re   *regexp.Regexp
}{
	// The ways an attempt dies.
	{"dead_actions", regexp.MustCompile(`(?i)actions in a row changed nothing`)},
	{"read_loop", regexp.MustCompile(`(?i)read \d+ times in a row|reads? in a row without acting`)},
	{"push_failed", regexp.MustCompile(`(?i)could not push the branch`)},
	{"nothing_to_test", regexp.MustCompile(`(?i)nothing to test`)},
	{"nothing_to_commit", regexp.MustCompile(`(?i)nothing to commit`)},
	{"spec_broken", regexp.MustCompile(`(?i)specification is broken|spec is broken`)},
	{"unparseable", regexp.MustCompile(`(?i)could not parse|unparseable`)},

	// The ways it succeeds. Worth their own codes: "how many attempts merged" is
	// as much a rate worth watching as "how many died on a read loop".
	{"pushed", regexp.MustCompile(`(?i)^pushed `)},
	{"merged", regexp.MustCompile(`(?i)^merged `)},
	{"planned", regexp.MustCompile(`(?i)^opened \d+ tasks|^planned as`)},
	{"designed", regexp.MustCompile(`(?i)^designed:`)},
	{"reviewed", regexp.MustCompile(`(?i)^clean \(|^concerns \(`)},
	{"already_compiles", regexp.MustCompile(`(?i)sections already compile together`)},
}

// Reason maps a free-text outcome detail onto a bounded label.
//
// FIRST MATCH WINS, and the order above is therefore part of the taxonomy: the
// failure codes come before the success ones, because a detail that mentions
// both — "pushed, but could not push the branch on the retry" — is a failure.
func Reason(detail string) string {
	for _, r := range reasons {
		if r.re.MatchString(detail) {
			return r.code
		}
	}
	return ReasonOther
}

// Codes lists the closed set, so a test can assert that nothing outside it ever
// reaches a label.
func Codes() []string {
	out := make([]string, 0, len(reasons)+1)
	for _, r := range reasons {
		out = append(out, r.code)
	}
	return append(out, ReasonOther)
}
