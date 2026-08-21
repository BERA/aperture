package sqlite

import (
	"context"
	"fmt"
	"sort"
	"strings"

	aerr "github.com/frankbardon/aperture/errors"
)

// ---- the startup guard against a database this build cannot read ----
//
// Setup applies schema.sql, which is entirely CREATE ... IF NOT EXISTS. On a
// database that already has Aperture's tables it therefore creates nothing and
// complains about nothing -- including when those tables are the OLD shape,
// with RFC3339Nano text in created_at/updated_at and the audit log's instant
// still named ts_nanos. SQLite is dynamically typed, so nothing in the engine
// objects either: a TEXT value sits happily in a column the newer schema
// declares INTEGER. Without this guard the failure surfaces hours later, as a
// scan error or -- worse -- a zero timestamp, far from its cause.
//
// So Setup refuses such a database up front. This is a DETECTOR, NOT A
// MIGRATOR. Aperture ships no migration tool, no schema versioning, and no
// user_version pragma; a schema break is a hard break and the remedy is a new
// database. Refusing loudly at startup IS the feature.
//
// Two signals, and why one is not enough
//
//	(1) The DECLARED column type, from PRAGMA table_info. Every timestamp
//	    column this build writes is declared INTEGER; the pre-change schema
//	    declared TEXT. This catches any database an older build created, and
//	    it costs nothing -- no table is read.
//	(2) The STORED value type, from typeof(). Signal (1) inspects what some
//	    CREATE TABLE once claimed; the declaration is what a fresh Setup
//	    writes, while the values are what a read actually decodes. In SQLite
//	    those can disagree: copy the old rows into freshly created tables
//	    (INSERT ... SELECT, an ATTACH-and-copy, a partial hand upgrade) and
//	    every declaration reads INTEGER while every value is still RFC3339
//	    text -- INTEGER affinity does not convert a string that is not a
//	    well-formed integer. That database is precisely the one this guard
//	    exists to catch, and signal (1) alone would wave it through. So the
//	    values are checked too.
//
// The old ts_nanos column name is a third, name-level signal, independent of
// both types -- it was already INTEGER before the change, so only its name
// betrays it.
//
// The break BEFORE this one
//
// Signals (1)-(3) all read apt_-prefixed tables, so on their own they miss the
// oldest databases entirely: v0.5.0 predates the apt_ prefix and named its
// tables accounts, grants, audit_log. Such a database has no apt_ table at all,
// so the checks above find nothing, Setup happily creates the full apt_ schema
// beside the old tables, and Aperture starts up EMPTY while every account, role
// and grant sits untouched and unread a few tables away. That is the quietest
// failure of the three and the one an operator is least equipped to diagnose,
// so unprefixedShape looks for it too -- see there for how it avoids mistaking
// a host's own `accounts` table for Aperture's.
//
// Cost on the happy path is bounded: one sqlite_master query, one PRAGMA per
// Aperture table, and one single-row probe per table. No full scan; a healthy
// database pays microseconds.

// timestampColumns are the column names that carry an instant in this schema.
// Every one of them is INTEGER nanoseconds since the Unix epoch (see the Time
// section of schema.sql). Adding a timestamp column to schema.sql means adding
// its name here, or the guard silently stops covering it.
var timestampColumns = []string{"created_at", "occurred_at", "updated_at"}

// retiredColumns are column names this schema no longer uses, mapped to what
// replaced them. Finding one is proof on its own that the database predates
// this build, whatever its column types say.
var retiredColumns = map[string]string{
	"ts_nanos": "occurred_at",
}

// tablePrefix is the prefix every table Aperture owns carries, so the guard
// inspects only Aperture's own tables and ignores anything else sharing the
// database.
const tablePrefix = "apt_"

