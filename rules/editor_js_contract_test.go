package rules

import (
	stderrors "errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	aerr "github.com/frankbardon/aperture/errors"
)

// This file closes the Go/JS drift hole that editor_contract_test.go cannot.
//
// editor_contract_test.go pins the JSON SHAPES the editor is expected to
// produce, but it never reads the JS. Adding an operator to rules/ast.go alone —
// or changing one operand rule on one side — leaves it green, and because CI is
// NODE-FREE (`node internal/server/static/js/rules-serializer.test.js` is a
// manual development aid that no pipeline runs) nothing else catches it.
//
// The tests below READ internal/server/static/js/rules-serializer.js FROM DISK
// and compare its machine-readable tables against the Go originals, entry for
// entry:
//
//	JS `OP_SPECS`       <-> Go `opSpecs`          (ast.go) — membership AND shape
//	JS `RIGHT`          <-> Go `rightShape`       (ast.go)
//	JS `TYPES`          <-> Go `NodeType` consts  (ast.go)
//	JS `ROOTS`          <-> Go `allowedRoots`     (ast.go)
//	JS `BLOCKED_CALLS`  <-> Go `blockedCallNames` (ast.go)
//	JS `FUNCTIONS`      <-> the registrations in `defaultFunctions` (compiler.go)
//
// They are modelled on TestCollectionOperatorTablesAgree (shape_test.go), which
// does the same job for the two GO tables. The only difference is that one side
// of the comparison has to be scanned out of a JavaScript file, so this file
// carries a small hand-rolled scanner — no JS-parser dependency, no new module,
// and node is never invoked.
//
// A missing or unreadable serializer FAILS; it never skips. A silent skip would
// recreate the exact gap these tests exist to close.

// serializerPath locates the JS serializer relative to this package directory
// (`go test` runs with the package directory as its working directory).
const serializerPath = "../internal/server/static/js/rules-serializer.js"

// compilerPath is this package's own compiler.go, read to discover which
// functions defaultFunctions actually registers. Reading the source rather than
// hand-listing the names is what makes the FUNCTIONS comparison notice a
// function added to Go and forgotten in JS.
const compilerPath = "compiler.go"

// astPath is this package's own ast.go, read to discover the NodeType consts.
const astPath = "ast.go"

// jsShapeNames maps each Go rightShape to the JS `RIGHT` key that must mirror
// it. A rightShape added to ast.go without a JS twin has no entry here, and
// TestEditorOperatorTablesAgree reports it against the operator that uses it.
var jsShapeNames = map[rightShape]string{
	rightAny:        "ANY",
	rightCollection: "COLLECTION",
	rightElement:    "ELEMENT",
	rightNone:       "NONE",
	rightBounds:     "BOUNDS",
}

// goBackingFuncConsts resolves the fn* constants compiler.go registers its
// backing functions under. defaultFunctions names them through constants rather
// than literals, so the scanner needs this to read the value; an unrecognised
// fn* identifier is reported rather than ignored.
var goBackingFuncConsts = map[string]string{
	"fnHasAll":       fnHasAll,
	"fnHasAny":       fnHasAny,
	"fnHasNone":      fnHasNone,
	"fnSubsetOf":     fnSubsetOf,
	"fnIsEmpty":      fnIsEmpty,
	"fnIsNotEmpty":   fnIsNotEmpty,
	"fnCollectionOp": fnCollectionOp,
	"fnDateOp":       fnDateOp,
	"fnRelativeDate": fnRelativeDate,
}

