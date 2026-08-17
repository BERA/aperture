package rules

import (
	"bytes"
	"context"
	"encoding/json"
	stderrors "errors"
	"strings"
	"testing"
	"time"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/identity"
)

// pinnedInstant is the reference instant every test in this file resolves
// against. It is deliberately NOT midnight and NOT the first of a month, so a
// TODAY anchor is distinguishable from a NOW anchor and a snap is
// distinguishable from the anchor it is applied to. Nothing here reads
// time.Now(): an expectation derived from the real clock only exercises the
// interesting calendar path on the days the calendar happens to cooperate.
var pinnedInstant = time.Date(2026, 3, 4, 12, 30, 45, 0, time.UTC)

// evalAgainst compiles n and evaluates it against md with the reference instant
// pinned to now. It is the direct-library path — no engine, no fetcher — so a
// test asserting a decision is asserting the node and nothing else.
func evalAgainst(t *testing.T, n *Node, md map[string]any, now time.Time) bool {
	t.Helper()
	compiled, err := NewCompiler().Compile(n)
	if err != nil {
		t.Fatalf("compile %v: %v", n, err)
	}
	got, err := compiled.Eval(context.Background(), Input{Object: md, Now: now})
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	return got
}

// --- shape and round-trip ----------------------------------------------------

// TestRelativeDateJSONIsByteStable pins the persisted shape. The node's JSON is
// stored rule data and the editor's serialization target, so the four keys, their
// order, and the fact that none of them is ever omitted are all API.
func TestRelativeDateJSONIsByteStable(t *testing.T) {
	cases := map[string]string{
		"three months ago":       `{"type":"relativeDate","anchor":"NOW","n":-3,"unit":"months","snap":"none"}`,
		"start of year, 5 back":  `{"type":"relativeDate","anchor":"NOW","n":-5,"unit":"years","snap":"startOfYear"}`,
		"today, no offset":       `{"type":"relativeDate","anchor":"TODAY","n":0,"unit":"days","snap":"none"}`,
		"today, start of year":   `{"type":"relativeDate","anchor":"TODAY","n":0,"unit":"days","snap":"startOfYear"}`,
		"forward two quarters":   `{"type":"relativeDate","anchor":"NOW","n":2,"unit":"quarters","snap":"endOfQuarter"}`,
		"a fortnight ahead":      `{"type":"relativeDate","anchor":"TODAY","n":2,"unit":"weeks","snap":"endOfWeek"}`,
		"ninety days, start day": `{"type":"relativeDate","anchor":"NOW","n":-90,"unit":"days","snap":"startOfDay"}`,
		"twelve hours":           `{"type":"relativeDate","anchor":"NOW","n":12,"unit":"hours","snap":"none"}`,
		"thirty minutes":         `{"type":"relativeDate","anchor":"NOW","n":-30,"unit":"minutes","snap":"endOfDay"}`,
		"start of month":         `{"type":"relativeDate","anchor":"TODAY","n":-1,"unit":"months","snap":"startOfMonth"}`,
		"end of month":           `{"type":"relativeDate","anchor":"NOW","n":1,"unit":"months","snap":"endOfMonth"}`,
		"start of quarter":       `{"type":"relativeDate","anchor":"NOW","n":-1,"unit":"quarters","snap":"startOfQuarter"}`,
		"end of year":            `{"type":"relativeDate","anchor":"TODAY","n":0,"unit":"years","snap":"endOfYear"}`,
		"start of week":          `{"type":"relativeDate","anchor":"NOW","n":-1,"unit":"weeks","snap":"startOfWeek"}`,
	}

	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			var n Node
			if err := json.Unmarshal([]byte(src), &n); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			// Validate through the position the node is legal in; a bare node is
			// deliberately rejected (see TestRelativeDateIsOnlyLegalAsADateOperand).
			if err := Compare(OpBefore, Var("object.hired_at"), &n).Validate(); err != nil {
				t.Fatalf("validate: %v", err)
			}
			out, err := json.Marshal(&n)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if !bytes.Equal([]byte(src), out) {
				t.Errorf("relative date is not byte-stable\n  in:  %s\n  out: %s", src, out)
			}
		})
	}
}

// TestRelativeDateAnchorsReadBackAsAuthored pins the TODAY decision: TODAY is a
// DISTINCT persisted anchor, defined as NOW snapped to the start of its UTC day,
// and it is never rewritten into that definition. An author who picked TODAY gets
// TODAY back — the editor shows the choice that was made, not an equivalent one.
func TestRelativeDateAnchorsReadBackAsAuthored(t *testing.T) {
	for _, anchor := range []string{AnchorNow, AnchorToday} {
		t.Run(anchor, func(t *testing.T) {
			built := RelativeDate(anchor, -3, UnitMonths, SnapNone)
			raw, err := json.Marshal(built)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var back Node
			if err := json.Unmarshal(raw, &back); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if back.Anchor != anchor {
				t.Errorf("anchor read back as %q, want %q — the anchor was normalised away", back.Anchor, anchor)
			}
			if back.Snap != SnapNone {
				t.Errorf("snap read back as %q, want %q — TODAY must not be rewritten into a snap", back.Snap, SnapNone)
			}
		})
	}

	// The two anchors are distinct AST values, so they render differently and
	// therefore hash — and cache — differently.
	now, _ := RelativeDate(AnchorNow, 0, UnitDays, SnapNone).Expr()
	today, _ := RelativeDate(AnchorToday, 0, UnitDays, SnapNone).Expr()
	if now == today {
		t.Errorf("NOW and TODAY render identically (%q); they must stay distinguishable", now)
	}
}

