package storagetest_test

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// These gates protect the conformance suite from the two changes that would make
// it stop proving anything while still passing:
//
//   - a tolerance, rounding, or truncation knob in a timestamp comparison, which
//     would let a backend that loses precision satisfy the whole suite;
//   - a backend-conditional assertion, which would let two backends disagree
//     while the suite that exists to prove them identical stays green.
//
// Both are enforced the house way: parse the source with a real parser and walk
// the AST, never grep. Both FAIL rather than skip when the tree they govern is
// missing.

const (
	// suiteFile is the conformance suite itself.
	suiteFile = "storagetest.go"

	// canonicalNormTime is the exact body normTime must have. It normalizes the
	// location and strips the monotonic reading; Round(0) is documented by the
	// standard library as rounding nothing at all. Anything else — a Truncate, a
	// non-zero Round, a configurable precision — is a tolerance knob.
	canonicalNormTime = "return t.UTC().Round(0)"

	// backendPrefix covers the storage backends. The suite must not import one:
	// a case that cannot name a backend cannot branch on one.
	backendPrefix = "github.com/frankbardon/aperture/storage/"

	// scannedFloor is an anti-vacuity floor. If the suite is split or moved,
	// these gates must fail loudly rather than pass by inspecting nothing.
	scannedFloor = 1
)

// suiteFiles parses every non-test Go file of this package, failing if the
// package or the suite file has moved.
func suiteFiles(t *testing.T) (*token.FileSet, map[string]*ast.File) {
	t.Helper()

	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("resolve package directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, suiteFile)); err != nil {
		t.Fatalf("cannot read %s (%v). These gates govern the conformance suite; "+
			"if it moved, move the gates with it — do not delete them.", suiteFile, err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}

	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", name, perr)
		}
		files[name] = f
	}

	if len(files) < scannedFloor {
		t.Fatalf("parsed %d non-test Go files in this package, expected at least %d. "+
			"Either the suite moved or this gate is passing vacuously.", len(files), scannedFloor)
	}
	if _, ok := files[suiteFile]; !ok {
		t.Fatalf("%s is not part of this package's non-test sources", suiteFile)
	}
	return fset, files
}

// TestNormTimeIsExactlyUTCRound0 pins the one normalization the suite applies to
// a stored instant. normTime exists to make reflect.DeepEqual compare instants
// rather than location pointers and monotonic readings — nothing more. The
// moment it truncates, every backend passes every timestamp case regardless of
// the precision it actually stores.
func TestNormTimeIsExactlyUTCRound0(t *testing.T) {
	fset, files := suiteFiles(t)

	var fn *ast.FuncDecl
	ast.Inspect(files[suiteFile], func(n ast.Node) bool {
		d, ok := n.(*ast.FuncDecl)
		if ok && d.Recv == nil && d.Name.Name == "normTime" {
			fn = d
			return false
		}
		return true
	})
	if fn == nil {
		t.Fatalf("normTime is not declared in %s. The suite's timestamp comparisons "+
			"depend on it; if it was renamed, rename it here too — do not drop this gate.", suiteFile)
	}
	if fn.Body == nil || len(fn.Body.List) != 1 {
		t.Fatalf("normTime must be a single statement, got %d. Anything more is room "+
			"for a precision knob.", len(fn.Body.List))
	}

	var buf strings.Builder
	if err := printer.Fprint(&buf, fset, fn.Body.List[0]); err != nil {
		t.Fatalf("render normTime body: %v", err)
	}
	if got := strings.TrimSpace(buf.String()); got != canonicalNormTime {
		t.Fatalf("normTime body is %q, want exactly %q.\n\n"+
			"Round(0) strips the monotonic reading and rounds nothing. Truncation, "+
			"tolerance, or a precision knob here would let a backend that cannot store "+
			"nanoseconds pass the whole conformance suite — which is the one thing the "+
			"suite exists to prevent.", got, canonicalNormTime)
	}
}

// TestConformanceSuiteHasNoPrecisionKnob rejects truncation and non-zero
// rounding anywhere in the suite, not just inside normTime. A tolerance
// introduced at a single call site is just as fatal to the contract as one in
// the shared helper.
func TestConformanceSuiteHasNoPrecisionKnob(t *testing.T) {
	fset, files := suiteFiles(t)

	var violations []string
	for name, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pos := fset.Position(call.Lparen)
			where := name + ":" + strconv.Itoa(pos.Line) + ": "
			switch sel.Sel.Name {
			case "Truncate":
				violations = append(violations, where+
					"Truncate(...) — truncating a stored instant is exactly the loss this suite must detect")
			case "Round":
				if len(call.Args) != 1 || !isZeroLiteral(call.Args[0]) {
					violations = append(violations, where+
						"Round(...) with a non-zero argument — rounding a stored instant hides precision loss")
				}
			}
			return true
		})
	}
	if len(violations) > 0 {
		sort.Strings(violations)
		t.Fatalf("the conformance suite grew a precision knob:\n  %s\n\n"+
			"Stored instants are int64 nanoseconds and are compared exactly. A suite "+
			"that compares approximately stops proving the backends agree.",
			strings.Join(violations, "\n  "))
	}
}

// TestConformanceSuiteIsBackendBlind proves the suite cannot tell which backend
// it is running against, so no assertion can be conditional on one.
//
// A Factory hands the suite a model.Storage and nothing else. There are exactly
// two ways a case could recover the concrete backend from it — importing the
// backend package, or type-asserting/type-switching on the interface value — and
// this gate closes both. With neither available, the identity of the backend is
// not an expression the suite can write, so a carve-out cannot be spelled at
// all. (Backend names remain fair game in comments and failure messages: prose
// cannot branch.)
func TestConformanceSuiteIsBackendBlind(t *testing.T) {
	fset, files := suiteFiles(t)

	var violations []string
	for name, f := range files {
		report := func(n ast.Node, msg string) {
			pos := fset.Position(n.Pos())
			violations = append(violations, name+":"+strconv.Itoa(pos.Line)+": "+msg)
		}

		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				continue
			}
			if strings.HasPrefix(path, backendPrefix) {
				report(imp, "imports "+path+" — the suite must not be able to name a backend")
			}
		}

		ast.Inspect(f, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.TypeAssertExpr:
				report(node, "type assertion — the only reason to assert on a model.Storage here is to single out a backend")
			case *ast.TypeSwitchStmt:
				report(node, "type switch — the only reason to switch on a model.Storage here is to single out a backend")
			}
			return true
		})
	}
	if len(violations) > 0 {
		sort.Strings(violations)
		t.Fatalf("the conformance suite can tell its backends apart:\n  %s\n\n"+
			"storagetest is one contract every backend satisfies identically. A "+
			"backend-conditional assertion means the backends have diverged and the "+
			"suite is hiding it — fix the backend, not the assertion.",
			strings.Join(violations, "\n  "))
	}
}

// isZeroLiteral reports whether an expression is the untyped constant 0.
func isZeroLiteral(e ast.Expr) bool {
	lit, ok := e.(*ast.BasicLit)
	return ok && lit.Kind == token.INT && lit.Value == "0"
}
