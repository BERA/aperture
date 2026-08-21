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
