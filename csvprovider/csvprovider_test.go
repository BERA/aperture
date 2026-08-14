package csvprovider

import (
	"context"
	stderrors "errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/identity"
	"github.com/frankbardon/aperture/provider"
)

// write drops content into a temp .csv and returns its path.
func write(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "data.csv")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func TestFetch_TypedFields(t *testing.T) {
	p := New(write(t, strings.Join([]string{
		"id,category_id,seats:int,active:bool,budget:float",
		"brand:1,category:5,12,true,9.5",
	}, "\n")))

	md, err := p.Fetch(context.Background(), identity.MustParse("brand:1"))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if md["category_id"] != "category:5" {
		t.Errorf("category_id = %#v, want string category:5", md["category_id"])
	}
	if md["seats"] != int64(12) {
		t.Errorf("seats = %#v, want int64(12)", md["seats"])
	}
	if md["active"] != true {
		t.Errorf("active = %#v, want bool true", md["active"])
	}
	if md["budget"] != 9.5 {
		t.Errorf("budget = %#v, want float64 9.5", md["budget"])
	}
}

func TestFetch_MissingIsNotFound(t *testing.T) {
	p := New(write(t, "id,category_id\nbrand:1,category:5\n"))
	_, err := p.Fetch(context.Background(), identity.MustParse("brand:404"))
	if got := aerr.CodeOf(err); got != aerr.APERTURE_NOT_FOUND {
		t.Fatalf("code = %s, want APERTURE_NOT_FOUND", got)
	}
}

func TestList_PreservesFileOrder(t *testing.T) {
	p := New(write(t, "id,category_id\nbrand:3,c\nbrand:1,c\nbrand:2,c\n"))
	objs, err := p.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	got := []string{objs[0].ID.String(), objs[1].ID.String(), objs[2].ID.String()}
	want := []string{"brand:3", "brand:1", "brand:2"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func TestQuery_FieldsAndPatternAndLimit(t *testing.T) {
	p := New(write(t, strings.Join([]string{
		"id,facing",
		"app:be,external",
		"app:bt,external",
		"app:bi,internal",
	}, "\n")))

	// Field equality.
	ext, err := p.Query(context.Background(), provider.Filter{Fields: map[string]any{"facing": "external"}})
	if err != nil {
		t.Fatalf("Query fields: %v", err)
	}
	if len(ext) != 2 {
		t.Fatalf("external apps = %d, want 2", len(ext))
	}

	// Pattern.
	pat := identity.MustParsePattern("app:bi")
	only, err := p.Query(context.Background(), provider.Filter{Pattern: &pat})
	if err != nil {
		t.Fatalf("Query pattern: %v", err)
	}
	if len(only) != 1 || only[0].ID.String() != "app:bi" {
		t.Fatalf("pattern result = %v, want [app:bi]", only)
	}

	// Limit.
	lim, err := p.Query(context.Background(), provider.Filter{Limit: 1})
	if err != nil {
		t.Fatalf("Query limit: %v", err)
	}
	if len(lim) != 1 {
		t.Fatalf("limited result = %d, want 1", len(lim))
	}
}

func TestEmptyCellOmitsField(t *testing.T) {
	p := New(write(t, "id,category_id\nbrand:1,\n"))
	md, err := p.Fetch(context.Background(), identity.MustParse("brand:1"))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if _, present := md["category_id"]; present {
		t.Errorf("empty cell should omit field, got %#v", md["category_id"])
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string]string{
		"no id column":     "name,category_id\nfoo,bar\n",
		"duplicate id":     "id,category_id\nbrand:1,a\nbrand:1,b\n",
		"unknown type":     "id,seats:money\nbrand:1,5\n",
		"bad int":          "id,seats:int\nbrand:1,notanumber\n",
		"wrong col count":  "id,category_id\nbrand:1\n",
		"duplicate column": "id,category_id,category_id\nbrand:1,a,b\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := p_load(t, content)
			if got := aerr.CodeOf(err); got != aerr.APERTURE_CONFIG_INVALID {
				t.Fatalf("code = %s, want APERTURE_CONFIG_INVALID", got)
			}
		})
	}
}

