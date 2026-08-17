package bench

import (
	"context"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/frankbardon/aperture/rules"
)

// Date comparisons on the cached-Check hot path.
//
// Every other rule variant decides from values the metadata bag already holds in
// their final form. A date comparison does not: both operands are canonical
// STRINGS and both are PARSED to instants on every evaluation, because comparing
// the text is wrong across granularities ("2026-03-04" and "2026-03-04T00:00:00Z"
// name the same instant and must compare equal, yet sort apart as text). A
// relative bound adds calendar arithmetic on top, and then formats a canonical
// string that the comparison immediately re-parses.
//
// That is new, unavoidable, per-evaluation work on the path the NFR floor
// (p99 < 1 ms, >= 10 000 checks/sec) is measured over — so the question this file
// exists to answer is not "is it correct" but "what does it cost, and does the
// floor still hold with it in". The previous effort's collection copy is the
// reason the question is asked this way round: it passed every correctness test,
// held a comfortable p99, and still fell 38 % under the throughput floor. Only
// throughput could see it.
//
// WHAT IS COVERED, AND WHERE
//
// Eight date operators, three relative-date vocabularies, a clamping branch and a
// deny branch is more surface than one fixture can carry, so it is split across
// two tiers with a guard that neither can fall behind the registry:
//
//   - The ASSERTED tier is ruleVariants() (fixture_test.go), every member of
//     which TestCheckNFRCollections holds to the p99 and throughput floors, audit
//     on and off. All EIGHT date operators live here — five of them inside
//     rule-date-multi — along with a literal comparison, a relative comparison, a
//     between with two relative bounds, a clamping offset, a calendar-bucket
//     comparison, and the deny path.
//   - The INFORMATIONAL tier is the benchmarks below, which separate costs the
//     asserted tier reports only in aggregate: per operator, per stage of
//     relative-date complexity, and per vocabulary member. There is no agreed
//     threshold for a per-evaluation number, so nothing here asserts one.
//
// TestDateBenchCoverage reads the operator registry and the three vocabularies
// out of rules/ SOURCE and fails if any member has no fixture — so a ninth
// operator or a twelfth snap cannot land with the bench suite quietly unchanged.
//
// MEASURED, Apple M1 Max, pinned reference instant, per EVALUATION
// (BenchmarkRuleEval — the rule alone, with the ~55 µs of grant resolution a
// Check pays stripped out):
//
//	variant                 ns/op    B/op  allocs/op
//	------------------------------------------------
//	rule-scalar (control)   156.3     128      3
//	rule-date-same-year     526.4     704      7
//	rule-date-compare       590.6     704      7
//	rule-date-deny          661.5     824     11
//	rule-date-between       751.5     704      7
//	rule-date-clamped       897.0     872     10
//	rule-date-relative      898.3     872     10
//	rule-date-rel-bounds   1334       976     12
//	rule-date-multi        2112      1344     14
//
// Four things are readable off that, and each is a claim a future regression has
// to break:
//
//  1. **The parse itself allocates nothing.** rule-date-between parses THREE
//     operands to rule-date-compare's two and is byte-identical at 704 B / 7
//     allocs. The ~576 B a date comparison costs over the scalar control is the
//     $date dispatcher's fixed EIGHT-ARGUMENT call boxing, not parsing.
//  2. **Clamping is free.** rule-date-clamped snaps to a 31-day month end and
//     offsets into a 28-day month, so the day clamp genuinely runs; it measures
//     897.0 ns / 872 B / 10 allocs against the non-clamping rule-date-relative's
//     898.3 / 872 / 10. The clamp is a comparison and a substitution, and it
//     shows up as nothing.
//  3. **A second relative operand costs a second full resolution.** The
//     per-decision NOW snapshot is shared — that is what makes a decision
//     reproducible — but only the INSTANT is shared, not the arithmetic:
//     rule-date-rel-bounds is +583 ns and +2 allocs over rule-date-between, which
//     is two resolutions at ~290 ns rather than one amortised. This is expected
//     from the design ($rel is an ordinary call per operand) rather than a
//     defect, but it means a rule's cost scales with its RELATIVE OPERAND count,
//     not with its decision count.
//  4. **The deny path allocates, and is bounded.** rule-date-deny is +4 allocs /
//     +120 B over rule-date-compare — the *time.ParseError the standard library
//     constructs on a failed time.Parse, which provider classifies and discards
//     because its Error() would quote the value. It is a fixed cost on one
//     branch, it does not scale with anything, and TestDateCheckAllocations pins
//     it so it cannot start scaling.
//
// AND THE SAME VARIANTS PER CACHED CHECK (TestDateCheckAllocations, audit off,
// 2 000 iterations — the whole facade, where the fixture's own grant resolution
// is the ~58 allocs/op baseline every row shares):
//
//	variant                allocs/op      B/op   delta vs rule-scalar
//	------------------------------------------------------------------
//	rule-scalar (control)       58.1    19 082    —
//	rule-date-deny              63.1    19 831    +5.0
//	rule-date-compare           65.1    19 952    +7.0
//	rule-date-between           65.1    19 983    +7.0
//	rule-date-same-year         65.1    19 921    +7.0
//	rule-date-relative          70.1    20 168    +12.0
//	rule-date-clamped           70.1    20 167    +12.0
//	rule-date-rel-bounds        73.1    20 626    +15.0
//	rule-date-multi             82.1    22 212    +24.0
//
// A date comparison costs +7 allocations and ~870 B on a ~19 KB, 58-allocation
// Check. Five of them cost +24, not +35 — each comparison after the first is
// 4.2 allocs, so the cost is linear in comparison count with a fixed part paid
// once. rule-date-deny is BELOW rule-date-compare because a rule that denies also
// leaves the grant unselected and the decision does less afterwards; the isolated
// deny cost is in the per-evaluation table above.
//
// AND THE RELATIVE-DATE LADDER (BenchmarkRelativeDateComplexity), which is where
// the cost of a relative bound comes apart into its stages:
//
//	stage                  ns/op    B/op  allocs/op   delta
//	---------------------------------------------------------------
//	literal-bound          420.4     688      6       — (no $rel)
//	anchor                 769.9     856      9       +349 ns, +168 B, +3
//	offset                 789.0     856      9       +19 ns,  +0 B, +0
//	snap                   792.8     856      9       +23 ns,  +0 B, +0
//	snap-offset            846.2     856      9       +76 ns,  +0 B, +0
//	snap-offset-clamped    859.7     856      9       +13 ns,  +0 B, +0
//	two-relative-bounds   1300       960     11       a second full resolution
//
// The ladder's headline is that **the calendar arithmetic is not the expensive
// part**. Snapping, offsetting and clamping together are ~90 ns and ZERO
// allocations on top of the bare anchor; the +349 ns and all +3 allocations
// arrive with the very first $rel, and they are its call boxing plus the
// canonical string it formats for $date to immediately re-parse. Removing that
// round trip is the one optimization with real headroom behind it, and it is
// deliberately not done here — it changes the dispatcher's operand contract, and
// this story measures rather than tunes.
//
// BenchmarkRelativeDateVocabulary reports every anchor, unit and snap at a flat
// ~1.1 µs / 960 B / 11 allocs per evaluation (two resolutions each, see
// relativeVocabularyCases). No vocabulary member is materially dearer than
// another — the ISO-week and quarter boundaries included — so there is no
// member-specific cost for a rule author to avoid.
//
// The floors the gate saw and the two optimizations deliberately left undone are
// in docs/benchmarks.md.

