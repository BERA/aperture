package rules

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The calendar arithmetic's tests. Every expectation in this file is written out
// LITERALLY — none is computed by the code under test, and none is derived from
// time.Now(). A month-end clamp driven by the real clock exercises the
// interesting path on 31 days a year, and an expectation computed by the same
// arithmetic it is checking asserts nothing at all.

// utc builds midnight UTC on a calendar day, for anchors whose time of day is not
// the point of the case.
func utc(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// resolved runs the seam and returns the canonical string, failing the test when
// the arithmetic denies.
func resolved(t *testing.T, now time.Time, anchor string, n int, unit, snap string) string {
	t.Helper()
	v, ok := resolveRelativeDate(now, anchor, n, unit, snap)
	if !ok {
		t.Fatalf("resolveRelativeDate(%s, %s, %d, %s, %s) denied", now.Format(time.RFC3339), anchor, n, unit, snap)
	}
	return v.String()
}

// TestCalendarOffsetClampsAtMonthEnd is the story's central table: the three
// CALENDAR units pin the day to the last valid day of the target month instead of
// rolling it forward the way time.AddDate does.
//
// It covers every 31-day month in both directions, February in a leap year and a
// non-leap year, the century and 400-year leap rules, and offsets that cross a
// year boundary — and it deliberately includes the cases where NO clamp is needed,
// so a clamp that fired spuriously would fail here too.
func TestCalendarOffsetClampsAtMonthEnd(t *testing.T) {
	cases := []struct {
		name   string
		anchor time.Time
		n      int
		unit   string
		want   string
		clamps bool
	}{
		// The three cases named in the research probe, verbatim.
		{"march 31 minus a month", utc(2026, time.March, 31), -1, UnitMonths, "2026-02-28T00:00:00Z", true},
		{"may 31 minus a month", utc(2026, time.May, 31), -1, UnitMonths, "2026-04-30T00:00:00Z", true},
		{"leap day minus a year", utc(2024, time.February, 29), -1, UnitYears, "2023-02-28T00:00:00Z", true},

		// Every 31-day month, forward and back.
		{"jan 31 plus a month", utc(2026, time.January, 31), 1, UnitMonths, "2026-02-28T00:00:00Z", true},
		{"jan 31 minus a month", utc(2026, time.January, 31), -1, UnitMonths, "2025-12-31T00:00:00Z", false},
		{"mar 31 plus a month", utc(2026, time.March, 31), 1, UnitMonths, "2026-04-30T00:00:00Z", true},
		{"may 31 plus a month", utc(2026, time.May, 31), 1, UnitMonths, "2026-06-30T00:00:00Z", true},
		{"jul 31 plus a month", utc(2026, time.July, 31), 1, UnitMonths, "2026-08-31T00:00:00Z", false},
		{"jul 31 minus a month", utc(2026, time.July, 31), -1, UnitMonths, "2026-06-30T00:00:00Z", true},
		{"aug 31 plus a month", utc(2026, time.August, 31), 1, UnitMonths, "2026-09-30T00:00:00Z", true},
		{"aug 31 minus a month", utc(2026, time.August, 31), -1, UnitMonths, "2026-07-31T00:00:00Z", false},
		{"oct 31 plus a month", utc(2026, time.October, 31), 1, UnitMonths, "2026-11-30T00:00:00Z", true},
		{"oct 31 minus a month", utc(2026, time.October, 31), -1, UnitMonths, "2026-09-30T00:00:00Z", true},
		{"dec 31 plus a month", utc(2026, time.December, 31), 1, UnitMonths, "2027-01-31T00:00:00Z", false},
		{"dec 31 minus a month", utc(2026, time.December, 31), -1, UnitMonths, "2026-11-30T00:00:00Z", true},

		// February, leap and non-leap, in both directions.
		{"jan 31 plus a month in a leap year", utc(2024, time.January, 31), 1, UnitMonths, "2024-02-29T00:00:00Z", true},
		{"mar 31 minus a month in a leap year", utc(2024, time.March, 31), -1, UnitMonths, "2024-02-29T00:00:00Z", true},
		{"leap day plus a year", utc(2024, time.February, 29), 1, UnitYears, "2025-02-28T00:00:00Z", true},
		{"leap day plus four years", utc(2024, time.February, 29), 4, UnitYears, "2028-02-29T00:00:00Z", false},
		// Clamping never EXTENDS a date: Feb 28 into a leap year stays the 28th.
		{"feb 28 into a leap year", utc(2023, time.February, 28), 1, UnitYears, "2024-02-28T00:00:00Z", false},
		{"jan 31 plus a month in a century", utc(2100, time.January, 31), 1, UnitMonths, "2100-02-28T00:00:00Z", true},
		{"jan 31 plus a month in a 400-year leap", utc(2000, time.January, 31), 1, UnitMonths, "2000-02-29T00:00:00Z", true},

		// Quarters are three months, and clamp identically.
		{"may 31 minus a quarter", utc(2026, time.May, 31), -1, UnitQuarters, "2026-02-28T00:00:00Z", true},
		{"may 31 plus a quarter", utc(2026, time.May, 31), 1, UnitQuarters, "2026-08-31T00:00:00Z", false},
		{"jan 31 plus a quarter", utc(2026, time.January, 31), 1, UnitQuarters, "2026-04-30T00:00:00Z", true},
		{"jan 31 minus a quarter", utc(2026, time.January, 31), -1, UnitQuarters, "2025-10-31T00:00:00Z", false},
		{"mar 31 minus five quarters", utc(2026, time.March, 31), -5, UnitQuarters, "2024-12-31T00:00:00Z", false},

		// Multi-year offsets, so the year borrow is exercised in both directions.
		{"mar 31 minus twelve months", utc(2026, time.March, 31), -12, UnitMonths, "2025-03-31T00:00:00Z", false},
		{"mar 31 minus thirteen months", utc(2026, time.March, 31), -13, UnitMonths, "2025-02-28T00:00:00Z", true},
		{"dec 31 plus fourteen months", utc(2026, time.December, 31), 14, UnitMonths, "2028-02-29T00:00:00Z", true},
		{"mar 31 minus a year", utc(2026, time.March, 31), -1, UnitYears, "2025-03-31T00:00:00Z", false},

		// Zero of a calendar unit is the anchor itself.
		{"zero months", utc(2026, time.March, 31), 0, UnitMonths, "2026-03-31T00:00:00Z", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolved(t, tc.anchor, AnchorNow, tc.n, tc.unit, SnapNone); got != tc.want {
				t.Errorf("got %s, want %s", got, tc.want)
			}
			// The clamp is a DIVERGENCE from the standard library, so the cases
			// that clamp must disagree with time.AddDate — and the ones that do
			// not must agree, which is what proves the divergence is confined to
			// the month-end case rather than a second, subtly different calendar.
			months := tc.n
			switch tc.unit {
			case UnitYears:
				months = tc.n * 12
			case UnitQuarters:
				months = tc.n * 3
			}
			stdlib := tc.anchor.AddDate(0, months, 0).Format("2006-01-02T15:04:05Z")
			if tc.clamps && stdlib == tc.want {
				t.Errorf("case is marked as clamping but time.AddDate agrees (%s); the fixture is not exercising the clamp", stdlib)
			}
			if !tc.clamps && stdlib != tc.want {
				t.Errorf("case is marked as NOT clamping but time.AddDate answers %s, want %s", stdlib, tc.want)
			}
		})
	}
}

