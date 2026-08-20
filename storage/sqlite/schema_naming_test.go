package sqlite

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/frankbardon/aperture/internal/sqlreserved"
)

// This file is the gate that keeps Aperture's database-identifier convention
// from rotting, and — because this effort deliberately ships no prose docs — it
// is also the only place that convention is written down. If you are reading it
// because a build turned red, everything you need is here.
//
// # The convention
//
//  1. EVERY table Aperture owns is named apt_<thing>. Blanket, no exceptions:
//     apt_accounts, apt_grants, apt_audit_log. A prefixed name can never collide
//     with a SQL keyword, and in a database Aperture shares with its host it is
//     immediately obvious which tables are ours.
//
//  2. A COLUMN is prefixed apt_ only when its WHOLE name spells a reserved word,
//     counting the singular of a plural: apt_action, apt_actions, apt_identity,
//     apt_grants, apt_object. A compound name (account_id, object_type, role_id)
//     can never be a keyword, so it is left alone. Prefixing more than the rule
//     requires is noise, not safety.
//
//  3. Index names keep their idx_ prefix and track the renamed table:
//     idx_apt_grants_account_subject.
//
// This is a DATABASE-IDENTIFIER convention only. Go types, struct fields, JSON
// and wire field names, and CLI flags are untouched — a `grants` local variable
// and an `object` parameter in sqlite.go are not this gate's business.
//
// # Why it exists
//
// Aperture writes hand-written SQL against an embedded SQLite database, and has
// no ORM, no query builder and no migration tool to quote identifiers for it. A
// column named `action` or a table named `grants` is a latent parse error: it
// happens to work in SQLite today, breaks the moment a statement is ported to
// another engine, and forces every future statement that touches it to remember
// the quotes. The alternative fix — quoting identifiers everywhere — is the one
// this project rejected: it makes every statement noisier to defend one word.
//
// The rename was a HARD BREAK. There is no schema versioning here (no
// user_version pragma, no migration table), so an old database does not upgrade.
// That price was paid once. Re-introducing a reserved word would mean paying it
// again, which is exactly what this gate prevents.
//
// # What the gate actually does
//
// It PARSES schema.sql — tokenizer, statement splitter, CREATE TABLE reader —
// rather than pattern-matching the text, following the house precedent set by
// TestDriverValueMappingTableMatchesTheTypeSwitch (sqlprovider/values_test.go),
// which reads Go source with go/ast instead of grepping. A text scan is not
// merely inelegant here, it is WRONG: schema.sql's own header comment quotes the
// old names to explain the convention, string defaults can contain anything, and
// `apt_grants` is both a table name and a column name on apt_templates — a scan
// cannot tell those two positions apart. The parser can, and does.
//
// Reserved-word membership comes from internal/sqlreserved, a vendored snapshot
// of ten keyword lists carrying per-word provenance, so a failure can name the
// lists that object rather than only asserting that some list does.
//
// A missing or unparseable schema.sql FAILS; it never skips, matching
// rules/editor_js_contract_test.go. A gate that quietly skips is worse than no
// gate, because it also tells you it ran.
//
// # If this gate fires on a name you believe is legitimate
//
// Weaken nothing. Add a narrow, named, justified exception with the reasoning
// written next to it — that decision was made when this gate was designed, and a
// reviewer is entitled to see the argument rather than a relaxed rule. Note that
// a word reserved ONLY by "SQL Server future keywords" is reserved by no
// shipping engine today; that is the weakest case for a rename and the most
// likely candidate for an exception. Every other source is a live constraint.

// schemaFile is schema.sql relative to this package directory, which is `go
// test`'s working directory. It is read from DISK on purpose, matching
// rules/editor_js_contract_test.go: a file that has moved must FAIL rather than
// vanish into a skip. The gate then compares what it read against the embedded
// copy, so it can never audit a file the Store does not execute.
const schemaFile = "schema.sql"

// schemaDisplayPath is how the file is named in failure messages: repo-relative,
// so the path can be pasted into an editor from wherever the test was run.
const schemaDisplayPath = "storage/sqlite/schema.sql"

