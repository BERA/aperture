// Package sqlprovider implements provider.ObjectProvider over a relational
// database, so a host serves its real objects to Aperture from the tables it
// already has instead of exporting them to a CSV. It is a drop-in sibling of
// csvprovider: register a *Provider under an object-type in a
// provider.Registry exactly as the CSV loader is registered, and the Registry's
// cache, invalidation, and rules wiring are unchanged.
//
//	db, _ := sql.Open("pgx", os.Getenv("DATABASE_URL"))
//	brands, err := sqlprovider.New(db, sqlprovider.Config{
//		FetchQuery: `SELECT tier, seats, active FROM brands WHERE id = $1`,
//	})
//	if err != nil { return err }
//	reg.MustRegister("brand", brands)
//
// # The Querier seam
//
// The dependency is the narrow Querier interface below, never a concrete
// *sql.DB. A *sql.DB satisfies it, and so does an *sqlx.DB, a pgx stdlib
// handle, a *sql.Conn, a *sql.Tx, or a host's own tracing/retrying wrapper —
// anything with database/sql's two read methods. Aperture therefore owns no
// connection lifecycle here: it does not open, close, ping, pool, or configure
// anything. The host builds the handle it wants, with the driver, pool sizes,
// TLS, and observability it already runs, and hands it over.
//
// This is also why the package imports NO driver. A driver is a host choice;
// linking one in would force every Aperture binary to carry it.
//
// Note that this is a HOST data source. It is unrelated to Aperture's own
// storage (storage/, hand-written SQL on modernc.org/sqlite) and shares no
// connection handling with it: provider data is pulled, read-only, and never
// persisted as source of truth.
//
// # What Fetch binds
//
// A fetch statement takes exactly ONE parameter, and Aperture binds the
// identity's TERMINAL SEGMENT VALUE to it — not the identity string. Fetching
// "brand:42" and "account:acme/brand:42" both bind "42":
//
//	Fetch("account:acme/brand:42")  →  QueryContext(stmt, "42")
//
// That is what lets the developer write `WHERE b.id = $1` against the primary
// key they already have, and hit its index. Full Aperture identities are rarely
// a column in a host's schema, and a statement that had to reconstruct one per
// row could not use an index at all.
//
// Placeholders are ENGINE-NATIVE and passed through untouched: Aperture never
// rewrites "$1" to "?" or vice versa. Write the placeholder syntax your database
// speaks — "$1" for Postgres, "?" for MySQL or SQLite — and it reaches the
// driver exactly as written. There is no dialect layer to be surprised by.
//
// Parameters are always BOUND, never interpolated. This is the SQL-injection
// boundary of the whole feature and it is not configurable: an object id
// originates outside the process, so a provider that pasted it into a statement
// would hand every caller of Check arbitrary SQL execution. There is no API here
// that builds a statement from a value.
//
// # Columns become metadata
//
// Every column the fetch statement returns becomes a metadata field keyed by its
// COLUMN NAME, so the SELECT list is the field list and the developer controls
// it in SQL:
//
//	SELECT tier, seats, active FROM brands WHERE id = $1
//	→ {"tier": "gold", "seats": int64(5), "active": true}
//
// A column therefore needs a name: an unnamed expression (`SELECT count(*)`) or
// two columns with the same name (a `SELECT b.*, o.*` over two tables that both
// have "name") is an APERTURE_SQL_PROVIDER_SCAN error naming the column, not a
// field silently dropped or overwritten by whichever came last.
//
// A NULL column OMITS its field rather than storing nil, which is csvprovider's
// absent-vs-zero rule applied to SQL: an absent field never matches a
// Filter.Fields predicate and a rule can supply its own default, whereas a
// stored nil is a value that compares — and a zero that compares is how a NULL
// end_date silently satisfies every "before" rule ever written against it.
//
// # Driver values become metadata
//
// What a column becomes is decided by the GO TYPE database/sql scans it into,
// and that mapping is a closed table:
//
//	NULL       →  the field is OMITTED (absent is not zero)
//	bool       →  the scalar, as-is
//	int64      →  the scalar, as-is
//	float64    →  the scalar, as-is
//	string     →  the scalar, as-is
//	[]byte     →  JSON-DECODED (this is how arrays and nested objects arrive)
//	time.Time  →  converted to UTC, then the canonical "2006-01-02T15:04:05Z"
//	anything else → APERTURE_SQL_PROVIDER_SCAN naming the column and the type
//
// Every mapped row is then checked against the shared metadata value model
// (provider.ValidateMetadata) before it is returned, so a shape the expression
// evaluator cannot handle fails the fetch instead of surfacing as an evaluation
// error on the Check hot path. See values.go for the rule-by-rule reasoning; the
// three consequences a developer has to act on are these.
//
// CAST IT IN THE STATEMENT. The statement is the only typing mechanism there is.
// There is no per-column type declaration to write in YAML, because two
// spellings for one intent drift apart, and the statement is the one the
// developer is already writing. So:
//
//	to_jsonb(tags) AS tags     an array — the ONLY way one arrives
//	hired_on::text AS hired_on a day-granular date ("2026-03-04")
//	amount::float8 AS amount   a numeric meant as a number
//	sku::text AS sku           a numeric or uuid meant as an identifier
//
// A []byte IS JSON, unconditionally. It does not fall back to a string. That is
// deliberate: a fallback would let one column change TYPE depending on its
// contents. A bytea therefore does not work — encode it in the statement, or
// leave it out, since an access-control decision has no business reading a blob.
// A JSON null omits its field, exactly as a SQL NULL does.
//
// A time.Time is ALWAYS a timestamp, never a day. A database date and a database
// timestamp are the same Go type, so granularity cannot be inferred; the
// datetime form is the one that loses nothing. Write `col::text` for a day.
//
// # The trap this package cannot catch for you
//
// Selecting an array column WITHOUT casting it compiles, runs, and is wrong:
//
//	SELECT tags FROM brands WHERE id = $1        -- WRONG
//
// A Postgres text[] does not arrive as a list. It arrives as the raw array
// literal, the six-character string "{a,b}" — a perfectly valid metadata string,
// indistinguishable from a string a host meant to store. Nothing in this package
// can tell the two apart, so nothing here will complain. What happens instead is
// that every membership predicate written against that field silently matches
// nothing, forever, and the rule reads as though it simply never applies.
//
// The fix is one function call in the SELECT list:
//
//	SELECT to_jsonb(tags) AS tags FROM brands WHERE id = $1   -- RIGHT
//
// If a list-valued field is matching nothing, check its cast first.
//
// # Absent, ambiguous, and broken
//
// The three outcomes a fetch can have are three different errors, deliberately:
//
//   - ZERO rows is APERTURE_NOT_FOUND — the documented ObjectProvider contract,
//     so the Registry can distinguish an absent object from an operational
//     failure. It is the only one of the three that is a normal, expected
//     answer.
//   - MORE THAN ONE row is APERTURE_SQL_PROVIDER_AMBIGUOUS, naming the identity.
//     The first row is NOT silently taken. Without an ORDER BY, which row a
//     database hands back first is unspecified, so taking one would make an
//     object's metadata — and every decision computed from it — vary between two
//     identical Checks. The usual cause is a join that fans out; the fix is in
//     the statement, not in a LIMIT 1 that would only hide it.
//   - A DRIVER or connection failure is APERTURE_SQL_PROVIDER_QUERY with the
//     driver's error wrapped verbatim (reachable with errors.Is / errors.As).
//     An error that already carries an APERTURE_* code — a coded error raised by
//     a host's wrapping Querier — passes through untouched; wrappers never
//     re-stamp a code.
//
// # Timeouts
//
// Every statement runs under context.WithTimeout, DefaultTimeout (5s) unless
// Config.Timeout says otherwise. Fetch sits under Check, which owes a p99 under
// a millisecond, so an unbounded query against a host database is not a slow
// decision — it is a decision that never returns, holding a connection while it
// does not. The deadline applies on top of whatever the caller's own context
// already carries, so the earlier of the two wins and a caller can always be
// stricter than the provider.
//
// # Concurrency and the read-only contract
//
// A *Provider is immutable after New — it holds a Querier and two configured
// values and mutates nothing — so it is safe for concurrent use, as the
// ObjectProvider contract requires. Concurrency beneath it is the Querier's
// business, and *sql.DB is itself concurrency-safe.
//
// Per the provider.Metadata contract every returned map is freshly allocated for
// that one Fetch, with no container shared with another call or retained by the
// provider, and must be treated as read-only by its holder.
//
// Dependencies stay minimal: sqlprovider imports database/sql from the standard
// library plus errors, identity, and provider — no driver, and no CGO.
package sqlprovider

