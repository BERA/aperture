package bench

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/frankbardon/aperture/engine"
	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/identity"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/provider"
	"github.com/frankbardon/aperture/rules"
	"github.com/frankbardon/aperture/storage/memory"
)

// Fixture scale. These produce a sizable model — thousands of grants across
// several accounts with hundreds of principals — so a single Check resolves a
// non-trivial subject set and candidate set rather than a toy three-grant model.
const (
	numAccounts   = 8
	numRoles      = 60
	numGroups     = 60
	numPrincipals = 480
	// concreteDocs are the concrete (wildcard-free) document grants attached to
	// role0 in every account, so Enumerate has a real, bounded candidate set to
	// materialise (literal coverage cannot enumerate a wildcard grant).
	concreteDocs = 60
)

// model bundles a seeded store with a representative request the benchmarks and
// the NFR test exercise.
//
// registry / ruleEngine / ruleReqs are the E5-S2 rule-backed extension: the SAME
// seeded store additionally carries one rule-backed inclusive grant per rule
// variant, so a collection-rule Check runs against the same non-trivial model
// rather than a parallel toy harness. They are consumed only by newRuleService
// (bench_test.go) — the literal-scope benchmarks are untouched by their presence,
// because an engine only consults scope resolvers when built
// WithScopeResolution.
type benchModel struct {
	store    *memory.Store
	req      engine.Request
	enumReq  engine.EnumerateRequest
	registry *provider.Registry
	rules    *rules.Engine
	ruleReqs map[string]engine.Request
}

// ---------------------------------------------------------------------------
// E5-S2: collection / nested-object rule fixture
// ---------------------------------------------------------------------------

// Array sizing for the collection fixtures, DERIVED from the metadata value
// model's per-field size cap rather than picked for roundness.
//
// provider.ValueBytes measures a []any of strings as the SUM OF THEIR LENGTHS
// (container framing is free), and provider.DefaultMaxValueBytes is 64 KiB per
// FIELD value. So at a fixed tag width the cap fixes an exact element count, and
// that count — not a round number — is the largest array a host can legally load
// under the defaults. That upper bound is the number the NFR has to survive; if
// it does not, the finding is about the cap, not about the benchmark.
const (
	// benchTagWidth is the byte width of a fixture tag: "tag-" + 5 digits = 9.
	benchTagWidth = 9
	// tagsAtCapLen is the LARGEST tag array the default 64 KiB per-value cap
	// admits at benchTagWidth: floor(65536/9) = 7281 tags occupying 65 529 of
	// the 65 536 available bytes, where 7282 would breach it (asserted by
	// TestCollectionFixtureSitsAtTheValueCap). This is the adversarial-but-legal
	// worst case, and the case the "is 64 KiB too generous?" question is about.
	tagsAtCapLen = provider.DefaultMaxValueBytes / benchTagWidth
	// tagsTypicalLen is a REALISTIC document tag list — a dozen labels, ~108
	// bytes, about 0.16% of the cap. It is the other end of the same axis: the
	// pair of sizes is what makes the scaling behaviour visible, rather than a
	// single point that could be read as either typical or worst case.
	tagsTypicalLen = 12
)

