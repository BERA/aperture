package rules

import (
	"context"
	"testing"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/identity"
	"github.com/frankbardon/aperture/provider"
)

// E1-S3: the principal's kind picks the attribute provider, and the floor bag is
// {id, kind}.
//
// The assertion below is the story's first acceptance criterion expressed the
// only way that cannot rot: a *provider.AttributeRegistry IS a PrincipalResolver,
// structurally, with provider importing nothing from here. If either side's
// signature drifts, this file stops compiling.
var _ PrincipalResolver = (*provider.AttributeRegistry)(nil)

const anyObject = "document:1"

// attributeFixture builds a registry whose user and machine slots are filled from
// the given records. A nil map for a slot leaves that slot UNREGISTERED, which is
// the wiring gap the leniency case is about.
func attributeFixture(t *testing.T, users, machines map[string]provider.Metadata) *provider.AttributeRegistry {
	t.Helper()
	reg := provider.NewAttributeRegistry()
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
			t.Fatalf("static attributes for %s: %v", slot, err)
		}
		reg.MustRegister(slot, p)
	}
	register(provider.AttributeSlotUser, users)
	register(provider.AttributeSlotMachine, machines)
	return reg
}

// TestThePrincipalKindPicksTheProvider is the point of the whole story. The same
// principal id is declared in BOTH directories with a different tier, so the only
// thing that can decide which bag a rule reads is the kind — and it does.
func TestThePrincipalKindPicksTheProvider(t *testing.T) {
	reg := attributeFixture(t,
		map[string]provider.Metadata{"shared": {"tier": "gold"}},
		map[string]provider.Metadata{"shared": {"tier": "bronze"}},
	)
	eng := NewEngine(
		MapSource{"gold": {AST: Compare(OpEq, Var("principal.tier"), Lit("gold"))}},
		nil, WithPrincipalResolver(reg))
	ctx := context.Background()
	obj := identity.MustParse(anyObject)

	if ok, err := eng.Selected(ctx, "gold", obj, "acme", "user", "shared", "read"); err != nil || !ok {
		t.Fatalf("the user slot says gold, so the rule must select (ok=%v err=%v)", ok, err)
	}
	if ok, err := eng.Selected(ctx, "gold", obj, "acme", "machine", "shared", "read"); err != nil || ok {
		t.Fatalf("the machine slot says bronze, so the rule must not select (ok=%v err=%v)", ok, err)
	}
}

// TestAnUnregisteredSlotStillDecides is the leniency case this story owns. A
// deployment that wires a user directory and no machine directory must keep
// deciding: its machine principals evaluate against the floor, and a rule reading
// an attribute they do not have denies rather than erroring.
func TestAnUnregisteredSlotStillDecides(t *testing.T) {
	reg := attributeFixture(t, map[string]provider.Metadata{"alice": {"tier": "gold"}}, nil)
	if reg.Has(provider.AttributeSlotMachine) {
		t.Fatal("the fixture registered a machine provider; the leniency case needs it absent")
	}
	eng := NewEngine(
		MapSource{"gold": {AST: Compare(OpEq, Var("principal.tier"), Lit("gold"))}},
		nil, WithPrincipalResolver(reg))
	ctx := context.Background()
	obj := identity.MustParse(anyObject)

	ok, err := eng.Selected(ctx, "gold", obj, "acme", "machine", "svc-1", "read")
	if err != nil {
		t.Fatalf("an unregistered slot must not be an error (code %s): %v", aerr.CodeOf(err), err)
	}
	if ok {
		t.Error("a machine principal with no directory has no tier; the rule must not select")
	}

	// And the wired half is unaffected.
	if ok, err := eng.Selected(ctx, "gold", obj, "acme", "user", "alice", "read"); err != nil || !ok {
		t.Fatalf("the registered user slot still answers (ok=%v err=%v)", ok, err)
	}
}

// TestAnUnknownKindResolvesNoSlot covers the empty kind, which is a live decision
// path rather than a hypothetical: an entry point that never had the principal's
// record in hand reports no kind at all. It must decide, and it must NOT fall back
// to a directory — an unknown kind answered out of the user slot is exactly the
// substitution PrincipalResolver forbids. "account" is refused for the same
// reason even though it names a real slot: it is not a principal kind.
func TestAnUnknownKindResolvesNoSlot(t *testing.T) {
	reg := attributeFixture(t, map[string]provider.Metadata{"alice": {"tier": "gold"}}, nil)
	for _, kind := range []string{"", "account", "robot"} {
		bag, err := reg.Attributes(context.Background(), kind, "alice")
		if err != nil {
			t.Fatalf("kind %q: unknown kinds must not error: %v", kind, err)
		}
		if len(bag) != 0 {
			t.Errorf("kind %q resolved a bag %v; alice's user record must not answer for it", kind, bag)
		}
	}
}

