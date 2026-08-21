package schemagate

import (
	"fmt"
	"sort"
	"strings"
	"testing"
)

// This file is the CROSS-DIALECT parity gate: schema_naming_test.go asks whether
// each dialect obeys the convention on its own, and this one asks whether the
// dialects still describe the SAME DATABASE.
//
// Two hand-written schema files with nothing forcing them to agree is exactly
// the drift this repo legislates against everywhere else — TestEditorOperator-
// TablesAgree for the rule editor, TestDriverValueMappingTableMatchesTheType-
// Switch for the driver-value table. Both parse the source with a real parser
// and diff the tables; so does this, reusing the parser in schema_sql_test.go.
//
// It is the mitigation for an accepted risk: CI never runs the Postgres backend,
// so nothing in `make test` proves Postgres BEHAVES. This cannot prove that
// either. What it can prove is that Postgres has not silently fallen BEHIND —
// that a table, a column, or a foreign-key action added to one file was added to
// the other. It needs no container and no node, only the two files on disk.
//
// The comparison is SYMMETRIC. There is no reference dialect whose spelling the
// others must match: every fact is collected from every dialect and reported as
// "declared by X, missing from Y". A gate with a privileged side drifts the
// moment somebody edits the unprivileged one.
//
// The diffs are pure functions returning problems, and the tests below turn
// those into failures. That split is what lets schema_parity_fixtures_test.go
// prove the gate BITES against synthetic schemas, without ever mutating a real
// schema.sql — the same rule the naming fixtures follow, for the same reason.

// ---------------------------------------------------------------------------
// The divergence mapping
//
// The dialects legitimately differ, and a blanket "types don't count" exemption
// would gut the gate — it would pass a schema whose created_at had become TEXT
// in one backend. So each legitimate difference is written down as an EXPLICIT
// mapping from a dialect's physical spelling to the logical type Aperture means,
// and everything else must match verbatim.
//
// The four known divergences, and what this file does with each:
//
//  1. INTEGER (SQLite) vs BIGINT (Postgres), uniformly. -> physicalTypes below.
//     SQLite's INTEGER is 64-bit whatever the column holds; Postgres INTEGER is
//     32-bit, so every column SQLite spells INTEGER is spelled BIGINT there.
//  2. Table declaration order (Postgres resolves REFERENCES at CREATE time and
//     needs parents first). -> handled by comparing SETS, not sequences. Order
//     is not a fact this gate collects.
//  3. Single-column TEXT primary keys read back NOT NULL in Postgres and
//     nullable in SQLite. -> needs NO mapping entry, because it is an ENGINE
//     behaviour and not a spelling: both files write `id TEXT PRIMARY KEY`,
//     character for character. A file-parsing gate cannot see this divergence
//     and does not have to. If SQLite's schema ever spelled a key column NOT
//     NULL to close the gap, THAT would be a real textual divergence and this
//     gate would fire — correctly, because it would then be a difference a
//     reader of the two files can see.
//  4. The `apt_schema.` qualifier on every Postgres table identifier. -> the
//     parser already drops it (token.identName), and
//     TestSchemaQualifiersAreStillWhatTheRegistrySays pins that the file still
//     spells it that way, so it cannot be dropped from a name that never had it.
// ---------------------------------------------------------------------------

// The logical types Aperture's storage layer actually has. They are deliberately
// few: a column here holds a string or a signed 64-bit integer, and nothing else.
const (
	logicalText  = "text (unbounded UTF-8 string)"
	logicalInt64 = "int64 (signed 64-bit integer)"
)

// physicalTypes is the mapping, per dialect, from a physical type SPELLING to
// the logical type it means. It is exhaustive by construction: a spelling that
// appears in a schema file and is not in its dialect's map is a FAILURE, not an
// unknown that passes. That is the difference between an explicit mapping and an
// exemption — adding a type to a schema forces a decision about what it means in
// the other dialect, in the same commit.
var physicalTypes = map[string]map[string]string{
	"sqlite": {
		"TEXT":    logicalText,
		"INTEGER": logicalInt64, // 64-bit in SQLite regardless of declared width
	},
	"postgres": {
		"TEXT":   logicalText,
		"BIGINT": logicalInt64,
	},
}

