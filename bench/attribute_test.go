package bench

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/frankbardon/aperture/engine"
	"github.com/frankbardon/aperture/identity"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/provider"
	"github.com/frankbardon/aperture/rules"
	"github.com/frankbardon/aperture/scope"
	"github.com/frankbardon/aperture/service"
	"github.com/frankbardon/aperture/storage/memory"
)

// E4-S4: the attribute path on the asserted NFR.
//
// The gate this file extends —
//
//	APERTURE_BENCH_ASSERT=1 go test -run TestCheckNFR ./bench/
//
// — asserts p99 cached Check < 1 ms and >= 10 000 checks/sec, but only over the
// fixtures it has. Before this file it had none that read `principal.*` or
// `account.*`, so every rule it measured went through floorPrincipal and
// floorAccount: no registry, no provider, no resolver, no memo. The gate would
// have kept passing, in full, while covering none of the attribute-providers
// effort's hot path. That is the failure mode that PASSES rather than fails, and
// the only fix for it is fixtures.
//
// # Four shapes, because they are four different code paths
//
//   - attr-principal reads `principal.*` from a registered user directory: the
//     PrincipalResolver seam, the user slot's cache, and principalBag's fresh-map
//     floor stamp.
//   - attr-account reads `account.*`: the AccountResolver seam, a different slot
//     with its own cache, and accountBag's floor.
//   - attr-both reads BOTH alongside object metadata, in one rule, which is the
//     realistic policy shape and the only one that pays all three resolutions
//     (object fetch + principal bag + account bag) inside a single evaluation.
//   - attr-absent is the LENIENCY path, and it denies. Its principal is one the
//     directory has no record for, so the resolver reports the documented
//     (nil, no error) absence, the decision proceeds against the floor alone, and
//     every comparison against a missing attribute is false. A deny is a
//     different branch, not a cheaper one, and an unwired-or-unknown subject is
//     the steady state in any deployment that has not finished populating its
//     directory — so it is held to the same floor as every allow, the same way
//     rule-date-deny is.
//
// # The enumeration shape, and why it is here rather than implied
//
// Everything above is one decision over ONE object. The shape that makes
// per-decision memoization matter is one decision over MANY objects: an
// Enumerate resolves grants, gathers members through a rule, and then runs an
// ordinary decision per surviving candidate. Without the memo (rules/attributes.go)
// each of those evaluations would resolve the principal and the account for
// itself — the same two facts, re-learned once per object, as a host round trip
// inside a loop. A 1 000-candidate enumeration would perform thousands of
// directory reads to answer one question, and worse, a bag cache expiring
// part-way through would judge the first objects against one view of the
// principal and the last against another.
//
// So the enumeration fixture asserts the memo COUNTABLY, not by wall clock:
// however many objects a decision touches, the resolver is called exactly once
// for the principal and once for the account. That is the claim, it cannot flake,
// and TestAttributeDecisionResolvesEachSubjectOnce runs it in the default
// `make test`.
//
// # Why the counter sits ABOVE the registry cache
//
// countingResolver wraps the *provider.AttributeRegistry rather than the
// AttributeProvider under it. Counting underneath would prove nothing: the
// registry caches a bag per slot per key, so a naive resolve-per-evaluation
// implementation would still show ONE provider fetch and look identical to a
// memoized one. The memo's evidence is the number of RESOLUTIONS a decision
// performs, which is what this counter measures. The provider fetches are counted
// too, and asserted, but as a statement about the cache rather than about the
// memo.
//
// It is also why the memo's payoff is deliberately NOT read off this fixture's
// wall clock: here the "directory" is an in-process map behind a never-expiring
// cache, so a redundant resolution costs a map lookup. In a real deployment it
// costs a network round trip. The timing half of this file exists to prove the
// attribute path as a whole clears the committed NFR; the counting half is what
// proves the memo is still doing its job.

