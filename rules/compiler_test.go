package rules

import (
	"context"
	"testing"

	aerr "github.com/frankbardon/aperture/errors"
)

func TestCompileAndEvalOverMetadata(t *testing.T) {
	c := NewCompiler()
	rule := And(
		Compare(OpEq, Var("object.classification"), Lit("public")),
		Compare(OpGe, Var("object.version"), Lit(2)),
	)
	compiled, err := c.Compile(rule)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	ctx := context.Background()
	cases := []struct {
		name string
		md   map[string]any
		want bool
	}{
		{"match", map[string]any{"classification": "public", "version": 3}, true},
		{"wrong class", map[string]any{"classification": "secret", "version": 3}, false},
		{"low version", map[string]any{"classification": "public", "version": 1}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := compiled.Eval(ctx, Input{Object: tc.md})
			if err != nil {
				t.Fatalf("eval: %v", err)
			}
			if got != tc.want {
				t.Fatalf("eval = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestEvalEqualityAgainstMissingFieldIsFalse documents that an equality against a
// metadata field the object lacks reads as nil and yields false — not an error.
func TestEvalEqualityAgainstMissingFieldIsFalse(t *testing.T) {
	c := NewCompiler()
	compiled, err := c.Compile(Compare(OpEq, Var("object.classification"), Lit("public")))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	got, err := compiled.Eval(context.Background(), Input{Object: map[string]any{}})
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	if got {
		t.Fatalf("equality against a missing field should be false")
	}
}

// TestEvalOrderedComparisonAgainstMissingFieldErrors documents that an ordered
// comparison (>=, <, …) against a field the object lacks is an APERTURE_RULE_EVAL
// error — the rule assumes a field the object does not carry. The scope resolver
// treats it as a non-decision rather than silently selecting.
func TestEvalOrderedComparisonAgainstMissingFieldErrors(t *testing.T) {
	c := NewCompiler()
	compiled, err := c.Compile(Compare(OpGe, Var("object.version"), Lit(2)))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	_, err = compiled.Eval(context.Background(), Input{Object: map[string]any{}})
	if err == nil {
		t.Fatalf("expected an eval error for an ordered comparison against a missing field")
	}
	if code := aerr.CodeOf(err); code != aerr.APERTURE_RULE_EVAL {
		t.Fatalf("code = %q, want APERTURE_RULE_EVAL", code)
	}
}

// TestEvalNestedAccessIsNilSafe pins the E2-S2 guarantee: because NodeVar renders
// with optional chaining (object?.owner?.dept), reading a nested path through a
// MISSING intermediate yields nil and the enclosing comparison goes false — it is
// never the expr-lang runtime error "cannot fetch dept from <nil>", which would
// surface as APERTURE_RULE_EVAL at Check time. A path through a PRESENT
// intermediate is unchanged.
func TestEvalNestedAccessIsNilSafe(t *testing.T) {
	c := NewCompiler()
	ctx := context.Background()

	cases := []struct {
		name string
		rule *Node
		in   Input
		want bool
	}{
		{
			name: "present nested path matches",
			rule: Compare(OpEq, Var("object.owner.dept"), Lit("eng")),
			in:   Input{Object: map[string]any{"owner": map[string]any{"dept": "eng"}}},
			want: true,
		},
		{
			name: "present nested path does not match",
			rule: Compare(OpEq, Var("object.owner.dept"), Lit("eng")),
			in:   Input{Object: map[string]any{"owner": map[string]any{"dept": "sales"}}},
			want: false,
		},
		{
			name: "missing intermediate is false, not an error",
			rule: Compare(OpEq, Var("object.owner.dept"), Lit("eng")),
			in:   Input{Object: map[string]any{"title": "hello"}},
			want: false,
		},
		{
			name: "missing leaf under a present intermediate is false",
			rule: Compare(OpEq, Var("object.owner.dept"), Lit("eng")),
			in:   Input{Object: map[string]any{"owner": map[string]any{"id": "u1"}}},
			want: false,
		},
		{
			name: "nil object map is false, not an error",
			rule: Compare(OpEq, Var("object.owner.dept"), Lit("eng")),
			in:   Input{},
			want: false,
		},
		{
			name: "depth-3 path through a missing intermediate is false",
			rule: Compare(OpEq, Var("principal.attrs.team.name"), Lit("core")),
			in:   Input{Principal: map[string]any{"id": "alice"}},
			want: false,
		},
		{
			name: "depth-3 path fully present matches",
			rule: Compare(OpEq, Var("principal.attrs.team.name"), Lit("core")),
			in: Input{Principal: map[string]any{
				"attrs": map[string]any{"team": map[string]any{"name": "core"}},
			}},
			want: true,
		},
		{
			name: "in over a nested list through a present intermediate",
			rule: Compare(OpIn, Lit("eu"), Var("object.owner.regions")),
			in: Input{Object: map[string]any{
				"owner": map[string]any{"regions": []any{"us", "eu"}},
			}},
			want: true,
		},
		{
			name: "in over a nested list through a missing intermediate is false",
			rule: Compare(OpIn, Lit("eu"), Var("object.owner.regions")),
			in:   Input{Object: map[string]any{}},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			compiled, err := c.Compile(tc.rule)
			if err != nil {
				t.Fatalf("compile: %v", err)
			}
			got, err := compiled.Eval(ctx, tc.in)
			if err != nil {
				t.Fatalf("eval must not error on a nested read: %v", err)
			}
			if got != tc.want {
				t.Fatalf("eval = %v, want %v (source %s)", got, tc.want, compiled.Source())
			}
		})
	}
}

// TestEvalNinOverAbsentFieldGrants pins a surprising but pre-existing expr-lang
// behavior, unchanged by the optional-chaining render: `x not in <nil>` is TRUE.
// A "nin" rule therefore SELECTS (grants) for an object that simply lacks the
// field — a deny-list over an absent column passes everything. Nested objects and
// list-valued metadata make this far easier to hit, so it is pinned here rather
// than left to be rediscovered in production. Authors who need the field to exist
// must say so explicitly, e.g. And(Compare(OpNe, Var(path), Lit(nil)), nin...).
func TestEvalNinOverAbsentFieldGrants(t *testing.T) {
	c := NewCompiler()
	ctx := context.Background()

	// Absent at the top level.
	flat, err := c.Compile(Compare(OpNin, Lit("alice"), Var("object.blocklist")))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	got, err := flat.Eval(ctx, Input{Object: map[string]any{}})
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	if !got {
		t.Fatalf(`"x not in <absent field>" must be true (pre-existing expr behavior)`)
	}
	// And the dual: `in` over the same absent field is false.
	in, err := c.Compile(Compare(OpIn, Lit("alice"), Var("object.blocklist")))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	got, err = in.Eval(ctx, Input{Object: map[string]any{}})
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	if got {
		t.Fatalf(`"x in <absent field>" must be false`)
	}
	// Absent through a missing intermediate behaves identically now that nested
	// reads are nil-safe.
	nested, err := c.Compile(Compare(OpNin, Lit("alice"), Var("object.owner.blocklist")))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	got, err = nested.Eval(ctx, Input{Object: map[string]any{}})
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	if !got {
		t.Fatalf(`"x not in <absent nested field>" must be true`)
	}
	// The deny-list actually denies once the field is present and matches.
	got, err = nested.Eval(ctx, Input{Object: map[string]any{
		"owner": map[string]any{"blocklist": []any{"alice"}},
	}})
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	if got {
		t.Fatalf("nin must be false when the element IS in the list")
	}

	// The documented workaround (docs/src/concepts/rules.md, skills/rules-engine.md):
	// require the field explicitly so a deny-list stops granting on absent data.
	guarded, err := c.Compile(And(
		Compare(OpNe, Var("object.blocklist"), Lit(nil)),
		Compare(OpNin, Lit("alice"), Var("object.blocklist")),
	))
	if err != nil {
		t.Fatalf("compile guarded nin: %v", err)
	}
	for _, tc := range []struct {
		name string
		md   map[string]any
		want bool
	}{
		{"field absent: guard denies", map[string]any{}, false},
		{"field present, not listed: selects", map[string]any{"blocklist": []any{"bob"}}, true},
		{"field present, listed: denies", map[string]any{"blocklist": []any{"alice"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := guarded.Eval(ctx, Input{Object: tc.md})
			if err != nil {
				t.Fatalf("eval: %v", err)
			}
			if got != tc.want {
				t.Fatalf("eval = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestEvalUsesPrincipalActionAndFunctions(t *testing.T) {
	c := NewCompiler()
	rule := And(
		Compare(OpEq, Var("principal.id"), Lit("alice")),
		Compare(OpEq, Var("action"), Lit("read")),
		Compare(OpEq, Call("lower", Var("object.owner")), Lit("alice")),
		Compare(OpIn, Var("object.region"), List(Lit("us"), Lit("eu"))),
	)
	compiled, err := c.Compile(rule)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	in := Input{
		Object:    map[string]any{"owner": "ALICE", "region": "eu"},
		Principal: map[string]any{"id": "alice"},
		Action:    "read",
	}
	got, err := compiled.Eval(context.Background(), in)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	if !got {
		t.Fatalf("expected rule to select; got false")
	}
}

// TestEvalIsPure proves evaluation does not mutate the supplied metadata map and
// is deterministic across repeated runs over the same snapshot.
func TestEvalIsPure(t *testing.T) {
	c := NewCompiler()
	compiled, err := c.Compile(Compare(OpEq, Var("object.tier"), Lit("gold")))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	md := map[string]any{"tier": "gold"}
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		got, err := compiled.Eval(ctx, Input{Object: md})
		if err != nil || !got {
			t.Fatalf("run %d: got=%v err=%v", i, got, err)
		}
	}
	if len(md) != 1 || md["tier"] != "gold" {
		t.Fatalf("evaluation mutated the metadata snapshot: %v", md)
	}
}

func TestCompileValidationFailures(t *testing.T) {
	c := NewCompiler()
	cases := []struct {
		name string
		node *Node
		code aerr.Code
	}{
		{
			name: "unknown variable root",
			node: Compare(OpEq, Var("subject.id"), Lit("x")),
			code: aerr.APERTURE_RULE_UNKNOWN_VARIABLE,
		},
		{
			name: "structurally invalid",
			node: And(Var("object.x")),
			code: aerr.APERTURE_RULE_INVALID,
		},
		{
			name: "type error: string compared to number",
			node: Compare(OpLt, Var("action"), Lit(5)),
			code: aerr.APERTURE_RULE_TYPE_ERROR,
		},
		{
			name: "type error: non-boolean result",
			node: Lit(5),
			code: aerr.APERTURE_RULE_TYPE_ERROR,
		},
		{
			name: "type error: unknown function",
			node: Compare(OpEq, Call("frobnicate", Var("object.x")), Lit("y")),
			code: aerr.APERTURE_RULE_TYPE_ERROR,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.Compile(tc.node)
			if err == nil {
				t.Fatalf("expected compile error, got nil")
			}
			if code := aerr.CodeOf(err); code != tc.code {
				t.Fatalf("code = %q, want %q (err: %v)", code, tc.code, err)
			}
		})
	}
}

// TestDisabledBuiltinsKeepEvalDeterministic proves no nondeterministic builtin
// (e.g. now()) is reachable: referencing one is rejected at compile time, so a
// rule can never read wall-clock state.
func TestDisabledBuiltinsKeepEvalDeterministic(t *testing.T) {
	c := NewCompiler()
	_, err := c.Compile(Compare(OpEq, Call("now"), Lit("x")))
	if err == nil {
		t.Fatalf("now() must not be callable from a rule")
	}
	if code := aerr.CodeOf(err); code != aerr.APERTURE_RULE_TYPE_ERROR {
		t.Fatalf("code = %q, want APERTURE_RULE_TYPE_ERROR", code)
	}
}

func TestHostFunctionRegistration(t *testing.T) {
	c := NewCompiler(Function("riskScore", func(args ...any) (any, error) {
		// pure: deterministic over its argument
		s, _ := args[0].(string)
		return len(s), nil
	}))
	compiled, err := c.Compile(Compare(OpGt, Call("riskScore", Var("object.label")), Lit(2)))
	if err != nil {
		t.Fatalf("compile with host function: %v", err)
	}
	got, err := compiled.Eval(context.Background(), Input{Object: map[string]any{"label": "abcd"}})
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	if !got {
		t.Fatalf("riskScore('abcd')=4 > 2 should be true")
	}
}