// TestEditorOperatorTablesAgree is the drift gate proper: every operator in Go's
// opSpecs must exist in the JS OP_SPECS with the SAME right-operand shape, and
// vice versa. Adding an operator to one side only fails here, naming the
// operator and the side it is missing from.
func TestEditorOperatorTablesAgree(t *testing.T) {
	js := readSerializer(t)

	// RIGHT first: the shape vocabulary has to line up before a per-operator
	// shape comparison means anything.
	right := jsObjectEntries(t, jsConstBody(t, js, "RIGHT"))
	for shape, key := range jsShapeNames {
		val, ok := right[key]
		if !ok {
			t.Errorf("Go rightShape %d maps to JS RIGHT.%s, but %s is not a key of RIGHT in %s",
				shape, key, key, serializerPath)
			continue
		}
		if want := strings.ToLower(key); jsUnquote(val) != want {
			t.Errorf("JS RIGHT.%s = %s, want %q (the value must be the lowercased key)", key, val, want)
		}
	}
	if len(right) != len(jsShapeNames) {
		t.Errorf("JS RIGHT has %d members %v, Go rightShape has %d — one side gained a shape",
			len(right), sortedKeys(right), len(jsShapeNames))
	}

	specs := jsOpSpecs(t, js)

	// Go -> JS.
	for op, spec := range opSpecs {
		jsShape, ok := specs[op]
		if !ok {
			t.Errorf("operator %q is in Go opSpecs (rules/ast.go) but MISSING from JS OP_SPECS (%s): "+
				"add `%s: { right: RIGHT.%s },` to the JS table (and a case to rules-serializer.test.js)",
				op, serializerPath, op, jsShapeNames[spec.shape])
			continue
		}
		wantShape, known := jsShapeNames[spec.shape]
		if !known {
			t.Errorf("operator %q uses Go rightShape %d, which has no JS RIGHT name: "+
				"add it to jsShapeNames and to RIGHT in %s", op, spec.shape, serializerPath)
			continue
		}
		if jsShape != wantShape {
			t.Errorf("operator %q right-operand shape differs: Go opSpecs says RIGHT.%s, JS OP_SPECS says RIGHT.%s",
				op, wantShape, jsShape)
		}
	}

	// JS -> Go.
	for op, jsShape := range specs {
		if _, ok := opSpecs[op]; !ok {
			t.Errorf("operator %q is in JS OP_SPECS (%s, RIGHT.%s) but MISSING from Go opSpecs (rules/ast.go): "+
				"add an Op* const and an opSpecs entry, or drop it from the JS table",
				op, serializerPath, jsShape)
		}
	}

	if len(specs) != len(opSpecs) {
		t.Errorf("JS OP_SPECS has %d operators, Go opSpecs has %d", len(specs), len(opSpecs))
	}

	// OPS must stay DERIVED from OP_SPECS. If someone re-hand-writes the list,
	// the palette can fall behind the registry without any table disagreeing.
	if !strings.Contains(js, "const OPS = Object.keys(OP_SPECS)") {
		t.Errorf("%s: `OPS` must stay derived as `Object.keys(OP_SPECS)`; "+
			"a hand-written list can drift from the registry", serializerPath)
	}
}

// TestEditorUnaryOperatorsAgree pins the unary set specifically. It is the one
// place the two sides can disagree about ARITY rather than about a shape, and a
// disagreement here means the editor emits (or refuses) a `right` key the server
// does not expect — which breaks the byte-identical round-trip rather than just
// failing validation.
func TestEditorUnaryOperatorsAgree(t *testing.T) {
	js := readSerializer(t)
	specs := jsOpSpecs(t, js)

	var goUnary []string
	for op, spec := range opSpecs {
		if spec.shape == rightNone {
			goUnary = append(goUnary, op)
		}
	}
	var jsUnary []string
	for op, shape := range specs {
		if shape == "NONE" {
			jsUnary = append(jsUnary, op)
		}
	}

	// The set is closed and named in both files' doc comments; pin the literal
	// membership too, so neither side can quietly promote a binary operator.
	want := sorted([]string{OpExists, OpIsEmpty, OpIsNotEmpty})
	if got := sorted(goUnary); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("Go unary operators (opSpecs entries with shape rightNone) = %v, want %v", got, want)
	}
	if got := sorted(jsUnary); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("JS unary operators (OP_SPECS entries with right: RIGHT.NONE in %s) = %v, want %v",
			serializerPath, got, want)
	}

	// isUnaryOp drives both serializer directions and inputKeys; if it stops
	// reading OP_SPECS, the table agreement above stops proving anything about
	// the editor's behaviour.
	if !strings.Contains(js, "spec.right === RIGHT.NONE") {
		t.Errorf("%s: isUnaryOp must decide from OP_SPECS (`spec.right === RIGHT.NONE`), not from a separate list",
			serializerPath)
	}
}

