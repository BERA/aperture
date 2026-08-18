# Decisions

**Audience:** operators and integrators asking and auditing access-control
questions from a shell.

The decision commands are the read-only core of the CLI. They never change the
model — they ask a question of it. All three of `check`, `enumerate`, and
`explain` take the **subject principal as a positional argument** (the principal
the question is *about*), not a `--principal` flag; see
[Global options](global-options.md#the-acting-principal-principal) for why the
write commands differ. `identifiers` inspects an object type's source.

All four build the same decision stack `aperture serve` does, so a question asked
in a shell resolves exactly as it does over HTTP — see
[Rule-backed permissions](#rule-backed-permissions-decide-the-same-here-as-on-the-server).

## `check` — decide one question

```text
aperture check [options] <principal> <action> <object>
```

`check` prints a one-word verdict (`allow` / `deny`) and a reason, and **carries
the verdict in its exit code** — `0` for allow, non-zero for deny — so it
composes in a pipeline (`aperture check … && deploy`).

```bash
bin/aperture check alice read account:acme/project:atlas/document:42
```

```text
allow
reason: allowed by grant g-eng-read-atlas (allow account:acme/project:atlas/**) at specificity 39300; 1 matching grant(s) considered
```

A question with no matching grant is always a deny — Aperture fails closed:

```bash
bin/aperture check bob write account:acme/project:atlas/document:42
```

```text
deny
reason: default deny: no grant matched action "write" on "account:acme/project:atlas/document:42" for principal "bob" in account "acme"
```

Full flags: [`check`](../reference/cli.md#aperture-check).

### Rule-backed permissions decide the same here as on the server

`check`, `enumerate`, `identifiers` and `explain` build the **same** decision
stack `aperture serve` does: the object metadata declared in the seed's
`providers:` and `objects:` sections, the rules engine over the stored rules, and
scope resolution. A permission whose scope strategy is rule-backed
(`inclusive;rule=…` / `exclusive;rule=…`) is therefore evaluated identically from
the shell and over HTTP.

This was not always true. Before the shared stack landed, the one-shot commands
wired no rules engine, so a rule-backed permission had no evaluator, the scope
resolver reported `APERTURE_SCOPE_RULE_UNCONFIGURED` and the fail-closed policy
turned that into a deny. If you scripted against the old behaviour, those checks
now return the verdict the rule implies — which may be `allow`.

## `explain` — why a decision resolved

```text
aperture explain [options] <principal> <action> <object>
```

`explain` takes the same three arguments as `check` and prints the whole
decision trace: the subject set, every grant considered, why each did or did not
apply, and the deciding grant (marked `*`). It is a first-class operation, not a
debug afterthought.

```bash
bin/aperture explain alice read account:acme/project:atlas/document:secret
```

```text
Explain alice/read on account:acme/project:atlas/document:secret in account acme
  subjects: principal:alice, role:editor, group:engineering
  grants considered (3):
     g-eng-read-atlas [allow account:acme/project:atlas/**] allow covers the object via literal scope at specificity 39300
     g-editor-write-atlas [allow account:acme/project:atlas/**] action "write" does not match the requested "read"
   * g-deny-secret-read [deny account:acme/project:atlas/document:secret] deny covers the object via literal scope at specificity 60300
  verdict: DENY (top specificity 60300)
  reason: denied by grant g-deny-secret-read (deny account:acme/project:atlas/document:secret) at specificity 60300; 2 matching grant(s) considered
```

When a grant's scope is decided by a rule, the trace also carries the
**evaluation notes** that rule recorded — a metadata field read with the wrong
shape, or a match that happened only because the field was absent:

```text
  evaluation notes (1):
     g-viewer-read [rule public-documents]: object.tags: expected collection, got string
```

Notes are diagnostic only; they never change a verdict, and they name paths and
shapes, never metadata values.

Full flags: [`explain`](../reference/cli.md#aperture-explain).

## `enumerate` — list objects a principal may act on

```text
aperture enumerate [options] <principal> <action> <pattern>
```

`enumerate` turns the question around: instead of one object, it lists the
object ids under a `<pattern>` that the principal may take `<action>` on, one id
per line. `--limit` caps the result count. Enumeration expands objects from the
object sources the model declares — `providers:` (a file- or database-backed
provider per type) and `objects:` (metadata declared inline) — so a model with
neither (like the embedded example) yields an empty list. Run `enumerate` against
a seed that declares an object source for the type.

```bash
bin/aperture enumerate alice read 'account:acme/project:atlas/document:*' \
  --seed ./model-with-providers.yaml --limit 100
```

### Narrowing by object metadata

`--field` and `--fields-json` filter the listing by the objects' **metadata** —
"which of the datasets I may list carry brand Y?". They apply on top of the
access decision, so they can only ever *remove* objects from the list; an object
a `check` would deny is never returned however the predicate is written.

The predicate is **typed**: a field matches only when its value equals the wanted
value *and* is of the same kind, so the string `"5"` never matches the number
`5`. A shell flag only carries a string, and guessing which strings are "really"
numbers would make `enumerate` return objects `check` then denies — so there are
two flags rather than one, and each says what it sends:

| Flag | Sends | Notes |
|---|---|---|
| `--field key=value` | **always a string** | Repeatable. Everything after the first `=` is the value, so `--field expr=a=b` wants `"a=b"`. |
| `--fields-json '{…}'` | a JSON object — real numbers, bools, lists | Use it when the metadata value genuinely is not a string. |

**Both may be given.** `--fields-json` is merged **first** and `--field` entries
then **override it by key**, so a stored JSON body can be reused with one value
swapped from the shell.

```bash
# a string field
bin/aperture enumerate alice list 'account:acme/**' --seed ./model.yaml \
  --field tier=premium

# a list field, matched by MEMBERSHIP — "datasets carrying brand Y"
bin/aperture enumerate alice list 'account:acme/**' --seed ./model.yaml \
  --field brands=brand:Y

# seats is a NUMBER, so it needs the JSON spelling; --field seats=5 matches nothing
bin/aperture enumerate alice list 'account:acme/**' --seed ./model.yaml \
  --fields-json '{"seats":5,"active":true}'

# merged: seats from JSON, tier overridden from the shell
bin/aperture enumerate alice list 'account:acme/**' --seed ./model.yaml \
  --fields-json '{"seats":5,"tier":"basic"}' --field tier=premium
```

The rules the listing obeys:

- **Predicates are ANDed** — every one must hold.
- **A list-valued field matches by membership**; a whole list in
  `--fields-json` is a container compared by equality.
- **A field the object does not carry never matches** — not even against
  `null`.
- **Filtering happens before `--limit`.** `--field tier=premium --limit 10` gives
  the first ten *premium* objects, not the premium ones among the first ten
  candidates.

A malformed predicate is a usage error (`APERTURE_INVALID_INPUT`) naming the
offending text — a `--field` with no `=`, an empty key, a `--fields-json` that is
not JSON or is JSON but not an object. It is never silently skipped: a dropped
predicate would *widen* the result, and a filter that silently widens is a filter
that authorizes. Parsing happens before the store is opened, so a usage error
never boots a decision stack.

Filtering needs an object source for the type, exactly as enumeration itself
does. A model that declares none reports `APERTURE_PROVIDER_UNREGISTERED` rather
than printing nothing — an empty list would read as "no access". (Because the
predicate is applied per candidate, you only see that error once the principal is
allowed at least one object under the pattern.)

Full flags: [`enumerate`](../reference/cli.md#aperture-enumerate).

## `identifiers` — a type's valid instance ids

```text
aperture identifiers [options] <object_type>
```

`identifiers` lists every valid instance id of an object type, read from the
object source the model binds to that type — a `providers:` entry (a CSV file
today, a data source later) or inline `objects:` metadata. `--exclude` drops ids
from the result — this is how an exclusive "all except these" allowance expands
into a positive allow-list. Because it needs a source, `identifiers` errors with
`APERTURE_PROVIDER_UNREGISTERED` against a model that declares none, so run it
against a seed that binds the type:

```bash
bin/aperture identifiers document --seed ./model-with-providers.yaml
bin/aperture identifiers document --seed ./model-with-providers.yaml --exclude secret
```

Full flags: [`identifiers`](../reference/cli.md#aperture-identifiers).

## Related

- [Global options](global-options.md) — `--seed` / `--store` / `--account`, and why the principal is positional here.
- [First decision (CLI)](../getting-started/first-decision-cli.md) — the same commands walked through against the example model.
- [Mutations](mutations.md) — change the grants these decisions read.
- [Command-Line Reference](../reference/cli.md) — the generated flag tables.
