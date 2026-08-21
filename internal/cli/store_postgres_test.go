package cli

import (
	"strings"
	"testing"

	ucli "github.com/urfave/cli/v3"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/storage/postgres"
	"github.com/frankbardon/aperture/storage/sqlite"
)

// E4-S4, the CLI half: the Postgres backend existed and was complete, and no
// `aperture` command could reach it. These tests are about the selection rule
// and the boot-time refusal — never about talking to a server, which is what
// keeps them inside `make test`. sql.Open is lazy, so opening a Postgres store
// against a DSN nobody is listening on connects to nothing.

// TestOpenStoreSelectsTheBackendFromTheDSN pins the whole rule. The cases that
// matter are the NEGATIVE ones: --store's other value is a filesystem path, and
// a classifier that reached for Postgres on anything vaguely URL-shaped would
// send an operator's local database over the network.
func TestOpenStoreSelectsTheBackendFromTheDSN(t *testing.T) {
	cases := []struct {
		dsn      string
		postgres bool
		why      string
	}{
		{"postgres://user:pw@db.internal:5432/aperture?sslmode=require", true, "the canonical URL"},
		{"postgresql://user@localhost/aperture", true, "libpq's other scheme"},
		{"postgres://", true, "degenerate, but unambiguously the scheme"},
		{"", false, "empty is the in-memory demo store, handled before either branch"},
		{"/var/lib/aperture/aperture.db", false, "an absolute path"},
		{"file:aperture.db?_pragma=foreign_keys(1)", false, "SQLite's own URI form"},
		{":memory:", false, "SQLite in-memory"},
		{"aperture.db", false, "a bare relative path"},
		{"./data/postgres/aperture.db", false, "a path that CONTAINS the word postgres"},
		{"host=db.internal dbname=aperture", false, "a libpq keyword DSN is deliberately not accepted"},
		{"mypostgres://x", false, "the scheme must be at the start"},
		{"POSTGRES://x", false, "the match is exact; an uppercase scheme is not a path Aperture will guess at"},
	}
	for _, tc := range cases {
		if got := isPostgresDSN(tc.dsn); got != tc.postgres {
			t.Errorf("isPostgresDSN(%q) = %v, want %v — %s", tc.dsn, got, tc.postgres, tc.why)
		}
	}
}

// TestOpenStoreBuildsAPostgresStore is the criterion: an operator can point
// Aperture at Postgres. It asserts the TYPE, because "no error" would also be
// true of a SQLite store cheerfully creating a file called
// "postgres:/user@host/db".
func TestOpenStoreBuildsAPostgresStore(t *testing.T) {
	store, err := openStore("postgres://nobody@127.0.0.1:1/nothing?sslmode=disable")
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, ok := store.(*postgres.Store); !ok {
		t.Fatalf("a postgres:// DSN produced %T, want *postgres.Store", store)
	}
	if _, ok := store.(*sqlite.Store); ok {
		t.Fatalf("a postgres:// DSN produced a SQLite store, which would have created a FILE named after the DSN")
	}
}

// TestOpenStoreConfiguresThePostgresSchemaFromTheEnvironment is the wiring, and
// the reason there is no --store-schema flag: the environment variable IS the
// knob, so it has to be read at the one place the CLI opens the backend.
func TestOpenStoreConfiguresThePostgresSchemaFromTheEnvironment(t *testing.T) {
	t.Setenv(postgres.EnvSchema, "aperture_authz")
	store, err := openStore("postgres://nobody@127.0.0.1:1/nothing?sslmode=disable")
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	pg, ok := store.(*postgres.Store)
	if !ok {
		t.Fatalf("got %T, want *postgres.Store", store)
	}
	if pg.Schema() != "aperture_authz" {
		t.Errorf("Schema() = %q, want %q — %s is not reaching the backend",
			pg.Schema(), "aperture_authz", postgres.EnvSchema)
	}
}

