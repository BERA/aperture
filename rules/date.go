package rules

import (
	"github.com/frankbardon/aperture/provider"
)

// This file is the RUNTIME half of the date operators — the twin of shape.go,
// which does the same job for the collection operators.
//
// ast.go registers the eight date operators, validates their operand shapes, and
// renders every one of them to the guarded dispatcher `$date`, which compiler.go
// registers. What lives here is the comparison policy itself: the operator table
// (the twin of collOps), the parse of each operand through the canonical date
// value model, and the deny-safe note for an operand that is not a date.
//
// THE POLICY, as the render already commits to it:
//
//   - Ordering is over INSTANTS, never over text. "2026-03-04" and
//     "2026-03-04T00:00:00Z" name the same instant and compare EQUAL, so
//     granularity never affects ordering and string comparison is never the
//     mechanism. Every operator is built on provider.DateValue.Compare.
//   - between is INCLUSIVE AT BOTH ENDS: `between [lo, hi]` is exactly
//     `onOrAfter lo && onOrBefore hi`. Bounds are never reordered — an inverted
//     range matches nothing, which is what the author wrote, and is noted rather
//     than silently denying everything.
//   - sameDay / sameMonth / sameYear are calendar-bucket equality in UTC.
//     provider.DateValue always carries a UTC instant, so there is no conversion
//     here and no zone knob anywhere.
//   - An operand that is absent, the wrong shape, or not a canonical date
//     evaluates the comparison to FALSE and records a note. It never raises —
//     one malformed host-supplied date must not break every Check that touches
//     the field. Same reasoning, and the same shape, as evalCollectionOp.
//
// WHY ABSENT IS FALSE, uniformly, for all eight operators — and why that is not
// the asymmetry shape.go carries. A collection operator can give an absent field
// a meaning (it reads as an EMPTY collection, which the operator's own polarity
// then decides), so shape.go treats absence and wrong-shape differently. A date
// operator has no such reading: there is no empty date, and every date operator
// is POSITIVE — none of the eight matches by the absence of a comparison. So an
// absent operand is deny-safe false for all of them, exactly as a wrong-shaped
// one is, and the two collapse into a single shape-mismatch note whose Actual is
// "absent". The corollary is that no absence can ever produce a MATCH here, which
// is why NoteAbsentField — the "grant on missing data" diagnostic — is never
// recorded by a date operator. If a negative date operator is ever added, that
// stops being true and it needs the note.
//
// PARSING IS ON THE COMPARISON HOT PATH. Every operand is parsed on every
// evaluation, so this file uses provider.DateValueOf (which takes an any,
// absorbs the wrong-type case, and builds no error) and never
// provider.ParseDateValue (the load path, which constructs a coded error). Note
// construction is likewise skipped outright when there is no sink, so a Check —
// which installs none — allocates nothing for a failed parse.

// shapeDate is the expected-shape word a date note reports. It is the date
// operators' analogue of expectation.String() in shape.go: the vocabulary is the
// rule author's, not Go's.
const shapeDate = "date"

// dateForms is the canonical spelling pair a note reports as what was expected.
// It is a constant, folded at compile time from the value model's two layouts, so
// naming it in a note costs no allocation and cannot drift from provider.
const dateForms = provider.DateLayout + " or " + provider.DateTimeLayout

// dateOp is one date operator's RUNTIME contract: whether it consumes a second
// right-hand operand, and what it decides once every operand has parsed.
//
// It is the runtime twin of opSpecs (ast.go), which owns the same operators'
// validation and rendering. TestDateOperatorTablesAgree pins the two tables
// against each other so an operator can never gain a render without a runtime
// policy, or the reverse — the same guarantee TestCollectionOperatorTablesAgree
// gives the collection half.
type dateOp struct {
	// ternary reports whether the operator reads the dispatcher's THIRD
	// path/value pair. Only between does; for every other operator that pair is
	// ""/nil and must not be classified, or a binary comparison would report a
	// spurious note about an operand its author never wrote.
	ternary bool
	// eval decides the comparison. left and right are the first two operands;
	// upper is between's upper bound and is the zero DateValue otherwise.
	eval func(left, right, upper provider.DateValue) bool
}

