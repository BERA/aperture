package rules

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/frankbardon/aperture/identity"
)

// E4-S1: the principal and the account are resolved ONCE per decision.
//
// The instrument is a directory that COUNTS its calls and can change its answer
// between them, which is what makes "resolved once" observable at all. A
// directory that always answered the same thing would pass whether it was
// consulted once or a thousand times — the same reason now_test.go drives the
// instant with a clock that advances on every read.

// countingDirectory is a PrincipalResolver and an AccountResolver in one type
// (as *provider.AttributeRegistry is), counting every call and recording the
// exact map it handed out so a test can assert bag IDENTITY, not just equality.
type countingDirectory struct {
	mu sync.Mutex
	// tiers is keyed kind+"/"+id, so the same principal id under two kinds is two
	// different subjects — the discriminating case for the memo's key.
	tiers map[string]string
	plans map[string]string
	// failNext, when set, fails the next principal resolution and clears itself.
	failNext error

	principalCalls int
	accountCalls   int
	principalBags  []map[string]any
	accountBags    []map[string]any
}

func newCountingDirectory() *countingDirectory {
	return &countingDirectory{
		tiers: map[string]string{"user/alice": "gold", "user/bob": "bronze", "machine/alice": "bronze"},
		plans: map[string]string{"acme": "enterprise", "globex": "free"},
	}
}

func (d *countingDirectory) Attributes(_ context.Context, kind, principal string) (map[string]any, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.principalCalls++
	if err := d.failNext; err != nil {
		d.failNext = nil
		return nil, err
	}
	bag := map[string]any{"tier": d.tiers[kind+"/"+principal]}
	d.principalBags = append(d.principalBags, bag)
	return bag, nil
}

func (d *countingDirectory) AccountAttributes(_ context.Context, account string) (map[string]any, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.accountCalls++
	bag := map[string]any{"plan": d.plans[account]}
	d.accountBags = append(d.accountBags, bag)
	return bag, nil
}

func (d *countingDirectory) counts() (int, int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.principalCalls, d.accountCalls
}

// setTier changes what the directory will answer with from now on. The point of
// the memo is that a decision already under way does not see it.
func (d *countingDirectory) setTier(key, tier string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.tiers[key] = tier
}

// memoEngine is an engine whose rules read one attribute each: "gold" reads the
// principal bag, "paid" reads the account bag. Two rules rather than one
// conjunction, because Selected resolves BOTH bags whatever the rule reads, and
// keeping them separate makes a miscounted slot obvious.
func memoEngine(dir *countingDirectory) *Engine {
	return NewEngine(MapSource{
		"gold": {Name: "gold", AST: Compare(OpEq, Var("principal.tier"), Lit("gold"))},
		"paid": {Name: "paid", AST: Compare(OpEq, Var("account.plan"), Lit("enterprise"))},
	}, nil, WithPrincipalResolver(dir), WithAccountResolver(dir))
}

// sameMap reports whether two maps are the SAME map, not merely equal ones. Bag
// identity is the property under test: two evaluations that got equal copies
// could still have paid for two fetches.
func sameMap(a, b map[string]any) bool {
	return reflect.ValueOf(a).Pointer() == reflect.ValueOf(b).Pointer()
}

// TestABagIsResolvedOncePerDecision is the story's central criterion: however
// many objects and rules a decision evaluates, the principal is resolved once and
// the account is resolved once.
func TestABagIsResolvedOncePerDecision(t *testing.T) {
	dir := newCountingDirectory()
	eng := memoEngine(dir)
	ctx, _ := WithDecisionAttributes(context.Background())

	for _, obj := range []string{"document:1", "document:2", "document:3"} {
		for _, rule := range []string{"gold", "paid"} {
			ok, err := eng.Selected(ctx, rule, identity.MustParse(obj), "acme", "user", "alice", "read")
			if err != nil {
				t.Fatalf("Selected(%s, %s): %v", rule, obj, err)
			}
			if !ok {
				t.Fatalf("rule %q must select for alice in acme", rule)
			}
		}
	}

	principal, account := dir.counts()
	if principal != 1 {
		t.Errorf("six evaluations resolved the principal %d times, want exactly 1", principal)
	}
	if account != 1 {
		t.Errorf("six evaluations resolved the account %d times, want exactly 1", account)
	}
}

