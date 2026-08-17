---
name: ui-shell
description: Aperture's embedded admin shell — a vendored static frontend (Alpine + tokens) served from the site root, carrying the dev bearer on API calls. Chrome + plumbing only; domain screens land later.
applies_to: [frontend, http]
---

# UI shell

Aperture ships a single-page admin shell embedded in the binary. It is the
**chrome** — a sidebar + top bar + content frame — plus the domain screens that
fill it. The nav is Model (CRUD), Grants, Audit, What-if, Import / export, and
Rules; E7 mounts the Rete rule canvas in the Rules section and (E7-S3) wires it
into the live system — load / save / server-validate / live what-if. There is no
node build: the whole frontend is pre-built, committed blobs behind `//go:embed`.

## Where it lives

- `internal/server/static/` — the embedded tree (`//go:embed all:static` in
  `internal/server/static.go`). `index.html` is the shell; `css/styles.css` is the
  authoritative styling; `js/app.js` is the shell Alpine component; per-screen
  components live alongside it (`js/crud.js` fills the Model section, E6-S2, with a
  data-driven entity-CRUD component driving the Twirp entity RPCs; `js/grants.js`
  fills the Grants section, E6-S3, with a three-tab provisioning component — raw
  grant management, template apply with a client-side preview + bulk provisioning,
  and delegation bestow/revoke — over the grant / template / delegation RPCs);
  `js/audit.js` fills the Audit section (E6-S4), a READ-ONLY queryable table over
  the append-only trail via the `QueryAudit` RPC (filter by actor, account, event
  type, outcome, and time window), showing both the real actor and the effective
  subject for impersonation/delegation events; `js/whatif.js` fills the What-if
  section (E6-S4), a READ-ONLY decision simulator that runs the open `Check` +
  `Explain` RPCs against the LIVE model and renders the verdict plus the full
  trace (expanded subject set, every grant considered with its action-match /
  coverage / specificity, and the deciding grants); `js/portability.js` fills the
  Import / export section (E6-S4), driving `Export` (download the declarative
  model file) and `Import` (upload → client-side preview DIFF of would-be
  adds/changes against a fresh export → confirm → transactional upsert), all three
  gated by the tier probe (audit read is system- OR account-admin; what-if is
  open; export/import is system-admin); `js/rules.js` fills the Rules section
  (E7-S2 node editor + E7-S3 integration): the Rete canvas over the
  `rules.Node` AST, plus a load picker (`ListRules`/`GetRule` → `fromAST`), a
  Save that persists the serialized AST via `PutRule` (SYSTEM tier, gated by the
  tier probe — non-admins load/validate/preview but cannot save), a server
  Validate (`ValidateRule`, non-persisting, surfacing `APERTURE_RULE_*` on the
  canvas), and a READ-ONLY object what-if that previews the UNSAVED rule on the
  canvas (`ListObjectTypes` / `ObjectIdentifiers` to pick a type + a sample
  object, then `EvaluateRule`, which persists nothing) — see "What-if preview"
  below; `vendor/` holds the pre-built blobs (see `vendor/README.md` for pinned
  versions + regeneration).
- `js/rules-serializer.js` is the pure, DOM-free, dependency-free bridge under
  `js/rules.js`: `astToGraph` / `graphToAST` are lossless inverses over the exact
  `rules.Node` JSON shape, plus `validateAST`, a client-side mirror of the Go
  validator. It is unit-testable under plain `node` — see below.
- Served by `staticHandler()`, mounted **LAST** on the mux in `server.New`
  (`mux.Handle("/", …)`). net/http resolves by longest matching pattern, so the
  Twirp prefix, `POST /check`, and `GET /healthz` win over root `/` — the file
  server never shadows the API. Guarded by `internal/server/static_test.go`.

## The rule serializer contract (`js/rules-serializer.js`)

The rule editor has **no** rule format of its own. `js/rules-serializer.js`
carries a hand-maintained JS mirror of `rules/ast.go`, and the two must move
together:

| JS (`rules-serializer.js`) | Go | Compared by |
|---|---|---|
| `TYPES` | `NodeType` consts (`rules/ast.go`) | `TestEditorVocabularyTablesAgree` |
| `OP_SPECS` / `OPS` (derived) | `opSpecs` / `Op*` consts (`rules/ast.go`) | `TestEditorOperatorTablesAgree` |
| `RIGHT` | `rightShape` (`rules/ast.go`) | `TestEditorOperatorTablesAgree` |
| `DATE_OPS` | `opSpecs` entries with kind `renderDate` (`rules/ast.go`) | `TestEditorOperatorTablesAgree` |
| `ROOTS` | `allowedRoots` (`rules/ast.go`) | `TestEditorVocabularyTablesAgree` |
| `BLOCKED_CALLS` | `blockedCallNames` (`rules/ast.go`) | `TestEditorVocabularyTablesAgree` |
| `FUNCTIONS` | `defaultFunctions` (`rules/compiler.go`) | `TestEditorVocabularyTablesAgree` |
| `ANCHORS` | `relativeAnchors` (`rules/relative.go`) | `TestEditorVocabularyTablesAgree` |
| `UNITS` | `relativeUnits` (`rules/relative.go`) | `TestEditorVocabularyTablesAgree` |
| `SNAPS` | `relativeSnaps` (`rules/relative.go`) | `TestEditorVocabularyTablesAgree` |
| `VAR_PATH` | `varPath` (`rules/ast.go`) | — (identical literal, by eye) |
| `validateAST` | `(*Node).Validate` (`rules/ast.go`) | `TestEditorValidationMessagesAgree` |

`OP_SPECS` is the single registry both directions and the validator read, so a
new operator is one entry, never a scattered conditional. It records each
operator's **right-operand shape** — `any` (the scalar comparisons), `collection`
(a `list` or a `var`: `in` `nin` `hasAll` `hasAny` `hasNone` `subsetOf`),
`element` (anything but a `list`: `has` `hasKey`, and seven of the eight date
operators), `none` (the unary `isEmpty` `isNotEmpty` `exists`), or `bounds` (a
`list` of **exactly two** items: `between` alone). `validateAST` enforces exactly
those rules and reports the Go validator's own codes and message wording, so a
rule the canvas refuses is refused for the same stated reason server-side.

### Dates in the serializer

The eight date operators (`before` `after` `onOrBefore` `onOrAfter` `between`
`sameDay` `sameMonth` `sameYear`) are ordinary `compare` nodes. `between`'s
`right` is a two-item `list` — **no new field and no new JSON key**, so every
rule persisted before dates existed round-trips byte-identically, and the
author's one `between` node reads back as one node rather than as a re-sugared
pair of comparisons.

Date-ness lives in **`DATE_OPS`**, not in the `OP_SPECS` entries, because those
entries have to stay in the literal `{ right: RIGHT.X }` form the Go scanner
reads; the gate compares `DATE_OPS` against the Go table instead. It is needed
because date-ness is a **positional permission**:

**`relativeDate`** is the one node type that is an operand and nothing else. It
is legal only directly under a date operator — either side, or either `between`
bound — and `APERTURE_RULE_INVALID` everywhere else (as an `eq` operand, an `in`
list item, a call argument, a logical child, or the whole rule). **The permission
is not inherited**: a relative date nested inside a call that is itself a date
operand is still refused, exactly as the Go walk refuses it.

It carries four fields and **all four are always present** — `anchor`, `n`,
`unit`, `snap`, emitted in that key order after `type`. "No offset" is `n: 0` and
"no snap" is the vocabulary member `"none"`; absence never means anything, so an
empty control is a validation problem rather than a silently different rule.
`n` is a JSON **number**, and **negative goes into the past** — there is no
direction field anywhere.

```json
{"type":"relativeDate","anchor":"NOW","n":-3,"unit":"months","snap":"none"}
```

