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
// declaration of what exists, and the decision to make an ambiguous one fatal is
// taken by the host that builds the registry, where a reviewer sees it.
type BuildOption func(*buildConfig)

// buildConfig is the resolved set of BuildOptions.
type buildConfig struct {
	// strictProviderCollision refuses to build a document whose two wiring
	// sections claim the same object type, instead of applying the default
	// type-level precedence rule. See StrictProviderCollision.
	strictProviderCollision bool
}

// StrictProviderCollision refuses to build a document that declares an object
// type in BOTH the providers: and objects: sections, failing with
// APERTURE_CONFIG_INVALID naming the colliding type(s), instead of applying the
// default precedence rule.
//
// The rule it replaces is the DEFAULT, and it is TOTAL and TYPE-LEVEL: the
// file-backed providers: entry wins, and EVERY inline objects: entry for that
// type is discarded. There is no object-level merge, no field-level merge, and
// no fallback — an inline id the file happens to lack is simply not resolvable,
// exactly as if the entry had never been written. Field-level merging is the
// most useful-sounding behaviour and the most impossible to debug: a rule
// reading a field the CSV silently did not override is a support ticket nobody
// can reproduce. Predictability wins.
//
// The discard is the default because adding a CSV for a type that also has
// inline entries is a normal migration step, not a fault: a seed that booted
// yesterday must not refuse to boot today because someone added a providers:
// row. This option is for the host that would rather read the overlap as an
// authoring mistake — a checked-in seed nobody is mid-migration on — and wants
// the build to stop instead.
//
// The default is not silent either: Document.ProviderCollisions reports exactly
// which types the build discards, so a host with a logger can say so at load.
//
// Every inline entry is validated either way — a malformed declaration fails the
// load whether or not its type is ultimately discarded.
func StrictProviderCollision() BuildOption {
	return func(c *buildConfig) { c.strictProviderCollision = true }
}

// BuildRegistry constructs a live *provider.Registry from the document's three
// wiring sections: providers (a declared implementation per object-type),
// objects (metadata declared inline, served by an in-memory provider.Static per
// object-type), and field_types (the declared type of selected inline metadata
// fields, currently dates). Relative file paths are resolved against baseDir
// (typically the seed file's directory; pass "" to resolve against the process
// CWD). It always returns a usable registry — empty when no section is declared
// — so a caller can wire it unconditionally.
//
// A malformed providers entry (missing object_type, unknown kind, missing path,
// unparseable ttl, or a duplicate object_type), a malformed objects entry
// (missing id, a duplicate id, metadata that is not a mapping, or a value the
// shared value model rejects), and a malformed field_types entry (missing
// object_type, a duplicated object_type, an empty field name, or an unknown
// declared type) are all APERTURE_CONFIG_INVALID; a malformed id passes through
// as APERTURE_IDENTITY_INVALID, and a type claimed twice by two providers
// entries is APERTURE_PROVIDER_INVALID.
//
// A type claimed by BOTH the providers: and objects: sections is not an error:
// the providers: entry wins and every inline entry for that type is discarded
// entirely. The discarded types are reported by Document.ProviderCollisions, and
// StrictProviderCollision turns the collision back into an
// APERTURE_CONFIG_INVALID for hosts that want one.
//
// field_types: applies to the objects: section ONLY. A providers: entry carries
// its own typing (a CSV header's :date / :datetime column suffix), so a declared
// field type is never silently imposed on rows a provider loaded — one type
// declaration in two places could disagree with itself, which is the failure
// this document's derive-the-object-type-from-the-identity rule already avoids
// elsewhere. Inline entries are still fully validated against the declaration
// when a providers: entry wins their type; the canonicalised values are then
// discarded along with the rest of those entries.
//
// The field_types: section is validated FIRST, before anything is registered, so
// a typo'd declaration fails the build even in a document that declares no
// objects at all.
//
// No section touches storage: Apply writes no row for any of them, and an export
// reproduces none.
func (d *Document) BuildRegistry(baseDir string, opts ...BuildOption) (*provider.Registry, error) {
	var cfg buildConfig
	for _, opt := range opts {
		opt(&cfg)
	}
	declared, err := d.fieldTypeIndex()
	if err != nil {
		return nil, err
	}
	reg := provider.NewRegistry()
	// providers: is registered FIRST so the objects: section can see which types
	// are already file-backed. That ordering IS the precedence rule.
	for _, p := range d.Providers {
		if err := registerProvider(reg, p, baseDir); err != nil {
			return nil, err
		}
	}
	if err := d.registerObjects(reg, cfg, declared); err != nil {
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
