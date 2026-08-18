package rpc

import (
	"math"
	"reflect"
	"testing"

	aerr "github.com/frankbardon/aperture/errors"
	"google.golang.org/protobuf/types/known/structpb"
)

func mustStruct(t *testing.T, m map[string]any) *structpb.Value {
	t.Helper()
	v, err := structpb.NewValue(m)
	if err != nil {
		t.Fatalf("structpb.NewValue(%v): %v", m, err)
	}
	return v
}

func mustList(t *testing.T, items ...any) *structpb.Value {
	t.Helper()
	v, err := structpb.NewValue(items)
	if err != nil {
		t.Fatalf("structpb.NewValue(%v): %v", items, err)
	}
	return v
}

// Every Value kind lands on exactly one Go shape the metadata value model
// admits, and nothing is coerced between them.
func TestFieldsFromWire_KindMapping(t *testing.T) {
	cases := []struct {
		name string
		in   *structpb.Value
		want any
	}{
		{"null", structpb.NewNullValue(), nil},
		{"nil value", nil, nil},
		{"number", structpb.NewNumberValue(5), float64(5)},
		{"negative number", structpb.NewNumberValue(-2.5), float64(-2.5)},
		{"string", structpb.NewStringValue("5"), "5"},
		{"empty string", structpb.NewStringValue(""), ""},
		{"bool", structpb.NewBoolValue(true), true},
		{"list", mustList(t, "brand:X", "brand:Y"), []any{"brand:X", "brand:Y"}},
		{"empty list", mustList(t), []any{}},
		{"mixed list", mustList(t, "a", 1.0, true, nil), []any{"a", float64(1), true, nil}},
		{"struct", mustStruct(t, map[string]any{"dept": "eng"}), map[string]any{"dept": "eng"}},
		{"nested", mustStruct(t, map[string]any{"a": []any{1.0}}), map[string]any{"a": []any{float64(1)}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := FieldsFromWire(map[string]*structpb.Value{"f": tc.in})
			if err != nil {
				t.Fatalf("FieldsFromWire: unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got["f"], tc.want) {
				t.Fatalf("FieldsFromWire = %#v (%T), want %#v (%T)", got["f"], got["f"], tc.want, tc.want)
			}
		})
	}
}

// A number arrives as float64 and a string arrives as a string — the two are
// never reconciled. This is the property the "5" != 5 rule rests on.
func TestFieldsFromWire_NeverCoercesBetweenStringAndNumber(t *testing.T) {
	got, err := FieldsFromWire(map[string]*structpb.Value{
		"n": structpb.NewNumberValue(5),
		"s": structpb.NewStringValue("5"),
	})
	if err != nil {
		t.Fatalf("FieldsFromWire: %v", err)
	}
	if _, ok := got["n"].(float64); !ok {
		t.Fatalf("number decoded as %T, want float64", got["n"])
	}
	if _, ok := got["s"].(string); !ok {
		t.Fatalf("string decoded as %T, want string", got["s"])
	}
}

// An omitted map on the wire is a nil map in Go — the engine's "no predicate"
// case, which does not even consult a metadata source.
func TestFieldsFromWire_AbsentIsIndistinguishableFromNil(t *testing.T) {
	for _, in := range []map[string]*structpb.Value{nil, {}} {
		got, err := FieldsFromWire(in)
		if err != nil {
			t.Fatalf("FieldsFromWire(%v): %v", in, err)
		}
		if got != nil {
			t.Fatalf("FieldsFromWire(%v) = %#v, want a nil map", in, got)
		}
	}
}

// NaN and infinity have no metadata counterpart; structpb would render them as
// the strings "NaN"/"Infinity", silently turning a numeric want into a string
// want that matches nothing. They are rejected at the boundary instead.
func TestFieldsFromWire_RejectsNonFiniteNumbers(t *testing.T) {
	cases := map[string]*structpb.Value{
		"nan":         structpb.NewNumberValue(math.NaN()),
		"+inf":        structpb.NewNumberValue(math.Inf(+1)),
		"-inf":        structpb.NewNumberValue(math.Inf(-1)),
		"in a list":   mustList(t, math.NaN()),
		"in a struct": mustStruct(t, map[string]any{"n": math.Inf(-1)}),
	}
	for name, v := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := FieldsFromWire(map[string]*structpb.Value{"seats": v})
			if err == nil {
				t.Fatalf("FieldsFromWire = %#v, want an error", got)
			}
			if code := aerr.CodeOf(err); code != aerr.APERTURE_INVALID_INPUT {
				t.Fatalf("code = %q, want APERTURE_INVALID_INPUT", code)
			}
		})
	}
}