// --- closed-set validation ---------------------------------------------------

// TestRelativeDateRejectsAnythingOutsideItsClosedSets is the vocabulary gate.
// Anchors, units, and snaps are closed sets checked in the AST, so a typo fails
// when the rule is SAVED rather than denying silently on every decision after.
func TestRelativeDateRejectsAnythingOutsideItsClosedSets(t *testing.T) {
	cases := []struct {
		name string
		node *Node
	}{
		{"unknown anchor", RelativeDate("YESTERDAY", -3, UnitMonths, SnapNone)},
		{"lower-case anchor", RelativeDate("now", -3, UnitMonths, SnapNone)},
		{"empty anchor", RelativeDate("", -3, UnitMonths, SnapNone)},
		{"unknown unit", RelativeDate(AnchorNow, -3, "fortnights", SnapNone)},
		{"singular unit", RelativeDate(AnchorNow, -3, "month", SnapNone)},
		{"empty unit", RelativeDate(AnchorNow, -3, "", SnapNone)},
		{"unknown snap", RelativeDate(AnchorNow, -3, UnitMonths, "startOfFiscalYear")},
		{"empty snap", RelativeDate(AnchorNow, -3, UnitMonths, "")},
		{"snap spelled as omitted", RelativeDate(AnchorNow, 0, UnitDays, "null")},

		{"fractional offset", &Node{Type: NodeRelativeDate,
			Anchor: AnchorNow, Offset: "1.5", Unit: UnitMonths, Snap: SnapNone}},
		{"exponent offset", &Node{Type: NodeRelativeDate,
			Anchor: AnchorNow, Offset: "1e3", Unit: UnitMonths, Snap: SnapNone}},
		{"offset with a trailing point", &Node{Type: NodeRelativeDate,
			Anchor: AnchorNow, Offset: "3.", Unit: UnitMonths, Snap: SnapNone}},
		{"absent offset", &Node{Type: NodeRelativeDate,
			Anchor: AnchorNow, Unit: UnitMonths, Snap: SnapNone}},
		{"offset too large for a calendar", &Node{Type: NodeRelativeDate,
			Anchor: AnchorNow, Offset: "99999999999999", Unit: UnitMonths, Snap: SnapNone}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Compare(OpBefore, Var("object.hired_at"), tc.node).Validate()
			if err == nil {
				t.Fatalf("Validate accepted a relative date outside its closed sets")
			}
			if got := aerr.CodeOf(err); got != aerr.APERTURE_RULE_INVALID {
				t.Fatalf("code = %s, want %s", got, aerr.APERTURE_RULE_INVALID)
			}
		})
	}
}

// TestRelativeDateFieldsFailIndependently proves each field has its OWN
// diagnostic. They are four different corrections, and the editor renders the
// message it is handed beside the offending control, so one shared "invalid
// relative date" would be a worse message four times over.
func TestRelativeDateFieldsFailIndependently(t *testing.T) {
	messages := map[string]string{}
	cases := []struct {
		field string
		node  *Node
	}{
		{"anchor", RelativeDate("YESTERDAY", -3, UnitMonths, SnapNone)},
		{"n", &Node{Type: NodeRelativeDate, Anchor: AnchorNow, Offset: "1.5", Unit: UnitMonths, Snap: SnapNone}},
		{"unit", RelativeDate(AnchorNow, -3, "fortnights", SnapNone)},
		{"snap", RelativeDate(AnchorNow, -3, UnitMonths, "startOfFiscalYear")},
	}
	for _, tc := range cases {
		err := Compare(OpBefore, Var("object.hired_at"), tc.node).Validate()
		if err == nil {
			t.Fatalf("%s: Validate accepted an invalid field", tc.field)
		}
		var ce *aerr.CodedError
		if !stderrors.As(err, &ce) {
			t.Fatalf("%s: error is not an *errors.CodedError: %v", tc.field, err)
		}
		if _, ok := ce.Context[tc.field]; !ok {
			t.Errorf("%s: error context %v does not name the offending field", tc.field, ce.Context)
		}
		if prev, seen := messages[ce.Msg]; seen {
			t.Errorf("%s and %s share the message %q; each field needs its own", tc.field, prev, ce.Msg)
		}
		messages[ce.Msg] = tc.field
	}
	if len(messages) != len(cases) {
		t.Errorf("got %d distinct messages for %d fields", len(messages), len(cases))
	}
}

