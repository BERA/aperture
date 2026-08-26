package scope

import (
	"context"
	"slices"
	"testing"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/identity"
)

// fakeLister is a test ObjectLister that returns a fixed set of identities of a
// type, honouring the bounding pattern and limit the resolver passes.
type fakeLister struct {
	objects []identity.Identity
}

func (l fakeLister) List(_ context.Context, _ string, pattern identity.Pattern, limit int) ([]identity.Identity, error) {
	out := make([]identity.Identity, 0, len(l.objects))
	for _, o := range l.objects {
		if pattern.Matches(o) {
			out = append(out, o)
			if limit > 0 && len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

// fakeRules selects objects whose canonical string is in the selected set,
// regardless of the rule ref — enough to exercise the rule seam.
type fakeRules struct {
	selected map[string]bool
	// seen, when non-nil, records the evaluation context of the last call. It is
	// how a test proves the account and principal kind survive the trip from
	// GrantContext to the evaluator instead of being dropped in the resolver.
	seen *ruleCall
}

// ruleCall is the request context a rule-backed resolver hands the evaluator.
type ruleCall struct {
	account   string
	kind      string
	principal string
	action    string
}

func (r fakeRules) Selected(_ context.Context, _ string, object identity.Identity, account, kind, principal, action string) (bool, error) {
	if r.seen != nil {
		*r.seen = ruleCall{account: account, kind: kind, principal: principal, action: action}
	}
	return r.selected[object.String()], nil
}

func mustResolve(t *testing.T, r *Registry, gc GrantContext, deps Deps) ScopeResolver {
	t.Helper()
	res, err := r.Resolve(gc, deps)
	if err != nil {
		t.Fatalf("Resolve(%s): %v", gc.Spec.Strategy, err)
	}
	return res
}

func gc(strategy, pattern, objType string, ids []string, rule string) GrantContext {
	return GrantContext{
		Pattern:    identity.MustParsePattern(pattern),
		ObjectType: objType,
		Spec:       Spec{Strategy: strategy, IDs: ids, Rule: rule},
	}
}

// --- implicit ---

func TestImplicitContains(t *testing.T) {
	r := DefaultRegistry()
	res := mustResolve(t, r, gc(StrategyImplicit, "account:acme/**", "document", nil, ""), Deps{})
	ctx := context.Background()

	// Any document under acme is a member.
	if ok, _ := res.Contains(ctx, identity.MustParse("account:acme/document:42")); !ok {
		t.Errorf("implicit should cover account:acme/document:42")
	}
	if ok, _ := res.Contains(ctx, identity.MustParse("account:acme/project:atlas/document:7")); !ok {
		t.Errorf("implicit should cover a nested document under acme")
	}
	// A non-document object of a different terminal type is not a member.
	if ok, _ := res.Contains(ctx, identity.MustParse("account:acme/project:atlas")); ok {
		t.Errorf("implicit document scope must not cover a project terminal")
	}
	// Outside the pattern scope: not a member.
	if ok, _ := res.Contains(ctx, identity.MustParse("account:other/document:1")); ok {
		t.Errorf("implicit must not cover objects outside the pattern scope")
	}
}

func TestImplicitRejectsConfig(t *testing.T) {
	_, err := newImplicitResolver(gc(StrategyImplicit, "**", "document", []string{"document:1"}, ""), Deps{})
	if code := aerr.CodeOf(err); code != aerr.APERTURE_SCOPE_INVALID {
		t.Fatalf("implicit with ids: code = %q, want APERTURE_SCOPE_INVALID", code)
	}
}

func TestImplicitMembersNeedsLister(t *testing.T) {
	r := DefaultRegistry()
	res := mustResolve(t, r, gc(StrategyImplicit, "account:acme/**", "document", nil, ""), Deps{})
	_, err := res.Members(context.Background(), identity.MustParsePattern("**"))
	if code := aerr.CodeOf(err); code != aerr.APERTURE_SCOPE_LISTER_UNCONFIGURED {
		t.Fatalf("implicit Members without lister: code = %q, want APERTURE_SCOPE_LISTER_UNCONFIGURED", code)
	}
}

func TestImplicitMembersWithLister(t *testing.T) {
	r := DefaultRegistry()
	lister := fakeLister{objects: []identity.Identity{
		identity.MustParse("account:acme/document:1"),
		identity.MustParse("account:acme/document:2"),
		identity.MustParse("account:other/document:3"),
	}}
	res := mustResolve(t, r, gc(StrategyImplicit, "account:acme/**", "document", nil, ""), Deps{Lister: lister})
	got, err := res.Members(context.Background(), identity.MustParsePattern("**"))
	if err != nil {
		t.Fatalf("Members: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("members = %v, want the two acme documents", got)
	}
}

// --- inclusive ---

func TestInclusiveListContains(t *testing.T) {
	r := DefaultRegistry()
	ids := []string{"account:acme/document:42", "account:acme/document:99"}
	res := mustResolve(t, r, gc(StrategyInclusive, "account:acme/**", "document", ids, ""), Deps{})
	ctx := context.Background()

	if ok, _ := res.Contains(ctx, identity.MustParse("account:acme/document:42")); !ok {
		t.Errorf("inclusive should cover a listed object")
	}
	if ok, _ := res.Contains(ctx, identity.MustParse("account:acme/document:7")); ok {
		t.Errorf("inclusive must not cover an unlisted object")
	}
	// Listed but outside the pattern scope: not covered (pattern bounds the list).
	res2 := mustResolve(t, r, gc(StrategyInclusive, "account:acme/project:atlas/**", "document",
		[]string{"account:acme/document:42"}, ""), Deps{})
	if ok, _ := res2.Contains(ctx, identity.MustParse("account:acme/document:42")); ok {
		t.Errorf("inclusive must not cover a listed object outside the pattern scope")
	}
}

func TestInclusiveRequiresIdsOrRule(t *testing.T) {
	_, err := newInclusiveResolver(gc(StrategyInclusive, "**", "document", nil, ""), Deps{})
	if code := aerr.CodeOf(err); code != aerr.APERTURE_SCOPE_INVALID {
		t.Fatalf("inclusive with no config: code = %q, want APERTURE_SCOPE_INVALID", code)
	}
}

func TestInclusiveMembersListPath(t *testing.T) {
	r := DefaultRegistry()
	ids := []string{"account:acme/document:42", "account:acme/document:99", "account:other/document:1"}
	res := mustResolve(t, r, gc(StrategyInclusive, "account:acme/**", "document", ids, ""), Deps{})
	got, err := res.Members(context.Background(), identity.MustParsePattern("**"))
	if err != nil {
		t.Fatalf("Members: %v", err)
	}
	// Only the two acme documents fall within the pattern scope; no lister needed.
	if len(got) != 2 {
		t.Fatalf("members = %v, want 2 acme documents", got)
	}
}

func TestInclusiveRulePath(t *testing.T) {
	r := DefaultRegistry()
	rules := fakeRules{selected: map[string]bool{"account:acme/document:5": true}}
	res := mustResolve(t, r, gc(StrategyInclusive, "account:acme/**", "document", nil, "myrule"),
		Deps{Rules: rules})
	ctx := context.Background()

	if ok, err := res.Contains(ctx, identity.MustParse("account:acme/document:5")); err != nil || !ok {
		t.Errorf("rule-selected object should be covered (ok=%v err=%v)", ok, err)
	}
	if ok, _ := res.Contains(ctx, identity.MustParse("account:acme/document:6")); ok {
		t.Errorf("non-selected object must not be covered")
	}
}

// TestRuleContextReachesTheEvaluator pins the whole point of the account and
// principal-kind fields on GrantContext: a resolver that carries them but does
// not hand them over would compile, evaluate, and be silently wrong — the rule
// would read attributes for the wrong account. Both rule-backed strategies are
// asserted, because they call the evaluator from two different places.
func TestRuleContextReachesTheEvaluator(t *testing.T) {
	object := identity.MustParse("account:acme/document:5")
	want := ruleCall{account: "acme", kind: "machine", principal: "svc-1", action: "read"}

	for _, tc := range []struct {
		name     string
		strategy string
	}{
		{"inclusive", StrategyInclusive},
		{"exclusive", StrategyExclusive},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var seen ruleCall
			ctxOf := gc(tc.strategy, "account:acme/**", "document", nil, "myrule")
			ctxOf.Account = want.account
			ctxOf.PrincipalKind = want.kind
			ctxOf.Principal = want.principal
			ctxOf.Action = want.action

			res := mustResolve(t, DefaultRegistry(), ctxOf,
				Deps{Rules: fakeRules{selected: map[string]bool{object.String(): true}, seen: &seen}})
			if _, err := res.Contains(context.Background(), object); err != nil {
				t.Fatalf("Contains: %v", err)
			}
			if seen != want {
				t.Errorf("evaluator saw %+v, want %+v", seen, want)
			}
		})
	}
}

// TestInclusiveMembers is the endpoint-agreement gate: whatever Contains answers
// object-by-object, Members must return as a set. The rule half is
// list-then-filter over the ObjectLister — a rule is an arbitrary expression over
// metadata and is not invertible, so there is nothing to invert.
func TestInclusiveMembers(t *testing.T) {
	// One catalogue for every case, so the difference between rows is the grant's
	// configuration and the wired deps — never the object population.
	catalogue := fakeLister{objects: []identity.Identity{
		identity.MustParse("account:acme/document:1"),
		identity.MustParse("account:acme/document:2"),
		identity.MustParse("account:acme/document:3"),
		identity.MustParse("account:other/document:9"),
	}}
	// The rule selects documents 2 and 3, plus one object the lister never
	// returns (proving the rule half is bounded by the listing, not by the rule).
	selecting := fakeRules{selected: map[string]bool{
		"account:acme/document:2":   true,
		"account:acme/document:3":   true,
		"account:acme/document:404": true,
		"account:other/document:9":  true,
	}}

	cases := []struct {
		name     string
		ids      []string
		rule     string
		query    string
		deps     Deps
		want     []string
		wantCode aerr.Code
	}{{
		name:  "list only needs no lister",
		ids:   []string{"account:acme/document:1", "account:other/document:9"},
		query: "**",
		deps:  Deps{},
		// document:9 is outside the grant pattern, so the pattern bounds the list.
		want: []string{"account:acme/document:1"},
	}, {
		name:  "rule only enumerates through the lister",
		rule:  "selective",
		query: "**",
		deps:  Deps{Lister: catalogue, Rules: selecting},
		want:  []string{"account:acme/document:2", "account:acme/document:3"},
	}, {
		name:  "list and rule enumerate their union without duplicates",
		ids:   []string{"account:acme/document:1", "account:acme/document:2"},
		rule:  "selective",
		query: "**",
		deps:  Deps{Lister: catalogue, Rules: selecting},
		// document:2 is in BOTH halves and appears once.
		want: []string{"account:acme/document:1", "account:acme/document:2", "account:acme/document:3"},
	}, {
		name:  "the query pattern bounds both halves",
		ids:   []string{"account:acme/document:1"},
		rule:  "selective",
		query: "account:acme/document:2",
		deps:  Deps{Lister: catalogue, Rules: selecting},
		want:  []string{"account:acme/document:2"},
	}, {
		name: "a rule that selects nothing enumerates an empty set, not an error",
		rule: "selective",
		// Nothing in the catalogue is under this account, so the listing is empty.
		query: "account:empty/**",
		deps:  Deps{Lister: catalogue, Rules: selecting},
		want:  nil,
	}, {
		name:     "rule-backed with no lister reports the missing lister",
		rule:     "selective",
		query:    "**",
		deps:     Deps{Rules: selecting},
		wantCode: aerr.APERTURE_SCOPE_LISTER_UNCONFIGURED,
	}, {
		name:     "a list alongside a rule does not excuse the missing lister",
		ids:      []string{"account:acme/document:1"},
		rule:     "selective",
		query:    "**",
		deps:     Deps{Rules: selecting},
		wantCode: aerr.APERTURE_SCOPE_LISTER_UNCONFIGURED,
	}, {
		name:     "rule-backed with no evaluator reports the missing evaluator",
		rule:     "selective",
		query:    "**",
		deps:     Deps{Lister: catalogue},
		wantCode: aerr.APERTURE_SCOPE_RULE_UNCONFIGURED,
	}}

	r := DefaultRegistry()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := mustResolve(t, r,
				gc(StrategyInclusive, "account:acme/**", "document", tc.ids, tc.rule), tc.deps)
			got, err := res.Members(context.Background(), identity.MustParsePattern(tc.query))
			if tc.wantCode != "" {
				if code := aerr.CodeOf(err); code != tc.wantCode {
					t.Fatalf("code = %q, want %q (members %v, err %v)", code, tc.wantCode, got, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Members: %v", err)
			}
			ids := make([]string, 0, len(got))
			for _, o := range got {
				ids = append(ids, o.String())
			}
			slices.Sort(ids)
			want := slices.Clone(tc.want)
			slices.Sort(want)
			if !slices.Equal(ids, want) {
				t.Fatalf("members = %v, want %v", ids, want)
			}
		})
	}
}

// TestInclusiveMembersAgreesWithContains pins the property the table above only
// samples: Members and Contains are two views of one membership predicate.
func TestInclusiveMembersAgreesWithContains(t *testing.T) {
	objects := []identity.Identity{
		identity.MustParse("account:acme/document:1"),
		identity.MustParse("account:acme/document:2"),
		identity.MustParse("account:acme/document:3"),
	}
	res := mustResolve(t, DefaultRegistry(),
		gc(StrategyInclusive, "account:acme/**", "document",
			[]string{"account:acme/document:1"}, "selective"),
		Deps{
			Lister: fakeLister{objects: objects},
			Rules:  fakeRules{selected: map[string]bool{"account:acme/document:3": true}},
		})

	ctx := context.Background()
	members, err := res.Members(ctx, identity.MustParsePattern("**"))
	if err != nil {
		t.Fatalf("Members: %v", err)
	}
	inMembers := make(map[string]bool, len(members))
	for _, m := range members {
		inMembers[m.String()] = true
	}
	for _, o := range objects {
		want, err := res.Contains(ctx, o)
		if err != nil {
			t.Fatalf("Contains(%s): %v", o, err)
		}
		if got := inMembers[o.String()]; got != want {
			t.Errorf("%s: Members says %v, Contains says %v", o, got, want)
		}
	}
}

func TestInclusiveRulePathUnconfigured(t *testing.T) {
	r := DefaultRegistry()
	res := mustResolve(t, r, gc(StrategyInclusive, "**", "document", nil, "myrule"), Deps{})
	_, err := res.Contains(context.Background(), identity.MustParse("document:1"))
	if code := aerr.CodeOf(err); code != aerr.APERTURE_SCOPE_RULE_UNCONFIGURED {
		t.Fatalf("rule path without evaluator: code = %q, want APERTURE_SCOPE_RULE_UNCONFIGURED", code)
	}
}

// --- exclusive ---

func TestExclusiveListContains(t *testing.T) {
	r := DefaultRegistry()
	res := mustResolve(t, r, gc(StrategyExclusive, "account:acme/**", "document",
		[]string{"account:acme/document:7"}, ""), Deps{})
	ctx := context.Background()

	// Of-type, in scope, not excluded: covered.
	if ok, _ := res.Contains(ctx, identity.MustParse("account:acme/document:1")); !ok {
		t.Errorf("exclusive should cover a non-excluded document")
	}
	// Excluded: not covered.
	if ok, _ := res.Contains(ctx, identity.MustParse("account:acme/document:7")); ok {
		t.Errorf("exclusive must not cover an excluded document")
	}
	// Wrong terminal type: not covered (exclusive is all-OF-TYPE minus list).
	if ok, _ := res.Contains(ctx, identity.MustParse("account:acme/project:atlas")); ok {
		t.Errorf("exclusive document scope must not cover a project terminal")
	}
	// Outside the pattern scope: not covered.
	if ok, _ := res.Contains(ctx, identity.MustParse("account:other/document:1")); ok {
		t.Errorf("exclusive must not cover objects outside the pattern scope")
	}
}

func TestExclusiveRequiresConfig(t *testing.T) {
	_, err := newExclusiveResolver(gc(StrategyExclusive, "**", "document", nil, ""), Deps{})
	if code := aerr.CodeOf(err); code != aerr.APERTURE_SCOPE_INVALID {
		t.Fatalf("exclusive with no config: code = %q, want APERTURE_SCOPE_INVALID", code)
	}
}

func TestExclusiveMembersWithLister(t *testing.T) {
	r := DefaultRegistry()
	lister := fakeLister{objects: []identity.Identity{
		identity.MustParse("account:acme/document:1"),
		identity.MustParse("account:acme/document:7"), // excluded
		identity.MustParse("account:acme/document:9"),
	}}
	res := mustResolve(t, r, gc(StrategyExclusive, "account:acme/**", "document",
		[]string{"account:acme/document:7"}, ""), Deps{Lister: lister})
	got, err := res.Members(context.Background(), identity.MustParsePattern("**"))
	if err != nil {
		t.Fatalf("Members: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("members = %v, want documents 1 and 9 (7 excluded)", got)
	}
	for _, o := range got {
		if o.String() == "account:acme/document:7" {
			t.Errorf("excluded document leaked into Members")
		}
	}
}

func TestExclusiveRulePath(t *testing.T) {
	r := DefaultRegistry()
	rules := fakeRules{selected: map[string]bool{"account:acme/document:3": true}}
	res := mustResolve(t, r, gc(StrategyExclusive, "account:acme/**", "document", nil, "quarantine"),
		Deps{Rules: rules})
	ctx := context.Background()

	// Rule selects document:3 for exclusion.
	if ok, err := res.Contains(ctx, identity.MustParse("account:acme/document:3")); err != nil || ok {
		t.Errorf("rule-excluded object must not be covered (ok=%v err=%v)", ok, err)
	}
	if ok, err := res.Contains(ctx, identity.MustParse("account:acme/document:4")); err != nil || !ok {
		t.Errorf("non-excluded object should be covered (ok=%v err=%v)", ok, err)
	}
}
