package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/frankbardon/aperture/internal/server"
	"github.com/frankbardon/aperture/internal/wire/rpc"

	"google.golang.org/protobuf/types/known/structpb"
)

// E1-S1 added the engine's metadata seam but left it unwired: every surface
// built its engine with ScopeDeps{Lister: reg} and no WithMetadata, so a
// filtered enumerate reported APERTURE_PROVIDER_UNREGISTERED however well the
// registry was populated. buildDecisionStack is where both halves of the
// registry wiring belong, so wiring it there fixes `serve` and the one-shot
// commands in one place.
//
// The fixture's objects (writeRuleBackedSeed) are:
//
//	document:42  tags ["public"]     — a collection, matched by MEMBERSHIP
//	document:99  tags ["internal"]
//	document:7   tags "public"       — a STRING, matched by EQUALITY

// serveEnumerate runs a filtered Enumerate against the surface `serve` exposes,
// built exactly as runServe builds it.
func serveEnumerate(t *testing.T, seedPath string, fields map[string]*structpb.Value) ([]string, error) {
	t.Helper()
	ctx := context.Background()
	srv := httptest.NewServer(server.New(serverService(t, ctx, seedPath)))
	t.Cleanup(srv.Close)

	cli := rpc.NewApertureServiceJSONClient(srv.URL, http.DefaultClient)
	res, err := cli.Enumerate(ctx, &rpc.EnumerateRequest{
		Account: "acme", Principal: "alice", Action: "list",
		Pattern: "account:acme/**", Fields: fields,
	})
	if err != nil {
		return nil, err
	}
	out := slices.Clone(res.ObjectIds)
	slices.Sort(out)
	return out, nil
}

func TestServeEnumerateFiltersByMetadata(t *testing.T) {
	seedPath := writeRuleBackedSeed(t)

	all := []string{"account:acme/document:42", "account:acme/document:7", "account:acme/document:99"}
	if got, err := serveEnumerate(t, seedPath, nil); err != nil || !slices.Equal(got, all) {
		t.Fatalf("unfiltered enumerate = %v (err %v), want %v", got, err, all)
	}

	cases := []struct {
		name   string
		fields map[string]*structpb.Value
		want   []string
	}{
		{
			// Membership for document:42's list, equality for document:7's string.
			name:   "tags=public",
			fields: map[string]*structpb.Value{"tags": structpb.NewStringValue("public")},
			want:   []string{"account:acme/document:42", "account:acme/document:7"},
		},
		{
			name:   "tags=internal",
			fields: map[string]*structpb.Value{"tags": structpb.NewStringValue("internal")},
			want:   []string{"account:acme/document:99"},
		},
		{
			name:   "no object carries the field",
			fields: map[string]*structpb.Value{"tier": structpb.NewStringValue("premium")},
			want:   nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := serveEnumerate(t, seedPath, tc.fields)
			if err != nil {
				t.Fatalf("Enumerate: %v", err)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("enumerate %v = %v, want %v", tc.fields, got, tc.want)
			}
		})
	}
}
