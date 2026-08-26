package sqlprovider

import (
	"context"
	"strings"
	"time"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/provider"
)

// The attribute seam, over the same database.
//
// Attributes is *Provider's counterpart for provider.AttributeProvider: two
// statements, the same Querier, the same driver-value mapping table, the same
// value model, serving one attribute SLOT instead of one object-type. It is what
// lets a host point Aperture at the `users` and `accounts` tables it already has
// and have a rule read `principal.department` with no Go written:
//
//	connections:
//	  main:
//	    dsn_env: APP_DATABASE_URL
//	attribute_providers:
//	  - subject: user
//	    kind: sql
//	    connection: main
//	    get_one: SELECT department, clearance FROM users WHERE id = $1
//	    get_all: SELECT u.id AS id, u.department, u.clearance FROM users u
//
// Everything the package doc says about the Querier seam, engine-native
// placeholders, bound-never-interpolated parameters, columns becoming metadata,
// the driver-value mapping, the four casting rules, the timeout, and the
// read-only Metadata contract is true here WORD FOR WORD. There is no second
// mapping and no second value model: metadataValue is literally the same
// function, so a column read through an object provider and the same column read
// through an attribute provider produce the same Go value and therefore the same
// decision.
//
// Two things differ, and both are contract rather than convenience.
//
// # 1. get_one binds the BARE subject id, verbatim
//
// An object provider binds the identity's TERMINAL SEGMENT VALUE, so "brand:42"
// and "account:acme/brand:42" both bind "42". An attribute key has no segments
// to strip — it is a principal id or an account id, an opaque handle into the
// host's directory that Aperture never parses (see provider.AttributeRecord) —
// so it is bound exactly as it arrived:
//
//	Fetch(ctx, "alice")  →  QueryContext(stmt, "alice")
//
// That is a simplification, not a special case: there is nothing to strip, so
// nothing is stripped.
//
// # 2. get_all selects a BARE id
//
//	get_all: SELECT u.id AS id, u.department FROM users u          CORRECT
//	get_all: SELECT 'user:' || u.id AS id, u.department FROM users u   WRONG
//
// This is the one an author copying an object provider's statement gets wrong,
// and it is the one with NO ERROR ATTACHED. An identity-shaped key is a
// perfectly legal opaque string, so it enumerates happily, caches happily, and
// then matches no principal id any Fetch ever presents. The slot simply never
// answers, and nothing anywhere complains. There is no check this package could
// add — the key is opaque, so there is nothing to test it against — which is
// exactly why the asymmetry is written down here, in csvprovider's Attributes,
// and on seed's AttributeProvider.GetAll.
//
// # get_all is OPTIONAL, where an object provider's ListQuery is REQUIRED
//
// Config.ListQuery is required because an ObjectProvider that could be fetched
// from but not enumerated answers List with an error, and an errored enumeration
// reads as "no access" one layer up — a denial caused by a wiring gap.
//
// That reason does not apply to an attribute slot. Attribute enumeration never
// participates in scope resolution: a *provider.AttributeRegistry structurally
// cannot be a scope.ObjectLister, and enumeration is a system-tier admin read
// and nothing else. So an empty AttributeConfig.ListQuery yields a FETCH-ONLY
// provider: every decision path works unchanged, and only List and Query refuse,
// with a coded error naming the statement to declare.
//
// That is a feature. It lets a host expose the attributes of the principal
// currently being decided about WITHOUT exposing its whole user table to an
// admin enumeration.
//
// # What an error may name
//
// A diagnostic names the SLOT, the COLUMN, the row's POSITION in the result, and
// the KEY — the same things the object seam names, for the same reason: they are
// the developer's own statement and the caller's own input, and they are the only
// handle anyone has on the row that went wrong. A metadata VALUE is never named:
// it is host data belonging to some account, and an error is a thing that gets
// logged.

// compile-time assertion: an *Attributes is a usable AttributeProvider.
var _ provider.AttributeProvider = (*Attributes)(nil)

// attributeWildcardKey is the account wildcard, spelled here as a literal for
// the reason provider and csvprovider spell it as one: it is model.AccountWildcard,
// and none of the three packages may import model.
//
// A wildcard reaching an attribute fetch would mean "the attributes of every
// account", and the only bag that could satisfy it is one account's data served
// as another's. The registry refuses it before a provider is consulted; refusing
// it here too costs two lines and means a *Attributes used directly — by a host
// that wired it in Go, or by a test — cannot be talked into binding it.
const attributeWildcardKey = "*"

