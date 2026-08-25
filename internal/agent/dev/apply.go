package dev

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"sort"
	"strings"

	"github.com/code-armory-app/blacksmith/internal/edit"
)

// MaxFileBytes bounds one file the agent writes.
const MaxFileBytes = 256 << 10

// MaxUndoDepth bounds the snapshots kept for undo_edit.
//
// The useful undo is the last one or two. An agent that needs to walk back ten
// edits has lost the thread, and the iteration budget is the thing that should
// stop it.
const MaxUndoDepth = 2

// DocFiles are the architect's, and no stage below it may edit them.
func IsDocFile(p string) bool {
	switch strings.ToLower(path.Ext(path.Clean(p))) {
	case ".md", ".rst", ".adoc", ".txt":
		return true
	}
	return false
}

// Apply validates a batch of edits and commits it to the staged tree.
//
// EVERY RETURN HERE IS A MESSAGE THE AGENT READS, and naming the symptom rather
// than the cause is the most expensive single failure class in this repository's
// history: it sends a correct agent to the wrong place. r69 spent 41 refusals
// re-deriving a routing diagnosis that was right the first time, because the
// refusal said "main.go is not valid Go after this edit" when the fault was a
// line range crossing a function boundary.
//
// So the shape of this function is: convert every address to a range, decide
// what is refused and say WHY in terms the agent can act on, and only then
// commit — all of it, or none.
func Apply(s *State, edits []edit.Edit, mode Mode) error {
	if len(edits) == 0 {
		return errors.New("write_files with no edits")
	}
	if len(edits) > MaxWriteFiles {
		return fmt.Errorf("write_files with %d edits, limit is %d", len(edits), MaxWriteFiles)
	}
	paths := make([]string, len(edits))
	for i, e := range edits {
		paths[i] = e.Path
	}
	if _, err := edit.ValidatePaths(paths, MaxWriteFiles); err != nil {
		return err
	}

	// EVERYTHING IS VALIDATED AGAINST A COPY and committed only once every edit
	// in the batch has succeeded. A batch that half-applies leaves the tree in a
	// state neither the model nor the prompt describes, and the next turn would
	// be reasoning about a file that is partly edited and partly not.
	staged := maps(s.Staged)

	// Only the files this batch wrote are syntax-checked. A file that arrived
	// broken from the repository is not this edit's fault, and refusing a write
	// because of it would trap the agent on someone else's mistake.
	touched := map[string]string{}

	// What each ranged edit CUT: the file as it was, and the lines asked for.
	// Kept so a syntax failure can name the declaration the range crossed instead
	// of reporting only where the braces stopped balancing.
	type cutRecord struct {
		before   string
		from, to int
		replace  string
	}
	cut := map[string]cutRecord{}

	ordered := append([]edit.Edit(nil), edits...)
	appends := map[int]bool{}

	// TEXT AND DECLARATION ADDRESSES ARE RESOLVED TO RANGES FIRST, so everything
	// downstream — ordering, the whole-file check, the syntax report — works on
	// one representation. The agent chooses how to point at the code; the harness
	// converts, and reports a conversion failure in the terms the agent used
	// rather than in line numbers it never supplied.
	for i := range ordered {
		e := &ordered[i]
		if e.OldStr == "" && e.Decl == "" {
			continue
		}
		cp := path.Clean(e.Path)
		before, have := s.contentOf(staged, cp)
		if !have {
			return fmt.Errorf("%s has not been read, so its text cannot be matched. read_files it first", cp)
		}

		// AN IDENTICAL PAIR CHANGES NOTHING, whatever its length. The schema emits
		// properties alphabetically, so a long old_str is followed a field later by
		// replace, and repeating what was just written is the cheapest continuation
		// — 11 of 26 refusals in one window were exactly that. "Your edits changed
		// nothing" does not name the cause; this does.
		if e.OldStr != "" && e.OldStr == e.Replace {
			return fmt.Errorf("%s: old_str and replace are IDENTICAL, so this edit would change "+
				"nothing. old_str is the text as it is NOW; replace is what it should BECOME. If you "+
				"are rewriting a whole function, name it in \"decl\" and send the new body once in "+
				`"replace" — you never have to type the old text twice`, cp)
		}

		// Both filled is not a mistake the agent can avoid: every field is required
		// by the schema, so it emits all of them. old_str is the more specific
		// address and wins, with decl kept as the fallback below.
		fallbackDecl := e.Decl
		if e.OldStr != "" {
			e.Decl = ""
		}

		var from, to int
		var err error
		switch {
		case e.OldStr != "":
			from, to, err = resolveQuote(before, *e, fallbackDecl)
		case e.Decl != "":
			from, to, err = resolveDeclaration(before, e, cp, appends, i)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", cp, err)
		}
		e.StartLine, e.EndLine = from, to
	}

	// BOTTOM-UP, because a line range names a position in the file as the model
	// saw it. Applying an edit at line 10 shifts everything below it, so a later
	// edit at line 90 would land somewhere the model never looked. Working from
	// the bottom means every range is still valid when its turn comes.
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].StartLine > ordered[j].StartLine })

	// Mixing a whole-file replacement with line edits for the same file is
	// REFUSED rather than ordered: the two describe different files, and picking
	// a winner would silently discard one of them.
	whole, ranged := map[string]bool{}, map[string]bool{}
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

	for i, e := range ordered {
		p := path.Clean(e.Path)
		if err := mode.mayWrite(p, s.Tree); err != nil {
			return err
		}
		before, have := s.contentOf(staged, p)

		if e.StartLine == 0 {
			content, err := s.wholeFile(e, p, before, have, mode)
			if err != nil {
				return err
			}
			staged[p], touched[p] = content, content
			continue
		}

		if !have {
			if slices.Contains(s.Tree, p) && !s.Missing[p] {
				return fmt.Errorf("%s has not been read, so you cannot know what its lines are. "+
					"read_files it first", p)
			}
			return fmt.Errorf("%s does not exist, so it has no lines to replace. To create it, omit "+
				"start_line and end_line and put the whole file in \"replace\"", p)
		}

		after, from, to, err := applyRange(before, e, p, appends[i])
		if err != nil {
			return err
		}
		staged[p] = edit.WithTrailingNewline(after)
		touched[p] = staged[p]
		cut[p] = cutRecord{before, from, to, e.Replace}
	}

	// THE HARNESS MAKES UP FOR THE MODEL WHERE IT CAN. A write that leaves a Go
	// file unparseable is repaired if the repair is unambiguous, and refused only
	// when it is not — because the alternative costs a turn to say something the
	// harness already knows, and the agent may not recover at all. Measured: a
	// developer wrote store.go with every struct tag missing its closing
	// backtick, then spent 34 consecutive actions failing to search and replace
	// its way out, because search text never matches a file it has already broken.
	for _, p := range sortedKeys(touched) {
		content := touched[p]
		repaired, fixed, err := edit.RepairGoSource(p, content)
		if err != nil {
			k, ok := cut[p]
			return brokenAfterEdit(p, content, err, ok, k.before, k.from, k.to, k.replace)
		}
		if fixed {
			staged[p] = repaired
			s.Repairs = append(s.Repairs, p)
		}
		// CAUGHT ON THE TURN IT HAPPENS. A declaration inserted while its original
		// is still present compiles to "redeclared in this block" — reported later,
		// from a line number in a file the agent has not seen, and tedious to
		// unpick from the inside. Naming the symbol here stops the edit instead.
		if dups := edit.DuplicateDecls(staged[p]); len(dups) > 0 {
			return fmt.Errorf("%s would declare %s twice. Your replacement adds a declaration whose "+
				`original is still in the file — replace the existing one in place (name it in "decl") `+
				"rather than adding a second copy, or remove the old one in the same edit",
				p, strings.Join(dups, " and "))
		}
	}

	// SNAPSHOT BEFORE COMMITTING, so the previous state survives the write that
	// is about to replace it.
	s.UndoStack = append(s.UndoStack, maps(s.Staged))
	if n := len(s.UndoStack); n > MaxUndoDepth {
		s.UndoStack = append([]map[string]string(nil), s.UndoStack[n-MaxUndoDepth:]...)
	}

	if s.Staged == nil {
		s.Staged = map[string]string{}
	}
	if s.Read == nil {
		s.Read = map[string]string{}
	}
	for p, c := range staged {
		s.Staged[p] = c
		s.Read[p] = c // the agent sees its own edit next turn
	}
	return nil
}

