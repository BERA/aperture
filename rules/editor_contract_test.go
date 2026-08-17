package rules

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// TestEditorASTContract guards the E7-S2 node-editor <-> AST contract from the
// Go side. The JS serializer (internal/server/static/js/rules-serializer.js)
// emits these exact JSON shapes; this test parses the same representative ASTs
// through rules.Node and asserts each is (a) structurally valid and (b) stable
// under marshal->unmarshal->marshal. Because the JS round-trip test is not in
// the Go-only CI path, this is the CI-guarded half of the invariant: if the AST
// JSON shape ever drifts from what the editor produces, one side breaks here.
//
// The cases mirror rules-serializer.test.js one-for-one (nested and/or/not,
// compare with var+literal, in/nin with a list and with a var, a call, the nine
// collection operators including the three unary ones, and the falsy-scalar edge
// cases false/0/""/null that must survive omitempty).
func TestEditorASTContract(t *testing.T) {
	cases := map[string]string{
		"compare var eq string": `{"type":"compare","op":"eq","left":{"type":"var","name":"object.classification"},"right":{"type":"literal","value":"public"}}`,

		"nested and/or/not": `{"type":"and","children":[` +
			`{"type":"or","children":[` +
			`{"type":"compare","op":"gt","left":{"type":"var","name":"object.level"},"right":{"type":"literal","value":3}},` +
			`{"type":"compare","op":"eq","left":{"type":"var","name":"principal.tier"},"right":{"type":"literal","value":"gold"}}]},` +
			`{"type":"not","children":[` +
			`{"type":"compare","op":"eq","left":{"type":"var","name":"account.suspended"},"right":{"type":"literal","value":true}}]}]}`,

		"in with list literal": `{"type":"compare","op":"in","left":{"type":"var","name":"object.region"},"right":{"type":"list","items":[{"type":"literal","value":"us"},{"type":"literal","value":"eu"},{"type":"literal","value":"apac"}]}}`,

		"nin with var on right": `{"type":"compare","op":"nin","left":{"type":"var","name":"principal.id"},"right":{"type":"var","name":"object.blocklist"}}`,

		"call len ge number": `{"type":"compare","op":"ge","left":{"type":"call","name":"len","items":[{"type":"var","name":"object.tags"}]},"right":{"type":"literal","value":1}}`,

		// The nine collection operators (E4-S1). The six binary ones keep the
		// left/right shape; the three unary ones (isEmpty, isNotEmpty, exists)
		// carry NO "right" key at all — that omission is the contract, so a rule
		// authored as "is empty" reads back as "is empty".
		"has element": `{"type":"compare","op":"has","left":{"type":"var","name":"object.tags"},"right":{"type":"literal","value":"urgent"}}`,

		"hasAll list": `{"type":"compare","op":"hasAll","left":{"type":"var","name":"object.tags"},"right":{"type":"list","items":[{"type":"literal","value":"a"},{"type":"literal","value":"b"}]}}`,

		"hasAny list": `{"type":"compare","op":"hasAny","left":{"type":"var","name":"object.tags"},"right":{"type":"list","items":[{"type":"literal","value":"a"},{"type":"literal","value":"b"}]}}`,

		"hasNone list": `{"type":"compare","op":"hasNone","left":{"type":"var","name":"object.tags"},"right":{"type":"list","items":[{"type":"literal","value":"a"}]}}`,

		"subsetOf var": `{"type":"compare","op":"subsetOf","left":{"type":"var","name":"object.tags"},"right":{"type":"var","name":"principal.allowedTags"}}`,

		"hasKey": `{"type":"compare","op":"hasKey","left":{"type":"var","name":"object.owner"},"right":{"type":"literal","value":"dept"}}`,

		"isEmpty unary":    `{"type":"compare","op":"isEmpty","left":{"type":"var","name":"object.tags"}}`,
		"isNotEmpty unary": `{"type":"compare","op":"isNotEmpty","left":{"type":"var","name":"object.tags"}}`,
		"exists unary":     `{"type":"compare","op":"exists","left":{"type":"var","name":"object.owner.dept"}}`,

		"collection ops nested under and/or": `{"type":"and","children":[` +
			`{"type":"compare","op":"exists","left":{"type":"var","name":"object.tags"}},` +
			`{"type":"or","children":[` +
			`{"type":"compare","op":"hasAny","left":{"type":"var","name":"object.tags"},"right":{"type":"list","items":[{"type":"literal","value":"gold"}]}},` +
			`{"type":"not","children":[{"type":"compare","op":"isEmpty","left":{"type":"var","name":"object.owner"}}]}]}]}`,

		// The relative-date operand. All four of its fields are always present —
		// "no offset" is n:0 and "no snap" is "none" — and the key order below is
		// the struct's, so a drift in either would break byte-stability here.
		// Both anchors, several units, and several snaps are covered.
		"relative date months ago": `{"type":"compare","op":"onOrAfter","left":{"type":"var","name":"object.touched_at"},` +
			`"right":{"type":"relativeDate","anchor":"NOW","n":-3,"unit":"months","snap":"none"}}`,

		"relative date today start of year": `{"type":"compare","op":"onOrAfter","left":{"type":"var","name":"object.touched_at"},` +
			`"right":{"type":"relativeDate","anchor":"TODAY","n":0,"unit":"days","snap":"startOfYear"}}`,

		"relative date forward hours": `{"type":"compare","op":"before","left":{"type":"var","name":"object.expires_at"},` +
			`"right":{"type":"relativeDate","anchor":"NOW","n":12,"unit":"hours","snap":"none"}}`,

		"relative date end of quarter": `{"type":"compare","op":"onOrBefore","left":{"type":"var","name":"object.due_at"},` +
			`"right":{"type":"relativeDate","anchor":"TODAY","n":1,"unit":"quarters","snap":"endOfQuarter"}}`,

		"relative date same year": `{"type":"compare","op":"sameYear","left":{"type":"var","name":"object.hired_at"},` +
			`"right":{"type":"relativeDate","anchor":"NOW","n":-1,"unit":"years","snap":"startOfWeek"}}`,

		// Year to date plus five years of history: one between whose lower bound
		// is relative and whose upper bound is the anchor itself.
		"between relative bounds": `{"type":"compare","op":"between","left":{"type":"var","name":"object.hired_at"},` +
			`"right":{"type":"list","items":[` +
			`{"type":"relativeDate","anchor":"NOW","n":-5,"unit":"years","snap":"startOfYear"},` +
			`{"type":"relativeDate","anchor":"TODAY","n":0,"unit":"days","snap":"endOfDay"}]}}`,

		"between literal and relative": `{"type":"compare","op":"between","left":{"type":"var","name":"object.hired_at"},` +
			`"right":{"type":"list","items":[` +
			`{"type":"literal","value":"2026-01-01"},` +
			`{"type":"relativeDate","anchor":"NOW","n":-30,"unit":"minutes","snap":"none"}]}}`,

		"relative date nested under and": `{"type":"and","children":[` +
			`{"type":"compare","op":"eq","left":{"type":"var","name":"object.classification"},"right":{"type":"literal","value":"public"}},` +
			`{"type":"compare","op":"after","left":{"type":"var","name":"object.touched_at"},` +
			`"right":{"type":"relativeDate","anchor":"NOW","n":-90,"unit":"days","snap":"startOfDay"}}]}`,

		"falsy false": `{"type":"compare","op":"eq","left":{"type":"var","name":"object.archived"},"right":{"type":"literal","value":false}}`,
		"falsy zero":  `{"type":"compare","op":"eq","left":{"type":"var","name":"object.count"},"right":{"type":"literal","value":0}}`,
		"falsy empty": `{"type":"compare","op":"ne","left":{"type":"var","name":"object.note"},"right":{"type":"literal","value":""}}`,
		"falsy null":  `{"type":"compare","op":"eq","left":{"type":"var","name":"object.owner"},"right":{"type":"literal","value":null}}`,
	}

	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			assertEditorJSONRoundTrips(t, src)
		})
	}

	// The handwritten table above documents what the editor actually emits; it
	// is not proof that every operator is covered. That is
	// TestEditorASTContractCoversEveryOperator's job (editor_js_contract_test.go),
	// which drives the same assertion off opSpecs itself.
	for op := range opSpecs {
		if !opAppearsIn(cases, op) {
			t.Logf("operator %q has no handwritten editor case; "+
				"TestEditorASTContractCoversEveryOperator covers it generically", op)
		}
	}
}

// opAppearsIn reports whether any case JSON carries `"op":"<op>"`.
func opAppearsIn(cases map[string]string, op string) bool {
	needle := `"op":"` + op + `"`
	for _, src := range cases {
		if strings.Contains(src, needle) {
			return true
		}
	}
	return false
}

// assertEditorJSONRoundTrips is the single assertion both contract tests share:
// the editor's JSON parses into a rules.Node, validates, and marshals back
// BYTE-IDENTICALLY. Byte identity is the real contract — the falsy scalars
// (false/0/""/null) are non-empty RawMessage and must not be dropped by
// omitempty, and the unary operators must not gain a `right` key.
func assertEditorJSONRoundTrips(t *testing.T, src string) {
	t.Helper()
	var n Node
	if err := json.Unmarshal([]byte(src), &n); err != nil {
		t.Fatalf("unmarshal editor JSON: %v", err)
	}
	if err := n.Validate(); err != nil {
		t.Fatalf("editor AST failed validation: %v", err)
	}
	out, err := json.Marshal(&n)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Equal([]byte(src), out) {
		t.Errorf("editor AST is not byte-stable\n  in:  %s\n  out: %s", src, out)
	}
}