// ---------------------------------------------------------------------------
// The informational corpus
// ---------------------------------------------------------------------------

// dateCase is one informational date fixture: an AST, and the verdict it must
// produce against benchMetadata at benchNow.
//
// The verdict is declared and asserted for the same reason the asserted tier
// declares one — a relative date that fails to resolve at all evaluates to
// false through the deny-safe branch, which is a MUCH cheaper path than the one
// these benchmarks exist to measure. Without an expected verdict, a fixture whose
// arithmetic silently stopped resolving would keep reporting a number, and the
// number would be of the wrong thing.
type dateCase struct {
	name  string
	ast   *rules.Node
	allow bool
}

// dateOpEvalCases is one single-comparison rule per date operator, so the eight
// operators' per-evaluation costs are separable. The asserted tier holds all
// eight to the NFR floor but bundles five of them into one rule; this is where
// they come apart.
//
// Every case compares the same date-only field, so the only thing that differs
// between two rows is the operator's own post-parse work — which for the ordering
// operators is one DateValue.Compare and for the bucket operators is reading
// calendar components off both sides.
func dateOpEvalCases() []dateCase {
	hired := func() *rules.Node { return rules.Var("object.hired_at") }
	return []dateCase{
		{rules.OpBefore, rules.Compare(rules.OpBefore, hired(), rules.Lit("2026-12-31T23:59:59Z")), true},
		{rules.OpAfter, rules.Compare(rules.OpAfter, hired(), rules.Lit("2026-01-01")), true},
		{rules.OpOnOrBefore, rules.Compare(rules.OpOnOrBefore, hired(), rules.Lit(benchHiredAt)), true},
		{rules.OpOnOrAfter, rules.Compare(rules.OpOnOrAfter, hired(), rules.Lit(benchHiredAt)), true},
		{rules.OpBetween, rules.Between(hired(), rules.Lit("2026-01-01"), rules.Lit("2026-12-31T23:59:59Z")), true},
		// The bucket operators are given a MIXED-granularity right operand, which
		// is the case a text comparison would get wrong and therefore the one
		// worth measuring: the two sides are the same day and different strings.
		{rules.OpSameDay, rules.Compare(rules.OpSameDay, hired(), rules.Lit("2026-03-04T18:00:00Z")), true},
		{rules.OpSameMonth, rules.Compare(rules.OpSameMonth, hired(), rules.Lit("2026-03-31")), true},
		{rules.OpSameYear, rules.Compare(rules.OpSameYear, hired(), rules.Lit("2026-12-31T23:59:59Z")), true},
	}
}

