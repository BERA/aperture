package engine

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/provider"
	"github.com/frankbardon/aperture/rules"
	"github.com/frankbardon/aperture/scope"
	"github.com/frankbardon/aperture/storage/memory"
)

// E3-S2: enumerating THROUGH a declared reference — "which brands belong to
// dataset X?".
//
// The fixture is the epic's own catalogue: datasets hold brands (the declared,
// holding-side reference), campaigns hold brands too (so two edges can AND), and
// brands hold related brands (so "exactly one hop" has something to fail on if it
// ever stopped being true). One dataset points at a brand that no longer exists,
// which is the dangling case; one dataset lives in ANOTHER account, which is the
// disclosure boundary.

func referenceDatasets() map[string]provider.Metadata {
	return map[string]provider.Metadata{
		"account:acme/dataset:x": {
			"tier": "premium",
			// brand:gone is referenced but no longer served — the dangling case.
			"current_brands": []any{
				"account:acme/brand:1",
				"account:acme/brand:2",
				"account:acme/brand:gone",
			},
		},
		"account:acme/dataset:y": {
			"tier":           "basic",
			"current_brands": []any{"account:acme/brand:2"},
		},
		// A dataset in ANOTHER account, so "present but out of account" and
		// "absent and out of account" are both exercisable.
		"account:other/dataset:z": {
			"tier":           "premium",
			"current_brands": []any{"account:other/brand:9"},
		},
		// A dataset with the field entirely absent: an absent field resolves to
		// nothing, exactly as a null value does.
		"account:acme/dataset:empty": {"tier": "basic"},
	}
}

func referenceBrands() map[string]provider.Metadata {
	return map[string]provider.Metadata{
		"account:acme/brand:1": {
			"region": "us",
			// The second hop that must never be taken.
			"related_brands": []any{"account:acme/brand:3"},
		},
		"account:acme/brand:2": {"region": "eu"},
		"account:acme/brand:3": {"region": "us"},
	}
}

func referenceCampaigns() map[string]provider.Metadata {
	return map[string]provider.Metadata{
		"account:acme/campaign:spring": {
			"brands": []any{"account:acme/brand:2", "account:acme/brand:3"},
		},
	}
}

// refFixture wires an engine whose scope lister, metadata source AND declared-
// reference source are one *provider.Registry — the production wiring.
type refFixture struct {
	t     *testing.T
	eng   *Engine
	reg   *provider.Registry
	store *memory.Store
	logs  *bytes.Buffer
}

func newRefFixture(t *testing.T, opts ...Option) *refFixture {
	t.Helper()
	ctx := context.Background()
	store := memory.New()
	if err := store.Setup(ctx); err != nil {
		t.Fatalf("setup: %v", err)
	}
	mustSeed(t, store.PutAccount(ctx, model.Account{ID: acctAcme, Name: acctAcme}))
	mustSeed(t, store.PutAccount(ctx, model.Account{ID: acctOther, Name: acctOther}))
	for _, typ := range []string{"dataset", "brand", "campaign"} {
		mustSeed(t, store.PutObjectType(ctx, model.ObjectType{Name: typ, Actions: []string{"read"}}))
		mustSeed(t, store.PutPermission(ctx, model.Permission{
			ID: "p-" + typ, ObjectType: typ, Action: "read", ScopeStrategy: scope.StrategyImplicit,
		}))
	}
	// alice is a member of acme and may read everything in it; bob is a member
	// with no grants of his own (the impersonation operator); mallory is a member
	// of nothing.
	for _, p := range []string{"alice", "bob", "mallory"} {
		mustSeed(t, store.PutPrincipal(ctx, model.Principal{
			ID: p, Kind: model.PrincipalUser, Identity: "user:" + p,
		}))
	}
	mustSeed(t, store.PutMembership(ctx, model.Membership{PrincipalID: "alice", AccountID: acctAcme}))
	mustSeed(t, store.PutMembership(ctx, model.Membership{PrincipalID: "bob", AccountID: acctAcme}))
	for _, typ := range []string{"dataset", "brand", "campaign"} {
		mustSeed(t, store.PutGrant(ctx, model.Grant{
			ID: "g-" + typ, AccountID: acctAcme,
			Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: "alice"},
			PermissionID: "p-" + typ, Object: "account:acme/**", Effect: model.EffectAllow,
		}))
	}

	reg := provider.NewRegistry()
	reg.MustRegister("dataset", metaProvider{md: referenceDatasets()})
	reg.MustRegister("brand", metaProvider{md: referenceBrands()})
	reg.MustRegister("campaign", metaProvider{md: referenceCampaigns()})
	reg.MustDeclareReference("dataset", "current_brands", "brand")
	reg.MustDeclareReference("campaign", "brands", "brand")
	reg.MustDeclareReference("brand", "related_brands", "brand")

	logs := &bytes.Buffer{}
	base := []Option{
		WithScopeResolution(scope.DefaultRegistry(), ScopeDeps{Lister: reg}),
		WithMetadata(reg),
		WithReferences(reg),
		WithMembershipEnforcement(),
		WithLogger(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn}))),
	}
	return &refFixture{t: t, eng: New(store, append(base, opts...)...), reg: reg, store: store, logs: logs}
}

