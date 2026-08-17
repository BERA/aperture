package service

import (
	"context"
	"testing"
	"time"

	"github.com/frankbardon/aperture/engine"
	"github.com/frankbardon/aperture/identity"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/provider"
	"github.com/frankbardon/aperture/rules"
	"github.com/frankbardon/aperture/seed"
	"github.com/frankbardon/aperture/storage/memory"
)

// newSeededService builds a read+simulate facade over the embedded example model.
func newSeededService(t *testing.T) (*Service, model.Storage) {
	t.Helper()
	ctx := context.Background()
	store := memory.New()
	if err := store.Setup(ctx); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := seed.Load(ctx, store, seed.Example, seed.FormatYAML); err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return New(engine.New(store), WithStorage(store)), store
}

const (
	simAccount   = seed.ExampleAccount
	simPrincipal = "alice"
	simAction    = "read"
	// nimbus sits outside the atlas-scoped grants, so no grant covers it: a real
	// Check default-denies, and a hypothetical allow is what flips it.
	simUncovered = "account:acme/project:nimbus/document:1"
)

// TestSimulateOverlayChangesDecisionWithoutWriting is the core what-if assertion:
// a hypothetical allow grant flips a decision under Simulate, yet the live model
// is untouched (a real Check still denies, and the stored grant set is unchanged).
func TestSimulateOverlayChangesDecisionWithoutWriting(t *testing.T) {
	ctx := context.Background()
	svc, store := newSeededService(t)

	q := Query{Account: simAccount, Principal: simPrincipal, Action: simAction, Object: simUncovered}

	// Baseline: the uncovered object default-denies.
	base, err := svc.Check(ctx, q)
	if err != nil {
		t.Fatalf("baseline check: %v", err)
	}
	if base.Allow {
		t.Fatal("expected baseline default-deny on the uncovered object")
	}

	grantsBefore, _ := store.ListGrants(ctx, simAccount)

	// A hypothetical allow on the uncovered object flips the default-deny to allow.
	ov := Overlay{Grants: []model.Grant{{
		ID:           "what-if-unseal",
		AccountID:    simAccount,
		Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: simPrincipal},
		PermissionID: "perm-doc-read",
		Object:       simUncovered,
		Effect:       model.EffectAllow,
	}}}

	sim, err := svc.Simulate(ctx, ov, q)
	if err != nil {
		t.Fatalf("simulate: %v", err)
	}
	if !sim.Allow {
		t.Errorf("expected the hypothetical allow to flip the decision; got deny: %s", sim.Reason)
	}

	// The live model must be untouched: a real Check still denies and the grant set
	// is the same length.
	after, err := svc.Check(ctx, q)
	if err != nil {
		t.Fatalf("post-sim check: %v", err)
	}
	if after.Allow {
		t.Error("simulate leaked into the live model: real Check now allows")
	}
	grantsAfter, _ := store.ListGrants(ctx, simAccount)
	if len(grantsAfter) != len(grantsBefore) {
		t.Errorf("simulate persisted a grant: %d -> %d", len(grantsBefore), len(grantsAfter))
	}
}

// TestSimulateExplainTracesOverlay asserts SimulateExplain returns a trace whose
// deciding grant is the hypothetical one.
func TestSimulateExplainTracesOverlay(t *testing.T) {
	ctx := context.Background()
	svc, _ := newSeededService(t)

	ov := Overlay{Grants: []model.Grant{{
		ID:           "what-if-unseal",
		AccountID:    simAccount,
		Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: simPrincipal},
		PermissionID: "perm-doc-read",
		Object:       simUncovered,
		Effect:       model.EffectAllow,
	}}}
	q := Query{Account: simAccount, Principal: simPrincipal, Action: simAction, Object: simUncovered}

	tr, err := svc.SimulateExplain(ctx, ov, q)
	if err != nil {
		t.Fatalf("simulate explain: %v", err)
	}
	if !tr.Decision.Allow {
		t.Fatalf("expected allow under overlay, got: %s", tr.Decision.Reason)
	}
	if !containsGrant(tr, "what-if-unseal") {
		t.Errorf("expected the hypothetical grant among considered grants; trace: %+v", tr.Considered)
	}
}

// TestSimulateReadOnlyFacadeUnimplemented asserts Simulate requires the entity
// surface (a read-only facade with no storage cannot overlay).
func TestSimulateReadOnlyFacadeUnimplemented(t *testing.T) {
	ctx := context.Background()
	svc, store := newSeededService(t)
	bare := New(svc.eng) // no WithStorage
	_ = store

	_, err := bare.Simulate(ctx, Overlay{}, Query{Account: simAccount, Principal: simPrincipal, Action: simAction, Object: simUncovered})
	if err == nil {
		t.Fatal("expected APERTURE_UNIMPLEMENTED from a storage-less facade")
	}
}

