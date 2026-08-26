package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/frankbardon/aperture/authz"
	"github.com/frankbardon/aperture/engine"
	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/provider"
	"github.com/frankbardon/aperture/rules"
	"github.com/frankbardon/aperture/scope"
	"github.com/frankbardon/aperture/storage/memory"
)

// E5-S2: listing an attribute slot is a system-tier read.
//
// The fixture is built so the two halves of the story are separable and both
// observable on ONE registry:
//
//   - mallory holds NO administrative authority at all and her decision still
//     reads her bag, because Fetch on the decision path is not gated (gating it
//     would break every decision in every deployment that wires a directory);
//   - the same mallory is refused the bulk read of the very slot that bag came
//     out of.
//
// The directory is a SPY that counts Fetch, List and Query separately, because
// "the decision path can only Fetch" is the structural claim the whole design
// rests on and a count is how it is observed rather than assumed.

// The secret the user directory carries beyond the tier the rule compares. It is
// the canary: nothing a refused caller receives — slice, count, error string, or
// error context — may contain it, and neither may anything else disclose that
// the slot has rows at all.
const directorySecret = "employee-number-90210"

// attributeSpy wraps a real AttributeProvider and counts the three calls
// separately. Separately, not in one total, because the invariant is not "the
// directory was quiet" but "the directory was FETCHED and never ENUMERATED" —
// a single counter cannot tell a decision reading one bag from a decision
// listing the table.
type attributeSpy struct {
	inner provider.AttributeProvider

	mu      sync.Mutex
	fetches int
	lists   int
	queries int
}

func (s *attributeSpy) Fetch(ctx context.Context, id string) (provider.Metadata, error) {
	s.mu.Lock()
	s.fetches++
	s.mu.Unlock()
	return s.inner.Fetch(ctx, id)
}

func (s *attributeSpy) List(ctx context.Context) ([]provider.AttributeRecord, error) {
	s.mu.Lock()
	s.lists++
	s.mu.Unlock()
	return s.inner.List(ctx)
}

func (s *attributeSpy) Query(ctx context.Context, f provider.AttributeFilter) ([]provider.AttributeRecord, error) {
	s.mu.Lock()
	s.queries++
	s.mu.Unlock()
	return s.inner.Query(ctx, f)
}

func (s *attributeSpy) counts() (fetches, lists, queries int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fetches, s.lists, s.queries
}

