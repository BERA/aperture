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
// are FIXED-LENGTH and need no clamping. See resolveRelativeDate for where that
// arithmetic lands.
const (
	UnitYears    = "years"
	UnitQuarters = "quarters"
	UnitMonths   = "months"
	UnitWeeks    = "weeks"
	UnitDays     = "days"
	UnitHours    = "hours"
	UnitMinutes  = "minutes"
)

// The eleven snaps — what the offset result is rounded to once it is computed.
//
// A snap is applied AFTER the offset, so `NOW, n: -5, unit: years, snap:
// startOfYear` is "the start of the year five years ago", not "five years before
// the start of this year". Week boundaries are ISO 8601: a week starts on Monday.
// Every boundary is computed in UTC, because every instant in the model is UTC
// and there is no timezone knob anywhere in the engine.
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
// the node, called by the `$rel` dispatcher, and it is the SEAM the calendar
// arithmetic fills.
//
// What it decides today:
//
//   - NO REFERENCE INSTANT, NO DATE. A zero `now` means the evaluation was handed
//     no reference instant (a hand-built rules.Input; an Engine always supplies
//     one). Resolving against year 1 would silently answer every "in the last 90
//     days" question with a date in the year 1, so this denies instead — the
//     dispatcher returns nil and the enclosing date operator applies its ordinary
//     deny-safe policy.
//   - THE ANCHOR. NOW is the instant to the second; TODAY is midnight of the
//     instant's UTC day. Both are exact, neither can clamp, and TODAY is resolved
//     here rather than desugared into a snap so that the persisted anchor and the
//     resolved value stay one-to-one.
//
// WHAT IS NOT IMPLEMENTED YET, and is deliberately deny-safe until it is: the
// calendar arithmetic — offsetting by the seven units (with month-end CLAMPING
// for years, quarters, and months, which time.AddDate does not do) and the ten
// non-identity snaps (with ISO-8601 Monday weeks). Until that lands, a node with
// a non-zero offset or a snap other than "none" resolves to nothing and its
// comparison is false. Denying is the only safe placeholder: an approximation
// here would grant or withhold access against a date nobody chose, and would do
// it silently.
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
	if n != 0 || snap != SnapNone {
		// The calendar arithmetic — offsetting t by n of `unit` (clamping at
		// month end for the three calendar units) and then applying `snap` (with
		// ISO-8601 Monday weeks) — is the half of this seam that is still to be
		// written. Until it is, anything that needs it resolves to nothing and
		// the enclosing comparison denies. See the note above for why an
		// approximation is not an acceptable placeholder.
		return provider.DateValue{}, false
	}
	return provider.DateTimeOf(t), true
}