`ANCHORS` / `UNITS` / `SNAPS` are the three closed vocabularies, one per field.
The Go side holds each as a map and the gate compares them **as sets**, so their
order in the serializer is purely presentational and is chosen there, once, for
the editor's controls: anchors as declared, units coarsest-first, snaps with the
identity `none` leading and the rest widest-to-narrowest, start before end.
`validateRelativeDate` checks each field against its set and reports each
**independently**, so all four controls can be flagged at once.

`normalizeOffset` turns whatever the `n` control holds into that JSON number: a
number passes through untouched (so a loaded rule round-trips byte-for-byte), an
empty control is `0`, and a token that is not a JSON number is kept **verbatim**
for the validator to name rather than coerced into a cutoff the author never
wrote.

**Never build a JS `Date` out of any of this.** The engine is UTC end to end and
the editor renders stored date strings verbatim, `Z` included: `toLocaleString()`
would show a viewer in UTC-5 a stored `2026-01-01T00:00:00Z` as
`2025-12-31 19:00` — a different calendar year. That is a correctness rule, not a
preference.

**The unary operators carry no `right` key at all.** Neither direction may emit
one — `graphToAST` omits it (never `right: null`), `astToGraph` wires no `right`
pin (`inputKeys("compare", arity, op)` drops it), and a `right` supplied anyway
is an `APERTURE_RULE_INVALID` error rather than something silently dropped.
Emitting the key would break the byte-identical round-trip the AST guarantees.

Client-side validation is **shape only**, never types: operand types, function
arity, and signatures are the compiler's job and stay server-side behind
`ValidateRule`. An unknown function NAME is likewise not rejected — a host may
register more — but expr's predicate builtins (`BLOCKED_CALLS`) are, because the
server denies them structurally.

Run the round-trip test with plain node, no dependencies and no build step:

```
node internal/server/static/js/rules-serializer.test.js   # exit 0 = pass
```

**CI is node-free, so that test never runs in the pipeline.** Two Go test files
are the CI-guarded half of the invariant, and they do different jobs:

- **`rules/editor_contract_test.go`** pins the AST JSON the editor emits:
  `TestEditorASTContract` parses the identical strings through `rules.Node` and
  asserts each is valid and **byte-stable** under marshal→unmarshal→marshal.
  `TestEditorASTContractCoversEveryOperator` drives that same assertion off
  `opSpecs` itself, so a new operator is round-trip-covered the moment it lands
  and the arity rule is pinned both ways (a unary with a `right` is rejected, a
  binary without one is rejected).
- **`rules/editor_js_contract_test.go`** actually **READS
  `rules-serializer.js` from disk** and compares its tables against the Go
  originals, entry for entry — that is what makes an operator added to ONE side
  fail. It scans the JS with a small hand-rolled scanner (no JS-parser
  dependency, no new module, **node is never invoked**), and a missing or
  renamed table **fails**, never skips.

The table above names which test compares each pair. A failure names the
operator and the side it is missing from:

```
operator "hasNone" is in Go opSpecs (rules/ast.go) but MISSING from JS
OP_SPECS (…/rules-serializer.js): add `hasNone: { right: RIGHT.COLLECTION },`
to the JS table (and a case to rules-serializer.test.js)
```

So a new operator lands in `opSpecs`, in `OP_SPECS`, and in
`rules-serializer.test.js` — the first two are enforced, the third stays manual
discipline because nothing runs node. Keep `OP_SPECS` entries in the literal
`{ right: RIGHT.X }` form and `OPS` derived as `Object.keys(OP_SPECS)`; both are
asserted, because the comparison is only as good as the table's readability.

## Authoring operators (rule editor palette + comparison node)

Every comparison operator is authorable **by clicking**. The palette's Compare
group is GENERATED in `js/rules.js` (`buildPalette` → `operatorEntries`) from the
serializer's `OP_SPECS`, so it lists each operator exactly once, in `ast.go`'s
`Op*` order, and a new operator appears with no edit to the palette. Clicking one
drops a `compare` node already set to that operator.

