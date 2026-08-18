package seed

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/identity"
	"github.com/frankbardon/aperture/provider"
	"github.com/frankbardon/aperture/storage/memory"
)

// referenceFixture writes the two CSVs the reference tests read: brands, and
// datasets whose current_brands column holds FULL brand identities.
func referenceFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("brands.csv", "id,tier\nbrand:1,gold\nbrand:2,silver\n")
	write("datasets.csv",
		"id,current_brands:list,owner_brand\n"+
			"dataset:10,brand:1|brand:2,brand:1\n"+
			"dataset:11,,brand:2\n")
	return dir
}

func TestBuildRegistry_ReferencesAreQueryableAndResolve(t *testing.T) {
	dir := referenceFixture(t)
	doc, err := Parse([]byte(`
providers:
  - object_type: dataset
    kind: csv
    path: datasets.csv
    ttl: "0"
    references:
      current_brands: brand
      owner_brand: brand
  - {object_type: brand, kind: csv, path: brands.csv, ttl: "0"}
`), FormatYAML)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := doc.Providers[0].References["current_brands"]; got != "brand" {
		t.Fatalf("references decoded as %+v", doc.Providers[0].References)
	}

	// The brand provider is declared AFTER the entry referencing it: references
	// are applied in their own pass, so file order cannot break a document.
	reg, err := doc.BuildRegistry(dir)
	if err != nil {
		t.Fatalf("BuildRegistry: %v", err)
	}

	want := []provider.Reference{
		{ObjectType: "dataset", Field: "current_brands", Target: "brand"},
		{ObjectType: "dataset", Field: "owner_brand", Target: "brand"},
	}
	if got := reg.AllReferences(); !reflect.DeepEqual(got, want) {
		t.Fatalf("AllReferences() = %v; want %v", got, want)
	}
	if target, ok := reg.ReferenceTarget("dataset", "current_brands"); !ok || target != "brand" {
		t.Fatalf("ReferenceTarget = %q,%v; want brand,true", target, ok)
	}

	// Array-valued and scalar reference fields both resolve.
	ids, err := reg.ResolveReference(context.Background(), identity.MustParse("dataset:10"), "current_brands")
	if err != nil {
		t.Fatalf("ResolveReference: %v", err)
	}
	var got []string
	for _, id := range ids {
		got = append(got, id.String())
	}
	if !reflect.DeepEqual(got, []string{"brand:1", "brand:2"}) {
		t.Errorf("current_brands resolved to %v", got)
	}
	ids, err = reg.ResolveReference(context.Background(), identity.MustParse("dataset:11"), "owner_brand")
	if err != nil {
		t.Fatalf("ResolveReference: %v", err)
	}
	if len(ids) != 1 || ids[0].String() != "brand:2" {
		t.Errorf("owner_brand resolved to %v", ids)
	}
}

// A reference may point at a type the objects: section serves — the target must
// be REGISTERED, not file-backed.
func TestBuildRegistry_ReferenceToAnInlineObjectType(t *testing.T) {
	dir := referenceFixture(t)
	doc, err := Parse([]byte(`
providers:
  - object_type: dataset
    kind: csv
    path: datasets.csv
    references: {current_brands: brand}
objects:
  - {id: "brand:1", metadata: {tier: gold}}
  - {id: "brand:2", metadata: {tier: silver}}
`), FormatYAML)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	reg, err := doc.BuildRegistry(dir)
	if err != nil {
		t.Fatalf("BuildRegistry: %v", err)
	}
	if _, ok := reg.ReferenceTarget("dataset", "current_brands"); !ok {
		t.Fatal("reference to an inline object type was not declared")
	}
}

