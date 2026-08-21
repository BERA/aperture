package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/frankbardon/aperture/model"
)

// The measured half of the COLLATE "C" defence. Same gate as the other live
// tests — see gate_test.go.
//
// Until this story the collation argument was reasoned and untested: E4-S3 wrote
// COLLATE "C" onto every text ORDER BY because glibc's en_US.UTF-8 weights
// punctuation below alphanumerics, and recorded that the hazard "is not
// reproducible on every server — macOS's libc and ICU both order these two ids
// the way C does". That is true of the DEFAULT collation on macOS, and it is why
// nobody could demonstrate the bug the defence prevents.
//
// It is reproducible anyway, on any server, because the hazard is not really
// about the operating system: it is about a COLUMN whose collation is linguistic
// rather than byte-wise. PostgreSQL 17+ ships the ICU root collation as
// pg_catalog."unicode" on every build, so this test creates the hazardous
// condition deliberately — Setup runs, then every TEXT column in the scratch
// schema is retyped to COLLATE "unicode", exactly the situation a glibc cluster
// hands Aperture by default — and then asserts the two things that matter:
//
//  1. the hazard is REAL on this server (an unpinned ORDER BY over those columns
//     really does come back in a different order), and
//  2. Aperture's own list methods return byte order regardless.
//
// (1) is what stops the test being vacuous, and it is what makes this a
// regression detector rather than a tautology: strip COLLATE "C" from a
// statement and the matching case below fails HERE, on a developer's laptop,
// instead of on a customer's Linux cluster.

// collationHazardIDs are ids whose ICU order and byte order disagree in two
// independent ways: '_' (0x5F) sorts after '-' and '1' byte-wise but ICU
// ignores punctuation weight on the first pass, and 'A' sorts before 'a'
// byte-wise while ICU orders lower case first. Measured against PostgreSQL 18.4:
//
//	byte order: g-star, g1, gA, g_x, ga
//	ICU root:   g_x, g-star, g1, ga, gA
//
// Both are ordinary Aperture ids — nothing here is a pathological string.
var collationHazardIDs = []string{"g-star", "g1", "gA", "g_x", "ga"}

// linguisticCollation is the collation the scratch columns are retyped to. It is
// ICU's root locale, shipped in pg_catalog by every PostgreSQL 17+ build, so the
// hazard can be staged without depending on which locales the host happens to
// have installed.
const linguisticCollation = "unicode"

// byteOrder returns ids sorted the way SQLite's BINARY collation and Go's
// sort.Strings both order them: by byte value. It is the contract.
func byteOrder(ids []string) []string {
	out := append([]string(nil), ids...)
	sort.Strings(out)
	return out
}

// makeColumnsLinguistic retypes every TEXT column of Aperture's tables in schema
// to COLLATE "unicode", reproducing a cluster whose default collation is
// linguistic. It returns how many columns it changed.
func makeColumnsLinguistic(t *testing.T, ctx context.Context, admin *sql.DB, schema string) int {
	t.Helper()
	rows, err := admin.QueryContext(ctx, `
SELECT c.relname, a.attname
  FROM pg_catalog.pg_attribute a
  JOIN pg_catalog.pg_class     c  ON c.oid = a.attrelid
  JOIN pg_catalog.pg_namespace ns ON ns.oid = c.relnamespace
 WHERE ns.nspname = $1
   AND c.relkind = 'r'
   AND c.relname LIKE 'apt\_%'
   AND a.attnum > 0
   AND NOT a.attisdropped
   AND a.atttypid = 'text'::regtype
 ORDER BY c.relname, a.attnum`, schema)
	if err != nil {
		t.Fatalf("list the schema's text columns: %v", err)
	}
	type col struct{ table, name string }
	var cols []col
	for rows.Next() {
		var c col
		if err := rows.Scan(&c.table, &c.name); err != nil {
			t.Fatalf("scan a column: %v", err)
		}
		cols = append(cols, c)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the schema's text columns: %v", err)
	}
	_ = rows.Close()

	for _, c := range cols {
		stmt := fmt.Sprintf(`ALTER TABLE %s.%s ALTER COLUMN %s TYPE text COLLATE %s`,
			quoteIdent(schema), quoteIdent(c.table), quoteIdent(c.name), quoteIdent(linguisticCollation))
		if _, err := admin.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("retype %s.%s: %v", c.table, c.name, err)
		}
	}
	return len(cols)
}