import (
	"context"
	"database/sql"
	"strings"
	"time"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/identity"
	"github.com/frankbardon/aperture/provider"
)

// compile-time assertion: a *Provider is a usable ObjectProvider.
var _ provider.ObjectProvider = (*Provider)(nil)

// DefaultTimeout bounds one statement when Config.Timeout is zero. Fetch runs
// under Check, so the default is short on purpose: a provider query that has not
// answered in five seconds has already failed the decision it was serving.
const DefaultTimeout = 5 * time.Second

// Querier is the seam this provider depends on: database/sql's two read methods
// and nothing else. It is deliberately narrower than *sql.DB so a host can hand
// over whatever handle it already owns — a *sql.DB or *sql.Conn, an *sqlx.DB, a
// pgx stdlib handle, a *sql.Tx, or its own tracing, retrying, or read-replica
// wrapper — without Aperture taking over connection lifecycle or pooling.
//
// The context passed to either method already carries this provider's timeout;
// an implementation must honour it rather than starting its own unbounded work.
type Querier interface {
	// QueryContext runs query with args bound as parameters and returns its rows.
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	// QueryRowContext runs a query expected to return at most one row. Fetch does
	// not use it — it must be able to SEE a second row to reject it, and
	// QueryRowContext discards one — but it is part of the seam so a host can
	// satisfy the interface with a handle it already has and so later single-row
	// reads need no second interface.
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Config is the developer-supplied wiring for one object-type's provider.
type Config struct {
	// FetchQuery is the "get one" statement. It takes exactly one placeholder,
	// written in the database's own syntax and passed through untouched, to which
	// Fetch binds the identity's terminal segment value:
	//
	//	SELECT tier, seats FROM brands WHERE id = $1
	//
	// Every column it returns becomes a metadata field keyed by the column name,
	// so the SELECT list is the field list. Required.
	FetchQuery string
	// Timeout bounds one statement, applied with context.WithTimeout on top of
	// the caller's context. Zero means DefaultTimeout; negative is a
	// configuration error. There is no "no timeout" setting.
	Timeout time.Duration
}

// Provider is a SQL-backed ObjectProvider for one object-type. It is immutable
// after New and therefore safe for concurrent use; the Querier owns whatever
// pooling happens beneath it.
type Provider struct {
	q          Querier
	fetchQuery string
	timeout    time.Duration
}

// New returns a Provider that reads objects through q using cfg's statements.
// Register it under the object-type whose instances the statement describes.
//
// Unlike csvprovider.New — which defers to first use because the file it names
// may not exist yet — this validates now: a nil Querier, a blank FetchQuery, or
// a negative Timeout is a wiring mistake that would otherwise first surface as a
// failed decision, and an access-control engine should not learn about its own
// misconfiguration from a denied Check.
func New(q Querier, cfg Config) (*Provider, error) {
	if q == nil {
		return nil, aerr.New(aerr.APERTURE_CONFIG_INVALID,
			"sqlprovider: nil Querier; pass a *sql.DB or another handle with QueryContext and QueryRowContext")
	}
	stmt := strings.TrimSpace(cfg.FetchQuery)
	if stmt == "" {
		return nil, aerr.New(aerr.APERTURE_CONFIG_INVALID,
			"sqlprovider: Config.FetchQuery is required; it is the statement that fetches one object by its terminal segment value")
	}
	if cfg.Timeout < 0 {
		return nil, aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
			"sqlprovider: Config.Timeout cannot be negative; leave it zero for the default",
			map[string]any{"timeout": cfg.Timeout.String(), "default": DefaultTimeout.String()})
	}
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	return &Provider{q: q, fetchQuery: stmt, timeout: timeout}, nil
}

