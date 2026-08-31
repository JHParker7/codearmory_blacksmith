package main

// A fix is not done because a reviewer liked it. It is done when the TOOL THAT
// RAISED THE ALARM runs again and stays quiet. The reviewer says the change
// reads right; the scanner says the detector no longer sees the hole. The
// operator asked for the second by name — "the linter/sca/sast or whatever the
// source of the issue is should be reran and the issue is fixed if it doesn't
// get detected again" — so a fix passes only when both agree.
//
// Which scanner is "the source" is not a guess: a finding is KINDED at filing
// time, and the kind names the tools. security → SAST + SCA, quality → lint.

import (
	"context"
	"regexp"
	"sort"
	"strings"

	"github.com/code-armory-app/blacksmith/internal/tools"
)

// scannersForKind returns the scanners that could have raised a finding of this
// kind — the ones re-run to confirm the fix. Empty for an unknown kind, which
// means no scanner gate and the fix rests on the review alone.
func scannersForKind(kind string) []scanSpec {
	var out []scanSpec
	for _, s := range scanSpecs {
		switch {
		case kind == "security" && (s.name == "sast" || s.name == "sca"):
			out = append(out, s)
		case kind == "quality" && s.name == "lint":
			out = append(out, s)
		}
	}
	return out
}

// runKindScanners runs a kind's source scanners over a tree and returns their
// combined report. Empty when no scanner or no sandbox is available — an absent
// detector cannot confirm OR deny, and the caller reads empty as "no scanner
// signal" and falls back to the review, never as a clean bill.
func runKindScanners(ctx context.Context, box tools.Sandbox, files map[string]string, kind string) string {
	specs := scannersForKind(kind)
	if box == nil || len(specs) == 0 {
		return ""
	}
	var b strings.Builder
	for _, spec := range specs {
		out, err := box.Run(ctx, files, spec.cmd)
		b.WriteString("$ " + spec.cmd + "\n")
		if err != nil {
			// The sandbox itself failed: no signal, not a clean bill.
			b.WriteString("the scanner could not be run: " + err.Error() + "\n")
			continue
		}
		b.WriteString(out.Stdout)
		b.WriteString(out.Stderr)
		b.WriteString("\n")
	}
	return b.String()
}

// scanSignatures reduces a scanner report to the SET OF ISSUES it names, so two
// runs can be compared. The common denominator across gosec, staticcheck, and
// govulncheck text output is a `path.go:line` location; that location is the
// issue's identity here — a fix that clears it makes it vanish from the report,
// and a fix that regresses adds a new one. A `:col` suffix is dropped so the
// same issue matches even if a fix shifts the column.
var sigLine = regexp.MustCompile(`([\w./-]+\.go):(\d+)`)

func scanSignatures(report string) map[string]bool {
	sigs := map[string]bool{}
	for _, m := range sigLine.FindAllStringSubmatch(report, -1) {
		sigs[m[1]+":"+m[2]] = true
	}
	return sigs
}

// scanVerdict is the comparison of a scanner's before-and-after around a fix.
type scanVerdict struct {
	ok         bool     // the fix may be accepted on the scanner's evidence
	introduced []string // NEW issue signatures the fix added — regressions
	stuck      bool     // the scanner had issues and the fix cleared none
}

// scanVerify judges a fix by what the scanner said before it and after it. A
// fix is confirmed only when the detector goes quieter and nowhere louder: it
// must introduce NO new issue, and — when the scanner had something to say
// before — must clear at least one. A finding the scanner never saw (empty
// before) is a reviewer's own catch that the scanner cannot speak to; it passes
// here and rests on the review.
func scanVerify(before, after map[string]bool) scanVerdict {
	var v scanVerdict
	for s := range after {
		if !before[s] {
			v.introduced = append(v.introduced, s)
		}
	}
	sort.Strings(v.introduced)
	if len(v.introduced) > 0 {
		return v // regressed: not ok, and the caller names what appeared
	}
	if len(before) > 0 && len(after) >= len(before) {
		v.stuck = true // cleared nothing the scanner still sees
		return v
	}
	v.ok = true
	return v
}
