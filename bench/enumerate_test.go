package bench

import (
	"context"
	"fmt"
	"testing"

	"github.com/frankbardon/aperture/engine"
	"github.com/frankbardon/aperture/identity"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/provider"
	"github.com/frankbardon/aperture/rules"
	"github.com/frankbardon/aperture/scope"
	"github.com/frankbardon/aperture/service"
	"github.com/frankbardon/aperture/storage/memory"
)

// Rule-backed Enumerate: what a bounded enumeration through a rule actually
// costs.
//
// Everything else in bench/ measures a single Check, where a rule-backed grant
// is evaluated exactly once. Enumerate is a different shape entirely. An
// inclusive grant whose membership is decided by a rule is not invertible, so
// the resolver LISTS the object type and filters each candidate through the
// rule — and then the engine runs its ordinary deny-overrides decision over
// every surviving candidate, which consults the same grant again. So one
// Enumerate is up to 2 x candidates rule evaluations:
//
//	scope.inclusiveResolver.Members -> enumerateOfType(keep: Contains) -> 1 eval per LISTED candidate
//	engine.enumerateWithSubjects    -> evaluate -> scopeCoverer.cover -> 1 eval per SELECTED candidate
//
// Both halves are bounded by the SAME existing limits — scope.DefaultMaxMembers
// on the member set and engine.DefaultEnumerateLimit on the result, both 1000 —
// so the worst case is fixed rather than proportional to the object population.
// TestEnumerateRuleBackedStaysBounded below pins that; no second limit exists
// and none is introduced here.
//
// These benchmarks are INFORMATIONAL. They are deliberately NOT part of
// TestCheckNFR's asserted gate: that gate is about the cached single-Check NFR
// (p99 < 1 ms, >= 10k checks/sec), and rule-backed enumeration has no agreed
// threshold to assert against. The number they exist to publish is the per-
// candidate cost, so a future change that regresses it has something to regress
// against.
//
// MEASURED COST PER CANDIDATE (Apple M1 Max, go test -benchtime=2s, audit off,
// one scalar-comparison rule over one metadata field, every candidate selected):
//
//	candidates |    ns/op    | ns/candidate | allocs/op | B/op
//	-----------+-------------+--------------+-----------+-----------
//	        10 |      40 472 |        4 047 |       532 |    37 223
//	       100 |     391 125 |        3 911 |     5 075 |   367 144
//	     1 000 |   4 195 286 |        4 195 |    50 118 | 3 690 931
//	     2 000 |   4 434 250 |        4 434 |    50 123 | 3 725 176
//
// And the rule half in isolation (BenchmarkEnumerateRuleBackedRuleEval, 1 000
// rules.Engine.Selected calls with no engine around them):
//
//	1 235 ns/eval, 14 allocs/eval, ~945 B/eval
//
// Read four things off that:
//
//  1. **~4.2 µs and ~50 allocations per candidate**, flat. The cost is LINEAR in
//     the candidate count with no super-linear term hiding in the resolver.
//  2. **The rule is ~60% of it.** Two evaluations per candidate at 1 235 ns is
//     ~2.5 µs of the ~4.2 µs, and 28 of the ~50 allocations. The remaining
//     ~1.7 µs is the decision engine's own per-candidate work. So halving the
//     rule cost is worth about a third of the total, and the per-decision AST
//     re-walk (BenchmarkRuleCompileCached) is the largest single line item
//     inside the rule half.
//  3. **The 1 000 and 2 000 rows are the same** to within run-to-run noise.
//     Doubling the object population does not double the work, because the
//     existing bound clamps the member set before the provider is even fully
//     walked. That is the bound doing its job, and it makes the worst case a
//     constant a host can budget for.
//  4. **A bound-limit enumeration is ~4.2 ms** — around 1 000x a cached Check.
//     That is arithmetic, not a defect (it IS ~2 000 rule evaluations plus 1 000
//     decisions), but it means rule-backed Enumerate is an interactive
//     operation, not a hot-path one.
//
// One asymmetry the table does not show, worth knowing before reading a caller's
// EnumerateRequest.Limit as a cost knob: a smaller Limit shortens only the
// SECOND half. The engine bounds its result loop by the caller's limit, but the
// member set is gathered first and is bounded by scope.DefaultMaxMembers
// regardless — so Limit=10 over 1 000 candidates still pays ~1 000 rule
// evaluations to build the member set, then ~10 decisions. That is a property of
// the existing bounds, not something these benchmarks change.

// The rule-backed enumeration fixture's identifiers. It is a SEPARATE store,
// registry and rules engine from buildModel's: the existing benchmarks are the
// committed baseline for the cached Check, and adding thousands of objects and
// grants to their model would move numbers this story is not about.
const (
	enumAccount    = "acctenum"
	enumRole       = "roleenum"
	enumUser       = "userenum"
	enumAction     = "enumread"
	enumPermission = "perm-enumread"
	enumRule       = "rule-enum-public"
	enumProject    = "projenum"
)