// TestRelativeDateAcceptsEveryMemberOfEveryVocabulary walks the three tables so a
// member added to one of them cannot be one validation forgets to accept.
func TestRelativeDateAcceptsEveryMemberOfEveryVocabulary(t *testing.T) {
	for anchor := range relativeAnchors {
		for unit := range relativeUnits {
			for snap := range relativeSnaps {
				n := Compare(OpBefore, Var("object.hired_at"), RelativeDate(anchor, -1, unit, snap))
				if err := n.Validate(); err != nil {
					t.Errorf("Validate rejected {%s, -1 %s, %s}: %v", anchor, unit, snap, err)
				}
				if _, err := NewCompiler().Compile(n); err != nil {
					t.Errorf("Compile rejected {%s, -1 %s, %s}: %v", anchor, unit, snap, err)
				}
			}
		}
	}
}

// TestRelativeDateOffsetZeroIsTheAnchorItself pins the zero case: it is legal,
// and it is what "the anchor itself" is spelled as.
func TestRelativeDateOffsetZeroIsTheAnchorItself(t *testing.T) {
	n := Compare(OpOnOrBefore, Var("object.hired_at"), RelativeDate(AnchorNow, 0, UnitDays, SnapNone))
	if err := n.Validate(); err != nil {
		t.Fatalf("a zero offset must be legal: %v", err)
	}
	// The anchor is the reference instant, so a field holding exactly that
	// instant is on-or-before it and a field one second later is not.
	if !evalAgainst(t, n, map[string]any{"hired_at": "2026-03-04T12:30:45Z"}, pinnedInstant) {
		t.Errorf("a field at the reference instant must satisfy onOrBefore NOW")
	}
	if evalAgainst(t, n, map[string]any{"hired_at": "2026-03-04T12:30:46Z"}, pinnedInstant) {
		t.Errorf("a field one second after the reference instant must not satisfy onOrBefore NOW")
	}
}

// TestRelativeDateTodayIsTheStartOfItsUTCDay pins the anchor's definition, and
// with it the difference between the two anchors: at 12:30:45 UTC, TODAY is
// midnight of the same day and NOW is not.
func TestRelativeDateTodayIsTheStartOfItsUTCDay(t *testing.T) {
	md := map[string]any{"hired_at": "2026-03-04"} // midnight UTC on the same day

	sameAsToday := Compare(OpSameDay, Var("object.hired_at"), RelativeDate(AnchorToday, 0, UnitDays, SnapNone))
	if !evalAgainst(t, sameAsToday, md, pinnedInstant) {
		t.Errorf("TODAY must fall on the reference instant's calendar day")
	}

	// Equal to the start of the day: true for TODAY, false for NOW.
	atToday := Compare(OpOnOrBefore, Var("object.hired_at"), RelativeDate(AnchorToday, 0, UnitDays, SnapNone))
	if !evalAgainst(t, atToday, md, pinnedInstant) {
		t.Errorf("midnight must be on-or-before TODAY")
	}
	afterToday := Compare(OpOnOrAfter, Var("object.hired_at"), RelativeDate(AnchorToday, 0, UnitDays, SnapNone))
	if !evalAgainst(t, afterToday, md, pinnedInstant) {
		t.Errorf("midnight must be on-or-after TODAY — TODAY is midnight, so both hold")
	}
	afterNow := Compare(OpOnOrAfter, Var("object.hired_at"), RelativeDate(AnchorNow, 0, UnitDays, SnapNone))
	if evalAgainst(t, afterNow, md, pinnedInstant) {
		t.Errorf("midnight must NOT be on-or-after NOW at 12:30:45 — the anchors are not interchangeable")
	}
}

// TestRelativeDateSnapComposesOnTopOfTheAnchor covers the composition a reader
// wonders about: TODAY already means the start of a day, and a wider snap on top
// of it is legal and simply subsumes that — `anchor: TODAY, snap: startOfYear` is
// the start of the year, not an error and not a contradiction.
func TestRelativeDateSnapComposesOnTopOfTheAnchor(t *testing.T) {
	n := Compare(OpOnOrAfter, Var("object.hired_at"), RelativeDate(AnchorToday, 0, UnitDays, SnapStartOfYear))
	if err := n.Validate(); err != nil {
		t.Fatalf("TODAY with a wider snap must be legal: %v", err)
	}
	if _, err := NewCompiler().Compile(n); err != nil {
		t.Fatalf("TODAY with a wider snap must compile: %v", err)
	}
	// The snap is carried into the render as its own argument, so the anchor and
	// the snap remain two independent facts about the node rather than one
	// collapsed one.
	src, err := n.Expr()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if want := `$rel(__now, "TODAY", 0, "days", "startOfYear")`; !strings.Contains(src, want) {
		t.Errorf("rendered %q, want it to contain %q", src, want)
	}
}

// --- position: an operand of a date operator, and nowhere else ----------------

