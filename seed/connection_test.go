package seed

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/identity"
	"github.com/frankbardon/aperture/storage/memory"
)

// A hand-rolled fake database, so every test here runs in plain `make test`
// with NO database present — CI has no service containers. It is a database/sql
// DRIVER rather than a Pool implementation because Pool embeds
// sqlprovider.Querier, whose methods return *sql.Rows, a type only
// database/sql can build. Going in through sql.Register therefore exercises the
// real pool: the real SetMaxOpenConns bookkeeping, the real context plumbing,
// and the real Close, with only the canned rows at the bottom being ours.
//
// The real pgx opener is swapped out with WithConnectionOpener, which is the
// documented seam for exactly this.

const fakeDriverName = "aperture-seed-fake"

func init() { sql.Register(fakeDriverName, fakeDriver{}) }

// fakeTable is one canned result set, keyed by the statement that returns it.
type fakeTable struct {
	cols []string
	rows [][]driver.Value
}

// fakeDB is the canned behaviour of one fake database plus what it observed.
type fakeDB struct {
	mu           sync.Mutex
	tables       map[string]fakeTable
	queries      []string
	lastDeadline time.Duration // remaining budget observed on the last statement
}

func (f *fakeDB) record(ctx context.Context, query string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries = append(f.queries, query)
	if dl, ok := ctx.Deadline(); ok {
		f.lastDeadline = time.Until(dl)
	}
}

func (f *fakeDB) observedQueries() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.queries...)
}

func (f *fakeDB) budget() time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastDeadline
}

var (
	fakeSeq atomic.Int64
	fakeMu  sync.Mutex
	fakeReg = map[string]*fakeDB{}
)

// newFakeDSN registers db under a fresh DSN so tests stay independent.
func newFakeDSN(t *testing.T, db *fakeDB) string {
	t.Helper()
	dsn := fmt.Sprintf("fake-%d", fakeSeq.Add(1))
	fakeMu.Lock()
	fakeReg[dsn] = db
	fakeMu.Unlock()
	t.Cleanup(func() {
		fakeMu.Lock()
		delete(fakeReg, dsn)
		fakeMu.Unlock()
	})
	return dsn
}

type fakeDriver struct{}

func (fakeDriver) Open(dsn string) (driver.Conn, error) {
	fakeMu.Lock()
	db, ok := fakeReg[dsn]
	fakeMu.Unlock()
	if !ok {
		return nil, fmt.Errorf("no fake database registered for %q", dsn)
	}
	return &fakeConn{db: db}, nil
}

type fakeConn struct{ db *fakeDB }

var (
	_ driver.Conn           = (*fakeConn)(nil)
	_ driver.QueryerContext = (*fakeConn)(nil)
)

func (c *fakeConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("fake: Prepare is not supported")
}
func (c *fakeConn) Close() error              { return nil }
func (c *fakeConn) Begin() (driver.Tx, error) { return nil, errors.New("fake: no transactions") }

func (c *fakeConn) QueryContext(ctx context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	c.db.record(ctx, query)
	c.db.mu.Lock()
	tbl, ok := c.db.tables[query]
	c.db.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("fake: no canned result for %q", query)
	}
	return &fakeRows{cols: tbl.cols, rows: tbl.rows}, nil
}

type fakeRows struct {
	cols []string
	rows [][]driver.Value
	i    int
}

func (r *fakeRows) Columns() []string { return r.cols }
func (r *fakeRows) Close() error      { return nil }
func (r *fakeRows) Next(dest []driver.Value) error {
	if r.i >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.i])
	r.i++
	return nil
}

// countingOpener returns a ConnectionOpener over the fake driver that records
// the settings it was handed, per connection name, and how many times it was
// called for each. The call count is what proves one pool per NAME rather than
// one per provider entry.
type countingOpener struct {
	mu       sync.Mutex
	dsn      string
	calls    map[string]int
	settings map[string]ConnectionSettings
	opened   []*sql.DB
	failOn   string // connection name to refuse, for the teardown-on-failure test
}

func newCountingOpener(dsn string) *countingOpener {
	return &countingOpener{dsn: dsn, calls: map[string]int{}, settings: map[string]ConnectionSettings{}}
}