// refusedTypes are spellings a dialect must NOT use, with the reason, so the
// failure explains itself instead of only reporting an unmapped type. Each entry
// records a decision already argued in the schema file's own header; the point
// of repeating it here is that the argument is easy to lose and this is where a
// maintainer will be standing when they are tempted to lose it.
var refusedTypes = map[string]map[string]string{
	"postgres": {
		"INTEGER": "Postgres INTEGER is 32-bit while SQLite's is 64-bit. Mapping it to the\n" +
			"same logical type would let a column silently narrow its domain relative to\n" +
			"the reference backend — a behavioural difference between two backends that\n" +
			"storagetest asserts are identical. Spell it BIGINT.",
		"SMALLINT": "Narrower than the int64 every integer column in this schema carries. Spell it BIGINT.",
		"BOOLEAN": "delegatable stays a 0/1 integer rather than becoming BOOLEAN, because BOOLEAN\n" +
			"would make the Go scan target differ per backend — precisely the per-dialect\n" +
			"carve-out storagetest forbids.",
		"JSON": "Aperture stores the rules and template packages' own canonical serialization\n" +
			"VERBATIM so a round-trip is byte-stable. Use TEXT.",
		"JSONB": "jsonb normalizes key order and whitespace, which would silently rewrite the\n" +
			"canonical serialization Aperture stores verbatim. Use TEXT.",
		"TIMESTAMPTZ": "There is no per-dialect timestamp type anywhere in Aperture's storage: every\n" +
			"instant is a BIGINT count of nanoseconds since the Unix epoch.",
		"TIMESTAMP": "There is no per-dialect timestamp type anywhere in Aperture's storage: every\n" +
			"instant is a BIGINT count of nanoseconds since the Unix epoch.",
	},
	"sqlite": {
		"DATETIME": "There is no text timestamp anywhere in Aperture's storage: every instant is\n" +
			"an INTEGER count of nanoseconds since the Unix epoch.",
		"BIGINT": "SQLite spells its 64-bit integer INTEGER. BIGINT is the Postgres spelling and\n" +
			"only looks like a type here — SQLite's affinity rules would accept it, which\n" +
			"is what makes the mistake quiet.",
	},
}

// ---------------------------------------------------------------------------
// The facts a dialect declares
// ---------------------------------------------------------------------------

// schemaFacts is one dialect's schema reduced to the things the dialects have to
// agree about. Deliberately absent: declaration order (DIVERGENCE 2), line
// numbers as anything but a place to send the reader, and comments.
type schemaFacts struct {
	d      dialect
	tables map[string]tableFacts // keyed by lower-cased table name
}

type tableFacts struct {
	name    string
	line    int
	columns map[string]columnFacts // keyed by lower-cased column name
	edges   map[string]edgeFacts   // keyed by edgeKey
}

type columnFacts struct {
	name string
	line int
	// physical is the spelling as written (TEXT, BIGINT).
	physical string
	// constraints is everything after the type, normalized. Compared VERBATIM:
	// NOT NULL, DEFAULT 0 and PRIMARY KEY mean the same in both engines, so a
	// difference there is a real difference.
	constraints string
}

type edgeFacts struct {
	from       string
	columns    []string
	table      string
	refColumns []string
	onDelete   string
	onUpdate   string
	line       int
}

// edgeKey identifies an edge by its ORIGIN — which table, which local columns —
// so that a difference in the target or in either action is reported as a
// changed edge rather than as one edge vanishing and another appearing.
func edgeKey(from string, cols []string) string {
	return fmt.Sprintf("%s(%s)", strings.ToLower(from), strings.ToLower(strings.Join(cols, ",")))
}

