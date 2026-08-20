package service

import (
	"context"
	"strings"
	"testing"

	"github.com/frankbardon/aperture/audit"
	"github.com/frankbardon/aperture/authz"
	"github.com/frankbardon/aperture/engine"
	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/storage/memory"
)

// newManagedFacade wires a fully-mutating facade over an in-memory store with
// the given entity-management posture, seeding system-admin authority for
// "alice" (so an allowed write actually succeeds) and a bare principal
// "mallory" with no authority at all (so the gate's position relative to
// authorization is observable). The audit recorder is synchronous, which is what
// lets a refusal be read straight back out of the trail.
func newManagedFacade(t *testing.T, m ManagedEntities) (*Service, *memory.Store, *audit.Recorder) {
	t.Helper()
	ctx := context.Background()
	store := memory.New()
	if err := store.Setup(ctx); err != nil {
		t.Fatalf("setup: %v", err)
	}
	mustPut(t, store.PutObjectType(ctx, model.ObjectType{Name: "system", Actions: []string{authz.AdminAction}}))
	mustPut(t, store.PutPermission(ctx, model.Permission{ID: "p-admin", ObjectType: "system", Action: authz.AdminAction}))
	mustPut(t, store.PutPrincipal(ctx, model.Principal{ID: "alice", Kind: model.PrincipalUser, Identity: "user:alice"}))
	mustPut(t, store.PutPrincipal(ctx, model.Principal{ID: "mallory", Kind: model.PrincipalUser, Identity: "user:mallory"}))
	mustPut(t, store.PutAccount(ctx, model.Account{ID: "acme", Name: "Acme"}))
	mustPut(t, store.PutGrant(ctx, model.Grant{
		ID: "g-admin", AccountID: "acme",
		Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: "alice"},
		PermissionID: "p-admin", Object: "**", Effect: model.EffectAllow,
	}))

	eng := engine.New(store)
	rec := audit.New(store, audit.WithClock(deterministicClock()), audit.WithIDFunc(seqID()), audit.WithSampleRate(1))
	svc := New(eng, WithStorage(store), WithGate(authz.NewGate(eng)), WithAudit(rec), WithManagedEntities(m))
	return svc, store, rec
}

// lifecycleCall names one gated lifecycle method and how to invoke it, so the
// tests below can assert the SAME behaviour across all six without six copies.
type lifecycleCall struct {
	name string
	call func(*Service, context.Context, Actor) error
}

func lifecycleCalls() map[string][]lifecycleCall {
	return map[string][]lifecycleCall{
		"account": {
			{"PutAccount", func(s *Service, ctx context.Context, a Actor) error {
				return s.PutAccount(ctx, a, model.Account{ID: "beta", Name: "Beta"})
			}},
			{"DeleteAccount", func(s *Service, ctx context.Context, a Actor) error {
				return s.DeleteAccount(ctx, a, "acme")
			}},
		},
		"principal": {
			{"PutPrincipal", func(s *Service, ctx context.Context, a Actor) error {
				return s.PutPrincipal(ctx, a, model.Principal{ID: "bob", Kind: model.PrincipalUser, Identity: "user:bob"})
			}},
			{"DeletePrincipal", func(s *Service, ctx context.Context, a Actor) error {
				return s.DeletePrincipal(ctx, a, "mallory")
			}},
		},
		"membership": {
			{"PutMembership", func(s *Service, ctx context.Context, a Actor) error {
				return s.PutMembership(ctx, a, model.Membership{PrincipalID: "alice", AccountID: "acme"})
			}},
			{"DeleteMembership", func(s *Service, ctx context.Context, a Actor) error {
				return s.DeleteMembership(ctx, a, "alice", "acme")
			}},
		},
	}
}

func lockedPosture(kind string) ManagedEntities {
	switch kind {
	case "account":
		return ManagedEntities{Accounts: ManagedNo}
	case "principal":
		return ManagedEntities{Principals: ManagedNo}
	default:
		return ManagedEntities{Memberships: ManagedNo}
	}
}

