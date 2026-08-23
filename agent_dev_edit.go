// Applying an edit to a file, and deciding what an edit even points AT.
//
// Split out of agent_dev.go, which had grown past five thousand lines. This half
// is the part that changed most often: every addressing scheme this loop has used
// — search text, line ranges, a short anchor, a declaration name — was replaced
// after a run showed the model could not drive it, and the reasons are recorded
// on each function rather than in a changelog nobody reads.

package main

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"path"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
)

type devFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// devEdit is one search/replace against one file — the edit format aider
// settled on, with typed fields instead of text delimiters.
//
// The delimiters are the part deliberately NOT copied. Aider marks its blocks
// with <<<<<<< SEARCH / ======= / >>>>>>> REPLACE because it parses free text;
// here the arguments arrive as a typed tool call, so the fields are already
// separate and a marker would only be one more thing for a model to malform.
// Every delimiter this department has used has eventually turned up inside the
// content it was supposed to delimit.
// devEdit names WHERE to change a file by line, not by quoting its text back.
//
// Search-and-replace asked the model to reproduce existing text byte for byte,
// and that turned out to be the single thing it was worst at: search misses were
// the last surviving failure class, with an agent unable to reproduce text it
// could see on screen. Line numbers cannot be got subtly wrong — they either name
// a range that exists or they do not — and the model already reasons in them
// unprompted ("line 185 where there's a := operator").
type devEdit struct {
	Path string `json:"path"`
	// OldStr addresses an edit BY ITS TEXT, and is the preferred form.
	//
	// Line arithmetic is the thing this model cannot do. Measured on r69: one
	// ticket, 77 refusals, 41 of them a range that cut across a function and 9 an
	// end_line before the start, while its diagnosis of the actual bug never
	// changed. Addressing by text removes the arithmetic entirely — the agent
	// quotes what it can see rather than counting to it.
	//
	// This is also where the field evidence points. Reproducing Qwen3.6-27B on
	// SWE-bench Pro, a bash-only agent scores ~28% pass@1 and the same agent with
	// SWE-agent's str_replace_editor — exact string matching, no line numbers —
	// scores ~50.7%. Qwen-Code's own edit/write_file tools measured zero lift.
	//
	// MUST BE UNIQUE in the file. Ambiguity is the failure mode of text addressing
	// as arithmetic is of line addressing, so a match count of anything but one is
	// refused with the candidates named, and the agent widens its quote.
	OldStr string `json:"old_str,omitempty"`
	// Decl addresses a whole top-level declaration by name — "main",
	// "apiTasksHandler", "(*Store).Add". Immune to line drift and to a file the
	// agent has already broken, and it matches how the model reasons: r69's
	// developer named apiTasksHandler, apiTaskHandler and main in its prose on
	// every turn while failing to name their lines.
	Decl string `json:"decl,omitempty"`
	// StartLine and EndLine are 1-indexed and INCLUSIVE. Zero means the whole
	// file: a create, or a deliberate wholesale rewrite where that is permitted.
	//
	// DEMOTED TO A DISAMBIGUATOR. Kept because a range is the one unambiguous way
	// to name a repeated line, which is the case text addressing cannot express —
	// the same conclusion SWE-agent reached when it added an optional range to
	// str_replace rather than replacing it.
	StartLine int `json:"start_line"`
	EndLine   int `json:"end_line"`
	// Replace is what takes the range's place. Empty deletes those lines, which is
	// legitimate — it is why this is not guarded the way a whole-file blank is.
	Replace string `json:"replace"`
	// isAppend marks an edit that ADDS to the end of the file rather than
	// replacing part of it. Internal: the model never sets it, the decl resolver
	// does when the declaration named does not exist yet.
	//
	// A flag rather than a line range because "one past the end" collides with
	// three separate guards — the inverted-range check, the transposition repair
	// and the past-the-end check — each of which is right about ordinary edits and
	// wrong about this one.
	isAppend bool `json:"-"`
}

// stackFrameFile matches the indented "file:line" line of a Go panic frame.
var stackFrameFile = regexp.MustCompile(`(?m)^\s+(/?[^\s:]+\.go):\d+`)

// noopEditNotice chooses what to tell an agent whose edit changed nothing.
//
// IT MUST NOT NAME A TOOL THAT NO LONGER EXISTS. The first version of this told
// the agent to "STOP EDITING AND RUN THE TESTS", which was good advice until
// run_tests was removed from the vocabulary — after which it was an instruction
// the agent could not follow. Measured on the dev fixture: 28 no-op refusals in a
// row, each one telling a spec author to call something that was not there.
//
// The advice now turns on the only thing the agent can still do — write — and on
// whether this tree already has a verdict.
func noopEditNotice(noopEdits int, verified, testsPass bool, lastTest string) string {
	switch {
	case verified && testsPass:
		// Done and proved. The stage ends on its own; nothing is left to write.
		return "Rejected: your edits changed nothing — the file already contains exactly what you " +
			"wrote, and the checks have already PASSED against this tree. There is nothing left to do; " +
			"this ticket is finished and will be handed on."
	case verified:
		// Tested and failing. Rewriting the same text cannot change that.
		return fmt.Sprintf(
			"Rejected: your edits changed nothing — the file already contains exactly what you wrote, "+
				"and the checks have already been run against this exact tree: they FAILED.\n\nWriting "+
				"the same text again cannot help. You must change something DIFFERENT — a different line "+
				"range, or different content. The failure was:\n\n%s", clip(lastTest, 1200))
	case noopEdits >= 2:
		return fmt.Sprintf(
			"Rejected: that is the %d%s edit in a row that changed nothing — the file already contains "+
				"exactly what you wrote, so nothing was checked and nothing can be.\n\nThe checks run "+
				"BY THEMSELVES on any edit that actually changes the file. Make a real change: read the "+
				"numbered contents again, pick the line range you actually mean, and write something "+
				"different from what is already there.",
			noopEdits, ordinalSuffix(noopEdits))
	default:
		return "Rejected: your edits produced no change — the file already contained exactly what you " +
			"wrote. An earlier attempt at this ticket may already have written it. The checks run by " +
			"themselves on any edit that actually changes the file, so make a real change or leave it " +
			"alone."
	}
}