// Fetch returns id's metadata by running the configured fetch statement with
// id's TERMINAL SEGMENT VALUE bound as its single parameter ("brand:42" and
// "account:acme/brand:42" both bind "42").
//
// No rows yields APERTURE_NOT_FOUND, so the Registry can distinguish an absent
// object from an operational failure. More than one row yields
// APERTURE_SQL_PROVIDER_AMBIGUOUS naming the identity — the first row is never
// silently taken, because which row that is would be unspecified. A driver
// failure yields APERTURE_SQL_PROVIDER_QUERY wrapping the cause, unless the
// cause already carries an APERTURE_* code, which passes through verbatim.
//
// The returned map is freshly allocated for this call and is read-only to its
// holder, per the provider.Metadata contract.
func (p *Provider) Fetch(ctx context.Context, id identity.Identity) (provider.Metadata, error) {
	key, err := terminalValue(id)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	rows, err := p.q.QueryContext(ctx, p.fetchQuery, key)
	if err != nil {
		return nil, queryError(err, id)
	}
	defer rows.Close()

	if !rows.Next() {
		// rows.Err first: a connection that died mid-statement also reports "no
		// next row", and reporting that as NOT_FOUND would turn a broken database
		// into a confident "this object does not exist" — and, one layer up, into
		// a deny that looks deliberate.
		if err := rows.Err(); err != nil {
			return nil, queryError(err, id)
		}
		return nil, aerr.WithContext(aerr.APERTURE_NOT_FOUND,
			"sqlprovider: no object with this id", map[string]any{"id": id.String()})
	}

	md, err := scanRow(rows, id)
	if err != nil {
		return nil, err
	}

	// A second row is checked BEFORE the metadata is returned, never after it is
	// used: an ambiguous fetch is a broken statement, not a fetch with a bonus.
	if rows.Next() {
		return nil, aerr.WithContext(aerr.APERTURE_SQL_PROVIDER_AMBIGUOUS,
			"sqlprovider: fetch statement returned more than one row for one identity; "+
				"filter on a unique key rather than adding LIMIT 1, which would make the metadata depend on an unspecified row order",
			map[string]any{"id": id.String()})
	}
	if err := rows.Err(); err != nil {
		return nil, queryError(err, id)
	}
	return md, nil
}

