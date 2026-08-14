// This file is deliberately in the EXTERNAL test package, for the same reason
// membership_demo_test.go is: it imports rules to run the real expression
// evaluator, and keeping it out of package csvprovider leaves the provider's
// import budget (errors, identity, provider, stdlib) untouched.
package csvprovider_test

import (
	"context"
	"strings"
	"testing"

	"github.com/frankbardon/aperture/provider"
	"github.com/frankbardon/aperture/rules"
)

// TestFieldsMembershipMatchesExpr is the correspondence gate behind
// provider.ValuesEqual's doc comment.
//
// provider/ is a strict leaf (identity + errors + stdlib), so it MAY NOT import
// expr-lang; ValuesEqual therefore reimplements expr's equality instead of
// calling runtime.Equal the way rules/ does. A reimplementation that drifts is
// worse than none: Query is how an Enumerate bounds itself, so a membership test
// that disagrees with `in` would select objects a Check then denies. This test
// runs BOTH — provider.MatchField and a compiled `<want> in object.field` rule —
// over the same metadata and fails on any disagreement.
//
// Literals go through rules.Lit, which marshals to JSON, so the table stays in
// the value space a rule can actually spell: strings, integers, bools, and null.
func TestFieldsMembershipMatchesExpr(t *testing.T) {
	md := provider.Metadata{
		"tags":   []any{"premium", "launch"},
		"ranks":  []any{int64(3), int64(5)},
		"flags":  []any{true},
		"prices": []any{9.5, 10.0},
		"empty":  []any{},
		"holes":  []any{nil, "a"},
	}

	cases := []struct {
		field string
		want  any
	}{
		{"tags", "premium"},
		{"tags", "launch"},
		{"tags", "trial"},
		{"tags", "premium-trial"}, // the blob-match failure mode, from the other side
		{"tags", 5},
		{"tags", true},
		{"tags", nil},
		{"ranks", 5},
		{"ranks", 3},
		{"ranks", 4},
		{"ranks", "5"}, // the silent-coercion failure mode
		{"flags", true},
		{"flags", false},
		{"flags", "true"},
		{"prices", 10}, // an int literal against a float element
		{"prices", 9},
		{"empty", "premium"},
		{"empty", nil},
		{"holes", nil},
		{"holes", "a"},
	}

	compiler := rules.NewCompiler()
	for _, tc := range cases {
		name := strings.Join([]string{tc.field, sprintWant(tc.want)}, "/")
		t.Run(name, func(t *testing.T) {
			node := rules.Compare(rules.OpIn, rules.Lit(tc.want), rules.Var("object."+tc.field))
			if err := node.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
			program, err := compiler.Compile(node)
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}
			byRule, err := program.Eval(context.Background(), rules.Input{Object: md, Action: "read"})
			if err != nil {
				t.Fatalf("Eval: %v", err)
			}
			byFilter := provider.MatchField(md[tc.field], tc.want)
			if byFilter != byRule {
				t.Errorf("provider.MatchField(%s, %#v) = %v but the rule `%#v in object.%s` = %v — "+
					"Query and Check now disagree over the same value",
					tc.field, tc.want, byFilter, tc.want, tc.field, byRule)
			}
		})
	}
}

// sprintWant names a want for a subtest without pulling fmt's rendering into the
// assertion itself.
func sprintWant(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case string:
		return "string:" + x
	case bool:
		if x {
			return "bool:true"
		}
		return "bool:false"
	}
	return "number"
}
