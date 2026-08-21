package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/frankbardon/aperture/auth"
	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/internal/server"
	"github.com/frankbardon/aperture/internal/wire/rpc"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/service"

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
//
// E3-S3 adds the two assertions that make the refusal ACTIONABLE rather than
// merely attributable: the transport class is FailedPrecondition (412), so an
// alert rule keyed on 5xx does not fire and a retrying client does not retry;
// and the code the client reads back actually keys a Registry entry carrying
// fixups, since the code surviving is only worth anything if the guidance behind
// it exists.
func wantConstraint(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected a refusal, got success", what)
	}
	if got := twirpCode(err); got != string(aerr.APERTURE_STORAGE_CONSTRAINT) {
		t.Fatalf("%s: meta[\"code\"] = %q, want %q (err: %v)",
			what, got, aerr.APERTURE_STORAGE_CONSTRAINT, err)
	}
	te, ok := err.(twirp.Error)
	if !ok {
		t.Fatalf("%s: not a twirp error: %v", what, err)
	}
	if te.Code() != twirp.FailedPrecondition {
		t.Fatalf("%s: twirp code = %q, want %q — a constraint refusal is the caller's "+
			"ordering mistake, not a broken server, and %d pages an on-call for it",
			what, te.Code(), twirp.FailedPrecondition,
			twirp.ServerHTTPStatusFromErrorCode(te.Code()))
	}
	entry, ok := aerr.Registry[aerr.APERTURE_STORAGE_CONSTRAINT]
	if !ok || len(entry.Fixups) == 0 {
		t.Fatalf("%s: APERTURE_STORAGE_CONSTRAINT must carry Registry fixup guidance — "+
			"the code is the client's index into it", what)
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
//
// E3-S3 extends the teardown to a SECOND object type and permission ("doc" /
// "p-read"). The reserved "system" type and "perm-admin" deliberately survive —
// root's own wildcard admin grant cites the permission, and root needs that grant
// to authorize every step below — so without a second pair, DeleteObjectType and
// DeletePermission had no end-to-end SUCCESS on this surface at all, only the
// refusals above. The pair is bundled into role "editor" so the role still has to
// go first: the ordering is proven, not sidestepped.
func TestTwirpTeardownChildrenFirstSucceeds(t *testing.T) {
	srv, _ := newTestServer(t)
	c := client(srv)
	ctx := asPrincipal(context.Background(), t, "root")
	actor := &rpc.Actor{Principal: "root", Account: acct}

	must(t, putObjectType(t, ctx, c, actor, model.ObjectType{Name: "doc", Actions: []string{"read"}}))
	must(t, putPermission(t, ctx, c, actor, model.Permission{ID: "p-read", ObjectType: "doc", Action: "read"}))
	must(t, putRole(t, ctx, c, actor, model.Role{
		ID: "editor", Name: "Editor", PermissionIDs: []string{"perm-admin", "p-read"},
	}))
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
		// Only now is p-read unreferenced: the role bundled it until the line
		// above, and no grant ever cited it.
		{"DeletePermission p-read", func() error {
			_, err := c.DeletePermission(ctx, &rpc.DeleteRequest{Actor: actor, Id: "p-read"})
			return err
		}},
		// And only now is the type it hung off unreferenced.
		{"DeleteObjectType doc", func() error {
			_, err := c.DeleteObjectType(ctx, &rpc.DeleteRequest{Actor: actor, Id: "doc"})
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

func putObjectType(t *testing.T, ctx context.Context, c rpc.ApertureService, actor *rpc.Actor, ot model.ObjectType) error {
	t.Helper()
	_, err := c.PutObjectType(ctx, &rpc.EntityRequest{Actor: actor, EntityJson: mustJSON(t, ot)})
	return err
}

func putPermission(t *testing.T, ctx context.Context, c rpc.ApertureService, actor *rpc.Actor, p model.Permission) error {
	t.Helper()
	_, err := c.PutPermission(ctx, &rpc.EntityRequest{Actor: actor, EntityJson: mustJSON(t, p)})
	return err
}

// E3-S3: the status a blocked delete gets on the wire, and what the server says
// about it in its own logs.
//
// codeToTwirp used to have no case for APERTURE_STORAGE_CONSTRAINT, so it fell
// through to twirp.Internal and a blocked delete came back 500. That is wrong in
// both directions a status is read. A client library treats 5xx as the retryable
// class, and this is the one refusal a retry of the identical request can NEVER
// clear. And the Twirp error hook logs at WARN with the twirp code attached, so
// every alert rule keyed on internal errors fired — paging an on-call because an
// admin deleted a role somebody still held.
//
// Both assertions below are here rather than folded into wantConstraint because
// neither is visible through the generated client's error value: the HTTP status
// needs a raw request, and the log line needs a server built with a logger the
// test can read back.

// TestBlockedDeleteIsA412OnTheWire drives one blocked delete as a RAW HTTP
// request — no generated client in the way — and reads the status line. This is
// the byte a load balancer, a retry policy, and an SLO dashboard all key on, and
// it is the one thing a twirp.Error value assertion cannot show.
func TestBlockedDeleteIsA412OnTheWire(t *testing.T) {
	srv, _ := newTestServer(t)
	c := client(srv)
	ctx := asPrincipal(context.Background(), t, "root")
	actor := &rpc.Actor{Principal: "root", Account: acct}

	must(t, putRole(t, ctx, c, actor, model.Role{ID: "editor", Name: "Editor", PermissionIDs: []string{"perm-admin"}}))
	must(t, putPrincipal(t, ctx, c, actor, model.Principal{
		ID: "alice", Kind: model.PrincipalUser, Identity: "user:alice", RoleIDs: []string{"editor"},
	}))

	body := `{"actor":{"principal":"root","account":"` + acct + `"},"id":"editor"}`
	req, err := http.NewRequest(http.MethodPost,
		srv.URL+"/twirp/aperture.ApertureService/DeleteRole", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer root")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var payload struct {
		Code string            `json:"code"`
		Msg  string            `json:"msg"`
		Meta map[string]string `json:"meta"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode twirp error body: %v", err)
	}
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("HTTP status = %d, want %d — a delete refused for referential integrity "+
			"is the caller's ordering mistake and must not land in the retryable 5xx class "+
			"(twirp code %q, msg %q)",
			resp.StatusCode, http.StatusPreconditionFailed, payload.Code, payload.Msg)
	}
	if payload.Code != string(twirp.FailedPrecondition) {
		t.Fatalf("twirp code in body = %q, want %q", payload.Code, twirp.FailedPrecondition)
	}
	// The status is the coarse transport class; meta["code"] is the only channel
	// carrying the code that keys the fixups, and it must survive the remap.
	if got := payload.Meta["code"]; got != string(aerr.APERTURE_STORAGE_CONSTRAINT) {
		t.Fatalf("meta[\"code\"] = %q, want %q", got, aerr.APERTURE_STORAGE_CONSTRAINT)
	}
}

// TestBlockedDeleteIsNotLoggedAsAnInternalError reads the server's OWN log line
// for a blocked delete. loggingHooks writes the twirp code as a structured
// field, and that field is what an alert rule matches on, so "internal" appearing
// there is the paging incident this story exists to stop — regardless of what the
// client was told.
func TestBlockedDeleteIsNotLoggedAsAnInternalError(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	svc, _ := newTestService(t, service.ManagedEntities{})
	srv := httptest.NewServer(server.Authenticate(auth.NewDev(), server.New(svc, logger)))
	t.Cleanup(srv.Close)

	c := client(srv)
	ctx := asPrincipal(context.Background(), t, "root")
	actor := &rpc.Actor{Principal: "root", Account: acct}

	must(t, putRole(t, ctx, c, actor, model.Role{ID: "editor", Name: "Editor", PermissionIDs: []string{"perm-admin"}}))
	must(t, putPrincipal(t, ctx, c, actor, model.Principal{
		ID: "alice", Kind: model.PrincipalUser, Identity: "user:alice", RoleIDs: []string{"editor"},
	}))
	logs.Reset()

	if _, err := c.DeleteRole(ctx, &rpc.DeleteRequest{Actor: actor, Id: "editor"}); err == nil {
		t.Fatal("DeleteRole with a principal still holding it must be refused")
	}

	var found bool
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not JSON: %q", line)
		}
		// Matched on the "code" attribute rather than on msg: loggingHooks passes
		// an attribute also named "msg" (the error text), and slog's JSON handler
		// emits BOTH keys, so decoding into a map keeps the last one. "code" is
		// the field an alert rule matches anyway, and only the error hook sets it.
		code, ok := rec["code"].(string)
		if !ok {
			continue
		}
		found = true
		if code == string(twirp.Internal) {
			t.Fatalf("the refused delete was logged as an internal error: %q\n"+
				"every alert rule keyed on internal errors now pages for an admin's "+
				"ordering mistake", line)
		}
		if code != string(twirp.FailedPrecondition) {
			t.Fatalf("logged twirp code = %q, want %q (line %q)", code, twirp.FailedPrecondition, line)
		}
	}
	if !found {
		t.Fatalf("the refusal produced no \"rpc error\" log line at all; captured:\n%s", logs.String())
	}
}