// TestCalendarOffsetCarriesTheTimeOfDay pins that a calendar offset moves the
// DATE and leaves the clock alone — including when the day clamps, which is the
// case a rebuilt-from-parts implementation is most likely to lose.
func TestCalendarOffsetCarriesTheTimeOfDay(t *testing.T) {
	anchor := time.Date(2026, time.March, 31, 12, 30, 45, 0, time.UTC)
	cases := map[string]struct {
		n    int
		unit string
		want string
	}{
		"clamping month": {-1, UnitMonths, "2026-02-28T12:30:45Z"},
		"clean month":    {-4, UnitMonths, "2025-11-30T12:30:45Z"},
		"year":           {-1, UnitYears, "2025-03-31T12:30:45Z"},
		"quarter":        {-1, UnitQuarters, "2025-12-31T12:30:45Z"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := resolved(t, anchor, AnchorNow, tc.n, tc.unit, SnapNone); got != tc.want {
				t.Errorf("got %s, want %s", got, tc.want)
			}
		})
	}
}

// TestFixedLengthOffsetsNeverClamp covers the four units that are pure duration.
// A day offset that crosses a month end must land on the first of the next month,
// NOT be pinned to it — the clamp belongs to the calendar units alone, and a
// clamp leaking into these would silently shorten every "in the last 90 days"
// window that spans a month end.
func TestFixedLengthOffsetsNeverClamp(t *testing.T) {
	cases := []struct {
		name   string
		anchor time.Time
		n      int
		unit   string
		want   string
	}{
		{"a day off a 31-day month", utc(2026, time.March, 31), 1, UnitDays, "2026-04-01T00:00:00Z"},
		{"a day off january 31", utc(2026, time.January, 31), 1, UnitDays, "2026-02-01T00:00:00Z"},
		{"a day off february in a common year", utc(2026, time.February, 28), 1, UnitDays, "2026-03-01T00:00:00Z"},
		{"a day off february in a leap year", utc(2024, time.February, 28), 1, UnitDays, "2024-02-29T00:00:00Z"},
		{"a day back across new year", utc(2026, time.January, 1), -1, UnitDays, "2025-12-31T00:00:00Z"},
		{"ninety days back", utc(2026, time.March, 4), -90, UnitDays, "2025-12-04T00:00:00Z"},
		{"a week back off a month end", utc(2026, time.March, 31), -1, UnitWeeks, "2026-03-24T00:00:00Z"},
		{"two weeks forward", utc(2026, time.February, 25), 2, UnitWeeks, "2026-03-11T00:00:00Z"},
		{"twelve hours across midnight", time.Date(2026, time.March, 4, 12, 30, 45, 0, time.UTC), 12, UnitHours, "2026-03-05T00:30:45Z"},
		{"thirty minutes back", time.Date(2026, time.March, 4, 12, 30, 45, 0, time.UTC), -30, UnitMinutes, "2026-03-04T12:00:45Z"},
		{"minutes across a year end", time.Date(2025, time.December, 31, 23, 45, 0, 0, time.UTC), 30, UnitMinutes, "2026-01-01T00:15:00Z"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolved(t, tc.anchor, AnchorNow, tc.n, tc.unit, SnapNone); got != tc.want {
				t.Errorf("got %s, want %s", got, tc.want)
			}
		})
	}
}

