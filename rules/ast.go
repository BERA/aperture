// Package rules is Aperture's rules engine: it decides allow/deny — and, on the
// scope seam, object-membership selection — from a domain object's metadata plus
// the principal/action context, using the expr-lang/expr expression evaluator.
//
// The package has three layers:
//
//   - The rule AST (this file). A small, explicit, typed node set — logical
//     and/or/not, comparisons, variable references, scalar literals, list
//     literals, and function calls. The AST is BOTH the engine's input and the
//     node editor's serialization target: it has a stable JSON form that
//     round-trips (marshal→unmarshal→marshal is byte-identical), so the Rete.js
//     editor (E7-S2) reads/writes it and the state file (E5-S2) persists it. The
//     editor maps its nodes one-to-one onto these AST nodes; there is no second
//     rule format.
//   - The compiler + cache (compiler.go, cache.go). An AST is validated, rendered
//     to an expr-lang expression, and compiled once to a reusable program by
//     expr-lang/expr (a pure-Go evaluator). Compiled programs are cached by the
//     rule's canonical hash so per-Check cost is bounded (the NFR lever E4-S4 tunes).
//   - The engine (engine.go). It resolves a rule reference through a RuleSource,
//     compiles-and-caches it, builds the evaluation context from the object's
//     metadata (fetched through the provider registry) and the principal/action,
//     and evaluates. It satisfies scope.RuleEvaluator, so the inclusive/exclusive
//     scope resolvers (E2-S1) get their rule-backed variant.
//
// Evaluation is pure and deterministic given a fixed metadata snapshot: the
// expression environment is built only from the supplied context, no wall-clock
// or random builtins are exposed, and the only functions callable are the curated
// pure set plus any a host explicitly registers.
package rules

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strconv"

	aerr "github.com/frankbardon/aperture/errors"
)

// NodeType is the discriminator for an AST node. The set is deliberately small
// and closed so the node editor can map its palette one-to-one onto it.
type NodeType string

const (
	// NodeAnd is the logical conjunction of two or more child nodes.
	NodeAnd NodeType = "and"
	// NodeOr is the logical disjunction of two or more child nodes.
	NodeOr NodeType = "or"
	// NodeNot is the logical negation of exactly one child node.
	NodeNot NodeType = "not"
	// NodeCompare is a binary comparison between a left and a right operand.
	NodeCompare NodeType = "compare"
	// NodeVar is a reference to a context variable by dotted path.
	NodeVar NodeType = "var"
	// NodeLiteral is a scalar constant (string, number, bool, or null).
	NodeLiteral NodeType = "literal"
	// NodeList is an ordered list of operand nodes, used as the right side of a
	// collection comparison (in, nin, hasAll, hasAny, hasNone, subsetOf).
	NodeList NodeType = "list"
	// NodeCall is a call to a registered pure function.
	NodeCall NodeType = "call"
)

// Comparison operators carried in a NodeCompare's Op field.
//
// The first eight are the scalar/membership operators. The nine that follow are
// the collection operators, which read a list- or object-valued metadata field.
// Three of those (OpIsEmpty, OpIsNotEmpty, OpExists) are UNARY: they reuse
// NodeCompare with Right OMITTED rather than introducing a new node type, so the
// editor gains nine operators and no new palette primitive.
const (
	OpEq  = "eq"  // ==
	OpNe  = "ne"  // !=
	OpLt  = "lt"  // <
	OpLe  = "le"  // <=
	OpGt  = "gt"  // >
	OpGe  = "ge"  // >=
	OpIn  = "in"  // in  (right operand is a list/collection)
	OpNin = "nin" // not in

	OpHas        = "has"        // object.tags has "x"           (array)
	OpHasAll     = "hasAll"     // object.tags has all [a, b]    (array)
	OpHasAny     = "hasAny"     // object.tags has any [a, b]    (array)
	OpHasNone    = "hasNone"    // object.tags has none [a, b]   (array)
	OpSubsetOf   = "subsetOf"   // object.tags subset of [a, b]  (array)
	OpHasKey     = "hasKey"     // object.owner has key "dept"   (object)
	OpIsEmpty    = "isEmpty"    // object.tags is empty          (array, object) — unary
	OpIsNotEmpty = "isNotEmpty" // object.tags is not empty      (array, object) — unary
	OpExists     = "exists"     // object.owner.dept exists      (any path)      — unary
)

// rightShape constrains what a comparison's right operand may be — the
// operand-shape half of an operator's contract, enforced by Validate.
type rightShape uint8