// isTestFile reports whether a path is a Go test file.
//
// The suffix is the whole rule because it is also the compiler's rule: Go builds
// *_test.go only under `go test`, so this exactly separates "code that ships"
// from "code that checks it". A language with a different convention would need
// this to be configuration; today it would be configuration with one possible
// value.
func isTestFile(p string) bool { return strings.HasSuffix(path.Clean(p), "_test.go") }

// applyEdits validates and records proposed search/replace edits.
//
// SEARCH/REPLACE, NOT WHOLE FILES, and the difference is the reason this
// function exists in this shape. A whole-file write makes "add a struct" and
// "replace the file with a struct" the same action, so a model that forgets to
// carry the rest forward deletes it silently — measured, and it removed an entire
// HTTP server from a branch whose tests still passed because the only tests
// written covered the struct. An edit that names the text it is replacing cannot
// do that: what it does not mention, it does not touch.
//
// It also fails LOUDLY when the file is not what the model believed. A search
// that matches nothing is a wrong assumption caught before it is written, which
// is worth more than a write that succeeds against the wrong content.
//
// The mode check is the developer/author split: a developer that can edit tests
// can make a failing test pass by weakening it, and an author that can edit the
// implementation can make its own test pass by changing the code under it.
// Either way the branch ends up certified against itself.
func applyEdits(s *devState, edits []devEdit, mode agentMode) error {
	if len(edits) == 0 {
		return errors.New("write_files with no edits")
	}
	if len(edits) > maxWriteFiles {
		return fmt.Errorf("write_files with %d edits, limit is %d", len(edits), maxWriteFiles)
	}
	paths := make([]string, len(edits))
	for i, e := range edits {
		paths[i] = e.Path
	}
	if _, err := validatePaths(paths, maxWriteFiles); err != nil {
		return err
	}

	// Everything is validated against a COPY and only committed once every edit
	// in the batch has succeeded. A batch that half-applies leaves the tree in a
	// state neither the model nor the prompt describes, and the next turn would
	// be reasoning about a file that is partly edited and partly not.
	staged := map[string]string{}
	for k, v := range s.staged {
		staged[k] = v
	}
	// Only the files this batch wrote are syntax-checked. A file that arrived
	// broken from the repository is not this edit's fault, and refusing a write
	// because of it would trap the agent on someone else's mistake.
	touched := map[string]string{}
	// What each ranged edit CUT: the file as it was, and the lines asked for. Kept
	// so a syntax failure can name the declaration the range crossed instead of
	// reporting only where the braces stopped balancing. See enclosingDecl.
	cut := map[string]struct {
		before   string
		from, to int
		replace  string
	}{}

	// BOTTOM-UP, because a line range names a position in the file as the model
	// saw it. Applying an edit at line 10 shifts everything below it, so a later
	// edit at line 90 would land somewhere the model never looked. Working from
	// the bottom means every range is still valid when its turn comes.
	//
	// Mixing a whole-file replacement with line edits for the same file is refused
	// rather than ordered: the two describe different files, and picking a winner
	// would silently discard one of them.
	ordered := make([]devEdit, len(edits))
	copy(ordered, edits)

	// TEXT AND DECLARATION ADDRESSES ARE RESOLVED TO RANGES FIRST, so everything
	// downstream — ordering, the whole-file check, the syntax report — keeps
	// working on one representation. The agent chooses how to point at the code;
	// the harness converts, and reports the conversion failure in the terms the
	// agent used rather than in line numbers it never supplied.
	for i := range ordered {
		e := &ordered[i]
		if e.OldStr == "" && e.Decl == "" {
			continue
		}

		cp := path.Clean(e.Path)
		before, have := staged[cp]
		if !have {
			before, have = s.read[cp]
		}
		if !have {
			return fmt.Errorf("%s has not been read, so its text cannot be matched. read_files it first", cp)
		}
		// IDENTICAL FIELDS ARE A TRANSCRIPTION ARTEFACT, not an intention. The schema
		// emits properties alphabetically, so a long old_str is followed a field later
		// by replace, and the model copies rather than composes — 9 of 15 edits in the
		// first window under this format sent the two identical, which is a no-op by
		// construction. "Your edits changed nothing" does not name that; this does.
		// AN IDENTICAL PAIR CHANGES NOTHING, whatever its length. The schema emits
		// properties alphabetically, so a long old_str is followed a field later by
		// replace, and repeating what was just written is the cheapest continuation —
		// 11 of 26 refusals in one window were exactly that.
		if e.OldStr != "" && e.OldStr == e.Replace {
			return fmt.Errorf("%s: old_str and replace are IDENTICAL, so this edit would change "+
				"nothing. old_str is the text as it is NOW; replace is what it should BECOME. If you "+
				"are rewriting a whole function, name it in \"decl\" and send the new body once in "+
				"\"replace\" — you never have to type the old text twice", path.Clean(e.Path))
		}
		// Both filled is not a mistake the agent can avoid: every field is required by
		// the schema, so it emits all of them. old_str is the more specific address
		// and wins, with decl kept as the fallback below.
		fallbackDecl := e.Decl
		if e.OldStr != "" {
			e.Decl = ""
		}

		var from, to int
		var err error
		switch {
		case e.OldStr != "":
			// LENGTH IS ONLY A PROBLEM WHEN THE QUOTE FAILS. Capping it up front was
			// aimed at the developer copying long spans, and it blocked the
			// SPECIFICATION AUTHOR instead — a test function is twenty-odd lines by
			// nature, and five consecutive refusals told it its perfectly matchable
			// quote was too long. So the quote is tried first, and its size is only
			// mentioned if it did not resolve.
			from, to, err = resolveOldStr(before, e.OldStr)
			if err != nil {
				switch {
				case fallbackDecl != "":
					// A quote that missed alongside a declaration name is a correct decl
					// address with a redundant quote attached.
					from, to, err = resolveDecl(before, fallbackDecl)
				case declCount(e.OldStr) == 1:
					// DERIVE IT FROM THE QUOTE, but only when the span is exactly one
					// declaration. A span covering several is a multi-declaration edit, and
					// naming it after its first line replaces one while inserting all of
					// them — that put main, store and apiTasksHandler into main.go twice.
					if name := declNameFromSource(e.OldStr); name != "" {
						from, to, err = resolveDecl(before, name)
					}
				case len(strings.Split(strings.TrimSuffix(e.OldStr, "\n"), "\n")) > maxOldStrLines:
					err = fmt.Errorf("%w\n\nIt is also %d lines. A quote is an ANCHOR — a short unique "+
						"snippet is far easier to match than a whole function, and \"replace\" may be as "+
						"long as you like. To replace a whole declaration, name it in \"decl\" instead",
						err, len(strings.Split(strings.TrimSuffix(e.OldStr, "\n"), "\n")))
				}
			}
		case e.Decl != "":
			// "REPLACE THIS FILE" ARRIVES AS A DECL NAMED AFTER THE FILE. The spec
			// author rewrites whole test files by nature, and the branched schema left
			// it no obvious whole-file route — so it put the filename in decl and the
			// entire file in replace. Measured: 18 refusals of `no top-level
			// declaration named "handlers_read_test.go"` in five minutes.
			//
			// Converted to the explicit full range rather than refused, which leaves the
			// DECISION where it already lives: a spec author may rewrite its own tests,
			// and a developer may not blindly overwrite a file the sections share. This
			// only translates the request; the existing guards still answer it.
			if strings.HasPrefix(strings.TrimSpace(e.Replace), "package ") ||
				e.Decl == cp || e.Decl == path.Base(cp) {
				lines := strings.Split(strings.TrimSuffix(before, "\n"), "\n")
				from, to, err = 1, len(lines), nil
				break
			}
			from, to, err = resolveDecl(before, e.Decl)
			if err != nil && declCount(e.Replace) >= 1 {
				// A DECLARATION THAT IS NOT THERE YET IS ONE TO ADD. The decl shape could
				// only ever REPLACE, which leaves test-first work — whose entire job is
				// writing functions that do not exist — with no natural move. Measured:
				// 55 refusals of "no top-level declaration named GETApiTasksHandler"
				// against a file that was supposed to gain exactly that.
				//
				// Appended rather than refused, and only when the replacement really is
				// one or more declarations: an append of a stray fragment would break the
				// file, and the duplicate gate below still catches a name that already
				// exists elsewhere.
				lines := strings.Split(strings.TrimSuffix(before, "\n"), "\n")
				from, to, err = len(lines)+1, len(lines), nil
				e.isAppend = true
				e.Replace = "\n" + strings.TrimLeft(e.Replace, "\n")
			}
		}
		if err != nil {
			return fmt.Errorf("%s: %w", cp, err)
		}
		e.StartLine, e.EndLine = from, to
	}

	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].StartLine > ordered[j].StartLine })
	whole := map[string]bool{}
	ranged := map[string]bool{}
	for _, e := range ordered {
		if e.StartLine == 0 {
			whole[path.Clean(e.Path)] = true
		} else {
			ranged[path.Clean(e.Path)] = true
		}
	}
	for p := range whole {
		if ranged[p] {
			return fmt.Errorf("%s has both a whole-file replacement and a line-range edit in the same "+
				"call, which describe two different files. Send one or the other", p)
		}
	}

	for _, e := range ordered {
		p := path.Clean(e.Path)
		switch {
		case mode == modeCoverage && !isTestFile(p):
			return fmt.Errorf("%s is not a test file. You may only ADD tests — the implementation is "+
				"finished and reviewed work, and changing it to make a test pass is not covering it", p)
		case mode == modeCoverage && slices.Contains(s.tree, p):
			// THE SPEC IS NOT YOURS TO EDIT. The tests written before the code are
			// what the developer was held to, and the cheapest way to raise a
			// coverage number is to weaken one of them — which would hit the target
			// while destroying the thing the target is a proxy for. New files only.
			return fmt.Errorf("%s already existed before this stage. Those tests are the specification the "+
				"developer was held to and must not be edited. Put your new tests in a new file, "+
				"for example coverage_test.go", p)
		case mode == modeSpecMerge && !isTestFile(p):
			return fmt.Errorf("%s is not a test file. You are reconciling the tests several authors "+
				"wrote onto this branch; the implementation is not yours to change", p)
		case mode == modeTest && !isTestFile(p):
			return fmt.Errorf("%s is not a test file. You may only edit *_test.go — "+
				"the implementation is another agent's work, and changing it to make your test pass "+
				"defeats the point of writing the test. If the code is wrong, write the test that "+
				"proves it and let it fail", p)
		case mode == modeDevelop && isTestFile(p):
			return fmt.Errorf("%s is a test file, and tests are written by a separate agent against "+
				"the ticket. Edit the implementation; do not edit tests", p)
		case mode == modeDevelop && isDocFile(p):
			// THE DOCUMENTATION IS THE ARCHITECT'S, and it is already on the base
			// branch when this agent's sandbox clones. It describes the WHOLE
			// system — the names and interfaces every other ticket is being built
			// against — so a developer editing it to match its own half is not
			// documenting anything, it is quietly rewriting five other agents'
			// specification.
			//
			// Belt and braces: the product manager no longer emits documentation
			// subtasks (see notCodeWork), so a developer should never be pointed at
			// one. This is the guard for when that fails anyway, and it is cheap —
			// the same situation unguarded cost 642 seconds of a large-class slot
			// and left the developer a gate it could not pass.
			return fmt.Errorf("%s is documentation. It is written once by the architect before the work "+
				"is broken down, and it is the specification the OTHER tickets are being built against — "+
				"not this ticket's to change. Edit the implementation", p)
		}

		before, have := staged[p]
		if !have {
			before, have = s.read[p]
		}

		if e.StartLine == 0 {
			// A create, or a deliberate rewrite of something already READ.
			//
			// The guard here was "it must not already exist", which is right for a
			// BLIND overwrite — silently discarding contents the model never saw —
			// and wrong once it has read them. They are in its prompt; replacing
			// them wholesale is no less informed than a search/replace, and it is
			// the only move available when an earlier attempt left a file on the
			// branch that this attempt cannot match exactly.
			//
			// Measured: a section author retried onto a branch already holding its
			// own file, could not reproduce the search text byte for byte, and had
			// no legal way to rewrite it — 21 turns alternating a refused edit with
			// a refused verification.
			// The exception is NARROW: a spec author rewriting a test file it has
			// read. That file is its own output — an earlier attempt of this same
			// ticket wrote it — and replacing it wholesale loses nothing that
			// belongs to anyone else. A developer replacing main.go is the failure
			// this guard exists for and stays refused.
			_, seen := s.read[p]
			ownTestFile := mode == modeTest && isTestFile(p) && seen
			// NARROW ON PURPOSE, and it was briefly widened to "any file this agent has
			// read in full" before the tests said no. They were right: sections of one
			// task share a branch, so a developer rewriting main.go wholesale can drop
			// a concurrent section's work between its read and its write. A spec author
			// rewriting its own test file cannot — that file is its own output.
			//
			// The search-miss problem this was meant to solve is a DISCOVERABILITY
			// problem, not a permissions one. The escape already existed for the agent
			// that was failing; it simply did not know about it, which the prompt now
			// fixes up front rather than only in a refusal.
			if have && !ownTestFile && strings.TrimSpace(before) != "" {
				// SAY HOW TO DO WHAT IT IS TRYING TO DO. Replacing a file wholesale is a
				// legitimate thing to want — main.go arrives as a six-line stub and
				// writing the implementation into it is not a two-line edit — and a
				// line range expresses it exactly. Refusing without naming that cost
				// r65: 78 refusals, all this message, the developer asking for the one
				// thing it was never told it already had.
				return fmt.Errorf("%s already exists, and omitting start_line and end_line replaces the "+
					"WHOLE file — including anything another section of this task has written to the same "+
					"branch.\n\nTo replace ALL of it deliberately, say so with a range: start_line 1 and "+
					"end_line %d (it has %d lines). To change part of it, take the range from the numbered "+
					"contents shown to you.",
					p, len(strings.Split(strings.TrimSuffix(before, "\n"), "\n")),
					len(strings.Split(strings.TrimSuffix(before, "\n"), "\n")))
			}
			if !have && slices.Contains(s.tree, p) && !s.missing[p] {
				return fmt.Errorf("%s already exists and you have not read it. read_files it first, "+
					"then give the exact text to replace", p)
			}
			if strings.TrimSpace(e.Replace) == "" {
				return fmt.Errorf("%s has an empty search and an empty replace, so it would create an "+
					"empty file. Put the file's text in \"replace\"", p)
			}
			staged[p] = withTrailingNewline(e.Replace)
			touched[p] = staged[p]
			continue
		}

		if !have {
			if slices.Contains(s.tree, p) && !s.missing[p] {
				return fmt.Errorf("%s has not been read, so you cannot know what its lines are. "+
					"read_files it first", p)
			}
			return fmt.Errorf("%s does not exist, so it has no lines to replace. To create it, omit "+
				"start_line and end_line and put the whole file in \"replace\"", p)
		}

		lines := strings.Split(strings.TrimSuffix(before, "\n"), "\n")
		if e.EndLine == 0 {
			e.EndLine = e.StartLine
		}
		// AN END EXACTLY ONE BEFORE THE START IS A TRANSPOSITION, and it is read as
		// the single line at start_line rather than refused. All 9 of r69's inverted
		// ranges were this shape — "start_line 88, end_line 87", "start_line 86,
		// end_line 85" — and each cost a turn to a message that only restated the
		// rule.
		//
		// NARROW ON PURPOSE. Swapping ANY inversion was the first version and it is
		// unsafe: "start_line 3, end_line 1" would then replace three lines from the
		// top of the file, including the package clause, on a guess about what was
		// meant. A wider inversion has no single reading, so it stays refused.
		if !e.isAppend && e.EndLine == e.StartLine-1 && e.StartLine >= 1 {
			e.EndLine = e.StartLine
		}
		if !e.isAppend && (e.StartLine < 1 || e.EndLine < e.StartLine) {
			return fmt.Errorf("start_line %d, end_line %d is not a range. Lines are numbered from 1 "+
				"and end_line must not be before start_line", e.StartLine, e.EndLine)
		}
		// AN END PAST THE LAST LINE CAN ONLY MEAN "TO THE END", so it is clamped
		// rather than refused. Measured on the implementation fixture: 7 refusals,
		// every one of them start_line 1 with an end a few lines past a 180-line
		// file — 182, 184, 185, 191. The agent was asking to replace the whole file
		// and missing the final line number by a rounding error, and refusing taught
		// it nothing it could act on.
		//
		// A start past the end is different and stays refused: there is no sensible
		// reading of "begin after the file ends" — EXCEPT exactly one line past it,
		// which is an append, and is how a decl address adds a declaration the file
		// does not have yet. See the resolveDecl fallback.
		if !e.isAppend && e.StartLine > len(lines) {
			return fmt.Errorf("%s has %d lines, so line %d is past its end. The contents shown to you "+
				"are numbered — take the range from those", p, len(lines), e.StartLine)
		}
		if e.EndLine > len(lines) {
			e.EndLine = len(lines)
		}

		var out []string
		out = append(out, lines[:e.StartLine-1]...)
		if e.Replace != "" {
			out = append(out, strings.Split(strings.TrimSuffix(e.Replace, "\n"), "\n")...)
		}
		out = append(out, lines[e.EndLine:]...)
		after := strings.Join(out, "\n")
		if len(after) > maxFileBytes {
			return fmt.Errorf("%s would be %d bytes, limit is %d", p, len(after), maxFileBytes)
		}
		staged[p] = withTrailingNewline(after)
		touched[p] = staged[p]
		cut[p] = struct {
			before   string
			from, to int
			replace  string
		}{before, e.StartLine, e.EndLine, e.Replace}
	}

	// THE HARNESS MAKES UP FOR THE MODEL WHERE IT CAN. A write that leaves a Go
	// file unparseable is repaired if the repair is unambiguous, and refused only
	// when it is not — because the alternative costs a turn to say something the
	// harness already knows, and the agent may not recover at all. Measured: a
	// developer wrote store.go with every struct tag missing its closing backtick,
	// and then spent 34 consecutive actions failing to search/replace its way out,
	// because search text never matches a file the model has already broken.
	for p, c := range touched {
		repaired, fixed, err := repairGoSource(p, c)
		if err != nil {
			// SHOW WHAT IT PRODUCED, not just the error. A line range makes it easy to
			// cut across a brace boundary — the format removed "cannot find the text"
			// and introduced "cut in the wrong place" — and the compiler's line number
			// refers to a file the agent has never seen: the POST-edit one. Naming a
			// line it cannot look at is the same mistake as pointing at a tool it does
			// not have. Measured on the implementation fixture: 19 refusals across two
			// break points, none of them showing the damage.
			// LEAD WITH THE CAUSE, NOT THE SYMPTOM. "Not valid Go" reads as "your code
			// is wrong", so the agent re-derives an analysis that was right the first
			// time — r69 spent 41 refusals doing exactly that while its diagnosis of
			// the routing bug never changed. The replacement text is usually fine; the
			// RANGE crossed a declaration and stranded it at file scope.
			if k, okCut := cut[p]; okCut {
				// THE TEXT IS ASKED FIRST. Blaming the range for a broken literal sends
				// the agent to re-cut a range that was already right — measured directly
				// on the retest, where it was told "lines 230-241 cut across func main,
				// which spans lines 230-241" for a `rune literal not terminated`.
				if replacementIsMalformed(k.replace) {
					return fmt.Errorf("Your LINE RANGE is fine — the replacement TEXT does not parse: %v.\n\n"+
						"This is a fault in what you wrote, not in where you put it, so re-sending the same "+
						"text with a different range cannot help. Look for an unterminated string or rune "+
						"literal, an unbalanced brace, or a stray quote in the text you supplied.", err)
				}
				// AND THE RANGE IS ONLY BLAMED WHEN IT DIFFERS from the declaration it
				// touches. When they are identical the agent already sent the whole thing
				// and repeating the advice is a loop.
				if name, from, to, okDecl := enclosingDecl(k.before, k.from, k.to); okDecl &&
					(from != k.from || to != k.to) {
					return fmt.Errorf("Your replacement text is probably fine — the LINE RANGE is what "+
						"broke it. Lines %d-%d cut across %s, which spans lines %d-%d, so what you wrote "+
						"was left outside any function and %s is no longer valid Go (%v).\n\nRe-issue the "+
						"same change with start_line %d and end_line %d, giving the WHOLE of %s in "+
						"\"replace\". Do not re-analyse the code — your diagnosis was not the problem.",
						k.from, k.to, name, from, to, p, err, from, to, name)
				}
			}
			return fmt.Errorf("%s is not valid Go after this edit: %v.\n\nThis is the file AS YOUR EDIT "+
				"LEFT IT, around the problem — the braces will not balance:\n\n%s\n\nEither give a range "+
				"that covers whole declarations, or rewrite the file with start_line 1 and end_line %d.",
				p, err, windowAround(c, err.Error()),
				len(strings.Split(strings.TrimSuffix(c, "\n"), "\n")))
		}
		if fixed {
			staged[p] = repaired
			s.repairs = append(s.repairs, p)
		}
		// CAUGHT ON THE TURN IT HAPPENS. A declaration inserted while its original is
		// still present compiles to "redeclared in this block" — reported later, from
		// a line number in a file the agent has not seen, and tedious to unpick from
		// the inside. Naming the symbol here stops the edit instead.
		if dups := duplicateDecls(staged[p]); len(dups) > 0 {
			return fmt.Errorf("%s would declare %s twice. Your replacement adds a declaration whose "+
				"original is still in the file — replace the existing one in place (name it in \"decl\") "+
				"rather than adding a second copy, or remove the old one in the same edit",
				p, strings.Join(dups, " and "))
		}
	}

	// SNAPSHOT BEFORE COMMITTING, so the previous state survives the write that
	// is about to replace it. Bounded, because the useful undo is the last one or
	// two — an agent that needs to walk back ten edits has lost the thread and the
	// iteration budget is the thing that should stop it.
	prev := make(map[string]string, len(s.staged))
	for k, v := range s.staged {
		prev[k] = v
	}
	s.undoStack = append(s.undoStack, prev)
	if len(s.undoStack) > maxUndoDepth {
		s.undoStack = append([]map[string]string(nil), s.undoStack[len(s.undoStack)-maxUndoDepth:]...)
	}

	for p, c := range staged {
		s.staged[p] = c
		s.read[p] = c // the agent sees its own edit next turn
	}
	return nil
}

