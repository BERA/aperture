package sqlprovider

import (
	"context"
	"database/sql"
	"database/sql/driver"
	stderrors "errors"
	"reflect"
	"sync"
	"testing"
	"time"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/identity"
	"github.com/frankbardon/aperture/provider"
)

// The Querier seam must be satisfiable by the handles a host actually owns,
// without any of them being named in the interface. These assertions are the
// documentation for that claim.
var (
	_ Querier = (*sql.DB)(nil)
	_ Querier = (*sql.Conn)(nil)
	_ Querier = (*sql.Tx)(nil)
)

const stmt = `SELECT tier, seats FROM brands WHERE id = $1`

// newProvider wires a Provider onto a fake database with the given script.
func newProvider(t *testing.T, s *script, cfg Config) *Provider {
	t.Helper()
	if cfg.FetchQuery == "" {
		cfg.FetchQuery = stmt
	}
	p, err := New(newFakeDB(t, s), cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func mustCode(t *testing.T, err error, want aerr.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("want error with code %s, got nil", want)
	}
	if got := aerr.CodeOf(err); got != want {
		t.Fatalf("code = %q, want %s (err: %v)", got, want, err)
	}
}

func TestNewRejectsMisconfiguration(t *testing.T) {
	db := newFakeDB(t, &script{})
	cases := []struct {
		name string
		q    Querier
		cfg  Config
	}{
		{"nil querier", nil, Config{FetchQuery: stmt}},
		{"blank fetch query", db, Config{}},
		{"whitespace fetch query", db, Config{FetchQuery: "   \n\t "}},
		{"negative timeout", db, Config{FetchQuery: stmt, Timeout: -time.Second}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := New(tc.q, tc.cfg)
			if p != nil {
				t.Fatalf("New returned a provider for an invalid config")
			}
			mustCode(t, err, aerr.APERTURE_CONFIG_INVALID)
		})
	}
}

func TestNewAppliesTheDefaultTimeout(t *testing.T) {
	p := newProvider(t, &script{}, Config{})
	if p.timeout != DefaultTimeout {
		t.Fatalf("timeout = %v, want the %v default", p.timeout, DefaultTimeout)
	}
	p = newProvider(t, &script{}, Config{Timeout: 250 * time.Millisecond})
	if p.timeout != 250*time.Millisecond {
		t.Fatalf("timeout = %v, want the configured 250ms", p.timeout)
	}
}

// Fetch binds the identity's TERMINAL SEGMENT VALUE, never the identity string:
// that is what lets a developer write WHERE b.id = $1 against an indexed primary
// key, and full identities are rarely a column in a host's schema.
func TestFetchBindsTheTerminalSegmentValue(t *testing.T) {
	cases := []struct {
		id   string
		want string
	}{
		{"brand:42", "42"},
		{"account:acme/brand:42", "42"},
		{"account:acme/project:atlas/document:7", "7"},
		{"brand:a-b_c.d", "a-b_c.d"},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			s := &script{cols: []string{"tier"}, rows: [][]driver.Value{{"gold"}}}
			p := newProvider(t, s, Config{})
			if _, err := p.Fetch(context.Background(), identity.MustParse(tc.id)); err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			_, _, args := s.observed()
			if len(args) != 1 {
				t.Fatalf("bound %d args, want exactly 1: %v", len(args), args)
			}
			if got, ok := args[0].(string); !ok || got != tc.want {
				t.Fatalf("bound %#v, want the terminal segment value %q", args[0], tc.want)
			}
		})
	}
}

// Placeholders are engine-native and passed through untouched — Aperture never
// rewrites $1 to ? or the reverse, and never interpolates a parameter into the
// statement text.
func TestFetchPassesTheStatementThroughUntouched(t *testing.T) {
	for _, q := range []string{
		`SELECT tier FROM brands WHERE id = $1`,
		`SELECT tier FROM brands WHERE id = ?`,
		`SELECT tier FROM brands WHERE id = :1`,
	} {
		t.Run(q, func(t *testing.T) {
			s := &script{cols: []string{"tier"}, rows: [][]driver.Value{{"gold"}}}
			p := newProvider(t, s, Config{FetchQuery: q})
			if _, err := p.Fetch(context.Background(), identity.MustParse("brand:42")); err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			_, got, args := s.observed()
			if got != q {
				t.Fatalf("statement = %q, want it passed through as %q", got, q)
			}
			// And the value never reached the statement text.
			if len(args) != 1 || args[0] != "42" {
				t.Fatalf("args = %#v, want the value bound, never interpolated", args)
			}
		})
	}
}