// aptPrefix is the namespace every Aperture table carries, and the escape hatch
// for the handful of columns whose bare name is reserved.
const aptPrefix = "apt_"

// knownTableFloor is an anti-vacuity tripwire: the schema had this many tables
// when the gate landed, and a parser that silently stopped understanding the
// file would report fewer and then pass with nothing to check. Adding a table is
// free. REMOVING one trips this deliberately — lower the number in the same
// commit that removes the table, and only then.
const knownTableFloor = 14

// ---------------------------------------------------------------------------
// The gate
// ---------------------------------------------------------------------------

// TestSchemaUsesNoReservedIdentifiers is the gate. It runs in `make test`; it is
// not behind an environment variable, because a convention nobody is forced to
// keep is a convention that is already gone.
func TestSchemaUsesNoReservedIdentifiers(t *testing.T) {
	src, err := os.ReadFile(schemaFile)
	if err != nil {
		// Fail, never skip. If the schema moved, this gate is not "not
		// applicable" — it is broken, and so is the store that embeds the file.
		t.Fatalf("read %s: %v\n\nThis gate audits the embedded SQLite schema and cannot run without it.\nIf the file moved, move schemaFile in this test with it.", schemaDisplayPath, err)
	}

	if got := string(src); got != schema {
		t.Fatalf("%s on disk differs from the copy embedded at sqlite.go's //go:embed directive (%d bytes on disk, %d embedded).\nThe gate would be auditing a file the Store never executes.", schemaDisplayPath, len(got), len(schema))
	}

	tables, err := parseCreateTables(string(src))
	if err != nil {
		t.Fatalf("parse %s: %v\n\nThis gate parses the schema rather than scanning it, so SQL it cannot read is a failure.\nEither the schema uses syntax the parser does not cover — teach it — or the schema is malformed.", schemaDisplayPath, err)
	}

	if len(tables) < knownTableFloor {
		t.Fatalf("parsed only %d CREATE TABLE statements out of %s, expected at least %d.\nEither a table was removed (lower knownTableFloor in the same commit) or the parser lost track of the file and this gate is checking nothing.", len(tables), schemaDisplayPath, knownTableFloor)
	}

	if vs := checkNaming(tables); len(vs) > 0 {
		t.Error(report(vs))
	}
}

// ---------------------------------------------------------------------------
// The rule
// ---------------------------------------------------------------------------

// violation is one identifier that breaks the convention, carrying everything a
// maintainer needs to act: what it is, where it is, and who objects to it.
type violation struct {
	kind  string // "table" or "column"
	name  string // the offending identifier, as written
	table string // the table it belongs to ("" for a table violation)
	line  int    // 1-based line in schema.sql
	// hit is the reserved-word match, if any. A table violation can be clean
	// under the reserved lists and still fail, because the apt_ rule for tables
	// is blanket.
	hit    reservedHit
	hasHit bool
}

// reservedHit records WHICH reserved word an identifier collided with and which
// lists reserve it. The word can differ from the identifier when the match came
// through singularization (grants -> GRANT).
type reservedHit struct {
	word       string // the reserved spelling, upper-cased
	sources    sqlreserved.Source
	singular   bool   // true when the match required singularizing the identifier
	matchedAs  string // the candidate that actually matched, as fed to the lookup
	identifier string // the identifier that was checked
}

// checkNaming applies the convention to a parsed schema and returns every
// violation, in source order.
func checkNaming(tables []tableDef) []violation {
	var vs []violation
	for _, tbl := range tables {
		if !hasAptPrefix(tbl.name) {
			v := violation{kind: "table", name: tbl.name, line: tbl.line}
			// A non-prefixed table is a violation whether or not it is reserved,
			// but if it IS reserved the reader deserves to know.
			if hit, ok := reservedMatch(tbl.name); ok {
				v.hit, v.hasHit = hit, true
			}
			vs = append(vs, v)
		}
		for _, col := range tbl.columns {
			// apt_ is the fix, so a prefixed column is by construction fine.
			if hasAptPrefix(col.name) {
				continue
			}
			hit, ok := reservedMatch(col.name)
			if !ok {
				continue
			}
			vs = append(vs, violation{
				kind: "column", name: col.name, table: tbl.name, line: col.line,
				hit: hit, hasHit: true,
			})
		}
	}
	return vs
}

