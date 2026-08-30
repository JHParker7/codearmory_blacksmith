package main

import (
	"context"
	"strings"
	"testing"

	"github.com/code-armory-app/blacksmith/internal/config"
	"github.com/code-armory-app/blacksmith/internal/tools"
)

// scanBox records what was asked of it and answers like a scanner that found
// things: non-zero exit WITH output, which is the scanner working, not
// failing.
type scanBox struct {
	commands []string
}

func (b *scanBox) Run(_ context.Context, _ map[string]string, cmd string) (tools.Output, error) {
	b.commands = append(b.commands, cmd)
	return tools.Output{ExitCode: 1, Stdout: "G104: unhandled error at store.go:42\n"}, nil
}

// The reports land in the tree where the reviewer reads them, exit status and
// all — a scanner's non-zero exit means findings, and folding it into the
// report keeps the reviewer from mistaking a loud scanner for a broken one.
func TestScannerReportsJoinTheTreeUnderScan(t *testing.T) {
	wireScanners(config.Config{})
	defer func() { scanSpecs = nil }()

	box := &scanBox{}
	files := runScanners(context.Background(), box, map[string]string{"main.go": "package main\n"}, "plan-sec")

	for _, want := range []string{"scan/sast.txt", "scan/sca.txt"} {
		report, ok := files[want]
		if !ok {
			t.Fatalf("%s is missing from the tree", want)
		}
		if !strings.Contains(report, "exit 1") || !strings.Contains(report, "G104") {
			t.Errorf("%s does not carry the scanner's output: %q", want, report)
		}
	}
	if len(box.commands) != 2 {
		t.Fatalf("ran %d scanners, want 2: %v", len(box.commands), box.commands)
	}
	if !strings.Contains(box.commands[0], "gosec") || !strings.Contains(box.commands[1], "govulncheck") {
		t.Fatalf("the default scanners are not the Go pair: %v", box.commands)
	}
}

// The operator's ScanCommand and SCACommand override the defaults — the same
// fields the department reads, because "which scanner" is a property of the
// repository.
func TestTheOperatorsScannersWin(t *testing.T) {
	cfg := config.Config{}
	cfg.Repo.ScanCommand = "semgrep scan"
	cfg.Repo.SCACommand = "osv-scanner ."
	wireScanners(cfg)
	defer func() { scanSpecs = nil }()

	box := &scanBox{}
	runScanners(context.Background(), box, map[string]string{}, "plan-sec")
	if box.commands[0] != "semgrep scan" || box.commands[1] != "osv-scanner ." {
		t.Fatalf("the operator's scanners were ignored: %v", box.commands)
	}
}

// No sandbox, no scanners — a document-only run must not fail over a scan it
// cannot host, and the reviewer's prompt already covers the missing-reports
// case.
func TestNoSandboxMeansNoScanNotAFailure(t *testing.T) {
	wireScanners(config.Config{})
	defer func() { scanSpecs = nil }()

	files := runScanners(context.Background(), nil, map[string]string{"a.md": "x\n"}, "plan-sec")
	if _, ok := files["scan/sast.txt"]; ok {
		t.Fatal("a scan report appeared without a sandbox to have run it")
	}
}