// TestSnapsResolveToTheirBoundaries covers all eleven snaps against one anchor —
// Wednesday 2026-03-04T12:30:45Z, deliberately mid-week, mid-month, mid-quarter
// and mid-year so no boundary coincides with the anchor and a snap that silently
// did nothing would be visible.
func TestSnapsResolveToTheirBoundaries(t *testing.T) {
	if got := pinnedInstant.Weekday(); got != time.Wednesday {
		t.Fatalf("the pinned instant is a %s; these expectations assume Wednesday", got)
	}
	cases := map[string]string{
		SnapNone:           "2026-03-04T12:30:45Z",
		SnapStartOfYear:    "2026-01-01T00:00:00Z",
		SnapEndOfYear:      "2026-12-31T23:59:59Z",
		SnapStartOfQuarter: "2026-01-01T00:00:00Z",
		SnapEndOfQuarter:   "2026-03-31T23:59:59Z",
		SnapStartOfMonth:   "2026-03-01T00:00:00Z",
		SnapEndOfMonth:     "2026-03-31T23:59:59Z",
		SnapStartOfWeek:    "2026-03-02T00:00:00Z",
		SnapEndOfWeek:      "2026-03-08T23:59:59Z",
		SnapStartOfDay:     "2026-03-04T00:00:00Z",
		SnapEndOfDay:       "2026-03-04T23:59:59Z",
	}
	if len(cases) != len(relativeSnaps) {
		t.Fatalf("this table covers %d snaps but the vocabulary has %d", len(cases), len(relativeSnaps))
	}
	for snap, want := range cases {
		t.Run(snap, func(t *testing.T) {
			if _, ok := relativeSnaps[snap]; !ok {
				t.Fatalf("%q is not a member of the snap vocabulary", snap)
			}
			if got := resolved(t, pinnedInstant, AnchorNow, 0, UnitDays, snap); got != want {
				t.Errorf("got %s, want %s", got, want)
			}
		})
	}
}