// TestUnmanagedEntityRefusesTheWholeLifecycle proves the switch locks Put AND
// Delete — not creation alone — and that the refusal names the entity kind a
// human has to act on, even though one code is shared by all three.
func TestUnmanagedEntityRefusesTheWholeLifecycle(t *testing.T) {
	ctx := context.Background()
	alice := Actor{Principal: "alice", Account: "acme"}
	for kind, calls := range lifecycleCalls() {
		for _, c := range calls {
			t.Run(c.name, func(t *testing.T) {
				svc, _, rec := newManagedFacade(t, lockedPosture(kind))
				defer rec.Close()
				err := c.call(svc, ctx, alice)
				if err == nil {
					t.Fatalf("%s: expected a refusal with %s tier unmanaged", c.name, kind)
				}
				if got := aerr.CodeOf(err); got != aerr.APERTURE_ENTITY_UNMANAGED {
					t.Fatalf("%s: code = %q, want %q", c.name, got, aerr.APERTURE_ENTITY_UNMANAGED)
				}
				if !strings.Contains(err.Error(), kind) {
					t.Fatalf("%s: message must name the entity kind %q, got %q", c.name, kind, err.Error())
				}
			})
		}
	}
}

// TestUnmanagedGateRunsBeforeAuthorization is the ordering assertion the whole
// design rests on. A system-admin is denied nothing, so if the gate ran after
// authorize it would still report "unmanaged" here — the case that discriminates
// is an actor with NO authority at all: it must ALSO hear "unmanaged", because
// permission never got to decide.
func TestUnmanagedGateRunsBeforeAuthorization(t *testing.T) {
	ctx := context.Background()
	for kind, calls := range lifecycleCalls() {
		for _, c := range calls {
			t.Run(c.name, func(t *testing.T) {
				svc, _, rec := newManagedFacade(t, lockedPosture(kind))
				defer rec.Close()
				// mallory holds no admin authority whatsoever.
				err := c.call(svc, ctx, Actor{Principal: "mallory", Account: "acme"})
				if got := aerr.CodeOf(err); got != aerr.APERTURE_ENTITY_UNMANAGED {
					t.Fatalf("%s: unauthorized actor got %q, want %q — the gate must precede authorize",
						c.name, got, aerr.APERTURE_ENTITY_UNMANAGED)
				}
			})
		}
	}
}

// TestUnmanagedRefusalIsAudited confirms the deferred recordMutation still fires
// with the gate in front of it, and carries the coded error — so a locked
// deployment's refusals are as visible in the trail as a denied one's.
func TestUnmanagedRefusalIsAudited(t *testing.T) {
	svc, store, rec := newManagedFacade(t, ManagedEntities{Accounts: ManagedNo})
	defer rec.Close()
	ctx := context.Background()

	if err := svc.PutAccount(ctx, Actor{Principal: "alice", Account: "acme"}, model.Account{ID: "beta"}); err == nil {
		t.Fatal("expected PutAccount to be refused")
	}
	events := queryAudit(t, store, model.AuditFilter{})
	var found *model.AuditEvent
	for i := range events {
		if events[i].Action == "PutAccount" {
			found = &events[i]
			break
		}
	}
	if found == nil {
		t.Fatal("refused PutAccount was not audited")
	}
	if found.Outcome != model.OutcomeFailure {
		t.Fatalf("outcome = %q, want %q", found.Outcome, model.OutcomeFailure)
	}
	if !strings.Contains(found.Reason, "does not manage accounts") {
		t.Fatalf("audit reason must carry the refusal, got %q", found.Reason)
	}
	if found.Target != "account:beta" {
		t.Fatalf("target = %q, want account:beta", found.Target)
	}
}

