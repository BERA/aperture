// This file is HAND-WRITTEN and sits beside the generated service.pb.go /
// service.twirp.go. `make proto` regenerates those two and leaves this one
// alone, so the wire package can own the one thing the generator cannot: the
// meaning of EnumerateRequest.fields on the Go side of the boundary.

package rpc

import (
	"math"
	"sort"

	aerr "github.com/frankbardon/aperture/errors"
	"google.golang.org/protobuf/types/known/structpb"
)

// The metadata-filter wire encoding, in both directions.
//
// EnumerateRequest.fields rides as map<string, google.protobuf.Value> rather
// than as a JSON string because the predicate it feeds — provider.MatchFields —
// compares by TYPE: a string "5" never matches a numeric 5, a list is a
// container want compared by equality, and numbers compare across Go numeric
// types by value. A JSON blob in a string field would carry those distinctions
// too, but only to a client willing to hand-assemble JSON; Value states them in
// the schema, so a generated client in any language builds a well-formed
// predicate and a malformed one is rejected at the boundary rather than deep
// inside the engine.
//
// The conversion is deliberately total and lossless in the direction that
// matters — Value -> any produces exactly the Go shapes provider.Metadata
// admits (string, float64, bool, nil, []any, map[string]any) — with one
// documented caveat: Value carries every number as a double, so an integer
// beyond 2^53 is not representable on the wire and must be sent as a string.

// FieldsFromWire converts the wire's field predicates into the map[string]any
// the engine's Fields predicate is written against.
//
// The mapping is Value's own, restated explicitly rather than delegated to
// structpb's AsInterface so the boundary owns it: null -> nil, number ->
// float64, string -> string, bool -> bool, list -> []any, struct ->
// map[string]any, recursively. Nothing is re-interpreted on the way through —
// a number is never parsed out of a string, and a string is never coerced to a
// number, because that coercion is precisely what would make an enumeration's
// answer disagree with a Check's.
//
// An ABSENT map is indistinguishable from a nil Go map: both return nil, which
// the engine reads as "no predicate" and does not even consult a metadata
// source for.
//
// The one rejection is a non-finite number (NaN / ±Inf). structpb renders those
// as the strings "NaN" / "Infinity", which would silently turn a numeric want
// into a string want and match nothing — an unsatisfiable predicate that reads
// as "no access". It is an APERTURE_INVALID_INPUT error instead.
func FieldsFromWire(fields map[string]*structpb.Value) (map[string]any, error) {
	if len(fields) == 0 {
		return nil, nil
	}
	// Sorted so a request carrying two bad keys reports the same one every
	// time; a map's iteration order would make the error nondeterministic.
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make(map[string]any, len(fields))
	for _, name := range names {
		v, err := valueFromWire(fields[name])
		if err != nil {
			return nil, aerr.Wrapf(aerr.APERTURE_INVALID_INPUT, err,
				"rpc: enumerate field predicate %q is not a legal filter value", name)
		}
		out[name] = v
	}
	return out, nil
}

// FieldsToWire converts a Go predicate map into the wire encoding — the inverse
// of FieldsFromWire, so a Go caller building a request over Twirp encodes the
// same predicate the library would evaluate in-process.
//
// A nil or empty map converts to a nil map, so "no predicate" travels as an
// omitted field.
//
// A Go integer wider than 2^53 converts without error but arrives as the
// nearest double: that loss is a property of google.protobuf.Value, not
// something this function can repair. Send such a key as a string.
func FieldsToWire(fields map[string]any) (map[string]*structpb.Value, error) {
	if len(fields) == 0 {
		return nil, nil
	}
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make(map[string]*structpb.Value, len(fields))
	for _, name := range names {
		v, err := structpb.NewValue(normalizeForWire(fields[name]))
		if err != nil {
			return nil, aerr.Wrapf(aerr.APERTURE_INVALID_INPUT, err,
				"rpc: enumerate field predicate %q cannot be encoded on the wire", name)
		}
		out[name] = v
	}
	return out, nil
}

// valueFromWire converts one Value, recursively. A nil *Value — a map entry
// explicitly carrying no value — reads as null, i.e. a nil want, which the
// Fields contract says an absent metadata field still does not match.
func valueFromWire(v *structpb.Value) (any, error) {
	if v == nil {
		return nil, nil
	}
	switch k := v.GetKind().(type) {
	case *structpb.Value_NullValue:
		return nil, nil
	case *structpb.Value_NumberValue:
		if math.IsNaN(k.NumberValue) || math.IsInf(k.NumberValue, 0) {
			return nil, aerr.New(aerr.APERTURE_INVALID_INPUT,
				"a filter number must be finite; NaN and infinity have no metadata counterpart")
		}
		return k.NumberValue, nil
	case *structpb.Value_StringValue:
		return k.StringValue, nil
	case *structpb.Value_BoolValue:
		return k.BoolValue, nil
	case *structpb.Value_ListValue:
		items := k.ListValue.GetValues()
		out := make([]any, 0, len(items))
		for _, item := range items {
			elem, err := valueFromWire(item)
			if err != nil {
				return nil, err
			}
			out = append(out, elem)
		}
		return out, nil
	case *structpb.Value_StructValue:
		in := k.StructValue.GetFields()
		out := make(map[string]any, len(in))
		for name, field := range in {
			elem, err := valueFromWire(field)
			if err != nil {
				return nil, err
			}
			out[name] = elem
		}
		return out, nil
	default:
		// A Value with no kind set at all. Proto3 cannot express this through a
		// generated client, but a hand-rolled one can, so it is a caller error
		// rather than a panic.
		return nil, aerr.New(aerr.APERTURE_INVALID_INPUT,
			"a filter value must set one of null/number/string/bool/list/struct")
	}
}

// normalizeForWire widens the numeric Go types structpb.NewValue does not
// itself accept (int8/int16/uint8/uint16 and float32) so a predicate assembled
// from, say, a decoded metadata value encodes rather than erroring. The wider
// types NewValue already handles (int, int32, int64, uint, uint32, uint64,
// float64) are left to it, including its float64 narrowing of a large integer.
func normalizeForWire(v any) any {
	switch n := v.(type) {
	case int8:
		return int64(n)
	case int16:
		return int64(n)
	case uint8:
		return uint64(n)
	case uint16:
		return uint64(n)
	case float32:
		return float64(n)
	case []any:
		out := make([]any, len(n))
		for i, elem := range n {
			out[i] = normalizeForWire(elem)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(n))
		for name, elem := range n {
			out[name] = normalizeForWire(elem)
		}
		return out
	}
	return v
}