- **Readable spellings.** An author never sees a raw token. `OP_LABELS` maps
  token → spelling — `has member`, `has all`, `has any`, `has none`, `subset of`,
  `has key`, `is empty`, `is not empty`, `exists`, plus `equals`, `not equals`,
  `less than`, `less or equal`, `greater than`, `greater or equal`, `in`,
  `not in`, and the eight date operators as sentences: `is before`, `is after`,
  `is on or before`, `is on or after`, `is between`, `is on the same day as`,
  `is in the same month as`, `is in the same year as`. It is a spelling table
  only: **membership and order come from `OP_SPECS`**, and an operator with no
  spelling shows its bare token rather than vanishing. The spelling is
  presentation — `opFromLabel` resolves it back to the AST token in
  `reteToGraph`, so nothing downstream ever sees it, an unrecognised spelling
  passes through verbatim for the validator to name, and a raw token typed into
  the control is still accepted.
- **The palette entry's `kind` is compound** (`"compare:hasAll"`) because the
  shell template keys the palette by `kind`; `rules().add()` splits it into the
  node kind plus the seed operator, keeping the operator out of the template.
- **The unary operators render as SINGLE-input nodes.** A comparison's pins come
  from the serializer's `inputKeys(kind, arity, op)` — never a hardcoded
  `left`/`right` pair — both when the node is built and when its operator
  changes (`applyCompareInputs`). Choosing `is empty` removes the `right` pin
  *and any wire into it* before the pin goes; choosing a binary operator restores
  it. The re-shape is a diff, so surviving pins keep their wires, and it is
  serialised per node so a burst of edits cannot double-add a pin. An operand
  left unwired by the change stays on the canvas — the editor never deletes an
  author's work — and shows up as a second root in validation until it is
  re-wired or removed.
- **The operator control is a text input bound to a native `<datalist>`**, not a
  `<select>`: Rete's classic control set is text/number only and the React
  renderer is a committed vendored blob this repo never rebuilds, so a custom
  control would mean a node build. The list is attached by delegation on
  pointerdown/focusin (the input belongs to the renderer and is re-created by it),
  which is idempotent and survives re-renders. Every dropdown below works the
  same way, and every one of them resolves its text back through the closed set
  before the graph is read — **a `<datalist>` suggests, it does not constrain.**

## Authoring dates (relative-date node, operand modes, `between` bounds)

A date rule is built entirely by clicking. `js/rules.js` adds three things on top
of the operator palette:

### The relative-date node and its four controls

A **Relative date** palette entry sits with Variable and Literal in Operands (it
is a leaf operand: it produces a value and takes no wires). The node carries the
AST's four fields as four flat controls, in the order the node reads as a
sentence — *"the start of the year, five years back"*:

| Control | Type | Source | Notes |
|---|---|---|---|
| `anchor` | text + `<datalist>` | `ANCHORS` | `now` / `today` |
| `n` | text (`inputmode="numeric"`) | — | whole number; **negative goes into the past** |
| `unit` | text + `<datalist>` | `UNITS` | coarsest first |
| `snap` | text + `<datalist>` | `SNAPS` | identity `none` first |

- **The lists are never hand-typed in the editor.** They are read from the
  serializer's three tables (which the Go contract test compares against
  `rules/relative.go`), in the presentation order chosen there, so a unit added
  on the Go side appears in the dropdown with no edit to `rules.js`.
- **Spellings are derived, not tabled.** `vocabLabel` lowercases an ALL-CAPS
  token (`NOW` → `now`) and splits a camelCase one (`startOfYear` → `start of
  year`); `vocabFromLabel` is the inverse against the same closed list and
  returns unrecognised text **verbatim**, so a typed `fortnights` is reported as
  `relative date has an unknown offset unit: fortnights` — the Go validator's own
  wording — rather than being silently substituted. A third hand-maintained copy
  of a set already written down twice is exactly how the copies drift.