// A Value with no kind set at all cannot come from a generated client, but a
// hand-rolled one can send it. It is a caller error, not a panic.
func TestFieldsFromWire_RejectsKindlessValue(t *testing.T) {
	_, err := FieldsFromWire(map[string]*structpb.Value{"f": {}})
	if err == nil {
		t.Fatal("FieldsFromWire(kindless) = nil error, want APERTURE_INVALID_INPUT")
	}
	if code := aerr.CodeOf(err); code != aerr.APERTURE_INVALID_INPUT {
		t.Fatalf("code = %q, want APERTURE_INVALID_INPUT", code)
	}
}

func TestFieldsToWire_AbsentIsIndistinguishableFromNil(t *testing.T) {
	for _, in := range []map[string]any{nil, {}} {
		got, err := FieldsToWire(in)
		if err != nil {
			t.Fatalf("FieldsToWire(%v): %v", in, err)
		}
		if got != nil {
			t.Fatalf("FieldsToWire(%v) = %#v, want a nil map", in, got)
		}
	}
}

// The two directions compose: a Go predicate encodes and decodes back to a
// predicate that means the same thing. Numbers normalise to float64 (Value has
// one numeric kind, a double) — which is exactly why ValuesEqual compares
// numbers across Go types by value.
func TestFieldsRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]any
		want map[string]any
	}{
		{"strings", map[string]any{"tier": "premium"}, map[string]any{"tier": "premium"}},
		{"int64", map[string]any{"seats": int64(5)}, map[string]any{"seats": float64(5)}},
		{"int", map[string]any{"seats": 5}, map[string]any{"seats": float64(5)}},
		{"int8", map[string]any{"seats": int8(5)}, map[string]any{"seats": float64(5)}},
		{"uint16", map[string]any{"seats": uint16(5)}, map[string]any{"seats": float64(5)}},
		{"float32", map[string]any{"ratio": float32(0.5)}, map[string]any{"ratio": float64(0.5)}},
		{"float64", map[string]any{"ratio": 0.5}, map[string]any{"ratio": 0.5}},
		{"bool", map[string]any{"active": true}, map[string]any{"active": true}},
		{"nil", map[string]any{"owner": nil}, map[string]any{"owner": nil}},
		{
			"list", map[string]any{"brands": []any{"brand:X", int64(3)}},
			map[string]any{"brands": []any{"brand:X", float64(3)}},
		},
		{
			"object", map[string]any{"labels": map[string]any{"dept": "eng"}},
			map[string]any{"labels": map[string]any{"dept": "eng"}},
		},
		{
			"several keys",
			map[string]any{"tier": "premium", "seats": int64(5), "brands": []any{"brand:Y"}},
			map[string]any{"tier": "premium", "seats": float64(5), "brands": []any{"brand:Y"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wire, err := FieldsToWire(tc.in)
			if err != nil {
				t.Fatalf("FieldsToWire: %v", err)
			}
			got, err := FieldsFromWire(wire)
			if err != nil {
				t.Fatalf("FieldsFromWire: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("round trip = %#v, want %#v", got, tc.want)
			}
		})
	}
}

// The documented caveat, pinned so it cannot regress into a silent surprise:
// Value carries a number as a double, so an integer beyond 2^53 does not
// survive the trip. A key that large must be sent as a string.
func TestFieldsRoundTrip_LargeIntegerLosesPrecision(t *testing.T) {
	const big = int64(1)<<53 + 1

	wire, err := FieldsToWire(map[string]any{"id": big})
	if err != nil {
		t.Fatalf("FieldsToWire: %v", err)
	}
	got, err := FieldsFromWire(wire)
	if err != nil {
		t.Fatalf("FieldsFromWire: %v", err)
	}
	if f, ok := got["id"].(float64); !ok || int64(f) == big {
		t.Fatalf("id = %#v; the 2^53 caveat no longer holds — update the proto comment and the docs", got["id"])
	}

	// The documented workaround: send it as a string and it survives verbatim.
	wire, err = FieldsToWire(map[string]any{"id": "9007199254740993"})
	if err != nil {
		t.Fatalf("FieldsToWire(string): %v", err)
	}
	got, err = FieldsFromWire(wire)
	if err != nil {
		t.Fatalf("FieldsFromWire(string): %v", err)
	}
	if got["id"] != "9007199254740993" {
		t.Fatalf("id = %#v, want the string verbatim", got["id"])
	}
}

// A Go value outside the wire encoding is a coded caller error, never a panic.
func TestFieldsToWire_RejectsUnencodableValue(t *testing.T) {
	_, err := FieldsToWire(map[string]any{"when": struct{ A int }{1}})
	if err == nil {
		t.Fatal("FieldsToWire(struct) = nil error, want APERTURE_INVALID_INPUT")
	}
	if code := aerr.CodeOf(err); code != aerr.APERTURE_INVALID_INPUT {
		t.Fatalf("code = %q, want APERTURE_INVALID_INPUT", code)
	}
}
