package service

import (
	stderrors "errors"
	"strings"
	"testing"

	"context"

	"github.com/frankbardon/aperture/audit"
	"github.com/frankbardon/aperture/authz"
	"github.com/frankbardon/aperture/engine"
	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/storage/memory"
	"github.com/frankbardon/aperture/storage/sqlite"
)

// E3-S1: every delete call path through the facade, swept against RESTRICT.
//
// THE ENUMERATION. These are all of them in service/ (delegation/ has one, and
// it is swept in delegation/delete_order_test.go). "Child edges" names what the
// storage layer now refuses to strand; a path with none cannot be refused for
// referential reasons at all.
//
//	service/mutations.go
//	  DeleteObjectType   -> store.DeleteObjectType    apt_permissions.object_type
//	  DeletePermission   -> store.DeletePermission    apt_role_permissions.permission_id,
//	                                                  apt_grants.permission_id
//	  DeletePrincipal    -> store.DeletePrincipal     apt_memberships.principal_id,
//	                                                  apt_group_members.principal_id,
//	                                                  apt_grants.(subject_kind,subject_id)
//	                                                  [apt_principal_roles.principal_id CASCADEs]
//	  DeleteRole         -> store.DeleteRole          apt_principal_roles.role_id,
//	                                                  apt_grants.(subject_kind,subject_id)
//	                                                  [apt_role_permissions.role_id CASCADEs]
//	  DeleteGroup        -> store.DeleteGroup         apt_grants.(subject_kind,subject_id)
//	                                                  [apt_group_members.group_id CASCADEs]
//	  DeleteAccount      -> store.DeleteAccount       apt_memberships.account_id,
//	                                                  apt_grants.account_id (both Go-checked)
//	  DeleteMembership   -> store.DeleteMembership    none — a membership is a leaf edge
//	  DeleteGrant        -> store.DeleteGrant         none — a grant is authority, nothing cites it
//	service/templates.go
//	  DeleteTemplate     -> store.DeleteTemplate      none — apt_templates.apt_grants is a JSON
//	                                                  value column, not a relationship
//	  BulkDeleteGrants   -> tx.DeleteGrant (Atomic)   none — grants only, so batch order is free
//	service/rules.go
//	  DeleteRule         -> store.DeleteRule          none — a permission names a rule by string,
//	                                                  there is no key
//	service/simulate.go
//	  overlayStore.Delete*                            never reaches storage (APERTURE_UNIMPLEMENTED);
//	                                                  the what-if overlay is structurally read-only
//
// EVERY path is a SINGLE storage call. None of them composes a parent delete
// with a child delete, so there is no ordering inside the facade to get wrong:
// the order is the CALLER's, and TestFacadeDeletesChildrenBeforeParents pins the
// one that works end to end. The six paths with child edges refuse when the
// caller skipped a step, and TestBlockedFacadeDeleteKeepsTheConstraintCode pins
// that the refusal arrives with APERTURE_STORAGE_CONSTRAINT still on it.

// deleteBackends runs the sweep against both live backends. SQLite refuses
// through real SQL foreign keys and the Go-level sentinel checks; memory refuses
// through the hand-written mirror of both. A facade that flattened or re-stamped
// the error would show it on one of them.
type deleteBackend struct {
	name string
	open func(t *testing.T) model.Storage
}

func deleteBackends() []deleteBackend {
	return []deleteBackend{
		{"memory", func(t *testing.T) model.Storage { return memory.New() }},
		{"sqlite", func(t *testing.T) model.Storage {
			t.Helper()
			st, err := sqlite.OpenMemory()
			if err != nil {
				t.Fatalf("open sqlite: %v", err)
			}
			t.Cleanup(func() { _ = st.Close() })
			return st
		}},
	}
}

// memoryBackend is the one the audit assertion uses; it needs no cleanup and the
// trail is the same either way.
func memoryBackend(t *testing.T) model.Storage { return memory.New() }

// root is the platform administrator the sweep acts as. Its admin grant is
// stamped model.AccountWildcard rather than "acme" ON PURPOSE: the teardown
// deletes every grant in acme, and an account-stamped admin grant would revoke
// the actor's own authority halfway through and turn a referential-integrity
// test into an authorization one.
const deleteActorAccount = "acme"