// withTrailingNewline ensures a written file ends with one.
//
// Models routinely emit a final line with no newline after it, and these are
// WHOLE-FILE writes, so whatever comes back is the file. The result is a diff
// carrying "\ No newline at end of file" on every file the agent touches, which
// gofmt flags, POSIX says is not a text file, and reviewers read as carelessness
// — noise on every change, obscuring the change itself.
//
// Empty content is left alone: a file the agent deliberately emptied should stay
// empty rather than gain a blank line.
func withTrailingNewline(content string) string {
	if content == "" || strings.HasSuffix(content, "\n") {
		return content
	}
	return content + "\n"
}

// validatePaths refuses anything that could reach outside the checkout.
//
// The model supplies these strings, so this is a trust boundary, not a tidiness
// check: "src/main.go" and "../../etc/cron.d/x" differ only in content.
func validatePaths(paths []string, max int) ([]string, error) {
	if len(paths) == 0 {
		return nil, errors.New("no paths given")
	}
	if len(paths) > max {
		return nil, fmt.Errorf("%d paths, limit is %d", len(paths), max)
	}
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		p = strings.TrimSpace(p)
		switch {
		case p == "":
			return nil, errors.New("empty path")
		case strings.HasPrefix(p, "/"):
			return nil, fmt.Errorf("absolute path not allowed: %q", p)
		case strings.Contains(p, "\x00"):
			return nil, fmt.Errorf("path contains a null byte: %q", p)
		}
		clean := path.Clean(p)
		if clean == ".." || strings.HasPrefix(clean, "../") {
			return nil, fmt.Errorf("path escapes the repository: %q", p)
		}
		out = append(out, clean)
	}
	return out, nil
}

