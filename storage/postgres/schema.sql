-- Aperture Postgres schema. Hand-written, embedded, no ORM/migration tool.
-- Every statement is CREATE ... IF NOT EXISTS so Setup is idempotent.
--
-- This file is the Postgres peer of storage/sqlite/schema.sql. It carries the
-- SAME 14 tables, the SAME columns, the SAME 9 foreign-key edges with the same
-- actions, and the SAME 7 indexes. Where the two files differ, the difference is
-- a dialect requirement and is written down at the point it occurs. A backend
-- that is "mostly the same" is a backend that authorizes differently, so read
-- the divergences below before adding a fifteenth.
--
-- Applying this file (settled empirically, E4-S1):
--   * The whole file is executed as ONE ExecContext with NO bind arguments, the
--     same way sqlite.Setup does. It is not split into statements and it does
--     not need simple-protocol mode. That is not an assumption; see the "How
--     this file is applied" section of the package doc, which records the
--     measurements.
--   * The one thing that would break it: passing a BIND ARGUMENT alongside this
--     script. pgx routes an argument-free Exec through the simple query
--     protocol (which accepts multiple commands) and ANY argument through the
--     extended protocol (which does not, and rejects the whole script with
--     SQLSTATE 42601). That is why the schema qualifier below is a TEXTUAL
--     substitution and can never become a parameter.
--
-- The schema qualifier:
--   * Every table identifier in this file is written apt_schema.apt_<thing>.
--     `apt_schema.` is a PLACEHOLDER, not a schema Aperture creates: the loader
--     replaces that exact literal with the configured qualifier before
--     executing -- with "" to use whatever search_path resolves to (the
--     default, and the reason the apt_ table prefix exists at all), or with
--     a QUOTED "<name>." to pin the tables into a named schema. The qualifier
--     is built by storage/postgres/config.go from a validated schema name; a
--     schema name cannot be a bind parameter, so that validator and the
--     quoting beside it are the whole of what guards this seam.
--   * The seam is here from the first commit ON PURPOSE. This is a static
--     embedded file, so retrofitting a qualifier later means touching every
--     statement in it, and every statement in the statement set built on top.
--   * The substitution is a plain literal replacement over the whole file, so
--     it rewrites the placeholder where it appears in THESE COMMENTS too. That
--     is harmless -- comments are not executed -- and it is preferable to a
--     smarter substitution that would need to know where the comments are.
--   * A placeholder shaped like a SQL identifier, rather than a template
--     delimiter like {{schema}}, is deliberate. The house schema gate parses
--     the file with a real SQL tokenizer and already drops a schema qualifier
--     from an identifier (main.apt_grants reads as apt_grants); a brace would
--     be a parse error. It also means the raw, unsubstituted file fails LOUDLY
--     against a real server (SQLSTATE 3F000, schema "apt_schema" does not
--     exist) instead of quietly doing something else.
--   * INDEX NAMES are deliberately NOT qualified. Postgres forbids schema-
--     qualifying the name of a new index (measured: CREATE INDEX ns.idx ... is
--     a syntax error at the dot); the index is created in whatever schema its
--     table lives in, which was also measured. So the table in
--     `ON apt_schema.apt_grants` is qualified and the index name beside it is
--     bare -- an asymmetry that looks like a typo and is not.
--
-- Naming (identical to SQLite -- one convention, both backends):
--   * Every table Aperture owns is prefixed apt_ (apt_accounts, apt_grants,
--     apt_audit_log, ...), so no table name can collide with a SQL reserved
--     word and Aperture's tables are recognizable in a shared database. In
--     Postgres that prefix does double duty: it is also what makes an
--     unqualified search_path deployment safe, so no dedicated schema is
--     required to avoid colliding with the host's tables.
--   * A column is prefixed apt_ only when its whole name is a reserved word,
--     counting the singular of a plural (grants -> GRANT): apt_action,
--     apt_actions, apt_identity, apt_grants, apt_object. Compound names
--     (account_id, object_type, ...) never qualify and are left alone.
--   * Index names keep the leading idx_ and track the table:
--     idx_apt_grants_account_subject.
--   * This is a database-identifier convention only. Go types, struct fields,
--     wire field names, and CLI flags are unaffected.
--
-- Time:
--   * EVERY instant in this schema -- created_at, updated_at, and the audit
--     log's occurred_at alike -- is a BIGINT count of NANOSECONDS since the
--     Unix epoch (1970-01-01T00:00:00Z), in UTC. There is no timestamptz and no
--     per-dialect timestamp type anywhere in Aperture's storage: SQLite spells
--     this column INTEGER and Postgres spells it BIGINT, and both carry the
--     identical int64.
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
--     mis-sorts variable-length fractional seconds.
--
-- Integer widths (DIVERGENCE 1, and the only column-type divergence):
--   * SQLite's INTEGER is a 64-bit signed integer, whatever the column holds.
--     Postgres INTEGER is 32-bit. So EVERY column SQLite spells INTEGER is
--     spelled BIGINT here -- the timestamps, and also seq, version and
--     delegatable -- rather than only the timestamps. The mapping is uniform on
--     purpose: a column that silently narrowed its domain relative to the
--     reference backend would be a behavioural difference between two backends
--     that storagetest asserts are identical, and a uniform rule is one an
--     automated parity gate can check, while a per-column judgement call is not.
--   * delegatable stays a 0/1 integer rather than becoming BOOLEAN for the same
--     reason: BOOLEAN would make the Go scan target differ per backend, which
--     is precisely the kind of per-dialect carve-out storagetest forbids.
--
-- Table order (DIVERGENCE 2):
--   * SQLite resolves a REFERENCES clause lazily, so its schema can declare
--     apt_memberships before apt_principals. Postgres resolves it at CREATE
--     time: measured, a forward reference fails with SQLSTATE 42P01, relation
--     "..." does not exist. Tables are therefore ordered PARENTS FIRST here.
--     The alternative -- declaring the keys afterwards with ALTER TABLE ADD
--     CONSTRAINT -- was rejected because Postgres has no IF NOT EXISTS for it
--     (measured: a syntax error, not a no-op), which would cost the whole file
--     its idempotency.
--   * That is an ordering difference only. The set of tables, columns, keys and
--     indexes is identical; nothing about the logical shape depends on it.
--
-- Primary-key nullability (DIVERGENCE 3, and it costs nothing):
--   * SQLite lets a non-INTEGER PRIMARY KEY column hold NULL -- a long-standing
--     quirk it keeps for backwards compatibility -- so the nine single-column
--     TEXT primary keys read back as NULLABLE there and as NOT NULL here.
--     Postgres is simply stricter, and nothing in Aperture relies on the
--     looseness: every id is a Go string, validation refuses an empty one, and
--     no statement in either backend writes a NULL into a key column. It is
--     recorded rather than fixed because "fix" would mean editing the SQLite
--     schema for a value neither backend can produce.
--
-- Referential integrity:
--   * Nine relationship columns carry a REAL foreign key, declared as a table
--     constraint next to the PRIMARY KEY, so a row can never name a parent that
--     does not exist and a parent can never be deleted out from under a child
--     that still points at it. Three columns deliberately carry none, each for a
--     reason written down below: apt_grants.subject_id (polymorphic) and the two
--     account_id columns (they carry a reserved sentinel). Those three are
--     enforced in the application layer instead, on identical terms and in every
--     backend -- see storage/sqlite/integrity.go, whose refusal messages this
--     backend must reproduce verbatim. "No foreign key" does not mean "no
--     integrity"; it means SQL could not express this one.
--   * ON DELETE CASCADE appears on exactly THREE edges -- the ones where an
--     entity owns its own join rows and the Go code already cascades by hand in
--     a transaction: apt_principal_roles.principal_id, apt_role_permissions.
--     role_id, apt_group_members.group_id. There the join row has no meaning
--     without its owner, so deleting the owner deletes it.
--   * ON DELETE RESTRICT everywhere else (the other six edges). Deleting a
--     permission a grant still cites, a role a principal still holds, or a
--     principal that is still in a group is refused with
--     APERTURE_STORAGE_CONSTRAINT. That refusal is the point: before these keys
--     existed the same delete silently orphaned the child rows.
--   * ON UPDATE RESTRICT throughout, without exception. An id in Aperture is
--     immutable -- nothing in the model renames one -- so an UPDATE that moved a
--     parent key would be a bug, and RESTRICT makes it a loud one rather than a
--     quiet re-parenting.
--   * The constraints are left UNNAMED, as in SQLite. Postgres derives a
--     deterministic, self-describing name (apt_memberships_principal_id_fkey),
--     which is what a 23503 error will carry; naming them by hand would add a
--     second registry to keep in step with this file and there is no gate over
--     it.
--   * Every key is immediate, not DEFERRABLE. A deferred key would move the
--     refusal from the statement to the COMMIT, which would change which
--     operation storagetest sees fail -- a behavioural difference between
--     backends, not a tuning knob.
--   * Unlike SQLite there is NOTHING to switch on: Postgres enforces foreign
--     keys unconditionally, with no per-connection pragma. The Open-forces-
--     plus-Setup-verifies dance in sqlite.go has no analogue here and must not
--     be ported.
--   * JSON value columns take no foreign keys: apt_object_types.apt_actions and
--     apt_templates.apt_grants are value lists, not relationships. They are
--     also plain TEXT rather than json/jsonb -- Aperture stores the rules and
--     template packages' own canonical serialization VERBATIM so a round-trip is
--     byte-stable, and jsonb normalizes key order and whitespace, which would
--     silently rewrite it.
--
-- The two account_id columns, and why they carry no foreign key:
--   * apt_memberships.account_id and apt_grants.account_id look like plain
--     references to apt_accounts(id), and they were planned as two further
--     edges. They CANNOT be, and the reason is in the model, not here.
--   * model.AccountWildcard is the reserved account id "*". A grant stamped "*"
--     applies in EVERY account (model/model.go), and a MEMBERSHIP stamped "*"
--     enrolls its principal in every account (engine.requireMembership, which
--     falls back to IsMember(principal, "*") before denying). Both are shipped,
--     documented features -- the cross-account super-admin depends on them.
--   * And "*" is deliberately NOT an account row: ValidateAccount REFUSES it, so
--     that no Account can ever shadow the wildcard (model/validate.go). So the
--     parent these two columns would reference does not exist and may not be
--     created.
--   * A foreign key there would therefore reject every wildcard grant and every
--     wildcard membership at the INSERT. SQL has no partial or conditional
--     foreign key -- Postgres has none either, which is why this is not a SQLite
--     gap that a "real" database fixes -- and the only shapes that would work
--     are model changes: NULL-encode the wildcard (changing what every
--     account-scoped query binds, including the hot-path index below), or mint a
--     real "*" account row (which the model explicitly forbids). Neither is a
--     schema decision.
--   * So account_id joins (subject_kind, subject_id) as a column checked in Go
--     rather than by a constraint. The rule is "an apt_accounts row OR exactly
--     model.AccountWildcard", enforced in BOTH directions and in every backend
--     -- a write naming an account that does not exist is refused, and so is
--     deleting an account a membership or a grant is still stamped with, which
--     is the ON DELETE RESTRICT these two columns could not declare. A naive
--     existence check there refuses "*" and breaks both shipped features.
--   * Because a DELETE-then-INSERT upsert removes the conflicting row before
--     inserting the new one -- firing ON DELETE actions as it goes, which once
--     wiped a group's members on re-save -- no parent table is written that way.
--     Every entity upsert is an ON CONFLICT DO UPDATE, which mutates the row in
--     place and leaves the children alone. That holds here too.
--
-- Concurrency (for the loader, not for this file):
--   * CREATE ... IF NOT EXISTS is NOT race-free in Postgres: two sessions can
--     both pass the existence check and the loser fails on a pg_class unique
--     violation (42P07 / 23505). Two `aperture serve` instances booting against
--     one database is an ordinary deployment. Setup therefore takes a
--     transaction-scoped advisory lock around this script -- E4-S2's job, noted
--     here because the hazard belongs to the schema, not to the code.
--
-- Design notes:
--   * Tables are explicit and extensible: timestamps live on every entity;
--     membership is normalized into join tables.
--   * Object-type action verb sets are stored as a JSON text column
--     (apt_object_types.apt_actions -- a value list, not a relationship);
--     membership edges are real join tables.
--   * apt_grants rows carry account_id (the cross-account isolation stamp) and
--     are indexed by (account_id, subject_kind, subject_id) for the decision
--     engine's hot-path GrantsForSubjects query. That index's column list and
--     order are load-bearing and match SQLite exactly.

