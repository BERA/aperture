package csvprovider

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/identity"
	"github.com/frankbardon/aperture/provider"
)

// writeNamed drops content into a temp file called name and returns its path. It
// is write with the file NAME under the test's control, because half of what the
// attribute loader promises is that its errors say which file.
func writeNamed(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

// attrs loads content as an attribute file, failing the test if it will not
// load.
func attrs(t *testing.T, content string) *Attributes {
	t.Helper()
	a, err := NewAttributes(writeNamed(t, "users.csv", content))
	if err != nil {
		t.Fatalf("NewAttributes: %v", err)
	}
	return a
}

// everySpelling is one row exercising every column-suffix spelling of the value
// model at once: the four scalars, both date granularities, a plain list, an
// element-typed list, a re-delimited list, and a json object.
const everySpelling = "dept,seats:int,active:bool,budget:float,tags:list,aliases:list(;)," +
	"ratios:list<float>,hired_at:date,seen_at:datetime,owner:json"

const everySpellingCells = "eng,12,true,9.5,platform|oncall,acme;acme-co,0.5|1.5," +
	"2024-03-04,2024-03-04T12:30:00Z," +
	`"{""dept"":""eng"",""seats"":12}"`

// TestAttributesSpellTheValueModelExactlyAsObjectsDo is the story's central
// claim, asserted rather than asserted-by-inspection: "same shape and rules as
// object providers" buys the VALUE MODEL, and it buys it whole.
//
// The same header and the same cells are loaded through both seams — once as an
// object row keyed "brand:1", once as an attribute row keyed "alice" — and the
// two bags must be deeply equal. Every suffix is in that row, so a divergence in
// any one of them (an int that came back a string, a list that came back a blob,
// a date that kept the cell's spelling instead of the canonical one) fails here.
//
// It would be cheaper to assert the attribute bag against a hand-written
// expectation, and it would be worth less: the property is not "these are the
// values", it is "there is ONE value model", and only a comparison against the
// other seam can say that.
func TestAttributesSpellTheValueModelExactlyAsObjectsDo(t *testing.T) {
	ctx := context.Background()

	object := New(write(t, "id,"+everySpelling+"\nbrand:1,"+everySpellingCells+"\n"))
	want, err := object.Fetch(ctx, identity.MustParse("brand:1"))
	if err != nil {
		t.Fatalf("object Fetch: %v", err)
	}

	subject := attrs(t, "id,"+everySpelling+"\nalice,"+everySpellingCells+"\n")
	got, err := subject.Fetch(ctx, "alice")
	if err != nil {
		t.Fatalf("attribute Fetch: %v", err)
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the two seams disagree about the value model:\n attribute = %#v\n object    = %#v", got, want)
	}
	// A spot check that the comparison above is not two identical wrongs: the
	// typed values are the typed values, because an untyped one decides
	// differently (5 in a []string{"5"} is false).
	if got["seats"] != int64(12) {
		t.Errorf("seats = %#v; want int64(12)", got["seats"])
	}
	if !reflect.DeepEqual(got["tags"], []any{"platform", "oncall"}) {
		t.Errorf("tags = %#v; want a real []any", got["tags"])
	}
	if got["hired_at"] != "2024-03-04" {
		t.Errorf("hired_at = %#v; want the canonical date string", got["hired_at"])
	}
}

// TestAttributesFetchAnsweringAndMissing pins the distinction the whole seam
// exists to preserve: a key the file has, and a key it does not, are different
// answers — not an empty bag for both. An unknown subject must be
// APERTURE_NOT_FOUND, because the resolvers above collapse exactly that code (and
// an unregistered slot) into "decide against the floor", and collapse nothing
// else.
func TestAttributesFetchAnsweringAndMissing(t *testing.T) {
	ctx := context.Background()
	a := attrs(t, "id,department\nalice,eng\nbob,sales\n")

	md, err := a.Fetch(ctx, "alice")
	if err != nil {
		t.Fatalf("Fetch(alice): %v", err)
	}
	if md["department"] != "eng" {
		t.Errorf("department = %#v; want eng", md["department"])
	}
	if _, err := a.Fetch(ctx, "mallory"); aerr.CodeOf(err) != aerr.APERTURE_NOT_FOUND {
		t.Fatalf("code = %s; want APERTURE_NOT_FOUND", aerr.CodeOf(err))
	}
}

// TestAttributesListAndQuery covers the admin read: List enumerates in FILE
// order, and Query filters through provider.MatchFields — so a list column
// matches by membership and everything else by typed equality — with Limit
// honoured.
func TestAttributesListAndQuery(t *testing.T) {
	ctx := context.Background()
	a := attrs(t, strings.Join([]string{
		"id,department,teams:list,clearance:int",
		"alice,eng,platform|oncall,3",
		"bob,sales,crm,1",
		"carol,eng,platform,2",
		"",
	}, "\n"))

	list, err := a.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var ids []string
	for _, rec := range list {
		ids = append(ids, rec.ID)
	}
	if want := []string{"alice", "bob", "carol"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("List order = %v; want file order %v", ids, want)
	}
	if a.Len() != 3 {
		t.Errorf("Len() = %d; want 3", a.Len())
	}

	cases := []struct {
		name   string
		filter provider.AttributeFilter
		want   []string
	}{
		{
			name:   "scalar equality",
			filter: provider.AttributeFilter{Fields: map[string]any{"department": "eng"}},
			want:   []string{"alice", "carol"},
		},
		{
			// A collection field matches by MEMBERSHIP, which is the half of the
			// Fields contract a naive equality implementation gets wrong.
			name:   "collection membership",
			filter: provider.AttributeFilter{Fields: map[string]any{"teams": "platform"}},
			want:   []string{"alice", "carol"},
		},
		{
			// Typed equality: the column is :int, so "3" is not 3.
			name:   "a string never equals an int",
			filter: provider.AttributeFilter{Fields: map[string]any{"clearance": "3"}},
			want:   nil,
		},
		{
			name:   "typed equality",
			filter: provider.AttributeFilter{Fields: map[string]any{"clearance": int64(3)}},
			want:   []string{"alice"},
		},
		{
			// Every predicate must hold — the map is an AND.
			name: "two predicates are an AND",
			filter: provider.AttributeFilter{Fields: map[string]any{
				"department": "eng", "teams": "oncall",
			}},
			want: []string{"alice"},
		},
		{
			name:   "limit",
			filter: provider.AttributeFilter{Fields: map[string]any{"department": "eng"}, Limit: 1},
			want:   []string{"alice"},
		},
		{
			// A field ABSENT from a bag never matches.
			name:   "an absent field never matches",
			filter: provider.AttributeFilter{Fields: map[string]any{"region": "eu"}},
			want:   nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recs, err := a.Query(ctx, tc.filter)
			if err != nil {
				t.Fatalf("Query: %v", err)
			}
			var got []string
			for _, rec := range recs {
				got = append(got, rec.ID)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Query = %v; want %v", got, tc.want)
			}
		})
	}
}