// TestEditorVocabularyTablesAgree covers the rest of the mirrored vocabulary:
// node types, variable roots, the blocked predicate builtins, and the pure
// function set. Each is a table on both sides, so each is compared as a set with
// the offending name reported.
func TestEditorVocabularyTablesAgree(t *testing.T) {
	js := readSerializer(t)

	t.Run("TYPES vs NodeType consts", func(t *testing.T) {
		entries := jsObjectEntries(t, jsConstBody(t, js, "TYPES"))
		got := make([]string, 0, len(entries))
		for _, v := range entries {
			got = append(got, jsUnquote(v))
		}
		diffSets(t, "node type", goNodeTypes(t), got,
			"Go NodeType consts (rules/ast.go)", "JS TYPES ("+serializerPath+")")
	})

	t.Run("ROOTS vs allowedRoots", func(t *testing.T) {
		diffSets(t, "variable root", keysOf(allowedRoots), jsStringArray(t, jsConstBody(t, js, "ROOTS")),
			"Go allowedRoots (rules/ast.go)", "JS ROOTS ("+serializerPath+")")
	})

	t.Run("BLOCKED_CALLS vs blockedCallNames", func(t *testing.T) {
		diffSets(t, "blocked call", keysOf(blockedCallNames), jsStringArray(t, jsConstBody(t, js, "BLOCKED_CALLS")),
			"Go blockedCallNames (rules/ast.go)", "JS BLOCKED_CALLS ("+serializerPath+")")
	})

	t.Run("FUNCTIONS vs defaultFunctions", func(t *testing.T) {
		diffSets(t, "pure function", goDefaultFunctionNames(t), jsStringArray(t, jsConstBody(t, js, "FUNCTIONS")),
			"Go defaultFunctions (rules/compiler.go)", "JS FUNCTIONS ("+serializerPath+")")
	})

	// The relative-date node's three closed vocabularies. Each drives one editor
	// control, so a member the editor does not offer is a rule an author cannot
	// write, and a member the editor offers but Go does not know is a rule the
	// server rejects on save. Both directions are drift, so both are compared.
	t.Run("ANCHORS vs relativeAnchors", func(t *testing.T) {
		diffSets(t, "relative-date anchor", keysOf(relativeAnchors), jsStringArray(t, jsConstBody(t, js, "ANCHORS")),
			"Go relativeAnchors (rules/relative.go)", "JS ANCHORS ("+serializerPath+")")
	})

	t.Run("UNITS vs relativeUnits", func(t *testing.T) {
		diffSets(t, "relative-date offset unit", keysOf(relativeUnits), jsStringArray(t, jsConstBody(t, js, "UNITS")),
			"Go relativeUnits (rules/relative.go)", "JS UNITS ("+serializerPath+")")
	})

	t.Run("SNAPS vs relativeSnaps", func(t *testing.T) {
		diffSets(t, "relative-date snap", keysOf(relativeSnaps), jsStringArray(t, jsConstBody(t, js, "SNAPS")),
			"Go relativeSnaps (rules/relative.go)", "JS SNAPS ("+serializerPath+")")
	})
}