// TestEndSnapsAreTheLastRepresentableInstant pins the endOf* precision decision:
// 23:59:59 on the period's last day, NOT midnight of the next period.
//
// The choice is load-bearing because `between` is inclusive at both bounds. An
// exclusive next-boundary would admit the first instant of the following period
// as well — a whole extra day of access nobody asked for — while a bound at
// 23:59:59 admits the final day and nothing beyond it. Seconds rather than
// nanoseconds because the value model floors to whole seconds, so there is no
// representable instant in between.
func TestEndSnapsAreTheLastRepresentableInstant(t *testing.T) {
	for _, snap := range []string{SnapEndOfYear, SnapEndOfQuarter, SnapEndOfMonth, SnapEndOfWeek, SnapEndOfDay} {
		t.Run(snap, func(t *testing.T) {
			got := resolved(t, pinnedInstant, AnchorNow, 0, UnitDays, snap)
			if !strings.HasSuffix(got, "T23:59:59Z") {
				t.Errorf("%s resolved to %s; every end-of snap must be the last second of its period", snap, got)
			}
		})
	}

	// The boundary itself, through a real inclusive between: the last second of
	// March is inside an endOfMonth bound and the first second of April is not.
	n := Between(Var("object.hired_at"),
		RelativeDate(AnchorNow, 0, UnitDays, SnapStartOfMonth),
		RelativeDate(AnchorNow, 0, UnitDays, SnapEndOfMonth))
	boundary := map[string]bool{
		"2026-03-01T00:00:00Z": true,  // the lower bound, inclusive
		"2026-03-31T23:59:58Z": true,  // a second inside
		"2026-03-31T23:59:59Z": true,  // the upper bound, inclusive
		"2026-03-31":           true,  // the whole final day, at date granularity
		"2026-04-01T00:00:00Z": false, // the next period's first instant
		"2026-02-28T23:59:59Z": false, // the previous period's last instant
	}
	for value, want := range boundary {
		if got := evalAgainst(t, n, map[string]any{"hired_at": value}, pinnedInstant); got != want {
			t.Errorf("between startOfMonth and endOfMonth: %s = %v, want %v", value, got, want)
		}
	}
}

// TestWeekSnapsAreISOMonday is the off-by-one-week test. Go's Weekday counts from
// Sunday, so a Sunday anchor is the case an implementation that forgets to
// translate gets wrong — it would snap forward into the NEXT week rather than back
// to the Monday that started this one.
func TestWeekSnapsAreISOMonday(t *testing.T) {
	cases := []struct {
		name               string
		anchor             time.Time
		weekday            time.Weekday
		wantStart, wantEnd string
	}{
		{"monday", utc(2026, time.March, 2), time.Monday, "2026-03-02T00:00:00Z", "2026-03-08T23:59:59Z"},
		{"tuesday", utc(2026, time.March, 3), time.Tuesday, "2026-03-02T00:00:00Z", "2026-03-08T23:59:59Z"},
		{"saturday", utc(2026, time.March, 7), time.Saturday, "2026-03-02T00:00:00Z", "2026-03-08T23:59:59Z"},
		// THE CASE: a Sunday belongs to the week that started six days EARLIER.
		{"sunday", utc(2026, time.March, 8), time.Sunday, "2026-03-02T00:00:00Z", "2026-03-08T23:59:59Z"},
		// The Monday after that Sunday starts a new week.
		{"the next monday", utc(2026, time.March, 9), time.Monday, "2026-03-09T00:00:00Z", "2026-03-15T23:59:59Z"},
		// A week that spans a month and a year boundary.
		{"first of a month", utc(2026, time.April, 1), time.Wednesday, "2026-03-30T00:00:00Z", "2026-04-05T23:59:59Z"},
		{"new year's day", utc(2027, time.January, 1), time.Friday, "2026-12-28T00:00:00Z", "2027-01-03T23:59:59Z"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The fixture states the weekday it assumes, so a wrong date in the
			// table fails as a wrong date rather than as a wrong boundary.
			if got := tc.anchor.Weekday(); got != tc.weekday {
				t.Fatalf("%s is a %s, not a %s", tc.anchor.Format("2006-01-02"), got, tc.weekday)
			}
			if got := resolved(t, tc.anchor, AnchorNow, 0, UnitDays, SnapStartOfWeek); got != tc.wantStart {
				t.Errorf("startOfWeek = %s, want %s", got, tc.wantStart)
			}
			if got := resolved(t, tc.anchor, AnchorNow, 0, UnitDays, SnapEndOfWeek); got != tc.wantEnd {
				t.Errorf("endOfWeek = %s, want %s", got, tc.wantEnd)
			}
		})
	}
}