// attributeFixture wires the facade the way a real deployment does: one
// *provider.AttributeRegistry serving BOTH the decision path (through the rules
// engine's resolver seams) and the gated admin read (through WithAttributes), so
// the two paths cannot be told apart by which registry they touch.
//
// alice is a system-admin (allow on aperture.admin over **, in acme). mallory is
// an ordinary principal with a document grant and no authority whatsoever.
func attributeFixture(t *testing.T) (*Service, *attributeSpy, context.Context) {
	t.Helper()
	ctx := context.Background()
	store := memory.New()
	if err := store.Setup(ctx); err != nil {
		t.Fatalf("setup: %v", err)
	}
	mustPut(t, store.PutAccount(ctx, model.Account{ID: "acme", Name: "Acme"}))
	mustPut(t, store.PutObjectType(ctx, model.ObjectType{Name: "system", Actions: []string{authz.AdminAction}}))
	mustPut(t, store.PutPermission(ctx, model.Permission{ID: "p-admin", ObjectType: "system", Action: authz.AdminAction}))
	mustPut(t, store.PutObjectType(ctx, model.ObjectType{Name: "document", Actions: []string{"read"}}))
	mustPut(t, store.PutPermission(ctx, model.Permission{
		ID: "p-doc", ObjectType: "document", Action: "read", ScopeStrategy: "inclusive;rule=gold",
	}))
	mustPut(t, store.PutPrincipal(ctx, model.Principal{ID: "alice", Kind: model.PrincipalUser, Identity: "user:alice"}))
	mustPut(t, store.PutPrincipal(ctx, model.Principal{ID: "mallory", Kind: model.PrincipalUser, Identity: "user:mallory"}))
	mustPut(t, store.PutMembership(ctx, model.Membership{PrincipalID: "alice", AccountID: "acme"}))
	mustPut(t, store.PutMembership(ctx, model.Membership{PrincipalID: "mallory", AccountID: "acme"}))
	mustPut(t, store.PutGrant(ctx, model.Grant{
		ID: "g-admin", AccountID: "acme",
		Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: "alice"},
		PermissionID: "p-admin", Object: "**", Effect: model.EffectAllow,
	}))
	mustPut(t, store.PutGrant(ctx, model.Grant{
		ID: "g-doc", AccountID: "acme",
		Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: "mallory"},
		PermissionID: "p-doc", Object: "account:acme/**", Effect: model.EffectAllow,
	}))

	users, err := provider.NewStaticAttributes([]provider.AttributeRecord{
		{ID: "mallory", Attributes: provider.Metadata{"tier": "gold", "badge": directorySecret}},
		{ID: "alice", Attributes: provider.Metadata{"tier": "silver", "badge": directorySecret}},
	})
	if err != nil {
		t.Fatalf("user directory: %v", err)
	}
	spy := &attributeSpy{inner: users}
	attrs := provider.NewAttributeRegistry()
	attrs.MustRegister(provider.AttributeSlotUser, spy)

	rulesEng := rules.NewEngine(
		rules.MapSource{"gold": {Name: "gold", AST: rules.Compare(
			rules.OpEq, rules.Var("principal.tier"), rules.Lit("gold"))}},
		nil, rules.WithPrincipalResolver(attrs))
	eng := engine.New(store,
		engine.WithScopeResolution(scope.DefaultRegistry(), engine.ScopeDeps{Rules: rulesEng}))

	svc := New(eng,
		WithStorage(store),
		WithGate(authz.NewGate(eng)),
		WithAttributes(attrs),
	)
	return svc, spy, ctx
}

// admin / nobody are the two actors every case below contrasts.
var (
	adminActor  = Actor{Principal: "alice", Account: "acme"}
	deniedActor = Actor{Principal: "mallory", Account: "acme"}
)