// TestTwoEvaluationsObserveTheIdenticalBag is the consistency half of the story,
// and it is the half that is not about speed. The directory's answer CHANGES
// between the two evaluations — exactly what a cache expiring mid-decision does —
// and the decision must not notice: both evaluations read the same map, and the
// second still selects on the value the first one saw.
func TestTwoEvaluationsObserveTheIdenticalBag(t *testing.T) {
	dir := newCountingDirectory()
	eng := memoEngine(dir)
	ctx, scope := WithDecisionAttributes(context.Background())
	obj := identity.MustParse("document:1")

	if ok, err := eng.Selected(ctx, "gold", obj, "acme", "user", "alice", "read"); err != nil || !ok {
		t.Fatalf("first evaluation: selected=%v err=%v, want true/nil", ok, err)
	}
	dir.setTier("user/alice", "bronze")
	if ok, err := eng.Selected(ctx, "gold", obj, "acme", "user", "alice", "read"); err != nil || !ok {
		t.Fatalf("second evaluation saw the directory change mid-decision: selected=%v err=%v", ok, err)
	}

	if principal, _ := dir.counts(); principal != 1 {
		t.Fatalf("the principal was resolved %d times, want exactly 1", principal)
	}
	bag, ok := scope.Principal()
	if !ok {
		t.Fatal("the scope reports no principal bag was resolved, but two evaluations read one")
	}
	if !sameMap(bag, dir.principalBags[0]) {
		t.Error("the scope holds a copy of the directory's bag, not the bag itself")
	}
	if account, ok := scope.Account(); !ok || !sameMap(account, dir.accountBags[0]) {
		t.Error("the account slot must hold the directory's own bag too")
	}
}

// TestAnUnscopedEvaluationResolvesItsOwnBags: the scope is a memo, never a
// requirement. A host driving rules.Engine directly — or a library caller
// building a rules.Input by hand — still decides correctly, paying one resolution
// per evaluation.
func TestAnUnscopedEvaluationResolvesItsOwnBags(t *testing.T) {
	dir := newCountingDirectory()
	eng := memoEngine(dir)
	ctx := context.Background()
	obj := identity.MustParse("document:1")

	for i := range 2 {
		if ok, err := eng.Selected(ctx, "gold", obj, "acme", "user", "alice", "read"); err != nil || !ok {
			t.Fatalf("unscoped evaluation %d: selected=%v err=%v, want true/nil", i, ok, err)
		}
	}

	principal, account := dir.counts()
	if principal != 2 || account != 2 {
		t.Fatalf("unscoped evaluations resolved principal=%d account=%d, want 2 and 2 — "+
			"an unscoped evaluation takes its own fetch", principal, account)
	}
	if bag, ok := (*DecisionAttributes)(nil).Principal(); ok || bag != nil {
		t.Error("a nil scope must report no bag rather than panicking")
	}
}

// TestTheMemoNeverAnswersForADifferentSubject is the safety property of keying
// the memo. Within one decision the subject never changes, but the scope travels
// on a context and a context goes where it is passed; a memo that answered
// positionally would serve one principal's attributes as another's, which is the
// worst failure this seam has.
func TestTheMemoNeverAnswersForADifferentSubject(t *testing.T) {
	dir := newCountingDirectory()
	eng := memoEngine(dir)
	ctx, _ := WithDecisionAttributes(context.Background())
	obj := identity.MustParse("document:1")

	cases := []struct {
		name           string
		kind, id, acct string
		gold, paid     bool
	}{
		{"alice is a gold user in acme", "user", "alice", "acme", true, true},
		{"bob is bronze in the same account", "user", "bob", "acme", false, true},
		// Same id, different kind: a different subject, and the human directory's
		// answer must not stand in for the machine's.
		{"alice-the-machine is a different subject", "machine", "alice", "acme", false, true},
		{"globex is on the free plan", "user", "alice", "globex", true, false},
	}
	for _, tc := range cases {
		gold, err := eng.Selected(ctx, "gold", obj, tc.acct, tc.kind, tc.id, "read")
		if err != nil {
			t.Fatalf("%s: gold: %v", tc.name, err)
		}
		paid, err := eng.Selected(ctx, "paid", obj, tc.acct, tc.kind, tc.id, "read")
		if err != nil {
			t.Fatalf("%s: paid: %v", tc.name, err)
		}
		if gold != tc.gold || paid != tc.paid {
			t.Errorf("%s: gold=%v paid=%v, want %v/%v — the memo answered for the wrong subject",
				tc.name, gold, paid, tc.gold, tc.paid)
		}
	}
}

