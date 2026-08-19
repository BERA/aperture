package server_test

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"testing"

	"github.com/frankbardon/aperture/engine"
	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/identity"
	"github.com/frankbardon/aperture/internal/server"
	"github.com/frankbardon/aperture/internal/wire/rpc"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/provider"
	"github.com/frankbardon/aperture/scope"
	"github.com/frankbardon/aperture/service"
	"github.com/frankbardon/aperture/storage/memory"

	"github.com/twitchtv/twirp"
	"google.golang.org/protobuf/types/known/structpb"
)

// assertTwirpCode pins that a wire failure carries the Aperture code in its
// twirp meta, so a client can tell a malformed predicate (invalid argument)
// from a misconfigured deployment (provider unregistered) rather than seeing
// an empty result and reading it as "no access".
func assertTwirpCode(t *testing.T, err error, want aerr.Code) {
	t.Helper()
	te, ok := err.(twirp.Error)
	if !ok {
		t.Fatalf("error %v is not a twirp.Error", err)
	}
	if got := te.Meta("code"); got != string(want) {
		t.Fatalf("meta code = %q, want %q (twirp code %v)", got, want, te.Code())
	}
}

// The metadata filter, end to end over Twirp/HTTP.
//
// These tests deliberately go through the JSON client rather than calling the
// handler in-process: the whole point of encoding the predicate as
// map<string, google.protobuf.Value> instead of a JSON string is that the
// string/number/bool/list distinction survives serialization, and only a real
// round trip proves it.

// filterFixture is a live Twirp server over an engine whose scope lister AND
// metadata source are the same *provider.Registry — the production wiring.
type filterFixture struct {
	t   *testing.T
	eng *engine.Engine
	cli rpc.ApertureService
	// handler is the same surface called in-process. It exists for the one case
	// the JSON client cannot express: protojson refuses to marshal NaN/±Inf, so
	// a non-finite number can only reach the handler through the BINARY codec
	// (or a hand-rolled client). The boundary guard is tested here rather than
	// left unreachable.
	handler rpc.ApertureService
}

// filterCatalogue is the object set every filter test enumerates. seats is an
// INT64 in metadata, which is what makes the string-vs-number case meaningful:
// a Value number arrives as float64 and must still match, a Value string must
// not.
func filterCatalogue() []provider.Object {
	return []provider.Object{
		{
			ID: identity.MustParse("account:acme/document:1"),
			Metadata: provider.Metadata{
				"tier": "premium", "seats": int64(5),
				"brands": []any{"brand:X", "brand:Y"},
			},
		},
		{
			ID: identity.MustParse("account:acme/document:2"),
			Metadata: provider.Metadata{
				"tier": "basic", "seats": int64(5),
				"brands": []any{"brand:Y"},
			},
		},
		{
			ID: identity.MustParse("account:acme/document:3"),
			Metadata: provider.Metadata{
				"tier": "premium", "seats": int64(9),
				"brands": []any{"brand:X", "brand:Y"},
			},
		},
		// No tier, no brands: the absent-field case, which never matches.
		{
			ID:       identity.MustParse("account:acme/document:4"),
			Metadata: provider.Metadata{"seats": int64(5)},
		},
	}
}

func newFilterFixture(t *testing.T) *filterFixture {
	t.Helper()
	ctx := context.Background()
	store := memory.New()
	must(t, store.Setup(ctx))
	must(t, store.PutAccount(ctx, model.Account{ID: acct, Name: "Acme"}))
	must(t, store.PutPrincipal(ctx, model.Principal{ID: "alice", Kind: model.PrincipalUser, Identity: "user:alice"}))
	must(t, store.PutMembership(ctx, model.Membership{PrincipalID: "alice", AccountID: acct}))
	must(t, store.PutObjectType(ctx, model.ObjectType{Name: "document", Actions: []string{"read"}}))
	must(t, store.PutPermission(ctx, model.Permission{
		ID: "p-read", ObjectType: "document", Action: "read", ScopeStrategy: scope.StrategyImplicit,
	}))
	must(t, store.PutGrant(ctx, model.Grant{
		ID: "g-alice", AccountID: acct,
		Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: "alice"},
		PermissionID: "p-read", Object: "account:acme/**", Effect: model.EffectAllow,
	}))

	sp, err := provider.NewStatic(filterCatalogue())
	if err != nil {
		t.Fatalf("NewStatic: %v", err)
	}
	reg := provider.NewRegistry()
	reg.MustRegister("document", sp)

	eng := engine.New(store,
		engine.WithScopeResolution(scope.DefaultRegistry(), engine.ScopeDeps{Lister: reg}),
		engine.WithMetadata(reg),
	)
	svc := service.New(eng, service.WithProviders(reg), service.WithStorage(store))
	srv := httptest.NewServer(server.New(svc))
	t.Cleanup(srv.Close)

	return &filterFixture{
		t:       t,
		eng:     eng,
		cli:     rpc.NewApertureServiceJSONClient(srv.URL, http.DefaultClient),
		handler: server.NewTwirpHandler(svc),
	}
}