func TestBadIdentityPassesThrough(t *testing.T) {
	_, err := p_load(t, "id,category_id\nbrand:,a\n") // empty segment id
	if got := aerr.CodeOf(err); got != aerr.APERTURE_IDENTITY_INVALID {
		t.Fatalf("code = %s, want APERTURE_IDENTITY_INVALID", got)
	}
}

// p_load forces a load through the public API and returns any error.
func p_load(t *testing.T, content string) (*Provider, error) {
	t.Helper()
	p := New(write(t, content))
	_, err := p.List(context.Background())
	return p, err
}

func TestReload_SwapsSetAndKeepsOldMapsImmutable(t *testing.T) {
	path := write(t, "id,facing\napp:be,external\n")
	p := New(path)

	before, err := p.Fetch(context.Background(), identity.MustParse("app:be"))
	if err != nil {
		t.Fatalf("Fetch before: %v", err)
	}

	if err := os.WriteFile(path, []byte("id,facing\napp:be,internal\napp:bt,external\n"), 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if err := p.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	after, err := p.Fetch(context.Background(), identity.MustParse("app:be"))
	if err != nil {
		t.Fatalf("Fetch after: %v", err)
	}
	if after["facing"] != "internal" {
		t.Errorf("after reload facing = %v, want internal", after["facing"])
	}
	if before["facing"] != "external" {
		t.Errorf("pre-reload map mutated: facing = %v, want external", before["facing"])
	}
	objs, _ := p.List(context.Background())
	if len(objs) != 2 {
		t.Errorf("after reload count = %d, want 2", len(objs))
	}
}

// TestRegistryIntegration proves the provider is a drop-in for the Registry:
// registration, cache-first Fetch, and scope-style List all work through it.
func TestRegistryIntegration(t *testing.T) {
	p := New(write(t, "id,category_id\nbrand:1,category:5\nbrand:2,category:9\n"))
	reg := provider.NewRegistry()
	reg.MustRegister("brand", p)

	md, err := reg.Fetch(context.Background(), identity.MustParse("brand:1"))
	if err != nil {
		t.Fatalf("registry Fetch: %v", err)
	}
	if md["category_id"] != "category:5" {
		t.Errorf("category_id = %v, want category:5", md["category_id"])
	}

	// Second Fetch is a cache hit (provider not consulted again).
	if _, err := reg.Fetch(context.Background(), identity.MustParse("brand:1")); err != nil {
		t.Fatalf("registry Fetch (cached): %v", err)
	}
	if s, _ := reg.Stats("brand"); s.Hits != 1 || s.Misses != 1 {
		t.Errorf("stats = %+v, want 1 hit / 1 miss", s)
	}

	// Registry.List (scope.ObjectLister) enumerates through the provider.
	ids, err := reg.List(context.Background(), "brand", identity.MustParsePattern("brand:*"), 0)
	if err != nil {
		t.Fatalf("registry List: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("List count = %d, want 2", len(ids))
	}
}

// ---------------------------------------------------------------------------
// Array-valued columns: name:type[<elem>][(delim)]
// ---------------------------------------------------------------------------

// TestListColumns covers the list grammar cell-by-cell: the default delimiter,
// each element type, the per-column delimiter override, and trimming.
func TestListColumns(t *testing.T) {
	cases := []struct {
		name   string
		header string
		cell   string
		want   []any
	}{
		{"strings, default delimiter", "tags:list", "premium|launch", []any{"premium", "launch"}},
		{"single element", "tags:list", "premium", []any{"premium"}},
		{"elements are trimmed", "tags:list", "premium | launch ", []any{"premium", "launch"}},
		{"int elements", "seats:list<int>", "3|5", []any{int64(3), int64(5)}},
		{"float elements", "ratios:list<float>", "1.5|2", []any{1.5, 2.0}},
		{"bool elements", "flags:list<bool>", "true|false", []any{true, false}},
		{"explicit string elements", "tags:list<string>", "3|5", []any{"3", "5"}},
		{"delimiter override", "aliases:list(;)", "acme;acme-co", []any{"acme", "acme-co"}},
		{"override is per column: default delimiter is now data",
			"aliases:list(;)", "acme|co;acme-co", []any{"acme|co", "acme-co"}},
		{"element type and delimiter together", "seats:list<int>(;)", "3;5", []any{int64(3), int64(5)}},
		{"multi-character delimiter", "aliases:list(::)", "a::b", []any{"a", "b"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			field, _, _ := strings.Cut(tc.header, ":")
			p := New(write(t, "id,"+tc.header+"\nbrand:1,"+tc.cell+"\n"))
			md, err := p.Fetch(context.Background(), identity.MustParse("brand:1"))
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			got, ok := md[field].([]any)
			if !ok {
				t.Fatalf("%s = %#v, want []any", field, md[field])
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("%s = %#v, want %#v", field, got, tc.want)
			}
		})
	}
}

// TestListEmptyCellYieldsEmptyList pins the one departure from scalar columns:
// an empty list cell is an EMPTY list, not an absent field, so a membership
// rule evaluates to a definite false instead of running against nil.
func TestListEmptyCellYieldsEmptyList(t *testing.T) {
	p := New(write(t, "id,tags:list,note\nbrand:1,,\n"))
	md, err := p.Fetch(context.Background(), identity.MustParse("brand:1"))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	tags, present := md["tags"]
	if !present {
		t.Fatal("empty list cell omitted the field, want an empty list")
	}
	list, ok := tags.([]any)
	if !ok || list == nil {
		t.Fatalf("tags = %#v, want a non-nil []any", tags)
	}
	if len(list) != 0 {
		t.Errorf("tags = %#v, want an empty list", list)
	}
	// The scalar column beside it keeps the existing omit-on-empty behaviour.
	if _, present := md["note"]; present {
		t.Errorf("empty scalar cell should still omit the field, got %#v", md["note"])
	}
}

// TestListDelimiterCollision proves a value that collides with the column
// delimiter is a hard error at parse — there is no escape syntax — and that the
// error names the column, the line, and the offending cell.
func TestListDelimiterCollision(t *testing.T) {
	cases := map[string]string{
		"doubled delimiter":  "premium||launch",
		"leading delimiter":  "|premium",
		"trailing delimiter": "premium|",
		"delimiter only":     "|",
		"whitespace element": "premium| |launch",
	}
	for name, cell := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := p_load(t, "id,tags:list\nbrand:1,"+cell+"\n")
			if got := aerr.CodeOf(err); got != aerr.APERTURE_CONFIG_INVALID {
				t.Fatalf("code = %s, want APERTURE_CONFIG_INVALID", got)
			}
			ctx := codedContext(t, err)
			if ctx["field"] != "tags" || ctx["line"] != 2 || ctx["delimiter"] != "|" {
				t.Errorf("context = %#v, want field=tags line=2 delimiter=|", ctx)
			}
			if !strings.Contains(err.Error(), "list(;)") {
				t.Errorf("error should point at the per-column delimiter override, got %q", err.Error())
			}
		})
	}
}

