package engine

import (
	"context"
	"strings"
	"testing"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/provider"
	"github.com/frankbardon/aperture/rules"
	"github.com/frankbardon/aperture/scope"
	"github.com/frankbardon/aperture/storage/memory"
)

// E5-S1: deny-safe shape mismatches, visible in Explain.
//
// These tests exercise the whole stack the policy has to survive — provider
// registry, rule-backed inclusive scope, decision engine — because the seam being
// proved is that a rule's diagnostic travels back OUT through scope.ScopeResolver
// (whose interface carries no notes) and lands in the trace.

// shapeFixture builds an engine whose single grant covers objects selected by
// ast, over metadata that is variously a proper array, a STRING where an array
// was meant (the mistyped case a host-implemented ObjectProvider can produce and
// no loader validates), and absent.
func shapeFixture(t *testing.T, ast *rules.Node, md map[string]provider.Metadata) (*Engine, context.Context) {
	t.Helper()
	ctx := context.Background()
	store := memory.New()
	if err := store.Setup(ctx); err != nil {
		t.Fatalf("setup: %v", err)
	}
	mustSeed(t, store.PutObjectType(ctx, model.ObjectType{Name: "document", Actions: []string{"read"}}))
	mustSeed(t, store.PutPermission(ctx, model.Permission{
		ID: "p-shape", ObjectType: "document", Action: "read", ScopeStrategy: "inclusive;rule=tagged",
	}))
	mustSeed(t, store.PutPrincipal(ctx, model.Principal{ID: "alice", Kind: model.PrincipalUser, Identity: "user:alice"}))
	mustSeed(t, store.PutGrant(ctx, model.Grant{
		ID: "g-shape", AccountID: acctAcme, Subject: model.Subject{Kind: model.SubjectPrincipal, ID: "alice"},
		PermissionID: "p-shape", Object: "account:acme/**", Effect: model.EffectAllow,
	}))

	reg := provider.NewRegistry()
	reg.MustRegister("document", metaProvider{md: md})
	rulesEng := rules.NewEngine(rules.MapSource{"tagged": {Name: "tagged", AST: ast}}, reg)
	return New(store, WithScopeResolution(scope.DefaultRegistry(), ScopeDeps{Lister: reg, Rules: rulesEng})), ctx
}

func shapeMetadata() map[string]provider.Metadata {
	return map[string]provider.Metadata{
		"account:acme/document:good":     {"tags": []any{"urgent"}},
		"account:acme/document:mistyped": {"tags": "urgent"},
		"account:acme/document:absent":   {"title": "no tags here"},
	}
}

// TestShapeMismatchIsDeniedNotErrored is the decision-surface half of the policy:
// a rule that reads a wrong-shaped metadata field DENIES rather than failing the
// whole request. Before E5-S1 this raised APERTURE_RULE_EVAL out of Check, so one
// mistyped field broke every decision that touched it.
func TestShapeMismatchIsDeniedNotErrored(t *testing.T) {
	eng, ctx := shapeFixture(t,
		rules.Compare(rules.OpHas, rules.Var("object.tags"), rules.Lit("urgent")), shapeMetadata())

	// The well-formed object is still selected: the policy changes only the
	// mismatch case.
	good, err := eng.Check(ctx, Request{Account: acctAcme, Principal: "alice", Action: "read", Object: "account:acme/document:good"})
	if err != nil {
		t.Fatalf("Check(good): %v", err)
	}
	if !good.Allow {
		t.Fatalf("the well-shaped object should still be allowed: %+v", good)
	}

	// The mistyped object denies, with no error at all.
	bad, err := eng.Check(ctx, Request{Account: acctAcme, Principal: "alice", Action: "read", Object: "account:acme/document:mistyped"})
	if err != nil {
		t.Fatalf("a shape mismatch must not fail Check: %v (code %s)", err, aerr.CodeOf(err))
	}
	if bad.Allow {
		t.Fatalf("a shape mismatch must deny: %+v", bad)
	}

	// Enumerate must agree with Check on the very same grant: the deny-safe policy
	// is a property of the decision, not of one endpoint. A rule-backed inclusive
	// grant enumerates by listing the type and keeping what the rule selects, so
	// the mistyped object is filtered out of the member list rather than failing
	// the whole enumeration.
	members, err := eng.Enumerate(ctx, EnumerateRequest{
		Account: acctAcme, Principal: "alice", Action: "read", Pattern: "account:acme/**"})
	if err != nil {
		t.Fatalf("a shape mismatch must not fail Enumerate: %v (code %s)", err, aerr.CodeOf(err))
	}
	if len(members) != 1 || members[0] != "account:acme/document:good" {
		t.Fatalf("members = %v, want only the well-shaped document", members)
	}
}