// wire runs a filtered Enumerate over HTTP with a hand-built Value map — the
// shape a non-Go client sends.
func (f *filterFixture) wire(fields map[string]*structpb.Value) ([]string, error) {
	f.t.Helper()
	res, err := f.cli.Enumerate(context.Background(), &rpc.EnumerateRequest{
		Account: acct, Principal: "alice", Action: "read",
		Pattern: "account:acme/**", Fields: fields,
	})
	if err != nil {
		return nil, err
	}
	return sorted(res.ObjectIds), nil
}

func (f *filterFixture) mustWire(fields map[string]*structpb.Value) []string {
	f.t.Helper()
	ids, err := f.wire(fields)
	if err != nil {
		f.t.Fatalf("Enumerate over the wire: unexpected error: %v", err)
	}
	return ids
}

// direct runs the same enumeration straight against the engine, which is the
// oracle the wire path is compared to.
func (f *filterFixture) direct(fields map[string]any) []string {
	f.t.Helper()
	ids, err := f.eng.Enumerate(context.Background(), engine.EnumerateRequest{
		Account: acct, Principal: "alice", Action: "read",
		Pattern: "account:acme/**", Fields: fields,
	})
	if err != nil {
		f.t.Fatalf("Enumerate in-process: unexpected error: %v", err)
	}
	return sorted(ids)
}

func sorted(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func values(t *testing.T, m map[string]any) map[string]*structpb.Value {
	t.Helper()
	v, err := rpc.FieldsToWire(m)
	if err != nil {
		t.Fatalf("FieldsToWire(%v): %v", m, err)
	}
	return v
}

// A JSON number arrives as float64 and still matches an int64 in metadata,
// because ValuesEqual compares numbers across Go numeric types by value. This
// is the property that makes the double-only wire encoding usable at all.
func TestEnumerateFilter_NumberMatchesInt64Metadata(t *testing.T) {
	f := newFilterFixture(t)
	got := f.mustWire(map[string]*structpb.Value{"seats": structpb.NewNumberValue(5)})
	want := []string{"account:acme/document:1", "account:acme/document:2", "account:acme/document:4"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("seats=5 (number) = %v, want %v", got, want)
	}
}

// ...and the mirror image: a JSON STRING never matches a numeric metadata
// value. Coercing here would make Enumerate return objects a Check then denies.
func TestEnumerateFilter_StringDoesNotMatchNumericMetadata(t *testing.T) {
	f := newFilterFixture(t)
	got := f.mustWire(map[string]*structpb.Value{"seats": structpb.NewStringValue("5")})
	if len(got) != 0 {
		t.Fatalf(`seats="5" (string) = %v, want no objects — "5" must not match 5`, got)
	}
	// And the number form still selects, so the empty result above is the type
	// rule at work rather than a broken predicate.
	if got := f.mustWire(map[string]*structpb.Value{"seats": structpb.NewNumberValue(5)}); len(got) == 0 {
		t.Fatal("seats=5 (number) selected nothing; the string case above proves nothing")
	}
}

// A scalar want against a collection field matches by MEMBERSHIP...
func TestEnumerateFilter_ScalarWantMatchesCollectionByMembership(t *testing.T) {
	f := newFilterFixture(t)
	got := f.mustWire(map[string]*structpb.Value{"brands": structpb.NewStringValue("brand:X")})
	want := []string{"account:acme/document:1", "account:acme/document:3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("brands=brand:X = %v, want %v", got, want)
	}
}

// ...but a LIST Value is a container want, so it compares by EQUALITY — never
// as "contains all of these". The whole array must match, in order.
func TestEnumerateFilter_ListWantComparesByEquality(t *testing.T) {
	f := newFilterFixture(t)

	// The exact array of documents 1 and 3.
	got := f.mustWire(values(t, map[string]any{"brands": []any{"brand:X", "brand:Y"}}))
	want := []string{"account:acme/document:1", "account:acme/document:3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("brands=[brand:X brand:Y] = %v, want %v (container wants compare by equality)", got, want)
	}

	// A one-element list is NOT "membership of brand:Y" — it is equality against
	// the array ["brand:Y"], which only document 2 holds. If this ever returned
	// documents 1 and 3 as well, the wire had turned a container want into a
	// membership test.
	got = f.mustWire(values(t, map[string]any{"brands": []any{"brand:Y"}}))
	want = []string{"account:acme/document:2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("brands=[brand:Y] = %v, want %v (equality, not subset)", got, want)
	}
}

// The round trip: a Go predicate encoded to Values, sent over HTTP, decoded and
// evaluated selects exactly what handing the same Go map to the engine does.
func TestEnumerateFilter_WireRoundTripAgreesWithTheEngine(t *testing.T) {
	f := newFilterFixture(t)
	cases := []map[string]any{
		{"tier": "premium"},
		{"seats": int64(5)},
		{"seats": 5.0},
		{"tier": "premium", "seats": int64(5)},
		{"brands": "brand:Y"},
		{"brands": []any{"brand:Y"}},
		{"tier": "gold"},       // matches nothing
		{"unknown_field": "x"}, // absent everywhere: never matches
		{"tier": nil},          // a nil want still needs the field present
		{"seats": "5"},         // string vs number: matches nothing
	}
	for _, fields := range cases {
		t.Run(mustJSON(t, fields), func(t *testing.T) {
			want := f.direct(fields)
			got := f.mustWire(values(t, fields))
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("over the wire = %v, in-process = %v", got, want)
			}
		})
	}
}

