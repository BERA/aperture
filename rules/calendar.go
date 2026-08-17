package rules

import "time"

// This file is the CALENDAR ARITHMETIC behind the relative-date node: the two
// halves of resolveRelativeDate that turn an anchor instant into the date an
// author actually meant — snapping to a calendar boundary, and offsetting by a
// number of units.
//
// ORDER OF OPERATIONS: SNAP FIRST, THEN OFFSET. The node reads left to right the
// way it is spoken — "the start of the year, five years back" — so the boundary
// is taken on the anchor and the offset is applied to that boundary. The reverse
// order is a genuinely different function, not a rearrangement of the same one
// (start of the month, plus one day, is the second of THIS month; plus one day
// then start of the month is the first of NEXT month when the anchor is a month
// end), which is why the order is stated here, tested for a case where the two
// disagree, and treated as API rather than as an implementation detail.
//
// ⚠️ CALENDAR OFFSETS CLAMP. THE STANDARD LIBRARY NORMALISES.
//
// time.AddDate — and time.Date, which it is built on — rolls an out-of-range day
// FORWARD into the following month:
//
//	2026-03-31.AddDate(0, -1, 0)  ->  2026-03-03   (Feb 31 rolled forward)
//	2026-05-31.AddDate(0, -1, 0)  ->  2026-05-01   (Apr 31 rolled forward)
//	2024-02-29.AddDate(-1, 0, 0)  ->  2023-03-01   (Feb 29 2023 does not exist)
//
// So "one month before March 31st" lands in March, a two-day window where the
// author asked for a one-month one. That failure is silent — no error, no note,
// the rule simply decides — and it is input-dependent: the same rule is right on
// most days and wrong only when the anchor falls on a long month end, so neither
// a reviewer reading the rule nor a fixture written on an arbitrary date will
// catch it.
//
// This package CLAMPS instead: the day is pinned to the last valid day of the
// target month, so 2026-03-31 minus one month is 2026-02-28 and 2024-02-29 minus
// one year is 2023-02-28. That is what java.time, Luxon, and date-fns all do, so
// it is what an author who has met any other date library already expects. It
// applies to the three CALENDAR units (years, quarters, months); weeks, days,
// hours, and minutes are fixed-length and cannot land on a day that does not
// exist, so they are added as durations and never clamp.
//
// Because the divergence is the whole point, nothing here calls time.AddDate and
// nothing here relies on time.Date's normalisation: the target month's last day
// is computed from an explicit table (daysInMonth) and every constructed instant
// is already in range. A test asserts the two disagree exactly where they should,
// so a future edit that "simplifies" this to AddDate fails loudly.
//
// EVERYTHING IS UTC. Every boundary is constructed with time.UTC and no code path
// consults time.Local or time.LoadLocation — there is no timezone knob anywhere
// in the engine. DST is therefore a non-issue by construction: UTC has no
// transitions, so adding 24 hours always advances exactly one calendar day.
//
// OUT OF RANGE IS DENY, NOT WRAP. An offset large enough to leave the four-digit
// year range the canonical string forms can express (or to overflow a
// time.Duration) resolves to nothing rather than to a wrapped or unformattable
// instant. The caller's deny-safe policy then applies, which is the same answer a
// missing reference instant gets. An instant that cannot be written in the
// canonical form is not a date this system has.

// The year range the two canonical string forms can express. Both layouts write
// the year as exactly four digits, so an instant outside this range would format
// into text that no parser in this system accepts — a value that compares as
// nothing and reads as a typo. Resolution denies instead.
const (
	minCanonicalYear = 1
	maxCanonicalYear = 9999
)

// daysPerMonth is the length of each month in a common year, indexed by
// (month - 1). February is corrected for leap years by daysInMonth.
//
// The table is explicit rather than derived from time.Date(y, m+1, 0, ...) — the
// "day zero of the next month" trick — precisely because that trick works by
// invoking the normalisation this file exists to avoid. A reader checking whether
// the clamp is right should not have to reason about normalisation to do it.
var daysPerMonth = [12]int{31, 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31}

// isLeapYear reports whether y is a leap year in the proleptic Gregorian
// calendar: divisible by 4, except centuries, except every fourth century.
func isLeapYear(y int) bool {
	return y%4 == 0 && (y%100 != 0 || y%400 == 0)
}

// daysInMonth returns the number of days in month m of year y — the value a
// calendar offset clamps to.
func daysInMonth(y int, m time.Month) int {
	if m == time.February && isLeapYear(y) {
		return 29
	}
	return daysPerMonth[int(m)-1]
}

// inCanonicalRange reports whether t can be written in the canonical forms.
func inCanonicalRange(t time.Time) bool {
	y := t.Year()
	return y >= minCanonicalYear && y <= maxCanonicalYear
}