// Every returned column becomes a metadata field keyed by its column name; a
// NULL omits the field rather than storing nil (csvprovider's absent-vs-zero
// rule), and a timestamp is canonicalised to the shared date value model's UTC
// layout so two rows naming one instant are one string.
func TestFetchMapsColumnsToMetadata(t *testing.T) {
	s := &script{
		cols: []string{"tier", "seats", "budget", "active", "slug", "hired_at", "note"},
		rows: [][]driver.Value{{
			"gold",
			int64(5),
			float64(2.5),
			true,
			[]byte("acme"),
			time.Date(2026, 3, 4, 12, 30, 0, 0, time.FixedZone("east", 5*3600)),
			nil,
		}},
	}
	p := newProvider(t, s, Config{})
	md, err := p.Fetch(context.Background(), identity.MustParse("brand:42"))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	want := provider.Metadata{
		"tier":     "gold",
		"seats":    int64(5),
		"budget":   float64(2.5),
		"active":   true,
		"slug":     "acme",
		"hired_at": "2026-03-04T07:30:00Z", // converted to UTC, canonical layout
	}
	if !reflect.DeepEqual(md, want) {
		t.Fatalf("metadata = %#v, want %#v", md, want)
	}
	if _, present := md["note"]; present {
		t.Fatalf("a NULL column produced a field; it must be absent, not nil")
	}
	// A mapped row must satisfy the shared value model.
	if err := provider.ValidateMetadata(md); err != nil {
		t.Fatalf("mapped metadata violates the value model: %v", err)
	}
}

// Zero rows is APERTURE_NOT_FOUND — the documented ObjectProvider contract, so
// the Registry can tell an absent object from a broken database.
func TestFetchZeroRowsIsNotFound(t *testing.T) {
	s := &script{cols: []string{"tier"}}
	p := newProvider(t, s, Config{})
	_, err := p.Fetch(context.Background(), identity.MustParse("brand:42"))
	mustCode(t, err, aerr.APERTURE_NOT_FOUND)
}

// More than one row is a hard error naming the identity. The first row is never
// silently taken: which row that is would be unspecified without an ORDER BY, so
// the object's metadata — and the decision made from it — would vary between two
// identical Checks.
func TestFetchMultipleRowsIsAmbiguous(t *testing.T) {
	s := &script{
		cols: []string{"tier"},
		rows: [][]driver.Value{{"gold"}, {"silver"}},
	}
	p := newProvider(t, s, Config{})
	md, err := p.Fetch(context.Background(), identity.MustParse("account:acme/brand:42"))
	mustCode(t, err, aerr.APERTURE_SQL_PROVIDER_AMBIGUOUS)
	if md != nil {
		t.Fatalf("metadata = %#v, want nil: the first row must not be taken", md)
	}
	var ce *aerr.CodedError
	if !stderrors.As(err, &ce) {
		t.Fatalf("error is not a *CodedError: %v", err)
	}
	if got := ce.Context["id"]; got != "account:acme/brand:42" {
		t.Fatalf("error context id = %v, want the identity named", got)
	}
}

// A driver or connection failure is an operational error, wrapped so the cause
// stays reachable — never NOT_FOUND, which would report a broken database as a
// confident "this object does not exist".
func TestFetchDriverErrorIsCodedAndWrapsTheCause(t *testing.T) {
	boom := stderrors.New("connection refused")
	s := &script{cols: []string{"tier"}, queryErr: boom}
	p := newProvider(t, s, Config{})
	_, err := p.Fetch(context.Background(), identity.MustParse("brand:42"))
	mustCode(t, err, aerr.APERTURE_SQL_PROVIDER_QUERY)
	if !stderrors.Is(err, boom) {
		t.Fatalf("driver cause is not reachable through errors.Is: %v", err)
	}
}