// TestListingAttributesRequiresSystemAdmin is the story's headline: the same
// call, made by two authenticated principals, is a directory page for the
// system-admin and a coded refusal for everybody else.
func TestListingAttributesRequiresSystemAdmin(t *testing.T) {
	svc, _, ctx := attributeFixture(t)

	recs, err := svc.ListAttributes(ctx, adminActor, "user", provider.AttributeFilter{})
	if err != nil {
		t.Fatalf("a system-admin must be able to list a slot: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("system-admin listed %d records, want 2", len(recs))
	}

	recs, err = svc.ListAttributes(ctx, deniedActor, "user", provider.AttributeFilter{})
	if code := aerr.CodeOf(err); code != aerr.APERTURE_AUTHZ_DENIED {
		t.Fatalf("non-admin listing = %v (code %q), want APERTURE_AUTHZ_DENIED", err, code)
	}
	if recs != nil {
		t.Fatalf("a refused listing returned %d record(s); it must return nothing at all", len(recs))
	}
	// The gate's own code and its registry fixups must reach the operator: Wrap
	// re-stamps, and a same-code re-wrap is invisible to CodeOf, so depth is what
	// proves the refusal passed through rather than being re-classified here.
	if depth := codedAttributeDepth(err); depth != 1 {
		t.Errorf("refusal has %d Aperture-coded errors in its chain, want exactly 1 "+
			"(the gate's APERTURE_AUTHZ_DENIED, verbatim)", depth)
	}
}

// TestAFilteredListingIsGatedToo pins that the Fields/Limit form of the read is
// the same door. `List` and `Query` are one facade method precisely so a future
// filtered variant cannot arrive ungated beside the gated one.
func TestAFilteredListingIsGatedToo(t *testing.T) {
	svc, _, ctx := attributeFixture(t)
	filter := provider.AttributeFilter{Fields: map[string]any{"tier": "gold"}, Limit: 1}

	recs, err := svc.ListAttributes(ctx, adminActor, "user", filter)
	if err != nil {
		t.Fatalf("admin filtered listing: %v", err)
	}
	if len(recs) != 1 || recs[0].ID != "mallory" {
		t.Fatalf("filtered listing = %+v, want just mallory", recs)
	}

	if _, err := svc.ListAttributes(ctx, deniedActor, "user", filter); aerr.CodeOf(err) != aerr.APERTURE_AUTHZ_DENIED {
		t.Fatalf("non-admin filtered listing = %v, want APERTURE_AUTHZ_DENIED", err)
	}
}

// TestARefusalDisclosesNothingAboutTheSlot is the disclosure criterion. A
// refused caller must not be able to learn, from the refusal alone, whether a
// slot has ten thousand rows, zero rows, or no provider at all — so the three
// refusals must be INDISTINGUISHABLE, and none of them may carry directory data.
//
// This is what forces the gate to run before the slot is resolved: resolving
// first would answer "is this slot wired?" to a caller who is not allowed to
// know it.
func TestARefusalDisclosesNothingAboutTheSlot(t *testing.T) {
	svc, spy, ctx := attributeFixture(t)

	populated := refusalText(t, svc, ctx, "user")       // two records
	unregistered := refusalText(t, svc, ctx, "account") // a real slot, no provider wired
	unknown := refusalText(t, svc, ctx, "unicorn")      // not a slot at all

	if populated != unregistered || populated != unknown {
		t.Errorf("refusals differ by slot state, so a denied caller can probe the deployment:\n"+
			"  populated:    %s\n  unregistered: %s\n  unknown:      %s",
			populated, unregistered, unknown)
	}
	for _, leak := range []string{directorySecret, "mallory's", "gold", "2 record"} {
		if strings.Contains(populated, leak) {
			t.Errorf("refusal text contains %q; a refusal must carry no directory data", leak)
		}
	}
	// And nothing was read to produce it: the provider is never consulted for a
	// caller the gate refuses, so a refusal costs no query and warms no cache.
	if _, lists, queries := spy.counts(); lists != 0 || queries != 0 {
		t.Errorf("a refused listing hit the directory (%d list, %d query); the gate must run first",
			lists, queries)
	}
}

// refusalText renders a non-admin's refusal for slot as a single comparable
// string — code, message, and every context field — so two refusals can be
// compared for equality rather than for the absence of a specific leak somebody
// remembered to check for.
func refusalText(t *testing.T, svc *Service, ctx context.Context, slot string) string {
	t.Helper()
	recs, err := svc.ListAttributes(ctx, deniedActor, slot, provider.AttributeFilter{})
	if recs != nil {
		t.Fatalf("slot %q: refusal returned %d record(s)", slot, len(recs))
	}
	if aerr.CodeOf(err) != aerr.APERTURE_AUTHZ_DENIED {
		t.Fatalf("slot %q: got %v (code %q), want APERTURE_AUTHZ_DENIED — a slot's state "+
			"must not change which refusal a non-admin receives", slot, err, aerr.CodeOf(err))
	}
	var ce *aerr.CodedError
	if !errors.As(err, &ce) {
		t.Fatalf("slot %q: refusal is not a CodedError: %v", slot, err)
	}
	return string(ce.Code) + "|" + ce.Error()
}

// TestFetchOnTheDecisionPathIsNotGated is the other half of the design, and the
// half a careless gate breaks. mallory holds no administrative authority of any
// kind; her decision still reads her attribute bag and the rule still selects on
// it. A gate on the decision-path fetch would deny every rule-backed decision in
// every deployment that wires a directory.
func TestFetchOnTheDecisionPathIsNotGated(t *testing.T) {
	svc, spy, ctx := attributeFixture(t)

	// Ungated, by construction: Check takes no Actor at all.
	res, err := svc.Check(ctx, Query{
		Account: "acme", Principal: "mallory", Action: "read", Object: "account:acme/document:1",
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !res.Allow {
		t.Fatalf("mallory is gold and the grant is rule-backed; the check must allow (%s)", res.Reason)
	}
	if fetches, _, _ := spy.counts(); fetches == 0 {
		t.Fatal("the decision allowed without reading the directory; the fixture is not proving anything")
	}

	// The same principal, refused the bulk read of the slot her own bag came from.
	if _, err := svc.ListAttributes(ctx, deniedActor, "user", provider.AttributeFilter{}); aerr.CodeOf(err) != aerr.APERTURE_AUTHZ_DENIED {
		t.Fatalf("mallory's listing = %v, want APERTURE_AUTHZ_DENIED — reading one's own bag "+
			"through a decision is not authority to read the directory", err)
	}
}

// TestTheGateCannotBeBypassedFromADecisionPath is the bypass criterion. Every
// UNGATED entry point on the facade — the whole decision API, single and batch —
// is driven with the registry wired exactly as production wires it, and the
// directory must record fetches and NOT ONE enumeration.
//
// That is the behavioural half of a structural guarantee: an
// *provider.AttributeRegistry cannot satisfy scope.ObjectLister (asserted in
// provider.TestAttributeRegistryIsNotAScopeLister against the real interface),
// so a scope resolver has no seam to enumerate a directory through even though
// it enumerates object types through exactly that shape mid-decision. There is
// no actor inside a decision to gate on, so the leak is prevented by the type
// rather than policed by a check.
func TestTheGateCannotBeBypassedFromADecisionPath(t *testing.T) {
	svc, spy, ctx := attributeFixture(t)
	q := Query{Account: "acme", Principal: "mallory", Action: "read", Object: "account:acme/document:1"}

	if _, err := svc.Check(ctx, q); err != nil {
		t.Fatalf("Check: %v", err)
	}
	if _, err := svc.Explain(ctx, q); err != nil {
		t.Fatalf("Explain: %v", err)
	}
	svc.CheckBatch(ctx, []Query{q, q})
	svc.ExplainBatch(ctx, []Query{q})
	// Enumerate needs an object source this fixture deliberately does not wire, so
	// its error is uninteresting; what matters is that travelling that path still
	// enumerates no directory.
	_, _ = svc.Enumerate(ctx, EnumerateQuery{
		Account: "acme", Principal: "mallory", Action: "read", Pattern: "account:acme/document:*",
	})

	fetches, lists, queries := spy.counts()
	if fetches == 0 {
		t.Fatal("no decision read the directory at all; the fixture proves nothing")
	}
	if lists != 0 || queries != 0 {
		t.Fatalf("a decision path enumerated the directory (%d List, %d Query); enumeration is a "+
			"system-tier admin read and must be unreachable without an actor", lists, queries)
	}
}

// TestExplainingTheRefusedAuthorityStillWorks is the operator's half of a
// denial. Being refused is not the same as being told nothing: the authority
// decision behind the refusal is an ordinary engine decision and stays
// explainable, for the very actor who was refused.
func TestExplainingTheRefusedAuthorityStillWorks(t *testing.T) {
	svc, _, ctx := attributeFixture(t)

	if _, err := svc.ListAttributes(ctx, deniedActor, "user", provider.AttributeFilter{}); err == nil {
		t.Fatal("mallory must be refused")
	}
	trace, err := svc.ExplainAttributeAuthority(ctx, deniedActor)
	if err != nil {
		t.Fatalf("a refused operator must still be able to see why: %v", err)
	}
	if trace.Decision.Allow {
		t.Fatal("the explained authority decision allows, but the listing was refused; " +
			"the trace must match the verdict it explains")
	}
	if trace.Decision.Reason == "" {
		t.Error("the authority trace carries no reason; a refusal an operator cannot read is a support ticket")
	}
	// And it names the authority that was missing, not the directory that was
	// withheld — the trace is about the caller, never about the slot.
	if strings.Contains(trace.Decision.Reason, directorySecret) {
		t.Error("the authority trace carries directory data")
	}

	// The admin's own trace allows, which is what makes the denied one meaningful.
	allowed, err := svc.ExplainAttributeAuthority(ctx, adminActor)
	if err != nil {
		t.Fatalf("ExplainAttributeAuthority(admin): %v", err)
	}
	if !allowed.Decision.Allow {
		t.Fatal("alice holds system-admin authority; her authority trace must allow")
	}
}

// TestTheUnwiredAndUnauthenticatedRefusals pins the two failures that precede
// the authority check, and the reason they precede it: with no gate there is no
// authority to resolve, so the surface refuses rather than degrading to the
// unrestricted local-CLI behaviour entity reads allow themselves.
func TestTheUnwiredAndUnauthenticatedRefusals(t *testing.T) {
	svc, _, ctx := attributeFixture(t)

	if _, err := svc.ListAttributes(ctx, Actor{Account: "acme"}, "user", provider.AttributeFilter{}); aerr.CodeOf(err) != aerr.APERTURE_UNAUTHENTICATED {
		t.Errorf("anonymous listing = %v, want APERTURE_UNAUTHENTICATED", err)
	}

	// No registry: the surface is simply not there.
	bare := New(engine.New(memory.New()))
	if _, err := bare.ListAttributes(ctx, adminActor, "user", provider.AttributeFilter{}); aerr.CodeOf(err) != aerr.APERTURE_UNIMPLEMENTED {
		t.Errorf("unwired facade listing = %v, want APERTURE_UNIMPLEMENTED", err)
	}

	// A registry but NO GATE. This is the fail-open shape: a facade with the
	// directory in hand and nothing to authorize against must refuse, not serve.
	attrs := provider.NewAttributeRegistry()
	users, err := provider.NewStaticAttributes([]provider.AttributeRecord{{ID: "u", Attributes: provider.Metadata{"tier": "gold"}}})
	if err != nil {
		t.Fatalf("static: %v", err)
	}
	attrs.MustRegister(provider.AttributeSlotUser, users)
	ungated := New(engine.New(memory.New()), WithAttributes(attrs))
	recs, err := ungated.ListAttributes(ctx, adminActor, "user", provider.AttributeFilter{})
	if aerr.CodeOf(err) != aerr.APERTURE_UNIMPLEMENTED {
		t.Errorf("gateless listing = %v, want APERTURE_UNIMPLEMENTED", err)
	}
	if recs != nil {
		t.Errorf("gateless listing returned %d record(s); an ungated system-tier read must serve nothing", len(recs))
	}
}

// TestAnAdminStillGetsTheSlotDiagnostics is the flip side of the indistinguishable
// refusals above: the caller who IS allowed gets the real, actionable code for a
// slot that is unknown or unwired, because for that caller those are wiring
// diagnostics rather than disclosures.
func TestAnAdminStillGetsTheSlotDiagnostics(t *testing.T) {
	svc, _, ctx := attributeFixture(t)

	if _, err := svc.ListAttributes(ctx, adminActor, "unicorn", provider.AttributeFilter{}); aerr.CodeOf(err) != aerr.APERTURE_ATTRIBUTE_SLOT_UNKNOWN {
		t.Errorf("admin listing an unknown slot = %v, want APERTURE_ATTRIBUTE_SLOT_UNKNOWN", err)
	}
	if _, err := svc.ListAttributes(ctx, adminActor, "account", provider.AttributeFilter{}); aerr.CodeOf(err) != aerr.APERTURE_ATTRIBUTE_PROVIDER_UNREGISTERED {
		t.Errorf("admin listing an unwired slot = %v, want APERTURE_ATTRIBUTE_PROVIDER_UNREGISTERED", err)
	}
}

// codedAttributeDepth counts the Aperture-coded errors in a chain. A same-code
// re-stamp is invisible to CodeOf, so depth is what proves a pass-through
// actually passed through.
func codedAttributeDepth(err error) int {
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
