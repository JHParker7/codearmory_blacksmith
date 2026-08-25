// Package edit turns an agent's description of a change into a line range in a
// file, and refuses the descriptions that cannot mean one thing.
//
// SEARCH/REPLACE, NOT WHOLE FILES, and the difference is the reason this package
// exists. A whole-file write makes "add a struct" and "replace the file with a
// struct" the same action, so a model that forgets to carry the rest forward
// deletes it silently — measured, and it removed an entire HTTP server from a
// branch whose tests still passed, because the only tests written covered the
// struct. An edit that names what it is replacing cannot do that: what it does
// not mention, it does not touch.
//
// It also fails LOUDLY when the file is not what the model believed. An address
// that matches nothing is a wrong assumption caught before it is written, which
// is worth more than a write that succeeds against the wrong content.
package edit

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"path"
	"sort"
	"strconv"
	"strings"
)

// Edit is one change to one file.
//
// THREE WAYS TO SAY WHERE, and the branching is the point. Under a single flat
// shape the model sent the quoted text identical to its replacement in 39 of 47
// turns — a copy the sampler could not resist, because the two fields sat side
// by side and one was a plausible completion of the other. Naming the address by
// its KIND makes that copy unrepresentable rather than merely discouraged.
type Edit struct {
	Path string `json:"path"`

	// OldStr addresses an edit BY ITS TEXT, and is the preferred form.
	//
	// Line arithmetic is the thing this model cannot do. Measured: one ticket, 77
	// refusals, 41 of them a range that cut across a function and 9 an end before
	// the start, while its diagnosis of the actual bug never changed. Addressing
	// by text removes the arithmetic — the agent quotes what it can see rather
	// than counting to it.
	//
	// The field evidence agrees. On a public benchmark a bash-only agent scores
	// about 28% and the same agent with an exact-string editor about 51%.
	//
	// MUST BE UNIQUE. Ambiguity is the failure mode of text addressing as
	// arithmetic is of line addressing, so a match count of anything but one is
	// refused with the candidates named, and the agent widens its quote.
	OldStr string `json:"old_str,omitempty"`

	// Decl addresses a whole top-level declaration by name — "main",
	// "(*Store).Add". Immune to line drift and to a file the agent has already
	// broken, and it matches how the model reasons: a developer named the three
	// functions it cared about in prose on every turn while failing to name their
	// lines.
	Decl string `json:"decl,omitempty"`

	// StartLine and EndLine are 1-indexed and INCLUSIVE. Zero means the whole
	// file: a create, or a deliberate wholesale rewrite where that is permitted.
	//
	// DEMOTED TO A DISAMBIGUATOR. Kept because a range is the one unambiguous way
	// to name a repeated line, which is the case text addressing cannot express.
	StartLine int `json:"start_line"`
	EndLine   int `json:"end_line"`

	// Replace takes the range's place. Empty DELETES those lines, which is
	// legitimate — it is why this is not guarded the way a whole-file blank is.
	Replace string `json:"replace"`
}

// Span is a resolved address: an inclusive 1-indexed line range.
type Span struct {
	From, To int

	// Append marks a change that ADDS to the end of the file rather than
	// replacing part of it. The model never asks for this; the declaration
	// resolver produces it when the declaration named does not exist yet.
	//
	// A FLAG RATHER THAN A LINE RANGE, because "one past the end" collides with
	// three separate guards — the inverted-range check, the transposition repair
	// and the past-the-end check — each right about ordinary edits and wrong about
	// this one.
	Append bool
}

// MaxOldStrLines caps the anchor an edit may quote.
//
// Long enough to disambiguate anything that needs it — three lines of context
// around a changed line is ample in a file of a few hundred — and short enough
// that quoting a whole function is NO LONGER EXPRESSIBLE, which is what made the
// quoted text a copy of the replacement.
const MaxOldStrLines = 5

// IsTestFile reports whether a path is a Go test file.
//
// The suffix is the whole rule because it is also the compiler's rule: Go builds
// these only under `go test`, so it exactly separates "code that ships" from
// "code that checks it". A language with a different convention would need this
// to be configuration; today it would be configuration with one possible value.
func IsTestFile(p string) bool { return strings.HasSuffix(path.Clean(p), "_test.go") }

