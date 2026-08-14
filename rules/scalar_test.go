package rules

import (
	"bytes"
	"encoding/json"
	"strconv"
	"testing"

	aerr "github.com/frankbardon/aperture/errors"
)

// This file guards the literal fast path (issue #9): classifyScalar lets
// Validate and render skip json.NewDecoder for the literal forms that dominate
// real rules, which is the single largest term in the AST walk rules.Engine
// re-runs on every decision.
//
// The fast path is only ever safe because it is ONE-SIDED — it must agree with
// the decoder wherever it answers at all, and defer wherever it does not. Every
// test here is a statement of that property rather than of a specific output, so
// the guarantee survives someone widening the classifier later.

// scalarCorpus is the input set the equivalence tests run over: the forms the
// classifier is expected to take, the forms it must decline, and the malformed
// bytes it must not accidentally bless.
//
// `classified` records which PATH an input is expected to take, not whether it
// is valid. Validity is deliberately never hand-written here: decodeScalar is
// the definition, and it has quirks worth preserving rather than re-deriving —
// json.Decoder.Decode reads a single value and ignores whatever follows, so
// `nullable` and `0x10` are accepted today as `null` and `0`. The tests compare
// the two paths against each other instead, which is the property that has to
// hold.
var scalarCorpus = []struct {
	name       string
	raw        string
	classified bool
}{
	// Plain classified forms — the hot path.
	{"string", `"public"`, true},
	{"string empty", `""`, true},
	{"string with spaces", `"us east"`, true},
	{"string punctuation", `"a-b_c.d:e/f"`, true},
	{"string single quote", `"it's"`, true},
	{"string tilde high ascii", `"~"`, true},
	{"string space only", `" "`, true},
	{"int", `3`, true},
	{"int zero", `0`, true},
	{"int negative", `-17`, true},
	{"int large exact", `9007199254740993`, true},
	{"float", `3.25`, true},
	{"float negative", `-0.5`, true},
	{"exponent lower", `1e5`, true},
	{"exponent upper signed", `2.5E-3`, true},
	{"true", `true`, true},
	{"false", `false`, true},
	{"null", `null`, true},
	{"leading whitespace", "  \n\t\"padded\"", true},
	{"trailing whitespace", "\"padded\"  \n", true},

	// Valid, but deliberately NOT classified: an escape or a non-ASCII byte means
	// the JSON spelling and strconv.Quote's spelling can differ, so the decoder
	// decides.
	{"string escaped quote", `"say \"hi\""`, false},
	{"string escaped newline", `"a\nb"`, false},
	{"string escaped backslash", `"a\\b"`, false},
	{"string unicode escape", `"a\u003cb"`, false},
	{"string non-ascii", `"café"`, false},
	{"string emoji", `"🚀"`, false},

	// Composites: rejected as non-scalars, and the classifier must not touch them.
	{"array", `[1,2]`, false},
	{"object", `{"a":1}`, false},
	{"empty array", `[]`, false},
	{"nested array", `[[1]]`, false},

	// Malformed or ambiguous JSON. The classifier must decline every one of
	// these — several of which the decoder nonetheless accepts by truncation.
	{"bare word", `nope`, false},
	{"truthy prefix", `truthy`, false},
	{"nullish prefix", `nullable`, false},
	{"falsey prefix", `falsey`, false},
	{"unterminated string", `"abc`, false},
	{"quote only", `"`, false},
	{"leading plus", `+1`, false},
	{"leading zero", `01`, false},
	{"bare minus", `-`, false},
	{"bare dot", `.5`, false},
	{"trailing dot", `1.`, false},
	{"exponent no digits", `1e`, false},
	{"exponent sign only", `1e+`, false},
	{"double minus", `--1`, false},
	{"hex", `0x10`, false},
	{"infinity", `Infinity`, false},
	{"nan", `NaN`, false},
	{"single quoted", `'x'`, false},
	{"empty", ``, false},
}

