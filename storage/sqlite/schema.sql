-- Aperture SQLite schema. Hand-written, embedded, no ORM/migration tool.
-- Every statement is CREATE ... IF NOT EXISTS so Setup is idempotent.
--
-- Naming:
--   * Every table Aperture owns is prefixed apt_ (apt_accounts, apt_grants,
--     apt_audit_log, ...), so no table name can collide with a SQL reserved
--     word and Aperture's tables are recognizable in a shared database.
--   * A column is prefixed apt_ only when its whole name is a reserved word,
--     counting the singular of a plural (grants -> GRANT): apt_action,
--     apt_actions, apt_identity, apt_grants, apt_object. Compound names
--     (account_id, object_type, ...) never qualify and are left alone.
--   * Index names keep the leading idx_ and track the renamed table:
--     idx_apt_grants_account_subject.
--   * This is a database-identifier convention only. Go types, struct fields,
--     wire field names, and CLI flags are unaffected.
--
-- Time:
--   * EVERY instant in this schema -- created_at, updated_at, and the audit
--     log's occurred_at alike -- is an INTEGER count of NANOSECONDS since the
--     Unix epoch (1970-01-01T00:00:00Z), in UTC. There is no text timestamp
--     and no per-dialect timestamp type anywhere in Aperture's storage.
--   * The representable window is the signed 64-bit nanosecond range:
--     1677-09-21T00:12:43.145224192Z .. 2262-04-11T23:47:16.854775807Z.
--     An instant outside it is refused with APERTURE_INVALID_INPUT before the
--     write; it is never wrapped, clamped, or stored as an overflow value.
--   * 0 means UNSET, not the Unix epoch. That is what lets every timestamp
--     column stay NOT NULL DEFAULT 0 and still read back as the zero time.
--     Aperture stamps from a real clock and never writes the epoch itself, so
--     nothing in the model can collide with the sentinel.
--   * storage/storagetime owns both mappings (time.Time{} <-> 0 and the range
--     check) and is the ONLY place in the storage layer where a time.Time
--     becomes an integer; storage/storagetime/exclusivity_test.go enforces it.
--   * Integers rather than text because comparison is the point: range filters
--     and newest-first ordering compare numerically, while RFC3339 text
--     mis-sorts variable-length fractional seconds. The Postgres column type
--     is BIGINT, carrying the same unit, for the same reason.
--
-- Design notes:
--   * Tables are explicit and extensible: timestamps live on every entity for
--     forward-compatibility with audit (E4-S2); membership is normalized into
--     join tables so a future Postgres port maps over cleanly.
--   * Object-type action verb sets are stored as a JSON text column
--     (apt_object_types.apt_actions -- a value list, not a relationship);
--     membership edges are real join tables.
--   * apt_grants rows carry account_id (the cross-account isolation stamp) and
--     are indexed by (account_id, subject_kind, subject_id) for the decision
--     engine's hot-path GrantsForSubjects query.

CREATE TABLE IF NOT EXISTS apt_accounts (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL DEFAULT 0,
    updated_at  INTEGER NOT NULL DEFAULT 0
);

-- apt_memberships rows are edges keyed by the (principal_id, account_id) pair:
-- a principal is a member of an account at most once. Indexed both ways so
-- "accounts for a principal" and "members of an account" are both cheap.
CREATE TABLE IF NOT EXISTS apt_memberships (
    principal_id TEXT NOT NULL,
    account_id   TEXT NOT NULL,
    created_at   INTEGER NOT NULL DEFAULT 0,
    updated_at   INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (principal_id, account_id)
);

CREATE INDEX IF NOT EXISTS idx_apt_memberships_account
    ON apt_memberships (account_id);