// TestQuarterSnapsAreCalendarQuarters pins Jan–Mar, Apr–Jun, Jul–Sep, Oct–Dec:
// any day in February snaps back to January 1st, and a fiscal-year reading (which
// would put February in a different quarter) is not what this vocabulary means.
func TestQuarterSnapsAreCalendarQuarters(t *testing.T) {
	cases := []struct {
		anchor     time.Time
		start, end string
	}{
		{utc(2026, time.January, 1), "2026-01-01T00:00:00Z", "2026-03-31T23:59:59Z"},
		{utc(2026, time.February, 14), "2026-01-01T00:00:00Z", "2026-03-31T23:59:59Z"},
		{utc(2026, time.March, 31), "2026-01-01T00:00:00Z", "2026-03-31T23:59:59Z"},
		{utc(2026, time.April, 1), "2026-04-01T00:00:00Z", "2026-06-30T23:59:59Z"},
		{utc(2026, time.June, 30), "2026-04-01T00:00:00Z", "2026-06-30T23:59:59Z"},
		{utc(2026, time.July, 1), "2026-07-01T00:00:00Z", "2026-09-30T23:59:59Z"},
		{utc(2026, time.August, 20), "2026-07-01T00:00:00Z", "2026-09-30T23:59:59Z"},
		{utc(2026, time.October, 1), "2026-10-01T00:00:00Z", "2026-12-31T23:59:59Z"},
		{utc(2026, time.December, 31), "2026-10-01T00:00:00Z", "2026-12-31T23:59:59Z"},
		// A leap year changes February's length but not the quarter's end.
		{utc(2024, time.February, 29), "2024-01-01T00:00:00Z", "2024-03-31T23:59:59Z"},
	}
	for _, tc := range cases {
		t.Run(tc.anchor.Format("2006-01-02"), func(t *testing.T) {
			if got := resolved(t, tc.anchor, AnchorNow, 0, UnitDays, SnapStartOfQuarter); got != tc.start {
				t.Errorf("startOfQuarter = %s, want %s", got, tc.start)
			}
			if got := resolved(t, tc.anchor, AnchorNow, 0, UnitDays, SnapEndOfQuarter); got != tc.end {
				t.Errorf("endOfQuarter = %s, want %s", got, tc.end)
			}
		})
	}
}

// TestMonthEndSnapKnowsFebruary is the leap-year half of endOfMonth, which is
// where a hard-coded 28 or a rolled-forward March 1 would show up.
func TestMonthEndSnapKnowsFebruary(t *testing.T) {
	cases := map[string]string{
		"2026-02-10": "2026-02-28T23:59:59Z", // common year
		"2024-02-10": "2024-02-29T23:59:59Z", // leap year
		"2100-02-10": "2100-02-28T23:59:59Z", // century, not a leap year
		"2000-02-10": "2000-02-29T23:59:59Z", // 400-year leap
	}
	for day, want := range cases {
		t.Run(day, func(t *testing.T) {
			anchor, err := time.Parse("2006-01-02", day)
			if err != nil {
				t.Fatalf("fixture: %v", err)
			}
			if got := resolved(t, anchor, AnchorNow, 0, UnitDays, SnapEndOfMonth); got != want {
				t.Errorf("endOfMonth = %s, want %s", got, want)
			}
		})
	}
}

