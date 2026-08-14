package provider

import (
	"context"
	"reflect"
	"testing"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/identity"
)

// staticFixture is the object set most of these tests query. Declaration order is
// deliberately NOT sorted, so an implementation that sorted would be caught.
func staticFixture(t *testing.T) *Static {
	t.Helper()
	p, err := NewStatic([]Object{
		{ID: identity.MustParse("account:acme/brand:2"), Metadata: Metadata{
			"tier":  "silver",
			"seats": int64(5),
			"tags":  []any{"launch"},
		}},
		{ID: identity.MustParse("account:acme/brand:1"), Metadata: Metadata{
			"tier":  "gold",
			"seats": int64(12),
			"tags":  []any{"premium", "launch"},
			"owner": map[string]any{"dept": "eng", "lead": "alice"},
		}},
		{ID: identity.MustParse("account:other/brand:9"), Metadata: Metadata{
			"tier": "gold",
			"tags": []any{"premium"},
		}},
	})
	if err != nil {
		t.Fatalf("NewStatic: %v", err)
	}
	return p
}

func TestStaticFetch(t *testing.T) {
	p := staticFixture(t)
	md, err := p.Fetch(context.Background(), identity.MustParse("account:acme/brand:1"))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if md["tier"] != "gold" {
		t.Errorf("tier = %v, want gold", md["tier"])
	}
	if md["seats"] != int64(12) {
		t.Errorf("seats = %#v, want int64(12)", md["seats"])
	}
	owner, ok := md["owner"].(map[string]any)
	if !ok || owner["dept"] != "eng" {
		t.Errorf("owner = %#v, want {dept: eng, ...}", md["owner"])
	}
}

func TestStaticFetchNotFound(t *testing.T) {
	p := staticFixture(t)
	_, err := p.Fetch(context.Background(), identity.MustParse("account:acme/brand:404"))
	if aerr.CodeOf(err) != aerr.APERTURE_NOT_FOUND {
		t.Fatalf("code = %s, want APERTURE_NOT_FOUND", aerr.CodeOf(err))
	}
}

func TestStaticListIsDeclarationOrder(t *testing.T) {
	p := staticFixture(t)
	objs, err := p.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []string{"account:acme/brand:2", "account:acme/brand:1", "account:other/brand:9"}
	got := make([]string, len(objs))
	for i, o := range objs {
		got[i] = o.ID.String()
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("List order = %v, want %v", got, want)
	}
	if p.Len() != len(want) {
		t.Errorf("Len = %d, want %d", p.Len(), len(want))
	}
}

