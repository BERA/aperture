package sqlprovider

import (
	"context"
	"database/sql/driver"
	stderrors "errors"
	"reflect"
	"strings"
	"testing"
	"time"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/provider"
)

// The attribute seam's tests, over the same fake driver the object seam uses.
//
// They pin the two things that are DIFFERENT — a bare key bound verbatim, a bare
// id selected — and the one thing that must be IDENTICAL: the driver-value
// mapping. The mapping is proved by replaying mappingCases() through
// Attributes.Fetch, so a divergent copy of the table would fail here rather than
// be discovered as a decision that disagrees with itself.

const (
	attrFetch = `SELECT department, clearance FROM users WHERE id = $1`
	attrAll   = `SELECT u.id AS id, u.department FROM users u`
)

// newAttributes builds an *Attributes over s, defaulting the fields a test does
// not care about.
func newAttributes(t *testing.T, s *script, cfg AttributeConfig) *Attributes {
	t.Helper()
	if cfg.Slot == "" {
		cfg.Slot = provider.AttributeSlotUser
	}
	if cfg.FetchQuery == "" {
		cfg.FetchQuery = attrFetch
	}
	a, err := NewAttributes(newFakeDB(t, s), cfg)
	if err != nil {
		t.Fatalf("NewAttributes: %v", err)
	}
	return a
}

// TestAttributesFetchBindsTheBareKeyVerbatim is the first of the two contract
// differences from an object provider.
//
// An object provider binds the identity's TERMINAL SEGMENT VALUE, so "brand:42"
// and "account:acme/brand:42" both bind "42". An attribute key has no segments
// to strip, so whatever the decision path holds is what reaches the placeholder
// — including a key that merely LOOKS like an identity, which is a legal opaque
// handle and must not be parsed.
func TestAttributesFetchBindsTheBareKeyVerbatim(t *testing.T) {
	for _, key := range []string{
		"alice",
		"acme",
		"11111111-1111-1111-1111-111111111111",
		// A host whose directory keys genuinely contain a colon. Nothing is
		// stripped: an attribute key is opaque and Aperture never parses it.
		"user:alice",
		"account:acme/user:alice",
	} {
		t.Run(key, func(t *testing.T) {
			s := &script{cols: []string{"department"}, rows: [][]driver.Value{{"eng"}}}
			a := newAttributes(t, s, AttributeConfig{})
			if _, err := a.Fetch(context.Background(), key); err != nil {
				t.Fatalf("Fetch(%q): %v", key, err)
			}
			calls, query, args := s.observed()
			if calls != 1 {
				t.Fatalf("statement ran %d times, want 1", calls)
			}
			if query != attrFetch {
				t.Errorf("ran %q, want the configured get_one", query)
			}
			if len(args) != 1 || args[0] != any(key) {
				t.Fatalf("bound %#v, want exactly the bare key %q — nothing is stripped", args, key)
			}
		})
	}
}

// TestAttributesFetchColumnsBecomeFields: the SELECT list is the field list,
// keyed by column name, exactly as it is for an object.
func TestAttributesFetchColumnsBecomeFields(t *testing.T) {
	s := &script{
		cols: []string{"department", "clearance", "active", "teams"},
		rows: [][]driver.Value{{"eng", int64(3), true, []byte(`["platform","oncall"]`)}},
	}
	a := newAttributes(t, s, AttributeConfig{})
	bag, err := a.Fetch(context.Background(), "alice")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	want := provider.Metadata{
		"department": "eng",
		"clearance":  int64(3),
		"active":     true,
		"teams":      []any{"platform", "oncall"},
	}
	if !reflect.DeepEqual(map[string]any(bag), map[string]any(want)) {
		t.Fatalf("bag = %#v, want %#v", bag, want)
	}
	// The bag the decision path reads must be legal metadata. This is the
	// guarantee every rule above it is entitled to assume.
	if err := provider.ValidateMetadata(bag); err != nil {
		t.Fatalf("bag violates the value model: %v", err)
	}
}

