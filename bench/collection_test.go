package bench

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"runtime"
	"testing"
	"time"

	"github.com/frankbardon/aperture/engine"
	"github.com/frankbardon/aperture/identity"
	"github.com/frankbardon/aperture/provider"
	"github.com/frankbardon/aperture/rules"
	"github.com/frankbardon/aperture/scope"
	"github.com/frankbardon/aperture/service"
)

// E5-S2: collection and nested-object rules on the cached-Check hot path.
//
// Everything before this file measured SCALAR metadata through the literal scope
// path, which never reaches the provider registry or the rules engine at all. The
// metadata-collections effort put array iteration, map traversal, and the E5-S1
// guarded dispatcher on that path, and the 64 KiB per-value cap admits arrays of
// thousands of elements — a dimension nothing measured. These cases add it.
//
// Both E5-S1 render strategies are covered, because they are materially
// different code paths:
//
//   - a collection comparison over a VARIABLE renders to the guarded dispatcher
//     $op(...), a registered backing function that classifies both operands
//     against the runtime shape policy before deciding (rule-tags-typical,
//     rule-tags-at-cap);
//   - a comparison against a LIST LITERAL keeps its native render, so the
//     overwhelmingly common `object.region in [...]` shape pays nothing extra
//     (rule-literal-in).
//
// rule-scalar is the control every other number is read against, and rule-nested
// covers E2-S2's optional-chaining render (object?.owner?.dept).

// newRuleService builds the decision facade WITH scope resolution wired, so a
// grant's inclusive;rule= strategy actually consults the rules engine and the
// provider registry. The literal-scope benchmarks keep using newService — an
// engine only consults scope resolvers when built WithScopeResolution, so the
// baseline numbers are unaffected by this fixture existing.
func newRuleService(tb testing.TB, m benchModel, withAudit bool) (*service.Service, func()) {
	tb.Helper()
	eng := engine.New(m.store, engine.WithScopeResolution(
		scope.DefaultRegistry(),
		engine.ScopeDeps{Lister: m.registry, Rules: m.rules},
	))
	if !withAudit {
		return service.New(eng), func() {}
	}
	return newAuditedService(tb, m.store, eng)
}

// ruleQuery returns the Check query for a rule variant, failing loudly rather
// than silently benchmarking a zero request if the fixture ever stops seeding it.
func ruleQuery(tb testing.TB, m benchModel, variant string) service.Query {
	tb.Helper()
	req, ok := m.ruleReqs[variant]
	if !ok {
		tb.Fatalf("fixture has no rule variant %q", variant)
	}
	return toQuery(req)
}

// TestCollectionFixtureSitsAtTheValueCap pins the derivation behind
// tagsAtCapLen: it is the LARGEST array the default per-field cap admits at the
// fixture's tag width, not a round number. One more tag must breach the cap.
//
// This runs in the default `make test` — it is pure arithmetic over the value
// model, with no wall clock in it.
func TestCollectionFixtureSitsAtTheValueCap(t *testing.T) {
	tags := benchTags(tagsAtCapLen)
	bytesAtCap := provider.ValueBytes(tags)
	if bytesAtCap > provider.DefaultMaxValueBytes {
		t.Fatalf("tagsAtCapLen=%d measures %d bytes, over the %d-byte cap",
			tagsAtCapLen, bytesAtCap, provider.DefaultMaxValueBytes)
	}
	if err := provider.ValidateField("tagsAtCap", tags); err != nil {
		t.Fatalf("cap-sized tag array should be a legal metadata value: %v", err)
	}
	// One more element must not fit: that is what makes this the maximum.
	oneMore := provider.ValueBytes(append(tags, benchTag(tagsAtCapLen)))
	if oneMore <= provider.DefaultMaxValueBytes {
		t.Fatalf("tagsAtCapLen=%d is not maximal: %d tags still measure %d bytes, within the %d-byte cap",
			tagsAtCapLen, tagsAtCapLen+1, oneMore, provider.DefaultMaxValueBytes)
	}
	t.Logf("tagsAtCapLen = %d tags = %d bytes of the %d-byte per-value cap (%d tags would be %d)",
		tagsAtCapLen, bytesAtCap, provider.DefaultMaxValueBytes, tagsAtCapLen+1, oneMore)

	if got := provider.ValueBytes(benchTags(tagsTypicalLen)); got > provider.DefaultMaxValueBytes/100 {
		t.Fatalf("tagsTypicalLen is meant to be a realistic list, not a large one: %d bytes", got)
	}
}