func TestStaticQuery(t *testing.T) {
	p := staticFixture(t)
	pattern := identity.MustParsePattern("account:acme/brand:*")
	cases := map[string]struct {
		filter Filter
		want   []string
	}{
		"zero filter is List": {
			Filter{},
			[]string{"account:acme/brand:2", "account:acme/brand:1", "account:other/brand:9"},
		},
		"pattern bounds the result": {
			Filter{Pattern: &pattern},
			[]string{"account:acme/brand:2", "account:acme/brand:1"},
		},
		"limit truncates in declaration order": {
			Filter{Limit: 2},
			[]string{"account:acme/brand:2", "account:acme/brand:1"},
		},
		"collection field matches by membership": {
			Filter{Fields: map[string]any{"tags": "premium"}},
			[]string{"account:acme/brand:1", "account:other/brand:9"},
		},
		"scalar field matches by equality": {
			Filter{Fields: map[string]any{"tier": "gold"}},
			[]string{"account:acme/brand:1", "account:other/brand:9"},
		},
		"membership is typed, not stringy": {
			Filter{Fields: map[string]any{"seats": "12"}},
			[]string{},
		},
		"numeric equality crosses Go numeric types": {
			Filter{Fields: map[string]any{"seats": 12}},
			[]string{"account:acme/brand:1"},
		},
		// brand:2 and brand:9 have no owner at all: an absent field never matches,
		// so the object field's deep equality selects brand:1 alone.
		"an object field matches by deep equality": {
			Filter{Fields: map[string]any{"owner": map[string]any{"dept": "eng", "lead": "alice"}}},
			[]string{"account:acme/brand:1"},
		},
		"an object field is not key membership": {
			Filter{Fields: map[string]any{"owner": "dept"}},
			[]string{},
		},
		"every predicate must hold": {
			Filter{Fields: map[string]any{"tier": "gold", "tags": "launch"}},
			[]string{"account:acme/brand:1"},
		},
		"pattern and fields and limit compose": {
			Filter{Pattern: &pattern, Fields: map[string]any{"tags": "launch"}, Limit: 1},
			[]string{"account:acme/brand:2"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			objs, err := p.Query(context.Background(), tc.filter)
			if err != nil {
				t.Fatalf("Query: %v", err)
			}
			got := make([]string, 0, len(objs))
			for _, o := range objs {
				got = append(got, o.ID.String())
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Query = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNewStaticRejectsBadInput(t *testing.T) {
	cases := map[string]struct {
		objects []Object
		want    aerr.Code
	}{
		"empty identity": {
			[]Object{{Metadata: Metadata{"tier": "gold"}}},
			aerr.APERTURE_PROVIDER_INVALID,
		},
		"duplicate identity": {
			[]Object{
				{ID: identity.MustParse("brand:1")},
				{ID: identity.MustParse("brand:1")},
			},
			aerr.APERTURE_PROVIDER_INVALID,
		},
		"array of objects": {
			[]Object{{ID: identity.MustParse("brand:1"), Metadata: Metadata{
				"members": []any{map[string]any{"id": int64(1)}},
			}}},
			aerr.APERTURE_METADATA_INVALID,
		},
		"typed container": {
			[]Object{{ID: identity.MustParse("brand:1"), Metadata: Metadata{
				"tags": []string{"premium"},
			}}},
			aerr.APERTURE_METADATA_INVALID,
		},
		"past the depth cap": {
			[]Object{{ID: identity.MustParse("brand:1"), Metadata: Metadata{
				"owner": map[string]any{"lead": map[string]any{"name": map[string]any{"first": "a"}}},
			}}},
			aerr.APERTURE_METADATA_INVALID,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := NewStatic(tc.objects); aerr.CodeOf(err) != tc.want {
				t.Fatalf("code = %s, want %s (err=%v)", aerr.CodeOf(err), tc.want, err)
			}
		})
	}
}

func TestNewStaticIsEmptyWithoutObjects(t *testing.T) {
	p, err := NewStatic(nil)
	if err != nil {
		t.Fatalf("NewStatic: %v", err)
	}
	objs, err := p.List(context.Background())
	if err != nil || len(objs) != 0 {
		t.Fatalf("List = %v, %v; want empty", objs, err)
	}
	md, err := p.Fetch(context.Background(), identity.MustParse("brand:1"))
	if md != nil || aerr.CodeOf(err) != aerr.APERTURE_NOT_FOUND {
		t.Fatalf("Fetch = %v, %v; want APERTURE_NOT_FOUND", md, err)
	}
}

// An object declared with no metadata serves an empty map, not a nil one, so a
// consumer reads an absent field rather than a nil map.
func TestStaticMetadataIsNeverNil(t *testing.T) {
	p, err := NewStatic([]Object{{ID: identity.MustParse("brand:1")}})
	if err != nil {
		t.Fatalf("NewStatic: %v", err)
	}
	md, err := p.Fetch(context.Background(), identity.MustParse("brand:1"))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if md == nil {
		t.Fatal("Fetch returned a nil Metadata")
	}
	if len(md) != 0 {
		t.Errorf("Metadata = %v, want empty", md)
	}
}

// The construction copy is what makes the read-only contract hold against a
// caller that keeps its input: mutating the input afterwards — at any depth —
// must not reach metadata the Registry may already have cached.
func TestNewStaticCopiesItsInput(t *testing.T) {
	tags := []any{"premium"}
	owner := map[string]any{"dept": "eng"}
	in := []Object{{ID: identity.MustParse("brand:1"), Metadata: Metadata{
		"tags": tags, "owner": owner, "tier": "gold",
	}}}
	p, err := NewStatic(in)
	if err != nil {
		t.Fatalf("NewStatic: %v", err)
	}
	// Mutate every level of the input the caller still holds.
	in[0].Metadata["tier"] = "bronze"
	tags[0] = "downgraded"
	owner["dept"] = "ops"

	md, err := p.Fetch(context.Background(), identity.MustParse("brand:1"))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if md["tier"] != "gold" {
		t.Errorf("tier = %v, want gold (top-level input mutation leaked)", md["tier"])
	}
	if got := md["tags"].([]any)[0]; got != "premium" {
		t.Errorf("tags[0] = %v, want premium (nested slice mutation leaked)", got)
	}
	if got := md["owner"].(map[string]any)["dept"]; got != "eng" {
		t.Errorf("owner.dept = %v, want eng (nested map mutation leaked)", got)
	}
}

// The mirror of csvprovider's TestListSlicesAreNeverShared /
// TestJSONObjectsAreNeverShared: two objects declared from the same source value
// must not end up sharing one container, or a mutation of one is a mutation of
// the other.
func TestStaticContainersAreNeverShared(t *testing.T) {
	shared := []any{"premium"}
	sharedOwner := map[string]any{"dept": "eng"}
	p, err := NewStatic([]Object{
		{ID: identity.MustParse("brand:1"), Metadata: Metadata{"tags": shared, "owner": sharedOwner}},
		{ID: identity.MustParse("brand:2"), Metadata: Metadata{"tags": shared, "owner": sharedOwner}},
	})
	if err != nil {
		t.Fatalf("NewStatic: %v", err)
	}
	objs, err := p.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	a, b := objs[0].Metadata, objs[1].Metadata
	if &a == &b {
		t.Fatal("two objects share one Metadata map")
	}
	as, bs := a["tags"].([]any), b["tags"].([]any)
	as[0] = "mutated"
	if bs[0] != "premium" {
		t.Error("two objects share one tags slice")
	}
	am, bm := a["owner"].(map[string]any), b["owner"].(map[string]any)
	am["dept"] = "mutated"
	if bm["dept"] != "eng" {
		t.Error("two objects share one owner map")
	}
}

// Reads hand out the provider's own map BY REFERENCE — nothing is copied on the
// Fetch path, which is the allocation-aware half of the read-only contract.
func TestStaticReadsShareOneMap(t *testing.T) {
	p := staticFixture(t)
	id := identity.MustParse("account:acme/brand:1")
	first, err := p.Fetch(context.Background(), id)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	second, err := p.Fetch(context.Background(), id)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if reflect.ValueOf(first).Pointer() != reflect.ValueOf(second).Pointer() {
		t.Error("Fetch copied the metadata map; it must hand out the same map by reference")
	}
	objs, err := p.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if reflect.ValueOf(objs[1].Metadata).Pointer() != reflect.ValueOf(first).Pointer() {
		t.Error("List returned a different map than Fetch for the same object")
	}
}
