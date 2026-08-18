package cli

import (
	"encoding/json"
	"strconv"
	"strings"

	aerr "github.com/frankbardon/aperture/errors"

	ucli "github.com/urfave/cli/v3"
)

// The metadata filter on the command line.
//
// `aperture enumerate` narrows its result with object-metadata predicates, and
// the predicate is TYPED: provider.MatchFields compares the string "5" against a
// string field and the number 5 against a numeric one and never reconciles the
// two. A shell flag, though, only ever carries a string. Guessing is not an
// option — parsing "5" into a number here would make an enumeration return
// objects a Check then denies — so the command offers both spellings and says
// plainly which is which:
//
//	--field key=value    the value is ALWAYS a string; repeatable
//	--fields-json '{…}'  a JSON object, so a number / bool / list means itself
//
// Both may be supplied. --fields-json is merged FIRST and --field entries then
// override by key, so a stored JSON body can be reused with one value swapped
// from the shell. The precedence is stated in the flag usage and in the
// command's description rather than left to be discovered.
//
// Nothing here re-interprets a value beyond the one decision above: a --field
// value becomes a Go string verbatim, and a --fields-json value becomes exactly
// the Go shape encoding/json yields (nil, bool, float64, string, []any,
// map[string]any) — the same shapes internal/wire/rpc.FieldsFromWire produces
// for the Twirp surface, so the three surfaces filter identically.
const (
	fieldFlagName      = "field"
	fieldsJSONFlagName = "fields-json"
)

// metadataFilterFlags are the two flags that build an enumerate metadata filter.
// They are defined together because their meaning is a pair: neither usage
// string is complete without the other's precedence rule.
func metadataFilterFlags() []ucli.Flag {
	return []ucli.Flag{
		&ucli.StringSliceFlag{
			Name: fieldFlagName,
			Usage: "object-metadata predicate as key=value, repeatable; the value is ALWAYS a string, " +
				"so --field seats=5 matches the string \"5\" and never the number 5 (use --fields-json for that). " +
				"Overrides --fields-json on a key collision",
		},
		&ucli.StringFlag{
			Name: fieldsJSONFlagName,
			Usage: "object-metadata predicates as a JSON object, for values that are genuinely a number, " +
				"bool, or list (e.g. '{\"seats\":5,\"active\":true,\"tags\":[\"a\"]}'). " +
				"Merged first; --field entries then override by key",
		},
	}
}

// parseMetadataFilter builds the enumerate Fields predicate from the two flags.
//
// fieldsJSON is merged first and kvs override by key, so the precedence is the
// one documented on the flags. The result is nil when neither flag was given —
// nil is "no predicate", which the engine does not even consult a metadata
// source for, and it is deliberately indistinguishable from an unfiltered
// enumeration.
//
// Every rejection is an APERTURE_INVALID_INPUT coded error naming the offending
// input, never a silent skip: a dropped predicate would WIDEN the result, and a
// filter that silently widens is a filter that authorizes.
func parseMetadataFilter(fieldsJSON string, kvs []string) (map[string]any, error) {
	out := map[string]any{}

	if strings.TrimSpace(fieldsJSON) != "" {
		// Decoded into `any` rather than straight into a map so a JSON array or
		// scalar reports "not an object" instead of a decoder message about Go
		// types. A non-finite number needs no separate check: JSON has no NaN or
		// infinity literal, and a magnitude beyond float64 fails to decode — the
		// same rejection FieldsFromWire makes for the wire surface, and for the
		// same reason (an unsatisfiable predicate reads as "no access").
		var decoded any
		if err := json.Unmarshal([]byte(fieldsJSON), &decoded); err != nil {
			return nil, aerr.Wrapf(aerr.APERTURE_INVALID_INPUT, err,
				"--%s %s is not valid JSON", fieldsJSONFlagName, quoteFlagValue(fieldsJSON))
		}
		obj, ok := decoded.(map[string]any)
		if !ok {
			return nil, aerr.Newf(aerr.APERTURE_INVALID_INPUT,
				"--%s %s must be a JSON object of field predicates, e.g. '{\"seats\":5}'",
				fieldsJSONFlagName, quoteFlagValue(fieldsJSON))
		}
		for name, v := range obj {
			out[name] = v
		}
	}

	for _, kv := range kvs {
		name, value, found := strings.Cut(kv, "=")
		if !found {
			return nil, aerr.Newf(aerr.APERTURE_INVALID_INPUT,
				"--%s %s is not key=value; a predicate needs an '=', e.g. --%s tier=premium",
				fieldFlagName, quoteFlagValue(kv), fieldFlagName)
		}
		if name == "" {
			return nil, aerr.Newf(aerr.APERTURE_INVALID_INPUT,
				"--%s %s has an empty field name", fieldFlagName, quoteFlagValue(kv))
		}
		// A string, always. Everything after the FIRST '=' is the value, so
		// --field expr=a=b wants the string "a=b".
		out[name] = value
	}

	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// quoteFlagValue renders a flag value for an error message, bounded so a large
// --fields-json body cannot turn one validation failure into a screenful.
func quoteFlagValue(s string) string {
	const max = 160
	r := []rune(s)
	if len(r) > max {
		return strconv.Quote(string(r[:max])) + "… (truncated)"
	}
	return strconv.Quote(s)
}