// resolveQuote turns a text anchor into a line range.
func resolveQuote(before string, e edit.Edit, fallbackDecl string) (int, int, error) {
	// LENGTH IS ONLY A PROBLEM WHEN THE QUOTE FAILS. Capping it up front was aimed
	// at the developer copying long spans, and it blocked the SPECIFICATION AUTHOR
	// instead — a test function is twenty-odd lines by nature, and five
	// consecutive refusals told it its perfectly matchable quote was too long. So
	// the quote is tried first, and its size is only mentioned if it did not
	// resolve.
	from, to, err := edit.ResolveText(before, e.OldStr)
	if err == nil {
		return from, to, nil
	}

	switch {
	case fallbackDecl != "":
		// A quote that missed alongside a declaration name is a correct decl
		// address with a redundant quote attached.
		return edit.ResolveDecl(before, fallbackDecl)

	case edit.DeclCount(e.OldStr) == 1:
		// DERIVE IT FROM THE QUOTE, but only when the span is exactly one
		// declaration. A span covering several is a multi-declaration edit, and
		// naming it after its first line replaces one while inserting all of them —
		// that put main, store and apiTasksHandler into main.go twice.
		if name := edit.DeclNameFromSource(e.OldStr); name != "" {
			return edit.ResolveDecl(before, name)
		}

	default:
		if n := lineCount(e.OldStr); n > edit.MaxOldStrLines {
			return 0, 0, fmt.Errorf("%w\n\nIt is also %d lines. A quote is an ANCHOR — a short unique "+
				`snippet is far easier to match than a whole function, and "replace" may be as `+
				`long as you like. To replace a whole declaration, name it in "decl" instead`, err, n)
		}
	}
	return 0, 0, err
}