// The attribute fixture's identifiers. It is a SEPARATE store, provider registry
// and rules engine from buildModel's, for the same reason enumerate_test.go
// builds its own: the existing benchmarks are the committed baseline for the
// cached Check, and wiring attribute resolvers into their rules engine would
// change what every one of their numbers measures.
const (
	attrAccount     = "acctattr"
	attrRole        = "roleattr"
	attrUser        = "userattr"
	attrUnknownUser = "userattr-unlisted"
	attrProject     = "projattr"
)

// The rule-variant names. Each is simultaneously the rule reference, the document
// action whose permission carries the inclusive;rule= strategy that reaches it,
// and the benchmark/gate sub-name — so a number maps to exactly one rendered
// expression, exactly as the collection fixture's variants do.
const (
	// attrPrincipal reads a field the user directory supplies, off `principal`.
	attrPrincipal = "attr-principal"
	// attrAccountRule reads a field the account directory supplies, off `account`.
	// It is spelled with the Rule suffix because attrAccount above is the account
	// ID; the two are different things and a shared name would eventually put one
	// where the other belongs.
	attrAccountRule = "attr-account"
	// attrBoth reads principal, account AND object metadata in one rule — the
	// realistic policy shape, and the only variant that pays all three resolutions
	// in a single evaluation.
	attrBoth = "attr-both"
	// attrAbsent reads the same principal field as attrPrincipal, for a principal
	// the directory has no record for. The resolver reports the documented absence
	// (nil bag, no error), the decision runs against the floor, and the comparison
	// is false. It is this file's only DENY.
	attrAbsent = "attr-absent"
)

// attrEnumerateLabel names the enumeration sub-case in the gate's output. It is
// not a rule variant — the enumeration reuses attrBoth's rule — but it is a
// distinct measured shape and gets its own line.
const attrEnumerateLabel = "attr-enumerate"

// The directory values the rules compare against. They are ordinary metadata
// scalars: an attribute bag is a value in the SAME value model as object
// metadata (provider/attribute.go), so nothing here is a special shape.
const (
	attrTier = "gold"
	attrPlan = "enterprise"
	attrDept = "platform"
)

// attributeEnumerateCandidates is how many documents the enumeration fixture
// holds by default: the existing member bound, so the memo assertion is made at
// the largest member set a rule-backed enumeration can produce rather than at a
// size chosen to be convenient. A memo that resolved per object would be doing it
// this many times.
var attributeEnumerateCandidates = scope.DefaultMaxMembers

// attributeVariant is one attribute-reading benchmark case: the rule AST, the
// principal the Check is made as, and the verdict the warm-up asserts.
//
// principal is per variant because attr-absent's whole point is a subject the
// directory does not know; allow is per variant for the reason ruleVariant states
// — a fixture that silently stops deciding what it was written to decide must
// fail loudly rather than measure the other branch.
type attributeVariant struct {
	name      string
	ast       *rules.Node
	principal string
	render    string
	allow     bool
}

// attributeVariants is the closed set of attribute-reading cases the gate covers.
func attributeVariants() []attributeVariant {
	return []attributeVariant{
		{
			name:      attrPrincipal,
			ast:       rules.Compare(rules.OpEq, rules.Var("principal.tier"), rules.Lit(attrTier)),
			principal: attrUser,
			render:    `native infix over the user slot: (principal?.tier == "gold")`,
			allow:     true,
		},
		{
			name:      attrAccountRule,
			ast:       rules.Compare(rules.OpEq, rules.Var("account.plan"), rules.Lit(attrPlan)),
			principal: attrUser,
			render:    `native infix over the account slot: (account?.plan == "enterprise")`,
			allow:     true,
		},
		{
			// Three roots in one rule, and the principal side compared against
			// OBJECT metadata rather than a literal — the ownership shape a real
			// policy is written in, and the one that makes a decision pay the
			// object fetch and both bag resolutions together.
			name: attrBoth,
			ast: rules.And(
				rules.Compare(rules.OpEq, rules.Var("principal.dept"), rules.Var("object.owner.dept")),
				rules.Compare(rules.OpEq, rules.Var("account.plan"), rules.Lit(attrPlan)),
				rules.Compare(rules.OpEq, rules.Var("object.classification"), rules.Lit("public")),
			),
			principal: attrUser,
			render:    `and of three: (principal?.dept == object?.owner?.dept), (account?.plan == ...), (object?.classification == ...)`,
			allow:     true,
		},
		{
			// The absence path. Same comparison as attr-principal, different
			// SUBJECT: the directory has no record for this principal, so the
			// registry's leniency turns APERTURE_NOT_FOUND into a nil bag and the
			// rule sees the floor alone. Reading it as a delta from attr-principal
			// is what separates "resolving a bag costs X" from "the comparison
			// costs X".
			name:      attrAbsent,
			ast:       rules.Compare(rules.OpEq, rules.Var("principal.tier"), rules.Lit(attrTier)),
			principal: attrUnknownUser,
			render:    `same render as attr-principal, floor-only bag: (principal?.tier == "gold") -> false`,
			allow:     false,
		},
	}
}

