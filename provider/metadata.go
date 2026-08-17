package provider

import (
	"encoding/json"
	"fmt"
	"slices"

	aerr "github.com/frankbardon/aperture/errors"
)

// The shared metadata value model.
//
// Metadata is host-defined, but its *shape* is not: the rules engine hands
// metadata straight to the expression evaluator, so a value of the wrong shape
// is not a false decision — it is a runtime evaluation error on the Check hot
// path. The model below is therefore enforced at LOAD time, once, by every
// loader (csvprovider, the seed document, a future database-backed provider),
// so a malformed value can never reach a Check.
//
// A metadata field value is one of:
//
//   - a SCALAR: nil, bool, string, any Go integer or float type, or a
//     json.Number;
//   - an ARRAY: a []any whose elements are all scalars;
//   - an OBJECT: a map[string]any whose values are scalars, scalar arrays, or
//     one further object level.
//
// Arrays of objects are rejected at ANY position: expression authors compare
// against arrays with `in` / `not in`, which has no useful meaning over a list
// of maps, and permitting them would invite rules that fail at evaluation time.
// Typed containers ([]string, map[string]string, …) are likewise rejected: the
// model is deliberately spelled in the two types the expression environment and
// the JSON wire format share, so a loader normalises once instead of every
// consumer type-switching.
//
// Two caps bound the model, both configurable through ValueLimits rather than
// baked into a call site:
//
//   - DEPTH (default DefaultMaxValueDepth = 2), counted in containers below the
//     field root — see ValueDepth.
//   - SIZE (default DefaultMaxValueBytes = 64KiB) per field value — see
//     ValueBytes.
//
// Note on time values: time.Time is deliberately NOT a scalar. A rule literal
// is a JSON scalar, so a time.Time in metadata could never be compared against
// one; a loader formats timestamps as RFC 3339 strings instead.
//
// Those strings have exactly two canonical forms, defined in date.go:
// "2006-01-02" for a calendar day and "2006-01-02T15:04:05Z" for a timestamp,
// always UTC. A date therefore rides INSIDE this model as an ordinary string
// scalar — it is not a fourth shape, and nothing here changes to accommodate it.
// The model stays date-blind on purpose: it cannot know which strings a host
// means as dates, so declaring a field to be one, and running its values through
// provider.ParseDateValue, is a loader's job.

const (
	// DefaultMaxValueDepth is the default container depth a metadata field value
	// may reach, counted below the field root: a scalar is depth 0, an array of
	// scalars or a flat object is depth 1, and one nested container inside an
	// object is depth 2.
	DefaultMaxValueDepth = 2
	// DefaultMaxValueBytes is the default per-field-value size cap (64KiB),
	// measured by ValueBytes.
	DefaultMaxValueBytes = 64 << 10
)

// ValueLimits are the tunable bounds of the metadata value model. The zero
// ValueLimits is usable and means "the package defaults"; any field that is
// zero or negative falls back to its default, so a caller overrides only what
// it cares about.
//
//	// accept one more level of nesting, keep the default size cap
//	limits := provider.ValueLimits{MaxDepth: 3}
//	err := limits.ValidateMetadata(md)
type ValueLimits struct {
	// MaxDepth is the deepest container nesting a value may reach, counted
	// below the field root (see ValueDepth). Zero or negative means
	// DefaultMaxValueDepth.
	MaxDepth int
	// MaxBytes is the per-field-value size cap in bytes (see ValueBytes). Zero
	// or negative means DefaultMaxValueBytes.
	MaxBytes int
}

// DefaultValueLimits returns the package defaults spelled out explicitly, for
// callers that want to start from them and adjust one field.
func DefaultValueLimits() ValueLimits {
	return ValueLimits{MaxDepth: DefaultMaxValueDepth, MaxBytes: DefaultMaxValueBytes}
}

// resolve fills zero/negative fields with their defaults.
func (l ValueLimits) resolve() ValueLimits {
	if l.MaxDepth <= 0 {
		l.MaxDepth = DefaultMaxValueDepth
	}
	if l.MaxBytes <= 0 {
		l.MaxBytes = DefaultMaxValueBytes
	}
	return l
}

// ValidateMetadata checks every field of md against the default value model.
// It is the entry point a loader calls once per object, before the metadata is
// handed to a Registry.
//
// Fields are checked in sorted key order, so a document with several offending
// fields always reports the same one.
func ValidateMetadata(md Metadata) error {
	return ValueLimits{}.ValidateMetadata(md)
}

// ValidateField checks a single field's value against the default value model.
func ValidateField(field string, value any) error {
	return ValueLimits{}.ValidateField(field, value)
}

// ValidateMetadata checks every field of md against these limits, returning the
// first violation as an APERTURE_METADATA_INVALID coded error.
func (l ValueLimits) ValidateMetadata(md Metadata) error {
	if len(md) == 0 {
		return nil
	}
	l = l.resolve()
	fields := make([]string, 0, len(md))
	for name := range md {
		fields = append(fields, name)
	}
	slices.Sort(fields)
	for _, name := range fields {
		if err := l.ValidateField(name, md[name]); err != nil {
			return err
		}
	}
	return nil
}

