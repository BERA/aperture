package rules

import (
	"context"
	"strings"
	"testing"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/identity"
)

// shapeMetadata is the fixture the shape-policy tests evaluate against: one value
// of every shape a metadata field can take, plus the arrays and objects the
// collection operators are meant for.
func shapeMetadata() map[string]any {
	return map[string]any{
		"tags":     []any{"a", "b"},
		"owner":    map[string]any{"dept": "eng"},
		"title":    "hello",
		"n":        int64(5),
		"flag":     true,
		"empty":    []any{},
		"emptyObj": map[string]any{},
		// No "missing" key: that is the absent-field case.
	}
}

// collectionOperators is the closed list of operators that read a collection.
// exists is the eleventh — it renders to a nil test, has no shape contract, and
// so is deliberately outside collOps.
var collectionOperators = []string{
	OpIn, OpNin, OpHas, OpHasKey, OpHasAll, OpHasAny, OpHasNone, OpSubsetOf,
	OpIsEmpty, OpIsNotEmpty,
}

// nodeForOp builds a well-formed comparison for op that reads path as its
// COLLECTION operand, so one table can drive every operator.
func nodeForOp(t *testing.T, op, path string) *Node {
	t.Helper()
	switch op {
	case OpIn, OpNin:
		// The collection is the RIGHT operand; the left is the element.
		return Compare(op, Lit("a"), Var(path))
	case OpHas, OpHasKey:
		return Compare(op, Var(path), Lit("a"))
	case OpHasAll, OpHasAny, OpHasNone, OpSubsetOf:
		return Compare(op, Var(path), List(Lit("a")))
	case OpIsEmpty, OpIsNotEmpty:
		return Unary(op, Var(path))
	default:
		t.Fatalf("nodeForOp: %q is not a collection operator", op)
		return nil
	}
}

// evalNotes compiles and evaluates n against md, returning the result and the
// notes the evaluation recorded.
func evalNotes(t *testing.T, n *Node, md map[string]any) (bool, []Note) {
	t.Helper()
	compiled, err := NewCompiler().Compile(n)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	got, notes, err := compiled.EvalWithNotes(context.Background(), Input{Object: md})
	if err != nil {
		t.Fatalf("eval must never fail on a shape mismatch (source %s): %v", compiled.Source(), err)
	}
	return got, notes
}

// TestShapeMismatchIsDenySafe is the core of E5-S1: a collection operator applied
// to a value that is not a collection evaluates to FALSE — for EVERY operator,
// including the negative ones — rather than raising APERTURE_RULE_EVAL.
//
// The negative operators are the point. `nin` over a string used to be a runtime
// error; treating the mismatch as an empty collection instead would make it
// return TRUE, i.e. GRANT on mistyped data. Deny-safety says a mismatch never
// matches, whatever the operator's polarity.
func TestShapeMismatchIsDenySafe(t *testing.T) {
	md := shapeMetadata()

	cases := []struct {
		field    string // the wrong-shaped metadata field
		actual   string // the shape a note must report
		ops      []string
		expected string // the shape the operators require
	}{
		{"title", shapeString, []string{OpIn, OpNin, OpHas, OpHasKey}, "collection"},
		{"n", shapeNumber, []string{OpIn, OpNin, OpHas, OpHasKey}, "collection"},
		{"flag", shapeBool, []string{OpIn, OpNin, OpHas, OpHasKey}, "collection"},

		{"title", shapeString, []string{OpHasAll, OpHasAny, OpHasNone, OpSubsetOf}, "array"},
		{"n", shapeNumber, []string{OpHasAll, OpHasAny, OpHasNone, OpSubsetOf}, "array"},
		{"flag", shapeBool, []string{OpHasAll, OpHasAny, OpHasNone, OpSubsetOf}, "array"},
		// An OBJECT where an array was expected: a real shape mismatch for the
		// set-algebra operators, even though it is a perfectly good collection
		// for the membership ones.
		{"owner", shapeObject, []string{OpHasAll, OpHasAny, OpHasNone, OpSubsetOf}, "array"},

		{"n", shapeNumber, []string{OpIsEmpty, OpIsNotEmpty}, "array, object, or string"},
		{"flag", shapeBool, []string{OpIsEmpty, OpIsNotEmpty}, "array, object, or string"},
	}

	for _, tc := range cases {
		for _, op := range tc.ops {
			t.Run(op+" over object."+tc.field, func(t *testing.T) {
				got, notes := evalNotes(t, nodeForOp(t, op, "object."+tc.field), md)
				if got {
					t.Fatalf("%s over a %s must be false (deny-safe), got true", op, tc.actual)
				}
				want := Note{
					Kind: NoteShapeMismatch, Op: op, Path: "object." + tc.field,
					Expected: tc.expected, Actual: tc.actual,
				}
				if len(notes) != 1 || notes[0] != want {
					t.Fatalf("notes = %+v, want exactly [%+v]", notes, want)
				}
				if msg, wantMsg := notes[0].String(),
					"object."+tc.field+": expected "+tc.expected+", got "+tc.actual; msg != wantMsg {
					t.Fatalf("note message = %q, want %q", msg, wantMsg)
				}
			})
		}
	}
}

