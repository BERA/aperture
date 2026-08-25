package rules

import (
	"context"
	"sync"
)

// This file is the DECISION ATTRIBUTES: the ONE principal bag and the ONE account
// bag a decision is evaluated against. It is now.go's twin — same shape, same
// rationale, deliberately readable as one pattern rather than two.
//
// Why it exists, and why it is not just a call to the resolver. Object metadata
// is legitimately per object: a decision that touches a thousand objects reads a
// thousand metadata bags, and each one describes something different. The
// principal and the account are not like that. Both are CONSTANT for the whole
// decision — the same principal is asking about every object, inside the same
// tenancy — so resolving them per evaluation is a host round-trip inside a loop,
// and an N-object enumeration performs N principal fetches to learn the same
// fact N times.
//
// The speed is the visible half. The half that decides correctness is
// CONSISTENCY. An attribute bag is served through a cache with a TTL, and a TTL
// that expires halfway through an enumeration means the first objects were
// judged against one version of the principal and the last against another: a
// result set that no single view of the principal justifies, and no error
// anywhere saying so. That is precisely the defect a decision straddling a tick
// has (see now.go), and it takes precisely the same fix.
//
// So the bags are RESOLVED ONCE PER DECISION and threaded through evaluation as
// ordinary data:
//
//	PrincipalResolver -> DecisionAttributes -> principalBag -> Input.Principal
//	AccountResolver   -> DecisionAttributes -> accountBag   -> Input.Account
//
// SCOPE. A decision can evaluate several rules — one per candidate grant for a
// Check, once per candidate and once per member gather for an Enumerate, once
// per grant for an Explain. Without a scope each of those would resolve its own
// bags. The decision engine therefore opens a scope (WithDecisionAttributes) at
// the same three boundaries it opens the decision instant at, and every
// evaluation underneath it shares the FIRST bags resolved. Outside a scope — a
// bare Engine.Selected, a host driving rules.Engine directly — each evaluation
// resolves its own, which is the same guarantee narrowed to one evaluation. The
// scope is never mandatory: an unscoped evaluation is correct, merely unmemoized.
//
// A MEMO, NOT A CACHE. Two properties follow from the word, and both are load
// bearing. It holds ONE principal bag and ONE account bag, not a map of them, so
// it cannot grow with what a decision happens to look at and it cannot outlive
// the decision — the scope is a context value and dies with the request. And it
// is not an INJECTION POINT: there is no API for a caller to hand in a bag. The
// value always comes from the engine's resolver, exactly as the decision
// instant always comes from the engine's Clock, because a caller-supplied
// principal bag is a caller-supplied answer to "who is asking".
//
// KEYED, so it cannot answer for the wrong subject. The memo records which
// subject each bag was resolved for and re-resolves on a mismatch. Within a
// decision that never happens — a decision has one principal and one active
// account — but the scope travels on a context, and a context can be passed
// somewhere the author did not picture. Serving one principal's attributes as
// another's is the worst failure this seam has, and a key makes it impossible by
// construction rather than by convention. The failure mode of a mismatch is a
// re-fetch: correct and slow, which is the right direction.
//
// FAILURES ARE NOT MEMOIZED; ABSENCES ARE. Which codes are lenient stays the
// resolver's contract (see provider.AttributeRegistry.Attributes) and is not
// decided here, but the two outcomes it produces land on opposite sides of this
// memo, and that split is deliberate:
//
//   - An ABSENCE — no provider wired for the slot, or a directory with no record
//     for this subject — reaches the memo as a successful resolution of a nil
//     bag, and is retained like any other. "The host knows nothing about this
//     subject" is a complete answer, it describes a steady state rather than a
//     blip, and re-resolving it per object would reintroduce exactly the
//     TTL-straddling inconsistency this type exists to remove. A 1,000-object
//     enumeration against an unwired slot therefore performs ONE resolution. It
//     is also what lets Principal/Account report (nil, true) — resolved, and
//     empty — which is the distinction a floor-only decision trace is built on.
//   - A FAILURE is not retained, so a directory that blinks for one round trip
//     does not decide the rest of the decision. The cost of that choice is
//     bounded and small: every consumer of a rule verdict treats an error as a
//     NON-DECISION and returns immediately (scope's resolvers stop enumerating
//     on the first error; the decision engine stops on the first grant), so
//     "retried" means at most one further attempt in this decision, never one
//     per object. Freezing the blip instead would be unbounded in the direction
//     that matters — a wrong answer, held for the whole decision.
//
// READ-ONLY, AND MORE SO THAN BEFORE. A resolved bag was always read-only
// transitively (see PrincipalResolver, AccountResolver, and provider's
// attribute.go). Memoizing widens who holds it: one bag is now shared by every
// evaluation in the decision, on top of being shared by every concurrent
// decision for the same key through the provider's cache. A single write through
// it at any depth is therefore not one bad evaluation but every rule, for every
// object, in every in-flight decision for that subject. The engine's own
// consumption honours this — principalBag and accountBag stamp the floor into a
// FRESH map and never into the resolver's — and every other holder must too.

// DecisionAttributes is the per-decision memo of the principal and account
// attribute bags, shared by every rule evaluation performed under one decision.
//
// It is a MEMO with no setter: the first evaluation that needs a bag resolves it
// through the engine's resolver and every later one reads that same bag back. A
// caller cannot choose the value, only the scope it spans.
//
// A nil *DecisionAttributes is a valid, inert scope — every method is a no-op and
// a resolution through it simply calls the resolver — which is what lets the
// evaluator run unscoped without branching.
//
// DecisionAttributes is safe for concurrent use: one decision can fan out across
// goroutines and still agree on who is asking. The fan-out is also why the
// resolver is called with the memo's lock HELD: releasing it around the call
// would let N concurrent evaluations start N identical fetches and then discard
// N-1 of them, which is the round-trip-per-object this type exists to remove.
// The cost is that concurrent evaluations in one decision wait for the first
// fetch, which is the shape of the guarantee, not a side effect of it.
type DecisionAttributes struct {
	principal attributeMemo
	account   attributeMemo
}

