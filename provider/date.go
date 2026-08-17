package provider

import (
	"errors"
	"strings"
	"time"

	aerr "github.com/frankbardon/aperture/errors"
)

// The shared date value model.
//
// Three packages need one definition of what a date is: the CSV loader (typed
// columns), the seed document (its field schema), and the rules engine (operand
// parsing at evaluation time). provider is the only package all three may
// import, so the definition lives here, beside the metadata value model it
// extends.
//
// It EXTENDS that model rather than changing it. A date is a plain string
// scalar — the standing decision that time.Time is not a metadata scalar is
// upheld, not overturned (see the note in metadata.go). What this file adds is
// the constraint that a string a loader has been told is a date must be one of
// exactly TWO canonical forms:
//
//	DateLayout      "2006-01-02"            an RFC 3339 full-date
//	DateTimeLayout  "2006-01-02T15:04:05Z"  an RFC 3339 date-time, always Z
//
// and nothing else. Granularity is carried by the string itself — a value
// containing a 'T' is a timestamp, one that does not is a calendar day — so no
// consumer needs a separate type tag travelling alongside the value, and a
// canonical value round-trips through JSON, YAML, CSV, and the expression
// environment as the string it already is.
//
// Three properties are load-bearing, and each exists because its absence is a
// silent wrong answer rather than a loud failure:
//
//   - UTC ONLY, and an explicit offset is REJECTED rather than converted.
//     Converting is instant-correct and calendar-surprising: a host that writes
//     2026-01-01T00:00:00+05:00 means January 1st, and canonicalising it yields
//     2025-12-31T19:00:00Z, which answers a "same year" question with 2025. An
//     offset-free timestamp is read as UTC (accepted, because it asserts no
//     other zone) and a Z suffix is accepted; +05:00 / -05:00 is a load error.
//
//   - FRACTIONAL SECONDS TRUNCATE, they never round. Rounding can carry a value
//     across the boundary a rule is testing, which is the entire class of defect
//     this model exists to prevent.
//
//   - PARSING IS MANDATORY; comparing the canonical strings is not a permissible
//     shortcut. The two forms sort correctly within one granularity but not
//     across it: the date-only form is a strict prefix of the timestamp form, so
//     "2026-03-04" sorts before "2026-03-04T00:00:00Z" as a string even though
//     the two name the same instant. A cutoff written against a date-only field
//     and compared to a timestamp would be wrong at exactly the boundary
//     instant. DateValue.Compare compares the parsed instants; see
//     TestDateOrderingDisagreesWithStringOrder.
//
// The value model itself stays date-blind on purpose: ValidateField cannot know
// which strings a host means as dates, so it keeps accepting any string scalar.
// Declaring a field to be a date — and so subjecting it to ParseDateValue — is a
// loader's job, through its own typed column or field schema.

const (
	// DateLayout is the canonical date-only form: an RFC 3339 full-date, a
	// calendar day with no clock and no zone.
	DateLayout = "2006-01-02"
	// DateTimeLayout is the canonical timestamp form: an RFC 3339 date-time at
	// second granularity, always in UTC and always spelled with a literal Z.
	// The Z is a literal here, not Go's Z07:00 zone element, so formatting can
	// never emit a numeric offset and parsing with this layout can never accept
	// one.
	DateTimeLayout = "2006-01-02T15:04:05Z"

	// dateTimeBodyLayout is the zone-free layout the parser actually uses. The
	// zone suffix is inspected as TEXT before parsing (an offset must be
	// rejected, not converted), so by the time this layout is applied the value
	// carries no zone at all and time.Parse yields a UTC instant.
	dateTimeBodyLayout = "2006-01-02T15:04:05"
)

// DateGranularity reports which of the two canonical forms a DateValue carries.
// It is descriptive, not a mode: a parser reports the granularity it found, no
// caller selects one.
type DateGranularity uint8

const (
	// GranularityUnknown is the zero DateGranularity, carried only by the zero
	// DateValue. No parse ever produces it.
	GranularityUnknown DateGranularity = iota
	// GranularityDate is the date-only form, "2006-01-02". Its instant is
	// midnight UTC on that calendar day.
	GranularityDate
	// GranularityDateTime is the timestamp form, "2006-01-02T15:04:05Z".
	GranularityDateTime
)

// String renders the granularity for a diagnostic. It names the FORM, never a
// value, so it is safe in an error context.
func (g DateGranularity) String() string {
	switch g {
	case GranularityDate:
		return "date"
	case GranularityDateTime:
		return "datetime"
	default:
		return "unknown"
	}
}

// DateValue is a parsed, canonical Aperture date: an instant that is always in
// UTC and always at second granularity, plus the form it was written in.
//
// Its fields are unexported deliberately. Every DateValue in existence comes
// from ParseDateValue, DateValueOf, DateOf, or DateTimeOf, so there is no way to
// express one that is in another zone, carries sub-second precision, or claims a
// granularity its instant does not support. The zero DateValue is the only
// invalid one, and IsZero identifies it.
type DateValue struct {
	instant     time.Time
	granularity DateGranularity
}