func deleteActor() Actor { return Actor{Principal: "root", Account: deleteActorAccount} }

// newDeleteFacade builds a fully-mutating facade over a referentially COMPLETE
// model: every id below names a row that exists, which is the state E2 made
// mandatory. The shape covers all nine child edges the six affected deletes can
// trip over.
func newDeleteFacade(t *testing.T, open func(*testing.T) model.Storage) (*Service, model.Storage) {
	t.Helper()
	ctx := context.Background()
	store := open(t)
	if err := store.Setup(ctx); err != nil {
		t.Fatalf("setup: %v", err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	must(store.PutObjectType(ctx, model.ObjectType{Name: "system", Actions: []string{authz.AdminAction}}))
	must(store.PutPermission(ctx, model.Permission{ID: "p-admin", ObjectType: "system", Action: authz.AdminAction}))
	must(store.PutPrincipal(ctx, model.Principal{ID: "root", Kind: model.PrincipalUser, Identity: "user:root"}))
	must(store.PutGrant(ctx, model.Grant{
		ID: "g-admin", AccountID: model.AccountWildcard,
		Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: "root"},
		PermissionID: "p-admin", Object: "**", Effect: model.EffectAllow,
	}))

	// The model under test.
	must(store.PutAccount(ctx, model.Account{ID: "acme", Name: "Acme"}))
	must(store.PutObjectType(ctx, model.ObjectType{Name: "doc", Actions: []string{"read"}}))
	must(store.PutPermission(ctx, model.Permission{ID: "p-read", ObjectType: "doc", Action: "read"}))
	must(store.PutRole(ctx, model.Role{ID: "r-reader", Name: "reader", PermissionIDs: []string{"p-read"}}))
	must(store.PutPrincipal(ctx, model.Principal{
		ID: "bob", Kind: model.PrincipalUser, Identity: "user:bob", RoleIDs: []string{"r-reader"},
	}))
	must(store.PutGroup(ctx, model.Group{ID: "gr-team", Name: "team", MemberPrincipalIDs: []string{"bob"}}))
	must(store.PutMembership(ctx, model.Membership{PrincipalID: "bob", AccountID: "acme"}))
	must(store.PutGrant(ctx, model.Grant{
		ID: "g-bob", AccountID: "acme",
		Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: "bob"},
		PermissionID: "p-read", Object: "account:acme/**", Effect: model.EffectAllow,
	}))
	must(store.PutGrant(ctx, model.Grant{
		ID: "g-team", AccountID: "acme",
		Subject:      model.Subject{Kind: model.SubjectGroup, ID: "gr-team"},
		PermissionID: "p-read", Object: "account:acme/**", Effect: model.EffectAllow,
	}))

	eng := engine.New(store)
	rec := audit.New(store, audit.WithClock(deterministicClock()), audit.WithIDFunc(seqID()), audit.WithSampleRate(1))
	t.Cleanup(func() { rec.Close() })
	svc := New(eng, WithStorage(store), WithGate(authz.NewGate(eng)), WithAudit(rec))
	return svc, store
}

