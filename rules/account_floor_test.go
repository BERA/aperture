package rules

import (
	"context"
	"testing"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/identity"
	"github.com/frankbardon/aperture/provider"
)

// E2-S1: `account` stops being an empty literal. It is resolved from the ACTIVE
// account through the attribute registry's account slot, with `{id}` as its floor.
//
// The assertion below is the story's structural criterion, expressed the only way
// that cannot rot: a *provider.AttributeRegistry IS an AccountResolver, with
// provider importing nothing from here. The SAME registry already satisfies
// PrincipalResolver (principal_floor_test.go), which is why the two seams spell
// their methods differently — if either side's signature drifts, this file stops
// compiling.
var _ AccountResolver = (*provider.AttributeRegistry)(nil)

// accountResolverFunc adapts a plain function to AccountResolver.
type accountResolverFunc func(ctx context.Context, account string) (map[string]any, error)

func (f accountResolverFunc) AccountAttributes(ctx context.Context, account string) (map[string]any, error) {
	return f(ctx, account)
}

// accountFixture builds a registry whose ACCOUNT slot is filled from the given
// bags. A nil map leaves the slot unregistered, which is the wiring gap the
// leniency case is about.
func accountFixture(t *testing.T, accounts map[string]provider.Metadata) *provider.AttributeRegistry {
	t.Helper()
	reg := provider.NewAttributeRegistry()
	if accounts == nil {
		return reg
	}
	records := make([]provider.AttributeRecord, 0, len(accounts))
	for id, md := range accounts {
		records = append(records, provider.AttributeRecord{ID: id, Attributes: md})
	}
	p, err := provider.NewStaticAttributes(records)
	if err != nil {
		t.Fatalf("static account attributes: %v", err)
	}
	reg.MustRegister(provider.AttributeSlotAccount, p)
	return reg
}

// TestTheActiveAccountPicksTheBag is the point of the story: `account` is no
// longer a literal empty map, and which bag it holds is decided by the account the
// decision is being made IN. The same rule, the same object, the same principal —
// two accounts, two verdicts.
func TestTheActiveAccountPicksTheBag(t *testing.T) {
	reg := accountFixture(t, map[string]provider.Metadata{
		"acme":   {"plan": "enterprise"},
		"globex": {"plan": "free"},
	})
	eng := NewEngine(
		MapSource{"paid": {AST: Compare(OpEq, Var("account.plan"), Lit("enterprise"))}},
		nil, WithAccountResolver(reg))
	ctx := context.Background()
	obj := identity.MustParse(anyObject)

	if ok, err := eng.Selected(ctx, "paid", obj, "acme", "user", "alice", "read"); err != nil || !ok {
		t.Fatalf("acme is on the enterprise plan, so the rule must select (ok=%v err=%v)", ok, err)
	}
	if ok, err := eng.Selected(ctx, "paid", obj, "globex", "user", "alice", "read"); err != nil || ok {
		t.Fatalf("globex is on the free plan, so the rule must not select (ok=%v err=%v)", ok, err)
	}
}

// TestAnUnregisteredAccountSlotStillDecides is the leniency this story owes,
// mirroring E1-S3's for principals: a deployment that wires a user directory and
// no account directory keeps deciding, against the floor.
func TestAnUnregisteredAccountSlotStillDecides(t *testing.T) {
	reg := accountFixture(t, nil)
	if reg.Has(provider.AttributeSlotAccount) {
		t.Fatal("the fixture registered an account provider; the leniency case needs it absent")
	}
	eng := NewEngine(
		MapSource{"paid": {AST: Compare(OpEq, Var("account.plan"), Lit("enterprise"))}},
		nil, WithAccountResolver(reg))

	ok, err := eng.Selected(context.Background(), "paid", identity.MustParse(anyObject),
		"acme", "user", "alice", "read")
	if err != nil {
		t.Fatalf("an unregistered account slot must not be an error (code %s): %v", aerr.CodeOf(err), err)
	}
	if ok {
		t.Error("an account with no directory has no plan; the rule must not select")
	}
}

// TestAnUnknownAccountStillDecides is the other half of the same contract: a
// registered directory that simply has no record for this account answers
// APERTURE_NOT_FOUND, and that is a tenant with no attributes, not a non-decision.
func TestAnUnknownAccountStillDecides(t *testing.T) {
	reg := accountFixture(t, map[string]provider.Metadata{"acme": {"plan": "enterprise"}})

	bag, err := reg.AccountAttributes(context.Background(), "globex")
	if err != nil {
		t.Fatalf("an unknown account must not error (code %s): %v", aerr.CodeOf(err), err)
	}
	if len(bag) != 0 {
		t.Errorf("an unknown account resolved a bag %v, want nothing", bag)
	}
}

// TestTheAccountFloorIsTheID pins what a rule can rely on with no host wiring at
// all: `account.id`, and nothing else. The floor is deliberately NOT {id, kind} —
// there is one account slot, so a kind key would be the same constant in every bag
// in every deployment.
func TestTheAccountFloorIsTheID(t *testing.T) {
	contributed, err := floorAccount{}.AccountAttributes(context.Background(), "acme")
	if err != nil {
		t.Fatalf("floorAccount.AccountAttributes: %v", err)
	}
	if len(contributed) != 0 {
		t.Errorf("the default resolver contributed %v, want nothing", contributed)
	}
	bag := accountBag(contributed, "acme")
	if len(bag) != 1 || bag["id"] != "acme" {
		t.Fatalf("unwired account = %v, want exactly {id}", bag)
	}

	// And end to end, through Selected, with no resolver wired at all — which is
	// also the proof that Selected really builds the floor rather than the empty
	// literal it used to pass.
	eng := NewEngine(MapSource{"here": {AST: Compare(OpEq, Var("account.id"), Lit("acme"))}}, nil)
	ctx := context.Background()
	obj := identity.MustParse(anyObject)
	if ok, err := eng.Selected(ctx, "here", obj, "acme", "user", "alice", "read"); err != nil || !ok {
		t.Fatalf("account.id must be the active account (ok=%v err=%v)", ok, err)
	}
	if ok, err := eng.Selected(ctx, "here", obj, "globex", "user", "alice", "read"); err != nil || ok {
		t.Fatalf("account.id must not match a different account (ok=%v err=%v)", ok, err)
	}
}

