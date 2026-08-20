//go:build ignore

// Command gen regenerates words.go from the canonical vendor and ISO keyword
// lists.
//
// It is NOT part of the build or of `make test`: the //go:build ignore
// constraint keeps it out of `go build ./...`, `go vet ./...` and staticcheck,
// and nothing invokes it automatically. It needs the network, and CI has no
// network guarantee, so it is run by hand:
//
//	cd internal/sqlreserved && go run gen.go -out words.go
//
// or, equivalently, `go generate ./internal/sqlreserved`.
//
// Everything it emits is parsed mechanically out of the fetched pages. Nothing
// is typed from memory. If a page's structure changes, this program fails
// loudly (a section that yields implausibly few words is an error, not a silent
// short list) rather than writing a truncated table.
//
// It also probes dev.mysql.com. MySQL's own page was unreachable when the list
// was first researched and MariaDB stood in for it; if MySQL becomes reachable
// again this program STOPS and says so, because that stand-in is a documented
// caveat a human needs to revisit rather than a decision a script should quietly
// reverse.
package main

import (
	"flag"
	"fmt"
	"go/format"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Source mirrors the Source bits in sqlreserved.go. Kept as plain strings here
// so the generator has no dependency on the package it writes into.
type sourceSpec struct {
	konst string // the Go constant name emitted into the table
	label string // human name used in the header
	url   string // where the list came from
	min   int    // a floor on the parsed count; fewer means the page moved
}

const (
	srcSQLite     = "SQLite"
	srcPostgreSQL = "PostgreSQL"
	srcSQL2023    = "SQL2023"
	srcSQL2016    = "SQL2016"
	srcSQL92      = "SQL92"
	srcTSQL       = "TSQL"
	srcODBC       = "ODBC"
	srcFuture     = "SQLServerFuture"
	srcOracle     = "Oracle"
	srcMariaDB    = "MariaDB"
)

const (
	urlSQLite   = "https://www.sqlite.org/lang_keywords.html"
	urlPostgres = "https://www.postgresql.org/docs/current/sql-keywords-appendix.html"
	urlTSQL     = "https://learn.microsoft.com/en-us/sql/t-sql/language-elements/reserved-keywords-transact-sql"
	urlTSQLMD   = urlTSQL + "?view=sql-server-ver17&accept=text/markdown"
	urlOracle   = "https://docs.oracle.com/en/database/oracle/oracle-database/23/sqlrf/Oracle-SQL-Reserved-Words.html"
	urlMariaDB  = "https://mariadb.com/kb/en/reserved-words/"
	urlMySQL    = "https://dev.mysql.com/doc/refman/8.4/en/keywords.html"
)

// order fixes both the emitted bit order and the header's row order.
var order = []sourceSpec{
	{srcSQLite, "SQLite keywords", urlSQLite, 120},
	{srcPostgreSQL, "PostgreSQL reserved", urlPostgres, 80},
	{srcSQL2023, "SQL:2023 reserved", urlPostgres, 300},
	{srcSQL2016, "SQL:2016 reserved", urlPostgres, 300},
	{srcSQL92, "SQL-92 reserved", urlPostgres, 180},
	{srcTSQL, "T-SQL reserved", urlTSQL, 150},
	{srcODBC, "ODBC reserved", urlTSQL, 200},
	{srcFuture, "SQL Server future keywords", urlTSQL, 230},
	{srcOracle, "Oracle reserved", urlOracle, 40},
	{srcMariaDB, "MariaDB reserved", urlMariaDB, 200},
}

func main() {
	out := flag.String("out", "words.go", "file to write")
	date := flag.String("date", time.Now().UTC().Format("2006-01-02"), "fetch date recorded in the header (UTC)")
	flag.Parse()

	union := map[string]map[string]bool{}
	counts := map[string]int{}
	add := func(src string, ws []string) {
		counts[src] = len(ws)
		for _, w := range ws {
			if union[w] == nil {
				union[w] = map[string]bool{}
			}
			union[w][src] = true
		}
	}

	add(srcSQLite, parseSQLite(fetch(urlSQLite)))

	pg := parsePostgres(fetch(urlPostgres))
	add(srcPostgreSQL, pg[0])
	add(srcSQL2023, pg[1])
	add(srcSQL2016, pg[2])
	add(srcSQL92, pg[3])

	ms := parseMicrosoft(fetch(urlTSQLMD))
	add(srcTSQL, ms[0])
	add(srcODBC, ms[1])
	add(srcFuture, ms[2])

	add(srcOracle, parseOracle(fetch(urlOracle)))
	add(srcMariaDB, parseMariaDB(fetch(urlMariaDB)))

	for _, s := range order {
		if counts[s.konst] < s.min {
			log.Fatalf("%s: parsed only %d words (expected at least %d) — the page structure has changed; fix the parser, do not lower the floor",
				s.label, counts[s.konst], s.min)
		}
	}

	mysqlStatus := probeMySQL()

	if err := os.WriteFile(*out, render(union, counts, *date, mysqlStatus), 0o644); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("wrote %s: %d distinct words\n", *out, len(union))
}

// fetch retrieves url and fails the run on anything but a 200.
func fetch(url string) string {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		log.Fatalf("GET %s: %v", url, err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; aperture-reservedwords-gen)")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Fatalf("GET %s: %v", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		log.Fatalf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	return string(body)
}

// probeMySQL returns a one-line description of what dev.mysql.com served, and
// stops the run if it served the keyword page. See caveat 2 in the header.
func probeMySQL() string {
	req, err := http.NewRequest(http.MethodGet, urlMySQL, nil)
	if err != nil {
		return fmt.Sprintf("not fetched (%v)", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; aperture-reservedwords-gen)")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Sprintf("unreachable (%v)", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		title := "no title"
		if m := regexp.MustCompile(`(?is)<title>(.*?)</title>`).FindStringSubmatch(string(body)); m != nil {
			title = strings.TrimSpace(m[1])
		}
		return fmt.Sprintf("unreachable — HTTP %d, %q", resp.StatusCode, title)
	}
	log.Fatalf("dev.mysql.com now serves %s with HTTP 200. MariaDB stands in for MySQL only because that page was unreachable. "+
		"Parse the real MySQL list, add a MySQL source bit, and rewrite caveat 2 — do not let this generator quietly keep the stand-in.", urlMySQL)
	return ""
}

// ---------------------------------------------------------------- parsers

var (
	// SQLite renders each keyword as a bare list item with no markup inside.
	reSQLiteItem = regexp.MustCompile(`(?m)^<li>([A-Z][A-Z0-9_]*)</li>$`)

	reRow       = regexp.MustCompile(`(?is)<tr>(.*?)</tr>`)
	reCell      = regexp.MustCompile(`(?is)<td[^>]*>(.*?)</td>`)
	rePGToken   = regexp.MustCompile(`(?is)^\s*<code class="token">([A-Z][A-Z0-9_]*)</code>\s*$`)
	reTag       = regexp.MustCompile(`(?s)<[^>]+>`)
	reOracleULs = regexp.MustCompile(`(?is)<ul class="simple"[^>]*>(.*?)</ul>`)
	reListItem  = regexp.MustCompile(`(?is)<li>(.*?)</li>`)
	reOracleKW  = regexp.MustCompile(`(?is)<code class="codeph">([A-Z][A-Z0-9_]*)`)
	reWord      = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)
	reH2ID      = regexp.MustCompile(`(?is)<h2 id="([a-z0-9-]+)"`)
	reCellPara  = regexp.MustCompile(`(?is)role="cell"(.*?)(?:</p>)`)
	reParaText  = regexp.MustCompile(`(?is)<p[^>]*>(.*)$`)
)