// TestManagedSwitchesAreIndependent locks accounts only and proves the other two
// kinds keep working — the three switches are not one switch.
func TestManagedSwitchesAreIndependent(t *testing.T) {
	svc, _, rec := newManagedFacade(t, ManagedEntities{Accounts: ManagedNo})
	defer rec.Close()
	ctx := context.Background()
	alice := Actor{Principal: "alice", Account: "acme"}

	if err := svc.PutAccount(ctx, alice, model.Account{ID: "beta"}); aerr.CodeOf(err) != aerr.APERTURE_ENTITY_UNMANAGED {
		t.Fatalf("PutAccount: code = %q, want unmanaged", aerr.CodeOf(err))
	}
	if err := svc.PutPrincipal(ctx, alice, model.Principal{ID: "bob", Kind: model.PrincipalUser, Identity: "user:bob"}); err != nil {
		t.Fatalf("PutPrincipal must still work with only accounts locked: %v", err)
	}
	if err := svc.PutMembership(ctx, alice, model.Membership{PrincipalID: "bob", AccountID: "acme"}); err != nil {
		t.Fatalf("PutMembership must still work with only accounts locked: %v", err)
	}
}

// TestZeroPostureManagesEverything pins the polarity: the zero ManagedEntities —
// what a facade built without WithManagedEntities holds — must mean MANAGED, not
// the reverse. Every existing caller depends on this.
func TestZeroPostureManagesEverything(t *testing.T) {
	svc, _, rec := newManagedFacade(t, ManagedEntities{})
	defer rec.Close()
	ctx := context.Background()
	alice := Actor{Principal: "alice", Account: "acme"}

	mustPut(t, svc.PutAccount(ctx, alice, model.Account{ID: "beta", Name: "Beta"}))
	mustPut(t, svc.PutPrincipal(ctx, alice, model.Principal{ID: "bob", Kind: model.PrincipalUser, Identity: "user:bob"}))
	mustPut(t, svc.PutMembership(ctx, alice, model.Membership{PrincipalID: "bob", AccountID: "acme"}))
	mustPut(t, svc.DeleteMembership(ctx, alice, "bob", "acme"))
	mustPut(t, svc.DeletePrincipal(ctx, alice, "bob"))
	mustPut(t, svc.DeleteAccount(ctx, alice, "beta"))
}

// TestManagedYesIsIndistinguishableFromDefault — an explicit opt-in records an
// operator's choice but must not behave differently from the default.
func TestManagedYesIsIndistinguishableFromDefault(t *testing.T) {
	svc, _, rec := newManagedFacade(t, ManagedEntities{
		Accounts: ManagedYes, Principals: ManagedYes, Memberships: ManagedYes,
	})
	defer rec.Close()
	ctx := context.Background()
	alice := Actor{Principal: "alice", Account: "acme"}
	mustPut(t, svc.PutAccount(ctx, alice, model.Account{ID: "beta", Name: "Beta"}))
	mustPut(t, svc.PutPrincipal(ctx, alice, model.Principal{ID: "bob", Kind: model.PrincipalUser, Identity: "user:bob"}))
	mustPut(t, svc.PutMembership(ctx, alice, model.Membership{PrincipalID: "bob", AccountID: "acme"}))
}

// TestUnmanagedRefusalLeaksNoEntityData — the refusal is deployment posture, the
// same fact for every caller, so it must name only the KIND. An id in the
// message would turn a config switch into a cross-account disclosure channel.
func TestUnmanagedRefusalLeaksNoEntityData(t *testing.T) {
	svc, _, rec := newManagedFacade(t, ManagedEntities{Memberships: ManagedNo})
	defer rec.Close()
	ctx := context.Background()

	err := svc.PutMembership(ctx, Actor{Principal: "alice", Account: "acme"},
		model.Membership{PrincipalID: "secret-principal", AccountID: "secret-account"})
	if err == nil {
		t.Fatal("expected a refusal")
	}
	for _, leak := range []string{"secret-principal", "secret-account"} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("refusal message leaked %q: %q", leak, err.Error())
		}
	}
}
