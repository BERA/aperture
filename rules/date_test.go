package rules

import (
	"context"
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

// ---------------------------------------------------------------------------
// The runtime half: the deny-safe comparison policy (date.go).
// ---------------------------------------------------------------------------

// dateMetadata is the object fixture the evaluation tests read. It carries one
// value of each canonical granularity, the same instant spelled both ways, and
// one value of every shape a date operand can wrongly be — including a string
// that simply is not a date, which is the case no shape check can catch.
//
// There is deliberately no "missing" key: absence is a case, not a value.
func dateMetadata() map[string]any {
	return map[string]any{
		"hired_at":     "2026-03-04",
		"reviewed_at":  "2026-03-04T09:30:00Z",
		"midnight":     "2026-03-04T00:00:00Z",
		"other_day":    "2026-03-05",
		"other_month":  "2026-04-04",
		"other_year":   "2025-03-04",
		"garbage":      "04/03/2026",
		"offset":       "2026-03-04T09:30:00+05:00",
		"impossible":   "2026-02-30",
		"empty":        "",
		"count":        int64(7),
		"flag":         true,
		"tags":         []any{"2026-03-04"},
		"owner":        map[string]any{"since": "2026-03-04"},
		"nested_dates": map[string]any{"hired_at": "2026-03-04"},
	}
}

// datePrincipal is the principal-side fixture, so a date comparison between two
// VARIABLES (rather than against a literal) is exercised too — that is the shape
// a real cutoff rule takes.
func datePrincipal() map[string]any {
	return map[string]any{
		"id":       "user:alice",
		"start_at": "2026-03-04",
		"end_at":   "2026-12-31",
		"garbage":  "not a date",
	}
}

// evalDate compiles and evaluates n against the date fixture, returning the
// result and the notes recorded. It fails the test if evaluation RAISES: the
// whole policy is that a date operand never can.
func evalDate(t *testing.T, n *Node) (bool, []Note) {
	t.Helper()
	compiled, err := NewCompiler().Compile(n)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	got, notes, err := compiled.EvalWithNotes(context.Background(),
		Input{Object: dateMetadata(), Principal: datePrincipal()})
	if err != nil {
		t.Fatalf("a date comparison must never fail evaluation (source %s): %v", compiled.Source(), err)
	}
	return got, notes
}

// TestDateOperatorTablesAgree keeps the runtime policy (dateOps, date.go) and the
// validate/render registry (opSpecs, ast.go) in lockstep, exactly as
// TestCollectionOperatorTablesAgree does for the collection half: an operator can
// never gain a render without a runtime policy, nor a policy without a render.
func TestDateOperatorTablesAgree(t *testing.T) {
	for op := range dateOps {
		spec, ok := opSpecs[op]
		if !ok {
			t.Errorf("dateOps has %q but opSpecs does not", op)
			continue
		}
		if spec.kind != renderDate {
			t.Errorf("dateOps has %q but opSpecs renders it as kind %d, not renderDate", op, spec.kind)
		}
	}
	for _, op := range dateOperators {
		if _, ok := dateOps[op]; !ok {
			t.Errorf("date operator %q has no runtime policy in dateOps", op)
		}
	}
	if len(dateOps) != len(dateOperators) {
		t.Errorf("dateOps has %d entries, want the %d date operators",
			len(dateOps), len(dateOperators))
	}
	// The ternary flag is the runtime twin of the rightBounds shape: exactly the
	// operators whose AST carries two bounds may read the dispatcher's third pair.
	for op, spec := range dateOps {
		if want := opSpecs[op].shape == rightBounds; spec.ternary != want {
			t.Errorf("dateOps[%q].ternary = %v, but opSpecs shape is rightBounds = %v; the "+
				"runtime arity and the AST shape must agree", op, spec.ternary, want)
		}
	}
}

// TestDateOperatorTruthTable is every operator's true AND false case over
// well-formed operands. Each case is a comparison against the fixture's
// 2026-03-04, so the operators are readable against one another.
func TestDateOperatorTruthTable(t *testing.T) {
	cases := []struct {
		name string
		node *Node
		want bool
	}{
		{"before an earlier date is false",
			Compare(OpBefore, Var("object.hired_at"), Lit("2026-01-01")), false},
		{"before a later date is true",
			Compare(OpBefore, Var("object.hired_at"), Lit("2026-12-31")), true},
		{"before is STRICT at the boundary",
			Compare(OpBefore, Var("object.hired_at"), Lit("2026-03-04")), false},

		{"after a later date is false",
			Compare(OpAfter, Var("object.hired_at"), Lit("2026-12-31")), false},
		{"after an earlier date is true",
			Compare(OpAfter, Var("object.hired_at"), Lit("2026-01-01")), true},
		{"after is STRICT at the boundary",
			Compare(OpAfter, Var("object.hired_at"), Lit("2026-03-04")), false},

		{"onOrBefore an earlier date is false",
			Compare(OpOnOrBefore, Var("object.hired_at"), Lit("2026-01-01")), false},
		{"onOrBefore a later date is true",
			Compare(OpOnOrBefore, Var("object.hired_at"), Lit("2026-12-31")), true},
		{"onOrBefore is INCLUSIVE at the boundary",
			Compare(OpOnOrBefore, Var("object.hired_at"), Lit("2026-03-04")), true},

		{"onOrAfter a later date is false",
			Compare(OpOnOrAfter, Var("object.hired_at"), Lit("2026-12-31")), false},
		{"onOrAfter an earlier date is true",
			Compare(OpOnOrAfter, Var("object.hired_at"), Lit("2026-01-01")), true},
		{"onOrAfter is INCLUSIVE at the boundary",
			Compare(OpOnOrAfter, Var("object.hired_at"), Lit("2026-03-04")), true},

		{"between a range that contains the value is true",
			Between(Var("object.hired_at"), Lit("2026-01-01"), Lit("2026-12-31")), true},
		{"between a range entirely after the value is false",
			Between(Var("object.hired_at"), Lit("2026-06-01"), Lit("2026-12-31")), false},
		{"between a range entirely before the value is false",
			Between(Var("object.hired_at"), Lit("2025-01-01"), Lit("2025-12-31")), false},

		{"sameDay on the same day is true",
			Compare(OpSameDay, Var("object.hired_at"), Lit("2026-03-04")), true},
		{"sameDay on the next day is false",
			Compare(OpSameDay, Var("object.hired_at"), Var("object.other_day")), false},
		{"sameMonth in the same month is true",
			Compare(OpSameMonth, Var("object.hired_at"), Var("object.other_day")), true},
		{"sameMonth in another month is false",
			Compare(OpSameMonth, Var("object.hired_at"), Var("object.other_month")), false},
		{"sameYear in the same year is true",
			Compare(OpSameYear, Var("object.hired_at"), Var("object.other_month")), true},
		{"sameYear in another year is false",
			Compare(OpSameYear, Var("object.hired_at"), Var("object.other_year")), false},

		// A variable on BOTH sides — the shape a real cutoff rule takes.
		{"variable against variable, both well-formed",
			Compare(OpOnOrAfter, Var("object.hired_at"), Var("principal.start_at")), true},
		{"between two variable bounds",
			Between(Var("object.hired_at"), Var("principal.start_at"), Var("principal.end_at")), true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, notes := evalDate(t, tc.node)
			if got != tc.want {
				t.Fatalf("eval = %v, want %v", got, tc.want)
			}
			if len(notes) != 0 {
				t.Fatalf("well-formed operands must record no notes, got %+v", notes)
			}
		})
	}
}

