package rules

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/frankbardon/aperture/identity"
)

// fakeClock is a manually advanced clock so TTL expiry is exercised without
// sleeping, keeping the test deterministic.
type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func TestCacheReusesCompiledProgram(t *testing.T) {
	eng := NewEngine(MapSource{}, nil)
	rule := Compare(OpEq, Var("object.tier"), Lit("gold"))

	first, err := eng.Compile(rule)
	if err != nil {
		t.Fatalf("first compile: %v", err)
	}
	second, err := eng.Compile(rule)
	if err != nil {
		t.Fatalf("second compile: %v", err)
	}
	if first != second {
		t.Fatalf("expected the cached *Compiled to be reused (same pointer)")
	}

	st := eng.CacheStats()
	if st.Hits != 1 || st.Misses != 1 || st.Entries != 1 {
		t.Fatalf("stats = %+v, want hits=1 misses=1 entries=1", st)
	}
}

// TestCacheKeyedByCanonicalForm proves two distinct rule definitions whose ASTs
// render to the same expression share one compiled program (keyed by hash).
func TestCacheKeyedByCanonicalForm(t *testing.T) {
	eng := NewEngine(MapSource{}, nil)
	// Same canonical expression, two independently-built AST values.
	a := Compare(OpEq, Var("object.tier"), Lit("gold"))
	b := Compare(OpEq, Var("object.tier"), Lit("gold"))

	ca, err := eng.Compile(a)
	if err != nil {
		t.Fatalf("compile a: %v", err)
	}
	cb, err := eng.Compile(b)
	if err != nil {
		t.Fatalf("compile b: %v", err)
	}
	if ca.Hash() != cb.Hash() {
		t.Fatalf("identical canonical forms must hash equally")
	}
	if ca != cb {
		t.Fatalf("identical canonical forms must share the cached program")
	}
}

func TestCacheTTLExpiry(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	eng := NewEngine(MapSource{}, nil, WithCacheTTL(time.Minute), WithClock(clk))
	rule := Compare(OpEq, Var("object.tier"), Lit("gold"))

	first, err := eng.Compile(rule)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	// Within the TTL: a hit, same program.
	clk.advance(30 * time.Second)
	again, err := eng.Compile(rule)
	if err != nil {
		t.Fatalf("compile within ttl: %v", err)
	}
	if again != first {
		t.Fatalf("within TTL the cached program must be reused")
	}

	// Past the TTL: the entry expires, recompiles, and is counted as eviction.
	clk.advance(2 * time.Minute)
	fresh, err := eng.Compile(rule)
	if err != nil {
		t.Fatalf("compile after ttl: %v", err)
	}
	if fresh == first {
		t.Fatalf("after TTL expiry a fresh program must be compiled")
	}
	st := eng.CacheStats()
	if st.Evictions == 0 {
		t.Fatalf("expected at least one eviction after TTL expiry; stats = %+v", st)
	}
}

func TestCacheInvalidateAll(t *testing.T) {
	eng := NewEngine(MapSource{}, nil)
	rule := Compare(OpEq, Var("object.tier"), Lit("gold"))
	first, _ := eng.Compile(rule)
	eng.InvalidateAll()
	if st := eng.CacheStats(); st.Entries != 0 {
		t.Fatalf("InvalidateAll should empty the cache; entries = %d", st.Entries)
	}
	second, err := eng.Compile(rule)
	if err != nil {
		t.Fatalf("recompile: %v", err)
	}
	if second == first {
		t.Fatalf("after invalidation a fresh program must be compiled")
	}
}

func TestCacheInvalidateOne(t *testing.T) {
	eng := NewEngine(MapSource{}, nil)
	rule := Compare(OpEq, Var("object.tier"), Lit("gold"))
	first, _ := eng.Compile(rule)

	dropped, err := eng.Invalidate(rule)
	if err != nil {
		t.Fatalf("invalidate: %v", err)
	}
	if !dropped {
		t.Fatalf("Invalidate should report the entry was present")
	}
	if again, _ := eng.Invalidate(rule); again {
		t.Fatalf("second Invalidate should report nothing cached")
	}
	second, err := eng.Compile(rule)
	if err != nil {
		t.Fatalf("recompile: %v", err)
	}
	if second == first {
		t.Fatalf("after per-rule invalidation a fresh program must be compiled")
	}
}

// TestCacheConcurrent runs concurrent compiles of the same rule to surface data
// races under the race detector and confirm the cache stays consistent.
func TestCacheConcurrent(t *testing.T) {
	eng := NewEngine(MapSource{}, nil)
	rule := Compare(OpEq, Var("object.tier"), Lit("gold"))
	const n = 50
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			_, err := eng.Compile(rule)
			errs <- err
		}()
	}
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent compile: %v", err)
		}
	}
	if st := eng.CacheStats(); st.Entries != 1 {
		t.Fatalf("concurrent compiles of one rule must yield a single entry; stats = %+v", st)
	}
}

// TestCacheHitCountIsExactUnderConcurrency is what makes the atomic counters
// CORRECT rather than merely faster.
//
// The hit path no longer takes the write lock to bump its counter, so nothing in
// the mutex discipline serialises two concurrent hits any more — a non-atomic
// increment would lose updates, and would lose them silently: every decision
// would still be right, only the observability number would drift. Run it with
// -race and the count is also the race detector's target.
//
// The arithmetic is exact by construction, not approximate:
//
//   - the cache is warmed FIRST, so exactly one miss is recorded and every
//     subsequent get finds a live entry;
//   - no TTL is configured, so nothing can expire mid-run and turn a hit into a
//     miss + eviction;
//   - each Selected performs exactly one cache get (Engine.compile), so the
//     final hit count must be goroutines x perGoroutine on the nose.
func TestCacheHitCountIsExactUnderConcurrency(t *testing.T) {
	const (
		goroutines   = 32
		perGoroutine = 100
	)
	eng := newTestEngine()
	ctx := context.Background()
	object := identity.MustParse("account:acme/document:1")

	// Warm: the single compile that populates the entry, so the concurrent phase
	// below is all hits and the miss count is pinned at one.
	if _, err := eng.Selected(ctx, "public", object, "alice", "read"); err != nil {
		t.Fatalf("warm Selected: %v", err)
	}
	if st := eng.CacheStats(); st.Hits != 0 || st.Misses != 1 {
		t.Fatalf("after warming, stats = %+v, want hits=0 misses=1", st)
	}

	var wg sync.WaitGroup
	errs := make(chan error, goroutines)
	start := make(chan struct{})
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // release everyone at once, so the bumps genuinely overlap
			for i := 0; i < perGoroutine; i++ {
				if _, err := eng.Selected(ctx, "public", object, "alice", "read"); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Selected: %v", err)
	}

	st := eng.CacheStats()
	const wantHits = goroutines * perGoroutine
	if st.Hits != wantHits {
		t.Errorf("hits = %d, want exactly %d (one per evaluation); a lost update means "+
			"the counter increment is not atomic", st.Hits, wantHits)
	}
	if st.Misses != 1 {
		t.Errorf("misses = %d, want 1 (only the warming compile); stats = %+v", st.Misses, st)
	}
	if st.Evictions != 0 {
		t.Errorf("evictions = %d, want 0 (no TTL is configured); stats = %+v", st.Evictions, st)
	}
	if st.Entries != 1 {
		t.Errorf("entries = %d, want 1; stats = %+v", st.Entries, st)
	}
}