// TestAttributeDriverValueMappingIsTheSameTable replays every case of the object
// seam's mapping table through the ATTRIBUTE seam.
//
// It is the assertion that there is no second mapping. A column read through an
// object provider and the same column read through an attribute provider must
// produce the same Go value, because the rules engine compares them with the same
// semantics — an int64 on one side and a float64 on the other is a silent false.
func TestAttributeDriverValueMappingIsTheSameTable(t *testing.T) {
	for _, tc := range mappingCases() {
		t.Run(tc.name, func(t *testing.T) {
			s := &script{cols: []string{"col"}, rows: [][]driver.Value{{tc.in}}}
			a := newAttributes(t, s, AttributeConfig{})
			bag, err := a.Fetch(context.Background(), "alice")

			if tc.wantErr {
				mustCode(t, err, aerr.APERTURE_SQL_PROVIDER_SCAN)
				var ce *aerr.CodedError
				if !stderrors.As(err, &ce) {
					t.Fatalf("error is not a *CodedError: %v", err)
				}
				if got := ce.Context["column"]; got != "col" {
					t.Fatalf("error context column = %v, want the column named", got)
				}
				// The KEY, where the object seam names the identity: the same
				// address, spelled for this seam.
				if got := ce.Context["id"]; got != "alice" {
					t.Fatalf("error context id = %v, want the row's key named", got)
				}
				if got := ce.Context["reason"]; got != tc.wantReason {
					t.Fatalf("error context reason = %v, want %q", got, tc.wantReason)
				}
				return
			}

			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			got, present := bag["col"]
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
		})
	}
}

// TestAttributesFetchAbsentAmbiguousBroken: the three outcomes a fetch can have
// are three different errors, and the distinction is the whole taxonomy.
//
// NOT_FOUND is the only one that is a normal answer — the registry resolves it
// to the floor bag — so collapsing an unreachable directory into it would turn an
// outage into a silent authorization change.
func TestAttributesFetchAbsentAmbiguousBroken(t *testing.T) {
	t.Run("no rows is NOT_FOUND", func(t *testing.T) {
		a := newAttributes(t, &script{cols: []string{"department"}}, AttributeConfig{})
		_, err := a.Fetch(context.Background(), "mallory")
		mustCode(t, err, aerr.APERTURE_NOT_FOUND)
	})

	t.Run("two rows is AMBIGUOUS, never the first row", func(t *testing.T) {
		s := &script{cols: []string{"department"}, rows: [][]driver.Value{{"eng"}, {"sales"}}}
		a := newAttributes(t, s, AttributeConfig{})
		_, err := a.Fetch(context.Background(), "alice")
		mustCode(t, err, aerr.APERTURE_SQL_PROVIDER_AMBIGUOUS)
		if !strings.Contains(err.Error(), "LIMIT 1") {
			t.Errorf("the refusal does not warn against LIMIT 1: %v", err)
		}
	})

	t.Run("a driver failure is QUERY, not NOT_FOUND", func(t *testing.T) {
		s := &script{cols: []string{"department"}, queryErr: stderrors.New("connection refused")}
		a := newAttributes(t, s, AttributeConfig{})
		_, err := a.Fetch(context.Background(), "alice")
		mustCode(t, err, aerr.APERTURE_SQL_PROVIDER_QUERY)
		// The driver's error stays reachable, and the slot is named so an
		// operator running three directories over one pool knows which is down.
		if !strings.Contains(err.Error(), "connection refused") {
			t.Errorf("the driver error was not wrapped verbatim: %v", err)
		}
		if !strings.Contains(err.Error(), `"user"`) {
			t.Errorf("the failure does not name the slot: %v", err)
		}
	})

	t.Run("a mid-stream failure is QUERY, not NOT_FOUND", func(t *testing.T) {
		s := &script{cols: []string{"department"}, rowsErr: stderrors.New("connection reset")}
		a := newAttributes(t, s, AttributeConfig{})
		_, err := a.Fetch(context.Background(), "alice")
		mustCode(t, err, aerr.APERTURE_SQL_PROVIDER_QUERY)
	})
}