// Address reports which addressing mode an edit uses, for a caller that has to
// refuse one that uses none or several.
func (e Edit) Address() (mode string, ok bool) {
	switch {
	case e.OldStr != "" && e.Decl == "":
		return "old_str", true
	case e.Decl != "" && e.OldStr == "":
		return "decl", true
	case e.OldStr == "" && e.Decl == "" && e.StartLine > 0:
		return "lines", true
	case e.OldStr == "" && e.Decl == "" && e.StartLine == 0 && e.EndLine == 0:
		return "whole file", true
	}
	return "", false
}

// Resolve turns an address into a line span in `before`.
//
// The order is the preference order: text, then declaration, then lines. A
// caller that has already rejected an ambiguous address gets the same answer
// either way, and one that has not gets the preferred reading.
func Resolve(before string, e Edit) (Span, error) {
	switch {
	case e.OldStr != "":
		from, to, err := ResolveText(before, e.OldStr)
		return Span{From: from, To: to}, err

	case e.Decl != "":
		return ResolveOrAppend(before, e.Decl, e.Replace)

	default:
		return Span{From: e.StartLine, To: e.EndLine}, nil
	}
}

// ResolveText turns an exact-text address into a line range, or explains why it
// cannot.
//
// AMBIGUITY IS TEXT ADDRESSING'S ARITHMETIC. A quote matching nothing and a
// quote matching four places are different mistakes and need different advice,
// so the count is reported either way and the candidates are named by line.
// "Not found" alone is what made the previous format unrecoverable.
func ResolveText(before, old string) (from, to int, err error) {
	if old == "" {
		return 0, 0, errors.New("old_str is empty")
	}

	lines := split(before)
	oldLines := split(old)

	at := matches(lines, oldLines, trimRight)

	// A SECOND PASS IGNORING INDENTATION, and only when the first found nothing.
	// The model retypes a quote as often as it copies one, and gets the leading
	// tabs wrong when it does — a transcription slip, not a different intention.
	// Accepted only when it still resolves to exactly ONE place, so tolerance
	// never buys ambiguity.
	if len(at) == 0 {
		if loose := matches(lines, oldLines, strings.TrimSpace); len(loose) == 1 {
			at = loose
		}
	}

	switch len(at) {
	case 1:
		return at[0], at[0] + len(oldLines) - 1, nil

	case 0:
		// WHERE IT DIVERGED, not merely that it did. The model quotes long spans
		// from memory and one wrong character anywhere fails the whole match.
		// "Does not appear" leaves it to guess which character; naming the first
		// differing line turns a dead end into a correction it can make.
		if start, n := LongestPrefixMatch(lines, oldLines); n > 0 {
			return 0, 0, fmt.Errorf(
				"old_str does not match. Its first %d line(s) DO match at line %d, but line %d of "+
					"your quote differs:\n\n  you wrote: %q\n  the file has: %q\n\n"+
					"Copy the text from the numbered contents rather than retyping it, or quote fewer "+
					"lines — a short unique snippet is easier to get exactly right than a whole "+
					"function. To replace a whole function or type, name it in \"decl\" instead and "+
					"quote nothing.",
				n, start, n+1,
				clip(strings.TrimSpace(oldLines[n]), 80),
				clip(strings.TrimSpace(lineAt(lines, start+n)), 80))
		}
		return 0, 0, fmt.Errorf(
			"old_str does not appear in the file, and not even its first line does. It must match "+
				"EXACTLY — copy it from the numbered contents rather than retyping it. To replace a "+
				"whole function or type, name it in \"decl\" instead. You gave %d line(s) beginning %q",
			len(oldLines), clip(oldLines[0], 60))

	default:
		return 0, 0, fmt.Errorf(
			"old_str appears %d times (lines %s), so it does not say which one you mean. Add the "+
				"lines above or below it until the quote is unique, or address that one place with "+
				"start_line and end_line",
			len(at), joinInts(at))
	}
}

// matches lists the 1-indexed lines where oldLines occurs, comparing each line
// through norm.
func matches(lines, oldLines []string, norm func(string) string) []int {
	var at []int
	for i := 0; i+len(oldLines) <= len(lines); i++ {
		found := true
		for j, ol := range oldLines {
			if norm(lines[i+j]) != norm(ol) {
				found = false
				break
			}
		}
		if found {
			at = append(at, i+1)
		}
	}
	return at
}

func trimRight(s string) string { return strings.TrimRight(s, " \t") }