// List returns the objects of this provider's type.
//
// Enumeration is E2-S3: it needs a second configured statement and an id column
// to build each identity from, neither of which exists yet. It reports
// APERTURE_UNIMPLEMENTED rather than an empty slice, because an empty
// enumeration reads as "no objects" — and, through scope resolution, as "no
// access" — which would hide the gap instead of naming it.
func (p *Provider) List(_ context.Context) ([]provider.Object, error) {
	return nil, aerr.New(aerr.APERTURE_UNIMPLEMENTED,
		"sqlprovider: List is not implemented yet; this provider currently serves Fetch only")
}

// Query returns the objects of this provider's type that satisfy filter. Like
// List it lands in E2-S3, and reports APERTURE_UNIMPLEMENTED for the same
// reason: a silently empty filtered enumeration is indistinguishable from a
// denial.
func (p *Provider) Query(_ context.Context, _ provider.Filter) ([]provider.Object, error) {
	return nil, aerr.New(aerr.APERTURE_UNIMPLEMENTED,
		"sqlprovider: Query is not implemented yet; this provider currently serves Fetch only")
}

// terminalValue extracts the value Fetch binds: the id part of the identity's
// terminal segment. An empty identity has no terminal segment and so nothing to
// bind; it is rejected here rather than binding "" and letting the database
// answer "no rows", which would report a caller's bug as a missing object.
func terminalValue(id identity.Identity) (string, error) {
	segs := id.Segments()
	if len(segs) == 0 {
		return "", aerr.New(aerr.APERTURE_IDENTITY_INVALID,
			"sqlprovider: cannot fetch an empty identity; it has no terminal segment to bind")
	}
	return segs[len(segs)-1].ID, nil
}