// TestSameBucketOperatorsAreCalendarBucketsNotDistance pins what sameMonth and
// sameYear actually ask. They are EQUALITY over a calendar bucket, not a distance
// test: two dates a day apart across a month boundary are NOT in the same month,
// and two dates eleven months apart within one year ARE in the same year.
func TestSameBucketOperatorsAreCalendarBucketsNotDistance(t *testing.T) {
	cases := []struct {
		name       string
		op         string
		left, righ string
		want       bool
	}{
		{"a day apart across a month boundary is not the same month",
			OpSameMonth, "2026-03-31", "2026-04-01", false},
		{"a day apart across a year boundary is not the same year",
			OpSameYear, "2026-12-31", "2027-01-01", false},
		{"eleven months apart inside one year is the same year",
			OpSameYear, "2026-01-01", "2026-12-31", true},
		{"the same month number in different years is not the same month",
			OpSameMonth, "2026-03-04", "2025-03-04", false},
		{"the same day number in different months is not the same day",
			OpSameDay, "2026-03-04", "2026-04-04", false},
		{"the last instant of a day is still that day",
			OpSameDay, "2026-03-04T23:59:59Z", "2026-03-04", true},
		{"midnight is the first instant of its day, not the last of the previous",
			OpSameDay, "2026-03-04T00:00:00Z", "2026-03-03", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := evalDateOp(tc.op, nil, "", tc.left, "", tc.righ, "", nil)
			if got != tc.want {
				t.Fatalf("%s(%s, %s) = %v, want %v", tc.op, tc.left, tc.righ, got, tc.want)
			}
		})
	}
}