// AttributeConfig is the developer-supplied wiring for one attribute slot's
// provider. It is Config for the attribute seam, and every field means what its
// Config counterpart means unless this doc says otherwise.
type AttributeConfig struct {
	// Slot is the attribute slot this provider serves, and it is the slot it is
	// registered under in a provider.AttributeRegistry. Required, and it must be
	// one of the three declared slots.
	//
	// Unlike Config.ObjectType it is NOT load-bearing — an attribute key is
	// opaque, so there is no per-row check it could drive — but it is required
	// all the same, because it is what a runtime diagnostic names. A deployment
	// with three slots over one pool otherwise reports "the fetch statement
	// failed" without saying which of its three directories is down.
	Slot provider.AttributeSlot
	// FetchQuery is the "get one" statement. It takes exactly one placeholder,
	// written in the database's own syntax and passed through untouched, to which
	// Fetch binds the BARE subject id verbatim:
	//
	//	SELECT department, clearance FROM users WHERE id = $1
	//
	// Every column it returns becomes an attribute field keyed by the column
	// name, so the SELECT list is the field list. Required.
	FetchQuery string
	// ListQuery is the "get all" statement behind List and Query. It takes NO
	// parameters, returns one row per subject this slot serves, and must select
	// each row's BARE id as IDColumn:
	//
	//	SELECT u.id AS id, u.department FROM users u
	//
	// NOT 'user:' || u.id AS id — see the file doc for why that spelling fails
	// silently rather than loudly.
	//
	// It is OPTIONAL. Empty yields a fetch-only provider whose List and Query
	// refuse with APERTURE_CONFIG_INVALID; the file doc explains why the
	// asymmetry with Config.ListQuery is deliberate.
	ListQuery string
	// IDColumn names the ListQuery result column holding each row's bare subject
	// id. Empty means DefaultIDColumn ("id"). The column is removed from the row
	// before the rest becomes the attribute bag: it is the key, not a field.
	IDColumn string
	// Timeout bounds one statement, applied with context.WithTimeout on top of
	// the caller's context. Zero means DefaultTimeout; negative is a
	// configuration error. There is no "no timeout" setting.
	//
	// It matters more here than it does for an object: an attribute bag is read
	// by every rule against every object in a decision, so an unbounded statement
	// is not one object type answering slowly, it is the whole decision hanging.
	Timeout time.Duration
}

// Attributes is a SQL-backed provider.AttributeProvider for one attribute slot.
// It is immutable after NewAttributes and therefore safe for concurrent use, as
// the AttributeProvider contract requires; the Querier owns whatever pooling
// happens beneath it.
type Attributes struct {
	q          Querier
	slot       provider.AttributeSlot
	fetchQuery string
	listQuery  string // empty for a fetch-only slot
	idColumn   string
	timeout    time.Duration
}

// NewAttributes returns an Attributes that reads one slot's bags through q using
// cfg's statements. Register it under the slot cfg names.
//
// It validates NOW, for the reason New does and then some: a nil Querier, an
// unknown slot, a blank FetchQuery, or a negative Timeout would otherwise first
// surface as a failed decision, and an attribute slot's failures are every
// decision for that slot rather than one object type's.
func NewAttributes(q Querier, cfg AttributeConfig) (*Attributes, error) {
	if q == nil {
		return nil, aerr.New(aerr.APERTURE_CONFIG_INVALID,
			"sqlprovider: nil Querier; pass a *sql.DB or another handle with QueryContext and QueryRowContext")
	}
	// ParseAttributeSlot is the one crossing point between a bare string and a
	// slot the registry serves, and its code — APERTURE_ATTRIBUTE_SLOT_UNKNOWN,
	// with its own fixups naming the closed set — passes through rather than
	// being re-stamped as a generic configuration error.
	slot, err := provider.ParseAttributeSlot(string(cfg.Slot))
	if err != nil {
		return nil, err
	}
	stmt := strings.TrimSpace(cfg.FetchQuery)
	if stmt == "" {
		return nil, aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
			"sqlprovider: AttributeConfig.FetchQuery is required; it is the statement that fetches one subject's bag, binding the BARE subject id to its single placeholder",
			map[string]any{"slot": slot.String()})
	}
	// ListQuery is deliberately NOT required; see the file doc.
	listStmt := strings.TrimSpace(cfg.ListQuery)
	idColumn := strings.TrimSpace(cfg.IDColumn)
	if idColumn == "" {
		idColumn = DefaultIDColumn
	}
	if cfg.Timeout < 0 {
		return nil, aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
			"sqlprovider: AttributeConfig.Timeout cannot be negative; leave it zero for the default",
			map[string]any{"slot": slot.String(), "timeout": cfg.Timeout.String(), "default": DefaultTimeout.String()})
	}
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	return &Attributes{
		q:          q,
		slot:       slot,
		fetchQuery: stmt,
		listQuery:  listStmt,
		idColumn:   idColumn,
		timeout:    timeout,
	}, nil
}