func (o *countingOpener) open(name string, cfg ConnectionSettings) (Pool, error) {
	o.mu.Lock()
	o.calls[name]++
	o.settings[name] = cfg
	fail := o.failOn == name
	o.mu.Unlock()
	if fail {
		return nil, aerr.New(aerr.APERTURE_SQL_PROVIDER_CONNECTION, "fake opener: refusing "+name)
	}
	db, err := sql.Open(fakeDriverName, cfg.DSN)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(cfg.MaxOpenConns)
	db.SetMaxIdleConns(cfg.MaxIdleConns)
	db.SetConnMaxLifetime(cfg.ConnMaxLifetime)
	o.mu.Lock()
	o.opened = append(o.opened, db)
	o.mu.Unlock()
	return db, nil
}

func (o *countingOpener) callsFor(name string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.calls[name]
}

func (o *countingOpener) settingsFor(name string) ConnectionSettings {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.settings[name]
}

// openedPools returns every *sql.DB the opener handed out.
func (o *countingOpener) openedPools() []*sql.DB {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]*sql.DB(nil), o.opened...)
}

const (
	brandFetch = `SELECT tier, seats FROM brands WHERE id = $1`
	brandList  = `SELECT 'brand:' || id AS id, tier, seats FROM brands`
	appList    = `SELECT 'app:' || id AS id, name FROM apps`
	appFetch   = `SELECT name FROM apps WHERE id = $1`
)

// brandDB is the canned database both statements read.
func brandDB() *fakeDB {
	return &fakeDB{tables: map[string]fakeTable{
		brandFetch: {
			cols: []string{"tier", "seats"},
			rows: [][]driver.Value{{"gold", int64(12)}},
		},
		brandList: {
			cols: []string{"id", "tier", "seats"},
			rows: [][]driver.Value{
				{"brand:1", "gold", int64(12)},
				{"brand:2", "silver", int64(3)},
			},
		},
		appFetch: {
			cols: []string{"name"},
			rows: [][]driver.Value{{"console"}},
		},
		appList: {
			cols: []string{"id", "name"},
			rows: [][]driver.Value{{"app:9", "console"}},
		},
	}}
}