// Rule-variant names. Each is simultaneously the rule reference, the document
// action its permission declares, and the benchmark sub-name, so a number in
// `make bench` output maps to exactly one rendered expression.
const (
	// ruleScalar is the CONTROL: a scalar comparison over metadata, native infix
	// render, no collection anywhere. Every other variant's cost is read as a
	// delta from this one.
	ruleScalar = "rule-scalar"
	// ruleLiteralIn is the E5-S1 unguarded common path: the collection operand is
	// a LIST LITERAL, statically known to be an array, so the comparison keeps its
	// native `in` render and never reaches the guarded dispatcher.
	ruleLiteralIn = "rule-literal-in"
	// ruleNested reads through a nested object via the E2-S2 optional-chaining
	// render (object.owner.dept -> object?.owner?.dept).
	ruleNested = "rule-nested"
	// ruleTagsTypical is a collection operation over a VARIABLE operand, which
	// E5-S1 renders to the guarded dispatcher $op(...), over a realistic array.
	ruleTagsTypical = "rule-tags-typical"
	// ruleTagsAtCap is the same guarded render over the cap-maximal array.
	ruleTagsAtCap = "rule-tags-at-cap"
	// ruleDateCompare is a binary DATE comparison, which puts PARSING on the
	// comparison hot path: both operands are canonical date strings and both are
	// parsed to instants on every evaluation, because comparing the strings is
	// wrong across granularities. Read as a delta from rule-scalar, this is the
	// per-comparison cost of a date operator.
	ruleDateCompare = "rule-date-compare"
	// ruleDateBetween is the ternary form, which parses THREE operands per
	// evaluation rather than two. The pair makes the per-operand parse cost
	// readable by difference rather than as one absolute number.
	ruleDateBetween = "rule-date-between"
	// ruleDateRelative is the same binary comparison with its bound RESOLVED per
	// evaluation instead of read from a literal: calendar arithmetic (a snap and
	// a clamping quarter offset) plus the canonical-string round trip $rel does
	// on the way into $date. Read as a delta from rule-date-compare, this is the
	// per-evaluation cost of a relative date rather than a fixed one.
	ruleDateRelative = "rule-date-relative"
	// ruleDateSameYear is the calendar-BUCKET shape rather than an ordering one.
	// sameDay/sameMonth/sameYear do not compare instants at all once parsed —
	// they read calendar components off both sides — so their post-parse cost is
	// a different branch from before/after/between and is measured separately.
	ruleDateSameYear = "rule-date-same-year"
	// ruleDateRelBounds is the most expensive shape the AST can express: a
	// ternary between whose BOTH bounds are relative dates. One evaluation pays
	// two anchor resolutions, two snaps, two offsets, two canonical formats and
	// three parses. If relative resolution were ever shared across the operands
	// of one decision, this is the fixture that would show it.
	ruleDateRelBounds = "rule-date-rel-bounds"
	// ruleDateClamped exercises the CLAMPING branch of the calendar arithmetic,
	// which a mid-month anchor never reaches: the anchor is snapped to a 31-day
	// month end and then offset into a 28-day month, so addMonthsClamped's day
	// clamp actually runs. Read as a delta from rule-date-relative (a
	// non-clamping quarter offset), this is what clamping costs.
	ruleDateClamped = "rule-date-clamped"
	// ruleDateMulti is one rule carrying FIVE date comparisons rather than one,
	// so the per-comparison cost is readable against the single-comparison
	// variants instead of being confounded with the fixed per-decision overhead
	// every rule pays. It is also where the four date operators no other asserted
	// fixture uses are held to the floor.
	ruleDateMulti = "rule-date-multi"
	// ruleDateDeny is the DENY path, which is a different branch and not a
	// cheaper one: a string of exactly the date-only WIDTH that is not a date
	// reaches time.Parse, whose failure constructs a *time.ParseError inside the
	// standard library before provider classifies and discards it. That is the
	// one bounded allocating path in an otherwise allocation-free parse, and a
	// rule over host-supplied data is exactly where malformed values live, so it
	// is held to the same floor as every allow.
	ruleDateDeny = "rule-date-deny"
)

// The date fixtures' reference instant.
//
// It is PINNED rather than read from the wall clock, for two reasons that both
// bite this fixture specifically. A relative-date bound moves with the clock, so
// a fixture built against time.Now() decides a different comparison every day and
// its warm-up assertion is a promise about the calendar rather than about the
// code. And the clamping variant only reaches the branch it exists to measure
// when the anchor month is longer than the target month — under a live clock that
// is true for part of the year and quietly untrue for the rest, which is the
// worst possible property for a regression guard.
//
// 2026-08-31 is chosen so that every date variant resolves to a stable verdict
// AND the clamping variant genuinely clamps: August has 31 days, and six months
// back is February 2026 with 28. TestDateFixtureActuallyClamps asserts that
// derivation rather than trusting this comment.
//
// One documented coupling comes with it (rules.WithClock): the engine has ONE
// time source, so pinning it also freezes compiled-rule cache expiry. That is
// harmless here and mildly helpful — the rule engine is built with the default
// TTL (entries live until invalidated), and the NFR is about the cached Check, so
// a frozen clock can only keep the measured window in the cached steady state.
var benchNow = time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC)

