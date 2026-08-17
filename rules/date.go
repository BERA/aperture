package rules

// This file is the RUNTIME half of the date operators — the twin of shape.go,
// which does the same job for the collection operators.
//
// The authoring and compilation surface is complete: ast.go registers the nine
// date operators, validates their operand shapes, and renders every one of them
// to the guarded dispatcher `$date`, which compiler.go registers. What lands
// here next is the comparison policy itself: the operator table (the twin of
// collOps), the parse of each operand through the canonical date value model,
// and the deny-safe note for an operand that is not a date.
//
// THE POLICY the table will implement, stated now because the render already
// commits to it:
//
//   - Ordering is over INSTANTS, never over text. "2026-03-04" and
//     "2026-03-04T00:00:00Z" name the same instant and compare EQUAL, so
//     granularity never affects ordering and string comparison is never the
//     mechanism.
//   - between is INCLUSIVE AT BOTH ENDS: `between [lo, hi]` is exactly
//     `onOrAfter lo && onOrBefore hi`.
//   - sameDay / sameMonth / sameYear are calendar-bucket equality in UTC.
//   - An operand that is absent, the wrong shape, or not a canonical date
//     evaluates the comparison to FALSE and records a note. It never raises —
//     one malformed host-supplied date must not break every Check that touches
//     the field. Same reasoning, and the same shape, as evalCollectionOp.

// evalDateOp is the single runtime home of the date-comparison policy, called by
// the `$date` dispatcher renderDateOp emits.
//
// Its arguments mirror the rendered call exactly: the operator, the notes sink
// (nil on the decision hot path, where nothing is recorded), and three
// path/value pairs — the left operand, the first right operand, and the second
// right operand, which is between's upper bound and is ""/nil for every other
// date operator.
//
// It returns false for every operator until the comparison policy above lands.
// False is the deny-safe answer and the one this seam must default to: a date
// comparison that cannot be decided must not grant.
func evalDateOp(op string, sink *NoteCollector, leftPath string, left any, rightPath string, right any, right2Path string, right2 any) bool {
	_, _, _, _, _, _, _, _ = op, sink, leftPath, left, rightPath, right, right2Path, right2
	return false
}