// TestEditorValidationMessagesAgree asserts the JS validator reports the Go
// validator's own codes and wording. The Go text is taken from Validate itself
// rather than hard-coded, so rewording a Go message without touching the JS
// fails here.
//
// The JS appends the operator/name after a colon where Go carries it as a
// context field, so the assertion is that the JS source contains the Go message
// (minus its "rule: " prefix) as the START of a string literal.
func TestEditorValidationMessagesAgree(t *testing.T) {
	js := readSerializer(t)

	cases := []struct {
		name string
		node *Node
		code aerr.Code
	}{
		{"unknown operator", Compare("nope", Var("object.tags"), Lit("a")), aerr.APERTURE_RULE_INVALID},
		{"missing left", &Node{Type: NodeCompare, Op: OpEq, Right: Lit("a")}, aerr.APERTURE_RULE_INVALID},
		{"missing right", &Node{Type: NodeCompare, Op: OpEq, Left: Var("object.tags")}, aerr.APERTURE_RULE_INVALID},
		{"unary with a right operand", Compare(OpIsEmpty, Var("object.tags"), Lit("a")), aerr.APERTURE_RULE_INVALID},
		{"collection operand is neither list nor var", Compare(OpHasAll, Var("object.tags"), Lit("a")), aerr.APERTURE_RULE_INVALID},
		{"element operand is a list", Compare(OpHas, Var("object.tags"), List(Lit("a"))), aerr.APERTURE_RULE_INVALID},
		{"ternary operand is not two bounds", &Node{Type: NodeCompare, Op: OpBetween,
			Left: Var("object.hired_at"), Right: List(Lit("2026-01-01"))}, aerr.APERTURE_RULE_INVALID},
		{"not requires exactly one child", &Node{Type: NodeNot}, aerr.APERTURE_RULE_INVALID},
		{"not a dotted path", Var("object..tags"), aerr.APERTURE_RULE_INVALID},
		{"unexposed root", Var("secrets.key"), aerr.APERTURE_RULE_UNKNOWN_VARIABLE},
		{"blocked call", Call("filter", Var("object.tags")), aerr.APERTURE_RULE_INVALID},
		{"literal has no value", &Node{Type: NodeLiteral}, aerr.APERTURE_RULE_INVALID},
		{"composite literal", &Node{Type: NodeLiteral, Value: []byte(`["a"]`)}, aerr.APERTURE_RULE_INVALID},

		// The relative-date node. Its four fields fail independently, because
		// each names a different correction, and the editor renders whichever
		// message it is handed beside the offending control.
		{"relative date outside a date operator",
			Compare(OpEq, Var("object.hired_at"), RelativeDate(AnchorNow, -3, UnitMonths, SnapNone)),
			aerr.APERTURE_RULE_INVALID},
		{"relative date unknown anchor",
			Compare(OpBefore, Var("object.hired_at"), RelativeDate("YESTERDAY", 0, UnitDays, SnapNone)),
			aerr.APERTURE_RULE_INVALID},
		{"relative date unknown unit",
			Compare(OpBefore, Var("object.hired_at"), RelativeDate(AnchorNow, -3, "fortnights", SnapNone)),
			aerr.APERTURE_RULE_INVALID},
		{"relative date unknown snap",
			Compare(OpBefore, Var("object.hired_at"), RelativeDate(AnchorNow, -3, UnitMonths, "startOfFiscalYear")),
			aerr.APERTURE_RULE_INVALID},
		{"relative date offset is not a whole number",
			Compare(OpBefore, Var("object.hired_at"),
				&Node{Type: NodeRelativeDate, Anchor: AnchorNow, Offset: "1.5", Unit: UnitMonths, Snap: SnapNone}),
			aerr.APERTURE_RULE_INVALID},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.node.Validate()
			if err == nil {
				t.Fatalf("Validate accepted a node this case expects it to reject")
			}
			if got := aerr.CodeOf(err); got != tc.code {
				t.Fatalf("Validate returned code %s, want %s", got, tc.code)
			}
			if !strings.Contains(js, string(tc.code)) {
				t.Errorf("%s never reports %s, but the Go validator does for this case",
					serializerPath, tc.code)
			}
			msg := strings.TrimPrefix(messageOf(t, err), "rule: ")
			if !strings.Contains(js, `"`+msg) {
				t.Errorf("%s does not carry the Go validator's wording for this case.\n"+
					"  Go:   %q\n"+
					"  want: a JS string literal starting with that text "+
					"(the JS may append \": \" + the op/name)", serializerPath, msg)
			}
		})
	}
}

// TestEditorASTContractCoversEveryOperator drives the byte-identical round-trip
// off opSpecs itself, so an operator added to Go is covered the moment it lands.
// The handwritten case table in editor_contract_test.go documents the editor's
// real output; this proves NOTHING in the registry is left untested.
//
// It also pins the arity rule from both directions: a unary operator with a
// `right` is rejected, and a binary operator without one is rejected.
func TestEditorASTContractCoversEveryOperator(t *testing.T) {
	for op, spec := range opSpecs {
		t.Run(op, func(t *testing.T) {
			src := editorJSONForOp(t, op, spec.shape)
			assertEditorJSONRoundTrips(t, src)

			if spec.shape == rightNone {
				n := Compare(op, Var("object.tags"), Lit("a"))
				if err := n.Validate(); err == nil {
					t.Errorf("unary operator %q accepted a right operand; the editor omits the key, "+
						"so the AST would stop round-tripping predictably", op)
				}
				return
			}
			n := &Node{Type: NodeCompare, Op: op, Left: Var("object.tags")}
			if err := n.Validate(); err == nil {
				t.Errorf("binary operator %q accepted a missing right operand", op)
			}
		})
	}
}