// TestDateGranularityNeverAffectsOrdering is the property string comparison gets
// wrong: the date-only form is a strict PREFIX of the timestamp form, so
// "2026-03-04" sorts before "2026-03-04T00:00:00Z" as text even though the two
// name the same instant. Every operator here is decided over instants.
func TestDateGranularityNeverAffectsOrdering(t *testing.T) {
	const day = "2026-03-04"
	const midnight = "2026-03-04T00:00:00Z"

	// The premise: as TEXT these are ordered, which is exactly the bug.
	if !(day < midnight) {
		t.Fatalf("fixture assumption broken: %q must sort before %q as text", day, midnight)
	}

	cases := []struct {
		name        string
		op          string
		left, right string
		want        bool
	}{
		{"the same instant is not before itself", OpBefore, day, midnight, false},
		{"the same instant is not after itself", OpAfter, day, midnight, false},
		{"the same instant is on or before itself", OpOnOrBefore, day, midnight, true},
		{"the same instant is on or after itself", OpOnOrAfter, day, midnight, true},
		{"flipped: the timestamp is not before the day", OpBefore, midnight, day, false},
		{"flipped: the timestamp is on or before the day", OpOnOrBefore, midnight, day, true},
		{"the same instant is the same day", OpSameDay, day, midnight, true},
		{"the same instant is the same month", OpSameMonth, day, midnight, true},
		{"the same instant is the same year", OpSameYear, day, midnight, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := evalDateOp(tc.op, nil, "", tc.left, "", tc.right, "", nil); got != tc.want {
				t.Fatalf("%s(%q, %q) = %v, want %v", tc.op, tc.left, tc.right, got, tc.want)
			}
		})
	}

	// The same property through the whole compile-and-evaluate path, so the
	// render cannot reintroduce a text comparison.
	got, notes := evalDate(t, And(
		Compare(OpOnOrBefore, Var("object.hired_at"), Var("object.midnight")),
		Compare(OpOnOrAfter, Var("object.hired_at"), Var("object.midnight")),
		Not(Compare(OpBefore, Var("object.hired_at"), Var("object.midnight"))),
	))
	if !got {
		t.Fatalf("a date and the timestamp naming the same instant must compare equal; notes %+v", notes)
	}
}

// TestBetweenIsInclusiveAtBothBounds tests the two boundaries EXPLICITLY. This is
// where off-by-one access bugs live: an exclusive upper bound silently denies
// every object created on the last day of the window.
func TestBetweenIsInclusiveAtBothBounds(t *testing.T) {
	const lo = "2026-01-01"
	const hi = "2026-12-31"
	cases := []struct {
		name  string
		value string
		want  bool
	}{
		{"exactly the lower bound matches", lo, true},
		{"exactly the upper bound matches", hi, true},
		{"one day below the lower bound does not", "2025-12-31", false},
		{"one day above the upper bound does not", "2027-01-01", false},
		{"one second below the lower bound does not", "2025-12-31T23:59:59Z", false},
		{"one second above the upper bound does not", "2026-12-31T00:00:01Z", false},
		{"the lower bound at a coarser granularity still matches", "2026-01-01T00:00:00Z", true},
		{"inside the range matches", "2026-06-15", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := evalDateOp(OpBetween, nil, "", tc.value, "", lo, "", hi); got != tc.want {
				t.Fatalf("between(%q, [%q, %q]) = %v, want %v", tc.value, lo, hi, got, tc.want)
			}
		})
	}

	// A degenerate one-instant range is the inclusivity property at its limit:
	// lo == hi matches exactly that instant and nothing else.
	if !evalDateOp(OpBetween, nil, "", "2026-03-04", "", "2026-03-04", "", "2026-03-04") {
		t.Fatal("between [x, x] must match x — both ends are inclusive")
	}
}

