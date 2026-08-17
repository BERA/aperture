package seed

import (
	"context"
	goerrors "errors"
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

// datedObjectsYAML declares both field types and exercises every accepted
// spelling of a value: canonical, offset-free, and fractional-second.
const datedObjectsYAML = `
field_types:
  - object_type: brand
    fields:
      hired_at: date
      last_seen: datetime

objects:
  - id: account:acme/brand:1
    metadata:
      tier: gold
      hired_at: "2026-03-04"
      last_seen: "2026-03-04T12:30:00Z"
  - id: account:acme/brand:2
    metadata:
      hired_at: "2026-03-04"
      last_seen: "2026-03-04T01:02:03.456Z"
  - id: account:acme/brand:3
    metadata:
      tier: silver
`

// The declared types are honoured, and the CANONICAL text is what is stored: a
// fractional-second value truncates to seconds and an offset-free timestamp
// gains its Z, so two objects naming one instant hold one string.
func TestFieldTypes_LoadAndCanonicalize(t *testing.T) {
	doc, err := Parse([]byte(datedObjectsYAML), FormatYAML)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	reg, err := doc.BuildRegistry("")
	if err != nil {
		t.Fatalf("BuildRegistry: %v", err)
	}

	md, err := reg.Fetch(context.Background(), identity.MustParse("account:acme/brand:1"))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	want := map[string]any{
		"tier":      "gold",
		"hired_at":  "2026-03-04",
		"last_seen": "2026-03-04T12:30:00Z",
	}
	if !reflect.DeepEqual(map[string]any(md), want) {
		t.Errorf("metadata = %#v, want %#v", md, want)
	}

	// Fractional seconds TRUNCATE (never round) into the canonical form.
	md2, err := reg.Fetch(context.Background(), identity.MustParse("account:acme/brand:2"))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if md2["last_seen"] != "2026-03-04T01:02:03Z" {
		t.Errorf("last_seen = %v, want the truncated canonical form 2026-03-04T01:02:03Z", md2["last_seen"])
	}
}

// A canonical value is what a Filter.Fields predicate compares against — which
// is the whole point of canonicalising at load. A non-canonical predicate, or
// one of the wrong granularity, matches nothing.
func TestFieldTypes_QueryableByCanonicalEquality(t *testing.T) {
	doc, err := Parse([]byte(datedObjectsYAML), FormatYAML)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	reg, err := doc.BuildRegistry("")
	if err != nil {
		t.Fatalf("BuildRegistry: %v", err)
	}
	md, err := reg.Fetch(context.Background(), identity.MustParse("account:acme/brand:2"))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	cases := map[string]struct {
		fields map[string]any
		want   bool
	}{
		"canonical date":            {map[string]any{"hired_at": "2026-03-04"}, true},
		"canonical datetime":        {map[string]any{"last_seen": "2026-03-04T01:02:03Z"}, true},
		"as written, non-canonical": {map[string]any{"last_seen": "2026-03-04T01:02:03.456Z"}, false},
		"wrong granularity":         {map[string]any{"hired_at": "2026-03-04T00:00:00Z"}, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := provider.MatchFields(md, tc.fields); got != tc.want {
				t.Errorf("MatchFields(%v) = %v, want %v", tc.fields, got, tc.want)
			}
		})
	}
}

// A declared field the object simply does not carry is NOT an error: the section
// declares a type, never a requirement. An explicitly EMPTY value omits the
// field rather than storing "", matching the CSV loader's empty-cell rule.
func TestFieldTypes_AbsentAndEmptyValues(t *testing.T) {
	doc, err := Parse([]byte(datedObjectsYAML+
		"  - id: account:acme/brand:4\n    metadata:\n      hired_at: \"\"\n      tier: bronze\n"), FormatYAML)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	reg, err := doc.BuildRegistry("")
	if err != nil {
		t.Fatalf("BuildRegistry: %v", err)
	}

	// brand:3 declares neither declared field — a legal object.
	md, err := reg.Fetch(context.Background(), identity.MustParse("account:acme/brand:3"))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !reflect.DeepEqual(map[string]any(md), map[string]any{"tier": "silver"}) {
		t.Errorf("metadata = %#v, want just tier", md)
	}

	// brand:4 spells hired_at as empty — the field is ABSENT, not "".
	md4, err := reg.Fetch(context.Background(), identity.MustParse("account:acme/brand:4"))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if v, ok := md4["hired_at"]; ok {
		t.Errorf("hired_at = %#v, want the field to be omitted entirely", v)
	}
	if md4["tier"] != "bronze" {
		t.Errorf("tier = %v, want bronze", md4["tier"])
	}
}

