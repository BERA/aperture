package sqlite

import (
	"strings"
	"testing"
)

// These tests prove the gate in schema_naming_test.go BITES. A convention gate
// that always passes is worse than no gate, so every rule it enforces is
// exercised here against a deliberately-broken fixture, and every name the rule
// must NOT flag is exercised against a clean one.
//
// The fixtures are strings. The real schema.sql is never mutated to test the
// gate — a test that edits the file it audits can leave the repository broken
// when it fails partway through, and it cannot run in parallel with anything.

// mustParse parses a fixture and fails the test if it will not parse.
func mustParse(t *testing.T, src string) []tableDef {
	t.Helper()
	tables, err := parseCreateTables(src)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	return tables
}

// TestSchemaNamingGateFlagsBrokenSchemas feeds the gate schemas that break the
// convention and asserts both that it complains and that the complaint is
// actionable: the identifier, its table, and the lists that reserve it.
func TestSchemaNamingGateFlagsBrokenSchemas(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		// want are substrings the failure message must contain. Asserting on the
		// message, not merely on the count, is what keeps the message useful:
		// the whole point of the vendored provenance is that a maintainer can
		// act without opening another file.
		want []string
	}{
		{
			name: "table without the apt_ prefix",
			sql:  `CREATE TABLE IF NOT EXISTS accounts (id TEXT PRIMARY KEY);`,
			want: []string{
				`table "accounts" is not prefixed apt_`,
				`Rename it to apt_accounts`,
			},
		},
		{
			name: "table that is itself a reserved word",
			sql:  `CREATE TABLE grants (id TEXT PRIMARY KEY);`,
			want: []string{
				`table "grants" is not prefixed apt_`,
				`Reserved as GRANT, matched as the singular of "grants"`,
				`Reserved by: PostgreSQL`,
				`Oracle`,
			},
		},
		{
			name: "column that is a reserved word outright",
			sql:  `CREATE TABLE apt_permissions (id TEXT PRIMARY KEY, action TEXT NOT NULL);`,
			want: []string{
				`column "action" on table apt_permissions spells a SQL reserved word`,
				`Reserved as ACTION.`,
				`SQLite`,
				`Rename it to apt_action`,
			},
		},
		{
			name: "plural column caught through its singular",
			sql:  `CREATE TABLE apt_templates (name TEXT NOT NULL, grants TEXT NOT NULL DEFAULT '[]');`,
			want: []string{
				`column "grants" on table apt_templates spells a SQL reserved word`,
				`Reserved as GRANT, matched as the singular of "grants"`,
			},
		},
		{
			name: "roles is caught through ROLE",
			sql:  `CREATE TABLE apt_principals (id TEXT PRIMARY KEY, roles TEXT NOT NULL);`,
			want: []string{
				`column "roles" on table apt_principals spells a SQL reserved word`,
				`Reserved as ROLE, matched as the singular of "roles"`,
				// ROLE is reserved ONLY by SQL Server's future-keywords list. The
				// gate still fires — apt_object was renamed on exactly that
				// basis — but the message must say so, because it is the one
				// case where an exception could be argued.
				`Reserved by: SQL Server future keywords.`,
				`no shipping`,
			},
		},
		{
			name: "-ies plural caught through its singular",
			sql:  `CREATE TABLE apt_principals (id TEXT PRIMARY KEY, identities TEXT NOT NULL);`,
			want: []string{
				`Reserved as IDENTITY, matched as the singular of "identities"`,
			},
		},
		{
			name: "quoting a reserved word is not an exemption",
			sql:  `CREATE TABLE apt_audit_log ("action" TEXT NOT NULL);`,
			want: []string{
				`column "action" on table apt_audit_log spells a SQL reserved word`,
				`Reserved as ACTION.`,
			},
		},
		{
			name: "a delimited constraint keyword is a column, and is checked",
			sql:  "CREATE TABLE apt_rules (`primary` TEXT NOT NULL);",
			want: []string{
				`column "primary" on table apt_rules spells a SQL reserved word`,
				`Reserved as PRIMARY.`,
			},
		},
		{
			name: "several violations arrive in one message",
			sql: `CREATE TABLE grants (id TEXT PRIMARY KEY, object TEXT NOT NULL);
			      CREATE TABLE apt_permissions (action TEXT NOT NULL);`,
			want: []string{
				`3 problems`,
				`table "grants" is not prefixed apt_`,
				`column "object" on table grants`,
				`column "action" on table apt_permissions`,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vs := checkNaming(mustParse(t, tc.sql))
			if len(vs) == 0 {
				t.Fatalf("gate found nothing wrong with:\n%s\nA gate that does not bite is worse than no gate.", tc.sql)
			}
			msg := report(vs)
			for _, want := range tc.want {
				if !strings.Contains(msg, want) {
					t.Errorf("failure message is missing %q; a maintainer cannot act on it.\n--- message ---\n%s", want, msg)
				}
			}
			// Every message restates the convention and points at the second
			// place a rename has to land.
			for _, always := range []string{
				"The convention:",
				"storage/sqlite/sqlite.go",
				"deleteByID",
				"internal/sqlreserved",
			} {
				if !strings.Contains(msg, always) {
					t.Errorf("failure message is missing %q", always)
				}
			}
		})
	}
}

