package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"
)

// The residue guard. A gated suite that has to be run by hand is only rerunnable
// if it leaves the operator's database exactly as it found it — and "exactly" is
// not something a per-test t.Cleanup can promise on its own, because a cleanup
// that is skipped, mis-scoped, or added to only fourteen of fifteen tests looks
// identical to one that works until the second run.
//
// So the check is made ONCE, around the whole run, by TestMain: snapshot the
// database before any test executes, run them, snapshot again, and report the
// difference. Three kinds of debris are looked for, each of which has actually
// happened to somebody:
//
//   - SCHEMAS. Every live test works in a scratch schema of its own and drops it
//     on the way out. One that does not accumulates a schema per run, forever, in
//     whatever database the operator pointed at — usually a shared development
//     one.
//   - ADVISORY LOCKS. Setup's lock is transaction-scoped (pg_advisory_xact_lock),
//     so it is released by COMMIT or ROLLBACK and cannot outlive the transaction.
//     A session-scoped one (pg_advisory_lock) would look identical in every test
//     and would then block the NEXT process to call Setup, indefinitely.
//   - BACKENDS. Every Store owns a pool; a test that opens one without closing it
//     holds real server memory until the process exits. Inside one `go test` run
//     that is invisible — the process does exit — which is exactly why it has to
//     be measured here rather than assumed.
//
// Ungated, this does nothing at all: `make test` runs with no database and must
// stay that way.

// TestMain wraps the package's tests with the residue check above.
func TestMain(m *testing.M) {
	d := decideGate(os.Getenv(pgGateEnv), os.Getenv(pgDSNEnv))
	if d.DSN == "" {
		// Ungated (or misconfigured): let the tests run and let the gate itself
		// report the problem, per test, in the ordinary way.
		os.Exit(m.Run())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	before, err := snapshotDatabase(ctx, d.DSN)
	cancel()
	if err != nil {
		fmt.Fprintf(os.Stderr, "residue guard: cannot read the database before the run: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()

	ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// database/sql closes a pool's connections promptly but not synchronously, so
	// the backend count is polled rather than sampled once: a single reading taken
	// the instant m.Run returns reports a leak that is really a few milliseconds
	// of shutdown. The schema and lock readings do not need this — DDL and lock
	// release are committed, not deferred — but they ride along for free.
	var after dbSnapshot
	deadline := time.Now().Add(5 * time.Second)
	for {
		after, err = snapshotDatabase(ctx, d.DSN)
		if err != nil {
			fmt.Fprintf(os.Stderr, "residue guard: cannot read the database after the run: %v\n", err)
			os.Exit(1)
		}
		if after.backends <= before.backends || time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	if problems := diffSnapshots(before, after); len(problems) > 0 {
		fmt.Fprintf(os.Stderr, "\nFAIL\tthe gated run left residue in the database:\n  %s\n\n"+
			"A live suite must be rerunnable against the same database indefinitely. Everything it\n"+
			"creates is scratch, and everything scratch is dropped on the way out — otherwise the\n"+
			"second run is not measuring the same starting state as the first.\n",
			strings.Join(problems, "\n  "))
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

// dbSnapshot is what the guard compares.
type dbSnapshot struct {
	schemas  map[string]bool
	advisory int
	backends int
}

func snapshotDatabase(ctx context.Context, dsn string) (dbSnapshot, error) {
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return dbSnapshot{}, err
	}
	defer func() { _ = db.Close() }()
	if err := db.PingContext(ctx); err != nil {
		return dbSnapshot{}, err
	}

	snap := dbSnapshot{schemas: map[string]bool{}}
	rows, err := db.QueryContext(ctx, `
SELECT nspname FROM pg_catalog.pg_namespace
 WHERE nspname NOT LIKE 'pg\_%' AND nspname <> 'information_schema'`)
	if err != nil {
		return dbSnapshot{}, err
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			_ = rows.Close()
			return dbSnapshot{}, err
		}
		snap.schemas[name] = true
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return dbSnapshot{}, err
	}
	_ = rows.Close()

	// Advisory locks held by any session other than this one. Aperture only ever
	// takes transaction-scoped ones, so a survivor is a defect, not a race.
	if err := db.QueryRowContext(ctx, `
SELECT count(*) FROM pg_catalog.pg_locks
 WHERE locktype = 'advisory' AND pid <> pg_backend_pid()`).Scan(&snap.advisory); err != nil {
		return dbSnapshot{}, err
	}

	// Backends on this database other than the guard's own. Counting the DELTA
	// rather than trying to recognise "our" connections is deliberate: pgx sets no
	// application_name by default, so any rule that named them would also name an
	// unrelated client and would break the moment one connected.
	if err := db.QueryRowContext(ctx, `
SELECT count(*) FROM pg_catalog.pg_stat_activity
 WHERE datname = current_database() AND pid <> pg_backend_pid()`).Scan(&snap.backends); err != nil {
		return dbSnapshot{}, err
	}
	return snap, nil
}

// diffSnapshots reports what the run added and did not take away. Connections are
// polled rather than sampled once: database/sql closes a pool's connections
// promptly but not synchronously, so a single reading after m.Run would report a
// leak that is merely a few milliseconds of shutdown.
func diffSnapshots(before, after dbSnapshot) []string {
	var problems []string

	var added []string
	for name := range after.schemas {
		if !before.schemas[name] {
			added = append(added, name)
		}
	}
	if len(added) > 0 {
		sort.Strings(added)
		problems = append(problems, fmt.Sprintf(
			"%d schema(s) survived the run: %s — a scratch schema must be dropped by the test that made it",
			len(added), strings.Join(added, ", ")))
	}
	if after.advisory > before.advisory {
		problems = append(problems, fmt.Sprintf(
			"%d advisory lock(s) are still held (%d before the run, %d after) — Setup's lock is "+
				"transaction-scoped and cannot outlive its transaction, so something took a "+
				"session-scoped one and the next Setup anywhere will block on it",
			after.advisory-before.advisory, before.advisory, after.advisory))
	}
	if after.backends > before.backends {
		problems = append(problems, fmt.Sprintf(
			"%d connection(s) are still open that were not before the run (%d before, %d after) — a "+
				"Store was opened without a matching Close, and every one of them is server memory the "+
				"operator did not agree to",
			after.backends-before.backends, before.backends, after.backends))
	}
	return problems
}
