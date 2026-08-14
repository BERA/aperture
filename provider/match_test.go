package provider

import (
	"encoding/json"
	"math"
	"testing"
)

// TestMatchField walks the Fields contract stated on Filter, one row per rule.
func TestMatchField(t *testing.T) {
	cases := []struct {
		name  string
		value any
		want  any
		match bool
	}{
		// Collection field: membership.
		{"array contains the wanted string", []any{"premium", "launch"}, "premium", true},
		{"array does not contain the wanted string", []any{"premium", "launch"}, "trial", false},
		{"membership is not substring matching", []any{"premium-trial"}, "premium", false},
		{"empty array is a member of nothing", []any{}, "premium", false},
		{"nil array is a member of nothing", []any(nil), "premium", false},

		// Typed elements: expr's rule, not fmt.Sprint's.
		{"int64 element matches an int want", []any{int64(3), int64(5)}, 5, true},
		{"int64 element matches an int64 want", []any{int64(3), int64(5)}, int64(5), true},
		{"int64 element matches a float want of equal value", []any{int64(3), int64(5)}, 5.0, true},
		{"int64 element does not match its string spelling", []any{int64(3), int64(5)}, "5", false},
		{"string element does not match a numeric want", []any{"3", "5"}, 5, false},
		{"float element matches an equal float", []any{9.5}, 9.5, true},
		{"bool element matches a bool want", []any{true, false}, true, true},
		{"bool element does not match its string spelling", []any{true}, "true", false},
		{"nil element matches a nil want", []any{nil, "a"}, nil, true},

		// Container want: equality, at both ends.
		{"array want equals an identical array", []any{"a", "b"}, []any{"a", "b"}, true},
		{"array want is not subset matching", []any{"a", "b"}, []any{"a"}, false},
		{"array want does not match a scalar field", "a", []any{"a"}, false},

		// Scalar field: equality, unchanged.
		{"scalar string equality", "gold", "gold", true},
		{"scalar string inequality", "gold", "silver", false},
		{"scalar int64 matches an int want", int64(12), 12, true},
		{"scalar int64 matches a float want of equal value", int64(12), 12.0, true},
		{"scalar int64 does not match its string spelling", int64(12), "12", false},
		{"scalar bool equality", true, true, true},
		{"scalar nil equality", nil, nil, true},
		{"scalar nil does not match a string", nil, "", false},

		// Object field: equality, never key membership, never a rendering match.
		{"object field does not match one of its keys", map[string]any{"dept": "eng"}, "dept", false},
		{"object field does not match one of its values", map[string]any{"dept": "eng"}, "eng", false},
		{"object field does not match its rendering", map[string]any{"dept": "eng"}, "map[dept:eng]", false},
		{"object field equals an identical object", map[string]any{"dept": "eng"}, map[string]any{"dept": "eng"}, true},
		{"object field differs from a different object", map[string]any{"dept": "eng"}, map[string]any{"dept": "ops"}, false},
		{"nested object equality", map[string]any{"lead": map[string]any{"name": "x"}}, map[string]any{"lead": map[string]any{"name": "x"}}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := MatchField(tc.value, tc.want); got != tc.match {
				t.Errorf("MatchField(%#v, %#v) = %v, want %v", tc.value, tc.want, got, tc.match)
			}
		})
	}
}

