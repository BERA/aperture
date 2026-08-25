package engine

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/provider"
	"github.com/frankbardon/aperture/rules"
	"github.com/frankbardon/aperture/scope"
	"github.com/frankbardon/aperture/storage/memory"
)

// E4-S3, end to end: under an ACTIVE impersonation the rule's `principal.*` must
// describe the same principal the grant set does — the target under become, the
// operator under augment.
//
// It is proven through CheckAs/EnumerateAs rather than through the GrantContext
// alone (grant_context_test.go covers that) because the field a rule reads has to
// travel all the way from the impersonation decorator, through the subject-set
// expansion, the scope strategy and the attribute slot, into the compiled
// expression. Only the real decision path has every one of those layers.

// recordingAttributes wraps an AttributeProvider and records every key fetched. It
// is the instrument for the account-boundary case: "the answer was a deny" and
// "the other account's directory was never asked" are different claims, and only
// the second one is about disclosure.
type recordingAttributes struct {
	inner provider.AttributeProvider
	mu    sync.Mutex
	keys  []string
}

func (r *recordingAttributes) Fetch(ctx context.Context, id string) (provider.Metadata, error) {
	r.mu.Lock()
	r.keys = append(r.keys, id)
	r.mu.Unlock()
	return r.inner.Fetch(ctx, id)
}

func (r *recordingAttributes) List(ctx context.Context) ([]provider.AttributeRecord, error) {
	return r.inner.List(ctx)
}

func (r *recordingAttributes) Query(ctx context.Context, f provider.AttributeFilter) ([]provider.AttributeRecord, error) {
	return r.inner.Query(ctx, f)
}

func (r *recordingAttributes) fetched(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, k := range r.keys {
		if k == key {
			return true
		}
	}
	return false
}

// impersonatedAttributeFixture wires the whole stack twice over: two accounts,
// three principals, a rule-backed inclusive grant per principal, and an attribute
// registry whose user and machine slots disagree about who is gold.
//
//	root  (machine, acme)   tier bronze — the operator; its own grant never allows
//	alice (user,    acme)   tier gold   — the target; her own grant allows
//	dave  (user,    globex) tier gold   — a principal of ANOTHER account
//
// The operator and the target therefore give OPPOSITE verdicts under the same
// rule, which is what makes "whose attributes answered?" observable in the
// verdict itself rather than only in a recorded context.
func impersonatedAttributeFixture(t *testing.T) (*Engine, *recordingAttributes, *recordingAttributes) {
	t.Helper()
	ctx := context.Background()
	store := memory.New()
	if err := store.Setup(ctx); err != nil {
		t.Fatalf("setup: %v", err)
	}
	mustSeed(t, store.PutAccount(ctx, model.Account{ID: acctAcme, Name: acctAcme}))
	mustSeed(t, store.PutAccount(ctx, model.Account{ID: "globex", Name: "globex"}))
	mustSeed(t, store.PutObjectType(ctx, model.ObjectType{Name: "document", Actions: []string{"read"}}))
	mustSeed(t, store.PutPermission(ctx, model.Permission{
		ID: "p-tier", ObjectType: "document", Action: "read", ScopeStrategy: "inclusive;rule=tiered",
	}))

	seed := []struct {
		id      string
		kind    model.PrincipalKind
		account string
	}{
		{"root", model.PrincipalMachine, acctAcme},
		{"alice", model.PrincipalUser, acctAcme},
		{"dave", model.PrincipalUser, "globex"},
	}
	for _, p := range seed {
		mustSeed(t, store.PutPrincipal(ctx, model.Principal{
			ID: p.id, Kind: p.kind, Identity: string(p.kind) + ":" + p.id,
		}))
		mustSeed(t, store.PutMembership(ctx, model.Membership{PrincipalID: p.id, AccountID: p.account}))
		mustSeed(t, store.PutGrant(ctx, model.Grant{
			ID: "g-" + p.id, AccountID: p.account,
			Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: p.id},
			PermissionID: "p-tier", Object: "account:" + p.account + "/**", Effect: model.EffectAllow,
		}))
	}

	md := map[string]provider.Metadata{
		"account:acme/document:1":   {"level": "open"},
		"account:acme/document:2":   {"level": "open"},
		"account:globex/document:9": {"level": "open"},
	}
	objects := provider.NewRegistry()
	objects.MustRegister("document", metaProvider{md: md})

	users, err := provider.NewStaticAttributes([]provider.AttributeRecord{
		{ID: "alice", Attributes: provider.Metadata{"tier": "gold"}},
		{ID: "dave", Attributes: provider.Metadata{"tier": "gold"}},
		// The operator's id also exists in the HUMAN directory, as gold. If the
		// kind did not travel with the id, augment would read the wrong slot and
		// the operator would allow for the wrong reason.
		{ID: "root", Attributes: provider.Metadata{"tier": "gold"}},
	})
	if err != nil {
		t.Fatalf("user directory: %v", err)
	}
	machines, err := provider.NewStaticAttributes([]provider.AttributeRecord{
		{ID: "root", Attributes: provider.Metadata{"tier": "bronze"}},
	})
	if err != nil {
		t.Fatalf("machine directory: %v", err)
	}
	userDir := &recordingAttributes{inner: users}
	machineDir := &recordingAttributes{inner: machines}

	attrs := provider.NewAttributeRegistry()
	attrs.MustRegister(provider.AttributeSlotUser, userDir)
	attrs.MustRegister(provider.AttributeSlotMachine, machineDir)

	rulesEng := rules.NewEngine(
		rules.MapSource{"tiered": {Name: "tiered", AST: rules.Compare(
			rules.OpEq, rules.Var("principal.tier"), rules.Lit("gold"))}},
		objects, rules.WithPrincipalResolver(attrs))

	eng := New(store,
		WithScopeResolution(scope.DefaultRegistry(), ScopeDeps{Lister: objects, Rules: rulesEng}))
	return eng, userDir, machineDir
}