// benchClock pins the rules engine's single time source to benchNow.
type benchClock struct{}

func (benchClock) Now() time.Time { return benchNow }

// The date fixture's field values. They are the two canonical granularities, so
// the mixed-granularity path (a date-only field compared against a timestamp
// bound, which parses through a different branch) is exercised rather than only
// the same-shape case.
const (
	benchHiredAt    = "2026-03-04"
	benchReviewedAt = "2026-03-04T09:30:00Z"
	// benchMalformedAt is the deny fixture's operand: a legal metadata string
	// scalar that is not a canonical date. Its WIDTH is the load-bearing part —
	// it is exactly as long as the date-only layout, which is what carries it
	// past provider's cheap length screen and into time.Parse, so the deny
	// variant measures the allocating failure branch rather than the free one.
	benchMalformedAt = "04/03/2026"
)

// benchObject is the object every rule variant is checked against. Using ONE
// object for all variants is deliberate: the provider fetch and its cache hit are
// then byte-identical across variants, so the measured delta is purely the rule's
// evaluation cost and not a difference in metadata plumbing.
const benchObject = "account:acct0/project:proj0/document:doc42"

// The rule variants' subject. Deliberately NOT user0/role0: the literal-scope
// benchmarks are the control this story compares against, and adding grants to
// role0 would change their measured allocation profile.
const (
	ruleUser = "user-rules"
	ruleRole = "role-rules"
)

// ruleVariant is one benchmark case: a rule AST, the action whose permission
// carries the inclusive;rule= scope strategy that reaches it, and the render
// strategy it is here to measure (recorded so the two E5-S1 strategies are
// visibly both covered).
type ruleVariant struct {
	name   string
	ast    *rules.Node
	render string
	// allow is the verdict the variant's Check must return, asserted on every
	// warm-up. It exists because one variant DENIES by design: a benchmark that
	// silently measured a fail-closed deny would report a meaningless number, so
	// the expected verdict is stated per variant rather than assumed, and a
	// variant that stops deciding what it was written to decide fails loudly
	// instead of quietly measuring the other branch.
	allow bool
}