// dateEpoch is the left operand of every relative-complexity case: the earliest
// date the canonical forms can express plus a day.
//
// It is a constant rather than a metadata field so that each case's verdict is
// true EXACTLY WHEN its relative bound resolved. Any resolved bound is after it,
// and an unresolved bound denies — so the benchmark's own allow assertion doubles
// as proof that the arithmetic the case exists to measure actually ran, rather
// than silently degrading to the deny-safe branch.
const dateEpoch = "0001-01-02"

// relativeComplexityCases is the relative-date cost LADDER: the same comparison
// with its right operand made progressively more expensive, one stage at a time,
// so each stage's contribution is a difference between adjacent rows rather than
// a single aggregate number.
//
//	literal-bound        a fixed string — no $rel at all, the control
//	anchor               $rel with neither snap nor offset (n = 0, snap none)
//	offset               offset only
//	snap                 snap only
//	snap-offset          both — what a real "last quarter" rule looks like
//	snap-offset-clamped  both, on a month-end anchor, so the day clamp runs
//	two-relative-bounds  a between whose BOTH bounds are relative
//
// The last row is the one that answers whether relative resolution is shared
// across the operands of a single evaluation. It is not — see the file header.
func relativeComplexityCases() []dateCase {
	before := func(name string, right *rules.Node) dateCase {
		return dateCase{name, rules.Compare(rules.OpBefore, rules.Lit(dateEpoch), right), true}
	}
	return []dateCase{
		before("literal-bound", rules.Lit("2026-01-01")),
		before("anchor", rules.RelativeDate(rules.AnchorNow, 0, rules.UnitDays, rules.SnapNone)),
		before("offset", rules.RelativeDate(rules.AnchorNow, -1, rules.UnitQuarters, rules.SnapNone)),
		before("snap", rules.RelativeDate(rules.AnchorNow, 0, rules.UnitDays, rules.SnapStartOfQuarter)),
		before("snap-offset", rules.RelativeDate(rules.AnchorNow, -1, rules.UnitQuarters, rules.SnapStartOfQuarter)),
		before("snap-offset-clamped", rules.RelativeDate(rules.AnchorNow, -6, rules.UnitMonths, rules.SnapEndOfMonth)),
		{
			name: "two-relative-bounds",
			ast: rules.Between(rules.Lit("2026-06-15"),
				rules.RelativeDate(rules.AnchorNow, -5, rules.UnitYears, rules.SnapStartOfYear),
				rules.RelativeDate(rules.AnchorToday, 0, rules.UnitDays, rules.SnapEndOfDay)),
			allow: true,
		},
	}
}

// The three relative-date vocabularies, as the bench suite spells them.
//
// They are written out with the EXPORTED constants, so a member that is renamed
// cannot drift here, and TestDateBenchCoverage compares them against the tables
// scanned out of rules/relative.go, so a member that is ADDED cannot land without
// a fixture. Hand-listing plus a registry-derived guard is the same arrangement
// the Go/JS editor contract uses, and for the same reason: the list has to be
// readable at the fixture, and it must not be trusted.
var (
	benchAnchors = []string{rules.AnchorNow, rules.AnchorToday}
	benchUnits   = []string{
		rules.UnitYears, rules.UnitQuarters, rules.UnitMonths, rules.UnitWeeks,
		rules.UnitDays, rules.UnitHours, rules.UnitMinutes,
	}
	benchSnaps = []string{
		rules.SnapNone,
		rules.SnapStartOfYear, rules.SnapEndOfYear,
		rules.SnapStartOfQuarter, rules.SnapEndOfQuarter,
		rules.SnapStartOfMonth, rules.SnapEndOfMonth,
		rules.SnapStartOfWeek, rules.SnapEndOfWeek,
		rules.SnapStartOfDay, rules.SnapEndOfDay,
	}
)

