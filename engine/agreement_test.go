package engine

import (
	"context"
	"slices"
	"testing"

	"github.com/frankbardon/aperture/identity"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/provider"
	"github.com/frankbardon/aperture/rules"
	"github.com/frankbardon/aperture/scope"
	"github.com/frankbardon/aperture/storage/memory"
)

// Aperture's decision API is Check / Enumerate / Explain, and the three agreeing
// about the same model is the invariant worth testing directly. Each endpoint
// reaches object membership by a DIFFERENT route — Check and Explain ask a scope
// resolver's Contains one object at a time, Enumerate asks its Members for the
// whole set — so nothing structural forces the two routes to describe the same
// set. They have already drifted once: exclusive scope resolved Members by
// listing the object type and filtering through Contains, while inclusive scope
// answered from its id-list alone and reported its rule dependency unconfigured,
// so a rule-backed inclusive grant that Check allowed could not be enumerated at
// all. A single-strategy example test would not have caught that, because the
// strategy that was correct and the strategy that was broken looked identical
// from the outside; only putting every strategy in one table makes the asymmetry
// visible.
//
// This file is therefore a property over a matrix (every registered strategy ×
// every backing that strategy accepts), not another example. Two things keep it
// from decaying:
//
//   - The agreement assertion is BIDIRECTIONAL. See assertEndpointsAgree.
//   - The matrix is checked against the scope registry itself, so a strategy or
//     a backing added without a row FAILS rather than silently reducing
//     coverage. See TestScopeStrategyMatrixCoversEveryBackedStrategy.
//
// The rule-backed rows use a real rules.Engine compiling a real AST over real
// object metadata, not a stub evaluator, so the assertion exercises the
// production rule path (provider fetch, compile, cache, evaluate) rather than a
// test double that cannot disagree with itself.

// The pattern every grant in the matrix is bestowed over. It is deliberately NOT
// the account root: candidates live both inside and outside it, so each case
// exercises the pattern bound as well as the strategy.
const agreementGrantPattern = "account:acme/project:atlas/**"

// The pattern Enumerate is asked with. It is WIDER than the grant pattern, so an
// enumeration that forgot to intersect with the grant's own scope would visibly
// overreach instead of being masked by an equally narrow query.
const agreementQueryPattern = "account:acme/**"

// The fixed candidate set. Every combination in the matrix is evaluated against
// all of these, including the ones it must exclude.
const (
	// Inside the grant pattern, of the grant's object type, selected by the rule.
	agreeDocAlpha = "account:acme/project:atlas/document:alpha"
	// Inside the pattern and the id-list, but NOT selected by the rule.
	agreeDocBeta = "account:acme/project:atlas/document:beta"
	// Inside the pattern, selected by the rule, in no id-list.
	agreeDocGamma = "account:acme/project:atlas/document:gamma"
	// Inside the pattern, in no id-list, not selected by the rule — the object
	// only an implicit or exclusive scope reaches.
	agreeDocDelta = "account:acme/project:atlas/document:delta"
	// OUTSIDE the grant pattern, yet named by the inclusive id-list and selected
	// by the rule: the grant's pattern must still bound membership.
	agreeDocOmega = "account:acme/project:zeta/document:omega"
	// Inside the grant pattern but NOT of the grant's object type.
	agreeFolderInbox = "account:acme/project:atlas/folder:inbox"
)

// agreementCandidates is the fixed object set every case is evaluated over.
func agreementCandidates() []string {
	return []string{
		agreeDocAlpha,
		agreeDocBeta,
		agreeDocGamma,
		agreeDocDelta,
		agreeDocOmega,
		agreeFolderInbox,
	}
}

// agreementRule is the rule reference the rule-backed rows use.
const agreementRule = "public-documents"

// agreementDocuments is the metadata the "document" provider serves. The rule
// reads object.tags, so this is what makes the rule rows real.
func agreementDocuments() map[string]provider.Metadata {
	return map[string]provider.Metadata{
		agreeDocAlpha: {"tags": []any{"public"}},
		agreeDocBeta:  {"tags": []any{"internal"}},
		agreeDocGamma: {"tags": []any{"public"}},
		agreeDocDelta: {"tags": []any{"internal"}},
		agreeDocOmega: {"tags": []any{"public"}},
	}
}

// agreementFolders is the metadata the "folder" provider serves. A second object
// type exists so "Enumerate never returns an identity outside the grant's object
// type" is a real assertion and not a vacuous one over a single-type registry.
func agreementFolders() map[string]provider.Metadata {
	return map[string]provider.Metadata{
		agreeFolderInbox: {"tags": []any{"internal"}},
	}
}

