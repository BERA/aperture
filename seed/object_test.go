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
	"github.com/frankbardon/aperture/storage/memory"
)

const inlineObjectsYAML = `
objects:
  - id: account:acme/brand:2
    metadata:
      tier: silver
      seats: 5
  - id: account:acme/brand:1
    metadata:
      tier: gold
      seats: 12
      budget: 10.5
      active: true
      tags: [premium, launch]
      owner:
        dept: eng
        lead: alice
  - id: account:acme/app:be
    metadata:
      tier: gold
  - id: account:acme/brand:3
`

func TestInlineObjects_ParseAndBuild(t *testing.T) {
	doc, err := Parse([]byte(inlineObjectsYAML), FormatYAML)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(doc.Objects) != 4 || doc.Objects[0].ID != "account:acme/brand:2" {
		t.Fatalf("objects = %+v", doc.Objects)
	}

	reg, err := doc.BuildRegistry("")
	if err != nil {
		t.Fatalf("BuildRegistry: %v", err)
	}
	// The object-type is derived from each identity's TERMINAL segment, so two
	// types are registered from one flat list and no entry declares its type.
	keys := reg.Keys()
	if len(keys) != 2 || !reg.Has("brand") || !reg.Has("app") {
		t.Fatalf("registry keys = %v, want brand and app", keys)
	}

	md, err := reg.Fetch(context.Background(), identity.MustParse("account:acme/brand:1"))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	want := map[string]any{
		"tier":   "gold",
		"seats":  int64(12), // an exact integer normalises to int64, as a CSV :int does
		"budget": 10.5,
		"active": true,
		"tags":   []any{"premium", "launch"},
		"owner":  map[string]any{"dept": "eng", "lead": "alice"},
	}
	if !reflect.DeepEqual(map[string]any(md), want) {
		t.Errorf("metadata = %#v, want %#v", md, want)
	}
}

// An entry with no metadata: key is a legal object with no fields, not an error.
func TestInlineObjects_NoMetadata(t *testing.T) {
	doc, err := Parse([]byte(inlineObjectsYAML), FormatYAML)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	reg, err := doc.BuildRegistry("")
	if err != nil {
		t.Fatalf("BuildRegistry: %v", err)
	}
	md, err := reg.Fetch(context.Background(), identity.MustParse("account:acme/brand:3"))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(md) != 0 {
		t.Errorf("metadata = %v, want empty", md)
	}
}

