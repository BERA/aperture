package rules

import (
	"reflect"
)

// This file is the RUNTIME SHAPE POLICY for the collection operators — the
// deny-safe half of the evaluation-notes channel (notes.go).
//
// THE POLICY, in one sentence: a collection operator applied to a value that is
// not a collection evaluates to FALSE and records a note; it never raises.
//
// Why false and not an error. expr-lang's own semantics are inconsistent:
// `"a" in object.missing` (absent field) is false with no error, while
// `"a" in object.title` (a string) is the runtime error
// `operator "in" not defined on string`. Inheriting that means one mistyped
// metadata field — from a host-implemented ObjectProvider or the principal
// attribute bag, neither of which any loader validates — breaks EVERY Check that
// touches it. Deny-safety says a decision engine must degrade to "no" rather than
// to "error", so a wrong-shaped operand denies.
//
// Why false and not "treat it as empty". Treating a mismatch as an empty
// collection would be uniform with the absent-field case, but it would make the
// NEGATIVE operators (nin, hasNone, subsetOf) GRANT on a mistyped field. The
// deny-safe reading of "wrong shape" is that the comparison did not match, full
// stop, whatever the operator's polarity. So: absent field keeps its per-operator
// semantics (nil reads as empty), wrong shape is uniformly false.
//
// Why a note. Silent-false is how access-control bugs hide. Every mismatch is
// recorded with the variable path, the shape expected and the shape found, and
// Explain surfaces it on every decision surface. An absent operand that produced
// a MATCH is recorded too (NoteAbsentField): that is the "negative operator
// grants on missing data" case, which is invisible otherwise.
//
// Shape names are deliberately the JSON vocabulary (array/object/string/number/
// bool/absent) rather than Go type names: the notes are read by rule authors, who
// think in the metadata model, not in Go.

// Shape names a note reports. These are shapes, never values.
const (
	shapeArray  = "array"
	shapeObject = "object"
	shapeString = "string"
	shapeNumber = "number"
	shapeBool   = "bool"
	shapeAbsent = "absent"
)

// expectation is the runtime shape contract one operand of a collection operator
// must satisfy. It is the RUNTIME counterpart to rightShape in ast.go, which
// constrains the operand's AST NODE at validation time; this constrains the VALUE
// the node produces at evaluation time.
type expectation uint8

const (
	// expectNone places no shape constraint: the operand is an element being
	// tested for membership, and any value (including nil) is legitimate.
	expectNone expectation = iota
	// expectCollection accepts an array or an object. Membership over an object
	// tests its KEYS, which is exactly what expr's native `in` does — so has,
	// hasKey, in and nin keep working over both shapes.
	expectCollection
	// expectArray accepts an array only. The set-algebra operators compare
	// elements pairwise, and an object's key set is not what an author writing
	// `hasAll` means.
	expectArray
	// expectSizable accepts anything with a length: an array, an object, or a
	// string. Only isEmpty / isNotEmpty ask for it.
	expectSizable
)

// String is how the expectation appears in a note ("expected collection, ...").
func (e expectation) String() string {
	switch e {
	case expectCollection:
		return "collection"
	case expectArray:
		return "array"
	case expectSizable:
		return "array, object, or string"
	default:
		return "any"
	}
}

// operand is one collection operand normalized for the policy: the shape it
// turned out to have, whether it satisfied the operator's expectation, and — when
// it did — its members and length.
//
// members holds an array's ELEMENTS or an object's KEYS, mirroring expr's `in`.
// Object keys arrive in map-iteration (random) order, which is safe because every
// operator consuming members asks only membership or emptiness questions — never
// anything order-dependent — so evaluation stays deterministic.
type operand struct {
	path    string
	raw     any
	shape   string
	absent  bool
	ok      bool
	members []any
	length  int
}

// classify normalizes v against want. A nil value (an ABSENT metadata field, and
// what optional chaining yields for a missing intermediate) always satisfies the
// expectation and reads as an empty collection — that is the absent-field
// semantics E4-S1 established, and this change does not touch it.
func classify(path string, v any, want expectation) operand {
	o := operand{path: path, raw: v, shape: shapeName(v)}
	if want == expectNone {
		o.ok = true
		o.absent = o.shape == shapeAbsent
		return o
	}
	if o.shape == shapeAbsent {
		o.ok, o.absent = true, true
		return o
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		o.ok = want == expectCollection || want == expectArray || want == expectSizable
		o.length = rv.Len()
		if o.ok {
			o.members = make([]any, rv.Len())
			for i := range o.members {
				o.members[i] = rv.Index(i).Interface()
			}
		}
	case reflect.Map:
		o.ok = want == expectCollection || want == expectSizable
		o.length = rv.Len()
		if o.ok {
			keys := rv.MapKeys()
			o.members = make([]any, len(keys))
			for i, k := range keys {
				o.members[i] = k.Interface()
			}
		}
	case reflect.String:
		o.ok = want == expectSizable
		o.length = rv.Len()
	}
	return o
}