// TestBuildRegistry_SQLProviderEndToEnd is the story's headline: a seed file
// ALONE — connections: plus kind: sql — produces a live registry that fetches
// and enumerates, with no Go wiring and no database present.
func TestBuildRegistry_SQLProviderEndToEnd(t *testing.T) {
	db := brandDB()
	dsn := newFakeDSN(t, db)
	t.Setenv("APERTURE_TEST_DSN", dsn)
	opener := newCountingOpener(dsn)

	doc, err := Parse([]byte(`
connections:
  main:
    dsn_env: APERTURE_TEST_DSN
    max_open_conns: 4
    max_idle_conns: 2
    conn_max_lifetime: 1h
    query_timeout: 3s
providers:
  - object_type: brand
    kind: sql
    connection: main
    get_one: "`+brandFetch+`"
    get_all: "`+brandList+`"
    ttl: "0"
`), FormatYAML)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := doc.Connections["main"].DSNEnv; got != "APERTURE_TEST_DSN" {
		t.Fatalf("dsn_env = %q", got)
	}

	reg, conns, err := doc.BuildRegistryWithConnections("", WithConnectionOpener(opener.open))
	if err != nil {
		t.Fatalf("BuildRegistryWithConnections: %v", err)
	}
	defer func() { _ = conns.Close() }()

	if !reg.Has("brand") {
		t.Fatal("registry has no brand provider")
	}
	md, err := reg.Fetch(context.Background(), identity.MustParse("brand:1"))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if md["tier"] != "gold" {
		t.Errorf("tier = %v, want gold", md["tier"])
	}
	if md["seats"] != int64(12) {
		t.Errorf("seats = %v (%T), want int64(12)", md["seats"], md["seats"])
	}

	ids, err := reg.Identifiers(context.Background(), "brand")
	if err != nil {
		t.Fatalf("Identifiers: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("Identifiers returned %d objects, want 2", len(ids))
	}

	// The filter contract is sqlprovider's, inherited verbatim through the YAML
	// path: a typed equality over an enumerated row. The registry reaches it as
	// an ObjectLister, so go through the pattern-and-limit form the scope
	// resolver uses, then through the provider's own Query for the predicate.
	matched, err := reg.List(context.Background(), "brand", identity.MustParsePattern("brand:*"), 10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(matched) != 2 {
		t.Fatalf("List returned %d objects, want 2", len(matched))
	}

	// The connection's pool settings and its statement budget reached the driver.
	got := opener.settingsFor("main")
	if got.DSN != dsn {
		t.Errorf("opener saw DSN %q, want the value of APERTURE_TEST_DSN", got.DSN)
	}
	if got.MaxOpenConns != 4 || got.MaxIdleConns != 2 || got.ConnMaxLifetime != time.Hour {
		t.Errorf("pool settings = %+v", got)
	}
	if got.QueryTimeout != 3*time.Second {
		t.Errorf("query timeout = %v, want 3s", got.QueryTimeout)
	}
	if b := db.budget(); b <= 0 || b > 3*time.Second {
		t.Errorf("statement ran with %v of budget, want a positive value <= 3s", b)
	}
	if qs := db.observedQueries(); len(qs) == 0 {
		t.Fatal("the fake database saw no statements")
	}
}

// TestConnectionDefaults pins the documented defaults for every optional key.
func TestConnectionDefaults(t *testing.T) {
	dsn := newFakeDSN(t, brandDB())
	t.Setenv("APERTURE_TEST_DSN", dsn)
	opener := newCountingOpener(dsn)

	doc := &Document{Connections: map[string]Connection{
		"main": {DSNEnv: "APERTURE_TEST_DSN"},
	}}
	_, conns, err := doc.BuildRegistryWithConnections("", WithConnectionOpener(opener.open))
	if err != nil {
		t.Fatalf("BuildRegistryWithConnections: %v", err)
	}
	defer func() { _ = conns.Close() }()

	got := opener.settingsFor("main")
	want := ConnectionSettings{
		DSN:             dsn,
		MaxOpenConns:    DefaultMaxOpenConns,
		MaxIdleConns:    DefaultMaxIdleConns,
		ConnMaxLifetime: DefaultConnMaxLifetime,
		QueryTimeout:    DefaultQueryTimeout,
	}
	if got != want {
		t.Errorf("defaults = %+v, want %+v", got, want)
	}
	// The story fixes this one by name.
	if DefaultQueryTimeout != 5*time.Second {
		t.Errorf("DefaultQueryTimeout = %v, want 5s", DefaultQueryTimeout)
	}
}

// TestParse_RejectsLiteralDSN is the security rule: a seed file is committed, so
// a DSN written into one is refused at DECODE, in both formats, by a coded error
// that names the offending connection and the key to use instead.
func TestParse_RejectsLiteralDSN(t *testing.T) {
	cases := map[string]struct {
		data   []byte
		format Format
	}{
		"yaml": {[]byte("connections:\n  main:\n    dsn: postgres://u:pw@h/db\n"), FormatYAML},
		"json": {[]byte(`{"connections":{"main":{"dsn":"postgres://u:pw@h/db"}}}`), FormatJSON},
		"yaml alongside dsn_env": {
			[]byte("connections:\n  main:\n    dsn_env: X\n    dsn: postgres://u:pw@h/db\n"), FormatYAML},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			doc, err := Parse(tc.data, tc.format)
			if err == nil {
				t.Fatalf("Parse accepted a literal dsn:, doc = %+v", doc)
			}
			if aerr.CodeOf(err) != aerr.APERTURE_SQL_PROVIDER_DSN_LITERAL {
				t.Fatalf("code = %s, want APERTURE_SQL_PROVIDER_DSN_LITERAL", aerr.CodeOf(err))
			}
			msg := err.Error()
			for _, want := range []string{`"main"`, "dsn:", "dsn_env"} {
				if !strings.Contains(msg, want) {
					t.Errorf("message %q does not mention %q", msg, want)
				}
			}
			// The rejection must not echo the credential it is objecting to.
			if strings.Contains(msg, "pw") {
				t.Errorf("message leaks the rejected DSN: %q", msg)
			}
		})
	}
}

// TestParse_AcceptsDSNEnv guards the other direction: the trap field must not
// make a legitimate document fail.
func TestParse_AcceptsDSNEnv(t *testing.T) {
	doc, err := Parse([]byte("connections:\n  main:\n    dsn_env: APP_DATABASE_URL\n"), FormatYAML)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if doc.Connections["main"].DSNEnv != "APP_DATABASE_URL" {
		t.Fatalf("connections = %+v", doc.Connections)
	}
}