// TestBetweenInvertedBoundsMatchNothingAndAreNoted pins the authoring-error case.
// Bounds are NEVER reordered: an inverted range matches nothing, which is what
// the author wrote, and the note is what stops that being indistinguishable from
// a rule nothing happens to satisfy.
func TestBetweenInvertedBoundsMatchNothingAndAreNoted(t *testing.T) {
	node := Between(Var("object.hired_at"), Lit("2026-12-31"), Lit("2026-01-01"))
	got, notes := evalDate(t, node)
	if got {
		t.Fatal("an inverted between range must match nothing")
	}
	want := Note{Kind: NoteDateBoundsInverted, Op: OpBetween, Path: "object.hired_at",
		Expected: "lower bound on or before upper bound"}
	if len(notes) != 1 || notes[0] != want {
		t.Fatalf("notes = %+v, want exactly [%+v]", notes, want)
	}
	const wantMsg = "object.hired_at: between bounds are inverted; the lower bound is after " +
		"the upper bound, so nothing can match"
	if msg := notes[0].String(); msg != wantMsg {
		t.Fatalf("note message = %q, want %q", msg, wantMsg)
	}

	// Not even a value that sits between the two bounds READ THE OTHER WAY
	// matches — the range is empty, it is not silently flipped.
	for _, value := range []string{"2026-01-01", "2026-06-15", "2026-12-31"} {
		if evalDateOp(OpBetween, nil, "", value, "", "2026-12-31", "", "2026-01-01") {
			t.Fatalf("inverted bounds matched %q; bounds must never be reordered", value)
		}
	}
}

// TestDateOperandsAreDenySafe is the core of the runtime policy, driven off the
// operator registry so it cannot fall behind: EVERY date operator, over an
// operand that is absent, the wrong shape, or an unparseable string, evaluates to
// FALSE and records a note. None of them raises.
func TestDateOperandsAreDenySafe(t *testing.T) {
	// Each case names a metadata field (or the absent one) and the note it must
	// produce for it.
	cases := []struct {
		name  string
		field string
		kind  NoteKind
		shape string // for a shape mismatch
	}{
		{"a number", "count", NoteShapeMismatch, shapeNumber},
		{"a bool", "flag", NoteShapeMismatch, shapeBool},
		{"an array", "tags", NoteShapeMismatch, shapeArray},
		{"an object", "owner", NoteShapeMismatch, shapeObject},
		{"an absent field", "missing", NoteShapeMismatch, shapeAbsent},
		{"an absent nested field", "owner.missing", NoteShapeMismatch, shapeAbsent},
		{"a string that is not a date", "garbage", NoteDateInvalid, ""},
		{"a date with a non-UTC offset", "offset", NoteDateInvalid, ""},
		{"a date that does not exist", "impossible", NoteDateInvalid, ""},
		{"an empty string", "empty", NoteDateInvalid, ""},
	}

	for _, tc := range cases {
		for _, op := range dateOperators {
			path := "object." + tc.field
			t.Run(op+" over "+tc.name, func(t *testing.T) {
				// LEFT operand malformed.
				got, notes := evalDate(t, dateNodeFor(op, Var(path)))
				if got {
					t.Fatalf("%s over %s must be false (deny-safe), got true", op, tc.name)
				}
				want := Note{Kind: tc.kind, Op: op, Path: path, Actual: tc.shape}
				if tc.kind == NoteShapeMismatch {
					want.Expected = shapeDate
				} else {
					want.Expected = dateForms
				}
				if len(notes) != 1 || notes[0] != want {
					t.Fatalf("notes = %+v, want exactly [%+v]", notes, want)
				}
			})

			t.Run(op+" with "+tc.name+" on the right", func(t *testing.T) {
				// RIGHT operand malformed. between's LOWER bound is the right
				// operand, so one table covers the binary and ternary shapes.
				var node *Node
				if opSpecs[op].shape == rightBounds {
					node = Between(Var("object.hired_at"), Var(path), Lit("2026-12-31"))
				} else {
					node = Compare(op, Var("object.hired_at"), Var(path))
				}
				got, notes := evalDate(t, node)
				if got {
					t.Fatalf("%s against %s must be false (deny-safe), got true", op, tc.name)
				}
				if len(notes) != 1 || notes[0].Path != path || notes[0].Kind != tc.kind {
					t.Fatalf("notes = %+v, want one %s note naming %s", notes, tc.kind, path)
				}
			})
		}
	}
}

