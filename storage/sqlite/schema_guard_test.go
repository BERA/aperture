package sqlite_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/storage/sqlite"

	_ "modernc.org/sqlite"
)

// preChangeSchema is the schema as it stood BEFORE timestamps became integer
// nanoseconds. The tests below build a real database from it — old column
// types, old column names, real RFC3339Nano values — so the guard is proven
// against a genuine pre-change database and not against a synthetic marker a
// real upgrade would never contain.
const preChangeSchema = "testdata/schema_pre_nanoseconds.sql"

// TestSetupRefusesAPreChangeDatabase is the story: a database written by the
// previous release must fail at Setup, not hours later at the first read.
func TestSetupRefusesAPreChangeDatabase(t *testing.T) {
	path := buildPreChangeDatabase(t)

	s, err := sqlite.Open("file:" + path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	err = s.Setup(t.Context())
	if err == nil {
		t.Fatal("Setup accepted a pre-change database; it must refuse one")
	}
	assertRefusal(t, err)

	// The refusal must name the cause specifically enough to act on. The old
	// database's first offending column, in sorted table order, is
	// apt_accounts.created_at, declared TEXT.
	msg := err.Error()
	for _, want := range []string{"apt_accounts", "created_at", "TEXT"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal does not name %q:\n%s", want, msg)
		}
	}
}

// TestSetupRefusesRetiredColumnName covers the second signal: apt_audit_log's
// instant was INTEGER before the change too, so only its NAME (ts_nanos) marks
// it. Types alone would never catch this table.
func TestSetupRefusesRetiredColumnName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aperture.db")
	withRawDB(t, path, func(db *sql.DB) {
		// Only the audit table, in its pre-change shape. Every other table is
		// absent, so no text timestamp exists anywhere to be found instead.
		mustExec(t, db, `CREATE TABLE apt_audit_log (
			id         TEXT PRIMARY KEY,
			ts_nanos   INTEGER NOT NULL,
			event_type TEXT NOT NULL
		)`)
		mustExec(t, db, `INSERT INTO apt_audit_log (id, ts_nanos, event_type) VALUES ('e1', 1719792000000000000, 'check')`)
	})

	err := setupAt(t, path)
	if err == nil {
		t.Fatal("Setup accepted a database still carrying ts_nanos")
	}
	assertRefusal(t, err)
	for _, want := range []string{"apt_audit_log", "ts_nanos", "occurred_at"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not name %q:\n%s", want, err.Error())
		}
	}
}

// TestSetupRefusesCurrentDeclarationsHoldingTextValues is the case that makes
// the value check earn its keep. SQLite is dynamically typed: an operator who
// copies the old rows into freshly created tables gets INTEGER declarations
// everywhere and RFC3339 text in every row, because INTEGER affinity does not
// convert a string that is not a well-formed integer. A guard that trusted the
// declaration would wave through exactly the database it exists to catch.
func TestSetupRefusesCurrentDeclarationsHoldingTextValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aperture.db")

	// Create the CURRENT schema through Setup itself — no hand-written copy, so
	// this cannot drift from the real declarations.
	if err := setupAt(t, path); err != nil {
		t.Fatalf("first setup on a fresh database: %v", err)
	}

	withRawDB(t, path, func(db *sql.DB) {
		mustExec(t, db, `INSERT INTO apt_accounts (id, name, description, created_at, updated_at)
			VALUES ('acme', 'Acme', '', '2024-07-01T00:00:00.123456789Z', '2024-07-01T00:00:00.123456789Z')`)

		// Guard the guard: assert the premise. If SQLite had coerced the text
		// to an integer there would be nothing here to detect, and this test
		// would be passing for the wrong reason.
		var kind string
		if err := db.QueryRow(`SELECT typeof(created_at) FROM apt_accounts WHERE id = 'acme'`).Scan(&kind); err != nil {
			t.Fatalf("typeof: %v", err)
		}
		if kind != "text" {
			t.Fatalf("premise broken: an INTEGER-declared column stored the RFC3339 value as %q, not text", kind)
		}
	})

	err := setupAt(t, path)
	if err == nil {
		t.Fatal("Setup accepted INTEGER-declared columns holding text timestamps")
	}
	assertRefusal(t, err)
	for _, want := range []string{"apt_accounts", "created_at", "text"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not name %q:\n%s", want, err.Error())
		}
	}
}