// ruleVariants is the closed set of rule-backed benchmark cases.
//
// The two hasAny cases match on the LAST element of the array on purpose: the
// operator short-circuits on the first hit, so matching early would measure a
// best case that a real tag list does not guarantee.
func ruleVariants() []ruleVariant {
	return []ruleVariant{
		{
			name:   ruleScalar,
			ast:    rules.Compare(rules.OpEq, rules.Var("object.classification"), rules.Lit("public")),
			render: `native infix: (object.classification == "public")`,
			allow:  true,
		},
		{
			name: ruleLiteralIn,
			ast: rules.Compare(rules.OpIn, rules.Var("object.region"),
				rules.List(rules.Lit("us-east"), rules.Lit("us-west"), rules.Lit("eu-west"))),
			render: `native infix, list literal: (object.region in ["us-east", ...])`,
			allow:  true,
		},
		{
			name:   ruleNested,
			ast:    rules.Compare(rules.OpEq, rules.Var("object.owner.dept"), rules.Lit("platform")),
			render: `optional chaining: (object?.owner?.dept == "platform")`,
			allow:  true,
		},
		{
			name: ruleTagsTypical,
			ast: rules.Compare(rules.OpHasAny, rules.Var("object.tags"),
				rules.List(rules.Lit(benchTag(tagsTypicalLen-1)))),
			render: `guarded dispatcher: $op("hasAny", __notes, "object.tags", object?.tags, ...)`,
			allow:  true,
		},
		{
			name: ruleTagsAtCap,
			ast: rules.Compare(rules.OpHasAny, rules.Var("object.tagsAtCap"),
				rules.List(rules.Lit(benchTag(tagsAtCapLen-1)))),
			render: `guarded dispatcher: $op("hasAny", __notes, "object.tagsAtCap", object?.tagsAtCap, ...)`,
			allow:  true,
		},
		// The date cases compare a date-only FIELD against a TIMESTAMP bound on
		// purpose: granularity never affects ordering, so the mixed case is the
		// one a real rule hits and the one a string comparison would get wrong.
		{
			name: ruleDateCompare,
			ast: rules.Compare(rules.OpBefore, rules.Var("object.hired_at"),
				rules.Lit("2026-12-31T23:59:59Z")),
			render: `guarded dispatcher: $date("before", __notes, "object.hired_at", object?.hired_at, ...) — 2 parses`,
			allow:  true,
		},
		{
			name: ruleDateBetween,
			ast: rules.Between(rules.Var("object.reviewed_at"),
				rules.Lit("2026-01-01"), rules.Lit("2026-12-31T23:59:59Z")),
			render: `guarded dispatcher: $date("between", __notes, "object.reviewed_at", object?.reviewed_at, ...) — 3 parses`,
			allow:  true,
		},
		// Calendar-bucket equality. Once both operands have parsed, sameYear does
		// not compare instants at all — it reads a calendar component off each
		// side — so its post-parse branch differs from every ordering operator
		// above and the pair separates parse cost from comparison cost.
		{
			name: ruleDateSameYear,
			ast: rules.Compare(rules.OpSameYear, rules.Var("object.hired_at"),
				rules.Lit("2026-07-15")),
			render: `guarded dispatcher: $date("sameYear", __notes, "object.hired_at", object?.hired_at, ...) — 2 parses`,
			allow:  true,
		},
		// The relative bound is resolved on EVERY evaluation — snap, offset,
		// format, and then the parse $date does on the way back in — where the
		// three cases above read a literal. The rule reads "hired before the
		// start of the previous quarter"; against the pinned instant that bound
		// is 2026-04-01, which the fixture's 2026-03-04 hire date precedes.
		{
			name: ruleDateRelative,
			ast: rules.Compare(rules.OpBefore, rules.Var("object.hired_at"),
				rules.RelativeDate(rules.AnchorNow, -1, rules.UnitQuarters, rules.SnapStartOfQuarter)),
			render: `guarded dispatcher: $date("before", ..., $rel(__now, "NOW", -1, "quarters", "startOfQuarter"), ...) — 2 parses + calendar arithmetic`,
			allow:  true,
		},
		// The ternary form with BOTH bounds relative — the most work a single
		// comparison can be made to do. Two anchors (one of them TODAY, which
		// resolves through a different branch from NOW), two snaps, two offsets,
		// two canonical formats, three parses. The pair with rule-date-relative
		// is what answers "does a second relative operand cost a second full
		// resolution, or is the anchor shared?".
		{
			name: ruleDateRelBounds,
			ast: rules.Between(rules.Var("object.hired_at"),
				rules.RelativeDate(rules.AnchorNow, -5, rules.UnitYears, rules.SnapStartOfYear),
				rules.RelativeDate(rules.AnchorToday, 0, rules.UnitDays, rules.SnapEndOfDay)),
			render: `guarded dispatcher: $date("between", ..., $rel(...,"NOW",-5,"years","startOfYear"), $rel(...,"TODAY",0,"days","endOfDay")) — 3 parses + 2 x calendar arithmetic`,
			allow:  true,
		},
		// The CLAMPING branch. Snap to the end of a 31-day month, then offset six
		// months back into a 28-day one: addMonthsClamped's day clamp runs, where
		// rule-date-relative's quarter offset off a start-of-quarter boundary
		// never touches it. Against the pinned instant the bound is
		// 2026-02-28T23:59:59 (asserted by TestDateFixtureActuallyClamps), which
		// the 2026-03-04 hire date follows.
		{
			name: ruleDateClamped,
			ast: rules.Compare(rules.OpAfter, rules.Var("object.hired_at"),
				rules.RelativeDate(rules.AnchorNow, -6, rules.UnitMonths, rules.SnapEndOfMonth)),
			render: `guarded dispatcher: $date("after", ..., $rel(__now, "NOW", -6, "months", "endOfMonth"), ...) — 2 parses + CLAMPING calendar arithmetic`,
			allow:  true,
		},
		// Five date comparisons in one rule, over both fixture granularities. It
		// measures what a realistic date-heavy policy costs — nothing else here
		// has more than one comparison — and it is where onOrAfter, onOrBefore,
		// sameDay and sameMonth are held to the NFR floor, so every one of the
		// eight date operators sits inside the asserted set rather than only
		// inside an informational benchmark. TestDateBenchCoverage enforces that.
		{
			name: ruleDateMulti,
			ast: rules.And(
				rules.Compare(rules.OpOnOrAfter, rules.Var("object.hired_at"), rules.Lit("2026-03-04")),
				rules.Compare(rules.OpAfter, rules.Var("object.reviewed_at"), rules.Lit("2026-03-04")),
				rules.Compare(rules.OpOnOrBefore, rules.Var("object.hired_at"), rules.Lit("2026-12-31T23:59:59Z")),
				rules.Compare(rules.OpSameDay, rules.Var("object.reviewed_at"), rules.Lit("2026-03-04")),
				rules.Compare(rules.OpSameMonth, rules.Var("object.hired_at"), rules.Lit("2026-03-20")),
			),
			render: `five $date(...) calls under one and — 10 parses`,
			allow:  true,
		},
		// The DENY path, and the only variant here that denies. Its left operand
		// is a well-formed metadata string that is not a canonical date, so the
		// comparison takes the deny-safe branch instead of the comparison branch.
		// Deliberately measured rather than assumed cheap: this is the one input
		// shape that allocates inside the parse (a *time.ParseError the value
		// model classifies and discards), and host-supplied data is where
		// malformed dates actually come from.
		{
			name: ruleDateDeny,
			ast: rules.Compare(rules.OpBefore, rules.Var("object.malformed_at"),
				rules.Lit("2026-12-31T23:59:59Z")),
			render: `guarded dispatcher: $date("before", ..., "object.malformed_at", ...) — parse FAILS, deny-safe false`,
			allow:  false,
		},
	}
}