// The JSON path must land on the same values as the YAML path — the seed file's
// format cannot change what a rule sees. int64 is the interesting one: YAML hands
// over an int and JSON a float64, and both must normalise to int64.
func TestInlineObjects_JSONAndYAMLAgree(t *testing.T) {
	yamlDoc, err := Parse([]byte("objects:\n  - id: brand:1\n    metadata:\n      seats: 12\n      budget: 10.5\n"), FormatYAML)
	if err != nil {
		t.Fatalf("Parse YAML: %v", err)
	}
	jsonDoc, err := Parse([]byte(`{"objects":[{"id":"brand:1","metadata":{"seats":12,"budget":10.5}}]}`), FormatJSON)
	if err != nil {
		t.Fatalf("Parse JSON: %v", err)
	}
	want := map[string]any{"seats": int64(12), "budget": 10.5}
	for name, doc := range map[string]*Document{"yaml": yamlDoc, "json": jsonDoc} {
		t.Run(name, func(t *testing.T) {
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

// Fetching an id the document never declared is APERTURE_NOT_FOUND, the contract
// every provider owes the Registry.
func TestInlineObjects_FetchUnknownIsNotFound(t *testing.T) {
	doc, err := Parse([]byte(inlineObjectsYAML), FormatYAML)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	reg, err := doc.BuildRegistry("")
	if err != nil {
		t.Fatalf("BuildRegistry: %v", err)
	}
	_, err = reg.Fetch(context.Background(), identity.MustParse("account:acme/brand:404"))
	if aerr.CodeOf(err) != aerr.APERTURE_NOT_FOUND {
		t.Fatalf("code = %s, want APERTURE_NOT_FOUND", aerr.CodeOf(err))
	}
}

// Enumeration through the Registry (the scope.ObjectLister seam) sees the objects
// in DECLARATION order, bounded by the pattern and the limit.
func TestInlineObjects_ListOrderPatternAndLimit(t *testing.T) {
	doc, err := Parse([]byte(inlineObjectsYAML), FormatYAML)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	reg, err := doc.BuildRegistry("")
	if err != nil {
		t.Fatalf("BuildRegistry: %v", err)
	}
	ids, err := reg.List(context.Background(), "brand", identity.MustParsePattern("account:acme/brand:*"), 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	got := make([]string, len(ids))
	for i, id := range ids {
		got[i] = id.String()
	}
	want := []string{"account:acme/brand:2", "account:acme/brand:1", "account:acme/brand:3"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("List = %v, want declaration order %v", got, want)
	}

	limited, err := reg.List(context.Background(), "brand", identity.MustParsePattern("account:acme/brand:*"), 2)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(limited) != 2 || limited[0].String() != "account:acme/brand:2" {
		t.Errorf("limited List = %v, want the first two in declaration order", limited)
	}
}

func TestInlineObjects_Errors(t *testing.T) {
	cases := map[string]struct {
		yaml string
		want aerr.Code
	}{
		"missing id": {
			"objects:\n  - metadata: {tier: gold}\n",
			aerr.APERTURE_CONFIG_INVALID,
		},
		"malformed id": {
			"objects:\n  - id: \"not an identity\"\n",
			aerr.APERTURE_IDENTITY_INVALID,
		},
		"duplicate id": {
			"objects:\n  - id: brand:1\n  - id: brand:1\n",
			aerr.APERTURE_CONFIG_INVALID,
		},
		"metadata is not a mapping": {
			"objects:\n  - id: brand:1\n    metadata: [a, b]\n",
			aerr.APERTURE_CONFIG_INVALID,
		},
		"metadata is a scalar": {
			"objects:\n  - id: brand:1\n    metadata: gold\n",
			aerr.APERTURE_CONFIG_INVALID,
		},
		"array of objects": {
			"objects:\n  - id: brand:1\n    metadata:\n      members: [{id: 1}]\n",
			aerr.APERTURE_CONFIG_INVALID,
		},
		"nested array": {
			"objects:\n  - id: brand:1\n    metadata:\n      tags: [[a]]\n",
			aerr.APERTURE_CONFIG_INVALID,
		},
		"past the depth cap": {
			"objects:\n  - id: brand:1\n    metadata:\n      owner: {lead: {name: {first: a}}}\n",
			aerr.APERTURE_CONFIG_INVALID,
		},
		"empty field name": {
			"objects:\n  - id: brand:1\n    metadata:\n      \"\": gold\n",
			aerr.APERTURE_CONFIG_INVALID,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			doc, err := Parse([]byte(tc.yaml), FormatYAML)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			_, err = doc.BuildRegistry("")
			if aerr.CodeOf(err) != tc.want {
				t.Fatalf("code = %s, want %s (err=%v)", aerr.CodeOf(err), tc.want, err)
			}
		})
	}
}

// A value-model violation names the object id AND the field, so an author can go
// straight to the offending line, and keeps the inner APERTURE_METADATA_INVALID
// (with its path/type context) in the chain.
func TestInlineObjects_ValueModelViolationNamesIDAndField(t *testing.T) {
	doc, err := Parse([]byte("objects:\n  - id: account:acme/brand:1\n    metadata:\n      members: [{id: 1}]\n"), FormatYAML)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	_, err = doc.BuildRegistry("")
	if aerr.CodeOf(err) != aerr.APERTURE_CONFIG_INVALID {
		t.Fatalf("code = %s, want APERTURE_CONFIG_INVALID", aerr.CodeOf(err))
	}
	msg := err.Error()
	for _, want := range []string{"account:acme/brand:1", "members", string(aerr.APERTURE_METADATA_INVALID)} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not mention %q", msg, want)
		}
	}
}

// The objects section is WIRING, not model state. Applying a document that
// contains nothing else must leave the store exactly as it was, and an export of
// that store must not reproduce the section — the "never persist provider data as
// source of truth" Non-Goal, asserted rather than assumed.
func TestInlineObjects_ApplyWritesNothingAndExportOmitsThem(t *testing.T) {
	ctx := context.Background()
	doc, err := Parse([]byte(inlineObjectsYAML), FormatYAML)
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
		t.Errorf("Apply wrote model state for the objects section:\n got %+v\nwant %+v", after, baseline)
	}
	if len(after.Objects) != 0 {
		t.Errorf("Export emitted %d objects; the section is never exported", len(after.Objects))
	}

	// Nor does it reach the marshalled file.
	out, err := Marshal(after, FormatYAML)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(out), "objects:") {
		t.Errorf("marshalled export contains an objects section:\n%s", out)
	}
}