// resolveDeclaration turns a declaration name into a line range, translating the
// two things agents ask for that the decl shape could not originally express.
func resolveDeclaration(before string, e *edit.Edit, cp string, appends map[int]bool, i int) (int, int, error) {
	lines := lineCount(before)

	// "REPLACE THIS FILE" ARRIVES AS A DECL NAMED AFTER THE FILE. The spec author
	// rewrites whole test files by nature, and the branched schema left it no
	// obvious whole-file route — so it put the filename in decl and the entire
	// file in replace. Measured: 18 refusals of `no top-level declaration named
	// "handlers_read_test.go"` in five minutes.
	//
	// Converted to the explicit full range rather than refused, which leaves the
	// DECISION where it already lives: a spec author may rewrite its own tests,
	// and a developer may not blindly overwrite a file the sections share. This
	// only translates the request; the guards below still answer it.
	if strings.HasPrefix(strings.TrimSpace(e.Replace), "package ") ||
		e.Decl == cp || e.Decl == path.Base(cp) {
		return 1, lines, nil
	}

	from, to, err := edit.ResolveDecl(before, e.Decl)
	if err == nil {
		return from, to, nil
	}

	// A DECLARATION THAT IS NOT THERE YET IS ONE TO ADD. The decl shape could only
	// ever REPLACE, which leaves test-first work — whose entire job is writing
	// functions that do not exist — with no natural move. Measured: 55 refusals of
	// "no top-level declaration named GETApiTasksHandler" against a file that was
	// supposed to gain exactly that.
	//
	// Appended rather than refused, and only when the replacement really is one or
	// more declarations: an append of a stray fragment would break the file, and
	// the duplicate gate still catches a name that already exists elsewhere.
	if edit.DeclCount(e.Replace) >= 1 {
		appends[i] = true
		e.Replace = "\n" + strings.TrimLeft(e.Replace, "\n")
		return lines + 1, lines, nil
	}
	return 0, 0, err
}

