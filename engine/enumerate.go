package engine

import (
	"context"
	"sort"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/identity"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/provider"
	"github.com/frankbardon/aperture/rules"
)

// DefaultEnumerateLimit bounds Enumerate's result when the caller imposes no
// positive limit, so an enumeration can never materialise an unbounded set even
// if a provider lister would. It matches the scope/provider enumeration bound.
const DefaultEnumerateLimit = 1000

// EnumerateRequest is the input to Enumerate: the principal asking, the action,
// and the object PATTERN that bounds the search, scoped to an account. Pattern
// is an identity pattern (e.g. "account:acme/**" or "account:acme/document:*")
// that both bounds the candidate set and is intersected with each grant's own
// scope. Limit caps the number of returned ids; <= 0 means DefaultEnumerateLimit.
type EnumerateRequest struct {
	// Account is the active account the enumeration is scoped to. Mandatory.
	Account string
	// Principal is the id of the principal whose access is enumerated. Mandatory.
	Principal string
	// Action is the verb being enumerated (e.g. "read"). Mandatory.
	Action string
	// Pattern is the identity pattern bounding the search. Mandatory.
	Pattern string
	// Fields are OPTIONAL object-metadata predicates that narrow the result
	// further: an allowed object is returned only when its metadata satisfies
	// every one of them. Nil or empty (the default) filters nothing, so an
	// existing caller is unaffected.
	//
	// The meaning is provider.Filter's Fields contract verbatim — AND across
	// keys, a field ABSENT from an object never matches (not even against a nil
	// want), a COLLECTION field matches by MEMBERSHIP, everything else by
	// EQUALITY, and comparison is TYPED (int64(5) matches a float64(5) want,
	// "5" does not match a 5 want). It is evaluated by provider.MatchFields, the
	// same implementation a provider's Query uses, so an enumeration filtered
	// here and one filtered in a provider select the same objects.
	//
	// Metadata is read through the engine's metadata source (WithMetadata).
	// Filtering an object-type that has no metadata source is an APERTURE_*
	// coded error, never a silently empty result.
	Fields map[string]any
	// Limit caps the number of returned object ids. <= 0 means the default bound.
	Limit int
}

// MetadataFetcher supplies an object's metadata so Enumerate can evaluate
// EnumerateRequest.Fields against it. The signature matches
// *provider.Registry.Fetch, so a registry — with its per-type cache and TTL — is
// wired straight in as the engine's metadata source, and it matches
// rules.MetadataFetcher so a deployment wires ONE source for both. The returned
// map is READ-ONLY, transitively: Enumerate only reads it.
type MetadataFetcher interface {
	Fetch(ctx context.Context, id identity.Identity) (provider.Metadata, error)
}

// WithMetadata gives the engine the object-metadata source Enumerate's Fields
// predicate reads through — normally the *provider.Registry that already backs
// the scope lister and the rule evaluator, so a candidate's metadata is served
// from the per-type cache rather than re-pulled.
//
// It is only consulted when a request actually carries Fields; an engine wired
// without it keeps exactly today's behaviour for every unfiltered enumeration,
// and fails closed (a coded error, never an empty result that reads as "no
// access") for a filtered one.
func WithMetadata(f MetadataFetcher) Option {
	return func(e *Engine) { e.metadata = f }
}

// Enumerate returns the object ids under Pattern that Principal may take Action
// on, in the active account — the inverse of Check (FR-10). The result respects
// deny-overrides and specificity exactly as Check does: every returned id is one
// Check would allow, so a denied object is NEVER returned.
//
// Algorithm: the candidate set is the union of every ALLOW grant's covered
// objects (a scope resolver's bounded Members for implicit/inclusive/exclusive,
// or the grant's own concrete identity for literal), intersected with Pattern.
// Each candidate is then run through the same deny-overrides/specificity
// decision, so a candidate carved out by a more-specific or equal-specificity
// deny is dropped. Because any allowable object must be covered by at least one
// allow grant, gathering candidates from allow grants alone is complete; deny
// grants only ever subtract.
//
// When the request carries Fields, each candidate that SURVIVES that decision is
// then tested against the metadata predicates with provider.MatchFields, so the
// filter can only ever subtract from the allowed set — a deny-carved object is
// never returned, whatever the predicate says. The order is load-bearing: the
// candidate set is materialised and predicated BEFORE Limit truncates it, so
// asking for the first 10 objects tagged "brand:Y" searches every candidate for
// them rather than tagging the first 10 candidates and returning the few that
// stuck.
//
// Enumerate is the most cache-sensitive op, so it is deliberately bounded: each
// resolver's Members is itself limited, and the overall result is capped by
// Limit (default DefaultEnumerateLimit). Object order is deterministic (sorted
// by canonical id). An operational failure (storage, an unresolvable strategy,
// or an unconfigured lister an implicit/exclusive grant needs) is returned as an
// APERTURE_* coded error and the caller treats it as a non-result.
func (e *Engine) Enumerate(ctx context.Context, req EnumerateRequest) ([]string, error) {
	if err := validateEnumerateRequest(req); err != nil {
		return nil, err
	}
	query, err := identity.ParsePattern(req.Pattern)
	if err != nil {
		return nil, err
	}

	member, err := e.requireMembership(ctx, req.Account, req.Principal)
	if err != nil {
		return nil, err
	}
	if !member {
		// Fail-closed: a non-member may act on nothing in this account.
		return []string{}, nil
	}

	subjects, err := e.subjectSet(ctx, req.Principal)
	if err != nil {
		return nil, err
	}
	return e.enumerateWithSubjects(ctx, req, query, subjects)
}