// countingAttributeProvider counts host round trips: calls that reach the
// directory itself, BELOW the registry's per-slot cache.
//
// It embeds the interface rather than reimplementing it, so it stays a faithful
// AttributeProvider if the seam grows a method — a hand-written stub would
// silently stop compiling, which is fine, but a hand-written stub that still
// compiles because the method has a default is exactly how a fixture drifts.
type countingAttributeProvider struct {
	provider.AttributeProvider
	fetches atomic.Int64
}

func (c *countingAttributeProvider) Fetch(ctx context.Context, id string) (provider.Metadata, error) {
	c.fetches.Add(1)
	return c.AttributeProvider.Fetch(ctx, id)
}

// countingResolver wraps the attribute registry at the RESOLVER seam — the two
// methods rules.Engine calls once it decides it needs a bag — and counts the
// calls.
//
// This is the memo's instrument. Every call it records is one resolution the
// decision performed; the per-decision memo (rules.DecisionAttributes) is the
// only thing between "one per decision" and "one per rule evaluation", which for
// an Enumerate is one per object. See the file doc for why counting below the
// registry cache instead would prove nothing.
type countingResolver struct {
	reg       *provider.AttributeRegistry
	principal atomic.Int64
	account   atomic.Int64
}

func (c *countingResolver) Attributes(ctx context.Context, kind, principal string) (map[string]any, error) {
	c.principal.Add(1)
	return c.reg.Attributes(ctx, kind, principal)
}

func (c *countingResolver) AccountAttributes(ctx context.Context, account string) (map[string]any, error) {
	c.account.Add(1)
	return c.reg.AccountAttributes(ctx, account)
}

// reset zeroes the counters so one measurement cannot read another's calls.
func (c *countingResolver) reset() {
	c.principal.Store(0)
	c.account.Store(0)
}

// counts returns the resolutions recorded since the last reset.
func (c *countingResolver) counts() (principal, account int64) {
	return c.principal.Load(), c.account.Load()
}

// attributeModel is the self-contained attribute fixture: a seeded store, the
// object provider registry, the attribute registry and its two counted
// directories, the rules engine wired to both resolver seams, one Check request
// per variant, and the enumeration query.
type attributeModel struct {
	store      *memory.Store
	registry   *provider.Registry
	resolver   *countingResolver
	users      *countingAttributeProvider
	accounts   *countingAttributeProvider
	rules      *rules.Engine
	reqs       map[string]engine.Request
	enumQuery  service.EnumerateQuery
	candidates int
	// enumWant is how many ids the enumeration must return: every candidate is
	// selected by the rule, so it is the candidate count clamped by the existing
	// member bound.
	enumWant int
}

