package provider

import (
	"context"
	"reflect"
	"strings"
	"testing"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/identity"
)

// referenceRegistry builds a registry with a "brand" and a "dataset" type, the
// dataset objects carrying the scalar and array reference fields under test.
func referenceRegistry(t *testing.T) *Registry {
	t.Helper()
	brands, err := NewStatic([]Object{
		{ID: identity.MustParse("brand:1")},
		{ID: identity.MustParse("brand:2")},
	})
	if err != nil {
		t.Fatalf("NewStatic(brand): %v", err)
	}
	datasets, err := NewStatic([]Object{
		{ID: identity.MustParse("dataset:10"), Metadata: Metadata{
			"current_brands": []any{"brand:1", "brand:2"},
			"owner_brand":    "brand:1",
		}},
		{ID: identity.MustParse("dataset:11"), Metadata: Metadata{
			"current_brands": []any{},
		}},
		{ID: identity.MustParse("dataset:12"), Metadata: Metadata{
			"current_brands": "team:7",
		}},
		{ID: identity.MustParse("dataset:13"), Metadata: Metadata{
			"current_brands": []any{"brand:1", 42},
		}},
		{ID: identity.MustParse("dataset:14"), Metadata: Metadata{
			"current_brands": nil,
		}},
		{ID: identity.MustParse("dataset:15"), Metadata: Metadata{}},
	})
	if err != nil {
		t.Fatalf("NewStatic(dataset): %v", err)
	}
	reg := NewRegistry()
	reg.MustRegister("brand", brands, WithTTL(0))
	reg.MustRegister("dataset", datasets, WithTTL(0))
	return reg
}

func TestDeclareReferenceIsQueryableFromTheRegistry(t *testing.T) {
	reg := referenceRegistry(t)
	reg.MustDeclareReference("dataset", "current_brands", "brand")
	if err := reg.DeclareReference("dataset", "owner_brand", "brand"); err != nil {
		t.Fatalf("DeclareReference: %v", err)
	}

	// The lookup the engine resolves an enumeration through.
	target, ok := reg.ReferenceTarget("dataset", "current_brands")
	if !ok || target != "brand" {
		t.Fatalf("ReferenceTarget(dataset.current_brands) = %q,%v; want brand,true", target, ok)
	}
	if _, ok := reg.ReferenceTarget("dataset", "nope"); ok {
		t.Error("ReferenceTarget reported an undeclared field as a reference")
	}
	if _, ok := reg.ReferenceTarget("brand", "current_brands"); ok {
		t.Error("a reference leaked onto the target type; declarations are holding-side only")
	}
	if _, ok := reg.ReferenceTarget("unregistered", "current_brands"); ok {
		t.Error("ReferenceTarget answered for an unregistered type")
	}

	want := []Reference{
		{ObjectType: "dataset", Field: "current_brands", Target: "brand"},
		{ObjectType: "dataset", Field: "owner_brand", Target: "brand"},
	}
	if got := reg.References("dataset"); !reflect.DeepEqual(got, want) {
		t.Errorf("References(dataset) = %v; want %v", got, want)
	}
	if got := reg.References("brand"); got != nil {
		t.Errorf("References(brand) = %v; want nil", got)
	}
	if got := reg.AllReferences(); !reflect.DeepEqual(got, want) {
		t.Errorf("AllReferences() = %v; want %v", got, want)
	}
	if got := want[0].String(); got != "dataset.current_brands -> brand" {
		t.Errorf("Reference.String() = %q", got)
	}
}