// enumerateWithSubjects runs the Enumerate algorithm over an already-resolved
// subject set. It is shared by Enumerate (subjects = the principal's own set)
// and EnumerateAs (subjects = the impersonation-elevated set), so the impersonated
// enumeration is the exact same deny-overrides walk over a different subject set —
// no impersonation-specific decision logic. decReq.Principal stays the requesting
// principal so a rule-backed scope strategy still sees the real operator.
func (e *Engine) enumerateWithSubjects(ctx context.Context, req EnumerateRequest, query identity.Pattern, subjects []model.Subject) ([]string, error) {
	// One reference instant for the WHOLE enumeration — the member gather and
	// every per-candidate decision underneath it. A rule-backed Enumerate
	// evaluates its rule twice per candidate, so this is where a long-running
	// enumeration would otherwise straddle a tick and return a set no single
	// instant justifies.
	ctx, _ = rules.WithDecisionInstant(ctx)

	grants, err := e.store.GrantsForSubjects(ctx, req.Account, subjects)
	if err != nil {
		return nil, aerr.Wrap(aerr.APERTURE_STORAGE,
			"engine: failed to load grants for subjects", err)
	}

	limit := boundEnumerateLimit(req.Limit)
	permCache := make(map[string]*model.Permission, len(grants))

	// The decision context reused per candidate. Object is filled per candidate.
	decReq := Request{Account: req.Account, Principal: req.Principal, Action: req.Action}

	// Gather candidate ids from the ALLOW grants whose action matches.
	seen := make(map[string]struct{})
	candidates := make([]identity.Identity, 0)
	for _, g := range grants {
		if g.Effect != model.EffectAllow {
			continue
		}
		ok, err := e.actionMatches(ctx, g, req.Action, permCache)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		perm := permCache[g.PermissionID]
		members, err := e.coverer.members(ctx, decReq, g, perm, query)
		if err != nil {
			return nil, err
		}
		for _, m := range members {
			s := m.String()
			if _, dup := seen[s]; dup {
				continue
			}
			seen[s] = struct{}{}
			candidates = append(candidates, m)
		}
	}

	// Deterministic output: decide candidates in canonical-id order.
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].String() < candidates[j].String()
	})

	out := make([]string, 0, len(candidates))
	for _, obj := range candidates {
		decReq.Object = obj.String()
		dec, err := e.evaluate(ctx, decReq, obj, grants, permCache)
		if err != nil {
			return nil, err
		}
		if !dec.Allow {
			continue
		}
		// The metadata predicate runs on the ALLOWED candidate, before the limit
		// counts it: filtering after truncation would answer the wrong question
		// (the matches among the first Limit candidates, not the first Limit
		// matches).
		matched, err := e.matchesFields(ctx, obj, req.Fields)
		if err != nil {
			return nil, err
		}
		if !matched {
			continue
		}
		out = append(out, obj.String())
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// matchesFields reports whether obj's metadata satisfies every predicate in
// fields, per the provider.Filter contract. An empty or nil fields map matches
// everything WITHOUT touching the metadata source, so an unfiltered enumeration
// costs exactly what it did before and needs no source wired.
//
// Failure modes are deliberately asymmetric, because an enumeration that
// silently returns fewer objects reads as "no access" and one that returns more
// is an authorization bug:
//
//   - no metadata source wired, or no provider registered for the candidate's
//     object-type: an APERTURE_PROVIDER_UNREGISTERED error, so the caller sees a
//     misconfiguration rather than an empty answer;
//   - any other provider failure: surfaced verbatim (already coded), a
//     non-result;
//   - the object simply has no metadata row (APERTURE_NOT_FOUND): every field is
//     ABSENT, and absent never matches, so the candidate is excluded — the
//     restrictive direction, and the same answer MatchFields gives for an empty
//     metadata bag.
func (e *Engine) matchesFields(ctx context.Context, obj identity.Identity, fields map[string]any) (bool, error) {
	if len(fields) == 0 {
		return true, nil
	}
	if e.metadata == nil {
		return false, aerr.WithContext(aerr.APERTURE_PROVIDER_UNREGISTERED,
			"engine: enumerate field predicates need an object-metadata source, none is configured",
			map[string]any{"object": obj.String()})
	}
	md, err := e.metadata.Fetch(ctx, obj)
	if err != nil {
		if aerr.CodeOf(err) == aerr.APERTURE_NOT_FOUND {
			return false, nil
		}
		return false, err
	}
	// Read-only, transitively: MatchFields never writes to md at any depth.
	return provider.MatchFields(md, fields), nil
}

// boundEnumerateLimit normalises a caller limit to a positive bound.
func boundEnumerateLimit(limit int) int {
	if limit <= 0 || limit > DefaultEnumerateLimit {
		return DefaultEnumerateLimit
	}
	return limit
}

// validateEnumerateRequest rejects a request missing any required field before
// any storage work happens.
func validateEnumerateRequest(req EnumerateRequest) error {
	switch {
	case req.Account == "":
		return aerr.New(aerr.APERTURE_INVALID_INPUT, "engine: enumerate account is empty")
	case req.Principal == "":
		return aerr.New(aerr.APERTURE_INVALID_INPUT, "engine: enumerate principal is empty")
	case req.Action == "":
		return aerr.New(aerr.APERTURE_INVALID_INPUT, "engine: enumerate action is empty")
	case req.Pattern == "":
		return aerr.New(aerr.APERTURE_INVALID_INPUT, "engine: enumerate pattern is empty")
	}
	return nil
}
