package mcp

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/frankbardon/aperture/engine"
	"github.com/frankbardon/aperture/identity"
	"github.com/frankbardon/aperture/mcp/toolmeta"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/provider"
	"github.com/frankbardon/aperture/service"
	"github.com/frankbardon/aperture/storage/memory"
)

// E1-S3: an AGENT must be able to use the metadata filter, not only a Go caller
// and a Twirp client. The MCP enumerate tool takes the facade's EnumerateQuery
// verbatim (EnumerateIn is an alias), so the work is in the reflected schema —
// which is what an MCP client reads to decide whether it may send the argument
// at all — and in the enumerate command's wiring, tested in internal/cli.

// filterService wires the same graph `aperture mcp` now builds: an implicit
// scope so enumeration walks the object lister, plus the registry as the
// engine's metadata source so a Fields predicate has something to read.
//
//	dataset:a  tier "premium" seats 5   brands ["brand:Y","brand:Z"]
//	dataset:b  tier "basic"   seats 5   brands ["brand:Y"]
//	dataset:c  tier "premium" seats 12  (no brands field at all)
func filterService(t *testing.T) *service.Service {
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
	must(store.PutObjectType(ctx, model.ObjectType{Name: "dataset", Actions: []string{"list"}}))
	must(store.PutPermission(ctx, model.Permission{
		ID: "p-list", ObjectType: "dataset", Action: "list", ScopeStrategy: "implicit",
	}))
	must(store.PutGrant(ctx, model.Grant{
		ID: "g-list", AccountID: "acme", Subject: model.Subject{Kind: model.SubjectPrincipal, ID: "alice"},
		PermissionID: "p-list", Object: "account:acme/**", Effect: model.EffectAllow,
	}))

	static, err := provider.NewStatic([]provider.Object{
		{ID: identity.MustParse("account:acme/dataset:a"), Metadata: provider.Metadata{
			"tier": "premium", "seats": 5, "brands": []any{"brand:Y", "brand:Z"}}},
		{ID: identity.MustParse("account:acme/dataset:b"), Metadata: provider.Metadata{
			"tier": "basic", "seats": 5, "brands": []any{"brand:Y"}}},
		{ID: identity.MustParse("account:acme/dataset:c"), Metadata: provider.Metadata{
			"tier": "premium", "seats": 12}},
	})
	if err != nil {
		t.Fatalf("NewStatic: %v", err)
	}
	reg := provider.NewRegistry()
	reg.MustRegister("dataset", static, provider.WithTTL(0))

	eng := engine.New(store,
		engine.WithScopeResolution(nil, engine.ScopeDeps{Lister: reg}),
		engine.WithMetadata(reg),
	)
	return service.New(eng, service.WithProviders(reg))
}

// invokerFor returns the type-erased Invoke the adapter mounts for a tool.
func invokerFor(t *testing.T, name string) InvokeFunc {
	t.Helper()
	for _, d := range Tools(Config{}) {
		if d.Name == name {
			return d.Invoke
		}
	}
	t.Fatalf("no %s tool in the catalog", name)
	return nil
}

// TestEnumerateToolSchemaCarriesTheFilter is the contract half: an MCP client
// reads the reflected input schema and must find the predicate there, typed as
// an object of arbitrary JSON values (a want is a string, a number, a bool, or a
// list) and OPTIONAL. Optional is load-bearing: a required predicate would make
// an unfiltered enumeration — by far the common call — unrepresentable.
func TestEnumerateToolSchemaCarriesTheFilter(t *testing.T) {
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

		fields, ok := props["Fields"].(map[string]any)
		if !ok {
			t.Fatalf("%s input schema has no Fields property:\n%s", name, ts.InputSchema)
		}
		if fields["type"] != "object" {
			t.Errorf("%s Fields is typed %v, want object", name, fields["type"])
		}
		// additionalProperties:true is what makes the VALUES untyped, which is
		// the point: "5" and 5 are different predicates and both must be sendable.
		if fields["additionalProperties"] != true {
			t.Errorf("%s Fields must admit any JSON value per key, got additionalProperties=%v",
				name, fields["additionalProperties"])
		}
		desc, _ := fields["description"].(string)
		if desc == "" {
			t.Errorf("%s Fields carries no description — an agent has no other way to learn the semantics", name)
		}
		for _, want := range []string{"never matches", "membership"} {
			if !strings.Contains(desc, want) {
				t.Errorf("%s Fields description does not mention %q: %s", name, want, desc)
			}
		}
	}
}

