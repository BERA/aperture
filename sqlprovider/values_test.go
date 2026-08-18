package sqlprovider

import (
	"context"
	"database/sql/driver"
	stderrors "errors"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/identity"
	"github.com/frankbardon/aperture/provider"
)

// The driver-value mapping is a CONTRACT, not an implementation detail: it is
// the boundary where a host's database becomes something the rules engine
// compares, and a value that maps wrongly is a wrong decision rather than a
// crash. These tests therefore pin it three ways:
//
//  1. TestDriverValueMappingTable exercises every rule end to end, through the
//     real database/sql machinery and the fake driver's canned driver.Values.
//  2. TestDriverValueMappingTableIsExhaustive fails if a rule in the table has
//     no case, so coverage cannot fall behind the registry.
//  3. TestDriverValueMappingTableMatchesTheTypeSwitch PARSES values.go and diffs
//     the type switch against mappedDriverTypes, so a case added (or removed) in
//     code without the table — or the reverse — is build-red. It fails rather
//     than skips if the file moves, per the house pattern.

const testID = "account:acme/brand:42"

// mapCase is one row of the mapping table, expressed as a value a driver hands
// back and the metadata it must become.
type mapCase struct {
	name string
	// driverType is the mappedDriverTypes entry this case covers. Every entry
	// must be covered; see TestDriverValueMappingTableIsExhaustive.
	driverType string
	in         driver.Value
	// want is the metadata value; absent means the field must be OMITTED.
	want   any
	absent bool
	// wantErr means the row must fail to load, with the given reason.
	wantErr    bool
	wantReason string
}

