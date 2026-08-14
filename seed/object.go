package seed

import (
	"bytes"
	"encoding/json"
	"slices"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/identity"
	"github.com/frankbardon/aperture/provider"
)

// Object declares one host object's metadata INLINE in the seed document, so a
// small or fixed object set needs no separate CSV file beside the seed:
//
//	objects:
//	  - id: account:acme/brand:1
//	    metadata:
//	      tier: gold
//	      seats: 5
//	      tags: [premium, launch]
//	      owner:
//	        dept: eng
//	        lead: alice
//
// Like the providers section — and for the same reason — this is runtime WIRING,
// not model state. BuildRegistry turns the declared objects into an in-memory
// provider.Static registered under each object-type; Apply never writes a row for
// them, and because the model is exported by reading storage back, an export does
// not reproduce them. The "never persist provider data as source of truth"
// Non-Goal (export.go) is untouched: the seed FILE is the source of truth for
// inline object metadata, exactly as it is for provider wiring.
//
// The object-type is DERIVED from the identity's terminal segment
// (account:acme/brand:1 is a "brand"), never declared separately — one fact in
// one place cannot disagree with itself.
type Object struct {
	// ID is the object's canonical identity (e.g. "brand:1" or
	// "account:acme/brand:1"). Its terminal segment's type is the object-type the
	// entry is registered under.
	ID string `yaml:"id" json:"id"`
	// Metadata is the object's attribute bag, carried as raw JSON so the YAML and
	// JSON paths decode it identically and numbers keep their exact form until
	// they are normalised (see decodeObjectMetadata). Legal shapes are the shared
	// value model's: a scalar, an array of scalars, or an object one further level
	// deep. Omit it for an object with no metadata.
	Metadata json.RawMessage `yaml:"metadata,omitempty" json:"metadata,omitempty"`
}

// registerObjects builds the document's inline objects into in-memory providers
// and registers one per object-type, in first-declaration order.
//
// Objects of DIFFERENT types may be interleaved freely in the file — they are
// grouped here — but within a type, declaration order is preserved, because that
// is the order List and Query return.
func (d *Document) registerObjects(reg *provider.Registry) error {
	if len(d.Objects) == 0 {
		return nil
	}
	byType := make(map[string][]provider.Object)
	var types []string // first-declaration order of the object-types
	seen := make(map[string]bool, len(d.Objects))

	for _, o := range d.Objects {
		if o.ID == "" {
			return aerr.New(aerr.APERTURE_CONFIG_INVALID, "seed: object is missing id")
		}
		id, err := identity.Parse(o.ID)
		if err != nil {
			return err // passes through APERTURE_IDENTITY_INVALID
		}
		key := id.String()
		if seen[key] {
			return aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
				"seed: object is declared more than once", map[string]any{"id": key})
		}
		seen[key] = true

		md, err := decodeObjectMetadata(key, o.Metadata)
		if err != nil {
			return err
		}
		typ := terminalType(id)
		if _, ok := byType[typ]; !ok {
			types = append(types, typ)
		}
		byType[typ] = append(byType[typ], provider.Object{ID: id, Metadata: md})
	}

	for _, typ := range types {
		impl, err := provider.NewStatic(byType[typ])
		if err != nil {
			return err
		}
		// TTL 0 never expires: inline metadata is fixed for the life of the
		// process, so a freshness window would only buy re-reads of a value that
		// cannot have changed. Register surfaces APERTURE_PROVIDER_INVALID when the
		// type already has a provider (a providers: entry for the same type).
		if err := reg.Register(typ, impl, provider.WithTTL(0)); err != nil {
			return err
		}
	}
	return nil
}

// terminalType returns the object-type an identity belongs to: the type of its
// terminal segment. It is the one derivation of an object's type in this package.
func terminalType(id identity.Identity) string {
	segs := id.Segments()
	if len(segs) == 0 {
		return ""
	}
	return segs[len(segs)-1].Type
}

