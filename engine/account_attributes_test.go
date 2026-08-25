package engine

import (
	"context"
	"testing"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/provider"
	"github.com/frankbardon/aperture/rules"
	"github.com/frankbardon/aperture/scope"
	"github.com/frankbardon/aperture/storage/memory"
)

// E2-S1, end to end: a rule reads `account.plan` off a real attribute provider,
// and the ACTIVE account is what picks which bag answers.
//
// It is proven through Check rather than through rules.Engine.Selected for the
// same reason the principal test is: the active account is Request.Account, it
// reaches the rules engine only by travelling through scope.GrantContext, and
// Check is the only place all of that is real. The discriminating fixture is one
// principal who is a member of TWO accounts and holds the same grant in both —
// so the only thing that can differ between the two decisions is the tenancy.

const acctGlobex = "globex"

// planFixture wires the whole stack: two accounts, one principal enrolled in
// both, one rule-backed permission, and an account directory that puts the two
// accounts on different plans.
//
// withAccountDirectory=false leaves the account slot unregistered, which is the
// ordinary deployment that has no tenant directory at all.
func planFixture(t *testing.T, withAccountDirectory bool) (*Engine, context.Context) {
	t.Helper()
	ctx := context.Background()
	store := memory.New()
	if err := store.Setup(ctx); err != nil {
		t.Fatalf("setup: %v", err)
	}
	mustSeed(t, store.PutAccount(ctx, model.Account{ID: acctAcme, Name: acctAcme}))
	mustSeed(t, store.PutAccount(ctx, model.Account{ID: acctGlobex, Name: acctGlobex}))
	mustSeed(t, store.PutObjectType(ctx, model.ObjectType{Name: "document", Actions: []string{"read"}}))
	mustSeed(t, store.PutPermission(ctx, model.Permission{
		ID: "p-plan", ObjectType: "document", Action: "read", ScopeStrategy: "inclusive;rule=paid",
	}))
	mustSeed(t, store.PutPrincipal(ctx, model.Principal{
		ID: "alice", Kind: model.PrincipalUser, Identity: "user:alice",
	}))
	// The SAME principal, the SAME grant shape, in both accounts. Nothing in the
	// model distinguishes the two decisions; only the account bag can.
	for _, acct := range []string{acctAcme, acctGlobex} {
		mustSeed(t, store.PutMembership(ctx, model.Membership{PrincipalID: "alice", AccountID: acct}))
		mustSeed(t, store.PutGrant(ctx, model.Grant{
			ID: "g-" + acct, AccountID: acct,
			Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: "alice"},
			PermissionID: "p-plan", Object: "account:" + acct + "/**", Effect: model.EffectAllow,
		}))
	}

	attrs := provider.NewAttributeRegistry()
	if withAccountDirectory {
		accounts, err := provider.NewStaticAttributes([]provider.AttributeRecord{
			{ID: acctAcme, Attributes: provider.Metadata{"plan": "enterprise"}},
			{ID: acctGlobex, Attributes: provider.Metadata{"plan": "free"}},
		})
		if err != nil {
			t.Fatalf("account directory: %v", err)
		}
		attrs.MustRegister(provider.AttributeSlotAccount, accounts)
	}

	rulesEng := rules.NewEngine(
		rules.MapSource{"paid": {Name: "paid", AST: rules.Compare(
			rules.OpEq, rules.Var("account.plan"), rules.Lit("enterprise"))}},
		nil, rules.WithAccountResolver(attrs))
	return New(store, WithScopeResolution(scope.DefaultRegistry(), ScopeDeps{Rules: rulesEng})), ctx
}

func checkPlan(t *testing.T, eng *Engine, ctx context.Context, account string) bool {
	t.Helper()
	res, err := eng.Check(ctx, Request{
		Account: account, Principal: "alice", Action: "read",
		Object: "account:" + account + "/document:1",
	})
	if err != nil {
		t.Fatalf("Check(%s) must not fail (code %s): %v", account, aerr.CodeOf(err), err)
	}
	return res.Allow
}

// TestARuleReadsTheAccountsAttributes is the story's acceptance criterion: the
// same rule, the same principal, the same grant shape — allowed in one tenancy and
// denied in the other, purely on what the host directory says about the account.
func TestARuleReadsTheAccountsAttributes(t *testing.T) {
	eng, ctx := planFixture(t, true)

	if !checkPlan(t, eng, ctx, acctAcme) {
		t.Error("acme is on the enterprise plan; the rule must select and the check must allow")
	}
	if checkPlan(t, eng, ctx, acctGlobex) {
		t.Error("globex is on the free plan; the rule must not select and the check must deny")
	}
}

// TestAnUnregisteredAccountSlotIsNotANonDecision is the leniency case. A
// deployment with no tenant directory must keep deciding: every account evaluates
// against the floor, the rule finds no plan, and the check denies. A coded error
// here would be a non-decision, and the fail-closed facade would turn a wiring gap
// into a silent outage.
func TestAnUnregisteredAccountSlotIsNotANonDecision(t *testing.T) {
	eng, ctx := planFixture(t, false)

	for _, acct := range []string{acctAcme, acctGlobex} {
		if checkPlan(t, eng, ctx, acct) {
			t.Errorf("account %q has no directory and so no plan; the check must deny", acct)
		}
	}
}
