# Seed & portability

The `seed` package loads a **declarative authorization model** — a single JSON or
YAML document — into a `model.Storage`, and exports one back out. It is both the
human-authored on-ramp behind the `aperture check` / `aperture serve` demo and the
full round-trip state file the model portability endpoints use.

## One document, both directions

A `Document` is a flat list of each entity kind. The field tags cover **both** YAML
and JSON, so either format decodes into the same shape:

```yaml
accounts:      [{ id: acme, name: Acme }]
memberships:   [{ principal: alice, account: acme }]
object_types:  [{ name: document, actions: [read, write] }]
permissions:   [{ id: doc.read, object_type: document, action: read, scope_strategy: implicit }]
principals:    [{ id: alice, kind: user, roles: [reader] }]
roles:         [{ id: reader, name: Reader, permissions: [doc.read] }]
groups:        [ ... ]
grants:        [{ id: g1, account: acme, subject: { kind: principal, id: alice }, permission: doc.read, object: "account:acme/document:*", effect: allow }]
templates:     [ ... ]
rules:         [ ... ]
providers:     [ ... ]   # runtime wiring, not model state (see below)
objects:       [ ... ]   # inline object metadata — also wiring, not model state
```

Every field mirrors its `model` counterpart in declarative form. The `Document`
started as a minimal seed shape and was generalized to the complete model, so **an
export file is a strict superset of a seed file**: a seed that omits
`templates`/`rules`/`providers`/`objects` loads unchanged, and a full export
reloads through the very same path. The field set is additive-only, so old seeds
keep loading.

Rule ASTs are carried as **raw JSON** — exactly the `rules` package's canonical
`Node` serialization — so the file never invents a second rule format; it is the
same shape the node editor reads and writes.

## Loading (import)

```go
// From bytes, explicit format:
err := seed.Load(ctx, store, data, seed.FormatYAML)

// From a file — format inferred from the extension (.json ⇒ JSON, else YAML):
err := seed.LoadFile(ctx, store, "model.yaml")
```

`Parse` decodes the document; `Apply` upserts it into the store in **dependency
order**: accounts, object types, permissions, principals, memberships, roles,
groups, grants, templates, then rules. Each write goes through the storage layer's
own validation — a malformed entity surfaces the *same* coded error a programmatic
`Put` would (e.g. `APERTURE_ACTION_UNDECLARED` for a permission naming an
undeclared action). Rule ASTs are additionally validated against the rules
engine's contract before storing, so an import rejects a structurally broken rule
(`APERTURE_RULE_INVALID`) rather than persisting one the engine could never
compile.

> `Apply` is **not transactional** — a failure may leave a partial model. This is
> acceptable for the seed-and-demo use case. (The mutation API's bulk endpoints use
> `Storage.Atomic` when all-or-nothing is required.)

The YAML path routes through JSON internally (`yaml → generic → json → Document`)
so the raw-JSON rule AST decodes by exactly the same rules the JSON path uses.

### The committed example

`seed.Example` is the embedded `org → project → document` fixture stamped to
account `acme` (`seed.ExampleAccount`). It is what `aperture check` loads when no
`--seed` file is supplied, and it backs the end-to-end test.

## Exporting

`Export(ctx, store)` reads the **complete** model back out into a `Document`, and
`Marshal(doc, format)` renders it to on-disk bytes:

```go
doc, _ := seed.Export(ctx, store)
out, _ := seed.Marshal(doc, seed.FormatJSON)
```

Export captures every source-of-truth entity: accounts, memberships, object types,
permissions, principals, roles, groups, grants, templates, and rule ASTs. Two
properties make a round-trip trustworthy:

- **Byte-stable.** Every slice is emitted in a stable order (sorted by id, name,
  or natural key) and each rule AST is re-serialized to the rules package's
  canonical form, so a re-export of an unchanged model is byte-identical and
  human-diffable.
- **Wildcard edges are preserved.** Memberships and grants stamped to the wildcard
  account `*` (the cross-account super-admin reach) are not among the real
  accounts, so `Export` queries `*` explicitly — omitting it would silently drop a
  super-admin's reach on export/import.

## Inline object metadata

