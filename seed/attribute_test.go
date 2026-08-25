package seed

import (
	"context"
	"reflect"
	"strings"
	"testing"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/provider"
	"github.com/frankbardon/aperture/storage/memory"
)

// attributeDoc parses a YAML fragment or fails the test.
func attributeDoc(t *testing.T, body string) *Document {
	t.Helper()
	doc, err := Parse([]byte(body), FormatYAML)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return doc
}

// TestBuildAttributeRegistry_ServesEverySlot is the section's whole promise: a
// bag declared in YAML is fetchable from the slot its subject: names, with the
// values normalised the way every other loader normalises them.
func TestBuildAttributeRegistry_ServesEverySlot(t *testing.T) {
	ctx := context.Background()
	doc := attributeDoc(t, `
attributes:
  - subject: user
    id: alice
    metadata:
      department: eng
      clearance: 3
      teams: [platform, infra]
  - subject: machine
    id: ci-runner
    metadata: {department: eng}
  - subject: account
    id: acme
    metadata: {plan: enterprise}
`)
	reg, err := doc.BuildAttributeRegistry("")
	if err != nil {
		t.Fatalf("BuildAttributeRegistry: %v", err)
	}
	if got, want := reg.RegisteredSlots(), provider.AttributeSlots(); !reflect.DeepEqual(got, want) {
		t.Fatalf("RegisteredSlots() = %v; want %v", got, want)
	}

	md, err := reg.Fetch(ctx, provider.AttributeSlotUser, "alice")
	if err != nil {
		t.Fatalf("Fetch(user, alice): %v", err)
	}
	// An exact integer that fits int64 lands as an int64, so principal.clearance
	// == 3 answers the same in YAML, in JSON, and (E3-S1) from a CSV :int column.
	if got, ok := md["clearance"].(int64); !ok || got != 3 {
		t.Errorf("clearance = %#v; want int64(3)", md["clearance"])
	}
	if got, want := md["teams"], []any{"platform", "infra"}; !reflect.DeepEqual(got, want) {
		t.Errorf("teams = %#v; want %#v", got, want)
	}

	if md, err = reg.Fetch(ctx, provider.AttributeSlotMachine, "ci-runner"); err != nil {
		t.Fatalf("Fetch(machine, ci-runner): %v", err)
	}
	if md["department"] != "eng" {
		t.Errorf("machine department = %#v", md["department"])
	}
	if md, err = reg.Fetch(ctx, provider.AttributeSlotAccount, "acme"); err != nil {
		t.Fatalf("Fetch(account, acme): %v", err)
	}
	if md["plan"] != "enterprise" {
		t.Errorf("account plan = %#v", md["plan"])
	}
}

// The kind-dispatching entry point the rules engine actually calls: a user
// principal is answered out of the user slot, a machine principal out of the
// machine slot, and "account" is not a principal kind at all.
func TestBuildAttributeRegistry_ResolvesPrincipalsByKind(t *testing.T) {
	ctx := context.Background()
	doc := attributeDoc(t, `
attributes:
  - {subject: user, id: shared, metadata: {which: user-slot}}
  - {subject: machine, id: shared, metadata: {which: machine-slot}}
  - {subject: account, id: shared, metadata: {which: account-slot}}
`)
	reg, err := doc.BuildAttributeRegistry("")
	if err != nil {
		t.Fatalf("BuildAttributeRegistry: %v", err)
	}
	for _, tc := range []struct {
		kind string
		want any
	}{
		{"user", "user-slot"},
		{"machine", "machine-slot"},
		// "account" is a slot but NOT a principal kind: routing it here would
		// serve a tenant's bag as a principal's.
		{"account", nil},
		{"", nil},
	} {
		bag, err := reg.Attributes(ctx, tc.kind, "shared")
		if err != nil {
			t.Fatalf("Attributes(%q): %v", tc.kind, err)
		}
		if bag["which"] != tc.want {
			t.Errorf("Attributes(%q)[which] = %#v; want %#v", tc.kind, bag["which"], tc.want)
		}
	}
}

// A document declaring no attributes still builds a usable registry, so a caller
// wires it unconditionally. No slot is filled, so a fetch is a wiring diagnostic
// rather than an empty bag that reads as "this principal has no attributes".
func TestBuildAttributeRegistry_EmptyDocumentIsUsable(t *testing.T) {
	reg, err := (&Document{}).BuildAttributeRegistry("")
	if err != nil {
		t.Fatalf("BuildAttributeRegistry: %v", err)
	}
	if reg == nil {
		t.Fatal("BuildAttributeRegistry returned a nil registry for an empty document")
	}
	if slots := reg.RegisteredSlots(); len(slots) != 0 {
		t.Fatalf("RegisteredSlots() = %v; want none", slots)
	}
	_, err = reg.Fetch(context.Background(), provider.AttributeSlotUser, "alice")
	if got := aerr.CodeOf(err); got != aerr.APERTURE_ATTRIBUTE_PROVIDER_UNREGISTERED {
		t.Fatalf("code = %s; want APERTURE_ATTRIBUTE_PROVIDER_UNREGISTERED (%v)", got, err)
	}
}