// enumerateCandidateSizes is the sweep. It brackets the bound rather than
// stopping at it: scope.DefaultMaxMembers is where the member set clamps, and
// the 2x row exists to show that going past it costs nothing more.
func enumerateCandidateSizes() []int {
	return []int{10, 100, scope.DefaultMaxMembers, 2 * scope.DefaultMaxMembers}
}

// enumerateModel is the self-contained rule-backed enumeration fixture.
type enumerateModel struct {
	svc *service.Service
	// query enumerates every document of the fixture's project.
	query service.EnumerateQuery
	// candidates is how many objects the provider holds.
	candidates int
	// want is how many ids Enumerate must return: every candidate is selected by
	// the rule, so it is the candidate count clamped by the existing bound.
	want int
}

// buildEnumerateModel seeds a store with one rule-backed inclusive grant over
// `candidates` documents, all of which the rule selects, and returns the facade
// plus the enumeration query.
//
// Every candidate is selected on purpose. A rule that filtered some of them out
// would measure a mixture of the selected and rejected paths and make the
// per-candidate number depend on the selectivity rather than on the machinery;
// selecting all of them is also the worst case, because a rejected candidate
// skips the second (per-decision) evaluation.
func buildEnumerateModel(tb testing.TB, candidates int) enumerateModel {
	tb.Helper()
	ctx := context.Background()
	store := memory.New()

	must := func(err error) {
		tb.Helper()
		if err != nil {
			tb.Fatalf("seed: %v", err)
		}
	}

	must(store.PutObjectType(ctx, model.ObjectType{
		Name: "document", Actions: []string{enumAction},
	}))
	must(store.PutPermission(ctx, model.Permission{
		ID: enumPermission, ObjectType: "document", Action: enumAction,
		ScopeStrategy: "inclusive;rule=" + enumRule,
	}))
	must(store.PutAccount(ctx, model.Account{ID: enumAccount, Name: enumAccount}))
	must(store.PutRole(ctx, model.Role{ID: enumRole, Name: enumRole}))
	must(store.PutPrincipal(ctx, model.Principal{
		ID: enumUser, Kind: model.PrincipalUser, Identity: "user:" + enumUser,
		RoleIDs: []string{enumRole},
	}))
	// ONE grant, account-wide, whose membership is decided entirely by the rule —
	// so the whole measured cost is the rule-backed resolution path and not a
	// large grant set being walked.
	must(store.PutGrant(ctx, model.Grant{
		ID: "g-" + enumRule, AccountID: enumAccount,
		Subject:      model.Subject{Kind: model.SubjectRole, ID: enumRole},
		PermissionID: enumPermission,
		Object:       "account:" + enumAccount + "/**",
		Effect:       model.EffectAllow,
	}))

	objects := make([]provider.Object, 0, candidates)
	for i := 0; i < candidates; i++ {
		id, err := identity.Parse(enumObjectID(i))
		if err != nil {
			tb.Fatalf("parse candidate %d: %v", i, err)
		}
		objects = append(objects, provider.Object{
			ID: id, Metadata: provider.Metadata{"classification": "public"},
		})
	}
	static, err := provider.NewStatic(objects)
	if err != nil {
		tb.Fatalf("build static provider: %v", err)
	}
	// TTL 0 (never expire) for the same reason buildRuleLayer disables it: a long
	// benchmark run must not silently start re-fetching partway through and
	// measure a mix of the cached and uncached paths.
	reg := provider.NewRegistry()
	reg.MustRegister("document", static, provider.WithTTL(0))

	ruleEng := rules.NewEngine(rules.MapSource{
		enumRule: {
			Name: enumRule,
			AST:  rules.Compare(rules.OpEq, rules.Var("object.classification"), rules.Lit("public")),
		},
	}, reg)

	eng := engine.New(store, engine.WithScopeResolution(
		scope.DefaultRegistry(),
		engine.ScopeDeps{Lister: reg, Rules: ruleEng},
	))

	want := candidates
	if want > scope.DefaultMaxMembers {
		want = scope.DefaultMaxMembers
	}
	return enumerateModel{
		svc: service.New(eng),
		query: service.EnumerateQuery{
			Account:   enumAccount,
			Principal: enumUser,
			Action:    enumAction,
			Pattern:   "account:" + enumAccount + "/project:" + enumProject + "/document:*",
		},
		candidates: candidates,
		want:       want,
	}
}

// enumObjectID renders the i-th candidate's canonical identity. Fixed-width so
// the canonical sort order Enumerate returns matches the declaration order.
func enumObjectID(i int) string {
	return fmt.Sprintf("account:%s/project:%s/document:doc%05d", enumAccount, enumProject, i)
}

// warmEnumerate runs the query once, asserting it returns exactly the expected
// number of ids, so a benchmark never silently measures an empty enumeration.
// It also primes the parsed-pattern cache, the compiled-rule cache and the
// provider metadata cache, so the measured loop is the steady state.
func warmEnumerate(tb testing.TB, m enumerateModel) {
	tb.Helper()
	ids, err := m.svc.Enumerate(context.Background(), m.query)
	if err != nil {
		tb.Fatalf("warm Enumerate: %v", err)
	}
	if len(ids) != m.want {
		tb.Fatalf("warm Enumerate returned %d ids over %d candidates, want %d",
			len(ids), m.candidates, m.want)
	}
}