// benchTag renders the fixed-width tag the size derivation assumes.
func benchTag(i int) string { return fmt.Sprintf("tag-%05d", i) }

// benchTags builds an n-element []any of fixed-width tags.
func benchTags(n int) []any {
	out := make([]any, n)
	for i := range out {
		out[i] = benchTag(i)
	}
	return out
}

// benchMetadata is the single object's metadata bag. It carries every field the
// rule variants read, including BOTH tag arrays — legal because the 64 KiB cap is
// per FIELD, not per object — so one cached entry serves every variant.
func benchMetadata() provider.Metadata {
	return provider.Metadata{
		"classification": "public",
		"region":         "us-east",
		"owner": map[string]any{
			"dept": "platform",
			"team": "access",
		},
		"tags":      benchTags(tagsTypicalLen),
		"tagsAtCap": benchTags(tagsAtCapLen),
		// Dates ride inside the value model as ordinary string scalars, so they
		// cost the metadata bag nothing structurally — the cost the date variants
		// measure is the PARSE, which happens per evaluation and not per fetch.
		"hired_at":    benchHiredAt,
		"reviewed_at": benchReviewedAt,
		// A legal string scalar the value model has no opinion about and a date
		// operator cannot parse — the model is deliberately date-blind, so this
		// is exactly the shape a host loads when nothing declared the field a
		// date. It drives the deny variant.
		"malformed_at": benchMalformedAt,
	}
}

// benchProvider is the fixture's ObjectProvider: a single object, returned BY
// REFERENCE, so the registry cache stores and hands back the very map built here.
// That is the contract E5-S2 measures — a defensive copy anywhere between here
// and the rule evaluator shows up as allocated bytes per Check.
type benchProvider struct {
	id identity.Identity
	md provider.Metadata
}

