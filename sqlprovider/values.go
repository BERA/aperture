package sqlprovider

import (
	"bytes"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"io"
	"strings"
	"time"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/provider"
)

// The driver-value mapping table.
//
// This file owns exactly one question: a driver handed database/sql a value and
// database/sql scanned it into an `any` — what metadata value is that?
//
// The answer is a closed table, not an inference, and it is closed because the
// alternative is a value the expression evaluator silently mis-compares. The
// table lives in metadataValue's type switch, is named in mappedDriverTypes, and
// TestDriverValueMappingTableMatchesTheTypeSwitch parses this file and fails if
// the two ever disagree. Adding a case without adding it to the table (or the
// reverse) is a build-red change, on purpose.
//
// The rules were derived from measurement rather than recollection: both
// candidate Postgres drivers were probed against a live server, one SELECT per
// type, printing reflect.TypeOf of the scanned value. Three of those measured
// results are why the table looks the way it does, and each is called out at the
// case that implements it.
//
// See the "Driver values become metadata" section of the package doc for the
// developer-facing statement of the same rules.

// mappedDriverTypes names every Go type metadataValue accepts, in the order the
// package doc lists them. It is the registry half of the mapping contract: the
// unmapped-type diagnostic renders it, so a developer is told what IS accepted
// rather than only what is not, and the contract test diffs it against the type
// switch itself.
var mappedDriverTypes = []string{
	"nil",
	"bool",
	"int64",
	"float64",
	"string",
	"[]byte",
	"time.Time",
}

// Reasons a value failed to map. They exist so a caller — or a test — can branch
// on the cause without matching on message text, in the spirit of
// provider.DateReason.
const (
	// reasonUnmappedType is a driver value whose Go type is not in the table.
	reasonUnmappedType = "unmapped_type"
	// reasonJSONDecode is a []byte that is not a single well-formed JSON value.
	reasonJSONDecode = "json_decode"
	// reasonJSONNumber is a JSON number no int64 and no float64 can represent.
	reasonJSONNumber = "json_number"
	// reasonDate is a time.Time outside the canonical date value model's range.
	reasonDate = "date"
)

// metadataValue maps one scanned driver value to a metadata value, reporting
// present=false for a value that OMITS its field rather than storing something.
//
// The table:
//
//	nil        →  the field is OMITTED (absent is not zero)
//	bool       →  the scalar, as-is
//	int64      →  the scalar, as-is
//	float64    →  the scalar, as-is
//	string     →  the scalar, as-is
//	[]byte     →  JSON-decoded; a decode failure is a hard error
//	time.Time  →  .UTC(), then the canonical date value model's text
//	anything else → a hard error naming the column and the Go type
//
// Errors name the COLUMN and the row's KEY — the developer's own statement and
// the caller's own input — and never the value, which is host data belonging to
// some account and which ends up in a log.
//
// id is that key rendered as text, and it is a plain string rather than an
// identity.Identity because this table serves BOTH seams: an object provider
// passes its identity's String(), an attribute provider passes the bare subject
// key it was handed. The mapping itself does not depend on which — one table,
// one set of rules, so a host that reads a column through an object provider and
// the same column through an attribute provider gets the same value and
// therefore the same decision.
func metadataValue(v any, column, id string) (value any, present bool, err error) {
	switch x := v.(type) {
	case nil:
		// A NULL OMITS its field; it does not store nil. This is csvprovider's
		// absent-vs-zero rule applied to SQL, and it is load-bearing: an absent
		// field never matches a Filter.Fields predicate and lets a rule supply
		// its own default, whereas a stored nil is a value that COMPARES — which
		// is how a NULL end_date silently satisfies every "before" rule ever
		// written against it.
		return nil, false, nil
	case bool:
		return x, true, nil
	case int64:
		return x, true, nil
	case float64:
		return x, true, nil
	case string:
		// Note what is NOT here: no attempt to recognise a Postgres array
		// literal. A text[] arrives as the raw literal "{a,b}" — measured, in
		// both drivers — and parsing that correctly (quoting, embedded commas,
		// escapes, NULLs) is a real parser. Casting to JSON in the statement is
		// the supported spelling; see the trap in the package doc.
		return x, true, nil
	case []byte:
		return jsonValue(x, column, id)
	case time.Time:
		return dateValue(x, column, id)
	default:
		return nil, false, aerr.WithContext(aerr.APERTURE_SQL_PROVIDER_SCAN,
			"sqlprovider: result column holds a driver value of a Go type this provider does not map; cast or serialise it in the statement",
			map[string]any{
				"id":     id,
				"column": column,
				"type":   goTypeName(v),
				"mapped": strings.Join(mappedDriverTypes, ", "),
				"reason": reasonUnmappedType,
			})
	}
}

