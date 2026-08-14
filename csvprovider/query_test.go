package csvprovider

import (
	"context"
	"strings"
	"testing"

	"github.com/frankbardon/aperture/identity"
	"github.com/frankbardon/aperture/provider"
)

// queryFixture exercises every column shape a Fields predicate can land on: a
// scalar, a string list, a typed list, an empty list cell, and a json object.
const queryFixture = "id,tier,tags:list,ranks:list<int>,owner:json\n" +
	`brand:1,gold,premium|launch,3|5,"{""dept"":""eng"",""seats"":5}"` + "\n" +
	`brand:2,silver,premium-trial,1,"{""dept"":""ops""}"` + "\n" +
	"brand:3,gold,,,\n"

// TestQuery_FieldsMembership pins the Filter.Fields contract as csvprovider
// serves it: membership over a list column, typed element comparison, equality
// everywhere else, and an absent field that never matches.
func TestQuery_FieldsMembership(t *testing.T) {
	p := New(write(t, queryFixture))

	cases := []struct {
		name   string
		fields map[string]any
		want   []string
	}{
		{
			name:   "array membership selects the containing row",
			fields: map[string]any{"tags": "premium"},
			want:   []string{"brand:1"},
		},
		{
			name:   "membership is not substring matching",
			fields: map[string]any{"tags": "premium-trial"},
			want:   []string{"brand:2"},
		},
		{
			name:   "an empty list cell is a member of nothing",
			fields: map[string]any{"tags": "launch"},
			want:   []string{"brand:1"},
		},
		{
			name:   "the array's own rendering never matches",
			fields: map[string]any{"tags": "[premium launch]"},
			want:   nil,
		},
		{
			name:   "typed elements make numeric membership work",
			fields: map[string]any{"ranks": 5},
			want:   []string{"brand:1"},
		},
		{
			name:   "an int element does not match its string spelling",
			fields: map[string]any{"ranks": "5"},
			want:   nil,
		},
		{
			name:   "scalar equality is unchanged",
			fields: map[string]any{"tier": "gold"},
			want:   []string{"brand:1", "brand:3"},
		},
		{
			name:   "predicates are ANDed across columns",
			fields: map[string]any{"tier": "gold", "tags": "premium"},
			want:   []string{"brand:1"},
		},
		{
			name:   "a field absent from a row never matches",
			fields: map[string]any{"owner": map[string]any{"dept": "eng", "seats": int64(5)}},
			want:   []string{"brand:1"}, // brand:3's empty json cell omits the field
		},
		{
			name:   "an unknown column never matches",
			fields: map[string]any{"nope": "gold"},
			want:   nil,
		},
		{
			name:   "an object field is not key membership",
			fields: map[string]any{"owner": "dept"},
			want:   nil,
		},
		{
			name:   "an object field does not match one of its values",
			fields: map[string]any{"owner": "eng"},
			want:   nil,
		},
		{
			name:   "an object field does not match its rendering",
			fields: map[string]any{"owner": "map[dept:ops]"},
			want:   nil,
		},
		{
			name:   "nil fields select everything",
			fields: nil,
			want:   []string{"brand:1", "brand:2", "brand:3"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			objs, err := p.Query(context.Background(), provider.Filter{Fields: tc.fields})
			if err != nil {
				t.Fatalf("Query: %v", err)
			}
			got := make([]string, 0, len(objs))
			for _, o := range objs {
				got = append(got, o.ID.String())
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("Query(%#v) = %v, want %v", tc.fields, got, tc.want)
			}
		})
	}
}

// TestQuery_FieldsWithPatternAndLimit checks that membership composes with the
// other two filter dimensions rather than replacing them.
func TestQuery_FieldsWithPatternAndLimit(t *testing.T) {
	p := New(write(t, queryFixture))
	pat := identity.MustParsePattern("brand:3")

	objs, err := p.Query(context.Background(), provider.Filter{
		Fields:  map[string]any{"tier": "gold"},
		Pattern: &pat,
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(objs) != 1 || objs[0].ID.String() != "brand:3" {
		t.Fatalf("fields+pattern = %v, want [brand:3]", objs)
	}

	objs, err = p.Query(context.Background(), provider.Filter{
		Fields: map[string]any{"tier": "gold"},
		Limit:  1,
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(objs) != 1 || objs[0].ID.String() != "brand:1" {
		t.Fatalf("fields+limit = %v, want [brand:1]", objs)
	}
}
