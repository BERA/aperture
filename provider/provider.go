// Package provider lets a host application supply its domain objects and their
// metadata to Aperture through pull-based providers, and caches that metadata
// per object-type so the rules engine (E2-S3) and the decision engine can read
// it without breaking the Check latency NFR (p99 < 1ms; FR-12/FR-13).
//
// The shape is deliberately small:
//
//   - ObjectProvider is implemented by the host, once per object-type. It pulls
//     an object's metadata on demand (Fetch), and enumerates / filters the
//     objects of its type (List / Query). Aperture never owns this data — it is
//     the host's source of truth, and Aperture only ever caches a copy of it
//     (the "never persist provider data as source of truth" Non-Goal).
//   - Registry maps an object-type to its provider plus a per-type cache, and is
//     the seam every consumer resolves a type's provider through. The Registry
//     also satisfies the scope.ObjectLister contract (its List method has the
//     exact signature scope/E2-S1 left as a seam), so the implicit/exclusive
//     scope resolvers can enumerate "all objects of a type" through it without
//     this package importing scope.
//   - The cache is concurrency-safe, metrics-friendly (hit/miss/eviction/expiry
//     counters), and tunable per object-type (TTL, max size) with an explicit
//     invalidation API. The in-memory LRU is the default behind a pluggable
//     CacheBackend interface; a remote backend (e.g. Redis) is explicitly out of
//     scope here.
//
// Metadata is host-defined and map-like (Metadata = map[string]any), so the
// rules engine can expose each field as an expression variable directly without
// a translation layer. Its VALUE SHAPE is not free-form, though: a field value
// is a scalar, a []any of scalars, or a map[string]any whose values are scalars,
// scalar arrays, or one further object level. See metadata.go for the model and
// ValidateMetadata, the load-time entry point every loader calls.
//
// A cached Metadata value is treated as READ-ONLY by every consumer, and the
// contract is TRANSITIVE. The cache stores the provider's map by reference and
// never copies it on read (allocation-aware on the hot path), which means the
// nested maps and slices inside that map are shared by reference too — reaching
// through a returned Metadata to append to a []any or write a key into a nested
// map[string]any races every other reader exactly as writing the top-level map
// would. So:
//
//   - a provider returns a FRESH map per object, with fresh nested containers —
//     it must not hand out a value it also retains and mutates, and reloading a
//     source builds a new value rather than editing the old one in place;
//   - no holder — engine, rules, scope, CLI, server, host code — writes to a
//     Metadata it was given, at ANY depth;
//   - a consumer that needs to modify metadata copies it (deeply) first.
//
// Dependencies stay minimal: provider imports only identity and errors, never
// scope/engine/model, so it remains a leaf those layers adapt to.
package provider

import (
	"context"

	"github.com/frankbardon/aperture/identity"
)

// Metadata is a host-defined, map-like bag of an object's attributes. It is an
// alias for map[string]any so the rules engine (E2-S3) can read fields straight
// into its expression environment with no conversion — keep it an alias.
//
// Legal field values are constrained by the shared value model (metadata.go);
// loaders enforce it with ValidateMetadata. A Metadata value handed back by the
// cache is read-only, transitively down through its nested containers; see the
// package doc.
type Metadata = map[string]any

// Object pairs an object's identity with its metadata. Providers return Objects
// from List and Query; the Registry uses ID to key the cache and to filter
// enumerations against a pattern.
type Object struct {
	// ID is the object's canonical identity (e.g.
	// account:acme/project:atlas/document:42). Its terminal segment's type is
	// the object-type the providing ObjectProvider is registered under.
	ID identity.Identity
	// Metadata is the object's host-defined attribute bag. Read-only once cached.
	Metadata Metadata
}

