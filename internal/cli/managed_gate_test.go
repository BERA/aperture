package cli

import (
	"bytes"
	"context"
	"encoding/json"
	stderrors "errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/seed"
	"github.com/frankbardon/aperture/service"
)

// E1-S4, the CLI half. `aperture put account` reaches the same facade the wire
// does, over the same store, so a CLI that did not observe the deployment's
// entity-management posture would be a local bypass of a lock the Twirp surface
// honours — the switch would hold for anyone remote and not for anyone with a
// shell on the box. These tests drive the real command tree end to end
// (NewApp().Run with argv), so the wiring, the flag plumbing, and the error the
// operator actually reads are all in scope.

// gateSeed is the smallest model that can tell POSTURE from PERMISSION: "root"
// holds a platform-wide system-admin grant (authorization denies it nothing) and
// "alice" holds none at all (authorization denies it everything). A refusal that
// lands identically on both is a refusal authorization never got to make.
const gateSeed = `
accounts:
  - {id: acme, name: Acme Corp, description: The tenant every grant is stamped to.}
  # solo has no members and nothing stamped to it, on purpose: it is the account
  # the lifecycle test deletes. account_id is an enforced reference in every
  # backend -- an apt_accounts row or exactly the "*" wildcard -- so deleting
  # acme, which root and alice both belong to, would be refused for a reason that
  # has nothing to do with the posture switch this file is about.
  - {id: solo, name: Solo, description: The tenant nothing references.}
principals:
  - {id: root, kind: user, identity: "user:root", display_name: Root}
  - {id: alice, kind: user, identity: "user:alice", display_name: Alice}
  # dana belongs to no account on purpose: she is the principal the lifecycle
  # test deletes. root and alice are both members of acme, and
  # apt_memberships.principal_id is an enforced reference in every backend, so
  # deleting either would be refused for a reason that has nothing to do with the
  # posture switch this file is about.
  - {id: dana, kind: user, identity: "user:dana", display_name: Dana}
memberships:
  - {principal: root, account: acme}
  - {principal: alice, account: acme}
object_types:
  - name: system
    description: The reserved type admin authority is modelled on.
    actions: [aperture.admin]
permissions:
  - id: perm-admin
    object_type: system
    action: aperture.admin
    description: Administer the deployment.
grants:
  - id: g-root-admin
    account: "*"
    subject: {kind: principal, id: root}
    permission: perm-admin
    object: "**"
    effect: allow
`

func writeGateSeed(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gate.yaml")
	if err := os.WriteFile(path, []byte(gateSeed), 0o600); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	return path
}

func gateJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// runCLI runs one invocation through the real command tree and returns the
// output plus the error the binary's main would print. args[0] is the command;
// --seed is spliced in directly after it, ahead of the caller's own flags and
// positional arguments, which is the order urfave/cli parses.
func runCLI(t *testing.T, seedPath string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	app := NewApp("test")
	app.Writer = &out
	app.ErrWriter = &out
	app.Reader = strings.NewReader("")
	argv := append([]string{"aperture", args[0], "--seed", seedPath}, args[1:]...)
	err := app.Run(context.Background(), argv)
	return out.String(), err
}

// cliCall names one gated CLI invocation exactly as an operator would type it.
type cliCall struct {
	name string
	args []string
}

// gatedCLICalls are every `put`/`delete` invocation the three switches govern,
// grouped by the switch that governs it.
func gatedCLICalls(t *testing.T, who string) map[string][]cliCall {
	t.Helper()
	actor := []string{"--principal", who, "--account", "acme"}
	call := func(cmd string, rest ...string) []string {
		return append(append([]string{cmd}, actor...), rest...)
	}
	return map[string][]cliCall{
		"account": {
			{"put account", call("put", "--json", gateJSON(t, model.Account{ID: "beta", Name: "Beta"}), "account")},
			{"delete account", call("delete", "account", "solo")},
		},
		"principal": {
			{"put principal", call("put", "--json",
				gateJSON(t, model.Principal{ID: "carol", Kind: model.PrincipalUser, Identity: "user:carol"}), "principal")},
			// dana, not alice: every CLI invocation here builds a fresh store from
			// the seed, so the target must be a seeded principal — and alice is a
			// member of acme, which apt_memberships.principal_id refuses to orphan.
			// dana is seeded into no account precisely so this delete tests the
			// posture switch and nothing else.
			{"delete principal", call("delete", "principal", "dana")},
		},
		"membership": {
			{"put membership", call("put", "--json",
				gateJSON(t, model.Membership{PrincipalID: "alice", AccountID: "acme"}), "membership")},
			{"delete membership", call("delete", "--principal-id", "alice", "--account-id", "acme", "membership")},
		},
	}
}

