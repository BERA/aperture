---
name: metadata-values
description: The shared metadata value model — what shapes a provider.Metadata field may hold (scalar, scalar array, one object level), the depth and size caps, the load-time validation entry point every loader calls, how each loader spells the model (the csvprovider header grammar, the seed document's inline objects section and provider.Static), and how a Filter.Fields predicate compares against each shape (membership for a collection, typed equality for the rest).
applies_to: [cli, http, mcp]
---

# Metadata value model

`provider.Metadata` is a **type alias** for `map[string]any`, so the type
constrains nothing. The **shape** of a field value is constrained anyway, in one
place, by the model in `provider/metadata.go`.

The reason is the hot path. Metadata reaches the expression evaluator with **no
conversion** (`rules/compiler.go` puts it straight into the expr environment), so
a wrong-shaped value is a decision made on data nobody meant to write. The rules
engine is deny-safe about it — a collection operator over a non-collection is
`false`, plus an `Explain` note, never an `APERTURE_RULE_EVAL` error (see
`rules-engine`) — but a silent deny is still a bug in the model. Validating
shapes at **load** time is what turns it into a loud one, at the point the data
enters, with the offending field named.

Load-time validation cannot reach everything: a host-implemented `ObjectProvider`
and the `principal` attribute bag bypass the loaders. That is why the runtime
policy exists as well; the two are complements, not alternatives.

## The model

A metadata field value is one of:

- **scalar** — `nil`, `bool`, `string`, any Go integer or float type, or a
  `json.Number`;
- **array** — a `[]any` whose elements are **all scalars**;
- **object** — a `map[string]any` whose values are scalars, scalar arrays, or one
  further object level.

Rejected everywhere:

- **arrays of objects**, at any position (`tags: [{name: "a"}]`,
  `owner: {members: [{id: 1}]}`) — `in` / `not in` over a list of maps has no
  useful meaning, so permitting it only buys evaluation-time failures;
- **nested arrays** (`[["a"]]`), for the same reason;
- **typed containers** — `[]string`, `map[string]string`, structs. The model is
  spelled in the two types the expression environment and JSON share so a loader
  normalises once;
- **`time.Time`** — a rule literal is a JSON scalar and could never be compared
  against one. Loaders format timestamps as RFC 3339 strings.

## Caps

Both caps live in `provider.ValueLimits`, never as constants at a call site. The
zero `ValueLimits` means "the defaults", and any field left zero (or negative)
keeps its default, so a caller overrides only what it cares about.

| Cap | Field | Default | Measured by |
|---|---|---|---|
| Depth | `MaxDepth` | `DefaultMaxValueDepth` = 2 | `provider.ValueDepth` |
| Size (per field value) | `MaxBytes` | `DefaultMaxValueBytes` = 64 KiB | `provider.ValueBytes` |

**Depth** is counted in containers below the field root: a scalar is 0, every
array or object entered adds one, and an empty container is 1.

| Value | Depth | Legal? |
|---|---|---|
| `tags: ["a", "b"]` | 1 | yes |
| `owner: {dept: "eng"}` | 1 | yes |
| `owner: {lead: {name: "x"}}` | 2 | yes |
| `owner: {tags: ["a"]}` | 2 | yes |
| `owner: {a: {b: {c: "x"}}}` | 3 | no — past the depth cap |
| `owner: {members: [{id: 1}]}` | 3 | no — array of objects |
| `tags: [{name: "a"}]` | 2 | no — array of objects |

**Size** is measured structurally rather than by an encoding round-trip, so
nothing is allocated on a load path: a string costs its length in **bytes**, a
number 8, a bool 1, `nil` 0, an array the sum of its elements, and an object the
sum of `len(key) + value` per entry. Container framing costs nothing.
`provider.MetadataBytes` sums a whole object, but the enforced cap is per
**field**.

## The entry point

Every loader — `csvprovider`, the seed document, a future database-backed
provider — calls the same function. That is what lets a new loader inherit the
semantics instead of renegotiating them.

```go
// defaults
err := provider.ValidateMetadata(md)          // whole object
err = provider.ValidateField("tags", value)   // one field

// tuned
limits := provider.ValueLimits{MaxDepth: 3}   // MaxBytes stays default
err = limits.ValidateMetadata(md)
err = limits.ValidateField("tags", value)
```

## How each loader spells the model

The model is one thing; each loader's *syntax* for reaching it is its own. A
loader normalises into the model — it never widens it.

### `csvprovider`

A header column carries `name:type[<elem>][(delim)]`:

```text
id,tier,seats:int,tags:list,ranks:list<int>,aliases:list(;),owner:json
brand:1,gold,40,premium|launch,3|5,acme;acme-co,"{""dept"":""eng"",""tags"":[""a""]}"
brand:2,silver,,,,,
```

| Suffix | Value |
|---|---|
| none / `:string`, `:int`, `:float`, `:bool` | a **scalar** (`int` → `int64`, `float` → `float64`) |
| `:list` | an **array** of strings, split on `\|` |
| `:list<int>` / `:list<float>` / `:list<bool>` | an array whose elements go through the **same** scalar coercion |
| `:list(;)` | the delimiter override, for that column only |
| `:json` | an **object**, decoded from the cell — takes no `<elem>` and no `(delim)` |

Four rules the value model does not impose, but the CSV encoding must:

- **Element typing is mandatory for numeric membership.** expr does no
  numeric/string coercion, so `5 in object.seats` is `false` against `["3","5"]`.
  A silently wrong `false` is the worst failure mode here; `:list<int>` is what
  prevents it.
- **No escape syntax.** A value that must contain the delimiter needs a
  per-column delimiter it does not contain. A stray, doubled, leading, or
  trailing delimiter yields an empty element and is a **hard error at parse**
  (`APERTURE_CONFIG_INVALID`, naming the column, the line, and the cell) — never
  a silently mis-split row.
- **An empty list cell is `[]`, not an absent field** — the one departure from
  the scalar rule, so a membership rule gets a definite `false` instead of
  evaluating against `nil`. Empty *scalar* **and `:json`** cells still omit the
  field: an absent object is meaningfully different from an empty one.
- **A `:json` cell must decode to a JSON _object_.** An array, a scalar, or
  `null` is rejected (`APERTURE_CONFIG_INVALID`), so `:list` stays the only array
  path. The cell is RFC 4180 quoted — the whole cell in double quotes, every
  inner double quote doubled — and its numbers decode through `UseNumber` and
  then normalise **exactly as the scalar columns do**: an exact integer that fits
  `int64` becomes an `int64`, everything else a `float64`. That is what keeps
  `object.owner.seats == object.seats` from being a silent `false`.

Grammar, coercion, and `:json` decode failures are `APERTURE_CONFIG_INVALID`; the
value-model check that runs on every parsed cell is `APERTURE_METADATA_INVALID`.
A `:json` rejection carries the column, the line, and the JSON kind or the
decoder's message — never the cell.

### The seed document's `objects:` section

YAML nests natively, so the seed file spells the model **directly** — no encoding
to learn, no delimiter to pick:

```yaml
objects:
  - id: account:acme/brand:1
    metadata:
      tier: gold
      seats: 5
      tags: [premium, launch]
      owner: { dept: eng, lead: alice }
  - id: account:acme/brand:2      # metadata: may be omitted
```

The object-type is **derived from the identity's terminal segment**, never
declared. `Document.BuildRegistry` groups the entries by type and registers one
`provider.Static` per type with a TTL of 0; declaration order is what `List` and
`Query` return. This is runtime **wiring**, not model state: `Apply` writes no row
and an export never reproduces the section.

Three properties, all of them the same ones the CSV loader owes:

- **Numbers normalise identically.** The section is carried as raw JSON and
  decoded with `UseNumber`, then an exact integer that fits `int64` becomes an
  `int64` and everything else a `float64` — the same rule as `:int`/`:float` and a
  `:json` cell. Without it, YAML would hand over an `int` and JSON a `float64` and
  the *same document* would answer `object.seats == 5` differently depending on the
  format it was written in.
- **`metadata:` must be a mapping.** A scalar or an array there is rejected, so
  the section cannot smuggle in a shape the field-level model would refuse.
- **Every field goes through `provider.ValidateField`**, in sorted key order, at
  build time — never re-implemented. A violation is **`APERTURE_CONFIG_INVALID`
  naming the object id and the field**, wrapping the value model's own
  `APERTURE_METADATA_INVALID` (with its `path`/`type`/`depth` context) in the
  chain. A missing or duplicate `id`, or metadata that is not a mapping, is the
  same `APERTURE_CONFIG_INVALID`; a malformed id passes through as
  `APERTURE_IDENTITY_INVALID`.

`provider.Static` is the in-memory `ObjectProvider` behind the section, and is
reusable on its own (tests, embedded data). It is immutable after `NewStatic`
returns, validates the value model again itself rather than trusting its caller,
and **deep-copies its input** so a caller that keeps and mutates those maps cannot
reach into metadata the Registry has already cached. Reads hand out its own maps
**by reference**, exactly as `csvprovider` does.

## Querying the model: `Filter.Fields`

The shape a field holds decides how a `provider.Filter.Fields` predicate compares
against it. A provider *evaluates* `Fields`, but it does not *define* it — `Query`
is how scope enumeration bounds itself, so the rule is stated once on
`provider.Filter` and implemented once in `provider.MatchFields`. A provider
either calls that helper or reproduces it exactly (pushing the predicate into SQL,
say).

| Field shape | Rule | Example |
|---|---|---|
| **array** | **membership** | `Fields{"tags": "premium"}` selects every object whose `tags` **contains** `"premium"` |
| **scalar** | equality | `Fields{"tier": "gold"}` |
| **object** | equality, deep — **not** key membership | `Fields{"owner": map[string]any{"dept": "eng"}}` matches; `Fields{"owner": "dept"}` is a plain `false` |
| *absent* | never matches, `nil` want included | |

Two properties are load-bearing:

- **Comparison is typed, never a string rendering.** Numbers compare across Go
  numeric types by value (`int(5)` == `int64(5)` == `float64(5)`), but `"5"` is
  never `5` and a string equals only a string. That is the same rule the
  expression evaluator applies, so `Enumerate` cannot select an object a `Check`
  over the same value then denies. This is why element typing at load
  (`:list<int>`) is mandatory rather than decorative — it is what a `Fields`
  predicate compares against.
- **A want that is itself a container compares by equality at both ends**, since
  no element of a legal array (scalars only) could ever equal an array or object.

`provider.ValuesEqual` is the exported leaf comparison. It **reimplements**
expr-lang's equality rather than importing it, because `provider/` is a strict
leaf; `csvprovider/membership_equivalence_test.go` runs both over the same
metadata so the two cannot drift. The only documented divergences —
`time.Time`/`time.Duration`, and a `uint64` above `math.MaxInt64` — are outside
the value model.

## Errors

A violation is **`APERTURE_METADATA_INVALID`**. Its context carries:

| Key | Meaning |
|---|---|
| `field` | the metadata field name |
| `path` | the path within the value, e.g. `owner.members[0]` |
| `type` | the offending Go type (shape violations) |
| `depth` / `max_depth` | depth violations |
| `bytes` / `max_bytes` | size violations |

The error **never carries the value** — only the field, the path, and the Go
type. That is the cross-account leak rule (`CLAUDE.md`): a validation failure can
be logged and surfaced without one account's data reaching another's diagnostics.

Fields and object keys are walked in **sorted order**, so an object with several
offending values always reports the same one.

## The read-only contract is transitive

The cache stores a provider's map **by reference** and never copies it on read
(allocation-aware on the `Check` hot path). Now that values nest, the nested maps
and slices are shared too:

- a provider returns a **fresh** map per object, with fresh nested containers, and
  a reload builds a new value rather than editing the old one in place — in
  `csvprovider` every list cell is split into its own slice and every `:json`
  cell decoded into its own map, so no two rows share one, and `provider.Static`
  deep-copies its whole object set once at construction so two entries built from
  one source value never share a container either;
- **no holder** — engine, rules, scope, CLI, server, host code — writes to a
  `Metadata` it was given, **at any depth**;
- a consumer that needs to modify metadata copies it deeply first.

## Update-Demand

Changing the value model means changing all of these in the same PR:

| Change | Also update |
|---|---|
| The legal shapes, the caps, or their defaults | this doc + `docs/src/concepts/providers.md` |
| The `Filter.Fields` rule (`MatchFields`, `MatchField`, `ValuesEqual`) | the `Filter` doc comment in `provider/provider.go` — it is the contract every provider implements — plus this doc and `docs/src/concepts/providers.md` |
| `APERTURE_METADATA_INVALID` | `errors/codes.go` (`AllCodes` + `Registry`), then `make docs-gen` |
| A loader's coercion (`csvprovider`, seed) | the loader must call `provider.Validate*`, not re-implement the rules |
| A loader's **encoding** (the CSV header grammar, a seed key) | "How each loader spells the model" above + the loader's package doc + `docs/src/concepts/providers.md` |
| The seed `objects:` shape, or `provider.Static` | "The seed document's `objects:` section" above + `docs/src/concepts/seed.md` + `docs/src/concepts/providers.md` — and it must stay **wiring**: no storage table, no `Apply` row, no export |

`provider/` imports only `identity`, `errors`, and the standard library — the
value model must not change that.