// where renders an edge as the reader will want to see it, target and actions
// included.
func (e edgeFacts) where() string {
	return fmt.Sprintf("%s(%s) -> %s(%s) ON DELETE %s ON UPDATE %s",
		e.from, strings.Join(e.columns, ","), e.table, strings.Join(e.refColumns, ","),
		e.onDelete, e.onUpdate)
}

// reduce turns a parse into the comparable fact set. Duplicate table, column or
// edge declarations are ERRORS: the maps would silently keep the last one, and a
// schema that declared apt_grants twice is broken in a way this gate must not
// paper over by comparing whichever copy won.
func reduce(d dialect, tables []tableDef) (schemaFacts, error) {
	facts := schemaFacts{d: d, tables: map[string]tableFacts{}}
	for _, tbl := range tables {
		key := strings.ToLower(tbl.name)
		if _, dup := facts.tables[key]; dup {
			return schemaFacts{}, fmt.Errorf("%s:%d: table %s is declared twice", d.path, tbl.line, tbl.name)
		}
		tf := tableFacts{
			name:    tbl.name,
			line:    tbl.line,
			columns: map[string]columnFacts{},
			edges:   map[string]edgeFacts{},
		}
		for _, col := range tbl.columns {
			ck := strings.ToLower(col.name)
			if _, dup := tf.columns[ck]; dup {
				return schemaFacts{}, fmt.Errorf("%s:%d: table %s declares column %s twice", d.path, col.line, tbl.name, col.name)
			}
			tf.columns[ck] = columnFacts{
				name:        col.name,
				line:        col.line,
				physical:    col.typeName,
				constraints: col.constraints,
			}
		}
		for _, fk := range tbl.foreignKeys {
			ek := edgeKey(tbl.name, fk.columns)
			if _, dup := tf.edges[ek]; dup {
				return schemaFacts{}, fmt.Errorf("%s:%d: table %s declares two foreign keys on the same columns (%s)",
					d.path, fk.line, tbl.name, strings.Join(fk.columns, ", "))
			}
			tf.edges[ek] = edgeFacts{
				from:       tbl.name,
				columns:    fk.columns,
				table:      fk.table,
				refColumns: fk.refColumns,
				onDelete:   fk.onDelete,
				onUpdate:   fk.onUpdate,
				line:       fk.line,
			}
		}
		facts.tables[key] = tf
	}
	return facts, nil
}

// allFacts reads every registered dialect off disk. Fewer than two dialects is a
// FAILURE: a parity gate with one side compares nothing and reports success,
// which is the vacuous-pass shape this package refuses everywhere else.
func allFacts(t *testing.T) []schemaFacts {
	t.Helper()
	if len(dialects) < 2 {
		t.Fatalf("the dialect registry holds %d dialect(s); a parity gate needs at least two schemas to compare.\n"+
			"If a backend was removed, this gate is not 'not applicable' — decide whether parity still means anything and say so here.",
			len(dialects))
	}
	root := repoRoot(t)
	out := make([]schemaFacts, 0, len(dialects))
	for _, d := range dialects {
		// readAndParse is the fail-don't-skip path: a dialect file that is
		// missing, moved or unparseable fails here rather than quietly leaving
		// this gate with one side to compare.
		facts, err := reduce(d, readAndParse(t, root, d))
		if err != nil {
			t.Fatalf("%v", err)
		}
		out = append(out, facts)
	}
	return out
}

// ---------------------------------------------------------------------------
// The gate
// ---------------------------------------------------------------------------

// TestDialectSchemasDeclareTheSameTables is the table-set half. Adding a table
// to one dialect and not the other is build-red here.
func TestDialectSchemasDeclareTheSameTables(t *testing.T) {
	all := allFacts(t)
	for _, p := range diffTables(all) {
		t.Error(p)
	}

	// Anti-vacuity: a parser that lost track of a file would report no tables at
	// all, and "no tables differ" would be a pass. readAndParse already applies
	// each dialect's tableFloor; this is the same tripwire for the UNION, so a
	// parity run that compared nothing cannot look like a parity run that agreed.
	if n := len(unionOfTables(all)); n < highestTableFloor() {
		t.Fatalf("the union of every dialect's tables is %d, below the highest registered tableFloor (%d).\n"+
			"This gate compared almost nothing; fix the parse, or lower the floor in the same commit that removes the tables.",
			n, highestTableFloor())
	}
}

