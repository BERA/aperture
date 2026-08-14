package seed

import (
	"context"
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

// Until E3-S2 settles precedence, a type served by BOTH sections is a hard
// registration conflict rather than a silent winner.
func TestInlineObjects_ConflictWithProvidersSection(t *testing.T) {
	doc := &Document{
		Providers: []Provider{{ObjectType: "brand", Kind: "csv", Path: "brands.csv"}},
		Objects:   []Object{{ID: "brand:1"}},
	}
	_, err := doc.BuildRegistry("")
	if aerr.CodeOf(err) != aerr.APERTURE_PROVIDER_INVALID {
		t.Fatalf("code = %s, want APERTURE_PROVIDER_INVALID", aerr.CodeOf(err))
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