// mayWrite answers whether this stage may touch this path, and says why not in
// terms of what the stage is FOR.
func (m Mode) mayWrite(p string, tree []string) error {
	switch {
	case m == ModeCoverage && !edit.IsTestFile(p):
		return fmt.Errorf("%s is not a test file. You may only ADD tests — the implementation is "+
			"finished and reviewed work, and changing it to make a test pass is not covering it", p)

	case m == ModeCoverage && slices.Contains(tree, p):
		// THE SPECIFICATION IS NOT YOURS TO EDIT. The tests written before the code
		// are what the developer was held to, and the cheapest way to raise a
		// coverage number is to weaken one of them — which would hit the target
		// while destroying the thing the target is a proxy for. New files only.
		return fmt.Errorf("%s already existed before this stage. Those tests are the specification the "+
			"developer was held to and must not be edited. Put your new tests in a new file, "+
			"for example coverage_test.go", p)

	case m == ModeSpecMerge && !edit.IsTestFile(p):
		return fmt.Errorf("%s is not a test file. You are reconciling the tests several authors "+
			"wrote onto this branch; the implementation is not yours to change", p)

	case m == ModeTest && !edit.IsTestFile(p):
		return fmt.Errorf("%s is not a test file. You may only edit *_test.go — "+
			"the implementation is another agent's work, and changing it to make your test pass "+
			"defeats the point of writing the test. If the code is wrong, write the test that "+
			"proves it and let it fail", p)

	case m == ModeDevelop && edit.IsTestFile(p):
		return fmt.Errorf("%s is a test file, and tests are written by a separate agent against "+
			"the ticket. Edit the implementation; do not edit tests", p)

	case m == ModeDevelop && IsDocFile(p):
		// THE DOCUMENTATION IS THE ARCHITECT'S, and it is already on the base branch
		// when this agent's sandbox clones. It describes the WHOLE system — the
		// names and interfaces every other ticket is being built against — so a
		// developer editing it to match its own half is not documenting anything, it
		// is quietly rewriting five other agents' specification.
		//
		// Belt and braces: the breakdown no longer emits documentation subtasks, so
		// a developer should never be pointed at one. This is the guard for when
		// that fails anyway, and it is cheap — the same situation unguarded cost 642
		// seconds of a large-class slot and left the developer a gate it could not
		// pass.
		return fmt.Errorf("%s is documentation. It is written once by the architect before the work "+
			"is broken down, and it is the specification the OTHER tickets are being built against — "+
			"not this ticket's to change. Edit the implementation", p)
	}
	return nil
}