// TestSchemaNamingGateAcceptsLegitimateNames pins the false positives. Each of
// these was a real hazard: compounds contain keywords, the fix itself (apt_)
// contains the keyword it fixes, constraint clauses re-list column names, and
// schema.sql's own header comment quotes the OLD names to explain the rule.
func TestSchemaNamingGateAcceptsLegitimateNames(t *testing.T) {
	cases := []struct {
		name string
		sql  string
	}{
		{
			name: "compound names are never keywords",
			sql: `CREATE TABLE apt_grants (
			          account_id  TEXT NOT NULL,
			          object_type TEXT NOT NULL,
			          role_id     TEXT NOT NULL,
			          group_id    TEXT NOT NULL
			      );`,
		},
		{
			name: "apt_-prefixed names are the fix, not a violation",
			sql: `CREATE TABLE apt_object_types (
			          apt_action   TEXT NOT NULL,
			          apt_object   TEXT NOT NULL,
			          apt_identity TEXT NOT NULL,
			          apt_grants   TEXT NOT NULL,
			          apt_actions  TEXT NOT NULL
			      );`,
		},
		{
			name: "table constraints re-list columns and must not be re-reported",
			sql: `CREATE TABLE apt_memberships (
			          principal_id TEXT NOT NULL,
			          account_id   TEXT NOT NULL,
			          PRIMARY KEY (principal_id, account_id),
			          UNIQUE (account_id),
			          CHECK (principal_id <> ''),
			          FOREIGN KEY (account_id) REFERENCES apt_accounts (id),
			          CONSTRAINT grant_is_not_a_column CHECK (account_id <> '')
			      );`,
		},
		{
			name: "prose in comments is not schema",
			sql: `-- Renamed from grants; the action and identity columns moved too.
			      /* object, actions, grants -- all of these used to be bare. */
			      CREATE TABLE apt_grants ( -- was: grants
			          id     TEXT PRIMARY KEY, -- action
			          effect TEXT NOT NULL
			      );`,
		},
		{
			name: "string literals are not identifiers",
			sql: `CREATE TABLE apt_grants (
			          id     TEXT PRIMARY KEY,
			          effect TEXT NOT NULL DEFAULT 'grants',
			          reason TEXT NOT NULL DEFAULT 'action, identity, object'
			      );`,
		},
		{
			name: "the real schema's own column vocabulary is clean",
			sql: `CREATE TABLE apt_audit_log (
			          id                 TEXT PRIMARY KEY,
			          ts_nanos           INTEGER NOT NULL,
			          event_type         TEXT NOT NULL,
			          actor              TEXT NOT NULL DEFAULT '',
			          effective_subject  TEXT NOT NULL DEFAULT '',
			          impersonation_mode TEXT NOT NULL DEFAULT '',
			          account            TEXT NOT NULL DEFAULT '',
			          target             TEXT NOT NULL DEFAULT '',
			          outcome            TEXT NOT NULL DEFAULT '',
			          reason             TEXT NOT NULL DEFAULT '',
			          details            TEXT NOT NULL DEFAULT '',
			          params             TEXT NOT NULL DEFAULT '[]',
			          version            INTEGER NOT NULL,
			          seq                INTEGER NOT NULL,
			          ast                TEXT NOT NULL,
			          description        TEXT NOT NULL DEFAULT '',
			          name               TEXT NOT NULL,
			          kind               TEXT NOT NULL,
			          effect             TEXT NOT NULL,
			          delegatable        INTEGER NOT NULL DEFAULT 0,
			          scope_strategy     TEXT NOT NULL DEFAULT '',
			          display_name       TEXT NOT NULL DEFAULT ''
			      );`,
		},
		{
			name: "CREATE INDEX is not a table and carries no columns",
			sql: `CREATE TABLE apt_grants (id TEXT PRIMARY KEY, account_id TEXT NOT NULL);
			      CREATE INDEX IF NOT EXISTS idx_apt_grants_account_subject
			          ON apt_grants (account_id);`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if vs := checkNaming(mustParse(t, tc.sql)); len(vs) > 0 {
				t.Errorf("gate flagged a legitimate schema:\n%s", report(vs))
			}
		})
	}
}