// TestRelativeDateIsOnlyLegalAsADateOperand pins the one structural rule the node
// carries. It resolves to a date the author never sees, so anywhere it is not
// being compared AS a date it would be compared as an opaque string that changes
// every second — a silent wrong answer rather than a loud one.
func TestRelativeDateIsOnlyLegalAsADateOperand(t *testing.T) {
	rel := func() *Node { return RelativeDate(AnchorNow, 0, UnitDays, SnapNone) }

	legal := map[string]*Node{
		"right of before":     Compare(OpBefore, Var("object.hired_at"), rel()),
		"right of after":      Compare(OpAfter, Var("object.hired_at"), rel()),
		"right of onOrBefore": Compare(OpOnOrBefore, Var("object.hired_at"), rel()),
		"right of onOrAfter":  Compare(OpOnOrAfter, Var("object.hired_at"), rel()),
		"right of sameDay":    Compare(OpSameDay, Var("object.hired_at"), rel()),
		"right of sameMonth":  Compare(OpSameMonth, Var("object.hired_at"), rel()),
		"right of sameYear":   Compare(OpSameYear, Var("object.hired_at"), rel()),
		"left of a date op":   Compare(OpBefore, rel(), Var("object.hired_at")),
		"lower bound":         Between(Var("object.hired_at"), rel(), Lit("2026-12-31")),
		"upper bound":         Between(Var("object.hired_at"), Lit("2020-01-01"), rel()),
		"both bounds":         Between(Var("object.hired_at"), rel(), rel()),
		"under and/or/not":    And(Not(Compare(OpBefore, Var("object.hired_at"), rel())), Compare(OpEq, Var("action"), Lit("read"))),
	}
	for name, n := range legal {
		t.Run("legal/"+name, func(t *testing.T) {
			if err := n.Validate(); err != nil {
				t.Fatalf("Validate rejected a legal position: %v", err)
			}
			if _, err := NewCompiler().Compile(n); err != nil {
				t.Fatalf("Compile rejected a legal position: %v", err)
			}
		})
	}

	illegal := map[string]*Node{
		"bare, as a whole rule": rel(),
		"right of eq":           Compare(OpEq, Var("object.hired_at"), rel()),
		"right of ne":           Compare(OpNe, Var("object.hired_at"), rel()),
		"right of lt":           Compare(OpLt, Var("object.hired_at"), rel()),
		"left of eq":            Compare(OpEq, rel(), Lit("2026-01-01")),
		"right of has":          Compare(OpHas, Var("object.tags"), rel()),
		"inside an in list":     Compare(OpIn, Var("object.region"), List(rel())),
		"inside a hasAll list":  Compare(OpHasAll, Var("object.tags"), List(rel())),
		"collection operand":    Compare(OpHasAll, Var("object.tags"), rel()),
		"as a logical child":    And(rel(), Compare(OpEq, Var("action"), Lit("read"))),
		"as a not child":        Not(rel()),
		"as a call argument":    Compare(OpEq, Call("lower", rel()), Lit("x")),
		"left of a unary op":    Unary(OpIsEmpty, rel()),
		"nested in a date list": Compare(OpEq, Var("object.x"), List(List(rel()))),
		"left of a collection op": Compare(OpHasAll, rel(),
			List(Lit("a"))),
	}
	for name, n := range illegal {
		t.Run("illegal/"+name, func(t *testing.T) {
			err := n.Validate()
			if err == nil {
				t.Fatalf("Validate accepted a relative date outside a date operator")
			}
			if got := aerr.CodeOf(err); got != aerr.APERTURE_RULE_INVALID {
				t.Fatalf("code = %s, want %s", got, aerr.APERTURE_RULE_INVALID)
			}
		})
	}
}

// --- render ------------------------------------------------------------------

// TestRelativeDateRendersLiteralArguments asserts the rendered expression BYTE
// FOR BYTE. The render is the security boundary this node exists to preserve:
// every field is a quoted literal that has already passed a closed-set check, the
// instant is the environment identifier __now, and nothing an author types
// becomes syntax.
func TestRelativeDateRendersLiteralArguments(t *testing.T) {
	cases := []struct {
		name string
		node *Node
		want string
	}{
		{"three months ago", RelativeDate(AnchorNow, -3, UnitMonths, SnapNone),
			`$rel(__now, "NOW", -3, "months", "none")`},
		{"the anchor itself", RelativeDate(AnchorNow, 0, UnitDays, SnapNone),
			`$rel(__now, "NOW", 0, "days", "none")`},
		{"today, start of year", RelativeDate(AnchorToday, 0, UnitDays, SnapStartOfYear),
			`$rel(__now, "TODAY", 0, "days", "startOfYear")`},
		{"five years back to the start of the year", RelativeDate(AnchorNow, -5, UnitYears, SnapStartOfYear),
			`$rel(__now, "NOW", -5, "years", "startOfYear")`},
		{"forward two quarters", RelativeDate(AnchorNow, 2, UnitQuarters, SnapEndOfQuarter),
			`$rel(__now, "NOW", 2, "quarters", "endOfQuarter")`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.node.Expr()
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			if got != tc.want {
				t.Errorf("render:\n  got  %s\n  want %s", got, tc.want)
			}
		})
	}

	// In position, the operand sits inside the date dispatcher exactly where a
	// literal would, and carries no variable path (a note calls it "(expression)").
	full, err := Compare(OpBefore, Var("object.hired_at"), RelativeDate(AnchorNow, -3, UnitMonths, SnapNone)).Expr()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	want := `$date("before", __notes, "object.hired_at", object?.hired_at, "", $rel(__now, "NOW", -3, "months", "none"), "", nil)`
	if full != want {
		t.Errorf("in-position render:\n  got  %s\n  want %s", full, want)
	}
}