// lockKind sets ONLY the switch governing kind to false, leaving the other two
// unset — which is also what makes the independence assertion below meaningful.
func lockKind(t *testing.T, kind string) {
	t.Helper()
	t.Setenv(envFor[kind], "false")
}

// envFor maps a kind to the variable a refusal must name, so the operator is
// pointed at the switch that actually caused it rather than at the family.
var envFor = map[string]string{
	"account":    service.EnvManageAccounts,
	"principal":  service.EnvManagePrincipals,
	"membership": service.EnvManageMemberships,
}

// wantCLIUnmanaged asserts one CLI error is the deployment-posture refusal AND
// that it is actionable: the code, the entity kind in the message, the exact
// environment variable to set, the restart requirement, and the registry Fixups
// `aperture` publishes for the code.
func wantCLIUnmanaged(t *testing.T, what, kind, out string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected a refusal, got success (output %q)", what, out)
	}
	if got := aerr.CodeOf(err); got != aerr.APERTURE_ENTITY_UNMANAGED {
		t.Fatalf("%s: code = %q, want %q (err: %v)", what, got, aerr.APERTURE_ENTITY_UNMANAGED, err)
	}
	// err.Error() is verbatim what `aperture` prints to stderr.
	if !strings.Contains(err.Error(), kind) {
		t.Fatalf("%s: message must name the entity kind %q, got %q", what, kind, err.Error())
	}
	var ce *aerr.CodedError
	if !stderrors.As(err, &ce) {
		t.Fatalf("%s: error is not a *errors.CodedError: %v", what, err)
	}
	if ce.Context["env"] != envFor[kind] {
		t.Fatalf("%s: context env = %v, want %q", what, ce.Context["env"], envFor[kind])
	}
	fix, _ := ce.Context["fix"].(string)
	if !strings.Contains(fix, envFor[kind]) || !strings.Contains(fix, "restart") {
		t.Fatalf("%s: fix hint must name %s and the restart, got %q", what, envFor[kind], fix)
	}
	// The Fixups an operator reaches through the error-code reference must be
	// just as specific — a registry entry that said only "check your config"
	// would send them nowhere.
	fixups := strings.Join(aerr.Registry[aerr.APERTURE_ENTITY_UNMANAGED].Fixups, "\n")
	for _, want := range []string{
		service.EnvManageAccounts, service.EnvManagePrincipals, service.EnvManageMemberships, "RESTART",
	} {
		if !strings.Contains(fixups, want) {
			t.Fatalf("%s: registry fixups omit %q:\n%s", what, want, fixups)
		}
	}
}

// TestCLIRefusesEveryLockedLifecycleCommand walks `put` and `delete` for all
// three kinds with that kind's switch off. Each must fail with the same coded,
// actionable refusal — the CLI builds its OWN facade, so nothing about the
// gate's presence on the wire implies its presence here.
func TestCLIRefusesEveryLockedLifecycleCommand(t *testing.T) {
	for kind := range envFor {
		for _, c := range gatedCLICalls(t, "root")[kind] {
			t.Run(c.name, func(t *testing.T) {
				lockKind(t, kind)
				out, err := runCLI(t, writeGateSeed(t), c.args...)
				wantCLIUnmanaged(t, c.name, kind, out, err)
			})
		}
	}
}