// TestAttributesCodedQuerierErrorPassesThrough: Wrap RE-STAMPS, so a host's
// wrapping Querier that already classified a failure must not have its code
// replaced — and, because a same-code re-stamp is invisible to CodeOf, the
// assertion is on CHAIN DEPTH.
func TestAttributesCodedQuerierErrorPassesThrough(t *testing.T) {
	coded := aerr.New(aerr.APERTURE_UNAUTHENTICATED, "host querier: the read replica rejected the credential")
	s := &script{cols: []string{"department"}, queryErr: coded}
	a := newAttributes(t, s, AttributeConfig{})
	_, err := a.Fetch(context.Background(), "alice")
	mustCode(t, err, aerr.APERTURE_UNAUTHENTICATED)
	if got := codedDepth(err); got != 1 {
		t.Fatalf("the chain carries %d Aperture-coded errors, want exactly 1 — the wrapper re-stamped", got)
	}
}

// codedDepth counts the Aperture-coded errors in a chain. One is right; two means
// a wrapper added a layer where the guard should have returned the error as-is.
func codedDepth(err error) int {
	n := 0
	for err != nil {
		var ce *aerr.CodedError
		if !stderrors.As(err, &ce) {
			break
		}
		n++
		err = ce.Inner
	}
	return n
}

// TestAttributesEnumerateSelectsABareId is the second contract difference.
//
// The id column holds the host's BARE key, it is removed from the row before the
// rest becomes the bag, and the "get all" statement binds NO parameters.
func TestAttributesEnumerateSelectsABareId(t *testing.T) {
	s := &script{
		cols: []string{"id", "department", "clearance"},
		rows: [][]driver.Value{
			{"alice", "eng", int64(3)},
			{"bob", "sales", int64(1)},
		},
	}
	a := newAttributes(t, s, AttributeConfig{ListQuery: attrAll})
	recs, err := a.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []provider.AttributeRecord{
		{ID: "alice", Attributes: provider.Metadata{"department": "eng", "clearance": int64(3)}},
		{ID: "bob", Attributes: provider.Metadata{"department": "sales", "clearance": int64(1)}},
	}
	if !reflect.DeepEqual(recs, want) {
		t.Fatalf("List = %#v, want %#v", recs, want)
	}
	calls, query, args := s.observed()
	if calls != 1 || query != attrAll {
		t.Fatalf("ran %d statements, last %q; want one get_all", calls, query)
	}
	if len(args) != 0 {
		t.Fatalf("the get_all statement bound %#v, want no parameters", args)
	}
}

// TestAttributesEnumerateHonoursACustomIDColumn: id_column names the ALIAS the
// statement actually uses, and the aliased column is still the key rather than a
// field.
func TestAttributesEnumerateHonoursACustomIDColumn(t *testing.T) {
	s := &script{
		cols: []string{"account_id", "plan"},
		rows: [][]driver.Value{{"acme", "enterprise"}},
	}
	a := newAttributes(t, s, AttributeConfig{
		Slot:      provider.AttributeSlotAccount,
		ListQuery: `SELECT a.id AS account_id, a.plan FROM accounts a`,
		IDColumn:  "account_id",
	})
	recs, err := a.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(recs) != 1 || recs[0].ID != "acme" {
		t.Fatalf("List = %#v, want one record keyed acme", recs)
	}
	if _, leaked := recs[0].Attributes["account_id"]; leaked {
		t.Error("the id column leaked into the bag; it is the key, not a field")
	}
}

