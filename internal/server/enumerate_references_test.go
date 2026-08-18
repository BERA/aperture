package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/frankbardon/aperture/engine"
	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/identity"
	"github.com/frankbardon/aperture/internal/server"
	"github.com/frankbardon/aperture/internal/wire/rpc"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/provider"
	"github.com/frankbardon/aperture/scope"
	"github.com/frankbardon/aperture/service"
	"github.com/frankbardon/aperture/storage/memory"

	"github.com/twitchtv/twirp"
)

// E3-S3: the reference edge over Twirp/HTTP.
//
// The edge itself is three plain strings, so the interesting part of this
// surface is NOT the encoding — it is that E3-S2's disclosure boundary survives
// the trip. A boundary that turned an empty result into a 404, or a 404 into an
// empty list, would silently change what the system tells a caller about objects
// it never let them see, and no test in engine/ can catch that. So the
// empty-vs-NOT_FOUND split is asserted HERE, over a real JSON round trip:
//
//	unreadable holder                  -> 200, empty list, no error
//	absent holder INSIDE the account   -> NotFound + APERTURE_NOT_FOUND
//	absent holder OUTSIDE the account  -> 200, empty list, no error
//	present holder OUTSIDE the account -> 200, empty list, no error
//	non-member caller, absent holder   -> 200, empty list, no error

// refFixture is a live Twirp server over an engine whose scope lister, metadata
// source AND declared-reference source are one *provider.Registry — the
// production wiring — with membership enforcement on, so the non-member case is
// reachable.
type refFixture struct {
	t   *testing.T
	eng *engine.Engine
	cli rpc.ApertureService
}

// referenceDatasets is the holding side. dataset:x lists two live brands and one
// that no longer exists (the dangling case); dataset:secret exists but alice may
// not read it; dataset:z lives in ANOTHER account.
func referenceDatasets() []provider.Object {
	return []provider.Object{
		{
			ID: identity.MustParse("account:acme/dataset:x"),
			Metadata: provider.Metadata{"current_brands": []any{
				"account:acme/brand:1", "account:acme/brand:2", "account:acme/brand:gone",
			}},
		},
		{
			ID: identity.MustParse("account:acme/dataset:secret"),
			Metadata: provider.Metadata{"current_brands": []any{
				"account:acme/brand:1", "account:acme/brand:3",
			}},
		},
		{
			ID:       identity.MustParse("account:other/dataset:z"),
			Metadata: provider.Metadata{"current_brands": []any{"account:acme/brand:1"}},
		},
	}
}

func referenceBrands() []provider.Object {
	return []provider.Object{
		{ID: identity.MustParse("account:acme/brand:1"), Metadata: provider.Metadata{"region": "us"}},
		{ID: identity.MustParse("account:acme/brand:2"), Metadata: provider.Metadata{"region": "eu"}},
		{ID: identity.MustParse("account:acme/brand:3"), Metadata: provider.Metadata{"region": "us"}},
	}
}

func newRefFixture(t *testing.T) *refFixture {
	t.Helper()
	ctx := context.Background()
	store := memory.New()
	must(t, store.Setup(ctx))
	must(t, store.PutAccount(ctx, model.Account{ID: acct, Name: "Acme"}))
	for _, p := range []string{"alice", "mallory"} {
		must(t, store.PutPrincipal(ctx, model.Principal{
			ID: p, Kind: model.PrincipalUser, Identity: "user:" + p,
		}))
	}
	// alice is a member of acme; mallory is a member of nothing.
	must(t, store.PutMembership(ctx, model.Membership{PrincipalID: "alice", AccountID: acct}))
	for _, typ := range []string{"dataset", "brand"} {
		must(t, store.PutObjectType(ctx, model.ObjectType{Name: typ, Actions: []string{"read"}}))
		must(t, store.PutPermission(ctx, model.Permission{
			ID: "p-" + typ, ObjectType: typ, Action: "read", ScopeStrategy: scope.StrategyImplicit,
		}))
		must(t, store.PutGrant(ctx, model.Grant{
			ID: "g-" + typ, AccountID: acct,
			Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: "alice"},
			PermissionID: "p-" + typ, Object: "account:acme/**", Effect: model.EffectAllow,
		}))
	}
	// dataset:secret is carved out by a more specific deny: it EXISTS, and alice
	// may not read it. That is the fail-closed case.
	must(t, store.PutGrant(ctx, model.Grant{
		ID: "g-secret-deny", AccountID: acct,
		Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: "alice"},
		PermissionID: "p-dataset", Object: "account:acme/dataset:secret", Effect: model.EffectDeny,
	}))

	datasets, err := provider.NewStatic(referenceDatasets())
	if err != nil {
		t.Fatalf("NewStatic(datasets): %v", err)
	}
	brands, err := provider.NewStatic(referenceBrands())
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
		engine.WithMembershipEnforcement(),
	)
	svc := service.New(eng, service.WithProviders(reg), service.WithStorage(store))
	srv := httptest.NewServer(server.New(svc))
	t.Cleanup(srv.Close)

	return &refFixture{t: t, eng: eng, cli: rpc.NewApertureServiceJSONClient(srv.URL, http.DefaultClient)}
}