func mappingCases() []mapCase {
	return []mapCase{
		// nil — a SQL NULL omits its field. Absent is not zero: an absent field
		// never matches a Filter.Fields predicate, whereas a stored nil is a
		// value that compares.
		{name: "NULL omits its field", driverType: "nil", in: nil, absent: true},

		// The scalars pass through untouched. Their Go types are the ones the
		// expression evaluator compares, so any conversion here would be a
		// silent mis-compare later.
		{name: "bool true", driverType: "bool", in: true, want: true},
		{name: "bool false", driverType: "bool", in: false, want: false},
		{name: "int64", driverType: "int64", in: int64(5), want: int64(5)},
		{name: "int64 zero", driverType: "int64", in: int64(0), want: int64(0)},
		{name: "int64 negative", driverType: "int64", in: int64(-7), want: int64(-7)},
		{name: "float64", driverType: "float64", in: float64(2.5), want: float64(2.5)},
		{name: "string", driverType: "string", in: "gold", want: "gold"},
		{name: "empty string is a value, not an absence", driverType: "string", in: "", want: ""},
		// The measured trap, pinned as BEHAVIOUR so nobody "fixes" it by adding
		// an array-literal parser: an uncast text[] is a string and stays one.
		{name: "an uncast array literal stays a string", driverType: "string", in: "{a,b}", want: "{a,b}"},

		// []byte is JSON, unconditionally — the only way a list-valued field can
		// arrive, since no driver hands back a []any for a database array.
		{name: "json array of strings", driverType: "[]byte", in: []byte(`["a", "b"]`), want: []any{"a", "b"}},
		{name: "json array of numbers", driverType: "[]byte", in: []byte(`[1, 2]`), want: []any{int64(1), int64(2)}},
		{name: "json empty array", driverType: "[]byte", in: []byte(`[]`), want: []any{}},
		{name: "json object", driverType: "[]byte", in: []byte(`{"dept": "eng", "seats": 3}`),
			want: map[string]any{"dept": "eng", "seats": int64(3)}},
		{name: "json nested object", driverType: "[]byte", in: []byte(`{"lead": {"name": "x"}}`),
			want: map[string]any{"lead": map[string]any{"name": "x"}}},
		{name: "json scalar string", driverType: "[]byte", in: []byte(`"gold"`), want: "gold"},
		{name: "json scalar bool", driverType: "[]byte", in: []byte(`true`), want: true},
		// An exact integer is an int64 and a fraction is a float64 — csvprovider's
		// rule, at every depth, because the evaluator does no numeric coercion.
		{name: "json integer is an int64", driverType: "[]byte", in: []byte(`42`), want: int64(42)},
		{name: "json fraction is a float64", driverType: "[]byte", in: []byte(`1.5`), want: float64(1.5)},
		{name: "json numbers normalise at depth", driverType: "[]byte", in: []byte(`{"a": [1, 2.5]}`),
			want: map[string]any{"a": []any{int64(1), float64(2.5)}}},
		// A JSON null says exactly what a SQL NULL says.
		{name: "json null omits its field", driverType: "[]byte", in: []byte(`null`), absent: true},
		// A decode failure is HARD. Never a silent coercion to a string — that
		// would let one column change type depending on its contents.
		{name: "json decode failure is hard", driverType: "[]byte", in: []byte(`{a,b}`),
			wantErr: true, wantReason: reasonJSONDecode},
		{name: "a bare uuid does not decode", driverType: "[]byte", in: []byte(`11111111-1111-1111-1111-111111111111`),
			wantErr: true, wantReason: reasonJSONDecode},
		{name: "an empty []byte does not decode", driverType: "[]byte", in: []byte{},
			wantErr: true, wantReason: reasonJSONDecode},
		{name: "trailing content is hard", driverType: "[]byte", in: []byte(`{"a":1} {"b":2}`),
			wantErr: true, wantReason: reasonJSONDecode},
		{name: "an unrepresentable json number is hard", driverType: "[]byte",
			in:      []byte(`[1e400]`),
			wantErr: true, wantReason: reasonJSONNumber},

		// time.Time is converted to UTC FIRST — a timestamptz comes back in the
		// process's LOCAL zone in every measured driver — and then routed through
		// the shared date value model rather than merely formatted.
		{name: "time in UTC", driverType: "time.Time",
			in:   time.Date(2026, 3, 4, 12, 30, 0, 0, time.UTC),
			want: "2026-03-04T12:30:00Z"},
		{name: "time in a positive-offset zone is converted", driverType: "time.Time",
			in:   time.Date(2026, 3, 4, 17, 30, 0, 0, time.FixedZone("east", 5*3600)),
			want: "2026-03-04T12:30:00Z"},
		{name: "time in a negative-offset zone is converted", driverType: "time.Time",
			in:   time.Date(2026, 3, 4, 7, 30, 0, 0, time.FixedZone("west", -5*3600)),
			want: "2026-03-04T12:30:00Z"},
		// Sub-second precision truncates, never rounds: rounding can carry a
		// value across the boundary a rule is testing.
		{name: "sub-second precision truncates", driverType: "time.Time",
			in:   time.Date(2026, 3, 4, 12, 30, 0, 999999999, time.UTC),
			want: "2026-03-04T12:30:00Z"},
		// A date column and a timestamp column are the same Go type, so a date
		// gets the datetime form. Day granularity is the developer's ::text.
		{name: "a midnight date still gets the datetime form", driverType: "time.Time",
			in:   time.Date(2026, 3, 4, 0, 0, 0, 0, time.UTC),
			want: "2026-03-04T00:00:00Z"},
		{name: "an instant the canonical form cannot spell is hard", driverType: "time.Time",
			in:      time.Date(12026, 3, 4, 0, 0, 0, 0, time.UTC),
			wantErr: true, wantReason: reasonDate},
	}
}