// Principal returns the bag this decision's principal was resolved to and
// whether one was ever resolved. It reports false when the decision evaluated no
// rule, or evaluated only rules reached before any resolution succeeded — no bag
// is invented for a decision that never needed one.
//
// The returned bag is READ-ONLY, transitively: it is the resolver's own map,
// shared with every evaluation in the decision. Safe on a nil receiver.
func (d *DecisionAttributes) Principal() (map[string]any, bool) {
	if d == nil {
		return nil, false
	}
	return d.principal.value()
}

// Account returns the bag this decision's ACTIVE account was resolved to and
// whether one was ever resolved. It is Principal's counterpart in every respect,
// including the read-only contract and nil-receiver safety.
//
// It reports false for a decision made at the account wildcard: "*" is not an
// account and never reaches a resolver (see Engine.accountAttributes), so there
// is no bag to report rather than an empty one.
func (d *DecisionAttributes) Account() (map[string]any, bool) {
	if d == nil {
		return nil, false
	}
	return d.account.value()
}

// attributeMemo is one slot of the decision memo: the subject a bag was resolved
// for, the bag, and whether one was ever taken.
//
// kind is the principal kind for the principal slot and is EMPTY for the account
// slot, which has no kind — the same asymmetry the two floors have, and for the
// same reason (see accountBag: one account slot means a `kind` key could never
// discriminate).
type attributeMemo struct {
	mu    sync.Mutex
	kind  string
	id    string
	bag   map[string]any
	taken bool
}

// value returns the memoized bag and whether one was taken.
func (m *attributeMemo) value() (map[string]any, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.bag, m.taken
}

// principalAttributes returns the decision's principal bag, calling r at most
// once per scope for a given (kind, principal). Safe on a nil receiver, which is
// the unscoped case: the resolver is called and the answer is not retained.
//
// A resolution for a DIFFERENT subject replaces the memo rather than being
// rejected or accumulated: the memo describes the decision's current subject, and
// keeping both would make this a cache (see the file doc).
func (d *DecisionAttributes) principalAttributes(ctx context.Context, r PrincipalResolver, kind, principal string) (map[string]any, error) {
	if d == nil {
		return r.Attributes(ctx, kind, principal)
	}
	m := &d.principal
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.taken && m.kind == kind && m.id == principal {
		return m.bag, nil
	}
	bag, err := r.Attributes(ctx, kind, principal)
	if err != nil {
		// Deliberately not memoized: a transient directory failure must not be
		// frozen into every later evaluation in this decision, and it cannot
		// cost a fetch per object because the error ENDS the decision. An
		// absence does not arrive here at all — the resolver reports it as a nil
		// bag and no error, and it memoizes. See the file doc.
		return nil, err
	}
	m.kind, m.id, m.bag, m.taken = kind, principal, bag, true
	return bag, nil
}

// accountAttributes returns the decision's ACTIVE account bag, calling r at most
// once per scope for a given account. It is principalAttributes' counterpart in
// every respect — nil-safe, keyed, replacing on a mismatch, and never memoizing a
// failure.
func (d *DecisionAttributes) accountAttributes(ctx context.Context, r AccountResolver, account string) (map[string]any, error) {
	if d == nil {
		return r.AccountAttributes(ctx, account)
	}
	m := &d.account
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.taken && m.id == account {
		return m.bag, nil
	}
	bag, err := r.AccountAttributes(ctx, account)
	if err != nil {
		return nil, err
	}
	m.id, m.bag, m.taken = account, bag, true
	return bag, nil
}

// decisionAttributesKey is the private context key the scope travels under. It is
// a distinct empty struct type, so no other package can collide with it.
type decisionAttributesKey struct{}

// WithDecisionAttributes opens an attribute scope on ctx and returns it with the
// scope's DecisionAttributes. Every rule evaluation performed under the returned
// context shares ONE principal bag and ONE account bag — the first ones any of
// them resolves.
//
// It is IDEMPOTENT, for the same reason WithDecisionInstant is: Enumerate opens a
// scope for the whole enumeration and then runs the ordinary per-candidate
// decision underneath it, which opens one of its own. Without idempotence the
// inner scope would shadow the outer one and every candidate would resolve its
// own bags — the per-object fetch this seam removes.
//
// The decision engine calls this at each decision boundary (Check, Enumerate,
// Explain), paired with WithDecisionInstant. A host that drives rules.Engine
// directly does not have to: an unscoped evaluation resolves its own bags, just
// once per evaluation instead of once per decision.
func WithDecisionAttributes(ctx context.Context) (context.Context, *DecisionAttributes) {
	if d := decisionAttributesFrom(ctx); d != nil {
		return ctx, d
	}
	d := &DecisionAttributes{}
	return context.WithValue(ctx, decisionAttributesKey{}, d), d
}

// decisionAttributesFrom returns the scope installed in ctx, or nil when none is.
// The nil result is usable directly: every DecisionAttributes method is nil-safe,
// so the evaluator never branches on whether it is scoped.
func decisionAttributesFrom(ctx context.Context) *DecisionAttributes {
	if ctx == nil {
		return nil
	}
	d, _ := ctx.Value(decisionAttributesKey{}).(*DecisionAttributes)
	return d
}
