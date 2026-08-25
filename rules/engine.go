package rules

import (
	"context"
	"maps"
	"time"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/identity"
)

// Rule is a named rule definition: a reference label plus the AST that decides
// it. Rule is the unit a RuleSource resolves and the editor/state-file persist,
// so it serializes to stable JSON alongside its AST.
type Rule struct {
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	AST         *Node  `json:"ast"`
}

// RuleSource resolves a scope strategy's opaque rule reference to its definition.
// A host backs it with whatever store holds rules (a config map, a database, the
// state file); MapSource is the in-memory default. A reference with no matching
// rule yields APERTURE_RULE_NOT_FOUND.
type RuleSource interface {
	Lookup(ctx context.Context, ref string) (*Rule, error)
}

// MapSource is an in-memory RuleSource keyed by rule reference. It is the simple
// default for tests and static configurations.
type MapSource map[string]*Rule

// Lookup resolves ref, returning APERTURE_RULE_NOT_FOUND when it is absent.
func (m MapSource) Lookup(_ context.Context, ref string) (*Rule, error) {
	r, ok := m[ref]
	if !ok || r == nil {
		return nil, aerr.WithContext(aerr.APERTURE_RULE_NOT_FOUND,
			"rule: no rule registered for reference", map[string]any{"rule": ref})
	}
	return r, nil
}

// MetadataFetcher supplies an object's metadata for the evaluation context. Its
// signature matches *provider.Registry.Fetch (provider.Metadata is map[string]any),
// so a *provider.Registry is wired directly as the fetcher without this package
// importing provider. The returned map is treated as read-only.
type MetadataFetcher interface {
	Fetch(ctx context.Context, id identity.Identity) (map[string]any, error)
}

// PrincipalResolver supplies a principal's attribute bag for the evaluation
// context, keyed by principal kind and id. A host wires one (a directory, a
// *provider.AttributeRegistry) when its rules need principal attributes; without
// one, `principal` is exactly the floor bag. The returned map is treated as
// read-only, transitively — the engine copies it rather than writing into it,
// and no other holder writes to it at any depth.
//
// A resolver returns ONLY what the host knows. It does not need to publish `id`
// or `kind`: the engine stamps the floor over whatever comes back (see
// principalBag), so a resolver that returns nil is not an error and not an empty
// `principal` — it is the floor.
//
// kind is model.PrincipalKind's spelling — "user" or "machine" — passed as a
// string because rules imports no model. It is what lets one resolver dispatch to
// a different attribute source per kind (a human directory and a service-account
// registry are rarely the same store) without re-deriving the kind from the id.
// An empty kind means the caller did not have the principal's record in hand: a
// resolver treats that as unknown, and must not substitute a default kind, or a
// machine would silently be answered for out of the human directory.
type PrincipalResolver interface {
	Attributes(ctx context.Context, kind, principal string) (map[string]any, error)
}

// The floor keys. `principal` always carries these two, whatever resolver is
// wired and whether or not it answered.
const (
	principalKeyID   = "id"
	principalKeyKind = "kind"
)

// principalBag builds the `principal` root: a FRESH map holding the resolver's
// attributes with the floor — id and kind — stamped over them.
//
// # Why the floor is the engine's job, not the resolver's
//
// Every resolver gets it, including a host's own. A rule author can therefore
// write principal.id and principal.kind against ANY deployment, wired or not,
// which is the whole point of a floor: it is the part of `principal` that does
// not depend on what the host happened to configure.
//
// # Why `kind` is published at all
//
// Attribute providers are registered PER KIND, so a rule that reads a user
// directory's field is silently kind-dependent: for a machine principal that
// field is simply absent and the comparison is false. In an inclusive grant that
// is deny-safe; in an EXCLUSIVE one — where selection means "excluded" — a rule
// that quietly stops selecting WIDENS access. principal.kind is the tool an
// author needs to say which kinds a rule is about, so
// `principal.kind == "user" && principal.tier == "gold"` states the dependence
// instead of hiding it.
//
// # Why the floor wins on collision
//
// The floor is stamped LAST, so a host bag carrying its own "id" or "kind" key
// cannot shadow it. Not primarily as a defence — a directory is host-trusted
// infrastructure — but because the realistic collision is innocent: a directory
// with its own internal `id` column would silently redefine what
// `principal.id == object.owner` compares, and an ownership rule would start
// answering a different question with no error anywhere. A floor that can be
// shadowed is not a floor.
//
// # Why a fresh map
//
// bag may be a cached attribute bag, shared across every object in this decision
// and every concurrent decision for the same principal. Stamping into it would
// be a write through a read-only value at the widest blast radius Aperture has.
func principalBag(bag map[string]any, kind, principal string) map[string]any {
	out := make(map[string]any, len(bag)+2)
	maps.Copy(out, bag)
	out[principalKeyID] = principal
	// Published even when empty. An unknown kind is a value a rule can compare
	// against ("" matches neither "user" nor "machine"); an ABSENT key would make
	// "unknown" and "not published by this build" the same observation.
	out[principalKeyKind] = kind
	return out
}