// TestAttributesMalformedFileFailsAtLoadNamingTheFileAndTheRow walks the ways an
// attribute file can be wrong. Each one must be a coded error from NewAttributes
// — not from the first decision that needed the slot — and must carry the whole
// address of the fault: which file, and which row.
func TestAttributesMalformedFileFailsAtLoadNamingTheFileAndTheRow(t *testing.T) {
	cases := []struct {
		name string
		body string
		want aerr.Code
		line int // the row the fault is in; 0 when it is the header's
	}{
		{
			name: "no id column",
			body: "department\neng\n",
			want: aerr.APERTURE_CONFIG_INVALID,
		},
		{
			name: "unknown column type",
			body: "id,clearance:integer\nalice,3\n",
			want: aerr.APERTURE_CONFIG_INVALID,
		},
		{
			name: "malformed type suffix",
			body: "id,teams:list(;<string>\nalice,a;b\n",
			want: aerr.APERTURE_CONFIG_INVALID,
		},
		{
			name: "cell will not coerce",
			body: "id,clearance:int\nalice,three\n",
			want: aerr.APERTURE_CONFIG_INVALID,
			line: 2,
		},
		{
			name: "wrong column count",
			body: "id,department\nalice,eng,extra\n",
			want: aerr.APERTURE_CONFIG_INVALID,
			line: 2,
		},
		{
			name: "empty key",
			body: "id,department\n,eng\n",
			want: aerr.APERTURE_CONFIG_INVALID,
			line: 2,
		},
		{
			name: "duplicate key",
			body: "id,department\nalice,eng\nalice,sales\n",
			want: aerr.APERTURE_CONFIG_INVALID,
			line: 3,
		},
		{
			// The date value model, unchanged: an explicit offset is a load error
			// rather than a silent conversion that can move the day.
			name: "date cell the value model refuses",
			body: "id,hired_at:date\nalice,2024-03-04T00:00:00+05:00\n",
			want: aerr.APERTURE_CONFIG_INVALID,
			line: 2,
		},
		{
			// The shared value model, unchanged: a bag that breaks a cap fails the
			// LOAD with the model's own code, so the shape never reaches a Check.
			name: "value the model refuses",
			body: "id,note\nalice," + strings.Repeat("x", provider.DefaultMaxValueBytes+1) + "\n",
			want: aerr.APERTURE_METADATA_INVALID,
			line: 2,
		},
		{
			// "*" is the all-accounts grant sentinel. A row filed under it asks for
			// the attributes of every account, and the only bag that could answer
			// is one account's data served as another's — so it is refused with the
			// registry's own code, at the line that holds it.
			name: "the account wildcard as a key",
			body: "id,plan\n*,enterprise\n",
			want: aerr.APERTURE_ATTRIBUTE_PROVIDER_INVALID,
			line: 2,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeNamed(t, "users.csv", tc.body)
			a, err := NewAttributes(path)
			if err == nil {
				t.Fatalf("NewAttributes accepted a malformed file (%d rows)", a.Len())
			}
			if got := aerr.CodeOf(err); got != tc.want {
				t.Fatalf("code = %s; want %s (%v)", got, tc.want, err)
			}
			ctx := codedContext(t, err)
			if ctx["path"] != path {
				t.Errorf("error does not name the file: context = %v", ctx)
			}
			// And in the MESSAGE, not only the context: `aperture check` prints
			// the code and the message and nothing else, so an address that lives
			// only in a structured field is an address the operator never sees.
			if !strings.Contains(err.Error(), path) {
				t.Errorf("error message does not name the file: %v", err)
			}
			// The row is named in the context where the loader raised the error
			// itself, and in the message where it classified someone else's
			// (encoding/csv's field-count error, the value model's rejection). Both
			// count: what the criterion asks for is that the operator is told which
			// row, not which field of the error carries it.
			if tc.line != 0 && ctx["line"] != tc.line &&
				!strings.Contains(err.Error(), fmt.Sprintf("line %d", tc.line)) {
				t.Errorf("error does not name the row: %v (context = %v)", err, ctx)
			}
		})
	}
}