// parseSQLite reads https://www.sqlite.org/lang_keywords.html. The page states
// its own element count in prose; the floor in `order` guards a layout change.
func parseSQLite(page string) []string {
	var out []string
	for _, m := range reSQLiteItem.FindAllStringSubmatch(page, -1) {
		out = append(out, m[1])
	}
	return dedupe(out)
}

// parsePostgres reads the PostgreSQL key-words appendix. That table has FOUR
// status columns after the word — PostgreSQL, SQL:2023, SQL:2016, SQL-92 — and
// each cell is one of "reserved", "reserved (can be function or type name)",
// "non-reserved", "non-reserved (cannot be function or type name)", or blank.
// Only the cells whose text begins with "reserved" count. Collapsing the four
// columns into one is the classic way to over-report: `TARGET`, `VERSION` and
// `NAME` are all on this page, all non-reserved.
func parsePostgres(page string) [4][]string {
	var out [4][]string
	for _, row := range reRow.FindAllStringSubmatch(page, -1) {
		cells := reCell.FindAllStringSubmatch(row[1], -1)
		if len(cells) != 5 {
			continue
		}
		m := rePGToken.FindStringSubmatch(cells[0][1])
		if m == nil {
			continue
		}
		for i := 0; i < 4; i++ {
			if strings.HasPrefix(text(cells[i+1][1]), "reserved") {
				out[i] = append(out[i], m[1])
			}
		}
	}
	for i := range out {
		out[i] = dedupe(out[i])
	}
	return out
}

