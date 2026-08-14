package provider

import "reflect"

// The Filter.Fields predicate, implemented once for every provider.
//
// Fields is host-interpreted, but "host-interpreted" cannot mean "each provider
// invents its own rule": Query is how scope enumeration bounds itself, so two
// providers disagreeing about what Fields{"tags": "premium"} means is two
// different answers to the same authorization question. The rule is therefore
// stated on Filter (provider.go) as a CONTRACT and implemented here, once, so a
// provider satisfies it by calling MatchFields rather than by re-deriving it.
//
// The rule, in one sentence: a COLLECTION field matches by MEMBERSHIP and every
// other field by EQUALITY, with elements compared by type.
//
// # Why membership
//
// A tag list is the reason Fields exists in practice, and equality against a
// whole array is never what a caller filtering on one means. Before this, the
// predicate was fmt.Sprint string equality, so a []any field was compared
// against its literal rendering ("[premium launch]") and a tag filter could not
// match anything useful.
//
// # Why typed comparison
//
// The same reason the CSV loader coerces list elements: the expression evaluator
// does no numeric/string coercion, so 5 in object.seats is FALSE against the
// strings ["3","5"]. If Query's membership test coerced where a rule does not,
// Enumerate would return objects a Check then denies — a silent disagreement
// between the two halves of the decision API. ValuesEqual is therefore modelled
// on expr-lang's own equality (see its correspondence note), NOT on
// fmt.Sprint.

// MatchFields reports whether md satisfies every predicate in fields, per the
// contract documented on Filter. An empty or nil fields map matches everything,
// and a field absent from md never matches.
//
// It is the canonical implementation of Filter.Fields: an ObjectProvider that
// cannot push the predicate down into its own storage should call this rather
// than write its own, so every provider agrees on what a Query selects.
func MatchFields(md Metadata, fields map[string]any) bool {
	for name, want := range fields {
		value, present := md[name]
		if !present {
			// Absent never matches — including against a nil want. A provider
			// that has no opinion on a field is not the same as one that holds
			// nil there.
			return false
		}
		if !MatchField(value, want) {
			return false
		}
	}
	return true
}

// MatchField reports whether one metadata field VALUE satisfies one Filter.Fields
// want. Presence is the caller's business (see MatchFields); this is the value
// rule alone, exported for a provider that pushes name lookup down into storage
// and only needs the comparison.
//
//	value              want          result
//	["a","b"]          "a"           true   — membership
//	["a","b"]          "c"           false
//	[int64(3),int64(5)] 5            true   — 5 is numeric, elements are numeric
//	[int64(3),int64(5)] "5"          false  — a string never matches a number
//	["a","b"]          []any{"a","b"} true  — a container want is EQUALITY
//	["a","b"]          []any{"a"}     false — not "subset of"
//	"gold"             "gold"        true   — scalar equality
//	{"dept":"eng"}     "dept"        false  — NOT key membership
//	{"dept":"eng"}     map[string]any{"dept":"eng"}  true — deep equality
func MatchField(value, want any) bool {
	switch want.(type) {
	case []any, map[string]any:
		// A want that is itself a container compares by EQUALITY, at both ends.
		// Membership would be unsatisfiable — a legal array holds only scalars,
		// so no element can ever equal an array or an object — and an
		// unsatisfiable predicate that silently selects nothing is exactly the
		// failure mode this story removes.
		return ValuesEqual(value, want)
	}
	if list, ok := value.([]any); ok {
		for _, elem := range list {
			if ValuesEqual(elem, want) {
				return true
			}
		}
		return false
	}
	// Scalars AND objects fall through to equality. An object field is the
	// interesting case: it is NOT key membership (that is the rules engine's
	// hasKey operator, which a Fields predicate has no way to spell), so a
	// scalar want against an object field is simply false — never a panic, and
	// never the accidental fmt.Sprint match that would let "map[dept:eng]"
	// select a row.
	return ValuesEqual(value, want)
}