// TestAFailedResolutionIsNotMemoized: only a successful resolution is retained. A
// directory that blinks must not freeze its failure into the rest of the
// decision — and what a failure MEANS to a decision stays the resolver's
// contract, not this memo's.
func TestAFailedResolutionIsNotMemoized(t *testing.T) {
	dir := newCountingDirectory()
	dir.failNext = errors.New("directory unreachable")
	eng := memoEngine(dir)
	ctx, scope := WithDecisionAttributes(context.Background())
	obj := identity.MustParse("document:1")

	if _, err := eng.Selected(ctx, "gold", obj, "acme", "user", "alice", "read"); err == nil {
		t.Fatal("a failing directory must surface as an error, not as an empty bag")
	}
	if _, ok := scope.Principal(); ok {
		t.Fatal("a failed resolution must not be memoized")
	}
	if ok, err := eng.Selected(ctx, "gold", obj, "acme", "user", "alice", "read"); err != nil || !ok {
		t.Fatalf("the retry after a transient failure: selected=%v err=%v, want true/nil", ok, err)
	}
	if principal, _ := dir.counts(); principal != 2 {
		t.Fatalf("the principal was resolved %d times, want 2 — the failure must not be retained", principal)
	}
}

// TestOneResolutionAcrossAConcurrentFanOut: a decision may fan out across
// goroutines (a batch surface, a parallel enumeration) and must still agree on
// who is asking. The resolver is called under the memo's lock precisely so a
// concurrent fan-out yields ONE fetch rather than N racing ones. Run with -race.
func TestOneResolutionAcrossAConcurrentFanOut(t *testing.T) {
	dir := newCountingDirectory()
	eng := memoEngine(dir)
	ctx, _ := WithDecisionAttributes(context.Background())
	obj := identity.MustParse("document:1")

	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, err := eng.Selected(ctx, "gold", obj, "acme", "user", "alice", "read"); err != nil || !ok {
				t.Errorf("concurrent evaluation: selected=%v err=%v, want true/nil", ok, err)
			}
		}()
	}
	wg.Wait()

	principal, account := dir.counts()
	if principal != 1 || account != 1 {
		t.Fatalf("a 16-way fan-out resolved principal=%d account=%d, want 1 and 1", principal, account)
	}
}

// TestTheMemoAddsNoAllocationToTheFastPath guards the property the Check NFR
// rests on: an engine with no attribute resolver wired allocates no more inside a
// decision scope than outside one. The memo's whole state is two strings and a
// map header assigned into a struct that already exists, so being scoped must
// cost nothing per evaluation.
func TestTheMemoAddsNoAllocationToTheFastPath(t *testing.T) {
	// No resolvers: the floor engine, which is the configuration the NFR measures.
	eng := NewEngine(MapSource{
		"floor": {Name: "floor", AST: Compare(OpEq, Var("principal.id"), Lit("alice"))},
	}, nil)
	obj := identity.MustParse("document:1")
	unscoped := context.Background()
	scoped, _ := WithDecisionAttributes(unscoped)

	measure := func(ctx context.Context) float64 {
		return testing.AllocsPerRun(200, func() {
			if _, err := eng.Selected(ctx, "floor", obj, "acme", "user", "alice", "read"); err != nil {
				t.Fatalf("Selected: %v", err)
			}
		})
	}
	base := measure(unscoped)
	if got := measure(scoped); got > base {
		t.Fatalf("evaluating inside a decision scope allocated %v times, want no more than the "+
			"unscoped %v — the memo must add nothing to the cached fast path", got, base)
	}
}