// TestBuildRegistry_ConnectionErrors covers every hard failure the wiring owes
// at BUILD time. Each one would otherwise first surface as a failed decision,
// and a failed decision reads as a denial.
func TestBuildRegistry_ConnectionErrors(t *testing.T) {
	dsn := newFakeDSN(t, brandDB())

	sqlEntry := Provider{
		ObjectType: "brand", Kind: "sql", Connection: "main",
		GetOne: brandFetch, GetAll: brandList,
	}

	cases := map[string]struct {
		doc     *Document
		env     map[string]string
		wantErr aerr.Code
		mention []string
	}{
		"unset env var": {
			doc: &Document{
				Connections: map[string]Connection{"main": {DSNEnv: "APERTURE_TEST_MISSING_DSN"}},
				Providers:   []Provider{sqlEntry},
			},
			wantErr: aerr.APERTURE_SQL_PROVIDER_CONNECTION,
			mention: []string{"APERTURE_TEST_MISSING_DSN", "main"},
		},
		"empty env var": {
			doc: &Document{
				Connections: map[string]Connection{"main": {DSNEnv: "APERTURE_TEST_EMPTY_DSN"}},
				Providers:   []Provider{sqlEntry},
			},
			env:     map[string]string{"APERTURE_TEST_EMPTY_DSN": "   "},
			wantErr: aerr.APERTURE_SQL_PROVIDER_CONNECTION,
			mention: []string{"APERTURE_TEST_EMPTY_DSN"},
		},
		"missing dsn_env": {
			doc: &Document{
				Connections: map[string]Connection{"main": {}},
				Providers:   []Provider{sqlEntry},
			},
			wantErr: aerr.APERTURE_SQL_PROVIDER_CONNECTION,
			mention: []string{"dsn_env", "main"},
		},
		"unknown connection name": {
			doc: &Document{
				Connections: map[string]Connection{"main": {DSNEnv: "APERTURE_TEST_DSN"}},
				Providers: []Provider{{
					ObjectType: "brand", Kind: "sql", Connection: "typo",
					GetOne: brandFetch, GetAll: brandList,
				}},
			},
			env:     map[string]string{"APERTURE_TEST_DSN": dsn},
			wantErr: aerr.APERTURE_SQL_PROVIDER_CONNECTION,
			mention: []string{"typo", "connections:"},
		},
		"provider missing connection": {
			doc: &Document{Providers: []Provider{{
				ObjectType: "brand", Kind: "sql",
				GetOne: brandFetch, GetAll: brandList,
			}}},
			wantErr: aerr.APERTURE_CONFIG_INVALID,
			mention: []string{"connection"},
		},
		"provider missing get_one": {
			doc: &Document{
				Connections: map[string]Connection{"main": {DSNEnv: "APERTURE_TEST_DSN"}},
				Providers: []Provider{{
					ObjectType: "brand", Kind: "sql", Connection: "main", GetAll: brandList,
				}},
			},
			env:     map[string]string{"APERTURE_TEST_DSN": dsn},
			wantErr: aerr.APERTURE_CONFIG_INVALID,
			mention: []string{"FetchQuery"},
		},
		"provider missing get_all": {
			doc: &Document{
				Connections: map[string]Connection{"main": {DSNEnv: "APERTURE_TEST_DSN"}},
				Providers: []Provider{{
					ObjectType: "brand", Kind: "sql", Connection: "main", GetOne: brandFetch,
				}},
			},
			env:     map[string]string{"APERTURE_TEST_DSN": dsn},
			wantErr: aerr.APERTURE_CONFIG_INVALID,
			mention: []string{"ListQuery"},
		},
		"bad conn_max_lifetime": {
			doc: &Document{Connections: map[string]Connection{
				"main": {DSNEnv: "APERTURE_TEST_DSN", ConnMaxLifetime: "notaduration"},
			}},
			env:     map[string]string{"APERTURE_TEST_DSN": dsn},
			wantErr: aerr.APERTURE_SQL_PROVIDER_CONNECTION,
			mention: []string{"conn_max_lifetime"},
		},
		"bad query_timeout": {
			doc: &Document{Connections: map[string]Connection{
				"main": {DSNEnv: "APERTURE_TEST_DSN", QueryTimeout: "soon"},
			}},
			env:     map[string]string{"APERTURE_TEST_DSN": dsn},
			wantErr: aerr.APERTURE_SQL_PROVIDER_CONNECTION,
			mention: []string{"query_timeout"},
		},
		"zero query_timeout": {
			doc: &Document{Connections: map[string]Connection{
				"main": {DSNEnv: "APERTURE_TEST_DSN", QueryTimeout: "0"},
			}},
			env:     map[string]string{"APERTURE_TEST_DSN": dsn},
			wantErr: aerr.APERTURE_SQL_PROVIDER_CONNECTION,
			mention: []string{"query_timeout", "no timeout"},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			opener := newCountingOpener(dsn)
			_, conns, err := tc.doc.BuildRegistryWithConnections("", WithConnectionOpener(opener.open))
			if err == nil {
				_ = conns.Close()
				t.Fatal("build succeeded, want a hard error")
			}
			if aerr.CodeOf(err) != tc.wantErr {
				t.Fatalf("code = %s, want %s (%v)", aerr.CodeOf(err), tc.wantErr, err)
			}
			for _, want := range tc.mention {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("message %q does not mention %q", err.Error(), want)
				}
			}
			// A failed build strands nothing: every pool it opened is closed.
			assertPoolsClosed(t, opener.openedPools())
		})
	}
}