func TestBuildRegistry_ReferenceToAnUnknownTarget(t *testing.T) {
	dir := referenceFixture(t)
	doc, err := Parse([]byte(`
providers:
  - object_type: dataset
    kind: csv
    path: datasets.csv
    references: {current_brands: brand}
`), FormatYAML)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	_, err = doc.BuildRegistry(dir)
	if got := aerr.CodeOf(err); got != aerr.APERTURE_PROVIDER_REFERENCE_INVALID {
		t.Fatalf("code = %s; want APERTURE_PROVIDER_REFERENCE_INVALID (%v)", got, err)
	}
	for _, want := range []string{"current_brands", "brand"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

// Metadata fields are discovered at fetch, not declared, so a reference on a
// field the data does not carry builds fine and resolves to nothing.
func TestBuildRegistry_ReferenceOnAnUnknownFieldIsLegal(t *testing.T) {
	dir := referenceFixture(t)
	doc, err := Parse([]byte(`
providers:
  - object_type: dataset
    kind: csv
    path: datasets.csv
    references: {no_such_column: brand}
  - {object_type: brand, kind: csv, path: brands.csv}
`), FormatYAML)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	reg, err := doc.BuildRegistry(dir)
	if err != nil {
		t.Fatalf("BuildRegistry: %v", err)
	}
	ids, err := reg.ResolveReference(context.Background(), identity.MustParse("dataset:10"), "no_such_column")
	if err != nil {
		t.Fatalf("ResolveReference: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("got %v; want no identities", ids)
	}
}

func TestBuildRegistry_ReferenceWithAnEmptyTarget(t *testing.T) {
	dir := referenceFixture(t)
	doc := &Document{Providers: []Provider{
		{ObjectType: "dataset", Kind: "csv", Path: "datasets.csv", References: map[string]string{"current_brands": ""}},
	}}
	_, err := doc.BuildRegistry(dir)
	if got := aerr.CodeOf(err); got != aerr.APERTURE_PROVIDER_REFERENCE_INVALID {
		t.Fatalf("code = %s; want APERTURE_PROVIDER_REFERENCE_INVALID (%v)", got, err)
	}
}

// The Go registration path expresses exactly what the YAML path does: both
// reach Registry.DeclareReference, and the resulting registries agree.
func TestReferenceGoPathMatchesTheYAMLPath(t *testing.T) {
	dir := referenceFixture(t)
	doc, err := Parse([]byte(`
providers:
  - {object_type: brand, kind: csv, path: brands.csv}
  - object_type: dataset
    kind: csv
    path: datasets.csv
    references: {current_brands: brand}
`), FormatYAML)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	fromYAML, err := doc.BuildRegistry(dir)
	if err != nil {
		t.Fatalf("BuildRegistry: %v", err)
	}

	fromGo := provider.NewRegistry()
	brands, err := provider.NewStatic([]provider.Object{{ID: identity.MustParse("brand:1")}})
	if err != nil {
		t.Fatal(err)
	}
	datasets, err := provider.NewStatic([]provider.Object{
		{ID: identity.MustParse("dataset:10"), Metadata: provider.Metadata{"current_brands": []any{"brand:1"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	fromGo.MustRegister("brand", brands)
	fromGo.MustRegister("dataset", datasets)
	fromGo.MustDeclareReference("dataset", "current_brands", "brand")

	if !reflect.DeepEqual(fromYAML.AllReferences(), fromGo.AllReferences()) {
		t.Fatalf("YAML declared %v; Go declared %v", fromYAML.AllReferences(), fromGo.AllReferences())
	}
}

// TestReferenceWiringIsNotModelState extends the rule every wiring section
// shares (see TestSQLWiringIsNotModelState): a references: block is runtime
// wiring, so Apply writes none of it and an export reproduces none of it.
func TestReferenceWiringIsNotModelState(t *testing.T) {
	doc, err := Parse([]byte(`
accounts:
  - {id: acme, name: Acme}
providers:
  - object_type: dataset
    kind: csv
    path: datasets.csv
    references: {current_brands: brand}
`), FormatYAML)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	store := memory.New()
	if err := doc.Apply(context.Background(), store); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	out, err := Export(context.Background(), store)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if len(out.Providers) != 0 {
		t.Fatalf("export reproduced the providers: block: %+v", out.Providers)
	}
	for _, p := range out.Providers {
		if len(p.References) != 0 {
			t.Errorf("export reproduced a references: block: %+v", p.References)
		}
	}
}
