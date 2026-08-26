package engine

import (
	"context"
	stderrors "errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/provider"
	"github.com/frankbardon/aperture/rules"
	"github.com/frankbardon/aperture/scope"
	"github.com/frankbardon/aperture/storage/memory"
)

// E2-S2: the account-attribute boundary, proven where it is easiest to cross.
//
// E2-S1 shipped the behaviour — `account` resolves from the ACTIVE account. This
// file is the proof that the resolution is CONTAINED, and every test here is
// written so that it fails when the BOUNDARY moves, not merely when the feature
// breaks. Three deliberate choices buy that:
//
//   - the account directory is a SPY that records the keys it was asked for, so
//     "which tenancy did this decision read?" is observed directly rather than
//     inferred from a verdict that could be right for the wrong reason;
//   - the discriminating fixture is ONE grant stamped model.AccountWildcard, so
//     the grant's stamp and the active account are two different strings and the
//     code has to pick one — a fixture with per-account grants cannot tell them
//     apart, because there the two agree;
//   - the isolation assertion names the account that must NOT appear anywhere in
//     an error, so a leak is a failure rather than an unexamined extra field.
//
// The two accounts are acme (enterprise) and globex (free), declared in
// account_attributes_test.go, and alice is a member of both — the multi-account
// principal the isolation invariant is about.

// The secret each tenancy's bag carries beyond its plan. Nothing about a
// decision in one account may name the other's, in a verdict or in an error.
const (
	acmeSecret   = "acme-cost-centre-4711"
	globexSecret = "globex-cost-centre-8100"
)

// boundarySpy is the account directory under test: a real provider wrapped in a
// recorder. It records the KEYS it was asked for rather than a count, because a
// count cannot distinguish "asked about the active account" from "asked about
// the other one", and that distinction is the whole invariant.
//
// fail forces a fetch failure for one key, which is how the error-surface test
// gets a real APERTURE_ATTRIBUTE_PROVIDER_FETCH out of the live decision path
// rather than constructing one by hand.
type boundarySpy struct {
	inner provider.AttributeProvider
	fail  map[string]error

	mu   sync.Mutex
	keys []string
}

func (s *boundarySpy) record(id string) {
	s.mu.Lock()
	s.keys = append(s.keys, id)
	s.mu.Unlock()
}

// asked returns the keys this directory has been fetched for, in order. Note
// that the registry caches per slot, so a key already resolved in this test does
// not appear twice — which is why the assertions below are about the SET of
// accounts touched, never about call counts.
func (s *boundarySpy) asked() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.keys)
}

func (s *boundarySpy) Fetch(ctx context.Context, id string) (provider.Metadata, error) {
	s.record(id)
	if err, ok := s.fail[id]; ok {
		return nil, err
	}
	return s.inner.Fetch(ctx, id)
}

func (s *boundarySpy) List(ctx context.Context) ([]provider.AttributeRecord, error) {
	return s.inner.List(ctx)
}

func (s *boundarySpy) Query(ctx context.Context, filter provider.AttributeFilter) ([]provider.AttributeRecord, error) {
	return s.inner.Query(ctx, filter)
}

// boundaryDirectory is the host account directory both tenancies are served
// from: ONE source holding both bags, so every assertion about what a decision
// read is an assertion about containment rather than about which data happened
// to be reachable.
func boundaryDirectory(t *testing.T) *provider.StaticAttributes {
	t.Helper()
	directory, err := provider.NewStaticAttributes([]provider.AttributeRecord{
		{ID: acctAcme, Attributes: provider.Metadata{"plan": "enterprise", "cost_centre": acmeSecret}},
		{ID: acctGlobex, Attributes: provider.Metadata{"plan": "free", "cost_centre": globexSecret}},
	})
	if err != nil {
		t.Fatalf("account directory: %v", err)
	}
	return directory
}