// ValuesEqual reports whether two metadata values are equal for the purposes of
// Filter.Fields. It is the leaf comparison MatchField uses for a scalar field,
// for an array element, and for a whole container, and it is exported so a
// provider indexing its own data compares the same way Aperture would.
//
// The rule:
//
//   - NUMBERS compare across Go numeric types by VALUE, so int(5), int64(5),
//     and float64(5) are all equal;
//   - a STRING equals only a string, and a BOOL only a bool — a number is never
//     equal to its string spelling ("5" != 5), which is the property that keeps
//     Query's answer and a rule's answer the same;
//   - nil equals only nil;
//   - anything else (containers, json.Number, uintptr, host types) compares by
//     reflect.DeepEqual.
//
// # Correspondence with the rules engine
//
// These are expr-lang's `==` / `in` semantics, reimplemented rather than
// imported: provider is a strict leaf (identity + errors + stdlib only), so it
// may not depend on expr-lang, while rules/ compiles against it directly and its
// collection operators call expr's runtime.Equal. The two agree on every value
// the metadata value model admits — the numeric cross-type table, the refusal to
// coerce a string to a number, nil, and the DeepEqual fallback are expr's own,
// in that order. Two deliberate divergences, both outside the value model:
// time.Time and time.Duration (expr special-cases them; metadata cannot hold
// either) and a uint64 above math.MaxInt64 (expr narrows through int and wraps;
// this compares exactly). csvprovider's external membership_equivalence_test.go
// pins the correspondence by running both.
func ValuesEqual(a, b any) bool {
	if an, ok := asNumber(a); ok {
		bn, ok := asNumber(b)
		return ok && an.equal(bn)
	}
	switch av := a.(type) {
	case string:
		bv, ok := b.(string)
		return ok && av == bv
	case bool:
		bv, ok := b.(bool)
		return ok && av == bv
	}
	if isNil(a) || isNil(b) {
		return isNil(a) && isNil(b)
	}
	return reflect.DeepEqual(a, b)
}

// numKind distinguishes the three numeric representations a comparison has to
// keep apart to stay exact: a signed integer, an unsigned integer that may not
// fit one, and a float.
type numKind uint8

const (
	numNone numKind = iota
	numInt
	numUint
	numFloat
)

// number is a metadata value's numeric view.
type number struct {
	kind numKind
	i    int64
	u    uint64
	f    float64
}

// asNumber views v as a number, reporting false for every non-numeric value.
//
// json.Number and uintptr are deliberately NOT numeric here: expr-lang has no
// case for either, so both fall to its DeepEqual tail, and diverging would put
// Query's answer at odds with a rule's. A loader normalises json.Number to an
// int64 or a float64 anyway (csvprovider does, for exactly this reason).
func asNumber(v any) (number, bool) {
	switch x := v.(type) {
	case int:
		return number{kind: numInt, i: int64(x)}, true
	case int8:
		return number{kind: numInt, i: int64(x)}, true
	case int16:
		return number{kind: numInt, i: int64(x)}, true
	case int32:
		return number{kind: numInt, i: int64(x)}, true
	case int64:
		return number{kind: numInt, i: x}, true
	case uint:
		return number{kind: numUint, u: uint64(x)}, true
	case uint8:
		return number{kind: numUint, u: uint64(x)}, true
	case uint16:
		return number{kind: numUint, u: uint64(x)}, true
	case uint32:
		return number{kind: numUint, u: uint64(x)}, true
	case uint64:
		return number{kind: numUint, u: x}, true
	case float32:
		return number{kind: numFloat, f: float64(x)}, true
	case float64:
		return number{kind: numFloat, f: x}, true
	}
	return number{kind: numNone}, false
}

// equal compares two numbers by value. A float on either side promotes both; two
// integers of different signedness compare exactly rather than through a
// conversion that could wrap.
func (n number) equal(o number) bool {
	switch {
	case n.kind == numFloat || o.kind == numFloat:
		return n.float() == o.float()
	case n.kind == numUint && o.kind == numUint:
		return n.u == o.u
	case n.kind == numUint:
		return o.i >= 0 && uint64(o.i) == n.u
	case o.kind == numUint:
		return n.i >= 0 && uint64(n.i) == o.u
	default:
		return n.i == o.i
	}
}

// float widens a number for a comparison that involves a float.
func (n number) float() float64 {
	switch n.kind {
	case numInt:
		return float64(n.i)
	case numUint:
		return float64(n.u)
	default:
		return n.f
	}
}

// isNil reports whether v is nil, including a typed nil container. It mirrors
// expr-lang's own nil test, which runs before its DeepEqual tail —
// reflect.DeepEqual(nil, nil) is false, so the check cannot be skipped.
func isNil(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface,
		reflect.Map, reflect.Pointer, reflect.Slice, reflect.UnsafePointer:
		return rv.IsNil()
	}
	return false
}
