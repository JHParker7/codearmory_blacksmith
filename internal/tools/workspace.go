// Package tools is the sandbox and the tool set an agent acts through.
//
// One package, because a tool and the box it runs in are the same decision. The
// tools that only read and write source do it against an in-memory Workspace;
// the one that runs a command does it in a Sandbox. Splitting those apart would
// put the write guard in one package and the thing it guards in another.
package tools

import (
	"fmt"
	"go/parser"
	"go/token"
	"sort"
	"strings"

	"github.com/code-armory-app/blacksmith/internal/edit"
)

// Workspace is the file tree the agent edits, held in memory.
//
// IN MEMORY, NOT IN THE SANDBOX, because a sandbox here is a remote forge
// execution rather than a local container. Every tool call would otherwise cost
// a submit-and-poll round trip against the cluster, and a read of four files
// would cost four. Edits are resolved and applied locally against this tree and
// the whole tree is written out once, when something actually needs to run — see
// Sandbox.
//
// The tree is also what makes a no-op edit detectable at all: the content before
// an edit has to be in hand to notice that the content after it is identical.
type Workspace struct {
	files map[string]string

	// guard decides what may be written. A FUNCTION RATHER THAN A LIST, so that
	// "the developer may not edit tests" and "the architect may only write
	// markdown" are the same mechanism with different arguments.
	guard Guard

	// last is the single write undo_edit reverts, or nil when nothing has been
	// written yet. Content of "" with existed=false means the file was created
	// and undoing it means removing it.
	last *undoRecord

	// known is every path the agent has read or written, in the order it first
	// touched them.
	//
	// THIS IS THE AGENT'S MEMORY, and it exists because the prompt is rebuilt
	// from scratch each turn. Without it the only record of a file the agent
	// wrote twenty turns ago is a tool result in a rolling window, truncated —
	// and a truncated file is worse than no file, because the agent cannot tell
	// which half it is missing. Measured on the first live run: model.go
	// rendered to 2,348 characters, the trail trimmed results to 1,200 keeping
	// the TAIL, so `type Comment struct` was never once visible. The agent wrote
	// a Comment literal with a field the struct does not have, then re-read the
	// same file fifteen times trying to see the half it was never shown.
	knownOrder []string

	// writes counts edits that were actually applied. A stage that produced
	// nothing has not finished, however tidily it stopped.
	writes int
}

// Writes is how many edits this workspace has accepted.
func (w *Workspace) Writes() int { return w.writes }

// Seen records that the agent has just used a path, moving it to the most-recent
// end of the queue.
//
// MOST RECENTLY USED, NOT FIRST TOUCHED, and the difference is not academic.
// When the prompt budget cannot hold every file, the ones dropped are the ones
// at the far end — so under first-touch ordering a file the agent re-read ten
// times in a row stayed exactly as droppable as one it had not looked at since
// the first turn. Measured: a developer hit a compile error in a 335-line test
// file, re-read that file fifteen times, and stalled — the file it was asking
// for was the one the budget was discarding, and re-reading it could never bring
// it back. Promotion on use is what makes read_files a way OUT of that.
func (w *Workspace) Seen(path string) {
	for i, p := range w.knownOrder {
		if p == path {
			w.knownOrder = append(w.knownOrder[:i], w.knownOrder[i+1:]...)
			break
		}
	}
	w.knownOrder = append(w.knownOrder, path)
}

// Known lists the paths the agent has read or written, least recently used
// first, skipping any that have since been removed.
func (w *Workspace) Known() []string {
	out := make([]string, 0, len(w.knownOrder))
	for _, p := range w.knownOrder {
		if _, ok := w.files[p]; ok {
			out = append(out, p)
		}
	}
	return out
}

type undoRecord struct {
	path    string
	before  string
	existed bool
}

// NewWorkspace builds a workspace over a snapshot of a repository.
//
// The map is copied. A caller that keeps its own reference to the tree it passed
// would otherwise see edits appear in it, which makes "what did this agent
// change" unanswerable.
func NewWorkspace(files map[string]string, g Guard) *Workspace {
	copied := make(map[string]string, len(files))
	for p, c := range files {
		copied[p] = c
	}
	if g == nil {
		g = AllowAll
	}
	return &Workspace{files: copied, guard: g}
}

// Read returns a file's content and whether it exists.
func (w *Workspace) Read(path string) (string, bool) {
	c, ok := w.files[path]
	return c, ok
}