// Engine is the rules engine: it resolves a rule reference, compiles-and-caches
// it, builds the evaluation context from object metadata and principal/action,
// and evaluates. It satisfies scope.RuleEvaluator (see scope.go), so the
// inclusive/exclusive scope resolvers get their rule-backed variant by wiring an
// Engine as scope.Deps{Rules: engine}.
//
// Engine is safe for concurrent use: the compiler options are read-only and the
// compiled-rule cache is concurrency-safe.
type Engine struct {
	source    RuleSource
	fetcher   MetadataFetcher
	principal PrincipalResolver
	compiler  *Compiler
	cache     *compiledCache
	// clock is the engine's single time source: the compiled-rule cache expires
	// entries against it AND every decision takes its reference instant from it.
	// See Clock and WithClock.
	clock Clock
}

// Option configures an Engine.
type Option func(*engineConfig)

type engineConfig struct {
	compilerOpts []CompilerOption
	ttl          time.Duration
	clock        Clock
	principal    PrincipalResolver
}

// WithFunction registers a pure host function callable from rules (expr-lang's
// Function seam). See Function.
func WithFunction(name string, fn func(args ...any) (any, error)) Option {
	return func(c *engineConfig) { c.compilerOpts = append(c.compilerOpts, Function(name, fn)) }
}

// WithCacheTTL bounds how long a compiled rule stays cached. TTL <= 0 (the
// default) keeps compiled rules until explicitly invalidated.
func WithCacheTTL(ttl time.Duration) Option {
	return func(c *engineConfig) { c.ttl = ttl }
}

// WithClock injects the engine's SINGLE time source. Defaults to the real clock.
//
// It drives BOTH of the engine's time-dependent behaviours:
//
//   - compiled-rule cache TTL expiry (WithCacheTTL), and
//   - the per-decision reference instant rule date comparisons resolve against
//     (now.go), snapshotted once per decision and always converted to UTC.
//
// The second of those is the wider promise: an injected clock now decides what
// "now" MEANS to a rule, not merely when a cached program is dropped. Pinning the
// clock is therefore how a date rule is tested reproducibly — and, as the flip
// side of one time source, a pinned clock also FREEZES cache expiry, so a TTL'd
// entry under a clock that never advances never expires. That coupling is
// intended: two independent clocks inside one engine can disagree, which is worse
// than the coupling (see Clock).
func WithClock(clk Clock) Option {
	return func(c *engineConfig) { c.clock = clk }
}

// WithPrincipalResolver supplies principal attributes to the evaluation context.
// Without it, `principal` is the floor bag alone: its id and its kind. A
// *provider.AttributeRegistry is wired straight in here — it resolves the
// principal's kind to an attribute slot and fetches through that slot's provider.
func WithPrincipalResolver(r PrincipalResolver) Option {
	return func(c *engineConfig) { c.principal = r }
}

// NewEngine builds an Engine over a rule source and a metadata fetcher. A nil
// fetcher means object metadata is empty (for rules that read only the
// principal/action context). Pass a *provider.Registry as the fetcher to read
// real object metadata.
func NewEngine(source RuleSource, fetcher MetadataFetcher, opts ...Option) *Engine {
	cfg := &engineConfig{}
	for _, opt := range opts {
		opt(cfg)
	}
	var principal PrincipalResolver = floorPrincipal{}
	if cfg.principal != nil {
		principal = cfg.principal
	}
	// One clock, resolved once here and shared: the cache and the decision
	// instant must never be able to read different sources.
	var clock Clock = realClock{}
	if cfg.clock != nil {
		clock = cfg.clock
	}
	return &Engine{
		source:    source,
		fetcher:   fetcher,
		principal: principal,
		compiler:  NewCompiler(cfg.compilerOpts...),
		cache:     newCompiledCache(cfg.ttl, clock),
		clock:     clock,
	}
}