// DateOf returns the calendar day t falls on, as a date-only DateValue.
//
// t is converted to UTC FIRST and the day is the UTC day, matching the rest of
// the model: there is no zone knob anywhere in Aperture, so a non-UTC t is a
// caller's instant expressed elsewhere, not a request for a local calendar.
func DateOf(t time.Time) DateValue {
	t = t.UTC()
	return DateValue{
		instant:     time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC),
		granularity: GranularityDate,
	}
}

// DateTimeOf returns t as a timestamp DateValue: converted to UTC, with its
// sub-second component TRUNCATED (never rounded, for the boundary reason in the
// file comment).
func DateTimeOf(t time.Time) DateValue {
	t = t.UTC()
	return DateValue{
		instant:     time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), 0, time.UTC),
		granularity: GranularityDateTime,
	}
}

// Time returns the parsed instant, always in time.UTC and always with a zero
// nanosecond component.
func (v DateValue) Time() time.Time { return v.instant }

// Granularity reports which canonical form the value carries.
func (v DateValue) Granularity() DateGranularity { return v.granularity }

// IsZero reports whether v is the zero DateValue — the one value no parse and no
// constructor produces.
func (v DateValue) IsZero() bool { return v.granularity == GranularityUnknown }

// String renders the canonical text for the value's granularity: "2006-01-02"
// for a date, "2006-01-02T15:04:05Z" for a timestamp. The zero DateValue renders
// as the empty string.
//
// This is the form a loader STORES. Canonicalising twice is a no-op, which
// TestCanonicalizeDateIsIdempotent pins.
func (v DateValue) String() string {
	switch v.granularity {
	case GranularityDate:
		return v.instant.Format(DateLayout)
	case GranularityDateTime:
		return v.instant.Format(DateTimeLayout)
	default:
		return ""
	}
}

// Compare returns -1, 0, or +1 as v's instant is before, equal to, or after
// other's. It compares INSTANTS, so it is correct across granularities where
// comparing the canonical strings is not: "2026-03-04" and "2026-03-04T00:00:00Z"
// compare equal here and unequal as text. Every date operator in the rules engine
// is built on this, never on string order.
func (v DateValue) Compare(other DateValue) int { return v.instant.Compare(other.instant) }

// DateReason classifies WHY a date string was rejected. It exists so a caller
// can branch on the cause, and a test assert it, without matching on message
// text — and so a loader can render its own diagnostic (naming its column, line,
// and field) without ever echoing the cell.
type DateReason string

const (
	// DateReasonEmpty is an empty string. A loader that treats an empty cell as
	// an ABSENT field must check for that before calling the parser; reaching
	// the parser with "" is a rejection.
	DateReasonEmpty DateReason = "empty"
	// DateReasonLayout is a value that is not one of the two canonical forms —
	// a different separator, an unpadded component, a missing seconds field, a
	// non-RFC 3339 spelling, or trailing text.
	DateReasonLayout DateReason = "layout"
	// DateReasonCalendar is a well-formed value naming a date or clock time that
	// does not exist: 2026-02-30, 2026-13-01, 2023-02-29, an hour of 25.
	DateReasonCalendar DateReason = "calendar"
	// DateReasonNonUTCOffset is a timestamp carrying an explicit numeric offset
	// (+05:00, -05:00). It is rejected rather than converted because converting
	// silently moves the calendar day, and with it the answer to any question
	// about the day, month, or year.
	DateReasonNonUTCOffset DateReason = "non_utc_offset"
)

// dateReasonMessages is the fixed message per rejection cause. Every message is
// a CONSTANT: none is formatted from the offending value, because a date can be
// personal data — a birth date, a termination date — and an error is a thing
// that gets logged.
var dateReasonMessages = map[DateReason]string{
	DateReasonEmpty:        "provider: date value is empty",
	DateReasonLayout:       "provider: date value is not in a canonical form",
	DateReasonCalendar:     "provider: date value names a date or time that does not exist",
	DateReasonNonUTCOffset: "provider: date value carries an explicit non-UTC offset; store the instant in UTC with a Z suffix",
}

// ParseDateValue parses a canonical Aperture date. It is the entry point a
// loader calls for every value it has been told is a date.
//
// Accepted:
//
//	2026-03-04                    a full-date
//	2026-03-04T01:02:03Z          a date-time in UTC
//	2026-03-04T01:02:03           a date-time with no offset, READ AS UTC
//	2026-03-04T01:02:03.456Z      fractional seconds, TRUNCATED to 01:02:03
//
// Rejected, each as its own DateReason:
//
//	""                            DateReasonEmpty
//	2026-03-04T01:02:03+05:00     DateReasonNonUTCOffset
//	2026-02-30, 2026-13-01        DateReasonCalendar
//	03/04/2026, 2026-3-4          DateReasonLayout
//
// The returned error is APERTURE_CONFIG_INVALID and NEVER carries the offending
// value — not in its message, not in its context, and not through a wrapped
// *time.ParseError (whose own Error string quotes the input verbatim, which is
// why this function classifies that error and then discards it). Callers add
// their own locating context — column, line, field — around it.
func ParseDateValue(s string) (DateValue, error) {
	v, reason := parseDateValue(s)
	if reason != "" {
		return DateValue{}, aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
			dateReasonMessages[reason],
			map[string]any{"reason": string(reason), "expected": DateLayout + " or " + DateTimeLayout})
	}
	return v, nil
}

