// Package sqlreserved holds a vendored, point-in-time union of SQL reserved
// words drawn from every vendor and ISO list Aperture consulted, together with
// the provenance of each word.
//
// It exists so that a test can ask "is this identifier reserved anywhere?"
// without reaching the network. The word table itself lives in the generated
// file words.go, which carries the fetch date, the source URLs, and the parsing
// caveats that make the snapshot reproducible. Read that header before changing
// anything here.
//
// The package is deliberately internal. Aperture is library-first — its public
// packages at the module root are the product — and a snapshot of somebody
// else's keyword lists is a build-time convention artifact, not product
// surface. It also carries no runtime failure mode: every entry point is a pure
// lookup that cannot fail, so nothing here returns an error and no APERTURE_*
// code is needed.
package sqlreserved

import (
	"sort"
	"strings"
)

//go:generate go run gen.go -out words.go

// Source is a bit set naming the lists that reserve a word. A word's entry in
// the table carries every list it appears on, so a caller can report *why* an
// identifier is reserved rather than only that it is.
type Source uint32

// The ten consulted lists. See the header of words.go for the URL each one was
// fetched from and for the caveats that apply to the Microsoft and MySQL rows.
const (
	// SQLite is the keyword list of the engine Aperture actually embeds.
	SQLite Source = 1 << iota
	// PostgreSQL is the "PostgreSQL" column of the PostgreSQL key-words appendix.
	PostgreSQL
	// SQL2023 is the "SQL:2023" column of that same appendix.
	SQL2023
	// SQL2016 is the "SQL:2016" column of that same appendix.
	SQL2016
	// SQL92 is the "SQL-92" column of that same appendix.
	SQL92
	// TSQL is Microsoft's Transact-SQL reserved keywords — the first of the
	// three lists on that page, and the only one that is actually reserved
	// today by SQL Server.
	TSQL
	// ODBC is the ODBC reserved keywords section of the same Microsoft page.
	ODBC
	// SQLServerFuture is the "Future Keywords" section of the same Microsoft
	// page: words Microsoft says *could* become reserved. Nothing reserves
	// these today, so a hit on this source alone is a judgment call, not a
	// defect.
	SQLServerFuture
	// Oracle is Oracle SQL's reserved words.
	Oracle
	// MariaDB is MariaDB's reserved words, standing in for MySQL — see caveat 2
	// in words.go.
	MariaDB
)

// sourceNames pairs each bit with the label used in diagnostics. Declaration
// order is the order Names reports in, and matches the const block above.
var sourceNames = []struct {
	bit  Source
	name string
}{
	{SQLite, "SQLite"},
	{PostgreSQL, "PostgreSQL"},
	{SQL2023, "SQL:2023"},
	{SQL2016, "SQL:2016"},
	{SQL92, "SQL-92"},
	{TSQL, "T-SQL"},
	{ODBC, "ODBC"},
	{SQLServerFuture, "SQL Server future keywords"},
	{Oracle, "Oracle"},
	{MariaDB, "MariaDB"},
}

// All is every source bit or-ed together.
var All = func() Source {
	var s Source
	for _, sn := range sourceNames {
		s |= sn.bit
	}
	return s
}()

// Names lists the human-readable names of the set bits, in the order the
// constants are declared.
func (s Source) Names() []string {
	out := make([]string, 0, len(sourceNames))
	for _, sn := range sourceNames {
		if s&sn.bit != 0 {
			out = append(out, sn.name)
		}
	}
	return out
}

// String renders the set as a comma-separated list of source names, so a test
// failure can say exactly which lists object to an identifier.
func (s Source) String() string {
	names := s.Names()
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

// Sources reports every list that reserves word. Matching is case-insensitive;
// the zero Source means the word is reserved by none of the consulted lists.
func Sources(word string) Source {
	return words[strings.ToUpper(strings.TrimSpace(word))]
}

// IsReserved reports whether word appears on at least one consulted list.
func IsReserved(word string) bool {
	return Sources(word) != 0
}

// Len reports how many distinct words the union holds. It is the cheap sanity
// check that the table loaded at all.
func Len() int { return len(words) }

// Words returns the union in sorted order. The returned slice is freshly
// allocated, so a caller cannot corrupt the table.
func Words() []string {
	out := make([]string, 0, len(words))
	for w := range words {
		out = append(out, w)
	}
	sort.Strings(out)
	return out
}