func (p benchProvider) Fetch(_ context.Context, id identity.Identity) (provider.Metadata, error) {
	if id.String() == p.id.String() {
		return p.md, nil
	}
	return nil, aerr.New(aerr.APERTURE_NOT_FOUND, "bench provider: absent object")
}

func (p benchProvider) List(_ context.Context) ([]provider.Object, error) {
	return []provider.Object{{ID: p.id, Metadata: p.md}}, nil
}

func (p benchProvider) Query(ctx context.Context, f provider.Filter) ([]provider.Object, error) {
	all, err := p.List(ctx)
	if err != nil {
		return nil, err
	}
	if f.Pattern != nil && !f.Pattern.Matches(p.id) {
		return nil, nil
	}
	return all, nil
}

// buildModel seeds a sizable org -> project -> document authorization model into
// a fresh in-memory store and returns it alongside a representative cached-Check
// request (an allow with several matching candidates and a deny carve-out in the
// same scope, so deny-overrides + specificity actually run).
func buildModel(tb testing.TB) benchModel {
	tb.Helper()
	ctx := context.Background()
	store := memory.New()

	must := func(err error) {
		tb.Helper()
		if err != nil {
			tb.Fatalf("seed: %v", err)
		}
	}

	// Object types + permissions (read/write/delete on documents). Each E5-S2
	// rule variant additionally declares its own action verb, so one permission —
	// and therefore one scope strategy — reaches exactly one rule.
	docActions := []string{"read", "write", "delete", "share"}
	for _, v := range ruleVariants() {
		docActions = append(docActions, v.name)
	}
	must(store.PutObjectType(ctx, model.ObjectType{
		Name: "document", Actions: docActions,
	}))
	must(store.PutObjectType(ctx, model.ObjectType{
		Name: "project", Actions: []string{"read", "write", "delete"},
	}))
	const (
		permRead   = "perm-doc-read"
		permWrite  = "perm-doc-write"
		permDelete = "perm-doc-delete"
	)
	must(store.PutPermission(ctx, model.Permission{ID: permRead, ObjectType: "document", Action: "read"}))
	must(store.PutPermission(ctx, model.Permission{ID: permWrite, ObjectType: "document", Action: "write"}))
	must(store.PutPermission(ctx, model.Permission{ID: permDelete, ObjectType: "document", Action: "delete"}))

	// Accounts.
	for a := 0; a < numAccounts; a++ {
		must(store.PutAccount(ctx, model.Account{ID: acct(a), Name: acct(a)}))
	}

	// Roles + groups (empty bundles — the engine resolves grants by subject, not
	// by the role's own permission list, so the bundles need only exist).
	for r := 0; r < numRoles; r++ {
		must(store.PutRole(ctx, model.Role{ID: role(r), Name: role(r)}))
	}
	groupMembers := make([][]string, numGroups)

	// Principals: each gets three roles and is enrolled in two groups, so its
	// subject set is {self} U 3 roles U 2 groups = 6 subjects.
	for p := 0; p < numPrincipals; p++ {
		roles := []string{
			role(p % numRoles),
			role((p + 13) % numRoles),
			role((p + 27) % numRoles),
		}
		must(store.PutPrincipal(ctx, model.Principal{
			ID: user(p), Kind: model.PrincipalUser, Identity: "user:" + user(p), RoleIDs: roles,
		}))
		g0, g1 := p%numGroups, (p+7)%numGroups
		groupMembers[g0] = append(groupMembers[g0], user(p))
		if g1 != g0 {
			groupMembers[g1] = append(groupMembers[g1], user(p))
		}
	}
	for g := 0; g < numGroups; g++ {
		must(store.PutGroup(ctx, model.Group{ID: group(g), Name: group(g), MemberPrincipalIDs: groupMembers[g]}))
	}

	// Grants, per account:
	//   - each role gets a wildcard allow-read + allow-write on its own project,
	//     plus a more-specific deny-read carving out one sealed document;
	//   - each group gets a document-scoped allow-read on its project;
	//   - group0 additionally holds an account-wide broad allow-read and a broad
	//     deny-delete, so the hot Check sees several overlapping candidates of
	//     differing specificity.
	for a := 0; a < numAccounts; a++ {
		ac := acct(a)
		base := "account:" + ac // identity-path prefix for this account's objects
		for i := 0; i < numRoles; i++ {
			proj := fmt.Sprintf("project:proj%d", i)
			must(store.PutGrant(ctx, model.Grant{
				ID: gid(ac, "role-read", i), AccountID: ac,
				Subject:      model.Subject{Kind: model.SubjectRole, ID: role(i)},
				PermissionID: permRead, Object: base + "/" + proj + "/**", Effect: model.EffectAllow,
			}))
			must(store.PutGrant(ctx, model.Grant{
				ID: gid(ac, "role-write", i), AccountID: ac,
				Subject:      model.Subject{Kind: model.SubjectRole, ID: role(i)},
				PermissionID: permWrite, Object: base + "/" + proj + "/**", Effect: model.EffectAllow,
			}))
			must(store.PutGrant(ctx, model.Grant{
				ID: gid(ac, "role-deny-secret", i), AccountID: ac,
				Subject:      model.Subject{Kind: model.SubjectRole, ID: role(i)},
				PermissionID: permRead, Object: base + "/" + proj + "/document:secret", Effect: model.EffectDeny,
			}))
		}
		for i := 0; i < numGroups; i++ {
			proj := fmt.Sprintf("project:proj%d", i)
			must(store.PutGrant(ctx, model.Grant{
				ID: gid(ac, "group-read", i), AccountID: ac,
				Subject:      model.Subject{Kind: model.SubjectGroup, ID: group(i)},
				PermissionID: permRead, Object: base + "/" + proj + "/document:*", Effect: model.EffectAllow,
			}))
		}
		// group0 broad grants (overlapping candidates of differing specificity).
		must(store.PutGrant(ctx, model.Grant{
			ID: gid(ac, "group0-broad-read", 0), AccountID: ac,
			Subject:      model.Subject{Kind: model.SubjectGroup, ID: group(0)},
			PermissionID: permRead, Object: base + "/**", Effect: model.EffectAllow,
		}))
		must(store.PutGrant(ctx, model.Grant{
			ID: gid(ac, "group0-broad-deny-delete", 0), AccountID: ac,
			Subject:      model.Subject{Kind: model.SubjectGroup, ID: group(0)},
			PermissionID: permDelete, Object: base + "/project:proj0/**", Effect: model.EffectDeny,
		}))
		// Concrete document grants on role0 so Enumerate has a bounded, real set.
		for d := 0; d < concreteDocs; d++ {
			must(store.PutGrant(ctx, model.Grant{
				ID: gid(ac, "role0-doc", d), AccountID: ac,
				Subject:      model.Subject{Kind: model.SubjectRole, ID: role(0)},
				PermissionID: permRead,
				Object:       fmt.Sprintf("%s/project:proj0/document:doc%d", base, d),
				Effect:       model.EffectAllow,
			}))
		}
	}

	// Representative cached Check: user0 (roles role0/role13/role27, groups
	// group0/group7) reading a document under proj0 in acct0. Matching allow
	// candidates: role0's proj0/** read, group0's proj0/document:* read, and
	// group0's account-wide read — three overlapping allows of differing
	// specificity; the sealed "secret" deny does not cover doc42, so the verdict
	// is ALLOW resolved through the full deny-overrides walk.
	req := engine.Request{
		Account:   acct(0),
		Principal: user(0),
		Action:    "read",
		Object:    "account:" + acct(0) + "/project:proj0/document:doc42",
	}
	enumReq := engine.EnumerateRequest{
		Account:   acct(0),
		Principal: user(0),
		Action:    "read",
		Pattern:   "account:" + acct(0) + "/project:proj0/document:*",
	}

	// E5-S2: rule-backed inclusive grants over the same model. One permission +
	// one grant per variant, all on role0 (which user0 holds) in acct0, all
	// bounded to the same object — so the ONLY thing that differs between the
	// variants' measurements is the rule expression that decides membership.
	reg, ruleEng, ruleReqs := buildRuleLayer(tb, ctx, store, must)

	return benchModel{
		store: store, req: req, enumReq: enumReq,
		registry: reg, rules: ruleEng, ruleReqs: ruleReqs,
	}
}