// wire enumerates the brands `principal` may read, restricted by edges, over
// HTTP — the shape a non-Go client sends.
func (f *refFixture) wire(principal string, edges ...*rpc.ReferenceEdge) ([]string, error) {
	f.t.Helper()
	res, err := f.cli.Enumerate(context.Background(), &rpc.EnumerateRequest{
		Account: acct, Principal: principal, Action: "read",
		Pattern: "account:acme/brand:*", References: edges,
	})
	if err != nil {
		return nil, err
	}
	return sorted(res.ObjectIds), nil
}

func (f *refFixture) mustWire(principal string, edges ...*rpc.ReferenceEdge) []string {
	f.t.Helper()
	ids, err := f.wire(principal, edges...)
	if err != nil {
		f.t.Fatalf("Enumerate over the wire: unexpected error: %v", err)
	}
	return ids
}

// edge builds a wire edge with the holder type left to the identity, which is
// the spelling the CLI sends.
func edge(holder, field string) *rpc.ReferenceEdge {
	return &rpc.ReferenceEdge{HolderId: holder, Field: field}
}

// TestEnumerateThroughAReferenceOverTheWire is the motivating question asked by
// a non-Go client: the three strings arrive, the dereference happens, and the
// answer is the engine's own.
func TestEnumerateThroughAReferenceOverTheWire(t *testing.T) {
	f := newRefFixture(t)

	all := f.mustWire("alice")
	want := []string{"account:acme/brand:1", "account:acme/brand:2", "account:acme/brand:3"}
	if !reflect.DeepEqual(all, want) {
		t.Fatalf("unrestricted enumerate = %v, want %v", all, want)
	}

	// brand:3 exists and alice may read it, but dataset:x does not list it;
	// brand:gone is listed but no longer served (dangling) and is skipped.
	got := f.mustWire("alice", edge("account:acme/dataset:x", "current_brands"))
	want = []string{"account:acme/brand:1", "account:acme/brand:2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("enumerate via dataset:x = %v, want %v", got, want)
	}

	// The wire answer is the engine's answer for the same edge, so the boundary
	// adds nothing and subtracts nothing.
	direct, err := f.eng.Enumerate(context.Background(), engine.EnumerateRequest{
		Account: acct, Principal: "alice", Action: "read",
		Pattern: "account:acme/brand:*",
		References: []engine.ReferenceEdge{
			{HolderID: "account:acme/dataset:x", Field: "current_brands"},
		},
	})
	if err != nil {
		t.Fatalf("engine.Enumerate: %v", err)
	}
	if !reflect.DeepEqual(sorted(direct), want) {
		t.Fatalf("engine = %v, wire = %v", sorted(direct), want)
	}
}

// The holder type is optional on the wire and must AGREE when supplied — the
// three-string spelling can never drift from the identity it carries.
func TestEnumerateReferenceHolderTypeMustAgree(t *testing.T) {
	f := newRefFixture(t)

	stated := f.mustWire("alice", &rpc.ReferenceEdge{
		HolderType: "dataset", HolderId: "account:acme/dataset:x", Field: "current_brands",
	})
	derived := f.mustWire("alice", edge("account:acme/dataset:x", "current_brands"))
	if !reflect.DeepEqual(stated, derived) {
		t.Fatalf("stated holder type = %v, derived = %v", stated, derived)
	}

	_, err := f.wire("alice", &rpc.ReferenceEdge{
		HolderType: "brand", HolderId: "account:acme/dataset:x", Field: "current_brands",
	})
	if err == nil {
		t.Fatal("a holder type disagreeing with the holder id = no error, want a coded rejection")
	}
	assertTwirpCode(t, err, aerr.APERTURE_INVALID_INPUT)
}

