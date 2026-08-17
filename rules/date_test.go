package rules

import (
	"strings"
	"testing"

	"github.com/expr-lang/expr"

	aerr "github.com/frankbardon/aperture/errors"
)

// dateOperators is the closed list of operators that compare dates, mirroring
// collectionOperators in shape_test.go. Every test below drives off it, so an
// operator added to opSpecs without being added here is reported by
// TestDateOperatorRegistryIsClosed rather than silently untested.
var dateOperators = []string{
	OpBefore, OpAfter, OpOnOrBefore, OpOnOrAfter, OpBetween,
	OpSameDay, OpSameMonth, OpSameYear,
}

// TestDateOperatorRegistryIsClosed pins the registry from both directions: every
// name above is in opSpecs and renders through the date dispatcher, and every
// renderDate entry in opSpecs is named above.
func TestDateOperatorRegistryIsClosed(t *testing.T) {
	if len(dateOperators) != 8 {
		t.Fatalf("dateOperators has %d entries; the operator set is before/after/onOrBefore/"+
			"onOrAfter/between/sameDay/sameMonth/sameYear", len(dateOperators))
	}
	named := make(map[string]bool, len(dateOperators))
	for _, op := range dateOperators {
		named[op] = true
		spec, ok := opSpecs[op]
		if !ok {
			t.Errorf("date operator %q has no opSpecs entry", op)
			continue
		}
		if spec.kind != renderDate {
			t.Errorf("date operator %q renders as kind %d, want renderDate — a date operator "+
				"must never render to a native infix operator", op, spec.kind)
		}
	}
	for op, spec := range opSpecs {
		if spec.kind == renderDate && !named[op] {
			t.Errorf("opSpecs entry %q renders through the date dispatcher but is missing from "+
				"dateOperators, so nothing here covers it", op)
		}
	}
}

// TestDateOperatorShapes pins each operator's declared right-operand shape. The
// eight binary operators take a single element; between takes the two-bound
// list, which is the effort's one irreversible AST decision.
func TestDateOperatorShapes(t *testing.T) {
	want := map[string]rightShape{
		OpBefore:     rightElement,
		OpAfter:      rightElement,
		OpOnOrBefore: rightElement,
		OpOnOrAfter:  rightElement,
		OpBetween:    rightBounds,
		OpSameDay:    rightElement,
		OpSameMonth:  rightElement,
		OpSameYear:   rightElement,
	}
	for op, shape := range want {
		if got := opSpecs[op].shape; got != shape {
			t.Errorf("operator %q right-operand shape = %d, want %d", op, got, shape)
		}
	}
}

// TestDateOperatorRender is the render contract, byte for byte. Every operator
// compiles to a call to the guarded dispatcher and to nothing else — no infix
// operator appears anywhere in the output, which is what keeps a mistyped
// operand a false rather than an uncompilable rule.
func TestDateOperatorRender(t *testing.T) {
	cases := []struct {
		name string
		node *Node
		want string
	}{
		{"before a literal date",
			Compare(OpBefore, Var("object.hired_at"), Lit("2026-01-01")),
			`$date("before", __notes, "object.hired_at", object?.hired_at, "", "2026-01-01", "", nil)`},
		{"after a literal date",
			Compare(OpAfter, Var("object.hired_at"), Lit("2026-01-01T09:30:00Z")),
			`$date("after", __notes, "object.hired_at", object?.hired_at, "", "2026-01-01T09:30:00Z", "", nil)`},
		{"onOrBefore",
			Compare(OpOnOrBefore, Var("object.hired_at"), Lit("2026-01-01")),
			`$date("onOrBefore", __notes, "object.hired_at", object?.hired_at, "", "2026-01-01", "", nil)`},
		{"onOrAfter against another field",
			Compare(OpOnOrAfter, Var("object.hired_at"), Var("principal.start_at")),
			`$date("onOrAfter", __notes, "object.hired_at", object?.hired_at, "principal.start_at", principal?.start_at, "", nil)`},
		{"between carries both bounds",
			Between(Var("object.hired_at"), Lit("2026-01-01"), Lit("2026-12-31")),
			`$date("between", __notes, "object.hired_at", object?.hired_at, "", "2026-01-01", "", "2026-12-31")`},
		{"between with a variable upper bound names its path",
			Between(Var("object.hired_at"), Lit("2026-01-01"), Var("principal.review_at")),
			`$date("between", __notes, "object.hired_at", object?.hired_at, "", "2026-01-01", "principal.review_at", principal?.review_at)`},
		{"sameDay",
			Compare(OpSameDay, Var("object.hired_at"), Lit("2026-03-04")),
			`$date("sameDay", __notes, "object.hired_at", object?.hired_at, "", "2026-03-04", "", nil)`},
		{"sameMonth",
			Compare(OpSameMonth, Var("object.hired_at"), Lit("2026-03-04")),
			`$date("sameMonth", __notes, "object.hired_at", object?.hired_at, "", "2026-03-04", "", nil)`},
		{"sameYear over a nested path keeps optional chaining",
			Compare(OpSameYear, Var("object.audit.reviewed_at"), Lit("2026-03-04")),
			`$date("sameYear", __notes, "object.audit.reviewed_at", object?.audit?.reviewed_at, "", "2026-03-04", "", nil)`},
		{"nested under and/or like any other comparison",
			And(
				Compare(OpAfter, Var("object.hired_at"), Lit("2026-01-01")),
				Or(
					Compare(OpSameYear, Var("object.hired_at"), Lit("2026-06-01")),
					Compare(OpEq, Var("object.tier"), Lit("gold")),
				),
			),
			`($date("after", __notes, "object.hired_at", object?.hired_at, "", "2026-01-01", "", nil) && ` +
				`($date("sameYear", __notes, "object.hired_at", object?.hired_at, "", "2026-06-01", "", nil) || ` +
				`(object?.tier == "gold")))`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.node.Validate(); err != nil {
				t.Fatalf("validate: %v", err)
			}
			got, err := tc.node.Expr()
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			if got != tc.want {
				t.Fatalf("render mismatch\n got: %s\nwant: %s", got, tc.want)
			}
		})
	}
}

