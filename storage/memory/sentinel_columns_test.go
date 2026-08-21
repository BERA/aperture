package memory_test

import (
	"context"
	"strings"
	"testing"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/storage/memory"
)

// This file proves the three columns that carry NO foreign key are nevertheless
// enforced, in both directions, with the same code and message shape a foreign
// key produces (the "Referential integrity" header in memory.go):
//
//	apt_memberships.account_id -> apt_accounts(id) OR model.AccountWildcard
//	apt_grants.account_id      -> apt_accounts(id) OR model.AccountWildcard
//	apt_grants.subject_id      -> apt_principals | apt_roles | apt_groups,
//	                              whichever subject_kind selects
//
// Its mirror image is storage/sqlite/integrity_test.go. The two seed the
// same world and assert the same outcomes, because the whole point is that a
// caller cannot tell the backends apart — and neither check is a SQL feature, so
// there is nothing here for one backend to get for free.

// ---------------------------------------------------------------------------
// check 1, write side: account_id is a row OR the wildcard
// ---------------------------------------------------------------------------

func TestAWriteCannotNameAnAccountThatDoesNotExist(t *testing.T) {
	ctx := context.Background()

	t.Run("apt_memberships.account_id", func(t *testing.T) {
		s := seedWorld(t)
		mustConstraint(t, "membership stamped with an unknown account",
			s.PutMembership(ctx, model.Membership{PrincipalID: "alice", AccountID: "ghost-co"}))
		if _, err := s.GetMembership(ctx, "alice", "ghost-co"); aerr.CodeOf(err) != aerr.APERTURE_NOT_FOUND {
			t.Fatalf("the refused membership was written anyway: %v", err)
		}
	})

	t.Run("apt_grants.account_id", func(t *testing.T) {
		s := seedWorld(t)
		mustConstraint(t, "grant stamped with an unknown account",
			s.PutGrant(ctx, model.Grant{
				ID: "g-ghost-account", AccountID: "ghost-co",
				Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: "alice"},
				PermissionID: "p-read", Object: "**", Effect: model.EffectAllow,
			}))
		if _, err := s.GetGrant(ctx, "g-ghost-account"); aerr.CodeOf(err) != aerr.APERTURE_NOT_FOUND {
			t.Fatalf("the refused grant was written anyway: %v", err)
		}
	})
}

// TestTheWildcardIsAcceptedOnBothAccountColumns is the case a NAIVE check fails.
// The obvious implementation — "SELECT 1 FROM apt_accounts WHERE id = ?" and
// refuse when it misses — refuses "*" too, because "*" is deliberately not an
// account row and cannot be made one. That would reject every wildcard grant and
// every wildcard membership: the cross-account super-admin, gone.
//
// The test states the trap explicitly: it FIRST proves "*" is absent from
// apt_accounts and cannot be inserted, so nobody can make this pass by seeding a
// "*" row instead of writing the rule.
func TestTheWildcardIsAcceptedOnBothAccountColumns(t *testing.T) {
	ctx := context.Background()
	s := seedWorld(t)

	// The premise: "*" is NOT an account, and the model refuses to make it one.
	if err := s.PutAccount(ctx, model.Account{ID: model.AccountWildcard, Name: "star"}); aerr.CodeOf(err) != aerr.APERTURE_INVALID_INPUT {
		t.Fatalf("ValidateAccount must refuse the wildcard as an account id, got %v", err)
	}
	if _, err := s.GetAccount(ctx, model.AccountWildcard); aerr.CodeOf(err) != aerr.APERTURE_NOT_FOUND {
		t.Fatalf("apt_accounts holds a %q row; the wildcard check would then be vacuous: %v",
			model.AccountWildcard, err)
	}

	// The rule: both columns take it anyway.
	if err := s.PutGrant(ctx, model.Grant{
		ID: "g-star", AccountID: model.AccountWildcard,
		Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: "alice"},
		PermissionID: "p-read", Object: "**", Effect: model.EffectAllow,
	}); err != nil {
		t.Fatalf("wildcard-stamped grant refused: %v\n"+
			"a plain existence check on account_id breaks the cross-account grant", err)
	}
	if err := s.PutMembership(ctx, model.Membership{
		PrincipalID: "bob", AccountID: model.AccountWildcard,
	}); err != nil {
		t.Fatalf("wildcard-stamped membership refused: %v\n"+
			"engine.requireMembership falls back to IsMember(principal, \"*\") before denying", err)
	}

	// And they still function: the wildcard grant is returned for an account it
	// was never stamped with, and the wildcard membership reads back.
	got, err := s.GrantsForSubjects(ctx, "acme", []model.Subject{{Kind: model.SubjectPrincipal, ID: "alice"}})
	if err != nil {
		t.Fatalf("grants for subjects: %v", err)
	}
	var sawStar bool
	for _, g := range got {
		if g.ID == "g-star" {
			sawStar = true
		}
	}
	if !sawStar {
		t.Fatalf("the wildcard grant did not span into acme: %v", got)
	}
	if ok, err := s.IsMember(ctx, "bob", model.AccountWildcard); err != nil || !ok {
		t.Fatalf("IsMember(bob, %q) = %v, %v — the wildcard membership does not function",
			model.AccountWildcard, ok, err)
	}
}