// renderLiteralViaDecoder is the pre-fast-path renderer, kept verbatim as the
// ORACLE. Every byte the fast path produces must match what this produces, or
// the AST's canonical form — and with it the compiled-rule cache key and the
// byte-identical round-trip guarantee — has silently moved.
func renderLiteralViaDecoder(b *bytes.Buffer, raw json.RawMessage) error {
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

// assertRenderPathsAgree checks the fast path and the decoder produce the same
// error status, the same error code, and the same bytes for raw.
func assertRenderPathsAgree(t *testing.T, raw string) {
	t.Helper()
	msg := json.RawMessage(raw)

	var fast, oracle bytes.Buffer
	fastErr := renderLiteral(&fast, msg)
	oracleErr := renderLiteralViaDecoder(&oracle, msg)

	if (fastErr == nil) != (oracleErr == nil) {
		t.Fatalf("render disagreement for %s: fast err=%v, decoder err=%v", raw, fastErr, oracleErr)
	}
	if fastErr != nil {
		if aerr.CodeOf(fastErr) != aerr.CodeOf(oracleErr) {
			t.Fatalf("error code disagreement for %s: fast %q, decoder %q",
				raw, aerr.CodeOf(fastErr), aerr.CodeOf(oracleErr))
		}
		return
	}
	if fast.String() != oracle.String() {
		t.Fatalf("render mismatch for %s:\n fast    = %s\n decoder = %s",
			raw, fast.String(), oracle.String())
	}
}

// TestRenderLiteralFastPathMatchesDecoder is the equivalence proof: for every
// input, the fast path and the decoder must agree on both the error and the
// rendered bytes. A divergence would change a rule's canonical expression
// without changing its AST.
func TestRenderLiteralFastPathMatchesDecoder(t *testing.T) {
	for _, tc := range scalarCorpus {
		t.Run(tc.name, func(t *testing.T) {
			assertRenderPathsAgree(t, tc.raw)
		})
	}
}

// TestValidateLiteralFastPathMatchesDecoder pins the other half: the classifier
// must never widen — or narrow — what Validate accepts. It is compared against
// the decoder rather than against a hand-written verdict, so the pre-existing
// acceptance set is preserved exactly, quirks included.
func TestValidateLiteralFastPathMatchesDecoder(t *testing.T) {
	for _, tc := range scalarCorpus {
		t.Run(tc.name, func(t *testing.T) {
			msg := json.RawMessage(tc.raw)

			fastErr := validateLiteral(msg)
			_, oracleErr := decodeScalar(msg)
			if len(tc.raw) == 0 {
				// The empty case is refused before either path runs.
				if fastErr == nil {
					t.Fatal("validateLiteral accepted an empty value")
				}
				return
			}
			if (fastErr == nil) != (oracleErr == nil) {
				t.Fatalf("validateLiteral(%s) err=%v but the decoder says %v", tc.raw, fastErr, oracleErr)
			}
			if fastErr != nil && aerr.CodeOf(fastErr) != aerr.APERTURE_RULE_INVALID {
				t.Fatalf("validateLiteral(%s) code = %q, want APERTURE_RULE_INVALID", tc.raw, aerr.CodeOf(fastErr))
			}
		})
	}
}

// TestClassifyScalarTakesTheExpectedPath pins WHICH inputs get the fast path.
// The equivalence tests above would still pass if the classifier declined
// everything, so without this the performance claim is untested — and the
// one-sidedness check here is what makes a classified verdict load-bearing.
func TestClassifyScalarTakesTheExpectedPath(t *testing.T) {
	for _, tc := range scalarCorpus {
		t.Run(tc.name, func(t *testing.T) {
			msg := json.RawMessage(tc.raw)
			kind, tok := classifyScalar(msg)
			if got := kind != scalarUnclassified; got != tc.classified {
				t.Fatalf("classifyScalar(%s) classified=%v (kind %d), want %v", tc.raw, got, kind, tc.classified)
			}
			if kind == scalarUnclassified {
				return
			}
			if len(tok) == 0 {
				t.Fatalf("classifyScalar(%s) returned kind %d with an empty token", tc.raw, kind)
			}
			// One-sidedness: a classified verdict implies the decoder accepts.
			if _, err := decodeScalar(msg); err != nil {
				t.Fatalf("classifyScalar(%s) = %d but the decoder rejects it: %v", tc.raw, kind, err)
			}
		})
	}
}

// TestClassifiedLiteralsAllocateNothing is the measured half of issue #9 at unit
// scale: validating a literal is what rules.Engine.compile pays per literal on
// EVERY decision, and for a classified form it must now cost zero allocations.
// Before the fast path each of these cost roughly seven — a json.Decoder and its
// read buffer, built and thrown away immediately.
func TestClassifiedLiteralsAllocateNothing(t *testing.T) {
	for _, raw := range []string{`"public"`, `3`, `-1.5e3`, `true`, `false`, `null`} {
		t.Run(raw, func(t *testing.T) {
			msg := json.RawMessage(raw)
			if err := validateLiteral(msg); err != nil {
				t.Fatalf("validateLiteral(%s): %v", raw, err)
			}
			if got := testing.AllocsPerRun(200, func() {
				if err := validateLiteral(msg); err != nil {
					t.Fatal(err)
				}
			}); got != 0 {
				t.Errorf("validateLiteral(%s) allocates %.0f times per call; the classified "+
					"path must not reach the decoder (issue #9)", raw, got)
			}
		})
	}
}

// TestClassifyScalarRejectsTrailingContent is called out separately because the
// DECODER does not reject these: json.Decoder.Decode reads one value and ignores
// whatever follows. The classifier consumes the whole token or declines, so it
// never inherits that leniency by accident — and either way both paths still
// have to agree, which is the second half of what this asserts.
func TestClassifyScalarRejectsTrailingContent(t *testing.T) {
	for _, raw := range []string{`1 2`, `true false`, `"a" "b"`, `null null`, `1,`, `"a":1`} {
		t.Run(raw, func(t *testing.T) {
			if kind, _ := classifyScalar(json.RawMessage(raw)); kind != scalarUnclassified {
				t.Fatalf("classifyScalar(%s) = %d, want unclassified: the token is not self-delimiting", raw, kind)
			}
			assertRenderPathsAgree(t, raw)
		})
	}
}

// FuzzClassifyScalar states the safety property directly, so the corpus above is
// a starting point rather than the whole guarantee: whenever the classifier
// answers, the decoder must accept the same bytes AND the two must render
// identically. Nothing is asserted about inputs it declines — declining is
// always allowed.
func FuzzClassifyScalar(f *testing.F) {
	for _, tc := range scalarCorpus {
		f.Add(tc.raw)
	}
	f.Add(`"\ud83d\ude00"`)
	f.Add(`1e309`)
	f.Add(`-0`)

	f.Fuzz(func(t *testing.T, raw string) {
		msg := json.RawMessage(raw)
		kind, _ := classifyScalar(msg)
		if kind == scalarUnclassified {
			return
		}
		if _, err := decodeScalar(msg); err != nil {
			t.Fatalf("classified %q as kind %d but the decoder rejects it: %v", raw, kind, err)
		}
		var fast, oracle bytes.Buffer
		if err := renderLiteral(&fast, msg); err != nil {
			t.Fatalf("classified %q but rendering failed: %v", raw, err)
		}
		if err := renderLiteralViaDecoder(&oracle, msg); err != nil {
			t.Fatalf("decoder render failed for classified %q: %v", raw, err)
		}
		if fast.String() != oracle.String() {
			t.Fatalf("render mismatch for %q: fast = %s, decoder = %s", raw, fast.String(), oracle.String())
		}
	})
}