// guardSchemaShape refuses a database written by a build whose schema this one
// cannot read. It runs BEFORE schema.sql is applied -- after would tell it
// nothing new, since CREATE ... IF NOT EXISTS leaves an existing table exactly
// as it found it.
//
// A database with none of Aperture's tables is a fresh one: there is nothing to
// be wrong, and it returns nil. A database whose tables are all current also
// returns nil. Anything else returns APERTURE_STORAGE_SCHEMA_INCOMPATIBLE
// naming the offending column, what is wrong with it, and the remedy.
func guardSchemaShape(ctx context.Context, exec sqlExec) error {
	// Oldest break first. It is checked even when apt_ tables are already
	// present, because "ran the new build once against a v0.5.0 database" is a
	// real state: it leaves empty apt_ tables next to the full old ones, and
	// that database must be refused, not blessed on the second run.
	if err := unprefixedShape(ctx, exec); err != nil {
		return err
	}

	tables, err := apertureTables(ctx, exec)
	if err != nil {
		return err
	}
	if len(tables) == 0 {
		return nil // a fresh database: Setup is about to create everything.
	}

	// Pass 1 -- declarations. Cheapest and broadest: it reads no rows and
	// catches an empty table, which pass 2 by definition cannot.
	stamped := make(map[string][]string, len(tables))
	for _, table := range tables {
		cols, err := tableColumns(ctx, exec, table)
		if err != nil {
			return err
		}
		for _, col := range cols {
			if replacement, retired := retiredColumns[col.name]; retired {
				return incompatible(table, col.name,
					fmt.Sprintf("is a column this build no longer has (it was renamed to %s)", replacement))
			}
			if !isTimestampColumn(col.name) {
				continue
			}
			// SQLite records the declared type verbatim, so normalize before
			// comparing: "integer", "INTEGER", " Integer " are one type.
			if !strings.EqualFold(strings.TrimSpace(col.declType), "INTEGER") {
				return incompatible(table, col.name, fmt.Sprintf(
					"is declared %s, but this build stores every instant as INTEGER nanoseconds since the Unix epoch",
					quoteForMessage(col.declType)))
			}
			stamped[table] = append(stamped[table], col.name)
		}
	}

	// Pass 2 -- stored values. The declarations all claim INTEGER; confirm the
	// rows agree, because in SQLite the declaration is an intention and the
	// value is the fact, and a read decodes the fact.
	for _, table := range tables {
		cols := stamped[table]
		if len(cols) == 0 {
			continue
		}
		kinds, err := storedTypes(ctx, exec, table, cols)
		if err != nil {
			return err
		}
		for i, kind := range kinds {
			// "integer" is the only right answer. NULL cannot occur (every
			// timestamp column is NOT NULL) and "real"/"blob" are as wrong as
			// "text", so the check is an equality, not a text hunt.
			if !strings.EqualFold(kind, "integer") {
				return incompatible(table, cols[i], fmt.Sprintf(
					"holds %s values, not the INTEGER nanoseconds since the Unix epoch this build reads (SQLite stores a value's own type, so an INTEGER declaration does not convert rows written by an older build)",
					quoteForMessage(kind)))
			}
		}
	}
	return nil
}

// prePrefixFingerprint identifies a database from before the apt_ table prefix.
//
// The table NAMES alone cannot do it. `accounts`, `roles`, `groups` and
// `permissions` are among the most common table names in software, and the apt_
// prefix exists precisely so Aperture can share a database with a host schema
// (see schema.sql's header). Refusing to start because the host happens to own a
// table called `roles` would be a worse bug than the one being guarded against.
//
// So each entry pairs an old table name with columns that, together, are
// Aperture's own shape rather than a generic one: the join tables' exact
// (left_id, right_id, seq) triples, the audit log's ts_nanos, the grant's
// subject_kind/effect pair. A table matches only if it carries EVERY column
// listed for it.
var prePrefixFingerprint = []struct {
	table   string
	columns []string
}{
	{"principal_roles", []string{"principal_id", "role_id", "seq"}},
	{"role_permissions", []string{"role_id", "permission_id", "seq"}},
	{"group_members", []string{"group_id", "principal_id", "seq"}},
	{"grants", []string{"account_id", "subject_kind", "subject_id", "permission_id", "object", "effect"}},
	{"object_types", []string{"name", "actions"}},
	{"audit_log", []string{"id", "ts_nanos", "event_type"}},
}

// prePrefixQuorum is how many fingerprint tables must match before the database
// is called Aperture's. A real pre-prefix database always has all six, because
// one Setup created them together -- so the quorum costs no detection at all. It
// buys the margin: a host schema would have to reproduce three distinct
// Aperture-shaped tables, column set included, by coincidence.
const prePrefixQuorum = 3

// unprefixedShape refuses a database written before Aperture's tables gained the
// apt_ prefix (v0.5.0 and earlier). Left undetected this is the quietest failure
// of all: no apt_ table exists, so Setup creates the whole schema fresh and
// Aperture comes up empty while the operator's data sits in the unprefixed
// tables, unread and unmentioned.
func unprefixedShape(ctx context.Context, exec sqlExec) error {
	var matched []string
	for _, want := range prePrefixFingerprint {
		cols, err := tableColumns(ctx, exec, want.table)
		if err != nil {
			return err
		}
		if len(cols) == 0 {
			continue // no such table; PRAGMA on a missing table returns no rows
		}
		present := make(map[string]bool, len(cols))
		for _, c := range cols {
			present[c.name] = true
		}
		if hasAll(present, want.columns) {
			matched = append(matched, want.table)
		}
	}
	if len(matched) < prePrefixQuorum {
		return nil
	}
	// Same opening stem as incompatible(), so every refusal this guard can
	// produce is one recognizable sentence in a startup log.
	return aerr.Newf(aerr.APERTURE_STORAGE_SCHEMA_INCOMPATIBLE,
		"this database was written by an incompatible build of Aperture: it predates the %s table prefix "+
			"and carries the old unprefixed tables (%s), which this build does not read -- "+
			"starting against it would come up EMPTY while that data sat unread beside the new tables; "+
			"Aperture has no migration path (no schema versioning, no upgrade step) -- "+
			"move or delete the old database, let Setup create a fresh one, and re-seed it",
		tablePrefix, strings.Join(matched, ", "))
}

