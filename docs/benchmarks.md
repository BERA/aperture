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

Re-measured on the same Apple M1 Max, default `-benchtime`. The literal-scope
baseline above is **unchanged** by this fixture (61 890 B/op, 34 allocs/op, both
before and after), which is what makes the rule numbers readable as deltas.

Cached `Check` through a rule-backed grant, audit off (`BenchmarkCheckRule`):

| Variant | ns/op | B/op | allocs/op | `p99-ns` | parallel checks/sec |
|---|---:|---:|---:|---:|---:|
| `rule-scalar` (control) | 58 520 | 15 700 | 61 | 226 792 | 41 220 |
| `rule-literal-in` | 64 314 | 25 577 | 89 | 255 167 | 43 889 |
| `rule-nested` | 60 368 | 15 716 | 62 | 220 792 | 49 410 |
| `rule-tags-typical` (12) | 58 396 | 16 432 | 68 | 254 167 | 46 795 |
| `rule-tags-at-cap` (7 281) | **157 114** | **139 357** | 68 | 465 584 | **10 197** |

The gated `TestCheckNFRCollections`, single-goroutine over 20 000 samples:

| Variant | p99 (audit off / on) | checks/sec (audit off / on) | verdict |
|---|---|---|---|
| `rule-scalar` | 227 µs / 208 µs | 15 975 / 16 553 | pass |
| `rule-literal-in` | 251 µs / 238 µs | 15 227 / 15 636 | pass |
| `rule-nested` | 224 µs / 208 µs | 16 839 / 16 704 | pass |
| `rule-tags-typical` | 225 µs / 216 µs | 16 376 / 16 612 | pass |
| `rule-tags-at-cap` | 475 µs / 471 µs | **6 195 / 6 710** | **FAILS the throughput floor** |

The rule in isolation (`BenchmarkRuleEval`), which strips the ~60 µs of grant
resolution out of the numbers above:

| Variant | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| `rule-scalar` | 148.6 | 96 | 3 |
| `rule-literal-in` | 166.9 | 96 | 3 |
| `rule-nested` | 209.5 | 112 | 4 |
| `rule-tags-typical` | 531.4 | 592 | 8 |
| `rule-tags-at-cap` | 77 931 | 123 280 | 8 |

**What passes cleanly.** Optional chaining (`rule-nested`) costs +1 alloc/op and
+16 B/op over a flat field — it is a render-time concern and stays one at
runtime. The list-literal render E5-S1 deliberately left native evaluates
identically to a scalar comparison (166.9 ns vs 148.6 ns, same 3 allocs), so the
common `object.region in [...]` shape pays nothing for the operator. A realistic
12-tag collection adds ~380 ns and ~500 B per `Check` — noise against a 58 µs
decision.

## Findings

Two costs the collection fixtures surfaced. Both are **reported, not tuned
around**: the benchmarks assert the honest budget and currently fail, and neither
is fixed here (E5-S2 is a measurement story, and `rules/` is not its to change).

### 1. The guarded dispatcher copies the collection operand on every evaluation

`BenchmarkCollectionScaling` shows the cost is **linear in array length**, with
no fixed component worth speaking of:

| Elements | field bytes | ns/op | B/op | allocs/op |
|---:|---:|---:|---:|---:|
| 12 | 108 | 519.3 | 592 | 8 |
| 128 | 1 152 | 1 721 | 2 704 | 8 |
| 1 024 | 9 216 | 11 431 | 18 832 | 8 |
| 4 096 | 36 864 | 42 589 | 65 936 | 8 |
| 7 281 | 65 529 | 88 344 | 123 280 | 8 |

That is **~12 ns and ~16.9 bytes per element, per `Check`** — and 16 B/element is
exactly the cost of a fresh `[]any` backing store. The mechanism is
`rules/shape.go`, `classify`:

```go
o.members = make([]any, rv.Len())
for i := range o.members {
    o.members[i] = rv.Index(i).Interface()
}
```

Every guarded collection comparison materialises a fresh `[]any` copy of the
operand, filled element-by-element through `reflect`, before the operator runs.
It is reflect-based so that any slice kind works, but the validated metadata
model only ever admits `[]any` (see the `metadata-values` skill), so for the
shape that actually reaches it the copy buys nothing.

This is the exact failure mode E5-S2 was written to catch: `provider` goes to
trouble to hand metadata out by reference and to make that contract transitive,
and the value is copied anyway one layer up. It is invisible to every correctness
test — the decision is identical either way.

**Consequence for the 64 KiB cap.** With the fixture's baseline `Check` at
~62 µs, the budget to stay above the 10 000 checks/sec floor (100 µs/`Check`) is
~38 µs, which at ~12 ns/element is roughly **3 000 elements — about 40 % of what
the default cap permits**. The cap-maximal 7 281-element array lands at
6 195–6 710 checks/sec, ~0.6× the floor. Note that **p99 is not the failing
half**: it stays at ~475 µs, comfortably under the 1 ms ceiling. It is sustained
throughput that breaks.

So the interview's flagged risk is real, and there are two independent levers:
remove the copy (a `[]any` fast path in `classify` would take back the 16.9
B/element and most of the 12 ns), or lower `DefaultMaxValueBytes`, which is
configurable through `provider.ValueLimits` precisely so it can be revisited.
Which one — or both — is a decision, not a benchmark's call.

### 2. The compiled-rule cache does not remove the AST walk

`rules.Engine.Selected` calls `Engine.Compile` on **every** evaluation, and
`Compile` re-validates the AST, re-renders it to expression source, and re-hashes
that source *before* it can probe the compiled-program cache. The cache removes
the expr-lang compile; it does not remove the walk, whose cost scales with node
count (`BenchmarkRuleCompileCached`):

| Variant | AST nodes | ns/op | B/op | allocs/op |
|---|---:|---:|---:|---:|
| `rule-scalar` | 3 | 2 246 | 5 203 | 19 |
| `rule-nested` | 3 | 2 178 | 5 203 | 19 |
| `rule-tags-typical` | 4 | 2 509 | 5 419 | 21 |
| `rule-tags-at-cap` | 4 | 2 591 | 5 419 | 21 |
| `rule-literal-in` | 6 | 5 503 | 15 066 | 47 |

The dominant term is **per literal node**: ~14 allocs and ~4.9 KB each, because
`Validate` → `decodeScalar` builds a `json.NewDecoder` per literal on every
decision. That fully accounts for `rule-literal-in`'s +28 allocs/op and
+9.9 KB/op at the `Check` level (+28.1 and +9 876 measured) even though
`BenchmarkRuleEval` shows the two rules evaluating identically.

Even the minimal 3-node rule pays 2.2 µs, 19 allocs, and 5.2 KB per `Check` for a
walk whose result is already cached — a third of `rule-scalar`'s entire 15.7 KB
allocation. This is not a collections problem; the collection fixtures merely made
it legible.

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

**Known failing, by design, as of E5-S2** — see [Findings](#findings):

| Gate | Case | Why |
|---|---|---|
| `TestCheckNFRCollections` | `rule-tags-at-cap` | 6 195–6 710 checks/sec against a 10 000 floor, at the largest array the default 64 KiB cap permits |
| `TestCollectionCheckAllocations` | per-element copy detector | 16.9 B/element against an 8 B budget |
| `TestCollectionCheckAllocations` | `rule-literal-in` alloc budget | +28 allocs/op from the per-`Check` AST re-validation |

These were left asserting the honest budget rather than relaxed to green: the
numbers are the deliverable. Whichever way the cap-versus-copy decision goes, the
budgets are what the fix has to satisfy.