// TestDriverValueMappingTable runs every rule through the whole path: the fake
// driver hands back a canned driver.Value, database/sql scans it, and Fetch maps
// it. Nothing here is a unit test of a helper — the claim being pinned is about
// what a developer's SELECT produces.
func TestDriverValueMappingTable(t *testing.T) {
	for _, tc := range mappingCases() {
		t.Run(tc.name, func(t *testing.T) {
			s := &script{cols: []string{"col"}, rows: [][]driver.Value{{tc.in}}}
			p := newProvider(t, s, Config{})
			md, err := p.Fetch(context.Background(), identity.MustParse(testID))

			if tc.wantErr {
				mustCode(t, err, aerr.APERTURE_SQL_PROVIDER_SCAN)
				var ce *aerr.CodedError
				if !stderrors.As(err, &ce) {
					t.Fatalf("error is not a *CodedError: %v", err)
				}
				// A failure names the column AND the row, so a developer can
				// find the offending value without the error carrying it.
				if got := ce.Context["column"]; got != "col" {
					t.Fatalf("error context column = %v, want the column named", got)
				}
				if got := ce.Context["id"]; got != testID {
					t.Fatalf("error context id = %v, want the row's id named", got)
				}
				if got := ce.Context["reason"]; got != tc.wantReason {
					t.Fatalf("error context reason = %v, want %q", got, tc.wantReason)
				}
				return
			}

			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			got, present := md["col"]
			if tc.absent {
				if present {
					t.Fatalf("field is present as %#v, want it OMITTED", got)
				}
				return
			}
			if !present {
				t.Fatalf("field is absent, want %#v", tc.want)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("mapped to %#v (%T), want %#v (%T)", got, got, tc.want, tc.want)
			}
			// Whatever the mapping produced must be legal metadata. This is the
			// guarantee the rest of the engine is entitled to assume.
			if err := provider.ValidateMetadata(md); err != nil {
				t.Fatalf("mapped metadata violates the value model: %v", err)
			}
		})
	}
}

// A rule in the table with no case is coverage falling behind the registry.
func TestDriverValueMappingTableIsExhaustive(t *testing.T) {
	covered := map[string]int{}
	for _, tc := range mappingCases() {
		if tc.driverType == "" {
			t.Fatalf("case %q names no driverType; every case must say which mapping rule it covers", tc.name)
		}
		if !slices.Contains(mappedDriverTypes, tc.driverType) {
			t.Fatalf("case %q covers %q, which is not in mappedDriverTypes %v", tc.name, tc.driverType, mappedDriverTypes)
		}
		covered[tc.driverType]++
	}
	for _, typ := range mappedDriverTypes {
		if covered[typ] == 0 {
			t.Errorf("mapped driver type %q has no case in the mapping table test", typ)
		}
	}
}

// The real drift gate: values.go's type switch IS the mapping, and
// mappedDriverTypes is how it is described to a developer (and rendered into the
// unmapped-type diagnostic). A case added in code without the table would ship a
// rule nobody documented; an entry removed from the table without the code would
// tell a developer a type is unsupported when it is not.
//
// This parses the file from disk and FAILS — never skips — if it is missing, so
// moving or renaming values.go breaks the build rather than quietly disarming
// the gate.
func TestDriverValueMappingTableMatchesTheTypeSwitch(t *testing.T) {
	const file = "values.go"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("cannot parse %s: %v (the driver-value mapping contract test reads this file from disk; "+
			"if it moved, move this test with it rather than deleting it)", file, err)
	}

	fn := findFunc(f, "metadataValue")
	if fn == nil {
		t.Fatalf("%s no longer declares metadataValue; it is the driver-value mapping", file)
	}
	sw := findTypeSwitch(fn)
	if sw == nil {
		t.Fatalf("metadataValue no longer maps through a type switch; the contract test cannot read the table")
	}

	var inSwitch []string
	for _, stmt := range sw.Body.List {
		clause, ok := stmt.(*ast.CaseClause)
		if !ok {
			continue
		}
		if clause.List == nil {
			continue // the default clause: the unmapped-type error
		}
		for _, expr := range clause.List {
			inSwitch = append(inSwitch, types.ExprString(expr))
		}
	}

	got := slices.Clone(inSwitch)
	want := slices.Clone(mappedDriverTypes)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("the type switch in %s maps %v but mappedDriverTypes says %v;\n"+
			"they are one contract — update both, and the mapping table in the package doc with them",
			file, inSwitch, mappedDriverTypes)
	}
}

func findFunc(f *ast.File, name string) *ast.FuncDecl {
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Recv == nil && fn.Name.Name == name {
			return fn
		}
	}
	return nil
}