// TestExplainSurfacesShapeMismatchNotes is the diagnostic half: the denial is not
// silent. The trace names the grant, the rule, the variable path, the shape
// expected and the shape found — and the rendered report prints it.
func TestExplainSurfacesShapeMismatchNotes(t *testing.T) {
	eng, ctx := shapeFixture(t,
		rules.Compare(rules.OpHas, rules.Var("object.tags"), rules.Lit("urgent")), shapeMetadata())

	tr, err := eng.Explain(ctx, Request{Account: acctAcme, Principal: "alice", Action: "read", Object: "account:acme/document:mistyped"})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if tr.Decision.Allow {
		t.Fatalf("a shape mismatch must deny\n%s", tr.String())
	}
	if len(tr.Notes) != 1 {
		t.Fatalf("trace notes = %+v, want exactly one", tr.Notes)
	}
	want := EvaluationNote{
		GrantID: "g-shape", Rule: "tagged", Kind: "shape_mismatch", Op: rules.OpHas,
		Path: "object.tags", Expected: "collection", Actual: "string",
		Message: "object.tags: expected collection, got string",
	}
	if tr.Notes[0] != want {
		t.Fatalf("note = %+v, want %+v", tr.Notes[0], want)
	}
	if report := tr.String(); !strings.Contains(report, want.Message) {
		t.Fatalf("the rendered trace must print the note:\n%s", report)
	}

	// A well-shaped object produces no notes at all — the channel is silent when
	// there is nothing to say.
	clean, err := eng.Explain(ctx, Request{Account: acctAcme, Principal: "alice", Action: "read", Object: "account:acme/document:good"})
	if err != nil {
		t.Fatalf("Explain(good): %v", err)
	}
	if len(clean.Notes) != 0 {
		t.Fatalf("a well-shaped object should record no notes, got %+v", clean.Notes)
	}
	if strings.Contains(clean.String(), "evaluation notes") {
		t.Fatalf("the rendered trace should omit an empty notes section:\n%s", clean.String())
	}
}

// TestExplainSurfacesAbsentFieldGrantNotes covers the other diagnostic on the
// channel: a NEGATIVE operator matching because the field is MISSING. That grants
// access on the strength of absent data and is invisible in the verdict.
func TestExplainSurfacesAbsentFieldGrantNotes(t *testing.T) {
	eng, ctx := shapeFixture(t,
		rules.Compare(rules.OpHasNone, rules.Var("object.tags"), rules.List(rules.Lit("blocked"))), shapeMetadata())

	tr, err := eng.Explain(ctx, Request{Account: acctAcme, Principal: "alice", Action: "read", Object: "account:acme/document:absent"})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if !tr.Decision.Allow {
		t.Fatalf("hasNone over an absent field matches, so the grant covers\n%s", tr.String())
	}
	if len(tr.Notes) != 1 || tr.Notes[0].Kind != "absent_field" {
		t.Fatalf("trace notes = %+v, want one absent_field note", tr.Notes)
	}
	if got, want := tr.Notes[0].Path, "object.tags"; got != want {
		t.Fatalf("note path = %q, want %q", got, want)
	}
	if report := tr.String(); !strings.Contains(report, tr.Notes[0].Message) {
		t.Fatalf("the rendered trace must print the note:\n%s", report)
	}
}