// TestDateOperatorsNeverRenderInfix is the same rule stated negatively, driven
// off the registry so it cannot fall behind: no date comparison may emit a
// comparison operator token. `time.Time < string` is a COMPILE error in expr, so
// a native render would take the whole rule down rather than degrade one
// comparison.
func TestDateOperatorsNeverRenderInfix(t *testing.T) {
	for _, op := range dateOperators {
		t.Run(op, func(t *testing.T) {
			n := dateNodeFor(op, Var("object.hired_at"))
			src, err := n.Expr()
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			if !strings.HasPrefix(src, fnDateOp+"(") {
				t.Fatalf("operator %q rendered as %q, want a %s call", op, src, fnDateOp)
			}
			for _, tok := range []string{" < ", " <= ", " > ", " >= ", " == ", " != ", " in "} {
				if strings.Contains(src, tok) {
					t.Fatalf("operator %q rendered the infix token %q: %s", op, tok, src)
				}
			}
		})
	}
}

// TestDateOperatorsCompile proves the rendered call is one the compiler accepts:
// the dispatcher is registered, its arity matches what the renderer emits, and
// its declared signature returns a bool, so a bare date comparison satisfies
// expr.AsBool as a whole rule. A signature/arity drift shows up here rather than
// at Check time.
func TestDateOperatorsCompile(t *testing.T) {
	c := NewCompiler()
	for _, op := range dateOperators {
		t.Run(op, func(t *testing.T) {
			for _, right := range []*Node{Lit("2026-01-01"), Var("principal.start_at")} {
				var n *Node
				if opSpecs[op].shape == rightBounds {
					n = Between(Var("object.hired_at"), Lit("2026-01-01"), right)
				} else {
					n = Compare(op, Var("object.hired_at"), right)
				}
				if _, err := c.Compile(n); err != nil {
					t.Fatalf("compile %q: %v", op, err)
				}
			}
		})
	}
}

// TestDateOperatorNamesAreCallable is an UPGRADE CANARY, not a test of Aperture
// behaviour.
//
// expr's parser matches some names as infix operators BEFORE it consults the
// function table, so registering a function under one of those names silently
// does nothing — which is why `contains`, `startsWith` and `endsWith` have never
// been reachable through a call node. All nine names below (the eight operators
// plus the dispatcher) are clean today; this test fails if an expr upgrade
// reserves one, before anyone builds a backing function on the assumption.
//
// Nothing rendered today depends on this: an operator's name reaches the
// expression only as a QUOTED STRING argument to the dispatcher, so a reserved
// name could not break a rule as things stand. The test exists because the
// failure mode is silent — a reserved name compiles as something else rather
// than erroring — and the next person to reach for a per-operator backing
// function should find out here rather than from a rule that quietly stops
// dispatching.
func TestDateOperatorNamesAreCallable(t *testing.T) {
	for _, name := range append(append([]string{}, dateOperators...), fnDateOp) {
		t.Run(name, func(t *testing.T) {
			if called, err := probeCallable(name); err != nil {
				t.Fatalf("registering %q as a function: %v", name, err)
			} else if !called {
				t.Fatalf("expr did not dispatch %s(...) to the registered function — the name is "+
					"reserved by the grammar (as `contains`/`startsWith`/`endsWith` are), so a "+
					"backing function under this name would be silently unreachable", name)
			}
		})
	}
}