// decodeObjectMetadata turns one entry's raw metadata into a validated
// provider.Metadata.
//
// Two things happen here that the raw decode does not do on its own:
//
//   - NUMBERS are normalised the same way every other loader normalises them —
//     an exact integer that fits int64 becomes an int64, anything else a float64.
//     YAML would otherwise hand over an int and JSON a float64, so the same
//     document would answer `object.seats == 5` differently depending on the
//     format it was written in, and differently from the identical value loaded
//     from a CSV :int or :json column. A silent disagreement between two loaders
//     is the failure mode this normalisation exists to remove.
//   - Every field is checked against the shared value model at LOAD time, so a
//     malformed value fails the seed rather than surfacing as an evaluation error
//     on the Check hot path.
//
// Every failure is APERTURE_CONFIG_INVALID naming the object id and, where the
// fault is in one field, the field — never the value, which is host data. A
// value-model rejection is wrapped so the inner APERTURE_METADATA_INVALID (with
// its path/type/depth context) stays in the chain.
func decodeObjectMetadata(id string, raw json.RawMessage) (provider.Metadata, error) {
	if len(raw) == 0 {
		return provider.Metadata{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	// UseNumber is the house pattern (rules/ast.go decodeScalar, csvprovider's
	// json column): defer the int/float choice to normalizeNumbers rather than
	// floating every integer on the way in.
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, aerr.Wrapf(aerr.APERTURE_CONFIG_INVALID, err,
			"seed: object %q has undecodable metadata", id)
	}
	if v == nil {
		// An explicit `metadata:` with no value is an object with no fields, not
		// an error — the same reading as omitting the key.
		return provider.Metadata{}, nil
	}
	md, ok := v.(map[string]any)
	if !ok {
		return nil, aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
			"seed: object metadata must be a mapping of field names to values",
			map[string]any{"id": id, "kind": jsonKind(v)})
	}
	// Fields are walked in sorted order so a document with several offending
	// values always reports the same one.
	fields := make([]string, 0, len(md))
	for name := range md {
		fields = append(fields, name)
	}
	slices.Sort(fields)
	for _, name := range fields {
		val, err := normalizeNumbers(id, name, md[name])
		if err != nil {
			return nil, err
		}
		// The shared value model is the single authority on what a metadata value
		// may be; the seed loader never re-implements those rules.
		if err := provider.ValidateField(name, val); err != nil {
			return nil, aerr.Wrapf(aerr.APERTURE_CONFIG_INVALID, err,
				"seed: object %q metadata field %q is not a legal metadata value", id, name)
		}
		md[name] = val
	}
	return md, nil
}

// normalizeNumbers rewrites every json.Number in a decoded value, at every depth,
// to the int64/float64 the rest of Aperture compares against. The containers are
// the ones just decoded for this object, so rewriting them in place shares
// nothing with any other object.
func normalizeNumbers(id, field string, v any) (any, error) {
	switch x := v.(type) {
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i, nil // matches a CSV :int column and a :json integer
		}
		if f, err := x.Float64(); err == nil {
			return f, nil // matches a CSV :float column and a :json non-integer
		}
		return nil, aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
			"seed: object metadata holds a number no int64 or float64 can represent",
			map[string]any{"id": id, "field": field})
	case map[string]any:
		for k, elem := range x {
			nv, err := normalizeNumbers(id, field, elem)
			if err != nil {
				return nil, err
			}
			x[k] = nv
		}
		return x, nil
	case []any:
		for i, elem := range x {
			nv, err := normalizeNumbers(id, field, elem)
			if err != nil {
				return nil, err
			}
			x[i] = nv
		}
		return x, nil
	default:
		return v, nil
	}
}

// jsonKind names the JSON kind of a decoded value, so a rejection can say what
// the document held without repeating the value itself.
func jsonKind(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "bool"
	case json.Number:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return "unknown"
}