// TestDialectSchemasDeclareTheSameColumns is the column-set half, plus the type
// mapping. Adding a column to one dialect and not the other is build-red, and so
// is changing what a shared column's type MEANS — INTEGER<->BIGINT is a mapping
// entry, not a licence for the types to stop counting.
func TestDialectSchemasDeclareTheSameColumns(t *testing.T) {
	all := allFacts(t)
	problems, compared := diffColumns(all)
	for _, p := range problems {
		t.Error(p)
	}

	// Anti-vacuity. Every table the parser reads has at least one column (a
	// CREATE TABLE with none is a parse error), so the number of columns
	// compared can never legitimately fall below the number of tables. A count
	// under that means the comparison collapsed, and "nothing differed" would be
	// a pass reporting on nothing.
	if want := len(unionOfTables(all)); compared < want {
		t.Fatalf("compared only %d columns across dialects, fewer than the %d tables in the union.\n"+
			"This gate checked almost nothing; fix the parse rather than trusting the pass.", compared, want)
	}
}

// TestDialectSchemasDeclareTheSameForeignKeys is the edge-set half, actions
// included. Adding an edge to one dialect and not the other is build-red, and so
// is changing one edge's ON DELETE or ON UPDATE — which is the failure mode that
// matters most here, because it is invisible in a diff of two files that are
// already hundreds of lines apart and it changes what a delete DOES.
func TestDialectSchemasDeclareTheSameForeignKeys(t *testing.T) {
	all := allFacts(t)
	problems, compared := diffForeignKeys(all)
	for _, p := range problems {
		t.Error(p)
	}

	if compared < highestForeignKeyFloor() {
		t.Fatalf("compared only %d foreign keys across dialects, below the highest registered foreignKeyFloor (%d).\n"+
			"Either edges were removed (lower the floor in the same commit) or the parser stopped reading REFERENCES clauses and this gate is checking nothing.",
			compared, highestForeignKeyFloor())
	}
}

// ---------------------------------------------------------------------------
// The diffs
// ---------------------------------------------------------------------------

// diffTables reports every table that is not declared by every dialect.
func diffTables(all []schemaFacts) []string {
	var problems []string
	for _, name := range unionOfTables(all) {
		var have, missing []string
		for _, f := range all {
			if _, ok := f.tables[name]; ok {
				have = append(have, f.d.name)
			} else {
				missing = append(missing, f.d.name+" ("+f.d.path+")")
			}
		}
		if len(missing) > 0 {
			problems = append(problems, fmt.Sprintf("table %s is declared by %s but is missing from:\n  %s\n\n%s",
				name, strings.Join(have, ", "), strings.Join(missing, "\n  "), parityFooter))
		}
	}
	return problems
}

// diffColumns reports every column-level divergence and returns how many shared
// columns were actually compared, which is what the anti-vacuity floor reads.
func diffColumns(all []schemaFacts) (problems []string, compared int) {
	for _, name := range unionOfTables(all) {
		// A table missing from a dialect is diffTables' finding. Reporting every
		// one of its columns here as well would bury that one line under
		// fourteen.
		present := dialectsDeclaring(all, name)
		if len(present) < 2 {
			continue
		}
		for _, col := range unionOfColumns(present, name) {
			var have, missing []string
			for _, f := range present {
				if _, ok := f.tables[name].columns[col]; ok {
					have = append(have, f.d.name)
				} else {
					missing = append(missing, fmt.Sprintf("%s (%s:%d, table %s)",
						f.d.name, f.d.path, f.tables[name].line, f.tables[name].name))
				}
			}
			if len(missing) > 0 {
				problems = append(problems, fmt.Sprintf("column %s.%s is declared by %s but is missing from:\n  %s\n\n%s",
					name, col, strings.Join(have, ", "), strings.Join(missing, "\n  "), parityFooter))
				continue
			}
			compared++
			problems = append(problems, diffColumn(present, name, col)...)
		}
	}
	return problems, compared
}

