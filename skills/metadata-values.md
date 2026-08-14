---
name: metadata-values
description: The shared metadata value model — what shapes a provider.Metadata field may hold (scalar, scalar array, one object level), the depth and size caps, and the load-time validation entry point every loader calls.
applies_to: [cli, http, mcp]
---

# Metadata value model

`provider.Metadata` is a **type alias** for `map[string]any`, so the type
constrains nothing. The **shape** of a field value is constrained anyway, in one
place, by the model in `provider/metadata.go`.

The reason is the hot path. Metadata reaches the expression evaluator with **no
conversion** (`rules/compiler.go` puts it straight into the expr environment), so
a value of the wrong shape is not a `false` decision — it is a *runtime* error
during `Check` (`operator "in" not defined on string`). Validating shapes at
**load** time is what keeps that error out of the decision path.

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
id,tier,seats:int,tags:list,ranks:list<int>,aliases:list(;)
brand:1,gold,40,premium|launch,3|5,acme;acme-co
brand:2,silver,,,,
```

| Suffix | Value |
|---|---|
| none / `:string`, `:int`, `:float`, `:bool` | a **scalar** (`int` → `int64`, `float` → `float64`) |
| `:list` | an **array** of strings, split on `\|` |
| `:list<int>` / `:list<float>` / `:list<bool>` | an array whose elements go through the **same** scalar coercion |
| `:list(;)` | the delimiter override, for that column only |

Three rules the value model does not impose, but the CSV encoding must:

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
  evaluating against `nil`. Empty *scalar* cells still omit the field.

Grammar and coercion failures are `APERTURE_CONFIG_INVALID`; the value-model
check that runs on every parsed cell is `APERTURE_METADATA_INVALID`.

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
  a reload builds a new value rather than editing the old one in place;
- **no holder** — engine, rules, scope, CLI, server, host code — writes to a
  `Metadata` it was given, **at any depth**;
- a consumer that needs to modify metadata copies it deeply first.

## Update-Demand

Changing the value model means changing all of these in the same PR:

| Change | Also update |
|---|---|
| The legal shapes, the caps, or their defaults | this doc + `docs/src/concepts/providers.md` |
| `APERTURE_METADATA_INVALID` | `errors/codes.go` (`AllCodes` + `Registry`), then `make docs-gen` |
| A loader's coercion (`csvprovider`, seed) | the loader must call `provider.Validate*`, not re-implement the rules |
| A loader's **encoding** (the CSV header grammar, a seed key) | "How each loader spells the model" above + the loader's package doc + `docs/src/concepts/providers.md` |

`provider/` imports only `identity`, `errors`, and the standard library — the
value model must not change that.
