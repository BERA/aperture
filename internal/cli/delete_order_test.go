package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/model"
)

// E3-S2, the CLI half. Aperture's storage now carries real foreign keys, so a
// delete with live children is REFUSED where it previously succeeded by
// orphaning rows. `aperture delete` is one flag-parse plus one facade call, so
// the ordering that has to hold is the OPERATOR's — and the only thing the CLI
// owes them is a refusal they can act on.
//
// That is not free. Every command in this package returns the facade's error
// verbatim, and cmd/aperture/main.go prints it with %v; aerr.Wrap does NOT pass
// an existing code through, so a single well-meaning wrap anywhere on the way
// out would replace APERTURE_STORAGE_CONSTRAINT — and with it the six registry
// fixups that say exactly which children to remove — with whatever code the
// wrapper named. These tests assert the SPECIFIC code, on both backends, and
// assert it is in the text an operator actually reads.
//
// Both backends run every case on purpose. A refusal produced by real SQLite
// foreign keys and one produced by storage/memory's hand-written mirror have to
// be indistinguishable here, because `--store` is the only difference between
// them and an operator does not expect the error contract to move with it.

// delSeed is the smallest model that populates every RESTRICT edge `aperture
// delete` can reach: a role a principal holds, a permission a role bundles and a
// grant cites, an object type a permission hangs off, and an account with
// members and a grant stamped to it.
//
// root's admin grant is stamped to the "*" wildcard account, not to acme. That
// is load-bearing for the teardown below: deleting an account means deleting the
// grants stamped to it first, and an admin whose own grant named acme would
// revoke its own authority partway through and fail with APERTURE_AUTHZ_DENIED —
// a real hazard, but a different one from the constraint this file is about.
const delSeed = `
accounts:
  - {id: acme, name: Acme Corp, description: The tenant everything hangs off.}
principals:
  - {id: root, kind: user, identity: "user:root", display_name: Root}
  - {id: alice, kind: user, identity: "user:alice", display_name: Alice, roles: [editor]}
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
roles:
  - {id: editor, name: Editor, description: Bundles the admin permission., permissions: [perm-admin]}
groups:
  - {id: writers, name: Writers, description: Holds alice., members: [alice]}
grants:
  - id: g-root-admin
    account: "*"
    subject: {kind: principal, id: root}
    permission: perm-admin
    object: "**"
    effect: allow
  - id: g-writers
    account: acme
    subject: {kind: group, id: writers}
    permission: perm-admin
    object: "**"
    effect: allow
`

// emptySeed applies nothing. The CLI re-seeds on EVERY invocation (buildStore
// runs loadSeed unconditionally), so a multi-step teardown against a persistent
// store must stop re-applying the document or each step's deletions are undone
// by the next step's seed. This is the file the steps after the first point at.
const emptySeed = "{}\n"

func writeSeed(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write seed %s: %v", name, err)
	}
	return path
}

// runArgv runs one invocation through the real command tree with the caller's
// exact argv (minus argv[0]), returning the combined output and the error
// cmd/aperture/main.go would print. Unlike runCLI it splices nothing, so a
// subcommand like `template delete` and an explicit --store are both expressible.
func runArgv(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	app := NewApp("test")
	app.Writer = &out
	app.ErrWriter = &out
	app.Reader = strings.NewReader("")
	err := app.Run(context.Background(), append([]string{"aperture"}, args...))
	return out.String(), err
}

// backends are the two `--store` values a `make test` run can reach: the default
// in-memory backend, and SQLite at a temp file. The DSN is built per test so
// each case gets its own database.
func backends(t *testing.T) map[string]string {
	t.Helper()
	return map[string]string{
		"memory": "",
		"sqlite": "file:" + filepath.Join(t.TempDir(), "aperture.db"),
	}
}

// wantCLIConstraint asserts one CLI error is the referential-integrity refusal
// with its code intact — not merely that the command failed. It also asserts the
// code is in err.Error(), which is verbatim what `aperture` writes to stderr:
// the code is the operator's index into the registry fixups, so a message that
// dropped it would leave them with a refusal and no next step.
func wantCLIConstraint(t *testing.T, what, out string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected a refusal, got success (output %q)", what, out)
	}
	if got := aerr.CodeOf(err); got != aerr.APERTURE_STORAGE_CONSTRAINT {
		t.Fatalf("%s: code = %q, want %q (err: %v)", what, got, aerr.APERTURE_STORAGE_CONSTRAINT, err)
	}
	if !strings.Contains(err.Error(), string(aerr.APERTURE_STORAGE_CONSTRAINT)) {
		t.Fatalf("%s: the printed error must name the code, got %q", what, err.Error())
	}
}

// TestCLIDeleteRefusesAParentWithLiveChildren walks every RESTRICT edge
// `aperture delete` can reach, on both backends. Each invocation re-seeds from
// delSeed, so the cases are independent and order-free.
func TestCLIDeleteRefusesAParentWithLiveChildren(t *testing.T) {
	seedPath := writeSeed(t, "del.yaml", delSeed)
	cases := []struct {
		what string
		args []string
	}{
		{"delete role still held by a principal", []string{"delete", "role", "editor"}},
		{"delete permission still bundled by a role", []string{"delete", "permission", "perm-admin"}},
		{"delete principal still in an account and a group", []string{"delete", "principal", "alice"}},
		{"delete object type still carrying a permission", []string{"delete", "object-type", "system"}},
		{"delete account still holding members", []string{"delete", "account", "acme"}},
		// The polymorphic edge: subject_kind chooses which table subject_id must
		// exist in, so it is enforced in Go rather than by a foreign key. It must
		// still refuse, and with the same code.
		{"delete group still named as a grant subject", []string{"delete", "group", "writers"}},
	}
	for name, dsn := range backends(t) {
		t.Run(name, func(t *testing.T) {
			for _, tc := range cases {
				argv := append([]string{tc.args[0], "--seed", seedPath, "--store", dsn,
					"--principal", "root", "--account", "acme"}, tc.args[1:]...)
				out, err := runArgv(t, argv...)
				wantCLIConstraint(t, tc.what, out, err)
			}
		})
	}
}