// BenchmarkEnumerateRuleBacked sweeps a rule-backed Enumerate across candidate-
// set sizes, reporting ns/op, allocs/op, and the derived per-candidate cost.
//
// It reports and asserts nothing about a threshold — see the file comment.
func BenchmarkEnumerateRuleBacked(b *testing.B) {
	ctx := context.Background()
	for _, n := range enumerateCandidateSizes() {
		b.Run(fmt.Sprintf("candidates-%d", n), func(b *testing.B) {
			m := buildEnumerateModel(b, n)
			warmEnumerate(b, m)

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				ids, err := m.svc.Enumerate(ctx, m.query)
				if err != nil {
					b.Fatalf("Enumerate: %v", err)
				}
				if len(ids) != m.want {
					b.Fatalf("Enumerate returned %d ids, want %d", len(ids), m.want)
				}
			}
			b.StopTimer()
			// The number this benchmark exists to publish. Dividing by the
			// RETURNED id count (not the candidate count) keeps the 2x-bound row
			// comparable with the at-bound row: past the bound the extra objects
			// are never visited, so charging them would understate the real cost.
			if m.want > 0 && b.N > 0 {
				b.ReportMetric(
					float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(m.want),
					"ns/candidate")
			}
			b.ReportMetric(float64(m.want), "ids")
		})
	}
}

// BenchmarkEnumerateRuleBackedRuleEval isolates the rule half of the cost: the
// same number of rules.Engine.Selected calls the enumeration above makes, with
// no engine, no store and no grant walk around them.
//
// Reporting it beside BenchmarkEnumerateRuleBacked is what separates "the rule
// is expensive" from "enumeration is expensive": the difference between the two
// is everything the decision engine adds per candidate.
func BenchmarkEnumerateRuleBackedRuleEval(b *testing.B) {
	ctx := context.Background()
	reg := provider.NewRegistry()
	objects := make([]provider.Object, 0, scope.DefaultMaxMembers)
	ids := make([]identity.Identity, 0, scope.DefaultMaxMembers)
	for i := 0; i < scope.DefaultMaxMembers; i++ {
		id, err := identity.Parse(enumObjectID(i))
		if err != nil {
			b.Fatalf("parse candidate %d: %v", i, err)
		}
		ids = append(ids, id)
		objects = append(objects, provider.Object{
			ID: id, Metadata: provider.Metadata{"classification": "public"},
		})
	}
	static, err := provider.NewStatic(objects)
	if err != nil {
		b.Fatalf("build static provider: %v", err)
	}
	reg.MustRegister("document", static, provider.WithTTL(0))
	eng := rules.NewEngine(rules.MapSource{
		enumRule: {
			Name: enumRule,
			AST:  rules.Compare(rules.OpEq, rules.Var("object.classification"), rules.Lit("public")),
		},
	}, reg)

	for _, id := range ids { // warm the metadata cache and the compiled-rule cache
		if ok, err := eng.Selected(ctx, enumRule, id, enumUser, enumAction); err != nil || !ok {
			b.Fatalf("warm Selected(%s): ok=%v err=%v", id, ok, err)
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, id := range ids {
			if _, err := eng.Selected(ctx, enumRule, id, enumUser, enumAction); err != nil {
				b.Fatalf("Selected: %v", err)
			}
		}
	}
	b.StopTimer()
	if b.N > 0 {
		b.ReportMetric(
			float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(len(ids)),
			"ns/eval")
	}
}

// TestEnumerateRuleBackedStaysBounded is the ungated, always-on guard that the
// enumeration is bounded by the EXISTING limits and by nothing else.
//
// It seeds twice as many objects as scope.DefaultMaxMembers and asserts the
// result is exactly the bound: not more (the resolver would be materialising an
// unbounded member set) and not less (a second, tighter limit would have crept
// in, and a silently truncated Enumerate is a wrong access-control answer, not a
// performance tradeoff). It is structural arithmetic over the fixture rather
// than a timing assertion, so it cannot flake and runs in the default make test.
func TestEnumerateRuleBackedStaysBounded(t *testing.T) {
	m := buildEnumerateModel(t, 2*scope.DefaultMaxMembers)
	ids, err := m.svc.Enumerate(context.Background(), m.query)
	if err != nil {
		t.Fatalf("Enumerate: %v", err)
	}
	if len(ids) != scope.DefaultMaxMembers {
		t.Fatalf("Enumerate over %d rule-selected candidates returned %d ids, want exactly "+
			"the existing bound %d", m.candidates, len(ids), scope.DefaultMaxMembers)
	}
	if len(ids) > engine.DefaultEnumerateLimit {
		t.Fatalf("Enumerate returned %d ids, exceeding engine.DefaultEnumerateLimit %d",
			len(ids), engine.DefaultEnumerateLimit)
	}
	// Every returned id must be a real candidate, in canonical order.
	for i, id := range ids {
		if want := enumObjectID(i); id != want {
			t.Fatalf("id[%d] = %q, want %q (Enumerate must return candidates in canonical order)",
				i, id, want)
		}
	}
}
