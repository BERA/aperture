package rules

import (
	"encoding/json"
	"strconv"
	"time"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/provider"
)

// This file is the RELATIVE DATE: the AST node that names a date relative to the
// decision's reference instant instead of pinning a calendar day.
//
// WHY A NODE RATHER THAN AN EXPRESSION. "Touched in the last 90 days" has to keep
// meaning that tomorrow, so the rule cannot carry a literal cutoff. The obvious
// way to give an author that power is to expose the instant as a variable and let
// them do arithmetic on it — and that is exactly what this design refuses. See
// now.go: reflective METHOD calls survive expr.DisableAllBuiltins, so an exposed
// `NOW` root would make `NOW.AddDate(0, -3, 0)` and `NOW.Unix()` well-formed var
// paths, i.e. an unclamped calendar walk reachable from any rule. allowedRoots is
// therefore byte-unchanged and this node renders LITERAL ARGUMENTS to a
// $-prefixed dispatcher, reading the instant as `__now` the way $date already
// reads `__notes`.
//
// The author's whole vocabulary is three closed sets, checked in the AST rather
// than resolved as free strings at runtime:
//
//	anchor  NOW | TODAY
//	offset  n (a whole number) x one of seven units
//	snap    none | start/end of year | quarter | month | week | day
//
// SIGN CONVENTION: a NEGATIVE offset goes into the PAST. "three months ago" is
// n = -3, unit = months. This is stated once, here, because a sign error in a
// date rule is silent and grants access backwards in time; every surface
// (builder, JSON, render, editor control) uses the same convention, and nothing
// anywhere carries a separate "direction" field that could disagree with it.
//
// EVERY FIELD IS ALWAYS PRESENT. anchor, n, unit, and snap are each required and
// each validated against its set, "no offset" is spelled n = 0 (with whatever
// unit — zero of anything is the anchor itself), and "no snap" is the vocabulary
// member "none" rather than an absent key. One uniform rule per field means one
// uniform check on both sides of the Go/JS contract and four editor controls that
// are never empty, and it keeps the JSON minimal without making absence
// meaningful.
//
// TODAY IS A DISTINCT ANCHOR, NOT SUGAR. It is defined as NOW snapped to the
// start of its UTC day, and it is PERSISTED as itself: an author who chose TODAY
// reads back TODAY, never a normalised `NOW + snap: startOfDay`. A snap composes
// ON TOP of it, so `anchor: TODAY, snap: startOfYear` is legal and resolves to
// the start of the year — the wider snap simply subsumes the anchor's
// start-of-day.

// The two anchors — the point a relative date is measured from. Both are
// resolved from the decision's reference instant (now.go), never from a wall
// clock read during evaluation.
//
// They are spelled in upper case because they name CONSTANTS, not fields: the
// value is a closed-set token the renderer quotes as a string argument, and it is
// deliberately not, and can never become, a dereferenceable variable root.
const (
	// AnchorNow is the reference instant itself, to the second, in UTC.
	AnchorNow = "NOW"
	// AnchorToday is the reference instant snapped to the start of its UTC day —
	// midnight on the calendar day the decision happened.
	AnchorToday = "TODAY"
)

// The seven offset units.
//
// The first three are CALENDAR units: their length depends on where in the
// calendar they are applied, so offsetting by them CLAMPS at month end rather
// than normalising (Go's time.AddDate would turn 2026-03-31 minus one month into
// 2026-03-03, which is not what "a month earlier" means to anyone). The last four
// are FIXED-LENGTH and need no clamping. calendar.go owns that arithmetic and the
// rationale for diverging from the standard library.
const (
	UnitYears    = "years"
	UnitQuarters = "quarters"
	UnitMonths   = "months"
	UnitWeeks    = "weeks"
	UnitDays     = "days"
	UnitHours    = "hours"
	UnitMinutes  = "minutes"
)