// Slot reports which attribute slot this provider serves. It is the cheap way
// for a caller — a loader logging what it wired, a test — to confirm a provider
// landed where it was meant to.
func (a *Attributes) Slot() provider.AttributeSlot { return a.slot }

// Fetch returns id's attribute bag by running the configured fetch statement
// with id bound VERBATIM as its single parameter — no segment is stripped,
// because an attribute key has none.
//
// No rows yields APERTURE_NOT_FOUND, so the registry (and the resolvers above
// it) can tell "there is no such subject" from "the directory is unreachable";
// the two mean opposite things for a decision. More than one row yields
// APERTURE_SQL_PROVIDER_AMBIGUOUS naming the key — the first row is never
// silently taken, because which row that is would be unspecified, and a bag that
// varied between two identical Checks is a decision that varies with it. A driver
// failure yields APERTURE_SQL_PROVIDER_QUERY wrapping the cause, unless the cause
// already carries an APERTURE_* code, which passes through verbatim.
//
// The returned map is freshly allocated for this call and is read-only to its
// holder, transitively, per the provider.Metadata contract.
func (a *Attributes) Fetch(ctx context.Context, id string) (provider.Metadata, error) {
	if err := a.keyError(id); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()

	rows, err := a.q.QueryContext(ctx, a.fetchQuery, id)
	if err != nil {
		return nil, a.fetchError(err, id)
	}
	defer rows.Close()

	if !rows.Next() {
		// rows.Err first: a connection that died mid-statement also reports "no
		// next row", and reporting that as NOT_FOUND would turn a broken
		// directory into a confident "this subject does not exist" — which, one
		// layer up, decides against the floor bag and denies as though on
		// purpose.
		if err := rows.Err(); err != nil {
			return nil, a.fetchError(err, id)
		}
		return nil, aerr.WithContext(aerr.APERTURE_NOT_FOUND,
			"sqlprovider: no attribute row with this key",
			map[string]any{"slot": a.slot.String(), "key": id})
	}

	md, err := scanRow(rows, id)
	if err != nil {
		return nil, err
	}

	// A second row is checked BEFORE the bag is returned, never after it is
	// used: an ambiguous fetch is a broken statement, not a fetch with a bonus.
	if rows.Next() {
		return nil, aerr.WithContext(aerr.APERTURE_SQL_PROVIDER_AMBIGUOUS,
			"sqlprovider: attribute fetch statement returned more than one row for one subject; "+
				"filter on a unique key rather than adding LIMIT 1, which would make the bag depend on an unspecified row order",
			map[string]any{"slot": a.slot.String(), "key": id})
	}
	if err := rows.Err(); err != nil {
		return nil, a.fetchError(err, id)
	}
	return md, nil
}

// List returns every record this slot serves, in the order the "get all"
// statement returns them. It is Query with the zero AttributeFilter.
//
// A fetch-only provider — one declared with no ListQuery — refuses here rather
// than answering an empty page, because an empty page is indistinguishable from
// a directory that genuinely has no subjects.
func (a *Attributes) List(ctx context.Context) ([]provider.AttributeRecord, error) {
	return a.enumerate(ctx, provider.AttributeFilter{})
}

// Query returns the records satisfying filter.
//
// It runs the SAME "get all" statement List runs and applies AttributeFilter.Fields
// with provider.MatchFields, in Go. The predicates are never templated into the
// developer's SQL: the Fields contract is typed comparison ("5" never equals 5)
// and membership for collection fields, which is the rules engine's own
// semantics, and a database's own coercion rules do not reproduce it.
//
// Limit is honoured here as well as by the registry, so a bounded enumeration
// stops reading rows early and Query is correct when called standalone. Stopping
// early has the consequence the package doc records for the object seam: a row
// the limit never reaches cannot report its error, so the same enumeration with
// a larger limit can fail where a bounded one succeeded.
func (a *Attributes) Query(ctx context.Context, filter provider.AttributeFilter) ([]provider.AttributeRecord, error) {
	return a.enumerate(ctx, filter)
}

