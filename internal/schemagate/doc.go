// Package schemagate holds the gates that keep Aperture's dialect schemas
// honest, for EVERY dialect schema the project ships rather than for one of
// them. There are two, built on one parser:
//
//   - the NAMING gate (schema_naming_test.go), which asks whether each dialect
//     obeys Aperture's database-identifier convention on its own; and
//   - the PARITY gate (schema_parity_test.go), which asks whether the dialects
//     still describe the same database — the same tables, the same columns, and
//     the same foreign-key edges with the same ON DELETE / ON UPDATE actions.
//
// The gates, the rules, and the small SQL parser they are built on live in
// this package's _test.go files — they are test support and nothing in the
// shipping binary calls them. This file exists so the package has a home for
// the prose, and so `go build ./...` has something to build.
//
// # The convention
//
//  1. EVERY table Aperture owns is named apt_<thing>. Blanket, no exceptions:
//     apt_accounts, apt_grants, apt_audit_log. A prefixed name can never collide
//     with a SQL keyword, and in a database Aperture shares with its host it is
//     immediately obvious which tables are ours.
//
//  2. A COLUMN is prefixed apt_ only when its WHOLE name spells a reserved word,
//     counting the singular of a plural: apt_action, apt_actions, apt_identity,
//     apt_grants, apt_object. A compound name (account_id, object_type, role_id)
//     can never be a keyword, so it is left alone. Prefixing more than the rule
//     requires is noise, not safety.
//
//  3. Index names keep their idx_ prefix and track the renamed table:
//     idx_apt_grants_account_subject.
//
// This is a DATABASE-IDENTIFIER convention only. Go types, struct fields, JSON
// and wire field names, and CLI flags are untouched — a `grants` local variable
// and an `object` parameter in sqlite.go are not this gate's business.
//
// # Why it exists
//
// Aperture writes hand-written SQL against its stores, and has no ORM, no query
// builder and no migration tool to quote identifiers for it. A column named
// `action` or a table named `grants` is a latent parse error: it happens to work
// in SQLite today, breaks the moment a statement is ported to another engine —
// which is no longer hypothetical, because the Postgres backend IS that port —
// and forces every future statement that touches it to remember the quotes. The
// alternative fix, quoting identifiers everywhere, is the one this project
// rejected: it makes every statement noisier to defend one word.
//
// The rename was a HARD BREAK. There is no schema versioning here (no
// user_version pragma, no migration table), so an old database does not upgrade.
// That price was paid once. Re-introducing a reserved word would mean paying it
// again, which is exactly what this gate prevents.
//
// # What the gate actually does
//
// It PARSES each dialect's schema.sql — tokenizer, statement splitter, CREATE
// TABLE reader — rather than pattern-matching the text, following the house
// precedent set by TestDriverValueMappingTableMatchesTheTypeSwitch
// (sqlprovider/values_test.go), which reads Go source with go/ast instead of
// grepping. A text scan is not merely inelegant here, it is WRONG: each
// schema.sql's own header comment quotes the old names to explain the
// convention, string defaults can contain anything, and `apt_grants` is both a
// table name and a column name on apt_templates — a scan cannot tell those two
// positions apart. The parser can, and does.
//
// The parser also reads REFERENCES / ON DELETE / ON UPDATE clauses (both the
// table-constraint and the inline column forms) and the Postgres file's
// `apt_schema.` qualifier, so a qualified table name still reads as apt_<thing>
// and a foreign key's target is a table name rather than a word the parser had
// to step over blindly.
//
// Reserved-word membership comes from internal/sqlreserved, a vendored snapshot
// of ten keyword lists carrying per-word provenance, so a failure can name the
// lists that object rather than only asserting that some list does.
//
// A missing or unparseable schema file FAILS; it never skips, matching
// rules/editor_js_contract_test.go. A gate that quietly skips is worse than no
// gate, because it also tells you it ran. That holds for every registered
// dialect, and a dialect schema on disk that is NOT registered fails too — a
// gate that governs whichever files somebody remembered to list is a gate with a
// hole in it.
//
// # What the parity gate adds
//
// Two hand-written schema files with nothing forcing them to agree is the drift
// this repo legislates against everywhere else (TestEditorOperatorTablesAgree,
// TestDriverValueMappingTableMatchesTheTypeSwitch). The parity gate diffs the
// parsed dialects against each other, symmetrically — there is no reference
// dialect whose spelling the others must match — so adding a table, a column or
// an edge to one file and not the other is build-red.
//
// It is the standing mitigation for an accepted risk: CI runs no containers, so
// nothing in `make test` proves the Postgres backend BEHAVES. The parity gate
// cannot prove that either. What it proves is that Postgres has not silently
// fallen BEHIND its twin.
//
// The dialects legitimately differ, and the gate refuses to express that as a
// blanket exemption — "types don't count" would also stop it noticing a
// created_at that had become TEXT in one backend. Instead each dialect declares
// an explicit map from a physical type SPELLING to the logical type it means
// (SQLite INTEGER and Postgres BIGINT both mean int64), and a spelling the map
// does not know is a failure rather than a pass. Declaration ORDER is not a fact
// the gate collects, because Postgres resolves REFERENCES at CREATE time and
// must declare parents first.
//
// # Where the embed check lives
//
// This package audits the files on DISK. Each backend separately proves that the
// copy it //go:embeds is byte-identical to the file on disk
// (storage/sqlite/schema_embed_test.go, storage/postgres/postgres_test.go), so
// the chain "the gate read the file the Store executes" is complete without this
// package importing either backend.
//
// # If this gate fires on a name you believe is legitimate
//
// Weaken nothing. Add a narrow, named, justified exception with the reasoning
// written next to it — that decision was made when this gate was designed, and a
// reviewer is entitled to see the argument rather than a relaxed rule. Note that
// a word reserved ONLY by "SQL Server future keywords" is reserved by no
// shipping engine today; that is the weakest case for a rename and the most
// likely candidate for an exception. Every other source is a live constraint.
package schemagate