// parseMicrosoft reads the Transact-SQL reserved-keywords page in its markdown
// rendering, where every keyword sits alone on a line. The page hosts THREE
// lists and they are returned separately, in page order: T-SQL reserved, ODBC
// reserved, SQL Server future keywords. See caveat 1.
func parseMicrosoft(md string) [3][]string {
	start := strings.Index(md, "# Reserved Keywords (Transact-SQL)")
	if start < 0 {
		log.Fatalf("Microsoft page: could not find the H1; the page layout has changed")
	}
	sections := strings.Split(md[start:], "\n## ")
	byHeading := map[string]string{}
	for _, sec := range sections {
		heading, _, _ := strings.Cut(sec, "\n")
		byHeading[strings.ToLower(strings.TrimSpace(heading))] = sec
	}
	pick := func(want string) []string {
		for heading, sec := range byHeading {
			if strings.Contains(heading, want) {
				return markdownWords(sec)
			}
		}
		log.Fatalf("Microsoft page: no section heading containing %q; the page layout has changed", want)
		return nil
	}
	return [3][]string{
		pick("reserved keywords (transact-sql)"),
		pick("odbc reserved keywords"),
		pick("future keywords"),
	}
}

// markdownWords pulls the bare uppercase tokens out of one markdown section,
// undoing the underscore escaping the renderer applies (CUME\_DIST).
func markdownWords(sec string) []string {
	var out []string
	for _, line := range strings.Split(sec, "\n") {
		l := strings.TrimSpace(strings.ReplaceAll(line, `\`, ""))
		l = strings.TrimSpace(strings.Trim(l, "*"))
		if reWord.MatchString(l) {
			out = append(out, l)
		}
	}
	return dedupe(out)
}

// parseOracle reads Oracle's reserved-word list. Entries carry an asterisk when
// the word is also ANSI reserved, and some carry a trailing footnote; only the
// leading <code class="codeph"> token of each item is the word.
func parseOracle(page string) []string {
	uls := reOracleULs.FindAllStringSubmatch(page, -1)
	if len(uls) == 0 {
		log.Fatalf("Oracle page: no <ul class=\"simple\"> found; the page layout has changed")
	}
	var out []string
	for _, item := range reListItem.FindAllStringSubmatch(uls[0][1], -1) {
		if m := reOracleKW.FindStringSubmatch(item[1]); m != nil {
			out = append(out, m[1])
		}
	}
	return dedupe(out)
}

// parseMariaDB reads MariaDB's reserved-words page, which is rendered as ARIA
// grid cells. The page carries three word tables under separate H2s — the
// reserved words, an "Exceptions" table, and an Oracle-mode table — and only
// the first is the reserved list. The exceptions table is exactly where ACTION
// lives; see caveat 3. Entries may carry a version annotation, "OFFSET (10.6+)".
func parseMariaDB(page string) []string {
	idx := reH2ID.FindAllStringSubmatchIndex(page, -1)
	var start, end = -1, len(page)
	for i, m := range idx {
		if page[m[2]:m[3]] == "reserved-words" {
			start = m[0]
			if i+1 < len(idx) {
				end = idx[i+1][0]
			}
			break
		}
	}
	if start < 0 {
		log.Fatalf("MariaDB page: no <h2 id=\"reserved-words\">; the page layout has changed")
	}
	var out []string
	for _, cell := range reCellPara.FindAllStringSubmatch(page[start:end], -1) {
		p := reParaText.FindStringSubmatch(cell[1])
		if p == nil {
			continue
		}
		w, _, _ := strings.Cut(text(p[1]), " ") // drop "(10.6+)" annotations
		if reWord.MatchString(w) {
			out = append(out, w)
		}
	}
	return dedupe(out)
}

// text strips tags and unescapes the few entities these pages use.
func text(s string) string {
	s = reTag.ReplaceAllString(s, "")
	r := strings.NewReplacer("&nbsp;", " ", "&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`, "&#160;", " ")
	return strings.TrimSpace(r.Replace(s))
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := in[:0]
	for _, w := range in {
		if !seen[w] {
			seen[w] = true
			out = append(out, w)
		}
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------- rendering

func render(union map[string]map[string]bool, counts map[string]int, date, mysqlStatus string) []byte {
	words := make([]string, 0, len(union))
	for w := range union {
		words = append(words, w)
	}
	sort.Strings(words)

	var b strings.Builder
	fmt.Fprintf(&b, `// Code generated by gen.go; DO NOT EDIT.
//
// The union of SQL reserved words from every list Aperture consulted, with the
// lists that reserve each word.
//
// THIS IS A POINT-IN-TIME SNAPSHOT, fetched %[1]s. It is vendored so the
// schema-naming gate can run without network access, and it does not track the
// vendors. It will drift: a word added to a vendor's list after %[1]s is not
// here, and a word since dropped still is. Regenerate deliberately with
//
//	cd internal/sqlreserved && go run gen.go -out words.go
//
// Nothing regenerates it automatically, and gen.go is excluded from the build,
// so `+"`make test`"+` never touches the network.
//
// Sources, all fetched %[1]s and parsed mechanically (never transcribed):
//
`, date)
	for _, s := range order {
		fmt.Fprintf(&b, "//\t%-27s %4d  %s\n", s.label, counts[s.konst], s.url)
	}
	fmt.Fprintf(&b, `//
// Total distinct words: %[1]d.
//
// # Three caveats, recorded so they are not rediscovered
//
// 1. The Microsoft page is THREE lists, not one. Under one URL it carries
//    "Reserved Keywords (Transact-SQL)", "ODBC Reserved Keywords", and "Future
//    Keywords". A scrape that does not split by heading reports OBJECT, ROLE
//    and ACTION as T-SQL reserved. They are not: OBJECT and ROLE appear only
//    under Future Keywords, and ACTION only under ODBC and Future Keywords.
//    Words carrying only SQLServerFuture are reserved by no shipping engine and
//    by no ISO revision — treat a hit there as a judgment call, not a defect.
//
// 2. MySQL's own documentation was unreachable and MariaDB's list stands in for
//    it. %[3]s
//    serves a "Technical Difficulties" page rather than the keyword reference;
//    re-probed at generation time, it was: %[2]s.
//    The substitution changed no verdict for Aperture's schema — the only
//    MariaDB-only candidates are GRANT, GROUP and KEY, each independently
//    reserved by PostgreSQL and by the ISO standard. gen.go re-probes MySQL on
//    every run and STOPS if the page comes back, because reversing this
//    stand-in is a decision for a human.
//
// 3. MariaDB does NOT reserve ACTION. An early full-page scrape claimed it did,
//    because the page carries an "Exceptions" table (ACTION, NO) and an
//    Oracle-mode table below the reserved list. ACTION's standing rests on
//    SQLite, SQL-92 and ODBC — not on MySQL/MariaDB. The MariaDB parser reads
//    only the section under <h2 id="reserved-words">.
//
// A fourth thing worth knowing, though it trips up readers rather than parsers:
// the PostgreSQL appendix has FOUR status columns after the word — PostgreSQL,
// SQL:2023, SQL:2016, SQL-92 — and each is a separate verdict. Collapsing them
// over-reports: TARGET, VERSION and NAME are all listed on that page, all
// non-reserved everywhere, and none of them is in this table.

package sqlreserved

// words maps an upper-cased identifier to the set of lists that reserve it.
// Use Sources or IsReserved rather than reading it directly.
var words = map[string]Source{
`, len(words), mysqlStatus, urlMySQL)

	for _, w := range words {
		var bits []string
		for _, s := range order {
			if union[w][s.konst] {
				bits = append(bits, s.konst)
			}
		}
		fmt.Fprintf(&b, "\t%q: %s,\n", w, strings.Join(bits, " | "))
	}
	b.WriteString("}\n")

	src, err := format.Source([]byte(b.String()))
	if err != nil {
		log.Fatalf("gofmt: %v", err)
	}
	return src
}