// TestShapesTheOperatorsAccept is the negative of the table above: the shapes
// each operator legitimately takes record NO note and behave exactly as E4-S1
// specified. It is what keeps the mismatch policy from over-reaching — an object
// is a collection for `has`, and a string has a length for `isEmpty`.
func TestShapesTheOperatorsAccept(t *testing.T) {
	md := shapeMetadata()

	cases := []struct {
		name string
		node *Node
		want bool
	}{
		{"has over an array", Compare(OpHas, Var("object.tags"), Lit("a")), true},
		{"hasKey over an object", Compare(OpHasKey, Var("object.owner"), Lit("dept")), true},
		{"has over an object tests keys", Compare(OpHas, Var("object.owner"), Lit("dept")), true},
		{"in over an object tests keys", Compare(OpIn, Lit("dept"), Var("object.owner")), true},
		{"nin over an object tests keys", Compare(OpNin, Lit("nope"), Var("object.owner")), true},
		{"hasAll over an array", Compare(OpHasAll, Var("object.tags"), List(Lit("a"))), true},
		{"isEmpty over a string", Unary(OpIsEmpty, Var("object.title")), false},
		{"isNotEmpty over a string", Unary(OpIsNotEmpty, Var("object.title")), true},
		{"isEmpty over an object", Unary(OpIsEmpty, Var("object.emptyObj")), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, notes := evalNotes(t, tc.node, md)
			if got != tc.want {
				t.Fatalf("eval = %v, want %v", got, tc.want)
			}
			for _, n := range notes {
				if n.Kind == NoteShapeMismatch {
					t.Fatalf("accepted shape must not record a mismatch note: %+v", n)
				}
			}
		})
	}
}

// TestAbsentFieldRecordsNoMismatch pins that the absent-field case is untouched
// by this policy: nil is not a mismatch, it reads as an empty collection, and
// every operator keeps the E4-S1 result. Uniformity with the mismatch case is
// that NEITHER raises — not that they decide the same way.
func TestAbsentFieldRecordsNoMismatch(t *testing.T) {
	md := shapeMetadata()
	for _, op := range collectionOperators {
		t.Run(op, func(t *testing.T) {
			_, notes := evalNotes(t, nodeForOp(t, op, "object.missing"), md)
			for _, n := range notes {
				if n.Kind == NoteShapeMismatch {
					t.Fatalf("an absent field is not a shape mismatch: %+v", n)
				}
			}
		})
	}
}

// TestAbsentFieldGrantIsNoted covers the second diagnostic on the channel. A
// NEGATIVE operator over a missing field MATCHES — `nin` / `hasNone` / `subsetOf`
// / `isEmpty` all grant for an object that simply lacks the column — which is
// easy to hit and invisible in the decision. The note makes it diagnosable
// without changing the verdict.
func TestAbsentFieldGrantIsNoted(t *testing.T) {
	md := shapeMetadata()

	grants := []string{OpNin, OpHasNone, OpSubsetOf, OpIsEmpty}
	for _, op := range grants {
		t.Run(op+" grants and is noted", func(t *testing.T) {
			got, notes := evalNotes(t, nodeForOp(t, op, "object.missing"), md)
			if !got {
				t.Fatalf("%s over an absent field is expected to match", op)
			}
			want := Note{Kind: NoteAbsentField, Op: op, Path: "object.missing"}
			if len(notes) != 1 || notes[0] != want {
				t.Fatalf("notes = %+v, want exactly [%+v]", notes, want)
			}
			if msg := notes[0].String(); !strings.Contains(msg, "object.missing: absent") {
				t.Fatalf("note message = %q, want it to name the absent path", msg)
			}
		})
	}

	// A positive operator over the same absent field does NOT match, so there is
	// no grant to explain and no note.
	for _, op := range []string{OpIn, OpHas, OpHasKey, OpHasAny, OpIsNotEmpty} {
		t.Run(op+" does not match and is not noted", func(t *testing.T) {
			got, notes := evalNotes(t, nodeForOp(t, op, "object.missing"), md)
			if got {
				t.Fatalf("%s over an absent field is expected not to match", op)
			}
			if len(notes) != 0 {
				t.Fatalf("notes = %+v, want none", notes)
			}
		})
	}
}