// The eleven snaps — the calendar boundary the anchor is rounded to.
//
// A snap is applied BEFORE the offset, so the node reads left to right the way it
// is spoken: `NOW, n: -5, unit: years, snap: startOfYear` is "the start of the
// year, five years back". The order is API, not an implementation detail — for a
// month-end anchor the two orders give different days — and it is documented at
// the arithmetic itself (calendar.go). Week boundaries are ISO 8601: a week runs
// Monday through Sunday. An `end*` boundary is the LAST REPRESENTABLE INSTANT of
// its period (23:59:59, the model's precision), not the start of the next one, so
// an `endOfMonth` upper bound of an inclusive `between` admits the whole final
// day. Every boundary is computed in UTC, because every instant in the model is
// UTC and there is no timezone knob anywhere in the engine.
const (
	// SnapNone is the identity: the offset result is used as computed. It is a
	// real vocabulary member rather than an absent key, so the field always
	// carries a value and the editor's snap control always has something selected.
	SnapNone           = "none"
	SnapStartOfYear    = "startOfYear"
	SnapEndOfYear      = "endOfYear"
	SnapStartOfQuarter = "startOfQuarter"
	SnapEndOfQuarter   = "endOfQuarter"
	SnapStartOfMonth   = "startOfMonth"
	SnapEndOfMonth     = "endOfMonth"
	SnapStartOfWeek    = "startOfWeek"
	SnapEndOfWeek      = "endOfWeek"
	SnapStartOfDay     = "startOfDay"
	SnapEndOfDay       = "endOfDay"
)

// relativeAnchors, relativeUnits, and relativeSnaps are the three closed
// vocabularies, each the SINGLE table its half of the system reads: Validate
// checks membership against it, the renderer quotes a member that has already
// passed that check, resolveRelativeDate switches on it, and the Go/JS contract
// test diffs it against the editor's mirror. One table per vocabulary is what
// keeps validation, rendering, resolution, and the editor from drifting apart —
// the same guarantee opSpecs gives the operators.
//
// They are maps for the same reason allowedRoots and blockedCallNames are:
// membership is the only question ever asked of them, and it is asked on the
// compile path of every decision. Presentation ORDER is the editor's business,
// not the engine's.
var relativeAnchors = map[string]struct{}{
	AnchorNow:   {},
	AnchorToday: {},
}

var relativeUnits = map[string]struct{}{
	UnitYears:    {},
	UnitQuarters: {},
	UnitMonths:   {},
	UnitWeeks:    {},
	UnitDays:     {},
	UnitHours:    {},
	UnitMinutes:  {},
}

var relativeSnaps = map[string]struct{}{
	SnapNone:           {},
	SnapStartOfYear:    {},
	SnapEndOfYear:      {},
	SnapStartOfQuarter: {},
	SnapEndOfQuarter:   {},
	SnapStartOfMonth:   {},
	SnapEndOfMonth:     {},
	SnapStartOfWeek:    {},
	SnapEndOfWeek:      {},
	SnapStartOfDay:     {},
	SnapEndOfDay:       {},
}

// RelativeDate returns a relative-date operand node: anchor, offset by n units,
// then snapped.
//
// A NEGATIVE n goes into the past — "three months ago" is
// RelativeDate(AnchorNow, -3, UnitMonths, SnapNone) — and n == 0 means the anchor
// itself, whatever unit accompanies it. Every argument is checked against its
// closed set by Validate; nothing here rejects a bad one, so a node built by hand
// and a node decoded from JSON fail in exactly the same place.
//
// The node is legal ONLY as an operand of a date operator (see Node.Validate).
func RelativeDate(anchor string, n int, unit, snap string) *Node {
	return &Node{
		Type:   NodeRelativeDate,
		Anchor: anchor,
		Offset: json.Number(strconv.Itoa(n)),
		Unit:   unit,
		Snap:   snap,
	}
}

