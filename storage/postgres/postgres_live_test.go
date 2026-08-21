package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	aerr "github.com/frankbardon/aperture/errors"
)

// The Postgres backend's live tests. They talk to a real server, so they are
// GATED and must stay gated: CI runs with no service containers, and `make test`
// has to pass with no database present. The gate is the house one, shared with
// seed/postgres_integration_test.go so one DSN runs both:
//
//	APERTURE_PG_INTEGRATION=1 \
//	APERTURE_PG_DSN='postgres://aperture@127.0.0.1:5432/aperture?sslmode=disable' \
//	go test -run TestPostgresLive ./storage/postgres/
//
// Ungated it SKIPS. Gated with an empty DSN it FAILS — asking for the live run
// and silently not getting one is the outcome a gate must never produce.
//
// These tests exist because the three central claims of this backend's Setup
// cannot be established by reading documentation. That two concurrent Setups
// both succeed is a claim about lock acquisition, catalog visibility and
// transaction isolation interacting correctly; that a role without CREATE gets a
// coded error is a claim about a SQLSTATE a fake driver would simply be told to
// produce. Only a server can settle either.
const (
	pgGateEnv = "APERTURE_PG_INTEGRATION"
	pgDSNEnv  = "APERTURE_PG_DSN"
)

func requirePostgres(t *testing.T) string {
	t.Helper()
	if os.Getenv(pgGateEnv) != "1" {
		t.Skipf("skipping: set %s=1 and %s=<dsn> to run the live Postgres tests", pgGateEnv, pgDSNEnv)
	}
	dsn := os.Getenv(pgDSNEnv)
	if dsn == "" {
		t.Fatalf("%s=1 but %s is empty: the gate is on and there is no database to run against", pgGateEnv, pgDSNEnv)
	}
	return dsn
}

// withSearchPath returns dsn with the connection's search_path pinned to name.
// pgx passes any connection-string key it does not recognise to the server as a
// runtime parameter, which is how a test gets its own namespace without a
// configuration knob this story does not own.
func withSearchPath(t *testing.T, dsn, name string) string {
	t.Helper()
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatalf("parse %s: %v", pgDSNEnv, err)
		}
		q := u.Query()
		q.Set("search_path", name)
		u.RawQuery = q.Encode()
		return u.String()
	}
	return dsn + " search_path=" + name
}

// scratchSchema creates an empty schema for one test and drops it afterwards, so
// a live run leaves no residue in whatever database the operator pointed it at.
// It returns the schema name and a DSN whose search_path resolves to it.
func scratchSchema(t *testing.T, ctx context.Context, dsn string) (string, string) {
	t.Helper()
	name := fmt.Sprintf("aperture_e2e_%d", time.Now().UnixNano())
	admin := adminConn(t, ctx, dsn)
	if _, err := admin.ExecContext(ctx, `CREATE SCHEMA `+quoteIdent(name)); err != nil {
		t.Fatalf("create schema %s: %v", name, err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(`DROP SCHEMA IF EXISTS ` + quoteIdent(name) + ` CASCADE`)
	})
	return name, withSearchPath(t, dsn, name)
}

func adminConn(t *testing.T, ctx context.Context, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("ping %s: %v", pgDSNEnv, err)
	}
	return db
}

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// tableCount reports how many of Aperture's tables exist in schema name.
func tableCount(t *testing.T, ctx context.Context, db *sql.DB, name string) int {
	t.Helper()
	var n int
	err := db.QueryRowContext(ctx, `
SELECT count(*)
  FROM pg_catalog.pg_class c
  JOIN pg_catalog.pg_namespace ns ON ns.oid = c.relnamespace
 WHERE ns.nspname = $1 AND c.relkind IN ('r','p') AND c.relname LIKE 'apt\_%'`, name).Scan(&n)
	if err != nil {
		t.Fatalf("count tables in %s: %v", name, err)
	}
	return n
}

func liveCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// TestPostgresLive_SetupAppliesTheSchemaAndIsIdempotent is the baseline: every
// table and index lands, and a second Setup against the same database is a
// no-op that still returns nil.
func TestPostgresLive_SetupAppliesTheSchemaAndIsIdempotent(t *testing.T) {
	dsn := requirePostgres(t)
	ctx := liveCtx(t)
	name, storeDSN := scratchSchema(t, ctx, dsn)
	admin := adminConn(t, ctx, dsn)

	s, err := Open(storeDSN)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	if err := s.Setup(ctx); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if got := tableCount(t, ctx, admin, name); got != len(expectedTables) {
		t.Fatalf("schema %s has %d apt_ tables after Setup, want %d", name, got, len(expectedTables))
	}

	// Every index the schema declares landed in the same schema as its table,
	// unqualified index names notwithstanding.
	var indexes int
	if err := admin.QueryRowContext(ctx, `
SELECT count(*)
  FROM pg_catalog.pg_class c
  JOIN pg_catalog.pg_namespace ns ON ns.oid = c.relnamespace
 WHERE ns.nspname = $1 AND c.relkind = 'i' AND c.relname LIKE 'idx\_%'`, name).Scan(&indexes); err != nil {
		t.Fatalf("count indexes: %v", err)
	}
	if indexes != 7 {
		t.Errorf("schema %s has %d idx_ indexes, want 7", name, indexes)
	}

	// Idempotent: creating nothing is a success, not a refusal.
	if err := s.Setup(ctx); err != nil {
		t.Fatalf("second Setup: %v", err)
	}
	if got := tableCount(t, ctx, admin, name); got != len(expectedTables) {
		t.Errorf("schema %s has %d apt_ tables after the second Setup, want %d", name, got, len(expectedTables))
	}
}

// TestPostgresLive_ConcurrentSetupsAllSucceed is this story's central claim, and
// the reason it could not be settled by reading: CREATE TABLE IF NOT EXISTS is
// not race-free in PostgreSQL, so without the advisory lock several of these
// calls fail on 42P07 (duplicate_table) or 23505 (unique_violation on a pg_class
// index) while looking, in the source, entirely correct.
//
// Each goroutine gets its OWN Store and its own pool, which is what makes them
// genuinely concurrent sessions rather than statements multiplexed over one
// connection — the same shape as several aperture processes booting at once.
func TestPostgresLive_ConcurrentSetupsAllSucceed(t *testing.T) {
	dsn := requirePostgres(t)
	ctx := liveCtx(t)
	name, storeDSN := scratchSchema(t, ctx, dsn)
	admin := adminConn(t, ctx, dsn)

	const racers = 8
	stores := make([]*Store, racers)
	for i := range stores {
		s, err := Open(storeDSN)
		if err != nil {
			t.Fatalf("Open %d: %v", i, err)
		}
		stores[i] = s
		defer func() { _ = s.Close() }()
		// Force the connection open before the barrier, so the race is over
		// Setup rather than over TCP and authentication.
		if err := s.pool.PingContext(ctx); err != nil {
			t.Fatalf("ping %d: %v", i, err)
		}
	}

	start := make(chan struct{})
	errs := make([]error, racers)
	var wg sync.WaitGroup
	for i := range stores {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = stores[i].Setup(ctx)
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err == nil {
			continue
		}
		// Name the two states explicitly: seeing either one is not "a flake",
		// it is the lock not doing its job.
		for _, state := range []string{"42P07", "23505"} {
			if strings.Contains(err.Error(), state) {
				t.Errorf("concurrent Setup %d lost the CREATE race with SQLSTATE %s — the advisory lock is not excluding: %v",
					i, state, err)
			}
		}
		t.Errorf("concurrent Setup %d failed: %v", i, err)
	}
	if got := tableCount(t, ctx, admin, name); got != len(expectedTables) {
		t.Errorf("schema %s has %d apt_ tables after %d concurrent Setups, want %d",
			name, got, racers, len(expectedTables))
	}
}