// TestSnapIsAppliedBeforeTheOffset pins the ORDER, and pins it by showing that
// the other order gives a different answer — so the choice is visibly
// load-bearing rather than an accident of how the code happens to read.
//
// The node reads left to right the way it is spoken: "the start of the month,
// plus a day" is the 2nd of THIS month. Offsetting first and snapping afterwards
// would answer the 1st of the NEXT month for a month-end anchor, which is a
// month's difference from one field's ordering.
func TestSnapIsAppliedBeforeTheOffset(t *testing.T) {
	cases := []struct {
		name                   string
		anchor                 time.Time
		n                      int
		unit, snap             string
		wantSnapFirst          string
		wantOffsetFirstInstead string
	}{
		{
			name: "start of month, plus a day", anchor: utc(2026, time.January, 31),
			n: 1, unit: UnitDays, snap: SnapStartOfMonth,
			wantSnapFirst: "2026-01-02T00:00:00Z", wantOffsetFirstInstead: "2026-02-01T00:00:00Z",
		},
		{
			name: "start of month, plus thirty days", anchor: utc(2026, time.March, 4),
			n: 30, unit: UnitDays, snap: SnapStartOfMonth,
			wantSnapFirst: "2026-03-31T00:00:00Z", wantOffsetFirstInstead: "2026-04-01T00:00:00Z",
		},
		{
			name: "end of week, minus two days", anchor: utc(2026, time.March, 8),
			n: -2, unit: UnitDays, snap: SnapEndOfWeek,
			wantSnapFirst: "2026-03-06T23:59:59Z", wantOffsetFirstInstead: "2026-03-08T23:59:59Z",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolved(t, tc.anchor, AnchorNow, tc.n, tc.unit, tc.snap); got != tc.wantSnapFirst {
				t.Errorf("got %s, want %s (snap first, then offset)", got, tc.wantSnapFirst)
			}
			// The reverse order, computed here from the same primitives, must
			// disagree — otherwise the case does not pin anything.
			offsetFirst, ok := offsetInstant(tc.anchor, tc.n, tc.unit)
			if !ok {
				t.Fatalf("offsetInstant denied the fixture")
			}
			reverse, ok := snapInstant(offsetFirst, tc.snap)
			if !ok {
				t.Fatalf("snapInstant denied the fixture")
			}
			if got := reverse.Format("2006-01-02T15:04:05Z"); got != tc.wantOffsetFirstInstead {
				t.Errorf("the reverse order gives %s, want %s", got, tc.wantOffsetFirstInstead)
			}
			if tc.wantSnapFirst == tc.wantOffsetFirstInstead {
				t.Errorf("the two orders agree on this case, so it pins nothing")
			}
		})
	}
}

// TestRelativeDateWorkedExamples resolves the two examples the node's
// documentation carries, against the pinned clock. They are the reason the
// feature exists, so they are asserted as values rather than left implicit in the
// tables above.
func TestRelativeDateWorkedExamples(t *testing.T) {
	t.Run("three months prior to today", func(t *testing.T) {
		got := resolved(t, pinnedInstant, AnchorToday, -3, UnitMonths, SnapNone)
		if want := "2025-12-04T00:00:00Z"; got != want {
			t.Errorf("got %s, want %s", got, want)
		}
	})

	t.Run("start of the year, five years back", func(t *testing.T) {
		got := resolved(t, pinnedInstant, AnchorNow, -5, UnitYears, SnapStartOfYear)
		if want := "2021-01-01T00:00:00Z"; got != want {
			t.Errorf("got %s, want %s", got, want)
		}
	})

	// The composed form: five years of history through the end of today, as an
	// inclusive between — the shape the documentation shows.
	t.Run("year to date plus five years of history", func(t *testing.T) {
		n := Between(Var("object.hired_at"),
			RelativeDate(AnchorNow, -5, UnitYears, SnapStartOfYear),
			RelativeDate(AnchorToday, 0, UnitDays, SnapEndOfDay))
		if err := n.Validate(); err != nil {
			t.Fatalf("validate: %v", err)
		}
		cases := map[string]bool{
			"2020-12-31T23:59:59Z": false, // a second before the window opens
			"2021-01-01":           true,  // the lower bound, inclusive
			"2024-06-15":           true,  // inside
			"2026-03-04T23:59:59Z": true,  // the last second of today, inclusive
			"2026-03-05":           false, // tomorrow
		}
		for value, want := range cases {
			if got := evalAgainst(t, n, map[string]any{"hired_at": value}, pinnedInstant); got != want {
				t.Errorf("hired_at %s = %v, want %v", value, got, want)
			}
		}
	})
}

