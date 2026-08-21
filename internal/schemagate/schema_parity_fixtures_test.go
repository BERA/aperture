package schemagate

import (
	"strings"
	"testing"
)

// These tests prove the parity gate in schema_parity_test.go BITES, and that the
// parser records the two column facts the gate compares. They follow the rule
// the naming fixtures set: the fixtures are STRINGS, and a real schema.sql is
// never mutated to test the gate. A test that edits the file it audits can leave
// the repository broken when it fails partway through.
//
// The mutation proofs in the story — add a table, add a column, change an
// ON DELETE — were also run by hand against the real files. These are the
// permanent version of that, so the proof does not have to be redone by hand
// every time somebody wonders whether the gate still works.

// ---------------------------------------------------------------------------
// The parser half: types and constraints
// ---------------------------------------------------------------------------

// TestParserReadsColumnTypesAndConstraints is the anti-vacuity check for what
// E5-S2 added to the parser. The parity gate compares a column's type through
// the divergence mapping and everything ELSE about the declaration verbatim, so
// a parser that recorded an empty type or an empty tail would make both halves
// of that comparison trivially agree.
func TestParserReadsColumnTypesAndConstraints(t *testing.T) {
	cases := []struct {
		name            string
		sql             string
		wantType        []string
		wantConstraints []string
	}{
		{
			name: "the two spellings this repo actually uses",
			sql: `CREATE TABLE apt_x (
			          id         TEXT PRIMARY KEY,
			          created_at INTEGER NOT NULL DEFAULT 0,
			          note       TEXT NOT NULL DEFAULT ''
			      );`,
			wantType:        []string{"TEXT", "INTEGER", "TEXT"},
			wantConstraints: []string{"PRIMARY KEY", "NOT NULL DEFAULT 0", "NOT NULL DEFAULT ''"},
		},
		{
			name: "the Postgres spelling of the same table",
			sql: `CREATE TABLE IF NOT EXISTS apt_schema.apt_x (
			          id         TEXT PRIMARY KEY,
			          created_at BIGINT NOT NULL DEFAULT 0,
			          note       TEXT NOT NULL DEFAULT ''
			      );`,
			wantType:        []string{"TEXT", "BIGINT", "TEXT"},
			wantConstraints: []string{"PRIMARY KEY", "NOT NULL DEFAULT 0", "NOT NULL DEFAULT ''"},
		},
		{
			name: "keyword case does not change the declaration",
			sql: `CREATE TABLE apt_x (
			          a text not null default '',
			          b Integer Not Null Default 0
			      );`,
			wantType:        []string{"TEXT", "INTEGER"},
			wantConstraints: []string{"NOT NULL DEFAULT ''", "NOT NULL DEFAULT 0"},
		},
		{
			name: "a parameterised and a multi-word type arrive whole",
			sql: `CREATE TABLE apt_x (
			          a VARCHAR(255) NOT NULL,
			          b DOUBLE PRECISION NOT NULL,
			          c NUMERIC(10, 2) NOT NULL
			      );`,
			wantType:        []string{"VARCHAR(255)", "DOUBLE PRECISION", "NUMERIC(10, 2)"},
			wantConstraints: []string{"NOT NULL", "NOT NULL", "NOT NULL"},
		},
		{
			name: "a CHECK stays with the column it constrains",
			sql: `CREATE TABLE apt_x (
			          a TEXT NOT NULL CHECK (a <> '')
			      );`,
			wantType: []string{"TEXT"},
			// The tokenizer emits punctuation one character at a time, so `<>`
			// renders spaced. That is cosmetic and cannot hide a divergence:
			// both sides of every comparison go through the same renderer, so
			// two dialects spelling the CHECK identically still compare equal.
			wantConstraints: []string{"NOT NULL CHECK(A < > '')"},
		},
		{
			name: "an inline REFERENCES is lifted out and the rest of the definition survives",
			sql: `CREATE TABLE apt_x (
			          a TEXT REFERENCES apt_y (id) ON DELETE RESTRICT ON UPDATE RESTRICT NOT NULL
			      );`,
			wantType: []string{"TEXT"},
			// The clause is excised rather than truncated at, so the NOT NULL
			// that followed it is still part of the declaration.
			wantConstraints: []string{"NOT NULL"},
		},
		{
			name: "a column with no type at all reads as no type, not as its first constraint",
			sql: `CREATE TABLE apt_x (
			          a PRIMARY KEY
			      );`,
			wantType:        []string{""},
			wantConstraints: []string{"PRIMARY KEY"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tables := mustParse(t, tc.sql)
			if len(tables) != 1 {
				t.Fatalf("parsed %d tables, want 1", len(tables))
			}
			cols := tables[0].columns
			if len(cols) != len(tc.wantType) {
				t.Fatalf("parsed %d columns, want %d", len(cols), len(tc.wantType))
			}
			for i, col := range cols {
				if col.typeName != tc.wantType[i] {
					t.Errorf("column %q type = %q, want %q", col.name, col.typeName, tc.wantType[i])
				}
				if col.constraints != tc.wantConstraints[i] {
					t.Errorf("column %q constraints = %q, want %q", col.name, col.constraints, tc.wantConstraints[i])
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The gate half: the diffs bite
// ---------------------------------------------------------------------------

// fixtureDialects are two synthetic dialects named after the real ones, so the
// type mapping under test is the real one rather than a fixture of its own.
var (
	fixtureSQLite   = dialect{name: "sqlite", path: "fixture/sqlite/schema.sql"}
	fixturePostgres = dialect{name: "postgres", path: "fixture/postgres/schema.sql"}
)

// factsFrom parses a fixture schema into the fact set the diffs consume.
func factsFrom(t *testing.T, d dialect, src string) schemaFacts {
	t.Helper()
	facts, err := reduce(d, mustParse(t, src))
	if err != nil {
		t.Fatalf("reduce %s: %v", d.name, err)
	}
	return facts
}

// pair is the two-dialect fact set the diffs are given.
func pair(t *testing.T, sqliteSQL, postgresSQL string) []schemaFacts {
	t.Helper()
	return []schemaFacts{
		factsFrom(t, fixtureSQLite, sqliteSQL),
		factsFrom(t, fixturePostgres, postgresSQL),
	}
}

// The agreeing baseline: the same two tables, the same columns, the same edge,
// spelled the way each dialect spells it. Every case below is this with exactly
// one thing changed, so the failure it produces is attributable.
const (
	baselineSQLite = `
CREATE TABLE apt_parents (
    id         TEXT PRIMARY KEY,
    created_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE apt_children (
    id        TEXT PRIMARY KEY,
    parent_id TEXT NOT NULL,
    seq       INTEGER NOT NULL,
    FOREIGN KEY (parent_id) REFERENCES apt_parents (id) ON DELETE RESTRICT ON UPDATE RESTRICT
);`

	baselinePostgres = `
CREATE TABLE apt_schema.apt_parents (
    id         TEXT PRIMARY KEY,
    created_at BIGINT NOT NULL DEFAULT 0
);
CREATE TABLE apt_schema.apt_children (
    id        TEXT PRIMARY KEY,
    parent_id TEXT NOT NULL,
    seq       BIGINT NOT NULL,
    FOREIGN KEY (parent_id) REFERENCES apt_schema.apt_parents (id) ON DELETE RESTRICT ON UPDATE RESTRICT
);`
)

// TestTheAgreeingBaselineIsQuiet is the other half of a gate that bites: it must
// not fire on the divergences that ARE legitimate. This baseline carries all
// four of them at once — INTEGER vs BIGINT, a different declaration order, the
// apt_schema. qualifier — and must produce nothing.
func TestTheAgreeingBaselineIsQuiet(t *testing.T) {
	// Declared in the opposite order on purpose: Postgres needs parents first,
	// SQLite does not care, and the gate compares sets rather than sequences.
	reordered := `
CREATE TABLE apt_schema.apt_children (
    id        TEXT PRIMARY KEY,
    parent_id TEXT NOT NULL,
    seq       BIGINT NOT NULL,
    FOREIGN KEY (parent_id) REFERENCES apt_schema.apt_parents (id) ON DELETE RESTRICT ON UPDATE RESTRICT
);
CREATE TABLE apt_schema.apt_parents (
    id         TEXT PRIMARY KEY,
    created_at BIGINT NOT NULL DEFAULT 0
);`

	all := pair(t, baselineSQLite, reordered)
	if got := diffTables(all); len(got) > 0 {
		t.Errorf("the agreeing baseline reported table problems:\n%s", strings.Join(got, "\n"))
	}
	cols, comparedCols := diffColumns(all)
	if len(cols) > 0 {
		t.Errorf("the agreeing baseline reported column problems:\n%s", strings.Join(cols, "\n"))
	}
	if comparedCols != 5 {
		t.Errorf("compared %d columns, want 5; the baseline has that many and a gate that compared fewer is not agreeing, it is not looking", comparedCols)
	}
	edges, comparedEdges := diffForeignKeys(all)
	if len(edges) > 0 {
		t.Errorf("the agreeing baseline reported foreign-key problems:\n%s", strings.Join(edges, "\n"))
	}
	if comparedEdges != 1 {
		t.Errorf("compared %d edges, want 1", comparedEdges)
	}
}

// TestTheParityGateBites walks one deliberate divergence at a time. Each case
// names the diff that must fire and a phrase the message must carry, because a
// gate that fires with an unactionable message sends a maintainer nowhere.
func TestTheParityGateBites(t *testing.T) {
	cases := []struct {
		name     string
		sqlite   string
		postgres string
		// which diff must report it
		diff string // "tables", "columns", "edges"
		want string // a substring the message must contain
	}{
		{
			name:   "a table added to one dialect only",
			sqlite: baselineSQLite + "\nCREATE TABLE apt_widgets (id TEXT PRIMARY KEY);",
			// The SQLite side gained apt_widgets; Postgres did not.
			postgres: baselinePostgres,
			diff:     "tables",
			want:     "table apt_widgets is declared by sqlite but is missing from",
		},
		{
			name:     "a column added to one dialect only",
			sqlite:   baselineSQLite,
			postgres: strings.Replace(baselinePostgres, "    seq       BIGINT NOT NULL,", "    seq       BIGINT NOT NULL,\n    label     TEXT NOT NULL DEFAULT '',", 1),
			diff:     "columns",
			want:     "column apt_children.label is declared by postgres but is missing from",
		},
		{
			name:     "a shared column whose type stopped meaning the same thing",
			sqlite:   strings.Replace(baselineSQLite, "created_at INTEGER", "created_at TEXT", 1),
			postgres: baselinePostgres,
			diff:     "columns",
			want:     "column apt_parents.created_at does not mean the same thing in every dialect",
		},
		{
			name:     "a narrowing spelling, refused with its reason",
			sqlite:   baselineSQLite,
			postgres: strings.Replace(baselinePostgres, "created_at BIGINT", "created_at INTEGER", 1),
			diff:     "columns",
			want:     "Postgres INTEGER is 32-bit while SQLite's is 64-bit",
		},
		{
			name:     "a type nobody has decided the meaning of",
			sqlite:   baselineSQLite,
			postgres: strings.Replace(baselinePostgres, "created_at BIGINT", "created_at NUMERIC", 1),
			diff:     "columns",
			want:     "NUMERIC is not in the postgres type mapping",
		},
		{
			name:     "a NOT NULL dropped from one dialect",
			sqlite:   baselineSQLite,
			postgres: strings.Replace(baselinePostgres, "    seq       BIGINT NOT NULL,", "    seq       BIGINT,", 1),
			diff:     "columns",
			want:     "column apt_children.seq is not declared the same way in every dialect",
		},
		{
			name:   "an edge removed from one dialect",
			sqlite: baselineSQLite,
			postgres: strings.Replace(baselinePostgres,
				",\n    FOREIGN KEY (parent_id) REFERENCES apt_schema.apt_parents (id) ON DELETE RESTRICT ON UPDATE RESTRICT", "", 1),
			diff: "edges",
			want: "foreign key apt_children(parent_id) is declared by",
		},
		{
			name:     "an edge whose ON DELETE moved",
			sqlite:   baselineSQLite,
			postgres: strings.Replace(baselinePostgres, "ON DELETE RESTRICT ON UPDATE RESTRICT", "ON DELETE CASCADE ON UPDATE RESTRICT", 1),
			diff:     "edges",
			want:     "the dialects disagree about its ON DELETE",
		},
		{
			name:     "an edge whose ON UPDATE moved",
			sqlite:   baselineSQLite,
			postgres: strings.Replace(baselinePostgres, "ON DELETE RESTRICT ON UPDATE RESTRICT", "ON DELETE RESTRICT ON UPDATE CASCADE", 1),
			diff:     "edges",
			want:     "the dialects disagree about its ON UPDATE",
		},
		{
			name:     "an edge repointed at another table",
			sqlite:   baselineSQLite,
			postgres: strings.Replace(baselinePostgres, "REFERENCES apt_schema.apt_parents (id)", "REFERENCES apt_schema.apt_children (id)", 1),
			diff:     "edges",
			want:     "the dialects disagree about its target table",
		},
		{
			name:     "an edge that lost its referenced column list",
			sqlite:   baselineSQLite,
			postgres: strings.Replace(baselinePostgres, "REFERENCES apt_schema.apt_parents (id)", "REFERENCES apt_schema.apt_parents", 1),
			diff:     "edges",
			want:     "the dialects disagree about its referenced columns",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			all := pair(t, tc.sqlite, tc.postgres)
			var got []string
			switch tc.diff {
			case "tables":
				got = diffTables(all)
			case "columns":
				got, _ = diffColumns(all)
			case "edges":
				got, _ = diffForeignKeys(all)
			default:
				t.Fatalf("unknown diff %q", tc.diff)
			}
			if len(got) == 0 {
				t.Fatalf("the %s diff reported nothing; this divergence would land unnoticed", tc.diff)
			}
			joined := strings.Join(got, "\n")
			if !strings.Contains(joined, tc.want) {
				t.Errorf("the %s diff fired but never says %q:\n%s", tc.diff, tc.want, joined)
			}
		})
	}
}

// TestAMissingTableDoesNotDrownItsColumns keeps the failure readable. When a
// whole table is absent from a dialect, the table diff reports it once; the
// column diff must not restate it as one finding per column, or the one line
// that matters is buried under the fourteen that do not.
func TestAMissingTableDoesNotDrownItsColumns(t *testing.T) {
	sqlite := baselineSQLite + "\nCREATE TABLE apt_widgets (id TEXT PRIMARY KEY, label TEXT NOT NULL, seq INTEGER NOT NULL);"
	all := pair(t, sqlite, baselinePostgres)

	if got := diffTables(all); len(got) != 1 {
		t.Errorf("the table diff reported %d problems, want exactly 1", len(got))
	}
	got, _ := diffColumns(all)
	for _, p := range got {
		if strings.Contains(p, "apt_widgets") {
			t.Errorf("the column diff also reported apt_widgets, which the table diff already owns:\n%s", p)
		}
	}
}

// TestReduceRefusesADuplicateDeclaration guards the fact set itself. The maps
// keyed by name would silently keep the last declaration, so a schema declaring
// a table or a column twice has to be an error rather than a comparison of
// whichever copy won.
func TestReduceRefusesADuplicateDeclaration(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want string
	}{
		{
			name: "a table declared twice",
			sql:  "CREATE TABLE apt_x (a TEXT NOT NULL);\nCREATE TABLE apt_x (b TEXT NOT NULL);",
			want: "declared twice",
		},
		{
			name: "a column declared twice",
			sql:  "CREATE TABLE apt_x (a TEXT NOT NULL, a TEXT NOT NULL);",
			want: "declares column a twice",
		},
		{
			name: "two foreign keys on the same columns",
			sql: `CREATE TABLE apt_x (
			          a TEXT NOT NULL,
			          FOREIGN KEY (a) REFERENCES apt_y (id) ON DELETE RESTRICT ON UPDATE RESTRICT,
			          FOREIGN KEY (a) REFERENCES apt_z (id) ON DELETE CASCADE ON UPDATE RESTRICT
			      );`,
			want: "two foreign keys on the same columns",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := reduce(fixtureSQLite, mustParse(t, tc.sql))
			if err == nil {
				t.Fatalf("reduce accepted a duplicate declaration")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}
