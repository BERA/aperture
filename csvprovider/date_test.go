package csvprovider

import (
	"context"
	"fmt"
	"strings"
	"testing"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/identity"
	"github.com/frankbardon/aperture/provider"
)

// dateField loads a one-row file with a single date column and returns the
// stored metadata. Every accepting case in this file is the same shape, so the
// cases stay a table of value-in / value-out rather than a wall of loads.
func dateField(t *testing.T, header, cell string) provider.Metadata {
	t.Helper()
	p := New(write(t, strings.Join([]string{
		"id," + header,
		"brand:1," + cell,
	}, "\n")))
	md, err := p.Fetch(context.Background(), identity.MustParse("brand:1"))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	return md
}

// TestDateColumnsStoreCanonicalStrings walks the accepted forms. The stored
// value is the CANONICAL text, never the cell as written — which is the property
// that makes two rows naming one instant one string, and so makes a Filter
// equality predicate over the column mean anything.
func TestDateColumnsStoreCanonicalStrings(t *testing.T) {
	cases := []struct {
		name   string
		header string
		cell   string
		want   string
	}{
		{"calendar day", "hired_at:date", "2026-03-04", "2026-03-04"},
		{"timestamp", "last_seen:datetime", "2026-03-04T12:30:00Z", "2026-03-04T12:30:00Z"},
		{
			// No offset asserts no zone, so it is read as UTC and gains the Z.
			name:   "offset-free timestamp is read as UTC",
			header: "last_seen:datetime", cell: "2026-03-04T12:30:00",
			want: "2026-03-04T12:30:00Z",
		},
		{
			// Truncated, never rounded: rounding can carry a value across the
			// very boundary a rule is testing.
			name: "fractional seconds truncate", header: "last_seen:datetime",
			cell: "2026-03-04T12:30:59.999999999Z", want: "2026-03-04T12:30:59Z",
		},
		{"leap day", "hired_at:date", "2024-02-29", "2024-02-29"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			md := dateField(t, tc.header, tc.cell)
			field := strings.SplitN(tc.header, ":", 2)[0]
			got, ok := md[field].(string)
			if !ok {
				t.Fatalf("%s = %#v, want a string scalar", field, md[field])
			}
			if got != tc.want {
				t.Errorf("%s = %q, want %q", field, got, tc.want)
			}
			// The value model is unchanged by dates: the stored value is an
			// ordinary string scalar and still passes the shared validator.
			if err := provider.ValidateField(field, got); err != nil {
				t.Errorf("ValidateField rejected a stored date: %v", err)
			}
		})
	}
}

// TestDateEmptyCellOmitsField pins the empty-cell rule as ABSENCE, not an empty
// string and not a zero time. An absent date is meaningfully different from any
// date, and a zero time would silently satisfy every "before <anything>" rule
// ever written against the column.
func TestDateEmptyCellOmitsField(t *testing.T) {
	for _, header := range []string{"hired_at:date", "hired_at:datetime"} {
		t.Run(header, func(t *testing.T) {
			md := dateField(t, header, "")
			if v, present := md["hired_at"]; present {
				t.Errorf("empty cell should omit the field, got %#v", v)
			}
		})
	}
}

// TestDateCellRejections walks every way a declared date cell can fail. Each is
// APERTURE_CONFIG_INVALID naming the column and the line, and each parse failure
// carries the value model's own DateReason so a caller branches on the cause
// rather than on message text.
func TestDateCellRejections(t *testing.T) {
	cases := []struct {
		name   string
		header string
		cell   string
		reason provider.DateReason // "" when the failure is not a parse failure
	}{
		{"malformed layout", "hired_at:date", "03/04/2026", provider.DateReasonLayout},
		{"unpadded components", "hired_at:date", "2026-3-4", provider.DateReasonLayout},
		{"trailing text", "hired_at:date", "2026-03-04 and more", provider.DateReasonLayout},
		{"impossible day", "hired_at:date", "2026-02-30", provider.DateReasonCalendar},
		{"impossible month", "hired_at:date", "2026-13-01", provider.DateReasonCalendar},
		{"non-leap february", "hired_at:date", "2023-02-29", provider.DateReasonCalendar},
		{"impossible hour", "last_seen:datetime", "2026-03-04T25:00:00Z", provider.DateReasonCalendar},
		{
			// The one rejection a host is likely to hit while holding perfectly
			// reasonable data: converting would silently move the calendar day.
			name: "explicit positive offset", header: "last_seen:datetime",
			cell: "2026-03-04T12:30:00+05:00", reason: provider.DateReasonNonUTCOffset,
		},
		{
			name: "explicit negative offset", header: "last_seen:datetime",
			cell: "2026-03-04T12:30:00-05:00", reason: provider.DateReasonNonUTCOffset,
		},
		{
			// A valid date of the WRONG form for its declared column. Not a
			// parse failure, so no DateReason — the column type is the thing
			// that was violated.
			name: "timestamp in a date column", header: "hired_at:date",
			cell: "2026-03-04T12:30:00Z",
		},
		{
			name: "calendar day in a datetime column", header: "last_seen:datetime",
			cell: "2026-03-04",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := p_load(t, strings.Join([]string{
				"id," + tc.header,
				"brand:2," + tc.cell,
			}, "\n"))
			if got := aerr.CodeOf(err); got != aerr.APERTURE_CONFIG_INVALID {
				t.Fatalf("code = %s, want APERTURE_CONFIG_INVALID", got)
			}
			ctx := codedContext(t, err)
			field := strings.SplitN(tc.header, ":", 2)[0]
			if ctx["field"] != field {
				t.Errorf("context field = %#v, want %q", ctx["field"], field)
			}
			if ctx["line"] != 2 {
				t.Errorf("context line = %#v, want 2", ctx["line"])
			}
			if got := provider.DateReasonOf(err); got != tc.reason {
				t.Errorf("DateReasonOf = %q, want %q", got, tc.reason)
			}
		})
	}
}