// TestFacadeDeletesChildrenBeforeParents tears the whole model down through the
// facade, children first, and requires EVERY step to succeed. This is the
// end-to-end half of E3-S1: the sequence a caller has to follow after RESTRICT
// exists entirely within the facade's own surface — no store access, no new
// method, and nothing the schema made unreachable.
//
// Two of the steps are edits rather than deletes, because that is how the facade
// spells "detach a join row": a principal's roles and a group's members are
// fields on the parent entity, so the child rows come off with a Put.
func TestFacadeDeletesChildrenBeforeParents(t *testing.T) {
	ctx := context.Background()
	for _, be := range deleteBackends() {
		t.Run(be.name, func(t *testing.T) {
			svc, _ := newDeleteFacade(t, be.open)
			actor := deleteActor()

			steps := []struct {
				what string
				do   func() error
			}{
				// Grants cite a permission, an account, and a subject: they go first
				// or nothing else can move.
				{"DeleteGrant(g-team)", func() error { return svc.DeleteGrant(ctx, actor, "g-team") }},
				{"DeleteGrant(g-bob)", func() error { return svc.DeleteGrant(ctx, actor, "g-bob") }},
				// The group's own member rows cascade with it; only the grant that
				// named it as a subject held it, and that is gone.
				{"DeleteGroup(gr-team)", func() error { return svc.DeleteGroup(ctx, actor, "gr-team") }},
				// Detach bob from the role (apt_principal_roles) before the role goes.
				{"PutPrincipal(bob, no roles)", func() error {
					return svc.PutPrincipal(ctx, actor, model.Principal{
						ID: "bob", Kind: model.PrincipalUser, Identity: "user:bob",
					})
				}},
				// The role's permission rows cascade with it.
				{"DeleteRole(r-reader)", func() error { return svc.DeleteRole(ctx, actor, "r-reader") }},
				{"DeleteMembership(bob@acme)", func() error { return svc.DeleteMembership(ctx, actor, "bob", "acme") }},
				{"DeletePrincipal(bob)", func() error { return svc.DeletePrincipal(ctx, actor, "bob") }},
				{"DeletePermission(p-read)", func() error { return svc.DeletePermission(ctx, actor, "p-read") }},
				{"DeleteObjectType(doc)", func() error { return svc.DeleteObjectType(ctx, actor, "doc") }},
				{"DeleteAccount(acme)", func() error { return svc.DeleteAccount(ctx, actor, "acme") }},
			}
			for _, s := range steps {
				if err := s.do(); err != nil {
					t.Fatalf("%s must succeed once its children are gone, got %v", s.what, err)
				}
			}
		})
	}
}

// blockedDelete names one parent-before-child call and the child that is holding
// it, so the refusal can be attributed rather than merely observed.
type blockedDelete struct {
	what  string
	child string
	do    func(*Service, context.Context, Actor) error
}

func blockedDeletes() []blockedDelete {
	return []blockedDelete{
		{"DeleteObjectType(doc)", "permission p-read", func(s *Service, ctx context.Context, a Actor) error {
			return s.DeleteObjectType(ctx, a, "doc")
		}},
		{"DeletePermission(p-read)", "role r-reader and grants g-bob/g-team", func(s *Service, ctx context.Context, a Actor) error {
			return s.DeletePermission(ctx, a, "p-read")
		}},
		{"DeletePrincipal(bob)", "membership bob@acme, group gr-team, grant g-bob", func(s *Service, ctx context.Context, a Actor) error {
			return s.DeletePrincipal(ctx, a, "bob")
		}},
		{"DeleteRole(r-reader)", "bob's role assignment", func(s *Service, ctx context.Context, a Actor) error {
			return s.DeleteRole(ctx, a, "r-reader")
		}},
		{"DeleteGroup(gr-team)", "grant g-team", func(s *Service, ctx context.Context, a Actor) error {
			return s.DeleteGroup(ctx, a, "gr-team")
		}},
		{"DeleteAccount(acme)", "membership bob@acme and grants g-bob/g-team", func(s *Service, ctx context.Context, a Actor) error {
			return s.DeleteAccount(ctx, a, "acme")
		}},
	}
}

// TestBlockedFacadeDeleteKeepsTheConstraintCode is the propagation half of
// E3-S1, and it asserts the SPECIFIC code rather than "an error happened" — the
// distinction is the whole point. APERTURE_STORAGE_CONSTRAINT is what tells a
// caller "nothing failed, no retry helps, delete the children first", and its
// Registry fixups spell out which children. Flattened to APERTURE_STORAGE the
// caller gets "the storage layer broke", which is both wrong and unactionable.
func TestBlockedFacadeDeleteKeepsTheConstraintCode(t *testing.T) {
	ctx := context.Background()
	for _, be := range deleteBackends() {
		for _, bd := range blockedDeletes() {
			t.Run(be.name+"/"+bd.what, func(t *testing.T) {
				svc, _ := newDeleteFacade(t, be.open)
				err := bd.do(svc, ctx, deleteActor())
				if err == nil {
					t.Fatalf("%s must be refused while %s still references it", bd.what, bd.child)
				}
				if got := aerr.CodeOf(err); got != aerr.APERTURE_STORAGE_CONSTRAINT {
					t.Fatalf("%s: code = %q, want %q (a flattened code hides the fixups)",
						bd.what, got, aerr.APERTURE_STORAGE_CONSTRAINT)
				}
				// The guidance a caller acts on is keyed by the code, so the code
				// surviving is only useful if the Registry entry actually carries it.
				entry, ok := aerr.Registry[aerr.APERTURE_STORAGE_CONSTRAINT]
				if !ok || len(entry.Fixups) == 0 {
					t.Fatalf("APERTURE_STORAGE_CONSTRAINT must carry Registry fixup guidance")
				}
			})
		}
	}
}