// An error already carrying an APERTURE_* code passes through verbatim; wrappers
// never re-stamp it.
func TestFetchCodedDriverErrorPassesThroughVerbatim(t *testing.T) {
	coded := aerr.New(aerr.APERTURE_STORAGE, "host wrapper: replica unavailable")
	s := &script{cols: []string{"tier"}, queryErr: coded}
	p := newProvider(t, s, Config{})
	_, err := p.Fetch(context.Background(), identity.MustParse("brand:42"))
	mustCode(t, err, aerr.APERTURE_STORAGE)
	if !stderrors.Is(err, coded) {
		t.Fatalf("coded error was not passed through verbatim: %v", err)
	}
}

// A connection that dies mid-stream also reports "no next row". Reporting that
// as NOT_FOUND would turn a broken database into a deny that looks deliberate.
func TestFetchMidStreamFailureIsNotMistakenForNotFound(t *testing.T) {
	boom := stderrors.New("connection reset by peer")
	s := &script{cols: []string{"tier"}, rowsErr: boom}
	p := newProvider(t, s, Config{})
	_, err := p.Fetch(context.Background(), identity.MustParse("brand:42"))
	mustCode(t, err, aerr.APERTURE_SQL_PROVIDER_QUERY)
	if !stderrors.Is(err, boom) {
		t.Fatalf("cause is not reachable through errors.Is: %v", err)
	}
}

// The per-query timeout reaches the driver as a real context deadline.
func TestFetchAppliesATimeoutToTheStatement(t *testing.T) {
	s := &script{cols: []string{"tier"}, rows: [][]driver.Value{{"gold"}}}
	p := newProvider(t, s, Config{})
	before := time.Now()
	if _, err := p.Fetch(context.Background(), identity.MustParse("brand:42")); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	dl, ok := s.deadline()
	if !ok {
		t.Fatalf("the statement ran with no deadline; the default timeout was not applied")
	}
	if d := dl.Sub(before); d <= 0 || d > DefaultTimeout+time.Second {
		t.Fatalf("deadline is %v out, want ~%v (the default)", d, DefaultTimeout)
	}
}

func TestFetchTimeoutIsOverridableAndFires(t *testing.T) {
	s := &script{
		cols:  []string{"tier"},
		rows:  [][]driver.Value{{"gold"}},
		delay: 2 * time.Second,
	}
	p := newProvider(t, s, Config{Timeout: 20 * time.Millisecond})
	start := time.Now()
	_, err := p.Fetch(context.Background(), identity.MustParse("brand:42"))
	mustCode(t, err, aerr.APERTURE_SQL_PROVIDER_QUERY)
	if !stderrors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want a deadline-exceeded cause, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Fetch took %v; the configured 20ms timeout did not bound the statement", elapsed)
	}
}

// A caller's own context stays authoritative: the earlier deadline wins.
func TestFetchHonoursACallerDeadlineStricterThanItsOwn(t *testing.T) {
	s := &script{cols: []string{"tier"}, rows: [][]driver.Value{{"gold"}}, delay: 2 * time.Second}
	p := newProvider(t, s, Config{Timeout: time.Minute})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := p.Fetch(ctx, identity.MustParse("brand:42"))
	if err == nil {
		t.Fatalf("want an error from the caller's expired deadline")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Fetch took %v; the caller's 20ms deadline was not honoured", elapsed)
	}
}

// A column name is the metadata field key, so an unnamed or duplicated column is
// an error rather than a field silently dropped or overwritten by whichever the
// SELECT list happened to put last.
func TestFetchRejectsUnusableColumnNames(t *testing.T) {
	cases := []struct {
		name string
		cols []string
		row  []driver.Value
	}{
		{"unnamed column", []string{"tier", ""}, []driver.Value{"gold", int64(1)}},
		{"duplicate column", []string{"name", "name"}, []driver.Value{"acme", "atlas"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &script{cols: tc.cols, rows: [][]driver.Value{tc.row}}
			p := newProvider(t, s, Config{})
			_, err := p.Fetch(context.Background(), identity.MustParse("brand:42"))
			mustCode(t, err, aerr.APERTURE_SQL_PROVIDER_SCAN)
		})
	}
}