func containsGrant(tr engine.Trace, id string) bool {
	for _, ge := range tr.Considered {
		if ge.GrantID == id {
			return true
		}
	}
	return false
}

// --- The rule builder's what-if preview --------------------------------------

// newHiredAtRegistry builds a provider registry holding two staff objects: one
// whose hired_at is a canonical date, and one whose hired_at is a string that is
// not a date at all (the deny-safe case the preview has to be able to explain).
func newHiredAtRegistry(t *testing.T) *provider.Registry {
	t.Helper()
	p, err := provider.NewStatic([]provider.Object{
		{ID: identity.MustParse("staff:1"), Metadata: provider.Metadata{"hired_at": "2025-06-01"}},
		{ID: identity.MustParse("staff:2"), Metadata: provider.Metadata{"hired_at": "01/06/2025"}},
	})
	if err != nil {
		t.Fatalf("NewStatic: %v", err)
	}
	reg := provider.NewRegistry()
	reg.MustRegister("staff", p)
	return reg
}

// previewInstant is the pinned clock every preview test below evaluates at. It
// is a fixed instant rather than time.Now so a relative-date expectation is a
// constant: "three months back" from it is 2025-12-04T12:30:00Z on every run.
var previewInstant = time.Date(2026, 3, 4, 12, 30, 0, 0, time.UTC)

func newPreviewService(t *testing.T) *Service {
	t.Helper()
	return New(engine.New(memory.New()),
		WithProviders(newHiredAtRegistry(t)),
		WithClock(func() time.Time { return previewInstant }))
}

// TestEvaluateRulePreviewResolvesRelativeDates is the regression test for the
// preview's missing reference instant.
//
// A compiled rule evaluated with a zero rules.Input.Now has NO reference
// instant, and every relative date then correctly resolves to nothing — so the
// rule denies. That is right in the engine and wrong in the preview: before the
// facade supplied the instant, the literal-date row below passed and every
// relative-date row returned false at every offset, while the same rule allowed
// access in a real decision (engine.Check / Enumerate / Explain each open
// rules.WithDecisionInstant). The relative rows fail without the fix.
func TestEvaluateRulePreviewResolvesRelativeDates(t *testing.T) {
	ctx := context.Background()
	svc := newPreviewService(t)

	hiredAt := rules.Var("object.hired_at")
	cases := []struct {
		name string
		ast  *rules.Node
		want bool
	}{
		{
			// The control: a literal bound needs no instant, so this row passed
			// before the fix as well. It is here to prove the two forms agree.
			name: "literal bound",
			ast:  rules.Compare(rules.OpBefore, hiredAt, rules.Lit("2026-01-01")),
			want: true,
		},
		{
			// 2026-03-04T12:30:00Z minus three months is 2025-12-04T12:30:00Z,
			// which the 2025-06-01 hire date precedes.
			name: "relative bound, matching",
			ast: rules.Compare(rules.OpBefore, hiredAt,
				rules.RelativeDate(rules.AnchorNow, -3, rules.UnitMonths, rules.SnapNone)),
			want: true,
		},
		{
			// The same operand shape that must NOT match: three months back is
			// after the hire date, so `after` is false. Without an instant this
			// row would pass for the wrong reason, which is why the matching row
			// above carries the regression.
			name: "relative bound, not matching",
			ast: rules.Compare(rules.OpAfter, hiredAt,
				rules.RelativeDate(rules.AnchorNow, -3, rules.UnitMonths, rules.SnapNone)),
			want: false,
		},
		{
			// TODAY is a distinct anchor, and it resolves too.
			name: "today anchor",
			ast: rules.Compare(rules.OpBefore, hiredAt,
				rules.RelativeDate(rules.AnchorToday, 0, rules.UnitDays, rules.SnapNone)),
			want: true,
		},
		{
			// Both bounds relative: the whole of the year five years back is
			// before the hire date, so nothing matches; the year one year back
			// does contain it.
			name: "between two relative bounds",
			ast: rules.Between(hiredAt,
				rules.RelativeDate(rules.AnchorNow, -1, rules.UnitYears, rules.SnapStartOfYear),
				rules.RelativeDate(rules.AnchorNow, -1, rules.UnitYears, rules.SnapEndOfYear)),
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := svc.EvaluateRulePreview(ctx, tc.ast, "staff:1")
			if err != nil {
				t.Fatalf("EvaluateRulePreview: %v", err)
			}
			if p.Result != tc.want {
				t.Errorf("result = %v, want %v (the preview must evaluate against a reference instant)", p.Result, tc.want)
			}
			// The narrow projection must agree with the full one — they are one
			// evaluation, not two.
			got, md, err := svc.EvaluateRule(ctx, tc.ast, "staff:1")
			if err != nil {
				t.Fatalf("EvaluateRule: %v", err)
			}
			if got != p.Result {
				t.Errorf("EvaluateRule result = %v, EvaluateRulePreview result = %v", got, p.Result)
			}
			if md["hired_at"] != "2025-06-01" {
				t.Errorf("metadata snapshot = %v, want the object's hired_at", md)
			}
		})
	}
}