// editorJSONForOp writes the minimal well-formed editor JSON for op, choosing
// the right operand from the operator's declared shape. A shape with no case
// here is a hard failure rather than a skip: a new rightShape must not slip past
// the round-trip coverage.
func editorJSONForOp(t *testing.T, op string, shape rightShape) string {
	t.Helper()
	head := `{"type":"compare","op":"` + op + `","left":{"type":"var","name":"object.tags"}`
	switch shape {
	case rightNone:
		return head + `}`
	case rightCollection:
		return head + `,"right":{"type":"list","items":[{"type":"literal","value":"a"}]}}`
	case rightBounds:
		// The ternary shape: a list of EXACTLY two bounds, and no new JSON key.
		return head + `,"right":{"type":"list","items":[` +
			`{"type":"literal","value":"2026-01-01"},{"type":"literal","value":"2026-12-31"}]}}`
	case rightAny, rightElement:
		return head + `,"right":{"type":"literal","value":"a"}}`
	default:
		t.Fatalf("operator %q has right-operand shape %d with no round-trip case; add one here", op, shape)
		return ""
	}
}

// ---- helpers: Go side ------------------------------------------------------

// goNodeTypes scans ast.go for the NodeType consts so a new node type is picked
// up without editing this test.
func goNodeTypes(t *testing.T) []string {
	t.Helper()
	src := readRepoFile(t, astPath)
	m := regexp.MustCompile(`Node[A-Za-z]+\s+NodeType\s*=\s*"([^"]+)"`).FindAllStringSubmatch(src, -1)
	if len(m) == 0 {
		t.Fatalf("%s: found no `Node… NodeType = \"…\"` consts; the scanner needs updating", astPath)
	}
	out := make([]string, 0, len(m))
	for _, g := range m {
		out = append(out, g[1])
	}
	return out
}

// goDefaultFunctionNames scans the body of defaultFunctions for every function
// it registers. Names that varPath rejects — today only the guarded dispatcher
// `$op` — are compiler-internal: no NodeCall can name them, so the editor must
// NOT list them, and they are excluded here for the same reason.
func goDefaultFunctionNames(t *testing.T) []string {
	t.Helper()
	body := goFuncBody(t, readRepoFile(t, compilerPath), "defaultFunctions")

	var names []string
	add := func(name string) {
		if varPath.MatchString(name) {
			names = append(names, name)
		}
	}
	// expr.Function("literal", …) and expr.Function(identifier, …).
	for _, m := range regexp.MustCompile(`expr\.Function\(\s*(?:"([^"]+)"|([A-Za-z_]\w*))`).FindAllStringSubmatch(body, -1) {
		if m[1] != "" {
			add(m[1])
			continue
		}
		add(resolveFuncConst(t, m[2]))
	}
	// binaryCollectionFunc(fnX, …) / unaryCollectionFunc(fnX, …).
	for _, m := range regexp.MustCompile(`(?:binary|unary)CollectionFunc\(\s*([A-Za-z_]\w*)`).FindAllStringSubmatch(body, -1) {
		add(resolveFuncConst(t, m[1]))
	}
	if len(names) == 0 {
		t.Fatalf("%s: found no function registrations inside defaultFunctions; the scanner needs updating", compilerPath)
	}
	return names
}

func resolveFuncConst(t *testing.T, ident string) string {
	t.Helper()
	v, ok := goBackingFuncConsts[ident]
	if !ok {
		t.Fatalf("%s registers a function under the constant %s, which this test cannot resolve; "+
			"add it to goBackingFuncConsts", compilerPath, ident)
	}
	return v
}

