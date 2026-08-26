package rules

import (
	"context"
	"errors"
	"strconv"
	"sync/atomic"
	"testing"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/identity"
	"github.com/frankbardon/aperture/provider"
	"github.com/frankbardon/aperture/scope"
)

// E4-S2: the leniency contract, carried through the DECISION path.
//
// provider/attribute_leniency_test.go pins the same two bullets at the registry
// seam. This file pins them where they are actually spent — Engine.Selected, and
// the scope resolvers that consult it — because a contract honoured one layer
// down and lost one layer up is not honoured:
//
//   - NOTHING WAS PROMISED (no provider for the slot, or a registered provider
//     with no record for this key) → the floor bag, and the decision proceeds.
//   - SOMETHING WAS PROMISED AND FAILED → a coded error out of Selected, which
//     every scope resolver treats as a NON-DECISION. There is no select-on-error
//     and no empty-bag fallback.
//
// The last test in this file is different in kind from the rest: it documents an
// accepted RISK rather than a guarantee. Read its comment before changing it.

// brokenDirectory is a provider.AttributeProvider whose every call fails with one
// chosen error, counting the attempts. It is registered into a real
// *provider.AttributeRegistry so the tests below run the whole stack — provider,
// registry, resolver seam, engine — rather than a stand-in for it.
type brokenDirectory struct {
	err   error
	calls atomic.Int64
}

func (d *brokenDirectory) Fetch(context.Context, string) (provider.Metadata, error) {
	d.calls.Add(1)
	return nil, d.err
}

func (d *brokenDirectory) List(context.Context) ([]provider.AttributeRecord, error) {
	return nil, d.err
}

func (d *brokenDirectory) Query(context.Context, provider.AttributeFilter) ([]provider.AttributeRecord, error) {
	return nil, d.err
}

// codedChainDepth counts the Aperture-coded errors in a chain.
//
// The code alone cannot prove a pass-through: aerr.Wrap builds a FRESH
// CodedError with whatever code it is handed and aerr.CodeOf reports the
// outermost, so re-stamping an error with the code it already carries is
// invisible to a code assertion while still burying the original message and its
// structured context a layer down. Depth is the assertion that survives that.
func codedChainDepth(err error) int {
	depth := 0
	for err != nil {
		var ce *aerr.CodedError
		if !errors.As(err, &ce) {
			break
		}
		depth++
		err = errors.Unwrap(ce)
	}
	return depth
}

// fixedLister is a scope.ObjectLister over a fixed candidate set. It exists so a
// test can put MANY objects in front of one decision and observe how often a
// failing directory is consulted.
type fixedLister struct{ objects []identity.Identity }