// TestRelativeDateRenderRefusesAnUnvalidatedOffset covers the exported-Expr path:
// a hand-built node with a garbage offset must fail with the validator's coded
// error rather than emit a token that becomes an opaque expr-lang compile error.
func TestRelativeDateRenderRefusesAnUnvalidatedOffset(t *testing.T) {
	n := &Node{Type: NodeRelativeDate, Anchor: AnchorNow, Offset: "3 months", Unit: UnitMonths, Snap: SnapNone}
	if _, err := n.Expr(); err == nil {
		t.Fatalf("Expr rendered an unvalidated offset")
	} else if got := aerr.CodeOf(err); got != aerr.APERTURE_RULE_INVALID {
		t.Errorf("code = %s, want %s", got, aerr.APERTURE_RULE_INVALID)
	}
}

// TestRelativeDateAddsNoVariableRoot is the security assertion. The whole point
// of a structured node is that the instant never becomes a dereferenceable name:
// reflective method calls survive expr.DisableAllBuiltins, so a `NOW` root would
// make `NOW.AddDate(0, -3, 0)` a well-formed var path.
func TestRelativeDateAddsNoVariableRoot(t *testing.T) {
	want := map[string]bool{"object": true, "principal": true, "account": true, "action": true}
	if len(allowedRoots) != len(want) {
		t.Fatalf("allowedRoots has %d entries %v, want exactly %v", len(allowedRoots), keysOf(allowedRoots), want)
	}
	for root := range allowedRoots {
		if !want[root] {
			t.Errorf("allowedRoots gained %q; the relative-date node must not widen it", root)
		}
	}

	for _, path := range []string{"NOW", "NOW.AddDate", "TODAY", "now", "today", "__now", "__now.Unix"} {
		if err := Var(path).Validate(); err == nil {
			t.Errorf("Var(%q) validated; it must not be reachable from a rule", path)
		} else if got := aerr.CodeOf(err); got != aerr.APERTURE_RULE_UNKNOWN_VARIABLE {
			t.Errorf("Var(%q) code = %s, want %s", path, got, aerr.APERTURE_RULE_UNKNOWN_VARIABLE)
		}
	}

	// The dispatcher itself is unreachable for the structural reason $op and
	// $date are: '$' is outside the identifier grammar Validate enforces, so no
	// blockedCallNames entry is needed (and none was added).
	if err := Call(fnRelativeDate, Var("object.hired_at")).Validate(); err == nil {
		t.Errorf("a call node named %q validated", fnRelativeDate)
	}
	if _, blocked := blockedCallNames[fnRelativeDate]; blocked {
		t.Errorf("%q was added to blockedCallNames; the '$' already makes it unnameable", fnRelativeDate)
	}
}

// --- evaluation --------------------------------------------------------------

// TestRelativeDateDecidesAgainstThePinnedClock drives a rule the whole way
// through an Engine, which is where the reference instant actually comes from:
// rules.WithClock is the engine's single time source and it now decides what
// "now" means to a rule.
func TestRelativeDateDecidesAgainstThePinnedClock(t *testing.T) {
	clock := &fakeClock{t: pinnedInstant}
	eng := NewEngine(MapSource{
		"recent": {Name: "recent", AST: Compare(OpOnOrBefore,
			Var("object.hired_at"), RelativeDate(AnchorNow, 0, UnitDays, SnapNone))},
	}, fakeFetcher{
		"account:acme/document:1": {"hired_at": "2020-01-01"},
		"account:acme/document:2": {"hired_at": "2030-01-01"},
	}, WithClock(clock))

	ctx := context.Background()
	past := identity.MustParse("account:acme/document:1")
	future := identity.MustParse("account:acme/document:2")

	got, err := eng.Selected(ctx, "recent", past, "alice", "read")
	if err != nil {
		t.Fatalf("selected: %v", err)
	}
	if !got {
		t.Errorf("a 2020 date must be on or before the pinned instant")
	}
	got, err = eng.Selected(ctx, "recent", future, "alice", "read")
	if err != nil {
		t.Fatalf("selected: %v", err)
	}
	if got {
		t.Errorf("a 2030 date must not be on or before the pinned instant")
	}

	// Move the clock past the future date and the SAME cached program decides
	// the other way: the instant is data, never baked into the compiled source.
	clock.advance(20 * 365 * 24 * time.Hour)
	got, err = eng.Selected(ctx, "recent", future, "alice", "read")
	if err != nil {
		t.Fatalf("selected: %v", err)
	}
	if !got {
		t.Errorf("after advancing the clock past 2030 the same rule must select the object")
	}
}