// scopeBacking names how a scope strategy is configured to decide membership.
// It is the second axis of the matrix: the same strategy behaves differently, and
// reaches membership through different code, depending on which of the two
// backings (or both) a permission declares.
type scopeBacking string

const (
	// backingNone: the strategy takes neither an id-list nor a rule.
	backingNone scopeBacking = "none"
	// backingList: membership comes from an explicit id-list.
	backingList scopeBacking = "list"
	// backingRule: membership comes from a rule evaluated over object metadata.
	backingRule scopeBacking = "rule"
	// backingBoth: both, which must compose the same way in Contains and Members.
	backingBoth scopeBacking = "both"
)

// agreementCase is one cell of the strategy × backing matrix.
type agreementCase struct {
	name string
	// strategy is the scope strategy key, checked against the scope registry.
	strategy string
	// backing is which membership source the reference declares.
	backing scopeBacking
	// ref is the permission's opaque scope-strategy reference.
	ref string
	// wantAllowed pins the objects the combination actually covers, so the
	// agreement assertion cannot be satisfied by every endpoint answering "no".
	wantAllowed []string
}

// agreementCases is the matrix: every registered strategy paired with every
// backing that strategy accepts.
func agreementCases() []agreementCase {
	return []agreementCase{
		{
			name:        "implicit",
			strategy:    scope.StrategyImplicit,
			backing:     backingNone,
			ref:         scope.StrategyImplicit,
			wantAllowed: []string{agreeDocAlpha, agreeDocBeta, agreeDocGamma, agreeDocDelta},
		},
		{
			name:     "inclusive by id-list",
			strategy: scope.StrategyInclusive,
			backing:  backingList,
			// omega is listed but sits outside the grant pattern, so it must not
			// be covered — the list opts in, the pattern still bounds.
			ref:         "inclusive;ids=" + agreeDocAlpha + "," + agreeDocBeta + "," + agreeDocOmega,
			wantAllowed: []string{agreeDocAlpha, agreeDocBeta},
		},
		{
			name:        "inclusive by rule",
			strategy:    scope.StrategyInclusive,
			backing:     backingRule,
			ref:         "inclusive;rule=" + agreementRule,
			wantAllowed: []string{agreeDocAlpha, agreeDocGamma},
		},
		{
			name:     "inclusive by id-list and rule",
			strategy: scope.StrategyInclusive,
			backing:  backingBoth,
			ref:      "inclusive;ids=" + agreeDocAlpha + "," + agreeDocBeta + ";rule=" + agreementRule,
			// The union of the two halves, deduplicated: alpha is both listed and
			// rule-selected and is one member, not two.
			wantAllowed: []string{agreeDocAlpha, agreeDocBeta, agreeDocGamma},
		},
		{
			name:        "exclusive by id-list",
			strategy:    scope.StrategyExclusive,
			backing:     backingList,
			ref:         "exclusive;ids=" + agreeDocAlpha,
			wantAllowed: []string{agreeDocBeta, agreeDocGamma, agreeDocDelta},
		},
		{
			name:     "exclusive by rule",
			strategy: scope.StrategyExclusive,
			backing:  backingRule,
			ref:      "exclusive;rule=" + agreementRule,
			// The rule EXCLUDES what it selects, so the public documents drop out.
			wantAllowed: []string{agreeDocBeta, agreeDocDelta},
		},
		{
			name:     "exclusive by id-list and rule",
			strategy: scope.StrategyExclusive,
			backing:  backingBoth,
			ref:      "exclusive;ids=" + agreeDocBeta + ";rule=" + agreementRule,
			// All-of-type in scope, minus the listed beta, minus rule-selected
			// alpha and gamma.
			wantAllowed: []string{agreeDocDelta},
		},
	}
}