// DateValueOf is the evaluation-time form of ParseDateValue: it accepts any
// metadata value and reports ok rather than building an error.
//
// The rules engine calls it on the Check hot path, where a rejection is
// deny-safe — the comparison is false and a note is recorded — so constructing a
// coded error per failed compare would be pure allocation for a diagnostic
// nobody reads. It also absorbs the wrong-TYPE case: metadata reaching the
// engine from a host-implemented ObjectProvider never passed through a loader,
// so a date field may legitimately hold a number or an array.
//
// Only a string can be a date. The metadata value model has no time.Time, by the
// standing decision in metadata.go.
func DateValueOf(value any) (DateValue, bool) {
	s, ok := value.(string)
	if !ok {
		return DateValue{}, false
	}
	v, reason := parseDateValue(s)
	return v, reason == ""
}

// CanonicalizeDate parses s and returns its canonical text — the convenience a
// loader wants when it is storing the value rather than comparing it.
//
// It is idempotent: canonicalising an already-canonical value returns it
// unchanged.
func CanonicalizeDate(s string) (string, error) {
	v, err := ParseDateValue(s)
	if err != nil {
		return "", err
	}
	return v.String(), nil
}

// DateReasonOf reports the DateReason carried by an error returned from this
// package's date parsing, or "" for any other error. It is how a caller branches
// on the cause — a loader rendering a specific diagnostic, a test asserting a
// distinct rejection — without matching on message text.
func DateReasonOf(err error) DateReason {
	var ce *aerr.CodedError
	if !errors.As(err, &ce) {
		return ""
	}
	reason, _ := ce.Context["reason"].(string)
	switch DateReason(reason) {
	case DateReasonEmpty, DateReasonLayout, DateReasonCalendar, DateReasonNonUTCOffset:
		return DateReason(reason)
	default:
		return ""
	}
}

// parseDateValue is the shared parse core. It returns an empty DateReason on
// success and allocates nothing on either path, so the hot-path caller
// (DateValueOf) and the load-path caller (ParseDateValue) share one definition
// of what is accepted rather than drifting apart.
func parseDateValue(s string) (DateValue, DateReason) {
	switch {
	case s == "":
		return DateValue{}, DateReasonEmpty
	case len(s) == len(DateLayout):
		t, err := time.Parse(DateLayout, s)
		if err != nil {
			return DateValue{}, classifyDateParseError(err)
		}
		// A layout carrying no zone yields a UTC instant. That is what the
		// model wants, but it is implicit in time.Parse, so
		// TestOffsetFreeValuesAreReadAsUTC asserts it rather than trusting it.
		return DateValue{instant: t, granularity: GranularityDate}, ""
	case len(s) > len(DateLayout)+1 && s[len(DateLayout)] == 'T':
		return parseDateTimeValue(s)
	default:
		return DateValue{}, DateReasonLayout
	}
}

// parseDateTimeValue parses the timestamp form. The zone suffix is examined as
// TEXT first: an explicit offset has to be distinguishable from a layout error,
// because it is the one rejection a host is likely to hit while holding
// perfectly reasonable data, and it needs its own diagnostic.
func parseDateTimeValue(s string) (DateValue, DateReason) {
	// Only the part after the date can carry a zone sign; the date itself is
	// full of '-'.
	if strings.ContainsAny(s[len(DateLayout):], "+-") {
		return DateValue{}, DateReasonNonUTCOffset
	}
	body := strings.TrimSuffix(s, "Z")
	t, err := time.Parse(dateTimeBodyLayout, body)
	if err != nil {
		return DateValue{}, classifyDateParseError(err)
	}
	// Rebuilding through time.Date drops the sub-second component by
	// construction. time.Truncate would do the same thing arithmetically, but
	// "the nanoseconds are dropped" is the property worth being able to read off
	// the code.
	return DateValue{
		instant:     time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), 0, time.UTC),
		granularity: GranularityDateTime,
	}, ""
}

// classifyDateParseError separates "that date does not exist" from "that is not
// the layout". time.Parse reports a range violation with a ": <field> out of
// range" message and a layout mismatch with an element pair, so the distinction
// is available — but ONLY the classification is kept. The *time.ParseError is
// never wrapped or rendered: its Error string quotes the input value, and an
// "extra text" message embeds a fragment of it.
func classifyDateParseError(err error) DateReason {
	var pe *time.ParseError
	if errors.As(err, &pe) && strings.HasSuffix(pe.Message, " out of range") {
		return DateReasonCalendar
	}
	return DateReasonLayout
}