// TestRelativeDateWithoutAReferenceInstantDenies pins the zero-Input.Now
// contract. A hand-built rules.Input may carry no instant; resolving against year
// 1 would answer every "in the last 90 days" question with a date in the year 1
// and silently grant or withhold on it, so the node denies instead — and does not
// raise, because one missing instant must not break every Check that touches the
// field.
func TestRelativeDateWithoutAReferenceInstantDenies(t *testing.T) {
	md := map[string]any{"hired_at": "2020-01-01"}
	cases := map[string]*Node{
		"onOrBefore NOW": Compare(OpOnOrBefore, Var("object.hired_at"), RelativeDate(AnchorNow, 0, UnitDays, SnapNone)),
		"after NOW":      Compare(OpAfter, Var("object.hired_at"), RelativeDate(AnchorNow, 0, UnitDays, SnapNone)),
		"before TODAY":   Compare(OpBefore, Var("object.hired_at"), RelativeDate(AnchorToday, 0, UnitDays, SnapNone)),
		"sameYear NOW":   Compare(OpSameYear, Var("object.hired_at"), RelativeDate(AnchorNow, 0, UnitDays, SnapNone)),
		"between":        Between(Var("object.hired_at"), Lit("1900-01-01"), RelativeDate(AnchorNow, 0, UnitDays, SnapNone)),
	}
	for name, n := range cases {
		t.Run(name, func(t *testing.T) {
			compiled, err := NewCompiler().Compile(n)
			if err != nil {
				t.Fatalf("compile: %v", err)
			}
			// No Now on the Input at all — the zero value.
			got, err := compiled.Eval(context.Background(), Input{Object: md})
			if err != nil {
				t.Fatalf("a missing reference instant must deny, not raise: %v", err)
			}
			if got {
				t.Errorf("a missing reference instant must deny; %q matched", name)
			}
		})
	}
}

// TestRelativeDateNotesTheUnresolvedOperand pins the diagnostic. An unresolved
// relative date reaches the date operator as nothing at all, so it is reported
// with the operators' existing shape vocabulary — the operand is anonymous, so it
// is named "(expression)" rather than by a field path, and no value appears.
func TestRelativeDateNotesTheUnresolvedOperand(t *testing.T) {
	n := Compare(OpOnOrBefore, Var("object.hired_at"), RelativeDate(AnchorNow, 0, UnitDays, SnapNone))
	compiled, err := NewCompiler().Compile(n)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	got, notes, err := compiled.EvalWithNotes(context.Background(), Input{
		Object: map[string]any{"hired_at": "2020-01-01"},
	})
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	if got {
		t.Fatalf("an unresolved relative date must deny")
	}
	var found bool
	for _, note := range notes {
		if note.Kind == NoteShapeMismatch && note.Expected == shapeDate && note.Actual == shapeAbsent {
			found = true
		}
		if note.Path != "" {
			t.Errorf("a relative-date operand has no variable path; note carried %q", note.Path)
		}
	}
	if !found {
		t.Errorf("no shape note recorded for the unresolved operand; got %+v", notes)
	}
}

// TestRelativeDateBoundsCombinations covers the four ways a between's two bounds
// can be written. Each bound is an operand in its own right, so they vary
// independently — that is what the shape decision (a two-item list, not a second
// right-hand field) buys.
func TestRelativeDateBoundsCombinations(t *testing.T) {
	rel := func() *Node { return RelativeDate(AnchorNow, 0, UnitDays, SnapNone) }
	atInstant := map[string]any{"hired_at": "2026-03-04T12:30:45Z"} // exactly the pinned instant
	longAgo := map[string]any{"hired_at": "2020-01-01"}

	cases := []struct {
		name          string
		low, high     *Node
		wantAtInstant bool
		wantLongAgo   bool
	}{
		{"literal / literal", Lit("2020-01-01"), Lit("2030-01-01"), true, true},
		{"literal / relative", Lit("2020-01-01"), rel(), true, true},
		{"relative / literal", rel(), Lit("2030-01-01"), true, false},
		{"relative / relative", rel(), rel(), true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := Between(Var("object.hired_at"), tc.low, tc.high)
			if err := n.Validate(); err != nil {
				t.Fatalf("validate: %v", err)
			}
			if got := evalAgainst(t, n, atInstant, pinnedInstant); got != tc.wantAtInstant {
				t.Errorf("a field at the reference instant: got %v, want %v", got, tc.wantAtInstant)
			}
			if got := evalAgainst(t, n, longAgo, pinnedInstant); got != tc.wantLongAgo {
				t.Errorf("a field long before the reference instant: got %v, want %v", got, tc.wantLongAgo)
			}
		})
	}
}

