package cli

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/frankbardon/aperture/model"
)

// TestSeedingAnEnforcingStoreSucceeds is the regression guard for the ONE break
// that the rest of the suite structurally cannot catch.
//
// Aperture's SQLite schema carries real foreign keys, so seed.Document.Apply must
// write each entity after everything it references — roles before the principals
// that hold them, permissions before the roles that bundle them, principals
// before the groups and memberships that name them. Get that order wrong and
// seeding fails outright.
//
// Nothing else catches it. Every other test that exercises Apply runs against
// storage/memory, which enforces nothing and therefore accepts any order at all;
// the sqlite package's own tests never call the seed loader. The two halves are
// only brought together HERE, in the wiring that a real `aperture --store
// file:...` invocation actually walks: openStore picks the SQLite backend, then
// loadSeed applies the embedded example through it.
//
// This test is deliberately end-to-end and deliberately assertion-light. It does
// not check what was seeded — seed/ owns that. It checks that seeding an
// enforcing backend WORKS, which is the property that silently disappears.
func TestSeedingAnEnforcingStoreSucceeds(t *testing.T) {
	ctx := context.Background()
	dsn := "file:" + filepath.Join(t.TempDir(), "aperture.db")

	store, err := buildStore(ctx, dsn, "") // "" = the embedded example document
	if err != nil {
		t.Fatalf("seeding a SQLite store failed: %v\n\n"+
			"This is almost certainly the ORDER of the loops in seed.Document.Apply: "+
			"an entity was written before something it references, and the database refused it. "+
			"See the dependency order documented above Apply, and the \"Referential integrity\" "+
			"note in storage/sqlite/schema.sql.", err)
	}
	t.Cleanup(func() {
		if c, ok := store.(interface{ Close() error }); ok {
			_ = c.Close()
		}
	})

	// Anti-vacuity: a seed that wrote nothing would also "succeed".
	grants, err := store.ListGrants(ctx, "acme")
	if err != nil {
		t.Fatalf("list grants: %v", err)
	}
	if len(grants) == 0 {
		t.Fatal("the store seeded without error but holds no grants for acme; " +
			"this test would pass against a loader that did nothing")
	}

	// Re-seeding the SAME database exercises the upsert path, where every entity
	// row already exists. That path used to be INSERT OR REPLACE, which deletes
	// the row before re-inserting it and so fires the children's ON DELETE
	// actions — under these foreign keys, re-seeding a principal who is in a
	// group would be refused outright.
	if err := loadSeed(ctx, store, ""); err != nil {
		t.Fatalf("re-seeding an already-seeded store failed: %v\n\n"+
			"An entity upsert is deleting its row instead of updating it in place "+
			"(INSERT OR REPLACE rather than ON CONFLICT DO UPDATE).", err)
	}
	var _ model.Storage = store
}