// TestListElementCoercionFailure checks the per-element error names the column,
// the line, and the offending element.
func TestListElementCoercionFailure(t *testing.T) {
	_, err := p_load(t, "id,seats:list<int>\nbrand:1,3|nope\n")
	if got := aerr.CodeOf(err); got != aerr.APERTURE_CONFIG_INVALID {
		t.Fatalf("code = %s, want APERTURE_CONFIG_INVALID", got)
	}
	ctx := codedContext(t, err)
	if ctx["field"] != "seats" || ctx["line"] != 2 || ctx["index"] != 1 || ctx["value"] != "nope" {
		t.Errorf("context = %#v, want field=seats line=2 index=1 value=nope", ctx)
	}
}

// TestHeaderSuffixErrors walks the malformed corners of
// name:type[<elem>][(delim)]. Every one is APERTURE_CONFIG_INVALID naming the
// column.
func TestHeaderSuffixErrors(t *testing.T) {
	cases := map[string]string{
		"unknown element type":         "id,tags:list<money>\nbrand:1,a\n",
		"empty element type":           "id,tags:list<>\nbrand:1,a\n",
		"empty delimiter":              "id,tags:list()\nbrand:1,a\n",
		"unterminated delimiter group": "id,tags:list(;\nbrand:1,a\n",
		"unterminated element group":   "id,tags:list<int\nbrand:1,1\n",
		"groups out of order":          "id,tags:list(;)<int>\nbrand:1,1\n",
		"element type on a scalar":     "id,seats:int<int>\nbrand:1,1\n",
		"delimiter on a scalar":        "id,seats:int(;)\nbrand:1,1\n",
		"unknown type with groups":     "id,tags:set<int>\nbrand:1,1\n",
		"stray closing bracket":        "id,tags:list>\nbrand:1,a\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := p_load(t, content)
			if got := aerr.CodeOf(err); got != aerr.APERTURE_CONFIG_INVALID {
				t.Fatalf("code = %s, want APERTURE_CONFIG_INVALID", got)
			}
			if ctx := codedContext(t, err); ctx["name"] != "tags" && ctx["name"] != "seats" {
				t.Errorf("context = %#v, want the column named", ctx)
			}
		})
	}
}