// Omitting the edges over the wire is EXACTLY no edges in Go: an existing client
// that never learns this field exists is unaffected, and an explicitly empty
// list means the same thing as an absent one.
func TestEnumerateReferencesOmittedIsIndistinguishableFromNone(t *testing.T) {
	f := newRefFixture(t)

	omitted := f.mustWire("alice")
	empty := f.mustWire("alice", []*rpc.ReferenceEdge{}...)
	if !reflect.DeepEqual(omitted, empty) {
		t.Fatalf("omitted = %v, empty list = %v", omitted, empty)
	}
	direct, err := f.eng.Enumerate(context.Background(), engine.EnumerateRequest{
		Account: acct, Principal: "alice", Action: "read", Pattern: "account:acme/brand:*",
	})
	if err != nil {
		t.Fatalf("engine.Enumerate: %v", err)
	}
	if !reflect.DeepEqual(sorted(direct), omitted) {
		t.Fatalf("engine with nil References = %v, wire with none = %v", sorted(direct), omitted)
	}
}

// A holder the caller may not read is an EMPTY 200, never a 403 and never a 404:
// "you may not see dataset:secret" and "dataset:secret lists nothing you may
// see" have to be indistinguishable on the wire, or the edge is an oracle.
func TestEnumerateReferenceUnreadableHolderIsEmptyNotAnError(t *testing.T) {
	f := newRefFixture(t)

	got, err := f.wire("alice", edge("account:acme/dataset:secret", "current_brands"))
	if err != nil {
		t.Fatalf("unreadable holder = error %v, want an empty result", err)
	}
	if len(got) != 0 {
		t.Fatalf("unreadable holder = %v, want []", got)
	}
}

// An ABSENT holder inside the request's account is APERTURE_NOT_FOUND (a 404 on
// the wire, with the code in the twirp meta) — the ergonomics a typo deserves,
// confined to a caller already inside the account.
func TestEnumerateReferenceAbsentInAccountHolderIsNotFound(t *testing.T) {
	f := newRefFixture(t)

	_, err := f.wire("alice", edge("account:acme/dataset:nope", "current_brands"))
	if err == nil {
		t.Fatal("absent in-account holder = no error, want NOT_FOUND")
	}
	assertTwirpCode(t, err, aerr.APERTURE_NOT_FOUND)
	te, ok := err.(twirp.Error)
	if !ok || te.Code() != twirp.NotFound {
		t.Fatalf("absent in-account holder = twirp code %v, want NotFound", err)
	}
}

// The disclosure boundary. A holder OUTSIDE the request's account is empty
// whether it exists or not, so the surface never tells a caller in one account
// what does or does not exist in another — and the two out-of-account answers
// are identical to each other.
func TestEnumerateReferenceOutOfAccountHolderIsAlwaysEmpty(t *testing.T) {
	f := newRefFixture(t)

	present, err := f.wire("alice", edge("account:other/dataset:z", "current_brands"))
	if err != nil {
		t.Fatalf("present out-of-account holder = error %v, want an empty result", err)
	}
	absent, err := f.wire("alice", edge("account:other/dataset:missing", "current_brands"))
	if err != nil {
		t.Fatalf("absent out-of-account holder = error %v, want an empty result", err)
	}
	if len(present) != 0 || len(absent) != 0 {
		t.Fatalf("out-of-account holders = present %v / absent %v, want [] and []", present, absent)
	}
}

// A non-member learns nothing either: membership is decided before the holder is
// ever looked up, so even a holder that IS absent inside the account reports
// empty rather than NOT_FOUND.
func TestEnumerateReferenceNonMemberNeverSeesNotFound(t *testing.T) {
	f := newRefFixture(t)

	for _, holder := range []string{
		"account:acme/dataset:x",      // present and readable by a member
		"account:acme/dataset:nope",   // absent, in-account: NOT_FOUND for a member
		"account:acme/dataset:secret", // present, unreadable
	} {
		got, err := f.wire("mallory", edge(holder, "current_brands"))
		if err != nil {
			t.Fatalf("non-member via %s = error %v, want an empty result", holder, err)
		}
		if len(got) != 0 {
			t.Fatalf("non-member via %s = %v, want []", holder, got)
		}
	}
}