// ---------------------------------------------------------------------------
// check 1, delete side: an account may not be deleted while it is stamped on a
// membership or a grant — the ON DELETE RESTRICT those columns cannot declare
// ---------------------------------------------------------------------------

func TestDeletingAnAccountWithLiveChildrenIsRefused(t *testing.T) {
	ctx := context.Background()
	s := seedWorld(t)

	// alice and bob are members of acme, and g1 is stamped with it.
	err := s.DeleteAccount(ctx, "acme")
	mustConstraint(t, "delete an account with members", err)
	if !strings.Contains(err.Error(), "apt_memberships.account_id") {
		t.Fatalf("refusal does not name the edge that objected: %v", err)
	}
	// The refusal rolled the delete back: the account is still there.
	if _, err := s.GetAccount(ctx, "acme"); err != nil {
		t.Fatalf("the refused delete removed the account anyway: %v", err)
	}

	// Clear the memberships; the grant is what is left holding it.
	for _, p := range []string{"alice", "bob"} {
		if err := s.DeleteMembership(ctx, p, "acme"); err != nil {
			t.Fatalf("delete membership %s: %v", p, err)
		}
	}
	err = s.DeleteAccount(ctx, "acme")
	mustConstraint(t, "delete an account a grant is stamped with", err)
	if !strings.Contains(err.Error(), "apt_grants.account_id") {
		t.Fatalf("refusal does not name the edge that objected: %v", err)
	}

	// With both gone the account is free.
	if err := s.DeleteGrant(ctx, "g1"); err != nil {
		t.Fatalf("delete grant: %v", err)
	}
	if err := s.DeleteAccount(ctx, "acme"); err != nil {
		t.Fatalf("nothing references acme any more, yet: %v", err)
	}
}

// TestWildcardRowsDoNotPinARealAccount is the delete-side half of the same trap:
// a "*"-stamped membership or grant references NO account, so it must not keep a
// real one alive. A check that treated "*" as an ordinary account id would be
// wrong here in the opposite direction.
func TestWildcardRowsDoNotPinARealAccount(t *testing.T) {
	ctx := context.Background()
	s := seedWorld(t)

	if err := s.PutGrant(ctx, model.Grant{
		ID: "g-star", AccountID: model.AccountWildcard,
		Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: "alice"},
		PermissionID: "p-read", Object: "**", Effect: model.EffectAllow,
	}); err != nil {
		t.Fatalf("seed wildcard grant: %v", err)
	}
	if err := s.PutMembership(ctx, model.Membership{
		PrincipalID: "bob", AccountID: model.AccountWildcard,
	}); err != nil {
		t.Fatalf("seed wildcard membership: %v", err)
	}
	// Clear only the acme-stamped children.
	for _, p := range []string{"alice", "bob"} {
		if err := s.DeleteMembership(ctx, p, "acme"); err != nil {
			t.Fatalf("delete membership %s: %v", p, err)
		}
	}
	if err := s.DeleteGrant(ctx, "g1"); err != nil {
		t.Fatalf("delete grant: %v", err)
	}
	if err := s.DeleteAccount(ctx, "acme"); err != nil {
		t.Fatalf("the wildcard rows pinned a real account they never referenced: %v", err)
	}
}

// ---------------------------------------------------------------------------
// check 2, write side: subject_id exists in the table subject_kind selects
// ---------------------------------------------------------------------------

func TestAGrantSubjectMustExistInTheTableItsKindSelects(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		kind  model.SubjectKind
		table string
	}{
		{model.SubjectPrincipal, "apt_principals"},
		{model.SubjectRole, "apt_roles"},
		{model.SubjectGroup, "apt_groups"},
	} {
		t.Run(string(tc.kind), func(t *testing.T) {
			s := seedWorld(t)
			err := s.PutGrant(ctx, model.Grant{
				ID: "g-ghost-subject", AccountID: "acme",
				Subject:      model.Subject{Kind: tc.kind, ID: "ghost"},
				PermissionID: "p-read", Object: "account:acme/**", Effect: model.EffectAllow,
			})
			mustConstraint(t, "grant naming an unknown "+string(tc.kind), err)
			if !strings.Contains(err.Error(), tc.table) {
				t.Fatalf("refusal does not name %s, so subject_kind did not select the table: %v", tc.table, err)
			}
			if _, err := s.GetGrant(ctx, "g-ghost-subject"); aerr.CodeOf(err) != aerr.APERTURE_NOT_FOUND {
				t.Fatalf("the refused grant was written anyway: %v", err)
			}
		})
	}
}