// stubbedTests names the Test functions in the staged files whose bodies are
// empty.
//
// Parsed rather than pattern-matched: a comment inside a body is not a
// statement, which is exactly the distinction being drawn, and go/parser draws
// it for free. A file that does not parse returns nothing — that is the syntax
// gate's business one step later, and reporting it twice would send the author
// two different complaints about one mistake.
func stubbedTests(staged map[string]string) []string {
	var empty []string
	for _, p := range sortedKeys(staged) {
		if !isTestFile(p) {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), p, staged[p], parser.SkipObjectResolution)
		if err != nil {
			continue
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Body == nil {
				continue
			}
			if !strings.HasPrefix(fn.Name.Name, "Test") {
				continue
			}
			if isStubBody(fn.Body) {
				empty = append(empty, fn.Name.Name)
				continue
			}
			empty = append(empty, emptySubtests(fn)...)
		}
	}
	return empty
}

// windowAround renders the lines surrounding a compile error, numbered, so the
// agent can see the break it made rather than being told a line number in a file
// it was never shown.
func windowAround(content, errText string) string {
	lines := strings.Split(strings.TrimSuffix(content, "\n"), "\n")
	at := 0
	if m := regexp.MustCompile(`:(\d+):\d+:`).FindStringSubmatch(errText); m != nil {
		if n, e := strconv.Atoi(m[1]); e == nil {
			at = n
		}
	}
	// Clamp to the file BEFORE windowing. A compile error can name a line past the
	// end — "expected declaration" at the point where a truncated file simply
	// stops — and an unclamped window then starts after it ends and renders
	// nothing, which is worse than the message it replaced.
	if at == 0 || at > len(lines) {
		at = len(lines)
	}
	lo, hi := at-8, at+4
	if lo < 1 {
		lo = 1
	}
	if hi > len(lines) {
		hi = len(lines)
	}
	var b strings.Builder
	for i := lo; i <= hi; i++ {
		marker := "  "
		if i == at {
			marker = ">>"
		}
		fmt.Fprintf(&b, "%s %d\t%s\n", marker, i, lines[i-1])
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// repairGoSource parses a Go file and, if it does not parse, repairs the one
// mistake this model reliably makes before giving up on it.
//
// THE MISSING CLOSING BACKTICK ON A STRUCT TAG. Observed on a live ticket, eight
// fields in one struct, all identical:
//
//	ID    string `json:"id"
//
// It is the model's own output, not the harness losing anything: the same model
// through the same code path emitted the closing backtick correctly on an
// earlier run, and nothing in blacksmith touches backticks. It is simply a
// sequence this model drops sometimes, and no prompt reliably prevents it.
//
// REPAIRING IS SAFE BECAUSE THE RESULT IS VERIFIED, and that is the whole design.
// A Go raw string may legitimately span lines, so a lone backtick is not always a
// missing terminator — the repair is therefore a GUESS, and it is accepted only
// if the file parses afterwards. A wrong guess produces something that does not
// parse and is discarded, leaving the original error to be reported. The same
// discipline decodeModelJSON uses on control characters: repair what is
// unambiguous, verify, and never let a guess through unchecked.
//
// Returns the content to use, whether a repair was applied, and an error only
// when the file cannot be made to parse at all.
func repairGoSource(p, src string) (string, bool, error) {
	if !strings.HasSuffix(p, ".go") {
		return src, false, nil
	}
	if _, err := parser.ParseFile(token.NewFileSet(), p, src, parser.SkipObjectResolution); err == nil {
		return src, false, nil
	}

	lines := strings.Split(src, "\n")
	changed := false
	for i, line := range lines {
		// An even number of backticks on the line is balanced, whatever else is
		// wrong with it.
		if strings.Count(line, "`")%2 == 0 {
			continue
		}
		if loc := unterminatedTag.FindStringIndex(line); loc != nil && loc[1] == len(line) {
			lines[i] = line + "`"
			changed = true
		}
	}
	if !changed {
		_, err := parser.ParseFile(token.NewFileSet(), p, src, parser.SkipObjectResolution)
		return src, false, err
	}

	repaired := strings.Join(lines, "\n")
	if _, err := parser.ParseFile(token.NewFileSet(), p, repaired, parser.SkipObjectResolution); err != nil {
		// The guess was wrong. Report the ORIGINAL error, because an error quoting
		// a line the model never wrote is a worse clue than one quoting its own.
		_, orig := parser.ParseFile(token.NewFileSet(), p, src, parser.SkipObjectResolution)
		return src, false, orig
	}
	return repaired, true, nil
}

// enclosingDecl names the top-level declaration a line range falls inside, and
// the range that would cover the whole of it.
//
// THE AGENT IS NOT GETTING THE CODE WRONG, IT IS GETTING THE ARITHMETIC WRONG.
// Read from r69's stall: across 77 refusals the developer's diagnosis was correct
// and unchanging — Go's default mux prefix-matching "/api/tasks/" so it swallows
// "/api/tasks", a handler returning 200 where the test wants 404, duplicate route
// registrations. It knew all of that on the first turn. What it could not do was
// name the lines: 41 of those refusals were a range that cut across a function so
// the replacement landed at file scope, and 9 were an end_line before the start.
//
// And the refusal it got said "main.go is not valid Go after this edit", so it
// concluded its CODE was wrong and re-derived the same correct analysis, turn
// after turn. The message described the symptom at file scope and never named the
// cause, which is the same fault as the misleading toolchain errors that
// explainConfusingFailure exists to translate.
//
// So this answers the question the agent actually needs answered: which
// declaration did I cut, and what range covers it? Handing back real numbers ends
// the arithmetic, where "give a range that covers whole declarations" only
// restates the requirement it is already failing to meet.
func enclosingDecl(src string, startLine, endLine int) (name string, from, to int, ok bool) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "src.go", src, parser.SkipObjectResolution)
	if err != nil {
		return "", 0, 0, false // pre-edit source is already broken; not this check's business
	}
	for _, d := range f.Decls {
		a, b := fset.Position(d.Pos()).Line, fset.Position(d.End()).Line
		// Overlapping is the case that matters, not containment: a range that
		// starts inside one declaration and ends inside the next is precisely the
		// edit that leaves statements stranded between them.
		if startLine <= b && endLine >= a {
			switch t := d.(type) {
			case *ast.FuncDecl:
				n := t.Name.Name
				if t.Recv != nil && len(t.Recv.List) > 0 {
					n = "method " + n
				} else {
					n = "func " + n
				}
				return n, a, b, true
			case *ast.GenDecl:
				return t.Tok.String() + " declaration", a, b, true
			}
		}
	}
	return "", 0, 0, false
}