// TestBuildRegistry_OnePoolPerNamedConnection is the acceptance criterion this
// whole indirection exists for: duplicate pools to one server is the failure
// mode, so three provider entries over one connection must produce ONE pool.
func TestBuildRegistry_OnePoolPerNamedConnection(t *testing.T) {
	dsn := newFakeDSN(t, brandDB())
	t.Setenv("APERTURE_TEST_DSN", dsn)
	other := newFakeDSN(t, brandDB())
	t.Setenv("APERTURE_TEST_DSN_2", other)
	opener := newCountingOpener(dsn)

	doc := &Document{
		Connections: map[string]Connection{
			"main":      {DSNEnv: "APERTURE_TEST_DSN"},
			"reporting": {DSNEnv: "APERTURE_TEST_DSN_2"},
		},
		Providers: []Provider{
			{ObjectType: "brand", Kind: "sql", Connection: "main", GetOne: brandFetch, GetAll: brandList},
			{ObjectType: "app", Kind: "sql", Connection: "main", GetOne: appFetch, GetAll: appList},
			{ObjectType: "widget", Kind: "sql", Connection: "reporting", GetOne: appFetch, GetAll: appList},
		},
	}
	reg, conns, err := doc.BuildRegistryWithConnections("", WithConnectionOpener(opener.open))
	if err != nil {
		t.Fatalf("BuildRegistryWithConnections: %v", err)
	}
	defer func() { _ = conns.Close() }()

	if got := opener.callsFor("main"); got != 1 {
		t.Errorf("opener called %d times for connection \"main\", want exactly 1 — two provider entries must SHARE one pool", got)
	}
	if got := opener.callsFor("reporting"); got != 1 {
		t.Errorf("opener called %d times for connection \"reporting\", want 1", got)
	}
	if got := conns.Len(); got != 2 {
		t.Errorf("Connections.Len() = %d, want 2 (one per NAME, not one per provider entry)", got)
	}
	if got := len(reg.Keys()); got != 3 {
		t.Errorf("registry has %d types, want 3", got)
	}
	if got := strings.Join(conns.Names(), ","); got != "main,reporting" {
		t.Errorf("Names() = %q", got)
	}
}

// TestConnections_CloseReleasesEveryPool covers the shutdown half of the
// lifetime: Close closes every pool, is idempotent, and leaves the set empty.
func TestConnections_CloseReleasesEveryPool(t *testing.T) {
	dsn := newFakeDSN(t, brandDB())
	t.Setenv("APERTURE_TEST_DSN", dsn)
	t.Setenv("APERTURE_TEST_DSN_2", dsn)
	opener := newCountingOpener(dsn)

	doc := &Document{Connections: map[string]Connection{
		"main":  {DSNEnv: "APERTURE_TEST_DSN"},
		"other": {DSNEnv: "APERTURE_TEST_DSN_2"},
	}}
	_, conns, err := doc.BuildRegistryWithConnections("", WithConnectionOpener(opener.open))
	if err != nil {
		t.Fatalf("BuildRegistryWithConnections: %v", err)
	}
	if conns.Len() != 2 {
		t.Fatalf("Len() = %d, want 2", conns.Len())
	}
	if err := conns.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	assertPoolsClosed(t, opener.openedPools())
	if conns.Len() != 0 {
		t.Errorf("Len() after Close = %d, want 0", conns.Len())
	}
	if err := conns.Close(); err != nil {
		t.Errorf("second Close: %v, want nil (Close is idempotent)", err)
	}
}