// TestCLIImportRefusesALockedSection covers the one CLI command that reaches
// storage through a whole document rather than a mutator. `aperture import`
// shares buildService with put/delete, so it inherits the gate — but "inherits"
// is a claim about wiring, and the wiring is what this story checks.
func TestCLIImportRefusesALockedSection(t *testing.T) {
	for kind := range envFor {
		t.Run(kind, func(t *testing.T) {
			lockKind(t, kind)

			var doc seed.Document
			switch kind {
			case "account":
				doc.Accounts = []seed.Account{{ID: "beta", Name: "Beta"}}
			case "principal":
				doc.Principals = []seed.Principal{{ID: "carol", Kind: "user", Identity: "user:carol"}}
			default:
				doc.Memberships = []seed.Membership{{Principal: "alice", Account: "acme"}}
			}
			path := filepath.Join(t.TempDir(), "state.json")
			if err := os.WriteFile(path, []byte(gateJSON(t, doc)), 0o600); err != nil {
				t.Fatalf("write state file: %v", err)
			}

			out, err := runCLI(t, writeGateSeed(t), "import",
				"--principal", "root", "--account", "acme", "--file", path)
			wantCLIUnmanaged(t, "import ("+kind+")", kind, out, err)
		})
	}
}

// TestCLIUnmanagedIsNotADenial is the distinguishability assertion at the CLI,
// in both directions.
//
// Locked, root (whom authorization denies nothing) and alice (whom it denies
// everything) must hear the SAME thing — unmanaged — because the facade refuses
// before it authorizes. Unlocked, the same two must be told APART: root
// succeeds, alice gets APERTURE_AUTHZ_DENIED. Only the pair proves anything; the
// first half alone would also pass on a facade that had stopped authorizing.
func TestCLIUnmanagedIsNotADenial(t *testing.T) {
	body := gateJSON(t, model.Principal{ID: "carol", Kind: model.PrincipalUser, Identity: "user:carol"})
	put := func(t *testing.T, who string) error {
		t.Helper()
		_, err := runCLI(t, writeGateSeed(t), "put",
			"--principal", who, "--account", "acme", "--json", body, "principal")
		return err
	}

	t.Run("locked", func(t *testing.T) {
		t.Setenv(service.EnvManagePrincipals, "false")
		for _, who := range []string{"root", "alice"} {
			err := put(t, who)
			if got := aerr.CodeOf(err); got != aerr.APERTURE_ENTITY_UNMANAGED {
				t.Fatalf("%s: code = %q, want %q — posture must outrank permission",
					who, got, aerr.APERTURE_ENTITY_UNMANAGED)
			}
		}
	})

	t.Run("default", func(t *testing.T) {
		if err := put(t, "root"); err != nil {
			t.Fatalf("the system-admin write must succeed under the default posture: %v", err)
		}
		if got := aerr.CodeOf(put(t, "alice")); got != aerr.APERTURE_AUTHZ_DENIED {
			t.Fatalf("non-admin: code = %q, want %q — the two refusals must stay distinguishable",
				got, aerr.APERTURE_AUTHZ_DENIED)
		}
	})
}

// TestCLIMixedPostureIsIndependent locks accounts ONLY and drives all three
// kinds through the CLI. Principals and memberships must still be writable: if
// the three switches ever collapsed into one, a deployment that meant to hand
// its account records to an upstream system would silently lose the entity
// lifecycle it still owns.
func TestCLIMixedPostureIsIndependent(t *testing.T) {
	t.Setenv(service.EnvManageAccounts, "false")
	seedPath := writeGateSeed(t)
	actor := []string{"--principal", "root", "--account", "acme"}
	put := func(body, kind string) (string, error) {
		return runCLI(t, seedPath, append(append([]string{"put"}, actor...), "--json", body, kind)...)
	}

	out, err := put(gateJSON(t, model.Account{ID: "beta", Name: "Beta"}), "account")
	wantCLIUnmanaged(t, "put account", "account", out, err)

	if _, err := put(gateJSON(t, model.Principal{ID: "carol", Kind: model.PrincipalUser, Identity: "user:carol"}), "principal"); err != nil {
		t.Fatalf("put principal must still work with only accounts locked: %v", err)
	}
	if _, err := put(gateJSON(t, model.Membership{PrincipalID: "alice", AccountID: "acme"}), "membership"); err != nil {
		t.Fatalf("put membership must still work with only accounts locked: %v", err)
	}
	// The ungated kinds are governed by nothing here — the switch is about three
	// entity kinds, not about writing.
	if _, err := put(gateJSON(t, model.Role{ID: "reader", Name: "Reader"}), "role"); err != nil {
		t.Fatalf("put role must be unaffected by the account switch: %v", err)
	}
}

