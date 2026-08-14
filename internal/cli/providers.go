package cli

import (
	"context"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/identity"
	"github.com/frankbardon/aperture/provider"
	"github.com/frankbardon/aperture/seed"
)

// lenientFetcher adapts a *provider.Registry to rules.MetadataFetcher so an
// object whose type has no registered provider — or that a registered provider
// has no row for — yields EMPTY metadata rather than a coded error. That matches
// the nil-fetcher default (rules.NewEngine treats a nil fetcher as empty
// metadata), so declaring a provider for one object-type never turns a
// rule-backed scope on an unprovided type into a non-decision. Genuine provider
// failures (a malformed file, an IO error) still surface.
type lenientFetcher struct {
	reg *provider.Registry
}

// Fetch implements rules.MetadataFetcher.
func (f lenientFetcher) Fetch(ctx context.Context, id identity.Identity) (map[string]any, error) {
	md, err := f.reg.Fetch(ctx, id)
	if err != nil {
		switch aerr.CodeOf(err) {
		case aerr.APERTURE_PROVIDER_UNREGISTERED, aerr.APERTURE_NOT_FOUND:
			return map[string]any{}, nil
		default:
			return nil, err
		}
	}
	return md, nil
}

// hasObjectSources reports whether a seed document declares any object-metadata
// source at all. BOTH wiring sections count: `providers:` (a file- or
// database-backed ObjectProvider per type) and `objects:` (metadata declared
// inline and served from memory). BuildRegistry populates one *Registry from
// both, so gating the rules fetcher and the scope lister on `providers:` alone
// left a seed that used only `objects:` with a populated registry nothing ever
// read — inline metadata was invisible to rules and to scope enumeration.
func hasObjectSources(doc *seed.Document) bool {
	if doc == nil {
		return false
	}
	return len(doc.Providers) > 0 || len(doc.Objects) > 0
}