// Filter is the criteria an ObjectProvider.Query selects on. Every field is
// optional; the zero Filter selects every object of the type (equivalent to
// List). The host provider evaluates Fields; Aperture additionally enforces
// Pattern and Limit on the results it returns, so a provider that ignores them
// is still correct, only less efficient.
//
// # The Fields contract
//
// Fields is evaluated by the provider, but its MEANING is fixed here, and every
// implementation owes callers the same answer — Query is how scope enumeration
// bounds itself, so a provider that filters differently is a provider that
// authorizes differently. An implementation either calls MatchFields or
// reproduces it exactly (e.g. by pushing the predicate into SQL):
//
//   - EVERY predicate must hold (the map is an AND), and an empty or nil Fields
//     selects every object.
//   - A field ABSENT from an object never matches — not even against a nil want.
//   - A COLLECTION field ([]any) matches by MEMBERSHIP: Fields{"tags":
//     "premium"} selects every object whose tags array CONTAINS "premium".
//     Equality against a whole array is never what a caller filtering on a tag
//     list means.
//   - Every other field — scalar or object (map[string]any) — matches by
//     EQUALITY. An object field is deliberately NOT key membership, so a scalar
//     want against one is simply false rather than a panic or an accidental
//     string-rendering match.
//   - A want that is itself a container compares by equality at both ends, since
//     no element of a legal array could ever equal one.
//   - Comparison is TYPED, never a string rendering: numbers compare across Go
//     numeric types by value (int(5) == int64(5) == float64(5)), but a number
//     never equals its string spelling ("5" != 5), and a string equals only a
//     string. These are the rules engine's own comparison semantics
//     (expr-lang's), so Enumerate cannot select an object that Check then denies
//     over the same value. See ValuesEqual.
type Filter struct {
	// Pattern, when non-nil, restricts results to identities it matches. The
	// Registry's scope-lister adapter sets this to bound enumeration to a grant's
	// scope.
	Pattern *identity.Pattern
	// Fields are metadata predicates: membership for a collection field,
	// equality for everything else, per the contract on Filter. Aperture passes
	// them to the provider untouched; MatchFields is the shared implementation.
	Fields map[string]any
	// Limit bounds the number of results; <= 0 means the provider's own default.
	Limit int
}

// ObjectProvider is the host-implemented pull source for one object-type. A
// provider is registered under an object-type key in a Registry and consulted on
// demand; it must be safe for concurrent use.
//
// Implementations return APERTURE_NOT_FOUND (from errors/) for a Fetch of an
// object that does not exist, so the Registry can distinguish "absent" from an
// operational failure. Any error already carrying an APERTURE_* code is
// surfaced verbatim; a plain error is wrapped as APERTURE_PROVIDER_FETCH.
type ObjectProvider interface {
	// Fetch returns the metadata for id. The id's terminal segment type matches
	// the object-type this provider is registered under. A missing object yields
	// an APERTURE_NOT_FOUND coded error.
	Fetch(ctx context.Context, id identity.Identity) (Metadata, error)
	// List returns the objects of this provider's type. It is the unfiltered
	// enumeration; large domains should prefer Query.
	List(ctx context.Context) ([]Object, error)
	// Query returns the objects of this provider's type that satisfy filter.
	Query(ctx context.Context, filter Filter) ([]Object, error)
}

// ObjectLister is the enumeration contract the scope package (E2-S1) left as a
// seam for implicit/exclusive resolvers. It is restated here so this package can
// assert that *Registry satisfies it without importing scope; the signature is
// byte-for-byte scope.ObjectLister, so a *Registry is directly usable as
// scope.Deps.Lister / engine.ScopeDeps.Lister (the E2-S4 wiring).
type ObjectLister interface {
	List(ctx context.Context, objectType string, pattern identity.Pattern, limit int) ([]identity.Identity, error)
}

// terminalType returns the object-type an identity belongs to: the type of its
// terminal segment. An empty identity yields "", which no provider registers
// under, so it resolves to APERTURE_PROVIDER_UNREGISTERED.
func terminalType(id identity.Identity) string {
	segs := id.Segments()
	if len(segs) == 0 {
		return ""
	}
	return segs[len(segs)-1].Type
}
