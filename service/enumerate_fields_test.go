package service

import (
	"context"
	"reflect"
	"sort"
	"testing"

	"github.com/frankbardon/aperture/engine"
	"github.com/frankbardon/aperture/identity"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/provider"
	"github.com/frankbardon/aperture/scope"
	"github.com/frankbardon/aperture/storage/memory"
)

// The facade's job for EnumerateQuery.Fields is to carry the map and nothing
// else. These tests pin that: for every predicate, the facade's answer is the
// engine's own answer for the same Go map, and the caller's map comes back
// untouched.

// newFilterSvc returns a facade and the engine underneath it, sharing one
// registry as both scope lister and metadata source.
func newFilterSvc(t *testing.T) (*Service, *engine.Engine) {
	t.Helper()
	ctx := context.Background()
	store := memory.New()
	if err := store.Setup(ctx); err != nil {
		t.Fatalf("setup: %v", err)
	}
	mustPut(t, store.PutAccount(ctx, model.Account{ID: acct, Name: acct}))
	mustPut(t, store.PutObjectType(ctx, model.ObjectType{Name: "document", Actions: []string{"read"}}))
	mustPut(t, store.PutPermission(ctx, model.Permission{
		ID: "p-impl", ObjectType: "document", Action: "read", ScopeStrategy: scope.StrategyImplicit,
	}))
	mustPut(t, store.PutPrincipal(ctx, model.Principal{ID: "alice", Kind: model.PrincipalUser, Identity: "user:alice"}))
	mustPut(t, store.PutGrant(ctx, allowGrant("g-all", "p-impl", "account:acme/**")))

	sp, err := provider.NewStatic([]provider.Object{
		{
			ID: identity.MustParse("account:acme/document:1"),
			Metadata: provider.Metadata{
				"tier": "premium", "seats": int64(5), "brands": []any{"brand:X", "brand:Y"},
			},
		},
		{
			ID: identity.MustParse("account:acme/document:2"),
			Metadata: provider.Metadata{
				"tier": "basic", "seats": int64(9), "brands": []any{"brand:Y"},
			},
		},
	})
	if err != nil {
		t.Fatalf("NewStatic: %v", err)
	}
	reg := provider.NewRegistry()
	reg.MustRegister("document", sp)

	eng := engine.New(store,
		engine.WithScopeResolution(scope.DefaultRegistry(), engine.ScopeDeps{Lister: reg}),
		engine.WithMetadata(reg),
	)
	return New(eng, WithProviders(reg)), eng
}

func sortedIDs(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// The facade's Enumerate and the engine's agree on every predicate, single and
// batched: the query type carries Fields, request() forwards it, and nothing in
// between parses or coerces a value.
func TestService_EnumerateFieldsPassThrough(t *testing.T) {
	svc, eng := newFilterSvc(t)
	ctx := context.Background()

	cases := []map[string]any{
		nil,
		{},
		{"tier": "premium"},
		{"seats": int64(5)},
		{"seats": float64(5)},
		{"seats": "5"}, // a string never matches a number
		{"brands": "brand:Y"},
		{"brands": []any{"brand:Y"}},
		{"tier": "premium", "seats": int64(9)},
		{"missing": "x"},
	}
	for _, fields := range cases {
		q := EnumerateQuery{
			Account: acct, Principal: "alice", Action: "read",
			Pattern: "account:acme/**", Fields: fields,
		}
		want, err := eng.Enumerate(ctx, engine.EnumerateRequest{
			Account: acct, Principal: "alice", Action: "read",
			Pattern: "account:acme/**", Fields: fields,
		})
		if err != nil {
			t.Fatalf("engine.Enumerate(%v): %v", fields, err)
		}
		got, err := svc.Enumerate(ctx, q)
		if err != nil {
			t.Fatalf("svc.Enumerate(%v): %v", fields, err)
		}
		if !reflect.DeepEqual(sortedIDs(got), sortedIDs(want)) {
			t.Fatalf("Fields=%v: facade = %v, engine = %v", fields, got, want)
		}

		batched := svc.EnumerateBatch(ctx, []EnumerateQuery{q})
		if len(batched) != 1 || batched[0].Err != nil {
			t.Fatalf("Fields=%v: batch = %v", fields, batched)
		}
		if !reflect.DeepEqual(sortedIDs(batched[0].Result), sortedIDs(want)) {
			t.Fatalf("Fields=%v: batch = %v, engine = %v", fields, batched[0].Result, want)
		}
	}
}

// The facade must not rewrite the caller's map — it is the caller's, and a
// predicate silently normalised on one call would apply to the next.
func TestService_EnumerateFieldsDoesNotMutateTheCallersMap(t *testing.T) {
	svc, _ := newFilterSvc(t)
	fields := map[string]any{"tier": "premium", "seats": int64(5), "brands": []any{"brand:X"}}
	before := map[string]any{"tier": "premium", "seats": int64(5), "brands": []any{"brand:X"}}

	if _, err := svc.Enumerate(context.Background(), EnumerateQuery{
		Account: acct, Principal: "alice", Action: "read",
		Pattern: "account:acme/**", Fields: fields,
	}); err != nil {
		t.Fatalf("Enumerate: %v", err)
	}
	if !reflect.DeepEqual(fields, before) {
		t.Fatalf("the caller's Fields map was rewritten: %#v, want %#v", fields, before)
	}
}

// Filtering with no metadata source wired is a coded misconfiguration through
// the facade too — never an empty result that reads as "no access".
func TestService_EnumerateFieldsWithoutAMetadataSource(t *testing.T) {
	svc := newSvc(t, []string{"account:acme/document:1"},
		allowGrant("g-all", "p-impl", "account:acme/**"))

	_, err := svc.Enumerate(context.Background(), EnumerateQuery{
		Account: acct, Principal: "alice", Action: "read",
		Pattern: "account:acme/**", Fields: map[string]any{"tier": "premium"},
	})
	if err == nil {
		t.Fatal("filtered Enumerate with no metadata source = nil error, want a coded failure")
	}
}