// brands enumerates the brands alice may read, restricted by edges.
func (f *refFixture) brands(ctx context.Context, edges ...ReferenceEdge) ([]string, error) {
	f.t.Helper()
	return f.eng.Enumerate(ctx, EnumerateRequest{
		Account: acctAcme, Principal: "alice", Action: "read",
		Pattern: "account:acme/brand:*", References: edges,
	})
}

// inDatasetX is the motivating edge: the brands dataset x lists.
func inDatasetX() ReferenceEdge {
	return ReferenceEdge{HolderType: "dataset", HolderID: "account:acme/dataset:x", Field: "current_brands"}
}

func wantIDs(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("ids = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ids = %v, want %v", got, want)
		}
	}
}

// TestEnumerateThroughAReference is the motivating question: the enumeration is
// restricted to the identities the holder's declared field names, and to nothing
// else — brand:3 exists and alice may read it, but dataset x does not list it.
func TestEnumerateThroughAReference(t *testing.T) {
	f := newRefFixture(t)
	ctx := context.Background()

	all, err := f.brands(ctx)
	if err != nil {
		t.Fatalf("Enumerate(no edges): %v", err)
	}
	wantIDs(t, all, "account:acme/brand:1", "account:acme/brand:2", "account:acme/brand:3")

	got, err := f.brands(ctx, inDatasetX())
	if err != nil {
		t.Fatalf("Enumerate(edge): %v", err)
	}
	wantIDs(t, got, "account:acme/brand:1", "account:acme/brand:2")
}

// TestReferenceEdgeHolderTypeIsOptionalButChecked pins the three-string
// {holder_type, holder_id, field} spelling: the type may be omitted (it is
// derivable) but it may never DISAGREE, so a non-Go surface cannot smuggle in an
// edge whose declared type and identity point at different things.
func TestReferenceEdgeHolderTypeIsOptionalButChecked(t *testing.T) {
	f := newRefFixture(t)
	ctx := context.Background()

	derived, err := f.brands(ctx, ReferenceEdge{HolderID: "account:acme/dataset:x", Field: "current_brands"})
	if err != nil {
		t.Fatalf("Enumerate(derived type): %v", err)
	}
	wantIDs(t, derived, "account:acme/brand:1", "account:acme/brand:2")

	_, err = f.brands(ctx, ReferenceEdge{
		HolderType: "campaign", HolderID: "account:acme/dataset:x", Field: "current_brands",
	})
	if code := aerr.CodeOf(err); code != aerr.APERTURE_INVALID_INPUT {
		t.Fatalf("code = %q, want APERTURE_INVALID_INPUT (err: %v)", code, err)
	}
}

