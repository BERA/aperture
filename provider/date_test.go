package provider

import (
	"strings"
	"testing"
	"time"

	aerr "github.com/frankbardon/aperture/errors"
)

// utc is the pinned-clock helper every date test builds its expectations from.
// No test in this file reads time.Now(): a date model whose expectations come
// from the real clock only exercises its interesting paths on the days the clock
// happens to land on one.
func utc(y int, m time.Month, d, hh, mm, ss int) time.Time {
	return time.Date(y, m, d, hh, mm, ss, 0, time.UTC)
}

// TestParseDateValueAccepts pins the accepted input set, the instant each maps
// to, the granularity reported, and the canonical text stored.
func TestParseDateValueAccepts(t *testing.T) {
	tests := []struct {
		name        string
		in          string
		wantInstant time.Time
		wantGran    DateGranularity
		wantText    string
	}{
		{"full-date", "2026-03-04", utc(2026, 3, 4, 0, 0, 0), GranularityDate, "2026-03-04"},
		{"leap day", "2024-02-29", utc(2024, 2, 29, 0, 0, 0), GranularityDate, "2024-02-29"},
		{"first day of year", "2026-01-01", utc(2026, 1, 1, 0, 0, 0), GranularityDate, "2026-01-01"},
		{"date-time with Z", "2026-03-04T01:02:03Z", utc(2026, 3, 4, 1, 2, 3), GranularityDateTime, "2026-03-04T01:02:03Z"},
		{"date-time without offset", "2026-03-04T01:02:03", utc(2026, 3, 4, 1, 2, 3), GranularityDateTime, "2026-03-04T01:02:03Z"},
		{"midnight timestamp", "2026-03-04T00:00:00Z", utc(2026, 3, 4, 0, 0, 0), GranularityDateTime, "2026-03-04T00:00:00Z"},
		{"leap-day timestamp", "2024-02-29T23:59:59Z", utc(2024, 2, 29, 23, 59, 59), GranularityDateTime, "2024-02-29T23:59:59Z"},
		{"fractional seconds with Z", "2026-03-04T01:02:03.456Z", utc(2026, 3, 4, 1, 2, 3), GranularityDateTime, "2026-03-04T01:02:03Z"},
		{"fractional seconds without offset", "2026-03-04T01:02:03.456", utc(2026, 3, 4, 1, 2, 3), GranularityDateTime, "2026-03-04T01:02:03Z"},
		{"fractional seconds truncate, never round", "2026-03-04T01:02:03.999999999Z", utc(2026, 3, 4, 1, 2, 3), GranularityDateTime, "2026-03-04T01:02:03Z"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, err := ParseDateValue(tc.in)
			if err != nil {
				t.Fatalf("ParseDateValue(%q) = %v, want no error", tc.in, err)
			}
			if !v.Time().Equal(tc.wantInstant) {
				t.Errorf("instant = %v, want %v", v.Time(), tc.wantInstant)
			}
			if v.Time().Nanosecond() != 0 {
				t.Errorf("instant carries %d ns, want a whole second", v.Time().Nanosecond())
			}
			if v.Granularity() != tc.wantGran {
				t.Errorf("granularity = %v, want %v", v.Granularity(), tc.wantGran)
			}
			if got := v.String(); got != tc.wantText {
				t.Errorf("String() = %q, want %q", got, tc.wantText)
			}
			if v.IsZero() {
				t.Error("IsZero() = true for a parsed value")
			}
			// DateValueOf must accept exactly what ParseDateValue accepts.
			if _, ok := DateValueOf(tc.in); !ok {
				t.Errorf("DateValueOf(%q) = false, want true", tc.in)
			}
		})
	}
}