// snapInstant rounds t to a calendar boundary, reporting false only for a snap
// outside the vocabulary (unreachable through a validated AST, denied anyway).
//
// A start-of boundary is midnight on the first day of the period. An end-of
// boundary is the LAST REPRESENTABLE INSTANT of the period — 23:59:59 on its last
// day, not midnight of the next one.
//
// THAT CHOICE IS LOAD-BEARING, and it is inclusive-end rather than
// exclusive-start for one reason: `between` is inclusive at both bounds, so an
// endOfMonth upper bound has to admit the whole of the final day. An exclusive
// next-boundary would admit the first instant of the following month as well,
// which is a whole extra day of access nobody asked for. 23:59:59 rather than
// 23:59:59.999999999 because the model floors to whole seconds everywhere (see
// provider.DateTimeLayout); there is no representable instant between them.
//
// Week boundaries are ISO 8601: a week runs MONDAY through SUNDAY. Go's
// time.Weekday counts from Sunday, so the offset back to the week's start is
// (weekday + 6) % 7 — 0 for Monday, 6 for Sunday. Getting that wrong moves a
// Sunday anchor by a whole week, which is why both ends are tested explicitly.
//
// Quarters are calendar quarters — Jan–Mar, Apr–Jun, Jul–Sep, Oct–Dec — so any
// day in February snaps back to January 1st.
func snapInstant(t time.Time, snap string) (time.Time, bool) {
	y, m, d := t.Date()
	switch snap {
	case SnapNone:
		return t, true

	case SnapStartOfYear:
		return startOfDayIn(y, time.January, 1), true
	case SnapEndOfYear:
		return endOfDayIn(y, time.December, 31), true

	case SnapStartOfQuarter:
		return startOfDayIn(y, quarterStartMonth(m), 1), true
	case SnapEndOfQuarter:
		last := quarterStartMonth(m) + 2
		return endOfDayIn(y, last, daysInMonth(y, last)), true

	case SnapStartOfMonth:
		return startOfDayIn(y, m, 1), true
	case SnapEndOfMonth:
		return endOfDayIn(y, m, daysInMonth(y, m)), true

	case SnapStartOfWeek:
		return startOfDayIn(y, m, d).Add(-time.Duration(isoWeekdayIndex(t)) * day), true
	case SnapEndOfWeek:
		start := startOfDayIn(y, m, d).Add(-time.Duration(isoWeekdayIndex(t)) * day)
		end := start.Add(6 * day)
		return endOfDayIn(end.Year(), end.Month(), end.Day()), true

	case SnapStartOfDay:
		return startOfDayIn(y, m, d), true
	case SnapEndOfDay:
		return endOfDayIn(y, m, d), true
	}
	// Unreachable through a validated AST; deny-safe regardless.
	return time.Time{}, false
}

// The fixed-length units, as durations. In UTC a day is always exactly 24 hours
// and a week exactly seven of those — there are no transitions to absorb — so
// these units are addition and never touch the calendar.
const (
	day  = 24 * time.Hour
	week = 7 * day
)

// offsetInstant moves t by n of unit, clamping the three calendar units at month
// end and adding the four fixed-length ones as durations. It reports false for an
// unknown unit and for an offset that leaves the representable range.
//
// n == 0 is the identity for every unit — zero of anything is the instant itself
// — which is why "no offset" needs no separate spelling in the node.
func offsetInstant(t time.Time, n int, unit string) (time.Time, bool) {
	if n == 0 {
		return t, true
	}
	switch unit {
	case UnitYears:
		return addMonthsClamped(t, int64(n)*12)
	case UnitQuarters:
		return addMonthsClamped(t, int64(n)*3)
	case UnitMonths:
		return addMonthsClamped(t, int64(n))
	case UnitWeeks:
		return addFixed(t, n, week)
	case UnitDays:
		return addFixed(t, n, day)
	case UnitHours:
		return addFixed(t, n, time.Hour)
	case UnitMinutes:
		return addFixed(t, n, time.Minute)
	}
	// Unreachable through a validated AST; deny-safe regardless.
	return time.Time{}, false
}

// addMonthsClamped is THE CLAMP: it advances t by months calendar months and
// pins the day to the last day of the target month when the original day does not
// exist there.
//
// 2026-01-31 plus one month is 2026-02-28 (2024-01-31 plus one month is
// 2024-02-29, because 2024 is a leap year); 2026-03-31 minus one month is
// 2026-02-28. time.AddDate would answer 2026-03-03 for the last of those. The
// time-of-day component is carried through untouched, so a snapped end-of-day
// stays an end-of-day.
//
// The month index is accumulated in int64 so a large offset cannot wrap before
// the range check sees it; the result is denied rather than truncated.
func addMonthsClamped(t time.Time, months int64) (time.Time, bool) {
	y, m, d := t.Date()

	total := int64(y)*12 + int64(m-1) + months
	year := total / 12
	index := total % 12
	if index < 0 {
		// Go truncates toward zero; a calendar needs floor division, so a
		// negative remainder borrows a year.
		index += 12
		year--
	}
	if year < minCanonicalYear || year > maxCanonicalYear {
		return time.Time{}, false
	}

	month := time.Month(index + 1)
	if last := daysInMonth(int(year), month); d > last {
		d = last
	}
	return time.Date(int(year), month, d, t.Hour(), t.Minute(), t.Second(), 0, time.UTC), true
}

// addFixed adds n steps of a fixed-length unit, reporting false when the product
// overflows a time.Duration. The overflow check is a division rather than a
// bound: it is exact for every step size and needs no per-unit constant.
func addFixed(t time.Time, n int, step time.Duration) (time.Time, bool) {
	total := time.Duration(n) * step
	if total/step != time.Duration(n) {
		return time.Time{}, false
	}
	return t.Add(total), true
}

// quarterStartMonth returns the first month of the calendar quarter m falls in.
func quarterStartMonth(m time.Month) time.Month {
	return time.Month((int(m)-1)/3*3 + 1)
}

// isoWeekdayIndex returns the number of days from the ISO week's Monday to t:
// 0 for Monday through 6 for Sunday. Go's Weekday counts from Sunday == 0.
func isoWeekdayIndex(t time.Time) int {
	return (int(t.Weekday()) + 6) % 7
}

// startOfDayIn builds midnight UTC on the given calendar day. Every component is
// in range at every call site, so no normalisation can occur.
func startOfDayIn(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// endOfDayIn builds the last representable instant of the given calendar day —
// 23:59:59 UTC, whole seconds, matching the canonical form's precision.
func endOfDayIn(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 23, 59, 59, 0, time.UTC)
}
