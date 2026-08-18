package seed

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/frankbardon/aperture/identity"
)

// The one test in this repo that talks to a real Postgres.
//
// It is GATED, and it must stay gated: CI runs on ubuntu with no service
// containers, so `make test` has to pass with no database present. The gate
// mirrors the bench NFR convention (APERTURE_BENCH_ASSERT=1):
//
//	APERTURE_PG_INTEGRATION=1 \
//	APERTURE_PG_DSN='postgres://aperture:aperture@localhost:5432/aperture?sslmode=disable' \
//	go test -run TestPostgresIntegration ./seed/
//
// Ungated it SKIPS. Gated without a DSN it FAILS — asking for the integration
// test and silently not getting one is the outcome a gate must never produce.
//
// It exists because everything else about the SQL path is proved against a fake
// driver, and a fake cannot prove the two things that are only true of the real
// one: that pgx is linked into a CGO_ENABLED=0 binary and actually connects, and
// that a real Postgres result set lands in the value model as the mapping table
// in sqlprovider/values.go claims. E2-S1 could not write this test — that story
// forbade the driver import — and this is the story that takes the driver on.
const (
	pgGateEnv = "APERTURE_PG_INTEGRATION"
	pgDSNEnv  = "APERTURE_PG_DSN"
)

// requirePostgres skips unless the gate is set, and returns the DSN.
func requirePostgres(t *testing.T) string {
	t.Helper()
	if os.Getenv(pgGateEnv) != "1" {
		t.Skipf("skipping: set %s=1 and %s=<dsn> to run the real-Postgres integration test", pgGateEnv, pgDSNEnv)
	}
	dsn := os.Getenv(pgDSNEnv)
	if dsn == "" {
		t.Fatalf("%s=1 but %s is empty: the gate is on and there is no database to run against", pgGateEnv, pgDSNEnv)
	}
	return dsn
}

// TestPostgresIntegration_SeedFileAloneServesRealObjects drives the whole story
// end to end against a live server: a YAML document with a connections: block
// and a kind: sql provider entry, the DEFAULT opener (a real pgx pool), and no
// Go wiring beyond reading the file.
func TestPostgresIntegration_SeedFileAloneServesRealObjects(t *testing.T) {
	dsn := requirePostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// A scratch table, dropped on the way out, so the test leaves no residue in
	// whatever database the operator pointed it at.
	table := fmt.Sprintf("aperture_e2e_%d", time.Now().UnixNano())
	admin, err := sql.Open(driverName, dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer func() { _ = admin.Close() }()
	if err := admin.PingContext(ctx); err != nil {
		t.Fatalf("ping %s: %v", pgDSNEnv, err)
	}
	if _, err := admin.ExecContext(ctx, fmt.Sprintf(
		`CREATE TABLE %s (id text PRIMARY KEY, tier text NOT NULL, seats int NOT NULL, renews_on date NOT NULL)`,
		table)); err != nil {
		t.Fatalf("create table: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(fmt.Sprintf(`DROP TABLE IF EXISTS %s`, table))
	})
	if _, err := admin.ExecContext(ctx, fmt.Sprintf(
		`INSERT INTO %s VALUES ('1','gold',12,'2026-03-01'), ('2','silver',3,'2026-09-15')`, table)); err != nil {
		t.Fatalf("insert: %v", err)
	}

	t.Setenv(pgDSNEnv, dsn)
	doc, err := Parse([]byte(fmt.Sprintf(`
connections:
  main:
    dsn_env: %s
    max_open_conns: 4
    query_timeout: 10s
providers:
  - object_type: brand
    kind: sql
    connection: main
    get_one: "SELECT tier, seats, renews_on FROM %s WHERE id = $1"
    get_all: "SELECT 'brand:' || id AS id, tier, seats, renews_on FROM %s"
    ttl: "0"
`, pgDSNEnv, table, table)), FormatYAML)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	// No WithConnectionOpener: this is the DEFAULT pgx path.
	reg, conns, err := doc.BuildRegistryWithConnections("")
	if err != nil {
		t.Fatalf("BuildRegistryWithConnections: %v", err)
	}
	defer func() { _ = conns.Close() }()
	if conns.Len() != 1 {
		t.Fatalf("Connections.Len() = %d, want 1", conns.Len())
	}

	md, err := reg.Fetch(ctx, identity.MustParse("brand:1"))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if md["tier"] != "gold" {
		t.Errorf("tier = %v (%T), want gold", md["tier"], md["tier"])
	}
	// The value model, against a real driver: an int column arrives as a Go
	// integer, and a date arrives as the canonical UTC text form — never as a
	// time.Time restated in the reader's zone. sqlprovider always formats a
	// timestamp at datetime granularity, so a DATE column reads back as midnight
	// Z rather than as a bare day.
	if got := fmt.Sprint(md["seats"]); got != "12" {
		t.Errorf("seats = %v (%T), want 12", md["seats"], md["seats"])
	}
	if got, want := md["renews_on"], "2026-03-01T00:00:00Z"; got != want {
		t.Errorf("renews_on = %v (%T), want %q", got, got, want)
	}

	ids, err := reg.Identifiers(ctx, "brand")
	if err != nil {
		t.Fatalf("Identifiers: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("Identifiers = %v, want 2 objects", ids)
	}

	// An absent object is NOT FOUND, not an operational failure — the distinction
	// the whole error taxonomy rests on.
	if _, err := reg.Fetch(ctx, identity.MustParse("brand:missing")); err == nil {
		t.Error("Fetch of an absent id succeeded")
	}
}

// TestPostgresIntegration_UnsetDSNIsAHardError proves the environment rule
// against the real opener: the build fails at BuildRegistryWithConnections, not
// lazily on the first query.
func TestPostgresIntegration_UnsetDSNIsAHardError(t *testing.T) {
	_ = requirePostgres(t)
	t.Setenv("APERTURE_PG_ABSENT_DSN", "")
	doc := &Document{Connections: map[string]Connection{
		"main": {DSNEnv: "APERTURE_PG_ABSENT_DSN"},
	}}
	if _, conns, err := doc.BuildRegistryWithConnections(""); err == nil {
		_ = conns.Close()
		t.Fatal("build succeeded with an unset DSN environment variable")
	}
}