// Declaration order is what Enumerate returns, and entries of different subjects
// may be interleaved freely in the file.
func TestBuildAttributeRegistry_PreservesDeclarationOrderPerSlot(t *testing.T) {
	doc := attributeDoc(t, `
attributes:
  - {subject: user, id: carol}
  - {subject: account, id: acme}
  - {subject: user, id: alice}
  - {subject: user, id: bob}
`)
	reg, err := doc.BuildAttributeRegistry("")
	if err != nil {
		t.Fatalf("BuildAttributeRegistry: %v", err)
	}
	records, err := reg.Enumerate(context.Background(), provider.AttributeSlotUser, provider.AttributeFilter{})
	if err != nil {
		t.Fatalf("Enumerate: %v", err)
	}
	var got []string
	for _, rec := range records {
		got = append(got, rec.ID)
	}
	if want := []string{"carol", "alice", "bob"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Enumerate order = %v; want %v", got, want)
	}
}

func TestBuildAttributeRegistry_RejectsAMalformedEntry(t *testing.T) {
	cases := []struct {
		name string
		body string
		code aerr.Code
		// names are substrings the message must carry, so the author is told
		// WHICH entry to fix.
		names []string
	}{
		{
			name:  "unknown subject",
			body:  `attributes: [{subject: robot, id: r2, metadata: {a: 1}}]`,
			code:  aerr.APERTURE_ATTRIBUTE_SLOT_UNKNOWN,
			names: []string{"robot"},
		},
		{
			name:  "missing subject",
			body:  `attributes: [{id: alice, metadata: {a: 1}}]`,
			code:  aerr.APERTURE_CONFIG_INVALID,
			names: []string{"subject"},
		},
		{
			name:  "missing id",
			body:  `attributes: [{subject: user, metadata: {a: 1}}]`,
			code:  aerr.APERTURE_CONFIG_INVALID,
			names: []string{"id"},
		},
		{
			name:  "duplicate subject and id",
			body:  "attributes:\n  - {subject: user, id: alice}\n  - {subject: user, id: alice}\n",
			code:  aerr.APERTURE_CONFIG_INVALID,
			names: []string{"more than once"},
		},
		{
			name:  "metadata is not a mapping",
			body:  `attributes: [{subject: user, id: alice, metadata: [a, b]}]`,
			code:  aerr.APERTURE_CONFIG_INVALID,
			names: []string{"mapping"},
		},
		{
			// The account wildcard names every account at once, so no bag can
			// answer it. The refusal comes from the provider, which is the one
			// authority on what an attribute KEY may be.
			name:  "the account wildcard is not a key",
			body:  `attributes: [{subject: account, id: "*"}]`,
			code:  aerr.APERTURE_ATTRIBUTE_PROVIDER_INVALID,
			names: []string{"wildcard"},
		},
		{
			name:  "a value the model rejects",
			body:  `attributes: [{subject: user, id: alice, metadata: {teams: [{a: 1}]}}]`,
			code:  aerr.APERTURE_METADATA_INVALID,
			names: []string{"alice", "user"},
		},
		{
			name:  "a value nested too deep",
			body:  `attributes: [{subject: user, id: alice, metadata: {org: {team: {lead: {name: bob}}}}}]`,
			code:  aerr.APERTURE_METADATA_INVALID,
			names: []string{"alice"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := attributeDoc(t, tc.body)
			reg, err := doc.BuildAttributeRegistry("")
			if err == nil {
				t.Fatalf("the document built cleanly: %+v", reg.RegisteredSlots())
			}
			if got := aerr.CodeOf(err); got != tc.code {
				t.Fatalf("code = %s; want %s (%v)", got, tc.code, err)
			}
			for _, want := range tc.names {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not name %q", err, want)
				}
			}
		})
	}
}

// An oversized bag is rejected at LOAD, not on the Check hot path. It is asserted
// separately because the fixture has to be built rather than written out.
func TestBuildAttributeRegistry_RejectsAnOversizedBag(t *testing.T) {
	doc := &Document{Attributes: []Attribute{{
		Subject:  "user",
		ID:       "alice",
		Metadata: []byte(`{"blob":"` + strings.Repeat("x", provider.DefaultMaxValueBytes+1) + `"}`),
	}}}
	_, err := doc.BuildAttributeRegistry("")
	if got := aerr.CodeOf(err); got != aerr.APERTURE_METADATA_INVALID {
		t.Fatalf("code = %s; want APERTURE_METADATA_INVALID (%v)", got, err)
	}
	if !strings.Contains(err.Error(), "alice") {
		t.Errorf("error %q does not name the offending entry", err)
	}
}

