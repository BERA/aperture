package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/provider"
	"github.com/frankbardon/aperture/rules"
	"github.com/frankbardon/aperture/scope"
	"github.com/frankbardon/aperture/storage/memory"
)

// pinnedClock is a rules.Clock frozen at one instant. Time-dependent decisions
// are tested against a pinned clock and never against time.Now(): an expectation
// derived from the real clock only exercises the interesting calendar path on the
// days the calendar happens to cooperate.
type pinnedClock struct{ at time.Time }

func (c pinnedClock) Now() time.Time { return c.at }

// ruleBackedFixture wires the same stack TestExplain_ScopeAndRule does — memory
// storage, a provider registry as both metadata fetcher and object lister, and a
// rules engine as the rule evaluator — but lets the caller pin the rules engine's
// clock. It returns the decision engine and the object the rule selects.
func ruleBackedFixture(t *testing.T, opts ...rules.Option) *Engine {
	t.Helper()
	ctx := context.Background()
	store := memory.New()
	if err := store.Setup(ctx); err != nil {
		t.Fatalf("setup: %v", err)
	}
	mustSeed(t, store.PutObjectType(ctx, model.ObjectType{Name: "document", Actions: []string{"read"}}))
	mustSeed(t, store.PutPermission(ctx, model.Permission{
		ID: "p-rule", ObjectType: "document", Action: "read", ScopeStrategy: "inclusive;rule=sensitive",
	}))
	mustSeed(t, store.PutPrincipal(ctx, model.Principal{
		ID: "alice", Kind: model.PrincipalUser, Identity: "user:alice",
	}))
	mustSeed(t, store.PutGrant(ctx, model.Grant{
		ID: "g-rule", AccountID: acctAcme, Subject: model.Subject{Kind: model.SubjectPrincipal, ID: "alice"},
		PermissionID: "p-rule", Object: "account:acme/**", Effect: model.EffectAllow,
	}))

	reg := provider.NewRegistry()
	reg.MustRegister("document", metaProvider{md: map[string]provider.Metadata{
		"account:acme/document:secret": {"level": "secret"},
	}})

	rule := &rules.Rule{
		Name: "sensitive",
		AST:  rules.Compare(rules.OpEq, rules.Var("object.level"), rules.Lit("secret")),
	}
	rulesEng := rules.NewEngine(rules.MapSource{"sensitive": rule}, reg, opts...)

	return New(store, WithScopeResolution(scope.DefaultRegistry(), ScopeDeps{Lister: reg, Rules: rulesEng}))
}

// TestExplainRecordsTheDecisionInstant pins the reproducibility contract: a trace
// says WHEN it decided, so a date-sensitive verdict can be replayed against the
// same instant instead of against whatever "now" happens to be at replay time.
// The instant is the rules engine's clock — the engine's one time source — not
// the decision engine's grant-expiry clock, which is a separate seam.
func TestExplainRecordsTheDecisionInstant(t *testing.T) {
	pinned := time.Date(2026, 3, 31, 23, 59, 59, 0, time.UTC)
	eng := ruleBackedFixture(t, rules.WithClock(pinnedClock{at: pinned}))

	tr, err := eng.Explain(context.Background(), Request{
		Account: acctAcme, Principal: "alice", Action: "read", Object: "account:acme/document:secret",
	})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if !tr.Decision.Allow {
		t.Fatalf("the rule selects the secret document; trace says deny\n%s", tr.String())
	}
	if !tr.Now.Equal(pinned) {
		t.Fatalf("Trace.Now = %s, want the pinned %s", tr.Now, pinned)
	}
	if tr.Now.Location() != time.UTC {
		t.Fatalf("Trace.Now location = %v, want UTC", tr.Now.Location())
	}
}