// TestSetupAcceptsFreshAndCurrentDatabases is the other half of the contract:
// the guard may not cost correctness on the happy path. A brand-new database
// and an already-current one both proceed, including one carrying real rows.
func TestSetupAcceptsFreshAndCurrentDatabases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aperture.db")

	if err := setupAt(t, path); err != nil {
		t.Fatalf("fresh database: %v", err)
	}
	// Idempotent: Setup over the schema it just wrote, with no rows yet.
	if err := setupAt(t, path); err != nil {
		t.Fatalf("empty current database: %v", err)
	}

	withRawDB(t, path, func(db *sql.DB) {
		mustExec(t, db, `INSERT INTO apt_accounts (id, name, description, created_at, updated_at)
			VALUES ('acme', 'Acme', '', 1719792000000000000, 1719792000000000000)`)
		// 0 is the UNSET sentinel and must not read as suspicious.
		mustExec(t, db, `INSERT INTO apt_accounts (id, name, description, created_at, updated_at)
			VALUES ('zero', 'Zero', '', 0, 0)`)
		mustExec(t, db, `INSERT INTO apt_audit_log (id, occurred_at, event_type) VALUES ('e1', 1719792000000000000, 'check')`)
	})

	if err := setupAt(t, path); err != nil {
		t.Fatalf("populated current database: %v", err)
	}
}

// TestSetupIgnoresForeignTables keeps the guard inside Aperture's own namespace:
// a shared database may hold a host table with a text created_at, and that is
// none of Aperture's business.
func TestSetupIgnoresForeignTables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aperture.db")
	withRawDB(t, path, func(db *sql.DB) {
		mustExec(t, db, `CREATE TABLE host_documents (
			id         TEXT PRIMARY KEY,
			created_at TEXT NOT NULL,
			ts_nanos   TEXT NOT NULL
		)`)
		mustExec(t, db, `INSERT INTO host_documents (id, created_at, ts_nanos) VALUES ('d1', '2024-07-01T00:00:00Z', 'nope')`)
	})

	if err := setupAt(t, path); err != nil {
		t.Fatalf("Setup refused a database whose only offending table is not Aperture's: %v", err)
	}
}

// ---- helpers ----

// buildPreChangeDatabase writes a database from the archived pre-change schema
// and populates it the way the old build did: RFC3339Nano text timestamps and
// an audit row on ts_nanos.
func buildPreChangeDatabase(t *testing.T) string {
	t.Helper()

	src, err := os.ReadFile(preChangeSchema)
	if err != nil {
		// FAIL, never skip: a missing fixture means this test proves nothing.
		t.Fatalf("read %s: %v", preChangeSchema, err)
	}

	path := filepath.Join(t.TempDir(), "aperture.db")
	withRawDB(t, path, func(db *sql.DB) {
		mustExec(t, db, string(src))

		const stamp = "2024-07-01T00:00:00.123456789Z"
		mustExec(t, db, `INSERT INTO apt_accounts (id, name, description, created_at, updated_at) VALUES ('acme', 'Acme', '', ?, ?)`, stamp, stamp)
		mustExec(t, db, `INSERT INTO apt_object_types (name, apt_actions, description, created_at, updated_at) VALUES ('document', '["read"]', '', ?, ?)`, stamp, stamp)
		mustExec(t, db, `INSERT INTO apt_principals (id, kind, apt_identity, display_name, created_at, updated_at) VALUES ('p1', 'user', 'user:p1', 'P', ?, ?)`, stamp, stamp)
		mustExec(t, db, `INSERT INTO apt_audit_log (id, ts_nanos, event_type) VALUES ('e1', 1719792000000000000, 'check')`)
	})
	return path
}

// setupAt opens the database at path through the real Store and runs Setup,
// closing the handle before returning so the caller can reopen it.
func setupAt(t *testing.T, path string) error {
	t.Helper()
	s, err := sqlite.Open("file:" + path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = s.Close() }()
	return s.Setup(t.Context())
}

// withRawDB opens path with database/sql directly — deliberately bypassing the
// Store, so a fixture can be built in shapes the Store would refuse.
func withRawDB(t *testing.T, path string, fn func(*sql.DB)) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	fn(db)
}

func mustExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("exec %.60q: %v", q, err)
	}
}

// assertRefusal checks the code and the remedy. The code is what surfaces
// translate on; the remedy is what an operator acts on, and it must be in the
// message rather than only in the Registry fixups, because that message is what
// a startup log shows.
func assertRefusal(t *testing.T, err error) {
	t.Helper()
	if got := aerr.CodeOf(err); got != aerr.APERTURE_STORAGE_SCHEMA_INCOMPATIBLE {
		t.Errorf("code = %s, want %s", got, aerr.APERTURE_STORAGE_SCHEMA_INCOMPATIBLE)
	}
	msg := err.Error()
	if !strings.Contains(msg, "incompatible build") {
		t.Errorf("refusal does not name the cause:\n%s", msg)
	}
	for _, want := range []string{"no migration path", "delete the old database", "re-seed"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal does not name the remedy (%q):\n%s", want, msg)
		}
	}
}