// hasAptPrefix reports whether an identifier already carries the namespace. SQL
// identifiers are case-insensitive, so APT_GRANTS counts.
func hasAptPrefix(name string) bool {
	return strings.HasPrefix(strings.ToLower(name), aptPrefix)
}

// reservedMatch reports whether an identifier's WHOLE name is a reserved word,
// applying singular-family matching.
//
// Whole-name matching is what keeps compounds safe: account_id and object_type
// contain a keyword but are not one, and no engine cares. Singular-family
// matching is what catches the plurals a table-shaped schema naturally reaches
// for — `grants` is GRANT wearing an s, and renaming the column to `grants`
// rather than apt_grants would re-open exactly the hole this effort closed.
func reservedMatch(name string) (reservedHit, bool) {
	for i, cand := range singularFamily(name) {
		if src := sqlreserved.Sources(cand); src != 0 {
			return reservedHit{
				word:       strings.ToUpper(cand),
				sources:    src,
				singular:   i > 0,
				matchedAs:  cand,
				identifier: name,
			}, true
		}
	}
	return reservedHit{}, false
}

// singularFamily returns the identifier followed by the singular forms worth
// testing, most-literal first, so a direct hit is reported as a direct hit.
// English pluralization is not a solved problem and this is not an attempt at
// one — it covers the three endings a SQL schema actually produces.
func singularFamily(name string) []string {
	out := []string{name}
	lower := strings.ToLower(name)
	switch {
	case strings.HasSuffix(lower, "ies") && len(name) > 3:
		out = append(out, name[:len(name)-3]+"y") // identities -> identity
	case strings.HasSuffix(lower, "ses") && len(name) > 3:
		out = append(out, name[:len(name)-2]) // aliases -> alias
	case strings.HasSuffix(lower, "s") && !strings.HasSuffix(lower, "ss") && len(name) > 1:
		out = append(out, name[:len(name)-1]) // grants -> grant
	}
	return out
}

// ---------------------------------------------------------------------------
// The failure message
// ---------------------------------------------------------------------------

// report renders every violation as ONE failure, so the convention, the
// offending names and the fix arrive together rather than scattered across
// several t.Error blocks.
func report(vs []violation) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s breaks Aperture's database-identifier convention (%s):\n\n",
		schemaDisplayPath, plural(len(vs), "problem"))
	for _, v := range vs {
		b.WriteString(v.describe())
	}
	b.WriteString(conventionFooter)
	return b.String()
}

// describe renders one violation: what, where, who objects, and what to do.
func (v violation) describe() string {
	var b strings.Builder
	switch v.kind {
	case "table":
		fmt.Fprintf(&b, "  line %d: table %q is not prefixed %s\n", v.line, v.name, aptPrefix)
		fmt.Fprintf(&b, "      Every table Aperture owns is namespaced. Rename it to %s%s.\n",
			aptPrefix, strings.TrimPrefix(v.name, aptPrefix))
	case "column":
		fmt.Fprintf(&b, "  line %d: column %q on table %s spells a SQL reserved word\n", v.line, v.name, v.table)
	}
	if v.hasHit {
		h := v.hit
		if h.singular {
			fmt.Fprintf(&b, "      Reserved as %s, matched as the singular of %q.\n", h.word, h.identifier)
		} else {
			fmt.Fprintf(&b, "      Reserved as %s.\n", h.word)
		}
		fmt.Fprintf(&b, "      Reserved by: %s.\n", h.sources)
		if h.sources == sqlreserved.SQLServerFuture {
			b.WriteString("      NOTE: that is the SQL Server \"future keywords\" list ALONE — no shipping\n" +
				"      engine and no ISO revision reserves this word today. It is the one case\n" +
				"      where an exception is arguable; make the argument in writing.\n")
		}
	}
	if v.kind == "column" {
		fmt.Fprintf(&b, "      Rename it to %s%s.\n", aptPrefix, v.name)
	}
	b.WriteString("      Rename it in " + schemaDisplayPath + " AND in every SQL string literal in\n" +
		"      storage/sqlite/sqlite.go — including the table names passed to deleteByID as\n" +
		"      plain Go string arguments, which no grep for `FROM <name>` will find.\n\n")
	return b.String()
}

