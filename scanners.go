package main

// The scanner phase: SAST and SCA run in the sandbox after the developer and
// before the security reviewer, and their output lands in the TREE under
// scan/ — the seam the reviewer was built against. Reports are leads the
// reviewer verifies, never verdicts; nothing here gates.
//
// THE REPORTS ARE NEVER COMMITTED. They join the in-memory tree so the
// reviewer reads them like any file, and they reach the run directory's disk,
// but no git add sweeps them up and the push carries only committed history —
// scanner output is findings-shaped, and the operator's rule for findings is
// the board, not the branch.

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/code-armory-app/blacksmith/internal/config"
	"github.com/code-armory-app/blacksmith/internal/tools"
)

// scanSpec is one scanner: where its report lands, and what runs.
type scanSpec struct {
	name   string
	file   string
	cmd    string
	before string // the stage whose reports these are
}

// scanSpecs is wired at startup from the operator's config, with the Go
// defaults when none is given — the same fields the department reads
// (ScanCommand, SCACommand), because "which scanner" is a property of the
// repository, not of the arrangement.
var scanSpecs []scanSpec

func wireScanners(cfg config.Config) {
	sast := cfg.Repo.ScanCommand
	if sast == "" {
		sast = "gosec -quiet ./..."
	}
	sca := cfg.Repo.SCACommand
	if sca == "" {
		sca = "govulncheck ./..."
	}
	lint := cfg.Repo.LintCommand
	if lint == "" {
		lint = "staticcheck ./..."
	}
	scanSpecs = []scanSpec{
		{name: "sast", file: "scan/sast.txt", cmd: sast, before: "plan-sec"},
		{name: "sca", file: "scan/sca.txt", cmd: sca, before: "plan-sec"},
		{name: "lint", file: "scan/lint.txt", cmd: lint, before: "plan-review"},
	}
}

// runScanners executes each scanner over the tree and folds the reports in.
//
// A scanner's exit code is part of the report, not a verdict: gosec exits
// non-zero WHEN IT FINDS THINGS, which is the scanner working. A scanner that
// could not run at all still produces a report saying so, because the
// reviewer reading "command not found" knows to lean on its own eyes, while a
// silently missing file reads as a clean bill.
func runScanners(ctx context.Context, box tools.Sandbox, files map[string]string, before string) map[string]string {
	if box == nil || len(scanSpecs) == 0 {
		return files
	}
	for _, spec := range scanSpecs {
		if spec.before != before {
			continue
		}
		out, err := box.Run(ctx, files, spec.cmd)
		var b strings.Builder
		fmt.Fprintf(&b, "$ %s\n", spec.cmd)
		if err != nil {
			// Transport, not scanner: the sandbox itself was unreachable.
			fmt.Fprintf(&b, "the scanner could not be run: %v\n", err)
			slog.Warn("scanner did not run", "scanner", spec.name, "error", err)
		} else {
			fmt.Fprintf(&b, "exit %d\n%s%s", out.ExitCode, out.Stdout, out.Stderr)
		}
		files[spec.file] = b.String()
		slog.Info("scanner report ready", "scanner", spec.name, "bytes", len(files[spec.file]))
	}
	return files
}