// wholeFile handles a create, or a deliberate rewrite of something already read.
func (s *State) wholeFile(e edit.Edit, p, before string, have bool, mode Mode) (string, error) {
	// The guard here was "it must not already exist", which is right for a BLIND
	// overwrite — silently discarding contents the model never saw — and wrong
	// once it has read them. They are in its prompt; replacing them wholesale is
	// no less informed than a search and replace, and it is the only move
	// available when an earlier attempt left a file on the branch that this
	// attempt cannot match exactly.
	//
	// Measured: a section author retried onto a branch already holding its own
	// file, could not reproduce the search text byte for byte, and had no legal
	// way to rewrite it — 21 turns alternating a refused edit with a refused
	// verification.
	//
	// THE EXCEPTION IS NARROW: a spec author rewriting a test file it has read.
	// That file is its own output — an earlier attempt of this same ticket wrote
	// it — and replacing it wholesale loses nothing that belongs to anyone else. A
	// developer replacing main.go is the failure this guard exists for and stays
	// refused: sections of one task share a branch, so a wholesale rewrite can
	// drop a concurrent section's work between the read and the write.
	_, seen := s.Read[p]
	ownTestFile := mode == ModeTest && edit.IsTestFile(p) && seen

	if have && !ownTestFile && strings.TrimSpace(before) != "" {
		// SAY HOW TO DO WHAT IT IS TRYING TO DO. Replacing a file wholesale is a
		// legitimate thing to want — main.go arrives as a six-line stub and writing
		// the implementation into it is not a two-line edit — and a line range
		// expresses it exactly. Refusing without naming that cost r65: 78 refusals,
		// all this message, the developer asking for the one thing it was never told
		// it already had.
		n := lineCount(before)
		return "", fmt.Errorf("%s already exists, and omitting start_line and end_line replaces the "+
			"WHOLE file — including anything another section of this task has written to the same "+
			"branch.\n\nTo replace ALL of it deliberately, say so with a range: start_line 1 and "+
			"end_line %d (it has %d lines). To change part of it, take the range from the numbered "+
			"contents shown to you.", p, n, n)
	}
	if !have && slices.Contains(s.Tree, p) && !s.Missing[p] {
		return "", fmt.Errorf("%s already exists and you have not read it. read_files it first, "+
			"then give the exact text to replace", p)
	}
	if strings.TrimSpace(e.Replace) == "" {
		return "", fmt.Errorf("%s has an empty search and an empty replace, so it would create an "+
			`empty file. Put the file's text in "replace"`, p)
	}
	return edit.WithTrailingNewline(e.Replace), nil
}

// applyRange splices a replacement into a line range, forgiving the two
// arithmetic slips that have exactly one reading and refusing the ones that
// do not.
func applyRange(before string, e edit.Edit, p string, isAppend bool) (after string, from, to int, err error) {
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
	if !isAppend && e.EndLine == e.StartLine-1 && e.StartLine >= 1 {
		e.EndLine = e.StartLine
	}
	if !isAppend && (e.StartLine < 1 || e.EndLine < e.StartLine) {
		return "", 0, 0, fmt.Errorf("start_line %d, end_line %d is not a range. Lines are numbered "+
			"from 1 and end_line must not be before start_line", e.StartLine, e.EndLine)
	}

	// AN END PAST THE LAST LINE CAN ONLY MEAN "TO THE END", so it is clamped
	// rather than refused. Measured on the implementation fixture: 7 refusals,
	// every one start_line 1 with an end a few lines past a 180-line file — 182,
	// 184, 185, 191. The agent was asking to replace the whole file and missing
	// the final line number by a rounding error, and refusing taught it nothing.
	//
	// A START past the end is different and stays refused: there is no sensible
	// reading of "begin after the file ends" — EXCEPT exactly one line past it,
	// which is an append, and is how a decl address adds a declaration the file
	// does not have yet.
	if !isAppend && e.StartLine > len(lines) {
		return "", 0, 0, fmt.Errorf("%s has %d lines, so line %d is past its end. The contents shown "+
			"to you are numbered — take the range from those", p, len(lines), e.StartLine)
	}
	if e.EndLine > len(lines) {
		e.EndLine = len(lines)
	}

	out := append([]string(nil), lines[:e.StartLine-1]...)
	if e.Replace != "" {
		out = append(out, strings.Split(strings.TrimSuffix(e.Replace, "\n"), "\n")...)
	}
	out = append(out, lines[e.EndLine:]...)

	after = strings.Join(out, "\n")
	if len(after) > MaxFileBytes {
		return "", 0, 0, fmt.Errorf("%s would be %d bytes, limit is %d", p, len(after), MaxFileBytes)
	}
	return after, e.StartLine, e.EndLine, nil
}