// validateRelativeDate checks the node's four fields against their closed sets.
//
// Each field gets its OWN message, because each names a different mistake with a
// different fix — an anchor the author invented, a unit that does not exist, a
// snap that does not exist, and an offset that is not a whole number are four
// separate corrections, and the editor renders the message it is handed.
//
// Membership is checked here rather than at resolution deliberately: a relative
// date whose anchor is a typo must fail when the rule is SAVED, not deny silently
// on every decision afterwards. That is the same reason the vocabularies are
// closed sets in the AST rather than strings the runtime interprets.
func (n *Node) validateRelativeDate() error {
	if _, ok := relativeAnchors[n.Anchor]; !ok {
		return aerr.WithContext(aerr.APERTURE_RULE_INVALID,
			"rule: relative date has an unknown anchor",
			map[string]any{"anchor": n.Anchor})
	}
	if _, err := n.offsetCount(); err != nil {
		return err
	}
	if _, ok := relativeUnits[n.Unit]; !ok {
		return aerr.WithContext(aerr.APERTURE_RULE_INVALID,
			"rule: relative date has an unknown offset unit",
			map[string]any{"unit": n.Unit})
	}
	if _, ok := relativeSnaps[n.Snap]; !ok {
		return aerr.WithContext(aerr.APERTURE_RULE_INVALID,
			"rule: relative date has an unknown snap",
			map[string]any{"snap": n.Snap})
	}
	return nil
}

// offsetCount reads the offset as a whole number.
//
// The field is a JSON number token rather than a Go int so that THIS function
// owns the rejection: decoding `{"n": 1.5}` straight into an int would fail
// inside encoding/json with an uncoded error, before Validate ever ran, and the
// editor would get a message the Go validator never wrote. A token that is not an
// integer — a fraction, an exponent form, a value too large to apply to a
// calendar — is one APERTURE_RULE_INVALID with one wording, because all of them
// are the same correction: write a whole number.
func (n *Node) offsetCount() (int, error) {
	v, err := strconv.ParseInt(string(n.Offset), 10, 32)
	if err != nil {
		return 0, aerr.WithContext(aerr.APERTURE_RULE_INVALID,
			"rule: relative date offset must be a whole number",
			map[string]any{"n": string(n.Offset)})
	}
	return int(v), nil
}

// resolveRelativeDate turns a validated relative date into a concrete date value,
// reporting false when it cannot be resolved at all. It is the runtime half of
// the node, called by the `$rel` dispatcher.
//
// Three steps, in this order:
//
//   - THE ANCHOR. NOW is the instant to the second; TODAY is midnight of the
//     instant's UTC day. Both are exact, neither can clamp, and TODAY is resolved
//     here rather than desugared into a snap so that the persisted anchor and the
//     resolved value stay one-to-one.
//   - THE SNAP, applied to the anchor (calendar.go).
//   - THE OFFSET, applied to the snapped boundary (calendar.go), clamping at
//     month end for the three calendar units.
//
// SNAP FIRST, THEN OFFSET, because that is how the node reads aloud: "the start
// of the year, five years back". The two orders are different functions, not a
// rearrangement — start of the month then plus one day is the 2nd of this month,
// while plus one day then start of the month is the 1st of NEXT month for a
// month-end anchor — so the order is API. See calendar.go, which owns the
// arithmetic and the clamping rationale.
//
// NO REFERENCE INSTANT, NO DATE. A zero `now` means the evaluation was handed no
// reference instant (a hand-built rules.Input; an Engine always supplies one).
// Resolving against year 1 would silently answer every "in the last 90 days"
// question with a date in the year 1, so this denies instead — the dispatcher
// returns nil and the enclosing date operator applies its ordinary deny-safe
// policy. The same answer covers an offset that leaves the representable year
// range and an anchor, unit, or snap outside its vocabulary (both unreachable
// through a validated AST): every failure here is a deny, never a raise.
//
// The result is always the TIMESTAMP granularity. Granularity never affects
// ordering (provider.DateValue compares instants, so "2026-03-04" and
// "2026-03-04T00:00:00Z" are equal), so one form is one rule fewer for the
// arithmetic to keep straight.
func resolveRelativeDate(now time.Time, anchor string, n int, unit, snap string) (provider.DateValue, bool) {
	if now.IsZero() {
		return provider.DateValue{}, false
	}
	t := now.UTC()
	switch anchor {
	case AnchorNow:
	case AnchorToday:
		t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	default:
		// Unreachable through a validated AST; deny-safe regardless.
		return provider.DateValue{}, false
	}

	t, ok := snapInstant(t, snap)
	if !ok {
		return provider.DateValue{}, false
	}
	t, ok = offsetInstant(t, n, unit)
	if !ok {
		return provider.DateValue{}, false
	}
	if !inCanonicalRange(t) {
		// A fixed-length offset can leave the four-digit year range without
		// overflowing a Duration; the canonical forms cannot write the result, so
		// there is no date to compare against.
		return provider.DateValue{}, false
	}
	return provider.DateTimeOf(t), true
}