// The same bare id under two DIFFERENT subjects is two unrelated subjects — a
// tenant called "acme" and a service principal called "acme" — and refusing it
// would refuse a legal deployment. Only a collision WITHIN a slot is a fault.
func TestBuildAttributeRegistry_TheSameIDInTwoSlotsIsLegal(t *testing.T) {
	doc := attributeDoc(t, `
attributes:
  - {subject: user, id: acme, metadata: {which: principal}}
  - {subject: account, id: acme, metadata: {which: tenant}}
`)
	reg, err := doc.BuildAttributeRegistry("")
	if err != nil {
		t.Fatalf("BuildAttributeRegistry: %v", err)
	}
	ctx := context.Background()
	user, err := reg.Fetch(ctx, provider.AttributeSlotUser, "acme")
	if err != nil {
		t.Fatalf("Fetch(user): %v", err)
	}
	account, err := reg.Fetch(ctx, provider.AttributeSlotAccount, "acme")
	if err != nil {
		t.Fatalf("Fetch(account): %v", err)
	}
	if user["which"] != "principal" || account["which"] != "tenant" {
		t.Fatalf("the two slots served each other's bags: user=%v account=%v", user, account)
	}
}

// An entry may be declared with no metadata: at all — a subject that exists and
// carries nothing. It is a found subject with an empty bag, not a missing one.
func TestBuildAttributeRegistry_AnEntryMayCarryNothing(t *testing.T) {
	doc := attributeDoc(t, `
attributes:
  - {subject: user, id: alice}
  - {subject: user, id: bob, metadata: }
`)
	reg, err := doc.BuildAttributeRegistry("")
	if err != nil {
		t.Fatalf("BuildAttributeRegistry: %v", err)
	}
	for _, id := range []string{"alice", "bob"} {
		md, err := reg.Fetch(context.Background(), provider.AttributeSlotUser, id)
		if err != nil {
			t.Fatalf("Fetch(%s): %v", id, err)
		}
		if len(md) != 0 {
			t.Errorf("%s = %v; want an empty bag", id, md)
		}
	}
}

// TestAttributeWiringIsNotModelState is TestReferenceWiringIsNotModelState for
// the attribute seam, and it is the reason the block has its own key rather than
// a metadata: field on principals:/accounts:. Apply writes nothing for it, and
// because Export reads the model back OUT of storage, an export reproduces none
// of it — so hanging the bag off a model entry would have made the export either
// lossy or untruthful.
func TestAttributeWiringIsNotModelState(t *testing.T) {
	ctx := context.Background()
	doc := attributeDoc(t, `
accounts:
  - {id: acme, name: Acme}
principals:
  - {id: alice, kind: user, identity: "user:alice", display_name: Alice}
attributes:
  - {subject: user, id: alice, metadata: {department: eng}}
  - {subject: account, id: acme, metadata: {plan: enterprise}}
`)
	store := memory.New()
	if err := doc.Apply(ctx, store); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	out, err := Export(ctx, store)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if len(out.Attributes) != 0 {
		t.Fatalf("export reproduced the attributes: block: %+v", out.Attributes)
	}
	// The model entities themselves DID land — otherwise the assertion above
	// would pass against a build where Apply wrote nothing at all.
	if len(out.Principals) != 1 || out.Principals[0].ID != "alice" {
		t.Fatalf("Apply did not write the model: %+v", out.Principals)
	}
}

// The gate a host reads before wiring an attribute registry into its decision
// stack. It MUST count EVERY section that can declare an attribute source — the
// gate lives beside the fields it counts precisely so adding one is one edit in
// one file. hasObjectSources records the bug that taught us why: a gate written
// over one section of two is a silent one.
func TestHasAttributeSources(t *testing.T) {
	if (*Document)(nil).HasAttributeSources() {
		t.Error("a nil document declares no attribute source")
	}
	if (&Document{}).HasAttributeSources() {
		t.Error("an empty document declares no attribute source")
	}
	doc := &Document{Attributes: []Attribute{{Subject: "user", ID: "alice"}}}
	if !doc.HasAttributeSources() {
		t.Error("an inline attributes: block is an attribute source")
	}
	// A seed whose ONLY attribute source is external must wire the resolvers too,
	// or the directory it named would be read by nothing.
	doc = &Document{AttributeProviders: []AttributeProvider{{Subject: "user", Kind: "csv", Path: "u.csv"}}}
	if !doc.HasAttributeSources() {
		t.Error("an attribute_providers: block is an attribute source")
	}
}