// TestSchemaParserSeparatesTableAndColumnPositions is the case that forced a
// parser instead of a text scan: apt_grants is BOTH a table name and a column
// name on apt_templates. Anything that matches text rather than position
// conflates the two, and the fix for one is not the fix for the other.
func TestSchemaParserSeparatesTableAndColumnPositions(t *testing.T) {
	const sql = `CREATE TABLE grants (
	                 id     TEXT PRIMARY KEY,
	                 grants TEXT NOT NULL DEFAULT '[]'
	             );`
	vs := checkNaming(mustParse(t, sql))
	if len(vs) != 2 {
		t.Fatalf("got %d violations, want 2 (one table, one column):\n%s", len(vs), report(vs))
	}
	if vs[0].kind != "table" || vs[0].name != "grants" || vs[0].table != "" {
		t.Errorf("first violation = %+v, want the TABLE grants", vs[0])
	}
	if vs[1].kind != "column" || vs[1].name != "grants" || vs[1].table != "grants" {
		t.Errorf("second violation = %+v, want the COLUMN grants on table grants", vs[1])
	}

	// The legitimate arrangement — both positions prefixed — is silent.
	const fixed = `CREATE TABLE apt_grants (id TEXT PRIMARY KEY);
	               CREATE TABLE apt_templates (name TEXT NOT NULL, apt_grants TEXT NOT NULL DEFAULT '[]');`
	if got := checkNaming(mustParse(t, fixed)); len(got) > 0 {
		t.Errorf("gate flagged the schema as it actually ships:\n%s", report(got))
	}
}

// TestSchemaParserRecordsWhereItLooked pins the parse itself, so a fixture that
// happens to be silent because nothing was parsed cannot masquerade as a pass.
func TestSchemaParserRecordsWhereItLooked(t *testing.T) {
	const sql = `-- header
CREATE TABLE apt_accounts (
    id   TEXT PRIMARY KEY,
    name TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_apt_accounts_name ON apt_accounts (name);

CREATE TABLE IF NOT EXISTS main.apt_roles (
    id TEXT PRIMARY KEY,
    PRIMARY KEY (id)
);`
	tables := mustParse(t, sql)
	if len(tables) != 2 {
		t.Fatalf("parsed %d tables, want 2 (CREATE INDEX is not a table)", len(tables))
	}
	if tables[0].name != "apt_accounts" || tables[0].line != 2 {
		t.Errorf("table 0 = %q at line %d, want apt_accounts at line 2", tables[0].name, tables[0].line)
	}
	if got := len(tables[0].columns); got != 2 {
		t.Errorf("apt_accounts has %d columns, want 2", got)
	}
	if tables[0].columns[1].name != "name" || tables[0].columns[1].line != 4 {
		t.Errorf("column 1 = %q at line %d, want name at line 4", tables[0].columns[1].name, tables[0].columns[1].line)
	}
	// A schema qualifier is not part of the name.
	if tables[1].name != "apt_roles" {
		t.Errorf("table 1 = %q, want apt_roles (main. qualifier dropped)", tables[1].name)
	}
	if got := len(tables[1].columns); got != 1 {
		t.Errorf("apt_roles has %d columns, want 1 (PRIMARY KEY is a constraint)", got)
	}
}

