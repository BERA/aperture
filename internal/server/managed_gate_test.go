package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/internal/wire/rpc"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/seed"
	"github.com/frankbardon/aperture/service"

	"github.com/twitchtv/twirp"
)

// E1-S4, the Twirp half. The entity-management gate lives in the facade, one
// layer below every surface, and the claim that buys is "Twirp needs no
// surface-specific code". That claim is worth exactly as much as the test that
// checks it: a handler could decode into the wrong struct, swallow the error, or
// route a delete around the facade, and nothing above would notice. These tests
// drive the real HTTP stack — Twirp JSON client, auth middleware, handler,
// facade — and assert the SAME code, at the SAME status, for all seven gated
// entry points.

// wireCall names one gated RPC and how a client issues it, so each posture below
// is asserted against every entry point instead of a representative one.
type wireCall struct {
	name string
	call func(context.Context, rpc.ApertureService, *rpc.Actor) error
}

// gatedWireCalls groups the RPCs by the switch that governs them. Import appears
// under all three: one document can carry all three sections, so it is refused
// by whichever switch is off.
func gatedWireCalls(t *testing.T) map[string][]wireCall {
	t.Helper()
	return map[string][]wireCall{
		"account": {
			{"PutAccount", func(ctx context.Context, c rpc.ApertureService, a *rpc.Actor) error {
				_, err := c.PutAccount(ctx, &rpc.EntityRequest{
					Actor: a, EntityJson: mustJSON(t, model.Account{ID: "beta", Name: "Beta"})})
				return err
			}},
			{"DeleteAccount", func(ctx context.Context, c rpc.ApertureService, a *rpc.Actor) error {
				_, err := c.DeleteAccount(ctx, &rpc.DeleteRequest{Actor: a, Id: acct})
				return err
			}},
		},
		"principal": {
			{"PutPrincipal", func(ctx context.Context, c rpc.ApertureService, a *rpc.Actor) error {
				_, err := c.PutPrincipal(ctx, &rpc.EntityRequest{
					Actor: a, EntityJson: mustJSON(t, model.Principal{ID: "carol", Kind: model.PrincipalUser, Identity: "user:carol"})})
				return err
			}},
			{"DeletePrincipal", func(ctx context.Context, c rpc.ApertureService, a *rpc.Actor) error {
				_, err := c.DeletePrincipal(ctx, &rpc.DeleteRequest{Actor: a, Id: "alice"})
				return err
			}},
		},
		"membership": {
			{"PutMembership", func(ctx context.Context, c rpc.ApertureService, a *rpc.Actor) error {
				_, err := c.PutMembership(ctx, &rpc.EntityRequest{
					Actor: a, EntityJson: mustJSON(t, model.Membership{PrincipalID: "alice", AccountID: acct})})
				return err
			}},
			{"DeleteMembership", func(ctx context.Context, c rpc.ApertureService, a *rpc.Actor) error {
				_, err := c.DeleteMembership(ctx, &rpc.MembershipKeyRequest{
					Actor: a, PrincipalId: "alice", AccountId: acct})
				return err
			}},
		},
	}
}

// importOf builds an ImportRequest carrying exactly one gated section, so the
// refusal can be attributed to the switch under test rather than to whichever
// section Apply happens to reach first.
func importOf(t *testing.T, kind string, a *rpc.Actor) *rpc.ImportRequest {
	t.Helper()
	var doc seed.Document
	switch kind {
	case "account":
		doc.Accounts = []seed.Account{{ID: "beta", Name: "Beta"}}
	case "principal":
		doc.Principals = []seed.Principal{{ID: "carol", Kind: "user", Identity: "user:carol"}}
	default:
		doc.Memberships = []seed.Membership{{Principal: "alice", Account: acct}}
	}
	js, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal document: %v", err)
	}
	return &rpc.ImportRequest{Actor: a, DocumentJson: string(js)}
}

func lockedFor(kind string) service.ManagedEntities {
	switch kind {
	case "account":
		return service.ManagedEntities{Accounts: service.ManagedNo}
	case "principal":
		return service.ManagedEntities{Principals: service.ManagedNo}
	default:
		return service.ManagedEntities{Memberships: service.ManagedNo}
	}
}

// wantUnmanaged asserts one wire error is the deployment-posture refusal: a
// twirp FailedPrecondition (412) carrying APERTURE_ENTITY_UNMANAGED in meta,
// which is the field crud.js reads to render the banner.
func wantUnmanaged(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected a refusal, got nil", what)
	}
	te, ok := err.(twirp.Error)
	if !ok {
		t.Fatalf("%s: error is not a twirp.Error: %v", what, err)
	}
	if te.Code() != twirp.FailedPrecondition {
		t.Fatalf("%s: twirp code = %q, want %q", what, te.Code(), twirp.FailedPrecondition)
	}
	if got := te.Meta("code"); got != string(aerr.APERTURE_ENTITY_UNMANAGED) {
		t.Fatalf("%s: meta code = %q, want %q", what, got, aerr.APERTURE_ENTITY_UNMANAGED)
	}
}