// TestNoteNeverCarriesTheValue is the CLAUDE.md non-negotiable: a note reports
// SHAPE AND PATH ONLY. Explain output crosses account boundaries the same way an
// error message does, so a metadata value must never ride along — not in a field,
// not in the rendered message.
//
// The fixture stuffs a marker into every position a value could leak from: a
// scalar field, an object's key AND its value, and an array element.
func TestNoteNeverCarriesTheValue(t *testing.T) {
	const marker = "CROSS-ACCOUNT-SECRET"
	md := map[string]any{
		"title": marker + "-scalar",
		"n":     int64(5),
		"owner": map[string]any{marker + "-key": marker + "-value"},
		"tags":  []any{marker + "-element"},
	}

	// Every operator, over every field — mismatches, matches and absent fields
	// alike — so no path through the policy is exempt.
	fields := []string{"object.title", "object.n", "object.owner", "object.tags", "object.missing"}
	var all []Note
	for _, op := range collectionOperators {
		for _, f := range fields {
			_, notes := evalNotes(t, nodeForOp(t, op, f), md)
			all = append(all, notes...)
		}
	}
	if len(all) == 0 {
		t.Fatal("fixture recorded no notes at all; the assertion would be vacuous")
	}
	for _, n := range all {
		for _, field := range []string{string(n.Kind), n.Rule, n.Op, n.Path, n.Expected, n.Actual, n.String()} {
			if strings.Contains(field, marker) {
				t.Fatalf("note leaked a metadata value: %q in %+v", field, n)
			}
		}
	}
}

// TestGuardIsUnreachableFromARule proves the guard's two internal identifiers are
// structurally out of a rule author's reach — no denylist required, so nothing
// can drift. `$op` carries a character the variable/function-name grammar rejects,
// and `__notes` is not an exposed context root.
func TestGuardIsUnreachableFromARule(t *testing.T) {
	t.Run("the dispatcher cannot be called", func(t *testing.T) {
		err := Call(fnCollectionOp, Lit("in")).Validate()
		if code := aerr.CodeOf(err); code != aerr.APERTURE_RULE_INVALID {
			t.Fatalf("code = %q, want APERTURE_RULE_INVALID (err: %v)", code, err)
		}
	})
	t.Run("the notes sink cannot be read", func(t *testing.T) {
		err := Var(notesVar).Validate()
		if code := aerr.CodeOf(err); code != aerr.APERTURE_RULE_UNKNOWN_VARIABLE {
			t.Fatalf("code = %q, want APERTURE_RULE_UNKNOWN_VARIABLE (err: %v)", code, err)
		}
	})
}

// TestCollectionOperatorTablesAgree keeps the runtime policy (collOps, shape.go)
// and the validate/render registry (opSpecs, ast.go) in lockstep: an operator can
// never gain a shape policy without a render, nor a render without a policy.
func TestCollectionOperatorTablesAgree(t *testing.T) {
	for op := range collOps {
		if _, ok := opSpecs[op]; !ok {
			t.Errorf("collOps has %q but opSpecs does not", op)
		}
	}
	for _, op := range collectionOperators {
		if _, ok := collOps[op]; !ok {
			t.Errorf("collection operator %q has no runtime shape policy in collOps", op)
		}
	}
	if len(collOps) != len(collectionOperators) {
		t.Errorf("collOps has %d entries, want the %d collection operators",
			len(collOps), len(collectionOperators))
	}
	// exists is the deliberate exclusion: a nil test cannot mismatch.
	if _, ok := collOps[OpExists]; ok {
		t.Errorf("exists renders to a nil test and must not carry a shape policy")
	}
}

