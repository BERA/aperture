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