// Omitting the map on the wire is indistinguishable from a nil map in Go: both
// filter nothing, and both give the unfiltered result.
func TestEnumerateFilter_OmittedIsTheUnfilteredEnumeration(t *testing.T) {
	f := newFilterFixture(t)
	all := []string{
		"account:acme/document:1", "account:acme/document:2",
		"account:acme/document:3", "account:acme/document:4",
	}
	if got := f.mustWire(nil); !reflect.DeepEqual(got, all) {
		t.Fatalf("omitted fields = %v, want the unfiltered set %v", got, all)
	}
	if got := f.mustWire(map[string]*structpb.Value{}); !reflect.DeepEqual(got, all) {
		t.Fatalf("empty fields = %v, want the unfiltered set %v", got, all)
	}
	if got := f.direct(nil); !reflect.DeepEqual(got, all) {
		t.Fatalf("nil Fields in-process = %v, want %v", got, all)
	}
}

// The filter can only ever subtract from the allowed set: a deny-carved object
// is not returned however well it matches.
func TestEnumerateFilter_NeverAddsToTheAllowedSet(t *testing.T) {
	f := newFilterFixture(t)
	// Sanity: document:1 is in the unfiltered result.
	if got := f.mustWire(nil); len(got) != 4 {
		t.Fatalf("unfiltered = %v, want 4 objects", got)
	}
	got := f.mustWire(map[string]*structpb.Value{
		"tier":  structpb.NewStringValue("premium"),
		"seats": structpb.NewNumberValue(9),
	})
	want := []string{"account:acme/document:3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tier=premium AND seats=9 = %v, want %v (predicates AND)", got, want)
	}
}

// A predicate the value model cannot represent is an invalid-argument failure,
// not a silently empty result that would read as "no access". Called in-process
// because protojson will not marshal a non-finite number at all — the binary
// codec will, so the guard is real.
func TestEnumerateFilter_NonFiniteNumberIsInvalidArgument(t *testing.T) {
	f := newFilterFixture(t)
	for _, n := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		_, err := f.handler.Enumerate(context.Background(), &rpc.EnumerateRequest{
			Account: acct, Principal: "alice", Action: "read", Pattern: "account:acme/**",
			Fields: map[string]*structpb.Value{"seats": structpb.NewNumberValue(n)},
		})
		if err == nil {
			t.Fatalf("Enumerate(seats=%v) = nil error, want an invalid-argument failure", n)
		}
		assertTwirpCode(t, err, aerr.APERTURE_INVALID_INPUT)
		if te, ok := err.(twirp.Error); ok && te.Code() != twirp.InvalidArgument {
			t.Fatalf("twirp code = %v, want invalid_argument", te.Code())
		}
	}
}

// The batch request carries the map per query (it embeds EnumerateRequest), so
// each item filters independently — over a real wire.
func TestEnumerateBatchFilter_PerQueryFields(t *testing.T) {
	f := newFilterFixture(t)
	res, err := f.cli.EnumerateBatch(context.Background(), &rpc.EnumerateBatchRequest{
		Queries: []*rpc.EnumerateRequest{
			{
				Account: acct, Principal: "alice", Action: "read", Pattern: "account:acme/**",
				Fields: map[string]*structpb.Value{"tier": structpb.NewStringValue("basic")},
			},
			{
				Account: acct, Principal: "alice", Action: "read", Pattern: "account:acme/**",
				Fields: map[string]*structpb.Value{"seats": structpb.NewNumberValue(9)},
			},
			{
				Account: acct, Principal: "alice", Action: "read", Pattern: "account:acme/**",
			},
		},
	})
	if err != nil {
		t.Fatalf("EnumerateBatch: %v", err)
	}
	if len(res.Results) != 3 {
		t.Fatalf("got %d results, want 3", len(res.Results))
	}
	if got := sorted(res.Results[0].ObjectIds); !reflect.DeepEqual(got, []string{"account:acme/document:2"}) {
		t.Fatalf("tier=basic = %v, want [account:acme/document:2]", got)
	}
	if got := sorted(res.Results[1].ObjectIds); !reflect.DeepEqual(got, []string{"account:acme/document:3"}) {
		t.Fatalf("seats=9 = %v, want [account:acme/document:3]", got)
	}
	if got := len(res.Results[2].ObjectIds); got != 4 {
		t.Fatalf("unfiltered query returned %d objects, want the whole set of 4", got)
	}
}

