package server_test

import (
	"context"
	"testing"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/internal/wire/rpc"
	"github.com/frankbardon/aperture/model"

	"github.com/twitchtv/twirp"
)

// E3-S2, the Twirp half. Aperture's storage now carries real foreign keys, so a
// delete with live children is REFUSED where it previously succeeded by
// orphaning rows. Every mutation on this surface is a single facade call, so the
// ordering that has to hold is the CALLER's — the admin UI's, or any client
// driving these RPCs — and the surface's job is to make the refusal legible
// enough for that caller to fix it.
//
// Two properties are asserted here, and neither holds by construction:
//
//  1. A full teardown driven children-first through the RPCs SUCCEEDS. Nothing
//     else in this package tears the whole model down; crud_smoke_test.go
//     deletes leaf entities it just created.
//  2. A parent-first delete is refused with APERTURE_STORAGE_CONSTRAINT, and the
//     code survives the twirp translation. mapErr attaches it as meta["code"],
//     which is the ONLY machine-readable channel a Twirp client has — the twirp
//     ErrorCode is a coarse transport class and the message is prose. A handler
//     that wrapped the facade's error before returning it would re-stamp the
//     code (aerr.Wrap does not pass an existing code through) and this surface
//     would tell the client "internal error" for a mistake the client made and
//     can fix.

// twirpCode returns the Aperture code a Twirp error carries in meta["code"],
// which is what mapErr puts there. It returns "" when the error is not a Twirp
// error or carries no code, so a burial shows up as an empty string rather than
// a panic.
func twirpCode(err error) string {
	te, ok := err.(twirp.Error)
	if !ok {
		return ""
	}
	return te.Meta("code")
}

// wantConstraint asserts one RPC error is the referential-integrity refusal,
// with the specific code intact — not merely that the call failed. "It errored"
// would pass against a handler that flattened every failure to Internal.
func wantConstraint(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected a refusal, got success", what)
	}
	if got := twirpCode(err); got != string(aerr.APERTURE_STORAGE_CONSTRAINT) {
		t.Fatalf("%s: meta[\"code\"] = %q, want %q (err: %v)",
			what, got, aerr.APERTURE_STORAGE_CONSTRAINT, err)
	}
}

// TestTwirpDeleteRefusesAParentWithLiveChildren walks every RESTRICT edge a
// client can reach through this surface and asserts the refusal arrives coded.
func TestTwirpDeleteRefusesAParentWithLiveChildren(t *testing.T) {
	srv, _ := newTestServer(t)
	c := client(srv)
	ctx := asPrincipal(context.Background(), t, "root")
	actor := &rpc.Actor{Principal: "root", Account: acct}

	// A role held by a principal, a group holding a principal, and a permission
	// a role cites — the three edges the seeded fixture does not already have.
	must(t, putRole(t, ctx, c, actor, model.Role{ID: "editor", Name: "Editor", PermissionIDs: []string{"perm-admin"}}))
	must(t, putPrincipal(t, ctx, c, actor, model.Principal{
		ID: "alice", Kind: model.PrincipalUser, Identity: "user:alice", RoleIDs: []string{"editor"},
	}))
	must(t, putGroup(t, ctx, c, actor, model.Group{ID: "writers", Name: "Writers", MemberPrincipalIDs: []string{"alice"}}))

	_, err := c.DeleteRole(ctx, &rpc.DeleteRequest{Actor: actor, Id: "editor"})
	wantConstraint(t, "DeleteRole with a principal still holding it", err)

	_, err = c.DeletePermission(ctx, &rpc.DeleteRequest{Actor: actor, Id: "perm-admin"})
	wantConstraint(t, "DeletePermission with a role still citing it", err)

	_, err = c.DeletePrincipal(ctx, &rpc.DeleteRequest{Actor: actor, Id: "alice"})
	wantConstraint(t, "DeletePrincipal still in a group and an account", err)

	_, err = c.DeleteObjectType(ctx, &rpc.DeleteRequest{Actor: actor, Id: "system"})
	wantConstraint(t, "DeleteObjectType with a permission still on it", err)

	_, err = c.DeleteAccount(ctx, &rpc.DeleteRequest{Actor: actor, Id: acct})
	wantConstraint(t, "DeleteAccount with members still in it", err)
}