func liveSession(target string, mode Mode) ImpersonationContext {
	return ImpersonationContext{
		RealActor: "root", EffectiveSubject: target, Mode: mode,
		ExpiresAt: time.Now().Add(time.Hour),
	}
}

func operatorRequest() Request {
	return Request{
		Account: acctAcme, Principal: "root", Action: "read",
		Object: "account:acme/document:1",
	}
}

func operatorEnumerate() EnumerateRequest {
	return EnumerateRequest{
		Account: acctAcme, Principal: "root", Action: "read", Pattern: "account:acme/**",
	}
}

// TestBecomeReadsTheTargetsAttributes is the story's acceptance criterion. The
// operator is bronze and the target is gold under one rule, so a decision that
// resolves the target's GRANTS while reading the operator's ATTRIBUTES denies —
// which is precisely the invisible authorization bug this story removes. Both
// CheckAs and EnumerateAs are covered, because a surface that enumerated one way
// and checked another would answer two different questions about one session.
func TestBecomeReadsTheTargetsAttributes(t *testing.T) {
	ctx := context.Background()
	eng, _, _ := impersonatedAttributeFixture(t)

	// The operator acting purely as itself is denied: it is bronze.
	own, err := eng.Check(ctx, operatorRequest())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if own.Allow {
		t.Fatal("the operator is bronze; its own check must deny, or the fixture proves nothing")
	}

	dec, err := eng.CheckAs(ctx, operatorRequest(), liveSession("alice", ModeBecome))
	if err != nil {
		t.Fatalf("CheckAs(become): %v (code %s)", err, aerr.CodeOf(err))
	}
	if !dec.Allow {
		t.Error("under become the rule must read the TARGET's attributes (alice is gold); " +
			"a deny means principal.* still described the operator")
	}

	ids, err := eng.EnumerateAs(ctx, operatorEnumerate(), liveSession("alice", ModeBecome))
	if err != nil {
		t.Fatalf("EnumerateAs(become): %v (code %s)", err, aerr.CodeOf(err))
	}
	if len(ids) != 2 {
		t.Errorf("EnumerateAs(become) = %v, want both acme documents — the target's "+
			"attributes must select them", ids)
	}
}

// TestAugmentReadsTheOperatorsAttributes is the other half of the contract, and
// the half that does NOT change. Augment adds the target's grants while the
// operator keeps acting under its own identity, so the rule keeps describing the
// operator: alice's grant is in the set, but it is evaluated against root's
// bronze bag and selects nothing.
func TestAugmentReadsTheOperatorsAttributes(t *testing.T) {
	ctx := context.Background()
	eng, _, machineDir := impersonatedAttributeFixture(t)

	dec, err := eng.CheckAs(ctx, operatorRequest(), liveSession("alice", ModeAugment))
	if err != nil {
		t.Fatalf("CheckAs(augment): %v (code %s)", err, aerr.CodeOf(err))
	}
	if dec.Allow {
		t.Error("under augment the operator acts as itself; the rule must read root's " +
			"bronze bag and deny, however many of alice's grants joined the set")
	}
	if !machineDir.fetched("root") {
		t.Error("the operator's kind is machine, so augment must read the MACHINE slot; " +
			"the machine directory was never asked about root")
	}

	ids, err := eng.EnumerateAs(ctx, operatorEnumerate(), liveSession("alice", ModeAugment))
	if err != nil {
		t.Fatalf("EnumerateAs(augment): %v (code %s)", err, aerr.CodeOf(err))
	}
	if len(ids) != 0 {
		t.Errorf("EnumerateAs(augment) = %v, want empty — the operator's own attributes decide", ids)
	}
}

