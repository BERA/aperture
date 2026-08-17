package rules

import (
	"context"
	"sync"
	"time"
)

// This file is the DECISION INSTANT: the single reference "now" a decision is
// evaluated against.
//
// Why it exists, and why it is not just a call to time.Now(). Rule evaluation is
// otherwise pure — every expr-lang builtin is disabled, so nothing reachable from
// a rule can read a wall clock — and date comparisons have to keep that property.
// A rule that asks "was this hired in the last quarter" needs a reference
// instant, and if that instant were read afresh at each reference then one rule
// could compare against two different "now"s, and an Explain trace could not be
// reproduced against anything.
//
// So the instant is READ ONCE PER DECISION and threaded through evaluation as
// ordinary data:
//
//	Clock -> Engine.Selected -> Input.Now -> evalEnv.Now (identifier "__now")
//
// The clock is the engine's ONE time source (rules.WithClock): the same Clock
// that expires the compiled-rule cache produces the decision instant. Two
// independent ideas of "now" inside one engine can disagree, and that is a worse
// failure than the coupling — the coupling's visible cost is that pinning the
// clock for a date fixture also freezes cache expiry, which is documented on
// WithClock and in skills/rules-engine.md.
//
// SCOPE. A decision can evaluate several rules — one per candidate grant for a
// Check, once per candidate and once per member gather for an Enumerate, once per
// grant for an Explain. Without a scope, each of those would take its own
// snapshot and a slow decision could straddle midnight halfway through. The
// decision engine therefore opens a scope (WithDecisionInstant) at the decision
// boundary; every rule evaluation underneath it shares the FIRST instant taken.
// Outside a scope — a bare Engine.Selected, a direct library caller — each
// evaluation takes its own snapshot from the same clock, which is the same
// guarantee narrowed to one evaluation.
//
// The scope is a MEMO, not an injection point: the value always comes from the
// engine's Clock, never from the caller. There is deliberately no API for a
// caller to supply its own instant. Two callers disagreeing about "now" is
// exactly the defect this seam exists to prevent.
//
// UTC. The instant is converted at the snapshot boundary and again when the
// evaluation environment is built, so no zone ever reaches a comparison however
// an injected Clock is implemented. There is no timezone knob anywhere in the
// engine.
//
// NOT A VARIABLE ROOT. The instant is exposed to the compiled program under the
// identifier "__now" (nowVar), which is absent from allowedRoots — so no NodeVar
// can name it, exactly as with the notes sink "__notes". That is a real boundary
// rather than a stylistic one: reflective METHOD calls survive
// expr.DisableAllBuiltins, so an exposed `NOW` root would make
// `NOW.AddDate(0, -3, 0)` and `NOW.Unix()` well-formed var paths — an unclamped
// calendar walk reachable from any rule. A relative-date node renders literal
// arguments to a $-prefixed dispatcher and reads __now as one of its arguments,
// the way $date already reads __notes.

// DecisionInstant is the per-decision snapshot of the engine's clock, shared by
// every rule evaluation performed under one decision.
//
// It is a MEMO with no setter: the first evaluation to need an instant fills it
// from the engine's Clock and every later one reads that same value back. A
// caller cannot choose the value, only the scope it spans.
//
// A nil *DecisionInstant is a valid, inert scope — every method is a no-op and a
// snapshot taken through it simply reads the clock — which is what lets the
// evaluator carry an "unscoped" instant without branching.
//
// DecisionInstant is safe for concurrent use: one decision can fan out across
// goroutines and still agree on when it happened.
type DecisionInstant struct {
	mu    sync.Mutex
	at    time.Time
	taken bool
}

// At returns the instant this decision was evaluated against and whether one was
// ever taken. It reports false when the decision evaluated no rule at all — a
// literal-scope grant needs no reference instant, so none is invented for it.
// Safe on a nil receiver.
func (d *DecisionInstant) At() (time.Time, bool) {
	if d == nil {
		return time.Time{}, false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.at, d.taken
}

// snapshot returns the decision's instant, reading clk exactly once per scope.
// Safe on a nil receiver, which is the unscoped case: the clock is read and the
// value is not retained.
//
// The result is always UTC. Conversion happens here, at the boundary, so an
// injected Clock that answers in a local zone cannot leak one into evaluation.
func (d *DecisionInstant) snapshot(clk Clock) time.Time {
	if d == nil {
		return clk.Now().UTC()
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.taken {
		d.at = clk.Now().UTC()
		d.taken = true
	}
	return d.at
}

// decisionInstantKey is the private context key the scope travels under. It is a
// distinct empty struct type, so no other package can collide with it.
type decisionInstantKey struct{}

// WithDecisionInstant opens a decision scope on ctx and returns it with the
// scope's DecisionInstant. Every rule evaluation performed under the returned
// context shares ONE instant — the first one any of them needs.
//
// It is IDEMPOTENT: a context that already carries a scope is returned unchanged
// along with the scope it already has. That is what makes nesting safe, and it is
// load-bearing — Enumerate opens a scope for the whole enumeration and then runs
// the ordinary per-candidate decision underneath it, which opens one of its own.
// Without idempotence the inner scope would shadow the outer one and every
// candidate would get its own instant.
//
// The decision engine calls this at each decision boundary. A host that drives
// rules.Engine directly does not have to: an unscoped evaluation snapshots the
// same clock, just once per evaluation instead of once per decision.
func WithDecisionInstant(ctx context.Context) (context.Context, *DecisionInstant) {
	if d := decisionInstantFrom(ctx); d != nil {
		return ctx, d
	}
	d := &DecisionInstant{}
	return context.WithValue(ctx, decisionInstantKey{}, d), d
}

// decisionInstantFrom returns the scope installed in ctx, or nil when none is.
// The nil result is usable directly: every DecisionInstant method is nil-safe.
func decisionInstantFrom(ctx context.Context) *DecisionInstant {
	if ctx == nil {
		return nil
	}
	d, _ := ctx.Value(decisionInstantKey{}).(*DecisionInstant)
	return d
}