// buildAttributeModel seeds the attribute fixture over `candidates` documents,
// all of which attr-both's rule selects.
//
// Every candidate is selected on purpose, for the reason buildEnumerateModel
// states: a rule that filtered some out would make the per-candidate number
// depend on selectivity rather than on the machinery, and selecting all of them
// is also the worst case, because a rejected candidate skips the second
// (per-decision) evaluation.
func buildAttributeModel(tb testing.TB, candidates int) attributeModel {
	tb.Helper()
	ctx := context.Background()
	store := memory.New()

	must := func(err error) {
		tb.Helper()
		if err != nil {
			tb.Fatalf("seed: %v", err)
		}
	}

	actions := make([]string, 0, len(attributeVariants()))
	for _, v := range attributeVariants() {
		actions = append(actions, v.name)
	}
	must(store.PutObjectType(ctx, model.ObjectType{Name: "document", Actions: actions}))
	must(store.PutAccount(ctx, model.Account{ID: attrAccount, Name: attrAccount}))
	must(store.PutRole(ctx, model.Role{ID: attrRole, Name: attrRole}))
	// Both principals hold the same role and therefore the same grants. The ONLY
	// difference between them is whether the directory has a record — which is
	// what makes attr-absent a measurement of the absence path rather than of a
	// different grant set.
	for _, id := range []string{attrUser, attrUnknownUser} {
		must(store.PutPrincipal(ctx, model.Principal{
			ID: id, Kind: model.PrincipalUser, Identity: "user:" + id,
			RoleIDs: []string{attrRole},
		}))
	}

	// The objects. One metadata bag shape for every candidate, so the object half
	// of a decision is byte-identical across variants and the measured difference
	// is the rule.
	objects := make([]provider.Object, 0, candidates)
	for i := 0; i < candidates; i++ {
		id, err := identity.Parse(attrObjectID(i))
		if err != nil {
			tb.Fatalf("parse candidate %d: %v", i, err)
		}
		objects = append(objects, provider.Object{ID: id, Metadata: attrObjectMetadata()})
	}
	static, err := provider.NewStatic(objects)
	if err != nil {
		tb.Fatalf("build static object provider: %v", err)
	}
	// TTL 0 (never expire) for the reason buildRuleLayer disables it: the NFR is
	// about the CACHED decision, and a default TTL would let a long gated run
	// silently start re-fetching part-way through and report a mix of the cached
	// and uncached paths.
	reg := provider.NewRegistry()
	reg.MustRegister("document", static, provider.WithTTL(0))

	// The directories. Their bags are ordinary metadata — the shared value model,
	// validated by the same ValidateMetadata an object bag is — so a shape the
	// evaluator cannot survive is refused here rather than discovered mid-Check.
	users, err := provider.NewStaticAttributes([]provider.AttributeRecord{{
		ID: attrUser,
		Attributes: provider.Metadata{
			"tier":      attrTier,
			"dept":      attrDept,
			"clearance": 3,
		},
	}})
	if err != nil {
		tb.Fatalf("build user directory: %v", err)
	}
	accounts, err := provider.NewStaticAttributes([]provider.AttributeRecord{{
		ID: attrAccount,
		Attributes: provider.Metadata{
			"plan":   attrPlan,
			"region": "us-east",
		},
	}})
	if err != nil {
		tb.Fatalf("build account directory: %v", err)
	}
	countedUsers := &countingAttributeProvider{AttributeProvider: users}
	countedAccounts := &countingAttributeProvider{AttributeProvider: accounts}

	attrs := provider.NewAttributeRegistry()
	attrs.MustRegister(provider.AttributeSlotUser, countedUsers, provider.WithTTL(0))
	attrs.MustRegister(provider.AttributeSlotAccount, countedAccounts, provider.WithTTL(0))
	// The MACHINE slot is deliberately left unregistered. Nothing here reads it —
	// every principal in the fixture is a user — and registering a directory for a
	// slot no rule consults would be fixture that claims coverage it does not have.
	resolver := &countingResolver{reg: attrs}

	source := rules.MapSource{}
	reqs := make(map[string]engine.Request, len(attributeVariants()))
	for _, v := range attributeVariants() {
		source[v.name] = &rules.Rule{Name: v.name, AST: v.ast}
		must(store.PutPermission(ctx, model.Permission{
			ID: "perm-" + v.name, ObjectType: "document", Action: v.name,
			ScopeStrategy: "inclusive;rule=" + v.name,
		}))
		// ONE account-wide grant per variant, whose membership is decided entirely
		// by the rule — so what the measurement contains is the attribute-reading
		// resolution path and not a large grant set being walked.
		must(store.PutGrant(ctx, model.Grant{
			ID: "g-" + v.name, AccountID: attrAccount,
			Subject:      model.Subject{Kind: model.SubjectRole, ID: attrRole},
			PermissionID: "perm-" + v.name,
			Object:       "account:" + attrAccount + "/**",
			Effect:       model.EffectAllow,
		}))
		reqs[v.name] = engine.Request{
			Account:   attrAccount,
			Principal: v.principal,
			Action:    v.name,
			Object:    attrObjectID(0),
		}
	}

	want := candidates
	if want > scope.DefaultMaxMembers {
		want = scope.DefaultMaxMembers
	}
	return attributeModel{
		store:    store,
		registry: reg,
		resolver: resolver,
		users:    countedUsers,
		accounts: countedAccounts,
		// Both resolver seams are the SAME object, which is the wiring a host
		// actually uses: one *provider.AttributeRegistry holds every slot and every
		// slot's cache, and rules has two option names for it only because Go has
		// no overloading (see rules.AccountResolver).
		rules: rules.NewEngine(source, reg,
			rules.WithPrincipalResolver(resolver),
			rules.WithAccountResolver(resolver)),
		reqs: reqs,
		// The enumeration reuses attr-both's rule: the richest of the four, and the
		// one whose per-object cost would be paid N times over by a decision that
		// resolved its bags per evaluation.
		enumQuery: service.EnumerateQuery{
			Account:   attrAccount,
			Principal: attrUser,
			Action:    attrBoth,
			Pattern:   "account:" + attrAccount + "/project:" + attrProject + "/document:*",
		},
		candidates: candidates,
		enumWant:   want,
	}
}