// ResolveDecl finds a top-level declaration by name and returns its line span.
//
// Accepts "main", "Store.Add" and "(*Store).Add", because a model writing prose
// about a method writes it either way.
//
// A DECLARATION THAT IS NOT THERE IS AN ERROR HERE, and the caller decides
// whether to turn it into an append — see ResolveOrAppend. Keeping the two apart
// matters because the append is only safe under a condition this function cannot
// see: what the replacement contains.
func ResolveDecl(before, name string) (from, to int, err error) {
	fset := token.NewFileSet()
	f, perr := parser.ParseFile(fset, "src.go", before, parser.SkipObjectResolution)
	if perr != nil {
		return 0, 0, fmt.Errorf(
			"the file does not parse, so a declaration cannot be located in it: %v. Use undo_edit if "+
				"your own edit broke it, or address the change with start_line and end_line", perr)
	}

	want := strings.TrimSpace(name)
	var have []string
	for _, d := range f.Decls {
		got := declName(d)
		if got == "" {
			continue
		}
		have = append(have, got)
		if matchesDecl(got, want) {
			return fset.Position(d.Pos()).Line, fset.Position(d.End()).Line, nil
		}
	}

	return 0, 0, fmt.Errorf("no top-level declaration named %q. This file declares: %s",
		want, strings.Join(have, ", "))
}

// ResolveOrAppend resolves a declaration address, turning "it is not there yet"
// into an APPEND when the replacement really is one or more declarations.
//
// A DECLARATION THAT IS NOT THERE YET IS ONE TO ADD. The declaration address
// could only ever REPLACE, which left test-first work — whose entire job is
// writing functions that do not exist — with no natural move. Measured: 55
// refusals of "no top-level declaration named GETApiTasksHandler" against a file
// that was supposed to gain exactly that.
//
// THE CONDITION IS THE WHOLE SAFETY OF IT. Appending a stray fragment — a bare
// statement, half an expression — breaks the file, and the agent then spends its
// budget repairing damage this function did. So the append happens only when the
// replacement parses as declarations, and a caller's duplicate check still
// catches a name that already exists elsewhere.
func ResolveOrAppend(before, name, replace string) (Span, error) {
	from, to, err := ResolveDecl(before, name)
	if err == nil {
		return Span{From: from, To: to}, nil
	}
	if DeclCount(replace) >= 1 {
		return Span{Append: true}, nil
	}
	return Span{}, err
}

func declName(d ast.Decl) string {
	switch t := d.(type) {
	case *ast.FuncDecl:
		if t.Recv != nil && len(t.Recv.List) > 0 {
			recv := strings.TrimPrefix(types.ExprString(t.Recv.List[0].Type), "*")
			return recv + "." + t.Name.Name
		}
		return t.Name.Name
	case *ast.GenDecl:
		var got string
		for _, sp := range t.Specs {
			if ts, ok := sp.(*ast.TypeSpec); ok {
				got = ts.Name.Name
			}
		}
		return got
	}
	return ""
}

// matchesDecl accepts both spellings of a method: the parser produces
// "Store.Add" and a model writes "(*Store).Add" about as often.
func matchesDecl(got, want string) bool {
	return got == want || "(*"+strings.Replace(got, ".", ").", 1) == want
}

// ReplacementIsMalformed reports whether a replacement is not valid Go in any
// position.
//
// TWO PARSES, because a replacement is legitimately either. A function body
// fragment is not valid at file scope and a declaration is not valid inside one,
// so a single parse would refuse half of every agent's correct edits. Failing
// BOTH is what says the text itself is broken rather than merely misplaced.
func ReplacementIsMalformed(replace string) bool {
	if _, err := parser.ParseFile(token.NewFileSet(), "x.go", "package p\n"+replace,
		parser.SkipObjectResolution); err == nil {
		return false // valid as declarations
	}
	wrapped := "package p\nfunc _wrap() {\n" + replace + "\n}\n"
	if _, err := parser.ParseFile(token.NewFileSet(), "x.go", wrapped,
		parser.SkipObjectResolution); err == nil {
		return false // valid as statements, so it is placement that is wrong
	}
	return true
}

