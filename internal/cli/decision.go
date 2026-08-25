package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/frankbardon/aperture/engine"
	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/provider"
	"github.com/frankbardon/aperture/rules"
	"github.com/frankbardon/aperture/seed"
	"github.com/frankbardon/aperture/service"
)

// decisionStack is the fully-wired decision graph every Aperture surface shares:
// storage -> object providers -> rules engine -> scope resolution -> engine ->
// service facade. It exists so `serve` and the one-shot commands (`check`,
// `enumerate`, `identifiers`, `explain`) CANNOT answer the same question
// differently.
//
// They used to. `serve` wired the rules engine and scope resolution; the one-shot
// commands called service.New(engine.New(store)) with neither, so a permission
// with a rule-backed scope strategy (`inclusive;rule=...`) had no rule evaluator:
// the resolver reported APERTURE_SCOPE_RULE_UNCONFIGURED, the facade folded that
// into a fail-closed deny, and the CLI returned a DIFFERENT VERDICT from the
// server for the same model. Missing rule diagnostics in `explain` were only the
// visible symptom. One builder, used by every command, is the fix.
type decisionStack struct {
	// eng is the wired PDP: scope resolution on, rules attached, plus whatever
	// per-command engine options the caller added.
	eng *engine.Engine
	// registry is the object-metadata registry built from the seed's `providers:`
	// and `objects:` sections. It is always non-nil (BuildRegistry returns an
	// empty registry for a seed declaring neither) and is handed to the facade so
	// `identifiers` can enumerate a type.
	registry *provider.Registry
	// attributes is the SUBJECT-attribute registry built from the seed's
	// `attributes:` section — the bags a rule reads off `principal`. It is always
	// non-nil (BuildAttributeRegistry returns an empty registry for a seed
	// declaring none) and is wired into the rules engine as the principal
	// resolver, so the CLI and `serve` cannot disagree about what a principal
	// knows any more than they can disagree about a rule.
	attributes *provider.AttributeRegistry
	// ruleSource is the storage-backed rule source the engine resolves rule
	// references through. `serve` also hands it to the facade (WithRuleSource) so
	// the node editor's what-if can preview an UNSAVED rule; the one-shot commands
	// have no editor and do not need it.
	ruleSource *service.StorageRuleSource
	// fetcher is the object-metadata fetcher rules read `object.*` through, or nil
	// when the seed declares no object source at all (nil means empty metadata,
	// which is exactly rules.NewEngine's own default).
	fetcher rules.MetadataFetcher
	// collisions are the object types declared in BOTH the seed's `providers:` and
	// `objects:` sections. The file-backed provider wins and the inline entries for
	// those types are discarded entirely — the documented default. Discarding data
	// silently would be hostile, and `seed` has no logging path of its own, so it
	// reports the fact and the caller surfaces it (reportCollisions).
	collisions []string
	// conns are the database pools BuildRegistryWithConnections opened for the
	// seed's `connections:` block — one per named connection, shared by every
	// `kind: sql` provider entry referencing it. It is the only part of the stack
	// that holds an OS resource, and Close is what releases it. Always non-nil.
	conns *seed.Connections
}

// Close releases everything the stack holds open. Today that is the seed's
// database pools; a stack built from a seed with no `connections:` block closes
// nothing and the call is free, so every command defers it unconditionally
// rather than asking which kinds the seed happened to use.
//
// It is idempotent, so a `serve` that closes explicitly on shutdown may also
// defer it.
func (s decisionStack) Close() error {
	if s.conns == nil {
		return nil
	}
	return s.conns.Close()
}

// reportCollisions writes a warning naming every object type whose inline
// `objects:` entries were discarded because a `providers:` entry claimed the same
// type. Nothing is written when there is no collision, so a normal boot stays
// silent. Only object TYPES are named — never ids — so the warning cannot leak
// cross-account data.
func (s decisionStack) reportCollisions(w io.Writer) {
	if len(s.collisions) == 0 || w == nil {
		return
	}
	fmt.Fprintf(w, "warning: seed declares %d object type(s) in both providers: and objects: — "+
		"the providers: entry wins and the inline entries were discarded: %s\n",
		len(s.collisions), strings.Join(s.collisions, ", "))
}