For a small or fixed object set, `objects:` declares metadata **in the seed file
itself**, with no separate CSV beside it. YAML nests natively, so this is the most
direct way to author the arrays and nested objects the
[value model](providers.md#the-metadata-value-model) admits:

```yaml
objects:
  - id: account:acme/brand:1
    metadata:
      tier: gold
      seats: 5
      tags: [premium, launch]
      owner:
        dept: eng
        lead: alice
  - id: account:acme/brand:2      # metadata: may be omitted entirely
  - id: account:acme/app:be
    metadata: { tier: gold }
```

The object-type is **derived from the identity's terminal segment** —
`account:acme/brand:1` is a `brand` — never declared separately, so one fact in
one place cannot disagree with itself. Entries of different types may be
interleaved freely; `BuildRegistry` groups them and registers one in-memory
[`provider.Static`](providers.md#in-memory-objects-providerstatic) per type, with
a TTL of 0 (the data cannot go stale, because nothing can change it). Within a
type, **declaration order is preserved** — it is the order `List` and `Query`
return.

The provider it builds is a full one: `Fetch` (with `APERTURE_NOT_FOUND` for an
undeclared id), `List`, and `Query` honouring `Pattern`, `Limit`, and `Fields` on
the [same contract](providers.md#the-filterfields-contract) every other provider
implements — a collection field matches by membership, everything else by typed
equality.

Values are validated against the shared value model **when the document is
built**, before any `Check` can see them, and numbers are normalised exactly as
every other loader normalises them: an exact integer that fits `int64` becomes an
`int64`, anything else a `float64`. That is what keeps `object.seats == 5` from
answering differently depending on whether the object came from a seed file, a
JSON seed file, or a CSV `:int` column.

A missing or duplicate `id`, `metadata` that is not a mapping, or a value the
value model rejects is `APERTURE_CONFIG_INVALID` naming the object id and the
field (with the inner `APERTURE_METADATA_INVALID` kept in the chain); a malformed
id is `APERTURE_IDENTITY_INVALID`. Ids are deduplicated across the **whole**
section, not per type — the same id declared twice is the same object declared
twice, however it is spelled.

### When both sections claim a type

`providers:` and `objects:` can each claim the same object type. **Precedence is
type-level and total:**

> If a `providers:` entry exists for an object type, the file-backed provider
> wins and **every** inline `objects:` entry for that type is discarded.

There is **no object-level merge, no field-level merge, and no fallback**. An
inline id the file happens to lack is simply not resolvable — `Fetch` returns
`APERTURE_NOT_FOUND` and enumeration never lists it, exactly as if the entry had
never been written. A field only the inline entry declared does not appear on an
object the file *does* carry.

That is deliberate. Field-level merging is the most useful-sounding behaviour and
the most impossible to debug: a rule reading a field the CSV silently did not
override is a support ticket nobody can reproduce. Predictability wins.

**This is the default. A collision builds, it does not fail.** Pointing a type at
a CSV while its inline entries are still in the file is an ordinary migration
step, not an authoring fault — a seed that booted yesterday must not refuse to
boot today because someone added a `providers:` row.

The discard is not silent, though. `BuildRegistry` needs no logger to say so,
because the document can be asked directly:

```go
reg, err := doc.BuildRegistry(dir)
if err != nil {
    return err
}
if types := doc.ProviderCollisions(); len(types) > 0 {
    slog.Warn("seed: inline objects discarded, providers: entry wins",
        "object_types", types)
}
```

`ProviderCollisions` returns exactly the object types whose inline entries the
build discarded — sorted, deduplicated, and object **types** only, never object
ids (an id can embed an account, and this value is destined for a log line). It
reads the document alone — no file IO, no registry — so it answers the same
before and after a build, and a host surfaces it however it already surfaces
things. Nothing in `seed` picks a logger for you.

A host that would rather read the overlap as an authoring mistake — a checked-in
seed nobody is mid-migration on — opts into a refusal:

```go
reg, err := doc.BuildRegistry(dir, seed.StrictProviderCollision())
```

That returns `APERTURE_CONFIG_INVALID` naming every colliding object type
(sorted, in both the message and the error context — never the object ids). It is
Go wiring rather than a seed-file key on purpose: the file stays a plain
declaration of what exists, and the choice to make an ambiguous one fatal sits in
code, where a reviewer sees it.

**Validation is independent of precedence.** Every inline entry is checked —
`id`, identity, duplicates, the value model — *before* any type is discarded, so
a malformed declaration fails the load whether or not its type ultimately loses
to a `providers:` entry. Otherwise a document would silently stop being validated
the day someone added a CSV for one of its types.

Two `providers:` entries for one type remain a duplicate registration,
`APERTURE_PROVIDER_INVALID` — that is a straight contradiction with no winner to
pick, not a precedence question.

## What is *not* in the file

Two things are deliberately excluded from the model state file:

- **Live host domain-object metadata** — that is the [provider](providers.md)
  cache: derived, disposable, never source of truth. Because `Export` reads storage
  back, and a provider produces no model rows, it is never reproduced.
- **Runtime *wiring*** — the `providers:` and `objects:` sections are runtime
  wiring, not model state. `Apply` never writes either to storage; instead
  `Document.BuildRegistry(baseDir)` turns both into a live `*provider.Registry`.
  **The seed file is the source of truth for both**, exactly as auth config is —
  and exactly as for `providers:`, an export never reproduces `objects:`. A
  declared provider names an `object_type`, a `kind` (currently only `csv`), a
  `path` (resolved relative to the seed file), and optional cache `ttl`/`max_size`.
  A malformed entry is `APERTURE_CONFIG_INVALID` / `APERTURE_PROVIDER_INVALID`.
  When both sections claim one type, `providers:` wins the type outright — see
  [When both sections claim a type](#when-both-sections-claim-a-type).

## Related

- [The RBAC model](model.md) — the entities the document mirrors.
- [Rules engine](rules.md) — the canonical AST a rule's `ast` field carries.
- [Providers](providers.md) — the registry `providers:` wiring builds, and the
  cache that is never exported.
- [Storage](storage.md) — the `Storage` backend `Apply` writes through and `Export`
  reads back.
- [Portability CLI](../cli/portability.md) — the command surface over import/export.