// replacementIsMalformed reports whether the text the agent supplied is itself
// broken, as opposed to having been put in the wrong place.
//
// THE TWO FAULTS NEED OPPOSITE ADVICE and telling them apart by guessing is how
// the first version of this went wrong. It assumed any post-edit syntax error
// meant the RANGE had cut a declaration, and told the agent its replacement was
// probably fine — while the actual error was `rune literal not terminated`, a
// broken literal in the text itself. Nine refusals of confidently wrong advice,
// including "re-issue with start_line 230 and end_line 241" when 230-241 was
// exactly what it had already sent.
//
// So the text is asked directly. A replacement meant as whole declarations must
// parse as a file; one meant as statements must parse inside a function. If
// NEITHER holds, the fault is lexical — an unterminated literal, a stray quote —
// and no line range will fix it.
func replacementIsMalformed(replace string) bool {
	fset := token.NewFileSet()
	if _, err := parser.ParseFile(fset, "x.go", "package p\n"+replace, parser.SkipObjectResolution); err == nil {
		return false // valid as declarations
	}
	wrapped := "package p\nfunc _wrap() {\n" + replace + "\n}\n"
	if _, err := parser.ParseFile(token.NewFileSet(), "x.go", wrapped, parser.SkipObjectResolution); err == nil {
		return false // valid as statements, so it is placement that is wrong
	}
	return true
}