func hasAll(present map[string]bool, want []string) bool {
	for _, c := range want {
		if !present[c] {
			return false
		}
	}
	return true
}

// incompatible builds the refusal. It names WHERE the problem is, WHAT is wrong
// with it, and WHAT TO DO -- an operator should not have to read this file to
// act on it. The DSN is deliberately absent: it can carry credentials.
func incompatible(table, column, problem string) error {
	return aerr.Newf(aerr.APERTURE_STORAGE_SCHEMA_INCOMPATIBLE,
		"this database was written by an incompatible build of Aperture: %s.%s %s; "+
			"Aperture has no migration path (no schema versioning, no upgrade step) -- "+
			"move or delete the old database, let Setup create a fresh one, and re-seed it",
		table, column, problem)
}

// apertureTables lists the apt_-prefixed tables present, sorted, so a refusal
// names the same column every run rather than whichever the engine happened to
// return first.
func apertureTables(ctx context.Context, exec sqlExec) ([]string, error) {
	const q = `SELECT name FROM sqlite_master WHERE type = 'table' AND name LIKE ? ORDER BY name`
	rows, err := exec.QueryContext(ctx, q, tablePrefix+"%")
	if err != nil {
		return nil, wrapStorage("inspect existing schema", err)
	}
	defer rows.Close()

	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, wrapStorage("inspect existing schema", err)
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapStorage("inspect existing schema", err)
	}
	sort.Strings(tables)
	return tables, nil
}

// column is one row of PRAGMA table_info: a column's name and the type its
// CREATE TABLE declared (which SQLite stores as written and does not enforce).
type column struct {
	name     string
	declType string
}

func tableColumns(ctx context.Context, exec sqlExec, table string) ([]column, error) {
	// table_info takes no bind parameter, so the identifier is interpolated.
	// It came from sqlite_master and matched apt_%, and quoteIdent doubles any
	// embedded quote, so the value cannot escape its quoting.
	rows, err := exec.QueryContext(ctx, `PRAGMA table_info(`+quoteIdent(table)+`)`)
	if err != nil {
		return nil, wrapStorage("inspect existing schema", err)
	}
	defer rows.Close()

	var cols []column
	for rows.Next() {
		// cid, name, type, notnull, dflt_value, pk
		var (
			cid       int
			name      string
			declType  string
			notNull   int
			dfltValue any
			pk        int
		)
		if err := rows.Scan(&cid, &name, &declType, &notNull, &dfltValue, &pk); err != nil {
			return nil, wrapStorage("inspect existing schema", err)
		}
		cols = append(cols, column{name: name, declType: declType})
	}
	if err := rows.Err(); err != nil {
		return nil, wrapStorage("inspect existing schema", err)
	}
	return cols, nil
}

// storedTypes reports SQLite's own type tag for each named column in ONE row of
// the table -- LIMIT 1, so a large audit log costs a single row read, not a
// scan. One row is enough: a database in the pre-change shape has every row in
// it, because the whole point is that the old build wrote them all the same
// way. An empty table yields no row and nothing to judge; its declaration
// already spoke for it in pass 1.
func storedTypes(ctx context.Context, exec sqlExec, table string, cols []string) ([]string, error) {
	sel := make([]string, len(cols))
	for i, c := range cols {
		sel[i] = `typeof(` + quoteIdent(c) + `)`
	}
	q := `SELECT ` + strings.Join(sel, ", ") + ` FROM ` + quoteIdent(table) + ` LIMIT 1`

	rows, err := exec.QueryContext(ctx, q)
	if err != nil {
		return nil, wrapStorage("inspect existing schema", err)
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, wrapStorage("inspect existing schema", err)
		}
		return nil, nil // empty table
	}
	kinds := make([]string, len(cols))
	dest := make([]any, len(cols))
	for i := range kinds {
		dest[i] = &kinds[i]
	}
	if err := rows.Scan(dest...); err != nil {
		return nil, wrapStorage("inspect existing schema", err)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapStorage("inspect existing schema", err)
	}
	return kinds, nil
}

func isTimestampColumn(name string) bool {
	for _, c := range timestampColumns {
		if name == c {
			return true
		}
	}
	return false
}

// quoteIdent renders a SQLite identifier as a double-quoted literal, doubling
// any embedded quote so the identifier cannot terminate its own quoting.
func quoteIdent(ident string) string {
	return `"` + strings.ReplaceAll(ident, `"`, `""`) + `"`
}

// quoteForMessage renders a type name for the refusal text, so an empty or
// blank declared type reads as `""` rather than vanishing mid-sentence.
func quoteForMessage(s string) string {
	if strings.TrimSpace(s) == "" {
		return `""`
	}
	return s
}
