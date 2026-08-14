# Benchmarks & the performance NFR (FR-31)

Aperture's decision hot path carries a hard Success Metric:

> **p99 cached `Check` < 1 ms** and **≥ 10 000 checks/sec/instance.**

This document describes how that NFR is measured, how it is asserted without
flaking CI, and the committed numbers from the optimization pass (E4-S4).

## The suite

Everything lives in [`bench/`](../bench). It seeds a **sizable** authorization
model (not a three-grant toy) and exercises the full decision facade
(`service.Service.Check`), so the numbers reflect what a surface actually pays.

### Fixture (`bench/fixture_test.go`)

`buildModel` seeds an in-memory store with the generic org → project → document
domain at scale:

- 8 accounts, 60 roles, 60 groups, 480 principals;
- per account, every role carries wildcard allow-read / allow-write on its own
  project plus a more-specific deny-read carve-out, every group a
  document-scoped read, and `group0` an account-wide broad read plus a broad
  deny — overlapping candidates of differing specificity;
- 60 concrete (wildcard-free) document grants per account so `Enumerate` has a
  real, bounded candidate set to materialise.

The representative cached `Check` (`user0` reading a document under `proj0`)
resolves a subject set of six subjects to **~73 applicable grants**, of which
several match at different specificities — so deny-overrides + the specificity
tiebreak genuinely run, rather than short-circuiting on a single grant.

#### The rule-backed extension (E5-S2)

Everything above resolves through **literal** scope, which never reaches the
provider registry or the rules engine. The metadata-collections effort put array
iteration, map traversal, and E5-S1's guarded dispatcher on the same hot path, so
`buildModel` additionally seeds a rule-backed half: one `inclusive;rule=…`
permission and grant per **rule variant**, all bounded to a single object whose
metadata is served by a one-object `ObjectProvider` registered with expiry
disabled (`provider.WithTTL(0)` — the NFR is about the *cached* `Check`, and the
30 s default would silently start re-fetching partway through a long run).

The rule grants hang off a **dedicated principal and role** (`user-rules` /
`role-rules`) that `user0` does not hold, so the literal-scope baseline above is
measured on exactly the subject set and candidate grants it was measured on
before. (Attaching them to `role0` instead moved that baseline by +5 allocs/op
and +2 KB/op — a fixture that changes the number it is the control for.)

All five variants check the **same object**, so the provider fetch and its cache
hit are byte-identical across them and the measured delta is purely the rule:

| Variant | Rule | Renders to |
|---|---|---|
| `rule-scalar` | `object.classification == "public"` | native infix — **the control** |
| `rule-literal-in` | `object.region in ["us-east", …]` | native infix; the collection operand is a **list literal**, so E5-S1 leaves it unguarded |
| `rule-nested` | `object.owner.dept == "platform"` | E2-S2 **optional chaining**: `object?.owner?.dept` |
| `rule-tags-typical` | `object.tags hasAny [...]` over 12 tags | E5-S1 **guarded dispatcher** `$op("hasAny", …)` — the collection operand is a *variable* |
| `rule-tags-at-cap` | the same over **7 281** tags | the same guarded dispatcher |

##### Why 7 281 tags

The array sizes are **derived from the value model's caps**, not picked for
roundness. `provider.ValueBytes` measures a `[]any` of strings as the sum of
their lengths, and `provider.DefaultMaxValueBytes` is 64 KiB **per field value**,
so at the fixture's fixed 9-byte tag width (`tag-00042`) the cap fixes an exact
element count:

```text
floor(65536 / 9) = 7281 tags = 65 529 bytes;  7282 tags = 65 538 — over the cap
```

7 281 is therefore the **largest array a host can legally load under the
defaults** — the adversarial-but-permitted worst case the NFR has to survive.
`TestCollectionFixtureSitsAtTheValueCap` asserts that derivation rather than
leaving it in a comment, so a change to the caps fails there instead of quietly
benchmarking a different array. `rule-tags-typical`'s 12 tags (108 bytes, 0.16%
of the cap) is the realistic other end of the same axis; the pair is what makes
the scaling behaviour visible.

Both `hasAny` cases match on the **last** element on purpose: the operator
short-circuits on the first hit, so an early match would report a best case a
real tag list does not guarantee.

### Benchmarks (informational — `make bench`)

