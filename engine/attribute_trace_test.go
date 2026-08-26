package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/provider"
	"github.com/frankbardon/aperture/rules"
	"github.com/frankbardon/aperture/scope"
	"github.com/frankbardon/aperture/storage/memory"
)

// E5-S1, the decision surface: an Explain says which attributes were read, and
// says so when there were none.
//
// The two halves answer the same operator question — "why was this denied?" —
// from opposite ends. The TRACE carries the bags, values included, so an operator
// can see that tier was "silver" rather than only that a rule did not select. The
// NOTE covers the case the bags cannot: a floor-only bag renders as a bag with
// nothing surprising in it, and only the note says the rule compared against
// nothing at all.

// traceAttributeFixture wires the whole stack — storage, an attribute registry,
// a rule-backed inclusive grant — with the user and account directories under the
// test's control. A nil directory map leaves that slot UNREGISTERED, which is the
// deployment that wired nothing.
func traceAttributeFixture(t *testing.T, users, accounts map[string]provider.Metadata) (*Engine, context.Context) {
	t.Helper()
	ctx := context.Background()
	store := memory.New()
	if err := store.Setup(ctx); err != nil {
		t.Fatalf("setup: %v", err)
	}
	mustSeed(t, store.PutAccount(ctx, model.Account{ID: acctAcme, Name: acctAcme}))
	mustSeed(t, store.PutObjectType(ctx, model.ObjectType{Name: "document", Actions: []string{"read"}}))
	mustSeed(t, store.PutPermission(ctx, model.Permission{
		ID: "p-tier", ObjectType: "document", Action: "read", ScopeStrategy: "inclusive;rule=gold-only",
	}))
	mustSeed(t, store.PutPrincipal(ctx, model.Principal{ID: "alice", Kind: model.PrincipalUser, Identity: "user:alice"}))
	mustSeed(t, store.PutGrant(ctx, model.Grant{
		ID: "g-alice", AccountID: acctAcme, Subject: model.Subject{Kind: model.SubjectPrincipal, ID: "alice"},
		PermissionID: "p-tier", Object: "account:acme/**", Effect: model.EffectAllow,
	}))

	attrs := provider.NewAttributeRegistry()
	register := func(slot provider.AttributeSlot, bags map[string]provider.Metadata) {
		t.Helper()
		if bags == nil {
			return
		}
		records := make([]provider.AttributeRecord, 0, len(bags))
		for id, md := range bags {
			records = append(records, provider.AttributeRecord{ID: id, Attributes: md})
		}
		p, err := provider.NewStaticAttributes(records)
		if err != nil {
			t.Fatalf("directory for %s: %v", slot, err)
		}
		attrs.MustRegister(slot, p)
	}
	register(provider.AttributeSlotUser, users)
	register(provider.AttributeSlotAccount, accounts)

	rulesEng := rules.NewEngine(
		rules.MapSource{"gold-only": {Name: "gold-only", AST: rules.Compare(
			rules.OpEq, rules.Var("principal.tier"), rules.Lit("gold"))}},
		nil, rules.WithPrincipalResolver(attrs), rules.WithAccountResolver(attrs))
	return New(store, WithScopeResolution(scope.DefaultRegistry(), ScopeDeps{Rules: rulesEng})), ctx
}

func explainAlice(t *testing.T, eng *Engine, ctx context.Context) Trace {
	t.Helper()
	tr, err := eng.Explain(ctx, Request{
		Account: acctAcme, Principal: "alice", Action: "read", Object: "account:acme/document:1"})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	return tr
}

// TestATraceCarriesTheAttributesTheRuleRead is the first acceptance criterion.
// The trace shows the VALUES, not merely that a comparison failed: an operator
// looking at a denial has to be able to see that the tier was "silver".
func TestATraceCarriesTheAttributesTheRuleRead(t *testing.T) {
	eng, ctx := traceAttributeFixture(t,
		map[string]provider.Metadata{"alice": {"tier": "silver"}},
		map[string]provider.Metadata{acctAcme: {"plan": "starter"}})
	tr := explainAlice(t, eng, ctx)

	if tr.Decision.Allow {
		t.Fatalf("a silver principal must not match the gold-only rule\n%s", tr.String())
	}
	if got := tr.Attributes.Principal["tier"]; got != "silver" {
		t.Fatalf("trace principal attributes = %v, want the tier the rule actually compared "+
			"against", tr.Attributes.Principal)
	}
	if got := tr.Attributes.Account["plan"]; got != "starter" {
		t.Fatalf("trace account attributes = %v, want the account bag the decision ran in",
			tr.Attributes.Account)
	}
	// The floor is part of what a rule read, so it is part of what the trace
	// reports: a bag without id and kind is a bag no rule ever saw.
	if tr.Attributes.Principal["id"] != "alice" || tr.Attributes.Principal["kind"] != "user" {
		t.Fatalf("the trace's principal bag must carry the engine's floor, got %v", tr.Attributes.Principal)
	}
	if tr.Attributes.Account["id"] != acctAcme {
		t.Fatalf("the trace's account bag must carry the engine's floor, got %v", tr.Attributes.Account)
	}
	// And the rendered report an operator actually reads carries them too.
	report := tr.String()
	for _, want := range []string{"principal: id=alice, kind=user, tier=silver", "account: id=acme, plan=starter"} {
		if !strings.Contains(report, want) {
			t.Fatalf("explain output is missing %q:\n%s", want, report)
		}
	}
}

