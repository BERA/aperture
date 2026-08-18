package mcp

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/frankbardon/aperture/engine"
	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/identity"
	"github.com/frankbardon/aperture/mcp/toolmeta"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/provider"
	"github.com/frankbardon/aperture/service"
	"github.com/frankbardon/aperture/storage/memory"
)

// E3-S3: an AGENT must be able to enumerate THROUGH a declared reference, not
// only a Go caller and a Twirp client. aperture_enumerate takes the facade's
// EnumerateQuery verbatim (EnumerateIn is an alias), so the work is in the
// reflected schema — which is what an MCP client reads to decide whether it may
// send the argument at all — plus a per-surface check that the fail-closed
// semantics did not change shape on the way through the tool.

// refService wires the graph `aperture mcp` builds, with one registry as scope
// lister, metadata source and declared-reference source:
//
//	dataset:x       lists brand:1 and brand:2 (brand:3 exists but is not listed)
//	dataset:secret  exists, alice may NOT read it
//	brand:1..3      the target type
func refService(t *testing.T) *service.Service {
	t.Helper()
	ctx := context.Background()
	store := memory.New()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	must(store.Setup(ctx))
	must(store.PutAccount(ctx, model.Account{ID: "acme", Name: "Acme"}))
	must(store.PutPrincipal(ctx, model.Principal{ID: "alice", Kind: model.PrincipalUser, Identity: "user:alice"}))
	must(store.PutMembership(ctx, model.Membership{PrincipalID: "alice", AccountID: "acme"}))
	for _, typ := range []string{"dataset", "brand"} {
		must(store.PutObjectType(ctx, model.ObjectType{Name: typ, Actions: []string{"list"}}))
		must(store.PutPermission(ctx, model.Permission{
			ID: "p-" + typ, ObjectType: typ, Action: "list", ScopeStrategy: "implicit",
		}))
		must(store.PutGrant(ctx, model.Grant{
			ID: "g-" + typ, AccountID: "acme", Subject: model.Subject{Kind: model.SubjectPrincipal, ID: "alice"},
			PermissionID: "p-" + typ, Object: "account:acme/**", Effect: model.EffectAllow,
		}))
	}
	must(store.PutGrant(ctx, model.Grant{
		ID: "g-secret-deny", AccountID: "acme", Subject: model.Subject{Kind: model.SubjectPrincipal, ID: "alice"},
		PermissionID: "p-dataset", Object: "account:acme/dataset:secret", Effect: model.EffectDeny,
	}))

	datasets, err := provider.NewStatic([]provider.Object{
		{ID: identity.MustParse("account:acme/dataset:x"), Metadata: provider.Metadata{
			"current_brands": []any{"account:acme/brand:1", "account:acme/brand:2"}}},
		{ID: identity.MustParse("account:acme/dataset:secret"), Metadata: provider.Metadata{
			"current_brands": []any{"account:acme/brand:3"}}},
	})
	if err != nil {
		t.Fatalf("NewStatic(datasets): %v", err)
	}
	brands, err := provider.NewStatic([]provider.Object{
		{ID: identity.MustParse("account:acme/brand:1"), Metadata: provider.Metadata{"region": "us"}},
		{ID: identity.MustParse("account:acme/brand:2"), Metadata: provider.Metadata{"region": "eu"}},
		{ID: identity.MustParse("account:acme/brand:3"), Metadata: provider.Metadata{"region": "us"}},
	})
	if err != nil {
		t.Fatalf("NewStatic(brands): %v", err)
	}
	reg := provider.NewRegistry()
	reg.MustRegister("dataset", datasets, provider.WithTTL(0))
	reg.MustRegister("brand", brands, provider.WithTTL(0))
	reg.MustDeclareReference("dataset", "current_brands", "brand")

	eng := engine.New(store,
		engine.WithScopeResolution(nil, engine.ScopeDeps{Lister: reg}),
		engine.WithMetadata(reg),
		engine.WithReferences(reg),
	)
	return service.New(eng, service.WithProviders(reg))
}