// relativeVocabularyCases sweeps every member of every relative-date vocabulary,
// one case each, so no member's arithmetic is unmeasured.
//
// Each case compares a relative node against ITSELF (`rel onOrAfter rel`). That
// is not a stunt: a self-comparison is true exactly when the node resolved, and
// false the instant it does not — which is the only way to state "this
// vocabulary member's arithmetic ran" from outside the package, since an
// unresolved bound and a genuinely false comparison are indistinguishable at the
// Eval boundary. The reported ns/op is therefore TWO resolutions per evaluation
// and no metadata read at all; read it as a relative number across members, not
// against the ladder above.
func relativeVocabularyCases() []dateCase {
	self := func(name string, rel *rules.Node) dateCase {
		return dateCase{name, rules.Compare(rules.OpOnOrAfter, rel, rel), true}
	}
	cases := make([]dateCase, 0, len(benchAnchors)+len(benchUnits)+len(benchSnaps))
	for _, a := range benchAnchors {
		cases = append(cases, self("anchor-"+a, rules.RelativeDate(a, 0, rules.UnitDays, rules.SnapNone)))
	}
	for _, u := range benchUnits {
		cases = append(cases, self("unit-"+u, rules.RelativeDate(rules.AnchorNow, -1, u, rules.SnapNone)))
	}
	for _, s := range benchSnaps {
		cases = append(cases, self("snap-"+s, rules.RelativeDate(rules.AnchorNow, 0, rules.UnitDays, s)))
	}
	return cases
}

// ---------------------------------------------------------------------------
// Informational benchmarks
// ---------------------------------------------------------------------------

// benchmarkDateCases compiles each case once and evaluates it over the fixture's
// metadata at the pinned instant, asserting the declared verdict before the timer
// starts.
func benchmarkDateCases(b *testing.B, cases []dateCase) {
	ctx := context.Background()
	md := benchMetadata()
	compiler := rules.NewCompiler()

	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			compiled, err := compiler.Compile(c.ast)
			if err != nil {
				b.Fatalf("compile %s: %v", c.name, err)
			}
			in := rules.Input{Object: md, Action: c.name, Now: benchNow}
			ok, err := compiled.Eval(ctx, in)
			if err != nil || ok != c.allow {
				b.Fatalf("%s: eval=%v want %v err=%v (source: %s)",
					c.name, ok, c.allow, err, compiled.Source())
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := compiled.Eval(ctx, in); err != nil {
					b.Fatalf("%s: %v", c.name, err)
				}
			}
		})
	}
}

// BenchmarkDateOpEval reports the per-evaluation cost of each of the eight date
// operators in isolation.
func BenchmarkDateOpEval(b *testing.B) { benchmarkDateCases(b, dateOpEvalCases()) }

// BenchmarkRelativeDateComplexity is the relative-date cost ladder: no $rel, then
// anchor only, then offset, then snap, then both, then both with clamping, then
// two relative bounds in one comparison. Adjacent rows differ by exactly one
// stage, so the ladder separates the cost of each rather than reporting the sum.
func BenchmarkRelativeDateComplexity(b *testing.B) {
	benchmarkDateCases(b, relativeComplexityCases())
}

// BenchmarkRelativeDateVocabulary sweeps every anchor, unit and snap, so no
// vocabulary member's arithmetic goes unmeasured — the ISO-week boundaries and
// the quarter boundaries in particular do materially more work than a day
// boundary. Each case resolves its node twice; see relativeVocabularyCases.
func BenchmarkRelativeDateVocabulary(b *testing.B) {
	benchmarkDateCases(b, relativeVocabularyCases())
}

// ---------------------------------------------------------------------------
// Ungated structural guards (these run in the default `make test`)
// ---------------------------------------------------------------------------

// TestDateFixtureActuallyClamps pins the derivation behind rule-date-clamped: at
// the pinned reference instant, snapping to the end of the month and then
// offsetting six months back really does land on a day the target month does not
// have, so the clamp runs.
//
// It is asserted rather than left in a comment for the same reason
// TestCollectionFixtureSitsAtTheValueCap asserts its array size. A clamping
// fixture that has quietly stopped clamping still passes every latency and
// allocation assertion in this package — it simply measures the other branch —
// and the failure would be invisible. Pure calendar arithmetic, no wall clock, so
// it cannot flake.
func TestDateFixtureActuallyClamps(t *testing.T) {
	const monthsBack = 6 // must match the rule-date-clamped variant
	anchorDay := daysInMonthUTC(benchNow.Year(), benchNow.Month())
	target := benchNow.AddDate(0, 0, -benchNow.Day()) // any day in the previous month
	for i := 1; i < monthsBack; i++ {
		target = target.AddDate(0, 0, -target.Day())
	}
	targetDay := daysInMonthUTC(target.Year(), target.Month())

	t.Logf("anchor month %s has %d days; %d months back is %s with %d",
		benchNow.Format("2006-01"), anchorDay, monthsBack, target.Format("2006-01"), targetDay)
	if anchorDay <= targetDay {
		t.Fatalf("rule-date-clamped does not clamp at the pinned instant %s: the end of %s is day %d "+
			"and %s has %d days, so addMonthsClamped's day clamp never runs and the fixture measures "+
			"the same branch as rule-date-relative — move benchNow to a month longer than the one "+
			"%d months before it",
			benchNow.Format("2006-01-02"), benchNow.Format("2006-01"), anchorDay,
			target.Format("2006-01"), targetDay, monthsBack)
	}
}