// BenchmarkCheckRule reports ns/op and allocs/op for a cached Check per rule
// variant, on the audit-off path. `make bench` picks it up with no target change.
func BenchmarkCheckRule(b *testing.B) {
	m := buildModel(b)
	svc, closeFn := newRuleService(b, m, false)
	defer closeFn()
	ctx := context.Background()

	for _, v := range ruleVariants() {
		b.Run(v.name, func(b *testing.B) {
			q := ruleQuery(b, m, v.name)
			warm(b, svc, q, v.allow)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				res, err := svc.Check(ctx, q)
				if err != nil || res.Allow != v.allow {
					b.Fatalf("Check(%s): allow=%v want %v err=%v", v.name, res.Allow, v.allow, err)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(measureP99(ctx, svc, q, 20000).Nanoseconds()), "p99-ns")
		})
	}
}

// BenchmarkCheckRuleThroughput reports sustained parallel throughput per rule
// variant, the checks/sec half of the NFR.
func BenchmarkCheckRuleThroughput(b *testing.B) {
	m := buildModel(b)
	svc, closeFn := newRuleService(b, m, false)
	defer closeFn()

	for _, v := range ruleVariants() {
		b.Run(v.name, func(b *testing.B) {
			q := ruleQuery(b, m, v.name)
			warm(b, svc, q, v.allow)
			b.ReportAllocs()
			b.ResetTimer()
			start := time.Now()
			b.RunParallel(func(pb *testing.PB) {
				ctx := context.Background()
				for pb.Next() {
					res, err := svc.Check(ctx, q)
					if err != nil || res.Allow != v.allow {
						b.Fatalf("Check(%s): allow=%v want %v err=%v", v.name, res.Allow, v.allow, err)
					}
				}
			})
			b.StopTimer()
			if elapsed := time.Since(start); elapsed > 0 {
				b.ReportMetric(float64(b.N)/elapsed.Seconds(), "checks/sec")
			}
		})
	}
}

// BenchmarkRuleEval isolates the RULE EVALUATION cost from the surrounding
// decision engine. BenchmarkCheckRule measures the whole facade, where the
// fixture's subject/grant resolution dominates and would mask a change in the
// rule itself; this compiles each variant once and evaluates it directly over the
// same metadata, so the number is the cost the collection dimension actually
// adds to a Check.
func BenchmarkRuleEval(b *testing.B) {
	ctx := context.Background()
	md := benchMetadata()
	compiler := rules.NewCompiler()

	for _, v := range ruleVariants() {
		b.Run(v.name, func(b *testing.B) {
			compiled, err := compiler.Compile(v.ast)
			if err != nil {
				b.Fatalf("compile %s: %v", v.name, err)
			}
			// Now is what an Engine supplies at every decision; a variant with a
			// relative-date operand resolves to nothing without it and would
			// measure the deny path rather than the arithmetic. It is the SAME
			// pinned instant the Check-path fixture's clock reports, so the two
			// halves of the measurement decide the same comparisons.
			in := rules.Input{
				Object:    md,
				Principal: map[string]any{"id": user(0)},
				Action:    v.name,
				Now:       benchNow,
			}
			ok, err := compiled.Eval(ctx, in)
			if err != nil || ok != v.allow {
				b.Fatalf("%s: eval=%v want %v err=%v (source: %s)", v.name, ok, v.allow, err, compiled.Source())
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := compiled.Eval(ctx, in); err != nil {
					b.Fatalf("%s: %v", v.name, err)
				}
			}
		})
	}
}