- **The offset is a TEXT control, not a number one**, and that is load bearing.
  The classic number control coerces with `+value` on every keystroke, and a
  number input holding just `-` reports an EMPTY value — so `+""` is `0`, the
  control re-renders as `0`, and the minus is wiped before a digit can follow
  (typing `-3` yields `03`). A control that cannot express "into the past" cannot
  express this node.
- **The offset is handed to the serializer raw.** `normalizeOffset` turns a blank
  control into `0` and keeps an unparseable token verbatim; the control does not
  clamp or coerce, so it never invents a cutoff the author did not write. A
  fraction saves as a fraction and is reported as
  `relative date offset must be a whole number: 1.5` — a number control could not
  even produce that token, so the fraction would be silently coerced rather than
  named.
- **There is no ago/from-now toggle.** The sign is the direction; a second field
  meaning the same thing would be a second way to write one rule.
- `validateRelativeDate` reports the four fields **independently**, and the
  validation panel lists every problem, so all four controls can be flagged at
  once.

### The operand mode switch

A date comparison carries a **mode control** per operand slot — one (`right
operand`) for the seven single-operand date operators, two (`lower bound` /
`upper bound`) for `between`. Choosing `literal`, `relative`, or `variable`
builds that operand node and wires it in, so an author never has to find a
palette entry and drag a wire to get the four controls.

**The mode is DERIVED from the graph, never stored.** Nothing about it is in the
AST: it is read back off whatever is wired, so a rule loaded from the server and
a rule built by hand show the same modes for the same shape, and an operand
rewired by hand updates the control rather than being overwritten by it. An
operand no mode names (a call, a list, another comparison) reads back as a blank
control, never a wrong one.

### `between`'s bounds, and what survives an operator change

`between`'s ternary shape — a two-item `list` on the right — is a detail of how
the AST stores bounds, not something an author should have to know, so **the
editor builds it**: dropping `is between` scaffolds the list with two empty
literal bounds, both immediately editable and each with its own mode. All four
literal/relative combinations are authorable because neither mode control
consults the other.

Operator changes follow four rules, all in `reshapeDateOperands`:

- **Compatible operands survive.** `is before` → `is after` touches nothing.
- **Entering `between` WRAPS** the current right operand as the lower bound;
  **leaving it UNWRAPS** the lower bound back onto `right`. A configured operand
  survives the round trip in the position that still means the same thing.
- **An incompatible operand is cleared.** A relative date is legal only under a
  date operator, so `is before` → `has all` removes it rather than leaving a node
  that saves as an error the canvas does not explain.
- **Only LEAVES are ever deleted** (a var, a literal, a relative date — one field
  the author can retype). A displaced subtree is disconnected and left on the
  canvas: the editor does not throw away structure someone built.

None of this runs for an **unknown** operator. The control fires on every
keystroke, so an author retyping `is before` over `is between` passes through a
run of half-words, and reacting to those would tear down the bounds and delete a
configured relative date on the way to an operator they are still spelling.

### Labelling the controls