func findTypeSwitch(fn *ast.FuncDecl) *ast.TypeSwitchStmt {
	var found *ast.TypeSwitchStmt
	ast.Inspect(fn, func(n ast.Node) bool {
		if sw, ok := n.(*ast.TypeSwitchStmt); ok && found == nil {
			found = sw
		}
		return found == nil
	})
	return found
}

// The unmapped-type diagnostic must tell a developer what IS accepted, not only
// that their type is not. It renders the table, so the two cannot drift.
func TestUnmappedTypeErrorNamesTheTableAndNotTheValue(t *testing.T) {
	type opaque struct{ Secret string }
	s := &script{
		cols: []string{"tier", "weird"},
		rows: [][]driver.Value{{"gold", opaque{Secret: "another-account-data"}}},
	}
	p := newProvider(t, s, Config{})
	_, err := p.Fetch(context.Background(), identity.MustParse(testID))
	mustCode(t, err, aerr.APERTURE_SQL_PROVIDER_SCAN)

	var ce *aerr.CodedError
	if !stderrors.As(err, &ce) {
		t.Fatalf("error is not a *CodedError: %v", err)
	}
	if got := ce.Context["column"]; got != "weird" {
		t.Fatalf("error context column = %v, want the offending column named", got)
	}
	if got := ce.Context["reason"]; got != reasonUnmappedType {
		t.Fatalf("error context reason = %v, want %q", got, reasonUnmappedType)
	}
	mapped, _ := ce.Context["mapped"].(string)
	for _, typ := range mappedDriverTypes {
		if !strings.Contains(mapped, typ) {
			t.Fatalf("the diagnostic lists %q as the mapped types, missing %q", mapped, typ)
		}
	}
	// The TYPE is named; the value — host data, frequently another account's —
	// never is.
	if got, _ := ce.Context["type"].(string); !strings.Contains(got, "opaque") {
		t.Fatalf("error context type = %q, want the Go type named", got)
	}
	if strings.Contains(err.Error(), "another-account-data") {
		t.Fatalf("the error carries the offending value: %v", err)
	}
}

// A non-UTC time.Time and the equivalent UTC instant must produce the SAME
// canonical string. This is the property that keeps two hosts in two zones
// making the same decision from one row — and near midnight, it is the property
// that keeps them on the same calendar DAY.
func TestNonUTCTimeCanonicalisesLikeItsUTCInstant(t *testing.T) {
	// 2026-03-04T23:30:00+05:00 is 2026-03-04T18:30:00Z. Read in the +05:00
	// zone it is the 4th; read as a naive local clock it would still say 23:30,
	// and a value four and a half hours later would cross into the 5th.
	local := time.Date(2026, 3, 4, 23, 30, 0, 0, time.FixedZone("east", 5*3600))
	utc := local.UTC()

	fetchOne := func(v driver.Value) string {
		t.Helper()
		s := &script{cols: []string{"hired_at"}, rows: [][]driver.Value{{v}}}
		p := newProvider(t, s, Config{})
		md, err := p.Fetch(context.Background(), identity.MustParse(testID))
		if err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		got, _ := md["hired_at"].(string)
		return got
	}

	fromLocal, fromUTC := fetchOne(local), fetchOne(utc)
	if fromLocal != fromUTC {
		t.Fatalf("a non-UTC time mapped to %q but its UTC instant mapped to %q; "+
			"the same row must not depend on the process's zone", fromLocal, fromUTC)
	}
	if fromLocal != "2026-03-04T18:30:00Z" {
		t.Fatalf("mapped to %q, want the UTC instant 2026-03-04T18:30:00Z", fromLocal)
	}
	// And the stored text is one the shared date value model accepts, at the
	// instant it names — this loader spells dates exactly as every other does.
	dv, err := provider.ParseDateValue(fromLocal)
	if err != nil {
		t.Fatalf("the stored value is not a canonical date: %v", err)
	}
	if !dv.Time().Equal(utc.Truncate(time.Second)) {
		t.Fatalf("stored instant = %v, want %v", dv.Time(), utc)
	}
	if dv.Granularity() != provider.GranularityDateTime {
		t.Fatalf("granularity = %v, want datetime: a database date and timestamp are one Go type", dv.Granularity())
	}
}