// TestAttributesLoadEagerly is the reason NewAttributes returns an error at all.
// A missing or unreadable file is a build failure, not a surprise on the first
// decision that needed the slot: an attribute bag is read by every rule against
// every object in a decision, so a file that cannot be read is not one type
// failing to answer — it is the whole slot — and boot is where an operator is
// present to fix it.
func TestAttributesLoadEagerly(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.csv")
	_, err := NewAttributes(missing)
	if err == nil {
		t.Fatal("NewAttributes accepted a file that does not exist")
	}
	if got := aerr.CodeOf(err); got != aerr.APERTURE_CONFIG_INVALID {
		t.Fatalf("code = %s; want APERTURE_CONFIG_INVALID", got)
	}
	if ctx := codedContext(t, err); ctx["path"] != missing {
		t.Errorf("error does not name the file: context = %v", ctx)
	}
}

// TestAttributeKeysAreOpaqueAndNeverParsed is the asymmetry an author is most
// likely to get wrong by copying an object provider's file, written as a test so
// the behaviour is pinned rather than merely documented.
//
// An attribute key is an opaque handle from the host's directory. "user:alice"
// is therefore a LEGAL key — it loads, it enumerates, it caches — and it is also
// a key nothing will ever fetch, because the decision path fetches by the
// principal's bare id. The loader cannot detect that, and this test says so out
// loud: the file with identity-shaped ids answers nothing for "alice" while the
// file with bare ids answers.
func TestAttributeKeysAreOpaqueAndNeverParsed(t *testing.T) {
	ctx := context.Background()

	identityShaped := attrs(t, "id,department\nuser:alice,eng\n")
	if _, err := identityShaped.Fetch(ctx, "alice"); aerr.CodeOf(err) != aerr.APERTURE_NOT_FOUND {
		t.Fatalf("a bare id matched an identity-shaped key: %v", err)
	}
	// Stored verbatim, un-parsed: it is a string, not an identity.
	md, err := identityShaped.Fetch(ctx, "user:alice")
	if err != nil {
		t.Fatalf(`Fetch("user:alice"): %v`, err)
	}
	if md["department"] != "eng" {
		t.Errorf("department = %#v; want eng", md["department"])
	}

	// The same file written the RIGHT way answers the fetch the decision path
	// actually makes. A key with no identity grammar at all is equally fine —
	// Aperture never parses it.
	bare := attrs(t, "id,department\nalice,eng\nsvc/ci-runner@example,ops\n")
	if _, err := bare.Fetch(ctx, "alice"); err != nil {
		t.Fatalf("Fetch(alice): %v", err)
	}
	if _, err := bare.Fetch(ctx, "svc/ci-runner@example"); err != nil {
		t.Fatalf("a key with no identity grammar was rejected: %v", err)
	}
}

