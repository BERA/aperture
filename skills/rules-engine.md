---
name: rules-engine
description: The rules engine evaluates a JSON rule AST as a Pulse expression over object metadata and principal/action context, compiling once and caching, to back the inclusive/exclusive scope resolvers.
applies_to: [cli, http, mcp]
---

# Rules engine

The `rules` package decides object-membership selection (and, by extension,
allow/deny) from a domain object's metadata plus the principal/action context. It
is the rule-backed variant of the inclusive/exclusive scope resolvers (E2-S1): an
`*rules.Engine` satisfies `scope.RuleEvaluator`, so wiring it as
`engine.ScopeDeps{Rules: eng}` turns on rule-driven scope membership.

Expressions are evaluated by Pulse's expression evaluator (`expr-lang/expr`, the
same pure-Go engine Pulse uses for its `FILTER_EXPRESSION` predicate). Aperture
does not hand-roll a parser and stays `CGO_ENABLED=0` — it never pulls Pulse's
geo/h3 packages.

## The rule AST (the editor + state-file contract)

A rule is a tree of typed `rules.Node` values. The set is small, explicit, and
closed, so the Rete.js editor (E7-S2) maps its palette one-to-one and the state
file (E5-S2) persists the same shape. There is no second rule format.

| `type` | Fields | Meaning |
|---|---|---|
| `and` / `or` | `children` (>= 2) | logical conjunction / disjunction |
| `not` | `children` (exactly 1) | logical negation |
| `compare` | `op`, `left`, `right` | comparison; `right` is omitted for the unary ops |
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

Operand rules are enforced by `Validate` and return `APERTURE_RULE_INVALID`:

- The three unary ops require `right == nil`; supplying one is an error, not
  something ignored, so a rule has exactly one spelling.
- Every other op requires both operands.
- `in` `nin` `hasAll` `hasAny` `hasNone` `subsetOf` need a `list` or a `var` on
  the right — a set.
- `has` `hasKey` need a single element on the right; a `list` there is an error
  pointing at `hasAll`/`hasAny`/`hasNone`.

Build a unary node with `rules.Unary(op, left)`; everything else with
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

### Evaluation notes

A silent `false` is how an access-control bug hides, so every mismatch is
**recorded** and surfaced in `Explain` on every decision surface (library, CLI,
Twirp `trace_json`, MCP):

```
object.tags: expected collection, got string
```

Notes are `rules.Note` values — `Kind`, `Rule`, `Op`, `Path`, `Expected`,
`Actual` — carrying **shape and path only, never a metadata value**. Two kinds
are recorded today:

- `shape_mismatch` — a collection operator met a non-collection.
- `absent_field` — an operator **matched because the field is missing** (the
  `nin` / `hasNone` / `subsetOf` / `isEmpty` grant above), which is otherwise
  invisible in the verdict.

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
  collection operand is **not** one of these: it denies with a note.
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

A rule is rendered to its canonical Pulse expression, hashed (sha256), and the
compiled program is cached by that hash — so distinct rule references whose ASTs
render identically share one compiled program, and per-`Check` cost is bounded
(the NFR lever E4-S4 tunes). The cache is concurrency-safe with an optional TTL
read from an injected `Clock` (`WithCacheTTL` / `WithClock`); `CacheStats`
exposes hit/miss/eviction counters.

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