// TestEvaluateRulePreviewReportsTheResolvedBound pins the preview's main
// affordance: an author who writes "three months back" is shown the instant the
// evaluation used and the concrete date that operand became at it. Resolving it
// a second time in the browser is what this exists to avoid.
func TestEvaluateRulePreviewReportsTheResolvedBound(t *testing.T) {
	ctx := context.Background()
	svc := newPreviewService(t)

	ast := rules.Between(rules.Var("object.hired_at"),
		rules.RelativeDate(rules.AnchorNow, -5, rules.UnitYears, rules.SnapStartOfYear),
		rules.RelativeDate(rules.AnchorNow, 0, rules.UnitDays, rules.SnapNone))

	p, err := svc.EvaluateRulePreview(ctx, ast, "staff:1")
	if err != nil {
		t.Fatalf("EvaluateRulePreview: %v", err)
	}
	if !p.Now.Equal(previewInstant) {
		t.Errorf("Now = %s, want the pinned clock %s", p.Now, previewInstant)
	}
	if len(p.Bounds) != 2 {
		t.Fatalf("bounds = %d, want 2 (one per relative-date operand)", len(p.Bounds))
	}
	// Snap first, then offset: the start of THIS year, five years back.
	want := []rules.ResolvedRelativeDate{
		{Path: "$.right.items[0]", Anchor: rules.AnchorNow, Offset: "-5", Unit: rules.UnitYears,
			Snap: rules.SnapStartOfYear, Resolved: "2021-01-01T00:00:00Z"},
		{Path: "$.right.items[1]", Anchor: rules.AnchorNow, Offset: "0", Unit: rules.UnitDays,
			Snap: rules.SnapNone, Resolved: "2026-03-04T12:30:00Z"},
	}
	for i, w := range want {
		if p.Bounds[i] != w {
			t.Errorf("bound %d = %+v, want %+v", i, p.Bounds[i], w)
		}
	}
	// A rule with no relative date reports no bounds at all.
	plain, err := svc.EvaluateRulePreview(ctx,
		rules.Compare(rules.OpBefore, rules.Var("object.hired_at"), rules.Lit("2026-01-01")), "staff:1")
	if err != nil {
		t.Fatalf("EvaluateRulePreview (literal): %v", err)
	}
	if len(plain.Bounds) != 0 {
		t.Errorf("bounds = %+v, want none for a rule with no relative date", plain.Bounds)
	}
}

// TestEvaluateRulePreviewSurfacesDenySafeNotes pins that a false the DATA caused
// is distinguishable from a false the rule caused. A silent false is how an
// access-control bug hides: the object's hired_at here is a real string that is
// simply not a canonical date, and the preview has to say so.
func TestEvaluateRulePreviewSurfacesDenySafeNotes(t *testing.T) {
	ctx := context.Background()
	svc := newPreviewService(t)

	ast := rules.Compare(rules.OpBefore, rules.Var("object.hired_at"),
		rules.RelativeDate(rules.AnchorNow, -3, rules.UnitMonths, rules.SnapNone))

	// staff:2's hired_at is "01/06/2025" — right shape, wrong content.
	bad, err := svc.EvaluateRulePreview(ctx, ast, "staff:2")
	if err != nil {
		t.Fatalf("EvaluateRulePreview: %v", err)
	}
	if bad.Result {
		t.Fatal("a non-canonical date must deny")
	}
	if len(bad.Notes) != 1 || bad.Notes[0].Kind != rules.NoteDateInvalid {
		t.Fatalf("notes = %+v, want one %s note", bad.Notes, rules.NoteDateInvalid)
	}
	if bad.Notes[0].Path != "object.hired_at" {
		t.Errorf("note path = %q, want object.hired_at", bad.Notes[0].Path)
	}
	// A clean evaluation records nothing — notes explain a false, they do not
	// accompany every one.
	ok, err := svc.EvaluateRulePreview(ctx, ast, "staff:1")
	if err != nil {
		t.Fatalf("EvaluateRulePreview: %v", err)
	}
	if len(ok.Notes) != 0 {
		t.Errorf("notes = %+v, want none for a clean evaluation", ok.Notes)
	}
}