| Benchmark | What it reports |
|---|---|
| `BenchmarkCheckCachedAuditOff` / `...AuditOn` | single cached `Check` ns/op, allocs/op, and a computed `p99-ns` |
| `BenchmarkCheckThroughputAuditOff` / `...AuditOn` | sustained parallel throughput as a `checks/sec` metric |
| `BenchmarkEnumerateBounded` | bounded `Enumerate`; asserts the result never exceeds `engine.DefaultEnumerateLimit` |
| `BenchmarkCheckRule/<variant>` | cached `Check` through a rule-backed grant, per variant (E5-S2) |
| `BenchmarkCheckRuleThroughput/<variant>` | the same, as sustained parallel `checks/sec` |
| `BenchmarkRuleEval/<variant>` | the rule **in isolation** — compiled once, evaluated over the same metadata, so the fixture's ~60 µs of grant resolution cannot mask it |
| `BenchmarkCollectionScaling/tags-N` | the guarded collection operator swept across array sizes, with the field's `ValueBytes` reported alongside |
| `BenchmarkRuleCompileCached/<variant>` | the AST work a `Check` pays on **every** decision even when the compiled program is cached, with the AST's node count alongside |

`make bench` runs them all with `-benchmem`. The **audit toggle** is the
benchmark axis: audit-off is the `s.audit == nil` path; audit-on wires a sampled
(1%), asynchronous `audit.Recorder` — the production shape where decision audit
is off the critical path (E4-S2).

### The hard NFR assertion (gated — `TestCheckNFR`)

Wall-clock assertions are environment-sensitive, so the **hard** gate is a test
that is **off by default**:

- it `t.Skip`s under `go test -short`, **and**
- it `t.Skip`s unless `APERTURE_BENCH_ASSERT=1` is set.

The default `make test` therefore never runs a wall-clock assertion and stays
fast and deterministic. Run the gate explicitly:

```sh
APERTURE_BENCH_ASSERT=1 go test -run TestCheckNFR ./bench/
```

Methodology inside the gate:

- **p99:** time 100 000 cached `Check`s on a warm engine, sort the per-op
  latencies, take the 99th percentile, assert `< 1 ms`.
- **throughput:** run 200 000 cached `Check`s, divide by wall time, assert
  `≥ 10 000 checks/sec` (a conservative single-goroutine floor; a real instance
  parallelises well above it — see the throughput benchmark).
- both are run with audit **on** and **off**.

`TestCheckNFRCollections` (E5-S2) applies the **same two thresholds** to each
rule variant, audit on and off. Its name deliberately contains `TestCheckNFR` so
the invocation above — whose `-run` pattern is an unanchored regexp — picks it up
with no command change. It measures over 20 000 samples rather than 100 000/200
000 because the gate multiplies out over ten variant × audit combinations; the
thresholds are *rates*, so the smaller sample does not weaken them.

### The allocation guard (E5-S2)

`provider` hands a cached `Metadata` out **by reference** and documents the
read-only contract as **transitive** over nested values. A defensive deep copy
introduced on that read path would be invisible to every correctness test in the
repo — the decisions would stay identical — and fatal to the NFR, because it
would put an O(metadata) copy on the per-`Check` hot path. Two tests cover it,
deliberately at different costs:

| Test | Gated? | What it does |
|---|---|---|
| `TestMetadataIsSharedByReference` | **no** — runs in `make test` | structural: the map, its nested object, and its nested arrays returned by two successive `Fetch`es must be the **same objects by address**. Cannot flake, costs nothing, fails the instant someone clones on read. |
| `TestCollectionCheckAllocations` | yes (`APERTURE_BENCH_ASSERT=1`) | measured: the per-`Check` allocation profile of every variant, with a **per-element** copy detector |

The measured detector differences the two collection cases **against each other**
rather than against the scalar control. That cancels the guarded dispatcher's
fixed per-evaluation overhead exactly and leaves **bytes per array element** —
which is the actual signature of a copy. Sharing by reference reads as ~0 B per
element; materialising the array into a fresh `[]any` costs 16 B per element for
the backing store alone, so the budget sits at 8 B, well clear of both.

## Committed results

Measured on an Apple M1 Max (`go test -benchtime=2s`); absolute numbers are
hardware-dependent, but the **headroom** and the **allocation profile** are the
durable signal.