// diffColumn diffs one column that every dialect declares: what its type MEANS
// (through the mapping) and everything else about the declaration (verbatim).
func diffColumn(present []schemaFacts, table, col string) []string {
	var problems []string

	// The type, through the mapping.
	logicals := map[string][]string{}
	for _, f := range present {
		c := f.tables[table].columns[col]
		logical, err := logicalType(f.d.name, c.physical)
		if err != nil {
			return []string{fmt.Sprintf("%s:%d: column %s.%s is declared %s: %v\n\n%s",
				f.d.path, c.line, f.tables[table].name, c.name, displayType(c.physical), err, mappingFooter)}
		}
		logicals[logical] = append(logicals[logical], fmt.Sprintf("%s says %s (%s:%d)",
			f.d.name, displayType(c.physical), f.d.path, c.line))
	}
	if len(logicals) > 1 {
		problems = append(problems, fmt.Sprintf("column %s.%s does not mean the same thing in every dialect:\n  %s\n\n%s",
			table, col, strings.Join(flatten(logicals), "\n  "), mappingFooter))
	}

	// Everything else, verbatim. NOT NULL, DEFAULT 0 and PRIMARY KEY are spelled
	// identically in both engines, so a difference here is a real difference and
	// not a dialect requirement. If one ever IS a dialect requirement, it belongs
	// in a named mapping at the top of this file with the reasoning beside it —
	// not in a skip.
	rest := map[string][]string{}
	for _, f := range present {
		c := f.tables[table].columns[col]
		rest[c.constraints] = append(rest[c.constraints], fmt.Sprintf("%s says %q (%s:%d)",
			f.d.name, c.constraints, f.d.path, c.line))
	}
	if len(rest) > 1 {
		problems = append(problems, fmt.Sprintf("column %s.%s is not declared the same way in every dialect:\n  %s\n\n%s",
			table, col, strings.Join(flatten(rest), "\n  "), parityFooter))
	}
	return problems
}

// diffForeignKeys reports every edge-level divergence and returns how many
// shared edges were compared.
func diffForeignKeys(all []schemaFacts) (problems []string, compared int) {
	for _, name := range unionOfTables(all) {
		present := dialectsDeclaring(all, name)
		if len(present) < 2 {
			continue
		}
		for _, key := range unionOfEdges(present, name) {
			var have, missing []string
			for _, f := range present {
				if e, ok := f.tables[name].edges[key]; ok {
					have = append(have, fmt.Sprintf("%s (%s:%d) %s", f.d.name, f.d.path, e.line, e.where()))
				} else {
					missing = append(missing, f.d.name+" ("+f.d.path+")")
				}
			}
			if len(missing) > 0 {
				problems = append(problems, fmt.Sprintf("foreign key %s is declared by:\n  %s\nbut is missing from:\n  %s\n\n%s",
					key, strings.Join(have, "\n  "), strings.Join(missing, "\n  "), edgeFooter))
				continue
			}
			compared++
			problems = append(problems, diffEdge(present, name, key)...)
		}
	}
	return problems, compared
}

// diffEdge diffs one edge every dialect declares: its target, its referenced
// columns, and both referential actions. The actions are compared INDIVIDUALLY
// so the failure names which one moved.
func diffEdge(present []schemaFacts, table, key string) []string {
	var problems []string
	for _, f := range []struct {
		what string
		get  func(edgeFacts) string
	}{
		{"target table", func(e edgeFacts) string { return e.table }},
		{"referenced columns", func(e edgeFacts) string { return strings.Join(e.refColumns, ",") }},
		{"ON DELETE", func(e edgeFacts) string { return e.onDelete }},
		{"ON UPDATE", func(e edgeFacts) string { return e.onUpdate }},
	} {
		byValue := map[string][]string{}
		for _, sf := range present {
			e := sf.tables[table].edges[key]
			v := f.get(e)
			byValue[v] = append(byValue[v], fmt.Sprintf("%s says %q (%s:%d)", sf.d.name, v, sf.d.path, e.line))
		}
		if len(byValue) > 1 {
			problems = append(problems, fmt.Sprintf("foreign key %s: the dialects disagree about its %s:\n  %s\n\n%s",
				key, f.what, strings.Join(flatten(byValue), "\n  "), edgeFooter))
		}
	}
	return problems
}