// TestReseedingAChangedDocumentSucceeds is the E3-S2 half of the same problem.
//
// TestSeedingAnEnforcingStoreSucceeds re-applies the IDENTICAL document, which
// only exercises the upsert of unchanged rows. The case that actually breaks
// under RESTRICT is a re-seed that REPLACES an entity's children: a role's
// permission bundle, a principal's roles, and a group's members are entity
// FIELDS, and every backend implements "put" for them by clearing the join table
// and rewriting it. That clear is a delete, it is invisible from the seed call
// site, and it is fired against tables that sit on the child side of three
// enforced edges.
//
// So this seeds a full model, then re-seeds a document that empties all three
// bundles and renames the entities, and requires it to succeed AND to be
// observable. Against storage/memory it would pass no matter what; the point is
// the enforcing backend.
func TestReseedingAChangedDocumentSucceeds(t *testing.T) {
	ctx := context.Background()
	dsn := "file:" + filepath.Join(t.TempDir(), "reseed.db")

	store, err := buildStore(ctx, dsn, writeSeed(t, "before.yaml", delSeed))
	if err != nil {
		t.Fatalf("initial seed: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// The same ids, with every child bundle emptied and every label changed.
	const after = `
accounts:
  - {id: acme, name: Acme Renamed, description: Same account, new label.}
principals:
  - {id: root, kind: user, identity: "user:root", display_name: Root}
  - {id: alice, kind: user, identity: "user:alice", display_name: Alice Renamed, roles: []}
memberships:
  - {principal: root, account: acme}
  - {principal: alice, account: acme}
object_types:
  - {name: system, description: Same type, new label., actions: [aperture.admin]}
permissions:
  - {id: perm-admin, object_type: system, action: aperture.admin, description: Same permission, new label.}
roles:
  - {id: editor, name: Editor Renamed, description: No permissions any more., permissions: []}
groups:
  - {id: writers, name: Writers Renamed, description: No members any more., members: []}
`
	if err := loadSeed(ctx, store, writeSeed(t, "after.yaml", after)); err != nil {
		t.Fatalf("re-seeding a CHANGED document failed: %v\n\n"+
			"An entity's child bundle (role permissions, principal roles, group members) is "+
			"rewritten by clearing its join table first. That clear is a delete against the "+
			"child side of an enforced edge — see the \"Referential integrity\" note in "+
			"storage/sqlite/schema.sql.", err)
	}

	// Anti-vacuity: a re-seed that silently did nothing would also "succeed".
	role, err := store.GetRole(ctx, "editor")
	if err != nil {
		t.Fatalf("get role: %v", err)
	}
	if role.Name != "Editor Renamed" || len(role.PermissionIDs) != 0 {
		t.Fatalf("role after re-seed = %+v, want the renamed role with no permissions", role)
	}
	grp, err := store.GetGroup(ctx, "writers")
	if err != nil {
		t.Fatalf("get group: %v", err)
	}
	if len(grp.MemberPrincipalIDs) != 0 {
		t.Fatalf("group members after re-seed = %v, want none", grp.MemberPrincipalIDs)
	}
	p, err := store.GetPrincipal(ctx, "alice")
	if err != nil {
		t.Fatalf("get principal: %v", err)
	}
	if len(p.RoleIDs) != 0 {
		t.Fatalf("principal roles after re-seed = %v, want none", p.RoleIDs)
	}
}

// TestImportingIntoAnEnforcingStoreSucceeds closes the second half of the same
// blind spot, on the other caller of seed.Document.Apply.
//
// service.Import funnels a whole Document through Apply inside store.Atomic, and
// it is covered on storage/memory only — a backend that enforces nothing and so
// cannot tell a correct write order from a wrong one. SQLite's foreign keys are
// IMMEDIATE, not deferred, so being inside a transaction buys Apply nothing: a
// row written before the row it references is refused at the statement, not at
// COMMIT. This drives `aperture import` end to end over a SQLite store with a
// document that introduces a brand-new dependency chain, so every edge Apply
// orders for is actually exercised.
func TestImportingIntoAnEnforcingStoreSucceeds(t *testing.T) {
	ctx := context.Background()
	dsn := "file:" + filepath.Join(t.TempDir(), "import.db")

	// A whole new chain: object type -> permission -> role -> principal ->
	// membership + group -> grant. Nothing here exists in delSeed, so each write
	// depends on one earlier in Apply's loop order rather than on a seeded row.
	const state = `
accounts:
  - {id: beta, name: Beta, description: A second tenant.}
object_types:
  - {name: doc, description: A document., actions: [read]}
permissions:
  - {id: perm-read, object_type: doc, action: read, description: Read a document.}
roles:
  - {id: reader, name: Reader, description: Reads documents., permissions: [perm-read]}
principals:
  - {id: bob, kind: user, identity: "user:bob", display_name: Bob, roles: [reader]}
memberships:
  - {principal: bob, account: beta}
groups:
  - {id: readers, name: Readers, description: Holds bob., members: [bob]}
grants:
  - id: g-bob
    account: beta
    subject: {kind: group, id: readers}
    permission: perm-read
    object: "doc:*"
    effect: allow
`
	statePath := writeSeed(t, "state.yaml", state)
	out, err := runArgv(t, "import",
		"--seed", writeSeed(t, "boot.yaml", delSeed), "--store", dsn,
		"--principal", "root", "--account", "acme", "--file", statePath)
	if err != nil {
		t.Fatalf("importing a state file into a SQLite store failed: %v\n\noutput: %s\n\n"+
			"This is almost certainly the ORDER of the loops in seed.Document.Apply. "+
			"SQLite's foreign keys are immediate, so the surrounding transaction does not "+
			"defer the check to COMMIT.", err, out)
	}

	// Anti-vacuity: read the far end of the chain back out.
	store, err := buildStore(ctx, dsn, writeSeed(t, "empty.yaml", emptySeed))
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	grants, err := store.ListGrants(ctx, "beta")
	if err != nil {
		t.Fatalf("list grants: %v", err)
	}
	if len(grants) != 1 || grants[0].ID != "g-bob" {
		t.Fatalf("grants for beta = %+v, want exactly g-bob", grants)
	}
}