// TestSchemaParserRejectsMalformedSQL is the fail-don't-skip posture at the
// parser level: SQL the gate cannot read is an error it reports, never a silent
// empty result that passes every subsequent assertion.
func TestSchemaParserRejectsMalformedSQL(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want string
	}{
		{"unterminated block comment", "/* forever\nCREATE TABLE apt_x (id TEXT);", "unterminated /* block comment"},
		{"unterminated string", "CREATE TABLE apt_x (id TEXT DEFAULT 'oops);", "unterminated '...' literal"},
		{"unterminated quoted identifier", `CREATE TABLE apt_x ("oops TEXT);`, `unterminated "..." literal`},
		{"unclosed paren", "CREATE TABLE apt_x (id TEXT;", "left open at end of input"},
		{"stray close paren", "CREATE TABLE apt_x (id TEXT));", "unbalanced ')'"},
		{"missing semicolon", "CREATE TABLE apt_x (id TEXT)", "not terminated with ';'"},
		{"table with no name", "CREATE TABLE (id TEXT);", "without a table name"},
		{"table with no column list", "CREATE TABLE apt_x AS SELECT 1;", "expected '(' and a column list"},
		{"table with no columns", "CREATE TABLE apt_x (PRIMARY KEY (id));", "no column definitions"},
		{"trailing comma", "CREATE TABLE apt_x (id TEXT,);", "empty column definition"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tables, err := parseCreateTables(tc.sql)
			if err == nil {
				t.Fatalf("parsed %d tables with no error; malformed SQL must fail, not pass quietly", len(tables))
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestSchemaGateRefusesAVacuousParse covers the subtlest failure mode: a schema
// the parser reads without complaint but understands as empty. The gate would
// have nothing to check and would pass. knownTableFloor is what stops that, so
// prove the floor is above what an empty parse produces.
func TestSchemaGateRefusesAVacuousParse(t *testing.T) {
	tables, err := parseCreateTables("-- only a comment, no statements at all\n")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(tables) != 0 {
		t.Fatalf("parsed %d tables from a comment-only file, want 0", len(tables))
	}
	if len(tables) >= knownTableFloor {
		t.Fatalf("knownTableFloor is %d, which an empty parse satisfies; the anti-vacuity tripwire is disarmed", knownTableFloor)
	}
	if checkNaming(tables) != nil {
		t.Fatal("checkNaming found violations in an empty parse")
	}
}

// TestSingularFamilyStopsWhereItShould pins the matcher's reach. Singular-family
// matching is deliberate over-reach — it exists to catch `grants` — and its cost
// is the risk of one day flagging a legitimate name. These are the boundaries it
// must respect in the meantime.
func TestSingularFamilyStopsWhereItShould(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"grants", []string{"grants", "grant"}},
		{"roles", []string{"roles", "role"}},
		{"identities", []string{"identities", "identity"}},
		{"aliases", []string{"aliases", "alias"}},
		{"account_id", []string{"account_id"}}, // no trailing s: nothing derived
		{"address", []string{"address"}},       // -ss is not a plural
		{"s", []string{"s"}},                   // degenerate: nothing to strip
		{"ts_nanos", []string{"ts_nanos", "ts_nano"}},
	}
	for _, tc := range cases {
		got := singularFamily(tc.in)
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("singularFamily(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}

	// Whole-name matching is the other half of the rule: a compound that
	// CONTAINS a keyword is not a keyword, and must never be flagged.
	for _, name := range []string{"account_id", "object_type", "role_id", "group_id", "subject_kind", "created_at"} {
		if hit, ok := reservedMatch(name); ok {
			t.Errorf("reservedMatch(%q) flagged it as %s; compound names never qualify", name, hit.word)
		}
	}
}
