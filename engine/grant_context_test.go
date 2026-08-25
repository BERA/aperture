package engine

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/frankbardon/aperture/identity"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/scope"
	"github.com/frankbardon/aperture/storage/memory"
)

// recorder captures the GrantContexts a strategy is built from. A scope strategy
// is the only place the engine's answer to "who is asking, and where" becomes
// observable, so the recording resolver is the instrument for the whole file.
type recorder struct {
	mu   sync.Mutex
	seen []scope.GrantContext
}

func (r *recorder) factory(gc scope.GrantContext, _ scope.Deps) (scope.ScopeResolver, error) {
	r.mu.Lock()
	r.seen = append(r.seen, gc)
	r.mu.Unlock()
	return recordingResolver{gc: gc}, nil
}

func (r *recorder) last(t *testing.T) scope.GrantContext {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.seen) == 0 {
		t.Fatal("the scope strategy was never resolved; the decision never reached it")
	}
	return r.seen[len(r.seen)-1]
}

// recordingResolver covers everything inside the grant pattern, so a recorded
// decision is an ALLOW and the enumeration has a member to decide.
type recordingResolver struct{ gc scope.GrantContext }

func (r recordingResolver) Contains(_ context.Context, object identity.Identity) (bool, error) {
	return r.gc.Pattern.Matches(object), nil
}

func (r recordingResolver) Members(_ context.Context, query identity.Pattern) ([]identity.Identity, error) {
	id := identity.MustParse("account:acme/document:1")
	if !r.gc.Pattern.Matches(id) || !query.Matches(id) {
		return nil, nil
	}
	return []identity.Identity{id}, nil
}

// grantContextFixture seeds one account, one document type, one principal of the
// given kind, and one grant whose permission selects the recording strategy.
func grantContextFixture(t *testing.T, principalID string, kind model.PrincipalKind) (*Engine, *recorder) {
	t.Helper()
	ctx := context.Background()
	store := memory.New()
	mustSeed := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	mustSeed(store.Setup(ctx))
	mustSeed(store.PutAccount(ctx, model.Account{ID: acctAcme, Name: acctAcme}))
	mustSeed(store.PutObjectType(ctx, model.ObjectType{Name: "document", Actions: []string{"read"}}))
	mustSeed(store.PutPermission(ctx, model.Permission{
		ID: "p-rec", ObjectType: "document", Action: "read", ScopeStrategy: "recording",
	}))
	mustSeed(store.PutPrincipal(ctx, model.Principal{
		ID: principalID, Kind: kind, Identity: string(kind) + ":" + principalID,
	}))
	mustSeed(store.PutGrant(ctx, model.Grant{
		ID: "g-rec", AccountID: acctAcme,
		Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: principalID},
		PermissionID: "p-rec", Object: "account:acme/**", Effect: model.EffectAllow,
	}))

	rec := &recorder{}
	reg := scope.DefaultRegistry()
	reg.MustRegister("recording", rec.factory)
	return New(store, WithScopeResolution(reg)), rec
}

// TestGrantContextCarriesTheAccountAndPrincipalKind is the story's whole point.
// A rule-backed strategy is asked about attributes, and an attribute has no
// meaning without an account to read it in and a kind to read it from. Every
// decision entry point must supply both, or one surface would answer differently
// from another for the same principal.
func TestGrantContextCarriesTheAccountAndPrincipalKind(t *testing.T) {
	ctx := context.Background()

	t.Run("check", func(t *testing.T) {
		eng, rec := grantContextFixture(t, "svc-1", model.PrincipalMachine)
		if _, err := eng.Check(ctx, Request{
			Account: acctAcme, Principal: "svc-1", Action: "read", Object: "account:acme/document:1",
		}); err != nil {
			t.Fatalf("Check: %v", err)
		}
		assertGrantContext(t, rec.last(t), acctAcme, "machine", "svc-1")
	})

	t.Run("enumerate", func(t *testing.T) {
		eng, rec := grantContextFixture(t, "alice", model.PrincipalUser)
		if _, err := eng.Enumerate(ctx, EnumerateRequest{
			Account: acctAcme, Principal: "alice", Action: "read", Pattern: "account:acme/**",
		}); err != nil {
			t.Fatalf("Enumerate: %v", err)
		}
		assertGrantContext(t, rec.last(t), acctAcme, "user", "alice")
	})

	t.Run("explain", func(t *testing.T) {
		eng, rec := grantContextFixture(t, "svc-1", model.PrincipalMachine)
		if _, err := eng.Explain(ctx, Request{
			Account: acctAcme, Principal: "svc-1", Action: "read", Object: "account:acme/document:1",
		}); err != nil {
			t.Fatalf("Explain: %v", err)
		}
		assertGrantContext(t, rec.last(t), acctAcme, "machine", "svc-1")
	})
}