// dateOps is the closed runtime registry of the date operators. Every entry
// compares INSTANTS through DateValue.Compare — never the canonical strings,
// which sort correctly within one granularity and not across it.
var dateOps = map[string]dateOp{
	// Ordering. Strict and inclusive forms differ only in whether 0 counts.
	OpBefore: {eval: func(l, r, _ provider.DateValue) bool { return l.Compare(r) < 0 }},
	OpAfter:  {eval: func(l, r, _ provider.DateValue) bool { return l.Compare(r) > 0 }},
	OpOnOrBefore: {eval: func(l, r, _ provider.DateValue) bool {
		return l.Compare(r) <= 0
	}},
	OpOnOrAfter: {eval: func(l, r, _ provider.DateValue) bool {
		return l.Compare(r) >= 0
	}},

	// The one ternary operator. INCLUSIVE AT BOTH ENDS by construction: it is
	// spelled as the conjunction of the two inclusive operators above, so the
	// boundary behaviour cannot drift from them.
	OpBetween: {ternary: true, eval: func(l, lo, hi provider.DateValue) bool {
		return l.Compare(lo) >= 0 && l.Compare(hi) <= 0
	}},

	// Calendar-bucket equality, in UTC. These are NOT distance tests: sameMonth
	// asks whether two values fall in the same month OF THE SAME YEAR, not
	// whether they are within a month of each other.
	OpSameDay: {eval: func(l, r, _ provider.DateValue) bool {
		ly, lm, ld := l.Time().Date()
		ry, rm, rd := r.Time().Date()
		return ly == ry && lm == rm && ld == rd
	}},
	OpSameMonth: {eval: func(l, r, _ provider.DateValue) bool {
		ly, lm, _ := l.Time().Date()
		ry, rm, _ := r.Time().Date()
		return ly == ry && lm == rm
	}},
	OpSameYear: {eval: func(l, r, _ provider.DateValue) bool {
		return l.Time().Year() == r.Time().Year()
	}},
}

// evalDateOp is the single runtime home of the date-comparison policy, called by
// the `$date` dispatcher renderDateOp emits.
//
// Its arguments mirror the rendered call exactly: the operator, the notes sink
// (nil on the decision hot path, where nothing is recorded), and three
// path/value pairs — the left operand, the first right operand, and the second
// right operand, which is between's upper bound and is ""/nil for every other
// date operator.
//
// Every operand is classified before anything returns, so one evaluation reports
// every problem it found rather than only the first — the same reason
// evalCollectionOp notes both operands. sink may be nil, in which case nothing is
// recorded and the result is unchanged: notes never influence a decision.
func evalDateOp(op string, sink *NoteCollector, leftPath string, left any, rightPath string, right any, right2Path string, right2 any) bool {
	spec, ok := dateOps[op]
	if !ok {
		// Unreachable through a validated AST: only operators in this table are
		// ever rendered to the date dispatcher. Deny-safe regardless.
		return false
	}

	l, lok := dateOperand(op, sink, leftPath, left)
	r, rok := dateOperand(op, sink, rightPath, right)
	upper, upperOK := provider.DateValue{}, true
	if spec.ternary {
		upper, upperOK = dateOperand(op, sink, right2Path, right2)

		// An INVERTED range is an authoring error, not an empty one: `between
		// ["2026-12-31", "2026-01-01"]` denies every object, and without a note
		// that is indistinguishable from a rule nothing happens to satisfy. It is
		// reported whenever both bounds parsed, independently of the left
		// operand, because the defect is in the rule rather than in the data.
		if rok && upperOK && r.Compare(upper) > 0 {
			if sink != nil {
				sink.Add(Note{Kind: NoteDateBoundsInverted, Op: op, Path: leftPath,
					Expected: "lower bound on or before upper bound"})
			}
			return false
		}
	}

	if !lok || !rok || !upperOK {
		return false
	}
	return spec.eval(l, r, upper)
}

// dateOperand parses one operand through the canonical value model and records
// the deny-safe note when it will not parse. It reports the parsed value and
// whether the comparison may proceed.
//
// Two failures are distinguished, because they point at different people:
//
//   - The value is not a string at all — a number, an array, an object, or an
//     ABSENT field. That is a SHAPE problem, reported as NoteShapeMismatch with
//     the shape vocabulary shape.go already uses, so "expected date, got number"
//     reads the same way "expected collection, got string" does.
//   - The value is a string but not one of the two canonical forms. The shape is
//     right and the content is not, which is a different fix (canonicalise the
//     data, or declare the field as a date in the loader so it is validated at
//     load), so it gets its own kind.
//
// Neither note carries the offending value. A date can be personal data — a
// birth date, a termination date — and Explain output crosses account
// boundaries; the path and the reason are the whole diagnostic.
func dateOperand(op string, sink *NoteCollector, path string, v any) (provider.DateValue, bool) {
	dv, ok := provider.DateValueOf(v)
	if ok || sink == nil {
		// The nil-sink early return is the hot path: Check and Enumerate install
		// no collector, so a failed parse costs a type assertion and a parse and
		// allocates nothing at all.
		return dv, ok
	}
	if _, isString := v.(string); isString {
		sink.Add(Note{Kind: NoteDateInvalid, Op: op, Path: path, Expected: dateForms})
	} else {
		sink.Add(Note{Kind: NoteShapeMismatch, Op: op, Path: path,
			Expected: shapeDate, Actual: shapeName(v)})
	}
	return dv, false
}