// buildDecisionStack wires the decision graph over an already-seeded store.
//
// seedPath is the same --seed value buildStore was given: the seed file is read a
// second time as a Document because several of its sections — `providers:`,
// `objects:` and `attributes:` — are runtime WIRING that Apply never writes to
// storage, so the file is their only source of truth.
//
// Both sections feed ONE *provider.Registry, which in turn feeds BOTH the rules
// engine's metadata fetcher (so a rule can read object.category_id) AND the scope
// resolver's object lister (so implicit / exclusive scopes can enumerate a
// type's objects). When neither section is declared the fetcher and lister stay
// nil and behaviour is unchanged: rules see empty object metadata and enumeration
// of "all objects of a type" reports APERTURE_SCOPE_LISTER_UNCONFIGURED.
//
// Scope resolution falls back to literal pattern matching for grants whose
// permission declares no strategy, so plain pattern grants decide exactly as they
// did before.
//
// engOpts are per-command engine options appended after the shared ones — that is
// how `serve` adds --enforce-membership without forcing it on the one-shot
// commands.
func buildDecisionStack(store model.Storage, seedPath string, engOpts ...engine.Option) (decisionStack, error) {
	doc, err := seedDocument(seedPath)
	if err != nil {
		return decisionStack{}, err
	}
	// The two-return form, always: the seed may declare `connections:`, whose
	// pools outlive the build and have to be closed by whoever owns the stack.
	// The one-return BuildRegistry refuses such a document precisely because it
	// cannot hand the pools back.
	reg, conns, err := doc.BuildRegistryWithConnections(seedBaseDir(seedPath))
	if err != nil {
		return decisionStack{}, aerr.Wrap(aerr.APERTURE_BOOT, "cli: building object providers failed", err)
	}

	var fetcher rules.MetadataFetcher // nil => empty object metadata (unchanged default)
	// metaSource is the STRICT metadata source Enumerate's Fields predicate reads
	// through — the registry itself, not lenientFetcher. The leniency is right for
	// a rule (an unreadable object evaluates against empty metadata and the rule
	// denies) and wrong here: a filtered enumeration that quietly saw empty
	// metadata for every object would return nothing and read as "no access", so
	// the engine wants the registry's real error. Nil when the seed declares no
	// object source at all, which makes a filtered enumeration report
	// APERTURE_PROVIDER_UNREGISTERED rather than an empty result.
	var metaSource engine.MetadataFetcher
	scopeDeps := engine.ScopeDeps{}
	// The guard MUST test both wiring sections. Gating on `providers:` alone made
	// a seed that declared only `objects:` build a populated registry that nothing
	// ever read — inline metadata was invisible to rules and to enumeration.
	if hasObjectSources(doc) {
		fetcher = lenientFetcher{reg: reg}
		scopeDeps.Lister = reg
		metaSource = reg
	}

	// The seed's `attributes:` section is the second wiring section this builder
	// reads out of the same file, and it feeds a DIFFERENT registry: an attribute
	// bag is keyed by a bare subject id and is never an enumerable object set, so
	// it deliberately cannot reach the scope resolver's object lister.
	attrs, err := doc.BuildAttributeRegistry(seedBaseDir(seedPath))
	if err != nil {
		// bootError, not a bare wrap: an attribute declaration fails with
		// APERTURE_ATTRIBUTE_SLOT_UNKNOWN (naming the three legal slots) or
		// APERTURE_METADATA_INVALID (naming the field and the cap it broke), and
		// re-stamping either APERTURE_BOOT would hand the operator "aperture
		// failed to start" instead of the remedy.
		_ = conns.Close()
		return decisionStack{}, bootError("cli: building attribute providers failed", err)
	}
	var ruleOpts []rules.Option
	// The gate MUST count every attribute source the document can declare, which
	// is why it is asked of the document rather than re-derived here — see
	// seed.Document.HasAttributeSources, and hasObjectSources below it for the bug
	// that taught us why a gate written over one section of two is a silent one.
	//
	// Wiring the resolver unconditionally would be harmless today (an unfilled
	// slot resolves to the floor bag), but the gate states the intent: with no
	// declared source, `principal` is exactly its floor — id and kind — and no
	// attribute machinery is consulted at all.
	if doc.HasAttributeSources() {
		ruleOpts = append(ruleOpts, rules.WithPrincipalResolver(attrs))
	}

	// The rules engine resolves references against the SAME store the node editor
	// saves through PutRule, so a saved rule takes effect on the next decision and
	// there is no second rule store to keep in sync.
	ruleSource := service.NewStorageRuleSource(store)
	scopeDeps.Rules = rules.NewEngine(ruleSource, fetcher, ruleOpts...)

	opts := make([]engine.Option, 0, len(engOpts)+3)
	opts = append(opts, engine.WithScopeResolution(nil, scopeDeps))
	if metaSource != nil {
		opts = append(opts, engine.WithMetadata(metaSource))
		// The SAME registry is the declared-reference source, so one object
		// source backs the lister, the Fields predicate and the dereference, and
		// an enumeration through `--via` reads the holder from the cache the
		// decision already warmed. Gated on the same condition for the same
		// reason: with no object source at all, an enumeration through a
		// reference must report APERTURE_PROVIDER_UNREGISTERED rather than an
		// empty result that reads as "no access".
		opts = append(opts, engine.WithReferences(reg))
	}
	opts = append(opts, engOpts...)

	return decisionStack{
		eng:        engine.New(store, opts...),
		registry:   reg,
		attributes: attrs,
		ruleSource: ruleSource,
		fetcher:    fetcher,
		collisions: doc.ProviderCollisions(),
		conns:      conns,
	}, nil
}

// newService composes the stack into a service facade. The provider registry is
// always wired (it is what backs ObjectIdentifiers); opts add the per-surface
// dependencies — `serve` passes storage, the admin gate, delegation,
// impersonation, audit and the editor's rule source, while a one-shot command
// passes nothing and gets the read-only decision facade.
func (s decisionStack) newService(opts ...service.Option) *service.Service {
	all := make([]service.Option, 0, len(opts)+1)
	all = append(all, service.WithProviders(s.registry))
	all = append(all, opts...)
	return service.New(s.eng, all...)
}