// brokenAfterEdit decides which of two opposite faults to report.
//
// LEAD WITH THE CAUSE, NOT THE SYMPTOM. "Not valid Go" reads as "your code is
// wrong", so the agent re-derives an analysis that was right the first time —
// r69 spent 41 refusals doing exactly that while its diagnosis of the routing
// bug never changed.
func brokenAfterEdit(p, content string, err error, ranged bool, before string, from, to int, replace string) error {
	if ranged {
		// THE TEXT IS ASKED FIRST. Blaming the range for a broken literal sends the
		// agent to re-cut a range that was already right — measured directly on the
		// retest, where it was told "lines 230-241 cut across func main, which spans
		// lines 230-241" for a `rune literal not terminated`.
		if edit.ReplacementIsMalformed(replace) {
			return fmt.Errorf("Your LINE RANGE is fine — the replacement TEXT does not parse: %v.\n\n"+
				"This is a fault in what you wrote, not in where you put it, so re-sending the same "+
				"text with a different range cannot help. Look for an unterminated string or rune "+
				"literal, an unbalanced brace, or a stray quote in the text you supplied.", err)
		}
		// AND THE RANGE IS ONLY BLAMED WHEN IT DIFFERS from the declaration it
		// touches. When they are identical the agent already sent the whole thing
		// and repeating the advice is a loop.
		if name, dFrom, dTo, ok := edit.EnclosingDecl(before, from, to); ok && (dFrom != from || dTo != to) {
			return fmt.Errorf("Your replacement text is probably fine — the LINE RANGE is what "+
				"broke it. Lines %d-%d cut across %s, which spans lines %d-%d, so what you wrote "+
				"was left outside any function and %s is no longer valid Go (%v).\n\nRe-issue the "+
				`same change with start_line %d and end_line %d, giving the WHOLE of %s in `+
				`"replace". Do not re-analyse the code — your diagnosis was not the problem.`,
				from, to, name, dFrom, dTo, p, err, dFrom, dTo, name)
		}
	}
	return fmt.Errorf("%s is not valid Go after this edit: %v.\n\nThis is the file AS YOUR EDIT "+
		"LEFT IT, around the problem — the braces will not balance:\n\n%s\n\nEither give a range "+
		"that covers whole declarations, or rewrite the file with start_line 1 and end_line %d.",
		p, err, edit.WindowAround(content, err.Error()), lineCount(content))
}

// Undo puts the staged tree back to what it was before the last write, and
// names what changed.
//
// A REWIND IS PROGRESS, NOT A REFUSAL. It costs a turn and buys a file the agent
// can address again, which is strictly better than the alternative it used to
// have — editing blind against a file it had broken, where every subsequent
// old_str misses and every line number is wrong.
//
// THE READ COPY GOES WITH IT. Apply deliberately overwrites Read with the staged
// content so the agent sees its own edit next turn; undoing without touching
// Read would leave the prompt describing a file that no longer exists in that
// shape, which is the exact confusion undo is here to end.
func (s *State) Undo() (string, bool) {
	if len(s.UndoStack) == 0 {
		return "", false
	}
	prev := s.UndoStack[len(s.UndoStack)-1]
	s.UndoStack = s.UndoStack[:len(s.UndoStack)-1]

	var restored []string
	for p, was := range prev {
		if s.Staged[p] != was {
			restored = append(restored, p)
		}
		s.Staged[p] = was
		s.Read[p] = was
	}
	// A FILE THE UNDONE EDIT CREATED HAS NO PREVIOUS STATE AND MUST GO — from
	// Read as well, so the agent reads it again rather than addressing text the
	// tree no longer holds.
	for p := range s.Staged {
		if _, existed := prev[p]; !existed {
			delete(s.Staged, p)
			delete(s.Read, p)
			restored = append(restored, p+" (removed; the edit created it)")
		}
	}
	sort.Strings(restored)
	if len(restored) == 0 {
		return "nothing differed", true
	}
	return strings.Join(restored, ", "), true
}

// contentOf prefers the staged copy, then what was read.
func (s *State) contentOf(staged map[string]string, p string) (string, bool) {
	if c, ok := staged[p]; ok {
		return c, true
	}
	c, ok := s.Read[p]
	return c, ok
}

func maps(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func lineCount(s string) int {
	return len(strings.Split(strings.TrimSuffix(s, "\n"), "\n"))
}