// TestExplainInstantIsUTCWhateverTheClockSays proves the zone guarantee holds at
// the trace too: an injected clock is host code and may answer in any zone, and
// the conversion happens at the snapshot boundary rather than being asked of the
// clock.
func TestExplainInstantIsUTCWhateverTheClockSays(t *testing.T) {
	zone := time.FixedZone("plus5", 5*60*60)
	local := time.Date(2026, 4, 1, 4, 59, 59, 0, zone)
	eng := ruleBackedFixture(t, rules.WithClock(pinnedClock{at: local}))

	tr, err := eng.Explain(context.Background(), Request{
		Account: acctAcme, Principal: "alice", Action: "read", Object: "account:acme/document:secret",
	})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if tr.Now.Location() != time.UTC {
		t.Fatalf("Trace.Now location = %v, want UTC", tr.Now.Location())
	}
	if !tr.Now.Equal(local) {
		t.Fatalf("Trace.Now = %s, want the same instant as %s", tr.Now, local)
	}
	// The zone shift crosses a calendar day: recording the clock's own wall time
	// would have put this trace in March.
	if y, m, d := tr.Now.Date(); y != 2026 || m != time.March || d != 31 {
		t.Fatalf("Trace.Now = %s, want the UTC calendar day 2026-03-31", tr.Now)
	}
}

// TestExplainWithoutRulesRecordsNoInstant: a decision that consults no rule needs
// no reference instant, and none is invented. The zero value is the honest answer
// — stamping a "now" nothing looked at would make every literal-scope trace
// falsely time-sensitive.
func TestExplainWithoutRulesRecordsNoInstant(t *testing.T) {
	f := newFixture(t)
	f.principal("alice")
	f.grant("g-read", acctAcme, subjPrincipal("alice"), model.EffectAllow, permRead, "account:acme/**")

	tr, err := f.eng.Explain(context.Background(), Request{
		Account: acctAcme, Principal: "alice", Action: "read", Object: "account:acme/document:42",
	})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if !tr.Now.IsZero() {
		t.Fatalf("Trace.Now = %s, want the zero instant for a decision that evaluated no rule", tr.Now)
	}
}

// TestTraceStringOmitsTheInstant keeps String's byte-identical promise intact.
// The instant is a field, not a report line: two explains of one decision taken a
// second apart must still render identically, or every diffed trace grows a
// phantom change.
func TestTraceStringOmitsTheInstant(t *testing.T) {
	base := Trace{
		Request:  Request{Account: acctAcme, Principal: "alice", Action: "read", Object: "document:42"},
		Decision: Decision{Allow: true, Reason: "allowed"},
	}
	stamped := base
	stamped.Now = time.Date(2026, 3, 31, 23, 59, 59, 0, time.UTC)

	if got, want := stamped.String(), base.String(); got != want {
		t.Fatalf("the recorded instant changed the rendered report:\n--- want ---\n%s\n--- got ---\n%s", want, got)
	}
	if strings.Contains(stamped.String(), "2026") {
		t.Fatalf("the rendered report leaked the decision instant:\n%s", stamped.String())
	}
}

// TestEnumerateSharesOneInstant pins the widest scope: a rule-backed Enumerate
// evaluates its rule once per candidate to gather members and again per candidate
// in the decision walk, and every one of those evaluations must resolve against
// the same instant. A clock that moves on every read makes a second read visible.
func TestEnumerateSharesOneInstant(t *testing.T) {
	clk := &countingClock{at: time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC)}
	eng := ruleBackedFixture(t, rules.WithClock(clk))

	if _, err := eng.Enumerate(context.Background(), EnumerateRequest{
		Account: acctAcme, Principal: "alice", Action: "read", Pattern: "account:acme/document:*",
	}); err != nil {
		t.Fatalf("Enumerate: %v", err)
	}
	if got := clk.reads(); got != 1 {
		t.Fatalf("one enumeration read the clock %d times, want exactly 1", got)
	}
}

// countingClock advances on every read so a second read is observable, and counts
// them so the assertion can be exact.
type countingClock struct {
	at time.Time
	n  int
}

func (c *countingClock) Now() time.Time {
	c.n++
	now := c.at
	c.at = c.at.Add(time.Second)
	return now
}

func (c *countingClock) reads() int { return c.n }