// resolveOldStr turns an exact-text address into a line range, or explains why it
// cannot.
//
// AMBIGUITY IS TEXT ADDRESSING'S ARITHMETIC. A quote matching nothing and a quote
// matching four places are different mistakes and need different advice, so the
// count is reported either way and the candidates are named by line. "not found"
// alone is what made the previous search format unrecoverable.
func resolveOldStr(before, old string) (from, to int, err error) {
	if old == "" {
		return 0, 0, errors.New("old_str is empty")
	}
	lines := strings.Split(strings.TrimSuffix(before, "\n"), "\n")
	oldLines := strings.Split(strings.TrimSuffix(old, "\n"), "\n")

	var at []int
	for i := 0; i+len(oldLines) <= len(lines); i++ {
		match := true
		for j, ol := range oldLines {
			if strings.TrimRight(lines[i+j], " \t") != strings.TrimRight(ol, " \t") {
				match = false
				break
			}
		}
		if match {
			at = append(at, i+1)
		}
	}

	// A SECOND PASS IGNORING INDENTATION, and only when the first found nothing.
	// The model retypes a quote as often as it copies one, and gets the leading
	// tabs wrong when it does — which is a transcription slip, not a different
	// intention. Accepted only when it still resolves to exactly ONE place, so
	// tolerance never buys ambiguity.
	if len(at) == 0 {
		for i := 0; i+len(oldLines) <= len(lines); i++ {
			match := true
			for j, ol := range oldLines {
				if strings.TrimSpace(lines[i+j]) != strings.TrimSpace(ol) {
					match = false
					break
				}
			}
			if match {
				at = append(at, i+1)
			}
		}
		if len(at) > 1 {
			at = nil // ambiguous once indentation is ignored: report it as absent
		}
	}

	switch len(at) {
	case 1:
		return at[0], at[0] + len(oldLines) - 1, nil
	case 0:
		// WHERE IT DIVERGED, not merely that it did. The model quotes long spans from
		// memory — twenty lines of a function plus the declaration above it — and one
		// wrong character anywhere fails the whole match. "Does not appear" leaves it
		// to guess which character; naming the first differing line turns a dead end
		// into a correction it can make.
		if at, n := longestPrefixMatch(lines, oldLines); n > 0 {
			return 0, 0, fmt.Errorf("old_str does not match. Its first %d line(s) DO match at line %d, "+
				"but line %d of your quote differs:\n\n  you wrote: %q\n  the file has: %q\n\n"+
				"Copy the text from the numbered contents rather than retyping it, or quote fewer lines "+
				"— a short unique snippet is easier to get exactly right than a whole function. To "+
				"replace a whole function or type, name it in \"decl\" instead and quote nothing.",
				n, at, n+1, clip(strings.TrimSpace(oldLines[n]), 80), clip(strings.TrimSpace(lineAt(lines, at+n)), 80))
		}
		return 0, 0, fmt.Errorf("old_str does not appear in the file, and not even its first line does. "+
			"It must match EXACTLY — copy it from the numbered contents rather than retyping it. To "+
			"replace a whole function or type, name it in \"decl\" instead. You gave %d line(s) "+
			"beginning %q", len(oldLines), clip(oldLines[0], 60))
	default:
		return 0, 0, fmt.Errorf("old_str appears %d times (lines %s), so it does not say which one "+
			"you mean. Add the lines above or below it until the quote is unique, or address that one "+
			"place with start_line and end_line", len(at), joinInts(at))
	}
}

