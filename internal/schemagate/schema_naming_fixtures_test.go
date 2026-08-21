package schemagate

import (
	"fmt"
	"sort"
	"strings"
	"testing"
)

// These tests prove the gate in schema_naming_test.go BITES. A convention gate
// that always passes is worse than no gate, so every rule it enforces is
// exercised here against a deliberately-broken fixture, and every name the rule
// must NOT flag is exercised against a clean one.
//
// The fixtures are strings. A real schema.sql is never mutated to test the
// gate — a test that edits the file it audits can leave the repository broken
// when it fails partway through, and it cannot run in parallel with anything.
//
// These cases came across from storage/sqlite unchanged when the parser moved
// here, which is the point of them: they are the safety net for the move, and a
// net that was re-knitted on the way over is not one. What is NEW below is the
// coverage the move made necessary — REFERENCES / ON DELETE / ON UPDATE, and the
// Postgres file's schema qualifier — grouped at the bottom.

// mustParse parses a fixture and fails the test if it will not parse.
func mustParse(t *testing.T, src string) []tableDef {
	t.Helper()
	tables, err := parseCreateTables(src)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	return tables
}

// fixtureDialect is the dialect the message assertions below are written
// against. The failure message names per-dialect files (the schema and the Go
// files holding its SQL string literals), so the fixtures have to pick one; they
// pick SQLite because that is where these cases were written and what their
// expected strings say. Looking it up rather than hardcoding a literal means a
// registry that loses the row fails here too, loudly.
func fixtureDialect(t *testing.T) dialect {
	t.Helper()
	for _, d := range dialects {
		if d.name == "sqlite" {
			return d
		}
	}
	t.Fatal(`no "sqlite" dialect in the registry; the fixtures below assert on its file names`)
	return dialect{}
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
			msg := report(fixtureDialect(t), vs)
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
			          occurred_at        INTEGER NOT NULL,
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
				t.Errorf("gate flagged a legitimate schema:\n%s", report(fixtureDialect(t), vs))
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
		t.Fatalf("got %d violations, want 2 (one table, one column):\n%s", len(vs), report(fixtureDialect(t), vs))
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
		t.Errorf("gate flagged the schema as it actually ships:\n%s", report(fixtureDialect(t), got))
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
// have nothing to check and would pass. The per-dialect floors are what stop
// that, so prove EVERY floor is above what an empty parse produces — a registry
// row that ships a zero floor is a dialect that is registered and ungoverned.
func TestSchemaGateRefusesAVacuousParse(t *testing.T) {
	tables, err := parseCreateTables("-- only a comment, no statements at all\n")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(tables) != 0 {
		t.Fatalf("parsed %d tables from a comment-only file, want 0", len(tables))
	}
	if checkNaming(tables) != nil {
		t.Fatal("checkNaming found violations in an empty parse")
	}
	for _, d := range dialects {
		if len(tables) >= d.tableFloor {
			t.Errorf("%s: tableFloor is %d, which an empty parse satisfies; the anti-vacuity tripwire is disarmed", d.name, d.tableFloor)
		}
		if d.foreignKeyFloor <= 0 {
			t.Errorf("%s: foreignKeyFloor is %d; a schema that declares foreign keys needs a floor above zero or the clause reader can stop working unnoticed", d.name, d.foreignKeyFloor)
		}
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

// ---------------------------------------------------------------------------
// New with the move: REFERENCES clauses and the schema qualifier
//
// Everything above came from storage/sqlite unchanged. Everything below is the
// syntax the second dialect brought with it. The two are kept apart so a
// reviewer can see at a glance which assertions are the safety net for the move
// and which are new ground.
// ---------------------------------------------------------------------------

// fkKey renders one parsed edge in a comparable form, so a test can assert on
// the whole set rather than on fields one at a time.
func fkKey(from string, fk foreignKey) string {
	return fmt.Sprintf("%s(%s) -> %s(%s) ON DELETE %s ON UPDATE %s",
		from, strings.Join(fk.columns, ","), fk.table, strings.Join(fk.refColumns, ","),
		fk.onDelete, fk.onUpdate)
}

// allForeignKeys flattens a parse into fkKey strings, sorted.
func allForeignKeys(tables []tableDef) []string {
	var out []string
	for _, tbl := range tables {
		for _, fk := range tbl.foreignKeys {
			out = append(out, fkKey(tbl.name, fk))
		}
	}
	sort.Strings(out)
	return out
}

// TestParserReadsForeignKeyClauses is the REFERENCES / ON DELETE / ON UPDATE
// criterion. Before the move the parser stepped over a table constraint the
// moment it saw FOREIGN, which was correct while nothing needed the edges and
// became a hole the moment nine of them landed in two files that have to agree.
func TestParserReadsForeignKeyClauses(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want []string
	}{
		{
			name: "a table constraint with both actions",
			sql: `CREATE TABLE apt_principal_roles (
			          principal_id TEXT NOT NULL,
			          role_id      TEXT NOT NULL,
			          PRIMARY KEY (principal_id, role_id),
			          FOREIGN KEY (principal_id) REFERENCES apt_principals (id) ON DELETE CASCADE ON UPDATE RESTRICT,
			          FOREIGN KEY (role_id) REFERENCES apt_roles (id) ON DELETE RESTRICT ON UPDATE RESTRICT
			      );`,
			want: []string{
				"apt_principal_roles(principal_id) -> apt_principals(id) ON DELETE CASCADE ON UPDATE RESTRICT",
				"apt_principal_roles(role_id) -> apt_roles(id) ON DELETE RESTRICT ON UPDATE RESTRICT",
			},
		},
		{
			name: "the Postgres spelling: every identifier schema-qualified",
			sql: `CREATE TABLE IF NOT EXISTS apt_schema.apt_group_members (
			          group_id     TEXT NOT NULL,
			          principal_id TEXT NOT NULL,
			          PRIMARY KEY (group_id, principal_id),
			          FOREIGN KEY (group_id) REFERENCES apt_schema.apt_groups (id) ON DELETE CASCADE ON UPDATE RESTRICT,
			          FOREIGN KEY (principal_id) REFERENCES apt_schema.apt_principals (id) ON DELETE RESTRICT ON UPDATE RESTRICT
			      );`,
			// The qualifier is dropped on BOTH sides, which is what makes an edge
			// read out of the Postgres file comparable with the SQLite one.
			want: []string{
				"apt_group_members(group_id) -> apt_groups(id) ON DELETE CASCADE ON UPDATE RESTRICT",
				"apt_group_members(principal_id) -> apt_principals(id) ON DELETE RESTRICT ON UPDATE RESTRICT",
			},
		},
		{
			name: "a named constraint",
			sql: `CREATE TABLE apt_grants (
			          permission_id TEXT NOT NULL,
			          CONSTRAINT fk_grant_permission FOREIGN KEY (permission_id)
			              REFERENCES apt_permissions (id) ON DELETE RESTRICT ON UPDATE RESTRICT
			      );`,
			want: []string{"apt_grants(permission_id) -> apt_permissions(id) ON DELETE RESTRICT ON UPDATE RESTRICT"},
		},
		{
			name: "the inline column form is a foreign key too",
			sql: `CREATE TABLE apt_permissions (
			          id          TEXT PRIMARY KEY,
			          object_type TEXT NOT NULL REFERENCES apt_object_types (name) ON DELETE RESTRICT ON UPDATE RESTRICT
			      );`,
			want: []string{"apt_permissions(object_type) -> apt_object_types(name) ON DELETE RESTRICT ON UPDATE RESTRICT"},
		},
		{
			name: "compound keys, no referenced column list, and the clauses in the other order",
			sql: `CREATE TABLE apt_x (
			          a TEXT NOT NULL,
			          b TEXT NOT NULL,
			          FOREIGN KEY (a, b) REFERENCES apt_y ON UPDATE NO ACTION ON DELETE SET NULL
			      );`,
			want: []string{"apt_x(a,b) -> apt_y() ON DELETE SET NULL ON UPDATE NO ACTION"},
		},
		{
			name: "MATCH and DEFERRABLE are stepped over, not choked on",
			sql: `CREATE TABLE apt_x (
			          a TEXT NOT NULL,
			          FOREIGN KEY (a) REFERENCES apt_y (id) MATCH FULL
			              ON DELETE SET DEFAULT ON UPDATE CASCADE
			              DEFERRABLE INITIALLY DEFERRED
			      );`,
			want: []string{"apt_x(a) -> apt_y(id) ON DELETE SET DEFAULT ON UPDATE CASCADE"},
		},
		{
			name: "PRIMARY KEY, UNIQUE and CHECK are not foreign keys",
			sql: `CREATE TABLE apt_x (
			          a TEXT NOT NULL,
			          PRIMARY KEY (a),
			          UNIQUE (a),
			          CHECK (a <> ''),
			          CONSTRAINT a_is_set CHECK (a <> '')
			      );`,
			want: nil,
		},
		{
			name: "a column literally named references is not a clause",
			sql: `CREATE TABLE apt_x (
			          a            TEXT NOT NULL,
			          "references" TEXT NOT NULL DEFAULT ''
			      );`,
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := allForeignKeys(mustParse(t, tc.sql))
			if strings.Join(got, "\n") != strings.Join(tc.want, "\n") {
				t.Errorf("foreign keys =\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(tc.want, "\n  "))
			}
		})
	}
}

// TestParserRejectsForeignKeySyntaxItCannotRead is the fail-don't-skip posture
// carried into the clause reader. A FOREIGN KEY the parser gives up on must be
// an error, because the alternative — treating it as "not a foreign key" — is a
// key that silently leaves the edge set.
func TestParserRejectsForeignKeySyntaxItCannotRead(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want string
	}{
		{"FOREIGN without KEY", "CREATE TABLE apt_x (a TEXT, FOREIGN (a) REFERENCES apt_y (id));", "FOREIGN is not followed by KEY"},
		{"no key column list", "CREATE TABLE apt_x (a TEXT, FOREIGN KEY REFERENCES apt_y (id));", "expected '(' and a column list"},
		{"no REFERENCES", "CREATE TABLE apt_x (a TEXT, FOREIGN KEY (a));", "expected REFERENCES after the key columns"},
		{"REFERENCES nothing", "CREATE TABLE apt_x (a TEXT, FOREIGN KEY (a) REFERENCES);", "REFERENCES without a target table"},
		{"unknown ON DELETE action", "CREATE TABLE apt_x (a TEXT, FOREIGN KEY (a) REFERENCES apt_y (id) ON DELETE MAYBE);", `unknown referential action "MAYBE"`},
		{"unknown ON UPDATE action", "CREATE TABLE apt_x (a TEXT, FOREIGN KEY (a) REFERENCES apt_y (id) ON UPDATE SET FIRE);", `unknown referential action "SET"`},
		{"ON DELETE with nothing after it", "CREATE TABLE apt_x (a TEXT, FOREIGN KEY (a) REFERENCES apt_y (id) ON DELETE);", "missing referential action"},
		{"ON DELETE twice", "CREATE TABLE apt_x (a TEXT, FOREIGN KEY (a) REFERENCES apt_y (id) ON DELETE CASCADE ON DELETE RESTRICT);", "ON DELETE given twice"},
		{"MATCH with no mode", "CREATE TABLE apt_x (a TEXT, FOREIGN KEY (a) REFERENCES apt_y (id) MATCH ON DELETE CASCADE);", "MATCH must be FULL, PARTIAL or SIMPLE"},
		{"INITIALLY with no mode", "CREATE TABLE apt_x (a TEXT, FOREIGN KEY (a) REFERENCES apt_y (id) DEFERRABLE INITIALLY);", "INITIALLY must be DEFERRED or IMMEDIATE"},
		{"trailing syntax the parser has not been taught", "CREATE TABLE apt_x (a TEXT, FOREIGN KEY (a) REFERENCES apt_y (id) ENFORCED);", `unexpected "ENFORCED" after the clause`},
		{"an inline clause the parser cannot read", "CREATE TABLE apt_x (a TEXT REFERENCES apt_y (id) ON DELETE WHENEVER);", `unknown referential action "WHENEVER"`},
		{"CONSTRAINT without a name", "CREATE TABLE apt_x (a TEXT, CONSTRAINT FOREIGN KEY (a) REFERENCES apt_y (id));", "CONSTRAINT without a name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tables, err := parseCreateTables(tc.sql)
			if err == nil {
				t.Fatalf("parsed %d tables with no error; a foreign key the parser cannot read must fail, not vanish", len(tables))
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestQualifiedNamesAreCheckedThroughTheQualifier is the qualifier criterion at
// the rule level rather than the parser level: apt_schema.apt_grants is a legal
// table name and apt_schema.grants is not, and the gate has to be able to tell
// them apart through a prefix it is otherwise discarding.
func TestQualifiedNamesAreCheckedThroughTheQualifier(t *testing.T) {
	const clean = `CREATE TABLE IF NOT EXISTS apt_schema.apt_templates (
	                   name       TEXT NOT NULL,
	                   apt_grants TEXT NOT NULL DEFAULT '[]'
	               );`
	if vs := checkNaming(mustParse(t, clean)); len(vs) > 0 {
		t.Errorf("gate flagged the Postgres file's own spelling:\n%s", report(fixtureDialect(t), vs))
	}

	// The qualifier must not launder a violation. apt_schema. is a placeholder,
	// not a namespace the convention accepts in place of apt_.
	const broken = `CREATE TABLE IF NOT EXISTS apt_schema.grants (
	                    id     TEXT PRIMARY KEY,
	                    action TEXT NOT NULL
	                );`
	vs := checkNaming(mustParse(t, broken))
	if len(vs) != 2 {
		t.Fatalf("got %d violations, want 2 (the table and the column):\n%s", len(vs), report(fixtureDialect(t), vs))
	}
	if vs[0].kind != "table" || vs[0].name != "grants" {
		t.Errorf("first violation = %+v, want the TABLE grants with the qualifier dropped", vs[0])
	}
	if vs[1].kind != "column" || vs[1].name != "action" || vs[1].table != "grants" {
		t.Errorf("second violation = %+v, want the COLUMN action on grants", vs[1])
	}
}

// TestReportNamesTheDialectItIsTalkingAbout pins the one thing that had to
// change when the message stopped being about a single file: a Postgres failure
// must send a maintainer to the Postgres files, and must not quietly keep
// naming SQLite's.
func TestReportNamesTheDialectItIsTalkingAbout(t *testing.T) {
	vs := checkNaming(mustParse(t, `CREATE TABLE grants (id TEXT PRIMARY KEY);`))
	if len(vs) == 0 {
		t.Fatal("fixture produced no violations")
	}
	for _, d := range dialects {
		t.Run(d.name, func(t *testing.T) {
			msg := report(d, vs)
			for _, want := range append([]string{d.path}, d.statementSources...) {
				if !strings.Contains(msg, want) {
					t.Errorf("the %s failure message never mentions %q:\n%s", d.name, want, msg)
				}
			}
			for _, other := range dialects {
				if other.name == d.name {
					continue
				}
				if strings.Contains(msg, other.path) {
					t.Errorf("the %s failure message points at %s, which is the wrong file to edit:\n%s", d.name, other.path, msg)
				}
			}
			if d.qualifier != "" && !strings.Contains(msg, d.qualifier) {
				t.Errorf("the %s failure message never explains the %q qualifier its identifiers carry:\n%s", d.name, d.qualifier, msg)
			}
		})
	}
}
