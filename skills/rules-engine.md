---
name: rules-engine
description: The rules engine evaluates a JSON rule AST as an expr-lang expression over object metadata and principal/action context, compiling once and caching, to back the inclusive/exclusive scope resolvers.
applies_to: [cli, http, mcp]
---

# Rules engine

The `rules` package decides object-membership selection (and, by extension,
allow/deny) from a domain object's metadata plus the principal/action context. It
is the rule-backed variant of the inclusive/exclusive scope resolvers (E2-S1): an
`*rules.Engine` satisfies `scope.RuleEvaluator`, so wiring it as
`engine.ScopeDeps{Rules: eng}` turns on rule-driven scope membership.

Expressions are evaluated by [`expr-lang/expr`](https://github.com/expr-lang/expr)
**directly**: `rules` renders each AST to an expr-lang expression and compiles it
in-process with expr-lang's pure-Go evaluator. Aperture does not hand-roll a
parser, has **no dependency on Pulse**, and stays `CGO_ENABLED=0`. Any older doc
calling this a "Pulse expression" is stale — `rules` imports
`github.com/expr-lang/expr` and that is the whole engine.

## The rule AST (the editor + state-file contract)

A rule is a tree of typed `rules.Node` values. The set is small, explicit, and
closed, so the Rete.js editor (E7-S2) maps its palette one-to-one and the state
file (E5-S2) persists the same shape. There is no second rule format.

| `type` | Fields | Meaning |
|---|---|---|
| `and` / `or` | `children` (>= 2) | logical conjunction / disjunction |
| `not` | `children` (exactly 1) | logical negation |
| `compare` | `op`, `left`, `right` | comparison; `right` is omitted for the unary ops, and is a two-item `list` for `between` |
| `var` | `name` (dotted path) | context variable reference |
| `literal` | `value` (scalar JSON) | string / number / bool / null constant |
| `list` | `items` | list literal (right side of a collection op) |
| `call` | `name`, `items` (args) | call to a registered pure function |

The JSON form is stable and round-trips (marshal -> unmarshal -> marshal is
byte-identical), including falsy literals (`false`, `0`, `""`, `null`) and the
unary operators, whose `right` key stays absent.

```json
{"type":"compare","op":"eq",
 "left":{"type":"var","name":"object.classification"},
 "right":{"type":"literal","value":"public"}}
```

## Operators

Scalar comparisons — `left` and `right` are both required, any operand node:

| `op` | Reads as | Renders to |
|---|---|---|
| `eq` `ne` | `object.tier == "gold"` | `==` `!=` |
| `lt` `le` `gt` `ge` | `object.level >= 3` | `<` `<=` `>` `>=` |

Collection operators. `left` is the collection (or, for `exists`, any path);
`right` is the operand, and is **omitted entirely** for the three unary ops:

| `op` | Reads as | `left` applies to | `right` |
|---|---|---|---|
| `in` | `object.region in ["us","eu"]` | any | list or var |
| `nin` | `principal.id not in object.blocklist` | any | list or var |
| `has` | `object.tags has "urgent"` | array | one element (never a list) |
| `hasAll` | `object.tags has all ["a","b"]` | array | list or var |
| `hasAny` | `object.tags has any ["a","b"]` | array | list or var |
| `hasNone` | `object.tags has none ["a","b"]` | array | list or var |
| `subsetOf` | `object.tags subset of ["a","b"]` | array | list or var |
| `hasKey` | `object.owner has key "dept"` | object | one element (never a list) |
| `isEmpty` | `object.tags is empty` | array, object | **omitted** |
| `isNotEmpty` | `object.tags is not empty` | array, object | **omitted** |
| `exists` | `object.owner.dept exists` | any path | **omitted** |

Date operators. `left` is the date-valued field; `right` is another date operand
— a literal or a variable — except for `between`, which takes **two bounds**:

| `op` | Reads as | `right` |
|---|---|---|
| `before` | `object.hired_at before "2026-01-01"` | one element (strict) |
| `after` | `object.hired_at after "2026-01-01"` | one element (strict) |
| `onOrBefore` | `object.hired_at on or before "2026-01-01"` | one element (inclusive) |
| `onOrAfter` | `object.hired_at on or after "2026-01-01"` | one element (inclusive) |
| `between` | `object.hired_at between ["2026-01-01","2026-12-31"]` | a `list` of exactly two bounds |
| `sameDay` | `object.hired_at same day as "2026-03-04"` | one element |
| `sameMonth` | `object.hired_at same month as "2026-03-04"` | one element |
| `sameYear` | `object.hired_at same year as "2026-03-04"` | one element |

A date operand is one of the two canonical strings the value model defines —
`2006-01-02` or `2006-01-02T15:04:05Z` (see the `metadata-values` skill). Four
properties are worth stating outright:

- **`between` is INCLUSIVE AT BOTH ENDS.** `between [lo, hi]` is exactly
  `onOrAfter lo && onOrBefore hi`. Bounds are never reordered: a `lo` above the
  `hi` is a range that matches nothing, which is what the author wrote — and it
  is **noted**, because "matched nothing" is otherwise indistinguishable from a
  window nothing happens to fall in.
- **Granularity never affects ordering.** `"2026-03-04"` and
  `"2026-03-04T00:00:00Z"` name the same instant and compare **equal**. Ordering
  is over instants, never over text — which is also why no date operator renders
  to a native `<`.
- **`sameDay` / `sameMonth` / `sameYear` are calendar-bucket EQUALITY, in UTC.**
  Not distance: `2026-03-31` and `2026-04-01` are a day apart and are **not** in
  the same month, and `2026-03-04` and `2025-03-04` share a month number but are
  **not** in the same month. There is no timezone conversion anywhere and no
  timezone knob — every instant in the model is already UTC.
- **A date operand that will not parse denies, and never raises** — see
  "Malformed dates" below.

`between` needs two right-hand operands and `compare` carries one `right`, so
`right` is a `list` of exactly two items. **No new node type, no new field, and
no new JSON key**: every rule persisted before dates existed loads and
re-marshals byte-identically, and the author's one `between` node reads back as
one node rather than as an `and` of two comparisons.

```json
{"type":"compare","op":"between",
 "left":{"type":"var","name":"object.hired_at"},
 "right":{"type":"list","items":[
   {"type":"literal","value":"2026-01-01"},
   {"type":"literal","value":"2026-12-31"}]}}
```

Operand rules are enforced by `Validate` and return `APERTURE_RULE_INVALID`:

- The three unary ops require `right == nil`; supplying one is an error, not
  something ignored, so a rule has exactly one spelling.
- Every other op requires both operands.
- `in` `nin` `hasAll` `hasAny` `hasNone` `subsetOf` need a `list` or a `var` on
  the right — a set.
- `has` `hasKey` and the eight single-operand date ops need a single element on
  the right; a `list` there is an error.
- `between` needs a `list` of **exactly two** bounds on the right — one bound,
  three bounds, an empty list, or a bare operand are all rejected.

Build a unary node with `rules.Unary(op, left)`, a `between` node with
`rules.Between(left, low, high)`, and everything else with
`rules.Compare(op, left, right)`.

### How each operator compiles

`expr.DisableAllBuiltins()` is on, so nothing is assumed reachable. Each operator
either renders to native expr syntax or calls a pure function the compiler
registers:

- **Native.** `eq ne lt le gt ge in nin` are infix. `has` and `hasKey` render as
  a *flipped* `in` (`"urgent" in object?.tags`) — expr's `in` is element
  membership over an array **and key membership over an object**, so `hasKey`
  needs no new machinery. `exists` renders as `(left != nil)`.
- **Registered pure functions.** `hasAll` `hasAny` `hasNone` `subsetOf`
  `isEmpty` `isNotEmpty` have no native spelling with builtins off, so each is
  backed by a deterministic, side-effect-free function in the curated set.
- **Guarded.** Whichever of the two forms above applies, a collection operator
  whose collection operand is not *statically* a collection — anything but a
  list literal, so in practice any operand reading metadata — renders instead to
  the internal dispatcher `$op(op, __notes, leftPath, left, rightPath, right)`,
  which applies the deny-safe shape policy below and records the note. The
  common `object.region in ["us","eu"]` keeps its native `in`: a list literal is
  an array by construction and cannot mismatch, so the decision path pays
  nothing. Neither `$op` nor `__notes` is reachable from a rule — `$` is outside
  the name grammar `Validate` enforces, and `__notes` is not an exposed context
  root — so the guard is compiler-only by construction, not by denylist.
- **Dates are always guarded.** Every date operator renders to the internal
  dispatcher `$date(op, __notes, leftPath, left, rightPath, right, right2Path,
  right2)` — the last pair is `between`'s upper bound and is `""`/`nil` for
  every other date operator, so one arity covers the binary and the ternary
  operators. There is **no** native fallback, and that is a hard rule: the values
  are canonical date *strings*, so a native `<` would compare text (which orders
  `"2026-03-04"` before `"2026-03-04T00:00:00Z"` although they are the same
  instant), and parsing to `time.Time` first is worse — `time.Time < string` is a
  **compile** error in expr, so one mistyped operand would make the entire rule
  uncompilable instead of degrading one comparison. `$date` is unreachable from a
  rule for the same structural reason as `$op`.

**expr's predicate builtins are denied.** `expr.DisableAllBuiltins()` does not
reach `all`, `any`, `none`, `one`, `filter`, `map`, `count`, `sum`, `find`,
`findIndex`, `findLast`, `findLastIndex`, `groupBy`, `sortBy`, `reduce` — the
parser resolves those names before consulting the disabled table. `Validate`
rejects a `call` node naming any of them with `APERTURE_RULE_INVALID`.

## Context variables

The expression environment exposes four roots; a variable under any other root is
an unknown variable, rejected at validation:

- `object` — the object's metadata fields (read-only snapshot from the provider).
- `principal` — principal attributes; `principal.id` always present. Richer
  attributes come from a `PrincipalResolver` (`WithPrincipalResolver`).
- `account` — account attributes (reserved; empty until wired).
- `action` — the action verb (a string).

## Missing fields: nested access is nil-safe, but `nin` grants

Metadata is ragged — one object carries `owner`, the next does not. Two rules of
thumb, and the second is a trap.

**Nested access is nil-safe.** A `var` may read a nested path
(`object.owner.dept`). Every segment after the root renders with optional
chaining — `object?.owner?.dept` — so a **missing intermediate yields nil and the
enclosing comparison goes false**, never a runtime error. Without it, expr-lang
raises `cannot fetch dept from <nil>` and the rule fails with
`APERTURE_RULE_EVAL` at `Check` time on exactly the objects that lack the field.
A present path is unaffected. This is **render-time only**: the AST and its JSON
still store the plain dotted path, so the editor/state-file contract is unchanged.

**⚠️ `x not in <absent field>` is `true` — a `nin` rule GRANTS on objects missing
the field.** This is pre-existing expr-lang behavior (the correct dual of
`x in <nil>` being `false`), not something Aperture added, but list-valued and
nested metadata make it easy to hit: a deny-list over a column an object does not
have passes everything. Require the field explicitly when that matters:

```go
rules.And(
    rules.Compare(rules.OpNe, rules.Var("object.blocklist"), rules.Lit(nil)),
    rules.Compare(rules.OpNin, rules.Var("principal.id"), rules.Var("object.blocklist")),
)
```

Other missing-field behavior: equality against a missing field is `false`; an
ordered comparison (`lt le gt ge`) against one is an `APERTURE_RULE_EVAL` runtime
error.

**Every collection operator follows the same rule: an absent field reads as an
EMPTY collection, never an error.** Which way that falls depends on the
operator's polarity, and the negative ones grant just like `nin`:

| Field absent | `has` | `hasAll` | `hasAny` | `hasKey` | `isNotEmpty` | `exists` | `hasNone` | `subsetOf` | `isEmpty` |
|---|---|---|---|---|---|---|---|---|---|
| result | `false` | `false` | `false` | `false` | `false` | `false` | **`true`** | **`true`** | **`true`** |

## Wrong-shaped fields: deny-safe, and noted

A field of the **wrong shape** — `has` over a string, `hasAll` over a number,
`isEmpty` over a bool — **evaluates to `false`**. Every collection operator, no
exceptions, including the negative ones: `nin` over a string is `false`, not
`true`. A mismatch never matches, whatever the operator's polarity, so mistyped
data can only ever deny.

It is never an `APERTURE_RULE_EVAL` error. Load-time validation of the value
model stops mistyped data from the CSV and inline loaders, but it cannot cover a
host-implemented `ObjectProvider` or the `principal` attribute bag, which bypass
loaders entirely — and one mistyped field must not break every `Check` that
touches it.

Note the asymmetry with the absent-field table above, and that it is deliberate:

| operand | policy |
|---|---|
| absent (`nil`) | reads as an **empty collection**; the operator's own semantics decide (negative ops match) |
| wrong shape | the comparison is **`false`**, whatever the operator |

Which shapes each operator accepts:

| operators | accepts | expected-shape wording in a note |
|---|---|---|
| `in` `nin` `has` `hasKey` | array (elements) or object (keys) | `collection` |
| `hasAll` `hasAny` `hasNone` `subsetOf` | array | `array` |
| `isEmpty` `isNotEmpty` | array, object, or string | `array, object, or string` |
| `exists` | anything — a nil test cannot mismatch | — |

## Malformed dates: deny-safe, and noted

Date operators follow the same policy, for the same reason: metadata reaches the
evaluator from host-implemented `ObjectProvider`s and from the principal
attribute bag, neither of which any loader validates, so one malformed date must
not break every `Check` that touches the field.

**Any operand that will not parse makes the comparison `false`.** Never an
`APERTURE_RULE_EVAL` error. Both operands are parsed to instants on every
evaluation — comparing the canonical strings is not a permissible shortcut,
because the date-only form is a strict prefix of the timestamp form.

| operand | result | note |
|---|---|---|
| absent (`nil`) | **`false`** | `shape_mismatch`, `expected date, got absent` |
| a number, bool, array, or object | **`false`** | `shape_mismatch`, `expected date, got number` |
| a string that is not one of the two canonical forms | **`false`** | `date_invalid` |
| `between` whose lower bound is after its upper bound | **`false`** | `date_bounds_inverted` |

**Note the asymmetry with the collection operators, and that it is deliberate.**
A collection operator gives an absent field a meaning — it reads as an *empty
collection*, and the operator's own polarity decides from there, which is why
`hasNone` grants on missing data. A date operator has no such reading: there is
no empty date, and all eight date operators are **positive**, so none of them can
match on an absence. Absent is therefore uniformly `false`, exactly as a
wrong-shaped operand is, and no date operator ever records `absent_field`. (A
negative date operator, were one ever added, would need it.)

An unparseable string is kept **separate** from a shape mismatch because the fix
differs: the shape was right and the content was not, so the answer is to
canonicalise the data — or to declare the field as a date in the loader (a CSV
`:date` / `:datetime` column, or the seed document's `field_types:`) so it is
validated at load instead of denying silently at decision time.

### Evaluation notes

A silent `false` is how an access-control bug hides, so every mismatch is
**recorded** and surfaced in `Explain` on every decision surface (library, CLI,
Twirp `trace_json`, MCP):

```
object.tags: expected collection, got string
object.hired_at: not a canonical date; before expects 2006-01-02 or 2006-01-02T15:04:05Z
object.hired_at: between bounds are inverted; the lower bound is after the upper bound, so nothing can match
```

Notes are `rules.Note` values — `Kind`, `Rule`, `Op`, `Path`, `Expected`,
`Actual` — carrying **shape and path only, never a metadata value**. That last
point is not a nicety for dates: a date is often personal data (a birth date, a
termination date), and the same trace crosses the Twirp and MCP surfaces. Four
kinds are recorded today:

- `shape_mismatch` — a collection operator met a non-collection, **or** a date
  operator met a value that is not a string (including an absent field).
- `absent_field` — an operator **matched because the field is missing** (the
  `nin` / `hasNone` / `subsetOf` / `isEmpty` grant above), which is otherwise
  invisible in the verdict. No date operator records this.
- `date_invalid` — a date operator met a **string** that is not one of the two
  canonical forms. `Expected` names the forms; the offending value never appears.
- `date_bounds_inverted` — a `between` was written with its lower bound after its
  upper bound, so it matches nothing. `Path` names the compared field.

The channel is opt-in and costs the decision path nothing: `Check` and
`Enumerate` install no collector, so nothing is recorded and nothing is
allocated; `Explain` installs one per grant. Direct library use:

```go
ctx, notes := rules.WithNoteCollector(ctx) // engine.Explain does this for you
allowed, err := compiled.Eval(ctx, in)     // notes land in the collector
_ = notes.Notes()

ok, ns, err := compiled.EvalWithNotes(ctx, in) // or take them back directly
```

Notes are **diagnostic only** — they never influence a verdict.

## Validation before evaluation

`Compiler.Compile` (and `Engine.Compile`) validate and type-check before any
evaluation, surfacing coded errors:

- `APERTURE_RULE_INVALID` — malformed AST (bad arity, missing operand, a right
  operand on a unary op, the wrong operand shape for a collection op, non-scalar
  literal, non-identifier variable path, a `call` naming a denied predicate
  builtin).
- `APERTURE_RULE_UNKNOWN_VARIABLE` — a variable root outside `object` /
  `principal` / `account` / `action`.
- `APERTURE_RULE_TYPE_ERROR` — type-incompatible comparison, non-boolean result,
  or a call to an unregistered function (caught by the expression type-checker).
- `APERTURE_RULE_EVAL` — a runtime failure (e.g. an ordered comparison against a
  metadata field the object lacks) or a non-boolean result. A wrong-shaped
  collection operand is **not** one of these, and neither is an absent,
  wrong-shaped, or unparseable **date** operand: both deny with a note.
- `APERTURE_RULE_NOT_FOUND` — a scope rule reference the `RuleSource` cannot
  resolve.

Evaluation is pure and deterministic over a fixed metadata snapshot: all of
`expr-lang`'s builtins are disabled, so no wall-clock or random function is
reachable. The curated pure set is `lower`, `upper`, `contains`, `startsWith`,
`endsWith`, `len`, plus the collection-operator backings `hasAll`, `hasAny`,
`hasNone`, `subsetOf`, `isEmpty`, `isNotEmpty` — all deterministic and
side-effect-free. A host adds its own with `rules.Function` / `WithFunction`.
Note that `contains`, `startsWith` and `endsWith` are reserved as **infix
operators** by expr's grammar, so they are registered but not reachable through a
`call` node; the other nine are.

## Compile-once, cache

A rule is rendered to its canonical expr-lang expression, hashed (sha256), and the
compiled program is cached by that hash — so distinct rule references whose ASTs
render identically share one compiled program, and per-`Check` cost is bounded
(the NFR lever E4-S4 tunes). The cache is concurrency-safe with an optional TTL
read from an injected `Clock` (`WithCacheTTL` / `WithClock`); `CacheStats`
exposes hit/miss/eviction counters.

A cache **hit** takes only the read lock: the counters are `sync/atomic`, so
concurrent evaluations no longer serialise on a counter bump they never read.
That matters most for rule-backed `Enumerate`, which evaluates the rule once per
candidate to gather a grant's members and again per candidate in the decision
walk — see [benchmarks](../docs/benchmarks.md), "Rule-backed `Enumerate`". The
counters' meaning is unchanged, and `CacheStats` samples them atomically rather
than under a lock, so the four fields are not one instant's snapshot; they are
exact for a sequential caller.

What the cache removes is the **expr-lang compile**, not the AST walk: `Selected`
calls `Engine.Compile` per evaluation, and `Compile` re-validates, re-renders, and
re-hashes the AST before it can probe the cache by hash. That walk is still on
every decision, but it is now **flat in literal count** — ~0.65 µs / 5 allocs /
288 B for a 3-node rule and ~0.65 µs / 5 allocs / 320 B for a 6-node one. What
remains is the render buffer, the rendered string, and the sha256 + hex of it.

Getting there was issue #9. Both `Validate` and `render` used to decode every
literal through a `json.NewDecoder`, twice per decision, at ~14 allocs and
~4.9 KB per literal — so a rule's per-`Check` cost scaled with how many literals
it happened to contain (a 6-node rule cost 47 allocs against a 3-node rule's 19,
for expressions that evaluate identically). `rules.classifyScalar` now identifies
the common literal forms straight from the raw JSON bytes with no allocation, and
defers to the decoder only for escaped/non-ASCII strings, composites and
malformed input — so it can never widen what `Validate` accepts. The
`bench/collection_test.go` budget that tracked this is now 2 allocs, measured at
0.0.

## What a rule costs a `Check`

Measured in `bench/` (Apple M1 Max); see [benchmarks](../docs/benchmarks.md) for
the fixture, the full tables, and the two findings.

| Shape | Per-evaluation cost | Notes |
|---|---|---|
| scalar comparison | ~149 ns, 3 allocs | the control |
| list-literal collection (`in [...]`) | ~167 ns, 3 allocs | the render E5-S1 leaves native — the operator itself is free |
| nested read (`object?.owner?.dept`) | ~210 ns, 4 allocs | optional chaining is render-time only |
| collection over a **variable** (guarded `$op`) | ~12 ns and ~16.9 B **per element** | linear in the array; ~530 ns over 12 elements, ~78 µs over 7 281 |

The per-element term is the one to design around: a rule reading a large array
puts an O(n) traversal on the decision hot path, and the array's maximum length is
set by `provider.ValueLimits.MaxBytes` (see the `metadata-values` skill).

## Wiring

```go
eng := rules.NewEngine(ruleSource, providerRegistry) // *provider.Registry is the MetadataFetcher
authz := engine.New(store, engine.WithScopeResolution(nil, engine.ScopeDeps{
    Lister: providerRegistry,
    Rules:  eng,
}))
```

`RuleSource` resolves a scope strategy's opaque `rule=` reference to a `*Rule`
(its AST); `rules.MapSource` is the in-memory default. The metadata fetcher is
any `Fetch(ctx, id) (map[string]any, error)` — `*provider.Registry` fits directly.