// joinInts renders line numbers for a message.
func joinInts(n []int) string {
	parts := make([]string, len(n))
	for i, v := range n {
		parts[i] = strconv.Itoa(v)
	}
	return strings.Join(parts, ", ")
}

// resolveDecl finds a top-level declaration by name and returns its line span.
//
// Accepts "main", "apiTasksHandler" and the method forms "(*Store).Add" and
// "Store.Add", because a model writing prose about a method writes it either way.
func resolveDecl(before, name string) (from, to int, err error) {
	fset := token.NewFileSet()
	f, perr := parser.ParseFile(fset, "src.go", before, parser.SkipObjectResolution)
	if perr != nil {
		return 0, 0, fmt.Errorf("the file does not parse, so a declaration cannot be located in it: %v. "+
			"Use undo_edit if your own edit broke it, or address the change with start_line and end_line", perr)
	}
	want := strings.TrimSpace(name)
	var have []string
	for _, d := range f.Decls {
		var got string
		switch t := d.(type) {
		case *ast.FuncDecl:
			got = t.Name.Name
			if t.Recv != nil && len(t.Recv.List) > 0 {
				recv := strings.TrimPrefix(types.ExprString(t.Recv.List[0].Type), "*")
				got = recv + "." + t.Name.Name
			}
		case *ast.GenDecl:
			for _, sp := range t.Specs {
				if ts, ok := sp.(*ast.TypeSpec); ok {
					got = ts.Name.Name
				}
			}
		}
		if got == "" {
			continue
		}
		have = append(have, got)
		if got == want || "(*"+strings.Replace(got, ".", ").", 1) == want {
			return fset.Position(d.Pos()).Line, fset.Position(d.End()).Line, nil
		}
	}
	return 0, 0, fmt.Errorf("no top-level declaration named %q. This file declares: %s",
		want, strings.Join(have, ", "))
}