const (
	// rightAny accepts any operand node (the scalar comparisons).
	rightAny rightShape = iota
	// rightCollection requires a list literal or a variable: the right operand is
	// itself a collection (in, nin, hasAll, hasAny, hasNone, subsetOf).
	rightCollection
	// rightElement requires a single element — anything but a list node. A set on
	// the right is hasAll/hasAny/hasNone, not has (has, hasKey).
	rightElement
	// rightNone requires Right to be omitted entirely: the unary operators.
	rightNone
)

// renderKind is how a comparison operator turns into expr-lang source.
type renderKind uint8

const (
	// renderInfix writes `(left <text> right)` — the native spelling.
	renderInfix renderKind = iota
	// renderFlippedIn writes `(right in left)`: the operator reads
	// collection-first but compiles to expr's element-first `in`.
	renderFlippedIn
	// renderNotNil writes `(left != nil)`.
	renderNotNil
	// renderFunc writes `<text>(left)` or `<text>(left, right)`, calling one of
	// the backing functions registered in defaultFunctions().
	renderFunc
)

// opSpec is one comparison operator's contract: the shape its right operand must
// take, and how it renders to expr-lang. Keeping both in a single table is what
// stops Validate and render from disagreeing about the operator set. text is the
// infix operator for renderInfix and the backing function name for renderFunc;
// the other kinds do not use it.
type opSpec struct {
	shape rightShape
	kind  renderKind
	text  string
}

// opSpecs is the closed comparison-operator registry.
//
// RENDER STRATEGY, recorded per operator. expr-lang's builtin library is disabled
// (expr.DisableAllBuiltins, compiler.go), so nothing may be ASSUMED reachable:
// every operator either renders to NATIVE expr syntax or is backed by a pure
// function registered in defaultFunctions().
//
//   - eq ne lt le gt ge in nin — NATIVE infix, unchanged.
//   - has, hasKey — NATIVE, a flipped `in`: `(right in left)`. expr's `in` is
//     element membership over a slice AND KEY membership over a map, so hasKey is
//     pure spelling over behavior that already exists and neither operator needs
//     a backing function.
//   - exists — NATIVE: `(left != nil)`. Together with the optional chaining
//     renderVar emits, a missing intermediate reads as nil, so
//     `object.owner.dept exists` is false rather than a runtime error.
//   - hasAll hasAny hasNone subsetOf isEmpty isNotEmpty — REGISTERED FUNCTIONS.
//     Their native spellings would be expr's `all`/`any`/`len`, which are either
//     genuinely disabled (len) or reachable only through a `#` pointer this AST
//     cannot emit — and which validateCall explicitly denies. Each is backed by a
//     deterministic, side-effect-free func in defaultFunctions().
//
// GUARDING OVERRIDES ALL OF THE ABOVE (E5-S1). Whenever an operator's collection
// operand is not statically known to be a collection — i.e. anything but a list
// literal — the comparison renders instead to the guarded dispatcher `$op`
// (renderGuarded), which applies the deny-safe shape policy in shape.go and
// records a note. The forms above are what a comparison renders to when no
// operand can be the wrong shape; the runtime contract lives in collOps.
var opSpecs = map[string]opSpec{
	OpEq:         {shape: rightAny, kind: renderInfix, text: "=="},
	OpNe:         {shape: rightAny, kind: renderInfix, text: "!="},
	OpLt:         {shape: rightAny, kind: renderInfix, text: "<"},
	OpLe:         {shape: rightAny, kind: renderInfix, text: "<="},
	OpGt:         {shape: rightAny, kind: renderInfix, text: ">"},
	OpGe:         {shape: rightAny, kind: renderInfix, text: ">="},
	OpIn:         {shape: rightCollection, kind: renderInfix, text: "in"},
	OpNin:        {shape: rightCollection, kind: renderInfix, text: "not in"},
	OpHas:        {shape: rightElement, kind: renderFlippedIn},
	OpHasKey:     {shape: rightElement, kind: renderFlippedIn},
	OpHasAll:     {shape: rightCollection, kind: renderFunc, text: fnHasAll},
	OpHasAny:     {shape: rightCollection, kind: renderFunc, text: fnHasAny},
	OpHasNone:    {shape: rightCollection, kind: renderFunc, text: fnHasNone},
	OpSubsetOf:   {shape: rightCollection, kind: renderFunc, text: fnSubsetOf},
	OpIsEmpty:    {shape: rightNone, kind: renderFunc, text: fnIsEmpty},
	OpIsNotEmpty: {shape: rightNone, kind: renderFunc, text: fnIsNotEmpty},
	OpExists:     {shape: rightNone, kind: renderNotNil},
}