func TestDeclareReferenceRejectsUnusableDeclarations(t *testing.T) {
	cases := []struct {
		name                      string
		objectType, field, target string
		want                      aerr.Code
	}{
		{"unknown target", "dataset", "current_brands", "widget", aerr.APERTURE_PROVIDER_REFERENCE_INVALID},
		{"unregistered holder", "widget", "current_brands", "brand", aerr.APERTURE_PROVIDER_UNREGISTERED},
		{"empty object type", "", "current_brands", "brand", aerr.APERTURE_PROVIDER_REFERENCE_INVALID},
		{"empty field", "dataset", "  ", "brand", aerr.APERTURE_PROVIDER_REFERENCE_INVALID},
		{"empty target", "dataset", "current_brands", "", aerr.APERTURE_PROVIDER_REFERENCE_INVALID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg := referenceRegistry(t)
			err := reg.DeclareReference(tc.objectType, tc.field, tc.target)
			if err == nil {
				t.Fatal("want an error, got nil")
			}
			if got := aerr.CodeOf(err); got != tc.want {
				t.Fatalf("code = %s; want %s (%v)", got, tc.want, err)
			}
		})
	}
}

func TestDeclareReferenceNamesTheFieldAndTheTarget(t *testing.T) {
	reg := referenceRegistry(t)
	err := reg.DeclareReference("dataset", "current_brands", "widget")
	if err == nil {
		t.Fatal("want an error for an unknown target")
	}
	msg := err.Error()
	for _, want := range []string{"current_brands", "widget"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message %q does not name %q", msg, want)
		}
	}
}

func TestDeclareReferenceRejectsADuplicateField(t *testing.T) {
	reg := referenceRegistry(t)
	reg.MustDeclareReference("dataset", "current_brands", "brand")
	err := reg.DeclareReference("dataset", "current_brands", "dataset")
	if got := aerr.CodeOf(err); got != aerr.APERTURE_PROVIDER_REFERENCE_INVALID {
		t.Fatalf("code = %s; want APERTURE_PROVIDER_REFERENCE_INVALID (%v)", got, err)
	}
	if target, _ := reg.ReferenceTarget("dataset", "current_brands"); target != "brand" {
		t.Errorf("a rejected duplicate overwrote the declaration: target = %q", target)
	}
}

func TestDeclareReferenceAllowsSelfAndSharedTargets(t *testing.T) {
	reg := referenceRegistry(t)
	if err := reg.DeclareReference("dataset", "parent", "dataset"); err != nil {
		t.Fatalf("a self-reference must be legal: %v", err)
	}
	if err := reg.DeclareReference("dataset", "current_brands", "brand"); err != nil {
		t.Fatalf("DeclareReference: %v", err)
	}
	if err := reg.DeclareReference("dataset", "archived_brands", "brand"); err != nil {
		t.Fatalf("two fields may share one target: %v", err)
	}
}

// A declaration on a field no object carries resolves to nothing, and is never
// an error: metadata fields are discovered at fetch, not declared.
func TestDeclareReferenceOnAnUnknownFieldIsLegal(t *testing.T) {
	reg := referenceRegistry(t)
	if err := reg.DeclareReference("dataset", "no_such_field", "brand"); err != nil {
		t.Fatalf("DeclareReference: %v", err)
	}
	got, err := reg.ResolveReference(context.Background(), identity.MustParse("dataset:10"), "no_such_field")
	if err != nil {
		t.Fatalf("ResolveReference: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %v; want no identities", got)
	}
}

func TestResolveReferenceScalarAndArray(t *testing.T) {
	reg := referenceRegistry(t)
	reg.MustDeclareReference("dataset", "current_brands", "brand")
	reg.MustDeclareReference("dataset", "owner_brand", "brand")

	cases := []struct {
		name  string
		id    string
		field string
		want  []string
	}{
		{"array valued", "dataset:10", "current_brands", []string{"brand:1", "brand:2"}},
		{"scalar valued", "dataset:10", "owner_brand", []string{"brand:1"}},
		{"empty list", "dataset:11", "current_brands", nil},
		{"null value", "dataset:14", "current_brands", nil},
		{"absent field", "dataset:15", "current_brands", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := reg.ResolveReference(context.Background(), identity.MustParse(tc.id), tc.field)
			if err != nil {
				t.Fatalf("ResolveReference: %v", err)
			}
			var strs []string
			for _, id := range got {
				strs = append(strs, id.String())
			}
			if !reflect.DeepEqual(strs, tc.want) {
				t.Errorf("got %v; want %v", strs, tc.want)
			}
		})
	}
}