// maxUndoDepth bounds how far undo_edit can walk back.
//
// Two is enough for the case it exists for — an edit that broke the file, and
// the one before it. Deeper is a different failure: an agent that has lost track
// of what it changed is not recovered by rewinding further, and the iteration
// budget is what should end that.
const maxUndoDepth = 2

// longestPrefixMatch finds where the agent's quote stops matching the file, and
// how many of its lines matched there. Returns the 1-indexed file line the quote
// started at, and the count of leading lines that agreed.
func longestPrefixMatch(lines, oldLines []string) (at, n int) {
	best, bestAt := 0, 0
	for i := range lines {
		k := 0
		for k < len(oldLines) && i+k < len(lines) &&
			strings.TrimSpace(lines[i+k]) == strings.TrimSpace(oldLines[k]) {
			k++
		}
		if k > best {
			best, bestAt = k, i+1
		}
	}
	if best == 0 || best == len(oldLines) {
		return 0, 0 // no anchor, or a full match that the caller already handled
	}
	return bestAt, best
}

// lineAt returns the 1-indexed line, or "" past the end.
func lineAt(lines []string, n int) string {
	if n < 1 || n > len(lines) {
		return "(past the end of the file)"
	}
	return lines[n-1]
}

// maxOldStrLines caps the anchor an edit may quote.
//
// Long enough to disambiguate anything that needs it — three lines of context
// around a changed line is ample in a file of a few hundred — and short enough
// that quoting a whole function is no longer expressible, which is what made
// old_str a copy of replace.
const maxOldStrLines = 5

// declNameFromSource reads the declaration name off the first line of a quoted
// span, so an over-long anchor can be treated as the decl address it means.
//
// Deliberately narrow: only a span that BEGINS with a declaration is converted,
// because that is the case where the intent is unambiguous. A quote starting
// mid-body names nothing and is left to be refused.
func declNameFromSource(src string) string {
	first := strings.TrimSpace(strings.SplitN(src, "\n", 2)[0])
	switch {
	case strings.HasPrefix(first, "func "):
		rest := strings.TrimPrefix(first, "func ")
		if strings.HasPrefix(rest, "(") { // a method: func (s *Store) Add(...)
			end := strings.Index(rest, ")")
			if end < 0 {
				return ""
			}
			recv := strings.Fields(strings.Trim(rest[1:end], "*"))
			name := strings.TrimSpace(rest[end+1:])
			if i := strings.IndexAny(name, "("); i >= 0 {
				name = name[:i]
			}
			if len(recv) == 2 && name != "" {
				return strings.TrimPrefix(recv[1], "*") + "." + strings.TrimSpace(name)
			}
			return ""
		}
		if i := strings.IndexAny(rest, "([ "); i > 0 {
			return rest[:i]
		}
	case strings.HasPrefix(first, "type "):
		f := strings.Fields(first)
		if len(f) >= 2 {
			return f[1]
		}
	}
	return ""
}

// declCount counts the top-level declarations in a fragment.
//
// Parsed as a file body, because that is what a quoted span of Go is. A fragment
// that does not parse counts as zero, which keeps the caller conservative: an
// address is only derived from a span whose shape is certain.
func declCount(src string) int {
	f, err := parser.ParseFile(token.NewFileSet(), "x.go", "package p\n"+src, parser.SkipObjectResolution)
	if err != nil {
		return 0
	}
	return len(f.Decls)
}

// duplicateDecls names any top-level declaration that appears more than once.
//
// GO WOULD SAY "redeclared in this block", eventually, from a line number in a
// file the agent has not seen. Saying it here names the symbol instead, and
// catches the damage on the turn it happens rather than after a clone and a test
// run. It is worth checking however the duplication arose: an edit that inserts a
// declaration whose original is still present breaks the build in a way that is
// tedious to unpick from the inside — measured, a developer spending turn after
// turn reading a file to clean up duplicates it had not meant to create.
func duplicateDecls(src string) []string {
	f, err := parser.ParseFile(token.NewFileSet(), "x.go", src, parser.SkipObjectResolution)
	if err != nil {
		return nil // not this check's business; the syntax gate reports it
	}
	seen, dup := map[string]bool{}, map[string]bool{}
	note := func(n string) {
		if n == "" || n == "_" {
			return
		}
		if seen[n] {
			dup[n] = true
		}
		seen[n] = true
	}
	for _, d := range f.Decls {
		switch t := d.(type) {
		case *ast.FuncDecl:
			if t.Recv == nil {
				note(t.Name.Name)
			} else if len(t.Recv.List) > 0 {
				note(strings.TrimPrefix(types.ExprString(t.Recv.List[0].Type), "*") + "." + t.Name.Name)
			}
		case *ast.GenDecl:
			for _, sp := range t.Specs {
				switch v := sp.(type) {
				case *ast.TypeSpec:
					note(v.Name.Name)
				case *ast.ValueSpec:
					for _, n := range v.Names {
						note(n.Name)
					}
				}
			}
		}
	}
	out := make([]string, 0, len(dup))
	for n := range dup {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