// A type served by BOTH sections BUILDS by default: adding a CSV for a type that
// still carries inline entries is an ordinary migration step, not an authoring
// fault, and a document that booted yesterday must not stop booting because a
// providers: row was added. The discard is not silent — ProviderCollisions names
// the types — but it is not fatal either.
func TestInlineObjects_ConflictWithProvidersSectionBuildsByDefault(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "brands.csv"),
		[]byte("id,tier\nbrand:1,bronze\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	doc := &Document{
		Providers: []Provider{{ObjectType: "brand", Kind: "csv", Path: "brands.csv", TTL: "0"}},
		Objects:   []Object{{ID: "brand:1"}},
	}
	reg, err := doc.BuildRegistry(dir)
	if err != nil {
		t.Fatalf("BuildRegistry: %v", err)
	}
	if !reg.Has("brand") {
		t.Fatalf("registry keys = %v, want brand", reg.Keys())
	}
	// The file-backed provider is the one that survived.
	md, err := reg.Fetch(context.Background(), identity.MustParse("brand:1"))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if md["tier"] != "bronze" {
		t.Errorf("tier = %v, want bronze from the CSV", md["tier"])
	}
	if got := doc.ProviderCollisions(); !reflect.DeepEqual(got, []string{"brand"}) {
		t.Errorf("ProviderCollisions() = %#v, want [brand]", got)
	}
}

// StrictProviderCollision is the opt-in that turns the collision back into a
// refusal, for a host that reads the overlap as an authoring mistake rather than
// a migration in progress.
func TestInlineObjects_StrictCollisionRefusesToBuild(t *testing.T) {
	doc := &Document{
		Providers: []Provider{{ObjectType: "brand", Kind: "csv", Path: "brands.csv"}},
		Objects:   []Object{{ID: "brand:1"}},
	}
	_, err := doc.BuildRegistry("", StrictProviderCollision())
	if aerr.CodeOf(err) != aerr.APERTURE_CONFIG_INVALID {
		t.Fatalf("code = %s, want APERTURE_CONFIG_INVALID", aerr.CodeOf(err))
	}
	msg := err.Error()
	// The colliding TYPE is named so the author can find the two declarations...
	if !strings.Contains(msg, "brand") {
		t.Errorf("error %q does not name the colliding object type", msg)
	}
	// ...and the object id is NOT, because an id can embed an account.
	if strings.Contains(msg, "brand:1") {
		t.Errorf("error %q leaks an object id", msg)
	}
	// The error says which option produced it, so the reader knows the refusal
	// was asked for and how to stop asking.
	if !strings.Contains(msg, "StrictProviderCollision") {
		t.Errorf("error %q does not name the option that caused it", msg)
	}
}

// Under strict mode every colliding type is reported at once, in a stable order,
// so one fix pass clears the document.
func TestInlineObjects_StrictCollisionReportsEveryTypeSorted(t *testing.T) {
	doc := &Document{
		Providers: []Provider{
			{ObjectType: "brand", Kind: "csv", Path: "brands.csv"},
			{ObjectType: "app", Kind: "csv", Path: "apps.csv"},
		},
		Objects: []Object{{ID: "brand:1"}, {ID: "app:be"}},
	}
	_, err := doc.BuildRegistry("", StrictProviderCollision())
	if aerr.CodeOf(err) != aerr.APERTURE_CONFIG_INVALID {
		t.Fatalf("code = %s, want APERTURE_CONFIG_INVALID", aerr.CodeOf(err))
	}
	var ce *aerr.CodedError
	if !goerrors.As(err, &ce) {
		t.Fatalf("error is not a *CodedError: %v", err)
	}
	got, _ := ce.Context["object_types"].([]string)
	if !reflect.DeepEqual(got, []string{"app", "brand"}) {
		t.Errorf("object_types = %#v, want sorted [app brand]", ce.Context["object_types"])
	}
	// Sorted in the message too, so the text is stable across runs (the groups
	// themselves come back in declaration order, which is brand-then-app here).
	if !strings.Contains(err.Error(), "app, brand") {
		t.Errorf("error %q does not list both types in a stable order", err.Error())
	}
}