// A JSON-decoded value that the metadata value model rejects fails the FETCH.
// The loader does not re-implement those rules — provider.ValidateMetadata is
// the single authority — but it must run them, so a shape the expression
// evaluator cannot handle never reaches a Check.
func TestDecodedJSONIsStillSubjectToTheValueModel(t *testing.T) {
	cases := []struct {
		name string
		json string
	}{
		{"array of objects", `[{"id": 1}, {"id": 2}]`},
		{"nested arrays", `[[1, 2], [3]]`},
		{"past the depth cap", `{"a": {"b": {"c": 1}}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &script{cols: []string{"col"}, rows: [][]driver.Value{{[]byte(tc.json)}}}
			p := newProvider(t, s, Config{})
			_, err := p.Fetch(context.Background(), identity.MustParse(testID))
			mustCode(t, err, aerr.APERTURE_METADATA_INVALID)
		})
	}
}

// Read-only transitively: a decoded container is fresh per call, at every depth.
// A holder that writes into one must not be able to reach a later Fetch's value.
func TestDecodedContainersAreFreshPerFetch(t *testing.T) {
	s := &script{
		cols: []string{"tags", "owner"},
		rows: [][]driver.Value{{[]byte(`["a", "b"]`), []byte(`{"dept": "eng"}`)}},
	}
	p := newProvider(t, s, Config{})
	id := identity.MustParse(testID)

	first, err := p.Fetch(context.Background(), id)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	first["tags"].([]any)[0] = "MUTATED"
	first["owner"].(map[string]any)["dept"] = "MUTATED"

	second, err := p.Fetch(context.Background(), id)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got := second["tags"].([]any)[0]; got != "a" {
		t.Fatalf("a second Fetch saw a write into the first's nested slice: %v", got)
	}
	if got := second["owner"].(map[string]any)["dept"]; got != "eng" {
		t.Fatalf("a second Fetch saw a write into the first's nested map: %v", got)
	}
}

// A driver may reuse the buffer it handed back for the next row. Nothing mapped
// out of a []byte may alias it, or a cached Metadata would change under the
// Registry's feet.
func TestDecodedJSONDoesNotAliasTheDriverBuffer(t *testing.T) {
	buf := []byte(`{"dept": "eng"}`)
	s := &script{cols: []string{"owner"}, rows: [][]driver.Value{{buf}}}
	p := newProvider(t, s, Config{})
	md, err := p.Fetch(context.Background(), identity.MustParse(testID))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	copy(buf, []byte(`{"dept": "XXX"}`))
	if got := md["owner"].(map[string]any)["dept"]; got != "eng" {
		t.Fatalf("the mapped value aliased the driver's buffer: dept = %v", got)
	}
}

// A list-valued field must behave as a COLLECTION under the Filter.Fields
// contract — membership, not equality. That contract is what E3's object
// references ride on, and it is the reason arrays have to arrive as arrays.
func TestADecodedJSONArrayMatchesByMembership(t *testing.T) {
	s := &script{cols: []string{"tags"}, rows: [][]driver.Value{{[]byte(`["red", "blue"]`)}}}
	p := newProvider(t, s, Config{})
	md, err := p.Fetch(context.Background(), identity.MustParse(testID))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !provider.MatchFields(md, map[string]any{"tags": "red"}) {
		t.Fatalf("metadata %#v did not match tags=red by membership", md)
	}
	if provider.MatchFields(md, map[string]any{"tags": "green"}) {
		t.Fatalf("metadata %#v matched tags=green", md)
	}
	// And the uncast spelling — the documented trap — matches nothing, which is
	// exactly why the package doc has to warn about it.
	raw := provider.Metadata{"tags": "{red,blue}"}
	if provider.MatchFields(raw, map[string]any{"tags": "red"}) {
		t.Fatalf("an uncast array literal matched by membership; the trap's premise has changed")
	}
}