// TestTheFloorBagIsIDAndKind pins what a rule can rely on with no host wiring at
// all, and that it survives a resolver that answers.
func TestTheFloorBagIsIDAndKind(t *testing.T) {
	t.Run("unwired", func(t *testing.T) {
		contributed, err := floorPrincipal{}.Attributes(context.Background(), "machine", "svc-1")
		if err != nil {
			t.Fatalf("floorPrincipal.Attributes: %v", err)
		}
		bag := principalBag(contributed, "machine", "svc-1")
		if len(bag) != 2 || bag["id"] != "svc-1" || bag["kind"] != "machine" {
			t.Fatalf("unwired principal = %v, want exactly {id, kind}", bag)
		}
	})

	t.Run("merged over a provider bag", func(t *testing.T) {
		reg := attributeFixture(t, map[string]provider.Metadata{"alice": {"tier": "gold"}}, nil)
		attrs, err := reg.Attributes(context.Background(), "user", "alice")
		if err != nil {
			t.Fatalf("Attributes: %v", err)
		}
		bag := principalBag(attrs, "user", "alice")
		if bag["id"] != "alice" || bag["kind"] != "user" || bag["tier"] != "gold" {
			t.Fatalf("principal = %v, want the provider's tier with id and kind present", bag)
		}
	})
}

// TestARuleCanBranchOnThePrincipalKind is the reason kind is published: per-kind
// providers make a rule silently kind-dependent, and this is the only way an
// author can say so. It compares as a plain string, with no host wiring — which
// is also the end-to-end proof that Selected really builds the floor.
func TestARuleCanBranchOnThePrincipalKind(t *testing.T) {
	eng := NewEngine(
		MapSource{"humans": {AST: Compare(OpEq, Var("principal.kind"), Lit("user"))}}, nil)
	ctx := context.Background()
	obj := identity.MustParse(anyObject)

	if ok, err := eng.Selected(ctx, "humans", obj, "acme", "user", "alice", "read"); err != nil || !ok {
		t.Fatalf("a user principal should match principal.kind == %q (ok=%v err=%v)", "user", ok, err)
	}
	if ok, err := eng.Selected(ctx, "humans", obj, "acme", "machine", "svc-1", "read"); err != nil || ok {
		t.Fatalf("a machine principal should not (ok=%v err=%v)", ok, err)
	}
	// The id half of the floor, through the same path.
	idEng := NewEngine(MapSource{"me": {AST: Compare(OpEq, Var("principal.id"), Lit("alice"))}}, nil)
	if ok, err := idEng.Selected(ctx, "me", obj, "acme", "user", "alice", "read"); err != nil || !ok {
		t.Fatalf("principal.id must still be the floor's id (ok=%v err=%v)", ok, err)
	}
}

// TestTheFloorIsNotShadowedByTheProviderBag pins the collision rule. A directory
// with its own "id" or "kind" column is an ordinary accident, and if it could
// shadow the floor then `principal.id == object.owner` would silently start
// comparing something else — an authorization change with no error anywhere.
func TestTheFloorIsNotShadowedByTheProviderBag(t *testing.T) {
	reg := attributeFixture(t,
		map[string]provider.Metadata{"alice": {"id": "row-4711", "kind": "superuser"}}, nil)
	eng := NewEngine(
		MapSource{"me": {AST: Compare(OpEq, Var("principal.id"), Lit("alice"))}},
		nil, WithPrincipalResolver(reg))
	ok, err := eng.Selected(context.Background(), "me", identity.MustParse(anyObject),
		"acme", "user", "alice", "read")
	if err != nil {
		t.Fatalf("Selected: %v", err)
	}
	if !ok {
		t.Error("the directory's own id column shadowed principal.id")
	}

	attrs, err := reg.Attributes(context.Background(), "user", "alice")
	if err != nil {
		t.Fatalf("Attributes: %v", err)
	}
	bag := principalBag(attrs, "user", "alice")
	if bag["kind"] != "user" {
		t.Errorf("principal.kind = %v, want the kind the decision carried", bag["kind"])
	}
}

// TestThePrincipalBagIsBuiltFresh guards the read-only-transitive contract at its
// widest blast radius: the resolver's map is cached and shared across every object
// in the decision and every concurrent decision for the same principal, so
// stamping the floor into it would be a write through a value nobody may write.
func TestThePrincipalBagIsBuiltFresh(t *testing.T) {
	shared := map[string]any{"tier": "gold"}
	eng := NewEngine(
		MapSource{"gold": {AST: Compare(OpEq, Var("principal.tier"), Lit("gold"))}},
		nil, WithPrincipalResolver(principalResolverFunc(
			func(context.Context, string, string) (map[string]any, error) { return shared, nil })))

	// Two evaluations, so a bag that were mutated in place would also be observed
	// by the second — which is the real-world shape of the bug.
	for _, kind := range []string{"user", "machine"} {
		if ok, err := eng.Selected(context.Background(), "gold", identity.MustParse(anyObject),
			"acme", kind, "alice", "read"); err != nil || !ok {
			t.Fatalf("kind %q: Selected (ok=%v err=%v)", kind, ok, err)
		}
	}
	if len(shared) != 1 || shared["tier"] != "gold" {
		t.Fatalf("the resolver's own bag was written through: %v", shared)
	}

	// And directly: the composed bag is a distinct map, so a later mutation of one
	// cannot reach the other.
	bag := principalBag(shared, "user", "alice")
	bag["tier"] = "bronze"
	if shared["tier"] != "gold" {
		t.Error("the evaluation bag aliases the resolver's map")
	}
}