// TestConnections_EmptyForDocumentWithoutConnections lets a caller defer
// unconditionally: a seed that declares no connections still yields a usable,
// closable set.
func TestConnections_EmptyForDocumentWithoutConnections(t *testing.T) {
	_, conns, err := (&Document{}).BuildRegistryWithConnections("")
	if err != nil {
		t.Fatalf("BuildRegistryWithConnections: %v", err)
	}
	if conns == nil {
		t.Fatal("Connections is nil; a caller cannot defer Close on it")
	}
	if conns.Len() != 0 || len(conns.Names()) != 0 {
		t.Errorf("expected an empty set, got %v", conns.Names())
	}
	if err := conns.Close(); err != nil {
		t.Errorf("Close on an empty set: %v", err)
	}
}

// TestBuildRegistry_RefusesConnectionsWithoutAnOwner: the one-return form cannot
// hand back a pool's lifetime, so it refuses a document that has one instead of
// opening pools nothing can close.
func TestBuildRegistry_RefusesConnectionsWithoutAnOwner(t *testing.T) {
	doc := &Document{Connections: map[string]Connection{"main": {DSNEnv: "APERTURE_TEST_DSN"}}}
	_, err := doc.BuildRegistry("")
	if aerr.CodeOf(err) != aerr.APERTURE_SQL_PROVIDER_CONNECTION {
		t.Fatalf("code = %s, want APERTURE_SQL_PROVIDER_CONNECTION", aerr.CodeOf(err))
	}
	if !strings.Contains(err.Error(), "BuildRegistryWithConnections") {
		t.Errorf("message does not name the method to use instead: %q", err.Error())
	}
	// A document with no connections: still builds through the old signature.
	if _, err := (&Document{}).BuildRegistry(""); err != nil {
		t.Fatalf("BuildRegistry on a connection-free document: %v", err)
	}
}

// TestBuildRegistry_ClosesPoolsWhenALaterConnectionFails: the second connection
// refuses, and the first one's pool must not survive the failed build.
func TestBuildRegistry_ClosesPoolsWhenALaterConnectionFails(t *testing.T) {
	dsn := newFakeDSN(t, brandDB())
	t.Setenv("APERTURE_TEST_DSN", dsn)
	opener := newCountingOpener(dsn)
	opener.failOn = "zzz-second"

	doc := &Document{Connections: map[string]Connection{
		"aaa-first":  {DSNEnv: "APERTURE_TEST_DSN"},
		"zzz-second": {DSNEnv: "APERTURE_TEST_DSN"},
	}}
	_, _, err := doc.BuildRegistryWithConnections("", WithConnectionOpener(opener.open))
	if err == nil {
		t.Fatal("build succeeded, want the opener's failure")
	}
	pools := opener.openedPools()
	if len(pools) != 1 {
		t.Fatalf("opener handed out %d pools, want 1 before the failure", len(pools))
	}
	assertPoolsClosed(t, pools)
}