// TestMultipleReferenceEdgesAND: brands in dataset x AND in campaign spring. The
// intersection is brand:2 — brand:1 is only in the dataset, brand:3 only in the
// campaign.
func TestMultipleReferenceEdgesAND(t *testing.T) {
	f := newRefFixture(t)
	got, err := f.brands(context.Background(), inDatasetX(),
		ReferenceEdge{HolderType: "campaign", HolderID: "account:acme/campaign:spring", Field: "brands"})
	if err != nil {
		t.Fatalf("Enumerate(two edges): %v", err)
	}
	wantIDs(t, got, "account:acme/brand:2")
}

// TestReferenceEdgeComposesWithFields: an edge and a metadata predicate are both
// applied, and both only ever subtract. dataset x holds brand:1 (us) and brand:2
// (eu); asking for the eu ones leaves brand:2.
func TestReferenceEdgeComposesWithFields(t *testing.T) {
	f := newRefFixture(t)
	got, err := f.eng.Enumerate(context.Background(), EnumerateRequest{
		Account: acctAcme, Principal: "alice", Action: "read",
		Pattern:    "account:acme/brand:*",
		References: []ReferenceEdge{inDatasetX()},
		Fields:     map[string]any{"region": "eu"},
	})
	if err != nil {
		t.Fatalf("Enumerate(edge+fields): %v", err)
	}
	wantIDs(t, got, "account:acme/brand:2")
}

// TestReferenceEdgeTakesExactlyOneHop. brand:1 declares related_brands -> brand
// and lists brand:3. Dereferencing dataset x must not follow it: transitive
// traversal is a different feature with a different cost model.
func TestReferenceEdgeTakesExactlyOneHop(t *testing.T) {
	f := newRefFixture(t)
	got, err := f.brands(context.Background(), inDatasetX())
	if err != nil {
		t.Fatalf("Enumerate(edge): %v", err)
	}
	for _, id := range got {
		if id == "account:acme/brand:3" {
			t.Fatalf("ids = %v: brand:3 is only reachable through brand:1.related_brands, so the edge took two hops", got)
		}
	}
}

// TestReferenceEdgeStillPassesCheck: the restriction can only ever subtract from
// the allowed set. A deny on brand:1 removes it even though dataset x lists it,
// so deny-overrides and specificity are untouched.
func TestReferenceEdgeStillPassesCheck(t *testing.T) {
	f := newRefFixture(t)
	ctx := context.Background()
	store := f.store
	mustSeed(t, store.PutGrant(ctx, model.Grant{
		ID: "g-deny-brand-1", AccountID: acctAcme,
		Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: "alice"},
		PermissionID: "p-brand", Object: "account:acme/brand:1", Effect: model.EffectDeny,
	}))
	got, err := f.brands(ctx, inDatasetX())
	if err != nil {
		t.Fatalf("Enumerate(edge): %v", err)
	}
	wantIDs(t, got, "account:acme/brand:2")
}

// TestReferenceEdgeFiltersBeforeLimit: E1-S1's ordering holds for a reference-
// restricted result too. With limit 1 the answer is the FIRST brand in the
// dataset, not "the brands among the first candidate".
func TestReferenceEdgeFiltersBeforeLimit(t *testing.T) {
	f := newRefFixture(t)
	ctx := context.Background()
	// Unrestricted, brand:1 sorts first, so a naive "truncate then restrict"
	// would still find it. Restrict to dataset y instead, whose only brand is
	// brand:2 — the SECOND candidate in canonical order.
	got, err := f.eng.Enumerate(ctx, EnumerateRequest{
		Account: acctAcme, Principal: "alice", Action: "read",
		Pattern: "account:acme/brand:*", Limit: 1,
		References: []ReferenceEdge{{
			HolderType: "dataset", HolderID: "account:acme/dataset:y", Field: "current_brands",
		}},
	})
	if err != nil {
		t.Fatalf("Enumerate(edge, limit 1): %v", err)
	}
	wantIDs(t, got, "account:acme/brand:2")
}