// TestTwirpRefusesEveryLockedLifecycleRPC walks all six lifecycle RPCs plus
// Import with their kind locked. Every one must come back as the same coded
// refusal — a surface that gated five of seven would be a hole with a test suite
// that looked green.
func TestTwirpRefusesEveryLockedLifecycleRPC(t *testing.T) {
	for kind, calls := range gatedWireCalls(t) {
		actor := &rpc.Actor{Principal: "root", Account: acct}
		for _, wc := range calls {
			t.Run(wc.name, func(t *testing.T) {
				srv, _ := newTestServerManaged(t, lockedFor(kind))
				ctx := asPrincipal(context.Background(), t, "root")
				wantUnmanaged(t, wc.name, wc.call(ctx, client(srv), actor))
			})
		}
		t.Run("Import_"+kind, func(t *testing.T) {
			srv, _ := newTestServerManaged(t, lockedFor(kind))
			ctx := asPrincipal(context.Background(), t, "root")
			_, err := client(srv).Import(ctx, importOf(t, kind, actor))
			wantUnmanaged(t, "Import ("+kind+")", err)
		})
	}
}

// TestTwirpUnmanagedIsA412 pins the HTTP status a non-Go client actually sees.
// The twirp.Error code is the Go client's view; an operator reading a proxy log,
// and any alerting rule built on status classes, sees only the number — and 412
// rather than 500 is the difference between "configured this way" and a page.
func TestTwirpUnmanagedIsA412(t *testing.T) {
	srv, _ := newTestServerManaged(t, service.ManagedEntities{Accounts: service.ManagedNo})

	req, err := http.NewRequest(http.MethodPost,
		srv.URL+rpc.ApertureServicePathPrefix+"PutAccount",
		strings.NewReader(`{"actor":{"principal":"root","account":"`+acct+`"},"entity_json":"{\"id\":\"beta\"}"}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer root")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("HTTP status = %d, want %d", resp.StatusCode, http.StatusPreconditionFailed)
	}
	var body struct {
		Code string            `json:"code"`
		Meta map[string]string `json:"meta"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode twirp error body: %v", err)
	}
	if body.Code != string(twirp.FailedPrecondition) {
		t.Fatalf("error body code = %q, want %q", body.Code, twirp.FailedPrecondition)
	}
	if body.Meta["code"] != string(aerr.APERTURE_ENTITY_UNMANAGED) {
		t.Fatalf("error body meta code = %q, want %q", body.Meta["code"], aerr.APERTURE_ENTITY_UNMANAGED)
	}
}

// TestTwirpUnmanagedIsNotADenial is the distinguishability assertion at the
// wire, and it needs both halves to mean anything.
//
// Under a LOCKED posture, root (a platform system-admin, whom authorization
// denies nothing) and alice (an authenticated non-admin, whom authorization
// denies everything) must hear the SAME answer: unmanaged, 412. That is only
// possible because the facade refuses before it authorizes — if the gate ran
// second, alice would get a 403 and an operator debugging her report would go
// looking in the grant table for something a startup flag decided.
//
// Under the DEFAULT posture the same two callers must be told apart: root
// succeeds and alice gets 403 / APERTURE_AUTHZ_DENIED. Without this half, a
// facade that had simply stopped authorizing anyone would pass the first half.
func TestTwirpUnmanagedIsNotADenial(t *testing.T) {
	put := func(srv *httptest.Server, who string) error {
		ctx := asPrincipal(context.Background(), t, who)
		_, err := client(srv).PutPrincipal(ctx, &rpc.EntityRequest{
			Actor:      &rpc.Actor{Principal: who, Account: acct},
			EntityJson: mustJSON(t, model.Principal{ID: "carol", Kind: model.PrincipalUser, Identity: "user:carol"}),
		})
		return err
	}

	locked, _ := newTestServerManaged(t, service.ManagedEntities{Principals: service.ManagedNo})
	wantUnmanaged(t, "system-admin against a locked kind", put(locked, "root"))
	wantUnmanaged(t, "non-admin against a locked kind", put(locked, "alice"))

	open, _ := newTestServer(t)
	if err := put(open, "root"); err != nil {
		t.Fatalf("default posture: the system-admin write must succeed: %v", err)
	}
	err := put(open, "alice")
	te, ok := err.(twirp.Error)
	if !ok || te.Code() != twirp.PermissionDenied {
		t.Fatalf("default posture: non-admin write = %v, want twirp PermissionDenied", err)
	}
	if got := te.Meta("code"); got != string(aerr.APERTURE_AUTHZ_DENIED) {
		t.Fatalf("default posture: non-admin meta code = %q, want %q", got, aerr.APERTURE_AUTHZ_DENIED)
	}
}

// TestTwirpMixedPostureIsIndependentOverTheWire locks accounts ONLY and drives
// all three kinds over the wire. Principals and memberships must still be
// writable: three switches that turn out to be one switch would lock a
// deployment out of the entities it still masters, and the facade-level
// independence test cannot see a surface that collapsed them on the way down.
func TestTwirpMixedPostureIsIndependentOverTheWire(t *testing.T) {
	srv, _ := newTestServerManaged(t, service.ManagedEntities{Accounts: service.ManagedNo})
	c := client(srv)
	ctx := asPrincipal(context.Background(), t, "root")
	actor := &rpc.Actor{Principal: "root", Account: acct}

	_, err := c.PutAccount(ctx, &rpc.EntityRequest{Actor: actor, EntityJson: mustJSON(t, model.Account{ID: "beta", Name: "Beta"})})
	wantUnmanaged(t, "PutAccount with accounts locked", err)

	if _, err := c.PutPrincipal(ctx, &rpc.EntityRequest{
		Actor: actor, EntityJson: mustJSON(t, model.Principal{ID: "carol", Kind: model.PrincipalUser, Identity: "user:carol"}),
	}); err != nil {
		t.Fatalf("PutPrincipal must still work with only accounts locked: %v", err)
	}
	if _, err := c.PutMembership(ctx, &rpc.EntityRequest{
		Actor: actor, EntityJson: mustJSON(t, model.Membership{PrincipalID: "carol", AccountID: acct}),
	}); err != nil {
		t.Fatalf("PutMembership must still work with only accounts locked: %v", err)
	}
	// And the ungated kinds are untouched — the switch governs three entity
	// kinds, not "writes".
	if _, err := c.PutRole(ctx, &rpc.EntityRequest{
		Actor: actor, EntityJson: mustJSON(t, model.Role{ID: "reader", Name: "Reader"}),
	}); err != nil {
		t.Fatalf("PutRole must be unaffected by the account switch: %v", err)
	}
}

// TestTwirpDefaultPostureRunsTheWholeLifecycle is the regression check at the
// wire: a server built exactly as it always was (no WithManagedEntities) must
// still create, read back, and delete all three kinds. The gate's zero value is
// the only thing standing between "an operator can lock a kind" and "every
// deployment in existence just lost its entity CRUD".
func TestTwirpDefaultPostureRunsTheWholeLifecycle(t *testing.T) {
	srv, _ := newTestServer(t)
	c := client(srv)
	ctx := asPrincipal(context.Background(), t, "root")
	actor := &rpc.Actor{Principal: "root", Account: acct}

	steps := []struct {
		name string
		call func() error
	}{
		{"PutAccount", func() error {
			_, err := c.PutAccount(ctx, &rpc.EntityRequest{Actor: actor, EntityJson: mustJSON(t, model.Account{ID: "beta", Name: "Beta"})})
			return err
		}},
		{"PutPrincipal", func() error {
			_, err := c.PutPrincipal(ctx, &rpc.EntityRequest{Actor: actor, EntityJson: mustJSON(t, model.Principal{ID: "carol", Kind: model.PrincipalUser, Identity: "user:carol"})})
			return err
		}},
		{"PutMembership", func() error {
			_, err := c.PutMembership(ctx, &rpc.EntityRequest{Actor: actor, EntityJson: mustJSON(t, model.Membership{PrincipalID: "carol", AccountID: "beta"})})
			return err
		}},
		{"Import", func() error {
			_, err := c.Import(ctx, importOf(t, "membership", actor))
			return err
		}},
		{"DeleteMembership", func() error {
			_, err := c.DeleteMembership(ctx, &rpc.MembershipKeyRequest{Actor: actor, PrincipalId: "carol", AccountId: "beta"})
			return err
		}},
		{"DeletePrincipal", func() error {
			_, err := c.DeletePrincipal(ctx, &rpc.DeleteRequest{Actor: actor, Id: "carol"})
			return err
		}},
		{"DeleteAccount", func() error {
			_, err := c.DeleteAccount(ctx, &rpc.DeleteRequest{Actor: actor, Id: "beta"})
			return err
		}},
	}
	for _, s := range steps {
		if err := s.call(); err != nil {
			t.Fatalf("default posture: %s failed: %v", s.name, err)
		}
	}
}