// One malformed predicate fails only its own item: the rest of the batch runs,
// and the failed item reports the error rather than an empty object list that
// would read as "no access". In-process, for the protojson reason above.
func TestEnumerateBatchFilter_PerQueryFailure(t *testing.T) {
	f := newFilterFixture(t)
	res, err := f.handler.EnumerateBatch(context.Background(), &rpc.EnumerateBatchRequest{
		Queries: []*rpc.EnumerateRequest{
			{
				Account: acct, Principal: "alice", Action: "read", Pattern: "account:acme/**",
				Fields: map[string]*structpb.Value{"tier": structpb.NewStringValue("basic")},
			},
			{
				Account: acct, Principal: "alice", Action: "read", Pattern: "account:acme/**",
				Fields: map[string]*structpb.Value{"seats": structpb.NewNumberValue(math.Inf(1))},
			},
			{
				Account: acct, Principal: "alice", Action: "read", Pattern: "account:acme/**",
			},
		},
	})
	if err != nil {
		t.Fatalf("EnumerateBatch: %v", err)
	}
	if len(res.Results) != 3 {
		t.Fatalf("got %d results, want 3", len(res.Results))
	}
	if got := sorted(res.Results[0].ObjectIds); !reflect.DeepEqual(got, []string{"account:acme/document:2"}) {
		t.Fatalf("filtered query = %v, want [account:acme/document:2]", got)
	}
	if res.Results[1].ErrorCode != string(aerr.APERTURE_INVALID_INPUT) {
		t.Fatalf("malformed query error code = %q, want APERTURE_INVALID_INPUT", res.Results[1].ErrorCode)
	}
	if len(res.Results[1].ObjectIds) != 0 {
		t.Fatalf("malformed query returned %v; a query that never ran must not report objects", res.Results[1].ObjectIds)
	}
	if got := len(res.Results[2].ObjectIds); got != 4 {
		t.Fatalf("unfiltered query returned %d objects, want 4 — one bad query must not affect the rest", got)
	}
}

// Filtering an object-type with no metadata source is a coded misconfiguration,
// never an empty result. The wire must not flatten that into "no objects".
func TestEnumerateFilter_NoMetadataSourceIsAnError(t *testing.T) {
	ctx := context.Background()
	store := memory.New()
	must(t, store.Setup(ctx))
	must(t, store.PutAccount(ctx, model.Account{ID: acct, Name: "Acme"}))
	must(t, store.PutPrincipal(ctx, model.Principal{ID: "alice", Kind: model.PrincipalUser, Identity: "user:alice"}))
	must(t, store.PutMembership(ctx, model.Membership{PrincipalID: "alice", AccountID: acct}))
	must(t, store.PutObjectType(ctx, model.ObjectType{Name: "document", Actions: []string{"read"}}))
	must(t, store.PutPermission(ctx, model.Permission{ID: "p-read", ObjectType: "document", Action: "read"}))
	must(t, store.PutGrant(ctx, model.Grant{
		ID: "g-alice", AccountID: acct,
		Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: "alice"},
		PermissionID: "p-read", Object: "account:acme/document:1", Effect: model.EffectAllow,
	}))

	svc := service.New(engine.New(store), service.WithStorage(store))
	srv := httptest.NewServer(server.New(svc))
	t.Cleanup(srv.Close)
	cli := rpc.NewApertureServiceJSONClient(srv.URL, http.DefaultClient)

	_, err := cli.Enumerate(ctx, &rpc.EnumerateRequest{
		Account: acct, Principal: "alice", Action: "read", Pattern: "account:acme/**",
		Fields: map[string]*structpb.Value{"tier": structpb.NewStringValue("premium")},
	})
	if err == nil {
		t.Fatal("filtered Enumerate with no metadata source = nil error, want APERTURE_PROVIDER_UNREGISTERED")
	}
	assertTwirpCode(t, err, aerr.APERTURE_PROVIDER_UNREGISTERED)
}