// TestAttributesCannotDetectAnIdentityShapedKey pins the trap as BEHAVIOUR, so
// nobody "fixes" it by teaching this package to parse a key.
//
// 'user:' || u.id AS id is the mistake an author copying an object provider's
// statement makes. It produces a perfectly legal opaque key: the row enumerates,
// it caches, and it matches no principal id a Fetch will ever present. There is
// no error to raise — which is precisely why the asymmetry is documented on the
// type, on seed's AttributeProvider.GetAll, and in the fixups of
// APERTURE_SQL_PROVIDER_ROW_IDENTITY.
func TestAttributesCannotDetectAnIdentityShapedKey(t *testing.T) {
	s := &script{
		cols: []string{"id", "department"},
		rows: [][]driver.Value{{"user:alice", "eng"}},
	}
	a := newAttributes(t, s, AttributeConfig{
		ListQuery: `SELECT 'user:' || u.id AS id, u.department FROM users u`,
	})
	recs, err := a.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v — an identity-shaped key is a legal opaque key and must not error", err)
	}
	if len(recs) != 1 || recs[0].ID != "user:alice" {
		t.Fatalf("List = %#v, want the key stored verbatim", recs)
	}
}

// TestAttributesEnumerateRefusesAnUnusableRowKey: a row Aperture cannot key is an
// error naming the row's POSITION, never a row silently skipped — a short
// enumeration under-reports the directory, and the admin acting on it acts on a
// lie.
func TestAttributesEnumerateRefusesAnUnusableRowKey(t *testing.T) {
	cases := []struct {
		name   string
		cols   []string
		rows   [][]driver.Value
		want   aerr.Code
		substr string
	}{
		{
			name: "no id column at all",
			cols: []string{"department"},
			rows: [][]driver.Value{{"eng"}},
			want: aerr.APERTURE_SQL_PROVIDER_ROW_IDENTITY, substr: "BARE",
		},
		{
			name: "a NULL id",
			cols: []string{"id", "department"},
			rows: [][]driver.Value{{nil, "eng"}},
			want: aerr.APERTURE_SQL_PROVIDER_ROW_IDENTITY, substr: "NULL",
		},
		{
			name: "an empty id",
			cols: []string{"id", "department"},
			rows: [][]driver.Value{{"   ", "eng"}},
			want: aerr.APERTURE_SQL_PROVIDER_ROW_IDENTITY, substr: "empty",
		},
		{
			// A bare integer primary key. The decision path holds a principal id
			// as a STRING, so an int64 key would never equal it; the fix is the
			// ::text cast the message names.
			name: "a non-textual id",
			cols: []string{"id", "department"},
			rows: [][]driver.Value{{int64(7), "eng"}},
			want: aerr.APERTURE_SQL_PROVIDER_ROW_IDENTITY, substr: "::text",
		},
		{
			// The one key that is never legal: a row claiming to be every
			// account. Refused for the reason the registry and csvprovider refuse
			// it — the only bag that could answer it is one account's data served
			// as another's.
			name: "the account wildcard",
			cols: []string{"id", "plan"},
			rows: [][]driver.Value{{"*", "enterprise"}},
			want: aerr.APERTURE_ATTRIBUTE_PROVIDER_INVALID, substr: "wildcard",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &script{cols: tc.cols, rows: tc.rows}
			a := newAttributes(t, s, AttributeConfig{ListQuery: attrAll})
			_, err := a.List(context.Background())
			mustCode(t, err, tc.want)
			if !strings.Contains(err.Error(), tc.substr) {
				t.Errorf("error %q does not mention %q", err, tc.substr)
			}
		})
	}
}