// attrObjectID renders the i-th candidate's canonical identity. Fixed-width so
// the canonical order Enumerate returns matches the declaration order.
func attrObjectID(i int) string {
	return fmt.Sprintf("account:%s/project:%s/document:doc%05d", attrAccount, attrProject, i)
}

// attrObjectMetadata is the object half of every attribute decision: the field
// attr-both compares the principal's department against, plus the scalar every
// variant's object clause reads.
func attrObjectMetadata() provider.Metadata {
	return provider.Metadata{
		"classification": "public",
		"owner":          map[string]any{"dept": attrDept, "team": "access"},
	}
}

// newAttributeService builds the decision facade over the attribute fixture WITH
// scope resolution wired, so an inclusive;rule= grant actually consults the rules
// engine, the object registry and both attribute directories.
//
// The audit axis is wired through the same newAuditedService the other two
// service constructors use, so all three agree on what "audit on" means.
func newAttributeService(tb testing.TB, m attributeModel, withAudit bool) (*service.Service, func()) {
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

// attributeQuery returns the Check query for a variant, failing loudly rather
// than benchmarking a zero request if the fixture ever stops seeding it.
func attributeQuery(tb testing.TB, m attributeModel, variant string) service.Query {
	tb.Helper()
	req, ok := m.reqs[variant]
	if !ok {
		tb.Fatalf("attribute fixture has no variant %q", variant)
	}
	return toQuery(req)
}

// TestAttributeDecisionResolvesEachSubjectOnce is the UNGATED, always-on guard
// that per-decision memoization is still doing its job. It runs in the default
// `make test`.
//
// It asserts a COUNT, not a duration, so it cannot flake and does not care what
// machine it runs on — and the count is the thing the memo actually promises. A
// decision resolves the principal once and the account once, however many rule
// evaluations it performs and however many objects it touches. Take the memo away
// and the Check numbers barely move (this fixture's directory is an in-process
// map), but the enumeration's resolution count becomes proportional to the
// candidate set: one host round trip per object, to learn the same two facts
// every time. That is the regression this test exists to catch, and it is
// invisible to every correctness test in the repo because the decisions would all
// still be right.
//
// The provider fetch counts are asserted too, but they say something different:
// they are the registry's per-slot CACHE holding, not the memo. Both matter, and
// keeping them separate is what stops one from being read as evidence for the
// other.
func TestAttributeDecisionResolvesEachSubjectOnce(t *testing.T) {
	ctx := context.Background()
	m := buildAttributeModel(t, attributeEnumerateCandidates)
	svc, closeFn := newAttributeService(t, m, false)
	defer closeFn()

	// A single Check: one decision, one object.
	m.resolver.reset()
	res, err := svc.Check(ctx, attributeQuery(t, m, attrBoth))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !res.Allow {
		t.Fatalf("Check: allow=false, want true (reason: %s)", res.Reason)
	}
	if p, a := m.resolver.counts(); p != 1 || a != 1 {
		t.Fatalf("one Check resolved the principal %d times and the account %d times, want 1 and 1; "+
			"the per-decision memo (rules.DecisionAttributes) is not spanning the decision", p, a)
	}

	// The shape that matters: one decision, many objects, one rule. The member
	// gather evaluates the rule once per listed candidate and the engine then
	// decides once per surviving candidate, so this is thousands of evaluations —
	// and still exactly two resolutions.
	m.resolver.reset()
	ids, err := svc.Enumerate(ctx, m.enumQuery)
	if err != nil {
		t.Fatalf("Enumerate: %v", err)
	}
	if len(ids) != m.enumWant {
		t.Fatalf("Enumerate over %d candidates returned %d ids, want %d",
			m.candidates, len(ids), m.enumWant)
	}
	p, a := m.resolver.counts()
	if p != 1 || a != 1 {
		t.Fatalf("an Enumerate over %d objects resolved the principal %d times and the account %d "+
			"times, want 1 and 1; without the per-decision memo this is one host round trip PER "+
			"OBJECT to learn the same two facts, and a bag cache expiring part-way through would "+
			"judge the first objects against one view of the principal and the last against another",
			m.enumWant, p, a)
	}
	t.Logf("Enumerate over %d objects: %d principal resolutions, %d account resolutions, "+
		"%d user-directory fetches, %d account-directory fetches",
		m.enumWant, p, a, m.users.fetches.Load(), m.accounts.fetches.Load())

	// The directories themselves were read once each, for the whole fixture — the
	// registry's per-slot cache, not the memo. Counted from process start rather
	// than from a reset because that is the claim: over every decision this test
	// has made, the host was asked once.
	if got := m.users.fetches.Load(); got != 1 {
		t.Errorf("the user directory was read %d times, want 1; the user slot's cache is not holding", got)
	}
	if got := m.accounts.fetches.Load(); got != 1 {
		t.Errorf("the account directory was read %d times, want 1; the account slot's cache is not holding", got)
	}
}

// TestAttributeFixtureReadsWhatItClaimsTo pins the fixture to its own premise:
// every variant must actually reach the attribute path it is named for.
//
// Without this, an attribute rule that silently stopped reading its bag — a
// renamed field, a directory that stopped being registered, a resolver that
// stopped being wired — would keep DECIDING (the floor is always present, and a
// comparison against a missing field is simply false) and keep being measured, so
// the gate would report healthy numbers for a path it no longer touches. That is
// the same failure mode this whole story exists to close, one level down.
func TestAttributeFixtureReadsWhatItClaimsTo(t *testing.T) {
	ctx := context.Background()
	m := buildAttributeModel(t, 4)
	svc, closeFn := newAttributeService(t, m, false)
	defer closeFn()

	for _, v := range attributeVariants() {
		t.Run(v.name, func(t *testing.T) {
			m.resolver.reset()
			res, err := svc.Check(ctx, attributeQuery(t, m, v.name))
			if err != nil {
				t.Fatalf("Check: %v", err)
			}
			if res.Allow != v.allow {
				t.Fatalf("Check: allow=%v want %v (reason: %s) — the fixture is measuring the "+
					"other branch", res.Allow, v.allow, res.Reason)
			}
			p, a := m.resolver.counts()
			if p != 1 || a != 1 {
				t.Fatalf("%s resolved the principal %d times and the account %d times, want 1 and 1",
					v.name, p, a)
			}
		})
	}
}

// BenchmarkCheckAttributes reports ns/op, allocs/op and p99 for a cached Check
// per attribute variant, on the audit-off path. `make bench` picks it up with no
// target change.
func BenchmarkCheckAttributes(b *testing.B) {
	m := buildAttributeModel(b, 1)
	svc, closeFn := newAttributeService(b, m, false)
	defer closeFn()
	ctx := context.Background()

	for _, v := range attributeVariants() {
		b.Run(v.name, func(b *testing.B) {
			q := attributeQuery(b, m, v.name)
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

// BenchmarkCheckAttributesThroughput reports sustained parallel throughput per
// attribute variant — the checks/sec half of the NFR, on the attribute path.
func BenchmarkCheckAttributesThroughput(b *testing.B) {
	m := buildAttributeModel(b, 1)
	svc, closeFn := newAttributeService(b, m, false)
	defer closeFn()

	for _, v := range attributeVariants() {
		b.Run(v.name, func(b *testing.B) {
			q := attributeQuery(b, m, v.name)
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

// BenchmarkEnumerateAttributes sweeps the attribute-reading enumeration across
// candidate-set sizes and publishes the per-candidate cost, plus the number of
// attribute RESOLUTIONS each Enumerate performed.
//
// The resolutions metric is the point. It should read 2.0 at every size — one
// principal, one account, per decision — so the curve of ns/candidate can be read
// as the cost of the decision machinery rather than of a directory being asked
// the same question once per object.
func BenchmarkEnumerateAttributes(b *testing.B) {
	ctx := context.Background()
	for _, n := range []int{10, 100, scope.DefaultMaxMembers} {
		b.Run(fmt.Sprintf("candidates-%d", n), func(b *testing.B) {
			m := buildAttributeModel(b, n)
			svc, closeFn := newAttributeService(b, m, false)
			defer closeFn()
			if ids, err := svc.Enumerate(ctx, m.enumQuery); err != nil || len(ids) != m.enumWant {
				b.Fatalf("warm Enumerate: %d ids (want %d) err=%v", len(ids), m.enumWant, err)
			}

			m.resolver.reset()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				ids, err := svc.Enumerate(ctx, m.enumQuery)
				if err != nil {
					b.Fatalf("Enumerate: %v", err)
				}
				if len(ids) != m.enumWant {
					b.Fatalf("Enumerate returned %d ids, want %d", len(ids), m.enumWant)
				}
			}
			b.StopTimer()
			if b.N > 0 && m.enumWant > 0 {
				b.ReportMetric(
					float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(m.enumWant),
					"ns/candidate")
				p, a := m.resolver.counts()
				b.ReportMetric(float64(p+a)/float64(b.N), "resolutions/op")
			}
			b.ReportMetric(float64(m.enumWant), "ids")
		})
	}
}

// TestCheckNFRAttributes is the attribute half of the hard NFR gate: the same
// p99 < 1 ms / >= 10 000 decisions per second assertion as TestCheckNFR, over
// rules that read `principal.*`, `account.*`, both-with-object-metadata, and an
// absent bag — with decision audit both ON and OFF.
//
// It is gated identically (APERTURE_BENCH_ASSERT=1, skipped under -short), and
// its NAME deliberately contains "TestCheckNFR" so the documented invocation
//
//	APERTURE_BENCH_ASSERT=1 go test -run TestCheckNFR ./bench/
//
// — whose -run pattern is an unanchored regexp — covers these cases with no
// command change. That is not a stylistic choice: a gate case reachable only by a
// second, undocumented command is a gate case that will not be run.
func TestCheckNFRAttributes(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping NFR wall-clock assertion under -short")
	}
	if os.Getenv("APERTURE_BENCH_ASSERT") != "1" {
		t.Skip("set APERTURE_BENCH_ASSERT=1 to run the hard NFR latency/throughput gate")
	}

	m := buildAttributeModel(t, attributeEnumerateCandidates)
	for _, withAudit := range []bool{false, true} {
		name := "audit-off"
		if withAudit {
			name = "audit-on"
		}
		svc, closeFn := newAttributeService(t, m, withAudit)
		for _, v := range attributeVariants() {
			t.Run(name+"/"+v.name, func(t *testing.T) {
				assertCheckNFR(t, svc, attributeQuery(t, m, v.name), name+"/"+v.name, v.allow, variantSamples)
			})
		}
		t.Run(name+"/"+attrEnumerateLabel, func(t *testing.T) {
			assertEnumerateAttributeNFR(t, m, svc, name+"/"+attrEnumerateLabel)
		})
		closeFn()
	}
}

// enumerateNFRRuns is how many enumerations the enumeration gate measures over.
// Each one is a full member gather plus a decision per surviving candidate at the
// existing member bound, so a handful of runs is already hundreds of thousands of
// rule evaluations — enough for a stable rate and small enough that the gate stays
// a gate rather than a benchmark suite.
const enumerateNFRRuns = 20

// assertEnumerateAttributeNFR is the enumeration shape's half of the gate: one
// decision, many objects, one rule.
//
// TWO assertions, and they are about different things.
//
// The first is the throughput floor, and it introduces NO new threshold. An
// Enumerate makes one authorization decision per surviving candidate, so
// decisions-per-second is the same unit and the same rate the committed NFR
// already states — throughputMin, shared with assertCheckNFR — applied to a shape
// that produces its decisions in bulk. The denominator is the number of ids
// RETURNED, which undercounts the real work (the member gather evaluates the rule
// once more per listed candidate and is not charged), so the measured rate is
// conservative in the direction that makes the assertion honest.
//
// The second is the memo, asserted as a count: enumerateNFRRuns enumerations must
// perform exactly enumerateNFRRuns principal resolutions and the same number of
// account resolutions — one of each per decision, not one per object. It is here
// as well as in the ungated test because this is where it is measured at the
// bound, under both audit settings, and because a memo that quietly stopped
// spanning the decision would show up in this test's OWN throughput number first.
func assertEnumerateAttributeNFR(t *testing.T, m attributeModel, svc *service.Service, label string) {
	t.Helper()
	ctx := context.Background()

	// Warm the parsed-pattern, compiled-rule and metadata caches so the measured
	// window is the steady state rather than first-call setup.
	if ids, err := svc.Enumerate(ctx, m.enumQuery); err != nil || len(ids) != m.enumWant {
		t.Fatalf("%s: warm Enumerate returned %d ids (want %d): %v", label, len(ids), m.enumWant, err)
	}

	m.resolver.reset()
	start := time.Now()
	for i := 0; i < enumerateNFRRuns; i++ {
		ids, err := svc.Enumerate(ctx, m.enumQuery)
		if err != nil {
			t.Fatalf("%s: Enumerate: %v", label, err)
		}
		if len(ids) != m.enumWant {
			t.Fatalf("%s: Enumerate returned %d ids, want %d", label, len(ids), m.enumWant)
		}
	}
	elapsed := time.Since(start)

	decisions := float64(enumerateNFRRuns * m.enumWant)
	rate := decisions / elapsed.Seconds()
	t.Logf("%s: %d enumerations x %d objects = %.0f decisions in %v = %.0f decisions/sec (floor %.0f)",
		label, enumerateNFRRuns, m.enumWant, decisions, elapsed, rate, throughputMin)
	if rate < throughputMin {
		t.Errorf("%s: %.0f decisions/sec is below the NFR floor %.0f", label, rate, throughputMin)
	}

	p, a := m.resolver.counts()
	if p != enumerateNFRRuns || a != enumerateNFRRuns {
		t.Errorf("%s: %d enumerations over %d objects each performed %d principal and %d account "+
			"resolutions, want %d and %d — one per DECISION. A count proportional to the object "+
			"count means the per-decision memo has stopped spanning the decision, which is a host "+
			"round trip per object and a bag that can change mid-enumeration",
			label, enumerateNFRRuns, m.enumWant, p, a, enumerateNFRRuns, enumerateNFRRuns)
	}
}