// TestDateOperatorCallableProbeDetectsAReservedName is the canary's own control.
// Without it, TestDateOperatorNamesAreCallable would keep passing even if the
// probe stopped being able to observe a reserved name. `contains` is reserved
// today; if this fails, expr's grammar changed and the reserved-name notes in
// skills/rules-engine.md and docs/src/concepts/rules.md need revisiting.
func TestDateOperatorCallableProbeDetectsAReservedName(t *testing.T) {
	called, err := probeCallable("contains")
	if err == nil && called {
		t.Fatalf("expr now dispatches contains(...) to a registered function; the probe can no " +
			"longer prove a name is unreserved, and the reserved-name documentation is stale")
	}
}

// probeCallable registers name as a one-argument function in a bare environment
// matching the compiler's option set, then reports whether calling it actually
// reached the registration.
func probeCallable(name string) (bool, error) {
	called := false
	program, err := expr.Compile(name+"(object)",
		expr.Env(evalEnv{}),
		expr.AsBool(),
		expr.DisableAllBuiltins(),
		expr.Function(name, func(_ ...any) (any, error) {
			called = true
			return true, nil
		}, new(func(any) bool)),
	)
	if err != nil {
		return false, err
	}
	if _, err := expr.Run(program, Input{}.env(nil)); err != nil {
		return false, err
	}
	return called, nil
}

// TestBetweenASTIsByteStable pins the persisted JSON of the ternary shape. This
// is the decision that cannot be revisited: it is what stored rules will carry.
func TestBetweenASTIsByteStable(t *testing.T) {
	assertEditorJSONRoundTrips(t, `{"type":"compare","op":"between",`+
		`"left":{"type":"var","name":"object.hired_at"},`+
		`"right":{"type":"list","items":[`+
		`{"type":"literal","value":"2026-01-01"},`+
		`{"type":"literal","value":"2026-12-31"}]}}`)
}

// TestBetweenBuilderMatchesTheJSONShape proves the exported builder and the
// hand-written JSON above describe the same node — the editor writes the JSON,
// a library caller writes the builder, and they must not diverge.
func TestBetweenBuilderMatchesTheJSONShape(t *testing.T) {
	n := Between(Var("object.hired_at"), Lit("2026-01-01"), Lit("2026-12-31"))
	if err := n.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if n.Type != NodeCompare || n.Op != OpBetween {
		t.Fatalf("Between built a %s/%s node, want compare/between", n.Type, n.Op)
	}
	if n.Right == nil || n.Right.Type != NodeList || len(n.Right.Items) != 2 {
		t.Fatalf("Between must put a two-item list on the right; got %+v", n.Right)
	}
}

// TestDateOperandValidation is the operand contract. Every case is rejected with
// APERTURE_RULE_INVALID; the codes are asserted, never the message text.
func TestDateOperandValidation(t *testing.T) {
	cases := []struct {
		name string
		node *Node
	}{
		{"before with no right operand",
			&Node{Type: NodeCompare, Op: OpBefore, Left: Var("object.hired_at")}},
		{"after with no left operand",
			&Node{Type: NodeCompare, Op: OpAfter, Right: Lit("2026-01-01")}},
		{"sameDay with a list on the right",
			Compare(OpSameDay, Var("object.hired_at"), List(Lit("2026-01-01")))},
		{"between with no right operand",
			&Node{Type: NodeCompare, Op: OpBetween, Left: Var("object.hired_at")}},
		{"between with one bound",
			&Node{Type: NodeCompare, Op: OpBetween, Left: Var("object.hired_at"),
				Right: List(Lit("2026-01-01"))}},
		{"between with three bounds",
			&Node{Type: NodeCompare, Op: OpBetween, Left: Var("object.hired_at"),
				Right: List(Lit("2026-01-01"), Lit("2026-06-01"), Lit("2026-12-31"))}},
		{"between with an empty bounds list",
			&Node{Type: NodeCompare, Op: OpBetween, Left: Var("object.hired_at"),
				Right: List()}},
		{"between with a bare operand instead of a bounds list",
			&Node{Type: NodeCompare, Op: OpBetween, Left: Var("object.hired_at"),
				Right: Lit("2026-01-01")}},
		{"between with a variable instead of a bounds list",
			&Node{Type: NodeCompare, Op: OpBetween, Left: Var("object.hired_at"),
				Right: Var("object.window")}},
		{"a malformed bound is still validated",
			&Node{Type: NodeCompare, Op: OpBetween, Left: Var("object.hired_at"),
				Right: List(Lit("2026-01-01"), &Node{Type: NodeLiteral})}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.node.Validate()
			if err == nil {
				t.Fatalf("Validate accepted a node this case expects it to reject")
			}
			if got := aerr.CodeOf(err); got != aerr.APERTURE_RULE_INVALID {
				t.Fatalf("code = %q, want APERTURE_RULE_INVALID", got)
			}
		})
	}
}