// TestATraceDistinguishesAMissingBagFromARealMismatch is the criterion the whole
// story turns on. Two denials that are IDENTICAL in the verdict — same request,
// same grant, same rule, same "did not cover" outcome — must be tellable apart
// from the trace alone, because one of them means the rule compared against
// nothing and, in an exclusive grant, that is the silent widening the leniency
// contract accepted.
func TestATraceDistinguishesAMissingBagFromARealMismatch(t *testing.T) {
	realValue, ctx := traceAttributeFixture(t,
		map[string]provider.Metadata{"alice": {"tier": "silver"}}, nil)
	missing, _ := traceAttributeFixture(t,
		map[string]provider.Metadata{"someone-else": {"tier": "gold"}}, nil)

	mismatch := explainAlice(t, realValue, ctx)
	floorOnly := explainAlice(t, missing, ctx)

	if mismatch.Decision.Allow || floorOnly.Decision.Allow {
		t.Fatal("both fixtures must deny; the point is that the VERDICTS are identical")
	}
	if mismatch.Decision.Reason != floorOnly.Decision.Reason {
		t.Fatalf("the two denials were expected to be indistinguishable in the verdict:\n%q\n%q",
			mismatch.Decision.Reason, floorOnly.Decision.Reason)
	}

	if kinds := noteKinds(floorOnly, "principal"); len(kinds) != 1 || kinds[0] != string(rules.NoteAttributesFloorOnly) {
		t.Fatalf("the floor-only trace's principal notes = %v, want exactly one "+
			"attributes_floor_only\n%s", kinds, floorOnly.String())
	}
	if kinds := noteKinds(mismatch, "principal"); len(kinds) != 0 {
		t.Fatalf("a rule that compared against a REAL value must not be reported as floor-only, "+
			"got %v\n%s", kinds, mismatch.String())
	}
	// The note reaches the rendered report, where an operator is already looking.
	if !strings.Contains(floorOnly.String(), "principal: floor-only") {
		t.Fatalf("explain output must say the bag was floor-only:\n%s", floorOnly.String())
	}
	// And it names WHICH root, tied to the grant and the rule that read it.
	for _, n := range floorOnly.Notes {
		if n.Kind != string(rules.NoteAttributesFloorOnly) {
			continue
		}
		if n.GrantID != "g-alice" || n.Rule != "gold-only" {
			t.Fatalf("floor-only note = %+v, want it tied to the grant and rule that read the bag", n)
		}
	}
}

// TestATraceWithNoRuleInventsNoBags: a decision that evaluated no rule resolved
// no attributes, and the trace says nothing rather than reporting empty bags a
// caller could mistake for "the directory had nothing". It is the same contract
// Trace.Now holds.
func TestATraceWithNoRuleInventsNoBags(t *testing.T) {
	ctx := context.Background()
	store := memory.New()
	if err := store.Setup(ctx); err != nil {
		t.Fatalf("setup: %v", err)
	}
	mustSeed(t, store.PutAccount(ctx, model.Account{ID: acctAcme, Name: acctAcme}))
	mustSeed(t, store.PutObjectType(ctx, model.ObjectType{Name: "document", Actions: []string{"read"}}))
	mustSeed(t, store.PutPermission(ctx, model.Permission{
		ID: "p-lit", ObjectType: "document", Action: "read",
	}))
	mustSeed(t, store.PutPrincipal(ctx, model.Principal{ID: "alice", Kind: model.PrincipalUser, Identity: "user:alice"}))
	mustSeed(t, store.PutGrant(ctx, model.Grant{
		ID: "g-lit", AccountID: acctAcme, Subject: model.Subject{Kind: model.SubjectPrincipal, ID: "alice"},
		PermissionID: "p-lit", Object: "account:acme/**", Effect: model.EffectAllow,
	}))

	tr := explainAlice(t, New(store), ctx)
	if !tr.Decision.Allow {
		t.Fatalf("the literal grant should allow\n%s", tr.String())
	}
	if tr.Attributes.Principal != nil || tr.Attributes.Account != nil {
		t.Fatalf("a decision that evaluated no rule must report no bags, got %+v", tr.Attributes)
	}
	if strings.Contains(tr.String(), "\n  principal:") {
		t.Fatalf("the report must not print an attribute line for a decision that read none:\n%s", tr.String())
	}
}

// noteKinds returns the kinds of the trace's notes about one root.
func noteKinds(tr Trace, path string) []string {
	var out []string
	for _, n := range tr.Notes {
		if n.Path == path {
			out = append(out, n.Kind)
		}
	}
	return out
}
