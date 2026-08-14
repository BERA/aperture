package provider

import (
	"encoding/json"
	"strings"
	"testing"

	aerr "github.com/frankbardon/aperture/errors"
)

// TestValueDepthTable pins the depth counting to the interview table exactly:
// depth is the number of containers below the field root.
func TestValueDepthTable(t *testing.T) {
	tests := []struct {
		name  string
		value any
		want  int
	}{
		{"scalar string", "eng", 0},
		{"scalar int", int64(7), 0},
		{"scalar nil", nil, 0},
		{`tags: ["a","b"]`, []any{"a", "b"}, 1},
		{`owner: {dept:"eng"}`, map[string]any{"dept": "eng"}, 1},
		{`owner: {lead:{name:"x"}}`, map[string]any{"lead": map[string]any{"name": "x"}}, 2},
		{`owner: {tags:["a"]}`, map[string]any{"tags": []any{"a"}}, 2},
		{`owner: {members:[{id:1}]}`, map[string]any{"members": []any{map[string]any{"id": 1}}}, 3},
		{`tags: [{name:"a"}]`, []any{map[string]any{"name": "a"}}, 2},
		{"empty array", []any{}, 1},
		{"empty object", map[string]any{}, 1},
		{"deepest branch wins", map[string]any{"a": "x", "b": map[string]any{"c": "y"}}, 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ValueDepth(tc.value); got != tc.want {
				t.Fatalf("ValueDepth = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestValidateFieldTable walks the same table through the validator, asserting
// which rows are legal under the default caps and which are rejected.
func TestValidateFieldTable(t *testing.T) {
	tests := []struct {
		name        string
		field       string
		value       any
		wantErr     bool
		wantPath    string // substring expected in the error's path context
		wantMsgPart string
	}{
		// Legal — scalars.
		{name: "scalar string", field: "dept", value: "eng"},
		{name: "scalar bool", field: "active", value: true},
		{name: "scalar int64", field: "seats", value: int64(40)},
		{name: "scalar int", field: "seats", value: 40},
		{name: "scalar float64", field: "budget", value: 15000.50},
		{name: "scalar json.Number", field: "budget", value: json.Number("15000.50")},
		{name: "scalar nil", field: "note", value: nil},

		// Legal — depth 1.
		{name: "array of scalars", field: "tags", value: []any{"a", "b"}},
		{name: "array of mixed scalars", field: "mixed", value: []any{"a", int64(2), true, nil}},
		{name: "empty array", field: "tags", value: []any{}},
		{name: "flat object", field: "owner", value: map[string]any{"dept": "eng"}},
		{name: "empty object", field: "owner", value: map[string]any{}},

		// Legal — depth 2.
		{name: "nested object", field: "owner", value: map[string]any{"lead": map[string]any{"name": "x"}}},
		{name: "array inside object", field: "owner", value: map[string]any{"tags": []any{"a"}}},

		// Rejected — arrays of objects, at any position.
		{
			name: "array of objects at root", field: "tags",
			value: []any{map[string]any{"name": "a"}},
			// depth 2 is within the cap; it is the array-of-objects rule that rejects it.
			wantErr: true, wantPath: "tags[0]", wantMsgPart: "arrays of objects",
		},
		{
			name: "array of objects nested", field: "owner",
			value:   map[string]any{"members": []any{map[string]any{"id": int64(1)}}},
			wantErr: true, wantPath: "owner.members[0]", wantMsgPart: "arrays of objects",
		},
		{
			name: "array of objects at second element", field: "tags",
			value:   []any{"a", map[string]any{"name": "b"}},
			wantErr: true, wantPath: "tags[1]", wantMsgPart: "arrays of objects",
		},
		{
			name: "nested array inside array", field: "tags",
			value:   []any{[]any{"a"}},
			wantErr: true, wantPath: "tags[0]", wantMsgPart: "nested arrays",
		},

		// Rejected — depth.
		{
			name: "three object levels", field: "owner",
			value:   map[string]any{"a": map[string]any{"b": map[string]any{"c": "x"}}},
			wantErr: true, wantPath: "owner.a.b", wantMsgPart: "depth cap",
		},
		{
			name: "array under two object levels", field: "owner",
			value:   map[string]any{"a": map[string]any{"b": []any{"x"}}},
			wantErr: true, wantPath: "owner.a.b", wantMsgPart: "depth cap",
		},

		// Rejected — unsupported types.
		{
			name: "typed slice", field: "tags", value: []string{"a"},
			wantErr: true, wantPath: "tags", wantMsgPart: "unsupported metadata value type",
		},
		{
			name: "typed map", field: "owner", value: map[string]string{"dept": "eng"},
			wantErr: true, wantPath: "owner", wantMsgPart: "unsupported metadata value type",
		},
		{
			name: "struct value", field: "owner", value: struct{ A int }{1},
			wantErr: true, wantPath: "owner", wantMsgPart: "unsupported metadata value type",
		},
		{
			name: "typed slice inside array", field: "tags", value: []any{[]string{"a"}},
			wantErr: true, wantPath: "tags[0]", wantMsgPart: "unsupported metadata value type",
		},

		// Rejected — keys.
		{
			name: "empty field name", field: "", value: "x",
			wantErr: true, wantMsgPart: "field name is empty",
		},
		{
			name: "empty object key", field: "owner", value: map[string]any{"": "x"},
			wantErr: true, wantPath: "owner", wantMsgPart: "empty key",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateField(tc.field, tc.value)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("ValidateField(%q) = %v, want nil", tc.field, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidateField(%q) = nil, want an error", tc.field)
			}
			if code := aerr.CodeOf(err); code != aerr.APERTURE_METADATA_INVALID {
				t.Fatalf("code = %q, want APERTURE_METADATA_INVALID", code)
			}
			if tc.wantMsgPart != "" && !strings.Contains(err.Error(), tc.wantMsgPart) {
				t.Errorf("error %q does not mention %q", err.Error(), tc.wantMsgPart)
			}
			if tc.wantPath != "" {
				if got := contextString(t, err, "path"); got != tc.wantPath {
					t.Errorf("context path = %q, want %q", got, tc.wantPath)
				}
			}
		})
	}
}

// TestValidateMetadataMap covers the whole-map entry point.
func TestValidateMetadataMap(t *testing.T) {
	t.Run("nil map", func(t *testing.T) {
		if err := ValidateMetadata(nil); err != nil {
			t.Fatalf("ValidateMetadata(nil) = %v, want nil", err)
		}
	})
	t.Run("all legal", func(t *testing.T) {
		md := Metadata{
			"dept":  "eng",
			"tags":  []any{"a", "b"},
			"owner": map[string]any{"lead": map[string]any{"name": "x"}},
			"seats": int64(3),
		}
		if err := ValidateMetadata(md); err != nil {
			t.Fatalf("ValidateMetadata = %v, want nil", err)
		}
	})
	t.Run("reports the offending field", func(t *testing.T) {
		md := Metadata{"ok": "x", "bad": []any{map[string]any{"a": 1}}}
		err := ValidateMetadata(md)
		if err == nil {
			t.Fatal("want an error")
		}
		if got := contextString(t, err, "field"); got != "bad" {
			t.Fatalf("context field = %q, want %q", got, "bad")
		}
	})
	t.Run("deterministic across several offenders", func(t *testing.T) {
		md := Metadata{
			"zeta":  []any{map[string]any{"a": 1}},
			"alpha": []any{map[string]any{"a": 1}},
		}
		// Fields are checked in sorted order, so "alpha" is always the report.
		for i := 0; i < 50; i++ {
			err := ValidateMetadata(md)
			if err == nil {
				t.Fatal("want an error")
			}
			if got := contextString(t, err, "field"); got != "alpha" {
				t.Fatalf("iteration %d: context field = %q, want %q", i, got, "alpha")
			}
		}
	})
	t.Run("deterministic across several offending keys", func(t *testing.T) {
		md := Metadata{"owner": map[string]any{
			"zeta":  map[string]any{"deep": map[string]any{"x": 1}},
			"alpha": map[string]any{"deep": map[string]any{"x": 1}},
		}}
		for i := 0; i < 50; i++ {
			err := ValidateMetadata(md)
			if err == nil {
				t.Fatal("want an error")
			}
			if got := contextString(t, err, "path"); got != "owner.alpha.deep" {
				t.Fatalf("iteration %d: context path = %q", i, got)
			}
		}
	})
}

// TestValueBytes pins the structural size measure.
func TestValueBytes(t *testing.T) {
	tests := []struct {
		name  string
		value any
		want  int
	}{
		{"nil", nil, 0},
		{"bool", true, 1},
		{"int64", int64(1), 8},
		{"float64", 1.5, 8},
		{"string", "abcde", 5},
		{"multibyte string counts bytes", "é", 2},
		{"json.Number", json.Number("1500.25"), 7},
		{"array", []any{"ab", "cde"}, 5},
		{"object counts keys", map[string]any{"ab": "cde"}, 5},
		{"nested", map[string]any{"a": []any{"bb", "cc"}}, 5},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ValueBytes(tc.value); got != tc.want {
				t.Fatalf("ValueBytes = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestSizeCapBoundary walks the exact boundary of the default size cap.
func TestSizeCapBoundary(t *testing.T) {
	atCap := strings.Repeat("x", DefaultMaxValueBytes)
	overCap := strings.Repeat("x", DefaultMaxValueBytes+1)

	if err := ValidateField("blob", atCap); err != nil {
		t.Fatalf("value of exactly the cap rejected: %v", err)
	}
	err := ValidateField("blob", overCap)
	if err == nil {
		t.Fatal("value one byte over the cap accepted")
	}
	if code := aerr.CodeOf(err); code != aerr.APERTURE_METADATA_INVALID {
		t.Fatalf("code = %q, want APERTURE_METADATA_INVALID", code)
	}
	if !strings.Contains(err.Error(), "size cap") {
		t.Errorf("error %q does not mention the size cap", err.Error())
	}
	if got := contextInt(t, err, "max_bytes"); got != DefaultMaxValueBytes {
		t.Errorf("context max_bytes = %d, want %d", got, DefaultMaxValueBytes)
	}
	if got := contextInt(t, err, "bytes"); got != DefaultMaxValueBytes+1 {
		t.Errorf("context bytes = %d, want %d", got, DefaultMaxValueBytes+1)
	}

	t.Run("size is summed across a container", func(t *testing.T) {
		half := strings.Repeat("x", DefaultMaxValueBytes/2)
		ok := []any{half, half}
		if err := ValidateField("blob", ok); err != nil {
			t.Fatalf("array summing to the cap rejected: %v", err)
		}
		over := []any{half, half, "x"}
		if err := ValidateField("blob", over); err == nil {
			t.Fatal("array summing past the cap accepted")
		}
	})
}

// TestValueLimitsConfigurable proves both caps are tunable rather than baked in.
func TestValueLimitsConfigurable(t *testing.T) {
	deep := map[string]any{"a": map[string]any{"b": map[string]any{"c": "x"}}}

	if err := ValidateField("owner", deep); err == nil {
		t.Fatal("depth 3 accepted under the default cap")
	}
	if err := (ValueLimits{MaxDepth: 3}).ValidateField("owner", deep); err != nil {
		t.Fatalf("depth 3 rejected under MaxDepth=3: %v", err)
	}

	blob := strings.Repeat("x", 100)
	if err := (ValueLimits{MaxBytes: 99}).ValidateField("blob", blob); err == nil {
		t.Fatal("100-byte value accepted under MaxBytes=99")
	}
	if err := (ValueLimits{MaxBytes: 100}).ValidateField("blob", blob); err != nil {
		t.Fatalf("100-byte value rejected under MaxBytes=100: %v", err)
	}

	t.Run("zero value means defaults", func(t *testing.T) {
		if got := (ValueLimits{}).resolve(); got != DefaultValueLimits() {
			t.Fatalf("zero ValueLimits resolved to %+v, want %+v", got, DefaultValueLimits())
		}
	})
	t.Run("negative means defaults", func(t *testing.T) {
		if got := (ValueLimits{MaxDepth: -1, MaxBytes: -1}).resolve(); got != DefaultValueLimits() {
			t.Fatalf("negative ValueLimits resolved to %+v, want %+v", got, DefaultValueLimits())
		}
	})
	t.Run("partial override keeps the other default", func(t *testing.T) {
		got := (ValueLimits{MaxDepth: 5}).resolve()
		if got.MaxDepth != 5 || got.MaxBytes != DefaultMaxValueBytes {
			t.Fatalf("resolved %+v, want {5 %d}", got, DefaultMaxValueBytes)
		}
	})
}

// TestErrorNeverCarriesTheValue is the cross-account leak guard: a validation
// error names the field, the path, and the Go type, and never the host data
// that failed.
func TestErrorNeverCarriesTheValue(t *testing.T) {
	const secret = "acme-confidential-payload"
	cases := []struct {
		name  string
		value any
	}{
		{"array of objects", []any{map[string]any{"name": secret}}},
		{"too deep", map[string]any{"a": map[string]any{"b": map[string]any{"c": secret}}}},
		{"unsupported type", []string{secret}},
		{"over the size cap", strings.Repeat(secret, 4000)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateField("field", tc.value)
			if err == nil {
				t.Fatal("want an error")
			}
			if strings.Contains(err.Error(), secret) {
				t.Errorf("error message leaks the value: %q", err.Error())
			}
			var ce *aerr.CodedError
			if !asCoded(err, &ce) {
				t.Fatal("error is not an *aerr.CodedError")
			}
			for k, v := range ce.Context {
				if s, ok := v.(string); ok && strings.Contains(s, secret) {
					t.Errorf("error context %q leaks the value: %q", k, s)
				}
			}
		})
	}
}

// TestMetadataBytes covers the per-object convenience measure.
func TestMetadataBytes(t *testing.T) {
	md := Metadata{"ab": "cde", "f": []any{"gh"}}
	if got, want := MetadataBytes(md), 2+3+1+2; got != want {
		t.Fatalf("MetadataBytes = %d, want %d", got, want)
	}
	if got := MetadataBytes(nil); got != 0 {
		t.Fatalf("MetadataBytes(nil) = %d, want 0", got)
	}
}

// --- helpers -------------------------------------------------------------

func asCoded(err error, target **aerr.CodedError) bool {
	ce, ok := err.(*aerr.CodedError)
	if !ok {
		return false
	}
	*target = ce
	return true
}

func contextString(t *testing.T, err error, key string) string {
	t.Helper()
	var ce *aerr.CodedError
	if !asCoded(err, &ce) {
		t.Fatalf("error %v is not an *aerr.CodedError", err)
	}
	v, ok := ce.Context[key]
	if !ok {
		t.Fatalf("error context has no %q (context: %v)", key, ce.Context)
	}
	s, ok := v.(string)
	if !ok {
		t.Fatalf("error context %q is %T, want string", key, v)
	}
	return s
}

func contextInt(t *testing.T, err error, key string) int {
	t.Helper()
	var ce *aerr.CodedError
	if !asCoded(err, &ce) {
		t.Fatalf("error %v is not an *aerr.CodedError", err)
	}
	v, ok := ce.Context[key]
	if !ok {
		t.Fatalf("error context has no %q (context: %v)", key, ce.Context)
	}
	n, ok := v.(int)
	if !ok {
		t.Fatalf("error context %q is %T, want int", key, v)
	}
	return n
}