// TestReferenceEdgeAbsentFieldRestrictsToNothing: a declared field an object does
// not carry resolves to no identities, so the enumeration is restricted to the
// empty set — NOT left unrestricted. An empty restriction and no restriction at
// all are opposite answers and must not be confused.
func TestReferenceEdgeAbsentFieldRestrictsToNothing(t *testing.T) {
	f := newRefFixture(t)
	got, err := f.brands(context.Background(), ReferenceEdge{
		HolderType: "dataset", HolderID: "account:acme/dataset:empty", Field: "current_brands",
	})
	if err != nil {
		t.Fatalf("Enumerate(edge): %v", err)
	}
	wantIDs(t, got)
}

// --- the security semantics -------------------------------------------------

// TestUnreadableHolderYieldsEmptyNotError is the fail-closed rule: "you may not
// see dataset x" and "dataset x contains nothing you may see" have to be
// indistinguishable, or the edge is an oracle for objects the caller was never
// allowed to know about.
func TestUnreadableHolderYieldsEmptyNotError(t *testing.T) {
	f := newRefFixture(t)
	ctx := context.Background()
	mustSeed(t, f.store.PutGrant(ctx, model.Grant{
		ID: "g-deny-dataset-x", AccountID: acctAcme,
		Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: "alice"},
		PermissionID: "p-dataset", Object: "account:acme/dataset:x", Effect: model.EffectDeny,
	}))
	got, err := f.brands(ctx, inDatasetX())
	if err != nil {
		t.Fatalf("Enumerate(unreadable holder) returned an error: %v", err)
	}
	wantIDs(t, got)

	// And it is byte-identical to the answer for a holder that is readable but
	// lists nothing visible.
	empty, err := f.brands(ctx, ReferenceEdge{
		HolderType: "dataset", HolderID: "account:acme/dataset:empty", Field: "current_brands",
	})
	if err != nil {
		t.Fatalf("Enumerate(empty holder): %v", err)
	}
	if len(got) != len(empty) {
		t.Fatalf("denied holder = %v, empty holder = %v: the two must be indistinguishable", got, empty)
	}
}

// TestNonMemberYieldsEmptyThroughAReference: membership is decided at the door,
// before any edge is dereferenced, so a non-member learns nothing at all — not
// even whether the holder exists.
func TestNonMemberYieldsEmptyThroughAReference(t *testing.T) {
	f := newRefFixture(t)
	for _, holder := range []string{"account:acme/dataset:x", "account:acme/dataset:nope"} {
		got, err := f.eng.Enumerate(context.Background(), EnumerateRequest{
			Account: acctAcme, Principal: "mallory", Action: "read",
			Pattern: "account:acme/brand:*",
			References: []ReferenceEdge{{
				HolderType: "dataset", HolderID: holder, Field: "current_brands",
			}},
		})
		if err != nil {
			t.Fatalf("Enumerate(non-member, %s) returned an error: %v", holder, err)
		}
		wantIDs(t, got)
	}
}

// TestAbsentInAccountHolderIsNotFound: the bounded ergonomic disclosure. A member
// of the account who typos an id is told so, rather than staring at an empty list.
func TestAbsentInAccountHolderIsNotFound(t *testing.T) {
	f := newRefFixture(t)
	_, err := f.brands(context.Background(), ReferenceEdge{
		HolderType: "dataset", HolderID: "account:acme/dataset:nope", Field: "current_brands",
	})
	if code := aerr.CodeOf(err); code != aerr.APERTURE_NOT_FOUND {
		t.Fatalf("code = %q, want APERTURE_NOT_FOUND (err: %v)", code, err)
	}
}