// Every rejection path: the code, and the CAUSE read back through
// provider.DateReasonOf rather than off the message text. A granularity mismatch
// is deliberately NOT a parse failure, so it carries no reason.
func TestFieldTypes_Rejections(t *testing.T) {
	cases := map[string]struct {
		yaml   string
		reason provider.DateReason
	}{
		"impossible calendar day": {
			"field_types:\n  - {object_type: brand, fields: {hired_at: date}}\nobjects:\n  - id: brand:1\n    metadata: {hired_at: \"2026-02-30\"}\n",
			provider.DateReasonCalendar,
		},
		"non-RFC3339 layout": {
			"field_types:\n  - {object_type: brand, fields: {hired_at: date}}\nobjects:\n  - id: brand:1\n    metadata: {hired_at: \"03/04/2026\"}\n",
			provider.DateReasonLayout,
		},
		"explicit positive offset": {
			"field_types:\n  - {object_type: brand, fields: {seen: datetime}}\nobjects:\n  - id: brand:1\n    metadata: {seen: \"2026-01-01T00:00:00+05:00\"}\n",
			provider.DateReasonNonUTCOffset,
		},
		"explicit negative offset": {
			"field_types:\n  - {object_type: brand, fields: {seen: datetime}}\nobjects:\n  - id: brand:1\n    metadata: {seen: \"2026-01-01T00:00:00-05:00\"}\n",
			provider.DateReasonNonUTCOffset,
		},
		"timestamp in a date field": {
			"field_types:\n  - {object_type: brand, fields: {hired_at: date}}\nobjects:\n  - id: brand:1\n    metadata: {hired_at: \"2026-03-04T12:30:00Z\"}\n",
			"", // a granularity mismatch is not a parse failure
		},
		"bare day in a datetime field": {
			"field_types:\n  - {object_type: brand, fields: {seen: datetime}}\nobjects:\n  - id: brand:1\n    metadata: {seen: \"2026-03-04\"}\n",
			"",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			doc, err := Parse([]byte(tc.yaml), FormatYAML)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			_, err = doc.BuildRegistry("")
			if aerr.CodeOf(err) != aerr.APERTURE_CONFIG_INVALID {
				t.Fatalf("code = %s, want APERTURE_CONFIG_INVALID (err=%v)", aerr.CodeOf(err), err)
			}
			if got := provider.DateReasonOf(err); got != tc.reason {
				t.Errorf("DateReasonOf = %q, want %q", got, tc.reason)
			}
		})
	}
}

// A rejection names the object id and the field so an author can go straight to
// the offending entry — and NEVER the value. A date is frequently personal data
// (a birth date, a termination date) and an error is a thing that gets logged.
func TestFieldTypes_ErrorNamesFieldButNeverTheValue(t *testing.T) {
	const secret = "1979-06-14T09:15:00Z" // a well-formed value of the WRONG granularity
	doc, err := Parse([]byte(
		"field_types:\n  - {object_type: brand, fields: {born_on: date}}\n"+
			"objects:\n  - id: account:acme/brand:1\n    metadata: {born_on: \""+secret+"\"}\n"), FormatYAML)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	_, err = doc.BuildRegistry("")
	if err == nil {
		t.Fatal("BuildRegistry succeeded, want a rejection")
	}
	msg := err.Error()
	for _, want := range []string{"account:acme/brand:1", "born_on"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not mention %q", msg, want)
		}
	}
	// The whole value, and every fragment of it that could identify a person.
	for _, leak := range []string{secret, "1979", "06-14", "09:15"} {
		if strings.Contains(msg, leak) {
			t.Errorf("error %q leaks the offending value fragment %q", msg, leak)
		}
	}
}

// YAML resolves an UNQUOTED calendar day as a !!timestamp, so it reaches the
// loader already widened to midnight and a "date" field rejects it. The widening
// happens in the YAML decoder, before this package sees the document, so the
// rejection is the only honest outcome — but it carries a hint naming the fix,
// because "wrong granularity" for something the author wrote as a plain day is
// otherwise baffling.
func TestFieldTypes_UnquotedYAMLDayIsRejectedWithAHint(t *testing.T) {
	doc, err := Parse([]byte(
		"field_types:\n  - {object_type: brand, fields: {hired_at: date}}\n"+
			"objects:\n  - id: brand:1\n    metadata: {hired_at: 2026-03-04}\n"), FormatYAML)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	_, err = doc.BuildRegistry("")
	if aerr.CodeOf(err) != aerr.APERTURE_CONFIG_INVALID {
		t.Fatalf("code = %s, want APERTURE_CONFIG_INVALID (err=%v)", aerr.CodeOf(err), err)
	}
	if !strings.Contains(err.Error(), "quote it") {
		t.Errorf("error %q does not hint at quoting the value", err)
	}
	// Quoting the same document loads it.
	quoted, err := Parse([]byte(
		"field_types:\n  - {object_type: brand, fields: {hired_at: date}}\n"+
			"objects:\n  - id: brand:1\n    metadata: {hired_at: \"2026-03-04\"}\n"), FormatYAML)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, err := quoted.BuildRegistry(""); err != nil {
		t.Fatalf("BuildRegistry with a quoted value: %v", err)
	}
}

