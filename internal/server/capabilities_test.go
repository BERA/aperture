package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/frankbardon/aperture/internal/wire/rpc"
	"github.com/frankbardon/aperture/service"

	"google.golang.org/protobuf/reflect/protoreflect"
)

// E2-S1, the Twirp half. Capabilities exists so the admin shell can render a
// locked control as disabled on load instead of discovering the lock by failing
// a save, and the shell speaks Twirp over JSON — so the JSON client path, not
// just the Go method, is what these tests drive. Two properties beyond
// correctness are load-bearing and asserted here directly: the endpoint answers
// an anonymous caller (the shell has no credential when it boots), and the
// answer is three booleans and nothing else (which is the only reason answering
// anonymously is safe).

// TestCapabilities_EveryCombinationOverJSON drives all eight postures through
// the real HTTP stack — Twirp JSON client, auth middleware, handler, facade — so
// a handler that crossed two fields, or dropped one, is caught on the wire and
// not merely in the facade.
func TestCapabilities_EveryCombinationOverJSON(t *testing.T) {
	for mask := 0; mask < 8; mask++ {
		accounts := mask&1 != 0
		principals := mask&2 != 0
		memberships := mask&4 != 0

		managed := func(enabled bool) service.Managed {
			if enabled {
				return service.ManagedYes
			}
			return service.ManagedNo
		}
		posture := service.ManagedEntities{
			Accounts:    managed(accounts),
			Principals:  managed(principals),
			Memberships: managed(memberships),
		}

		t.Run(postureName(posture), func(t *testing.T) {
			srv, _ := newTestServerManaged(t, posture)
			// No bearer: the admin shell calls this before it has one.
			resp, err := client(srv).Capabilities(context.Background(), &rpc.Empty{})
			if err != nil {
				t.Fatalf("Capabilities: %v", err)
			}
			if resp.ManageAccounts != accounts {
				t.Errorf("manage_accounts = %v, want %v", resp.ManageAccounts, accounts)
			}
			if resp.ManagePrincipals != principals {
				t.Errorf("manage_principals = %v, want %v", resp.ManagePrincipals, principals)
			}
			if resp.ManageMemberships != memberships {
				t.Errorf("manage_memberships = %v, want %v", resp.ManageMemberships, memberships)
			}
		})
	}
}

// TestCapabilities_DefaultDeploymentIsFullyCapable pins the historical posture
// over the wire: a server built with the zero ManagedEntities — every deployment
// that predates the switches — reports all three true. The facade test proves
// the mapping; this proves the default survives the whole stack, including a
// protojson round trip where a false would be the zero value and could be
// silently dropped.
func TestCapabilities_DefaultDeploymentIsFullyCapable(t *testing.T) {
	srv, _ := newTestServer(t)

	resp, err := client(srv).Capabilities(context.Background(), &rpc.Empty{})
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if !resp.ManageAccounts || !resp.ManagePrincipals || !resp.ManageMemberships {
		t.Fatalf("an unconfigured deployment reports %+v, want all three true", resp)
	}
}

// TestCapabilities_IsOpen is the property the admin UI depends on: an
// unauthenticated request gets a normal answer, not a 401. It asserts the raw
// HTTP status rather than only the client error, because a Twirp client error is
// the same shape whatever the status behind it — and 401 here would leave the
// shell unable to decide what to render until after a login it may not have.
//
// Contrast TestTwirpUnauthenticated, which pins the opposite for mutations.
func TestCapabilities_IsOpen(t *testing.T) {
	srv, _ := newTestServerManaged(t, service.ManagedEntities{Accounts: service.ManagedNo})

	resp, err := http.Post(
		srv.URL+rpc.ApertureServicePathPrefix+"Capabilities",
		"application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("raw post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("anonymous Capabilities: HTTP %d, want 200 — the admin shell calls this with no credential", resp.StatusCode)
	}

	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := body["manage_accounts"]; got != false {
		t.Fatalf("manage_accounts = %v, want false", got)
	}
}

// TestCapabilities_CarriesNoModelData is the disclosure guard. The endpoint is
// deliberately unauthenticated, so what it may contain is not a style question:
// the response must be exactly three booleans. A future field carrying an
// account id, a principal id, or a count would become an anonymous leak the
// moment it landed, and this test is what makes that a build failure rather than
// a review miss.
//
// It checks both halves — the descriptor (what the proto declares) and the
// serialized JSON (what an anonymous caller actually receives) — because a field
// could be added to one view without the other noticing.
func TestCapabilities_CarriesNoModelData(t *testing.T) {
	want := []string{"manage_accounts", "manage_principals", "manage_memberships"}

	fields := (&rpc.CapabilitiesResponse{}).ProtoReflect().Descriptor().Fields()
	if fields.Len() != len(want) {
		t.Fatalf("CapabilitiesResponse declares %d fields, want exactly %d (%v) — this message is served unauthenticated", fields.Len(), len(want), want)
	}
	for i, n := range want {
		f := fields.Get(i)
		if string(f.Name()) != n {
			t.Errorf("field %d is %q, want %q", i, f.Name(), n)
		}
		if f.Kind() != protoreflect.BoolKind || f.IsList() || f.IsMap() {
			t.Errorf("field %s is %s (list=%v map=%v), want a scalar bool", f.Name(), f.Kind(), f.IsList(), f.IsMap())
		}
	}

	// And what an anonymous caller actually gets back. Twirp's JSON codec emits
	// unpopulated fields by default, so all three keys are present with explicit
	// values and the assertion is over the whole object, not a subset.
	srv, _ := newTestServer(t)
	resp, err := http.Post(
		srv.URL+rpc.ApertureServicePathPrefix+"Capabilities",
		"application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("raw post: %v", err)
	}
	defer resp.Body.Close()

	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body) != len(want) {
		t.Fatalf("response JSON has %d keys (%v), want exactly %v", len(body), body, want)
	}
	for _, n := range want {
		v, ok := body[n]
		if !ok {
			t.Errorf("response JSON is missing %q", n)
			continue
		}
		if _, isBool := v.(bool); !isBool {
			t.Errorf("response JSON %q = %#v, want a boolean", n, v)
		}
	}
}

// postureName labels a posture for a subtest.
func postureName(m service.ManagedEntities) string {
	return "accounts=" + m.Accounts.String() +
		",principals=" + m.Principals.String() +
		",memberships=" + m.Memberships.String()
}