| Metric (cached `Check`) | audit off | audit on |
|---|---|---|
| mean latency | ~66 µs/op | ~70 µs/op |
| allocations | 34 allocs/op | 34 allocs/op |
| p99 (gated `TestCheckNFR`, 100k samples) | ~0.275 ms | ~0.265 ms |
| throughput (single goroutine) | ~15 100 checks/sec | ~14 700 checks/sec |
| throughput (parallel benchmark) | ~20 000 checks/sec | ~30 000 checks/sec |

Both targets are met with comfortable headroom — p99 sits ~3.6× under the 1 ms
ceiling, and even the single-goroutine throughput clears the 10 k/s floor by
~1.5×, before parallelism. **Audit-on does not regress the target:** sampling is
a single `Sampler.Sample()` call on the un-kept path and the kept event is built
lazily and written asynchronously, so the decision never blocks on audit.

### Collection & nested-object rules (E5-S2)

Re-measured on the same Apple M1 Max, default `-benchtime`, **after both E5-S2
findings were fixed** (the `[]any` fast path in `rules/shape.go` and the literal
fast path in `rules/ast.go` — see [Findings](#findings) for the before/after).
The literal-scope baseline above is **unchanged** by this fixture (61 890 B/op,
34 allocs/op, both before and after), which is what makes the rule numbers
readable as deltas.

Cached `Check` through a rule-backed grant, audit off (`BenchmarkCheckRule`):

| Variant | ns/op | B/op | allocs/op | `p99-ns` | parallel checks/sec |
|---|---:|---:|---:|---:|---:|
| `rule-scalar` (control) | 54 061 | 10 779 | 47 | 220 708 | 54 557 |
| `rule-literal-in` | 54 047 | 10 812 | 47 | 211 834 | 55 782 |
| `rule-nested` | 56 716 | 10 795 | 48 | 216 625 | 59 501 |
| `rule-tags-typical` (12) | 56 003 | 11 285 | 52 | 221 625 | 55 441 |
| `rule-tags-at-cap` (7 281) | 85 925 | 11 286 | 52 | 273 625 | 45 245 |

The gated `TestCheckNFRCollections`, single-goroutine over 20 000 samples:

| Variant | p99 (audit off / on) | checks/sec (audit off / on) | verdict |
|---|---|---|---|
| `rule-scalar` | 205 µs / 224 µs | 17 593 / 18 586 | pass |
| `rule-literal-in` | 197 µs / 199 µs | 18 039 / 18 133 | pass |
| `rule-nested` | 214 µs / 178 µs | 18 413 / 17 656 | pass |
| `rule-tags-typical` | 239 µs / 188 µs | 18 216 / 17 020 | pass |
| `rule-tags-at-cap` | 260 µs / 251 µs | 11 462 / 11 747 | pass |

The rule in isolation (`BenchmarkRuleEval`), which strips the ~55 µs of grant
resolution out of the numbers above:

| Variant | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| `rule-scalar` | 157.7 | 96 | 3 |
| `rule-literal-in` | 176.6 | 96 | 3 |
| `rule-nested` | 209.3 | 112 | 4 |
| `rule-tags-typical` | 426.5 | 384 | 6 |
| `rule-tags-at-cap` | 27 141 | 384 | 6 |

**What passes cleanly.** Optional chaining (`rule-nested`) costs +1 alloc/op and
+16 B/op over a flat field — it is a render-time concern and stays one at
runtime. The list-literal render E5-S1 deliberately left native evaluates
identically to a scalar comparison (176.6 ns vs 157.7 ns, same 3 allocs) **and now
costs the same per `Check` too** (47 allocs/op against the control's 47) — so the
common `object.region in [...]` shape pays nothing for the operator anywhere. A
realistic 12-tag collection adds ~270 ns and ~500 B per `Check` — noise against a
54 µs decision. Note the collection variants' B/op no longer grows with array
length: `rule-tags-at-cap` allocates 11 286 B against `rule-tags-typical`'s
11 285, over 600× the elements.

## Findings

Two costs the collection fixtures surfaced. Both were **reported, not tuned
around** — the benchmarks were left asserting the honest budget and failing —
and both have since been fixed. The before/after is kept here because the
mechanisms are the interesting part, and because the budgets that caught them are
now the ratchets holding the fixes in place.

### 1. The guarded dispatcher copied the collection operand on every evaluation

**Status: fixed** (`rules/shape.go`). `BenchmarkCollectionScaling` originally
showed a cost **linear in array length** with no fixed component worth speaking
of — and, critically, allocated bytes growing at the same rate:

| Elements | field bytes | ns/op (before → after) | B/op (before → after) | allocs/op |
|---:|---:|---:|---:|---:|
| 12 | 108 | 519.3 → 410.2 | 592 → 384 | 8 → 6 |
| 128 | 1 152 | 1 721 → 892.5 | 2 704 → 384 | 8 → 6 |
| 1 024 | 9 216 | 11 431 → 4 170 | 18 832 → 384 | 8 → 6 |
| 4 096 | 36 864 | 42 589 → 15 669 | 65 936 → 384 | 8 → 6 |
| 7 281 | 65 529 | 88 344 → 27 169 | 123 280 → 384 | 8 → 6 |

The old cost was **~12 ns and ~16.9 bytes per element, per `Check`** — and
16 B/element is exactly the cost of a fresh `[]any` backing store. The mechanism
was `rules/shape.go`, `classify`:

```go
o.members = make([]any, rv.Len())
for i := range o.members {
    o.members[i] = rv.Index(i).Interface()
}
```

Every guarded collection comparison materialised a fresh `[]any` copy of the
operand, filled element-by-element through `reflect`, before the operator ran. The
reflect branch exists so that any slice kind works, but the validated metadata
model only ever admits `[]any` (see the `metadata-values` skill), so for the shape
that actually reaches it the copy bought nothing. `classify` now aliases an
`[]any` operand directly — safe because `members` is read-only after
construction, which is what preserves `provider`'s transitive by-reference
contract — and falls back to the reflect copy for anything else.

Traversal is now **~3.7 ns and 0 bytes per element**: allocated bytes are flat at
384 B from 12 elements to 7 281.

This was the exact failure mode E5-S2 was written to catch: `provider` goes to
trouble to hand metadata out by reference and to make that contract transitive,
and the value was copied anyway one layer up. It is invisible to every
correctness test — the decision is identical either way — which is why the guard
is a measured allocation budget rather than an assertion about behaviour.

**Consequence for the 64 KiB cap.** The interview's flagged risk was real before
the fix: at ~12 ns/element the budget above the 10 000 checks/sec floor
(100 µs/`Check`, of which the fixture's own resolution spent ~62 µs) was roughly
**3 000 elements, about 40 % of what the default cap permits**, and the
cap-maximal 7 281-element array measured 6 195–6 710 checks/sec. At ~3.7
ns/element the same arithmetic gives **~12 500 elements**, comfortably past the
7 281 the cap admits, and that array now measures 11 462–11 747 checks/sec. So
`DefaultMaxValueBytes` does not need lowering: the cap is inside the budget on
this hardware. It stays configurable through `provider.ValueLimits`, and a host
whose per-`Check` resolution is heavier than this fixture's should re-run the
arithmetic with its own baseline rather than inherit the conclusion.

### 2. The compiled-rule cache did not remove the AST walk

**Status: fixed** (`rules/ast.go`, [issue #9]). `rules.Engine.Selected` calls
`Engine.Compile` on **every** evaluation, and `Compile` re-validates the AST,
re-renders it to expression source, and re-hashes that source *before* it can
probe the compiled-program cache. The cache removes the expr-lang compile; it does
not remove the walk. That walk's cost scaled with node count
(`BenchmarkRuleCompileCached`):

| Variant | AST nodes | ns/op (before → after) | B/op (before → after) | allocs/op (before → after) |
|---|---:|---:|---:|---:|
| `rule-scalar` | 3 | 2 246 → 684 | 5 203 → 288 | 19 → 5 |
| `rule-nested` | 3 | 2 178 → 644 | 5 203 → 288 | 19 → 5 |
| `rule-tags-typical` | 4 | 2 509 → 834 | 5 419 → 488 | 21 → 7 |
| `rule-tags-at-cap` | 4 | 2 591 → 875 | 5 419 → 488 | 21 → 7 |
| `rule-literal-in` | 6 | 5 503 → 673 | 15 066 → 320 | 47 → 5 |

The dominant term was **per literal node**: ~14 allocs and ~4.9 KB each, because
`Validate` → `decodeScalar` built a `json.NewDecoder` per literal, on every walk,
twice per decision — the decoder's internal read buffer dwarfing the value it was
parsing. That fully accounted for `rule-literal-in`'s +28 allocs/op and
+9.9 KB/op at the `Check` level (+28.1 and +9 876 measured) even though
`BenchmarkRuleEval` shows the two rules evaluating identically.

The fix is `rules.classifyScalar`: it identifies the common literal forms —
`null`, `true`, `false`, any JSON number, and any string of unescaped printable
ASCII — straight from the raw bytes with **no allocation**, and both `Validate`
and `render` short-circuit on it. It is deliberately **one-sided**: it answers
only where the bytes are provably that form under the JSON grammar, and defers to
`decodeScalar` for escaped or non-ASCII strings, composites, and malformed input.
So it cannot widen what `Validate` accepts and cannot change a rendered byte —
properties pinned by `rules/scalar_test.go`, which runs every input through both
paths and asserts they agree, plus a fuzz target stating the same invariant.

What remains in the walk is flat in literal count: the render buffer, the rendered
string, and the sha256 + hex of it. `rule-literal-in` and `rule-scalar` now cost
the same 47 allocs per `Check` (measured delta **0.0**, down from 28.1), and even
the minimal 3-node rule dropped from 2.2 µs / 19 allocs / 5.2 KB to 0.68 µs / 5
allocs / 288 B. This was never a collections problem; the collection fixtures
merely made it legible.

**Not done: skipping the walk entirely on a cache hit.** Both designs issue #9
sketched — keying the cache on the rule reference, or memoising the rendered
source on the `Node` — trade the current key's best property for speed the fix
above already delivered. The source hash is *content-addressed*, so an edited
rule cannot keep deciding with a stale program by construction; a
reference-keyed cache needs correct invalidation on every mutation path to get
the same guarantee, and a memo on `Node` — a public, mutable struct the Rete
editor edits in place — cannot enforce its own invalidation at all. With the walk
at 0.68 µs against a ~54 µs `Check`, neither risk is worth taking.

[issue #9]: https://github.com/frankbardon/aperture/issues/9

## The optimization pass

`go test -bench -benchmem` identified the dominant per-`Check` allocator: the
literal/scope coverer re-parsed each grant's object **pattern on every candidate
of every Check** (`identity.ParsePattern(g.Object)`), so a principal resolving
~73 grants paid ~73 fresh pattern parses — each a string split plus a new
segment slice.

The fix is a parsed-pattern cache in the engine
([`engine/patterncache.go`](../engine/patterncache.go)): a concurrency-safe
`sync.Map` keyed by the object string, shared by the literal and scope coverers.
A parsed `Pattern` is immutable and a pure function of its source, so a cache hit
returns the identical pattern a fresh parse would — **decision semantics are
unchanged** and the full existing test suite stays green.

Effect on the cached `Check`: **172 → 34 allocs/op** (~5× fewer), with the
re-parse churn and its GC pressure removed from the hot path.

Measure-first discipline: the literal decision hot path does not exercise the
compiled-rule cache (`rules`, E2-S3) or the provider metadata cache (`provider`,
E2-S2) — those already bound their own costs behind hash-keyed / TTL+LRU caches —
so no change was made there absent a benchmark showing a win. E5-S2's rule-backed
fixture is what finally put both of those caches under measurement, and
[Findings](#findings) is what came back.

## Regression guard

`TestCheckNFR` and `TestCheckNFRCollections` are the threshold guards: they fail
if p99 ever crosses 1 ms or throughput drops below 10 k/s, on the literal path and
on each rule variant respectively. They are gated (see above) so they never flake
the default build, but they are wired and runnable on demand and in a dedicated CI
job/cron where the runner is known to be unloaded.
`TestMetadataIsSharedByReference` is the one guard that is **not** gated: it is
structural rather than timed, so it runs in every `make test`.

**All gates now pass.** Three were failing by design as of E5-S2, left asserting
the honest budget rather than relaxed to green — the numbers were the
deliverable, and the budgets were what a fix had to satisfy. Each is now a
ratchet rather than an open finding — see [Findings](#findings):

| Gate | Case | Budget | Was | Now |
|---|---|---|---|---|
| `TestCheckNFRCollections` | `rule-tags-at-cap` | ≥ 10 000 checks/sec at the largest array the default 64 KiB cap permits | 6 195–6 710 | 11 462–11 747 |
| `TestCollectionCheckAllocations` | per-element copy detector | ≤ 8 B/element | 16.9 B | ~0 B |
| `TestCollectionCheckAllocations` | `rule-literal-in` alloc budget | ≤ 2 allocs/op over the scalar control (was held at 30) | +28.1 | +0.0 |

The `rule-literal-in` budget in particular is now tight on purpose: any
per-literal decode reintroduced into the AST walk scales with literal count and
blows straight past 2.