// blockedCallNames are expr-lang's PREDICATE builtins. expr.DisableAllBuiltins
// does NOT reach them: the parser matches a predicate name (parser.predicates)
// BEFORE it consults the disabled-builtin table, so `all(...)` and `any(...)`
// still compile under Aperture's option set even though `len(...)` does not.
// None is reachable through a well-formed rule today — nothing in this AST emits
// expr's `#` pointer — but NodeCall renders `name(args...)` verbatim, so a
// NodeCall{Name: "all"} WOULD compile. Denying the names structurally in Validate
// closes that door for every caller, ValidateAST and the editor's save path
// included. Keep in sync with expr-lang/expr parser/parser.go.
var blockedCallNames = map[string]struct{}{
	"all": {}, "none": {}, "any": {}, "one": {}, "filter": {}, "map": {},
	"count": {}, "sum": {}, "find": {}, "findIndex": {}, "findLast": {},
	"findLastIndex": {}, "groupBy": {}, "sortBy": {}, "reduce": {},
}

// allowedRoots is the closed set of context-variable roots a rule may reference.
// A variable whose first path segment is outside this set is an unknown variable,
// caught by Validate before compilation — deterministically, without relying on
// the expression checker's wording.
var allowedRoots = map[string]struct{}{
	"object":    {},
	"principal": {},
	"account":   {},
	"action":    {},
}

