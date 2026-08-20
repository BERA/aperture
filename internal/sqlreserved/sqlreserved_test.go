package sqlreserved

import (
	"os"
	"strings"
	"testing"
)

// TestUnionLoaded is the cheapest possible proof that the generated table is
// actually linked in. A parser that silently produced nothing would leave an
// empty map and every other assertion here would pass vacuously.
func TestUnionLoaded(t *testing.T) {
	if got := Len(); got < 500 {
		t.Fatalf("union holds %d words; the ten consulted lists cannot produce fewer than 500 distinct words — the table did not load or gen.go parsed nothing", got)
	}
	if w := Words(); len(w) != Len() {
		t.Fatalf("Words() returned %d entries but Len() is %d", len(w), Len())
	}
}

// TestEverySourceContributes proves each of the ten lists parsed and landed. If
// a vendor changes its markup, gen.go is meant to fail loudly — but if it ever
// wrote a table with a whole source missing, this catches it.
func TestEverySourceContributes(t *testing.T) {
	var seen Source
	for _, w := range Words() {
		seen |= Sources(w)
	}
	if missing := All &^ seen; missing != 0 {
		t.Fatalf("no word carries these sources: %s — that list is missing from the snapshot", missing)
	}
}

// TestPresentWords is the sanity half of the load check: five words that must be
// in any honest union of these lists. They are also exactly the five identifiers
// the schema audit turned up, so this doubles as a guard on the audit's inputs.
func TestPresentWords(t *testing.T) {
	for _, w := range []string{"GROUP", "GRANT", "ACTION", "IDENTITY", "OBJECT"} {
		if !IsReserved(w) {
			t.Errorf("%s is absent from the union; it is reserved by at least one consulted list, so the snapshot is truncated or mis-parsed", w)
		}
	}
}

// TestDeliberateAbsences is a tripwire, not a coverage test.
//
// TARGET, VERSION and NAME were all assumed reserved before the lists were
// actually read, and none of them is: all three appear in the PostgreSQL
// appendix marked non-reserved in every column, and nowhere else. A regenerated
// snapshot that suddenly contains one of them is far more likely to mean the
// PostgreSQL parser stopped distinguishing the four status columns than that a
// vendor reserved a new word. Investigate the parser before believing the list.
func TestDeliberateAbsences(t *testing.T) {
	for _, w := range []string{"TARGET", "VERSION", "NAME"} {
		if s := Sources(w); s != 0 {
			t.Errorf("%s is present, attributed to %s — it is reserved nowhere. "+
				"Suspect a parsing regression (most likely the PostgreSQL appendix's four status columns being collapsed into one) before assuming a vendor change.", w, s)
		}
	}
}

// TestProvenanceIsExact pins the source set of every identifier the schema audit
// ruled on. These are the load-bearing verdicts: whether a column gets prefixed
// follows directly from which lists reserve its name, and OBJECT in particular
// rests on nothing but Microsoft's future-keywords list. If one of these sets
// changes, the rename decision it justified needs re-reading, not a test edit.
func TestProvenanceIsExact(t *testing.T) {
	cases := map[string]Source{
		"ACTION":   SQLite | SQL92 | ODBC | SQLServerFuture,
		"IDENTITY": SQL2023 | SQL2016 | SQL92 | TSQL | ODBC,
		"GRANT":    PostgreSQL | SQL2023 | SQL2016 | SQL92 | TSQL | ODBC | Oracle | MariaDB,
		"GROUP":    SQLite | PostgreSQL | SQL2023 | SQL2016 | SQL92 | TSQL | ODBC | Oracle | MariaDB,
		"OBJECT":   SQLServerFuture,
	}
	for word, want := range cases {
		if got := Sources(word); got != want {
			t.Errorf("Sources(%q) = %s; want %s", word, got, want)
		}
	}
}

// TestCaveatsHold asserts the three research caveats against the data itself,
// so a re-scrape that reintroduces the mistakes fails here rather than silently
// widening the rename set.
func TestCaveatsHold(t *testing.T) {
	// Caveat 1: the Microsoft page is three lists. OBJECT, ROLE and ACTION are
	// not T-SQL reserved, however a naive full-page scrape reports them.
	for _, w := range []string{"OBJECT", "ROLE", "ACTION"} {
		if Sources(w)&TSQL != 0 {
			t.Errorf("%s is attributed to T-SQL reserved; it appears only in the ODBC and/or Future Keywords sections of that page. gen.go must split the page by heading.", w)
		}
	}
	// Caveat 3: MariaDB does not reserve ACTION — it sits in that page's
	// "Exceptions" table, below the reserved list.
	if Sources("ACTION")&MariaDB != 0 {
		t.Error("ACTION is attributed to MariaDB; MariaDB lists it under Exceptions, not under reserved words. The MariaDB parser is reading past its section.")
	}
	// The words that make the MySQL stand-in harmless: each is independently
	// reserved outside the MySQL/MariaDB family, so substituting MariaDB for an
	// unreachable dev.mysql.com cannot have changed a verdict.
	for _, w := range []string{"GRANT", "GROUP", "KEY"} {
		if Sources(w)&^MariaDB == 0 {
			t.Errorf("%s is reserved by MariaDB alone; the MariaDB-stands-in-for-MySQL substitution is no longer verdict-neutral and caveat 2 must be revisited", w)
		}
	}
}

func TestLookupNormalizesInput(t *testing.T) {
	for _, in := range []string{"group", "Group", "  GROUP  ", "gRoUp"} {
		if !IsReserved(in) {
			t.Errorf("IsReserved(%q) = false; lookups are case- and space-insensitive", in)
		}
	}
	if IsReserved("apt_grants") {
		t.Error(`IsReserved("apt_grants") = true; a prefixed identifier is not a reserved word`)
	}
	if got := Source(0).String(); got != "none" {
		t.Errorf("Source(0).String() = %q, want %q", got, "none")
	}
}

// TestSnapshotDeclaresItsProvenance reads the generated file from disk and
// checks that it still says where it came from and when. Follows the repo's
// convention-gate posture: an unreadable words.go is a failure, never a skip.
func TestSnapshotDeclaresItsProvenance(t *testing.T) {
	raw, err := os.ReadFile("words.go")
	if err != nil {
		t.Fatalf("cannot read words.go: %v — the vendored snapshot and its provenance header are the point of this package", err)
	}
	header, _, ok := strings.Cut(string(raw), "\npackage sqlreserved")
	if !ok {
		t.Fatal("words.go has no package clause")
	}
	for _, want := range []string{
		"2026-08-20",                    // the fetch date
		"POINT-IN-TIME SNAPSHOT",        // the honesty clause
		"gen.go",                        // how to regenerate
		"sqlite.org/lang_keywords.html", // and every source URL
		"postgresql.org/docs/current/sql-keywords-appendix.html",
		"learn.microsoft.com/en-us/sql/t-sql/language-elements/reserved-keywords-transact-sql",
		"docs.oracle.com/en/database/oracle/oracle-database/23/sqlrf/Oracle-SQL-Reserved-Words.html",
		"mariadb.com/kb/en/reserved-words/",
		"dev.mysql.com/doc/refman/8.4/en/keywords.html", // caveat 2: the list that was unreachable
		"THREE lists",             // caveat 1
		"does NOT reserve ACTION", // caveat 3
	} {
		if !strings.Contains(header, want) {
			t.Errorf("words.go header no longer records %q; the snapshot must stay self-describing", want)
		}
	}
}