// TestAttributesFetchOnlySlotRefusesEnumeration is the asymmetry with
// Config.ListQuery, written as a test.
//
// An ObjectProvider must be enumerable because an errored enumeration reads as
// "no access" one layer up. Attribute enumeration never participates in scope
// resolution, so omitting get_all is a supported configuration: the decision path
// is untouched and only the admin read refuses — with a code, not an empty page.
func TestAttributesFetchOnlySlotRefusesEnumeration(t *testing.T) {
	ctx := context.Background()
	s := &script{cols: []string{"department"}, rows: [][]driver.Value{{"eng"}}}
	a := newAttributes(t, s, AttributeConfig{}) // no ListQuery

	// The decision path works, unchanged.
	bag, err := a.Fetch(ctx, "alice")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if bag["department"] != "eng" {
		t.Fatalf("department = %#v, want eng", bag["department"])
	}

	for _, call := range []struct {
		name string
		run  func() error
	}{
		{"List", func() error { _, err := a.List(ctx); return err }},
		{"Query", func() error { _, err := a.Query(ctx, provider.AttributeFilter{}); return err }},
	} {
		t.Run(call.name, func(t *testing.T) {
			err := call.run()
			mustCode(t, err, aerr.APERTURE_CONFIG_INVALID)
			if !strings.Contains(err.Error(), "get_all") {
				t.Errorf("the refusal does not name the statement to declare: %v", err)
			}
		})
	}
	// Nothing was run against the database for the refused calls: only the fetch
	// above touched it.
	if calls, _, _ := s.observed(); calls != 1 {
		t.Errorf("the database saw %d statements, want only the fetch", calls)
	}
}