// TestAnInertSessionElevatesNothing pins the non-negotiable: an expired session is
// not a weaker impersonation, it is NO impersonation. The *As entry point must
// produce exactly what its plain sibling produces — the whole Decision, including
// a nil Impersonation — so an expired become can never read as the target.
func TestAnInertSessionElevatesNothing(t *testing.T) {
	ctx := context.Background()
	eng, userDir, _ := impersonatedAttributeFixture(t)

	expired := ImpersonationContext{
		RealActor: "root", EffectiveSubject: "alice", Mode: ModeBecome,
		ExpiresAt: time.Now().Add(-time.Minute),
	}

	plain, err := eng.Check(ctx, operatorRequest())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	stale, err := eng.CheckAs(ctx, operatorRequest(), expired)
	if err != nil {
		t.Fatalf("CheckAs(expired): %v", err)
	}
	if !reflect.DeepEqual(plain, stale) {
		t.Errorf("an expired session changed the decision:\n plain = %+v\n  as   = %+v", plain, stale)
	}
	if stale.Allow {
		t.Error("an expired become allowed; elevation outlived its time-box")
	}
	if userDir.fetched("alice") {
		t.Error("an expired session read the TARGET's attributes; " +
			"an inert context must not reach the target at all")
	}

	plainIDs, err := eng.Enumerate(ctx, operatorEnumerate())
	if err != nil {
		t.Fatalf("Enumerate: %v", err)
	}
	staleIDs, err := eng.EnumerateAs(ctx, operatorEnumerate(), expired)
	if err != nil {
		t.Fatalf("EnumerateAs(expired): %v", err)
	}
	if !reflect.DeepEqual(plainIDs, staleIDs) {
		t.Errorf("an expired session changed the enumeration: %v vs %v", plainIDs, staleIDs)
	}
}

// TestCrossAccountImpersonationReadsNoAttributes is the disclosure boundary. The
// refusal already existed; what this story adds is a second thing that must not
// happen — the target's directory must not be consulted either. An attribute fetch
// for a principal of another account is a read of that account's data, and it
// would be one performed on behalf of somebody with no standing there, so the
// boundary check has to come BEFORE the fetch rather than merely discard its
// result.
func TestCrossAccountImpersonationReadsNoAttributes(t *testing.T) {
	ctx := context.Background()

	for _, mode := range []Mode{ModeBecome, ModeAugment} {
		t.Run(string(mode), func(t *testing.T) {
			eng, userDir, _ := impersonatedAttributeFixture(t)
			ic := liveSession("dave", mode)

			dec, err := eng.CheckAs(ctx, operatorRequest(), ic)
			if err != nil {
				t.Fatalf("CheckAs: %v", err)
			}
			if dec.Allow {
				t.Error("a cross-account impersonation must fail closed to a deny")
			}
			if dec.Impersonation == nil || dec.Impersonation.RealActor != "root" {
				t.Error("a refused session must still record the real actor for audit")
			}

			ids, err := eng.EnumerateAs(ctx, operatorEnumerate(), ic)
			if err != nil {
				t.Fatalf("EnumerateAs: %v", err)
			}
			if len(ids) != 0 {
				t.Errorf("EnumerateAs = %v, want the empty set", ids)
			}

			if userDir.fetched("dave") {
				t.Error("the directory was asked about a principal of ANOTHER account; " +
					"the boundary must be enforced before any attribute is read")
			}
		})
	}
}

// TestTheAuditTrailStillNamesTheRealActor is the guardrail on the change itself.
// `principal.*` now describes the target under become, and the one way that could
// go wrong is by taking the operator's identity with it. The rule sees whose
// authority answered; the audit trail sees who actually acted. Both, never either.
func TestTheAuditTrailStillNamesTheRealActor(t *testing.T) {
	ctx := context.Background()
	eng, _, _ := impersonatedAttributeFixture(t)

	dec, err := eng.CheckAs(ctx, operatorRequest(), liveSession("alice", ModeBecome))
	if err != nil {
		t.Fatalf("CheckAs: %v", err)
	}
	if dec.Impersonation == nil {
		t.Fatal("an active session must be recorded on the Decision")
	}
	if dec.Impersonation.RealActor != "root" {
		t.Errorf("Decision.Impersonation.RealActor = %q, want the operator",
			dec.Impersonation.RealActor)
	}
	if dec.Impersonation.EffectiveSubject != "alice" {
		t.Errorf("Decision.Impersonation.EffectiveSubject = %q, want the target",
			dec.Impersonation.EffectiveSubject)
	}

	tr, err := eng.ExplainAs(ctx, operatorRequest(), liveSession("alice", ModeBecome))
	if err != nil {
		t.Fatalf("ExplainAs: %v", err)
	}
	if tr.Request.Principal != "root" {
		t.Errorf("Trace.Request.Principal = %q, want the operator — the request records "+
			"who asked, not whose attributes answered", tr.Request.Principal)
	}
	if tr.Impersonation == nil || tr.Impersonation.RealActor != "root" {
		t.Error("the trace must carry the real actor")
	}
	if !tr.Decision.Allow {
		t.Error("ExplainAs must agree with CheckAs: both resolve the target's attributes")
	}
}