// A YAML mapping can hold any shape, so a declared date field may be handed a
// number, an array, or an object. Each gets its own diagnostic naming the
// declared type and the shape found — not a confusing parse failure about a
// value that was never text.
func TestFieldTypes_NonStringValue(t *testing.T) {
	cases := map[string]struct {
		metadata string
		kind     string
	}{
		"number": {"{hired_at: 2026}", "int"},
		"float":  {"{hired_at: 20.26}", "float"},
		"bool":   {"{hired_at: true}", "bool"},
		"array":  {"{hired_at: [2026-03-04]}", "array"},
		"object": {"{hired_at: {year: 2026}}", "object"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			doc, err := Parse([]byte(
				"field_types:\n  - {object_type: brand, fields: {hired_at: date}}\n"+
					"objects:\n  - id: brand:1\n    metadata: "+tc.metadata+"\n"), FormatYAML)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			_, err = doc.BuildRegistry("")
			if aerr.CodeOf(err) != aerr.APERTURE_CONFIG_INVALID {
				t.Fatalf("code = %s, want APERTURE_CONFIG_INVALID (err=%v)", aerr.CodeOf(err), err)
			}
			var ce *aerr.CodedError
			if !goerrors.As(err, &ce) {
				t.Fatalf("error is not a *CodedError: %v", err)
			}
			if ce.Context["kind"] != tc.kind {
				t.Errorf("kind = %v, want %q", ce.Context["kind"], tc.kind)
			}
			if ce.Context["type"] != "date" {
				t.Errorf("type = %v, want the declared type \"date\"", ce.Context["type"])
			}
			// It is a shape complaint, not a parse failure.
			if r := provider.DateReasonOf(err); r != "" {
				t.Errorf("DateReasonOf = %q, want empty", r)
			}
		})
	}
}

// The declaration itself is validated, before anything is registered — so a
// typo'd declaration fails the build even in a document that declares no objects
// at all. An unknown spelling is REJECTED, never ignored: a declaration the
// loader quietly skipped would read exactly like one it honoured while
// validating nothing.
func TestFieldTypes_DeclarationErrors(t *testing.T) {
	cases := map[string]string{
		"unknown type":               "field_types:\n  - {object_type: brand, fields: {hired_at: timestamp}}\n",
		"capitalised type":           "field_types:\n  - {object_type: brand, fields: {hired_at: Date}}\n",
		"time is not a type":         "field_types:\n  - {object_type: brand, fields: {hired_at: time}}\n",
		"empty type":                 "field_types:\n  - {object_type: brand, fields: {hired_at: \"\"}}\n",
		"missing object_type":        "field_types:\n  - {fields: {hired_at: date}}\n",
		"duplicate object_type":      "field_types:\n  - {object_type: brand, fields: {a: date}}\n  - {object_type: brand, fields: {b: date}}\n",
		"empty field name":           "field_types:\n  - {object_type: brand, fields: {\"\": date}}\n",
		"unknown type, with objects": "field_types:\n  - {object_type: brand, fields: {hired_at: timestamp}}\nobjects:\n  - id: brand:1\n",
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			parsed, err := Parse([]byte(doc), FormatYAML)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			_, err = parsed.BuildRegistry("")
			if aerr.CodeOf(err) != aerr.APERTURE_CONFIG_INVALID {
				t.Fatalf("code = %s, want APERTURE_CONFIG_INVALID (err=%v)", aerr.CodeOf(err), err)
			}
		})
	}
}

