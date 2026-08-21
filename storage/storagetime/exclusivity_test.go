package storagetime_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// storageRoot is the storage layer this gate governs, relative to this package's
// directory. storagetime itself is the one exemption.
const (
	storageRoot        = ".."
	storageDisplayPath = "storage/"
	thisPackageDir     = "storagetime"

	// filesFloor is an anti-vacuity floor. If a refactor moves the backends out
	// from under storage/, this gate would otherwise pass by scanning nothing.
	filesFloor = 3
)

// bannedCalls are the conversions between a time.Time and an integer count of
// seconds/millis/micros/nanos. Every one of them either overflows on the zero
// time, silently truncates, or hands back the Unix epoch where the caller meant
// "unset" — which is exactly the set of mistakes storagetime.Encode/Decode
// exist to make unrepeatable.
var bannedCalls = map[string]string{
	"UnixNano":  "use storagetime.Encode — UnixNano overflows on the zero time",
	"UnixMicro": "storage instants are nanoseconds; use storagetime.Encode",
	"UnixMilli": "storage instants are nanoseconds; use storagetime.Encode",
	"Unix":      "use storagetime.Encode/Decode — time.Unix(0, 0) is the epoch, not the unset sentinel",
}

// TestStorageTimeIsTheOnlyTimeIntegerConversion enforces the contract that the
// encode/decode pair is the SINGLE place in the storage layer where a time.Time
// becomes an integer and back. It parses every non-test Go file under storage/
// with go/ast (the house rule: parse, never grep) and fails on any other
// seconds/nanos conversion.
//
// It fails, rather than skips, when the tree it governs is missing.
func TestStorageTimeIsTheOnlyTimeIntegerConversion(t *testing.T) {
	root, err := filepath.Abs(storageRoot)
	if err != nil {
		t.Fatalf("resolve %s: %v", storageDisplayPath, err)
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("cannot read %s (%v). This gate governs that tree; if it moved, "+
			"move the gate with it — do not delete it.", storageDisplayPath, err)
	}

	var (
		scanned    int
		violations []string
	)
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == thisPackageDir && filepath.Dir(path) == root {
				return fs.SkipDir // storagetime is the exemption
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		scanned++

		fset := token.NewFileSet()
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			rel = path
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			why, banned := bannedCalls[sel.Sel.Name]
			if !banned {
				return true
			}
			// time.Unix(...) and t.Unix() are both conversions; so is any other
			// receiver spelling. Report the position either way.
			pos := fset.Position(call.Lparen)
			violations = append(violations, storageDisplayPath+rel+":"+
				strconv.Itoa(pos.Line)+": "+sel.Sel.Name+"(...) — "+why)
			return true
		})
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk %s: %v", storageDisplayPath, walkErr)
	}

	if scanned < filesFloor {
		t.Fatalf("scanned only %d non-test Go files under %s, expected at least %d. "+
			"Either the storage layer moved or this gate is passing vacuously.",
			scanned, storageDisplayPath, filesFloor)
	}

	if len(violations) > 0 {
		sort.Strings(violations)
		t.Fatalf("a time.Time became an integer outside storage/storagetime:\n  %s\n\n"+
			"storagetime.Encode/Decode own the zero mapping (time.Time{} <-> 0) and the "+
			"representable-range rejection. A conversion anywhere else re-opens both bugs.",
			strings.Join(violations, "\n  "))
	}
}