func (l fixedLister) List(_ context.Context, _ string, pattern identity.Pattern, limit int) ([]identity.Identity, error) {
	out := make([]identity.Identity, 0, len(l.objects))
	for _, obj := range l.objects {
		if !pattern.Matches(obj) {
			continue
		}
		out = append(out, obj)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

// TestAMissingBagIsNotAnOutage is the lenient half at the decision path. Each
// case is a deployment in which the attribute a rule reads simply does not
// exist, and every one of them must produce a VERDICT rather than an error.
//
// The APERTURE_NOT_FOUND cases are the ones this story adds. E1-S3 and E2-S1
// built and pinned the unregistered-slot half; the registered-but-no-record half
// was left collapsing to the floor with nothing asserting that it did, and it is
// the more dangerous of the two — the wiring is right, so nothing about the
// deployment looks wrong.
func TestAMissingBagIsNotAnOutage(t *testing.T) {
	obj := identity.MustParse(anyObject)

	cases := []struct {
		name string
		// engine is the deployment under test: a rule that reads one attribute,
		// and a registry that cannot supply it.
		engine func(t *testing.T) *Engine
		rule   string
	}{
		{
			name: "no principal directory is wired at all",
			rule: "gold",
			engine: func(t *testing.T) *Engine {
				t.Helper()
				return NewEngine(goldAndPaid(), nil,
					WithPrincipalResolver(attributeFixture(t, nil, nil)))
			},
		},
		{
			name: "the principal directory is wired and has no record for this principal",
			rule: "gold",
			engine: func(t *testing.T) *Engine {
				t.Helper()
				return NewEngine(goldAndPaid(), nil,
					WithPrincipalResolver(attributeFixture(t,
						map[string]provider.Metadata{"someone-else": {"tier": "gold"}}, nil)))
			},
		},
		{
			name: "no account directory is wired at all",
			rule: "paid",
			engine: func(t *testing.T) *Engine {
				t.Helper()
				return NewEngine(goldAndPaid(), nil,
					WithAccountResolver(accountFixture(t, nil)))
			},
		},
		{
			name: "the account directory is wired and has no record for this account",
			rule: "paid",
			engine: func(t *testing.T) *Engine {
				t.Helper()
				return NewEngine(goldAndPaid(), nil,
					WithAccountResolver(accountFixture(t,
						map[string]provider.Metadata{"some-other-account": {"plan": "enterprise"}})))
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, err := tc.engine(t).Selected(context.Background(), tc.rule, obj,
				"acme", "user", "alice", "read")
			if err != nil {
				t.Fatalf("a missing bag must not be an error (code %s): %v", aerr.CodeOf(err), err)
			}
			if ok {
				t.Error("the attribute is absent, so the comparison is false and the rule must not select")
			}
		})
	}
}

// TestABrokenProviderIsNotSilence is the strict half at the decision path: a
// source that EXISTS and could not answer produces a coded error out of
// Selected, never a verdict.
//
// Both halves of the returned pair matter. The error is what makes the failure a
// non-decision one layer up; the `false` alongside it is what makes the failure
// safe if a caller ever ignored the error — there is no select-on-error, so a
// broken directory can never turn into an inclusive grant's allow.
//
// Chain depth is asserted rather than the code alone because the code alone
// cannot see a same-code re-stamp, and because the three layers this error
// crosses — provider, registry, engine — are three chances to bury the
// classification the source chose.
func TestABrokenProviderIsNotSilence(t *testing.T) {
	unreachable := errors.New("dial tcp 10.0.0.7:5432: connect: connection refused")
	// An error the source ALREADY classified. Its fixups are about placeholders,
	// reachability and timeouts; a re-stamp anywhere on the way up would replace
	// them with a generic remedy, and the operator would lose the one that works.
	queryFailed := aerr.WithContext(aerr.APERTURE_SQL_PROVIDER_QUERY,
		"sqlprovider: attribute fetch statement failed", map[string]any{"slot": "user"})

	cases := []struct {
		name string
		slot provider.AttributeSlot
		rule string
		err  error
		want aerr.Code
	}{
		{
			name: "an unreachable principal directory",
			slot: provider.AttributeSlotUser,
			rule: "gold",
			err:  unreachable,
			want: aerr.APERTURE_ATTRIBUTE_PROVIDER_FETCH,
		},
		{
			name: "an unreachable account directory",
			slot: provider.AttributeSlotAccount,
			rule: "paid",
			err:  unreachable,
			want: aerr.APERTURE_ATTRIBUTE_PROVIDER_FETCH,
		},
		{
			name: "a principal directory whose query failed keeps its own code",
			slot: provider.AttributeSlotUser,
			rule: "gold",
			err:  queryFailed,
			want: aerr.APERTURE_SQL_PROVIDER_QUERY,
		},
		{
			name: "an account directory whose query failed keeps its own code",
			slot: provider.AttributeSlotAccount,
			rule: "paid",
			err:  queryFailed,
			want: aerr.APERTURE_SQL_PROVIDER_QUERY,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg := provider.NewAttributeRegistry()
			reg.MustRegister(tc.slot, &brokenDirectory{err: tc.err})
			eng := NewEngine(goldAndPaid(), nil,
				WithPrincipalResolver(reg), WithAccountResolver(reg))

			ok, err := eng.Selected(context.Background(), tc.rule, identity.MustParse(anyObject),
				"acme", "user", "alice", "read")
			if err == nil {
				t.Fatal("a broken directory returned no error; an outage must not read as " +
					"'this subject has no attributes'")
			}
			if ok {
				t.Error("Selected reported true alongside an error; there is no select-on-error")
			}
			if code := aerr.CodeOf(err); code != tc.want {
				t.Errorf("code = %s, want %s", code, tc.want)
			}
			if depth := codedChainDepth(err); depth != 1 {
				t.Errorf("%d Aperture-coded errors in the chain, want exactly 1 — the "+
					"provider, the registry and the engine are three chances to re-stamp, "+
					"and a same-code re-stamp is invisible to CodeOf", depth)
			}
			if !errors.Is(err, tc.err) {
				t.Error("the source's own error is no longer reachable through errors.Is")
			}
		})
	}
}

// TestAnAbsenceIsMemoizedAndAFailureIsNot states the memoization half of the
// contract, which is where the two bullets stop being symmetric.
//
// An ABSENCE is a SUCCESSFUL resolution. "The host knows nothing about this
// subject" is a complete answer, it cannot change halfway through a decision
// without the same TTL-straddling inconsistency the memo exists to prevent, and
// the deployment it describes — a machine principal in a shop that wired only a
// user directory — is a steady state rather than a blip. So it is taken ONCE and
// every later evaluation in the decision reads it back: a 1,000-object
// enumeration against an unwired slot performs ONE resolution, not a thousand.
// DecisionAttributes.Principal reports (nil, true) for it, which is exactly the
// distinction E5-S1's floor-only Explain note is built on: a bag was resolved,
// and it was empty.
//
// A FAILURE is not retained, and the reason it can afford not to be is the
// companion test below: the first failure ENDS the decision, so "retry" means at
// most one extra attempt, never one per object. Freezing it instead would let a
// blip that lasted one round-trip decide every remaining grant in the decision.
func TestAnAbsenceIsMemoizedAndAFailureIsNot(t *testing.T) {
	t.Run("an absence is resolved once per decision", func(t *testing.T) {
		var calls atomic.Int64
		// (nil, nil) is precisely what AttributeRegistry.Attributes returns for an
		// unregistered slot or an unknown key — asserted directly below, so this
		// stand-in cannot drift from the thing it stands in for.
		eng := NewEngine(goldAndPaid(), nil, WithPrincipalResolver(principalResolverFunc(
			func(context.Context, string, string) (map[string]any, error) {
				calls.Add(1)
				return nil, nil
			})))
		ctx, attrs := WithDecisionAttributes(context.Background())

		for _, obj := range []string{"document:1", "document:2", "document:3"} {
			ok, err := eng.Selected(ctx, "gold", identity.MustParse(obj), "acme", "user", "alice", "read")
			if err != nil || ok {
				t.Fatalf("%s: selected=%v err=%v, want false/nil", obj, ok, err)
			}
		}
		if n := calls.Load(); n != 1 {
			t.Errorf("the absent directory was consulted %d times, want 1 — an absence is a "+
				"successful resolution and is memoized like any other", n)
		}
		bag, taken := attrs.Principal()
		if !taken {
			t.Error("the memo reports no bag was taken; an absence must memoize as " +
				"'resolved, and empty', which is what a floor-only Explain note reads")
		}
		if len(bag) != 0 {
			t.Errorf("the memoized bag is %v, want an empty one", bag)
		}
	})

	t.Run("the registry really does answer an absence that way", func(t *testing.T) {
		// The anti-drift half of the case above: the stand-in resolver's (nil, nil)
		// is the registry's own answer, not a convenient fiction.
		reg := attributeFixture(t, map[string]provider.Metadata{"someone-else": {"tier": "gold"}}, nil)
		bag, err := reg.Attributes(context.Background(), "user", "alice")
		if err != nil || bag != nil {
			t.Fatalf("Attributes for an unknown key = (%v, %v), want (nil, nil)", bag, err)
		}
	})

	t.Run("a failure is retried, and the retry is bounded by the decision ending", func(t *testing.T) {
		// Fifty candidates, one failing directory, one enumeration. A resolver that
		// were consulted per object would be consulted fifty times; the scope
		// resolvers return on the FIRST error, so it is consulted once.
		//
		// This is the whole argument for not memoizing failures. The cost of the
		// choice is at most one extra attempt per decision, which is not a
		// thundering herd; the cost of the alternative is a one-round-trip blip
		// frozen into every remaining grant of the decision.
		dir := &brokenDirectory{err: errors.New("directory unreachable")}
		reg := provider.NewAttributeRegistry()
		reg.MustRegister(provider.AttributeSlotUser, dir)
		eng := NewEngine(goldAndPaid(), nil, WithPrincipalResolver(reg))

		res, err := scope.DefaultRegistry().Resolve(
			scope.GrantContext{
				Pattern:       identity.MustParsePattern("account:acme/**"),
				ObjectType:    "document",
				Spec:          scope.Spec{Strategy: scope.StrategyInclusive, Rule: "gold"},
				Account:       "acme",
				PrincipalKind: "user",
				Principal:     "alice",
				Action:        "read",
			},
			scope.Deps{Lister: fixedLister{objects: documentIDs(50)}, Rules: eng},
		)
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		ctx, _ := WithDecisionAttributes(context.Background())
		if _, err := res.Members(ctx, identity.MustParsePattern("account:acme/**")); err == nil {
			t.Fatal("an enumeration over a broken directory must fail, not return a short list")
		}
		if n := dir.calls.Load(); n != 1 {
			t.Errorf("the broken directory was consulted %d times for a 50-object "+
				"enumeration, want 1 — the first failure ends the decision", n)
		}
	})
}

// TestAMissingBagWidensAnExclusiveGrant ENCODES AN ACCEPTED RISK. It is not a
// bug report and it is not a test of a guarantee — it is the asymmetry the
// leniency contract deliberately leaves behind, written down in the only form
// that cannot quietly stop being true.
//
// The mechanism: an absent attribute makes every comparison against it false. A
// rule that fails to select is
//
//   - deny-safe in an INCLUSIVE grant, where selection means "covered": nothing
//     is covered, so nothing is allowed; and
//   - access-WIDENING in an EXCLUSIVE grant, where selection means "excluded": a
//     rule that stops selecting stops excluding, and the object the exclusion
//     was written to withhold becomes covered.
//
// Both halves are asserted below against the SAME rule, the same object and the
// same principal, so the only variable is whether the directory has a record.
//
// Do not "fix" this by erroring on an absent bag: that trades a silent widening
// for a deployment that cannot decide at all whenever a slot is unwired, which
// is the outage the first bullet of the contract exists to prevent. The chosen
// mitigation is VISIBILITY, and it ships in E5-S1 as an Explain note saying a bag
// came back floor-only — so an operator reading a surprising allow can see that
// the rule compared against nothing. Until then, `principal.kind` (see
// principalBag) is the tool a rule author has for stating a rule's dependence on
// a directory instead of hiding it.
func TestAMissingBagWidensAnExclusiveGrant(t *testing.T) {
	// The exclusion an operator wrote: contractors do not reach this object.
	rules := MapSource{
		"contractors": {Name: "contractors", AST: Compare(OpEq, Var("principal.tier"), Lit("contractor"))},
	}
	object := identity.MustParse("account:acme/document:42")
	pattern := identity.MustParsePattern("account:acme/**")

	// wired says whether alice's record exists. Everything else is held constant.
	engineFor := func(t *testing.T, wired bool) *Engine {
		t.Helper()
		var users map[string]provider.Metadata
		if wired {
			users = map[string]provider.Metadata{"alice": {"tier": "contractor"}}
		} else {
			users = map[string]provider.Metadata{"someone-else": {"tier": "employee"}}
		}
		return NewEngine(rules, nil, WithPrincipalResolver(attributeFixture(t, users, nil)))
	}

	contains := func(t *testing.T, strategy string, wired bool) bool {
		t.Helper()
		res, err := scope.DefaultRegistry().Resolve(
			scope.GrantContext{
				Pattern:       pattern,
				ObjectType:    "document",
				Spec:          scope.Spec{Strategy: strategy, Rule: "contractors"},
				Account:       "acme",
				PrincipalKind: "user",
				Principal:     "alice",
				Action:        "read",
			},
			scope.Deps{Rules: engineFor(t, wired)},
		)
		if err != nil {
			t.Fatalf("Resolve(%s): %v", strategy, err)
		}
		covered, err := res.Contains(context.Background(), object)
		if err != nil {
			t.Fatalf("Contains(%s, wired=%v): %v", strategy, wired, err)
		}
		return covered
	}

	t.Run("exclusive: the exclusion stops excluding", func(t *testing.T) {
		if contains(t, scope.StrategyExclusive, true) {
			t.Fatal("with the directory answering, the contractor rule must exclude the object")
		}
		if !contains(t, scope.StrategyExclusive, false) {
			t.Fatal("this test asserts the ACCEPTED hazard, and it just stopped being true. " +
				"If leniency was deliberately changed, rewrite this test and the contract " +
				"prose together; do not delete it")
		}
		// Stated plainly, because the assertion above reads as a success and is not
		// one: the same grant, the same object and the same principal are ALLOWED
		// when the directory has no record and DENIED when it does. E5-S1 makes that
		// visible in an explain; nothing makes it impossible.
	})

	t.Run("inclusive: the same absence is deny-safe", func(t *testing.T) {
		if !contains(t, scope.StrategyInclusive, true) {
			t.Fatal("with the directory answering, the rule must cover the object")
		}
		if contains(t, scope.StrategyInclusive, false) {
			t.Fatal("an absent attribute must not cover anything in an inclusive grant")
		}
	})
}

// goldAndPaid is the two-rule source these tests share: "gold" reads one field
// off the principal bag and "paid" one off the account bag. Two rules rather than
// one conjunction, so a case that fails for the wrong slot is obvious.
func goldAndPaid() MapSource {
	return MapSource{
		"gold": {Name: "gold", AST: Compare(OpEq, Var("principal.tier"), Lit("gold"))},
		"paid": {Name: "paid", AST: Compare(OpEq, Var("account.plan"), Lit("enterprise"))},
	}
}

// documentIDs returns n DISTINCT document identities under account:acme, so an
// enumeration over them is n candidates rather than one repeated.
func documentIDs(n int) []identity.Identity {
	out := make([]identity.Identity, 0, n)
	for i := range n {
		out = append(out, identity.MustParse("account:acme/document:"+strconv.Itoa(i)))
	}
	return out
}
