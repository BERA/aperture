package service

import (
	"context"
	"reflect"
	"testing"

	"github.com/frankbardon/aperture/engine"
	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/identity"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/provider"
	"github.com/frankbardon/aperture/scope"
	"github.com/frankbardon/aperture/storage/memory"
)

// The facade's job for EnumerateQuery.References is to CARRY the edges and
// nothing else — no identity parsing, no holder-type inference, no declaration
// check. Each of those is a decision with a disclosure consequence, and a facade
// that made one separately would be a second place for the answer to differ.
// These tests pin it by comparing the facade's answer to the engine's own for
// the same edges, including the two fail-closed cases.

// newRefSvc returns a facade and the engine underneath it, sharing one registry
// as scope lister, metadata source and declared-reference source.
func newRefSvc(t *testing.T) (*Service, *engine.Engine) {
	t.Helper()
	ctx := context.Background()
	store := memory.New()
	if err := store.Setup(ctx); err != nil {
		t.Fatalf("setup: %v", err)
	}
	mustPut(t, store.PutPrincipal(ctx, model.Principal{ID: "alice", Kind: model.PrincipalUser, Identity: "user:alice"}))
	for _, typ := range []string{"dataset", "brand"} {
		mustPut(t, store.PutObjectType(ctx, model.ObjectType{Name: typ, Actions: []string{"read"}}))
		mustPut(t, store.PutPermission(ctx, model.Permission{
			ID: "p-" + typ, ObjectType: typ, Action: "read", ScopeStrategy: scope.StrategyImplicit,
		}))
		mustPut(t, store.PutGrant(ctx, allowGrant("g-"+typ, "p-"+typ, "account:acme/**")))
	}
	// dataset:secret exists and is carved out by a more specific deny.
	mustPut(t, store.PutGrant(ctx, model.Grant{
		ID: "g-deny", AccountID: acct, Subject: model.Subject{Kind: model.SubjectPrincipal, ID: "alice"},
		PermissionID: "p-dataset", Object: "account:acme/dataset:secret", Effect: model.EffectDeny,
	}))

	datasets, err := provider.NewStatic([]provider.Object{
		{
			ID: identity.MustParse("account:acme/dataset:x"),
			Metadata: provider.Metadata{"current_brands": []any{
				"account:acme/brand:1", "account:acme/brand:2",
			}},
		},
		{
			ID:       identity.MustParse("account:acme/dataset:secret"),
			Metadata: provider.Metadata{"current_brands": []any{"account:acme/brand:3"}},
		},
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
	reg.MustRegister("dataset", datasets)
	reg.MustRegister("brand", brands)
	reg.MustDeclareReference("dataset", "current_brands", "brand")

	eng := engine.New(store,
		engine.WithScopeResolution(scope.DefaultRegistry(), engine.ScopeDeps{Lister: reg}),
		engine.WithMetadata(reg),
		engine.WithReferences(reg),
	)
	return New(eng, WithProviders(reg)), eng
}

// TestService_EnumerateReferencesPassThrough: for every edge list, the facade's
// answer is the engine's answer for the same edges — single and batched.
func TestService_EnumerateReferencesPassThrough(t *testing.T) {
	svc, eng := newRefSvc(t)
	ctx := context.Background()

	cases := [][]ReferenceEdge{
		nil,
		{},
		{{HolderID: "account:acme/dataset:x", Field: "current_brands"}},
		// The holder type stated explicitly, and it agrees.
		{{HolderType: "dataset", HolderID: "account:acme/dataset:x", Field: "current_brands"}},
		// Two edges AND; nothing is in both.
		{
			{HolderID: "account:acme/dataset:x", Field: "current_brands"},
			{HolderID: "account:acme/dataset:secret", Field: "current_brands"},
		},
		// Fail-closed: an unreadable holder is an empty result, not an error.
		{{HolderID: "account:acme/dataset:secret", Field: "current_brands"}},
		// Fail-closed: a holder outside the request's account is empty too.
		{{HolderID: "account:other/dataset:z", Field: "current_brands"}},
	}
	for _, edges := range cases {
		q := EnumerateQuery{
			Account: acct, Principal: "alice", Action: "read",
			Pattern: "account:acme/brand:*", References: edges,
		}
		want, err := eng.Enumerate(ctx, engine.EnumerateRequest{
			Account: acct, Principal: "alice", Action: "read",
			Pattern: "account:acme/brand:*", References: q.references(),
		})
		if err != nil {
			t.Fatalf("engine.Enumerate(%v): %v", edges, err)
		}
		got, err := svc.Enumerate(ctx, q)
		if err != nil {
			t.Fatalf("svc.Enumerate(%v): %v", edges, err)
		}
		if !reflect.DeepEqual(sortedIDs(got), sortedIDs(want)) {
			t.Fatalf("References=%v: facade = %v, engine = %v", edges, got, want)
		}

		batched := svc.EnumerateBatch(ctx, []EnumerateQuery{q})
		if len(batched) != 1 || batched[0].Err != nil {
			t.Fatalf("References=%v: batch = %v", edges, batched)
		}
		if !reflect.DeepEqual(sortedIDs(batched[0].Result), sortedIDs(want)) {
			t.Fatalf("References=%v: batch = %v, engine = %v", edges, batched[0].Result, want)
		}
	}
}

// An omitted edge list is a nil engine slice, so "no edges" costs an existing
// caller exactly nothing — the request the engine sees is the one it saw before
// the field existed.
func TestService_EnumerateReferencesOmittedIsNil(t *testing.T) {
	for _, edges := range [][]ReferenceEdge{nil, {}} {
		q := EnumerateQuery{Account: acct, Principal: "alice", Action: "read", Pattern: "account:acme/**", References: edges}
		if got := q.request().References; got != nil {
			t.Fatalf("References=%v produced engine References %#v, want nil", edges, got)
		}
	}
}

// The three strings reach the engine verbatim, in order — the facade neither
// fills in the holder type it could have derived nor reorders the edges.
func TestService_EnumerateReferencesAreCarriedVerbatim(t *testing.T) {
	edges := []ReferenceEdge{
		{HolderID: "account:acme/dataset:x", Field: "current_brands"},
		{HolderType: "dataset", HolderID: "account:acme/dataset:y", Field: "archived_brands"},
	}
	want := []engine.ReferenceEdge{
		{HolderID: "account:acme/dataset:x", Field: "current_brands"},
		{HolderType: "dataset", HolderID: "account:acme/dataset:y", Field: "archived_brands"},
	}
	q := EnumerateQuery{Account: acct, Principal: "alice", Action: "read", Pattern: "**", References: edges}
	if got := q.request().References; !reflect.DeepEqual(got, want) {
		t.Fatalf("engine References = %#v, want %#v", got, want)
	}
	// And the caller's own slice is untouched.
	if edges[0].HolderType != "" {
		t.Fatalf("the facade rewrote the caller's edge: %#v", edges[0])
	}
}

// A holder type that disagrees with the holder id is rejected once, in the
// engine, with the code every surface reports.
func TestService_EnumerateReferencesHolderTypeDisagreement(t *testing.T) {
	svc, _ := newRefSvc(t)

	_, err := svc.Enumerate(context.Background(), EnumerateQuery{
		Account: acct, Principal: "alice", Action: "read", Pattern: "account:acme/brand:*",
		References: []ReferenceEdge{
			{HolderType: "brand", HolderID: "account:acme/dataset:x", Field: "current_brands"},
		},
	})
	if aerr.CodeOf(err) != aerr.APERTURE_INVALID_INPUT {
		t.Fatalf("disagreeing holder type = %v (code %s), want APERTURE_INVALID_INPUT", err, aerr.CodeOf(err))
	}
}

// An absent holder INSIDE the account is NOT_FOUND through the facade too: the
// facade must not fold a coded error into an empty list, which would erase the
// distinction the engine drew.
func TestService_EnumerateReferencesAbsentInAccountHolder(t *testing.T) {
	svc, _ := newRefSvc(t)

	ids, err := svc.Enumerate(context.Background(), EnumerateQuery{
		Account: acct, Principal: "alice", Action: "read", Pattern: "account:acme/brand:*",
		References: []ReferenceEdge{{HolderID: "account:acme/dataset:nope", Field: "current_brands"}},
	})
	if aerr.CodeOf(err) != aerr.APERTURE_NOT_FOUND {
		t.Fatalf("absent in-account holder = %v (code %s), want APERTURE_NOT_FOUND", err, aerr.CodeOf(err))
	}
	if len(ids) != 0 {
		t.Fatalf("a failed enumeration returned ids: %v", ids)
	}
}

// Enumerating through a reference with no reference source wired is a coded
// misconfiguration through the facade — never an empty result that reads as
// "no access".
func TestService_EnumerateReferencesWithoutAReferenceSource(t *testing.T) {
	svc := newSvc(t, []string{"account:acme/document:1"},
		allowGrant("g-all", "p-impl", "account:acme/**"))

	_, err := svc.Enumerate(context.Background(), EnumerateQuery{
		Account: acct, Principal: "alice", Action: "read", Pattern: "account:acme/**",
		References: []ReferenceEdge{{HolderID: "account:acme/dataset:x", Field: "current_brands"}},
	})
	if err == nil {
		t.Fatal("enumerate through a reference with no reference source = nil error, want a coded failure")
	}
}
