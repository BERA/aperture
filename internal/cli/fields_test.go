package cli

import (
	"maps"
	"strings"
	"testing"

	aerr "github.com/frankbardon/aperture/errors"
)

// TestParseMetadataFilter pins the two spellings and, above all, the difference
// between them: --field is a STRING channel and --fields-json is a typed one.
// Collapsing the two — parsing "5" out of a --field value — is the mistake this
// table exists to prevent, because provider.MatchFields would then be handed a
// number the operator only ever typed as text.
func TestParseMetadataFilter(t *testing.T) {
	cases := []struct {
		name string
		json string
		kvs  []string
		want map[string]any
	}{
		{
			name: "neither flag filters nothing",
			want: nil,
		},
		{
			name: "a whitespace-only --fields-json is treated as absent",
			json: "   ",
			want: nil,
		},
		{
			name: "--field is repeatable and always yields strings",
			kvs:  []string{"tier=premium", "current_brands=brand:Y", "seats=5"},
			want: map[string]any{"tier": "premium", "current_brands": "brand:Y", "seats": "5"},
		},
		{
			name: "--field keeps an empty value as the empty string",
			kvs:  []string{"tier="},
			want: map[string]any{"tier": ""},
		},
		{
			name: "--field splits on the FIRST '=' only",
			kvs:  []string{"expr=a=b"},
			want: map[string]any{"expr": "a=b"},
		},
		{
			name: "--fields-json carries every JSON type through unchanged",
			json: `{"seats":5,"active":true,"tags":["public","beta"],"owner":null,"tier":"premium"}`,
			want: map[string]any{
				"seats":  float64(5),
				"active": true,
				"tags":   []any{"public", "beta"},
				"owner":  nil,
				"tier":   "premium",
			},
		},
		{
			name: "both merge, and --field wins the collision",
			json: `{"seats":5,"tier":"basic"}`,
			kvs:  []string{"tier=premium"},
			want: map[string]any{"seats": float64(5), "tier": "premium"},
		},
		{
			// The whole point of offering both: the same key spelled through the
			// two flags is two DIFFERENT predicates, and --field's string wins.
			name: "--field overrides a typed value with a string",
			json: `{"seats":5}`,
			kvs:  []string{"seats=5"},
			want: map[string]any{"seats": "5"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseMetadataFilter(tc.json, tc.kvs)
			if err != nil {
				t.Fatalf("parseMetadataFilter: %v", err)
			}
			if !maps.EqualFunc(got, tc.want, sameFilterValue) {
				t.Fatalf("filter = %#v, want %#v", got, tc.want)
			}
			if tc.want == nil && got != nil {
				t.Fatalf("no flags must yield a nil map (no predicate), got %#v", got)
			}
		})
	}
}

// TestParseMetadataFilterRejections asserts every malformed input is an
// APERTURE_INVALID_INPUT naming the offending text. A silently dropped predicate
// would WIDEN the enumeration, so "skip it" is never an acceptable outcome here.
func TestParseMetadataFilterRejections(t *testing.T) {
	cases := []struct {
		name     string
		json     string
		kvs      []string
		wantText []string
	}{
		{
			name:     "--field without an '='",
			kvs:      []string{"tier=premium", "just-a-key"},
			wantText: []string{"--field", `"just-a-key"`, "key=value"},
		},
		{
			name:     "--field with an empty key",
			kvs:      []string{"=premium"},
			wantText: []string{"--field", `"=premium"`, "empty field name"},
		},
		{
			name:     "--fields-json that is not JSON at all",
			json:     `{tier: premium}`,
			wantText: []string{"--fields-json", `"{tier: premium}"`, "not valid JSON"},
		},
		{
			name:     "--fields-json that is a JSON array",
			json:     `["tier"]`,
			wantText: []string{"--fields-json", `"[\"tier\"]"`, "must be a JSON object"},
		},
		{
			name:     "--fields-json that is a bare scalar",
			json:     `42`,
			wantText: []string{"--fields-json", `"42"`, "must be a JSON object"},
		},
		{
			// The wire surface rejects a non-finite number as
			// APERTURE_INVALID_INPUT (FieldsFromWire); JSON cannot even spell one,
			// so the decoder rejects it here under the SAME code rather than a
			// second spelling of the same failure.
			name:     "--fields-json with a number no float64 can hold",
			json:     `{"seats":1e999}`,
			wantText: []string{"--fields-json", "1e999"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseMetadataFilter(tc.json, tc.kvs)
			if err == nil {
				t.Fatalf("want a rejection, got filter %#v", got)
			}
			if code := aerr.CodeOf(err); code != aerr.APERTURE_INVALID_INPUT {
				t.Fatalf("code = %s, want %s (err: %v)", code, aerr.APERTURE_INVALID_INPUT, err)
			}
			for _, want := range tc.wantText {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q does not name %q", err.Error(), want)
				}
			}
		})
	}
}

// TestQuoteFlagValueTruncates keeps a pasted 100KiB --fields-json body from
// turning one validation failure into a screenful of terminal output.
func TestQuoteFlagValueTruncates(t *testing.T) {
	long := strings.Repeat("x", 500)
	got := quoteFlagValue(long)
	if !strings.Contains(got, "(truncated)") {
		t.Fatalf("a long value must be truncated, got %d chars: %s", len(got), got)
	}
	if len(got) > 220 {
		t.Fatalf("truncated value is still %d chars", len(got))
	}
	if short := quoteFlagValue("tier=premium"); short != `"tier=premium"` {
		t.Fatalf("a short value must be quoted verbatim, got %s", short)
	}
}

// sameFilterValue compares two filter values structurally, which maps.EqualFunc
// needs because a JSON list decodes to a []any and []any is not comparable.
func sameFilterValue(a, b any) bool {
	as, aok := a.([]any)
	bs, bok := b.([]any)
	if aok != bok {
		return false
	}
	if aok {
		if len(as) != len(bs) {
			return false
		}
		for i := range as {
			if !sameFilterValue(as[i], bs[i]) {
				return false
			}
		}
		return true
	}
	return a == b
}
