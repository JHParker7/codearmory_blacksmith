package edit

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"strconv"
	"strings"
)

// unterminatedTag matches a struct tag that opened and never closed.
var unterminatedTag = regexp.MustCompile("`[^`]*$")

// RepairGoSource parses a Go file and, if it does not parse, repairs the one
// mistake this model reliably makes before giving up on it.
//
// THE MISSING CLOSING BACKTICK ON A STRUCT TAG. Observed on a live ticket, eight
// fields in one struct, all identical:
//
//	ID    string `json:"id"
//
// It is the model's own output, not the harness losing anything: the same model
// through the same code path emitted the closing backtick correctly on an
// earlier run, and nothing here touches backticks. It is simply a sequence this
// model drops sometimes, and no prompt reliably prevents it.
//
// REPAIRING IS SAFE BECAUSE THE RESULT IS VERIFIED, and that is the whole
// design. A Go raw string may legitimately span lines, so a lone backtick is not
// always a missing terminator — the repair is therefore a GUESS, and it is
// accepted only if the file parses afterwards. A wrong guess produces something
// that does not parse and is discarded, leaving the original error to be
// reported. The same discipline model.DecodeJSON uses on control characters:
// repair what is unambiguous, verify, and never let a guess through unchecked.
//
// Returns the content to use, whether a repair was applied, and an error only
// when the file cannot be made to parse at all.
func RepairGoSource(path, src string) (string, bool, error) {
	if !strings.HasSuffix(path, ".go") {
		return src, false, nil
	}
	if _, err := parseGo(path, src); err == nil {
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
		_, err := parseGo(path, src)
		return src, false, err
	}

	repaired := strings.Join(lines, "\n")
	if _, err := parseGo(path, repaired); err != nil {
		// The guess was wrong. Report the ORIGINAL error, because an error quoting
		// a line the model never wrote is a worse clue than one quoting its own.
		_, orig := parseGo(path, src)
		return src, false, orig
	}
	return repaired, true, nil
}

// EnclosingDecl names the top-level declaration a line range falls inside, and
// the range that would cover the whole of it.
//
// THE AGENT IS NOT GETTING THE CODE WRONG, IT IS GETTING THE ARITHMETIC WRONG.
// Read from r69's stall: across 77 refusals the developer's diagnosis was
// correct and unchanging — Go's default mux prefix-matching "/api/tasks/" so it
// swallows "/api/tasks", a handler returning 200 where the test wants 404,
// duplicate route registrations. It knew all of that on the first turn. What it
// could not do was name the lines: 41 of those refusals were a range that cut
// across a function so the replacement landed at file scope, and 9 were an
// end_line before the start.
//
// And the refusal it got said "main.go is not valid Go after this edit", so it
// concluded its CODE was wrong and re-derived the same correct analysis, turn
// after turn. The message described the symptom at file scope and never named
// the cause.
//
// So this answers the question the agent actually needs answered: WHICH
// DECLARATION DID I CUT, AND WHAT RANGE COVERS IT. Handing back real numbers
// ends the arithmetic, where "give a range that covers whole declarations" only
// restates the requirement it is already failing to meet.
func EnclosingDecl(src string, startLine, endLine int) (name string, from, to int, ok bool) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "src.go", src, parser.SkipObjectResolution)
	if err != nil {
		// The pre-edit source is already broken; not this check's business.
		return "", 0, 0, false
	}
	for _, d := range f.Decls {
		a, b := fset.Position(d.Pos()).Line, fset.Position(d.End()).Line
		// OVERLAPPING IS THE CASE THAT MATTERS, not containment: a range that
		// starts inside one declaration and ends inside the next is precisely the
		// edit that leaves statements stranded between them.
		if startLine <= b && endLine >= a {
			switch t := d.(type) {
			case *ast.FuncDecl:
				if t.Recv != nil && len(t.Recv.List) > 0 {
					return "method " + t.Name.Name, a, b, true
				}
				return "func " + t.Name.Name, a, b, true
			case *ast.GenDecl:
				return t.Tok.String() + " declaration", a, b, true
			}
		}
	}
	return "", 0, 0, false
}

// errorLine pulls the line number out of a Go compiler or parser message.
var errorLine = regexp.MustCompile(`:(\d+):\d+:`)

// WindowAround shows the file AS THE EDIT LEFT IT, around the problem.
//
// A line range makes it easy to cut across a brace boundary, and the parser's
// line number refers to a file the agent has never seen: the POST-edit one.
// Naming a line it cannot look at is the same mistake as pointing at a tool it
// does not have. Measured on the implementation fixture: 19 refusals across two
// break points, none of them showing the damage.
func WindowAround(content, errText string) string {
	lines := strings.Split(strings.TrimSuffix(content, "\n"), "\n")

	at := 0
	if m := errorLine.FindStringSubmatch(errText); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil {
			at = n
		}
	}
	// CLAMP TO THE FILE BEFORE WINDOWING. A parse error can name a line past the
	// end — "expected declaration" at the point where a truncated file simply
	// stops — and an unclamped window then starts after it ends and renders
	// nothing, which is worse than the message it replaced.
	if at == 0 || at > len(lines) {
		at = len(lines)
	}
	lo, hi := max(at-8, 1), min(at+4, len(lines))

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

func parseGo(path, src string) (*ast.File, error) {
	return parser.ParseFile(token.NewFileSet(), path, src, parser.SkipObjectResolution)
}
