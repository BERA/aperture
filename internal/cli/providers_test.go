package cli

import (
	"encoding/json"
	"testing"

	"github.com/frankbardon/aperture/seed"
)

// TestHasObjectSources pins the wiring guard that decides whether `serve` hands
// the provider Registry to the rules engine's metadata fetcher and the scope
// resolver's object lister.
//
// The regression it guards is specific and was silent: the guard used to read
// `len(doc.Providers) > 0`, so a seed declaring ONLY an `objects:` section built
// a fully populated Registry that nothing ever read. Inline metadata was
// invisible to rules and to scope enumeration, and nothing failed — the server
// simply behaved as though no metadata existed. Both wiring sections must count.
func TestHasObjectSources(t *testing.T) {
	cases := []struct {
		name string
		doc  *seed.Document
		want bool
	}{
		{
			name: "nil document",
			doc:  nil,
			want: false,
		},
		{
			name: "neither section declared",
			doc:  &seed.Document{},
			want: false,
		},
		{
			name: "providers only",
			doc: &seed.Document{
				Providers: []seed.Provider{{ObjectType: "brand", Kind: "csv", Path: "brands.csv"}},
			},
			want: true,
		},
		{
			// The regression case: inline objects with no `providers:` entry.
			name: "objects only",
			doc: &seed.Document{
				Objects: []seed.Object{{
					ID:       "account:acme/brand:1",
					Metadata: json.RawMessage(`{"tags":["premium"]}`),
				}},
			},
			want: true,
		},
		{
			name: "both sections declared",
			doc: &seed.Document{
				Providers: []seed.Provider{{ObjectType: "brand", Kind: "csv", Path: "brands.csv"}},
				Objects: []seed.Object{{
					ID:       "account:acme/app:be",
					Metadata: json.RawMessage(`{"tier":"gold"}`),
				}},
			},
			want: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasObjectSources(tc.doc); got != tc.want {
				t.Fatalf("hasObjectSources() = %v, want %v", got, tc.want)
			}
		})
	}
}