// ValidateField checks one field's value against these limits, returning an
// APERTURE_METADATA_INVALID coded error naming the field and the path within
// the value that offends.
//
// The error never carries the offending VALUE — only its field, its path, and
// its Go type — so a validation failure can be logged and surfaced without
// leaking one account's data into another's diagnostics.
func (l ValueLimits) ValidateField(field string, value any) error {
	if field == "" {
		return aerr.WithContext(aerr.APERTURE_METADATA_INVALID,
			"provider: metadata field name is empty",
			map[string]any{"path": ""})
	}
	l = l.resolve()
	// Shape first: a wrong shape is the more actionable diagnostic, and the
	// walk is cheap.
	if err := l.walk(field, field, value, 0); err != nil {
		return err
	}
	if n := ValueBytes(value); n > l.MaxBytes {
		return aerr.WithContext(aerr.APERTURE_METADATA_INVALID,
			"provider: metadata value exceeds the size cap",
			map[string]any{"field": field, "path": field, "bytes": n, "max_bytes": l.MaxBytes})
	}
	return nil
}

// walk validates one value at the given container depth. depth is the number of
// containers already entered above value, so the field root is 0.
func (l ValueLimits) walk(field, path string, value any, depth int) error {
	switch v := value.(type) {
	case nil, bool, string, json.Number,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64, uintptr,
		float32, float64:
		return nil
	case []any:
		if depth+1 > l.MaxDepth {
			return l.tooDeep(field, path, depth+1)
		}
		for i, elem := range v {
			elemPath := fmt.Sprintf("%s[%d]", path, i)
			switch elem.(type) {
			case []any, map[string]any:
				// Arrays of objects (and nested arrays) are rejected at any
				// position, not merely when they breach the depth cap.
				return aerr.WithContext(aerr.APERTURE_METADATA_INVALID,
					"provider: metadata array element must be a scalar (arrays of objects and nested arrays are not supported)",
					map[string]any{"field": field, "path": elemPath, "type": typeName(elem)})
			}
			if err := l.walk(field, elemPath, elem, depth+1); err != nil {
				return err
			}
		}
		return nil
	case map[string]any:
		if depth+1 > l.MaxDepth {
			return l.tooDeep(field, path, depth+1)
		}
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			if k == "" {
				return aerr.WithContext(aerr.APERTURE_METADATA_INVALID,
					"provider: metadata object has an empty key",
					map[string]any{"field": field, "path": path})
			}
			if err := l.walk(field, path+"."+k, v[k], depth+1); err != nil {
				return err
			}
		}
		return nil
	default:
		return aerr.WithContext(aerr.APERTURE_METADATA_INVALID,
			"provider: unsupported metadata value type; use a scalar, []any, or map[string]any",
			map[string]any{"field": field, "path": path, "type": typeName(value)})
	}
}

func (l ValueLimits) tooDeep(field, path string, depth int) error {
	return aerr.WithContext(aerr.APERTURE_METADATA_INVALID,
		"provider: metadata value nests deeper than the depth cap",
		map[string]any{"field": field, "path": path, "depth": depth, "max_depth": l.MaxDepth})
}

// typeName renders a value's Go type for an error context. It names the TYPE
// only, never the value, so no host data reaches a diagnostic.
func typeName(v any) string {
	if v == nil {
		return "nil"
	}
	return fmt.Sprintf("%T", v)
}

// ValueDepth reports a metadata value's container depth, counted below the
// field root: every array or object entered adds one level.
//
//	tags:  ["a", "b"]              -> 1
//	owner: {dept: "eng"}           -> 1
//	owner: {lead: {name: "x"}}     -> 2
//	owner: {tags: ["a"]}           -> 2
//	owner: {members: [{id: 1}]}    -> 3
//
// A scalar is 0 and an empty container is 1. Depth is a property of the value
// alone; whether the value is LEGAL additionally depends on the array-of-objects
// rule and the caps (see ValueLimits.ValidateField).
func ValueDepth(value any) int {
	switch v := value.(type) {
	case []any:
		deepest := 0
		for _, elem := range v {
			if d := ValueDepth(elem); d > deepest {
				deepest = d
			}
		}
		return 1 + deepest
	case map[string]any:
		deepest := 0
		for _, elem := range v {
			if d := ValueDepth(elem); d > deepest {
				deepest = d
			}
		}
		return 1 + deepest
	default:
		return 0
	}
}

// ValueBytes reports the measured byte size of a metadata value, the quantity
// ValueLimits.MaxBytes caps. The measure is deliberately structural rather than
// an encoding round-trip, so it never allocates a serialised copy of the value
// on a load path:
//
//	nil                 0
//	bool                1
//	integer / float     8
//	json.Number         len of its text
//	string              len in BYTES (not runes)
//	[]any               sum of its elements
//	map[string]any      sum of len(key) + size of each value
//
// Container framing costs nothing, so the number is a payload measure, not a
// wire size.
func ValueBytes(value any) int {
	switch v := value.(type) {
	case nil:
		return 0
	case bool:
		return 1
	case string:
		return len(v)
	case json.Number:
		return len(string(v))
	case []any:
		total := 0
		for _, elem := range v {
			total += ValueBytes(elem)
		}
		return total
	case map[string]any:
		total := 0
		for k, elem := range v {
			total += len(k) + ValueBytes(elem)
		}
		return total
	case int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64, uintptr,
		float32, float64:
		return 8
	default:
		// An unsupported type never survives validation; measure it as its
		// rendered type name so the size path stays total.
		return len(typeName(v))
	}
}

// MetadataBytes reports the summed ValueBytes of every field in md, including
// the field names. It is a convenience for loaders that want to log or bound an
// object's total payload; the cap enforced by ValidateMetadata is per FIELD,
// not per object.
func MetadataBytes(md Metadata) int {
	total := 0
	for name, value := range md {
		total += len(name) + ValueBytes(value)
	}
	return total
}
