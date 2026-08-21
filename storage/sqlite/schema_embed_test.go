package sqlite

import (
	"os"
	"testing"
)

// The naming gate that used to live in this package now lives in
// internal/schemagate and audits EVERY dialect's schema file rather than this
// one. It reads those files from disk, which leaves exactly one link of the
// chain behind here: proving that the file on disk is the file this package
// //go:embeds and Setup actually executes. Without it the gate could be auditing
// a schema no Store ever runs.
//
// storage/postgres proves the same thing for its own file, at the top of
// TestExpectedTablesMatchTheSchema.

// TestEmbeddedSchemaMatchesTheFileOnDisk fails, and never skips, if schema.sql
// cannot be read here. A missing schema is not "not applicable" — it is a broken
// //go:embed away from a store that cannot create its own tables.
func TestEmbeddedSchemaMatchesTheFileOnDisk(t *testing.T) {
	// schema.sql is relative to this package directory, which is `go test`'s
	// working directory.
	onDisk, err := os.ReadFile("schema.sql")
	if err != nil {
		t.Fatalf("read storage/sqlite/schema.sql: %v\n\nThis package embeds that file and Setup executes it; it cannot be missing.\nIf it moved, move the //go:embed directive in sqlite.go and the `sqlite` row in\ninternal/schemagate's dialect registry with it.", err)
	}
	if string(onDisk) != schema {
		t.Fatalf("storage/sqlite/schema.sql on disk differs from the copy embedded at sqlite.go's //go:embed directive (%d bytes on disk, %d embedded).\nThe naming gate in internal/schemagate audits the file on DISK, so it would be\nauditing a file this Store never executes.", len(onDisk), len(schema))
	}
}
