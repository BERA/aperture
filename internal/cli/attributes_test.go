package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/frankbardon/aperture/service"
)

// attributeSeed closes the E1 slice: a seed file declares principal attributes
// INLINE, a rule reads one of those fields, and `aperture check` returns the
// verdict the rule implies — with no external data source and no Go written by
// the host.
//
// The permission's scope strategy is `inclusive;rule=engineering`, so the grant
// covers exactly the objects the rule selects. The rule reads NOTHING off the
// object: it asks only `principal.department == "eng"`, which is answerable only
// if the attributes: block reached the rules engine as a principal resolver.
//
// The four principals are the four interesting cases:
//
//	alice     user,    department eng     -> ALLOW
//	bob       user,    department sales   -> DENY  (the rule ran and said no)
//	ci-runner machine, department eng     -> ALLOW (the MACHINE slot answered)
//	mallory   user,    no attributes:entry-> DENY  (the floor bag has no department)
//
// bob and ci-runner together are what make the test two-sided: an allow-everything
// build fails on bob, and a build that never consults the attribute registry
// fails on alice. ci-runner additionally pins the kind dispatch — it is declared
// under `subject: machine`, so answering it out of the user directory (or failing
// to answer it at all) shows up as a deny.
const attributeSeed = `
accounts:
  - {id: acme, name: Acme Corp}
memberships:
  - {principal: alice, account: acme}
  - {principal: bob, account: acme}
  - {principal: ci-runner, account: acme}
  - {principal: mallory, account: acme}
object_types:
  - {name: document, description: A protected document., actions: [read]}
permissions:
  - id: perm-doc-read
    object_type: document
    action: read
    scope_strategy: "inclusive;rule=engineering"
    description: Read a document when the asker is in engineering.
principals:
  - {id: alice, kind: user, identity: "user:alice", display_name: Alice, roles: [viewer]}
  - {id: bob, kind: user, identity: "user:bob", display_name: Bob, roles: [viewer]}
  - {id: ci-runner, kind: machine, identity: "machine:ci-runner", display_name: CI, roles: [viewer]}
  - {id: mallory, kind: user, identity: "user:mallory", display_name: Mallory, roles: [viewer]}
roles:
  - {id: viewer, name: Viewer, description: May read engineering documents., permissions: [perm-doc-read]}
grants:
  - id: g-viewer-read
    account: acme
    subject: {kind: role, id: viewer}
    permission: perm-doc-read
    object: "account:acme/**"
    effect: allow
rules:
  - name: engineering
    description: The asker is in engineering.
    ast:
      type: compare
      op: eq
      left: {type: var, name: principal.department}
      right: {type: literal, value: "eng"}
attributes:
  - subject: user
    id: alice
    metadata:
      department: eng
      clearance: 3
  - subject: user
    id: bob
    metadata: {department: sales}
  - subject: machine
    id: ci-runner
    metadata: {department: eng}
`

// writeAttributeSeed materialises the fixture and returns its path.
func writeAttributeSeed(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "attributes.yaml")
	if err := os.WriteFile(path, []byte(attributeSeed), 0o600); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	return path
}

// TestInlineAttributesDecideAThroughCheck is the epic's vertical slice, driven
// through the real command tree.
func TestInlineAttributesDecideAThroughCheck(t *testing.T) {
	ctx := context.Background()
	seedPath := writeAttributeSeed(t)

	cases := []struct {
		name      string
		principal string
		wantAllow bool
	}{
		{
			// Only reachable by reading the inline attributes: block — nothing
			// else in the model distinguishes alice from bob.
			name:      "the user slot answers a human principal",
			principal: "alice",
			wantAllow: true,
		},
		{
			name:      "the rule ran and the attribute did not match",
			principal: "bob",
			wantAllow: false,
		},
		{
			// Declared under subject: machine. Answering it out of the user
			// directory, or not at all, denies.
			name:      "the machine slot answers a non-human principal",
			principal: "ci-runner",
			wantAllow: true,
		},
		{
			// No attributes: entry at all. A missing SUBJECT is not a failed
			// decision — `principal` is the floor bag, whose only keys are id and
			// kind — so the comparison is simply false and the grant does not
			// cover the object.
			name:      "an undeclared subject evaluates against the floor bag",
			principal: "mallory",
			wantAllow: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := runCheckCommand(t, ctx, seedPath, tc.principal, "read", "account:acme/document:42")
			if got != tc.wantAllow {
				t.Fatalf("check %s = allow:%v, want allow:%v", tc.principal, got, tc.wantAllow)
			}

			// The stack is shared, so `serve` must answer identically. Asserting
			// it here is what keeps the attribute wiring in buildDecisionStack
			// rather than in one command's handler, which is how the CLI and the
			// server came to disagree about rule-backed scopes in the first place.
			res, err := serverService(t, ctx, seedPath).Check(ctx, service.Query{
				Account: "acme", Principal: tc.principal, Action: "read",
				Object: "account:acme/document:42",
			})
			if err != nil {
				t.Fatalf("server Check: %v", err)
			}
			if res.Allow != got {
				t.Fatalf("the CLI and the server disagree on %s: cli allow=%v, server allow=%v",
					tc.principal, got, res.Allow)
			}
		})
	}
}

// A seed that declares NO attributes: block must decide exactly as it did before
// the section existed: `principal` is the floor bag, so a rule reading an
// attribute is deny-safe rather than a non-decision.
func TestASeedWithNoAttributesStillDecides(t *testing.T) {
	ctx := context.Background()
	seedPath := writeRuleBackedSeed(t)
	if got := runCheckCommand(t, ctx, seedPath, "alice", "read", "account:acme/document:42"); !got {
		t.Fatal("a seed with no attributes: block stopped deciding")
	}
}