// newAgreementEngine builds an engine whose single allow grant carries ref as its
// permission's scope strategy, wired to a provider registry (metadata fetcher AND
// object lister) and a real rules engine.
func newAgreementEngine(t *testing.T, ref string) (*Engine, context.Context) {
	t.Helper()
	ctx := context.Background()
	store := memory.New()
	if err := store.Setup(ctx); err != nil {
		t.Fatalf("setup: %v", err)
	}
	mustSeed(t, store.PutAccount(ctx, model.Account{ID: acctAcme, Name: acctAcme}))
	mustSeed(t, store.PutObjectType(ctx, model.ObjectType{Name: "document", Actions: []string{"read"}}))
	mustSeed(t, store.PutObjectType(ctx, model.ObjectType{Name: "folder", Actions: []string{"read"}}))
	mustSeed(t, store.PutPermission(ctx, model.Permission{
		ID: "p-agree", ObjectType: "document", Action: "read", ScopeStrategy: ref,
	}))
	mustSeed(t, store.PutPrincipal(ctx, model.Principal{
		ID: "alice", Kind: model.PrincipalUser, Identity: "user:alice",
	}))
	mustSeed(t, store.PutGrant(ctx, model.Grant{
		ID: "g-agree", AccountID: acctAcme,
		Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: "alice"},
		PermissionID: "p-agree", Object: agreementGrantPattern, Effect: model.EffectAllow,
	}))

	reg := provider.NewRegistry()
	reg.MustRegister("document", metaProvider{md: agreementDocuments()})
	reg.MustRegister("folder", metaProvider{md: agreementFolders()})

	// A real compiled rule over real object metadata, not a stub evaluator: the
	// rule rows must exercise the production path a deployment actually runs.
	rule := &rules.Rule{
		Name: agreementRule,
		AST:  rules.Compare(rules.OpHas, rules.Var("object.tags"), rules.Lit("public")),
	}
	rulesEng := rules.NewEngine(rules.MapSource{agreementRule: rule}, reg)

	eng := New(store, WithScopeResolution(scope.DefaultRegistry(),
		ScopeDeps{Lister: reg, Rules: rulesEng}))
	return eng, ctx
}

// TestDecisionEndpointsAgreeAcrossEveryScopeStrategy is the property: for every
// strategy × backing, Check, Enumerate, and Explain describe the same object set.
func TestDecisionEndpointsAgreeAcrossEveryScopeStrategy(t *testing.T) {
	for _, tc := range agreementCases() {
		t.Run(tc.name, func(t *testing.T) {
			eng, ctx := newAgreementEngine(t, tc.ref)
			assertEndpointsAgree(t, ctx, eng, tc)
		})
	}
}

// assertEndpointsAgree runs the three endpoints over the fixed candidate set and
// requires them to describe one set.
//
// WHY SUBSET AGREEMENT IS NOT ENOUGH. The tempting assertion — "every id
// Enumerate returns is one Check allows" — is only half the property, and it is
// the half that cannot fail interestingly: a resolver whose Members returns
// nothing at all satisfies it vacuously, which is exactly the failure mode this
// file exists to catch (a rule-backed inclusive grant used to enumerate to
// nothing while Check allowed its members). The converse half — "every id Check
// allows, Enumerate returns" — is the one that catches an under-enumerating
// resolver, and it is only meaningful when it is asserted over candidates the
// grant EXCLUDES as well as ones it covers, because a Members implementation
// that returns every object in the store would pass the converse alone.
//
// So agreement is asserted in both directions over a candidate set that contains
// covered objects, objects outside the grant pattern, and an object of the wrong
// type: Check(obj).Allow must equal (obj ∈ Enumerate()) for EVERY candidate, and
// Explain must reach the same verdict as Check for every candidate. wantAllowed
// then pins the set itself, so a build where all three endpoints agree on "deny
// everything" fails rather than passes.
func assertEndpointsAgree(t *testing.T, ctx context.Context, eng *Engine, tc agreementCase) {
	t.Helper()

	enumerated, err := eng.Enumerate(ctx, EnumerateRequest{
		Account: acctAcme, Principal: "alice", Action: "read", Pattern: agreementQueryPattern,
	})
	if err != nil {
		t.Fatalf("Enumerate(%s): unexpected error: %v", tc.ref, err)
	}
	inEnumerate := make(map[string]struct{}, len(enumerated))
	for _, id := range enumerated {
		inEnumerate[id] = struct{}{}
	}

	// The set itself, so the bidirectional assertion below cannot be satisfied by
	// three endpoints that all answer "nothing".
	got := slices.Clone(enumerated)
	want := slices.Clone(tc.wantAllowed)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("Enumerate = %v, want %v (strategy %s, backing %s)",
			got, want, tc.strategy, tc.backing)
	}

	for _, obj := range agreementCandidates() {
		decision, err := eng.Check(ctx, Request{
			Account: acctAcme, Principal: "alice", Action: "read", Object: obj,
		})
		if err != nil {
			t.Fatalf("Check(%s): unexpected error: %v", obj, err)
		}

		// Check is pinned independently, so a disagreement below names the
		// endpoint that moved rather than only reporting that two differ.
		wantAllow := slices.Contains(tc.wantAllowed, obj)
		if decision.Allow != wantAllow {
			t.Errorf("Check(%s).Allow = %v, want %v: %s", obj, decision.Allow, wantAllow, decision.Reason)
		}

		// Forward and converse in one comparison: an object Check allows must be
		// enumerated, and an object Check denies must not be.
		_, enumerated := inEnumerate[obj]
		if decision.Allow != enumerated {
			t.Errorf("Check and Enumerate disagree about %s: Check allow=%v, in Enumerate=%v",
				obj, decision.Allow, enumerated)
		}

		trace, err := eng.Explain(ctx, Request{
			Account: acctAcme, Principal: "alice", Action: "read", Object: obj,
		})
		if err != nil {
			t.Fatalf("Explain(%s): unexpected error: %v", obj, err)
		}
		if trace.Decision.Allow != decision.Allow {
			t.Errorf("Check and Explain disagree about %s: Check allow=%v, Explain allow=%v\n%s",
				obj, decision.Allow, trace.Decision.Allow, trace.String())
		}
	}

	// Enumerate must never widen the grant: nothing outside its object type, and
	// nothing outside its pattern. A resolver that listed the wrong type, or that
	// forgot to intersect the lister's output with the grant pattern, would leak
	// here even when every per-object verdict happened to agree.
	grantPattern, err := identity.ParsePattern(agreementGrantPattern)
	if err != nil {
		t.Fatalf("parse grant pattern: %v", err)
	}
	for _, raw := range enumerated {
		id, err := identity.Parse(raw)
		if err != nil {
			t.Fatalf("Enumerate returned an unparseable identity %q: %v", raw, err)
		}
		segs := id.Segments()
		if got := segs[len(segs)-1].Type; got != "document" {
			t.Errorf("Enumerate returned %s of type %q, but the grant's object type is %q",
				raw, got, "document")
		}
		if !grantPattern.Matches(id) {
			t.Errorf("Enumerate returned %s, which is outside the grant pattern %s",
				raw, agreementGrantPattern)
		}
	}
}