// boundaryFixture wires the full decision stack over two tenancies and returns
// the engine plus the spy directory serving the account slot.
//
// grantTo names the account each grant is stamped to. Pass
// model.AccountWildcard alone for the single all-accounts grant — the shape
// where "the grant's account" and "the account the decision is in" are different
// answers and only one of them is correct.
func boundaryFixture(t *testing.T, grantTo ...string) (*Engine, *boundarySpy, context.Context) {
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
	// alice is admitted to both tenancies. Nothing in the model distinguishes her
	// two decisions; only the active account can.
	mustSeed(t, store.PutMembership(ctx, model.Membership{PrincipalID: "alice", AccountID: acctAcme}))
	mustSeed(t, store.PutMembership(ctx, model.Membership{PrincipalID: "alice", AccountID: acctGlobex}))

	for _, stamp := range grantTo {
		// A wildcard-stamped grant spans every account, so its pattern must too:
		// bounding it to one account would re-introduce, in the fixture, exactly
		// the containment the test is trying to observe in the engine.
		object, id := "**", "g-star"
		if stamp != model.AccountWildcard {
			object, id = "account:"+stamp+"/**", "g-"+stamp
		}
		mustSeed(t, store.PutGrant(ctx, model.Grant{
			ID: id, AccountID: stamp,
			Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: "alice"},
			PermissionID: "p-plan", Object: object, Effect: model.EffectAllow,
		}))
	}

	spy := &boundarySpy{inner: boundaryDirectory(t)}
	attrs := provider.NewAttributeRegistry()
	attrs.MustRegister(provider.AttributeSlotAccount, spy)

	rulesEng := rules.NewEngine(
		rules.MapSource{"paid": {Name: "paid", AST: rules.Compare(
			rules.OpEq, rules.Var("account.plan"), rules.Lit("enterprise"))}},
		nil, rules.WithAccountResolver(attrs))
	eng := New(store, WithScopeResolution(scope.DefaultRegistry(), ScopeDeps{Rules: rulesEng}))
	return eng, spy, ctx
}

// checkIn runs the plan check as alice inside account, on an object that lives
// in that account.
func checkIn(t *testing.T, eng *Engine, ctx context.Context, account string) Decision {
	t.Helper()
	d, err := eng.Check(ctx, Request{
		Account: account, Principal: "alice", Action: "read",
		Object: "account:" + account + "/document:1",
	})
	if err != nil {
		t.Fatalf("Check in %q must not fail (code %s): %v", account, aerr.CodeOf(err), err)
	}
	return d
}

// TestAWildcardGrantResolvesTheActiveAccountsAttributes is the story's first
// criterion, and the sharpest test in this file: ONE grant, stamped
// model.AccountWildcard, evaluated in two tenancies.
//
// The grant is the same object in both decisions, so the only string that
// differs is Request.Account. If the rule path ever read the GRANT'S account
// instead of the active one, `account` would resolve from "*" in both decisions
// — and because the wildcard short-circuits to the floor, both would deny with
// no error anywhere. That is the failure this test is shaped to catch: acme's
// allow is the load-bearing assertion, not globex's deny.
//
// The spy is asserted as well as the verdict. A verdict can be right by
// coincidence; "the directory was asked about acme and never about globex" is
// the invariant itself, observed.
func TestAWildcardGrantResolvesTheActiveAccountsAttributes(t *testing.T) {
	eng, spy, ctx := boundaryFixture(t, model.AccountWildcard)

	if d := checkIn(t, eng, ctx, acctAcme); !d.Allow {
		t.Errorf("the wildcard grant in acme must read ACME's plan (enterprise) and allow; got %q", d.Reason)
	}
	if asked := spy.asked(); !slices.Equal(asked, []string{acctAcme}) {
		t.Errorf("deciding in acme asked the account directory for %v, want exactly [%s]", asked, acctAcme)
	}

	if d := checkIn(t, eng, ctx, acctGlobex); d.Allow {
		t.Errorf("the SAME wildcard grant in globex must read GLOBEX's plan (free) and deny; got %q", d.Reason)
	}
	if asked := spy.asked(); !slices.Equal(asked, []string{acctAcme, acctGlobex}) {
		t.Errorf("the two decisions asked for %v, want [%s %s] — one tenancy each",
			asked, acctAcme, acctGlobex)
	}
	if slices.Contains(spy.asked(), model.AccountWildcard) {
		t.Errorf("the grant's own stamp %q was used as an attribute key", model.AccountWildcard)
	}
}