// buildRuleLayer seeds the rule-backed half of the fixture and returns the
// provider registry, the rules engine, and one Check request per variant.
//
// The registry is built WITH EXPIRY DISABLED (provider.WithTTL(0)). The NFR is
// about the CACHED Check, and DefaultTTL is 30s — long benchmark runs would
// otherwise silently start re-fetching partway through and measure a mix of the
// cached and uncached paths.
func buildRuleLayer(tb testing.TB, ctx context.Context, store *memory.Store, must func(error)) (*provider.Registry, *rules.Engine, map[string]engine.Request) {
	tb.Helper()

	md := benchMetadata()
	// The fixture's own size claims are ASSERTED, not asserted-in-a-comment: if a
	// future change to the value model moves the caps, this fails here rather
	// than quietly benchmarking a different array.
	if err := provider.ValidateMetadata(md); err != nil {
		tb.Fatalf("bench metadata violates the value model: %v", err)
	}

	objID, err := identity.Parse(benchObject)
	if err != nil {
		tb.Fatalf("parse bench object: %v", err)
	}
	reg := provider.NewRegistry()
	reg.MustRegister("document", benchProvider{id: objID, md: md}, provider.WithTTL(0))

	// The rule grants hang off a DEDICATED principal and role that user0 does not
	// hold, so the pre-existing literal-scope baseline is measured on exactly the
	// subject set and candidate grants it was measured on before E5-S2. Attaching
	// them to role0 instead moved the committed baseline by +5 allocs/op and
	// +2 KB/op — a fixture that changes the number it is the control for.
	//
	// ruleUser keeps a comparable subject set (self + three roles) so the rule
	// variants are still resolved against the sizable model rather than a toy.
	must(store.PutRole(ctx, model.Role{ID: ruleRole, Name: ruleRole}))
	must(store.PutPrincipal(ctx, model.Principal{
		ID: ruleUser, Kind: model.PrincipalUser, Identity: "user:" + ruleUser,
		RoleIDs: []string{ruleRole, role(1), role(14)},
	}))

	source := rules.MapSource{}
	reqs := make(map[string]engine.Request, len(ruleVariants()))
	for _, v := range ruleVariants() {
		source[v.name] = &rules.Rule{Name: v.name, AST: v.ast}
		must(store.PutPermission(ctx, model.Permission{
			ID: "perm-" + v.name, ObjectType: "document", Action: v.name,
			ScopeStrategy: "inclusive;rule=" + v.name,
		}))
		must(store.PutGrant(ctx, model.Grant{
			ID: "g-" + v.name, AccountID: acct(0),
			Subject:      model.Subject{Kind: model.SubjectRole, ID: ruleRole},
			PermissionID: "perm-" + v.name,
			Object:       "account:" + acct(0) + "/**",
			Effect:       model.EffectAllow,
		}))
		reqs[v.name] = engine.Request{
			Account: acct(0), Principal: ruleUser, Action: v.name, Object: benchObject,
		}
	}
	// The clock is PINNED (benchNow). It is the engine's single time source, so
	// this fixes both what NOW means to a relative-date bound — which is what
	// makes the date variants decide the same comparison on every run and on
	// every day — and, as the documented flip side, compiled-rule cache expiry.
	// The latter is inert here: this engine takes the default TTL, under which
	// entries live until invalidated anyway.
	return reg, rules.NewEngine(source, reg, rules.WithClock(benchClock{})), reqs
}

func acct(i int) string  { return fmt.Sprintf("acct%d", i) }
func role(i int) string  { return fmt.Sprintf("role%d", i) }
func group(i int) string { return fmt.Sprintf("group%d", i) }
func user(i int) string  { return fmt.Sprintf("user%d", i) }
func gid(account, kind string, i int) string {
	return fmt.Sprintf("g-%s-%s-%d", account, kind, i)
}