// TestTheSubjectCheckIsPerKindNotAnyTable proves the dispatch is real: the world
// holds a principal "alice", a role "r-admin" and a group "eng", and naming one
// of them under ANOTHER kind is refused. A check that asked "does this id exist
// anywhere?" would accept all three.
func TestTheSubjectCheckIsPerKindNotAnyTable(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		kind model.SubjectKind
		id   string
	}{
		{"a principal id under kind=role", model.SubjectRole, "alice"},
		{"a role id under kind=group", model.SubjectGroup, "r-admin"},
		{"a group id under kind=principal", model.SubjectPrincipal, "eng"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := seedWorld(t)
			mustConstraint(t, tc.name, s.PutGrant(ctx, model.Grant{
				ID: "g-crossed", AccountID: "acme",
				Subject:      model.Subject{Kind: tc.kind, ID: tc.id},
				PermissionID: "p-read", Object: "account:acme/**", Effect: model.EffectAllow,
			}))
		})
	}
}

// TestEveryValidSubjectKindIsWritable is the anti-vacuity guard for the two
// tests above: each kind, pointed at a real row of its own table, goes through.
func TestEveryValidSubjectKindIsWritable(t *testing.T) {
	ctx := context.Background()
	s := seedWorld(t)

	for id, sub := range map[string]model.Subject{
		"g-p": {Kind: model.SubjectPrincipal, ID: "alice"},
		"g-r": {Kind: model.SubjectRole, ID: "r-admin"},
		"g-g": {Kind: model.SubjectGroup, ID: "eng"},
	} {
		if err := s.PutGrant(ctx, model.Grant{
			ID: id, AccountID: "acme", Subject: sub,
			PermissionID: "p-read", Object: "account:acme/**", Effect: model.EffectAllow,
		}); err != nil {
			t.Fatalf("grant %s (%s %q) refused: %v", id, sub.Kind, sub.ID, err)
		}
	}
}

// ---------------------------------------------------------------------------
// check 2, delete side: a subject a grant names may not be deleted
// ---------------------------------------------------------------------------

func TestDeletingASubjectAGrantNamesIsRefused(t *testing.T) {
	ctx := context.Background()

	// Each case clears the entity's OTHER (foreign-key) edges first, so the only
	// thing left refusing the delete is the polymorphic grant subject.
	for _, tc := range []struct {
		name  string
		kind  model.SubjectKind
		id    string
		table string
		free  func(t *testing.T, s *memory.Store)
		del   func(s *memory.Store) error
	}{
		{
			name: "principal", kind: model.SubjectPrincipal, id: "alice", table: "apt_principals",
			free: func(t *testing.T, s *memory.Store) {
				t.Helper()
				if err := s.DeleteMembership(ctx, "alice", "acme"); err != nil {
					t.Fatalf("free membership: %v", err)
				}
				if err := s.PutGroup(ctx, model.Group{ID: "eng", Name: "Engineering"}); err != nil {
					t.Fatalf("empty the group: %v", err)
				}
			},
			del: func(s *memory.Store) error { return s.DeletePrincipal(ctx, "alice") },
		},
		{
			name: "role", kind: model.SubjectRole, id: "r-admin", table: "apt_roles",
			free: func(t *testing.T, s *memory.Store) {
				t.Helper()
				// alice holds r-admin; drop the assignment so only the grant pins it.
				if err := s.PutPrincipal(ctx, model.Principal{
					ID: "alice", Kind: model.PrincipalUser, Identity: "user:alice",
				}); err != nil {
					t.Fatalf("drop alice's role assignment: %v", err)
				}
			},
			del: func(s *memory.Store) error { return s.DeleteRole(ctx, "r-admin") },
		},
		{
			name: "group", kind: model.SubjectGroup, id: "eng", table: "apt_groups",
			free: func(t *testing.T, s *memory.Store) {},
			del:  func(s *memory.Store) error { return s.DeleteGroup(ctx, "eng") },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := seedWorld(t)
			// Re-point the seeded grant at this subject.
			if err := s.PutGrant(ctx, model.Grant{
				ID: "g1", AccountID: "acme",
				Subject:      model.Subject{Kind: tc.kind, ID: tc.id},
				PermissionID: "p-read", Object: "account:acme/**", Effect: model.EffectAllow,
			}); err != nil {
				t.Fatalf("re-point the grant: %v", err)
			}
			tc.free(t, s)

			err := tc.del(s)
			mustConstraint(t, "delete a "+tc.name+" a grant names", err)
			if !strings.Contains(err.Error(), tc.table) {
				t.Fatalf("refusal does not name %s: %v", tc.table, err)
			}
			// The refusal rolled back: the entity is still there.
			if err := tc.del(s); aerr.CodeOf(err) != aerr.APERTURE_STORAGE_CONSTRAINT {
				t.Fatalf("the refused delete removed the %s anyway: %v", tc.name, err)
			}

			// Revoke the grant and the subject is free.
			if err := s.DeleteGrant(ctx, "g1"); err != nil {
				t.Fatalf("delete grant: %v", err)
			}
			if err := tc.del(s); err != nil {
				t.Fatalf("nothing names the %s any more, yet: %v", tc.name, err)
			}
		})
	}
}
