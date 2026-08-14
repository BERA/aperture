// This file is deliberately in the EXTERNAL test package: it imports rules to
// prove the end-to-end path, and keeping it out of package csvprovider keeps the
// provider's own import budget (errors, identity, provider, stdlib) untouched.
package csvprovider_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/frankbardon/aperture/csvprovider"
	"github.com/frankbardon/aperture/identity"
	"github.com/frankbardon/aperture/provider"
	"github.com/frankbardon/aperture/rules"
)

// TestMembershipRuleOverCSVArrays is the end-to-end demo: a CSV list column,
// loaded through the provider Registry, decided by a rule — with NO change to
// the rules package. rules.Validate already accepts a Var on the right of
// in/nin and expr does membership over []any, so arrays needed data, not engine
// work.
//
// It also pins the two failure modes the feature exists to prevent:
//
//   - a delimited BLOB matches "premium" inside "premium-trial" and grants
//     access it shouldn't; a real array does not;
//   - expr does no numeric/string coercion, so 5 in object.seats is FALSE
//     against string elements (research/expr-collection-semantics.md) — which is
//     why list<int> coerces elements at load.
func TestMembershipRuleOverCSVArrays(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "brands.csv")
	content := strings.Join([]string{
		"id,tags:list,seats:list<int>,aliases:list(;)",
		"brand:1,premium|launch,3|5,acme;acme-co",
		"brand:2,premium-trial,1,bcorp",
		"brand:3,,,",
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	reg := provider.NewRegistry()
	reg.MustRegister("brand", csvprovider.New(path))

	compiler := rules.NewCompiler()
	compile := func(t *testing.T, node *rules.Node) *rules.Compiled {
		t.Helper()
		if err := node.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
		program, err := compiler.Compile(node)
		if err != nil {
			t.Fatalf("Compile: %v", err)
		}
		return program
	}

	cases := []struct {
		name string
		id   string
		node *rules.Node
		want bool
	}{
		{
			name: `"premium" in object.tags`,
			id:   "brand:1",
			node: rules.Compare(rules.OpIn, rules.Lit("premium"), rules.Var("object.tags")),
			want: true,
		},
		{
			name: "a substring of another tag does not match",
			id:   "brand:2", // tags = ["premium-trial"]
			node: rules.Compare(rules.OpIn, rules.Lit("premium"), rules.Var("object.tags")),
			want: false,
		},
		{
			name: "typed elements make numeric membership work",
			id:   "brand:1",
			node: rules.Compare(rules.OpIn, rules.Lit(5), rules.Var("object.seats")),
			want: true,
		},
		{
			name: "the string 5 does not match an int element",
			id:   "brand:1",
			node: rules.Compare(rules.OpIn, rules.Lit("5"), rules.Var("object.seats")),
			want: false,
		},
		{
			name: "a custom-delimiter column is a real array too",
			id:   "brand:1",
			node: rules.Compare(rules.OpIn, rules.Lit("acme-co"), rules.Var("object.aliases")),
			want: true,
		},
		{
			name: "an empty list cell is a definite non-member",
			id:   "brand:3",
			node: rules.Compare(rules.OpIn, rules.Lit("premium"), rules.Var("object.tags")),
			want: false,
		},
		{
			name: "not in over a populated array",
			id:   "brand:2",
			node: rules.Compare(rules.OpNin, rules.Lit("premium"), rules.Var("object.tags")),
			want: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			md, err := reg.Fetch(context.Background(), identity.MustParse(tc.id))
			if err != nil {
				t.Fatalf("registry Fetch: %v", err)
			}
			got, err := compile(t, tc.node).Eval(context.Background(), rules.Input{
				Object: md,
				Action: "read",
			})
			if err != nil {
				t.Fatalf("Eval: %v", err)
			}
			if got != tc.want {
				t.Errorf("%s over %s = %v, want %v (metadata %#v)", tc.name, tc.id, got, tc.want, md)
			}
		})
	}
}
