package cli

import (
	"github.com/frankbardon/aperture/engine"
	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/provider"
	"github.com/frankbardon/aperture/rules"
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
	// ruleSource is the storage-backed rule source the engine resolves rule
	// references through. `serve` also hands it to the facade (WithRuleSource) so
	// the node editor's what-if can preview an UNSAVED rule; the one-shot commands
	// have no editor and do not need it.
	ruleSource *service.StorageRuleSource
	// fetcher is the object-metadata fetcher rules read `object.*` through, or nil
	// when the seed declares no object source at all (nil means empty metadata,
	// which is exactly rules.NewEngine's own default).
	fetcher rules.MetadataFetcher
}

// buildDecisionStack wires the decision graph over an already-seeded store.
//
// seedPath is the same --seed value buildStore was given: the seed file is read a
// second time as a Document because two of its sections — `providers:` and
// `objects:` — are runtime WIRING that Apply never writes to storage, so the file
// is their only source of truth.
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
	reg, err := doc.BuildRegistry(seedBaseDir(seedPath))
	if err != nil {
		return decisionStack{}, aerr.Wrap(aerr.APERTURE_BOOT, "cli: building object providers failed", err)
	}

	var fetcher rules.MetadataFetcher // nil => empty object metadata (unchanged default)
	scopeDeps := engine.ScopeDeps{}
	// The guard MUST test both wiring sections. Gating on `providers:` alone made
	// a seed that declared only `objects:` build a populated registry that nothing
	// ever read — inline metadata was invisible to rules and to enumeration.
	if hasObjectSources(doc) {
		fetcher = lenientFetcher{reg: reg}
		scopeDeps.Lister = reg
	}

	// The rules engine resolves references against the SAME store the node editor
	// saves through PutRule, so a saved rule takes effect on the next decision and
	// there is no second rule store to keep in sync.
	ruleSource := service.NewStorageRuleSource(store)
	scopeDeps.Rules = rules.NewEngine(ruleSource, fetcher)

	opts := make([]engine.Option, 0, len(engOpts)+1)
	opts = append(opts, engine.WithScopeResolution(nil, scopeDeps))
	opts = append(opts, engOpts...)

	return decisionStack{
		eng:        engine.New(store, opts...),
		registry:   reg,
		ruleSource: ruleSource,
		fetcher:    fetcher,
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