// TestTwirpTeardownChildrenFirstSucceeds drives a complete model teardown
// through the RPC surface in dependency order and requires every step to
// succeed. It is the positive half: the refusals above are only correct if the
// documented way out of them actually works over this surface.
//
// root's admin grant is stamped to the "*" wildcard account, so tearing down the
// concrete account does not revoke root's own authority mid-teardown. An admin
// whose grant named a real account would lose the ability to continue here, and
// the failure would be AUTHZ_DENIED rather than a constraint — a different bug
// with a different fix.
func TestTwirpTeardownChildrenFirstSucceeds(t *testing.T) {
	srv, _ := newTestServer(t)
	c := client(srv)
	ctx := asPrincipal(context.Background(), t, "root")
	actor := &rpc.Actor{Principal: "root", Account: acct}

	must(t, putRole(t, ctx, c, actor, model.Role{ID: "editor", Name: "Editor", PermissionIDs: []string{"perm-admin"}}))
	must(t, putPrincipal(t, ctx, c, actor, model.Principal{
		ID: "alice", Kind: model.PrincipalUser, Identity: "user:alice", RoleIDs: []string{"editor"},
	}))
	must(t, putGroup(t, ctx, c, actor, model.Group{ID: "writers", Name: "Writers", MemberPrincipalIDs: []string{"alice"}}))
	must(t, putGrant(t, ctx, c, actor, model.Grant{
		ID: "g-writers", AccountID: acct,
		Subject:      model.Subject{Kind: model.SubjectGroup, ID: "writers"},
		PermissionID: "perm-admin", Object: "**", Effect: model.EffectAllow,
	}))

	steps := []struct {
		what string
		call func() error
	}{
		// Grants first: they name a permission, an account, and a subject.
		{"DeleteGrant g-writers", func() error {
			_, err := c.DeleteGrant(ctx, &rpc.DeleteRequest{Actor: actor, Id: "g-writers"})
			return err
		}},
		// The group's membership of alice comes off with the group itself
		// (apt_group_members cascades from apt_groups).
		{"DeleteGroup writers", func() error {
			_, err := c.DeleteGroup(ctx, &rpc.DeleteRequest{Actor: actor, Id: "writers"})
			return err
		}},
		// alice's role assignment is an entity FIELD, so it comes off with a Put,
		// not a delete — the only "child removal" on this surface that is not a
		// Delete* RPC at all.
		{"PutPrincipal alice without roles", func() error {
			return putPrincipal(t, ctx, c, actor, model.Principal{
				ID: "alice", Kind: model.PrincipalUser, Identity: "user:alice",
			})
		}},
		{"DeleteMembership alice@acme", func() error {
			_, err := c.DeleteMembership(ctx, &rpc.MembershipKeyRequest{
				Actor: actor, PrincipalId: "alice", AccountId: acct,
			})
			return err
		}},
		{"DeletePrincipal alice", func() error {
			_, err := c.DeletePrincipal(ctx, &rpc.DeleteRequest{Actor: actor, Id: "alice"})
			return err
		}},
		// The role's permission bundle is a field too, but apt_role_permissions
		// cascades from apt_roles, so deleting the role clears it.
		{"DeleteRole editor", func() error {
			_, err := c.DeleteRole(ctx, &rpc.DeleteRequest{Actor: actor, Id: "editor"})
			return err
		}},
		// root's own admin grant is stamped to "*", so it is not a child of acme;
		// only root's membership is.
		{"DeleteMembership root@acme", func() error {
			_, err := c.DeleteMembership(ctx, &rpc.MembershipKeyRequest{
				Actor: actor, PrincipalId: "root", AccountId: acct,
			})
			return err
		}},
		{"DeleteAccount acme", func() error {
			_, err := c.DeleteAccount(ctx, &rpc.DeleteRequest{Actor: actor, Id: acct})
			return err
		}},
	}
	for _, s := range steps {
		if err := s.call(); err != nil {
			t.Fatalf("%s: %v\n\nteardown order is wrong, or a child edge has no reachable removal on this surface", s.what, err)
		}
	}
}

func putRole(t *testing.T, ctx context.Context, c rpc.ApertureService, actor *rpc.Actor, r model.Role) error {
	t.Helper()
	_, err := c.PutRole(ctx, &rpc.EntityRequest{Actor: actor, EntityJson: mustJSON(t, r)})
	return err
}

func putPrincipal(t *testing.T, ctx context.Context, c rpc.ApertureService, actor *rpc.Actor, p model.Principal) error {
	t.Helper()
	_, err := c.PutPrincipal(ctx, &rpc.EntityRequest{Actor: actor, EntityJson: mustJSON(t, p)})
	return err
}

func putGroup(t *testing.T, ctx context.Context, c rpc.ApertureService, actor *rpc.Actor, g model.Group) error {
	t.Helper()
	_, err := c.PutGroup(ctx, &rpc.EntityRequest{Actor: actor, EntityJson: mustJSON(t, g)})
	return err
}

func putGrant(t *testing.T, ctx context.Context, c rpc.ApertureService, actor *rpc.Actor, g model.Grant) error {
	t.Helper()
	_, err := c.PutGrant(ctx, &rpc.EntityRequest{Actor: actor, EntityJson: mustJSON(t, g)})
	return err
}