// TestParseDateValueRejects pins each rejection as its own testable condition.
// The assertion is on the CODE and the DateReason, never on message text.
func TestParseDateValueRejects(t *testing.T) {
	tests := []struct {
		name       string
		in         string
		wantReason DateReason
	}{
		{"empty string", "", DateReasonEmpty},
		{"positive offset", "2026-01-01T00:00:00+05:00", DateReasonNonUTCOffset},
		{"negative offset", "2026-01-01T00:00:00-05:00", DateReasonNonUTCOffset},
		{"compact positive offset", "2026-01-01T00:00:00+0500", DateReasonNonUTCOffset},
		{"zero offset spelled numerically", "2026-01-01T00:00:00+00:00", DateReasonNonUTCOffset},
		{"day out of range", "2026-02-30", DateReasonCalendar},
		{"month out of range", "2026-13-01", DateReasonCalendar},
		{"february 29 in a common year", "2023-02-29", DateReasonCalendar},
		{"hour out of range", "2026-03-04T25:00:00Z", DateReasonCalendar},
		{"US slash layout", "03/04/2026", DateReasonLayout},
		{"unpadded components", "2026-3-4", DateReasonLayout},
		{"month name", "Mar 4 2026", DateReasonLayout},
		{"space separator", "2026-03-04 01:02:03", DateReasonLayout},
		{"lowercase designators", "2026-03-04t01:02:03z", DateReasonLayout},
		{"missing seconds", "2026-03-04T01:02", DateReasonLayout},
		{"date with a stray Z", "2026-03-04Z", DateReasonLayout},
		{"trailing text", "2026-03-04T01:02:03Z later", DateReasonLayout},
		{"whitespace padding", " 2026-03-04 ", DateReasonLayout},
		{"not a date at all", "gold", DateReasonLayout},
		{"unix seconds", "1772582400", DateReasonLayout},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, err := ParseDateValue(tc.in)
			if err == nil {
				t.Fatalf("ParseDateValue(%q) = %v, want an error", tc.in, v)
			}
			if got := aerr.CodeOf(err); got != aerr.APERTURE_CONFIG_INVALID {
				t.Errorf("code = %q, want %q", got, aerr.APERTURE_CONFIG_INVALID)
			}
			if got := DateReasonOf(err); got != tc.wantReason {
				t.Errorf("DateReasonOf = %q, want %q", got, tc.wantReason)
			}
			if !v.IsZero() {
				t.Errorf("rejected input yielded %v, want the zero DateValue", v)
			}
			if _, ok := DateValueOf(tc.in); ok {
				t.Errorf("DateValueOf(%q) = true, want false", tc.in)
			}
			if _, err := CanonicalizeDate(tc.in); err == nil {
				t.Errorf("CanonicalizeDate(%q) = nil error, want a rejection", tc.in)
			}
		})
	}
}

// TestDateErrorNeverCarriesTheValue is the cross-account leak guard, mirroring
// TestErrorNeverCarriesTheValue in metadata_test.go. A date can be personal data
// — a birth date, a termination date — so a rejection names the cause and the
// expected layouts, never the value. This also guards against wrapping
// *time.ParseError, whose Error string quotes the input verbatim.
func TestDateErrorNeverCarriesTheValue(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"offset", "1979-07-14T00:00:00+05:00"},
		{"impossible date", "1979-02-30"},
		{"bad layout", "14/07/1979"},
		{"trailing text", "1979-07-14T00:00:00Z terminated"},
		{"not a date", "1979-07-14-terminated"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := func() error {
				_, err := ParseDateValue(tc.in)
				return err
			}()
			if err == nil {
				t.Fatal("want an error")
			}
			// Every distinctive fragment of the input, down to the digits, must
			// be absent from the rendered error.
			for _, fragment := range []string{tc.in, "1979", "07-14", "terminated"} {
				if strings.Contains(err.Error(), fragment) {
					t.Errorf("error message leaks %q: %q", fragment, err.Error())
				}
			}
			var ce *aerr.CodedError
			if !asCoded(err, &ce) {
				t.Fatal("error is not an *aerr.CodedError")
			}
			if ce.Inner != nil {
				t.Errorf("error wraps %v; a *time.ParseError quotes the input and must never be wrapped", ce.Inner)
			}
			for k, v := range ce.Context {
				s, ok := v.(string)
				if !ok {
					continue
				}
				for _, fragment := range []string{tc.in, "1979", "terminated"} {
					if strings.Contains(s, fragment) {
						t.Errorf("error context %q leaks %q: %q", k, fragment, s)
					}
				}
			}
		})
	}
}