// ---------------------------------------------------------------------------
// The mapping's own gates
// ---------------------------------------------------------------------------

// TestEveryDialectHasATypeMapping keeps the mapping from falling behind the
// registry in either direction. A dialect with no mapping would fail every
// column with "unmapped type", which is loud but unhelpful; a mapping for a
// dialect nobody registered is a mapping nothing reads.
func TestEveryDialectHasATypeMapping(t *testing.T) {
	registered := map[string]bool{}
	for _, d := range dialects {
		registered[d.name] = true
		if len(physicalTypes[d.name]) == 0 {
			t.Errorf("dialect %q has no entry in physicalTypes.\n"+
				"Every dialect needs an explicit spelling->meaning map; the parity gate refuses a\n"+
				"type it was not told the meaning of rather than assuming one.", d.name)
		}
	}
	for name := range physicalTypes {
		if !registered[name] {
			t.Errorf("physicalTypes has a map for %q, which is not in the dialect registry", name)
		}
	}
	for name := range refusedTypes {
		if !registered[name] {
			t.Errorf("refusedTypes has a map for %q, which is not in the dialect registry", name)
		}
	}
	// A spelling cannot be both mapped and refused; that would make the outcome
	// depend on which lookup ran first.
	for name, refused := range refusedTypes {
		for spelling, why := range refused {
			if _, ok := physicalTypes[name][spelling]; ok {
				t.Errorf("%s: %s is both mapped and refused", name, spelling)
			}
			if strings.TrimSpace(why) == "" {
				t.Errorf("%s: %s is refused with no reason; a refusal a maintainer cannot argue with is a refusal they will delete", name, spelling)
			}
		}
	}
}

// TestTheTypeMappingRefusesTheNarrowingSpellings pins the one mapping decision
// that would be invisible if it were wrong. Postgres INTEGER is 32 bits and
// SQLite INTEGER is 64; mapping both to the same logical type is a one-line edit
// that makes this whole gate agree a narrowed column is fine. It is asserted
// here rather than only written in a comment, because a comment is not a gate.
func TestTheTypeMappingRefusesTheNarrowingSpellings(t *testing.T) {
	for _, tc := range []struct{ dialect, spelling string }{
		{"postgres", "INTEGER"},
		{"postgres", "SMALLINT"},
		{"sqlite", "BIGINT"},
	} {
		if _, ok := physicalTypes[tc.dialect][tc.spelling]; ok {
			t.Errorf("physicalTypes[%q][%q] exists.\n"+
				"That spelling means a DIFFERENT domain in this dialect than the peer spelling it\n"+
				"would be paired with, so mapping it silently blesses a narrowed column. If this is\n"+
				"deliberate, the argument belongs here in writing.", tc.dialect, tc.spelling)
		}
		if _, err := logicalType(tc.dialect, tc.spelling); err == nil {
			t.Errorf("logicalType(%q, %q) returned no error; the gate would accept the spelling", tc.dialect, tc.spelling)
		}
	}
}

// ---------------------------------------------------------------------------
// The mapping, applied
// ---------------------------------------------------------------------------