// TestCalendarArithmeticIsUTCWhateverTheProcessZone runs the whole table again
// with the process's local zone set to a zone whose calendar day differs from
// UTC's. Every answer must be byte-identical: the engine has no timezone knob and
// nothing in the arithmetic may consult time.Local.
func TestCalendarArithmeticIsUTCWhateverTheProcessZone(t *testing.T) {
	fixtures := []struct {
		anchor     time.Time
		n          int
		unit, snap string
	}{
		{pinnedInstant, 0, UnitDays, SnapStartOfDay},
		{pinnedInstant, 0, UnitDays, SnapEndOfDay},
		{pinnedInstant, 0, UnitDays, SnapStartOfWeek},
		{pinnedInstant, 0, UnitDays, SnapStartOfYear},
		{pinnedInstant, -3, UnitMonths, SnapNone},
		{utc(2026, time.March, 31), -1, UnitMonths, SnapNone},
		// Late evening UTC is the previous day in the Americas and the next day
		// in Asia, so a leaked zone moves this one's calendar day.
		{time.Date(2026, time.March, 4, 23, 30, 0, 0, time.UTC), 0, UnitDays, SnapStartOfDay},
		{time.Date(2026, time.March, 4, 0, 30, 0, 0, time.UTC), 0, UnitDays, SnapEndOfMonth},
	}

	want := make([]string, len(fixtures))
	for i, f := range fixtures {
		want[i] = resolved(t, f.anchor, AnchorToday, f.n, f.unit, f.snap)
	}

	// TZ is set for completeness; time.Local is replaced outright because the
	// standard library reads TZ once, at first use, which may already have
	// happened in this process.
	t.Setenv("TZ", "America/New_York")
	saved := time.Local
	t.Cleanup(func() { time.Local = saved })
	time.Local = time.FixedZone("EST", -5*60*60)

	for i, f := range fixtures {
		got := resolved(t, f.anchor, AnchorToday, f.n, f.unit, f.snap)
		if got != want[i] {
			t.Errorf("fixture %d resolved to %s under a -05:00 local zone, want %s", i, got, want[i])
		}
	}

	// A clock that answers in another zone is converted, not trusted: the same
	// instant expressed as +05:00 resolves to the same UTC calendar day.
	zoned := time.Date(2026, time.March, 4, 17, 30, 45, 0, time.FixedZone("plus5", 5*60*60))
	if got, want := resolved(t, zoned, AnchorToday, 0, UnitDays, SnapEndOfMonth), "2026-03-31T23:59:59Z"; got != want {
		t.Errorf("a +05:00 instant resolved to %s, want %s", got, want)
	}
}

// TestCalendarSourceNeverTouchesAZoneOrAddDate is the structural half of the UTC
// rule, and the ratchet on the clamp.
//
// It parses the package's non-test sources and fails on any reference to
// time.Local, time.LoadLocation, or AddDate. The first two would introduce a
// timezone knob the engine deliberately does not have; the third is the standard
// library's NORMALISING calendar walk, which this package exists to diverge from
// — a future "simplification" back to AddDate would pass every fixture that does
// not land on a long month end, so it is blocked here rather than left to review.
//
// The check runs over the AST, not the text, so the rationale comments that name
// AddDate do not trip it.
func TestCalendarSourceNeverTouchesAZoneOrAddDate(t *testing.T) {
	banned := map[string]string{
		"AddDate":      "the standard library normalises at month end; this package clamps (see calendar.go)",
		"Local":        "every instant in the engine is UTC; there is no timezone knob",
		"LoadLocation": "every instant in the engine is UTC; there is no timezone knob",
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	var scanned int
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		scanned++
		ast.Inspect(file, func(node ast.Node) bool {
			sel, ok := node.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if why, bad := banned[sel.Sel.Name]; bad {
				t.Errorf("%s references %s: %s", fset.Position(sel.Pos()), sel.Sel.Name, why)
			}
			return true
		})
	}
	if scanned == 0 {
		t.Fatal("scanned no source files; the guard is not running")
	}
}

// TestCalendarArithmeticHasNoDSTDiscontinuity pins that UTC makes DST a
// non-issue by construction. Each anchor below is a transition day in some
// populated zone; in UTC a day is exactly 24 hours on every one of them, so
// offsetting by a day and by 24 hours must agree and no hour may go missing or
// repeat.
func TestCalendarArithmeticHasNoDSTDiscontinuity(t *testing.T) {
	transitions := map[string]time.Time{
		"US spring forward":   utc(2026, time.March, 8),
		"US fall back":        utc(2026, time.November, 1),
		"EU spring forward":   utc(2026, time.March, 29),
		"EU fall back":        utc(2026, time.October, 25),
		"southern transition": utc(2026, time.April, 5),
	}
	for name, anchor := range transitions {
		t.Run(name, func(t *testing.T) {
			v, ok := resolveRelativeDate(anchor, AnchorToday, 1, UnitDays, SnapNone)
			if !ok {
				t.Fatalf("a day after %s did not resolve", anchor.Format("2006-01-02"))
			}
			byHours, ok := resolveRelativeDate(anchor, AnchorToday, 24, UnitHours, SnapNone)
			if !ok {
				t.Fatalf("24 hours after %s did not resolve", anchor.Format("2006-01-02"))
			}
			if v.String() != byHours.String() {
				t.Errorf("a day (%s) and 24 hours (%s) disagree; a zone transition has leaked in",
					v.String(), byHours.String())
			}
			if got := v.Time().Sub(anchor); got != 24*time.Hour {
				t.Errorf("a day across %s is %v, want 24h", name, got)
			}
			// The day's own boundaries are still midnight to 23:59:59.
			if got, want := resolved(t, anchor, AnchorNow, 0, UnitDays, SnapStartOfDay),
				anchor.Format("2006-01-02")+"T00:00:00Z"; got != want {
				t.Errorf("startOfDay = %s, want %s", got, want)
			}
			if got, want := resolved(t, anchor, AnchorNow, 0, UnitDays, SnapEndOfDay),
				anchor.Format("2006-01-02")+"T23:59:59Z"; got != want {
				t.Errorf("endOfDay = %s, want %s", got, want)
			}
		})
	}
}