// TestCanonicalizeDateIsIdempotent pins the round-trip property:
// canonicalize(canonicalize(x)) == canonicalize(x) over every accepted form,
// including the ones that are not already canonical.
func TestCanonicalizeDateIsIdempotent(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"2026-03-04", "2026-03-04"},
		{"2024-02-29", "2024-02-29"},
		{"2026-03-04T01:02:03Z", "2026-03-04T01:02:03Z"},
		{"2026-03-04T01:02:03", "2026-03-04T01:02:03Z"},
		{"2026-03-04T01:02:03.456Z", "2026-03-04T01:02:03Z"},
		{"2026-03-04T01:02:03.999999999", "2026-03-04T01:02:03Z"},
		{"2026-03-04T00:00:00Z", "2026-03-04T00:00:00Z"},
		{"0001-01-01", "0001-01-01"},
		{"9999-12-31T23:59:59Z", "9999-12-31T23:59:59Z"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			once, err := CanonicalizeDate(tc.in)
			if err != nil {
				t.Fatalf("CanonicalizeDate(%q) = %v", tc.in, err)
			}
			if once != tc.want {
				t.Fatalf("CanonicalizeDate(%q) = %q, want %q", tc.in, once, tc.want)
			}
			twice, err := CanonicalizeDate(once)
			if err != nil {
				t.Fatalf("CanonicalizeDate(%q) = %v, want the canonical form to re-parse", once, err)
			}
			if twice != once {
				t.Fatalf("canonicalize is not idempotent: %q -> %q -> %q", tc.in, once, twice)
			}
		})
	}
}

// TestDateOrderingAgreesWithInstants is the ordering property: comparing parsed
// values agrees with the semantic order of the instants they name.
func TestDateOrderingAgreesWithInstants(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		want int
	}{
		{"same day", "2026-03-04", "2026-03-04", 0},
		{"earlier day", "2026-03-04", "2026-03-05", -1},
		{"later day", "2026-03-05", "2026-03-04", 1},
		{"same timestamp", "2026-03-04T01:02:03Z", "2026-03-04T01:02:03Z", 0},
		{"one second apart", "2026-03-04T01:02:03Z", "2026-03-04T01:02:04Z", -1},
		{"offset-free equals Z", "2026-03-04T01:02:03", "2026-03-04T01:02:03Z", 0},
		{"fraction truncates into equality", "2026-03-04T01:02:03.999Z", "2026-03-04T01:02:03Z", 0},
		{"day precedes a later time that day", "2026-03-04", "2026-03-04T00:00:01Z", -1},
		{"day follows an earlier day's timestamp", "2026-03-04", "2026-03-03T23:59:59Z", 1},
		{"year boundary", "2025-12-31T23:59:59Z", "2026-01-01", -1},
		{"leap day precedes march", "2024-02-29", "2024-03-01", -1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a, err := ParseDateValue(tc.a)
			if err != nil {
				t.Fatalf("ParseDateValue(%q) = %v", tc.a, err)
			}
			b, err := ParseDateValue(tc.b)
			if err != nil {
				t.Fatalf("ParseDateValue(%q) = %v", tc.b, err)
			}
			if got := a.Compare(b); got != tc.want {
				t.Errorf("Compare = %d, want %d", got, tc.want)
			}
			if got, want := b.Compare(a), -tc.want; got != want {
				t.Errorf("reversed Compare = %d, want %d", got, want)
			}
			if got, want := a.Compare(b), a.Time().Compare(b.Time()); got != want {
				t.Errorf("Compare = %d but the instants compare %d", got, want)
			}
		})
	}
}

// TestDateOrderingDisagreesWithStringOrder is the reason parsing exists at all.
// The date-only form is a strict PREFIX of the timestamp form, so at the
// granularity boundary the canonical strings sort in an order the instants do
// not share. A rule that compared the stored text would be wrong at exactly the
// boundary instant — the classic location for an access-control defect.
func TestDateOrderingDisagreesWithStringOrder(t *testing.T) {
	const day, midnight = "2026-03-04", "2026-03-04T00:00:00Z"

	if !(day < midnight) {
		t.Fatalf("premise changed: %q no longer sorts before %q as text", day, midnight)
	}

	a, err := ParseDateValue(day)
	if err != nil {
		t.Fatalf("ParseDateValue(%q) = %v", day, err)
	}
	b, err := ParseDateValue(midnight)
	if err != nil {
		t.Fatalf("ParseDateValue(%q) = %v", midnight, err)
	}
	if got := a.Compare(b); got != 0 {
		t.Fatalf("Compare(%q, %q) = %d, want 0 — they are the same instant", day, midnight, got)
	}
	if !a.Time().Equal(b.Time()) {
		t.Fatalf("instants differ: %v vs %v", a.Time(), b.Time())
	}
	// Same instant, different form: granularity is carried by the string and
	// survives the round trip, but it never affects ordering.
	if a.Granularity() == b.Granularity() {
		t.Fatalf("both values report %v; the granularity boundary is not being reported", a.Granularity())
	}
	if a.String() == b.String() {
		t.Fatalf("both values render as %q; the canonical forms must stay distinct", a.String())
	}
}