// daysInMonthUTC reports the length of a calendar month. It uses the day-zero
// normalisation trick deliberately: rules/calendar.go is forbidden from doing so
// (a test there walks its AST and rejects AddDate outright), which makes this an
// INDEPENDENT second opinion rather than a restatement of the code under test.
func daysInMonthUTC(year int, month time.Month) int {
	return time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// TestDateBenchCoverage is the coverage guard: it reads the date operator
// registry and the three relative-date vocabularies out of the rules/ SOURCE and
// fails if any member has no bench fixture.
//
// A guard that cannot fall behind the registry is worth more than one more
// fixture. Every other assertion in this package is about a fixture that exists;
// this one is about the fixture that does not — the ninth operator, the twelfth
// snap, the eighth unit — which is precisely the case a hand-maintained list
// misses, because nothing about adding an operator to rules/ makes anyone open
// bench/. The repo already gates the Go/JS editor contract, the collection
// operator tables and the scope strategy matrix this way; this is the same idea
// pointed at the benchmark suite.
//
// The two tiers are held to DIFFERENT standards, on purpose:
//
//   - An OPERATOR must appear in ruleVariants(), the asserted tier, because an
//     operator is a comparison on the Check hot path and the NFR floor is the
//     only thing that can see a throughput regression. A fixture outside the
//     assertion proves nothing.
//   - A VOCABULARY MEMBER must appear somewhere in the corpus, asserted or
//     informational. Holding 20 anchor/unit/snap combinations to a wall-clock
//     floor would multiply the gate's runtime for arithmetic that is bounded by
//     construction — none of it touches metadata, allocates per element, or
//     scales with anything — while the two shapes that DO reach the hot path (a
//     resolution, and two resolutions in one comparison) are both in the asserted
//     tier already.
//
// It reads source text, so it is subject to the same caveat as the Go/JS contract
// test: restructuring a table declaration can break the SCANNER even when the
// tables agree. It therefore fails loudly with the file and the pattern it could
// not find, and never skips.
func TestDateBenchCoverage(t *testing.T) {
	astConsts := goStringConsts(t, rulesASTSource)
	relConsts := goStringConsts(t, rulesRelativeSource)

	operators := goDateOperators(t, astConsts)
	anchors := goVocabulary(t, rulesRelativeSource, "relativeAnchors", relConsts)
	units := goVocabulary(t, rulesRelativeSource, "relativeUnits", relConsts)
	snaps := goVocabulary(t, rulesRelativeSource, "relativeSnaps", relConsts)

	// Floors on what the scanner found. They are today's counts: a table that
	// GAINS a member is what this test is for, and a table that LOSES one is a
	// deliberate removal of persisted API, so updating a floor is part of that
	// change. A scanner broken by a restructured declaration reports zero and
	// fails here rather than silently passing an empty comparison.
	requireAtLeast(t, "date operators (opSpecs, renderDate)", operators, 8, rulesASTSource)
	requireAtLeast(t, "relative-date anchors", anchors, 2, rulesRelativeSource)
	requireAtLeast(t, "relative-date units", units, 7, rulesRelativeSource)
	requireAtLeast(t, "relative-date snaps", snaps, 11, rulesRelativeSource)

	// The asserted tier: every rule variant TestCheckNFRCollections holds to the
	// p99 and throughput floors.
	assertedOps, assertedAnchors, assertedUnits, assertedSnaps := coverage(variantASTs())
	// The whole corpus: the asserted tier plus every informational benchmark.
	corpus := variantASTs()
	for _, c := range dateOpEvalCases() {
		corpus = append(corpus, c.ast)
	}
	for _, c := range relativeComplexityCases() {
		corpus = append(corpus, c.ast)
	}
	for _, c := range relativeVocabularyCases() {
		corpus = append(corpus, c.ast)
	}
	_, corpusAnchors, corpusUnits, corpusSnaps := coverage(corpus)

	covered := 0
	for op := range operators {
		if assertedOps[op] {
			covered++
		}
	}
	t.Logf("asserted tier covers %d/%d date operators, %d/%d anchors, %d/%d units, %d/%d snaps",
		covered, len(operators), len(assertedAnchors), len(anchors),
		len(assertedUnits), len(units), len(assertedSnaps), len(snaps))
	t.Logf("whole corpus covers %d anchors, %d units, %d snaps",
		len(corpusAnchors), len(corpusUnits), len(corpusSnaps))

	for op := range operators {
		if !assertedOps[op] {
			t.Errorf("date operator %q has no fixture in the ASSERTED NFR set: add a ruleVariant "+
				"using it (fixture_test.go), so TestCheckNFRCollections holds it to p99 < 1ms and "+
				">= 10k checks/sec like every other comparison on the Check hot path. Adding it only "+
				"to an informational benchmark does not close this — a fixture outside the assertion "+
				"proves nothing about the floor", op)
		}
	}
	// The other direction, over the one table that CLAIMS to be one case per date
	// operator: if an operator is retired from the registry, its per-operator
	// benchmark has to go with it, or the suite is publishing a number for
	// something that no longer exists.
	perOp, _, _, _ := coverage(func() []*rules.Node {
		out := make([]*rules.Node, 0, len(dateOpEvalCases()))
		for _, c := range dateOpEvalCases() {
			out = append(out, c.ast)
		}
		return out
	}())
	for op := range perOp {
		if !operators[op] {
			t.Errorf("dateOpEvalCases benchmarks operator %q, which %s no longer registers with "+
				"kind renderDate — drop the case, or repoint the scanner if the table moved",
				op, rulesASTSource)
		}
	}

	missing := func(kind string, registry, covered map[string]bool) {
		for m := range registry {
			if !covered[m] {
				t.Errorf("relative-date %s %q has no bench fixture anywhere: add it to bench%s "+
					"(date_test.go), which sweeps every vocabulary member, or to a rule variant if "+
					"it belongs on the asserted hot path",
					kind, m, strings.ToUpper(kind[:1])+kind[1:]+"s")
			}
		}
	}
	missing("anchor", anchors, corpusAnchors)
	missing("unit", units, corpusUnits)
	missing("snap", snaps, corpusSnaps)
}

// variantASTs returns the asserted tier's ASTs.
func variantASTs() []*rules.Node {
	out := make([]*rules.Node, 0, len(ruleVariants()))
	for _, v := range ruleVariants() {
		out = append(out, v.ast)
	}
	return out
}

// coverage walks a corpus of ASTs and reports which date operators and which
// relative-date vocabulary members appear in it.
func coverage(asts []*rules.Node) (ops, anchors, units, snaps map[string]bool) {
	ops, anchors, units, snaps = map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	var walk func(n *rules.Node)
	walk = func(n *rules.Node) {
		if n == nil {
			return
		}
		switch n.Type {
		case rules.NodeCompare:
			if n.Op != "" {
				ops[n.Op] = true
			}
		case rules.NodeRelativeDate:
			anchors[n.Anchor] = true
			units[n.Unit] = true
			snaps[n.Snap] = true
		}
		walk(n.Left)
		walk(n.Right)
		for _, c := range n.Children {
			walk(c)
		}
		for _, it := range n.Items {
			walk(it)
		}
	}
	for _, a := range asts {
		walk(a)
	}
	return ops, anchors, units, snaps
}

// requireAtLeast fails if a scanned registry came back smaller than it should.
func requireAtLeast(tb testing.TB, what string, got map[string]bool, want int, path string) {
	tb.Helper()
	if len(got) < want {
		tb.Fatalf("scanned only %d %s out of %s, expected at least %d — the table was most likely "+
			"restructured out from under the scanner (see the scanner caveat on the Go/JS editor "+
			"contract test, which reads source the same way). Fix the scanner, or lower this floor "+
			"if a member was genuinely removed", len(got), what, path, want)
	}
}

// ---------------------------------------------------------------------------
// The registry scanners
// ---------------------------------------------------------------------------

// The rules/ sources the coverage guard reads. Paths are relative to this
// package's directory, which is where `go test` runs a test binary.
const (
	rulesASTSource      = "../rules/ast.go"
	rulesRelativeSource = "../rules/relative.go"
)

// goStringConstRe matches an exported `Name = "value"` const declaration, which
// is how ast.go spells the operators and relative.go spells all three
// vocabularies. A trailing line comment is fine; a computed or concatenated value
// is deliberately not matched, since it would not be a closed-set token.
var goStringConstRe = regexp.MustCompile(`(?m)^\s*([A-Z]\w*)\s*=\s*"([^"]*)"`)

// opSpecDateRe matches an opSpecs entry whose spec renders to the date
// dispatcher — the definition of "is a date operator", taken from the registry
// rather than from a name convention.
var opSpecDateRe = regexp.MustCompile(`(?m)^\s*(Op\w+):\s*\{[^}]*renderDate[^}]*\},`)

// goStringConsts reads a Go source file and returns its exported string consts as
// name -> value.
func goStringConsts(tb testing.TB, path string) map[string]string {
	tb.Helper()
	src := readSource(tb, path)
	out := map[string]string{}
	for _, m := range goStringConstRe.FindAllStringSubmatch(src, -1) {
		out[m[1]] = m[2]
	}
	if len(out) == 0 {
		tb.Fatalf("no `Name = \"value\"` consts found in %s; the scanner cannot read it", path)
	}
	return out
}

// goDateOperators returns the operator VALUES registered in opSpecs with kind
// renderDate.
func goDateOperators(tb testing.TB, consts map[string]string) map[string]bool {
	tb.Helper()
	src := readSource(tb, rulesASTSource)
	out := map[string]bool{}
	for _, m := range opSpecDateRe.FindAllStringSubmatch(src, -1) {
		value, ok := consts[m[1]]
		if !ok {
			tb.Fatalf("opSpecs in %s registers %s with kind renderDate, but no `%s = \"...\"` const "+
				"declaration was found in the same file", rulesASTSource, m[1], m[1])
		}
		out[value] = true
	}
	return out
}

// goVocabulary returns the members of a `var <name> = map[string]struct{}{...}`
// table, resolved through the file's consts.
func goVocabulary(tb testing.TB, path, name string, consts map[string]string) map[string]bool {
	tb.Helper()
	src := readSource(tb, path)
	open := "var " + name + " = map[string]struct{}{"
	i := strings.Index(src, open)
	if i < 0 {
		tb.Fatalf("could not find `%s` in %s; the coverage guard reads that declaration verbatim",
			open, path)
	}
	body := src[i+len(open):]
	// The table ends at the first closing brace in the first column — the same
	// shape every top-level var block in the package uses.
	if end := strings.Index(body, "\n}"); end >= 0 {
		body = body[:end]
	}
	out := map[string]bool{}
	for _, line := range strings.Split(body, "\n") {
		for _, field := range strings.Split(line, ",") {
			field = strings.TrimSpace(field)
			key, _, found := strings.Cut(field, ":")
			if !found {
				continue
			}
			value, ok := consts[strings.TrimSpace(key)]
			if !ok {
				continue
			}
			out[value] = true
		}
	}
	return out
}

// readSource reads one of the rules/ sources, FAILING (never skipping) if it has
// moved. A coverage guard that quietly disappears when its input moves is worse
// than no guard, because the suite still reports green.
func readSource(tb testing.TB, path string) string {
	tb.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		tb.Fatalf("the date bench coverage guard reads %s and it is not there: %v — if the file "+
			"moved, repoint this scanner in the same change", path, err)
	}
	return string(b)
}