// enumerate is the one implementation behind List and Query: run the "get all"
// statement, turn every row into an AttributeRecord, and keep the ones filter
// selects.
//
// It streams — rows are consumed one at a time and never buffered as driver
// values — but it does materialise every selected record, which is the accepted
// cost of filtering in Go rather than in the host's SQL. The slot's TTL cache is
// what amortises it.
func (a *Attributes) enumerate(ctx context.Context, filter provider.AttributeFilter) ([]provider.AttributeRecord, error) {
	if a.listQuery == "" {
		// A fetch-only slot, refused with a code rather than an empty page: an
		// empty page reads as "this directory has no subjects", which is a
		// statement about the host's data rather than about Aperture's wiring,
		// and an admin acting on it would be acting on a lie.
		return nil, aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
			"sqlprovider: this attribute slot is fetch-only; it was declared with no get_all statement, so it cannot be enumerated",
			map[string]any{"slot": a.slot.String(), "declare": "get_all"})
	}

	ctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()

	// No arguments: the "get all" statement takes no parameters, and nothing a
	// caller supplied is ever interpolated into it.
	rows, err := a.q.QueryContext(ctx, a.listQuery)
	if err != nil {
		return nil, a.listError(err)
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, aerr.Wrapf(aerr.APERTURE_SQL_PROVIDER_SCAN, err,
			"sqlprovider: cannot read the result columns of the get_all statement for attribute slot %q", a.slot)
	}
	// The column shape is a property of the STATEMENT, so it is validated once
	// rather than re-derived per row.
	where := map[string]any{"slot": a.slot.String()}
	if err := validateColumns(cols, where); err != nil {
		return nil, err
	}
	idIndex := indexOf(cols, a.idColumn)
	if idIndex < 0 {
		return nil, aerr.WithContext(aerr.APERTURE_SQL_PROVIDER_ROW_IDENTITY,
			"sqlprovider: the get_all statement's result has no id column; select each row's BARE subject id under that name (SELECT u.id AS id, ...)",
			map[string]any{"slot": a.slot.String(), "id_column": a.idColumn, "columns": strings.Join(cols, ", ")})
	}

	// One scan buffer for the whole result — see scanSlots for why reusing it is
	// safe — and a non-nil result slice, so an empty enumeration is an empty
	// slice rather than a nil a caller might read as "unset".
	vals, dest := scanSlots(len(cols))
	out := make([]provider.AttributeRecord, 0, 16)
	row := 0
	for rows.Next() {
		row++
		if err := rows.Scan(dest...); err != nil {
			return nil, aerr.Wrapf(aerr.APERTURE_SQL_PROVIDER_SCAN, err,
				"sqlprovider: cannot scan row %d of the get_all statement for attribute slot %q", row, a.slot)
		}
		key, err := a.rowKey(vals[idIndex], row)
		if err != nil {
			return nil, err
		}
		md, err := rowMetadata(cols, vals, idIndex, key)
		if err != nil {
			return nil, err
		}
		if !provider.MatchFields(md, filter.Fields) {
			continue
		}
		out = append(out, provider.AttributeRecord{ID: key, Attributes: md})
		if filter.Limit > 0 && len(out) >= filter.Limit {
			break
		}
	}
	// rows.Err LAST and unconditionally. A connection that dies half way through
	// a result set ends the loop exactly as a complete one does, so skipping this
	// would report a truncated enumeration as a successful one.
	if err := rows.Err(); err != nil {
		return nil, a.listError(err)
	}
	return out, nil
}