// TestAMultiAccountPrincipalSeesOneTenancyAtATime is the story's second
// criterion. alice is a member of acme and globex and holds the same grant shape
// in each; switching only the active account switches which bag `account`
// carries, and the directory is never asked about the tenancy she is not in.
//
// It is the multi-account principal, not the grant stamp, that is under test
// here — the two are separate ways to cross the line and each gets its own test,
// because a fix that contained one and not the other must still fail something.
func TestAMultiAccountPrincipalSeesOneTenancyAtATime(t *testing.T) {
	eng, spy, ctx := boundaryFixture(t, acctAcme, acctGlobex)

	if d := checkIn(t, eng, ctx, acctAcme); !d.Allow {
		t.Errorf("alice in acme reads acme's plan (enterprise) and is allowed; got %q", d.Reason)
	}
	if asked := spy.asked(); !slices.Equal(asked, []string{acctAcme}) {
		t.Errorf("alice's acme decision asked for %v; a member of two accounts must "+
			"still read exactly one", asked)
	}

	if d := checkIn(t, eng, ctx, acctGlobex); d.Allow {
		t.Errorf("alice in globex reads globex's plan (free) and is denied; got %q", d.Reason)
	}
	if asked := spy.asked(); !slices.Equal(asked, []string{acctAcme, acctGlobex}) {
		t.Errorf("alice's two decisions asked for %v, want one key per tenancy in order", asked)
	}
}

// TestNoAccountAttributeFailureNamesAnotherAccount is the story's third
// criterion and the CLAUDE.md non-negotiable ("don't leak cross-account data
// through error messages"), applied to the surface this epic just created.
//
// The account slot is now on the decision's critical path, so its failures reach
// operators, logs, and — through the fail-closed facade — API responses. The
// directory here holds BOTH tenancies' bags, including a cost centre that is not
// the active account's, and the assertion is over everything an error carries:
// every message in the chain plus every structured context key and value. A
// future context entry that helpfully attaches "the records we had" fails here.
//
// The deny reason is checked on the same terms. It is not an error message, but
// it is the other string a decision hands back verbatim, and the leak would be
// identical.
func TestNoAccountAttributeFailureNamesAnotherAccount(t *testing.T) {
	// Each tenancy paired with what must never appear while it is active.
	tenancies := []struct{ active, otherAccount, otherSecret string }{
		{acctAcme, acctGlobex, globexSecret},
		{acctGlobex, acctAcme, acmeSecret},
	}

	t.Run("when the directory fails", func(t *testing.T) {
		for _, tc := range tenancies {
			eng, spy, ctx := boundaryFixture(t, model.AccountWildcard)
			// The ACTIVE account's fetch fails; the other tenancy's bag is sitting
			// right there in the same directory, which is what makes the assertion
			// meaningful rather than vacuous.
			spy.fail = map[string]error{tc.active: stderrors.New("directory unreachable")}

			_, err := eng.Check(ctx, Request{
				Account: tc.active, Principal: "alice", Action: "read",
				Object: "account:" + tc.active + "/document:1",
			})
			if err == nil {
				t.Fatalf("active account %q: an unreachable directory must be a non-decision, not a deny", tc.active)
			}
			if code := aerr.CodeOf(err); code != aerr.APERTURE_ATTRIBUTE_PROVIDER_FETCH {
				t.Fatalf("active account %q: code = %s, want APERTURE_ATTRIBUTE_PROVIDER_FETCH", tc.active, code)
			}
			assertNoTenancyLeak(t, tc.active, errorSurface(err), tc.otherAccount, tc.otherSecret)
		}
	})

	t.Run("when the key is the wildcard", func(t *testing.T) {
		// The seam's backstop refusal, which is what a caller that has NOT resolved
		// the sentinel would hit. It must say what is wrong without naming a tenant:
		// the directory it is refused against holds both.
		reg := provider.NewAttributeRegistry()
		reg.MustRegister(provider.AttributeSlotAccount, boundaryDirectory(t))
		_, err := reg.AccountAttributes(context.Background(), model.AccountWildcard)
		if code := aerr.CodeOf(err); code != aerr.APERTURE_ATTRIBUTE_PROVIDER_INVALID {
			t.Fatalf("code = %s, want APERTURE_ATTRIBUTE_PROVIDER_INVALID", code)
		}
		surface := errorSurface(err)
		// The instrument check. This assertion is not about isolation — it proves
		// errorSurface renders CodedError.Context and not merely Error(), so the
		// negative assertions in this test are looking at the whole surface rather
		// than at a substring that happens to be empty. The refusal's context is
		// {slot, key}, and the slot is the one field guaranteed to be there.
		if !strings.Contains(surface, provider.AttributeSlotAccount.String()) {
			t.Fatalf("errorSurface(%v) = %q and does not render the coded error's context; "+
				"every leak assertion below would be vacuous", err, surface)
		}
		for _, tc := range tenancies {
			assertNoTenancyLeak(t, model.AccountWildcard, surface, tc.active, tc.otherSecret)
		}
	})

	t.Run("when the decision denies", func(t *testing.T) {
		for _, tc := range tenancies {
			eng, _, ctx := boundaryFixture(t, model.AccountWildcard)
			d := checkIn(t, eng, ctx, tc.active)
			assertNoTenancyLeak(t, tc.active, d.Reason, tc.otherAccount, tc.otherSecret)
		}
	})
}