// TestDateErrorNeverCarriesTheCell is the privacy gate. A date is frequently
// personal data — a birth date, a termination date — and an error is a thing
// that gets logged, so neither the message nor any context value may echo the
// cell. The value model's own error is dropped for the same reason, so the whole
// chain is checked, not only the outermost message.
func TestDateErrorNeverCarriesTheCell(t *testing.T) {
	cells := []struct {
		header string
		cell   string
	}{
		{"birth_date:date", "1974-13-09"},                       // impossible month
		{"birth_date:date", "09/11/1974"},                       // wrong layout
		{"terminated_at:datetime", "1974-09-11T08:15:00+05:30"}, // offset
		{"terminated_at:datetime", "1974-09-11"},                // wrong granularity
	}
	for _, tc := range cells {
		t.Run(tc.cell, func(t *testing.T) {
			_, err := p_load(t, strings.Join([]string{
				"id," + tc.header,
				"brand:1," + tc.cell,
			}, "\n"))
			if err == nil {
				t.Fatal("want a rejection")
			}
			rendered := err.Error() + " " + fmt.Sprint(codedContext(t, err))
			if strings.Contains(rendered, tc.cell) {
				t.Errorf("error echoes the cell %q: %s", tc.cell, rendered)
			}
			// The year alone is enough to identify a birth date, so check the
			// leading component too rather than only the whole value.
			if strings.Contains(rendered, tc.cell[:4]) {
				t.Errorf("error echoes a fragment of the cell: %s", rendered)
			}
		})
	}
}

// TestDateIsNotAListElementType pins that arrays of dates are out of scope, and
// that the rejection is a decision with its own message rather than an
// accidental "unknown element type".
func TestDateIsNotAListElementType(t *testing.T) {
	for _, elem := range []string{"date", "datetime"} {
		t.Run(elem, func(t *testing.T) {
			_, err := p_load(t, "id,windows:list<"+elem+">\nbrand:1,2026-03-04\n")
			if got := aerr.CodeOf(err); got != aerr.APERTURE_CONFIG_INVALID {
				t.Fatalf("code = %s, want APERTURE_CONFIG_INVALID", got)
			}
			ctx := codedContext(t, err)
			if ctx["name"] != "windows" || ctx["elem"] != elem {
				t.Errorf("context = %#v, want name=windows elem=%s", ctx, elem)
			}
			if !strings.Contains(err.Error(), "cannot hold dates") {
				t.Errorf("rejection should name the reason, got %q", err.Error())
			}
		})
	}
}

// TestDateHeaderSuffixComposition pins that a date column is an undecorated
// type: it takes no element type and no delimiter, exactly as :int and :json do.
// The last case is the grammar's one genuine ambiguity — the header is cut at
// its FIRST colon, so a column name cannot itself contain one.
func TestDateHeaderSuffixComposition(t *testing.T) {
	cases := map[string]struct {
		content string
		wantCtx map[string]any
	}{
		"element type on a date": {
			"id,hired_at:date<int>\nbrand:1,2026-03-04\n",
			map[string]any{"name": "hired_at", "type": "date"},
		},
		"delimiter on a date": {
			"id,hired_at:date(;)\nbrand:1,2026-03-04\n",
			map[string]any{"name": "hired_at", "type": "date"},
		},
		"delimiter on a datetime": {
			"id,seen:datetime(;)\nbrand:1,2026-03-04T00:00:00Z\n",
			map[string]any{"name": "seen", "type": "datetime"},
		},
		"colon in the column name": {
			// "hired:at:date" cuts to name "hired", type "at:date". The name is
			// everything before the FIRST colon, so a colon is not available in
			// a column name and the remainder is read as a type — an unknown
			// one, which is a hard error rather than a silently untyped column.
			"id,hired:at:date\nbrand:1,2026-03-04\n",
			map[string]any{"name": "hired", "type": "at:date"},
		},
		"unknown near-miss type": {
			"id,hired_at:timestamp\nbrand:1,2026-03-04\n",
			map[string]any{"name": "hired_at", "type": "timestamp"},
		},
		"time-of-day is not a type": {
			"id,opens_at:time\nbrand:1,09:00:00\n",
			map[string]any{"name": "opens_at", "type": "time"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := p_load(t, tc.content)
			if got := aerr.CodeOf(err); got != aerr.APERTURE_CONFIG_INVALID {
				t.Fatalf("code = %s, want APERTURE_CONFIG_INVALID", got)
			}
			ctx := codedContext(t, err)
			for k, want := range tc.wantCtx {
				if ctx[k] != want {
					t.Errorf("context[%q] = %#v, want %#v (full: %#v)", k, ctx[k], want, ctx)
				}
			}
		})
	}
}