// jsonValue decodes a []byte column as JSON.
//
// []byte means JSON here, unconditionally. That is the decision the measurements
// force: no driver hands back a []any for a Postgres array, so a list-valued
// field — and E3's object references are lists — can only arrive by being cast
// to JSON in the developer's statement (to_jsonb(tags) AS tags), which reaches
// this package as jsonb, which is []byte. Falling back to "treat it as a string
// if it does not decode" would make the same column change TYPE depending on its
// contents, and a lib/pq numeric of 1.50 would arrive as the number 1.5 while a
// uuid stayed a string. So a decode failure is a HARD error naming the column
// and the row, never a silent coercion.
//
// The consequence a developer must know: a bytea column does not work here.
// Encode it in the statement (encode(bytes,'base64')) or leave it out of the
// SELECT list — an access-control decision has no business reading a blob.
//
// Every container comes fresh from the decoder on every call, which is what
// keeps provider.Metadata read-only transitively at depth. Nothing here aliases
// b, which matters twice over: a driver may reuse that buffer for the next row.
func jsonValue(b []byte, column, id string) (any, bool, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	// UseNumber is the house pattern (csvprovider's json column, rules/ast.go's
	// decodeScalar): defer the int/float choice to normalizeJSONNumbers instead
	// of floating every integer.
	dec.UseNumber()

	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false, aerr.WithContext(aerr.APERTURE_SQL_PROVIDER_SCAN,
			"sqlprovider: result column is a []byte that is not valid JSON; a []byte column is decoded as JSON, so cast a non-JSON value in the statement (to_jsonb(...) for an array, ::text for an identifier, ::float8 for a numeric)",
			map[string]any{"id": id, "column": column, "reason": reasonJSONDecode, "error": err.Error()})
	}
	if _, err := dec.Token(); !stderrors.Is(err, io.EOF) {
		return nil, false, aerr.WithContext(aerr.APERTURE_SQL_PROVIDER_SCAN,
			"sqlprovider: result column holds trailing content after its JSON value",
			map[string]any{"id": id, "column": column, "reason": reasonJSONDecode})
	}
	if v == nil {
		// A JSON null omits its field, exactly as a SQL NULL does. The two say
		// the same thing — this object has no value here — and to_jsonb(NULL)
		// is how the first becomes the second, so they must not disagree.
		return nil, false, nil
	}
	nv, err := normalizeJSONNumbers(v, column, id)
	if err != nil {
		return nil, false, err
	}
	return nv, true, nil
}

// normalizeJSONNumbers rewrites every json.Number below v to an int64 or a
// float64, in place for the containers it owns.
//
// Numbers are the one place a JSON column could disagree with the rest of the
// row, and the disagreement is invisible: the evaluator does no numeric
// coercion, so object.limits.seats == object.seats would be a silent false with
// a float64 on one side and an int64 on the other. The rule is therefore exactly
// the scalar columns' rule — an exact integer that fits int64 becomes an int64,
// everything else a float64 — applied at every depth. It is csvprovider's rule,
// deliberately identical, so a host that migrates a CSV to a table gets the same
// decisions.
func normalizeJSONNumbers(v any, column, id string) (any, error) {
	switch x := v.(type) {
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i, nil
		}
		if f, err := x.Float64(); err == nil {
			return f, nil
		}
		return nil, aerr.WithContext(aerr.APERTURE_SQL_PROVIDER_SCAN,
			"sqlprovider: result column holds a JSON number no int64 or float64 can represent",
			map[string]any{"id": id, "column": column, "reason": reasonJSONNumber})
	case map[string]any:
		for k, elem := range x {
			nv, err := normalizeJSONNumbers(elem, column, id)
			if err != nil {
				return nil, err
			}
			x[k] = nv
		}
		return x, nil
	case []any:
		for i, elem := range x {
			nv, err := normalizeJSONNumbers(elem, column, id)
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

// dateValue canonicalises a time.Time through the shared date value model.
//
// It converts to UTC FIRST, and that conversion is the whole point. A
// timestamptz comes back in the PROCESS's local zone — measured, in both drivers
// — so the same row read on two hosts would otherwise yield two different
// strings, and near midnight two different calendar DAYS. Aperture has no zone
// knob anywhere: provider/date.go rejects an explicit offset outright and
// rules/ is source-scanned for time.Local. UTC at this boundary is how that
// stays true.
//
// The value is then routed through provider.ParseDateValue rather than merely
// formatted, so this loader accepts exactly what every other loader accepts —
// one definition of a date, not a second one that agrees today.
//
// Granularity is NOT inferable: a Postgres date and a timestamp are the same Go
// type, so every time.Time becomes the DATETIME form. A day-granular column is
// the developer's `col::text`, which arrives as a string and is stored as-is.
// The alternative — a per-column granularity declaration in YAML — would be a
// second spelling for an intent the statement already carries, and two spellings
// for one intent drift apart.
func dateValue(t time.Time, column, id string) (any, bool, error) {
	s := t.UTC().Format(provider.DateTimeLayout)
	v, err := provider.ParseDateValue(s)
	if err != nil {
		// Reachable for an instant the canonical form cannot spell — a year
		// outside four digits, which formats to a string no parser accepts.
		return nil, false, aerr.WithContext(aerr.APERTURE_SQL_PROVIDER_SCAN,
			"sqlprovider: result column holds a timestamp the canonical date value model cannot represent",
			map[string]any{
				"id":       id,
				"column":   column,
				"reason":   reasonDate,
				"expected": provider.DateTimeLayout,
			})
	}
	return v.String(), true, nil
}

// goTypeName names a value's Go type for a diagnostic. It renders the TYPE and
// never the value — a metadata value is host data, frequently another account's,
// and an error is a thing that gets logged.
func goTypeName(v any) string {
	if v == nil {
		return "nil"
	}
	return fmt.Sprintf("%T", v)
}