// TestListRoundTrip walks a list column through Fetch, List, and Query — the
// shape of the file in the package doc.
func TestListRoundTrip(t *testing.T) {
	p := New(write(t, strings.Join([]string{
		"id,tags:list,seats:list<int>,aliases:list(;)",
		"brand:1,premium|launch,3|5,acme;acme-co",
		"brand:2,basic,1,bcorp",
	}, "\n")))

	md, err := p.Fetch(context.Background(), identity.MustParse("brand:1"))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !reflect.DeepEqual(md["tags"], []any{"premium", "launch"}) {
		t.Errorf("tags = %#v", md["tags"])
	}
	if !reflect.DeepEqual(md["seats"], []any{int64(3), int64(5)}) {
		t.Errorf("seats = %#v", md["seats"])
	}
	if !reflect.DeepEqual(md["aliases"], []any{"acme", "acme-co"}) {
		t.Errorf("aliases = %#v", md["aliases"])
	}

	objs, err := p.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(objs) != 2 {
		t.Fatalf("List count = %d, want 2", len(objs))
	}
	if !reflect.DeepEqual(objs[1].Metadata["tags"], []any{"basic"}) {
		t.Errorf("brand:2 tags = %#v", objs[1].Metadata["tags"])
	}

	got, err := p.Query(context.Background(), provider.Filter{Pattern: ptr(identity.MustParsePattern("brand:2"))})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got) != 1 || !reflect.DeepEqual(got[0].Metadata["seats"], []any{int64(1)}) {
		t.Fatalf("Query result = %#v", got)
	}
}