// TestAbsentOutOfAccountHolderIsEmptyNeverNotFound IS THE DISCLOSURE BOUNDARY.
//
// NOT_FOUND is an existence oracle. Inside the caller's own account that is a
// feature; across accounts it would let a member of acme probe which ids exist in
// another tenancy, which is the house rule against leaking cross-account data
// through an error message. The answer outside the account is therefore EMPTY,
// for an absent holder and a present-but-unreadable one alike — and the error
// message must not name the other account either.
func TestAbsentOutOfAccountHolderIsEmptyNeverNotFound(t *testing.T) {
	f := newRefFixture(t)
	for _, holder := range []string{"account:other/dataset:z", "account:other/dataset:nope"} {
		got, err := f.brands(context.Background(), ReferenceEdge{
			HolderType: "dataset", HolderID: holder, Field: "current_brands",
		})
		if err != nil {
			t.Fatalf("Enumerate(out-of-account holder %s): got error %v (code %q), want empty and no error",
				holder, err, aerr.CodeOf(err))
		}
		wantIDs(t, got)
	}
}

// --- wiring faults are loud -------------------------------------------------

// TestUndeclaredReferenceFieldIsACodedError: a field with no references:
// declaration is a wiring fault. Treating it as an empty edge would render as
// "no access", which is exactly how an access-control bug hides.
func TestUndeclaredReferenceFieldIsACodedError(t *testing.T) {
	f := newRefFixture(t)
	_, err := f.brands(context.Background(), ReferenceEdge{
		HolderType: "dataset", HolderID: "account:acme/dataset:x", Field: "tier",
	})
	if code := aerr.CodeOf(err); code != aerr.APERTURE_PROVIDER_REFERENCE_INVALID {
		t.Fatalf("code = %q, want APERTURE_PROVIDER_REFERENCE_INVALID (err: %v)", code, err)
	}
}

// TestHolderTypeWithNoProviderIsACodedError: same policy, other half. "This type
// serves nothing" stays distinguishable from "this field declares nothing".
func TestHolderTypeWithNoProviderIsACodedError(t *testing.T) {
	f := newRefFixture(t)
	_, err := f.brands(context.Background(), ReferenceEdge{
		HolderType: "widget", HolderID: "account:acme/widget:1", Field: "current_brands",
	})
	if code := aerr.CodeOf(err); code != aerr.APERTURE_PROVIDER_UNREGISTERED {
		t.Fatalf("code = %q, want APERTURE_PROVIDER_UNREGISTERED (err: %v)", code, err)
	}
}

// TestWiringFaultsDoNotDependOnEarlierEdgesHavingData: an empty intersection
// does not stop the walk, so whether a later edge's wiring fault is reported
// cannot depend on how much data an earlier edge happened to hold. dataset:empty
// restricts to nothing; the undeclared field behind it must still be raised.
func TestWiringFaultsDoNotDependOnEarlierEdgesHavingData(t *testing.T) {
	f := newRefFixture(t)
	_, err := f.brands(context.Background(),
		ReferenceEdge{HolderType: "dataset", HolderID: "account:acme/dataset:empty", Field: "current_brands"},
		ReferenceEdge{HolderType: "dataset", HolderID: "account:acme/dataset:x", Field: "tier"},
	)
	if code := aerr.CodeOf(err); code != aerr.APERTURE_PROVIDER_REFERENCE_INVALID {
		t.Fatalf("code = %q, want APERTURE_PROVIDER_REFERENCE_INVALID (err: %v)", code, err)
	}
}

// TestReferenceEdgeWithNoSourceConfiguredIsACodedError: an engine wired without a
// reference source fails loudly on an edge rather than silently answering "you
// may see nothing".
func TestReferenceEdgeWithNoSourceConfiguredIsACodedError(t *testing.T) {
	ctx := context.Background()
	store := memory.New()
	if err := store.Setup(ctx); err != nil {
		t.Fatalf("setup: %v", err)
	}
	mustSeed(t, store.PutPrincipal(ctx, model.Principal{ID: "alice", Kind: model.PrincipalUser, Identity: "user:alice"}))
	eng := New(store)
	_, err := eng.Enumerate(ctx, EnumerateRequest{
		Account: acctAcme, Principal: "alice", Action: "read", Pattern: "account:acme/brand:*",
		References: []ReferenceEdge{inDatasetX()},
	})
	if code := aerr.CodeOf(err); code != aerr.APERTURE_PROVIDER_UNREGISTERED {
		t.Fatalf("code = %q, want APERTURE_PROVIDER_UNREGISTERED (err: %v)", code, err)
	}
}