// queryError normalises a database failure. An error already carrying an
// APERTURE_* code passes through verbatim — a host's wrapping Querier may raise
// one, and re-stamping it would bury the classification the host chose —
// while a plain driver error is wrapped as APERTURE_SQL_PROVIDER_QUERY with the
// cause reachable through errors.Is / errors.As.
//
// The message names the identity, which is the caller's own input, and never the
// row values, which are host data belonging to some account.
func queryError(err error, id identity.Identity) error {
	if aerr.CodeOf(err) != "" {
		return err
	}
	return aerr.Wrapf(aerr.APERTURE_SQL_PROVIDER_QUERY, err,
		"sqlprovider: fetch statement failed for %s", id.String())
}

// scanRow turns the row rows is positioned on into a fresh Metadata, keyed by
// result column name.
//
// The destination slice is allocated per call — a Provider retains nothing
// between fetches — which is what keeps the read-only Metadata contract true for
// concurrent callers.
func scanRow(rows *sql.Rows, id identity.Identity) (provider.Metadata, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, aerr.Wrapf(aerr.APERTURE_SQL_PROVIDER_SCAN, err,
			"sqlprovider: cannot read the result columns for %s", id.String())
	}

	vals := make([]any, len(cols))
	dest := make([]any, len(cols))
	for i := range vals {
		dest[i] = &vals[i]
	}
	if err := rows.Scan(dest...); err != nil {
		return nil, aerr.Wrapf(aerr.APERTURE_SQL_PROVIDER_SCAN, err,
			"sqlprovider: cannot scan the row for %s", id.String())
	}

	md := make(provider.Metadata, len(cols))
	seen := make(map[string]bool, len(cols))
	for i, name := range cols {
		if name == "" {
			return nil, aerr.WithContext(aerr.APERTURE_SQL_PROVIDER_SCAN,
				"sqlprovider: result column has no name; alias every selected expression, because a column name is the metadata field key",
				map[string]any{"id": id.String(), "index": i})
		}
		if seen[name] {
			// Last-wins would make the field's value depend on the SELECT list's
			// order, which is exactly the kind of invisible dependency that turns
			// a schema change into a changed decision.
			return nil, aerr.WithContext(aerr.APERTURE_SQL_PROVIDER_SCAN,
				"sqlprovider: result has two columns with the same name; alias one, because a column name is the metadata field key",
				map[string]any{"id": id.String(), "column": name})
		}
		seen[name] = true

		// values.go owns the driver-value mapping table and builds its own coded
		// errors, so every rejection already names the column and this row's
		// identity.
		v, present, err := metadataValue(vals[i], name, id)
		if err != nil {
			return nil, err
		}
		if !present {
			continue // a NULL — or a JSON null — omits the field
		}
		md[name] = v
	}

	// The shared value model is the single authority on what a metadata value may
	// be; the loader never re-implements those rules. A violation fails the fetch
	// rather than reaching the expression evaluator.
	if err := provider.ValidateMetadata(md); err != nil {
		return nil, aerr.Wrapf(aerr.APERTURE_METADATA_INVALID, err,
			"sqlprovider: row for %s rejected by the metadata value model", id.String())
	}
	return md, nil
}