// TestUnrepresentableOffsetsDeny pins the out-of-range policy: an offset that
// leaves the four-digit year range the canonical forms can write, or that
// overflows a duration, resolves to NOTHING rather than to a wrapped instant or
// to text no parser accepts. The enclosing operator then denies, exactly as it
// does for a missing reference instant.
func TestUnrepresentableOffsetsDeny(t *testing.T) {
	denied := []struct {
		name string
		n    int
		unit string
	}{
		{"years before year one", -3000, UnitYears},
		{"years past the four-digit range", 9000, UnitYears},
		{"months past the four-digit range", 12 * 9000, UnitMonths},
		{"quarters before year one", -4 * 3000, UnitQuarters},
		{"days past the four-digit range", 3_000_000, UnitDays},
		{"weeks that overflow a duration", 2_000_000_000, UnitWeeks},
		{"minutes that overflow a duration", -2_000_000_000, UnitMinutes},
	}
	for _, tc := range denied {
		t.Run(tc.name, func(t *testing.T) {
			if v, ok := resolveRelativeDate(pinnedInstant, AnchorNow, tc.n, tc.unit, SnapNone); ok {
				t.Errorf("resolved to %s; an unrepresentable date must deny", v.String())
			}
			// Deny-safe end to end: the rule still compiles and still evaluates.
			n := Compare(OpOnOrBefore, Var("object.hired_at"), RelativeDate(AnchorNow, tc.n, tc.unit, SnapNone))
			if got := evalAgainst(t, n, map[string]any{"hired_at": "2020-01-01"}, pinnedInstant); got {
				t.Errorf("an unresolved relative date must deny")
			}
		})
	}

	// Large but representable offsets still resolve, so the guard is a range
	// check and not a cap on useful offsets.
	allowed := []struct {
		name string
		n    int
		unit string
		want string
	}{
		{"a century forward", 100, UnitYears, "2126-03-04T12:30:45Z"},
		{"a century back", -100, UnitYears, "1926-03-04T12:30:45Z"},
		{"ten thousand days", 10_000, UnitDays, "2053-07-20T12:30:45Z"},
	}
	for _, tc := range allowed {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolved(t, pinnedInstant, AnchorNow, tc.n, tc.unit, SnapNone); got != tc.want {
				t.Errorf("got %s, want %s", got, tc.want)
			}
		})
	}
}

// TestDaysInMonthMatchesTheCalendar cross-checks the explicit month-length table
// against an independent oracle — time.Date's "day zero of the next month", which
// is the standard library's own normalisation read backwards. The table is
// explicit precisely so the clamp does not depend on that normalisation; this is
// where the two are reconciled, once, rather than at every call site.
func TestDaysInMonthMatchesTheCalendar(t *testing.T) {
	for y := 1996; y <= 2104; y++ {
		for m := time.January; m <= time.December; m++ {
			want := time.Date(y, m+1, 0, 0, 0, 0, 0, time.UTC).Day()
			if got := daysInMonth(y, m); got != want {
				t.Errorf("daysInMonth(%d, %s) = %d, want %d", y, m, got, want)
			}
		}
	}
	// The three leap rules, named.
	for year, leap := range map[int]bool{2024: true, 2026: false, 1900: false, 2000: true, 2100: false, 2400: true} {
		if got := isLeapYear(year); got != leap {
			t.Errorf("isLeapYear(%d) = %v, want %v", year, got, leap)
		}
	}
}