// rowKey turns the id column of the row'th row into this slot's bare attribute
// key.
//
// It is deliberately NOT rowIdentity. There is no identity.Parse here and no
// terminal-segment check, because an attribute key has no grammar to validate
// against — it is the host's own opaque handle. What is left is what can still
// be wrong: the value must be text, and it must name somebody.
//
// The key must be TEXT. A []byte is accepted as raw text — unlike a metadata
// column, where a []byte is JSON — because the id column is not metadata and has
// no JSON reading that could compete: some drivers hand back a string column as
// bytes, and a key is a string either way. A bare integer key is rejected with
// the cast it is missing, because a Fetch binds the principal id as the STRING
// the decision path holds, and an int64 key would never equal it.
func (a *Attributes) rowKey(v any, row int) (string, error) {
	// Every diagnostic names the ROW's position in the result, which is the only
	// handle a developer has on a row whose id is exactly what went wrong.
	base := map[string]any{"slot": a.slot.String(), "id_column": a.idColumn, "row": row}
	where := func(extra map[string]any) map[string]any { return mergeContext(base, extra) }

	var raw string
	switch x := v.(type) {
	case string:
		raw = x
	case []byte:
		raw = string(x)
	case nil:
		return "", aerr.WithContext(aerr.APERTURE_SQL_PROVIDER_ROW_IDENTITY,
			"sqlprovider: a row's id column is NULL; every enumerated row needs a subject key, so exclude the row in the statement or give it one",
			where(nil))
	default:
		return "", aerr.WithContext(aerr.APERTURE_SQL_PROVIDER_ROW_IDENTITY,
			"sqlprovider: a row's id column is not text; select the BARE subject id (u.id AS id), casting it with ::text if it is numeric or a uuid",
			where(map[string]any{"type": goTypeName(v)}))
	}
	if strings.TrimSpace(raw) == "" {
		return "", aerr.WithContext(aerr.APERTURE_SQL_PROVIDER_ROW_IDENTITY,
			"sqlprovider: a row's id column is empty; every enumerated row needs a subject key",
			where(nil))
	}
	if raw == attributeWildcardKey {
		// Refused rather than enumerated, and the whole enumeration fails rather
		// than the row being skipped — the object seam's rule, for the object
		// seam's reason. A row keyed "*" claims to be every account, and skipping
		// it would under-report the directory instead of reporting the fault.
		return "", aerr.WithContext(aerr.APERTURE_ATTRIBUTE_PROVIDER_INVALID,
			"sqlprovider: a row's id column is the account wildcard, which is never an attribute key",
			where(map[string]any{"key": attributeWildcardKey}))
	}
	return raw, nil
}

// keyError rejects a fetch key that can never name one subject. It mirrors the
// registry's own guard rather than trusting it, because an *Attributes is also
// reachable directly by a host that wired it in Go.
//
// An empty key is rejected here rather than bound: binding "" and letting the
// database answer "no rows" would report a caller's bug as a subject that does
// not exist, and a subject that does not exist decides against the floor bag
// instead of failing.
func (a *Attributes) keyError(id string) error {
	switch id {
	case "":
		return aerr.WithContext(aerr.APERTURE_ATTRIBUTE_PROVIDER_INVALID,
			"sqlprovider: attribute key is empty; there is nothing to bind",
			map[string]any{"slot": a.slot.String()})
	case attributeWildcardKey:
		return aerr.WithContext(aerr.APERTURE_ATTRIBUTE_PROVIDER_INVALID,
			"sqlprovider: the account wildcard is not an attribute key",
			map[string]any{"slot": a.slot.String(), "key": attributeWildcardKey})
	}
	return nil
}

// fetchError normalises a failure of the "get one" statement. An error already
// carrying an APERTURE_* code passes through VERBATIM — a host's wrapping Querier
// may raise one, and Wrap RE-STAMPS, so wrapping would replace the
// classification the host chose — while a plain driver error is wrapped as
// APERTURE_SQL_PROVIDER_QUERY with the cause reachable through errors.Is /
// errors.As.
func (a *Attributes) fetchError(err error, id string) error {
	if aerr.CodeOf(err) != "" {
		return err
	}
	return aerr.Wrapf(aerr.APERTURE_SQL_PROVIDER_QUERY, err,
		"sqlprovider: attribute fetch statement failed for slot %q, key %q", a.slot, id)
}

// listError normalises a failure of the "get all" statement. Like fetchError it
// passes an already-coded error through verbatim, and names the slot rather than
// any row value.
func (a *Attributes) listError(err error) error {
	if aerr.CodeOf(err) != "" {
		return err
	}
	return aerr.Wrapf(aerr.APERTURE_SQL_PROVIDER_QUERY, err,
		"sqlprovider: get_all statement failed for attribute slot %q", a.slot)
}