// TestPostgresLive_TextOrderingIsByteOrderWhateverTheColumnCollation is the
// story's "convert a reasoned defence into a measured one" criterion.
func TestPostgresLive_TextOrderingIsByteOrderWhateverTheColumnCollation(t *testing.T) {
	dsn := requirePostgres(t)
	ctx := liveCtx(t)
	admin := adminConn(t, ctx, dsn)

	var has bool
	if err := admin.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_collation WHERE collname = $1)`,
		linguisticCollation).Scan(&has); err != nil {
		t.Fatalf("look for the %q collation: %v", linguisticCollation, err)
	}
	if !has {
		// Recorded rather than silently skipped: on a server without ICU the
		// hazard has to be staged with a glibc locale instead (any of
		// en_US.utf8, en_GB.utf8, ...), and until then this backend's text
		// ordering is argued, not measured, on this machine.
		t.Skipf("this server has no %q collation, so the linguistic-ordering hazard cannot be staged. "+
			"PostgreSQL 17+ ships it in pg_catalog; on an older or ICU-less build, re-run against a "+
			"database whose default collation is a glibc locale such as en_US.utf8.", linguisticCollation)
	}

	name, storeDSN := scratchSchema(t, ctx, dsn)
	s, err := Open(storeDSN)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Setup(ctx); err != nil {
		t.Fatalf("setup: %v", err)
	}

	if n := makeColumnsLinguistic(t, ctx, admin, name); n < 30 {
		t.Fatalf("retyped only %d text columns in %s; the hazard is not staged across the schema", n, name)
	}

	seedCollationFixture(t, ctx, s)

	// (1) The hazard is REAL here. An ORDER BY that does not pin the collation
	// comes back in ICU order, which is not byte order. If this ever stops being
	// true the rest of the test proves nothing, so it is a hard failure rather
	// than a skip.
	var unpinned []string
	rows, err := admin.QueryContext(ctx,
		`SELECT id FROM `+quoteIdent(name)+`.apt_accounts ORDER BY id`)
	if err != nil {
		t.Fatalf("read the accounts back unpinned: %v", err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		unpinned = append(unpinned, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}
	_ = rows.Close()
	if equalStrings(unpinned, byteOrder(collationHazardIDs)) {
		t.Fatalf("an unpinned ORDER BY returned byte order (%v) on columns collated %q. The hazard is "+
			"not staged, so every assertion below would pass with COLLATE \"C\" removed — fix the "+
			"staging rather than deleting the test.", unpinned, linguisticCollation)
	}
	t.Logf("hazard staged: unpinned ORDER BY gives %v, byte order is %v", unpinned, byteOrder(collationHazardIDs))

	// (2) Every list method Aperture exposes still returns byte order.
	want := byteOrder(collationHazardIDs)
	cases := []struct {
		name string
		got  func() ([]string, error)
		want []string
	}{
		{"ListAccounts", func() ([]string, error) {
			v, err := s.ListAccounts(ctx)
			return mapIDs(v, func(a model.Account) string { return a.ID }), err
		}, plus(want, collationSharedAccount)},
		{"ListPrincipals", func() ([]string, error) {
			v, err := s.ListPrincipals(ctx)
			return mapIDs(v, func(p model.Principal) string { return p.ID }), err
		}, plus(want, collationSharedPrincipal)},
		{"ListRoles", func() ([]string, error) {
			v, err := s.ListRoles(ctx)
			return mapIDs(v, func(r model.Role) string { return r.ID }), err
		}, want},
		{"ListGroups", func() ([]string, error) {
			v, err := s.ListGroups(ctx)
			return mapIDs(v, func(g model.Group) string { return g.ID }), err
		}, want},
		{"ListObjectTypes", func() ([]string, error) {
			v, err := s.ListObjectTypes(ctx)
			return mapIDs(v, func(o model.ObjectType) string { return o.Name }), err
		}, plus(want, collationObjectType)},
		{"ListPermissions", func() ([]string, error) {
			v, err := s.ListPermissions(ctx)
			return mapIDs(v, func(p model.Permission) string { return p.ID }), err
		}, want},
		{"ListRules", func() ([]string, error) {
			v, err := s.ListRules(ctx)
			return mapIDs(v, func(r model.Rule) string { return r.Name }), err
		}, want},
		{"ListTemplates", func() ([]string, error) {
			v, err := s.ListTemplates(ctx)
			return mapIDs(v, func(tp model.Template) string {
				return fmt.Sprintf("%s@%d", tp.Name, tp.Version)
			}), err
		}, templateOrder(want)},
		{"ListGrants", func() ([]string, error) {
			v, err := s.ListGrants(ctx, collationSharedAccount)
			return mapIDs(v, func(g model.Grant) string { return g.ID }), err
		}, want},
		{"GrantsForSubjects", func() ([]string, error) {
			v, err := s.GrantsForSubjects(ctx, collationSharedAccount,
				[]model.Subject{{Kind: model.SubjectPrincipal, ID: collationSharedPrincipal}})
			return mapIDs(v, func(g model.Grant) string { return g.ID }), err
		}, want},
		{"GroupsForPrincipal", func() ([]string, error) {
			v, err := s.GroupsForPrincipal(ctx, collationSharedPrincipal)
			return mapIDs(v, func(g model.Group) string { return g.ID }), err
		}, want},
		{"MembershipsForPrincipal", func() ([]string, error) {
			v, err := s.MembershipsForPrincipal(ctx, collationSharedPrincipal)
			return mapIDs(v, func(m model.Membership) string { return m.AccountID }), err
		}, plus(want, collationSharedAccount)},
		{"MembershipsForAccount", func() ([]string, error) {
			v, err := s.MembershipsForAccount(ctx, collationSharedAccount)
			return mapIDs(v, func(m model.Membership) string { return m.PrincipalID }), err
		}, plus(want, collationSharedPrincipal)},
		{"ListGrantsPage", func() ([]string, error) {
			v, _, err := s.ListGrantsPage(ctx, model.AllAccounts, 0, 100)
			return mapIDs(v, func(g model.Grant) string { return g.AccountID + "\x00" + g.ID }), err
		}, pagedGrantOrder(want)},
		{"QueryAudit", func() ([]string, error) {
			v, err := s.QueryAudit(ctx, model.AuditFilter{})
			return mapIDs(v, func(e model.AuditEvent) string { return e.ID }), err
		}, reverse(want)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.got()
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			expect := tc.want
			if tc.name != "ListGrantsPage" && tc.name != "ListTemplates" && tc.name != "QueryAudit" {
				expect = byteOrder(expect)
			}
			if !equalStrings(got, expect) {
				t.Errorf("%s returned\n  %v\nwant byte order\n  %v\n\n"+
					"The columns behind this query are collated %q. Byte order is the contract every "+
					"backend shares — storage/sqlite compares TEXT byte-wise and storage/memory sorts in "+
					"Go — so the statement behind %s has lost its COLLATE \"C\".",
					tc.name, got, expect, linguisticCollation, tc.name)
			}
		})
	}
}

// collationSharedAccount and collationSharedPrincipal are the two ordinary ids
// the fan-out queries hang off: one account every grant is stamped with, and one
// principal that is a member of every account and every group. They are spelled
// plainly so they cannot themselves be reordered by a collation.
const (
	collationSharedAccount   = "shared-account"
	collationSharedPrincipal = "shared-principal"
	collationObjectType      = "document"
)

// seedCollationFixture writes one row per hazardous id into every table with a
// TEXT ordering, plus the shared account and principal the fan-out queries need.
func seedCollationFixture(t *testing.T, ctx context.Context, s *Store) {
	t.Helper()
	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("seed %s: %v", what, err)
		}
	}

	must("shared account", s.PutAccount(ctx, model.Account{ID: collationSharedAccount, Name: "Shared"}))
	must("shared principal", s.PutPrincipal(ctx, model.Principal{
		ID: collationSharedPrincipal, Kind: model.PrincipalUser, Identity: "user:shared",
	}))
	// One ordinary object type for the permissions the grants cite; the
	// hazardous names below are what ListObjectTypes orders.
	must("object type", s.PutObjectType(ctx, model.ObjectType{
		Name: collationObjectType, Actions: []string{"read", "write", "delete", "list", "admin"},
	}))

	actions := []string{"read", "write", "delete", "list", "admin"}
	for i, id := range collationHazardIDs {
		must("account", s.PutAccount(ctx, model.Account{ID: id, Name: "A"}))
		must("principal", s.PutPrincipal(ctx, model.Principal{
			ID: id, Kind: model.PrincipalUser, Identity: fmt.Sprintf("user:p%d", i),
		}))
		must("role", s.PutRole(ctx, model.Role{ID: id, Name: "R"}))
		must("group", s.PutGroup(ctx, model.Group{
			ID: id, Name: "G", MemberPrincipalIDs: []string{collationSharedPrincipal},
		}))
		must("object type", s.PutObjectType(ctx, model.ObjectType{Name: id, Actions: []string{"read"}}))
		must("permission", s.PutPermission(ctx, model.Permission{
			ID: id, ObjectType: collationObjectType, Action: actions[i],
		}))
		// The shared principal belongs to every hazardous account, and every
		// hazardous principal belongs to the shared one.
		must("membership", s.PutMembership(ctx, model.Membership{
			PrincipalID: collationSharedPrincipal, AccountID: id,
		}))
		must("membership", s.PutMembership(ctx, model.Membership{
			PrincipalID: id, AccountID: collationSharedAccount,
		}))
		must("membership", s.PutMembership(ctx, model.Membership{
			PrincipalID: collationSharedPrincipal, AccountID: collationSharedAccount,
		}))
		// Two grants per id: one in the shared account (what ListGrants and
		// GrantsForSubjects order) and one in the hazardous account of the same
		// name (what makes ListGrantsPage's (account_id, id) ordering nontrivial).
		must("grant", s.PutGrant(ctx, model.Grant{
			ID: id, AccountID: collationSharedAccount,
			Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: collationSharedPrincipal},
			PermissionID: id, Object: collationObjectType + ":*", Effect: model.EffectAllow,
		}))
		must("grant", s.PutGrant(ctx, model.Grant{
			ID: "own-" + id, AccountID: id,
			Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: collationSharedPrincipal},
			PermissionID: id, Object: collationObjectType + ":*", Effect: model.EffectAllow,
		}))
		must("rule", s.PutRule(ctx, model.Rule{
			Name: id, AST: json.RawMessage(`{"type":"literal","value":true}`),
		}))
		// Two versions of one name, so `ORDER BY name COLLATE "C", version` is
		// exercised on both terms.
		for _, v := range []int{1, 2} {
			must("template", s.PutTemplate(ctx, model.Template{
				Name: id, Version: v, Description: "D",
				Params: []model.TemplateParam{{Name: "account", Type: model.ParamSegment}},
				Grants: []model.TemplateGrant{{
					Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: "${account}-member"},
					PermissionID: id, Object: collationObjectType + ":*", Effect: model.EffectAllow,
				}},
			}))
		}
		// One audit event per id, all at the SAME instant, so the tie is broken
		// by `id COLLATE "C" DESC` and nothing else.
		must("audit", s.AppendAudit(ctx, model.AuditEvent{
			ID: id, Timestamp: collationAuditInstant, EventType: model.AuditMutation,
			Action: "PutGrant", Actor: collationSharedPrincipal, Outcome: model.OutcomeSuccess,
		}))
	}
}

// collationAuditInstant is one fixed instant every seeded audit event carries,
// so QueryAudit's primary ordering term is constant and its tie-break — the one
// this test is about — decides the whole result.
var collationAuditInstant = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// templateOrder is the expected ListTemplates result: name byte-wise, version
// ascending within a name.
func templateOrder(names []string) []string {
	out := make([]string, 0, len(names)*2)
	for _, n := range byteOrder(names) {
		out = append(out, n+"@1", n+"@2")
	}
	return out
}

// pagedGrantOrder is the expected ListGrantsPage result across all accounts:
// account_id byte-wise, then id byte-wise. The two columns are compared as a
// TUPLE rather than as a joined string, because that is what the SQL does; the
// NUL joiner in the observed values exists only so a mismatch prints readably.
func pagedGrantOrder(ids []string) []string {
	type pair struct{ account, id string }
	var pairs []pair
	for _, id := range ids {
		pairs = append(pairs, pair{account: id, id: "own-" + id})
		pairs = append(pairs, pair{account: collationSharedAccount, id: id})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].account != pairs[j].account {
			return pairs[i].account < pairs[j].account
		}
		return pairs[i].id < pairs[j].id
	})
	out := make([]string, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, p.account+"\x00"+p.id)
	}
	return out
}

// plus returns byte order over ids with one more id added. It copies, so the
// shared expectation slice can never be aliased by an append.
func plus(ids []string, extra string) []string {
	out := make([]string, 0, len(ids)+1)
	out = append(out, ids...)
	out = append(out, extra)
	return byteOrder(out)
}

func reverse(in []string) []string {
	out := byteOrder(in)
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

func mapIDs[T any](in []T, id func(T) string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		out = append(out, id(v))
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
