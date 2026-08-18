---
name: api-surface
description: The full Aperture API over Twirp + net/http + CLI — decisions and mutations behind one service facade, with auth required and admin tiers enforced on every mutation.
applies_to: [twirp, http, cli, mcp]
---

# API surface

Aperture exposes its whole API — queries AND mutations — over three coordinated
surfaces that all drive ONE facade (`service.Service`), so the auth policy, the
admin-tier enforcement, and the fail-closed decision semantics live in exactly
one place (FR-26, FR-28).

- **Twirp** (`internal/wire/rpc`, package `aperture`) — the generated
  `ApertureService` server, mounted on the net/http `ServeMux` under the path
  prefix `/twirp/aperture.ApertureService/` with `twirp.ServerHooks` request +
  error logging (the orbit pattern). The handler is `internal/server/twirp.go`.
- **net/http** — the minimal plain `POST /check` decision route is preserved
  (E1-S5) alongside the Twirp surface, plus `GET /healthz`.
- **CLI** (`urfave/cli/v3`) — `check`, `enumerate`, `explain`, `identifiers`
  (decisions + provider reads) and
  `put`, `get`, `list`, `delete`, `bestow`, `revoke`, `impersonate`, `template`
  (`put`/`get`/`list`/`delete`/`apply`), `bulk` (`grant`/`revoke`) (mutations).
  Each command builds the same fully-wired facade in-process; `cmd/aperture`
  stays a thin adapter.

## The facade (`service.Service`)

`service.New(eng, opts...)` returns the facade. With no options it is read-only
(the decision API); the mutation surface turns on with `WithStorage` +
`WithGate` (+ `WithDelegation` / `WithImpersonation`). One facade is the single
seam the read-subset MCP (E4-S3), audit-wrapping (E4-S2), provisioning (E5), and
the UI (E6) all build on.

Full surface:

- **Decisions** (read): `Check`, `Enumerate`, `Explain` + `CheckBatch`,
  `EnumerateBatch`, `ExplainBatch`. Fail-closed (an operational error folds to a
  deny; only input-validation is returned). `EnumerateQuery` additionally carries
  the OPTIONAL metadata filter — see [the enumerate metadata
  filter](#the-enumerate-metadata-filter).
- **Audit query** (read): `QueryAudit(AuditFilter)` returns the append-only audit
  events matching the filter (actor, account, event type, outcome, since/until,
  limit), newest-first, each as canonical JSON. It is a GATED read — a
  system-admin reads the whole trail; an account-admin reads only events scoped to
  their own account (the filter must name it). It records nothing (not itself an
  audited mutation) and backs the E6-S4 audit viewer.
- **Entity CRUD**: `Put/Get/List/Delete` for `ObjectType`, `Permission`,
  `Principal`, `Role`, `Group`, `Account`; `Put/Delete` for `Membership`;
  `Put/Get/List/Delete` for `Grant`.
- **Object identifiers (read)**: `ObjectIdentifiers(objectType, exclude...)`
  enumerates a type's INSTANCE ids from its provider (the `providers:` or
  `objects:` section a seed declares, wired with `WithProviders`; when both
  sections claim a type the file-backed `providers:` entry wins the type outright
  by default and every inline entry for it is discarded, so the ids enumerated are
  the file's alone — see `skills/metadata-values.md`) — the complete,
  unbounded set, minus
  any `exclude` ids. It materialises the positive allow-list an EXCLUSIVE
  allowance ("all objects of this type except these ids") expands to. An
  object-type with no declared provider → `APERTURE_PROVIDER_UNREGISTERED`; a
  facade built without `WithProviders` → `APERTURE_UNIMPLEMENTED`.
- **Rules (E7-S3)**: `Put/Get/List/Delete` for `Rule` (the named rule-AST
  definitions the node editor authors and rule-backed scope strategies resolve;
  the AST rides as `rule_json`/`rules_json`, the exact `rules.Node` serialization).
  `PutRule` DEEP-validates the AST (structure + compile pass) and rejects an
  invalid rule with its `APERTURE_RULE_*` code before persisting. `ValidateRule`
  runs that same validation WITHOUT persisting, so the editor can check before it
  saves. Rule DEFINITION is SYSTEM tier; reads require auth only.
- **What-if (read-only, E7-S3)**: `Simulate` / `SimulateExplain` render the
  decision (and full Explain trace) for a query under a hypothetical `Overlay`
  (rules / grants / permissions / principals) layered over the live model,
  persisting nothing. They back the rule editor's live preview of an UNSAVED rule:
  the overlay rule shadows the stored one of the same name, so a preview reflects
  the edit against grants that reference it. Requires an authenticated principal.
- **Rule what-if against an object (read-only)**: `EvaluateRule(ast, objectID)`
  compiles an UNSAVED rule AST and evaluates it directly against ONE object's
  provider metadata (`WithProviders`), returning the boolean result plus the
  metadata snapshot. No account/principal/grant is involved — the rule reads only
  `object.*` — so the rule builder can sample an object (via `ObjectIdentifiers`)
  and show whether the rule selects it. Requires an authenticated principal.
  `EvaluateRulePreview` is the same evaluation returning the diagnostics too —
  the reference instant, what each relative-date operand resolved to at it, and
  the evaluation's deny-safe notes (`RulePreview`); `EvaluateRule` is its narrow
  projection. **This path supplies the reference instant itself** (from the
  facade clock, `WithClock`) because it compiles the AST directly rather than
  going through the decision engine's `rules.WithDecisionInstant` scope: without
  it every relative date resolves to nothing and the preview denies a rule that
  a real `Check` allows.
- **Delegation**: `Bestow`, `Revoke`.
- **Impersonation**: `ImpersonationStart`, `ImpersonationStop`.
- **Provisioning (E5-S1)**: `Put/Get/List/Delete` for `Template` (named,
  versioned, parameterized grant bundles); `ApplyTemplate` (resolve params →
  expand → apply transactionally → one audit event); `BulkPutGrants` /
  `BulkDeleteGrants` (provision/deprovision many grants atomically). Template
  DEFINITION is SYSTEM tier; APPLY and BULK write grants, so they are ACCOUNT tier
  in the target account. All three are TRANSACTIONAL via `Storage.Atomic` — a
  partial failure rolls the WHOLE operation back, so no grant persists if any
  fails.

## The enumerate metadata filter

`Enumerate` (and `EnumerateBatch`, and the impersonated `EnumerateAs`) takes an
OPTIONAL set of object-metadata predicates that narrow the result. It is one
input crossing five layers, and every layer spells it differently:

| Layer | Spelling |
|---|---|
| `engine.EnumerateRequest` | `Fields map[string]any` |
| `service.EnumerateQuery` | `Fields map[string]any` — forwarded to the engine **unchanged** by `request()` |
| Twirp (`service.proto`) | `EnumerateRequest.fields`, `map<string, google.protobuf.Value>` (field 6), decoded by `rpc.FieldsFromWire` / encoded by `rpc.FieldsToWire` (`internal/wire/rpc/fields.go`, hand-written beside the generated code) |
| CLI | `aperture enumerate --field key=value` (repeatable) and `--fields-json '{…}'` |
| MCP | `aperture_enumerate` / `aperture_enumerate_batch` input `Fields`, reflected off `service.EnumerateQuery` |

Omitting it everywhere means the same thing: a nil Go map, "no predicate", and
the engine does not even consult a metadata source.

### Semantics a caller cannot guess

The meaning is `provider.Filter`'s `Fields` contract **verbatim** — one
definition, one implementation (`provider.MatchFields`), so an enumeration
filtered by the engine and one filtered inside a provider's `Query` select the
same objects:

- **AND across keys.** Every predicate must hold.
- **A collection field matches by MEMBERSHIP.** `{"brands": "brand:Y"}` selects
  objects whose `brands` array contains `brand:Y`. A *list-valued want* is a
  container compared by equality, not a membership set.
- **An absent field NEVER matches** — not even against a `null`/`nil` want. An
  object whose metadata lacks the key is excluded.
- **Comparison is TYPED.** Numbers compare across Go numeric types by value
  (`int64(5)` matches a `float64(5)` want), but `"5"` never matches `5`.

Two ordering/failure rules matter as much as the comparison rules:

- **The filter runs BEFORE `Limit`.** Candidates are decided, then predicated,
  then truncated. Filtering after truncation would answer a different question —
  the matches among the first `Limit` candidates rather than the first `Limit`
  matches — and return a silently wrong answer.
- **The filter can only SUBTRACT from the allowed set.** It is applied to
  candidates that already survived deny-overrides and specificity, so no
  predicate can surface an object `Check` would deny.

### Failure modes

- **No metadata source, or no provider for the candidate's object-type** →
  `APERTURE_PROVIDER_UNREGISTERED`, never a silently empty result (an empty list
  reads as "no access" and would hide the misconfiguration). The metadata source
  is `engine.WithMetadata(reg)`, wired to the same `*provider.Registry` that
  backs the scope lister; `internal/cli.buildDecisionStack` does this for
  `serve`, the one-shot commands, and `aperture mcp` alike.
  Because the predicate runs **per candidate**, this error only surfaces when the
  enumeration has at least one ALLOWED candidate. An enumeration whose allowed
  set is empty returns empty regardless of how metadata is wired.
- **The object has no metadata row** (the provider's `Fetch` returns
  `APERTURE_NOT_FOUND`) → every field is absent, absent never matches, so the
  object is EXCLUDED rather than erroring. Any other `Fetch` failure surfaces
  verbatim.
- **A malformed predicate** → `APERTURE_INVALID_INPUT`, never a dropped
  predicate. A dropped predicate WIDENS the result, and a filter that silently
  widens is a filter that authorizes. In `EnumerateBatch` the rejection rides in
  that item's error slot alone (`BatchResult`'s contract) — one bad query never
  fails the batch, and the item reports the error rather than an empty list.