// goFuncBody returns the body of a top-level func, from its declaration to the
// closing brace in column 0.
func goFuncBody(t *testing.T, src, name string) string {
	t.Helper()
	loc := regexp.MustCompile(`(?m)^func ` + regexp.QuoteMeta(name) + `\(`).FindStringIndex(src)
	if loc == nil {
		t.Fatalf("%s: no top-level `func %s(` found; the scanner needs updating", compilerPath, name)
	}
	rest := src[loc[0]:]
	end := strings.Index(rest, "\n}\n")
	if end < 0 {
		t.Fatalf("%s: func %s is not terminated by a closing brace in column 0", compilerPath, name)
	}
	return rest[:end]
}

// messageOf pulls the bare message off a coded error, without the "[CODE] "
// prefix Error() renders.
func messageOf(t *testing.T, err error) string {
	t.Helper()
	var ce *aerr.CodedError
	if !stderrors.As(err, &ce) {
		t.Fatalf("error is not an *errors.CodedError: %v", err)
	}
	return ce.Msg
}

// ---- helpers: JS side ------------------------------------------------------

func readSerializer(t *testing.T) string {
	t.Helper()
	return readRepoFile(t, serializerPath)
}

// readRepoFile reads a file this contract test compares against, FAILING (never
// skipping) when it is absent. A skip here would silently reopen the drift hole.
func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.FromSlash(rel))
	if err != nil {
		t.Fatalf("cannot read %s, which this contract test compares against: %v\n"+
			"If the file moved, update the path in rules/editor_js_contract_test.go — do not delete the check.",
			rel, err)
	}
	return string(b)
}

// jsOpSpecs returns the JS OP_SPECS table as operator -> RIGHT key ("ANY",
// "COLLECTION", "ELEMENT", "NONE").
func jsOpSpecs(t *testing.T, js string) map[string]string {
	t.Helper()
	entries := jsObjectEntries(t, jsConstBody(t, js, "OP_SPECS"))
	re := regexp.MustCompile(`^\{\s*right\s*:\s*RIGHT\.([A-Z_]+)\s*,?\s*\}$`)
	out := make(map[string]string, len(entries))
	for op, raw := range entries {
		m := re.FindStringSubmatch(strings.TrimSpace(raw))
		if m == nil {
			t.Fatalf("%s: OP_SPECS entry %q = %s is not of the form `{ right: RIGHT.X }`; "+
				"keep the table machine-readable so this contract test can compare it",
				serializerPath, op, raw)
		}
		out[op] = m[1]
	}
	if len(out) == 0 {
		t.Fatalf("%s: OP_SPECS parsed as empty", serializerPath)
	}
	return out
}

// jsConstBody returns the `{…}` or `[…]` initializer of a top-level
// `const NAME = …` declaration, scanning balanced delimiters while skipping
// string literals and comments. It is deliberately a small hand-rolled scan: the
// repo takes no JS-parser dependency and CI has no node.
func jsConstBody(t *testing.T, src, name string) string {
	t.Helper()
	loc := regexp.MustCompile(`(?m)^\s*const\s+` + regexp.QuoteMeta(name) + `\s*=\s*`).FindStringIndex(src)
	if loc == nil {
		t.Fatalf("%s: no `const %s = …` declaration. This contract test reads that table; "+
			"if it was renamed, rename it here too — do not drop the check.", serializerPath, name)
	}
	rest := src[loc[1]:]
	if len(rest) == 0 || (rest[0] != '{' && rest[0] != '[') {
		t.Fatalf("%s: `const %s` is not initialized with an object or array literal", serializerPath, name)
	}
	open := rest[0]
	closer := byte('}')
	if open == '[' {
		closer = ']'
	}
	depth := 0
	for i := 0; i < len(rest); i++ {
		switch c := rest[i]; c {
		case '"', '\'':
			i = skipJSString(rest, i)
		case '/':
			if i+1 < len(rest) && rest[i+1] == '/' {
				for i < len(rest) && rest[i] != '\n' {
					i++
				}
			} else if i+1 < len(rest) && rest[i+1] == '*' {
				k := strings.Index(rest[i+2:], "*/")
				if k < 0 {
					t.Fatalf("%s: unterminated block comment inside const %s", serializerPath, name)
				}
				i += 2 + k + 1
			}
		case open:
			depth++
		case closer:
			depth--
			if depth == 0 {
				return rest[:i+1]
			}
		}
	}
	t.Fatalf("%s: unterminated initializer for const %s", serializerPath, name)
	return ""
}