// TestListSlicesAreNeverShared holds the read-only contract at depth: two rows
// with identical cells must not share one backing array, and a Reload must not
// touch a slice already handed out.
func TestListSlicesAreNeverShared(t *testing.T) {
	path := write(t, "id,tags:list\nbrand:1,a|b\nbrand:2,a|b\n")
	p := New(path)

	objs, err := p.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	first := objs[0].Metadata["tags"].([]any)
	second := objs[1].Metadata["tags"].([]any)
	if reflect.ValueOf(first).UnsafePointer() == reflect.ValueOf(second).UnsafePointer() {
		t.Fatal("two rows share one backing array; each row must own its slice")
	}

	if err := os.WriteFile(path, []byte("id,tags:list\nbrand:1,c|d\nbrand:2,a|b\n"), 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if err := p.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if !reflect.DeepEqual(first, []any{"a", "b"}) {
		t.Errorf("pre-reload slice mutated: %#v", first)
	}
	after, err := p.Fetch(context.Background(), identity.MustParse("brand:1"))
	if err != nil {
		t.Fatalf("Fetch after reload: %v", err)
	}
	if !reflect.DeepEqual(after["tags"], []any{"c", "d"}) {
		t.Errorf("after reload tags = %#v, want [c d]", after["tags"])
	}
}

// TestValueModelRejectsOversizedCell proves every parsed value goes through the
// shared value model: a cell past the per-field size cap fails the LOAD with
// APERTURE_METADATA_INVALID rather than reaching a Check.
func TestValueModelRejectsOversizedCell(t *testing.T) {
	huge := strings.Repeat("x", provider.DefaultMaxValueBytes+1)
	for name, content := range map[string]string{
		"list column":   "id,tags:list\nbrand:1," + huge + "\n",
		"scalar column": "id,note\nbrand:1," + huge + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := p_load(t, content)
			if got := aerr.CodeOf(err); got != aerr.APERTURE_METADATA_INVALID {
				t.Fatalf("code = %s, want APERTURE_METADATA_INVALID", got)
			}
			if !strings.Contains(err.Error(), "line 2") {
				t.Errorf("error should name the line, got %q", err.Error())
			}
		})
	}
}

// codedContext returns the structured context of the outermost CodedError.
func codedContext(t *testing.T, err error) map[string]any {
	t.Helper()
	var ce *aerr.CodedError
	if !stderrors.As(err, &ce) {
		t.Fatalf("error is not a *aerr.CodedError: %v", err)
	}
	return ce.Context
}

func ptr[T any](v T) *T { return &v }

func TestFromReader(t *testing.T) {
	p, err := FromReader(strings.NewReader("id,facing\napp:be,external\n"))
	if err != nil {
		t.Fatalf("FromReader: %v", err)
	}
	md, err := p.Fetch(context.Background(), identity.MustParse("app:be"))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if md["facing"] != "external" {
		t.Errorf("facing = %v, want external", md["facing"])
	}
	if err := p.Reload(); aerr.CodeOf(err) != aerr.APERTURE_CONFIG_INVALID {
		t.Errorf("reader Reload code = %s, want APERTURE_CONFIG_INVALID", aerr.CodeOf(err))
	}
}

// jsonCell renders raw as an RFC 4180 quoted CSV cell — the whole cell in double
// quotes, every inner double quote doubled. It is the escaping a CSV author has
// to do by hand for a json column, so every fixture below goes through it.
func jsonCell(raw string) string {
	return `"` + strings.ReplaceAll(raw, `"`, `""`) + `"`
}

// jsonFile builds a one-column, one-row json fixture around raw.
func jsonFile(raw string) string {
	return "id,owner:json\nbrand:1," + jsonCell(raw) + "\n"
}