// TestDateOperandsAcceptedShapes is the positive half: a literal and a variable
// are both legal date operands, on either side and in either bound.
func TestDateOperandsAcceptedShapes(t *testing.T) {
	nodes := []*Node{
		Compare(OpBefore, Var("object.hired_at"), Lit("2026-01-01")),
		Compare(OpBefore, Var("object.hired_at"), Var("principal.start_at")),
		Compare(OpAfter, Var("principal.start_at"), Var("object.hired_at")),
		Between(Var("object.hired_at"), Var("principal.start_at"), Var("principal.end_at")),
		Between(Var("object.hired_at"), Lit("2026-01-01"), Var("principal.end_at")),
	}
	for _, n := range nodes {
		if err := n.Validate(); err != nil {
			t.Fatalf("validate %s: %v", n.Op, err)
		}
	}
}

// TestNowIsNotAVariableRoot pins the decision that keeps relative dates out of
// the variable namespace. Reflective METHOD CALLS survive
// expr.DisableAllBuiltins, so if NOW were an exposed root then
// `NOW.AddDate(0, -3, 0)` would be a well-formed var path — a wall-clock read
// and a calendar walk reachable from any rule, neither of them deterministic nor
// clamped. Relative dates render literal arguments to a $-prefixed dispatcher
// instead; the root stays closed.
func TestNowIsNotAVariableRoot(t *testing.T) {
	for _, name := range []string{"NOW", "now", "TODAY", "today"} {
		if _, ok := allowedRoots[name]; ok {
			t.Fatalf("%q is an exposed variable root; a reflective method call on it would be a "+
				"well-formed var path", name)
		}
	}
	for _, path := range []string{"NOW", "NOW.AddDate", "now.Unix"} {
		err := Var(path).Validate()
		if err == nil {
			t.Fatalf("Validate accepted the variable %q", path)
		}
		if got := aerr.CodeOf(err); got != aerr.APERTURE_RULE_UNKNOWN_VARIABLE {
			t.Fatalf("Validate(%q) code = %q, want APERTURE_RULE_UNKNOWN_VARIABLE", path, got)
		}
	}
}

// TestDateDispatcherIsNotCallableFromARule pins the structural guard: `$date` is
// unreachable from a rule because varPath rejects the '$', so no call node can
// name it and no denylist entry is needed.
func TestDateDispatcherIsNotCallableFromARule(t *testing.T) {
	if varPath.MatchString(fnDateOp) {
		t.Fatalf("%q matches varPath, so a call node could name the dispatcher; the '$' prefix is "+
			"what makes the guard structural rather than denylisted", fnDateOp)
	}
	err := Call(fnDateOp, Var("object.hired_at")).Validate()
	if err == nil {
		t.Fatalf("Validate accepted a call node naming %s", fnDateOp)
	}
	if got := aerr.CodeOf(err); got != aerr.APERTURE_RULE_INVALID {
		t.Fatalf("code = %q, want APERTURE_RULE_INVALID", got)
	}
	if _, blocked := blockedCallNames[fnDateOp]; blocked {
		t.Fatalf("%s is in blockedCallNames; the '$' already forbids it structurally and a "+
			"denylist entry would imply the guard depends on the list", fnDateOp)
	}
}

// dateNodeFor builds the minimal well-formed node for a date operator, choosing
// the right-hand side from the operator's declared shape.
func dateNodeFor(op string, left *Node) *Node {
	if opSpecs[op].shape == rightBounds {
		return Between(left, Lit("2026-01-01"), Lit("2026-12-31"))
	}
	return Compare(op, left, Lit("2026-01-01"))
}