// TestBetweenUpperBoundIsDenySafeToo covers the third operand, which only between
// reads: a malformed upper bound denies and is noted like any other.
func TestBetweenUpperBoundIsDenySafeToo(t *testing.T) {
	got, notes := evalDate(t,
		Between(Var("object.hired_at"), Lit("2026-01-01"), Var("object.garbage")))
	if got {
		t.Fatal("a malformed upper bound must deny")
	}
	want := Note{Kind: NoteDateInvalid, Op: OpBetween, Path: "object.garbage", Expected: dateForms}
	if len(notes) != 1 || notes[0] != want {
		t.Fatalf("notes = %+v, want exactly [%+v]", notes, want)
	}
}

// TestDateEvaluationReportsEveryBadOperand pins that one evaluation reports every
// problem it found rather than only the first — the same behaviour
// evalCollectionOp has. A rule author fixing two malformed operands should not
// have to run Explain twice.
func TestDateEvaluationReportsEveryBadOperand(t *testing.T) {
	got, notes := evalDate(t,
		Between(Var("object.garbage"), Var("object.count"), Var("principal.garbage")))
	if got {
		t.Fatal("three malformed operands must deny")
	}
	if len(notes) != 3 {
		t.Fatalf("notes = %+v, want one per malformed operand (3)", notes)
	}
	byPath := map[string]Note{}
	for _, n := range notes {
		byPath[n.Path] = n
	}
	for path, kind := range map[string]NoteKind{
		"object.garbage":    NoteDateInvalid,
		"object.count":      NoteShapeMismatch,
		"principal.garbage": NoteDateInvalid,
	} {
		n, ok := byPath[path]
		if !ok {
			t.Fatalf("no note names %s: %+v", path, notes)
		}
		if n.Kind != kind {
			t.Errorf("note for %s has kind %q, want %q", path, n.Kind, kind)
		}
	}
}

// TestDateNoteNamesThePathNotTheValue is the leak guard. A date can be personal
// data — a birth date, a termination date — and Explain output crosses account
// boundaries, so a note must name the FIELD and the REASON and nothing else.
func TestDateNoteNamesThePathNotTheValue(t *testing.T) {
	const secret = "1974-11-02" // a birth date: exactly what must never appear
	md := map[string]any{"birth_date": secret + " (approx)"}
	compiled, err := NewCompiler().Compile(
		Compare(OpBefore, Var("object.birth_date"), Lit("2026-01-01")))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	got, notes, err := compiled.EvalWithNotes(context.Background(), Input{Object: md})
	if err != nil || got {
		t.Fatalf("eval = %v, err = %v; want a deny with no error", got, err)
	}
	if len(notes) != 1 {
		t.Fatalf("notes = %+v, want exactly one", notes)
	}
	n := notes[0]
	if n.Path != "object.birth_date" {
		t.Fatalf("note must name the path, got %q", n.Path)
	}
	for field, value := range map[string]string{
		"Path": n.Path, "Expected": n.Expected, "Actual": n.Actual,
		"Op": n.Op, "Message": n.String(),
	} {
		if strings.Contains(value, secret) {
			t.Fatalf("note field %s leaked the value: %q", field, value)
		}
	}
	const wantMsg = "object.birth_date: not a canonical date; before expects " +
		"2006-01-02 or 2006-01-02T15:04:05Z"
	if msg := n.String(); msg != wantMsg {
		t.Fatalf("note message = %q, want %q", msg, wantMsg)
	}
}

// TestDateNoteNamesAnExpressionOperand covers the operand that has no path: a
// literal. A note always names something, so it reads "(expression)" rather than
// an empty field — and still never the value.
func TestDateNoteNamesAnExpressionOperand(t *testing.T) {
	got, notes := evalDate(t, Compare(OpBefore, Var("object.hired_at"), Lit("not a date")))
	if got {
		t.Fatal("a malformed literal bound must deny")
	}
	if len(notes) != 1 || notes[0].Path != "" {
		t.Fatalf("notes = %+v, want one note with an empty path", notes)
	}
	if msg := notes[0].String(); !strings.HasPrefix(msg, anonymousOperand+":") {
		t.Fatalf("note message = %q, want it to name %s", msg, anonymousOperand)
	}
	if strings.Contains(notes[0].String(), "not a date") {
		t.Fatalf("note leaked the literal: %q", notes[0].String())
	}
}