// TestJSONPackageDocExample loads the RFC 4180 quoted file printed verbatim in
// the package doc and in docs/src/concepts/providers.md. The escaping is the
// least obvious part of the feature, so the documented bytes are pinned rather
// than paraphrased.
func TestJSONPackageDocExample(t *testing.T) {
	p, err := p_load(t, strings.Join([]string{
		`id,owner:json`,
		`brand:1,"{""dept"":""eng"",""lead"":""alice""}"`,
		`brand:2,"{""dept"":""ops"",""tags"":[""oncall"",""eu""]}"`,
		`brand:3,`,
		``,
	}, "\n"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	objs, err := p.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []map[string]any{
		{"dept": "eng", "lead": "alice"},
		{"dept": "ops", "tags": []any{"oncall", "eu"}},
		nil, // brand:3 has an empty cell, so the field is omitted
	}
	for i, w := range want {
		got := objs[i].Metadata["owner"]
		if w == nil {
			if _, ok := objs[i].Metadata["owner"]; ok {
				t.Errorf("row %d owner = %#v, want the field omitted", i, got)
			}
			continue
		}
		if !reflect.DeepEqual(got, w) {
			t.Errorf("row %d owner = %#v, want %#v", i, got, w)
		}
	}
}

// TestJSONColumnAccepts walks the shapes a json cell may hold: a flat object,
// one nested object level, and an object holding a scalar array.
func TestJSONColumnAccepts(t *testing.T) {
	cases := map[string]struct {
		raw  string
		want map[string]any
	}{
		"flat object": {
			raw:  `{"dept":"eng","lead":"alice"}`,
			want: map[string]any{"dept": "eng", "lead": "alice"},
		},
		"nested object": {
			raw:  `{"lead":{"name":"alice"}}`,
			want: map[string]any{"lead": map[string]any{"name": "alice"}},
		},
		"object with a scalar array": {
			raw:  `{"dept":"eng","tags":["a","b"]}`,
			want: map[string]any{"dept": "eng", "tags": []any{"a", "b"}},
		},
		"empty object": {
			raw:  `{}`,
			want: map[string]any{},
		},
		"mixed scalars": {
			raw:  `{"active":true,"note":null,"seats":3}`,
			want: map[string]any{"active": true, "note": nil, "seats": int64(3)},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			p, err := p_load(t, jsonFile(tc.raw))
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			md, err := p.Fetch(context.Background(), identity.MustParse("brand:1"))
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			if !reflect.DeepEqual(md["owner"], tc.want) {
				t.Errorf("owner = %#v, want %#v", md["owner"], tc.want)
			}
		})
	}
}

// TestJSONNumbersMatchScalarColumns pins the documented int/float rule: an exact
// integer that fits int64 becomes an int64 exactly as :int does, and everything
// else a float64 exactly as :float does. UseNumber is what makes the large
// integer survive — a plain decode would float it and lose the last digit.
func TestJSONNumbersMatchScalarColumns(t *testing.T) {
	p, err := p_load(t, "id,owner:json,seats:int,budget:float\nbrand:1,"+
		jsonCell(`{"seats":3,"budget":1.5,"exp":1e3,"huge":9007199254740993,"list":[1,2.5]}`)+
		",3,1.5\n")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	md, err := p.Fetch(context.Background(), identity.MustParse("brand:1"))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	owner := md["owner"].(map[string]any)
	want := map[string]any{
		"seats":  int64(3),
		"budget": 1.5,
		"exp":    float64(1000), // "1e3" is not an integer literal, so it floats
		"huge":   int64(9007199254740993),
		"list":   []any{int64(1), 2.5},
	}
	if !reflect.DeepEqual(owner, want) {
		t.Fatalf("owner = %#v, want %#v", owner, want)
	}
	// The point of the rule: a nested number and a scalar column of the same
	// declared type land in the SAME Go type, so a cross-column comparison in a
	// rule is not a silent false.
	if owner["seats"] != md["seats"] {
		t.Errorf("owner.seats = %T(%v), seats = %T(%v); the types must agree",
			owner["seats"], owner["seats"], md["seats"], md["seats"])
	}
	if owner["budget"] != md["budget"] {
		t.Errorf("owner.budget = %T(%v), budget = %T(%v); the types must agree",
			owner["budget"], owner["budget"], md["budget"], md["budget"])
	}
}

// TestJSONColumnRejects covers every way a json cell fails. A top-level array, a
// top-level scalar, and malformed JSON are APERTURE_CONFIG_INVALID from the
// loader; a shape, depth, or size violation below the top level is
// APERTURE_METADATA_INVALID from the shared value model. Both name the column
// and the line, and neither may panic or silently store the raw string.
func TestJSONColumnRejects(t *testing.T) {
	cases := map[string]struct {
		raw  string
		code aerr.Code
	}{
		"top-level array":        {`["a","b"]`, aerr.APERTURE_CONFIG_INVALID},
		"top-level array of one": {`[{"dept":"eng"}]`, aerr.APERTURE_CONFIG_INVALID},
		"top-level string":       {`"eng"`, aerr.APERTURE_CONFIG_INVALID},
		"top-level number":       {`5`, aerr.APERTURE_CONFIG_INVALID},
		"top-level bool":         {`true`, aerr.APERTURE_CONFIG_INVALID},
		"top-level null":         {`null`, aerr.APERTURE_CONFIG_INVALID},
		"malformed JSON":         {`{"dept":`, aerr.APERTURE_CONFIG_INVALID},
		"unquoted key":           {`{dept:"eng"}`, aerr.APERTURE_CONFIG_INVALID},
		"trailing content":       {`{"dept":"eng"} junk`, aerr.APERTURE_CONFIG_INVALID},
		"unrepresentable number": {`{"n":1e400}`, aerr.APERTURE_CONFIG_INVALID},
		"array of objects":       {`{"members":[{"id":1}]}`, aerr.APERTURE_METADATA_INVALID},
		"nested array":           {`{"tags":[["a"]]}`, aerr.APERTURE_METADATA_INVALID},
		"over depth":             {`{"a":{"b":{"c":"x"}}}`, aerr.APERTURE_METADATA_INVALID},
		"empty key":              {`{"":"x"}`, aerr.APERTURE_METADATA_INVALID},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := p_load(t, jsonFile(tc.raw))
			if got := aerr.CodeOf(err); got != tc.code {
				t.Fatalf("code = %s, want %s (err %v)", got, tc.code, err)
			}
			// The loader names the column and the line in the coded context;
			// the value-model wrap names them in the message. Either is enough
			// for an operator to find the cell.
			ctx := codedContext(t, err)
			if ctx["field"] != "owner" && !strings.Contains(err.Error(), "owner") {
				t.Errorf("error should name the column, got %q / %#v", err.Error(), ctx)
			}
			if ctx["line"] != 2 && !strings.Contains(err.Error(), "line 2") {
				t.Errorf("error should name the line, got %q / %#v", err.Error(), ctx)
			}
			// The raw cell is host data and must never reach a diagnostic.
			if strings.Contains(err.Error(), tc.raw) {
				t.Errorf("error leaks the cell contents: %q", err.Error())
			}
		})
	}
}