// TestScopeStrategyMatrixCoversEveryBackedStrategy is what stops the matrix above
// from decaying into a snapshot of the strategies that happened to exist when it
// was written.
//
// It does not hold a hand-maintained list to diff against — that would need
// editing in the same breath as the matrix and would therefore never fail. It
// interrogates the scope registry directly: for every registered strategy it
// tries to build a resolver under each of the four backing shapes, and treats
// "the factory accepted this configuration" as "this cell is legal and the matrix
// owes it a row". A new strategy, or a new backing an existing strategy starts
// accepting, fails here until a row is added. A row for a configuration the
// registry rejects fails too, so the matrix cannot claim coverage it does not
// have.
func TestScopeStrategyMatrixCoversEveryBackedStrategy(t *testing.T) {
	registry := scope.DefaultRegistry()
	pattern, err := identity.ParsePattern(agreementGrantPattern)
	if err != nil {
		t.Fatalf("parse grant pattern: %v", err)
	}

	declared := make(map[string]map[scopeBacking]bool)
	for _, tc := range agreementCases() {
		if declared[tc.strategy] == nil {
			declared[tc.strategy] = make(map[scopeBacking]bool)
		}
		if declared[tc.strategy][tc.backing] {
			t.Errorf("matrix declares strategy %q with backing %q twice", tc.strategy, tc.backing)
		}
		declared[tc.strategy][tc.backing] = true
	}

	for strategy := range declared {
		if !registry.Has(strategy) {
			t.Errorf("matrix declares strategy %q, which the scope registry does not register", strategy)
		}
	}

	backings := []scopeBacking{backingNone, backingList, backingRule, backingBoth}
	for _, strategy := range registry.Keys() {
		for _, backing := range backings {
			spec := scope.Spec{Strategy: strategy}
			switch backing {
			case backingList:
				spec.IDs = []string{agreeDocAlpha}
			case backingRule:
				spec.Rule = agreementRule
			case backingBoth:
				spec.IDs = []string{agreeDocAlpha}
				spec.Rule = agreementRule
			}
			_, err := registry.Resolve(scope.GrantContext{
				Pattern: pattern, ObjectType: "document", Spec: spec,
				Principal: "alice", Action: "read",
			}, scope.Deps{})
			legal := err == nil
			switch {
			case legal && !declared[strategy][backing]:
				t.Errorf("strategy %q accepts backing %q but the agreement matrix has no row for it; "+
					"add one rather than leaving the combination unproven", strategy, backing)
			case !legal && declared[strategy][backing]:
				t.Errorf("the agreement matrix declares strategy %q with backing %q, "+
					"but the scope registry rejects that configuration: %v", strategy, backing, err)
			}
		}
	}
}