// TestEnumerateToolSchemaCarriesTheReferenceEdges is the contract half: an MCP
// client reads the reflected input schema and must find the edges there, as an
// OPTIONAL array of {HolderType?, HolderID, Field}. Optional is load-bearing —
// a required edge list would make an unrestricted enumerate, by far the common
// call, unrepresentable for a schema-validating client.
func TestEnumerateToolSchemaCarriesTheReferenceEdges(t *testing.T) {
	for _, name := range []string{toolmeta.ToolEnumerate, toolmeta.ToolEnumerateBatch} {
		ts, ok := SchemaFor(name)
		if !ok {
			t.Fatalf("no schema registered for %s", name)
		}
		var schema map[string]any
		if err := json.Unmarshal(ts.InputSchema, &schema); err != nil {
			t.Fatalf("%s input schema is not JSON: %v", name, err)
		}
		props := enumerateQuerySchema(t, name, schema)

		refs, ok := props["References"].(map[string]any)
		if !ok {
			t.Fatalf("%s input schema has no References property:\n%s", name, ts.InputSchema)
		}
		// The reflector types an omittable slice as ["null","array"], so the
		// assertion is "an array is admissible", not an exact spelling.
		if !admitsType(refs["type"], "array") {
			t.Errorf("%s References is typed %v, want array", name, refs["type"])
		}
		desc, _ := refs["description"].(string)
		if desc == "" {
			t.Errorf("%s References carries no description — an agent has no other way to learn the semantics", name)
		}
		// The two things an agent cannot infer from the shape: several edges
		// AND, and an unreadable holder is empty rather than an error.
		for _, want := range []string{"ANDed", "empty result"} {
			if !strings.Contains(desc, want) {
				t.Errorf("%s References description does not mention %q: %s", name, want, desc)
			}
		}

		items, ok := refs["items"].(map[string]any)
		if !ok {
			t.Fatalf("%s References has no items schema:\n%s", name, ts.InputSchema)
		}
		edge, ok := items["properties"].(map[string]any)
		if !ok {
			t.Fatalf("%s References items has no properties:\n%s", name, ts.InputSchema)
		}
		for _, field := range []string{"HolderType", "HolderID", "Field"} {
			if _, ok := edge[field].(map[string]any); !ok {
				t.Errorf("%s reference edge has no %s property", name, field)
			}
		}
		required := map[string]bool{}
		req, _ := items["required"].([]any)
		for _, r := range req {
			if s, ok := r.(string); ok {
				required[s] = true
			}
		}
		// HolderID and Field are mandatory; HolderType is derived from the id
		// when omitted, so requiring it would force an agent to restate — and
		// possibly contradict — what the identity already says.
		if !required["HolderID"] || !required["Field"] {
			t.Errorf("%s reference edge must require HolderID and Field, got %v", name, required)
		}
		if required["HolderType"] {
			t.Errorf("%s marks HolderType REQUIRED — it is derived from HolderID when omitted", name)
		}
	}
}

// TestEnumerateToolSchemaDoesNotRequireReferences fails if the enumerate-query
// object marks the edge list required. It complements assertNotRequired (Fields)
// in the filter test — both properties must stay omissible for the same reason.
func TestEnumerateToolSchemaDoesNotRequireReferences(t *testing.T) {
	for _, name := range []string{toolmeta.ToolEnumerate, toolmeta.ToolEnumerateBatch} {
		ts, _ := SchemaFor(name)
		var schema map[string]any
		if err := json.Unmarshal(ts.InputSchema, &schema); err != nil {
			t.Fatalf("%s input schema is not JSON: %v", name, err)
		}
		obj := schema
		if props, ok := schema["properties"].(map[string]any); ok {
			if queries, ok := props["queries"].(map[string]any); ok {
				obj, _ = queries["items"].(map[string]any)
			}
		}
		req, _ := obj["required"].([]any)
		for _, r := range req {
			if r == "References" {
				t.Errorf("%s marks References REQUIRED — an unrestricted enumerate must stay expressible", name)
			}
		}
	}
}