// TestAttributesReloadSwapsTheSetAndKeepsOldBagsImmutable is the read-only
// contract at the reload seam: a bag already handed out (and cached by the
// registry) must not change under its holder, so Reload builds a fresh set and
// swaps it rather than editing maps in place.
func TestAttributesReloadSwapsTheSetAndKeepsOldBagsImmutable(t *testing.T) {
	ctx := context.Background()
	path := writeNamed(t, "users.csv", "id,department\nalice,eng\n")
	a, err := NewAttributes(path)
	if err != nil {
		t.Fatalf("NewAttributes: %v", err)
	}
	before, err := a.Fetch(ctx, "alice")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	if err := os.WriteFile(path, []byte("id,department\nalice,sales\nbob,eng\n"), 0o600); err != nil {
		t.Fatalf("rewrite fixture: %v", err)
	}
	if err := a.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if before["department"] != "eng" {
		t.Errorf("a bag handed out before the reload changed: %#v", before)
	}
	after, err := a.Fetch(ctx, "alice")
	if err != nil {
		t.Fatalf("Fetch after reload: %v", err)
	}
	if after["department"] != "sales" {
		t.Errorf("department = %#v; want the reloaded sales", after["department"])
	}
	if _, err := a.Fetch(ctx, "bob"); err != nil {
		t.Errorf("the reloaded row is missing: %v", err)
	}

	// A file that no longer parses leaves the current set alone: a bad edit
	// degrades to stale data, never to no directory at all.
	if err := os.WriteFile(path, []byte("id,clearance:int\nalice,three\n"), 0o600); err != nil {
		t.Fatalf("rewrite fixture: %v", err)
	}
	if err := a.Reload(); err == nil {
		t.Fatal("Reload accepted a malformed file")
	}
	if md, err := a.Fetch(ctx, "alice"); err != nil || md["department"] != "sales" {
		t.Errorf("a failed reload disturbed the served set: %#v (%v)", md, err)
	}
}

// TestAttributesFromReaderCannotReload mirrors FromReader's contract on the
// object seam: there is no path to re-read, so the refusal is explicit rather
// than a silent no-op that would leave a caller believing it had refreshed.
func TestAttributesFromReaderCannotReload(t *testing.T) {
	a, err := AttributesFromReader(strings.NewReader("id,department\nalice,eng\n"))
	if err != nil {
		t.Fatalf("AttributesFromReader: %v", err)
	}
	if _, err := a.Fetch(context.Background(), "alice"); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if err := a.Reload(); aerr.CodeOf(err) != aerr.APERTURE_CONFIG_INVALID {
		t.Fatalf("code = %s; want APERTURE_CONFIG_INVALID", aerr.CodeOf(err))
	}
}

// TestAttributesSatisfyTheRegistrySeam wires the loader the way a host does and
// reads back through the registry, so the adapter is proven against the real
// AttributeRegistry — its cache, its key guard, its Fields re-enforcement — and
// not only against its own methods.
func TestAttributesSatisfyTheRegistrySeam(t *testing.T) {
	ctx := context.Background()
	reg := provider.NewAttributeRegistry()
	reg.MustRegister(provider.AttributeSlotUser,
		attrs(t, "id,department,teams:list\nalice,eng,platform|oncall\nbob,sales,crm\n"),
		provider.WithTTL(0))

	// The principal resolver seam: a kind and a bare id in, the host's bag out.
	bag, err := reg.Attributes(ctx, "user", "alice")
	if err != nil {
		t.Fatalf("Attributes: %v", err)
	}
	if bag["department"] != "eng" {
		t.Errorf("department = %#v; want eng", bag["department"])
	}
	// Leniency: an unknown subject is not a failed decision.
	if bag, err := reg.Attributes(ctx, "user", "mallory"); err != nil || bag != nil {
		t.Errorf("an unknown subject = %#v, %v; want a nil bag and no error", bag, err)
	}
	// The system-tier admin read, through the registry's own Fields enforcement.
	recs, err := reg.Enumerate(ctx, provider.AttributeSlotUser,
		provider.AttributeFilter{Fields: map[string]any{"teams": "oncall"}})
	if err != nil {
		t.Fatalf("Enumerate: %v", err)
	}
	if len(recs) != 1 || recs[0].ID != "alice" {
		t.Fatalf("Enumerate = %+v; want alice alone", recs)
	}
}