// TestJSONOversizedCellRejected proves the size cap reaches inside a json cell
// too: the failure is at LOAD, not on the Check hot path.
func TestJSONOversizedCellRejected(t *testing.T) {
	huge := strings.Repeat("x", provider.DefaultMaxValueBytes+1)
	_, err := p_load(t, jsonFile(`{"note":"`+huge+`"}`))
	if got := aerr.CodeOf(err); got != aerr.APERTURE_METADATA_INVALID {
		t.Fatalf("code = %s, want APERTURE_METADATA_INVALID", got)
	}
	if !strings.Contains(err.Error(), "line 2") {
		t.Errorf("error should name the line, got %q", err.Error())
	}
}

// TestJSONEmptyCellOmitsField documents the empty-cell rule: a json column
// follows the SCALAR rule, not the list rule. An absent object is meaningfully
// different from an empty one.
func TestJSONEmptyCellOmitsField(t *testing.T) {
	p, err := p_load(t, "id,owner:json,tags:list\nbrand:1,,\n")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	md, err := p.Fetch(context.Background(), identity.MustParse("brand:1"))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if _, ok := md["owner"]; ok {
		t.Errorf("owner = %#v, want the field omitted", md["owner"])
	}
	// The list column in the same row still yields [], so the two rules stay
	// visibly distinct.
	if !reflect.DeepEqual(md["tags"], []any{}) {
		t.Errorf("tags = %#v, want []", md["tags"])
	}
}