### Wire encoding: `google.protobuf.Value`, and its one caveat

`Value` rather than a JSON string because the predicate compares by *type*: the
schema itself states the string/number/bool/list distinction, so a generated
client in any language builds a well-formed predicate and a malformed one is
rejected at the boundary. `FieldsFromWire` maps each kind onto exactly one Go
shape (`null`→`nil`, `number`→`float64`, `string`, `bool`, `list`→`[]any`,
`struct`→`map[string]any`), recursively, with **no coercion between kinds** — a
number is never parsed out of a string. `FieldsToWire` is its inverse.

- **`>2^53` loses precision.** `Value` carries every number as a double, so an
  integer beyond 2^53 does not survive the trip and must be sent (and stored) as
  a **string**. `FieldsToWire` does not error on it — the loss is a property of
  `Value`, not something the conversion can repair.
  `TestFieldsRoundTrip_LargeIntegerLosesPrecision` fails loudly if that stops
  holding.
- **Non-finite numbers are REJECTED.** NaN and ±Inf are
  `APERTURE_INVALID_INPUT`, not `structpb`'s strings `"NaN"` / `"Infinity"` — a
  numeric want silently turned into a string want would match nothing and read
  as "no access". protojson cannot marshal a non-finite `Value` at all, so only
  the binary codec can reach that guard.

### The CLI's two spellings

A shell flag only ever carries a string, but the predicate is typed, and
guessing (parsing `"5"` into a number in the adapter) would make an enumeration
return objects a `Check` then denies. So `aperture enumerate` offers both and
says which is which:

- `--field key=value` — repeatable; the value is **always a string**. Everything
  after the FIRST `=` is the value, so `--field expr=a=b` wants `"a=b"`.
- `--fields-json '{…}'` — a JSON object, for a value that genuinely is a number,
  bool, or list.

**Precedence: `--fields-json` is merged FIRST and `--field` entries then override
by key**, so a stored JSON body can be reused with one value swapped from the
shell. The rule is stated in both flag usages and the command description, and a
test asserts `--help` still says so. Parsing happens before the store is opened,
so a usage error never boots a decision stack.

### The MCP schema's `omitempty`

`service.EnumerateQuery.Fields` carries the first `json`/`jsonschema` struct tags
on any `service` type, because `mcp.EnumerateIn` **aliases** the struct and the
tool schema is reflected off it. The `omitempty` is load-bearing: without it the
reflected schema marks the predicate REQUIRED, making an unfiltered
`aperture_enumerate` — by far the common call — unrepresentable for a
schema-validating client. The JSON name stays PascalCase `Fields` so one object
does not mix two spellings. Do not drop either tag in a refactor.

## Auth + admin-tier policy

- **Decision RPCs are open** — `Check` / `Enumerate` / `Explain` require no
  credential, preserving the simple decision path. `POST /check` stays open too.