// TestMalformedReferenceEdgeIsRejectedUpFront: a malformed edge says nothing
// about the data and everything about the caller, so it is rejected before any
// storage or provider work happens.
func TestMalformedReferenceEdgeIsRejectedUpFront(t *testing.T) {
	f := newRefFixture(t)
	for name, edge := range map[string]ReferenceEdge{
		"no holder":     {HolderType: "dataset", Field: "current_brands"},
		"no field":      {HolderType: "dataset", HolderID: "account:acme/dataset:x"},
		"blank field":   {HolderType: "dataset", HolderID: "account:acme/dataset:x", Field: "   "},
		"bad identity":  {HolderType: "dataset", HolderID: "not an identity", Field: "current_brands"},
		"wrong type":    {HolderType: "brand", HolderID: "account:acme/dataset:x", Field: "current_brands"},
		"pattern given": {HolderType: "dataset", HolderID: "account:acme/dataset:*", Field: "current_brands"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := f.brands(context.Background(), edge)
			switch code := aerr.CodeOf(err); code {
			case aerr.APERTURE_INVALID_INPUT, aerr.APERTURE_IDENTITY_INVALID:
			default:
				t.Fatalf("code = %q, want an input-shaped code (err: %v)", code, err)
			}
		})
	}
}

// --- dangling references ----------------------------------------------------

// TestDanglingReferenceIsSkippedLoggedAndNoted: an application-level foreign key
// has no database constraint behind it, so a brand that no longer exists must not
// take down every decision on the dataset that still lists it. It is skipped, the
// operator gets a warning naming it, and the caller gets a note on the same
// evaluation-notes channel Explain renders — which names the DECLARATION and the
// target type, never the missing identity.
func TestDanglingReferenceIsSkippedLoggedAndNoted(t *testing.T) {
	f := newRefFixture(t)
	ctx, collector := rules.WithNoteCollector(context.Background())

	got, err := f.brands(ctx, inDatasetX())
	if err != nil {
		t.Fatalf("Enumerate(dangling reference) failed the decision: %v", err)
	}
	wantIDs(t, got, "account:acme/brand:1", "account:acme/brand:2")

	notes := collector.Notes()
	var found *rules.Note
	for i := range notes {
		if notes[i].Kind == rules.NoteDanglingReference {
			found = &notes[i]
		}
	}
	if found == nil {
		t.Fatalf("notes = %+v, want one of kind %q", notes, rules.NoteDanglingReference)
	}
	if found.Path != "dataset.current_brands" || found.Expected != "brand" || found.Actual != "absent" {
		t.Fatalf("note = %+v, want path dataset.current_brands, expected brand, actual absent", *found)
	}
	// A note crosses account boundaries the same way an error message does, so the
	// missing identity must not ride along in any field or in the rendering.
	for _, field := range []string{found.Path, found.Expected, found.Actual, found.Rule, found.Op, found.String()} {
		if strings.Contains(field, "gone") {
			t.Fatalf("note leaked the dangling identity: %q in %+v", field, *found)
		}
	}
	// The projection Explain serializes carries the new kind verbatim.
	projected := evaluationNotes("g-dataset", []rules.Note{*found})
	if len(projected) != 1 || projected[0].Kind != string(rules.NoteDanglingReference) {
		t.Fatalf("projected = %+v, want one dangling_reference note", projected)
	}
	if projected[0].Message != found.String() {
		t.Fatalf("projected message = %q, want %q", projected[0].Message, found.String())
	}

	// The operator, unlike the caller, is told exactly which identity to fix.
	logged := f.logs.String()
	if !strings.Contains(logged, "dangling") || !strings.Contains(logged, "account:acme/brand:gone") {
		t.Fatalf("warning log = %q, want it to name the dangling identity", logged)
	}
}

// --- batch and impersonation carry the input identically --------------------

