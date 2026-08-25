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

// E1-S3, end to end: a rule reads `principal.tier` off a real attribute provider,
// and the principal's KIND is what picks which provider answers.
//
// It is proven through Check rather than through rules.Engine.Selected because
// the kind is not something a rule-level test can supply honestly — it comes off
// the principal's stored record, is carried on the GrantContext, and reaches the
// resolver only if every layer in between passes it along. Check is the only
// place all of that is real.

// tierFixture wires the whole stack: a storage-backed model, an attribute
// registry whose user and machine slots hold DIFFERENT tiers for the same ids, a
// rules engine resolving the registry as its PrincipalResolver, and a grant whose
// permission selects the rule-backed inclusive strategy.
//
// withMachineDirectory=false leaves the machine slot unregistered, which is the
// ordinary deployment that has a human directory and nothing else.
func tierFixture(t *testing.T, withMachineDirectory bool) (*Engine, context.Context) {
	t.Helper()
	ctx := context.Background()
	store := memory.New()
	if err := store.Setup(ctx); err != nil {
		t.Fatalf("setup: %v", err)
	}
	mustSeed(t, store.PutAccount(ctx, model.Account{ID: acctAcme, Name: acctAcme}))
	mustSeed(t, store.PutObjectType(ctx, model.ObjectType{Name: "document", Actions: []string{"read"}}))
	mustSeed(t, store.PutPermission(ctx, model.Permission{
		ID: "p-tier", ObjectType: "document", Action: "read", ScopeStrategy: "inclusive;rule=tiered",
	}))
	for _, p := range []model.Principal{
		{ID: "alice", Kind: model.PrincipalUser, Identity: "user:alice"},
		{ID: "bob", Kind: model.PrincipalUser, Identity: "user:bob"},
		{ID: "svc-1", Kind: model.PrincipalMachine, Identity: "machine:svc-1"},
	} {
		mustSeed(t, store.PutPrincipal(ctx, p))
		mustSeed(t, store.PutGrant(ctx, model.Grant{
			ID: "g-" + p.ID, AccountID: acctAcme,
			Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: p.ID},
			PermissionID: "p-tier", Object: "account:acme/**", Effect: model.EffectAllow,
		}))
	}

	attrs := provider.NewAttributeRegistry()
	users, err := provider.NewStaticAttributes([]provider.AttributeRecord{
		{ID: "alice", Attributes: provider.Metadata{"tier": "gold"}},
		{ID: "bob", Attributes: provider.Metadata{"tier": "bronze"}},
		// The machine principal's id also exists in the HUMAN directory, with a
		// tier that would allow it. If the kind did not pick the slot, the
		// unregistered-machine case below would silently pass for the wrong reason.
		{ID: "svc-1", Attributes: provider.Metadata{"tier": "gold"}},
	})
	if err != nil {
		t.Fatalf("user directory: %v", err)
	}
	attrs.MustRegister(provider.AttributeSlotUser, users)
	if withMachineDirectory {
		machines, err := provider.NewStaticAttributes([]provider.AttributeRecord{
			{ID: "svc-1", Attributes: provider.Metadata{"tier": "platinum"}},
		})
		if err != nil {
			t.Fatalf("machine directory: %v", err)
		}
		attrs.MustRegister(provider.AttributeSlotMachine, machines)
	}

	rulesEng := rules.NewEngine(
		rules.MapSource{"tiered": {Name: "tiered", AST: rules.Compare(
			rules.OpIn, rules.Var("principal.tier"),
			rules.List(rules.Lit("gold"), rules.Lit("platinum")))}},
		nil, rules.WithPrincipalResolver(attrs))
	return New(store, WithScopeResolution(scope.DefaultRegistry(), ScopeDeps{Rules: rulesEng})), ctx
}

func checkTier(t *testing.T, eng *Engine, ctx context.Context, principal string) bool {
	t.Helper()
	res, err := eng.Check(ctx, Request{
		Account: acctAcme, Principal: principal, Action: "read", Object: "account:acme/document:1",
	})
	if err != nil {
		t.Fatalf("Check(%s) must not fail (code %s): %v", principal, aerr.CodeOf(err), err)
	}
	return res.Allow
}

// TestARuleReadsThePrincipalsAttributes is the acceptance criterion: the same
// rule, the same grant, two principals — allowed or denied purely on what the
// host directory says about them.
func TestARuleReadsThePrincipalsAttributes(t *testing.T) {
	eng, ctx := tierFixture(t, true)

	if !checkTier(t, eng, ctx, "alice") {
		t.Error("alice is gold in the user directory; the rule must select and the check must allow")
	}
	if checkTier(t, eng, ctx, "bob") {
		t.Error("bob is bronze; the rule must not select and the check must deny")
	}
}

// TestThePrincipalKindPicksTheDirectory proves the kind reaches the resolver
// through the real decision path. svc-1 is platinum in the machine directory and
// gold in the human one — both allow, so the discriminating case is the fixture
// WITHOUT a machine directory: if the kind were ignored, svc-1 would be answered
// out of the user slot and allowed.
func TestThePrincipalKindPicksTheDirectory(t *testing.T) {
	wired, ctx := tierFixture(t, true)
	if !checkTier(t, wired, ctx, "svc-1") {
		t.Error("svc-1 is platinum in the machine directory; the check must allow")
	}

	unwired, ctx := tierFixture(t, false)
	if checkTier(t, unwired, ctx, "svc-1") {
		t.Error("with no machine directory, svc-1 has no tier at all; " +
			"an allow means the user directory answered for a machine")
	}
}

// TestAnUnregisteredSlotIsNotANonDecision is the leniency case this story owns. A
// deployment that wires a user directory and nothing else must keep deciding
// normally for BOTH kinds: the machine principal denies (it has no tier), and the
// human principals are untouched. A coded error here would be a non-decision, and
// the fail-closed facade would turn a wiring gap into a silent outage.
func TestAnUnregisteredSlotIsNotANonDecision(t *testing.T) {
	eng, ctx := tierFixture(t, false)

	if checkTier(t, eng, ctx, "svc-1") {
		t.Error("a machine principal with no directory must deny, not allow")
	}
	if !checkTier(t, eng, ctx, "alice") {
		t.Error("the registered user slot still answers")
	}
	if checkTier(t, eng, ctx, "bob") {
		t.Error("bob is still bronze")
	}
}