// TestBackingFunctionsFollowTheSamePolicy covers the un-guarded render path: when
// no operand can be the wrong shape, a comparison still renders to its E4-S1
// backing function, and an author can call those functions directly with a
// NodeCall. Those calls carry no notes sink, but they must not raise either —
// otherwise the policy would depend on how a rule happened to be spelled.
func TestBackingFunctionsFollowTheSamePolicy(t *testing.T) {
	md := shapeMetadata()
	cases := []struct {
		name string
		node *Node
		want bool
	}{
		{"hasAll called directly over a string", Call(fnHasAll, Var("object.title"), List(Lit("a"))), false},
		{"hasNone called directly over a string", Call(fnHasNone, Var("object.title"), List(Lit("a"))), false},
		{"subsetOf called directly over a number", Call(fnSubsetOf, Var("object.n"), List(Lit("a"))), false},
		{"isEmpty called directly over a number", Call(fnIsEmpty, Var("object.n")), false},
		{"isNotEmpty called directly over a bool", Call(fnIsNotEmpty, Var("object.flag")), false},
		{"hasAll called directly over an array still works", Call(fnHasAll, Var("object.tags"), List(Lit("a"))), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := evalNotes(t, tc.node, md)
			if got != tc.want {
				t.Fatalf("eval = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestHotPathCollectsNothing pins the zero-cost property: without a collector in
// the context — which is every Check and every Enumerate — evaluation records
// nothing and decides identically.
func TestHotPathCollectsNothing(t *testing.T) {
	md := shapeMetadata()
	n := Compare(OpHas, Var("object.title"), Lit("a"))
	compiled, err := NewCompiler().Compile(n)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	ctx := context.Background()
	if c := NoteCollectorFrom(ctx); c != nil {
		t.Fatalf("a bare context must carry no collector, got %+v", c)
	}
	got, err := compiled.Eval(ctx, Input{Object: md})
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	if got {
		t.Fatalf("shape mismatch must still be false on the hot path")
	}

	// With a collector installed, the SAME evaluation publishes to it — and still
	// decides the same way.
	noteCtx, collector := WithNoteCollector(ctx)
	got2, err := compiled.Eval(noteCtx, Input{Object: md})
	if err != nil {
		t.Fatalf("eval with collector: %v", err)
	}
	if got2 != got {
		t.Fatalf("a collector changed the decision: %v -> %v", got, got2)
	}
	if collector.Len() != 1 {
		t.Fatalf("collector recorded %d notes, want 1", collector.Len())
	}
}

// TestNoteCollector covers the channel's own contract: nil-safety (the hot path
// carries a nil sink), deduplication (a rule re-evaluated per grant must not
// repeat itself), and copy-on-read.
func TestNoteCollector(t *testing.T) {
	var nilCollector *NoteCollector
	nilCollector.Add(Note{Kind: NoteShapeMismatch})
	if nilCollector.Len() != 0 || nilCollector.Notes() != nil {
		t.Fatal("a nil collector must be inert")
	}

	c := &NoteCollector{}
	n1 := Note{Kind: NoteShapeMismatch, Op: OpHas, Path: "object.title", Expected: "collection", Actual: "string"}
	n2 := Note{Kind: NoteAbsentField, Op: OpNin, Path: "object.missing"}
	c.Add(n1, n1, n2, n1)
	if got := c.Notes(); len(got) != 2 || got[0] != n1 || got[1] != n2 {
		t.Fatalf("notes = %+v, want [n1 n2] deduplicated in order", got)
	}

	// Notes returns a copy: mutating it must not reach the collector.
	got := c.Notes()
	got[0] = Note{}
	if c.Notes()[0] != n1 {
		t.Fatal("Notes must return a copy")
	}
}

// TestEngineStampsTheRuleOnNotes proves the engine seam: notes published through
// the context collector name the rule reference that produced them, which is what
// tells two rules' diagnostics apart in one Explain trace.
func TestEngineStampsTheRuleOnNotes(t *testing.T) {
	src := MapSource{"shapecheck": {
		Name: "shapecheck",
		AST:  Compare(OpHas, Var("object.title"), Lit("a")),
	}}
	fetcher := fakeFetcher{"account:acme/document:1": {"title": "hello"}}
	eng := NewEngine(src, fetcher)

	ctx, collector := WithNoteCollector(context.Background())
	object := identity.MustParse("account:acme/document:1")
	selected, err := eng.Selected(ctx, "shapecheck", object, "acme", "user", "alice", "read")
	if err != nil {
		t.Fatalf("Selected: %v", err)
	}
	if selected {
		t.Fatal("a shape mismatch must not select the object")
	}
	notes := collector.Notes()
	if len(notes) != 1 {
		t.Fatalf("notes = %+v, want exactly one", notes)
	}
	if notes[0].Rule != "shapecheck" {
		t.Fatalf("note rule = %q, want %q", notes[0].Rule, "shapecheck")
	}
	if notes[0].Kind != NoteShapeMismatch || notes[0].Path != "object.title" || notes[0].Actual != shapeString {
		t.Fatalf("note = %+v, want the shape mismatch on object.title", notes[0])
	}
}