// TestCLITeardownChildrenFirstSucceeds drives a whole-model teardown through the
// real command tree, in dependency order, and requires every step to succeed.
// The refusals above are only correct if the documented way out of them is
// actually reachable from a shell — including the one step that is not a delete
// at all, because a principal's roles are a FIELD and come off with a `put`.
//
// It runs on SQLite only: the teardown needs state to survive between
// invocations, and the in-memory backend is discarded when each command's store
// is closed.
func TestCLITeardownChildrenFirstSucceeds(t *testing.T) {
	full := writeSeed(t, "del.yaml", delSeed)
	empty := writeSeed(t, "empty.yaml", emptySeed)
	dsn := "file:" + filepath.Join(t.TempDir(), "teardown.db")

	alice, err := json.Marshal(model.Principal{
		ID: "alice", Kind: model.PrincipalUser, Identity: "user:alice", DisplayName: "Alice",
	})
	if err != nil {
		t.Fatalf("marshal principal: %v", err)
	}

	actor := []string{"--principal", "root", "--account", "acme"}
	step := func(seedPath string, args ...string) []string {
		return append(append([]string{args[0], "--seed", seedPath, "--store", dsn}, actor...), args[1:]...)
	}

	// perm-admin and the system object type deliberately survive this teardown:
	// root's own wildcard admin grant cites the permission, and root needs that
	// grant to authorize the steps below it. Deleting a permission is covered by
	// the refusal test above and by service/'s own sweep.
	steps := []struct {
		what string
		argv []string
	}{
		// The first invocation is the one that seeds the database; every later one
		// points at the empty document so it stops putting back what was removed.
		// Grants come off first: a grant names a permission, an account, and its
		// subject, so it is a child of three of the things below it.
		{"delete grant g-writers", step(full, "delete", "grant", "g-writers")},
		// The group's hold on alice cascades away with the group itself.
		{"delete group writers", step(empty, "delete", "group", "writers")},
		// alice's role assignment is an entity FIELD: there is no `delete` for it,
		// and re-putting her without roles is the only way to clear it.
		{"put alice without roles", step(empty, "put", "--json", string(alice), "principal")},
		{"delete membership alice@acme", step(empty, "delete", "--principal-id", "alice", "--account-id", "acme", "membership")},
		{"delete principal alice", step(empty, "delete", "principal", "alice")},
		// A role's permission bundle cascades from the role, so the role goes now.
		{"delete role editor", step(empty, "delete", "role", "editor")},
		{"delete membership root@acme", step(empty, "delete", "--principal-id", "root", "--account-id", "acme", "membership")},
		{"delete account acme", step(empty, "delete", "account", "acme")},
	}

	for _, s := range steps {
		out, err := runArgv(t, s.argv...)
		if err != nil {
			t.Fatalf("%s: %v\n\nteardown order is wrong, or a child edge has no removal reachable from the CLI.\noutput: %s", s.what, err, out)
		}
	}
}

// TestCLIBulkRevokeAndTemplateDeleteReachTheOperator covers the two delete paths
// that do not live on `aperture delete`: `aperture bulk revoke` and `aperture
// template delete`. Neither can hit a foreign key — no row references a grant or
// a template — so what is asserted is the other half of the contract: they
// succeed, and a coded failure from the facade still arrives coded.
func TestCLIBulkRevokeAndTemplateDeleteReachTheOperator(t *testing.T) {
	seedPath := writeSeed(t, "del.yaml", delSeed)
	for name, dsn := range backends(t) {
		t.Run(name, func(t *testing.T) {
			sub := func(group, cmd string, rest ...string) []string {
				return append([]string{group, cmd, "--seed", seedPath, "--store", dsn,
					"--principal", "root", "--account", "acme"}, rest...)
			}

			// bulk revoke of a live grant: no row references a grant, so it works.
			out, err := runArgv(t, sub("bulk", "revoke", "g-writers")...)
			if err != nil {
				t.Fatalf("bulk revoke: %v (output %q)", err, out)
			}

			// A grant id that does not exist is a coded failure, and it must reach
			// the operator as one rather than as a bare string.
			out, err = runArgv(t, sub("bulk", "revoke", "no-such-grant")...)
			if err == nil {
				t.Fatalf("bulk revoke of a missing grant: expected a refusal (output %q)", out)
			}
			if got := aerr.CodeOf(err); got == "" {
				t.Fatalf("bulk revoke of a missing grant: error carries no Aperture code: %v", err)
			}

			// template delete of a name with no versions: also coded.
			out, err = runArgv(t, sub("template", "delete", "no-such-template")...)
			if err == nil {
				t.Fatalf("template delete of a missing template: expected a refusal (output %q)", out)
			}
			if got := aerr.CodeOf(err); got == "" {
				t.Fatalf("template delete of a missing template: error carries no Aperture code: %v", err)
			}
		})
	}
}
