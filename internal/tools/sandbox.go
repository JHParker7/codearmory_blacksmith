package tools

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"fmt"
	"sort"
	"strings"

	"github.com/code-armory-app/blacksmith/internal/forge"
)

// Output is one command's result. Kept separate from an error because "the
// command ran and the tests failed" is a normal thing for an agent to reason
// about, while "the sandbox was unreachable" is not. Collapsing them makes a red
// suite indistinguishable from a broken cluster, and the two want opposite
// responses.
type Output struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

// A Sandbox runs one command over a file tree and reports what happened.
//
// AN INTERFACE WITH ONE REAL IMPLEMENTATION, so the tool set can be tested
// without a cluster. `make test` is meant to be safe to run anywhere, and a tool
// package whose tests need forge up is a package nobody runs the tests for.
type Sandbox interface {
	Run(ctx context.Context, files map[string]string, command string) (Output, error)
}

// ForgeSandbox runs commands as forge executions.
type ForgeSandbox struct {
	Sandbox *forge.Sandbox
	Rec     forge.Recorder
	// Env is exported ahead of every command — used to point GOCACHE/GOMODCACHE at a
	// mounted persistent cache volume so a role's per-iteration `go` check reads a warm
	// build+module cache. The cache lives OUTSIDE the reset working tree (/tmp/ws), so
	// it survives the reset --hard/clean the sandbox does between executions.
	Env map[string]string

	// RepoDir, when set, is the path in the lease to a git-backed checkout (the mounted
	// workflow volume, put there by the clone step). When present, Run builds the check's
	// /tmp/ws as a git WORKTREE of that repo's HEAD and then strips it to exactly the
	// agent's files — so the check keeps .git (origin/<ref>, the pre-stage base a check
	// needs for coverage-vs-base and a regression proof) while STILL seeing only what the
	// agent produced. The strip is the piece the old reset+overlay lacked: a committed
	// file the agent's tree omits is deleted, not restored (that leak — a repo's own
	// main.go supplying a func main the agent never wrote — is what forced the files-only
	// /tmp/ws that dropped git). Empty keeps the files-only tree (no repo to borrow .git
	// from), so a lease without a checkout is unaffected.
	RepoDir string
}

// Run writes the whole tree into the sandbox and then runs the command, IN ONE
// SCRIPT.
//
// It has to be one script. The sandbox resets its working tree between
// executions — reset --hard, clean -xfd — so a tree written by one execution is
// not there for the next one to compile. Anything an agent writes must be laid
// down and used within the same command.
//
// LARGE TREES GO COMPRESSED, because forge caps the execution body. Measured: a
// 71KB, 2,306-line tree — main, store, handlers and 1,450 lines of tests, the
// best output of any run to that point — was refused wholesale with `POST
// /executions: 400 Bad Request` on its FIRST check, killing the stage at the
// moment it went to verify. The earlier claim here, that a tree too large to
// send is too large to reason about, was simply false: the model was reasoning
// about that tree fine. Source gzips at 4-5x, which puts any tree the prompt
// budget admits well under the cap.
func (f ForgeSandbox) Run(ctx context.Context, files map[string]string, command string) (Output, error) {
	// A FRESH DIRECTORY, EVERY TIME, because the lease boots on a CLONE and the
	// tree used to be laid on top of it. Whatever the clone holds then leaks
	// into the check: measured, a leftover main.go from an earlier experiment
	// provided `func main` for an agent tree that had none, and the stage went
	// green on a program that does not build. The check's directory must contain
	// exactly what the agent sees — an overlay is a different tree wearing the
	// same name.
	// Export any env (GOCACHE/GOMODCACHE → the mounted cache) and ensure its dirs exist,
	// BEFORE the working tree is laid down — the cache is outside /tmp/ws so it persists
	// across executions and stages.
	var prefix string
	if len(f.Env) > 0 {
		keys := make([]string, 0, len(f.Env))
		for k := range f.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			prefix += "export " + k + "=" + forge.Quote(f.Env[k]) + "\n"
			if k == "GOCACHE" || k == "GOMODCACHE" {
				prefix += "mkdir -p " + forge.Quote(f.Env[k]) + "\n"
			}
		}
	}
	// Prepare the check's working directory. Two shapes, both landing at /tmp/ws
	// holding EXACTLY the agent's files:
	//   - files-only (RepoDir empty): a fresh dir, the files ARE the tree. No git.
	//   - worktree (RepoDir set): a detached worktree of the repo's HEAD — the
	//     pre-stage base, since the stage commits its journal only after the loop —
	//     stripped of every file but its own .git link, so the check has origin/<ref>
	//     and a real HEAD to diff against while a leftover base file cannot survive
	//     the strip. On a repo that has no commits yet, or if the worktree add fails,
	//     it falls back to the fresh dir so a check never dies on setup.
	var setup string
	if f.RepoDir != "" {
		rd := forge.Quote(f.RepoDir)
		setup = "if git -C " + rd + " rev-parse --verify -q HEAD >/dev/null 2>&1; then " +
			"git -C " + rd + " worktree remove --force /tmp/ws 2>/dev/null; git -C " + rd + " worktree prune 2>/dev/null; rm -rf /tmp/ws; " +
			"if git -C " + rd + " worktree add --detach --force /tmp/ws \"$(git -C " + rd + " rev-parse HEAD)\" >/dev/null 2>&1; then " +
			"find /tmp/ws -mindepth 1 -not -path '/tmp/ws/.git*' -delete; cd /tmp/ws; " +
			"else rm -rf /tmp/ws && mkdir -p /tmp/ws && cd /tmp/ws; fi; " +
			"else rm -rf /tmp/ws && mkdir -p /tmp/ws && cd /tmp/ws; fi\n"
	} else {
		setup = "rm -rf /tmp/ws && mkdir -p /tmp/ws && cd /tmp/ws\n"
	}
	script := prefix + setup + WriteTreeScript(files) + "\n" + command + "\n"
	if len(script) > PackThreshold {
		script = packed(script)
	}
	// ACTIONABLE REFUSAL BEFORE FORGE'S OPAQUE ONE. Even compressed, a big enough
	// tree overflows forge's execution-body cap (~64KB), and forge answers with a
	// bare "400 invalid request body" that names neither the size nor the cause —
	// the most expensive failure class here. Measured on the plan arm: a workflow
	// step ran `go build ./...` in the shared volume, which writes the compiled
	// binary INTO the tree; the 1.6MB binary was seeded into the next stage and its
	// first check 400'd. So refuse first, naming the largest files, because a build
	// artifact in a source tree is the usual cause and the fix is to keep it out
	// (build to a temp dir).
	if len(script) > MaxScriptBytes {
		return Output{}, fmt.Errorf(
			"the working tree is too large to run a command over: the materialized script is %d bytes and forge caps an execution body near 64KB. This is almost always a build artifact left in the tree, not source — the largest files are %s. Keep binaries and caches out of the workspace: build to a throwaway dir (e.g. go build -o /tmp/bin/ ./...) so the check reads a source-only tree",
			len(script), largestFiles(files))
	}
	res, err := f.Sandbox.Run(ctx, f.Rec, script)
	if err != nil {
		return Output{}, err
	}
	return Output{ExitCode: res.ExitCode, Stdout: res.Stdout, Stderr: res.Stderr}, nil
}