// TestMalformedDateDoesNotAffectAnotherComparison is the whole point of
// deny-safety stated as a property: one bad field denies its own comparison and
// leaves every other comparison in the same rule — and every other Check — alone.
func TestMalformedDateDoesNotAffectAnotherComparison(t *testing.T) {
	// The good comparison is under an OR with the bad one, so the rule as a whole
	// still selects. If a malformed operand raised, or poisoned the evaluation,
	// this would be false or an error.
	got, notes := evalDate(t, Or(
		Compare(OpBefore, Var("object.garbage"), Lit("2026-12-31")),
		Compare(OpBefore, Var("object.hired_at"), Lit("2026-12-31")),
	))
	if !got {
		t.Fatalf("a malformed operand must not affect the other branch; notes %+v", notes)
	}
	if len(notes) != 1 || notes[0].Path != "object.garbage" {
		t.Fatalf("notes = %+v, want exactly one naming object.garbage", notes)
	}

	// The same rule against a DIFFERENT object, whose field is well-formed,
	// decides on its own data: nothing about the first evaluation persists.
	compiled, err := NewCompiler().Compile(
		Compare(OpBefore, Var("object.hired_at"), Lit("2026-12-31")))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	ctx := context.Background()
	bad, _, err := compiled.EvalWithNotes(ctx, Input{Object: map[string]any{"hired_at": "nope"}})
	if err != nil || bad {
		t.Fatalf("malformed object: eval = %v, err = %v; want false with no error", bad, err)
	}
	good, notes2, err := compiled.EvalWithNotes(ctx, Input{Object: map[string]any{"hired_at": "2026-03-04"}})
	if err != nil || !good {
		t.Fatalf("well-formed object: eval = %v, err = %v, notes %+v; want true", good, err, notes2)
	}
}

// TestDateHotPathCollectsNothing pins the zero-cost property for the date
// dispatcher: without a collector in the context — which is every Check and every
// Enumerate — evaluation records nothing and decides IDENTICALLY. Notes are
// diagnostic; they never influence a verdict.
func TestDateHotPathCollectsNothing(t *testing.T) {
	nodes := []*Node{
		Compare(OpBefore, Var("object.hired_at"), Lit("2026-12-31")),          // true
		Compare(OpBefore, Var("object.garbage"), Lit("2026-12-31")),           // deny-safe false
		Compare(OpSameDay, Var("object.count"), Lit("2026-03-04")),            // shape mismatch
		Between(Var("object.hired_at"), Lit("2026-12-31"), Lit("2026-01-01")), // inverted bounds
		Between(Var("object.hired_at"), Lit("2026-01-01"), Lit("2026-12-31")), // true
	}
	for _, n := range nodes {
		compiled, err := NewCompiler().Compile(n)
		if err != nil {
			t.Fatalf("compile: %v", err)
		}
		in := Input{Object: dateMetadata(), Principal: datePrincipal()}

		// With a collector: the result plus whatever notes the policy recorded.
		noted, _, err := compiled.EvalWithNotes(context.Background(), in)
		if err != nil {
			t.Fatalf("eval with notes: %v", err)
		}
		// Without one: the sink is nil and nothing is recorded.
		bare, err := compiled.Eval(context.Background(), in)
		if err != nil {
			t.Fatalf("eval without notes: %v", err)
		}
		if noted != bare {
			t.Fatalf("%s decided %v with a collector and %v without; notes must never "+
				"influence a decision", compiled.Source(), noted, bare)
		}
		if NoteCollectorFrom(context.Background()) != nil {
			t.Fatal("a bare context must carry no collector")
		}
	}
}

// TestDateDispatcherRejectsAnUnknownOperator covers the unreachable branch: an
// operator name that is not in dateOps denies rather than panicking. It cannot
// arrive through a validated AST, but the dispatcher is the last line and must
// hold anyway.
func TestDateDispatcherRejectsAnUnknownOperator(t *testing.T) {
	sink := &NoteCollector{}
	if evalDateOp("sameWeek", sink, "object.hired_at", "2026-03-04", "", "2026-03-04", "", nil) {
		t.Fatal("an unregistered date operator must be false")
	}
	if sink.Len() != 0 {
		t.Fatalf("an unregistered operator should record nothing, got %+v", sink.Notes())
	}
}

