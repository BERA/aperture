package delegation

import (
	"context"
	stderrors "errors"
	"testing"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/model"
)

// E3-S1: the delegation package's delete call paths, swept against RESTRICT.
//
// THE ENUMERATION. There is exactly one, and one wrap on the way to it:
//
//	delegation.Revoke -> store.GetGrant, authorize, store.DeleteGrant
//	    child edges: NONE. A grant is authority; no table references one, so no
//	    foreign key and no Go-level check can refuse its deletion. Revoke is a
//	    CHILD delete — it is one of the steps a caller performs BEFORE deleting
//	    the permission, subject, or account the grant cites.
//	    the one wrap: authorize wraps a failed IsMember. It is shared with Bestow
//	    and runs BEFORE the delete, and it now passes an already-coded error
//	    through rather than re-stamping it (see the comment there).
//
// Bestow is the inverse mutation and writes a grant; it is not a delete path,
// but it is the write side of the same three references, so the tests below
// bestow first and then prove the delete order those references now impose.

// TestRevokeIsUnaffectedByReferentialIntegrity — a grant has no children, so
// revoking one must succeed with the permission, subject, and account it cites
// all still present. This is the "confirm rather than assume" half.
func TestRevokeIsUnaffectedByReferentialIntegrity(t *testing.T) {
	f := newFixture(t)
	f.giveAliceAuthority()
	ctx := context.Background()

	g := readGrant("g-bob-atlas", acctAcme, "account:acme/project:atlas/doc:1")
	if err := f.svc.Bestow(ctx, "alice", g); err != nil {
		t.Fatalf("bestow: %v", err)
	}
	if err := f.svc.Revoke(ctx, "alice", "g-bob-atlas"); err != nil {
		t.Fatalf("Revoke has no child edges and must succeed: %v", err)
	}
	if _, err := f.store.GetGrant(ctx, "g-bob-atlas"); aerr.CodeOf(err) != aerr.APERTURE_NOT_FOUND {
		t.Fatalf("revoked grant should be gone, got %v", err)
	}
}

// TestRevokeUnblocksDeletingTheSubjectItNamed is the ordering proof at this
// layer. A bestowed grant names its subject, and that reference is now enforced
// in every backend, so the subject cannot be deleted underneath it. Revoke is
// the child-first step that makes the parent delete legal — and until it runs,
// the parent delete is refused with APERTURE_STORAGE_CONSTRAINT, not with a bare
// storage error.
func TestRevokeUnblocksDeletingTheSubjectItNamed(t *testing.T) {
	f := newFixture(t)
	f.giveAliceAuthority()
	ctx := context.Background()

	// dave is a bare principal: no membership, no roles, no group. The ONLY thing
	// that will reference him is the bestowed grant, so the refusal below can only
	// be attributed to that grant.
	if err := f.store.PutPrincipal(ctx, model.Principal{
		ID: "dave", Kind: model.PrincipalUser, Identity: "user:dave",
	}); err != nil {
		t.Fatalf("seed dave: %v", err)
	}
	if err := f.store.DeletePrincipal(ctx, "dave"); err != nil {
		t.Fatalf("an unreferenced principal must be deletable: %v", err)
	}
	if err := f.store.PutPrincipal(ctx, model.Principal{
		ID: "dave", Kind: model.PrincipalUser, Identity: "user:dave",
	}); err != nil {
		t.Fatalf("re-seed dave: %v", err)
	}

	g := model.Grant{
		ID: "g-dave", AccountID: acctAcme,
		Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: "dave"},
		PermissionID: permRead, Object: "account:acme/project:atlas/doc:9",
		Effect: model.EffectAllow,
	}
	if err := f.svc.Bestow(ctx, "alice", g); err != nil {
		t.Fatalf("bestow: %v", err)
	}

	// Parent before child: refused, with the code that says which move to make.
	err := f.store.DeletePrincipal(ctx, "dave")
	if got := aerr.CodeOf(err); got != aerr.APERTURE_STORAGE_CONSTRAINT {
		t.Fatalf("deleting a bestowed grant's subject: code = %q, want %q", got, aerr.APERTURE_STORAGE_CONSTRAINT)
	}

	// Child first: revoke, then the same delete succeeds.
	if err := f.svc.Revoke(ctx, "alice", "g-dave"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if err := f.store.DeletePrincipal(ctx, "dave"); err != nil {
		t.Fatalf("after revoke the subject must be deletable: %v", err)
	}
}

// codedMemberFailure is a store whose IsMember fails with an APERTURE_* code
// already attached. Everything else delegates to the embedded real store.
type codedMemberFailure struct {
	model.Storage
	err error
}

func (c codedMemberFailure) IsMember(context.Context, string, string) (bool, error) {
	return false, c.err
}

// TestRevokePassesACodedStorageErrorThroughVerbatim pins the one wrap on the
// delegation delete path. authorize used to call aerr.Wrap unconditionally, and
// aerr.Wrap does NOT pass an existing code through — it stamps the code it is
// given — so a coded storage failure reached the caller as APERTURE_STORAGE with
// its real code buried one level down, where CodeOf never looks. The chain here
// must carry exactly one coded error: the store's own.
func TestRevokePassesACodedStorageErrorThroughVerbatim(t *testing.T) {
	f := newFixture(t)
	f.giveAliceAuthority()
	ctx := context.Background()

	g := readGrant("g-bob-atlas", acctAcme, "account:acme/project:atlas/doc:1")
	if err := f.svc.Bestow(ctx, "alice", g); err != nil {
		t.Fatalf("bestow: %v", err)
	}

	coded := aerr.New(aerr.APERTURE_STORAGE_CONSTRAINT, "storage: refused")
	svc := New(codedMemberFailure{Storage: f.store, err: coded}, f.eng)

	err := svc.Revoke(ctx, "alice", "g-bob-atlas")
	if got := aerr.CodeOf(err); got != aerr.APERTURE_STORAGE_CONSTRAINT {
		t.Fatalf("code = %q, want %q — a coded storage error must not be re-stamped", got, aerr.APERTURE_STORAGE_CONSTRAINT)
	}
	var count int
	for e := err; e != nil; e = stderrors.Unwrap(e) {
		var ce *aerr.CodedError
		if stderrors.As(e, &ce) && ce == e {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("error chain carries %d coded errors, want exactly 1: %v", count, err)
	}

	// An UNCODED failure still gets a code — the pass-through must not become a
	// hole through which a bare error escapes the package boundary.
	bare := New(codedMemberFailure{Storage: f.store, err: stderrors.New("dial tcp: refused")}, f.eng)
	if got := aerr.CodeOf(bare.Revoke(ctx, "alice", "g-bob-atlas")); got != aerr.APERTURE_STORAGE {
		t.Fatalf("uncoded failure: code = %q, want %q", got, aerr.APERTURE_STORAGE)
	}
}