// BenchmarkCollectionScaling sweeps the guarded collection operator across array
// sizes so the cost is readable as a CURVE rather than as two points.
//
// This is the benchmark the "is the 64 KiB per-value cap too generous?" question
// is answered from: divide the remaining per-Check budget (the NFR floor of
// 10 000 checks/sec is 100 µs/Check, of which the fixture's own resolution
// already spends ~81 µs) by the per-element cost this reports, and the result is
// the largest array a rule can traverse and still clear the floor.
func BenchmarkCollectionScaling(b *testing.B) {
	ctx := context.Background()
	compiler := rules.NewCompiler()

	// Powers-of-two waypoints up to the cap-maximal length, so the per-element
	// cost is visible across three orders of magnitude.
	sizes := []int{tagsTypicalLen, 128, 1024, 4096, tagsAtCapLen}
	for _, n := range sizes {
		b.Run(fmt.Sprintf("tags-%d", n), func(b *testing.B) {
			md := provider.Metadata{"tags": benchTags(n)}
			// Match on the LAST element: the operator short-circuits on the first
			// hit, so an early match would measure a best case.
			ast := rules.Compare(rules.OpHasAny, rules.Var("object.tags"),
				rules.List(rules.Lit(benchTag(n-1))))
			compiled, err := compiler.Compile(ast)
			if err != nil {
				b.Fatalf("compile: %v", err)
			}
			in := rules.Input{Object: md}
			if ok, err := compiled.Eval(ctx, in); err != nil || !ok {
				b.Fatalf("eval=%v err=%v", ok, err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := compiled.Eval(ctx, in); err != nil {
					b.Fatalf("%v", err)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(provider.ValueBytes(md["tags"])), "field-bytes")
		})
	}
}

// BenchmarkRuleCompileCached measures the AST work a Check pays on EVERY
// decision even when the compiled program is already cached.
//
// rules.Engine.Selected calls Engine.Compile per evaluation, and Compile
// re-validates the AST, re-renders it to expression source, and re-hashes that
// source before it can probe the compiled-program cache. So the cache removes
// the expr-lang compile, not the AST walk — and the AST walk's cost scales with
// NODE COUNT. Reporting it beside BenchmarkRuleEval is what separates "this
// operator is expensive" from "this rule has more nodes".
func BenchmarkRuleCompileCached(b *testing.B) {
	m := buildModel(b)
	for _, v := range ruleVariants() {
		b.Run(v.name, func(b *testing.B) {
			if _, err := m.rules.Compile(v.ast); err != nil { // prime the cache
				b.Fatalf("compile %s: %v", v.name, err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := m.rules.Compile(v.ast); err != nil {
					b.Fatalf("%s: %v", v.name, err)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(countNodes(v.ast)), "ast-nodes")
		})
	}
}

// countNodes reports an AST's total node count, the quantity the cached-compile
// cost is expected to scale with.
func countNodes(n *rules.Node) int {
	if n == nil {
		return 0
	}
	total := 1
	total += countNodes(n.Left) + countNodes(n.Right)
	for _, c := range n.Children {
		total += countNodes(c)
	}
	for _, it := range n.Items {
		total += countNodes(it)
	}
	return total
}

// allocProfile is the per-Check allocation cost of one rule variant.
type allocProfile struct {
	allocsPerOp float64
	bytesPerOp  float64
}

// measureAllocs runs n cached Checks and returns the per-op allocation count and
// allocated bytes, read from the runtime's cumulative counters.
//
// Cumulative Mallocs/TotalAlloc deltas are used rather than testing.AllocsPerRun
// because BYTES are the load-bearing signal here: a defensive copy of a
// cap-sized []any is a single ~116 KB allocation, which barely moves an
// allocation COUNT but is impossible to miss in bytes.
func measureAllocs(tb testing.TB, svc *service.Service, q service.Query, n int) allocProfile {
	tb.Helper()
	ctx := context.Background()
	// Warm every lazily-built cache (parsed patterns, compiled rule, metadata
	// entry) so the measured window is the steady state, not first-call setup.
	for i := 0; i < 64; i++ {
		if _, err := svc.Check(ctx, q); err != nil {
			tb.Fatalf("warm Check: %v", err)
		}
	}
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for i := 0; i < n; i++ {
		if _, err := svc.Check(ctx, q); err != nil {
			tb.Fatalf("Check: %v", err)
		}
	}
	runtime.ReadMemStats(&after)
	return allocProfile{
		allocsPerOp: float64(after.Mallocs-before.Mallocs) / float64(n),
		bytesPerOp:  float64(after.TotalAlloc-before.TotalAlloc) / float64(n),
	}
}

// TestMetadataIsSharedByReference is the CHEAP, ALWAYS-ON half of the
// no-defensive-copy guard, and it runs in the default `make test`.
//
// provider hands a cached Metadata out BY REFERENCE and documents the read-only
// contract as TRANSITIVE over nested values (provider/provider.go). A defensive
// deep copy introduced on that read path would be invisible to every correctness
// test in the repo — the decisions would stay identical — and fatal to the NFR,
// because it would put an O(metadata) copy on the per-Check hot path.
//
// Rather than infer that from a timing-sensitive allocation profile, this asserts
// it structurally: the map, its nested object, and its nested arrays that come
// back from two successive Fetches must be the SAME objects, by address. That
// cannot flake, costs nothing, and fails the instant someone clones on read.
func TestMetadataIsSharedByReference(t *testing.T) {
	ctx := context.Background()
	m := buildModel(t)
	objID, err := identity.Parse(benchObject)
	if err != nil {
		t.Fatalf("parse %s: %v", benchObject, err)
	}

	first, err := m.registry.Fetch(ctx, objID)
	if err != nil {
		t.Fatalf("first Fetch: %v", err)
	}
	second, err := m.registry.Fetch(ctx, objID)
	if err != nil {
		t.Fatalf("second Fetch (should be a cache hit): %v", err)
	}

	sameAddr := func(a, b any) bool {
		return reflect.ValueOf(a).UnsafePointer() == reflect.ValueOf(b).UnsafePointer()
	}
	if !sameAddr(first, second) {
		t.Fatal("registry copied the metadata map on read; it must be shared by reference")
	}
	for _, field := range []string{"owner", "tags", "tagsAtCap"} {
		a, ok := first[field]
		if !ok {
			t.Fatalf("fixture metadata lost its %q field", field)
		}
		if !sameAddr(a, second[field]) {
			t.Errorf("nested field %q was copied on read; the by-reference contract is transitive", field)
		}
	}
}

// TestCollectionCheckAllocations is the MEASURED half of the guard: the per-Check
// allocation profile of every rule variant, with a copy detector on the two
// collection cases.
//
// The assertion is on BYTES per Check and is scaled to the array: materialising
// either fixture array costs at least 16 bytes per element (the []any backing
// store alone), so a budget at that size is a copy detector independent of the
// absolute allocation level of the surrounding engine.
//
// GATED, like TestCheckNFR, behind APERTURE_BENCH_ASSERT=1 — it is a measurement
// over thousands of Checks, and the repo's discipline is that measurements never
// gate the default build. TestMetadataIsSharedByReference above is the ungated
// structural guard that runs every time.
//
// Both budgets below started out FAILING and are now met. The copy detector
// failed while the guarded dispatcher materialised a fresh []any of the
// collection operand on every evaluation (rules/shape.go, classify); the
// literal-node budget failed while every literal was decoded through a
// json.Decoder on every AST walk (issue #9, fixed by rules.classifyScalar).
// Both are held here as ratchets — see docs/benchmarks.md, "Findings".
func TestCollectionCheckAllocations(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping allocation profile under -short")
	}
	if os.Getenv("APERTURE_BENCH_ASSERT") != "1" {
		t.Skip("set APERTURE_BENCH_ASSERT=1 to run the per-Check allocation profile")
	}
	const iterations = 2000

	m := buildModel(t)
	svc, closeFn := newRuleService(t, m, false)
	defer closeFn()

	profiles := make(map[string]allocProfile, len(ruleVariants()))
	for _, v := range ruleVariants() {
		q := ruleQuery(t, m, v.name)
		warm(t, svc, q, v.allow)
		p := measureAllocs(t, svc, q, iterations)
		profiles[v.name] = p
		t.Logf("%-18s %7.1f allocs/op %9.0f B/op   (%s)", v.name, p.allocsPerOp, p.bytesPerOp, renderOf(v.name))
	}

	base := profiles[ruleScalar]

	// THE COPY DETECTOR. Comparing a collection case against the scalar baseline
	// would conflate two different things: the guarded dispatcher's FIXED
	// per-evaluation overhead (two operand classifications, the notes argument)
	// and any PER-ELEMENT cost. Differencing the two collection cases against
	// each other cancels the fixed part exactly, leaving bytes per ELEMENT — and
	// per-element bytes is precisely the signature of a copy.
	//
	// If metadata is genuinely shared by reference, this is ~0: reading an
	// element out of a []any allocates nothing. Materialising the array into a
	// fresh []any costs 16 bytes/element for the backing store alone, so the
	// budget sits well below that and well above zero.
	const maxBytesPerElement = 8.0
	small, large := profiles[ruleTagsTypical], profiles[ruleTagsAtCap]
	perElement := (large.bytesPerOp - small.bytesPerOp) / float64(tagsAtCapLen-tagsTypicalLen)
	t.Logf("collection cost scales at %.1f B per array element (%d -> %d elements)",
		perElement, tagsTypicalLen, tagsAtCapLen)
	if perElement > maxBytesPerElement {
		t.Errorf("a cached Check allocates %.1f B per collection element (budget %.1f); "+
			"metadata is being COPIED per Check rather than read through the reference the "+
			"provider cache hands out — 16 B/element is the cost of a fresh []any backing store",
			perElement, maxBytesPerElement)
	}

	// Optional chaining (E2-S2) is a RENDER-time concern only, so reading through
	// a nested object must cost essentially nothing over a flat field.
	if d := profiles[ruleNested].allocsPerOp - base.allocsPerOp; d > 4 {
		t.Errorf("%s costs %.1f allocs/op more than the %s baseline (%.1f vs %.1f); "+
			"optional chaining should be free at runtime",
			ruleNested, d, ruleScalar, profiles[ruleNested].allocsPerOp, base.allocsPerOp)
	}

	// The list-literal render is the path E5-S1 deliberately left native so the
	// common `object.region in [...]` shape pays nothing. Per-evaluation it does
	// (BenchmarkRuleEval: identical to the scalar rule) — so any per-Check excess
	// here is NOT the operator, it is the rules engine re-walking the AST on every
	// decision. Budgeted separately so the two mechanisms cannot be confused.
	//
	// ISSUE #9, FIXED. This budget was a ratchet held at 30 while the per-Check
	// AST re-walk cost ~14 allocations per LITERAL NODE — which is what made a
	// three-literal rule 28 allocations dearer than a one-literal rule that
	// evaluates identically. rules.decodeScalar built a json.Decoder for every
	// literal on every walk, twice per decision (Validate, then render); the
	// literal fast path in rules/ast.go (classifyScalar) removed it, and the
	// measured delta is now 0.0. The budget is 2 rather than 0 only to leave
	// room for a literal count that genuinely differs, NOT for a per-literal
	// decode to creep back: anything scaling with literal count blows past it.
	if d := profiles[ruleLiteralIn].allocsPerOp - base.allocsPerOp; d > 2 {
		t.Errorf("%s costs %.1f allocs/op more than the %s baseline (%.1f vs %.1f) even though "+
			"BenchmarkRuleEval shows the two rules evaluate identically; the excess scales with "+
			"the AST's LITERAL NODE count, which points at the per-Check re-validation in "+
			"rules.Engine.compile — a literal is being decoded rather than classified on the "+
			"walk (issue #9, rules.classifyScalar) — see docs/benchmarks.md, \"Findings\"",
			ruleLiteralIn, d, ruleScalar, profiles[ruleLiteralIn].allocsPerOp, base.allocsPerOp)
	}
}

// renderOf reports a variant's render strategy for the log line, so the
// allocation table reads with the code path beside each number.
func renderOf(name string) string {
	for _, v := range ruleVariants() {
		if v.name == name {
			return v.render
		}
	}
	return "unknown"
}

// TestCheckNFRCollections is the collection half of the hard NFR gate: the same
// p99 < 1ms / >= 10k checks/sec assertion as TestCheckNFR, per rule variant.
//
// It is gated identically (APERTURE_BENCH_ASSERT=1, and skipped under -short),
// and its NAME deliberately contains "TestCheckNFR" so the documented invocation
//
//	APERTURE_BENCH_ASSERT=1 go test -run TestCheckNFR ./bench/
//
// — whose -run pattern is an unanchored regexp — covers the new cases with no
// command change.
func TestCheckNFRCollections(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping NFR wall-clock assertion under -short")
	}
	if os.Getenv("APERTURE_BENCH_ASSERT") != "1" {
		t.Skip("set APERTURE_BENCH_ASSERT=1 to run the hard NFR latency/throughput gate")
	}

	m := buildModel(t)
	for _, withAudit := range []bool{false, true} {
		name := "audit-off"
		if withAudit {
			name = "audit-on"
		}
		svc, closeFn := newRuleService(t, m, withAudit)
		for _, v := range ruleVariants() {
			t.Run(name+"/"+v.name, func(t *testing.T) {
				assertCheckNFR(t, svc, ruleQuery(t, m, v.name), name+"/"+v.name, v.allow, variantSamples)
			})
		}
		closeFn()
	}
}