// ResolvedRelativeDate is one relative-date operand of an AST paired with the
// concrete date it resolves to at a given reference instant.
//
// It exists for the rule builder's what-if preview. An author who writes
// "three months back, snapped to the start of the month" has written a
// COMPUTATION, not a date, and the only way to see whether they wrote the one
// they meant is to see what it currently comes out as. Resolving it a second
// time in the browser was the alternative and was rejected: the clamping,
// snapping and ISO-week rules live in calendar.go, and a JavaScript copy of them
// would be a second implementation free to disagree with the one that actually
// decides access.
//
// Path is the operand's position in the AST, written as the dotted/indexed route
// from the root ("$.right.items[0]"), so a `between` with two relative bounds
// yields two distinguishable rows. It is a display label, not a selector: no
// code parses it back.
//
// The four field values are echoed exactly as the node carries them — Offset is
// the authored TOKEN, not a re-rendered number — so a node the validator would
// reject still describes itself accurately. Resolved is the canonical date-time
// the operand currently means, or "" when it resolves to nothing at all (no
// reference instant, an offset that leaves the representable year range, or a
// field outside its vocabulary). An empty Resolved is the same deny the engine
// applies, made visible.
// The JSON tags mirror the node's own persisted spelling (`n` for the offset),
// so a surface that hands this to a client hands it something an author can line
// up against the four controls without a translation table.
type ResolvedRelativeDate struct {
	Path     string `json:"path"`
	Anchor   string `json:"anchor"`
	Offset   string `json:"n"`
	Unit     string `json:"unit"`
	Snap     string `json:"snap"`
	Resolved string `json:"resolved"`
}

// ResolveRelativeDates walks n and resolves every relative-date operand it
// contains against now, in document order (left before right, children and list
// items in index order). An AST with no relative date yields nil.
//
// It resolves through the SAME resolveRelativeDate the `$rel` dispatcher calls,
// so the preview and the decision cannot disagree about what an operand means at
// a given instant. It never fails: an operand that cannot be resolved comes back
// with an empty Resolved, which is exactly the deny the evaluation would apply.
//
// A zero now means no reference instant is available, and every operand then
// resolves to nothing — the same rule Input.Now carries everywhere else.
func ResolveRelativeDates(n *Node, now time.Time) []ResolvedRelativeDate {
	var out []ResolvedRelativeDate
	walkRelativeDates(n, "$", &out, now)
	return out
}

// walkRelativeDates is the recursive half of ResolveRelativeDates. It is a total
// walk over the node shape rather than a type-directed one: a node carries only
// the children its type populates, so visiting all four child slots reaches
// every operand without a switch that a new node type could fall out of.
func walkRelativeDates(n *Node, path string, out *[]ResolvedRelativeDate, now time.Time) {
	if n == nil {
		return
	}
	if n.Type == NodeRelativeDate {
		*out = append(*out, resolvedAt(n, path, now))
		return
	}
	walkRelativeDates(n.Left, path+".left", out, now)
	walkRelativeDates(n.Right, path+".right", out, now)
	for i, ch := range n.Children {
		walkRelativeDates(ch, path+".children["+strconv.Itoa(i)+"]", out, now)
	}
	for i, it := range n.Items {
		walkRelativeDates(it, path+".items["+strconv.Itoa(i)+"]", out, now)
	}
}

// resolvedAt describes one relative-date node at now. An offset token that is
// not a whole number is reported verbatim with an empty Resolved rather than
// being coerced: the validator's job is to name it, and inventing a value here
// would show the author a date their rule does not mean.
func resolvedAt(n *Node, path string, now time.Time) ResolvedRelativeDate {
	r := ResolvedRelativeDate{
		Path:   path,
		Anchor: n.Anchor,
		Offset: string(n.Offset),
		Unit:   n.Unit,
		Snap:   n.Snap,
	}
	count, err := n.offsetCount()
	if err != nil {
		return r
	}
	if v, ok := resolveRelativeDate(now, n.Anchor, count, n.Unit, n.Snap); ok {
		r.Resolved = v.String()
	}
	return r
}