// TestEnumerateToolAppliesTheReferenceEdges drives the real tool invocation an
// MCP client makes, with the raw JSON arguments it would send — including the
// fail-closed cases, which must reach the agent the same way they reach every
// other surface.
func TestEnumerateToolAppliesTheReferenceEdges(t *testing.T) {
	invoke := invokerFor(t, toolmeta.ToolEnumerate)
	svc := refService(t)
	const base = `{"Account":"acme","Principal":"alice","Action":"list","Pattern":"account:acme/brand:*"`

	cases := []struct {
		name string
		args string
		want []string
	}{
		{
			name: "no References key at all enumerates everything",
			args: base + `}`,
			want: []string{"account:acme/brand:1", "account:acme/brand:2", "account:acme/brand:3"},
		},
		{
			name: "an empty edge list restricts nothing",
			args: base + `,"References":[]}`,
			want: []string{"account:acme/brand:1", "account:acme/brand:2", "account:acme/brand:3"},
		},
		{
			name: "an edge restricts to what the holder names",
			args: base + `,"References":[{"HolderID":"account:acme/dataset:x","Field":"current_brands"}]}`,
			want: []string{"account:acme/brand:1", "account:acme/brand:2"},
		},
		{
			name: "the holder type may be stated when it agrees",
			args: base + `,"References":[{"HolderType":"dataset","HolderID":"account:acme/dataset:x","Field":"current_brands"}]}`,
			want: []string{"account:acme/brand:1", "account:acme/brand:2"},
		},
		{
			// The disclosure rule, on the agent surface: a holder alice may not
			// read is an empty list, NOT a tool error the agent could read as
			// "this dataset exists but is forbidden".
			name: "an unreadable holder is empty, not an error",
			args: base + `,"References":[{"HolderID":"account:acme/dataset:secret","Field":"current_brands"}]}`,
			want: []string{},
		},
		{
			name: "a holder outside the account is empty, whether or not it exists",
			args: base + `,"References":[{"HolderID":"account:other/dataset:z","Field":"current_brands"}]}`,
			want: []string{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := invoke(context.Background(), svc, json.RawMessage(tc.args))
			if err != nil {
				t.Fatalf("invoke %s: %v", toolmeta.ToolEnumerate, err)
			}
			res, ok := out.(EnumerateOut)
			if !ok {
				t.Fatalf("enumerate returned %T, want EnumerateOut", out)
			}
			got := slices.Clone(res.Objects)
			slices.Sort(got)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("objects = %v, want %v", got, tc.want)
			}
		})
	}
}

// The other half of the boundary: an ABSENT holder inside the account is a
// CODED error the agent can act on, not an empty list. Collapsing it into
// "no objects" would take away the one diagnosis a typo deserves.
func TestEnumerateToolAbsentInAccountHolderIsACodedError(t *testing.T) {
	invoke := invokerFor(t, toolmeta.ToolEnumerate)
	svc := refService(t)

	_, err := invoke(context.Background(), svc, json.RawMessage(
		`{"Account":"acme","Principal":"alice","Action":"list","Pattern":"account:acme/brand:*",`+
			`"References":[{"HolderID":"account:acme/dataset:nope","Field":"current_brands"}]}`))
	if aerr.CodeOf(err) != aerr.APERTURE_NOT_FOUND {
		t.Fatalf("absent in-account holder = %v (code %s), want APERTURE_NOT_FOUND", err, aerr.CodeOf(err))
	}
}

// An undeclared field is a coded error too — never an empty list, which an agent
// would report to its user as "there are no brands in that dataset".
func TestEnumerateToolUndeclaredReferenceFieldIsACodedError(t *testing.T) {
	invoke := invokerFor(t, toolmeta.ToolEnumerate)
	svc := refService(t)

	_, err := invoke(context.Background(), svc, json.RawMessage(
		`{"Account":"acme","Principal":"alice","Action":"list","Pattern":"account:acme/brand:*",`+
			`"References":[{"HolderID":"account:acme/dataset:x","Field":"not_a_reference"}]}`))
	if aerr.CodeOf(err) != aerr.APERTURE_PROVIDER_REFERENCE_INVALID {
		t.Fatalf("undeclared field = %v (code %s), want APERTURE_PROVIDER_REFERENCE_INVALID",
			err, aerr.CodeOf(err))
	}
}

// admitsType reports whether a reflected "type" — a string, or the ["null",X]
// union the reflector emits for an omittable field — admits want.
func admitsType(got any, want string) bool {
	switch v := got.(type) {
	case string:
		return v == want
	case []any:
		for _, t := range v {
			if s, ok := t.(string); ok && s == want {
				return true
			}
		}
	}
	return false
}