// conventionFooter restates the rule and the reasoning under every failure, so
// the message is actionable without opening this file — or any other.
const conventionFooter = `The convention:
  * Every table Aperture owns is named apt_<thing>. Blanket, no exceptions.
  * A column is prefixed apt_ only when its WHOLE name is a reserved word,
    counting the singular of a plural (grants -> GRANT). Compound names
    (account_id, object_type, role_id) never qualify and are left alone.
  * Indexes keep idx_ and track the table: idx_apt_grants_account_subject.
  * Database identifiers only. Go types, struct fields, wire fields and CLI
    flags are unaffected.

Why: Aperture's SQL is hand-written with no ORM and no query builder to quote
identifiers for it, and there is no schema versioning, so a rename is a hard
break for every existing database. That break was paid once, deliberately.
Re-introducing a reserved word would mean paying it again.

Reserved-word membership comes from internal/sqlreserved, a vendored snapshot of
ten keyword lists (see the header of internal/sqlreserved/words.go for the
sources, the fetch date, and the caveats). Do not weaken this gate to make a name
fit; if a name is genuinely defensible, add a narrow exception with the reasoning
written beside it.`

// plural renders "1 problem" / "3 problems" so the header reads like English.
func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// ---------------------------------------------------------------------------
// A small SQL parser
//
// Only as much SQL as schema.sql speaks: comments, string and delimited-
// identifier literals, statements, and the shape of CREATE TABLE. Everything
// else is tokenized and stepped over. It exists so the gate can tell a table
// name from a column name from a word inside a comment — the three positions a
// text scan conflates.
// ---------------------------------------------------------------------------

type tokenKind int

const (
	tokWord   tokenKind = iota // bare identifier or keyword
	tokQuoted                  // delimited identifier: "x", `x`, [x]
	tokString                  // 'string literal'
	tokPunct                   // ( ) , ; and any other single character
)

// token is one lexical unit. text has any delimiters stripped, so a quoted
// identifier compares equal to its bare spelling — quoting a reserved word is
// the workaround this convention rejects, not an escape from it.
type token struct {
	kind tokenKind
	text string
	line int
}

// isWord reports whether the token is the given bare keyword, case-insensitively.
// A DELIMITED identifier is never a keyword: "table" is a column named table.
func (t token) isWord(kw string) bool {
	return t.kind == tokWord && strings.EqualFold(t.text, kw)
}

// isIdent reports whether the token can name something.
func (t token) isIdent() bool { return t.kind == tokWord || t.kind == tokQuoted }

// isPunct reports whether the token is the given punctuation character.
func (t token) isPunct(p string) bool { return t.kind == tokPunct && t.text == p }

// identName is the token's name with any schema qualifier dropped, so
// main.apt_grants reads as apt_grants.
func (t token) identName() string {
	if t.kind != tokWord {
		return t.text
	}
	if i := strings.LastIndex(t.text, "."); i >= 0 {
		return t.text[i+1:]
	}
	return t.text
}

// tableDef is one CREATE TABLE: its name and the columns it DEFINES. Table
// constraints (PRIMARY KEY (a, b), FOREIGN KEY ..., CHECK ...) are not column
// definitions and are deliberately absent — the identifiers inside them are
// references to columns already declared above, and reporting them again would
// double every finding.
type tableDef struct {
	name    string
	line    int
	columns []columnDef
}

type columnDef struct {
	name string
	line int
}

// constraintStarters are the bare keywords that open a TABLE constraint rather
// than a column definition. Bare only: a DELIMITED "check" is a column.
var constraintStarters = map[string]bool{
	"CONSTRAINT": true,
	"PRIMARY":    true,
	"UNIQUE":     true,
	"CHECK":      true,
	"FOREIGN":    true,
}

