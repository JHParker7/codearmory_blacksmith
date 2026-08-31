package main

import (
	"testing"

	"github.com/code-armory-app/blacksmith/internal/config"
)

// The source scanner for a finding follows from its kind, not a model's choice:
// security is what SAST and SCA raise, quality is what the linter raises.
func TestScannersForKindFollowsTheKind(t *testing.T) {
	wireScanners(config.Config{}) // defaults: sast, sca, lint

	sec := scannersForKind("security")
	if len(sec) != 2 {
		t.Fatalf("security should re-run 2 scanners (sast+sca), got %d", len(sec))
	}
	names := map[string]bool{sec[0].name: true, sec[1].name: true}
	if !names["sast"] || !names["sca"] {
		t.Errorf("security scanners = %v, want sast and sca", names)
	}

	qual := scannersForKind("quality")
	if len(qual) != 1 || qual[0].name != "lint" {
		t.Errorf("quality should re-run only lint, got %v", qual)
	}

	if got := scannersForKind("nonsense"); got != nil {
		t.Errorf("an unknown kind has no source scanner, got %v", got)
	}
}

// A scanner report is reduced to the set of file:line locations it names, so
// two runs can be compared. The column is dropped so a fix that shifts columns
// still matches the same issue.
func TestScanSignaturesExtractsLocations(t *testing.T) {
	report := `$ gosec -quiet ./...
[/src/server.go:42:9] - G104 (CWE-703): Errors unhandled.
[/src/server.go:42:9] - G104 (CWE-703): Errors unhandled.
handler.go:7:2: SA4006: value never used (staticcheck)
exit 1`
	sigs := scanSignatures(report)
	if !sigs["/src/server.go:42"] {
		t.Error("missed the gosec location server.go:42")
	}
	if !sigs["handler.go:7"] {
		t.Error("missed the staticcheck location handler.go:7")
	}
	if len(sigs) != 2 {
		t.Errorf("the two duplicate lines are one issue; got %d signatures", len(sigs))
	}
	if len(scanSignatures("$ gosec\nexit 0\n")) != 0 {
		t.Error("a clean report names no issues")
	}
}

func TestScanVerifyGatesTheFix(t *testing.T) {
	set := func(ss ...string) map[string]bool {
		m := map[string]bool{}
		for _, s := range ss {
			m[s] = true
		}
		return m
	}

	// Cleared the flagged issue, introduced nothing: confirmed.
	if v := scanVerify(set("a.go:1"), set()); !v.ok {
		t.Error("clearing the only issue should confirm the fix")
	}
	// Cleared one of two: still progress, confirmed.
	if v := scanVerify(set("a.go:1", "b.go:2"), set("b.go:2")); !v.ok {
		t.Error("clearing one of two issues should confirm the fix")
	}
	// Scanner never saw anything (reviewer's own catch): confirmed, rests on review.
	if v := scanVerify(set(), set()); !v.ok {
		t.Error("an empty baseline should pass the scanner gate")
	}
	// The issue is still flagged: not confirmed, stuck.
	v := scanVerify(set("a.go:1"), set("a.go:1"))
	if v.ok || !v.stuck {
		t.Errorf("an unchanged issue set must not confirm: %+v", v)
	}
	// A new issue appeared: not confirmed, and it is named.
	v = scanVerify(set("a.go:1"), set("c.go:9"))
	if v.ok || len(v.introduced) != 1 || v.introduced[0] != "c.go:9" {
		t.Errorf("a regression must be caught and named: %+v", v)
	}
}