// TestJSONHeaderSuffixComposition holds :json to the same header grammar as the
// scalars: it takes no element type and no delimiter, and misspelling it is an
// unknown type rather than a silent string column.
func TestJSONHeaderSuffixComposition(t *testing.T) {
	for name, content := range map[string]string{
		"element type on json": "id,owner:json<int>\nbrand:1," + jsonCell(`{"a":1}`) + "\n",
		"delimiter on json":    "id,owner:json(;)\nbrand:1," + jsonCell(`{"a":1}`) + "\n",
		"both on json":         "id,owner:json<int>(;)\nbrand:1," + jsonCell(`{"a":1}`) + "\n",
		"json as list element": "id,owner:list<json>\nbrand:1,a\n",
		"misspelled json":      "id,owner:jsonn\nbrand:1,a\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := p_load(t, content)
			if got := aerr.CodeOf(err); got != aerr.APERTURE_CONFIG_INVALID {
				t.Fatalf("code = %s, want APERTURE_CONFIG_INVALID", got)
			}
			if ctx := codedContext(t, err); ctx["name"] != "owner" {
				t.Errorf("context = %#v, want the column named", ctx)
			}
		})
	}
}

// TestJSONColumnComposesWithTheRestOfTheGrammar puts a json column beside every
// other column type in one header and walks the row through Fetch.
func TestJSONColumnComposesWithTheRestOfTheGrammar(t *testing.T) {
	p, err := p_load(t, "id,tier,seats:int,active:bool,tags:list,ranks:list<int>,aliases:list(;),owner:json\n"+
		"brand:1,gold,40,true,premium|launch,3|5,acme;acme-co,"+jsonCell(`{"dept":"eng","tags":["a","b"]}`)+"\n")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	md, err := p.Fetch(context.Background(), identity.MustParse("brand:1"))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	want := provider.Metadata{
		"tier":    "gold",
		"seats":   int64(40),
		"active":  true,
		"tags":    []any{"premium", "launch"},
		"ranks":   []any{int64(3), int64(5)},
		"aliases": []any{"acme", "acme-co"},
		"owner":   map[string]any{"dept": "eng", "tags": []any{"a", "b"}},
	}
	if !reflect.DeepEqual(md, want) {
		t.Fatalf("metadata = %#v, want %#v", md, want)
	}
}

// TestJSONObjectsAreNeverShared is TestListSlicesAreNeverShared for objects: two
// rows with byte-identical cells must not share one map, and a Reload must not
// touch a map already handed out.
func TestJSONObjectsAreNeverShared(t *testing.T) {
	cell := jsonCell(`{"dept":"eng","tags":["a"]}`)
	path := write(t, "id,owner:json\nbrand:1,"+cell+"\nbrand:2,"+cell+"\n")
	p := New(path)

	objs, err := p.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	first := objs[0].Metadata["owner"].(map[string]any)
	second := objs[1].Metadata["owner"].(map[string]any)
	if reflect.ValueOf(first).UnsafePointer() == reflect.ValueOf(second).UnsafePointer() {
		t.Fatal("two rows share one object; each row must own its map")
	}
	// The contract is transitive, so the nested array must not be shared either.
	firstTags := first["tags"].([]any)
	secondTags := second["tags"].([]any)
	if reflect.ValueOf(firstTags).UnsafePointer() == reflect.ValueOf(secondTags).UnsafePointer() {
		t.Fatal("two rows share one nested array; the read-only contract is transitive")
	}

	next := jsonCell(`{"dept":"ops","tags":["b"]}`)
	if err := os.WriteFile(path, []byte("id,owner:json\nbrand:1,"+next+"\nbrand:2,"+cell+"\n"), 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if err := p.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if !reflect.DeepEqual(first, map[string]any{"dept": "eng", "tags": []any{"a"}}) {
		t.Errorf("pre-reload map mutated: %#v", first)
	}
	after, err := p.Fetch(context.Background(), identity.MustParse("brand:1"))
	if err != nil {
		t.Fatalf("Fetch after reload: %v", err)
	}
	if !reflect.DeepEqual(after["owner"], map[string]any{"dept": "ops", "tags": []any{"b"}}) {
		t.Errorf("after reload owner = %#v", after["owner"])
	}
}