// TestOffsetFreeValuesAreReadAsUTC asserts the implicit half of the model:
// time.Parse with a zone-free layout yields a UTC instant, which is what the
// model wants but gets silently. The date-only form is checked the same way.
func TestOffsetFreeValuesAreReadAsUTC(t *testing.T) {
	for _, in := range []string{"2026-03-04", "2026-03-04T01:02:03", "2026-03-04T01:02:03Z"} {
		t.Run(in, func(t *testing.T) {
			v, err := ParseDateValue(in)
			if err != nil {
				t.Fatalf("ParseDateValue(%q) = %v", in, err)
			}
			if loc := v.Time().Location(); loc != time.UTC {
				t.Fatalf("location = %v, want time.UTC", loc)
			}
			if _, offset := v.Time().Zone(); offset != 0 {
				t.Fatalf("zone offset = %d, want 0", offset)
			}
		})
	}
}

// TestRejectedOffsetWouldHaveMovedTheCalendarDay documents WHY the offset
// rejection is a feature. It reconstructs what canonicalising would have
// produced and shows the calendar year moving.
func TestRejectedOffsetWouldHaveMovedTheCalendarDay(t *testing.T) {
	const written = "2026-01-01T00:00:00+05:00"

	if _, err := ParseDateValue(written); err == nil {
		t.Fatalf("ParseDateValue(%q) accepted an explicit offset", written)
	} else if got := DateReasonOf(err); got != DateReasonNonUTCOffset {
		t.Fatalf("DateReasonOf = %q, want %q", got, DateReasonNonUTCOffset)
	}

	// What accepting it would have meant: the host wrote January 1st 2026 and
	// every calendar question would have answered 2025.
	asWritten, err := time.Parse(time.RFC3339, written)
	if err != nil {
		t.Fatalf("time.Parse = %v", err)
	}
	converted := DateTimeOf(asWritten)
	if got, want := converted.String(), "2025-12-31T19:00:00Z"; got != want {
		t.Fatalf("converted = %q, want %q", got, want)
	}
	if got := converted.Time().Year(); got != 2025 {
		t.Fatalf("converted year = %d, want 2025", got)
	}
}

// TestDateOfConstructors covers the two ways to build a DateValue from a
// time.Time: both convert to UTC, and both drop precision the model does not
// carry.
func TestDateOfConstructors(t *testing.T) {
	fractional := time.Date(2026, 3, 4, 1, 2, 3, 999999999, time.UTC)

	if got, want := DateOf(fractional).String(), "2026-03-04"; got != want {
		t.Errorf("DateOf = %q, want %q", got, want)
	}
	if got, want := DateOf(fractional).Granularity(), GranularityDate; got != want {
		t.Errorf("DateOf granularity = %v, want %v", got, want)
	}
	if got, want := DateTimeOf(fractional).String(), "2026-03-04T01:02:03Z"; got != want {
		t.Errorf("DateTimeOf = %q, want %q — fractional seconds must truncate, not round", got, want)
	}
	if got, want := DateTimeOf(fractional).Granularity(), GranularityDateTime; got != want {
		t.Errorf("DateTimeOf granularity = %v, want %v", got, want)
	}

	// A non-UTC input is converted, never treated as a local calendar: the
	// instant 2026-01-01T00:00:00+05:00 falls on December 31st in UTC.
	east := time.FixedZone("east", 5*60*60)
	shifted := time.Date(2026, 1, 1, 0, 0, 0, 0, east)
	if got, want := DateOf(shifted).String(), "2025-12-31"; got != want {
		t.Errorf("DateOf(non-UTC) = %q, want %q", got, want)
	}
	if loc := DateOf(shifted).Time().Location(); loc != time.UTC {
		t.Errorf("DateOf(non-UTC) location = %v, want time.UTC", loc)
	}
	if got, want := DateTimeOf(shifted).String(), "2025-12-31T19:00:00Z"; got != want {
		t.Errorf("DateTimeOf(non-UTC) = %q, want %q", got, want)
	}

	// Constructed values round-trip through the canonical text.
	for _, v := range []DateValue{DateOf(fractional), DateTimeOf(fractional)} {
		parsed, err := ParseDateValue(v.String())
		if err != nil {
			t.Fatalf("ParseDateValue(%q) = %v", v.String(), err)
		}
		if parsed.Compare(v) != 0 || parsed.Granularity() != v.Granularity() {
			t.Fatalf("round trip of %q lost information: %v/%v", v.String(), parsed.Time(), parsed.Granularity())
		}
	}
}