// TestAttributesQueryFiltersInGo: predicates are applied with provider.MatchFields
// and never templated into the developer's statement, because the Fields contract
// is the rules engine's own comparison semantics and a database's coercion rules
// do not reproduce it.
func TestAttributesQueryFiltersInGo(t *testing.T) {
	newDB := func() *script {
		return &script{
			cols: []string{"id", "department", "clearance", "teams"},
			rows: [][]driver.Value{
				{"alice", "eng", int64(3), []byte(`["platform","oncall"]`)},
				{"bob", "sales", int64(1), []byte(`["crm"]`)},
				{"carol", "eng", int64(2), []byte(`["platform"]`)},
			},
		}
	}
	cases := []struct {
		name   string
		filter provider.AttributeFilter
		want   []string
	}{
		{"no filter selects everything", provider.AttributeFilter{}, []string{"alice", "bob", "carol"}},
		{"equality on a scalar", provider.AttributeFilter{Fields: map[string]any{"department": "eng"}}, []string{"alice", "carol"}},
		{"membership on a collection", provider.AttributeFilter{Fields: map[string]any{"teams": "oncall"}}, []string{"alice"}},
		// Typed comparison: "3" is not 3, and a rule that denied over the same
		// value must not have been selected by the enumeration.
		{"a string never equals a number", provider.AttributeFilter{Fields: map[string]any{"clearance": "3"}}, []string{}},
		{"the same number typed differently does match", provider.AttributeFilter{Fields: map[string]any{"clearance": float64(3)}}, []string{"alice"}},
		{"an absent field never matches", provider.AttributeFilter{Fields: map[string]any{"missing": nil}}, []string{}},
		{"a limit stops reading early", provider.AttributeFilter{Limit: 2}, []string{"alice", "bob"}},
		{"the predicates run before the limit", provider.AttributeFilter{
			Fields: map[string]any{"department": "eng"}, Limit: 1}, []string{"alice"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newAttributes(t, newDB(), AttributeConfig{ListQuery: attrAll})
			recs, err := a.Query(context.Background(), tc.filter)
			if err != nil {
				t.Fatalf("Query: %v", err)
			}
			got := make([]string, 0, len(recs))
			for _, r := range recs {
				got = append(got, r.ID)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Query = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestAttributesHonourTheTimeout: every statement runs under context.WithTimeout.
//
// It matters more here than it does for an object. An attribute bag is read by
// every rule against every object in a decision, so an unbounded statement is not
// one object type answering slowly — it is the whole decision hanging while it
// holds a connection.
func TestAttributesHonourTheTimeout(t *testing.T) {
	t.Run("the configured budget reaches the driver", func(t *testing.T) {
		s := &script{cols: []string{"department"}, rows: [][]driver.Value{{"eng"}}}
		a := newAttributes(t, s, AttributeConfig{Timeout: 2 * time.Second})
		if _, err := a.Fetch(context.Background(), "alice"); err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		deadline, ok := s.deadline()
		if !ok {
			t.Fatal("the statement ran with no deadline")
		}
		if remaining := time.Until(deadline); remaining <= 0 || remaining > 2*time.Second {
			t.Fatalf("remaining budget = %v, want a positive value no larger than the configured 2s", remaining)
		}
	})

	t.Run("a statement that outlives the budget fails", func(t *testing.T) {
		s := &script{cols: []string{"department"}, rows: [][]driver.Value{{"eng"}}, delay: 200 * time.Millisecond}
		a := newAttributes(t, s, AttributeConfig{Timeout: 10 * time.Millisecond})
		_, err := a.Fetch(context.Background(), "alice")
		// An expired statement is an OPERATIONAL failure, never NOT_FOUND: a slow
		// database must not read as "this subject does not exist".
		mustCode(t, err, aerr.APERTURE_SQL_PROVIDER_QUERY)
	})

	t.Run("the default applies when none is configured", func(t *testing.T) {
		s := &script{cols: []string{"department"}, rows: [][]driver.Value{{"eng"}}}
		a := newAttributes(t, s, AttributeConfig{})
		if a.timeout != DefaultTimeout {
			t.Fatalf("timeout = %v, want DefaultTimeout %v", a.timeout, DefaultTimeout)
		}
		if _, err := a.Fetch(context.Background(), "alice"); err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		if _, ok := s.deadline(); !ok {
			t.Fatal("the statement ran with no deadline; there is no 'no timeout' setting")
		}
	})
}

// TestAttributesRefuseAnUnusableFetchKey: the registry guards these too, and this
// provider guards them again — it is reachable directly by a host that wired it
// in Go, and an empty key bound into the statement would come back "no rows",
// reporting a caller's bug as a subject that does not exist.
func TestAttributesRefuseAnUnusableFetchKey(t *testing.T) {
	for _, key := range []string{"", "*"} {
		t.Run("key "+key, func(t *testing.T) {
			s := &script{cols: []string{"department"}, rows: [][]driver.Value{{"eng"}}}
			a := newAttributes(t, s, AttributeConfig{})
			_, err := a.Fetch(context.Background(), key)
			mustCode(t, err, aerr.APERTURE_ATTRIBUTE_PROVIDER_INVALID)
			if calls, _, _ := s.observed(); calls != 0 {
				t.Errorf("the unusable key reached the database (%d statements)", calls)
			}
		})
	}
}

// TestNewAttributesRejectsMisconfiguration: wiring mistakes fail at construction,
// because an access-control engine should not learn about its own
// misconfiguration from a denied Check.
func TestNewAttributesRejectsMisconfiguration(t *testing.T) {
	db := newFakeDB(t, &script{})
	cases := []struct {
		name string
		q    Querier
		cfg  AttributeConfig
		want aerr.Code
	}{
		{"nil querier", nil, AttributeConfig{Slot: provider.AttributeSlotUser, FetchQuery: attrFetch}, aerr.APERTURE_CONFIG_INVALID},
		{"no slot", db, AttributeConfig{FetchQuery: attrFetch}, aerr.APERTURE_ATTRIBUTE_SLOT_UNKNOWN},
		{"a slot outside the closed set", db, AttributeConfig{Slot: "tenant", FetchQuery: attrFetch}, aerr.APERTURE_ATTRIBUTE_SLOT_UNKNOWN},
		{"blank fetch query", db, AttributeConfig{Slot: provider.AttributeSlotUser}, aerr.APERTURE_CONFIG_INVALID},
		{"whitespace fetch query", db, AttributeConfig{Slot: provider.AttributeSlotUser, FetchQuery: "  "}, aerr.APERTURE_CONFIG_INVALID},
		{"negative timeout", db, AttributeConfig{Slot: provider.AttributeSlotUser, FetchQuery: attrFetch, Timeout: -time.Second}, aerr.APERTURE_CONFIG_INVALID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewAttributes(tc.q, tc.cfg)
			mustCode(t, err, tc.want)
		})
	}

	// The one thing that is NOT a misconfiguration, stated here so the asymmetry
	// with Config.ListQuery cannot be "tidied up" into a required field.
	a, err := NewAttributes(db, AttributeConfig{Slot: provider.AttributeSlotUser, FetchQuery: attrFetch})
	if err != nil {
		t.Fatalf("a fetch-only slot was refused at construction: %v", err)
	}
	if a.Slot() != provider.AttributeSlotUser {
		t.Errorf("Slot() = %q, want user", a.Slot())
	}
	if a.idColumn != DefaultIDColumn {
		t.Errorf("id column = %q, want the default %q", a.idColumn, DefaultIDColumn)
	}
}

// TestAttributesResultShapeIsValidatedOncePerStatement: a column name is the
// field key, so an unnamed or duplicated column is an error naming it rather than
// a field silently dropped or overwritten by whichever came last.
func TestAttributesResultShapeIsValidatedOncePerStatement(t *testing.T) {
	cases := []struct {
		name string
		cols []string
		rows [][]driver.Value
		run  func(*Attributes) error
	}{
		{
			name: "an unnamed fetch column",
			cols: []string{""},
			rows: [][]driver.Value{{int64(1)}},
			run:  func(a *Attributes) error { _, err := a.Fetch(context.Background(), "alice"); return err },
		},
		{
			name: "a duplicated fetch column",
			cols: []string{"name", "name"},
			rows: [][]driver.Value{{"a", "b"}},
			run:  func(a *Attributes) error { _, err := a.Fetch(context.Background(), "alice"); return err },
		},
		{
			name: "a duplicated get_all column",
			cols: []string{"id", "department", "department"},
			rows: [][]driver.Value{{"alice", "eng", "sales"}},
			run:  func(a *Attributes) error { _, err := a.List(context.Background()); return err },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &script{cols: tc.cols, rows: tc.rows}
			a := newAttributes(t, s, AttributeConfig{ListQuery: attrAll})
			mustCode(t, tc.run(a), aerr.APERTURE_SQL_PROVIDER_SCAN)
		})
	}
}

// TestAttributesEnumerateReportsAMidStreamFailure: rows.Err is checked after the
// loop, unconditionally. A connection that dies half way through a result set
// ends the loop exactly as a complete one does, so skipping the check would
// report a truncated directory as a whole one.
func TestAttributesEnumerateReportsAMidStreamFailure(t *testing.T) {
	s := &script{
		cols:    []string{"id", "department"},
		rows:    [][]driver.Value{{"alice", "eng"}},
		rowsErr: stderrors.New("connection reset"),
	}
	a := newAttributes(t, s, AttributeConfig{ListQuery: attrAll})
	_, err := a.List(context.Background())
	mustCode(t, err, aerr.APERTURE_SQL_PROVIDER_QUERY)
}

// TestAttributesBagsAreFreshPerCall: the registry caches bags by REFERENCE and
// never copies on read, so a provider that reused a container would let one
// decision's holder see another's data change under it.
func TestAttributesBagsAreFreshPerCall(t *testing.T) {
	s := &script{cols: []string{"teams"}, rows: [][]driver.Value{{[]byte(`["platform"]`)}}}
	a := newAttributes(t, s, AttributeConfig{})
	first, err := a.Fetch(context.Background(), "alice")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	// The same canned row, read twice: the two bags must still be independently
	// allocated, containers and all.
	second, err := a.Fetch(context.Background(), "alice")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if &first == &second {
		t.Fatal("two fetches returned the same map header")
	}
	firstList := first["teams"].([]any)
	secondList := second["teams"].([]any)
	if len(firstList) == 0 || len(secondList) == 0 {
		t.Fatalf("teams did not decode: %#v / %#v", first, second)
	}
	if &firstList[0] == &secondList[0] {
		t.Fatal("two fetches share a nested container; Metadata must be fresh at every depth")
	}
}