// Selected reports whether object is selected by the named rule for the given
// account/principal/action context. It is the scope.RuleEvaluator implementation
// the rule-backed inclusive/exclusive scope resolvers consult: an inclusive grant
// covers an object Selected reports true for, an exclusive grant excludes one.
//
// The flow is: resolve the rule reference, compile-and-cache its AST, fetch the
// object's metadata, build the context (including the decision's reference
// instant), and evaluate. Any failure is an APERTURE_* coded error and the
// resolver treats it as a non-decision — there is no select-on-error.
//
// account is accepted and not yet read. It is here now, ahead of the account
// attribute source that will consume it, because the alternative is breaking this
// exported signature twice: once for the principal's kind and again, later, for
// the account. One break is cheaper for every host than two, and an argument that
// is present-but-unread cannot change a decision — an argument absent from the
// interface is the one that later gets smuggled in through a context value and
// degrades to an empty account without saying so.
func (e *Engine) Selected(ctx context.Context, rule string, object identity.Identity, account, principalKind, principal, action string) (bool, error) {
	r, err := e.source.Lookup(ctx, rule)
	if err != nil {
		return false, err
	}
	if r == nil || r.AST == nil {
		return false, aerr.WithContext(aerr.APERTURE_RULE_NOT_FOUND,
			"rule: reference resolved to an empty rule", map[string]any{"rule": rule})
	}
	compiled, err := e.compile(r.AST)
	if err != nil {
		return false, err
	}
	metadata, err := e.metadata(ctx, object)
	if err != nil {
		return false, err
	}
	principalAttrs, err := e.principal.Attributes(ctx, principalKind, principal)
	if err != nil {
		return false, err
	}
	in := Input{
		Object: metadata,
		// The resolver's answer with the floor stamped over it, in a fresh map —
		// the resolver's bag may be cached and shared, and is read-only.
		Principal: principalBag(principalAttrs, principalKind, principal),
		// Deliberately still empty. The account arrives as an argument now, but
		// populating account.* from an attribute source is a separate change:
		// plumbing that also altered what a rule sees would make its own
		// correctness impossible to test in isolation.
		Account: map[string]any{},
		Action:  action,
		// The reference instant, read from the engine's one clock. Under a
		// decision scope (the decision engine opens one per Check / Enumerate /
		// Explain) every evaluation in that decision shares the first instant
		// taken; unscoped, this evaluation takes its own. Either way it is read
		// ONCE here and threaded as data, so a rule referring to it twice cannot
		// straddle a tick. See now.go.
		Now: decisionInstantFrom(ctx).snapshot(e.clock),
	}

	// Fast path: no collector installed (Check, Enumerate), so evaluation records
	// nothing and allocates nothing extra.
	sink := NoteCollectorFrom(ctx)
	if sink == nil {
		return compiled.Eval(ctx, in)
	}
	// Explain installed a collector. Evaluate into a local sink so each note can
	// be stamped with the rule reference that produced it before it is published
	// — the collector spans every grant in the trace, so the reference is what
	// tells two rules' notes apart.
	local := &NoteCollector{}
	selected, err := compiled.eval(in, local)
	notes := local.Notes()
	for i := range notes {
		notes[i].Rule = rule
	}
	sink.Add(notes...)
	return selected, err
}

// Compile validates, compiles, and caches an AST, returning the reusable program.
// A second call with an AST that renders to the same expression returns the
// cached program without recompiling. It is exported so a host can warm the cache
// or validate a rule (e.g. the node editor's save path) ahead of evaluation.
func (e *Engine) Compile(n *Node) (*Compiled, error) {
	return e.compile(n)
}

func (e *Engine) compile(n *Node) (*Compiled, error) {
	if err := n.Validate(); err != nil {
		return nil, err
	}
	src, err := n.Expr()
	if err != nil {
		return nil, err
	}
	hash := hashSource(src)
	if c, ok := e.cache.get(hash); ok {
		return c, nil
	}
	compiled, err := e.compiler.compileSource(src)
	if err != nil {
		return nil, err
	}
	e.cache.put(compiled)
	return compiled, nil
}

// metadata fetches object's metadata, or returns an empty map when no fetcher is
// configured (rules that read only principal/action still evaluate).
func (e *Engine) metadata(ctx context.Context, object identity.Identity) (map[string]any, error) {
	if e.fetcher == nil {
		return map[string]any{}, nil
	}
	md, err := e.fetcher.Fetch(ctx, object)
	if err != nil {
		return nil, err
	}
	if md == nil {
		md = map[string]any{}
	}
	return md, nil
}

// CacheStats exposes the compiled-rule cache counters for observability and the
// latency benchmark (E4-S4).
func (e *Engine) CacheStats() CacheStats { return e.cache.stats() }

// InvalidateAll clears the compiled-rule cache. A host calls it after a rule's
// definition changes underneath a cached compilation.
func (e *Engine) InvalidateAll() { e.cache.clear() }

// Invalidate drops the cached compilation of a single rule AST, reporting whether
// one was cached. A host calls it when exactly one rule's definition changed, so
// the next evaluation recompiles only that rule.
func (e *Engine) Invalidate(n *Node) (bool, error) {
	if err := n.Validate(); err != nil {
		return false, err
	}
	src, err := n.Expr()
	if err != nil {
		return false, err
	}
	return e.cache.invalidate(hashSource(src)), nil
}

// floorPrincipal is the default PrincipalResolver: it contributes NOTHING, so an
// unwired engine's `principal` is exactly the floor the engine stamps — id and
// kind (see principalBag).
//
// It returns nil rather than the floor map itself so there is exactly one
// definition of what the floor is. A default that built its own copy would be a
// second definition, free to drift from the one every host resolver gets.
type floorPrincipal struct{}

func (floorPrincipal) Attributes(context.Context, string, string) (map[string]any, error) {
	return nil, nil
}
