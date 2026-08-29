package tools

import (
	"context"
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
}

// Run writes the whole tree into the sandbox and then runs the command, IN ONE
// SCRIPT.
//
// It has to be one script. The sandbox resets its working tree between
// executions — reset --hard, clean -xfd — so a tree written by one execution is
// not there for the next one to compile. Anything an agent writes must be laid
// down and used within the same command.
//
// Writing the whole tree rather than a diff is the simple choice and is bounded
// by the same thing that bounds the prompt: a repository too large to send is
// also too large to reason about, and the failure is loud rather than subtle.
func (f ForgeSandbox) Run(ctx context.Context, files map[string]string, command string) (Output, error) {
	res, err := f.Sandbox.Run(ctx, f.Rec, WriteTreeScript(files)+"\n"+command+"\n")
	if err != nil {
		return Output{}, err
	}
	return Output{ExitCode: res.ExitCode, Stdout: res.Stdout, Stderr: res.Stderr}, nil
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