// TestCLIDefaultPostureRunsTheWholeLifecycle is the regression check at the CLI:
// with no APERTURE_MANAGE_* set at all — the state every existing deployment and
// every other test in this repo runs in — every gated command still works.
func TestCLIDefaultPostureRunsTheWholeLifecycle(t *testing.T) {
	for _, v := range []string{service.EnvManageAccounts, service.EnvManagePrincipals, service.EnvManageMemberships} {
		t.Setenv(v, "")
	}
	seedPath := writeGateSeed(t)
	for kind, calls := range gatedCLICalls(t, "root") {
		for _, c := range calls {
			if out, err := runCLI(t, seedPath, c.args...); err != nil {
				t.Fatalf("default posture: %s (%s) failed: %v (output %q)", c.name, kind, err, out)
			}
		}
	}
}

// TestSeedLoaderIsUngatedByDecision pins the documented exemption. The boot
// loader behind `aperture serve --seed` writes accounts, principals, and
// memberships STRAIGHT to storage through Document.Apply, bypassing the facade
// entirely, and that is deliberate: the operator seeding a process is the same
// operator configuring it, and a deployment must be able to lay down the
// entities it then declares unmanaged. Pinning it keeps the exemption a decision
// — someone who "fixes" it breaks a test that says why.
func TestSeedLoaderIsUngatedByDecision(t *testing.T) {
	for _, v := range []string{service.EnvManageAccounts, service.EnvManagePrincipals, service.EnvManageMemberships} {
		t.Setenv(v, "false")
	}
	ctx := context.Background()

	// buildStore is exactly what runServe calls for --seed.
	store, err := buildStore(ctx, "", writeGateSeed(t))
	if err != nil {
		t.Fatalf("seeding with every switch off must still work: %v", err)
	}
	defer func() { _ = store.Close() }()

	if _, err := store.GetAccount(ctx, "acme"); err != nil {
		t.Fatalf("seeded account did not land with accounts unmanaged: %v", err)
	}
	if _, err := store.GetPrincipal(ctx, "alice"); err != nil {
		t.Fatalf("seeded principal did not land with principals unmanaged: %v", err)
	}
	ms, err := store.MembershipsForAccount(ctx, "acme")
	if err != nil {
		t.Fatalf("memberships: %v", err)
	}
	if len(ms) != 2 {
		t.Fatalf("seeded memberships = %d, want 2 — the loader must stay ungated", len(ms))
	}

	// And the same process, asked the same question through the facade, still
	// refuses: the exemption is about the LOADER, not about the store.
	svc, mstore, err := buildService(ctx, "", writeGateSeed(t))
	if err != nil {
		t.Fatalf("buildService: %v", err)
	}
	defer func() { _ = mstore.Close() }()
	err = svc.PutAccount(ctx, service.Actor{Principal: "root", Account: "acme"}, model.Account{ID: "beta"})
	if got := aerr.CodeOf(err); got != aerr.APERTURE_ENTITY_UNMANAGED {
		t.Fatalf("facade PutAccount = %q, want %q", got, aerr.APERTURE_ENTITY_UNMANAGED)
	}
}

// TestCLIMalformedSwitchFailsTheCommand — a CLI mutation reads the switches
// itself, so a typo'd value has to fail the command with the coded config error
// rather than silently resolving to the default. An operator who typed "flase"
// and expected accounts locked must be told.
func TestCLIMalformedSwitchFailsTheCommand(t *testing.T) {
	t.Setenv(service.EnvManageAccounts, "flase")
	_, err := runCLI(t, writeGateSeed(t), "put",
		"--principal", "root", "--account", "acme",
		"--json", gateJSON(t, model.Account{ID: "beta", Name: "Beta"}), "account")
	if got := aerr.CodeOf(err); got != aerr.APERTURE_CONFIG_INVALID {
		t.Fatalf("code = %q, want %q", got, aerr.APERTURE_CONFIG_INVALID)
	}
}