// Paths lists every file, sorted, so that a listing and a prompt built from one
// are stable between runs. Map order would make two identical runs produce
// different prompts and different caches.
func (w *Workspace) Paths() []string {
	out := make([]string, 0, len(w.files))
	for p := range w.files {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// Files returns a copy of the whole tree, for writing into a sandbox.
func (w *Workspace) Files() map[string]string {
	out := make(map[string]string, len(w.files))
	for p, c := range w.files {
		out[p] = c
	}
	return out
}

// ApplyEdit resolves one edit's address, applies it, and returns a line
// describing what changed.
//
// EVERY RETURNED ERROR IS ADDRESSED TO THE MODEL, not to an operator. It names
// the cause and what to do instead, because a refusal that names only the
// symptom sends a correct agent to the wrong place — the most expensive failure
// class this repository has recorded.
func (w *Workspace) ApplyEdit(e edit.Edit) (string, error) {
	path, err := edit.ValidatePaths([]string{e.Path}, 1)
	if err != nil {
		return "", err
	}
	e.Path = path[0]

	if err := w.guard(e.Path); err != nil {
		return "", err
	}

	if _, ok := e.Address(); !ok {
		return "", fmt.Errorf(
			"%s: say WHERE in exactly one way — a short quote in \"old_str\", OR a name in "+
				"\"decl\", OR \"start_line\"/\"end_line\". You gave more than one, and which you "+
				"meant is not recoverable from the arguments", e.Path)
	}

	// AN IDENTICAL PAIR CHANGES NOTHING, whatever its length. Repeating what was
	// just written is the cheapest continuation for a sampler; carried forward
	// from the measurement in the previous developer loop, where it was 11 of 26
	// refusals in one window. "Your edit changed nothing" does not name the cause;
	// this does.
	if e.OldStr != "" && e.OldStr == e.Replace {
		return "", fmt.Errorf(
			"%s: old_str and replace are IDENTICAL, so this edit would change nothing. old_str is "+
				"the text as it is NOW; replace is what it should BECOME. If you are rewriting a "+
				"whole declaration, name it in \"decl\" and send the new body once in \"replace\" — "+
				"you never have to type the old text twice", e.Path)
	}

	before, existed := w.files[e.Path]

	// A whole-file write over a file that already has content is the silent
	// deletion this addressing scheme exists to prevent: whatever the model did
	// not carry forward is simply gone, and nothing in the reply says so.
	//
	// SOURCE ONLY. The asymmetry that justifies the refusal is a property of
	// code: a declaration dropped in a rewrite does not announce itself, it
	// surfaces a whole verification round later as a compile error somewhere
	// else. Prose has no such delay — a plan missing a section is missing it
	// visibly, to the next reader, and revising a document by rewriting it is the
	// normal way to revise one. Measured: an architect that had written a
	// 424-line plan tried to restructure it, was told to "quote a short snippet"
	// instead, and spent the rest of its budget circling.
	if mode, _ := e.Address(); mode == "whole file" && existed && strings.TrimSpace(before) != "" {
		if isSource(e.Path) {
			return "", fmt.Errorf(
				"%s already exists, so a whole-file write would discard whatever you did not carry "+
					"forward. Address the change instead: quote a short snippet in \"old_str\", or name "+
					"the declaration in \"decl\"", e.Path)
		}
		// A LONG DOCUMENT IS REFUSED FOR COST, NOT FOR SAFETY, and that is why the
		// message says something different. Rewriting prose whole is legitimate —
		// it is how documents are revised — but it regenerates the entire file on
		// every edit. Measured: an architect made eleven whole-file writes to a
		// 363-line plan at a median of 60 seconds each, 705 of the run's 831
		// seconds, and the last of them reproduced the file byte for byte and was
		// refused as a no-op. Reads on the same prompt took ten seconds, so this is
		// output, not context.
		if n := lineCount(before); n > MaxWholeRewriteLines {
			return "", fmt.Errorf(
				"%s is %d lines, and rewriting it whole means generating all %d again for one "+
					"change — which is most of what this stage's time is spent on. Change the part "+
					"you mean: quote the heading or the line you are replacing in \"old_str\", or "+
					"give \"start_line\"/\"end_line\" for a range. A file of %d lines or fewer may "+
					"still be written whole",
				e.Path, n, n, MaxWholeRewriteLines)
		}
	}

	// A WHOLE FILE IS A THIRD POSITION, and it needs its own parse. The two-parse
	// check prepends "package p" to test the text as declarations, so content that
	// already opens with its own package clause gives two of them and fails —
	// which refuses every valid new-file write. The same fix was needed in the
	// python rebuild for the same reason.
	// THE SYNTAX GATES ARE FOR GO AND ONLY FOR GO. Running them on everything
	// meant a heading in a markdown file was rejected as "not valid Go in any
	// position" — so an architect could not edit its own plan in place, and once
	// the plan passed the whole-rewrite threshold it could not replace it either.
	// Boxed in from both sides, it spent its budget circling, and every diagnosis
	// of that stall until now had been of a symptom.
	if isSource(e.Path) {
		if isWholeFile(e) {
			if !parsesAsFile(e.Replace) {
				return "", fmt.Errorf(
					"%s: the content does not parse as a Go file. A whole-file write must be "+
						"complete — a package clause, then the imports, then the declarations", e.Path)
			}
		} else if edit.ReplacementIsMalformed(e.Replace) {
			return "", fmt.Errorf(
				"%s: the replacement is not valid in any position — it does not parse as "+
					"declarations or as statements. Check the braces and quotes in what you sent",
				e.Path)
		}
	}

	span, err := edit.Resolve(before, e)
	if err != nil {
		return "", fmt.Errorf("%s: %w", e.Path, err)
	}

	after, err := edit.Apply(before, span, e.Replace)
	if err != nil {
		return "", fmt.Errorf("%s: %w", e.Path, err)
	}

	if after == before {
		return "", fmt.Errorf(
			"%s: that edit leaves the file exactly as it was, so it cost a turn for nothing. Read "+
				"the file and check that what you sent in \"replace\" differs from what is there",
			e.Path)
	}

	// A duplicate declaration does not compile, and the compiler is a whole
	// verification round away. Refusing at the write turns a lost round trip into
	// a message that names the symbol.
	if strings.HasSuffix(e.Path, ".go") {
		if dups := edit.DuplicateDecls(after); len(dups) > 0 {
			return "", fmt.Errorf(
				"%s: this edit would declare %s more than once, which does not compile. You are "+
					"probably adding a declaration the file already has — read it, then replace the "+
					"existing one by naming it in \"decl\"",
				e.Path, strings.Join(dups, ", "))
		}
		if err := edit.VetLike(e.Path, after); err != nil {
			return "", fmt.Errorf("%s: %w", e.Path, err)
		}
	}

	w.last = &undoRecord{path: e.Path, before: before, existed: existed}
	w.files[e.Path] = edit.WithTrailingNewline(after)
	w.writes++
	w.Seen(e.Path)

	where := fmt.Sprintf("lines %d-%d of", span.From, span.To)
	switch {
	case span.Append:
		where = "appended to"
	case !existed:
		where = "created"
	}
	return fmt.Sprintf("Edited: %s %s (%d lines now).", where, e.Path, lineCount(w.files[e.Path])), nil
}

// Undo reverts the last write, and reports which path it restored.
//
// EXACTLY ONE LEVEL, on purpose. The failure it answers is an edit that left a
// file the model no longer recognises: once the text on disk has diverged from
// what it expects, neither a quote nor a line number finds what it is looking
// for and further edits make it worse. One step back plus a re-read fixes that.
// A deeper history would invite unwinding a whole session, which is a different
// and much rarer need.
func (w *Workspace) Undo() (string, bool) {
	if w.last == nil {
		return "", false
	}
	u := w.last
	w.last = nil
	// The write being reverted was counted when it was applied, and a stage's
	// "produced something" gate reads this count. A write that was fully undone
	// is not a deliverable: without the decrement, write-then-undo-then-stall
	// reported a checkless stage as finished over an empty tree — the exact case
	// the gate was built for.
	if w.writes > 0 {
		w.writes--
	}
	if !u.existed {
		delete(w.files, u.path)
		return u.path, true
	}
	w.files[u.path] = u.before
	return u.path, true
}

// MaxWholeRewriteLines is the largest existing prose file that may be replaced
// wholesale rather than edited in place.
//
// The trade-off flips with size. A short document is cheaper to regenerate than
// to address, and refusing that would be pedantry; a long one costs a full
// regeneration per edit, and at 363 lines that was 60 seconds a turn against 10
// for a read. Set where a rewrite is still a second or two of generation.
const MaxWholeRewriteLines = 150

// isSource reports whether a path holds code, as opposed to prose.
//
// The distinction decides which files are protected from a wholesale rewrite.
// Only Go is listed because Go is the only language this department writes; a
// second one belongs here the day it does, and until then a shorter list is an
// honest one.
func isSource(p string) bool {
	return strings.HasSuffix(p, ".go")
}

// isWholeFile reports whether an edit addresses the file as a whole rather than
// a span within it.
func isWholeFile(e edit.Edit) bool {
	mode, _ := e.Address()
	return mode == "whole file"
}

// parsesAsFile reports whether the text is a complete, syntactically valid Go
// file. Empty content is allowed: deleting a file's contents is a legitimate
// edit and is not this check's business.
func parsesAsFile(src string) bool {
	if strings.TrimSpace(src) == "" {
		return true
	}
	_, err := parser.ParseFile(token.NewFileSet(), "x.go", src, parser.SkipObjectResolution)
	return err == nil
}

func lineCount(s string) int {
	if s == "" {
		return 0
	}
	return len(strings.Split(strings.TrimSuffix(s, "\n"), "\n"))
}