// parseCreateTables reads every CREATE TABLE out of a SQL script. Anything that
// is not a CREATE TABLE — CREATE INDEX, pragmas, comments — is stepped over.
// Malformed SQL is an ERROR, never a silent zero result.
func parseCreateTables(src string) ([]tableDef, error) {
	toks, err := tokenize(src)
	if err != nil {
		return nil, err
	}
	stmts, err := splitStatements(toks)
	if err != nil {
		return nil, err
	}
	var out []tableDef
	for _, stmt := range stmts {
		tbl, ok, err := parseCreateTable(stmt)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, tbl)
		}
	}
	return out, nil
}

// tokenize splits SQL into tokens, discarding comments and whitespace.
func tokenize(src string) ([]token, error) {
	var out []token
	line := 1
	for i := 0; i < len(src); {
		c := src[i]
		switch {
		case c == '\n':
			line++
			i++
		case c == ' ' || c == '\t' || c == '\r' || c == '\v' || c == '\f':
			i++
		case c == '-' && i+1 < len(src) && src[i+1] == '-':
			// Line comment. schema.sql's header comment quotes the OLD, reserved
			// names on purpose, to explain the convention; dropping comments here
			// is what makes that safe.
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			end := strings.Index(src[i+2:], "*/")
			if end < 0 {
				return nil, fmt.Errorf("line %d: unterminated /* block comment", line)
			}
			consumed := src[i : i+2+end+2]
			line += strings.Count(consumed, "\n")
			i += len(consumed)
		case c == '\'' || c == '"' || c == '`' || c == '[':
			closer, kind := c, tokQuoted
			switch c {
			case '[':
				closer = ']'
			case '\'':
				kind = tokString
			}
			text, n, err := scanDelimited(src[i:], c, closer)
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", line, err)
			}
			out = append(out, token{kind: kind, text: text, line: line})
			line += strings.Count(src[i:i+n], "\n")
			i += n
		case isIdentByte(c):
			j := i
			for j < len(src) && isIdentByte(src[j]) {
				j++
			}
			out = append(out, token{kind: tokWord, text: src[i:j], line: line})
			i = j
		default:
			out = append(out, token{kind: tokPunct, text: string(c), line: line})
			i++
		}
	}
	return out, nil
}

// scanDelimited reads one delimited run starting at s[0] == open, returning the
// contents with delimiters stripped and the number of bytes consumed. When the
// delimiters are the same character it is doubled to escape ('it”s"), which is
// how SQL escapes both string literals and quoted identifiers.
func scanDelimited(s string, open, closer byte) (string, int, error) {
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		if s[i] != closer {
			b.WriteByte(s[i])
			continue
		}
		if open == closer && i+1 < len(s) && s[i+1] == closer {
			b.WriteByte(closer)
			i++
			continue
		}
		return b.String(), i + 1, nil
	}
	return "", 0, fmt.Errorf("unterminated %c...%c literal", open, closer)
}