// TestBuildRegistry_NoLeakAcrossBuildAndTeardown: repeated build/Close cycles
// must not accumulate goroutines. database/sql runs a connectionOpener goroutine
// per pool, so a pool that outlived its Connections would show up here.
func TestBuildRegistry_NoLeakAcrossBuildAndTeardown(t *testing.T) {
	dsn := newFakeDSN(t, brandDB())
	t.Setenv("APERTURE_TEST_DSN", dsn)

	doc := &Document{
		Connections: map[string]Connection{"main": {DSNEnv: "APERTURE_TEST_DSN"}},
		Providers: []Provider{
			{ObjectType: "brand", Kind: "sql", Connection: "main", GetOne: brandFetch, GetAll: brandList},
			{ObjectType: "app", Kind: "sql", Connection: "main", GetOne: appFetch, GetAll: appList},
		},
	}

	cycle := func() {
		opener := newCountingOpener(dsn)
		reg, conns, err := doc.BuildRegistryWithConnections("", WithConnectionOpener(opener.open))
		if err != nil {
			t.Fatalf("BuildRegistryWithConnections: %v", err)
		}
		if _, err := reg.Identifiers(context.Background(), "brand"); err != nil {
			t.Fatalf("Identifiers: %v", err)
		}
		if err := conns.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}

	cycle() // warm up: the first cycle allocates whatever database/sql caches
	settle(t)
	baseline := runtime.NumGoroutine()
	for i := 0; i < 20; i++ {
		cycle()
	}
	settle(t)
	if got := runtime.NumGoroutine(); got > baseline+2 {
		t.Errorf("goroutines grew from %d to %d across 20 build/teardown cycles", baseline, got)
	}
}

// settle waits for background goroutines (database/sql's connectionOpener and
// connectionCleaner) to finish exiting after a Close.
func settle(t *testing.T) {
	t.Helper()
	for i := 0; i < 50; i++ {
		runtime.Gosched()
		time.Sleep(2 * time.Millisecond)
	}
}

// assertPoolsClosed checks each pool refuses further use, which is how a
// *sql.DB reports that Close happened.
func assertPoolsClosed(t *testing.T, pools []*sql.DB) {
	t.Helper()
	for i, db := range pools {
		rows, err := db.QueryContext(context.Background(), brandList)
		if err == nil {
			_ = rows.Close()
			t.Errorf("pool %d still answers queries after teardown", i)
		}
		if open := db.Stats().OpenConnections; open != 0 {
			t.Errorf("pool %d has %d open connections after teardown", i, open)
		}
	}
}

// TestRedactDSN: a driver's error must never carry the credential back out. The
// whole reason a literal dsn: is forbidden is to keep the password out of
// committed and logged text, and an error message is logged text — net/url's own
// errors quote the entire URL, so this is not hypothetical.
func TestRedactDSN(t *testing.T) {
	cases := []struct {
		name   string
		msg    string
		dsn    string
		secret string
	}{
		{
			name:   "url form, whole dsn quoted",
			msg:    `parse "postgres://app:s3cr3t@db.internal/prod": invalid port`,
			dsn:    "postgres://app:s3cr3t@db.internal/prod",
			secret: "s3cr3t",
		},
		{
			name:   "url form, only the password quoted",
			msg:    "authentication failed for password s3cr3t",
			dsn:    "postgres://app:s3cr3t@db.internal/prod",
			secret: "s3cr3t",
		},
		{
			name:   "keyword form",
			msg:    "cannot parse `host=db user=app password=s3cr3t`",
			dsn:    "host=db user=app password=s3cr3t",
			secret: "s3cr3t",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := redactDSN(tc.msg, tc.dsn)
			if strings.Contains(got, tc.secret) {
				t.Errorf("redactDSN left the password in: %q", got)
			}
			if strings.Contains(got, tc.dsn) {
				t.Errorf("redactDSN left the DSN in: %q", got)
			}
			if got == "" {
				t.Error("redactDSN erased the whole message; the operator needs something")
			}
		})
	}
	if got := redactDSN("some unrelated failure", ""); got != "some unrelated failure" {
		t.Errorf("an empty DSN must not rewrite the message, got %q", got)
	}
}

// TestSQLWiringIsNotModelState guards the rule the seed's wiring sections all
// share: Apply writes none of it to storage. A connections: block carrying a
// DSN is the one it would hurt most to break.
func TestSQLWiringIsNotModelState(t *testing.T) {
	doc, err := Parse([]byte(`
accounts:
  - {id: acme, name: Acme}
connections:
  main:
    dsn_env: APERTURE_TEST_DSN
providers:
  - object_type: brand
    kind: sql
    connection: main
    get_one: SELECT 1
    get_all: SELECT 1
`), FormatYAML)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	store := memory.New()
	if err := doc.Apply(context.Background(), store); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	out, err := Export(context.Background(), store)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if len(out.Connections) != 0 {
		t.Errorf("export reproduced the connections: block: %+v", out.Connections)
	}
	if len(out.Providers) != 0 {
		t.Errorf("export reproduced the providers: block: %+v", out.Providers)
	}
}