// ProviderCollisions is how the default discard reaches an operator, so it has to
// report the same set the build acts on: every colliding type, once, sorted, and
// nothing else. It is document-only — no registry, no file IO — so it answers the
// same before and after a build.
func TestInlineObjects_ProviderCollisionsReportsDiscardedTypes(t *testing.T) {
	cases := map[string]struct {
		doc  *Document
		want []string
	}{
		"no providers section": {
			&Document{Objects: []Object{{ID: "brand:1"}}}, nil,
		},
		"no objects section": {
			&Document{Providers: []Provider{{ObjectType: "brand", Kind: "csv", Path: "b.csv"}}}, nil,
		},
		"disjoint sections": {
			&Document{
				Providers: []Provider{{ObjectType: "app", Kind: "csv", Path: "a.csv"}},
				Objects:   []Object{{ID: "brand:1"}},
			}, nil,
		},
		"only the colliding type, sorted and deduplicated": {
			&Document{
				Providers: []Provider{
					{ObjectType: "brand", Kind: "csv", Path: "b.csv"},
					{ObjectType: "app", Kind: "csv", Path: "a.csv"},
				},
				Objects: []Object{
					{ID: "brand:1"}, {ID: "team:core"}, {ID: "app:be"}, {ID: "brand:2"},
				},
			}, []string{"app", "brand"},
		},
		"a nested id collides on its terminal type": {
			&Document{
				Providers: []Provider{{ObjectType: "brand", Kind: "csv", Path: "b.csv"}},
				Objects:   []Object{{ID: "account:acme/brand:1"}},
			}, []string{"brand"},
		},
		"an unparseable id is not a collision": {
			&Document{
				Providers: []Provider{{ObjectType: "brand", Kind: "csv", Path: "b.csv"}},
				Objects:   []Object{{ID: "not an identity"}},
			}, nil,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := tc.doc.ProviderCollisions()
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ProviderCollisions() = %#v, want %#v", got, tc.want)
			}
			// Types only, never ids: the value is destined for a log line and an
			// id can embed an account.
			for _, typ := range got {
				if strings.ContainsAny(typ, ":/") {
					t.Errorf("ProviderCollisions() returned %q, which is not a bare object type", typ)
				}
			}
		})
	}
}