func TestResolveReferenceRejectsAValueOfTheWrongType(t *testing.T) {
	reg := referenceRegistry(t)
	reg.MustDeclareReference("dataset", "current_brands", "brand")

	cases := []struct {
		name string
		id   string
	}{
		{"terminal segment type is not the target", "dataset:12"},
		{"list element is not an identity string", "dataset:13"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := reg.ResolveReference(context.Background(), identity.MustParse(tc.id), "current_brands")
			if got := aerr.CodeOf(err); got != aerr.APERTURE_PROVIDER_REFERENCE_MISMATCH {
				t.Fatalf("code = %s; want APERTURE_PROVIDER_REFERENCE_MISMATCH (%v)", got, err)
			}
		})
	}
}

func TestResolveReferenceValueShapes(t *testing.T) {
	reg := referenceRegistry(t)
	reg.MustDeclareReference("dataset", "current_brands", "brand")

	ok := []struct {
		name  string
		value any
		want  []string
	}{
		{"nil", nil, nil},
		{"scalar", "brand:1", []string{"brand:1"}},
		{"path identity", "account:acme/brand:1", []string{"account:acme/brand:1"}},
		{"list", []any{"brand:2", "brand:1"}, []string{"brand:2", "brand:1"}},
		{"duplicates kept in order", []any{"brand:1", "brand:1"}, []string{"brand:1", "brand:1"}},
	}
	for _, tc := range ok {
		t.Run(tc.name, func(t *testing.T) {
			got, err := reg.ResolveReferenceValue("dataset", "current_brands", tc.value)
			if err != nil {
				t.Fatalf("ResolveReferenceValue: %v", err)
			}
			var strs []string
			for _, id := range got {
				strs = append(strs, id.String())
			}
			if !reflect.DeepEqual(strs, tc.want) {
				t.Errorf("got %v; want %v", strs, tc.want)
			}
		})
	}

	bad := []struct {
		name  string
		value any
	}{
		{"number", 42},
		{"bool", true},
		{"map", map[string]any{"id": "brand:1"}},
		{"unparseable identity", "not an identity"},
		{"empty string", ""},
		{"wrong type", "team:7"},
		{"list with a nil element", []any{nil}},
		{"list with a wrong type", []any{"team:7"}},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			_, err := reg.ResolveReferenceValue("dataset", "current_brands", tc.value)
			if got := aerr.CodeOf(err); got != aerr.APERTURE_PROVIDER_REFERENCE_MISMATCH {
				t.Fatalf("code = %s; want APERTURE_PROVIDER_REFERENCE_MISMATCH (%v)", got, err)
			}
		})
	}
}

func TestResolveAnUndeclaredReferenceIsAnError(t *testing.T) {
	reg := referenceRegistry(t)
	_, err := reg.ResolveReference(context.Background(), identity.MustParse("dataset:10"), "current_brands")
	if got := aerr.CodeOf(err); got != aerr.APERTURE_PROVIDER_REFERENCE_INVALID {
		t.Fatalf("ResolveReference code = %s; want APERTURE_PROVIDER_REFERENCE_INVALID (%v)", got, err)
	}
	_, err = reg.ResolveReferenceValue("dataset", "current_brands", "brand:1")
	if got := aerr.CodeOf(err); got != aerr.APERTURE_PROVIDER_REFERENCE_INVALID {
		t.Fatalf("ResolveReferenceValue code = %s; want APERTURE_PROVIDER_REFERENCE_INVALID (%v)", got, err)
	}
}

func TestResolveReferenceSurfacesAFetchFailure(t *testing.T) {
	reg := referenceRegistry(t)
	reg.MustDeclareReference("dataset", "current_brands", "brand")
	_, err := reg.ResolveReference(context.Background(), identity.MustParse("dataset:404"), "current_brands")
	if got := aerr.CodeOf(err); got != aerr.APERTURE_NOT_FOUND {
		t.Fatalf("code = %s; want APERTURE_NOT_FOUND (%v)", got, err)
	}
}
