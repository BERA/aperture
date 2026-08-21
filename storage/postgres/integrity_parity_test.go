package postgres

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The three integrity refusals SQL cannot express are enforced in Go, once per
// backend, and their WORDING is a contract rather than prose: storagetest reads
// each refusal and asserts it names the edge that objected. Three hand-written
// copies of those sentences now exist (sqlite, postgres, and — spelled
// differently, over Go values rather than SQL — memory), and until this file
// nothing stopped two of them drifting apart. A drifted copy does not fail
// loudly: it fails as a conformance case on a backend CI cannot run.
//
// This gate diffs the two SQL backends' copies at the source level, in the house
// style — parse with a real parser, never grep (the precedent is
// sqlprovider/values_test.go and internal/schemagate). It
// compares only the parts that MUST be identical:
//
//   - every constraint(op, format, ...) format string, keyed by the function it
//     is raised from, in order; and
//   - subjectTable's kind -> (table, noun) mapping, which supplies the %s the
//     messages interpolate.
//
// It deliberately does NOT compare the queries: `?` versus `$1`, the schema
// qualifier and COLLATE "C" are exactly the dialect differences this backend
// exists to have.
//
// It FAILS, rather than skips, when either file is missing. A parity gate that
// silently stops finding what it governs is not a gate.

const (
	postgresIntegrityFile = "integrity.go"
	sqliteIntegrityFile   = "../sqlite/integrity.go"

	// refusalFloor is the anti-vacuity floor. There are five constraint() calls
	// across the four checks — one each in checkAccountRef, checkGrantSubject and
	// checkSubjectUncited, two in checkAccountUncited (a membership pins an
	// account, and so does a grant). A scan that suddenly finds none would
	// otherwise pass by comparing two empty maps.
	refusalFloor = 5
)

// refusalsIn returns every constraint(op, format, ...) format string in a file,
// keyed by the enclosing function's name and kept in source order within it.
func refusalsIn(t *testing.T, path string) map[string][]string {
	t.Helper()
	fset := token.NewFileSet()
	src, err := os.ReadFile(path)
	if err != nil {
		abs, _ := filepath.Abs(path)
		t.Fatalf("cannot read %s (%s): %v\n"+
			"This gate holds the two SQL backends' referential refusals in lockstep. "+
			"If the file moved, move the gate with it — do not delete it.", path, abs, err)
	}
	file, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	out := map[string][]string{}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			ident, ok := call.Fun.(*ast.Ident)
			if !ok || ident.Name != "constraint" || len(call.Args) < 2 {
				return true
			}
			lit, ok := call.Args[1].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				t.Errorf("%s: %s raises a refusal whose format is not a literal string; "+
					"this gate can only hold literal wording in lockstep", path, fn.Name.Name)
				return true
			}
			format, err := strconv.Unquote(lit.Value)
			if err != nil {
				t.Fatalf("%s: unquote %s: %v", path, lit.Value, err)
			}
			out[fn.Name.Name] = append(out[fn.Name.Name], format)
			return true
		})
	}
	return out
}

// stringsIn returns every string literal inside the named function, in source
// order. It is how subjectTable's mapping is compared without depending on how
// the switch is spelled.
func stringsIn(t *testing.T, path, funcName string) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != funcName {
			continue
		}
		var out []string
		ast.Inspect(fn, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil {
				t.Fatalf("%s: unquote %s: %v", path, lit.Value, err)
			}
			out = append(out, s)
			return true
		})
		return out
	}
	t.Fatalf("%s declares no function %s", path, funcName)
	return nil
}

// TestIntegrityRefusalsMatchTheSQLiteBackend is the gate this backend's
// duplicated integrity.go buys its existence with.
func TestIntegrityRefusalsMatchTheSQLiteBackend(t *testing.T) {
	pg := refusalsIn(t, postgresIntegrityFile)
	lite := refusalsIn(t, sqliteIntegrityFile)

	count := func(m map[string][]string) int {
		n := 0
		for _, v := range m {
			n += len(v)
		}
		return n
	}
	if got := count(lite); got < refusalFloor {
		t.Fatalf("found only %d refusal messages in %s (floor %d) — the scan is broken, not the backend",
			got, sqliteIntegrityFile, refusalFloor)
	}
	if got := count(pg); got < refusalFloor {
		t.Fatalf("found only %d refusal messages in %s (floor %d) — the scan is broken, not the backend",
			got, postgresIntegrityFile, refusalFloor)
	}

	names := map[string]bool{}
	for k := range pg {
		names[k] = true
	}
	for k := range lite {
		names[k] = true
	}
	sorted := make([]string, 0, len(names))
	for k := range names {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)

	for _, name := range sorted {
		p, l := pg[name], lite[name]
		if len(p) == 0 {
			t.Errorf("%s raises %d refusal(s) in storage/sqlite and none in storage/postgres:\n  %s",
				name, len(l), strings.Join(l, "\n  "))
			continue
		}
		if len(l) == 0 {
			t.Errorf("%s raises %d refusal(s) in storage/postgres and none in storage/sqlite:\n  %s",
				name, len(p), strings.Join(p, "\n  "))
			continue
		}
		if strings.Join(p, "\x00") == strings.Join(l, "\x00") {
			continue
		}
		t.Errorf("%s refuses in different words in the two backends — storagetest asserts this text, "+
			"so the Postgres conformance run would fail on a message nobody reads until then\n"+
			"  storage/sqlite:\n    %s\n  storage/postgres:\n    %s",
			name, strings.Join(l, "\n    "), strings.Join(p, "\n    "))
	}
}

// TestSubjectTableMappingMatchesTheSQLiteBackend covers the other half: the
// messages above interpolate subjectTable's table and noun, so identical format
// strings over a different mapping would still refuse in different words.
func TestSubjectTableMappingMatchesTheSQLiteBackend(t *testing.T) {
	pg := stringsIn(t, postgresIntegrityFile, "subjectTable")
	lite := stringsIn(t, sqliteIntegrityFile, "subjectTable")
	if len(lite) < 6 {
		t.Fatalf("found only %d strings in storage/sqlite's subjectTable; the scan is broken", len(lite))
	}
	if strings.Join(pg, ",") != strings.Join(lite, ",") {
		t.Errorf("subjectTable maps subject kinds differently in the two backends\n"+
			"  storage/sqlite:   %v\n  storage/postgres: %v", lite, pg)
	}
}

// TestSubjectTableIsUnqualified pins the one thing the parity gate above cannot
// see: subjectTable's table name is used BOTH as a refusal word and as half of a
// table identifier, and only the identifier may carry the schema qualifier. A
// qualified name here would make the refusal text vary with deployment
// configuration — which storagetest reads.
func TestSubjectTableIsUnqualified(t *testing.T) {
	for _, name := range stringsIn(t, postgresIntegrityFile, "subjectTable") {
		if strings.Contains(name, ".") {
			t.Errorf("subjectTable returns %q, which carries a schema qualifier; "+
				"the refusal message must name the bare table", name)
		}
	}
}