// TestTheAccountFloorIsNotShadowedByTheProviderBag pins the collision rule. A
// host account table with its own internal `id` column is an ordinary accident; if
// it could shadow the floor, `account.id == object.account` would silently start
// comparing a surrogate key and answer a different question with no error anywhere.
func TestTheAccountFloorIsNotShadowedByTheProviderBag(t *testing.T) {
	reg := accountFixture(t, map[string]provider.Metadata{
		"acme": {"id": "row-4711", "plan": "enterprise"},
	})
	eng := NewEngine(
		MapSource{"here": {AST: Compare(OpEq, Var("account.id"), Lit("acme"))}},
		nil, WithAccountResolver(reg))

	ok, err := eng.Selected(context.Background(), "here", identity.MustParse(anyObject),
		"acme", "user", "alice", "read")
	if err != nil {
		t.Fatalf("Selected: %v", err)
	}
	if !ok {
		t.Error("the directory's own id column shadowed account.id")
	}
}

// TestTheAccountBagIsBuiltFresh guards the read-only-transitive contract at the
// account slot's blast radius: one cached bag is shared by every decision the
// tenant is making at once, so stamping the floor into it would be a write through
// a value nobody may write.
func TestTheAccountBagIsBuiltFresh(t *testing.T) {
	shared := map[string]any{"plan": "enterprise"}
	eng := NewEngine(
		MapSource{"paid": {AST: Compare(OpEq, Var("account.plan"), Lit("enterprise"))}},
		nil, WithAccountResolver(accountResolverFunc(
			func(context.Context, string) (map[string]any, error) { return shared, nil })))

	// Two evaluations, so a bag mutated in place would also be observed by the
	// second — the real-world shape of the bug.
	for _, account := range []string{"acme", "globex"} {
		if ok, err := eng.Selected(context.Background(), "paid", identity.MustParse(anyObject),
			account, "user", "alice", "read"); err != nil || !ok {
			t.Fatalf("account %q: Selected (ok=%v err=%v)", account, ok, err)
		}
	}
	if len(shared) != 1 || shared["plan"] != "enterprise" {
		t.Fatalf("the resolver's own bag was written through: %v", shared)
	}

	bag := accountBag(shared, "acme")
	bag["plan"] = "free"
	if shared["plan"] != "enterprise" {
		t.Error("the evaluation bag aliases the resolver's map")
	}
}

// TestTheWildcardNeverReachesTheResolver is the load-bearing case. "*" is the
// all-accounts grant sentinel, not an account, and it is a LIVE active-account
// value (platform-tier authority is anchored there). Two things must hold, and
// they are two different statements:
//
//  1. the engine never asks a resolver about it — a decision at platform scope
//     sees the floor and proceeds, so a rule-backed grant guarding the system
//     anchor stays decidable; and
//  2. the seam that WOULD fetch refuses it with a coded error rather than serving
//     one tenant's bag as every tenant's.
func TestTheWildcardNeverReachesTheResolver(t *testing.T) {
	t.Run("the engine does not ask", func(t *testing.T) {
		asked := false
		eng := NewEngine(
			MapSource{"here": {AST: Compare(OpEq, Var("account.id"), Lit("*"))}},
			nil, WithAccountResolver(accountResolverFunc(
				func(_ context.Context, account string) (map[string]any, error) {
					asked = true
					return map[string]any{"plan": "enterprise"}, nil
				})))

		ok, err := eng.Selected(context.Background(), "here", identity.MustParse(anyObject),
			"*", "user", "root", "read")
		if err != nil {
			t.Fatalf("a platform-scope decision must still decide (code %s): %v", aerr.CodeOf(err), err)
		}
		if asked {
			t.Error("the resolver was asked for the wildcard account")
		}
		if !ok {
			t.Error("account.id must still be the floor's id at platform scope")
		}
	})

	t.Run("the engine ignores a wired directory's wildcard bag", func(t *testing.T) {
		// A host that declares a "*" bag anyway must not have it read: a global
		// attribute set applying in every tenancy is precisely what the isolation
		// invariant exists to prevent.
		eng := NewEngine(
			MapSource{"paid": {AST: Compare(OpEq, Var("account.plan"), Lit("enterprise"))}},
			nil, WithAccountResolver(accountResolverFunc(
				func(context.Context, string) (map[string]any, error) {
					return map[string]any{"plan": "enterprise"}, nil
				})))
		if ok, err := eng.Selected(context.Background(), "paid", identity.MustParse(anyObject),
			"*", "user", "root", "read"); err != nil || ok {
			t.Fatalf("no host bag may be read for the wildcard (ok=%v err=%v)", ok, err)
		}
	})

	t.Run("the registry refuses it", func(t *testing.T) {
		reg := accountFixture(t, map[string]provider.Metadata{"acme": {"plan": "enterprise"}})
		_, err := reg.AccountAttributes(context.Background(), "*")
		if got := aerr.CodeOf(err); got != aerr.APERTURE_ATTRIBUTE_PROVIDER_INVALID {
			t.Fatalf("AccountAttributes(\"*\") code = %q, want %q",
				got, aerr.APERTURE_ATTRIBUTE_PROVIDER_INVALID)
		}
	})
}