// TestTraceNoteNeverCarriesTheValue is the CLAUDE.md non-negotiable at the
// decision surface: a trace crosses account boundaries, so a note may name a path
// and a shape but never the metadata value that produced it.
func TestTraceNoteNeverCarriesTheValue(t *testing.T) {
	const marker = "CROSS-ACCOUNT-SECRET"
	eng, ctx := shapeFixture(t,
		rules.Compare(rules.OpHas, rules.Var("object.tags"), rules.Lit("urgent")),
		map[string]provider.Metadata{"account:acme/document:1": {"tags": marker}})

	tr, err := eng.Explain(ctx, Request{Account: acctAcme, Principal: "alice", Action: "read", Object: "account:acme/document:1"})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(tr.Notes) == 0 {
		t.Fatal("fixture recorded no notes; the assertion would be vacuous")
	}
	for _, n := range tr.Notes {
		for _, field := range []string{n.GrantID, n.Rule, n.Kind, n.Op, n.Path, n.Expected, n.Actual, n.Message} {
			if strings.Contains(field, marker) {
				t.Fatalf("trace note leaked a metadata value: %q in %+v", field, n)
			}
		}
	}
	if strings.Contains(tr.String(), marker) {
		t.Fatalf("the rendered trace leaked a metadata value:\n%s", tr.String())
	}
}

// dateFixtureMetadata is the date half of the same story: a well-formed date, a
// string a host wrote in some other format (which no loader validated, because a
// host-implemented ObjectProvider bypasses them all), a value of the wrong shape
// entirely, and an object missing the field.
func dateFixtureMetadata() map[string]provider.Metadata {
	return map[string]provider.Metadata{
		"account:acme/document:good":     {"hired_at": "2026-03-04"},
		"account:acme/document:mistyped": {"hired_at": "04/03/2026"},
		"account:acme/document:shape":    {"hired_at": 20260304},
		"account:acme/document:absent":   {"title": "no hire date here"},
	}
}

// TestDateOperandIsDeniedNotErrored is the decision-surface half of the date
// policy: a rule reading a malformed date DENIES rather than failing the whole
// request, on Check and on Enumerate alike, and the well-formed object is
// unaffected.
func TestDateOperandIsDeniedNotErrored(t *testing.T) {
	eng, ctx := dateFixture(t)

	good, err := eng.Check(ctx, Request{Account: acctAcme, Principal: "alice", Action: "read", Object: "account:acme/document:good"})
	if err != nil {
		t.Fatalf("Check(good): %v", err)
	}
	if !good.Allow {
		t.Fatalf("the well-formed date should be allowed: %+v", good)
	}

	for _, id := range []string{"mistyped", "shape", "absent"} {
		object := "account:acme/document:" + id
		res, err := eng.Check(ctx, Request{Account: acctAcme, Principal: "alice", Action: "read", Object: object})
		if err != nil {
			t.Fatalf("a malformed date must not fail Check(%s): %v (code %s)", id, err, aerr.CodeOf(err))
		}
		if res.Allow {
			t.Fatalf("a malformed date must deny (%s): %+v", id, res)
		}
	}

	members, err := eng.Enumerate(ctx, EnumerateRequest{
		Account: acctAcme, Principal: "alice", Action: "read", Pattern: "account:acme/**"})
	if err != nil {
		t.Fatalf("a malformed date must not fail Enumerate: %v (code %s)", err, aerr.CodeOf(err))
	}
	if len(members) != 1 || members[0] != "account:acme/document:good" {
		t.Fatalf("members = %v, want only the well-formed document", members)
	}
}

// TestExplainSurfacesDateNotes is the diagnostic half: each date failure reaches
// the trace with its own kind, naming the path and the reason and nothing else.
func TestExplainSurfacesDateNotes(t *testing.T) {
	eng, ctx := dateFixture(t)

	cases := []struct {
		object string
		want   EvaluationNote
	}{
		{"mistyped", EvaluationNote{
			GrantID: "g-shape", Rule: "tagged", Kind: "date_invalid", Op: rules.OpBefore,
			Path: "object.hired_at", Expected: "2006-01-02 or 2006-01-02T15:04:05Z",
			Message: "object.hired_at: not a canonical date; before expects 2006-01-02 or 2006-01-02T15:04:05Z",
		}},
		{"shape", EvaluationNote{
			GrantID: "g-shape", Rule: "tagged", Kind: "shape_mismatch", Op: rules.OpBefore,
			Path: "object.hired_at", Expected: "date", Actual: "number",
			Message: "object.hired_at: expected date, got number",
		}},
		{"absent", EvaluationNote{
			GrantID: "g-shape", Rule: "tagged", Kind: "shape_mismatch", Op: rules.OpBefore,
			Path: "object.hired_at", Expected: "date", Actual: "absent",
			Message: "object.hired_at: expected date, got absent",
		}},
	}

	for _, tc := range cases {
		t.Run(tc.object, func(t *testing.T) {
			tr, err := eng.Explain(ctx, Request{Account: acctAcme, Principal: "alice", Action: "read",
				Object: "account:acme/document:" + tc.object})
			if err != nil {
				t.Fatalf("Explain: %v", err)
			}
			if tr.Decision.Allow {
				t.Fatalf("a malformed date must deny\n%s", tr.String())
			}
			if len(tr.Notes) != 1 {
				t.Fatalf("trace notes = %+v, want exactly one", tr.Notes)
			}
			if tr.Notes[0] != tc.want {
				t.Fatalf("note = %+v, want %+v", tr.Notes[0], tc.want)
			}
			if report := tr.String(); !strings.Contains(report, tc.want.Message) {
				t.Fatalf("the rendered trace must print the note:\n%s", report)
			}
		})
	}

	// The well-formed object records nothing at all.
	clean, err := eng.Explain(ctx, Request{Account: acctAcme, Principal: "alice", Action: "read",
		Object: "account:acme/document:good"})
	if err != nil {
		t.Fatalf("Explain(good): %v", err)
	}
	if len(clean.Notes) != 0 {
		t.Fatalf("a well-formed date should record no notes, got %+v", clean.Notes)
	}
}

