package architect

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path"
	"strings"
)

// ErrNotDeclarations rejects a Go file from the architect that does more than
// declare.
var ErrNotDeclarations = errors.New("the architect may declare, not implement")

// DeclarationsMarker identifies a file as the architect's shapes rather than
// anyone's implementation.
//
// THE AUTHOR'S GATE COUNTS EXPORTED DECLARATIONS to decide whether a
// specification's subject is ALREADY BUILT, which is a real case: sections of one
// task share a branch, so whichever lands first leaves its code there for the
// rest. Declarations read exactly like that finished work.
//
// Mostly the panic rule prevents the confusion — a stub that panics cannot make a
// test pass, and the already-built check only runs on a GREEN suite. The gap is a
// test asserting on a declared var or const without calling anything: it passes,
// and a sound specification is marked finished. This names the one file whose
// declarations prove nothing.
const DeclarationsMarker = "// blacksmith:declarations"

// OnlyDeclarations reports whether a Go file the architect wrote is what this
// stage is allowed to produce: the NAMES and SHAPES the later stages agree on,
// with no behaviour behind them.
//
// WHY THE ARCHITECT WRITES GO AT ALL. Its documents named types in prose and the
// stages below disagreed about them constantly — run 11's specification called
// handleList as both (w, r) and (w, r, s) across eight sites and blocked the
// ticket; run 15's ARCHITECTURE.md said Fetch(id) and the delivered code used
// Get(id int64), a different name and an invented type. Sections are written by
// agents that cannot see each other, and prose is not a contract they can
// compile against.
//
// THE LARGER PRIZE IS THAT THE TESTS COMPILE. With no declarations, a
// specification fails at the BUILD stage on "undefined: NewStore", so go test
// never type-checks it and vet never runs — which is why run 81's t.Error("%q")
// was invisible to the author's own gate and only surfaced later, inside a file
// the developer was forbidden to edit. Declarations make the author's tests
// type-check, and everything that needs types can then run where the author can
// still act on it.
//
// BUT A STUB IS NOT AN IMPLEMENTATION. An architect that writes bodies has done
// the developer's work, and worse, has done it without seeing the tests. So
// every function body must be empty or a single panic: enough to compile,
// nothing to satisfy a test with.
func OnlyDeclarations(src string) error {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "arch.go", src, parser.SkipObjectResolution)
	if err != nil {
		return fmt.Errorf("%w: it does not parse: %v", ErrNotDeclarations, err)
	}

	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok {
			// A type, const, var or import block is exactly what is wanted.
			continue
		}
		if fn.Body == nil {
			continue // a declaration with no body at all
		}
		if stubBody(fn.Body) {
			continue
		}
		return fmt.Errorf("%w: %s has a body. Declare the signature and leave the body "+
			"empty or `panic(\"not implemented\")` — the developer writes what it does, "+
			"against tests you have not seen", ErrNotDeclarations, fn.Name.Name)
	}
	return nil
}

// stubBody reports whether a body does nothing a test could observe.
//
// EMPTY OR ONE PANIC. A stub that returns a zero value is tempting to allow and
// is not allowed: "return nil" is a behaviour, it is one a test can pass
// against, and a specification written against it would be green before the
// developer started — which the author's own gate would then read as work
// already finished.
func stubBody(b *ast.BlockStmt) bool {
	switch len(b.List) {
	case 0:
		return true
	case 1:
		expr, ok := b.List[0].(*ast.ExprStmt)
		if !ok {
			return false
		}
		call, ok := expr.X.(*ast.CallExpr)
		if !ok {
			return false
		}
		id, ok := call.Fun.(*ast.Ident)
		return ok && id.Name == "panic"
	default:
		return false
	}
}

// IsDeclarationFile reports whether a path is one the architect may write Go
// into: a .go file that is not a test.
//
// TESTS ARE THE AUTHOR'S AND ONLY THE AUTHOR'S. An architect that writes a test
// file has written the specification, from the request alone, before the stage
// whose whole job that is has seen it.
func IsDeclarationFile(p string) bool {
	c := strings.ToLower(path.Clean(p))
	return strings.HasSuffix(c, ".go") && !strings.HasSuffix(c, "_test.go")
}
