# CLAUDE.md — Aperture

Conventions catalog for `github.com/frankbardon/aperture`. Authority order:
`.planning/access-control/context.md` > `.planning/access-control/brief.md` >
this file > the orbit/pulse reference repos.

## Project overview

Aperture is a fine-grained access-control engine for the frankbardon/* family.
It is **library-first**: the public Go packages at the module root are the
product, and every surface (CLI, Twirp/HTTP, MCP) is a thin translator over a
single decision engine. Aperture mirrors **Orbit** structurally and **Lattice**
for the MCP boundary, but keeps its public packages at the module root like
**Pulse** rather than under `internal/`.

The decision API is `Check` / `Enumerate` / `Explain`, each available single and
bulk-batched.

## Stack & build

- **Go 1.26.1**, `CGO_ENABLED=0`, pure-Go end to end. Module
  `github.com/frankbardon/aperture`.
- **CLI** is `urfave/cli/v3`; `cmd/aperture/main.go` only assembles the command
  tree (no business logic).
- **Rules** use the `github.com/expr-lang/expr` evaluator **directly** (pure-Go,
  the same engine Pulse wraps) — Aperture has no dependency on Pulse. The rules
  package renders its AST to an expr-lang expression and compiles it in-process.
- **Storage**: hand-written SQL, `modernc.org/sqlite` (pure-Go) + an
  in-memory impl behind one `Storage` interface. No ORM / sqlc / migration tool.
- **RPC/HTTP**: `net/http` ServeMux + Twirp (`internal/server/`, proto at
  `internal/wire/rpc/service.proto`), with an admin UI shell served from
  `internal/server/static/`.
- **MCP**: SDK-free core (`mcp/`, surfaced as `aperture mcp` over stdio); one
  adapter package may import the protocol SDK, enforced by a firewall test.

```bash
make build   # produce bin/aperture (-ldflags="-s -w" -trimpath, CGO off)
make test    # go test ./...
make fmt     # go fmt ./...
make vet     # go vet ./...
make lint    # vet + staticcheck (degrades to vet-only when not installed)
```

## Coding conventions

### Errors

- Every failure is an `APERTURE_*` coded error from the root `errors/` package.
- Codes are **SCREAMING_SNAKE**, namespaced `APERTURE_*`, defined in
  `errors/codes.go` and listed in `AllCodes`.
- Each code has a `Registry` entry with a `Message` and either at least one
  `Fixup` or `FixupNotApplicable=true`. Gated by `TestCodesHaveFixups`.
- Construct via `errors.New` / `Newf` / `Wrap` / `Wrapf`; recover the code with
  `errors.CodeOf`. Any error already carrying an `APERTURE_*` code passes through
  verbatim — the wrappers never re-stamp it.

### Library-first

- Business logic lives in the public root packages. `cmd/aperture/` is a thin
  adapter; later stories add manual DI in the `serve` command (no wire/fx/dig).
- Config is env vars (`APERTURE_*`) + optional YAML; `.env` via dotenv.

### Naming

- No predecessor references (no `Aperture2` / `LegacyX`).

## Update Demand

Any change to a registered surface MUST update the matching `skills/` document in
the **same PR**. A surface change that lands without its doc update is a
non-skippable CI failure. The rule itself is documented in
`skills/update-demand.md` and is self-protecting.

| If you change... | You MUST also update... | Enforced by |
|---|---|---|
| An Aperture error code | `errors/codes.go` (`AllCodes` + `Registry` entry with Message + Fixups) | `TestCodesHaveFixups` |
| A `skills/*.md` doc | its YAML frontmatter (`name` matching the file stem + `description`) | `TestEverySkillHasFrontmatter` |
| The Update-Demand rule | `skills/update-demand.md` (must remain present with frontmatter) | `TestUpdateDemandDocPresent` |
| A rule operator (`Op*` / `opSpecs` in `rules/ast.go`) | `OP_SPECS` in `internal/server/static/js/rules-serializer.js`, the palette in `rules.js`, and `skills/rules-engine.md` | `TestEditorOperatorTablesAgree`, `TestEditorASTContractCoversEveryOperator` |
| A right-operand shape (`rightShape` in `rules/ast.go`) | `RIGHT` in `rules-serializer.js` **and** `jsShapeNames` + `editorJSONForOp` in `rules/editor_js_contract_test.go` | `TestEditorOperatorTablesAgree`, `TestEditorASTContractCoversEveryOperator` |
| A collection operator's shape expectation (`collOps` in `rules/shape.go`) | the matching `opSpecs` entry in `rules/ast.go` | `TestCollectionOperatorTablesAgree` |
| A date operator's runtime policy (`dateOps` in `rules/date.go`) | the matching `opSpecs` entry in `rules/ast.go`, and the deny-safe policy in `skills/rules-engine.md` | `TestDateOperatorTablesAgree` |
| A `rules.NoteKind` (`rules/notes.go`) | its `Note.String()` case and the note-kind list in `skills/rules-engine.md` | reviewed; no registry gate |
| A callable rule function (`defaultFunctions` in `rules/compiler.go`) or a blocked builtin (`blockedCallNames`) | `FUNCTIONS` / `BLOCKED_CALLS` in `rules-serializer.js` | `TestEditorVocabularyTablesAgree` |
| An AST node type, variable root, or var-path grammar | `TYPES` / `ROOTS` / `VAR_PATH` in `rules-serializer.js` | `TestEditorVocabularyTablesAgree` |
| The metadata value model (`provider/metadata.go`: legal shapes, depth cap, size cap) | `skills/metadata-values.md` and `docs/src/concepts/providers.md` | `TestEverySkillHasFrontmatter` (doc presence); model behaviour by `provider/metadata_test.go` |
| The date value model (`provider/date.go`: the canonical forms, the accept/reject set, `DateReason`) | `skills/metadata-values.md` ("Dates") and `docs/src/concepts/providers.md` | `TestEverySkillHasFrontmatter` (doc presence); model behaviour by `provider/date_test.go` |
| A loader's spelling of the value model (a CSV column suffix — `:int`, `:list<T>`, `:json`, `:date`, `:datetime` — or a seed key such as `objects:` / `field_types:`) | `skills/metadata-values.md` ("How each loader spells the model"), the loader's package doc, `docs/src/concepts/providers.md`, and `docs/src/concepts/seed.md` for a seed key | reviewed; no registry gate |

The Go↔JS rows matter more than they look: **CI is node-free**, so
`rules-serializer.test.js` never runs in the pipeline.
`rules/editor_js_contract_test.go` is what actually enforces parity — it reads
the JS file from disk and diffs the tables, and it **fails** rather than skips if
that file moves.

As the remaining surfaces land (identity, model, engine, scope, account, auth,
audit, mcp), each story adds a `skills/<feature>.md` doc and a coverage gate in
`skills/skills_test.go` that walks the surface's registry, then a row here.

## Non-skippable CI gates

- `TestCodesHaveFixups` — every `APERTURE_*` code has a Registry entry with a
  Message and a Fixup (or `FixupNotApplicable`).
- `TestRegistryHasNoOrphans` — `Registry` contains nothing absent from `AllCodes`.
- `TestCodesAreScreamingSnakeNamespaced` — every code is SCREAMING_SNAKE and
  `APERTURE_`-prefixed.
- `TestUpdateDemandDocPresent` — the Update-Demand seed doc exists with
  frontmatter.
- `TestEverySkillHasFrontmatter` — every `skills/*.md` has a `name` (matching its
  file stem) and a `description`.
- `TestEditorOperatorTablesAgree` / `TestEditorVocabularyTablesAgree` /
  `TestEditorUnaryOperatorsAgree` / `TestEditorValidationMessagesAgree` — the Go
  rule AST and the JS serializer expose the same operators, operand shapes, node
  types, roots, functions, blocked builtins, and validation wording. Reads
  `rules-serializer.js` from disk; fails (never skips) if it is missing.
- `TestEditorASTContractCoversEveryOperator` — every operator in `opSpecs` has a
  byte-stable AST JSON case, so coverage cannot fall behind the registry.
- `TestCollectionOperatorTablesAgree` — `collOps` (`rules/shape.go`) stays in
  lockstep with `opSpecs` (`rules/ast.go`).
- `TestDateOperatorTablesAgree` — `dateOps` (`rules/date.go`) stays in lockstep
  with `opSpecs` (`rules/ast.go`), membership and ternary arity alike.

Gated, NOT in `make test` (a loaded runner would flake them):

- `APERTURE_BENCH_ASSERT=1 go test -run TestCheckNFR ./bench/` — cached `Check`
  p99 < 1ms and ≥ 10k checks/sec, including the collection and nested-access
  fixtures.
- `node internal/server/static/js/rules-serializer.test.js` — CI is node-free, so
  this is a manual development aid; the Go contract tests above are the real gate.

## What NOT to do

- Don't put business logic in `cmd/aperture/`.
- Don't add a dependency on Pulse — the rules engine uses `expr-lang/expr`
  directly; keep `CGO_ENABLED=0` (no geo/h3 or other CGO packages).
- Don't return bare `errors.New`/`fmt.Errorf` across package boundaries — wrap in
  an `APERTURE_*` coded error.
- Don't commit `.planning/`.
- Don't leak cross-account data through error messages.