// A wiring fault is loud on the wire, not empty: an undeclared field would
// otherwise read as "no access" to a client that cannot see the deployment.
func TestEnumerateReferenceUndeclaredFieldIsACodedError(t *testing.T) {
	f := newRefFixture(t)

	_, err := f.wire("alice", edge("account:acme/dataset:x", "not_a_reference"))
	if err == nil {
		t.Fatal("undeclared reference field = no error, want a coded failure")
	}
	assertTwirpCode(t, err, aerr.APERTURE_PROVIDER_REFERENCE_INVALID)
}

// ...and it is loud as a 400, not a 500. An edge naming an undeclared field is
// the caller's own input — the same class of mistake as a --via with no '.' —
// and retrying it unchanged can never succeed, so the one status class a client
// is entitled to retry and an alert rule is entitled to page on is the wrong
// answer. The Aperture code rides in the twirp meta either way; this pins the
// HTTP status a client that only reads the status sees.
func TestEnumerateReferenceUndeclaredFieldIsAnInvalidArgumentNotAnInternal(t *testing.T) {
	f := newRefFixture(t)

	_, err := f.wire("alice", edge("account:acme/dataset:x", "not_a_reference"))
	te, ok := err.(twirp.Error)
	if !ok {
		t.Fatalf("undeclared reference field = %v, want a twirp.Error", err)
	}
	if te.Code() != twirp.InvalidArgument {
		t.Fatalf("undeclared reference field = twirp code %q (HTTP %d), want invalid_argument (400)",
			te.Code(), twirp.ServerHTTPStatusFromErrorCode(te.Code()))
	}
}

// The batch variant carries the edges PER QUERY, and each item keeps its own
// answer: one query's NOT_FOUND never becomes another's, and a fail-closed
// empty result is reported as an empty list rather than an error.
func TestEnumerateBatchCarriesReferenceEdgesPerQuery(t *testing.T) {
	f := newRefFixture(t)

	query := func(edges ...*rpc.ReferenceEdge) *rpc.EnumerateRequest {
		return &rpc.EnumerateRequest{
			Account: acct, Principal: "alice", Action: "read",
			Pattern: "account:acme/brand:*", References: edges,
		}
	}
	res, err := f.cli.EnumerateBatch(context.Background(), &rpc.EnumerateBatchRequest{
		Queries: []*rpc.EnumerateRequest{
			query(edge("account:acme/dataset:x", "current_brands")),
			query(edge("account:acme/dataset:secret", "current_brands")),
			query(edge("account:acme/dataset:nope", "current_brands")),
			query(),
		},
	})
	if err != nil {
		t.Fatalf("EnumerateBatch: %v", err)
	}
	if len(res.Results) != 4 {
		t.Fatalf("batch returned %d results, want 4", len(res.Results))
	}

	if got, want := sorted(res.Results[0].ObjectIds), []string{"account:acme/brand:1", "account:acme/brand:2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("batch[0] (via dataset:x) = %v, want %v", got, want)
	}
	if res.Results[0].ErrorCode != "" {
		t.Fatalf("batch[0] carried error %q", res.Results[0].ErrorCode)
	}

	if len(res.Results[1].ObjectIds) != 0 || res.Results[1].ErrorCode != "" {
		t.Fatalf("batch[1] (unreadable holder) = %v / %q, want [] and no error",
			res.Results[1].ObjectIds, res.Results[1].ErrorCode)
	}

	if res.Results[2].ErrorCode != string(aerr.APERTURE_NOT_FOUND) {
		t.Fatalf("batch[2] (absent in-account holder) = %q, want %s",
			res.Results[2].ErrorCode, aerr.APERTURE_NOT_FOUND)
	}
	if len(res.Results[2].ObjectIds) != 0 {
		t.Fatalf("batch[2] returned objects alongside its error: %v", res.Results[2].ObjectIds)
	}

	want := []string{"account:acme/brand:1", "account:acme/brand:2", "account:acme/brand:3"}
	if got := sorted(res.Results[3].ObjectIds); !reflect.DeepEqual(got, want) {
		t.Fatalf("batch[3] (no edges) = %v, want %v", got, want)
	}
}