// DuplicateDecls names any top-level declaration appearing more than once.
//
// GO WOULD SAY "redeclared in this block", eventually, from a line number in a
// file the agent has not seen. Saying it here names the SYMBOL instead, and
// catches the damage on the turn it happens rather than after a clone and a test
// run. Measured: a developer spending turn after turn reading a file to clean up
// duplicates it had not meant to create.
func DuplicateDecls(src string) []string {
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

// DeclCount counts the top-level declarations in a fragment.
//
// Parsed as a file body, because that is what a quoted span of Go is. A fragment
// that does not parse counts as ZERO, which keeps the caller conservative: an
// address is only derived from a span whose shape is certain.
func DeclCount(src string) int {
	f, err := parser.ParseFile(token.NewFileSet(), "x.go", "package p\n"+src, parser.SkipObjectResolution)
	if err != nil {
		return 0
	}
	return len(f.Decls)
}

// DeclNameFromSource reads the declaration name off the first line of a quoted
// span, so an over-long anchor can be treated as the declaration address it
// means.
//
// DELIBERATELY NARROW: only a span that BEGINS with a declaration is converted,
// because that is the case where the intent is unambiguous. A quote starting
// mid-body names nothing and is left to be refused.
func DeclNameFromSource(src string) string {
	first := strings.TrimSpace(strings.SplitN(src, "\n", 2)[0])

	switch {
	case strings.HasPrefix(first, "func "):
		rest := strings.TrimPrefix(first, "func ")
		if strings.HasPrefix(rest, "(") { // a method
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
		if f := strings.Fields(first); len(f) >= 2 {
			return f[1]
		}
	}
	return ""
}

// LongestPrefixMatch finds where the agent's quote stops matching the file, and
// how many of its lines matched there. Returns the 1-indexed line the quote
// started at and the count of leading lines that agreed.
func LongestPrefixMatch(lines, oldLines []string) (at, n int) {
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
		return 0, 0 // no anchor, or a full match the caller already handled
	}
	return bestAt, best
}

// WithTrailingNewline ends content with a newline.
//
// Models routinely emit a final line with none, and these are whole-file writes,
// so whatever comes back IS the file. The result is a diff carrying "no newline
// at end of file" on every file the agent touches — which the formatter flags,
// POSIX says is not a text file, and reviewers read as carelessness: noise on
// every change, obscuring the change itself.
//
// EMPTY CONTENT IS LEFT ALONE: a file the agent deliberately emptied should stay
// empty rather than gain a blank line.
func WithTrailingNewline(content string) string {
	if content == "" || strings.HasSuffix(content, "\n") {
		return content
	}
	return content + "\n"
}

// ValidatePaths refuses anything that could reach outside the checkout.
//
// THE MODEL SUPPLIES THESE STRINGS, so this is a trust boundary rather than a
// tidiness check: "src/main.go" and "../../etc/cron.d/x" differ only in content.
func ValidatePaths(paths []string, max int) ([]string, error) {
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

// Apply replaces a span's lines with the replacement and returns the new
// content.
//
// THE RANGE IS INCLUSIVE AND 1-INDEXED, matching what the agent is shown. An
// off-by-one here silently eats a line of code, which is the class of damage the
// whole package is arranged to prevent.
func Apply(before string, s Span, replace string) (string, error) {
	if s.Append {
		return WithTrailingNewline(WithTrailingNewline(before) + replace), nil
	}

	lines := split(before)
	if s.From == 0 && s.To == 0 {
		return WithTrailingNewline(replace), nil // a whole-file write
	}
	if s.From < 1 || s.From > len(lines) {
		return "", fmt.Errorf("start_line %d is outside the file, which has %d lines", s.From, len(lines))
	}
	if s.To < s.From {
		return "", fmt.Errorf("end_line %d is before start_line %d", s.To, s.From)
	}
	if s.To > len(lines) {
		return "", fmt.Errorf("end_line %d is past the end of the file, which has %d lines", s.To, len(lines))
	}

	var b strings.Builder
	for _, l := range lines[:s.From-1] {
		b.WriteString(l)
		b.WriteString("\n")
	}
	if replace != "" {
		b.WriteString(strings.TrimSuffix(replace, "\n"))
		b.WriteString("\n")
	}
	for _, l := range lines[s.To:] {
		b.WriteString(l)
		b.WriteString("\n")
	}
	return b.String(), nil
}

func split(s string) []string { return strings.Split(strings.TrimSuffix(s, "\n"), "\n") }

// lineAt returns the 1-indexed line, or a phrase saying there is none.
func lineAt(lines []string, n int) string {
	if n < 1 || n > len(lines) {
		return "(past the end of the file)"
	}
	return lines[n-1]
}

func joinInts(n []int) string {
	parts := make([]string, len(n))
	for i, v := range n {
		parts[i] = strconv.Itoa(v)
	}
	return strings.Join(parts, ", ")
}

// clip bounds a quoted fragment for a message, cutting on a rune boundary.
func clip(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