// TestBlockedFacadeDeleteIsNotDoubleWrapped is the assertion CodeOf alone cannot
// make. CodeOf reports the OUTERMOST code, so it would still read
// APERTURE_STORAGE_CONSTRAINT if the facade wrapped a constraint error in a
// SECOND constraint error — and the message a human reads would have picked up a
// duplicate prefix and buried the storage layer's description of the edge.
//
// aerr.Wrap does NOT pass an existing code through; it stamps the code it is
// given onto a fresh CodedError. So "the code survives translation" is a property
// of the CALL SITES returning storage errors unwrapped, not of the wrapper, and
// it has to be asserted rather than assumed. This walks the whole error chain and
// requires exactly ONE Aperture-coded error in it: the storage layer's own.
func TestBlockedFacadeDeleteIsNotDoubleWrapped(t *testing.T) {
	ctx := context.Background()
	for _, be := range deleteBackends() {
		for _, bd := range blockedDeletes() {
			t.Run(be.name+"/"+bd.what, func(t *testing.T) {
				svc, _ := newDeleteFacade(t, be.open)
				err := bd.do(svc, ctx, deleteActor())
				if err == nil {
					t.Fatalf("%s must be refused", bd.what)
				}
				var coded int
				for e := err; e != nil; e = stderrors.Unwrap(e) {
					var ce *aerr.CodedError
					if stderrors.As(e, &ce) && ce == e {
						coded++
					}
				}
				if coded != 1 {
					t.Fatalf("%s: error chain carries %d Aperture-coded errors, want exactly 1 — "+
						"the facade must return the storage error, not re-stamp it: %v", bd.what, coded, err)
				}
				// The refusal a caller reads is the storage layer's own, so the
				// message still opens with the constraint code and nothing was
				// prefixed in front of it.
				if !strings.HasPrefix(err.Error(), "["+string(aerr.APERTURE_STORAGE_CONSTRAINT)+"]") {
					t.Fatalf("%s: refusal was re-wrapped before the caller saw it: %q", bd.what, err.Error())
				}
			})
		}
	}
}

// TestBlockedFacadeDeleteWritesNothing — RESTRICT refuses, it does not partially
// apply. Every entity the blocked call touched must still be readable afterwards,
// or a "failed" delete would have quietly succeeded in part.
func TestBlockedFacadeDeleteWritesNothing(t *testing.T) {
	ctx := context.Background()
	for _, be := range deleteBackends() {
		t.Run(be.name, func(t *testing.T) {
			svc, store := newDeleteFacade(t, be.open)
			actor := deleteActor()
			for _, bd := range blockedDeletes() {
				if err := bd.do(svc, ctx, actor); err == nil {
					t.Fatalf("%s should have been refused", bd.what)
				}
			}
			if _, err := store.GetObjectType(ctx, "doc"); err != nil {
				t.Fatalf("object type doc must survive a refused delete: %v", err)
			}
			if _, err := store.GetPermission(ctx, "p-read"); err != nil {
				t.Fatalf("permission p-read must survive: %v", err)
			}
			if _, err := store.GetPrincipal(ctx, "bob"); err != nil {
				t.Fatalf("principal bob must survive: %v", err)
			}
			if _, err := store.GetRole(ctx, "r-reader"); err != nil {
				t.Fatalf("role r-reader must survive: %v", err)
			}
			if _, err := store.GetGroup(ctx, "gr-team"); err != nil {
				t.Fatalf("group gr-team must survive: %v", err)
			}
			if _, err := store.GetAccount(ctx, "acme"); err != nil {
				t.Fatalf("account acme must survive: %v", err)
			}
			// And the children that did the blocking are all still there.
			for _, id := range []string{"g-bob", "g-team"} {
				if _, err := store.GetGrant(ctx, id); err != nil {
					t.Fatalf("grant %s must survive: %v", id, err)
				}
			}
			member, err := store.IsMember(ctx, "bob", "acme")
			if err != nil || !member {
				t.Fatalf("membership bob@acme must survive: member=%v err=%v", member, err)
			}
		})
	}
}