// A declared TYPE that is not even a string (a number, a YAML boolean) is
// rejected by the document decode itself, before the section is reached — the
// map is typed map[string]string precisely so this cannot become a silently
// untyped field. It is a different code from the other declaration errors, which
// is fine: it is a malformed document, not a malformed declaration.
func TestFieldTypes_NonStringDeclaredType(t *testing.T) {
	for name, doc := range map[string]string{
		"number": "field_types:\n  - {object_type: brand, fields: {hired_at: 2026}}\n",
		"bool":   "field_types:\n  - {object_type: brand, fields: {hired_at: true}}\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(doc), FormatYAML)
			if aerr.CodeOf(err) != aerr.APERTURE_INVALID_INPUT {
				t.Fatalf("code = %s, want APERTURE_INVALID_INPUT (err=%v)", aerr.CodeOf(err), err)
			}
		})
	}
}

// Declaring field types for a type that has no objects: entries is legal — the
// entries may be arriving, or the type may be served by a providers: entry — and
// it registers nothing on its own.
func TestFieldTypes_DeclaredTypeWithNoObjectsIsFine(t *testing.T) {
	doc, err := Parse([]byte("field_types:\n  - {object_type: brand, fields: {hired_at: date}}\n"), FormatYAML)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	reg, err := doc.BuildRegistry("")
	if err != nil {
		t.Fatalf("BuildRegistry: %v", err)
	}
	if len(reg.Keys()) != 0 {
		t.Errorf("registry keys = %v, want none; a declaration registers nothing", reg.Keys())
	}
}

// field_types: applies to the objects: section ONLY. A providers: entry carries
// its own typing (the CSV header's :date / :datetime suffix), so a declaration
// is never silently imposed on rows a provider loaded — a value the CSV kept as
// an untyped string stays exactly that, however impossible a date it spells.
func TestFieldTypes_NotAppliedToProviderLoadedRows(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "brands.csv"),
		[]byte("id,hired_at\nbrand:1,2026-02-30\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	doc, err := Parse([]byte(
		"field_types:\n  - {object_type: brand, fields: {hired_at: date}}\n"+
			"providers:\n  - {object_type: brand, kind: csv, path: brands.csv, ttl: \"0\"}\n"), FormatYAML)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	reg, err := doc.BuildRegistry(dir)
	if err != nil {
		t.Fatalf("BuildRegistry: %v", err)
	}
	md, err := reg.Fetch(context.Background(), identity.MustParse("brand:1"))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if md["hired_at"] != "2026-02-30" {
		t.Errorf("hired_at = %v, want the untyped CSV cell verbatim; the declaration governs objects: only", md["hired_at"])
	}
}

// Validation does not depend on precedence: an inline entry is checked against
// the declaration even when a providers: entry wins its type and every inline
// entry is discarded. The collision is still reported through the existing
// ProviderCollisions channel, not a second one.
func TestFieldTypes_ValidatedEvenWhenInlineEntriesAreDiscarded(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "brands.csv"),
		[]byte("id,tier\nbrand:1,gold\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	const decl = "field_types:\n  - {object_type: brand, fields: {hired_at: date}}\n" +
		"providers:\n  - {object_type: brand, kind: csv, path: brands.csv, ttl: \"0\"}\n"

	bad, err := Parse([]byte(decl+"objects:\n  - id: brand:2\n    metadata: {hired_at: \"2026-02-30\"}\n"), FormatYAML)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, err := bad.BuildRegistry(dir); aerr.CodeOf(err) != aerr.APERTURE_CONFIG_INVALID {
		t.Fatalf("code = %s, want APERTURE_CONFIG_INVALID; a discarded entry is still validated", aerr.CodeOf(err))
	}

	good, err := Parse([]byte(decl+"objects:\n  - id: brand:2\n    metadata: {hired_at: \"2026-03-04\"}\n"), FormatYAML)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, err := good.BuildRegistry(dir); err != nil {
		t.Fatalf("BuildRegistry: %v", err)
	}
	if got := good.ProviderCollisions(); !reflect.DeepEqual(got, []string{"brand"}) {
		t.Errorf("ProviderCollisions = %v, want [brand]", got)
	}
}