-- ---------------------------------------------------------------------------
-- Root tables: no outbound foreign key. Declared first so the children below
-- can reference them (see DIVERGENCE 2 in the header).
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS apt_schema.apt_accounts (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    created_at  BIGINT NOT NULL DEFAULT 0,
    updated_at  BIGINT NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS apt_schema.apt_principals (
    id           TEXT PRIMARY KEY,
    kind         TEXT NOT NULL,
    apt_identity TEXT NOT NULL,
    display_name TEXT NOT NULL DEFAULT '',
    created_at   BIGINT NOT NULL DEFAULT 0,
    updated_at   BIGINT NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS apt_schema.apt_object_types (
    name        TEXT PRIMARY KEY,
    apt_actions TEXT NOT NULL,          -- JSON array of verb strings
    description TEXT NOT NULL DEFAULT '',
    created_at  BIGINT NOT NULL DEFAULT 0,
    updated_at  BIGINT NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS apt_schema.apt_roles (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    created_at  BIGINT NOT NULL DEFAULT 0,
    updated_at  BIGINT NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS apt_schema.apt_groups (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    created_at  BIGINT NOT NULL DEFAULT 0,
    updated_at  BIGINT NOT NULL DEFAULT 0
);

-- ---------------------------------------------------------------------------
-- Referencing tables.
-- ---------------------------------------------------------------------------

-- apt_memberships rows are edges keyed by the (principal_id, account_id) pair:
-- a principal is a member of an account at most once. Indexed both ways so
-- "accounts for a principal" and "members of an account" are both cheap.
CREATE TABLE IF NOT EXISTS apt_schema.apt_memberships (
    principal_id TEXT NOT NULL,
    account_id   TEXT NOT NULL,
    created_at   BIGINT NOT NULL DEFAULT 0,
    updated_at   BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (principal_id, account_id),
    -- RESTRICT: a principal outlives the membership, so it may not be deleted
    -- while the edge stands.
    --
    -- account_id carries NO foreign key -- see "The two account_id columns" in
    -- the header. A membership stamped "*" enrolls a principal in EVERY account
    -- (engine.requireMembership), and "*" is not an apt_accounts row.
    FOREIGN KEY (principal_id) REFERENCES apt_schema.apt_principals (id) ON DELETE RESTRICT ON UPDATE RESTRICT
);

CREATE INDEX IF NOT EXISTS idx_apt_memberships_account
    ON apt_schema.apt_memberships (account_id);

CREATE TABLE IF NOT EXISTS apt_schema.apt_permissions (
    id             TEXT PRIMARY KEY,
    object_type    TEXT NOT NULL,
    apt_action     TEXT NOT NULL,
    -- scope_strategy carries NO foreign key, and could not. It is not an id at
    -- all: it is an opaque scope reference in the scope package's own small
    -- grammar -- "", "literal", "inclusive;ids=a,b", "inclusive;rule=quarantine"
    -- (scope.ParseSpec). The strategy key resolves against an in-process
    -- registry of resolvers, not a table, and hosts register their own. The rule
    -- name that MAY appear inside one is a parameter buried in the string, not
    -- the column's value. There is nothing for a key to point at.
    scope_strategy TEXT NOT NULL DEFAULT '',
    delegatable    BIGINT NOT NULL DEFAULT 0,  -- 0/1: may this permission be bestowed
    description    TEXT NOT NULL DEFAULT '',
    created_at     BIGINT NOT NULL DEFAULT 0,
    updated_at     BIGINT NOT NULL DEFAULT 0,
    -- The object type is what makes a permission's action verb legal, so a
    -- permission cannot outlive it: dropping the type would leave the
    -- permission's typed-action validation with nothing to check against.
    FOREIGN KEY (object_type) REFERENCES apt_schema.apt_object_types (name) ON DELETE RESTRICT ON UPDATE RESTRICT
);

CREATE TABLE IF NOT EXISTS apt_schema.apt_principal_roles (
    principal_id TEXT NOT NULL,
    role_id      TEXT NOT NULL,
    seq          BIGINT NOT NULL,        -- preserves caller-supplied order
    PRIMARY KEY (principal_id, role_id),
    -- CASCADE on the OWNER (the principal owns its own role list; DeletePrincipal
    -- already clears these rows by hand inside its transaction), RESTRICT on the
    -- role, which is a shared entity a principal merely points at.
    FOREIGN KEY (principal_id) REFERENCES apt_schema.apt_principals (id) ON DELETE CASCADE ON UPDATE RESTRICT,
    FOREIGN KEY (role_id) REFERENCES apt_schema.apt_roles (id) ON DELETE RESTRICT ON UPDATE RESTRICT
);

CREATE TABLE IF NOT EXISTS apt_schema.apt_role_permissions (
    role_id       TEXT NOT NULL,
    permission_id TEXT NOT NULL,
    seq           BIGINT NOT NULL,
    PRIMARY KEY (role_id, permission_id),
    -- CASCADE on the OWNER (DeleteRole already clears these rows by hand),
    -- RESTRICT on the permission, which many roles may cite.
    FOREIGN KEY (role_id) REFERENCES apt_schema.apt_roles (id) ON DELETE CASCADE ON UPDATE RESTRICT,
    FOREIGN KEY (permission_id) REFERENCES apt_schema.apt_permissions (id) ON DELETE RESTRICT ON UPDATE RESTRICT
);

CREATE TABLE IF NOT EXISTS apt_schema.apt_group_members (
    group_id     TEXT NOT NULL,
    principal_id TEXT NOT NULL,
    seq          BIGINT NOT NULL,
    PRIMARY KEY (group_id, principal_id),
    -- CASCADE on the OWNER (DeleteGroup already clears these rows by hand),
    -- RESTRICT on the principal, which exists independently of any group.
    FOREIGN KEY (group_id) REFERENCES apt_schema.apt_groups (id) ON DELETE CASCADE ON UPDATE RESTRICT,
    FOREIGN KEY (principal_id) REFERENCES apt_schema.apt_principals (id) ON DELETE RESTRICT ON UPDATE RESTRICT
);

CREATE INDEX IF NOT EXISTS idx_apt_group_members_principal
    ON apt_schema.apt_group_members (principal_id);

CREATE TABLE IF NOT EXISTS apt_schema.apt_grants (
    id            TEXT PRIMARY KEY,
    account_id    TEXT NOT NULL,
    subject_kind  TEXT NOT NULL,
    subject_id    TEXT NOT NULL,
    permission_id TEXT NOT NULL,
    apt_object    TEXT NOT NULL,         -- identity pattern, string form
    effect        TEXT NOT NULL,
    created_at    BIGINT NOT NULL DEFAULT 0,
    updated_at    BIGINT NOT NULL DEFAULT 0,
    -- RESTRICT: a grant is authority, and authority citing a permission that has
    -- been deleted is authority nobody can read or revoke.
    --
    -- Note the TWO columns that carry no foreign key. (subject_kind, subject_id)
    -- is POLYMORPHIC -- it points at a principal, a role, or a group depending
    -- on the kind -- and no single-table foreign key can express that, so it is
    -- checked in Go instead, in both directions and in every backend: a grant
    -- may not name a subject that does not exist in the table its kind selects,
    -- and that subject may not be deleted while the grant names it. account_id
    -- carries the "*" sentinel and is checked the same way; see "The two
    -- account_id columns" in the header.
    --
    -- The index below is why neither becomes a schema change: nullable columns
    -- plus a CHECK would break idx_apt_grants_account_subject, the engine's
    -- hot-path index for GrantsForSubjects.
    FOREIGN KEY (permission_id) REFERENCES apt_schema.apt_permissions (id) ON DELETE RESTRICT ON UPDATE RESTRICT
);

CREATE INDEX IF NOT EXISTS idx_apt_grants_account_subject
    ON apt_schema.apt_grants (account_id, subject_kind, subject_id);

-- ---------------------------------------------------------------------------
-- Standalone tables: no inbound and no outbound foreign key.
-- ---------------------------------------------------------------------------

-- Provisioning templates: named, versioned bundles of parameterized grants.
-- Identity is the (name, version) pair so multiple versions of a name coexist;
-- apply selects the latest by default. The typed parameter declarations and the
-- parameterized grant templates ride as JSON value columns (a value list, not a
-- relationship), mirroring how object-type verb sets are stored -- so
-- apt_templates.apt_grants takes NO foreign key to apt_grants despite the name.
CREATE TABLE IF NOT EXISTS apt_schema.apt_templates (
    name        TEXT NOT NULL,
    version     BIGINT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    params      TEXT NOT NULL DEFAULT '[]',   -- JSON array of {Name,Type,Description}
    apt_grants  TEXT NOT NULL DEFAULT '[]',   -- JSON array of template grants
    created_at  BIGINT NOT NULL DEFAULT 0,
    updated_at  BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (name, version)
);

-- Named rules: the persisted home for the rule AST a scope strategy resolves.
-- The AST rides as a JSON text column carrying the rules package's canonical
-- serialization verbatim (a rules.Node), so a round-trip is byte-stable and the
-- editor's format is preserved exactly -- which is also why the column is TEXT
-- and not jsonb. Identity is the rule name; PutRule upserts on it.
CREATE TABLE IF NOT EXISTS apt_schema.apt_rules (
    name        TEXT PRIMARY KEY,
    description TEXT NOT NULL DEFAULT '',
    ast         TEXT NOT NULL,                -- rules.Node canonical JSON
    created_at  BIGINT NOT NULL DEFAULT 0,
    updated_at  BIGINT NOT NULL DEFAULT 0
);

-- Append-only audit trail. Writes are inserts only; deletes happen exclusively
-- through bulk retention pruning. The event's instant is occurred_at (a bare
-- compound name -- the apt_ prefix is only for columns whose WHOLE name is a
-- reserved word), carrying the same integer nanoseconds as every other timestamp
-- in this schema, which is what lets range filters and newest-first ordering
-- compare numerically. Unlike the entity tables it has no DEFAULT: an audit
-- record with no instant is not a record.
-- A record made under impersonation carries both the real actor (actor) and the
-- borrowed target (effective_subject + impersonation_mode). The details column
-- is an optional JSON blob for event-specific context.
--
-- apt_audit_log carries NO FOREIGN KEYS AT ALL, and that is the point rather
-- than an omission. actor, effective_subject, account and target name entities
-- an audit record must OUTLIVE: the trail's whole purpose is to record what was
-- done to a principal or an account that has since been deleted. A key on any
-- of them would either refuse the delete (RESTRICT, making the trail a reason
-- you cannot remove a user) or erase the evidence (CASCADE). Both defeat an
-- append-only trail, so the columns hold plain identifier strings.
CREATE TABLE IF NOT EXISTS apt_schema.apt_audit_log (
    id                 TEXT PRIMARY KEY,
    occurred_at        BIGINT NOT NULL,
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

CREATE INDEX IF NOT EXISTS idx_apt_audit_occurred_at ON apt_schema.apt_audit_log (occurred_at);
CREATE INDEX IF NOT EXISTS idx_apt_audit_actor ON apt_schema.apt_audit_log (actor);
CREATE INDEX IF NOT EXISTS idx_apt_audit_account ON apt_schema.apt_audit_log (account);
CREATE INDEX IF NOT EXISTS idx_apt_audit_event_type ON apt_schema.apt_audit_log (event_type);