// PackThreshold is the script size above which it is shipped compressed.
//
// Comfortably under wherever forge's cap sits — the cap is not documented, and
// a threshold discovered by binary search against a live deployment is a
// threshold that breaks when the deployment changes. Below it, plain scripts
// keep the transcript readable.
const PackThreshold = 24 * 1024

// MaxScriptBytes is the largest materialized script (after packing) blacksmith
// will submit. Comfortably under forge's ~64KB execution-body cap, with room for
// the JSON envelope around the command. Past it, Run refuses with a message that
// names the cause rather than letting forge answer "400 invalid request body".
const MaxScriptBytes = 56 * 1024

// largestFiles names the biggest files in a tree, for the too-large refusal. The
// culprit is nearly always one oversized artifact, so the top few point straight
// at it.
func largestFiles(files map[string]string) string {
	type fs struct {
		path string
		size int
	}
	all := make([]fs, 0, len(files))
	for p, c := range files {
		all = append(all, fs{p, len(c)})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].size > all[j].size })
	var b strings.Builder
	for i, f := range all {
		if i >= 5 {
			break
		}
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s (%dKB)", f.path, f.size/1024)
	}
	return b.String()
}

// packed wraps a script so it ships as one base64 line and unpacks in the box.
//
// The alphabet is why this is safe where interpolation was not: base64 emits no
// apostrophes, so the payload cannot close its own quote. `set -e` is restated
// inside because the leased prelude's one applies to the OUTER script, and the
// tree writes were relying on it.
func packed(script string) string {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write([]byte("set -e\n" + script))
	zw.Close()
	return "printf %s '" + base64.StdEncoding.EncodeToString(buf.Bytes()) +
		"' | base64 -d | gzip -d | sh\n"
}

// WriteTreeScript renders a shell script that lays a file tree down on disk.
//
// EVERY PATH AND EVERY BODY IS QUOTED through forge.Quote. Interpolating them
// raw reads correctly and works until a file contains an apostrophe, at which
// point the quote closes early and the shell exits on a syntax error — nothing
// in the script runs, and the failure surfaces as an empty result with no output
// explaining it. That has cost a real outage in this repository already.
//
// The body goes through `printf %s` rather than a heredoc because a heredoc
// delimiter can appear in the file being written, and Go source that happens to
// contain the delimiter word would silently truncate the file at that line.
func WriteTreeScript(files map[string]string) string {
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	// Sorted so two identical trees produce an identical script. An unstable
	// script is a cache miss and an undiffable transcript for no reason.
	sort.Strings(paths)

	var b strings.Builder
	for _, p := range paths {
		if dir := parentDir(p); dir != "" {
			fmt.Fprintf(&b, "mkdir -p %s\n", forge.Quote(dir))
		}
		fmt.Fprintf(&b, "printf %%s %s > %s\n", forge.Quote(files[p]), forge.Quote(p))
	}
	return b.String()
}

func parentDir(p string) string {
	i := strings.LastIndex(p, "/")
	if i <= 0 {
		return ""
	}
	return p[:i]
}