// TestDateOperandsNeverConvertTimezones pins the UTC rule at the operator level.
// An explicit offset is REJECTED at parse rather than converted, so a value a
// host wrote as midnight in another zone can never silently answer a same-day or
// same-year question about a different calendar day.
func TestDateOperandsNeverConvertTimezones(t *testing.T) {
	// 2026-01-01T00:00:00+05:00 is 2025-12-31T19:00:00Z. If it were converted,
	// sameYear against 2025 would be TRUE; if it were read as UTC, sameYear
	// against 2026 would be true. It is neither: it does not parse.
	for _, year := range []string{"2025-06-01", "2026-06-01"} {
		if evalDateOp(OpSameYear, nil, "", "2026-01-01T00:00:00+05:00", "", year, "", nil) {
			t.Fatalf("an offset-bearing value matched sameYear against %s; offsets must be "+
				"rejected, never converted", year)
		}
	}
	// An offset-FREE timestamp is read as UTC and does compare.
	if !evalDateOp(OpSameYear, nil, "", "2026-01-01T00:00:00", "", "2026-06-01", "", nil) {
		t.Fatal("an offset-free timestamp must be read as UTC and compare normally")
	}
}

// TestDatePolicyAllocationBudget is the hot-path guard. This effort put PARSING
// on the comparison path, and the previous effort's regression — a per-decision
// copy of the collection operand — passed every correctness test and was caught
// only by throughput. So the property is asserted directly rather than inferred
// from a benchmark.
//
// Deciding a date comparison allocates NOTHING: not on the matching path, not on
// the wrong-shape or absent deny paths, and not on the inverted-bounds path. Note
// construction is skipped outright when the sink is nil — which is every Check
// and every Enumerate — and provider.DateValueOf builds no coded error the way
// the load path's ParseDateValue does.
//
// THE ONE EXCEPTION, budgeted rather than hidden: a string of exactly the
// date-only WIDTH that is not a date ("04/03/2026") reaches time.Parse, whose
// failure path constructs a *time.ParseError. provider classifies that error and
// discards it — it is never wrapped, because its Error() quotes the input — but
// the allocation has already happened inside the standard library. It is bounded
// (it does not scale with anything), it is on a deny path only, and removing it
// would mean pre-validating the layout in provider rather than in time.Parse. The
// budget exists so the cost stays a known constant instead of drifting.
func TestDatePolicyAllocationBudget(t *testing.T) {
	cases := []struct {
		name               string
		op                 string
		left, right, upper any
		budget             float64
	}{
		{"a matching binary comparison", OpBefore, "2026-03-04", "2026-12-31T23:59:59Z", nil, 0},
		{"a mixed-granularity comparison", OpOnOrAfter, "2026-03-04", "2026-03-04T00:00:00Z", nil, 0},
		{"a calendar-bucket comparison", OpSameMonth, "2026-03-04T09:30:00Z", "2026-03-31", nil, 0},
		{"a ternary comparison", OpBetween, "2026-03-04T09:30:00Z", "2026-01-01", "2026-12-31", 0},
		{"a wrong-shaped operand (deny-safe)", OpBefore, int64(20260304), "2026-12-31", nil, 0},
		{"an absent operand (deny-safe)", OpBefore, nil, "2026-12-31", nil, 0},
		{"a bool operand (deny-safe)", OpSameDay, true, "2026-12-31", nil, 0},
		{"an empty-string operand (deny-safe)", OpBefore, "", "2026-12-31", nil, 0},
		{"a wrong-width string (deny-safe)", OpBefore, "nope", "2026-12-31", nil, 0},
		{"inverted bounds (deny-safe)", OpBetween, "2026-03-04", "2026-12-31", "2026-01-01", 0},
		// The budgeted case: date-width, but not a date.
		{"a date-width non-date (deny-safe)", OpBefore, "04/03/2026", "2026-12-31", nil, 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := testing.AllocsPerRun(200, func() {
				evalDateOp(tc.op, nil, "object.hired_at", tc.left, "", tc.right, "", tc.upper)
			})
			if got > tc.budget {
				t.Fatalf("deciding %s allocated %v times per evaluation (budget %v); with no "+
					"collector installed the date policy constructs no note and no coded error",
					tc.op, got, tc.budget)
			}
		})
	}
}