// TestRelativeDateResolvesToTheCanonicalTimestampForm pins what the dispatcher
// hands back: a canonical date string, byte-for-byte what a literal in the same
// position would be, so a relative date is interchangeable with a fixed one.
func TestRelativeDateResolvesToTheCanonicalTimestampForm(t *testing.T) {
	v, ok := resolveRelativeDate(pinnedInstant, AnchorNow, 0, UnitDays, SnapNone)
	if !ok {
		t.Fatalf("NOW with no offset and no snap must resolve")
	}
	if got, want := v.String(), "2026-03-04T12:30:45Z"; got != want {
		t.Errorf("NOW resolved to %q, want %q", got, want)
	}
	v, ok = resolveRelativeDate(pinnedInstant, AnchorToday, 0, UnitDays, SnapNone)
	if !ok {
		t.Fatalf("TODAY with no offset and no snap must resolve")
	}
	if got, want := v.String(), "2026-03-04T00:00:00Z"; got != want {
		t.Errorf("TODAY resolved to %q, want %q", got, want)
	}

	// A clock answering in another zone still yields UTC: the instant is
	// converted at the snapshot boundary and again when the environment is built,
	// and resolution converts once more rather than trusting either.
	zoned := time.Date(2026, 3, 4, 12, 30, 45, 0, time.FixedZone("plus5", 5*3600))
	v, ok = resolveRelativeDate(zoned, AnchorToday, 0, UnitDays, SnapNone)
	if !ok {
		t.Fatalf("a zoned instant must still resolve")
	}
	if got, want := v.String(), "2026-03-04T00:00:00Z"; got != want {
		t.Errorf("a +05:00 instant resolved to %q, want the UTC day %q", got, want)
	}
}

// TestRelativeDateResolvesOffsetsAndSnaps is the successor to the placeholder
// that pinned the unimplemented seam: every combination it used to assert denied
// must now resolve, and resolve to a named value. The exhaustive arithmetic
// tables live in calendar_test.go; this is the end-to-end assertion that the seam
// is wired, including the two worked examples from the node's own documentation.
func TestRelativeDateResolvesOffsetsAndSnaps(t *testing.T) {
	// The reference instant is 2026-03-04T12:30:45Z, a Wednesday.
	cases := []struct {
		name string
		n    int
		unit string
		snap string
		want string
	}{
		{"three months prior to today", -3, UnitMonths, SnapNone, "2025-12-04T12:30:45Z"},
		{"a forward offset", 1, UnitDays, SnapNone, "2026-03-05T12:30:45Z"},
		{"a snap alone", 0, UnitDays, SnapStartOfYear, "2026-01-01T00:00:00Z"},
		{"start of the year, five years back", -5, UnitYears, SnapStartOfYear, "2021-01-01T00:00:00Z"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, ok := resolveRelativeDate(pinnedInstant, AnchorNow, tc.n, tc.unit, tc.snap)
			if !ok {
				t.Fatalf("resolveRelativeDate denied %s; the arithmetic must resolve it", tc.name)
			}
			if got := v.String(); got != tc.want {
				t.Errorf("resolved to %s, want %s", got, tc.want)
			}
			// And end to end through a compiled rule: a field one day before the
			// resolved bound is on-or-before it, one day after is not.
			n := Compare(OpOnOrBefore, Var("object.hired_at"), RelativeDate(AnchorNow, tc.n, tc.unit, tc.snap))
			before := v.Time().Add(-24 * time.Hour).Format("2006-01-02T15:04:05Z")
			after := v.Time().Add(24 * time.Hour).Format("2006-01-02T15:04:05Z")
			if got := evalAgainst(t, n, map[string]any{"hired_at": before}, pinnedInstant); !got {
				t.Errorf("%s is not on or before the resolved bound %s", before, tc.want)
			}
			if got := evalAgainst(t, n, map[string]any{"hired_at": after}, pinnedInstant); got {
				t.Errorf("%s must not be on or before the resolved bound %s", after, tc.want)
			}
		})
	}
}

// --- describing a rule's relative dates for an author -----------------------

// TestResolveRelativeDatesWalksEveryOperandPosition pins the AST walk that backs
// the rule builder's "what did this become?" affordance. The order is document
// order and the paths are stable, because the editor lines each row up against
// an operand the author can see.
func TestResolveRelativeDatesWalksEveryOperandPosition(t *testing.T) {
	rel := func(anchor string, n int, unit, snap string) *Node {
		return RelativeDate(anchor, n, unit, snap)
	}
	hired := Var("object.hired_at")

	cases := []struct {
		name  string
		ast   *Node
		paths []string
	}{
		{
			name: "no relative date at all",
			ast:  Compare(OpBefore, hired, Lit("2026-01-01")),
		},
		{
			name:  "the right operand",
			ast:   Compare(OpBefore, hired, rel(AnchorNow, -3, UnitMonths, SnapNone)),
			paths: []string{"$.right"},
		},
		{
			name:  "the left operand",
			ast:   Compare(OpAfter, rel(AnchorToday, 0, UnitDays, SnapNone), hired),
			paths: []string{"$.left"},
		},
		{
			name: "both between bounds, in order",
			ast: Between(hired,
				rel(AnchorNow, -5, UnitYears, SnapStartOfYear),
				rel(AnchorToday, 0, UnitDays, SnapEndOfDay)),
			paths: []string{"$.right.items[0]", "$.right.items[1]"},
		},
		{
			name: "one bound relative, one literal",
			ast: Between(hired,
				Lit("2026-01-01"),
				rel(AnchorNow, -30, UnitMinutes, SnapNone)),
			paths: []string{"$.right.items[1]"},
		},
		{
			name: "nested under logical children",
			ast: And(
				Compare(OpEq, Var("object.classification"), Lit("public")),
				Or(
					Compare(OpAfter, hired, rel(AnchorNow, -90, UnitDays, SnapStartOfDay)),
					Compare(OpBefore, hired, rel(AnchorNow, 1, UnitQuarters, SnapEndOfQuarter)),
				),
			),
			paths: []string{"$.children[1].children[0].right", "$.children[1].children[1].right"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveRelativeDates(tc.ast, pinnedInstant)
			if len(got) != len(tc.paths) {
				t.Fatalf("resolved %d operands, want %d: %+v", len(got), len(tc.paths), got)
			}
			for i, want := range tc.paths {
				if got[i].Path != want {
					t.Errorf("operand %d path = %q, want %q", i, got[i].Path, want)
				}
				if got[i].Resolved == "" {
					t.Errorf("operand %d (%s) resolved to nothing", i, got[i].Path)
				}
			}
		})
	}
	// A nil AST is not a failure; it simply carries nothing.
	if got := ResolveRelativeDates(nil, pinnedInstant); got != nil {
		t.Errorf("nil AST resolved %+v, want nil", got)
	}
}