func TestMatchFields(t *testing.T) {
	md := Metadata{
		"tier":  "gold",
		"seats": int64(12),
		"tags":  []any{"premium", "launch"},
		"ranks": []any{int64(3), int64(5)},
		"owner": map[string]any{"dept": "eng"},
	}

	cases := []struct {
		name   string
		fields map[string]any
		match  bool
	}{
		{"nil fields match everything", nil, true},
		{"empty fields match everything", map[string]any{}, true},
		{"single scalar predicate", map[string]any{"tier": "gold"}, true},
		{"single membership predicate", map[string]any{"tags": "premium"}, true},
		{"typed membership predicate", map[string]any{"ranks": 5}, true},
		{"typed membership rejects the string spelling", map[string]any{"ranks": "5"}, false},
		{"predicates are ANDed", map[string]any{"tier": "gold", "tags": "launch"}, true},
		{"one failing predicate fails the object", map[string]any{"tier": "gold", "tags": "trial"}, false},
		{"absent field never matches", map[string]any{"missing": "anything"}, false},
		{"absent field never matches a nil want", map[string]any{"missing": nil}, false},
		{"object field by equality", map[string]any{"owner": map[string]any{"dept": "eng"}}, true},
		{"object field is not key membership", map[string]any{"owner": "dept"}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := MatchFields(md, tc.fields); got != tc.match {
				t.Errorf("MatchFields(md, %#v) = %v, want %v", tc.fields, got, tc.match)
			}
		})
	}

	// An empty object still answers: nothing matches a predicate, everything
	// matches no predicate.
	if !MatchFields(Metadata{}, nil) {
		t.Error("MatchFields(empty, nil) = false, want true")
	}
	if MatchFields(Metadata{}, map[string]any{"tier": "gold"}) {
		t.Error("MatchFields(empty, {tier:gold}) = true, want false")
	}
	if MatchFields(nil, map[string]any{"tier": "gold"}) {
		t.Error("MatchFields(nil, {tier:gold}) = true, want false")
	}
}

func TestValuesEqual(t *testing.T) {
	cases := []struct {
		name string
		a, b any
		want bool
	}{
		{"int and int64", 5, int64(5), true},
		{"int8 and int32", int8(5), int32(5), true},
		{"uint and int", uint(5), 5, true},
		{"uint and a negative int", uint(5), -5, false},
		{"float32 and float64", float32(0.5), 0.5, true},
		{"int and an equal float", 5, 5.0, true},
		{"int and an unequal float", 5, 5.5, false},
		{"uint64 above MaxInt64 equals itself", uint64(math.MaxInt64) + 1, uint64(math.MaxInt64) + 1, true},
		{"uint64 above MaxInt64 is not an int64", uint64(math.MaxInt64) + 1, int64(math.MaxInt64), false},
		{"number and its string spelling", int64(5), "5", false},
		{"string and its numeric value", "5", int64(5), false},
		{"bool and its string spelling", true, "true", false},
		{"string equality", "a", "a", true},
		{"string inequality", "a", "b", false},
		{"bool equality", false, false, true},
		{"bool inequality", false, true, false},
		{"nil and nil", nil, nil, true},
		{"nil and a typed nil slice", nil, []any(nil), true},
		{"nil and an empty slice", nil, []any{}, false},
		{"nil and a zero string", nil, "", false},
		{"nil and a zero int", nil, 0, false},
		// json.Number is not folded into the numeric set: expr-lang has no case
		// for it either, so both fall to DeepEqual. A loader normalises it away.
		{"json.Number equals the same json.Number", json.Number("5"), json.Number("5"), true},
		{"json.Number is not an int64", json.Number("5"), int64(5), false},
		{"json.Number is not a string", json.Number("5"), "5", false},
		{"deep equality over arrays", []any{"a", int64(1)}, []any{"a", int64(1)}, true},
		{"deep inequality over arrays", []any{"a"}, []any{"a", "b"}, false},
		{"deep equality over objects", map[string]any{"a": "b"}, map[string]any{"a": "b"}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ValuesEqual(tc.a, tc.b); got != tc.want {
				t.Errorf("ValuesEqual(%#v, %#v) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
			// Equality is symmetric; a one-sided rule would make a predicate's
			// answer depend on which side the provider happened to pass.
			if got := ValuesEqual(tc.b, tc.a); got != tc.want {
				t.Errorf("ValuesEqual(%#v, %#v) = %v, want %v (asymmetric)", tc.b, tc.a, got, tc.want)
			}
		})
	}
}