// jsObjectEntries splits an object literal into key -> raw value text, at the top
// level only. Keys may be bare identifiers or quoted.
func jsObjectEntries(t *testing.T, body string) map[string]string {
	t.Helper()
	inner := strings.TrimSpace(stripJSComments(body))
	if !strings.HasPrefix(inner, "{") || !strings.HasSuffix(inner, "}") {
		t.Fatalf("expected an object literal, got: %s", body)
	}
	inner = inner[1 : len(inner)-1]

	out := map[string]string{}
	var cur strings.Builder
	flush := func() {
		e := strings.TrimSpace(cur.String())
		cur.Reset()
		if e == "" {
			return
		}
		i := strings.Index(e, ":")
		if i < 0 {
			t.Fatalf("cannot parse object entry %q", e)
		}
		out[strings.Trim(strings.TrimSpace(e[:i]), `"'`)] = strings.TrimSpace(e[i+1:])
	}

	depth := 0
	for i := 0; i < len(inner); i++ {
		c := inner[i]
		switch {
		case c == '"' || c == '\'':
			j := skipJSString(inner, i)
			cur.WriteString(inner[i : j+1])
			i = j
			continue
		case c == '{' || c == '[' || c == '(':
			depth++
		case c == '}' || c == ']' || c == ')':
			depth--
		case c == ',' && depth == 0:
			flush()
			continue
		}
		cur.WriteByte(c)
	}
	flush()
	return out
}

// jsStringArray returns every string literal in an array literal, in order.
func jsStringArray(t *testing.T, body string) []string {
	t.Helper()
	src := stripJSComments(body)
	var out []string
	for i := 0; i < len(src); i++ {
		if c := src[i]; c == '"' || c == '\'' {
			j := skipJSString(src, i)
			out = append(out, src[i+1:j])
			i = j
		}
	}
	if len(out) == 0 {
		t.Fatalf("expected an array of string literals, got: %s", body)
	}
	return out
}

// skipJSString returns the index of the closing quote of the string literal that
// starts at i, honouring backslash escapes. It returns len(s)-1 for an
// unterminated literal, which the callers then report as a parse failure.
func skipJSString(s string, i int) int {
	q := s[i]
	for j := i + 1; j < len(s); j++ {
		switch s[j] {
		case '\\':
			j++
		case q:
			return j
		}
	}
	return len(s) - 1
}

// stripJSComments removes // and /* */ comments. It is only ever applied to an
// already-extracted table body, none of which contains a regex literal, so the
// classic "is that slash a division or a regex" ambiguity cannot arise.
func stripJSComments(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"' || c == '\'':
			j := skipJSString(s, i)
			b.WriteString(s[i : j+1])
			i = j
		case c == '/' && i+1 < len(s) && s[i+1] == '/':
			for i < len(s) && s[i] != '\n' {
				i++
			}
			b.WriteByte('\n')
		case c == '/' && i+1 < len(s) && s[i+1] == '*':
			k := strings.Index(s[i+2:], "*/")
			if k < 0 {
				return b.String()
			}
			i += 2 + k + 1
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func jsUnquote(s string) string { return strings.Trim(strings.TrimSpace(s), `"'`) }

// ---- helpers: set comparison ----------------------------------------------

// diffSets reports every name present on one side and absent from the other,
// naming the kind of entry and both sources so the failure is diagnosable from
// the test output alone.
func diffSets(t *testing.T, kind string, goSide, jsSide []string, goSource, jsSource string) {
	t.Helper()
	inJS := make(map[string]bool, len(jsSide))
	for _, n := range jsSide {
		inJS[n] = true
	}
	inGo := make(map[string]bool, len(goSide))
	for _, n := range goSide {
		inGo[n] = true
	}
	for _, n := range sorted(goSide) {
		if !inJS[n] {
			t.Errorf("%s %q is in %s but MISSING from %s", kind, n, goSource, jsSource)
		}
	}
	for _, n := range sorted(jsSide) {
		if !inGo[n] {
			t.Errorf("%s %q is in %s but MISSING from %s", kind, n, jsSource, goSource)
		}
	}
}

func keysOf(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return sorted(out)
}

func sorted(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}