// TestResolveRelativeDatesAgreesWithEvaluation is the property that makes the
// preview worth showing: the date an author is SHOWN is the date the rule
// COMPARES AGAINST. Both come from resolveRelativeDate, so the assertion is that
// nothing between them re-derives it — a comparison against the reported value
// as a literal must decide identically to the relative node itself.
func TestResolveRelativeDatesAgreesWithEvaluation(t *testing.T) {
	cases := []struct {
		anchor string
		n      int
		unit   string
		snap   string
		want   string
	}{
		{AnchorNow, 0, UnitDays, SnapNone, "2026-03-04T12:30:45Z"},
		{AnchorToday, 0, UnitDays, SnapNone, "2026-03-04T00:00:00Z"},
		{AnchorNow, -3, UnitMonths, SnapNone, "2025-12-04T12:30:45Z"},
		{AnchorNow, -5, UnitYears, SnapStartOfYear, "2021-01-01T00:00:00Z"},
		{AnchorNow, 0, UnitDays, SnapEndOfMonth, "2026-03-31T23:59:59Z"},
		{AnchorToday, 1, UnitQuarters, SnapEndOfQuarter, "2026-06-30T23:59:59Z"},
	}
	for _, tc := range cases {
		t.Run(tc.snap+"/"+tc.unit, func(t *testing.T) {
			node := RelativeDate(tc.anchor, tc.n, tc.unit, tc.snap)
			got := ResolveRelativeDates(Compare(OpBefore, Var("object.hired_at"), node), pinnedInstant)
			if len(got) != 1 {
				t.Fatalf("resolved %d operands, want 1", len(got))
			}
			if got[0].Resolved != tc.want {
				t.Fatalf("resolved = %q, want %q", got[0].Resolved, tc.want)
			}
			// The reported value, used as a literal bound, must decide the same
			// way the relative node does — for a value on each side of it.
			for _, hired := range []string{"2020-01-01", "2030-01-01"} {
				md := map[string]any{"hired_at": hired}
				relative := evalAgainst(t, Compare(OpBefore, Var("object.hired_at"), node), md, pinnedInstant)
				literal := evalAgainst(t, Compare(OpBefore, Var("object.hired_at"), Lit(got[0].Resolved)), md, pinnedInstant)
				if relative != literal {
					t.Errorf("hired_at %s: relative decided %v but the reported bound %q decided %v",
						hired, relative, got[0].Resolved, literal)
				}
			}
		})
	}
}

// TestResolveRelativeDatesReportsWhatDoesNotResolve pins the deny side. An
// operand that resolves to nothing denies at evaluation, and the preview has to
// say so rather than showing a blank the author reads as "still loading". The
// four field values are echoed whatever happens, so a node the validator would
// reject still describes itself.
func TestResolveRelativeDatesReportsWhatDoesNotResolve(t *testing.T) {
	cases := []struct {
		name string
		node *Node
		now  time.Time
	}{
		{
			name: "no reference instant",
			node: RelativeDate(AnchorNow, -3, UnitMonths, SnapNone),
			now:  time.Time{},
		},
		{
			name: "an offset that leaves the representable year range",
			node: RelativeDate(AnchorNow, 1<<30, UnitYears, SnapNone),
			now:  pinnedInstant,
		},
		{
			name: "an offset that is not a whole number",
			node: &Node{Type: NodeRelativeDate, Anchor: AnchorNow, Offset: "1.5", Unit: UnitDays, Snap: SnapNone},
			now:  pinnedInstant,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveRelativeDates(Compare(OpBefore, Var("object.hired_at"), tc.node), tc.now)
			if len(got) != 1 {
				t.Fatalf("resolved %d operands, want 1", len(got))
			}
			if got[0].Resolved != "" {
				t.Errorf("resolved = %q, want empty (it must not resolve)", got[0].Resolved)
			}
			if got[0].Anchor != tc.node.Anchor || got[0].Unit != tc.node.Unit ||
				got[0].Snap != tc.node.Snap || got[0].Offset != string(tc.node.Offset) {
				t.Errorf("fields = %+v, want the node's own %s/%s/%s/%s",
					got[0], tc.node.Anchor, tc.node.Offset, tc.node.Unit, tc.node.Snap)
			}
		})
	}
}