func assertGrantContext(t *testing.T, gc scope.GrantContext, account, kind, principal string) {
	t.Helper()
	if gc.Account != account {
		t.Errorf("GrantContext.Account = %q, want %q", gc.Account, account)
	}
	if gc.PrincipalKind != kind {
		t.Errorf("GrantContext.PrincipalKind = %q, want %q", gc.PrincipalKind, kind)
	}
	if gc.Principal != principal {
		t.Errorf("GrantContext.Principal = %q, want %q", gc.Principal, principal)
	}
}

// principalCountingStore counts the reads of the principal row the kind is taken
// from.
type principalCountingStore struct {
	model.Storage
	mu sync.Mutex
	n  int
}

func (s *principalCountingStore) GetPrincipal(ctx context.Context, id string) (model.Principal, error) {
	s.mu.Lock()
	s.n++
	s.mu.Unlock()
	return s.Storage.GetPrincipal(ctx, id)
}

func (s *principalCountingStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n
}

// TestPrincipalKindCostsNoExtraStorageRead pins the reason the kind is returned
// from subjectSet rather than fetched where it is needed: the decision ALREADY
// reads the principal row to expand the subject set. A second read would put a
// storage round-trip on the hot path for a value that was in hand all along, and
// nothing else in the decision would look different — which is exactly the kind
// of regression only a counter catches.
func TestPrincipalKindCostsNoExtraStorageRead(t *testing.T) {
	ctx := context.Background()
	base := memory.New()
	mustSeed := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	mustSeed(base.Setup(ctx))
	mustSeed(base.PutAccount(ctx, model.Account{ID: acctAcme, Name: acctAcme}))
	mustSeed(base.PutObjectType(ctx, model.ObjectType{Name: "document", Actions: []string{"read"}}))
	mustSeed(base.PutPermission(ctx, model.Permission{
		ID: "p-impl", ObjectType: "document", Action: "read", ScopeStrategy: scope.StrategyImplicit,
	}))
	mustSeed(base.PutPrincipal(ctx, model.Principal{
		ID: "alice", Kind: model.PrincipalUser, Identity: "user:alice",
	}))
	mustSeed(base.PutGrant(ctx, model.Grant{
		ID: "g1", AccountID: acctAcme,
		Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: "alice"},
		PermissionID: "p-impl", Object: "account:acme/**", Effect: model.EffectAllow,
	}))

	store := &principalCountingStore{Storage: base}
	eng := New(store, WithScopeResolution(scope.DefaultRegistry()))
	if _, err := eng.Check(ctx, Request{
		Account: acctAcme, Principal: "alice", Action: "read", Object: "account:acme/document:1",
	}); err != nil {
		t.Fatalf("Check: %v", err)
	}
	if got := store.count(); got != 1 {
		t.Errorf("GetPrincipal called %d times for one Check, want exactly 1", got)
	}
}

// TestImpersonatedBecomeReportsAnUnknownKind documents the one place the kind is
// deliberately absent. Under become, the request still names the operator — a
// rule-backed strategy is told about the operator, not the target — but the
// operator's row is never read on that path, only its account membership. The
// engine reports the kind as unknown rather than substituting the target's, which
// would describe the wrong principal, or buying a read to answer a question no
// rule has asked yet.
func TestImpersonatedBecomeReportsAnUnknownKind(t *testing.T) {
	ctx := context.Background()
	eng, rec := grantContextFixture(t, "alice", model.PrincipalUser)

	// The operator impersonates alice; both must be account members.
	store := eng.store.(*memory.Store)
	if err := store.PutPrincipal(ctx, model.Principal{
		ID: "root", Kind: model.PrincipalMachine, Identity: "machine:root",
	}); err != nil {
		t.Fatalf("seed operator: %v", err)
	}
	for _, id := range []string{"alice", "root"} {
		if err := store.PutMembership(ctx, model.Membership{PrincipalID: id, AccountID: acctAcme}); err != nil {
			t.Fatalf("membership %s: %v", id, err)
		}
	}

	ic := ImpersonationContext{
		RealActor:        "root",
		EffectiveSubject: "alice",
		Mode:             ModeBecome,
		ExpiresAt:        time.Now().Add(time.Hour),
	}
	if _, err := eng.CheckAs(ctx, Request{
		Account: acctAcme, Principal: "root", Action: "read", Object: "account:acme/document:1",
	}, ic); err != nil {
		t.Fatalf("CheckAs: %v", err)
	}
	gc := rec.last(t)
	if gc.Principal != "root" {
		t.Fatalf("GrantContext.Principal = %q, want the operator", gc.Principal)
	}
	if gc.PrincipalKind != "" {
		t.Errorf("GrantContext.PrincipalKind = %q, want empty (unknown, never the target's)", gc.PrincipalKind)
	}
	if gc.Account != acctAcme {
		t.Errorf("GrantContext.Account = %q, want %q", gc.Account, acctAcme)
	}
}