// shapeName reports a value's shape in the metadata model's vocabulary. It never
// reports the value itself, and never a Go type name for a shape the model has a
// word for.
func shapeName(v any) string {
	if v == nil {
		return shapeAbsent
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Pointer, reflect.Interface:
		if rv.IsNil() {
			return shapeAbsent
		}
		return shapeName(rv.Elem().Interface())
	case reflect.Slice, reflect.Array:
		return shapeArray
	case reflect.Map:
		return shapeObject
	case reflect.String:
		return shapeString
	case reflect.Bool:
		return shapeBool
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return shapeNumber
	default:
		// A shape the metadata model has no word for (a struct, a func). The KIND
		// is reported, never the value.
		return rv.Kind().String()
	}
}

// collOp is one collection operator's RUNTIME contract: what shape each operand
// must have, and what the operator decides once both shapes hold.
//
// It is the runtime twin of opSpecs (ast.go), which owns the same operators'
// validation and rendering. TestCollectionOperatorTablesAgree pins the two tables
// against each other so an operator can never gain a render without a runtime
// policy, or the reverse.
type collOp struct {
	left  expectation
	right expectation
	eval  func(l, r operand) bool
}

// collOps is the closed runtime registry of the collection operators. exists is
// deliberately absent: it renders to a nil test, which has no shape requirement
// and cannot mismatch.
var collOps = map[string]collOp{
	// Element-in-collection. The ELEMENT (left) is unconstrained; the COLLECTION
	// (right) must be one.
	OpIn: {left: expectNone, right: expectCollection,
		eval: func(l, r operand) bool { return containsValue(r.members, l.raw) }},
	OpNin: {left: expectNone, right: expectCollection,
		eval: func(l, r operand) bool { return !containsValue(r.members, l.raw) }},

	// Collection-has-element: the same test read collection-first.
	OpHas: {left: expectCollection, right: expectNone,
		eval: func(l, r operand) bool { return containsValue(l.members, r.raw) }},
	OpHasKey: {left: expectCollection, right: expectNone,
		eval: func(l, r operand) bool { return containsValue(l.members, r.raw) }},

	// Set algebra: both operands are arrays.
	OpHasAll: {left: expectArray, right: expectArray, eval: func(l, r operand) bool {
		for _, it := range r.members {
			if !containsValue(l.members, it) {
				return false
			}
		}
		return true
	}},
	OpHasAny: {left: expectArray, right: expectArray, eval: func(l, r operand) bool {
		for _, it := range r.members {
			if containsValue(l.members, it) {
				return true
			}
		}
		return false
	}},
	OpHasNone: {left: expectArray, right: expectArray, eval: func(l, r operand) bool {
		for _, it := range r.members {
			if containsValue(l.members, it) {
				return false
			}
		}
		return true
	}},
	OpSubsetOf: {left: expectArray, right: expectArray, eval: func(l, r operand) bool {
		for _, v := range l.members {
			if !containsValue(r.members, v) {
				return false
			}
		}
		return true
	}},

	// Emptiness: anything with a length.
	OpIsEmpty: {left: expectSizable, right: expectNone,
		eval: func(l, _ operand) bool { return l.length == 0 }},
	OpIsNotEmpty: {left: expectSizable, right: expectNone,
		eval: func(l, _ operand) bool { return l.length != 0 }},
}

// evalCollectionOp is the single runtime home of the policy: it classifies both
// operands, records a note for each shape mismatch, returns false when either
// mismatched, and otherwise runs the operator — recording an absent-field note
// when the match was produced by a MISSING field rather than by the data.
//
// sink may be nil (the decision hot path), in which case nothing is recorded and
// the result is unchanged: notes never influence a decision.
func evalCollectionOp(op string, sink *NoteCollector, leftPath string, left any, rightPath string, right any) bool {
	spec, ok := collOps[op]
	if !ok {
		// Unreachable through a validated AST: only operators in this table are
		// ever rendered to a guarded call. Deny-safe regardless.
		return false
	}
	l := classify(leftPath, left, spec.left)
	r := classify(rightPath, right, spec.right)

	mismatch := false
	if !l.ok {
		sink.Add(Note{Kind: NoteShapeMismatch, Op: op, Path: l.path,
			Expected: spec.left.String(), Actual: l.shape})
		mismatch = true
	}
	if !r.ok {
		sink.Add(Note{Kind: NoteShapeMismatch, Op: op, Path: r.path,
			Expected: spec.right.String(), Actual: r.shape})
		mismatch = true
	}
	if mismatch {
		return false
	}

	result := spec.eval(l, r)
	if result {
		// A match produced by an absent field is a grant that the object's data
		// never justified — the `nin` / `hasNone` / `subsetOf` / `isEmpty`
		// surprise. Record it so Explain can show it.
		if l.absent && spec.left != expectNone {
			sink.Add(Note{Kind: NoteAbsentField, Op: op, Path: l.path})
		}
		if r.absent && spec.right != expectNone {
			sink.Add(Note{Kind: NoteAbsentField, Op: op, Path: r.path})
		}
	}
	return result
}