// TestEnumerateBatchAndAsCarryReferenceEdges: a change to the enumerate input has
// to land on all three entry points, or a caller gets a different answer
// depending on which door it came through.
func TestEnumerateBatchAndAsCarryReferenceEdges(t *testing.T) {
	f := newRefFixture(t)
	ctx := context.Background()
	req := EnumerateRequest{
		Account: acctAcme, Principal: "alice", Action: "read",
		Pattern: "account:acme/brand:*", References: []ReferenceEdge{inDatasetX()},
	}

	batch := f.eng.EnumerateBatch(ctx, []EnumerateRequest{req})
	if len(batch) != 1 || batch[0].Err != nil {
		t.Fatalf("EnumerateBatch = %+v, want one successful item", batch)
	}
	wantIDs(t, batch[0].Result, "account:acme/brand:1", "account:acme/brand:2")

	// An inert context delegates to Enumerate; an ACTIVE one takes the separate
	// EnumerateAs path into the shared walk. Both must dereference the edge, and
	// the active one must do it against the ELEVATED subject set — bob holds no
	// grants of his own, so every id here is reached through alice's authority.
	inert, err := f.eng.EnumerateAs(ctx, req, ImpersonationContext{})
	if err != nil {
		t.Fatalf("EnumerateAs(inert): %v", err)
	}
	wantIDs(t, inert, "account:acme/brand:1", "account:acme/brand:2")

	bobReq := req
	bobReq.Principal = "bob"
	bare, err := f.eng.Enumerate(ctx, bobReq)
	if err != nil {
		t.Fatalf("Enumerate(bob): %v", err)
	}
	wantIDs(t, bare)

	elevated, err := f.eng.EnumerateAs(ctx, bobReq, ImpersonationContext{
		RealActor: "bob", EffectiveSubject: "alice", Mode: ModeBecome,
		ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("EnumerateAs(become alice): %v", err)
	}
	wantIDs(t, elevated, "account:acme/brand:1", "account:acme/brand:2")
}

// --- the four questions -----------------------------------------------------

// TestTheFourQuestions is the proof the whole effort works: one fixture, one
// principal, and the four questions the interview asked, each answered by the
// surface built for it.
//
// Note in particular that (3) and (4) are NOT the same mechanism. (4) is a
// FILTER — dataset holds the field, so a predicate on dataset expresses it, and
// E1 already answers it. (3) is a DEREFERENCE — brand holds no field naming its
// datasets, so no predicate on brand can express it at all, which is the entire
// reason the reference edge exists.
func TestTheFourQuestions(t *testing.T) {
	f := newRefFixture(t)
	ctx := context.Background()
	enumerate := func(pattern string, req EnumerateRequest) []string {
		t.Helper()
		req.Account, req.Principal, req.Action, req.Pattern = acctAcme, "alice", "read", pattern
		ids, err := f.eng.Enumerate(ctx, req)
		if err != nil {
			t.Fatalf("Enumerate(%s): %v", pattern, err)
		}
		return ids
	}

	// 1. Which datasets do I have access to?
	wantIDs(t, enumerate("account:acme/dataset:*", EnumerateRequest{}),
		"account:acme/dataset:empty", "account:acme/dataset:x", "account:acme/dataset:y")

	// 2. Which brands do I have access to?
	wantIDs(t, enumerate("account:acme/brand:*", EnumerateRequest{}),
		"account:acme/brand:1", "account:acme/brand:2", "account:acme/brand:3")

	// 3. Which brands belong to dataset x? — the dereference.
	wantIDs(t, enumerate("account:acme/brand:*", EnumerateRequest{
		References: []ReferenceEdge{inDatasetX()},
	}), "account:acme/brand:1", "account:acme/brand:2")

	// 4. Which datasets contain brand 2? — the filter, from E1.
	wantIDs(t, enumerate("account:acme/dataset:*", EnumerateRequest{
		Fields: map[string]any{"current_brands": "account:acme/brand:2"},
	}), "account:acme/dataset:x", "account:acme/dataset:y")
}
