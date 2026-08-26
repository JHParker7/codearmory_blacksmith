package edit

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
)

// ErrWouldNotVet marks a written file that compiles but that the toolchain will
// reject on sight.
//
// CAUGHT AT THE WRITE, because by the time it is caught anywhere else the file
// may belong to an agent that cannot edit it. Read off run 81: a specification
// author wrote t.Error("...%q...") — a format directive in a call that does not
// format — and its own gate could not see it, because with no implementation the
// package does not type-check and vet never runs. The developer met it later,
// diagnosed it correctly and repeatedly, and could not fix it: the fault was
// inside a test file. 99 turns across three attempts, not one verification, and
// the ticket blocked.
//
// THESE CHECKS NEED NO TYPES, which is the whole point — they run at spec time,
// where vet cannot.
var ErrWouldNotVet = errors.New("the toolchain would reject this file")

// formatless are the testing calls that take a message rather than a format.
//
// Passing a % verb to one of these is always a mistake and never a subtle one:
// the directive is printed literally and the argument is appended.
var formatless = map[string]bool{"Error": true, "Fatal": true, "Log": true, "Skip": true}

// VetLike reports what `go vet` would say about a file, for the subset that can
// be decided from syntax alone.
//
// It returns nil for anything it cannot judge without types. A check that needs
// a type is not weakened to run here — it is left to the gate, which has one.
func VetLike(path, src string) error {
	f, err := parser.ParseFile(token.NewFileSet(), "x.go", src, parser.SkipObjectResolution)
	if err != nil {
		// Not this function's business: the syntax check reports parse errors, and
		// reporting them twice in different words helps nobody.
		return nil
	}

	if err := formatDirectiveInPlainCall(f); err != nil {
		return err
	}
	return testingImportInProductionFile(path, f)
}

// formatDirectiveInPlainCall finds t.Error("...%s...") and its siblings.
func formatDirectiveInPlainCall(f *ast.File) error {
	var found error
	ast.Inspect(f, func(n ast.Node) bool {
		if found != nil {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || !formatless[sel.Sel.Name] {
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		if verb := formatVerb(lit.Value); verb != "" {
			found = fmt.Errorf("%w: %s is given the format directive %s, but it does "+
				"not format — it prints the directive literally and appends the rest. "+
				"Use %sf for a format, or drop the directive",
				ErrWouldNotVet, sel.Sel.Name, verb, sel.Sel.Name)
			return false
		}
		return true
	})
	return found
}

// formatVerb returns the first Printf verb in a quoted string, or "".
//
// "%%" IS AN ESCAPED PERCENT and not a directive, so it is skipped rather than
// reported — a message that legitimately prints a percent sign must stay
// writable.
func formatVerb(quoted string) string {
	s := strings.Trim(quoted, "`\"")
	for i := 0; i < len(s)-1; i++ {
		if s[i] != '%' {
			continue
		}
		c := s[i+1]
		if c == '%' {
			i++
			continue
		}
		if strings.IndexByte("vTtbcdoqxXUeEfFgGsp", c) >= 0 {
			return "%" + string(c)
		}
	}
	return ""
}

// testingImportInProductionFile finds a shipped file importing a test package.
//
// THE REFUSAL NAMES THE RENAME, which is the part run 83 was missing. The
// developer wrote helpers into server_test_helpers.go, imported testing, and was
// told only that shipped code must not import a testing package — so it spent 26
// writes trying to remove an import the file existed to use. Both directions
// fail; only the rename works, and nothing said so.
func testingImportInProductionFile(path string, f *ast.File) error {
	if IsTestFile(path) {
		return nil
	}
	for _, im := range f.Imports {
		p := strings.Trim(im.Path.Value, `"`)
		if p != "testing" && p != "net/http/httptest" && !strings.HasPrefix(p, "testing/") {
			continue
		}
		return fmt.Errorf("%w: %s is not a test file and imports %q. A file whose job "+
			"is to help tests must be named for one — rename it so the last part before "+
			".go is _test, and it becomes the specification author's to write",
			ErrWouldNotVet, path, p)
	}
	return nil
}