// ---------------------------------------------------------------------------
// The gated allocation profile
// ---------------------------------------------------------------------------

// TestDateCheckAllocations is the date half of the allocation guard: the
// per-Check allocation profile of every date variant, with a budget on each of
// the four costs this effort's design deliberately accepted.
//
// Allocation is the leading indicator. The previous effort's throughput
// regression was a per-element copy that showed up in bytes long before it showed
// up as a missed floor, and a date rule has three plausible places for the same
// mistake to hide: a per-evaluation re-parse of a constant bound, an intermediate
// time.Time per calendar step, and an error value built on a failed parse. The
// budgets below name each one, so a regression fails with the mechanism rather
// than with a number.
//
// GATED behind APERTURE_BENCH_ASSERT=1 like the rest of the measured suite — the
// repo's discipline is that measurements never gate the default build.
// TestDateBenchCoverage and TestDateFixtureActuallyClamps are the ungated
// structural halves.
func TestDateCheckAllocations(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping allocation profile under -short")
	}
	if os.Getenv("APERTURE_BENCH_ASSERT") != "1" {
		t.Skip("set APERTURE_BENCH_ASSERT=1 to run the per-Check date allocation profile")
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
		t.Logf("%-22s %7.1f allocs/op %9.0f B/op   (%s)", v.name, p.allocsPerOp, p.bytesPerOp, renderOf(v.name))
	}

	// 1. A DATE COMPARISON'S FIXED COST over the scalar control. It is the $date
	//    dispatcher's eight-argument call boxing, measured at ~4 allocs / ~576 B
	//    per evaluation; the budget leaves room for the Check-level noise around
	//    it but not for a second mechanism. What it would catch: a bound being
	//    re-parsed into a fresh allocation per evaluation, or a note being built
	//    on a path that installs no collector.
	const maxDateAllocsOverScalar = 8
	base := profiles[ruleScalar]
	for _, name := range []string{ruleDateCompare, ruleDateBetween, ruleDateSameYear} {
		if d := profiles[name].allocsPerOp - base.allocsPerOp; d > maxDateAllocsOverScalar {
			t.Errorf("%s costs %.1f allocs/op more than the %s control (budget %.1f); a single date "+
				"comparison should cost the $date dispatcher's fixed call boxing and nothing else — "+
				"parsing a canonical date allocates nothing",
				name, d, ruleScalar, float64(maxDateAllocsOverScalar))
		}
	}

	// 2. PARSING IS FREE, and the ternary form proves it: between parses THREE
	//    operands where before parses two, over the same dispatcher arity. If the
	//    parse allocated, this difference would be positive and would scale with
	//    operand count.
	const maxAllocsPerExtraParse = 1.0
	if d := profiles[ruleDateBetween].allocsPerOp - profiles[ruleDateCompare].allocsPerOp; d > maxAllocsPerExtraParse {
		t.Errorf("%s costs %.1f allocs/op more than %s (budget %.1f) despite differing only by ONE "+
			"extra operand parse; parsing a canonical date string must allocate nothing — see "+
			"provider.DateValueOf, which is the hot-path entry point precisely because it builds no "+
			"error", ruleDateBetween, d, ruleDateCompare, maxAllocsPerExtraParse)
	}

	// 3. CLAMPING IS FREE. addMonthsClamped clamps by comparing a day against a
	//    table entry and substituting — no intermediate time.Time, no AddDate
	//    normalisation round trip. So a clamping relative date must cost the same
	//    as a non-clamping one; a positive delta here means the arithmetic started
	//    materialising intermediates per calendar step.
	const maxClampingAllocs = 1.0
	if d := profiles[ruleDateClamped].allocsPerOp - profiles[ruleDateRelative].allocsPerOp; d > maxClampingAllocs {
		t.Errorf("%s costs %.1f allocs/op more than the non-clamping %s (budget %.1f); the month-end "+
			"clamp is a comparison and a substitution and must allocate nothing — a positive delta "+
			"means calendar arithmetic is materialising intermediate instants",
			ruleDateClamped, d, ruleDateRelative, maxClampingAllocs)
	}

	// 4. THE DENY PATH IS BOUNDED. A string of the date-only width that is not a
	//    date reaches time.Parse, whose failure constructs a *time.ParseError
	//    (4 allocations) that provider classifies and discards — it cannot carry
	//    the value, because its Error() quotes the input. That cost is accepted,
	//    documented, and fixed; the budget exists so it stays fixed rather than
	//    growing a second error construction on top of it.
	//
	//    READ THIS DELTA WITH CARE: at the CHECK level it is NEGATIVE (-2.0
	//    measured), because a rule that denies also leaves the grant unselected
	//    and the decision then does less downstream work than an allow — which
	//    more than pays for the four. The isolated number is BenchmarkRuleEval,
	//    where rule-date-deny is +4 allocs / +120 B over rule-date-compare with no
	//    decision around it. The budget here is therefore a CEILING on the whole
	//    branch rather than a measurement of the parse failure: a coded error
	//    constructed on this path (which is exactly what provider.DateValueOf
	//    exists to avoid) would clear it easily.
	const maxDenyAllocsOverCompare = 6.0
	if d := profiles[ruleDateDeny].allocsPerOp - profiles[ruleDateCompare].allocsPerOp; d > maxDenyAllocsOverCompare {
		t.Errorf("%s costs %.1f allocs/op more than the matching %s (budget %.1f); the deny path's "+
			"one allocating step is the *time.ParseError the standard library builds and provider "+
			"discards — anything beyond that is a coded error being constructed on the hot path, "+
			"which is what provider.DateValueOf exists to avoid",
			ruleDateDeny, d, ruleDateCompare, maxDenyAllocsOverCompare)
	}

	// 5. A SECOND RELATIVE OPERAND costs a second resolution, not a second
	//    decision's worth of setup. The per-decision instant is shared; the
	//    arithmetic is not. This budget states that the unshared part is bounded
	//    and small — if it ever exceeds a whole extra comparison's fixed cost,
	//    something per-decision is being redone per operand.
	const maxSecondBoundAllocs = 6.0
	if d := profiles[ruleDateRelBounds].allocsPerOp - profiles[ruleDateRelative].allocsPerOp; d > maxSecondBoundAllocs {
		t.Errorf("%s costs %.1f allocs/op more than the single-bound %s (budget %.1f); a second "+
			"relative operand pays a second calendar resolution, which is expected — but not a "+
			"second per-decision setup", ruleDateRelBounds, d, ruleDateRelative, maxSecondBoundAllocs)
	}

	// 6. A RULE'S DATE COST IS PER COMPARISON, and each additional comparison must
	//    be no dearer than the FIRST one (7.0 allocs/op over the scalar control).
	//    rule-date-multi carries five where rule-date-compare carries one, so the
	//    marginal cost is the difference over four; it measures 4.2, i.e. the
	//    first comparison carries fixed per-evaluation cost the rest do not repeat.
	//    A superlinear result would mean per-comparison work that scales with the
	//    rule rather than with the operand — the shape of an accidental re-walk.
	const maxAllocsPerExtraComparison = 7.0
	perComparison := (profiles[ruleDateMulti].allocsPerOp - profiles[ruleDateCompare].allocsPerOp) / 4
	t.Logf("each date comparison beyond the first costs %.1f allocs/op", perComparison)
	if perComparison > maxAllocsPerExtraComparison {
		t.Errorf("each date comparison after the first costs %.1f allocs/op (budget %.1f), more than "+
			"the first one's own fixed cost; a rule's date cost must be LINEAR in its comparison "+
			"count", perComparison, maxAllocsPerExtraComparison)
	}
}