CREATE TABLE IF NOT EXISTS apt_object_types (
    name        TEXT PRIMARY KEY,
    apt_actions TEXT NOT NULL,          -- JSON array of verb strings
    description TEXT NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL DEFAULT 0,
    updated_at  INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS apt_permissions (
    id             TEXT PRIMARY KEY,
    object_type    TEXT NOT NULL,
    apt_action     TEXT NOT NULL,
    scope_strategy TEXT NOT NULL DEFAULT '',
    delegatable    INTEGER NOT NULL DEFAULT 0,  -- 0/1: may this permission be bestowed (E3-S2)
    description    TEXT NOT NULL DEFAULT '',
    created_at     INTEGER NOT NULL DEFAULT 0,
    updated_at     INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS apt_principals (
    id           TEXT PRIMARY KEY,
    kind         TEXT NOT NULL,
    apt_identity TEXT NOT NULL,
    display_name TEXT NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL DEFAULT 0,
    updated_at   INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS apt_principal_roles (
    principal_id TEXT NOT NULL,
    role_id      TEXT NOT NULL,
    seq          INTEGER NOT NULL,       -- preserves caller-supplied order
    PRIMARY KEY (principal_id, role_id)
);

CREATE TABLE IF NOT EXISTS apt_roles (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL DEFAULT 0,
    updated_at  INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS apt_role_permissions (
    role_id       TEXT NOT NULL,
    permission_id TEXT NOT NULL,
    seq           INTEGER NOT NULL,
    PRIMARY KEY (role_id, permission_id)
);

CREATE TABLE IF NOT EXISTS apt_groups (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL DEFAULT 0,
    updated_at  INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS apt_group_members (
    group_id     TEXT NOT NULL,
    principal_id TEXT NOT NULL,
    seq          INTEGER NOT NULL,
    PRIMARY KEY (group_id, principal_id)
);

CREATE INDEX IF NOT EXISTS idx_apt_group_members_principal
    ON apt_group_members (principal_id);

CREATE TABLE IF NOT EXISTS apt_grants (
    id            TEXT PRIMARY KEY,
    account_id    TEXT NOT NULL,
    subject_kind  TEXT NOT NULL,
    subject_id    TEXT NOT NULL,
    permission_id TEXT NOT NULL,
    apt_object    TEXT NOT NULL,         -- identity pattern, string form
    effect        TEXT NOT NULL,
    created_at    INTEGER NOT NULL DEFAULT 0,
    updated_at    INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_apt_grants_account_subject
    ON apt_grants (account_id, subject_kind, subject_id);

-- Provisioning templates (E5-S1, FR-18/FR-19): named, versioned bundles of
-- parameterized grants. Identity is the (name, version) pair so multiple
-- versions of a name coexist; apply selects the latest by default. The typed
-- parameter declarations and the parameterized grant templates ride as JSON
-- value columns (a value list, not a relationship), mirroring how object-type
-- verb sets are stored.
CREATE TABLE IF NOT EXISTS apt_templates (
    name        TEXT NOT NULL,
    version     INTEGER NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    params      TEXT NOT NULL DEFAULT '[]',   -- JSON array of {Name,Type,Description}
    apt_grants  TEXT NOT NULL DEFAULT '[]',   -- JSON array of template grants
    created_at  INTEGER NOT NULL DEFAULT 0,
    updated_at  INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (name, version)
);

-- Named rules (E5-S2): the persisted home for the rule AST a scope strategy
-- resolves. The AST rides as a JSON text column carrying the rules package's
-- canonical serialization verbatim (a rules.Node), so a round-trip is
-- byte-stable and the editor's format is preserved exactly. Identity is the
-- rule name; PutRule upserts on it.
CREATE TABLE IF NOT EXISTS apt_rules (
    name        TEXT PRIMARY KEY,
    description TEXT NOT NULL DEFAULT '',
    ast         TEXT NOT NULL,                -- rules.Node canonical JSON
    created_at  INTEGER NOT NULL DEFAULT 0,
    updated_at  INTEGER NOT NULL DEFAULT 0
);

-- Append-only audit trail (E4-S2, FR-25). Writes are inserts only; deletes
-- happen exclusively through bulk retention pruning. The event's instant is
-- occurred_at (a bare compound name -- the apt_ prefix is only for columns whose
-- WHOLE name is a reserved word), carrying the same integer nanoseconds as every
-- other timestamp in this schema, which is what lets range filters and
-- newest-first ordering compare numerically. Unlike the entity tables it has no
-- DEFAULT: an audit record with no instant is not a record.
-- A record made under impersonation carries both the real actor (actor) and the
-- borrowed target (effective_subject + impersonation_mode). The details column
-- is an optional JSON blob for event-specific context.
CREATE TABLE IF NOT EXISTS apt_audit_log (
    id                 TEXT PRIMARY KEY,
    occurred_at        INTEGER NOT NULL,
    event_type         TEXT NOT NULL,
    apt_action         TEXT NOT NULL DEFAULT '',
    actor              TEXT NOT NULL DEFAULT '',
    effective_subject  TEXT NOT NULL DEFAULT '',
    impersonation_mode TEXT NOT NULL DEFAULT '',
    account            TEXT NOT NULL DEFAULT '',
    target             TEXT NOT NULL DEFAULT '',
    outcome            TEXT NOT NULL DEFAULT '',
    reason             TEXT NOT NULL DEFAULT '',
    details            TEXT NOT NULL DEFAULT ''     -- JSON object, '' when none
);

CREATE INDEX IF NOT EXISTS idx_apt_audit_occurred_at ON apt_audit_log (occurred_at);
CREATE INDEX IF NOT EXISTS idx_apt_audit_actor ON apt_audit_log (actor);
CREATE INDEX IF NOT EXISTS idx_apt_audit_account ON apt_audit_log (account);
CREATE INDEX IF NOT EXISTS idx_apt_audit_event_type ON apt_audit_log (event_type);