// TestOpenStoreWithNoSchemaConfiguredIsAmbient is the zero-config path, and it
// is the one that must not regress: unset means "use the connection's
// search_path", which is how every deployment that predates this knob keeps
// working and what lets the apt_ table prefix carry a shared database.
func TestOpenStoreWithNoSchemaConfiguredIsAmbient(t *testing.T) {
	t.Setenv(postgres.EnvSchema, "")
	store, err := openStore("postgres://nobody@127.0.0.1:1/nothing?sslmode=disable")
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	pg, ok := store.(*postgres.Store)
	if !ok {
		t.Fatalf("got %T, want *postgres.Store", store)
	}
	if pg.Schema() != "" {
		t.Errorf("Schema() = %q with %s unset, want the ambient search_path", pg.Schema(), postgres.EnvSchema)
	}
}

// TestOpenStoreRefusesAnInjectableSchemaWithItsOwnCode is the security wiring,
// and the assertion that matters is the CODE.
//
// bootError re-stamps a bare error APERTURE_BOOT, whose registry fixups are
// "check your environment variables" and "confirm the backend is reachable" —
// true of every startup failure there is and no help with this one. The refusal
// from postgres.Open already says which variable, which value, and what the rule
// is; the guard in bootError is what lets the operator read it. A wrap without
// that guard would bury the whole message class.
func TestOpenStoreRefusesAnInjectableSchemaWithItsOwnCode(t *testing.T) {
	t.Setenv(postgres.EnvSchema, `public"; DROP SCHEMA public CASCADE; --`)
	store, err := openStore("postgres://nobody@127.0.0.1:1/nothing?sslmode=disable")
	if err == nil {
		_ = store.Close()
		t.Fatalf("the CLI accepted an injectable %s", postgres.EnvSchema)
	}
	if store != nil {
		t.Errorf("openStore returned a store alongside the refusal")
	}
	if code := aerr.CodeOf(err); code != aerr.APERTURE_CONFIG_INVALID {
		t.Fatalf("the refusal reached the operator as %q, want APERTURE_CONFIG_INVALID (bootError must pass "+
			"an existing code through rather than re-stamping it APERTURE_BOOT)", code)
	}
	if !strings.Contains(err.Error(), postgres.EnvSchema) {
		t.Errorf("the refusal does not name the variable to fix: %v", err)
	}
	// The whole chain, so a future wrap that hides the cause is caught too.
	for _, code := range codeChain(err) {
		if code == aerr.APERTURE_BOOT {
			t.Errorf("APERTURE_BOOT appears in the chain %v; the specific code must be the outermost and only one",
				codeChain(err))
		}
	}
}

// TestStoreFlagUsageNamesBothBackendsAndTheSchemaVariable keeps the help text
// honest. --store used to say "sqlite DSN", which is now wrong in a way that
// hides a whole backend: an operator reading it would conclude Postgres is not
// supported. The flag is the only place the CLI can say otherwise, since the
// schema knob is an environment variable with no flag of its own.
func TestStoreFlagUsageNamesBothBackendsAndTheSchemaVariable(t *testing.T) {
	seen := 0
	var walk func(cmds []*ucli.Command)
	walk = func(cmds []*ucli.Command) {
		for _, cmd := range cmds {
			for _, f := range cmd.Flags {
				sf, ok := f.(*ucli.StringFlag)
				if !ok || sf.Name != "store" {
					continue
				}
				seen++
				for _, want := range []string{"postgres://", "SQLite", "in-memory", postgres.EnvSchema} {
					if !strings.Contains(sf.Usage, want) {
						t.Errorf("`%s --store` usage does not mention %q: %s", cmd.Name, want, sf.Usage)
					}
				}
			}
			walk(cmd.Commands)
		}
	}
	walk(NewApp("test").Commands)
	if seen == 0 {
		t.Fatalf("no --store flag was found in the command tree; this test is broken, not the help text")
	}
}