// TestAPlatformScopeDecisionNeverAsksAboutTheWildcardAccount is the story's
// fourth criterion, taken through the REAL decision path rather than the rules
// engine alone.
//
// service/reads.go anchors system-admin authority at model.AccountWildcard, so
// "*" genuinely arrives as Request.Account and travels the whole stack. Two
// things must hold together, and neither is sufficient alone: the decision must
// still be DECIDABLE (a refusal here would make every rule-backed grant
// undecidable at platform scope, including the ones that never mention
// `account`), and no provider may be consulted about "*" — the only bag that
// could answer for it is one tenant's data served as every other's.
func TestAPlatformScopeDecisionNeverAsksAboutTheWildcardAccount(t *testing.T) {
	eng, spy, ctx := boundaryFixture(t, model.AccountWildcard)

	d, err := eng.Check(ctx, Request{
		Account: model.AccountWildcard, Principal: "alice", Action: "read",
		Object: "account:acme/document:1",
	})
	if err != nil {
		t.Fatalf("a platform-scope decision must still decide (code %s): %v", aerr.CodeOf(err), err)
	}
	// The rule reads account.plan, and the floor publishes only account.id, so the
	// honest verdict at platform scope is a deny: there is no tenant plan to read.
	if d.Allow {
		t.Errorf("no host bag may be read for the wildcard account; got allow (%q)", d.Reason)
	}
	if asked := spy.asked(); len(asked) != 0 {
		t.Errorf("the account directory was asked for %v at platform scope; the wildcard "+
			"is a sentinel, and no provider may be consulted about it", asked)
	}
}

// assertNoTenancyLeak fails when text — an error surface or a deny reason
// produced while active was the account in play — names another tenancy or its
// data.
func assertNoTenancyLeak(t *testing.T, active, text, otherAccount, otherSecret string) {
	t.Helper()
	for _, forbidden := range []string{otherAccount, otherSecret} {
		if strings.Contains(text, forbidden) {
			t.Errorf("ISOLATION BREACH: deciding in account %q produced %q, which names %q",
				active, text, forbidden)
		}
	}
}

// errorSurface renders everything an operator can see of err: the rendered
// message chain plus every structured context key and value on every coded error
// in it. Error() alone is not enough — CodedError.Context is carried to the CLI
// and the RPC surface, and a leak there is just as visible as one in the prose.
func errorSurface(err error) string {
	var b strings.Builder
	b.WriteString(err.Error())
	for e := err; e != nil; e = stderrors.Unwrap(e) {
		ce, ok := e.(*aerr.CodedError)
		if !ok {
			continue
		}
		fmt.Fprintf(&b, " | %s %s", ce.Code, ce.Msg)
		for k, v := range ce.Context {
			fmt.Fprintf(&b, " %s=%v", k, v)
		}
	}
	return b.String()
}