// TestJSONCellIsNotDateValidated pins the boundary someone will assume runs the
// other way. :json is opaque structured data; only a DECLARED date column gets
// date treatment, so a date-shaped string inside a json cell is stored verbatim
// — non-canonical spelling, impossible day, explicit offset and all.
func TestJSONCellIsNotDateValidated(t *testing.T) {
	raw := `{"hired":"2026-13-45","seen":"2026-01-01T00:00:00+05:00","loose":"03/04/2026"}`
	md := dateField(t, "owner:json", jsonCell(raw))
	owner, ok := md["owner"].(map[string]any)
	if !ok {
		t.Fatalf("owner = %#v, want a map", md["owner"])
	}
	for k, want := range map[string]string{
		"hired": "2026-13-45",
		"seen":  "2026-01-01T00:00:00+05:00",
		"loose": "03/04/2026",
	} {
		if owner[k] != want {
			t.Errorf("owner[%q] = %#v, want %q stored verbatim", k, owner[k], want)
		}
	}
}

// TestDateQueryMatchesCanonicalStrings pins the Filter.Fields contract over a
// date column: EQUALITY over the canonical string, and nothing more. Range
// querying is not a provider concern — rules are where date ranges live — but
// equality has to work across spellings, which it does precisely because the
// loader stored the canonical form rather than the cell.
func TestDateQueryMatchesCanonicalStrings(t *testing.T) {
	p := New(write(t, strings.Join([]string{
		"id,hired_at:date,last_seen:datetime",
		"brand:1,2026-03-04,2026-03-04T12:30:00.750Z", // fractional, truncated at load
		"brand:2,2026-03-05,2026-03-04T12:30:00",      // offset-free, read as UTC
	}, "\n")))

	cases := []struct {
		name   string
		fields map[string]any
		want   []string
	}{
		{"exact day", map[string]any{"hired_at": "2026-03-04"}, []string{"brand:1"}},
		{
			// Both rows canonicalise to the same instant from different
			// spellings, so one predicate selects both.
			name:   "canonical timestamp across spellings",
			fields: map[string]any{"last_seen": "2026-03-04T12:30:00Z"},
			want:   []string{"brand:1", "brand:2"},
		},
		{
			// Equality is over the stored canonical string, so the predicate has
			// to be canonical too. This is the documented limit, not a bug.
			name:   "a non-canonical predicate matches nothing",
			fields: map[string]any{"last_seen": "2026-03-04T12:30:00.750Z"},
			want:   nil,
		},
		{
			name:   "granularity is not coerced in a predicate",
			fields: map[string]any{"last_seen": "2026-03-04"},
			want:   nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := p.Query(context.Background(), provider.Filter{Fields: tc.fields})
			if err != nil {
				t.Fatalf("Query: %v", err)
			}
			ids := make([]string, 0, len(got))
			for _, o := range got {
				ids = append(ids, o.ID.String())
			}
			if strings.Join(ids, ",") != strings.Join(tc.want, ",") {
				t.Errorf("ids = %v, want %v", ids, tc.want)
			}
		})
	}
}

// TestDatePackageDocExample loads the file from the package doc, so the
// documented shape cannot drift from the parser.
func TestDatePackageDocExample(t *testing.T) {
	p := New(write(t, strings.Join([]string{
		"id,tier,hired_at:date,last_seen:datetime",
		"brand:1,gold,2026-03-04,2026-03-04T12:30:00Z",
	}, "\n")))
	md, err := p.Fetch(context.Background(), identity.MustParse("brand:1"))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	want := map[string]any{
		"tier": "gold", "hired_at": "2026-03-04", "last_seen": "2026-03-04T12:30:00Z",
	}
	for k, v := range want {
		if md[k] != v {
			t.Errorf("%s = %#v, want %#v", k, md[k], v)
		}
	}
	// Round-trip: every stored value re-parses to itself through the shared
	// model, which is what "canonical" has to mean for the rules engine to be
	// able to read the column back without a second normalisation.
	for _, k := range []string{"hired_at", "last_seen"} {
		v, ok := provider.DateValueOf(md[k])
		if !ok {
			t.Fatalf("stored %s does not re-parse: %#v", k, md[k])
		}
		if v.String() != md[k] {
			t.Errorf("%s re-renders as %q, want %q", k, v.String(), md[k])
		}
	}
}