// TestDateValueOfNonStrings covers the evaluation-time entry point's other job:
// metadata from a host-implemented ObjectProvider never passed a loader, so a
// date field can legitimately hold anything. It must report false, not panic and
// not allocate an error.
func TestDateValueOfNonStrings(t *testing.T) {
	values := []any{
		nil,
		int64(1772582400),
		float64(20260304),
		true,
		[]any{"2026-03-04"},
		map[string]any{"date": "2026-03-04"},
		time.Date(2026, 3, 4, 0, 0, 0, 0, time.UTC),
	}
	for _, value := range values {
		v, ok := DateValueOf(value)
		if ok {
			t.Errorf("DateValueOf(%T) = true, want false", value)
		}
		if !v.IsZero() {
			t.Errorf("DateValueOf(%T) returned %v, want the zero DateValue", value, v)
		}
	}
}

// TestZeroDateValue pins the one invalid DateValue: the zero value, which no
// parse and no constructor produces.
func TestZeroDateValue(t *testing.T) {
	var v DateValue
	if !v.IsZero() {
		t.Error("zero DateValue reports IsZero() = false")
	}
	if got := v.String(); got != "" {
		t.Errorf("zero DateValue renders as %q, want the empty string", got)
	}
	if got := v.Granularity(); got != GranularityUnknown {
		t.Errorf("zero granularity = %v, want %v", got, GranularityUnknown)
	}
	if got := v.Granularity().String(); got != "unknown" {
		t.Errorf("GranularityUnknown.String() = %q, want %q", got, "unknown")
	}
	if got := GranularityDate.String(); got != "date" {
		t.Errorf("GranularityDate.String() = %q, want %q", got, "date")
	}
	if got := GranularityDateTime.String(); got != "datetime" {
		t.Errorf("GranularityDateTime.String() = %q, want %q", got, "datetime")
	}
}

// TestDateReasonOfNonDateErrors keeps the classifier from claiming errors that
// are not date rejections.
func TestDateReasonOfNonDateErrors(t *testing.T) {
	if got := DateReasonOf(nil); got != "" {
		t.Errorf("DateReasonOf(nil) = %q, want %q", got, "")
	}
	metadataErr := ValidateField("tags", []string{"a"})
	if metadataErr == nil {
		t.Fatal("want a value-model error to test against")
	}
	if got := DateReasonOf(metadataErr); got != "" {
		t.Errorf("DateReasonOf(value-model error) = %q, want %q", got, "")
	}
	other := aerr.WithContext(aerr.APERTURE_CONFIG_INVALID, "unrelated",
		map[string]any{"reason": "something else"})
	if got := DateReasonOf(other); got != "" {
		t.Errorf("DateReasonOf(foreign reason) = %q, want %q", got, "")
	}
}

// TestDatesRideInsideTheMetadataValueModel pins the decision this story settled:
// a date is a plain string SCALAR, so the value model is unchanged and needs no
// date-aware shape. The model stays date-blind — declaring a field to be a date
// is a loader's job, not ValidateField's.
func TestDatesRideInsideTheMetadataValueModel(t *testing.T) {
	md := Metadata{
		"hired_at":     "2026-03-04",
		"reviewed_at":  "2026-03-04T01:02:03Z",
		"review_dates": []any{"2026-03-04", "2026-04-01"},
		"employment":   map[string]any{"started": "2026-03-04"},
	}
	if err := ValidateMetadata(md); err != nil {
		t.Fatalf("ValidateMetadata = %v, want canonical dates to be ordinary string scalars", err)
	}
	// Date-blind by design: a non-date string is still a legal metadata value.
	if err := ValidateField("hired_at", "not a date"); err != nil {
		t.Fatalf("ValidateField = %v, want the value model to stay date-blind", err)
	}
	// And the model still refuses a time.Time, which is what makes the canonical
	// string the only representation a date can have.
	if err := ValidateField("hired_at", time.Date(2026, 3, 4, 0, 0, 0, 0, time.UTC)); err == nil {
		t.Fatal("ValidateField accepted a time.Time; it is deliberately not a scalar")
	} else if got := aerr.CodeOf(err); got != aerr.APERTURE_METADATA_INVALID {
		t.Fatalf("code = %q, want %q", got, aerr.APERTURE_METADATA_INVALID)
	}
}