// logicalType resolves one dialect's physical spelling to the logical type it
// means. An unknown spelling is an ERROR, never a pass-through: the whole value
// of an explicit mapping is that a type nobody has decided about stops the build.
func logicalType(dialect, physical string) (string, error) {
	if physical == "" {
		return "", fmt.Errorf("the column declares no type at all")
	}
	spelling := strings.ToUpper(physical)
	if logical, ok := physicalTypes[dialect][spelling]; ok {
		return logical, nil
	}
	if why, ok := refusedTypes[dialect][spelling]; ok {
		return "", fmt.Errorf("%s is not allowed in the %s schema.\n%s", spelling, dialect, why)
	}
	return "", fmt.Errorf("%s is not in the %s type mapping, so this gate does not know what it MEANS\n"+
		"and cannot tell whether the other dialect's column agrees with it", spelling, dialect)
}

// displayType renders a spelling for a message, naming the empty case rather
// than printing "".
func displayType(physical string) string {
	if physical == "" {
		return "(no type)"
	}
	return physical
}

// ---------------------------------------------------------------------------
// Set helpers
// ---------------------------------------------------------------------------

// dialectsDeclaring narrows the dialect list to the ones that declare a table.
func dialectsDeclaring(all []schemaFacts, table string) []schemaFacts {
	out := make([]schemaFacts, 0, len(all))
	for _, f := range all {
		if _, ok := f.tables[table]; ok {
			out = append(out, f)
		}
	}
	return out
}

func unionOfTables(all []schemaFacts) []string {
	seen := map[string]bool{}
	for _, f := range all {
		for name := range f.tables {
			seen[name] = true
		}
	}
	return sortedKeys(seen)
}

func unionOfColumns(all []schemaFacts, table string) []string {
	seen := map[string]bool{}
	for _, f := range all {
		for name := range f.tables[table].columns {
			seen[name] = true
		}
	}
	return sortedKeys(seen)
}

func unionOfEdges(all []schemaFacts, table string) []string {
	seen := map[string]bool{}
	for _, f := range all {
		for key := range f.tables[table].edges {
			seen[key] = true
		}
	}
	return sortedKeys(seen)
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// flatten turns a value->descriptions map into one stable list, so a failure
// message reads the same on every run rather than in map order.
func flatten(m map[string][]string) []string {
	var out []string
	for _, ds := range m {
		out = append(out, ds...)
	}
	sort.Strings(out)
	return out
}

func highestTableFloor() int {
	n := 0
	for _, d := range dialects {
		if d.tableFloor > n {
			n = d.tableFloor
		}
	}
	return n
}

func highestForeignKeyFloor() int {
	n := 0
	for _, d := range dialects {
		if d.foreignKeyFloor > n {
			n = d.foreignKeyFloor
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// The failure footers
// ---------------------------------------------------------------------------

const parityFooter = `The dialect schemas describe ONE database in two spellings. A table or column
that exists in one and not the other is not a dialect difference; it is a
backend that stores less than its peer, and storagetest asserts the two are
behaviourally identical. Land the change in EVERY storage/*/schema.sql in the
same commit.

If the difference is genuinely required by an engine, it belongs in the explicit
mapping at the top of internal/schemagate/schema_parity_test.go, named, with the
reasoning beside it. Do not widen the gate to make one schema fit.`

const mappingFooter = `Physical type spellings differ between dialects on purpose and are mapped, not
ignored: SQLite spells its 64-bit integer INTEGER and Postgres spells it BIGINT,
and both mean the same int64. The mapping lives at the top of
internal/schemagate/schema_parity_test.go.

A spelling the mapping does not know is a FAILURE rather than a pass, so that
adding a type to one schema forces a decision about what it means in the other.
Add the entry — do not reach for a blanket exemption, which would also stop this
gate noticing a created_at that had become TEXT in one backend.`

const edgeFooter = `Foreign keys are what make a delete refuse instead of orphaning rows, so an edge
present in one dialect and not the other — or carrying a different ON DELETE —
is a backend that behaves differently under exactly the operation the edge
exists to govern. ON DELETE CASCADE appears on exactly three edges (the join
rows an entity owns); every other edge is RESTRICT, and ON UPDATE is RESTRICT
throughout because an id in Aperture is immutable.

Change an edge in every storage/*/schema.sql in the same commit, and update the
referential-integrity section of each file's header, which states the counts.`