// enumerateQuerySchema digs out the properties of the enumerate-query object,
// which the batch tool nests one level down inside its queries array.
func enumerateQuerySchema(t *testing.T, name string, schema map[string]any) map[string]any {
	t.Helper()
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("%s input schema has no properties", name)
	}
	if queries, ok := props["queries"].(map[string]any); ok {
		items, ok := queries["items"].(map[string]any)
		if !ok {
			t.Fatalf("%s queries has no items schema", name)
		}
		if inner, ok := items["properties"].(map[string]any); ok {
			assertNotRequired(t, name, items)
			return inner
		}
		t.Fatalf("%s queries items has no properties", name)
	}
	assertNotRequired(t, name, schema)
	return props
}

// assertNotRequired fails if the enumerate-query object marks Fields required.
func assertNotRequired(t *testing.T, name string, obj map[string]any) {
	t.Helper()
	req, _ := obj["required"].([]any)
	for _, r := range req {
		if r == "Fields" {
			t.Errorf("%s marks Fields REQUIRED — an unfiltered enumerate must stay expressible", name)
		}
	}
}

// TestEnumerateToolAppliesTheFilter drives the real tool invocation an MCP
// client makes, with the raw JSON arguments it would send.
func TestEnumerateToolAppliesTheFilter(t *testing.T) {
	invoke := invokerFor(t, toolmeta.ToolEnumerate)
	svc := filterService(t)

	cases := []struct {
		name string
		args string
		want []string
	}{
		{
			name: "no Fields key at all enumerates everything",
			args: `{"Account":"acme","Principal":"alice","Action":"list","Pattern":"account:acme/**"}`,
			want: []string{"account:acme/dataset:a", "account:acme/dataset:b", "account:acme/dataset:c"},
		},
		{
			name: "an empty predicate object filters nothing",
			args: `{"Account":"acme","Principal":"alice","Action":"list","Pattern":"account:acme/**","Fields":{}}`,
			want: []string{"account:acme/dataset:a", "account:acme/dataset:b", "account:acme/dataset:c"},
		},
		{
			name: "a string want",
			args: `{"Account":"acme","Principal":"alice","Action":"list","Pattern":"account:acme/**","Fields":{"tier":"premium"}}`,
			want: []string{"account:acme/dataset:a", "account:acme/dataset:c"},
		},
		{
			// The typed distinction an agent gets for free that the CLI needs a
			// second flag for: JSON already says whether 5 is a number.
			name: "a numeric want matches the numeric field",
			args: `{"Account":"acme","Principal":"alice","Action":"list","Pattern":"account:acme/**","Fields":{"seats":5}}`,
			want: []string{"account:acme/dataset:a", "account:acme/dataset:b"},
		},
		{
			name: "the same want as a string matches nothing",
			args: `{"Account":"acme","Principal":"alice","Action":"list","Pattern":"account:acme/**","Fields":{"seats":"5"}}`,
			want: []string{},
		},
		{
			name: "a list field matches by membership",
			args: `{"Account":"acme","Principal":"alice","Action":"list","Pattern":"account:acme/**","Fields":{"brands":"brand:Z"}}`,
			want: []string{"account:acme/dataset:a"},
		},
		{
			name: "an absent field never matches",
			args: `{"Account":"acme","Principal":"alice","Action":"list","Pattern":"account:acme/**","Fields":{"brands":"brand:Q"}}`,
			want: []string{},
		},
		{
			name: "predicates AND",
			args: `{"Account":"acme","Principal":"alice","Action":"list","Pattern":"account:acme/**","Fields":{"tier":"premium","seats":12}}`,
			want: []string{"account:acme/dataset:c"},
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