- **Entity reads require an authenticated principal**, and account-scoped reads
  are additionally SCOPED to the caller's visibility (a customer's admin must not
  enumerate another customer's data). `ListAccounts` / `ListPrincipals` /
  `ListGrants` / `GetGrant` resolve through `service.readScope`: a system-admin
  (`aperture.admin` on `system:schema`, resolved in the `"*"` account) sees
  everything; any other principal is scoped to the accounts it can SEE — the
  accounts it is a MEMBER of plus the accounts it ADMINISTERS — and within those,
  their grants and the principals who are members of them (plus itself). Platform
  (`"*"`) grants are system-admin-only. A read for an
  account the caller does not administer returns `APERTURE_AUTHZ_DENIED`. The
  shared-schema catalogs (`ObjectType`, `Permission`, `Role`, `Group`, `Rule`,
  `Template`) stay readable by any authenticated principal — they are the
  vocabulary an account-admin needs, not per-account data. `ObjectIdentifiers` is
  likewise an auth-required read (all of a type's instance ids, not a
  principal-scoped decision). Scoping only engages when a gate is wired and a
  principal is identified, so the local CLI/MCP facades stay unrestricted.
### `ListGrants` — all-accounts view + pagination

`ListGrants` (`ListGrantsRequest` → `ListGrantsResponse`) lists grants for a
scope, gated by `service.readScope` as above, and is paginated:

- **Scope via `account_id`.** A **non-empty** `account_id` lists that single
  account's grants — the original behaviour, byte-for-byte compatible. An
  **empty** `account_id` is the **all-accounts sentinel**: it lists grants across
  EVERY account and is **SYSTEM-ADMIN ONLY** (`sys == true` from `readScope`). An
  account-admin — however many accounts it administers — is denied the
  all-accounts path with `APERTURE_AUTHZ_DENIED` / 403 (the service gates it
  before the store is touched, and never leaks which accounts exist). The
  wildcard-stamped (`"*"`) platform grants are returned **inline** in an
  all-accounts page, not filtered out. The empty string is a query sentinel, NOT
  a real account id — it is distinct from `"*"` (the wildcard account), which is
  never overloaded to mean "all".
- **Pagination via `offset` + `limit`.** The request carries `offset` (leading
  rows to skip) and `limit` (page size); the response is `ListGrantsResponse`,
  which carries the page as `entities_json` (each grant canonical JSON, like
  `EntityListResponse`) plus `total` (the full pre-pagination match count for the
  scope) and echoes the effective `offset` / `limit`. The client renders next/prev
  from `total` (`next_offset = offset + limit` while `next_offset < total`). A
  non-positive `limit` falls back to `model.DefaultGrantPageSize` and a `limit`
  above `model.MaxGrantPageSize` is clamped down (`model.ClampGrantPage`), so a
  single call never returns an unbounded page. A **negative** `offset` or `limit`
  is a caller error → `APERTURE_INVALID_INPUT` / 400. Older clients that omit the
  page fields get the default first page, preserving the original single-account
  behaviour. The `filter` (if any) applies server-side to the returned page.

- **Mutations require an authenticated principal AND the admin tier their kind
  needs** (`authz.Gate`): schema entities are SYSTEM tier (`system:*`);
  membership + raw grants are ACCOUNT tier (`account:<acct>/admin:*` in the
  target account). Unauthenticated → `APERTURE_UNAUTHENTICATED` / 401; wrong tier
  → `APERTURE_AUTHZ_DENIED` / 403.
- **Delegation and impersonation are NOT admin-gated** — they carry their own
  finer-grained authorization (the delegation subset rule / the impersonation
  guardrails), where the actor is the delegator / operator, not an admin.

On the Twirp surface the actor's principal is ALWAYS the authenticated identity
from the auth middleware, never a value from the request body — a caller cannot
act as someone else. The wire's `Actor.account` selects the active account.

## Wire encoding

Simple/hot-path messages are typed (`CheckRequest`, `Decision`,
`EnumerateRequest`). Rich or nested shapes ride as a canonical JSON string: model
entities as `entity_json` (the `encoding/json` form of the `model.*` struct), the
decision `Trace` as `trace_json`. This mirrors orbit's JSON-payload convention,
keeps the proto small, and sidesteps modelling the self-referential rule AST.

The one deliberate exception is `EnumerateRequest.fields`, which is
`map<string, google.protobuf.Value>` rather than a `*_json` string: the predicate
compares by TYPE, so the string/number/bool/list distinction has to live in the
schema where a generated client can see it, not inside an opaque blob. See [the
enumerate metadata filter](#the-enumerate-metadata-filter) for the conversion and
its `>2^53` caveat.

## Regenerating

`make proto` regenerates `service.pb.go` + `service.twirp.go` from
`service.proto` (needs `protoc` + `protoc-gen-go` + `protoc-gen-twirp`). The
generated files are COMMITTED; CI does not regenerate, so re-run `make proto` and
commit the result whenever the `.proto` changes.