// varPath matches a dotted identifier path, e.g. object.classification or
// principal.attrs.tier. Each segment is a Go-style identifier, which keeps the
// rendered expression injection-free.
var varPath = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*$`)

// Node is one node of the rule AST. A node uses only the fields its Type calls
// for; the rest stay zero and are omitted from JSON, so the serialized form is
// minimal and stable. The JSON shape is the contract the node editor and the
// state file share, so field names and the closed Type set are part of the API.
//
// Field usage by Type:
//
//	and/or   Children (>= 2)
//	not      Children (exactly 1)
//	compare  Op, Left, Right — Right is OMITTED for the unary operators
//	         (isEmpty, isNotEmpty, exists) and required for every other operator
//	var      Name (dotted path)
//	literal  Value (scalar JSON: string, number, bool, or null)
//	list     Items
//	call     Name (function), Items (arguments)
type Node struct {
	Type     NodeType        `json:"type"`
	Op       string          `json:"op,omitempty"`
	Name     string          `json:"name,omitempty"`
	Value    json.RawMessage `json:"value,omitempty"`
	Left     *Node           `json:"left,omitempty"`
	Right    *Node           `json:"right,omitempty"`
	Children []*Node         `json:"children,omitempty"`
	Items    []*Node         `json:"items,omitempty"`
}

// And returns a logical-and over children.
func And(children ...*Node) *Node { return &Node{Type: NodeAnd, Children: children} }

// Or returns a logical-or over children.
func Or(children ...*Node) *Node { return &Node{Type: NodeOr, Children: children} }

// Not returns the logical negation of child.
func Not(child *Node) *Node { return &Node{Type: NodeNot, Children: []*Node{child}} }

// Compare returns a binary comparison node for one of the Op* operators. Use
// Unary for OpIsEmpty / OpIsNotEmpty / OpExists, which take no right operand.
func Compare(op string, left, right *Node) *Node {
	return &Node{Type: NodeCompare, Op: op, Left: left, Right: right}
}

// Unary returns a unary comparison node for one of the unary operators —
// OpIsEmpty, OpIsNotEmpty, OpExists. It is still a NodeCompare; Right is left nil
// so it is omitted from the JSON form, which is exactly what Validate requires
// for these three operators.
func Unary(op string, left *Node) *Node {
	return &Node{Type: NodeCompare, Op: op, Left: left}
}

// Var returns a variable reference for a dotted path (e.g. object.classification).
func Var(path string) *Node { return &Node{Type: NodeVar, Name: path} }

// Lit returns a scalar literal node. v must marshal to a JSON scalar (string,
// number, bool, or null); a non-scalar is rejected by Validate.
func Lit(v any) *Node {
	b, err := json.Marshal(v)
	if err != nil {
		return &Node{Type: NodeLiteral}
	}
	return &Node{Type: NodeLiteral, Value: json.RawMessage(b)}
}

// List returns a list literal over items, used as the right operand of a
// collection comparison (in, nin, hasAll, hasAny, hasNone, subsetOf).
func List(items ...*Node) *Node { return &Node{Type: NodeList, Items: items} }

// Call returns a function-call node. The function must be one registered with the
// compiler (the curated pure set, plus any the host adds); an unknown function is
// caught at compile time, and expr's predicate builtins are refused by Validate
// (see blockedCallNames).
func Call(name string, args ...*Node) *Node {
	return &Node{Type: NodeCall, Name: name, Items: args}
}

// Validate checks that the node (and its subtree) is structurally well-formed,
// returning an APERTURE_RULE_INVALID coded error for a malformed node and
// APERTURE_RULE_UNKNOWN_VARIABLE for a variable outside the exposed roots.
// Validation is pure structure — it does not type-check; that is the compiler's
// job — so it never touches the expression engine.
func (n *Node) Validate() error {
	if n == nil {
		return aerr.New(aerr.APERTURE_RULE_INVALID, "rule: nil node")
	}
	switch n.Type {
	case NodeAnd, NodeOr:
		if len(n.Children) < 2 {
			return aerr.WithContext(aerr.APERTURE_RULE_INVALID,
				"rule: logical node requires at least two children",
				map[string]any{"type": string(n.Type), "children": len(n.Children)})
		}
		return validateAll(n.Children)
	case NodeNot:
		if len(n.Children) != 1 {
			return aerr.WithContext(aerr.APERTURE_RULE_INVALID,
				"rule: not requires exactly one child",
				map[string]any{"children": len(n.Children)})
		}
		return n.Children[0].Validate()
	case NodeCompare:
		return n.validateCompare()
	case NodeVar:
		return validateVar(n.Name)
	case NodeLiteral:
		return validateLiteral(n.Value)
	case NodeList:
		return validateAll(n.Items)
	case NodeCall:
		return n.validateCall()
	default:
		return aerr.WithContext(aerr.APERTURE_RULE_INVALID,
			"rule: unknown node type", map[string]any{"type": string(n.Type)})
	}
}

// validateCompare checks a NodeCompare against its operator's opSpec: the
// operator is known, the operand ARITY matches (a unary operator takes no right
// operand and a binary one requires both), and the right operand's SHAPE is what
// the operator can act on.
//
// The arity rule is deliberately per-operator rather than blanket: exactly
// OpIsEmpty / OpIsNotEmpty / OpExists must omit Right, and a Right supplied for
// one of them is an error rather than something silently ignored — otherwise the
// AST would have two spellings for the same rule and would stop round-tripping
// predictably.
func (n *Node) validateCompare() error {
	spec, ok := opSpecs[n.Op]
	if !ok {
		return aerr.WithContext(aerr.APERTURE_RULE_INVALID,
			"rule: unknown comparison operator", map[string]any{"op": n.Op})
	}
	if n.Left == nil {
		return aerr.WithContext(aerr.APERTURE_RULE_INVALID,
			"rule: comparison requires a left operand", map[string]any{"op": n.Op})
	}
	if spec.shape == rightNone {
		if n.Right != nil {
			return aerr.WithContext(aerr.APERTURE_RULE_INVALID,
				"rule: unary operator takes no right operand",
				map[string]any{"op": n.Op, "right": string(n.Right.Type)})
		}
		return n.Left.Validate()
	}
	if n.Right == nil {
		return aerr.New(aerr.APERTURE_RULE_INVALID,
			"rule: comparison requires a left and a right operand")
	}
	if err := n.Left.Validate(); err != nil {
		return err
	}
	switch spec.shape {
	case rightCollection:
		if n.Right.Type != NodeList && n.Right.Type != NodeVar {
			return aerr.WithContext(aerr.APERTURE_RULE_INVALID,
				"rule: operator requires a list or variable on the right",
				map[string]any{"op": n.Op, "right": string(n.Right.Type)})
		}
	case rightElement:
		if n.Right.Type == NodeList {
			return aerr.WithContext(aerr.APERTURE_RULE_INVALID,
				"rule: operator requires a single element on the right; use hasAll/hasAny/hasNone for a set",
				map[string]any{"op": n.Op, "right": string(n.Right.Type)})
		}
	}
	return n.Right.Validate()
}

// validateCall checks a NodeCall's function name. Beyond the identifier shape it
// rejects expr-lang's predicate builtins, which survive DisableAllBuiltins — see
// blockedCallNames. Whether the name is actually REGISTERED is the compiler's
// call (a host may add its own functions), surfaced as APERTURE_RULE_TYPE_ERROR.
func (n *Node) validateCall() error {
	if n.Name == "" || !varPath.MatchString(n.Name) {
		return aerr.WithContext(aerr.APERTURE_RULE_INVALID,
			"rule: call has an invalid function name", map[string]any{"name": n.Name})
	}
	if _, blocked := blockedCallNames[n.Name]; blocked {
		return aerr.WithContext(aerr.APERTURE_RULE_INVALID,
			"rule: function is not callable from a rule", map[string]any{"name": n.Name})
	}
	return validateAll(n.Items)
}

func validateAll(nodes []*Node) error {
	for _, c := range nodes {
		if err := c.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func validateVar(path string) error {
	if !varPath.MatchString(path) {
		return aerr.WithContext(aerr.APERTURE_RULE_INVALID,
			"rule: variable reference is not a dotted identifier path",
			map[string]any{"name": path})
	}
	root := path
	if i := indexByte(path, '.'); i >= 0 {
		root = path[:i]
	}
	if _, ok := allowedRoots[root]; !ok {
		return aerr.WithContext(aerr.APERTURE_RULE_UNKNOWN_VARIABLE,
			"rule: variable root is not an exposed context root",
			map[string]any{"name": path, "root": root})
	}
	return nil
}

// validateLiteral confirms the literal carries a scalar JSON value. Arrays and
// objects are rejected here so a literal can never smuggle a composite; use a
// NodeList for collections.
func validateLiteral(raw json.RawMessage) error {
	if len(raw) == 0 {
		return aerr.New(aerr.APERTURE_RULE_INVALID, "rule: literal has no value")
	}
	// FAST PATH. classifyScalar PROVES the bytes are a scalar without decoding
	// them, so a well-formed literal — every literal on the decision hot path —
	// costs no allocation at all. It never proves the negative: anything it cannot
	// classify falls through to decodeScalar, which remains the sole authority on
	// what a literal may be. Validation therefore accepts and rejects exactly the
	// same set it always did.
	if kind, _ := classifyScalar(raw); kind != scalarUnclassified {
		return nil
	}
	if _, err := decodeScalar(raw); err != nil {
		return err
	}
	return nil
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// Expr renders the validated AST to an expr-lang expression string — the
// predicate the compiler feeds to the evaluator. Expr assumes the node is valid;
// callers reach it through the compiler, which validates first.
//
// Variable references render with optional chaining after the root
// (object.owner.dept -> object?.owner?.dept) so a missing intermediate yields
// nil instead of a runtime error; see renderVar.
func (n *Node) Expr() (string, error) {
	var b bytes.Buffer
	if err := n.render(&b); err != nil {
		return "", err
	}
	return b.String(), nil
}

func (n *Node) render(b *bytes.Buffer) error {
	switch n.Type {
	case NodeAnd:
		return renderJoined(b, n.Children, " && ")
	case NodeOr:
		return renderJoined(b, n.Children, " || ")
	case NodeNot:
		b.WriteString("!(")
		if err := n.Children[0].render(b); err != nil {
			return err
		}
		b.WriteByte(')')
		return nil
	case NodeCompare:
		return n.renderCompare(b)
	case NodeVar:
		renderVar(b, n.Name)
		return nil
	case NodeLiteral:
		return renderLiteral(b, n.Value)
	case NodeList:
		b.WriteByte('[')
		for i, it := range n.Items {
			if i > 0 {
				b.WriteString(", ")
			}
			if err := it.render(b); err != nil {
				return err
			}
		}
		b.WriteByte(']')
		return nil
	case NodeCall:
		b.WriteString(n.Name)
		b.WriteByte('(')
		for i, a := range n.Items {
			if i > 0 {
				b.WriteString(", ")
			}
			if err := a.render(b); err != nil {
				return err
			}
		}
		b.WriteByte(')')
		return nil
	default:
		return aerr.WithContext(aerr.APERTURE_RULE_INVALID,
			"rule: cannot render unknown node type", map[string]any{"type": string(n.Type)})
	}
}

// renderCompare writes a NodeCompare in the form its operator's opSpec calls for.
// Each branch is documented at opSpecs; the short version is that has/hasKey flip
// to expr's element-first `in`, exists becomes a nil test, and the six operators
// with no native spelling call a registered backing function.
//
// GUARDING (E5-S1) comes first. A collection operator whose collection operand
// could be the wrong shape at runtime renders to the guarded dispatcher instead —
// see renderGuarded — so a mistyped field denies with a note rather than raising.
func (n *Node) renderCompare(b *bytes.Buffer) error {
	spec, ok := opSpecs[n.Op]
	if !ok {
		return aerr.WithContext(aerr.APERTURE_RULE_INVALID,
			"rule: cannot render unknown comparison operator", map[string]any{"op": n.Op})
	}
	if cspec, ok := collOps[n.Op]; ok && guardNeeded(n, cspec) {
		return n.renderGuarded(b)
	}
	switch spec.kind {
	case renderInfix:
		b.WriteByte('(')
		if err := n.Left.render(b); err != nil {
			return err
		}
		b.WriteByte(' ')
		b.WriteString(spec.text)
		b.WriteByte(' ')
		if err := n.Right.render(b); err != nil {
			return err
		}
		b.WriteByte(')')
		return nil

	case renderFlippedIn:
		// No backing function is needed: expr's `in` already covers slice
		// membership AND map-key membership, and it is nil-safe (`x in nil` is
		// false, never an error).
		b.WriteByte('(')
		if err := n.Right.render(b); err != nil {
			return err
		}
		b.WriteString(" in ")
		if err := n.Left.render(b); err != nil {
			return err
		}
		b.WriteByte(')')
		return nil

	case renderNotNil:
		// Because renderVar emits optional chaining, a path through a missing
		// intermediate reads as nil, so this is false for an absent field rather
		// than a runtime error.
		b.WriteByte('(')
		if err := n.Left.render(b); err != nil {
			return err
		}
		b.WriteString(" != nil)")
		return nil

	default: // renderFunc — a call is atomic, so it needs no wrapping parens.
		b.WriteString(spec.text)
		b.WriteByte('(')
		if err := n.Left.render(b); err != nil {
			return err
		}
		if n.Right != nil {
			b.WriteString(", ")
			if err := n.Right.render(b); err != nil {
				return err
			}
		}
		b.WriteByte(')')
		return nil
	}
}

// guardNeeded reports whether a collection comparison must render to the guarded
// dispatcher rather than to its native/backing-function form.
//
// The only operand that can be the wrong shape at runtime is one whose value is
// not known statically. A LIST LITERAL is an array by construction, so a rule of
// the overwhelmingly common form `object.region in ["us", "eu"]` keeps its native
// `in` render: the element on the left carries no shape contract, the collection
// on the right cannot mismatch, and the decision path pays nothing. Anything else
// — a variable, a function call — is guarded.
func guardNeeded(n *Node, spec collOp) bool {
	if spec.left != expectNone && !alwaysCollection(n.Left) {
		return true
	}
	if spec.right != expectNone && !alwaysCollection(n.Right) {
		return true
	}
	return false
}

// alwaysCollection reports whether a node's value is statically guaranteed to be
// a collection. Only a list literal qualifies.
func alwaysCollection(n *Node) bool { return n != nil && n.Type == NodeList }

// renderGuarded writes a collection comparison as a call to the guarded
// dispatcher:
//
//	$op("hasAll", __notes, "object.tags", object?.tags, "", ["a", "b"])
//
// The arguments are, in order: the operator; the evaluation-notes sink; the left
// operand's dotted variable PATH (for the note; empty when the operand is not a
// plain variable) and its value; then the same pair for the right operand, which
// renders as ""/nil for a unary operator.
//
// Neither name is reachable from a rule. `$op` contains a character varPath
// rejects, so no NodeCall can ever name it; `__notes` is not one of allowedRoots,
// so no NodeVar can reference it. The guard is therefore structurally
// compiler-only rather than protected by a denylist that could drift.
//
// The dispatcher returns a bool, so a guarded comparison composes exactly like
// the native form — including as a whole rule under expr.AsBool.
func (n *Node) renderGuarded(b *bytes.Buffer) error {
	b.WriteString(fnCollectionOp)
	b.WriteByte('(')
	b.WriteString(strconv.Quote(n.Op))
	b.WriteString(", ")
	b.WriteString(notesVar)
	b.WriteString(", ")
	b.WriteString(strconv.Quote(varPathOf(n.Left)))
	b.WriteString(", ")
	if err := n.Left.render(b); err != nil {
		return err
	}
	b.WriteString(", ")
	b.WriteString(strconv.Quote(varPathOf(n.Right)))
	b.WriteString(", ")
	if n.Right == nil {
		b.WriteString("nil")
	} else if err := n.Right.render(b); err != nil {
		return err
	}
	b.WriteByte(')')
	return nil
}

// varPathOf returns the dotted path a note should name an operand by: the
// variable path when the operand is a plain variable reference, and "" otherwise
// (a note renders that as "(expression)").
func varPathOf(n *Node) string {
	if n == nil || n.Type != NodeVar {
		return ""
	}
	return n.Name
}

// renderVar writes a variable reference with optional chaining on every segment
// after the root: object.owner.dept renders as object?.owner?.dept.
//
// Why: reading through a MISSING intermediate is a runtime error in expr-lang
// ("cannot fetch dept from <nil>"), not nil — so a rule that works against every
// object carrying an owner blows up as APERTURE_RULE_EVAL on the one object that
// lacks it. Optional chaining makes a missing intermediate yield nil, which turns
// the enclosing comparison false, so nested reads are uniformly deny-safe and no
// author has to write a guard. A present path is unaffected: `?.` differs from
// `.` only when the receiver is nil.
//
// The root needs no chaining — it is a typed field of evalEnv and is always
// present (a nil metadata map still reads as an empty map, not a missing name).
//
// This is deliberately a RENDER-time concern only: the AST keeps storing plain
// dotted paths, so the JSON form the node editor and the state file share is
// untouched. Validate (varPath) has already proven every segment is a Go-style
// identifier, so splitting on '.' cannot produce an empty or injectable segment.
func renderVar(b *bytes.Buffer, path string) {
	for first := true; ; first = false {
		i := indexByte(path, '.')
		if !first {
			b.WriteString("?.")
		}
		if i < 0 {
			b.WriteString(path)
			return
		}
		b.WriteString(path[:i])
		path = path[i+1:]
	}
}

func renderJoined(b *bytes.Buffer, children []*Node, sep string) error {
	b.WriteByte('(')
	for i, c := range children {
		if i > 0 {
			b.WriteString(sep)
		}
		if err := c.render(b); err != nil {
			return err
		}
	}
	b.WriteByte(')')
	return nil
}

func renderLiteral(b *bytes.Buffer, raw json.RawMessage) error {
	// FAST PATH, the render twin of validateLiteral's. Each classified form is
	// written straight from the raw bytes, which is byte-for-byte what the decode
	// path below produces — see classifyScalar for why that identity holds for a
	// number (json.Number.String() IS the token text) and for a plain string
	// (strconv.Quote of an unescaped printable-ASCII body IS the JSON token).
	// TestRenderLiteralFastPathMatchesDecoder pins the equivalence.
	switch kind, tok := classifyScalar(raw); kind {
	case scalarNull:
		b.WriteString("nil")
		return nil
	case scalarTrue:
		b.WriteString("true")
		return nil
	case scalarFalse:
		b.WriteString("false")
		return nil
	case scalarNumber, scalarPlainString:
		b.Write(tok)
		return nil
	}

	v, err := decodeScalar(raw)
	if err != nil {
		return err
	}
	switch x := v.(type) {
	case nil:
		b.WriteString("nil")
	case bool:
		if x {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case json.Number:
		b.WriteString(x.String())
	case string:
		b.WriteString(strconv.Quote(x))
	default:
		return aerr.New(aerr.APERTURE_RULE_INVALID, "rule: literal is not a scalar")
	}
	return nil
}

// scalarKind is what classifyScalar was able to prove about a literal's raw
// JSON bytes. It is a strictly OPTIONAL fast path: scalarUnclassified means
// "not proven", never "invalid", and every caller falls back to decodeScalar.
type scalarKind uint8

const (
	// scalarUnclassified means the bytes were not proven to be one of the forms
	// below. It carries no verdict — the caller must run decodeScalar.
	scalarUnclassified scalarKind = iota
	scalarNull
	scalarTrue
	scalarFalse
	// scalarNumber is a token matching the JSON number grammar exactly.
	scalarNumber
	// scalarPlainString is a JSON string whose body is printable ASCII with no
	// escape sequence and no embedded quote — the form whose JSON spelling and
	// Go strconv.Quote spelling are identical.
	scalarPlainString
)

// classifyScalar identifies a literal's raw JSON without decoding it, returning
// the kind and the trimmed token.
//
// WHY THIS EXISTS. Node.Validate and Node.render each decode every literal in
// the AST, and rules.Engine.compile runs both on EVERY decision before it can
// probe the compiled-program cache (issue #9). Decoding through
// json.NewDecoder costs ~7 allocations and ~2.4 KB per literal per call — the
// decoder's internal read buffer dwarfs the value it is parsing — so a rule's
// per-Check cost scaled with its literal count for no reason: the answer is the
// same every time.
//
// SAFETY. The classifier is one-sided. It returns a kind ONLY when the bytes
// are provably that kind under the JSON grammar; every uncertain case (escaped
// or non-ASCII strings, composites, malformed input, trailing garbage) returns
// scalarUnclassified and the caller decodes as before. So it can never widen
// what Validate accepts, and it can never change what render emits.
func classifyScalar(raw []byte) (scalarKind, []byte) {
	tok := trimJSONSpace(raw)
	if len(tok) == 0 {
		return scalarUnclassified, nil
	}
	switch c := tok[0]; {
	case c == 'n':
		if string(tok) == "null" {
			return scalarNull, tok
		}
	case c == 't':
		if string(tok) == "true" {
			return scalarTrue, tok
		}
	case c == 'f':
		if string(tok) == "false" {
			return scalarFalse, tok
		}
	case c == '"':
		if isPlainJSONString(tok) {
			return scalarPlainString, tok
		}
	case c == '-' || (c >= '0' && c <= '9'):
		if isJSONNumber(tok) {
			return scalarNumber, tok
		}
	}
	return scalarUnclassified, nil
}

func isJSONSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

// trimJSONSpace strips the four JSON whitespace bytes from both ends. A
// RawMessage produced by encoding/json carries none, but a hand-built Node may.
func trimJSONSpace(b []byte) []byte {
	i, j := 0, len(b)
	for i < j && isJSONSpace(b[i]) {
		i++
	}
	for j > i && isJSONSpace(b[j-1]) {
		j--
	}
	return b[i:j]
}

// isPlainJSONString reports whether tok is a JSON string whose body contains
// only printable ASCII other than '"' and '\'.
//
// That restriction is what makes the render fast path exact rather than merely
// close: with no escape to expand and no byte outside 0x20-0x7E, the decoded Go
// string is the body verbatim, and strconv.Quote of such a string re-emits it
// unchanged between the same two quotes. Anything else — an escape, a control
// byte, any non-ASCII rune — is left to the decoder, whose output Quote may
// legitimately re-spell. json.Marshal HTML-escapes '<' to the six-byte sequence
// backslash-u-0-0-3-c, for instance, so Lit("a<b") carries a backslash, takes
// the slow path, and renders exactly as it always has.
func isPlainJSONString(tok []byte) bool {
	if len(tok) < 2 || tok[len(tok)-1] != '"' {
		return false
	}
	for _, c := range tok[1 : len(tok)-1] {
		if c < 0x20 || c > 0x7e || c == '"' || c == '\\' {
			return false
		}
	}
	return true
}

// isJSONNumber reports whether tok is exactly one JSON number and nothing else:
// -?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?, consuming the whole token.
//
// It is deliberately the strict grammar. Anything it turns down (a leading '+',
// a bare '.', a leading zero, trailing bytes) falls back to the decoder, so a
// false negative costs an allocation and nothing else; a false POSITIVE would
// be a correctness bug, which is why nothing here is approximate.
func isJSONNumber(tok []byte) bool {
	i := 0
	if i < len(tok) && tok[i] == '-' {
		i++
	}
	switch {
	case i >= len(tok):
		return false
	case tok[i] == '0':
		i++
	case tok[i] >= '1' && tok[i] <= '9':
		for i < len(tok) && tok[i] >= '0' && tok[i] <= '9' {
			i++
		}
	default:
		return false
	}
	if i < len(tok) && tok[i] == '.' {
		i++
		start := i
		for i < len(tok) && tok[i] >= '0' && tok[i] <= '9' {
			i++
		}
		if i == start {
			return false
		}
	}
	if i < len(tok) && (tok[i] == 'e' || tok[i] == 'E') {
		i++
		if i < len(tok) && (tok[i] == '+' || tok[i] == '-') {
			i++
		}
		start := i
		for i < len(tok) && tok[i] >= '0' && tok[i] <= '9' {
			i++
		}
		if i == start {
			return false
		}
	}
	return i == len(tok)
}

// decodeScalar decodes a literal's raw JSON into a scalar (nil, bool, json.Number,
// or string), rejecting composites. UseNumber keeps integer literals exact so a
// rule round-trips and renders without float reformatting.
//
// This is the SLOW path — classifyScalar handles the common forms without it —
// but it remains the definition of what a literal may be: the fast path only
// ever short-circuits inputs this function would have accepted.
func decodeScalar(raw json.RawMessage) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, aerr.Wrap(aerr.APERTURE_RULE_INVALID, "rule: literal is not valid JSON", err)
	}
	switch v.(type) {
	case nil, bool, json.Number, string:
		return v, nil
	default:
		return nil, aerr.New(aerr.APERTURE_RULE_INVALID,
			"rule: literal must be a scalar (string, number, bool, or null); use a list node for collections")
	}
}