// A driver value of an unmapped Go type is refused by name rather than stored as
// something the expression evaluator would silently mis-compare. The full
// driver-value mapping table is E2-S2.
func TestFetchRejectsAnUnmappedDriverValue(t *testing.T) {
	type opaque struct{ N int }
	s := &script{
		cols: []string{"tier", "weird"},
		rows: [][]driver.Value{{"gold", opaque{N: 1}}},
	}
	p := newProvider(t, s, Config{})
	_, err := p.Fetch(context.Background(), identity.MustParse("brand:42"))
	mustCode(t, err, aerr.APERTURE_SQL_PROVIDER_SCAN)

	var ce *aerr.CodedError
	if !stderrors.As(err, &ce) {
		t.Fatalf("error is not a *CodedError: %v", err)
	}
	if got := ce.Context["column"]; got != "weird" {
		t.Fatalf("error context column = %v, want the offending column named", got)
	}
	// The TYPE is named; the value — host data — is not.
	if got, _ := ce.Context["type"].(string); got == "" {
		t.Fatalf("error context type is empty, want the Go type named")
	}
}

// An empty identity has no terminal segment, so there is nothing to bind. That
// is a caller bug, and binding "" would let the database report it as a missing
// object instead.
func TestFetchRejectsAnEmptyIdentity(t *testing.T) {
	s := &script{cols: []string{"tier"}}
	p := newProvider(t, s, Config{})
	_, err := p.Fetch(context.Background(), identity.Identity{})
	mustCode(t, err, aerr.APERTURE_IDENTITY_INVALID)
	if calls, _, _ := s.observed(); calls != 0 {
		t.Fatalf("the statement ran %d times for an empty identity, want 0", calls)
	}
}

// Every Fetch returns a freshly allocated map, per the read-only Metadata
// contract: a provider never hands out a value it also retains.
func TestFetchReturnsAFreshMapPerCall(t *testing.T) {
	s := &script{cols: []string{"tier"}, rows: [][]driver.Value{{"gold"}}}
	p := newProvider(t, s, Config{})
	id := identity.MustParse("brand:42")

	first, err := p.Fetch(context.Background(), id)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	first["injected"] = true

	second, err := p.Fetch(context.Background(), id)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if _, leaked := second["injected"]; leaked {
		t.Fatalf("a second Fetch saw a write to the first's map; the provider is sharing state")
	}
}

// The ObjectProvider contract requires concurrency safety. Run with -race.
func TestFetchIsSafeForConcurrentUse(t *testing.T) {
	s := &script{cols: []string{"tier", "seats"}, rows: [][]driver.Value{{"gold", int64(5)}}}
	p := newProvider(t, s, Config{})
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			md, err := p.Fetch(context.Background(), identity.MustParse("brand:42"))
			if err != nil {
				t.Errorf("Fetch: %v", err)
				return
			}
			if md["tier"] != "gold" || md["seats"] != int64(5) {
				t.Errorf("metadata = %#v", md)
			}
		}()
	}
	wg.Wait()
}

// List and Query land in E2-S3. Until then they report a code rather than an
// empty slice: an empty enumeration reads as "no access" and would hide the gap.
func TestListAndQueryAreNotImplementedYet(t *testing.T) {
	p := newProvider(t, &script{}, Config{})
	_, err := p.List(context.Background())
	mustCode(t, err, aerr.APERTURE_UNIMPLEMENTED)
	_, err = p.Query(context.Background(), provider.Filter{})
	mustCode(t, err, aerr.APERTURE_UNIMPLEMENTED)
}

// A *Provider is registrable exactly as csvprovider's is, which is the whole
// point of the drop-in claim.
func TestProviderRegistersInAProviderRegistry(t *testing.T) {
	s := &script{cols: []string{"tier"}, rows: [][]driver.Value{{"gold"}}}
	p := newProvider(t, s, Config{})
	reg := provider.NewRegistry()
	reg.MustRegister("brand", p)
	md, err := reg.Fetch(context.Background(), identity.MustParse("brand:42"))
	if err != nil {
		t.Fatalf("Registry.Fetch: %v", err)
	}
	if md["tier"] != "gold" {
		t.Fatalf("metadata = %#v", md)
	}
	// And an absent object still surfaces as NOT_FOUND through the Registry.
	empty := newProvider(t, &script{cols: []string{"tier"}}, Config{})
	reg2 := provider.NewRegistry()
	reg2.MustRegister("brand", empty)
	_, err = reg2.Fetch(context.Background(), identity.MustParse("brand:99"))
	mustCode(t, err, aerr.APERTURE_NOT_FOUND)
}