The classic renderer draws every control as a bare `<input>` with no label. It
does tag each control's wrapper `data-testid="control-<key>"`, so `css/styles.css`
hangs the field name on a `::before` scoped to `.rete-canvas` (a CSS pseudo
element belongs to CSS, not to the element tree React reconciles — nothing is
injected into the renderer's DOM), and `rules.js` sets `placeholder` / `title` /
`step` through the same delegation that attaches the datalists. **Never build a
JS `Date` to display any of it** — stored date strings render verbatim, `Z`
included.

## Auth wiring (external credentials, never issued here)

Aperture consumes credentials; it never mints them. The shell carries a bearer on
every API request through one wrapper, `window.apiFetch` (`js/app.js`), which
adds `Authorization: Bearer <token>` and, on a `401`, clears the token and
re-opens the sign-in affordance. Sign-in / sign-out dispatch DOM events —
`aperture:authenticated` (`detail.principal`), `aperture:signout`, and
`aperture:unauthenticated` (on a 401) — so per-screen components (e.g.
`js/crud.js`) can (re)load or reset when the presented principal changes. For local/demo the **dev/static authenticator**
(`auth/dev.go`) treats the bearer AS the principal id, so "sign in" only names
which principal the session presents. An unauthenticated shell shows a sign-in
modal; there is no credential issuance UI. Later per-screen Alpine components
reuse `window.apiFetch` so the auth header lives in exactly one place.

## What-if preview (rule editor) — read-only, by design

The Rules section's object what-if samples a REAL object of a chosen type and
shows the metadata snapshot the rule actually saw, returned on the
`EvaluateRule` response as `object_json`. It is **strictly read-only**: the panel
has no metadata input and there is no request path by which the client supplies
metadata. Supplying fake metadata is a different feature with different trust
implications and is deliberately out of scope — the preview's value is that it
shows real provider data.

Rendering (`js/rules.js`: `metadataRows` / `pushMetadataRows` /
`formatMetadataScalar`, displayed from `previewRows`) follows the provider value
model, which is closed rather than arbitrary JSON:

- A field value is a **scalar**, an **array of scalars**, or an **object** whose
  members are scalars, scalar arrays, or one further object level. The depth cap
  is 2, and arrays of objects are rejected at load — so no array element is ever
  a container.
- Snapshots are flattened to one indented row per field, plus one row per member
  of an object-valued field. Scalars render in their JSON form, so a string keeps
  its quotes and the string `"42"` stays distinguishable from the number `42`.
- Array elements render as one chip each, so a long array wraps inside the panel;
  the panel scrolls in its own container and never widens the page.
- **An absent field has no row at all**, whereas an empty list renders `[]` and an
  empty object renders `{}`, each with a count note. That distinction is load
  bearing: an empty `:list` cell produces a real `[]`, while an empty scalar or
  `:json` cell omits the field entirely, and a rule author debugging an `in`
  comparison has to tell the two apart.
- A collapsed "Raw JSON" disclosure keeps the whole snapshot available verbatim.

A change to what the preview renders, or to its read-only guarantee, updates this
section in the same PR.

## compliance (load-bearing)

The shell obeys `.planning/access-control/research/design-system.md`:

- **Named tokens only** — `css/styles.css` inlines the spec's `:root` block; no raw
  hex in component rules.
- **Sentence case** everywhere; the one uppercase exception is form-field labels.
- **No emoji.** Not in copy, empty states, or confirmations.
- **AI-pink (`#ff3399`) is reserved for AI affordances only** — never a primary
  action, alert, or decoration. It appears only on `.a-ai-pill`.
- **IBM Plex Mono for identity strings / IDs** (`.a-numeric` / mono stack);
  IBM Plex Sans body; BwGradual display. Fonts are not hotlinked — they fall back
  to the system stack per spec §12, so the binary stays self-contained.
- **Lucide icons**, stroke-only 1.5px, inlined as an SVG sprite (no icon CDN).
- **Layout §8** — fixed ~232px sidebar + ~52px top bar + flex content.

## Update-Demand

A change to the embedded shell's public surface — the static routes it exposes
(`/`, `/css/*`, `/js/*`, `/vendor/*`), the `apiFetch` bearer convention, the nav
skeleton, the `rules-serializer.js` mirror of `rules/ast.go` (operators, operand
shapes, blocked calls, the date-operator set, the relative-date vocabularies and
node shape, validation wording), the rule editor's operator palette and its
spellings, its node controls (the relative-date quartet, the operand mode
switches, the `between` bounds scaffold, and the `data-testid` labels they are
styled through), or a rule above — updates this doc in the **same PR**. The gate
in `skills/skills_test.go` fails the build if this doc loses its frontmatter.