// The seed file's FORMAT cannot change what a rule sees: a date declared in YAML
// and the same document written as JSON must land on the identical canonical
// string, and reject the identical values.
func TestFieldTypes_JSONAndYAMLAgree(t *testing.T) {
	yamlDoc, err := Parse([]byte(
		"field_types:\n  - {object_type: brand, fields: {hired_at: date, last_seen: datetime}}\n"+
			"objects:\n  - id: brand:1\n    metadata: {hired_at: \"2026-03-04\", last_seen: \"2026-03-04T01:02:03.456Z\"}\n"), FormatYAML)
	if err != nil {
		t.Fatalf("Parse YAML: %v", err)
	}
	jsonDoc, err := Parse([]byte(`{"field_types":[{"object_type":"brand","fields":{"hired_at":"date","last_seen":"datetime"}}],`+
		`"objects":[{"id":"brand:1","metadata":{"hired_at":"2026-03-04","last_seen":"2026-03-04T01:02:03.456Z"}}]}`), FormatJSON)
	if err != nil {
		t.Fatalf("Parse JSON: %v", err)
	}
	want := map[string]any{"hired_at": "2026-03-04", "last_seen": "2026-03-04T01:02:03Z"}
	for name, doc := range map[string]*Document{"yaml": yamlDoc, "json": jsonDoc} {
		t.Run(name, func(t *testing.T) {
			if len(doc.FieldTypes) != 1 || doc.FieldTypes[0].ObjectType != "brand" {
				t.Fatalf("field_types = %+v", doc.FieldTypes)
			}
			reg, err := doc.BuildRegistry("")
			if err != nil {
				t.Fatalf("BuildRegistry: %v", err)
			}
			md, err := reg.Fetch(context.Background(), identity.MustParse("brand:1"))
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			if !reflect.DeepEqual(map[string]any(md), want) {
				t.Errorf("metadata = %#v, want %#v", md, want)
			}
		})
	}
}

// The section is OPTIONAL and ADDITIVE. A document that predates it must load
// and build exactly as it did before — the committed example fixture is the
// regression guard, since it was authored with no declaration and cannot have
// been written against one.
func TestFieldTypes_DocumentWithNoDeclarationIsUnchanged(t *testing.T) {
	ctx := context.Background()
	doc, err := ParseFile(filepath.Join("testdata", "example.yaml"))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if len(doc.FieldTypes) != 0 {
		t.Fatalf("fixture declares field types (%+v); it is meant to predate the section", doc.FieldTypes)
	}
	if err := doc.Apply(ctx, memory.New()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if _, err := doc.BuildRegistry("testdata"); err != nil {
		t.Fatalf("BuildRegistry: %v", err)
	}

	// And the inline objects fixture, which does have metadata to leave alone.
	inline, err := Parse([]byte(inlineObjectsYAML), FormatYAML)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	reg, err := inline.BuildRegistry("")
	if err != nil {
		t.Fatalf("BuildRegistry: %v", err)
	}
	md, err := reg.Fetch(ctx, identity.MustParse("account:acme/brand:1"))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	// A date-shaped string in an UNDECLARED field is an ordinary string scalar:
	// the value model stays date-blind, and only a declaration changes that.
	if md["tier"] != "gold" || md["seats"] != int64(12) {
		t.Errorf("metadata = %#v, want the pre-existing values untouched", md)
	}
}

// A date-shaped string in a field nobody declared is NOT date-validated — the
// declaration is the only thing that turns a string into a date, exactly as the
// CSV column suffix is.
func TestFieldTypes_UndeclaredFieldIsNotDateValidated(t *testing.T) {
	doc, err := Parse([]byte(
		"field_types:\n  - {object_type: brand, fields: {hired_at: date}}\n"+
			"objects:\n  - id: brand:1\n    metadata: {hired_at: \"2026-03-04\", left_at: \"2026-02-30\"}\n"), FormatYAML)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	reg, err := doc.BuildRegistry("")
	if err != nil {
		t.Fatalf("BuildRegistry: %v", err)
	}
	md, err := reg.Fetch(context.Background(), identity.MustParse("brand:1"))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if md["left_at"] != "2026-02-30" {
		t.Errorf("left_at = %v, want the undeclared string verbatim", md["left_at"])
	}
}

// The section is WIRING, not model state: applying a document that contains
// nothing else must leave the store as it was, and an export must not reproduce
// it — the same contract providers: and objects: hold.
func TestFieldTypes_ApplyWritesNothingAndExportOmitsThem(t *testing.T) {
	ctx := context.Background()
	doc, err := Parse([]byte(datedObjectsYAML), FormatYAML)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	untouched := memory.New()
	baseline, err := Export(ctx, untouched)
	if err != nil {
		t.Fatalf("Export baseline: %v", err)
	}

	store := memory.New()
	if err := doc.Apply(ctx, store); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	after, err := Export(ctx, store)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if !reflect.DeepEqual(after, baseline) {
		t.Errorf("Apply wrote model state for the field_types section:\n got %+v\nwant %+v", after, baseline)
	}
	if len(after.FieldTypes) != 0 {
		t.Errorf("Export emitted %d field type declarations; the section is never exported", len(after.FieldTypes))
	}
	out, err := Marshal(after, FormatYAML)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(out), "field_types") {
		t.Errorf("marshalled export contains a field_types section:\n%s", out)
	}
}