// By default, precedence is TYPE-LEVEL and TOTAL: the file-backed
// provider serves the whole type and every inline entry for it is discarded.
// There is no merge at any granularity and no fallback — an inline id the CSV
// does not carry is simply not resolvable, and a field only the inline entry
// declared does not appear on the ids the CSV does carry.
func TestInlineObjects_ProviderPrecedenceDiscardsInlineEntirely(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "brands.csv"),
		[]byte("id,tier\nbrand:1,bronze\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	doc, err := Parse([]byte(`
providers:
  - {object_type: brand, kind: csv, path: brands.csv, ttl: "0"}
objects:
  - id: brand:1
    metadata: {tier: gold, seats: 12}
  - id: brand:2
    metadata: {tier: silver}
`), FormatYAML)
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
	// The file wins the value it does carry...
	if md["tier"] != "bronze" {
		t.Errorf("tier = %v, want bronze from the CSV", md["tier"])
	}
	// ...and does NOT gain the field only the inline entry declared: no
	// field-level merge, which is the whole point of the rule.
	if _, ok := md["seats"]; ok {
		t.Errorf("metadata = %v, want no inline field merged in", md)
	}
	// An id the CSV lacks is not resolvable: no object-level fallback either.
	if _, err := reg.Fetch(context.Background(), identity.MustParse("brand:2")); aerr.CodeOf(err) != aerr.APERTURE_NOT_FOUND {
		t.Errorf("code = %s, want APERTURE_NOT_FOUND for an id only the inline section declared", aerr.CodeOf(err))
	}
	// Nor through enumeration — the discarded entries are gone, not hidden.
	ids, err := reg.Identifiers(context.Background(), "brand")
	if err != nil {
		t.Fatalf("Identifiers: %v", err)
	}
	if len(ids) != 1 || ids[0].String() != "brand:1" {
		t.Errorf("Identifiers = %v, want only the CSV's ids", ids)
	}
}

// The discard is scoped to the colliding TYPE. Inline entries for a type with no
// providers: entry are untouched, in the same document.
func TestInlineObjects_NonCollidingTypesUnaffected(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "brands.csv"),
		[]byte("id,tier\nbrand:1,bronze\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	doc, err := Parse([]byte(`
providers:
  - {object_type: brand, kind: csv, path: brands.csv, ttl: "0"}
objects:
  - id: brand:1
    metadata: {tier: gold}
  - id: app:be
    metadata: {tier: gold, seats: 3}
`), FormatYAML)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	reg, err := doc.BuildRegistry(dir)
	if err != nil {
		t.Fatalf("BuildRegistry: %v", err)
	}
	if !reg.Has("brand") || !reg.Has("app") {
		t.Fatalf("registry keys = %v, want brand and app", reg.Keys())
	}
	md, err := reg.Fetch(context.Background(), identity.MustParse("app:be"))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	want := map[string]any{"tier": "gold", "seats": int64(3)}
	if !reflect.DeepEqual(map[string]any(md), want) {
		t.Errorf("app metadata = %#v, want %#v", md, want)
	}
	// The report is scoped the same way the discard is: app was kept, so it is
	// not named.
	if got := doc.ProviderCollisions(); !reflect.DeepEqual(got, []string{"brand"}) {
		t.Errorf("ProviderCollisions() = %#v, want [brand]", got)
	}
}

// Validation runs over EVERY inline entry before precedence is applied, so a
// malformed declaration still fails the load even when its type is about to be
// discarded. Otherwise a document would silently stop being checked the day
// someone added a CSV for one of its types.
func TestInlineObjects_DiscardedTypeIsStillValidated(t *testing.T) {
	cases := map[string]struct {
		yaml string
		want aerr.Code
	}{
		"value model violation": {
			"objects:\n  - id: brand:1\n    metadata:\n      members: [{id: 1}]\n",
			aerr.APERTURE_CONFIG_INVALID,
		},
		"duplicate id": {
			"objects:\n  - id: brand:1\n  - id: brand:1\n",
			aerr.APERTURE_CONFIG_INVALID,
		},
		"missing id": {
			"objects:\n  - metadata: {tier: gold}\n",
			aerr.APERTURE_CONFIG_INVALID,
		},
		"malformed id": {
			"objects:\n  - id: \"not an identity\"\n",
			aerr.APERTURE_IDENTITY_INVALID,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			doc, err := Parse([]byte(tc.yaml), FormatYAML)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			doc.Providers = []Provider{{ObjectType: "brand", Kind: "csv", Path: "brands.csv"}}
			// Both under the default discard and under the strict refusal: the
			// validation pass runs first either way, so the author is told what
			// is wrong with the entry rather than that it was ignored.
			if _, err := doc.BuildRegistry(""); aerr.CodeOf(err) != tc.want {
				t.Fatalf("default: code = %s, want %s (err=%v)", aerr.CodeOf(err), tc.want, err)
			}
			if _, err := doc.BuildRegistry("", StrictProviderCollision()); aerr.CodeOf(err) != tc.want {
				t.Fatalf("strict: code = %s, want %s (err=%v)", aerr.CodeOf(err), tc.want, err)
			}
		})
	}
}

// StrictProviderCollision changes nothing for a document that has no collision.
func TestInlineObjects_StrictCollisionIsANoOpWithoutCollision(t *testing.T) {
	doc, err := Parse([]byte(inlineObjectsYAML), FormatYAML)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	reg, err := doc.BuildRegistry("", StrictProviderCollision())
	if err != nil {
		t.Fatalf("BuildRegistry: %v", err)
	}
	if len(reg.Keys()) != 2 || !reg.Has("brand") || !reg.Has("app") {
		t.Fatalf("registry keys = %v, want brand and app", reg.Keys())
	}
	if got := doc.ProviderCollisions(); got != nil {
		t.Errorf("ProviderCollisions() = %#v, want none", got)
	}
}

// A document with neither wiring section still builds a usable, empty registry.
func TestInlineObjects_EmptySectionRegistersNothing(t *testing.T) {
	reg, err := (&Document{}).BuildRegistry("")
	if err != nil {
		t.Fatalf("BuildRegistry: %v", err)
	}
	if len(reg.Keys()) != 0 {
		t.Errorf("registry keys = %v, want none", reg.Keys())
	}
}
