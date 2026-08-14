package seed

import (
	"os"
	"path/filepath"
	"time"

	"github.com/frankbardon/aperture/csvprovider"
	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/provider"
)

// Provider declares an object-metadata provider for one object-type, so a host
// can link object instances to real data through YAML instead of Go wiring.
// BuildRegistry turns the document's providers into a live *provider.Registry.
//
// This section is runtime WIRING, not model state: Apply never writes it to
// storage (a provider produces no model rows), and because the model is exported
// by reading storage back, an export does not reproduce it — the seed file is
// the source of truth for provider wiring, exactly as auth config is.
type Provider struct {
	// ObjectType is the type whose instances this provider serves (e.g. "brand").
	// An object's identity terminal-segment type must equal it, and each type may
	// be declared at most once.
	ObjectType string `yaml:"object_type" json:"object_type"`
	// Kind selects the provider implementation. Currently only "csv".
	Kind string `yaml:"kind" json:"kind"`
	// Path is the data source for file-backed kinds (csv): the CSV file, resolved
	// relative to the seed file's directory when it is not absolute.
	Path string `yaml:"path,omitempty" json:"path,omitempty"`
	// TTL is the cache freshness window as a Go duration ("30s", "5m"). "0" (or an
	// empty string, which adopts the registry default of 30s) — set "0" for a
	// static file you reload explicitly so cached metadata never expires.
	TTL string `yaml:"ttl,omitempty" json:"ttl,omitempty"`
	// MaxSize caps cached entries for this type; 0 uses the registry default.
	MaxSize int `yaml:"max_size,omitempty" json:"max_size,omitempty"`
}

// ParseFile reads path and parses it into a Document, inferring the format from
// the file extension (.json → JSON, otherwise YAML). It is Parse plus file IO,
// without applying anything to storage; callers that also need the model loaded
// into a store use LoadFile.
func ParseFile(path string) (*Document, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, aerr.Wrap(aerr.APERTURE_CONFIG_INVALID, "seed: reading file failed", err)
	}
	return Parse(data, formatFor(path))
}

// BuildOption tunes how BuildRegistry resolves the document's two wiring
// sections. It is Go-level wiring on purpose: the seed FILE stays a plain
// declaration of what exists, and the decision to tolerate an ambiguous one is
// made by the host that builds the registry, where a reviewer sees it.
type BuildOption func(*buildConfig)

// buildConfig is the resolved set of BuildOptions.
type buildConfig struct {
	// allowProviderPrecedence accepts the type-level precedence rule instead of
	// refusing to build an ambiguous document. See AllowProviderPrecedence.
	allowProviderPrecedence bool
}

// AllowProviderPrecedence accepts the type-level precedence rule for an object
// type declared in BOTH the providers: and objects: sections, instead of failing
// the build with APERTURE_CONFIG_INVALID.
//
// The rule it accepts is TOTAL and TYPE-LEVEL: the file-backed providers: entry
// wins, and EVERY inline objects: entry for that type is discarded. There is no
// object-level merge, no field-level merge, and no fallback — an inline id the
// file happens to lack is simply not resolvable, exactly as if the entry had
// never been written. Field-level merging is the most useful-sounding behaviour
// and the most impossible to debug: a rule reading a field the CSV silently did
// not override is a support ticket nobody can reproduce. Predictability wins.
//
// Because the discard is total, a collision is a HARD APERTURE_CONFIG_INVALID by
// default — no operator can miss it, and no logging path leaves this package that
// could carry a warning instead. This option is the deliberate dev-time override
// (a CSV shadowing checked-in inline defaults, say): the discard still happens in
// full, but the host has said in code that it means it.
//
// Every inline entry is validated either way — a malformed declaration fails the
// load whether or not its type is ultimately discarded.
func AllowProviderPrecedence() BuildOption {
	return func(c *buildConfig) { c.allowProviderPrecedence = true }
}

// BuildRegistry constructs a live *provider.Registry from the document's two
// wiring sections: providers (a declared implementation per object-type) and
// objects (metadata declared inline, served by an in-memory provider.Static per
// object-type). Relative file paths are resolved against baseDir (typically the
// seed file's directory; pass "" to resolve against the process CWD). It always
// returns a usable registry — empty when neither section is declared — so a
// caller can wire it unconditionally.
//
// A malformed providers entry (missing object_type, unknown kind, missing path,
// unparseable ttl, or a duplicate object_type) and a malformed objects entry
// (missing id, a duplicate id, metadata that is not a mapping, or a value the
// shared value model rejects) are both APERTURE_CONFIG_INVALID; a malformed id
// passes through as APERTURE_IDENTITY_INVALID, and a type claimed twice by two
// providers entries is APERTURE_PROVIDER_INVALID.
//
// A type claimed by BOTH sections is APERTURE_CONFIG_INVALID naming the type,
// unless AllowProviderPrecedence is passed — see that option for the precedence
// rule it accepts and why the default is loud.
//
// Neither section touches storage: Apply writes no row for either, and an export
// reproduces neither.
func (d *Document) BuildRegistry(baseDir string, opts ...BuildOption) (*provider.Registry, error) {
	var cfg buildConfig
	for _, opt := range opts {
		opt(&cfg)
	}
	reg := provider.NewRegistry()
	// providers: is registered FIRST so the objects: section can see which types
	// are already file-backed. That ordering IS the precedence rule.
	for _, p := range d.Providers {
		if err := registerProvider(reg, p, baseDir); err != nil {
			return nil, err
		}
	}
	if err := d.registerObjects(reg, cfg); err != nil {
		return nil, err
	}
	return reg, nil
}

// registerProvider builds one declared provider and registers it under its
// object-type with the cache options its TTL/MaxSize imply.
func registerProvider(reg *provider.Registry, p Provider, baseDir string) error {
	if p.ObjectType == "" {
		return aerr.New(aerr.APERTURE_CONFIG_INVALID, "seed: provider is missing object_type")
	}
	var opts []provider.CacheOption
	if p.TTL != "" {
		ttl, err := time.ParseDuration(p.TTL)
		if err != nil {
			return aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
				"seed: provider has an invalid ttl",
				map[string]any{"object_type": p.ObjectType, "ttl": p.TTL})
		}
		opts = append(opts, provider.WithTTL(ttl))
	}
	if p.MaxSize != 0 {
		opts = append(opts, provider.WithMaxSize(p.MaxSize))
	}
	impl, err := buildObjectProvider(p, baseDir)
	if err != nil {
		return err
	}
	// Register surfaces APERTURE_PROVIDER_INVALID for a duplicate object_type.
	return reg.Register(p.ObjectType, impl, opts...)
}

// buildObjectProvider constructs the ObjectProvider for a declared kind.
func buildObjectProvider(p Provider, baseDir string) (provider.ObjectProvider, error) {
	switch p.Kind {
	case "csv":
		if p.Path == "" {
			return nil, aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
				"seed: csv provider is missing path",
				map[string]any{"object_type": p.ObjectType})
		}
		path := p.Path
		if !filepath.IsAbs(path) && baseDir != "" {
			path = filepath.Join(baseDir, path)
		}
		return csvprovider.New(path), nil
	default:
		return nil, aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
			"seed: unknown provider kind",
			map[string]any{"object_type": p.ObjectType, "kind": p.Kind})
	}
}
