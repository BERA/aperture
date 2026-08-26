package engine

import (
	"context"
	"sync"
	"testing"

	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/provider"
	"github.com/frankbardon/aperture/rules"
	"github.com/frankbardon/aperture/scope"
	"github.com/frankbardon/aperture/storage/memory"
)

// E4-S1, end to end: a decision resolves the principal once and the account once,
// however many objects it touches.
//
// It is proven through the decision engine rather than through rules.Engine
// because the scope is the DECISION's, not the evaluator's: Check, Enumerate and
// Explain are the three boundaries that open it, and an enumeration is where the
// fan-out actually bites — a rule-backed Enumerate evaluates its rule twice per
// candidate, so N objects would otherwise mean 2N principal fetches.

// countingDirectory counts every attribute resolution. It stands in for a host
// directory rather than for provider.AttributeRegistry deliberately: the
// registry's per-slot cache would answer the second fetch itself and hide a
// missing memo, and that cache is exactly the mechanism this memo does not rely
// on — it expires on a TTL, and a TTL that expires mid-enumeration is the
// inconsistency the memo exists to prevent.
type countingDirectory struct {
	mu             sync.Mutex
	principalCalls int
	accountCalls   int
}

func (d *countingDirectory) Attributes(_ context.Context, _, principal string) (map[string]any, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.principalCalls++
	if principal == "alice" {
		return map[string]any{"tier": "gold"}, nil
	}
	return nil, nil
}

func (d *countingDirectory) AccountAttributes(_ context.Context, account string) (map[string]any, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.accountCalls++
	if account == acctAcme {
		return map[string]any{"plan": "enterprise"}, nil
	}
	return nil, nil
}

func (d *countingDirectory) counts() (int, int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.principalCalls, d.accountCalls
}

// memoFixture wires a rule-backed inclusive grant over SEVERAL objects, with a
// counting directory serving both the principal and the account slot. The rule
// reads one field from each bag, so every evaluation needs both.
func memoFixture(t *testing.T) (*Engine, *countingDirectory) {
	t.Helper()
	ctx := context.Background()
	store := memory.New()
	if err := store.Setup(ctx); err != nil {
		t.Fatalf("setup: %v", err)
	}
	mustSeed(t, store.PutAccount(ctx, model.Account{ID: acctAcme, Name: acctAcme}))
	mustSeed(t, store.PutObjectType(ctx, model.ObjectType{Name: "document", Actions: []string{"read"}}))
	mustSeed(t, store.PutPermission(ctx, model.Permission{
		ID: "p-memo", ObjectType: "document", Action: "read", ScopeStrategy: "inclusive;rule=entitled",
	}))
	mustSeed(t, store.PutPrincipal(ctx, model.Principal{
		ID: "alice", Kind: model.PrincipalUser, Identity: "user:alice",
	}))
	mustSeed(t, store.PutGrant(ctx, model.Grant{
		ID: "g-memo", AccountID: acctAcme, Subject: model.Subject{Kind: model.SubjectPrincipal, ID: "alice"},
		PermissionID: "p-memo", Object: "account:acme/**", Effect: model.EffectAllow,
	}))

	md := make(map[string]provider.Metadata, 8)
	for _, id := range memoObjects {
		md[id] = provider.Metadata{"level": "open"}
	}
	reg := provider.NewRegistry()
	reg.MustRegister("document", metaProvider{md: md})

	dir := &countingDirectory{}
	rulesEng := rules.NewEngine(rules.MapSource{"entitled": {Name: "entitled", AST: rules.And(
		rules.Compare(rules.OpEq, rules.Var("principal.tier"), rules.Lit("gold")),
		rules.Compare(rules.OpEq, rules.Var("account.plan"), rules.Lit("enterprise")),
	)}}, reg, rules.WithPrincipalResolver(dir), rules.WithAccountResolver(dir))

	eng := New(store, WithScopeResolution(scope.DefaultRegistry(), ScopeDeps{Lister: reg, Rules: rulesEng}))
	return eng, dir
}

// memoObjects is deliberately more than a couple: a per-object fetch is only
// distinguishable from a per-decision one when the object count and the call
// count could differ.
var memoObjects = []string{
	"account:acme/document:1",
	"account:acme/document:2",
	"account:acme/document:3",
	"account:acme/document:4",
	"account:acme/document:5",
}

// TestEnumerateResolvesTheAttributesOnce is the story's headline case, and the
// worst fan-out Aperture has: one enumeration over five objects, one principal
// fetch, one account fetch.
func TestEnumerateResolvesTheAttributesOnce(t *testing.T) {
	eng, dir := memoFixture(t)

	ids, err := eng.Enumerate(context.Background(), EnumerateRequest{
		Account: acctAcme, Principal: "alice", Action: "read", Pattern: "account:acme/document:*",
	})
	if err != nil {
		t.Fatalf("Enumerate: %v", err)
	}
	if len(ids) != len(memoObjects) {
		t.Fatalf("Enumerate returned %d objects, want %d — the rule must select every one",
			len(ids), len(memoObjects))
	}
	principal, account := dir.counts()
	if principal != 1 || account != 1 {
		t.Fatalf("enumerating %d objects resolved principal=%d account=%d, want 1 and 1",
			len(memoObjects), principal, account)
	}
}

// TestCheckAndExplainResolveTheAttributesOnce covers the other two decision
// boundaries. Each is ONE decision and so pays one resolution — and two separate
// decisions pay separately, because the scope is per decision and a memo that
// outlived one would be a cache with no expiry.
func TestCheckAndExplainResolveTheAttributesOnce(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*Engine) error
	}{
		{"Check", func(e *Engine) error {
			res, err := e.Check(context.Background(), Request{
				Account: acctAcme, Principal: "alice", Action: "read", Object: memoObjects[0],
			})
			if err == nil && !res.Allow {
				t.Error("the rule selects; the check must allow")
			}
			return err
		}},
		{"Explain", func(e *Engine) error {
			tr, err := e.Explain(context.Background(), Request{
				Account: acctAcme, Principal: "alice", Action: "read", Object: memoObjects[0],
			})
			if err == nil && !tr.Decision.Allow {
				t.Errorf("the rule selects; the trace must allow\n%s", tr.String())
			}
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eng, dir := memoFixture(t)
			if err := tc.run(eng); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if principal, account := dir.counts(); principal != 1 || account != 1 {
				t.Fatalf("one %s resolved principal=%d account=%d, want 1 and 1", tc.name, principal, account)
			}
			if err := tc.run(eng); err != nil {
				t.Fatalf("second %s: %v", tc.name, err)
			}
			if principal, account := dir.counts(); principal != 2 || account != 2 {
				t.Fatalf("a second %s resolved principal=%d account=%d, want 2 and 2 — "+
					"the memo is per decision, not a cache", tc.name, principal, account)
			}
		})
	}
}
