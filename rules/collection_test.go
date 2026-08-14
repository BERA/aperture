package rules

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/expr-lang/expr/builtin"

	aerr "github.com/frankbardon/aperture/errors"
)

// collectionMetadata is the shared object snapshot the collection-operator tests
// evaluate against: a populated array, a populated object, their empty
// counterparts, plus a string and a number for the type-mismatch cases. Nothing
// here carries a field named "missing" — that is the absent-field case.
func collectionMetadata() map[string]any {
	return map[string]any{
		"tags":     []any{"a", "b"},
		"owner":    map[string]any{"dept": "eng"},
		"empty":    []any{},
		"emptyObj": map[string]any{},
		"title":    "hello",
		"seats":    []any{int64(1), int64(2)},
		"n":        int64(5),
	}
}

// TestCollectionOperatorRender pins the rendered expr-lang source for every new
// operator.
//
// E5-S1 changed what most of these render to. A comparison whose collection
// operand is a VARIABLE could meet the wrong shape at runtime, so it renders to
// the guarded dispatcher `$op` (ast.go renderGuarded) — that is what makes a
// mistyped field deny with a note instead of raising APERTURE_RULE_EVAL. The
// E4-S1 native/backing-function forms survive wherever no operand can mismatch:
// a LIST LITERAL is an array by construction, so `x in ["us"]` keeps its native
// `in`, and `hasAll([...], [...])` keeps its backing call. exists never guards —
// a nil test has no shape contract.
func TestCollectionOperatorRender(t *testing.T) {
	cases := []struct {
		name string
		node *Node
		want string
	}{
		{"has over a variable is guarded", Compare(OpHas, Var("object.tags"), Lit("x")),
			`$op("has", __notes, "object.tags", object?.tags, "", "x")`},
		{"hasKey over a variable is guarded", Compare(OpHasKey, Var("object.owner"), Lit("dept")),
			`$op("hasKey", __notes, "object.owner", object?.owner, "", "dept")`},
		{"hasAll over a variable is guarded", Compare(OpHasAll, Var("object.tags"), List(Lit("a"), Lit("b"))),
			`$op("hasAll", __notes, "object.tags", object?.tags, "", ["a", "b"])`},
		{"hasAny over a variable is guarded", Compare(OpHasAny, Var("object.tags"), List(Lit("a"))),
			`$op("hasAny", __notes, "object.tags", object?.tags, "", ["a"])`},
		{"hasNone over a variable is guarded", Compare(OpHasNone, Var("object.tags"), List(Lit("z"))),
			`$op("hasNone", __notes, "object.tags", object?.tags, "", ["z"])`},
		{"subsetOf carries both operand paths", Compare(OpSubsetOf, Var("object.tags"), Var("object.allowed")),
			`$op("subsetOf", __notes, "object.tags", object?.tags, "object.allowed", object?.allowed)`},
		{"isEmpty is unary (right renders as \"\"/nil)", Unary(OpIsEmpty, Var("object.tags")),
			`$op("isEmpty", __notes, "object.tags", object?.tags, "", nil)`},
		{"isNotEmpty is unary", Unary(OpIsNotEmpty, Var("object.owner")),
			`$op("isNotEmpty", __notes, "object.owner", object?.owner, "", nil)`},
		{"exists renders as a nil test through optional chaining", Unary(OpExists, Var("object.owner.dept")),
			`(object?.owner?.dept != nil)`},

		// No operand can be the wrong shape: the E4-S1 forms are kept.
		{"in over a list literal keeps its native render", Compare(OpIn, Var("object.region"), List(Lit("us"))),
			`(object?.region in ["us"])`},
		{"nin over a list literal keeps its native render", Compare(OpNin, Var("principal.id"), List(Lit("a"))),
			`(principal?.id not in ["a"])`},
		{"has over a list literal keeps its flipped in", Compare(OpHas, List(Lit("a")), Lit("a")),
			`("a" in ["a"])`},
		{"hasAll over two list literals keeps its backing call", Compare(OpHasAll, List(Lit("a")), List(Lit("a"))),
			`hasAll(["a"], ["a"])`},
		{"isEmpty over a list literal keeps its backing call", Unary(OpIsEmpty, List()),
			`isEmpty([])`},

		// A variable on either side is enough to guard.
		{"in over a variable collection is guarded", Compare(OpIn, Var("principal.id"), Var("object.editors")),
			`$op("in", __notes, "principal.id", principal?.id, "object.editors", object?.editors)`},
		{"nin over a variable collection is guarded", Compare(OpNin, Var("principal.id"), Var("object.blocklist")),
			`$op("nin", __notes, "principal.id", principal?.id, "object.blocklist", object?.blocklist)`},
		{"a non-variable operand has no path to report", Unary(OpIsEmpty, Call("lower", Var("object.title"))),
			`$op("isEmpty", __notes, "", lower(object?.title), "", nil)`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.node.Validate(); err != nil {
				t.Fatalf("validate: %v", err)
			}
			got, err := tc.node.Expr()
			if err != nil {
				t.Fatalf("Expr: %v", err)
			}
			if got != tc.want {
				t.Fatalf("render = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestCollectionOperatorEval is the per-operator behavior table: a true case, a
// false case, an ABSENT field, and an EMPTY collection for each of the nine.
//
// The absent-field rows are the deny-safety contract: an absent array is never an
// error. It reads as an EMPTY collection, so the positive operators go false and
// the negative ones (hasNone, subsetOf, isEmpty) go true — the same asymmetry
// `in`/`nin` already have.
func TestCollectionOperatorEval(t *testing.T) {
	c := NewCompiler()
	ctx := context.Background()
	md := collectionMetadata()

	cases := []struct {
		name string
		node *Node
		want bool
	}{
		// has — array element membership.
		{"has: present element", Compare(OpHas, Var("object.tags"), Lit("a")), true},
		{"has: absent element", Compare(OpHas, Var("object.tags"), Lit("z")), false},
		{"has: absent field", Compare(OpHas, Var("object.missing"), Lit("a")), false},
		{"has: empty array", Compare(OpHas, Var("object.empty"), Lit("a")), false},
		{"has: no coercion between string and number", Compare(OpHas, Var("object.seats"), Lit("1")), false},
		{"has: numeric element", Compare(OpHas, Var("object.seats"), Lit(1)), true},

		// hasKey — object key membership.
		{"hasKey: present key", Compare(OpHasKey, Var("object.owner"), Lit("dept")), true},
		{"hasKey: absent key", Compare(OpHasKey, Var("object.owner"), Lit("nope")), false},
		{"hasKey: absent field", Compare(OpHasKey, Var("object.missing"), Lit("dept")), false},
		{"hasKey: empty object", Compare(OpHasKey, Var("object.emptyObj"), Lit("dept")), false},

		// hasAll.
		{"hasAll: every element present", Compare(OpHasAll, Var("object.tags"), List(Lit("a"), Lit("b"))), true},
		{"hasAll: one element missing", Compare(OpHasAll, Var("object.tags"), List(Lit("a"), Lit("z"))), false},
		{"hasAll: absent field", Compare(OpHasAll, Var("object.missing"), List(Lit("a"))), false},
		{"hasAll: empty array", Compare(OpHasAll, Var("object.empty"), List(Lit("a"))), false},
		{"hasAll: empty requirement is vacuously true", Compare(OpHasAll, Var("object.tags"), List()), true},

		// hasAny.
		{"hasAny: one element present", Compare(OpHasAny, Var("object.tags"), List(Lit("z"), Lit("a"))), true},
		{"hasAny: no element present", Compare(OpHasAny, Var("object.tags"), List(Lit("y"), Lit("z"))), false},
		{"hasAny: absent field", Compare(OpHasAny, Var("object.missing"), List(Lit("a"))), false},
		{"hasAny: empty array", Compare(OpHasAny, Var("object.empty"), List(Lit("a"))), false},

		// hasNone — a negative operator, so it GRANTS on absent data.
		{"hasNone: no element present", Compare(OpHasNone, Var("object.tags"), List(Lit("y"), Lit("z"))), true},
		{"hasNone: one element present", Compare(OpHasNone, Var("object.tags"), List(Lit("a"))), false},
		{"hasNone: absent field grants", Compare(OpHasNone, Var("object.missing"), List(Lit("a"))), true},
		{"hasNone: empty array", Compare(OpHasNone, Var("object.empty"), List(Lit("a"))), true},

		// subsetOf — also grants on absent data (the empty set is a subset).
		{"subsetOf: contained", Compare(OpSubsetOf, Var("object.tags"), List(Lit("a"), Lit("b"), Lit("c"))), true},
		{"subsetOf: not contained", Compare(OpSubsetOf, Var("object.tags"), List(Lit("a"))), false},
		{"subsetOf: absent field grants", Compare(OpSubsetOf, Var("object.missing"), List(Lit("a"))), true},
		{"subsetOf: empty array", Compare(OpSubsetOf, Var("object.empty"), List(Lit("a"))), true},
		{"subsetOf: against another metadata array", Compare(OpSubsetOf, Var("object.tags"), Var("object.tags")), true},

		// isEmpty / isNotEmpty — arrays and objects alike.
		{"isEmpty: empty array", Unary(OpIsEmpty, Var("object.empty")), true},
		{"isEmpty: empty object", Unary(OpIsEmpty, Var("object.emptyObj")), true},
		{"isEmpty: populated array", Unary(OpIsEmpty, Var("object.tags")), false},
		{"isEmpty: populated object", Unary(OpIsEmpty, Var("object.owner")), false},
		{"isEmpty: absent field reads as empty", Unary(OpIsEmpty, Var("object.missing")), true},
		{"isNotEmpty: populated array", Unary(OpIsNotEmpty, Var("object.tags")), true},
		{"isNotEmpty: populated object", Unary(OpIsNotEmpty, Var("object.owner")), true},
		{"isNotEmpty: empty array", Unary(OpIsNotEmpty, Var("object.empty")), false},
		{"isNotEmpty: empty object", Unary(OpIsNotEmpty, Var("object.emptyObj")), false},
		{"isNotEmpty: absent field", Unary(OpIsNotEmpty, Var("object.missing")), false},

		// exists — any path, including through a missing intermediate.
		{"exists: present nested path", Unary(OpExists, Var("object.owner.dept")), true},
		{"exists: present top-level field", Unary(OpExists, Var("object.tags")), true},
		{"exists: empty array still exists", Unary(OpExists, Var("object.empty")), true},
		{"exists: absent leaf under a present parent", Unary(OpExists, Var("object.owner.nope")), false},
		{"exists: absent intermediate is false, not an error", Unary(OpExists, Var("object.missing.deep")), false},
		{"exists: absent top-level field", Unary(OpExists, Var("object.missing")), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			compiled, err := c.Compile(tc.node)
			if err != nil {
				t.Fatalf("compile: %v", err)
			}
			got, err := compiled.Eval(ctx, Input{Object: md})
			if err != nil {
				t.Fatalf("eval must not error (source %s): %v", compiled.Source(), err)
			}
			if got != tc.want {
				t.Fatalf("eval = %v, want %v (source %s)", got, tc.want, compiled.Source())
			}
		})
	}
}

// The other half of the policy — a field of the WRONG SHAPE — was an
// APERTURE_RULE_EVAL error in E4-S1 and is a deny-safe false plus an Explain note
// as of E5-S1. Its coverage lives in shape_test.go.

// TestUnaryOperandValidation is the arity contract. Exactly isEmpty, isNotEmpty
// and exists must OMIT Right; supplying one is an error rather than something
// quietly ignored, so a rule has only one spelling. Every other operator still
// requires both operands — the blanket rule is loosened, not removed.
func TestUnaryOperandValidation(t *testing.T) {
	unary := []string{OpIsEmpty, OpIsNotEmpty, OpExists}
	binary := []string{
		OpEq, OpNe, OpLt, OpLe, OpGt, OpGe, OpIn, OpNin,
		OpHas, OpHasKey, OpHasAll, OpHasAny, OpHasNone, OpSubsetOf,
	}

	for _, op := range unary {
		t.Run(op+" accepts an omitted right", func(t *testing.T) {
			if err := Unary(op, Var("object.tags")).Validate(); err != nil {
				t.Fatalf("unary %s should validate with no right operand: %v", op, err)
			}
		})
		t.Run(op+" rejects a present right", func(t *testing.T) {
			n := &Node{Type: NodeCompare, Op: op, Left: Var("object.tags"), Right: Lit("x")}
			err := n.Validate()
			if err == nil {
				t.Fatalf("unary %s must reject a right operand", op)
			}
			if code := aerr.CodeOf(err); code != aerr.APERTURE_RULE_INVALID {
				t.Fatalf("code = %q, want APERTURE_RULE_INVALID", code)
			}
		})
		t.Run(op+" still requires a left", func(t *testing.T) {
			n := &Node{Type: NodeCompare, Op: op}
			if err := n.Validate(); err == nil {
				t.Fatalf("unary %s must require a left operand", op)
			}
		})
		t.Run(op+" still validates its left", func(t *testing.T) {
			err := Unary(op, Var("subject.id")).Validate()
			if code := aerr.CodeOf(err); code != aerr.APERTURE_RULE_UNKNOWN_VARIABLE {
				t.Fatalf("code = %q, want APERTURE_RULE_UNKNOWN_VARIABLE", code)
			}
		})
	}

	for _, op := range binary {
		t.Run(op+" still requires a right", func(t *testing.T) {
			n := &Node{Type: NodeCompare, Op: op, Left: Var("object.tags")}
			err := n.Validate()
			if err == nil {
				t.Fatalf("binary %s must require a right operand", op)
			}
			if code := aerr.CodeOf(err); code != aerr.APERTURE_RULE_INVALID {
				t.Fatalf("code = %q, want APERTURE_RULE_INVALID", code)
			}
		})
	}
}

// TestCollectionOperandShapeValidation extends the in/nin operand-shape rule to
// the new operators: the set-valued ones take a list or a variable on the right,
// the element-valued ones take anything but a list.
func TestCollectionOperandShapeValidation(t *testing.T) {
	cases := []struct {
		name string
		node *Node
		ok   bool
	}{
		{"hasAll with a list", Compare(OpHasAll, Var("object.tags"), List(Lit("a"))), true},
		{"hasAll with a var", Compare(OpHasAll, Var("object.tags"), Var("object.other")), true},
		{"hasAll with a scalar", Compare(OpHasAll, Var("object.tags"), Lit("a")), false},
		{"hasAny with a scalar", Compare(OpHasAny, Var("object.tags"), Lit("a")), false},
		{"hasNone with a scalar", Compare(OpHasNone, Var("object.tags"), Lit("a")), false},
		{"subsetOf with a scalar", Compare(OpSubsetOf, Var("object.tags"), Lit("a")), false},
		{"subsetOf with a call", Compare(OpSubsetOf, Var("object.tags"), Call("lower", Var("object.s"))), false},

		{"has with a scalar", Compare(OpHas, Var("object.tags"), Lit("a")), true},
		{"has with a var", Compare(OpHas, Var("object.tags"), Var("principal.id")), true},
		{"has with a call", Compare(OpHas, Var("object.tags"), Call("lower", Var("principal.id"))), true},
		{"has with a list", Compare(OpHas, Var("object.tags"), List(Lit("a"))), false},
		{"hasKey with a list", Compare(OpHasKey, Var("object.owner"), List(Lit("dept"))), false},
		{"hasKey with a scalar", Compare(OpHasKey, Var("object.owner"), Lit("dept")), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.node.Validate()
			if tc.ok {
				if err != nil {
					t.Fatalf("expected valid, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected APERTURE_RULE_INVALID, got nil")
			}
			if code := aerr.CodeOf(err); code != aerr.APERTURE_RULE_INVALID {
				t.Fatalf("code = %q, want APERTURE_RULE_INVALID", code)
			}
		})
	}
}

// TestCollectionOperatorJSONRoundTrip proves the AST JSON is byte-identical
// through marshal -> unmarshal -> marshal for every operator, unary ops included:
// a unary node's "right" stays OMITTED, so a rule authored as "has all" reads
// back as "has all" and the editor/state-file contract holds.
func TestCollectionOperatorJSONRoundTrip(t *testing.T) {
	nodes := map[string]*Node{
		"has":        Compare(OpHas, Var("object.tags"), Lit("x")),
		"hasKey":     Compare(OpHasKey, Var("object.owner"), Lit("dept")),
		"hasAll":     Compare(OpHasAll, Var("object.tags"), List(Lit("a"), Lit("b"))),
		"hasAny":     Compare(OpHasAny, Var("object.tags"), List(Lit("a"))),
		"hasNone":    Compare(OpHasNone, Var("object.tags"), List(Lit("a"))),
		"subsetOf":   Compare(OpSubsetOf, Var("object.tags"), Var("object.allowed")),
		"isEmpty":    Unary(OpIsEmpty, Var("object.tags")),
		"isNotEmpty": Unary(OpIsNotEmpty, Var("object.tags")),
		"exists":     Unary(OpExists, Var("object.owner.dept")),
		"in":         Compare(OpIn, Var("object.region"), List(Lit("us"))),
		"nin":        Compare(OpNin, Var("object.region"), List(Lit("us"))),
	}
	unary := map[string]bool{"isEmpty": true, "isNotEmpty": true, "exists": true}

	for op, n := range nodes {
		t.Run(op, func(t *testing.T) {
			if err := n.Validate(); err != nil {
				t.Fatalf("validate: %v", err)
			}
			first, err := json.Marshal(n)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var back Node
			if err := json.Unmarshal(first, &back); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			second, err := json.Marshal(&back)
			if err != nil {
				t.Fatalf("re-marshal: %v", err)
			}
			if string(first) != string(second) {
				t.Fatalf("round-trip not byte-identical:\n first  = %s\n second = %s", first, second)
			}
			// The operator survives verbatim: "has all" reads back as "has all".
			if back.Op != op {
				t.Fatalf("op = %q, want %q", back.Op, op)
			}
			if err := back.Validate(); err != nil {
				t.Fatalf("decoded node no longer validates: %v", err)
			}
			// A unary node must carry no "right" key at all.
			var shape map[string]json.RawMessage
			if err := json.Unmarshal(first, &shape); err != nil {
				t.Fatalf("shape unmarshal: %v", err)
			}
			_, hasRight := shape["right"]
			if unary[op] && hasRight {
				t.Fatalf("unary %s must omit \"right\": %s", op, first)
			}
			if !unary[op] && !hasRight {
				t.Fatalf("binary %s must carry \"right\": %s", op, first)
			}
		})
	}
}

// curatedFunctions is the EXACT set of function names defaultFunctions registers.
// A host may add more with rules.Function; nothing else is reachable out of the
// box.
var curatedFunctions = []string{
	"lower", "upper", "contains", "startsWith", "endsWith", "len",
	fnHasAll, fnHasAny, fnHasNone, fnSubsetOf, fnIsEmpty, fnIsNotEmpty,
}

// reservedInfixFunctions are registered by defaultFunctions but are NOT reachable
// through a NodeCall: expr-lang's grammar reserves these three as INFIX operators
// (`object.s contains "x"`), so `contains(a, b)` does not parse as a call. Pinned
// here because it is a live gap between what the package registers and what a
// rule can actually invoke — pre-existing, and unchanged by the collection
// operators, which is exactly why it must not drift silently.
var reservedInfixFunctions = []string{"contains", "startsWith", "endsWith"}

// TestCallableFunctionSetIsPinned pins the callable surface from both sides.
//
// It walks expr-lang's OWN builtin registry and asserts that every name in it,
// other than the ones Aperture deliberately registers, is unreachable through a
// NodeCall. That matters because expr.DisableAllBuiltins does not do what its
// name suggests: the parser resolves the fifteen PREDICATE builtins (all, any,
// filter, map, …) before it consults the disabled table, so `all(...)` compiles
// under Aperture's exact option set. NodeCall renders `name(args...)` verbatim,
// so without the denylist in Validate a NodeCall{Name: "all"} would compile.
func TestCallableFunctionSetIsPinned(t *testing.T) {
	c := NewCompiler()
	curated := make(map[string]bool, len(curatedFunctions))
	for _, name := range curatedFunctions {
		curated[name] = true
	}

	// Side one: nothing from expr's builtin registry leaks through.
	for _, name := range builtin.Names {
		if curated[name] {
			continue // deliberately re-registered by defaultFunctions
		}
		t.Run("blocked/"+name, func(t *testing.T) {
			_, err := c.Compile(Compare(OpEq, Call(name, Var("object.x")), Lit("y")))
			if err == nil {
				t.Fatalf("expr builtin %q must not be callable from a rule", name)
			}
			switch code := aerr.CodeOf(err); code {
			case aerr.APERTURE_RULE_INVALID, aerr.APERTURE_RULE_TYPE_ERROR:
			default:
				t.Fatalf("%q rejected with %q, want APERTURE_RULE_INVALID or APERTURE_RULE_TYPE_ERROR", name, code)
			}
		})
	}

	// The predicate builtins specifically must be refused STRUCTURALLY, by
	// Validate, so ValidateAST and the editor's save path reject them too — not
	// only the compiler.
	for name := range blockedCallNames {
		t.Run("denylisted/"+name, func(t *testing.T) {
			if _, ok := builtin.Index[name]; !ok {
				t.Fatalf("%q is denylisted but is no longer an expr builtin — resync blockedCallNames", name)
			}
			err := Call(name, Var("object.tags")).Validate()
			if err == nil {
				t.Fatalf("predicate builtin %q must be rejected by Validate", name)
			}
			if code := aerr.CodeOf(err); code != aerr.APERTURE_RULE_INVALID {
				t.Fatalf("code = %q, want APERTURE_RULE_INVALID", code)
			}
		})
	}

	// Side two: every curated name IS callable, so the pin fails if one is ever
	// dropped as well as if one is ever added. The three names expr reserves as
	// infix operators are the documented exception.
	reserved := make(map[string]bool, len(reservedInfixFunctions))
	for _, name := range reservedInfixFunctions {
		reserved[name] = true
	}
	callable := map[string]*Node{
		"lower":      Compare(OpEq, Call("lower", Var("object.s")), Lit("x")),
		"upper":      Compare(OpEq, Call("upper", Var("object.s")), Lit("x")),
		"len":        Compare(OpGe, Call("len", Var("object.tags")), Lit(1)),
		fnHasAll:     Call(fnHasAll, Var("object.tags"), List(Lit("a"))),
		fnHasAny:     Call(fnHasAny, Var("object.tags"), List(Lit("a"))),
		fnHasNone:    Call(fnHasNone, Var("object.tags"), List(Lit("a"))),
		fnSubsetOf:   Call(fnSubsetOf, Var("object.tags"), List(Lit("a"))),
		fnIsEmpty:    Call(fnIsEmpty, Var("object.tags")),
		fnIsNotEmpty: Call(fnIsNotEmpty, Var("object.tags")),
	}
	if len(callable)+len(reserved) != len(curatedFunctions) {
		t.Fatalf("probes = %d callable + %d reserved, curated names = %d — keep them in step",
			len(callable), len(reserved), len(curatedFunctions))
	}
	for _, name := range curatedFunctions {
		if reserved[name] {
			continue
		}
		t.Run("callable/"+name, func(t *testing.T) {
			node, ok := callable[name]
			if !ok {
				t.Fatalf("no probe for curated function %q", name)
			}
			if _, err := c.Compile(node); err != nil {
				t.Fatalf("curated function %q must be callable: %v", name, err)
			}
		})
	}

	// And the reserved three are pinned as NOT callable, so the gap is visible
	// rather than folded into the general "unknown function" bucket.
	for _, name := range reservedInfixFunctions {
		t.Run("reserved-infix/"+name, func(t *testing.T) {
			_, err := c.Compile(Compare(OpNe, Call(name, Var("object.s"), Lit("x")), Lit(nil)))
			if err == nil {
				t.Fatalf("%q now parses as a call — update reservedInfixFunctions and the docs", name)
			}
			if code := aerr.CodeOf(err); code != aerr.APERTURE_RULE_TYPE_ERROR {
				t.Fatalf("code = %q, want APERTURE_RULE_TYPE_ERROR", code)
			}
		})
	}
}

// TestBackingFunctionsArePure proves the registered collection backings mutate
// nothing and are deterministic over a fixed snapshot, which is the purity
// guarantee the rules package doc makes.
func TestBackingFunctionsArePure(t *testing.T) {
	c := NewCompiler()
	ctx := context.Background()
	md := collectionMetadata()
	tags, _ := md["tags"].([]any)
	before := append([]any(nil), tags...)

	rule := And(
		Compare(OpHasAll, Var("object.tags"), List(Lit("a"), Lit("b"))),
		Compare(OpHasNone, Var("object.tags"), List(Lit("z"))),
		Unary(OpIsNotEmpty, Var("object.tags")),
	)
	compiled, err := c.Compile(rule)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	for i := 0; i < 5; i++ {
		got, err := compiled.Eval(ctx, Input{Object: md})
		if err != nil || !got {
			t.Fatalf("run %d: got=%v err=%v", i, got, err)
		}
	}
	if len(tags) != len(before) {
		t.Fatalf("evaluation mutated the metadata slice: %v", tags)
	}
	for i := range before {
		if tags[i] != before[i] {
			t.Fatalf("evaluation mutated the metadata slice: %v", tags)
		}
	}
	if len(md) != len(collectionMetadata()) {
		t.Fatalf("evaluation mutated the metadata map: %v", md)
	}
}