// TestPostgresLive_SetupWithoutCreatePrivilegeIsCoded proves the DDL-privilege
// criterion against the server, with no role management required: PostgreSQL
// refuses CREATE in pg_catalog with SQLSTATE 42501 for EVERY role, superuser
// included ("permission denied to create ...", detail "System catalog
// modifications are currently disallowed"). Pointing search_path there is the
// one privilege-free way to make a real server produce the exact state an
// under-privileged application role would.
func TestPostgresLive_SetupWithoutCreatePrivilegeIsCoded(t *testing.T) {
	dsn := requirePostgres(t)
	ctx := liveCtx(t)

	s, err := Open(withSearchPath(t, dsn, "pg_catalog"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	err = s.Setup(ctx)
	if err == nil {
		t.Fatalf("Setup into pg_catalog succeeded; it must be refused")
	}
	if code := aerr.CodeOf(err); code != aerr.APERTURE_STORAGE {
		t.Fatalf("Setup returned code %q, want APERTURE_STORAGE — a raw driver error reached the caller", code)
	}
	if !strings.Contains(err.Error(), "CREATE") {
		t.Errorf("the refusal does not tell the operator what to grant: %v", err)
	}
	if !strings.Contains(err.Error(), "pg_catalog") {
		t.Errorf("the refusal does not name the schema it could not create in: %v", err)
	}
}

// TestPostgresLive_SetupRefusesAConnectionWithNoSchema covers the ENSURE-SCHEMA
// step: a search_path naming nothing that exists leaves current_schema() NULL,
// and Postgres would otherwise report it as "no schema has been selected to
// create in" from the first CREATE, several steps later.
func TestPostgresLive_SetupRefusesAConnectionWithNoSchema(t *testing.T) {
	dsn := requirePostgres(t)
	ctx := liveCtx(t)

	s, err := Open(withSearchPath(t, dsn, "aperture_no_such_schema"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	err = s.Setup(ctx)
	if err == nil {
		t.Fatalf("Setup with an unresolvable search_path succeeded")
	}
	if code := aerr.CodeOf(err); code != aerr.APERTURE_STORAGE {
		t.Fatalf("Setup returned code %q, want APERTURE_STORAGE", code)
	}
	if !strings.Contains(err.Error(), "search_path") {
		t.Errorf("the refusal does not name search_path: %v", err)
	}
}

// TestPostgresLive_ConstraintViolationsAreCoded proves the SQLSTATE mapping
// against a real server rather than a fake: the foreign keys schema.sql declares
// really do fire, and 23503 and 23505 really do arrive as
// APERTURE_STORAGE_CONSTRAINT. The statement set that will normally produce
// these is E4-S3's; the writes here are deliberately raw so the mapping is
// tested and nothing else is.
func TestPostgresLive_ConstraintViolationsAreCoded(t *testing.T) {
	dsn := requirePostgres(t)
	ctx := liveCtx(t)
	_, storeDSN := scratchSchema(t, ctx, dsn)

	s, err := Open(storeDSN)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Setup(ctx); err != nil {
		t.Fatalf("Setup: %v", err)
	}

	// 23503: a membership naming a principal that does not exist.
	_, raw := s.pool.ExecContext(ctx,
		`INSERT INTO apt_memberships (principal_id, account_id) VALUES ('ghost', 'acct')`)
	if raw == nil {
		t.Fatalf("an orphan membership was accepted; the foreign key is not enforcing")
	}
	if state := sqlStateOf(raw); state != "23503" {
		t.Fatalf("orphan membership gave SQLSTATE %q, want 23503", state)
	}
	if code := aerr.CodeOf(wrapStorage("put membership", raw)); code != aerr.APERTURE_STORAGE_CONSTRAINT {
		t.Errorf("23503 mapped to %q, want APERTURE_STORAGE_CONSTRAINT", code)
	}

	// 23505: the same account inserted twice.
	if _, err := s.pool.ExecContext(ctx, `INSERT INTO apt_accounts (id, name) VALUES ('a', 'A')`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	_, raw = s.pool.ExecContext(ctx, `INSERT INTO apt_accounts (id, name) VALUES ('a', 'A')`)
	if raw == nil {
		t.Fatalf("a duplicate primary key was accepted")
	}
	if state := sqlStateOf(raw); state != "23505" {
		t.Fatalf("duplicate account gave SQLSTATE %q, want 23505", state)
	}
	if code := aerr.CodeOf(wrapStorage("put account", raw)); code != aerr.APERTURE_STORAGE_CONSTRAINT {
		t.Errorf("23505 mapped to %q, want APERTURE_STORAGE_CONSTRAINT", code)
	}
}