// TestExplainSurfacesInvertedBoundsNote covers the authoring-error diagnostic:
// `between` written backwards denies every object, and without the note that is
// indistinguishable from a window nothing falls in.
func TestExplainSurfacesInvertedBoundsNote(t *testing.T) {
	eng, ctx := shapeFixture(t,
		rules.Between(rules.Var("object.hired_at"), rules.Lit("2026-12-31"), rules.Lit("2026-01-01")),
		dateFixtureMetadata())

	tr, err := eng.Explain(ctx, Request{Account: acctAcme, Principal: "alice", Action: "read",
		Object: "account:acme/document:good"})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if tr.Decision.Allow {
		t.Fatalf("an inverted range matches nothing\n%s", tr.String())
	}
	if len(tr.Notes) != 1 || tr.Notes[0].Kind != "date_bounds_inverted" {
		t.Fatalf("trace notes = %+v, want one date_bounds_inverted note", tr.Notes)
	}
	if got, want := tr.Notes[0].Path, "object.hired_at"; got != want {
		t.Fatalf("note path = %q, want %q", got, want)
	}
	if report := tr.String(); !strings.Contains(report, tr.Notes[0].Message) {
		t.Fatalf("the rendered trace must print the note:\n%s", report)
	}
}

// TestTraceDateNoteNeverCarriesTheValue is the leak guard for dates specifically.
// A date is often personal data — a birth date, a termination date — so it is
// exactly the kind of value a trace crossing account boundaries must not carry.
func TestTraceDateNoteNeverCarriesTheValue(t *testing.T) {
	const marker = "1974-11-02-CROSS-ACCOUNT"
	eng, ctx := shapeFixture(t,
		rules.Compare(rules.OpBefore, rules.Var("object.hired_at"), rules.Lit("2026-01-01")),
		map[string]provider.Metadata{"account:acme/document:1": {"hired_at": marker}})

	tr, err := eng.Explain(ctx, Request{Account: acctAcme, Principal: "alice", Action: "read", Object: "account:acme/document:1"})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(tr.Notes) == 0 {
		t.Fatal("fixture recorded no notes; the assertion would be vacuous")
	}
	for _, n := range tr.Notes {
		for _, field := range []string{n.GrantID, n.Rule, n.Kind, n.Op, n.Path, n.Expected, n.Actual, n.Message} {
			if strings.Contains(field, marker) {
				t.Fatalf("trace note leaked a date value: %q in %+v", field, n)
			}
		}
	}
	if strings.Contains(tr.String(), marker) {
		t.Fatalf("the rendered trace leaked a date value:\n%s", tr.String())
	}
}

// dateFixture builds the shared date engine: one rule comparing object.hired_at
// against a fixed cutoff, over dateFixtureMetadata.
func dateFixture(t *testing.T) (*Engine, context.Context) {
	t.Helper()
	return shapeFixture(t,
		rules.Compare(rules.OpBefore, rules.Var("object.hired_at"), rules.Lit("2026-12-31")),
		dateFixtureMetadata())
}