// isIdentByte reports whether a byte can appear in a bare identifier or numeric
// literal. '.' is included so a schema-qualified name arrives as one token;
// identName drops the qualifier.
func isIdentByte(c byte) bool {
	return c == '_' || c == '$' || c == '.' ||
		(c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
		c >= 0x80
}

// splitStatements cuts a token stream at top-level semicolons. A semicolon
// inside parentheses does not end a statement, and an unterminated or unbalanced
// statement is an error rather than a truncated result.
func splitStatements(toks []token) ([][]token, error) {
	var stmts [][]token
	var cur []token
	depth := 0
	for _, t := range toks {
		if t.kind == tokPunct {
			switch t.text {
			case "(":
				depth++
			case ")":
				depth--
				if depth < 0 {
					return nil, fmt.Errorf("line %d: unbalanced ')'", t.line)
				}
			case ";":
				if depth == 0 {
					if len(cur) > 0 {
						stmts = append(stmts, cur)
						cur = nil
					}
					continue
				}
			}
		}
		cur = append(cur, t)
	}
	if depth != 0 {
		return nil, fmt.Errorf("unbalanced parentheses: %d '(' left open at end of input", depth)
	}
	if len(cur) > 0 {
		return nil, fmt.Errorf("line %d: statement is not terminated with ';'", cur[0].line)
	}
	return stmts, nil
}

// parseCreateTable reads one statement as a CREATE TABLE. The bool reports
// whether it was one; a statement that is not (CREATE INDEX, anything else) is
// stepped over without complaint, while a CREATE TABLE the parser cannot read is
// an error.
func parseCreateTable(stmt []token) (tableDef, bool, error) {
	i := 0
	if len(stmt) == 0 || !stmt[i].isWord("CREATE") {
		return tableDef{}, false, nil
	}
	i++
	for i < len(stmt) && (stmt[i].isWord("TEMP") || stmt[i].isWord("TEMPORARY")) {
		i++
	}
	if i >= len(stmt) || !stmt[i].isWord("TABLE") {
		return tableDef{}, false, nil // CREATE INDEX, CREATE VIEW, ...
	}
	i++
	if i+2 < len(stmt) && stmt[i].isWord("IF") && stmt[i+1].isWord("NOT") && stmt[i+2].isWord("EXISTS") {
		i += 3
	}
	if i >= len(stmt) || !stmt[i].isIdent() {
		return tableDef{}, false, fmt.Errorf("line %d: CREATE TABLE without a table name", stmt[0].line)
	}
	nameTok := stmt[i]
	i++

	if i >= len(stmt) || !stmt[i].isPunct("(") {
		// CREATE TABLE ... AS SELECT has no column list. Aperture's schema never
		// uses it, and a gate that silently ignored a table it could not read
		// would be a hole, so refuse rather than skip.
		return tableDef{}, false, fmt.Errorf("line %d: CREATE TABLE %s: expected '(' and a column list", nameTok.line, nameTok.identName())
	}
	body, err := parenBody(stmt[i:])
	if err != nil {
		return tableDef{}, false, fmt.Errorf("line %d: CREATE TABLE %s: %w", nameTok.line, nameTok.identName(), err)
	}

	tbl := tableDef{name: nameTok.identName(), line: nameTok.line}
	for _, item := range splitTopLevel(body) {
		if len(item) == 0 {
			return tableDef{}, false, fmt.Errorf("line %d: CREATE TABLE %s: empty column definition", nameTok.line, tbl.name)
		}
		first := item[0]
		// A table constraint is not a column definition. Only a BARE keyword
		// opens one, so a delimited "check" or "primary" stays a column and is
		// still checked — which is the point: quoting is not an exemption.
		if first.kind == tokWord && constraintStarters[strings.ToUpper(first.text)] {
			continue
		}
		if !first.isIdent() {
			return tableDef{}, false, fmt.Errorf("line %d: CREATE TABLE %s: expected a column name, got %q", first.line, tbl.name, first.text)
		}
		tbl.columns = append(tbl.columns, columnDef{name: first.identName(), line: first.line})
	}
	if len(tbl.columns) == 0 {
		return tableDef{}, false, fmt.Errorf("line %d: CREATE TABLE %s: no column definitions", nameTok.line, tbl.name)
	}
	return tbl, true, nil
}

// parenBody returns the tokens inside the parenthesised group that toks opens.
func parenBody(toks []token) ([]token, error) {
	depth := 0
	for i, t := range toks {
		switch {
		case t.isPunct("("):
			depth++
		case t.isPunct(")"):
			depth--
			if depth == 0 {
				return toks[1:i], nil
			}
		}
	}
	return nil, fmt.Errorf("unclosed '('")
}

// splitTopLevel splits a column list on commas that are not nested inside
// parentheses, so DEFAULT (a, b) and CHECK (x IN (1, 2)) stay whole.
func splitTopLevel(toks []token) [][]token {
	var out [][]token
	var cur []token
	depth := 0
	for _, t := range toks {
		switch {
		case t.isPunct("("):
			depth++
		case t.isPunct(")"):
			depth--
		case t.isPunct(",") && depth == 0:
			out = append(out, cur)
			cur = nil
			continue
		}
		cur = append(cur, t)
	}
	if len(cur) > 0 || len(out) > 0 {
		out = append(out, cur)
	}
	return out
}