// TestBlockedFacadeDeleteIsAudited — a refusal is a mutation outcome, so the
// deferred recordMutation must still fire and must carry the coded reason. An
// operator debugging "why can't I delete this account" reads the trail, and a
// refusal missing from it is indistinguishable from a call that never happened.
func TestBlockedFacadeDeleteIsAudited(t *testing.T) {
	ctx := context.Background()
	svc, store := newDeleteFacade(t, memoryBackend)
	if err := svc.DeleteAccount(ctx, deleteActor(), "acme"); err == nil {
		t.Fatal("DeleteAccount should have been refused")
	}
	events, err := store.QueryAudit(ctx, model.AuditFilter{})
	if err != nil {
		t.Fatalf("query audit: %v", err)
	}
	for _, ev := range events {
		if ev.Action != "DeleteAccount" {
			continue
		}
		if ev.Outcome != model.OutcomeFailure {
			t.Fatalf("outcome = %q, want %q", ev.Outcome, model.OutcomeFailure)
		}
		if !strings.Contains(ev.Reason, string(aerr.APERTURE_STORAGE_CONSTRAINT)) {
			t.Fatalf("audit reason must carry the constraint code, got %q", ev.Reason)
		}
		return
	}
	t.Fatal("refused DeleteAccount was not audited")
}

// TestGrantTemplateAndRuleDeletesAreUnaffected confirms — rather than assumes —
// the three paths the schema leaves alone. A grant is cited by nothing; a
// template's grants and an object type's actions are JSON value columns, not
// relationships; and a permission names a rule by string with no key behind it.
// All three must still delete while the model around them is fully populated.
func TestGrantTemplateAndRuleDeletesAreUnaffected(t *testing.T) {
	ctx := context.Background()
	for _, be := range deleteBackends() {
		t.Run(be.name, func(t *testing.T) {
			svc, store := newDeleteFacade(t, be.open)
			actor := deleteActor()

			if err := store.PutTemplate(ctx, model.Template{
				Name: "onboard", Version: 1,
				Grants: []model.TemplateGrant{{
					Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: "bob"},
					PermissionID: "p-read", Object: "account:acme/**", Effect: model.EffectAllow,
				}},
			}); err != nil {
				t.Fatalf("seed template: %v", err)
			}
			if err := store.PutRule(ctx, model.Rule{
				Name: "business-hours", AST: []byte(`{"type":"var","name":"object.x"}`),
			}); err != nil {
				t.Fatalf("seed rule: %v", err)
			}

			// Grants: single and bulk, with the permission, subject, and account
			// they cite all still present.
			if err := svc.DeleteGrant(ctx, actor, "g-team"); err != nil {
				t.Fatalf("DeleteGrant has no child edges and must succeed: %v", err)
			}
			if err := svc.BulkDeleteGrants(ctx, actor, []string{"g-bob"}); err != nil {
				t.Fatalf("BulkDeleteGrants has no child edges and must succeed: %v", err)
			}
			if err := svc.DeleteTemplate(ctx, actor, "onboard", 1); err != nil {
				t.Fatalf("DeleteTemplate must be unaffected by referential integrity: %v", err)
			}
			if err := svc.DeleteRule(ctx, actor, "business-hours"); err != nil {
				t.Fatalf("DeleteRule must be unaffected by referential integrity: %v", err)
			}
		})
	}
}
