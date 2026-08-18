package rules

import (
	"context"
	"fmt"
	"sync"
)

// This file is the EVALUATION-NOTES CHANNEL: the seam that carries diagnostic
// observations out of a rule evaluation without changing what the evaluation
// DECIDES.
//
// Why it exists. Aperture's deny-safety policy (E5-S1) is that a collection
// operator against a value of the wrong shape evaluates to FALSE rather than
// raising APERTURE_RULE_EVAL — no single mistyped metadata field can break every
// Check that touches it. Silent-false, though, is exactly how an access-control
// bug hides, so every mismatch is RECORDED here and surfaced in Explain. The
// decision path stays deny-safe; the diagnostic path stays honest.
//
// Design constraints, in the order they mattered:
//
//  1. Zero cost on the hot path. Check and Enumerate install no collector, so
//     evaluation allocates nothing extra and records nothing — the sink is a nil
//     pointer and every record site is a nil check.
//  2. No signature churn through scope. A rule evaluation is reached from the
//     decision engine through scope.ScopeResolver -> scope.RuleEvaluator, neither
//     of which this change may widen. The collector therefore travels in the
//     CONTEXT, which every one of those seams already carries.
//  3. Reusable. Note is a general observation (Kind + path + shape), not a
//     hard-coded message: a future diagnostic adds a NoteKind and reuses the same
//     channel end to end.
//
// A note NEVER carries a metadata value or any cross-account data. It records the
// variable PATH, the SHAPE expected, and the SHAPE found — never the content of
// the field. TestNoteNeverCarriesTheValue pins that.

// NoteKind classifies an evaluation note. It is a closed set today; adding a kind
// is how a future diagnostic joins the channel.
type NoteKind string

const (
	// NoteShapeMismatch records that a collection operator was applied to a value
	// that is not a collection. The comparison evaluated to false (deny-safe).
	NoteShapeMismatch NoteKind = "shape_mismatch"
	// NoteAbsentField records that an operator MATCHED because the field it reads
	// is absent, rather than because of anything the object says. This is the
	// "negative operator grants on missing data" shape — nin / hasNone /
	// subsetOf / isEmpty over an object that simply lacks the field — which grants
	// access and is otherwise invisible.
	//
	// No DATE operator records this: all eight are positive, so an absent operand
	// can only deny (see the policy note in date.go).
	NoteAbsentField NoteKind = "absent_field"
	// NoteDateInvalid records that a date operator was applied to a STRING that is
	// not one of the two canonical date forms. The shape was right and the content
	// was not, which is why it is distinct from NoteShapeMismatch: the fix is to
	// canonicalise the data (or to declare the field as a date in the loader, so
	// it is validated at load) rather than to change its type. The comparison
	// evaluated to false (deny-safe).
	NoteDateInvalid NoteKind = "date_invalid"
	// NoteDateBoundsInverted records that a `between` was written with its lower
	// bound after its upper bound. That range matches nothing — bounds are never
	// reordered, because reordering would silently decide the author meant
	// something other than what they wrote — so the comparison is false for every
	// object, which is indistinguishable from a rule nothing satisfies unless it
	// is said out loud.
	NoteDateBoundsInverted NoteKind = "date_bounds_inverted"
	// NoteDanglingReference records that a DECLARED OBJECT REFERENCE pointed at an
	// identity the target type's provider no longer serves — a dataset still
	// listing a brand that has since been deleted. The identity is SKIPPED and the
	// decision proceeds: an application-level foreign key has no database
	// constraint behind it, so one deleted row must not take down every decision
	// that traverses the field holding it.
	//
	// It is not recorded by rule evaluation — the reference channel is the engine's
	// enumeration path — but it is the same class of observation and rides the same
	// collector, so it renders through Explain's notes exactly like the others.
	// Path is the DECLARATION ("dataset.current_brands"), Expected the declared
	// target object-type, Actual "absent". As with every note, the dangling
	// identity itself is never carried: it goes to the operator's log, not to a
	// diagnostic that crosses account boundaries.
	NoteDanglingReference NoteKind = "dangling_reference"
)

// Note is one diagnostic observation recorded during rule evaluation.
//
// SHAPE AND PATH ONLY. A Note must never carry a metadata value, an object id, or
// anything else that could leak data across accounts: Explain renders notes to
// operators, and the same trace crosses the Twirp and MCP surfaces.
type Note struct {
	// Kind classifies the observation.
	Kind NoteKind `json:"kind"`
	// Rule is the rule reference the evaluation resolved, stamped by the Engine.
	// Empty when a Compiled program is evaluated directly.
	Rule string `json:"rule,omitempty"`
	// Op is the comparison operator that made the observation (e.g. "hasAll").
	Op string `json:"op,omitempty"`
	// Path is the dotted variable path of the operand (e.g. "object.tags"), or
	// empty when the operand is not a plain variable reference.
	Path string `json:"path,omitempty"`
	// Expected is the shape the operator requires ("collection", "array", ...).
	Expected string `json:"expected,omitempty"`
	// Actual is the shape actually found ("string", "number", "absent", ...).
	Actual string `json:"actual,omitempty"`
}

// anonymousOperand is what a note calls an operand that is not a plain variable
// reference (a function call, say), so a note always names something.
const anonymousOperand = "(expression)"

// String renders the note as the one-line diagnostic Explain prints, e.g.
//
//	object.tags: expected collection, got string
//	object.tags: absent; hasNone matched because the field is missing
//	object.hired_at: not a canonical date; before expects 2006-01-02 or 2006-01-02T15:04:05Z
//	object.hired_at: between bounds are inverted; the lower bound is after the upper bound, so nothing can match
//	dataset.current_brands: references a brand that no longer exists; the identity was skipped
func (n Note) String() string {
	path := n.Path
	if path == "" {
		path = anonymousOperand
	}
	switch n.Kind {
	case NoteAbsentField:
		return fmt.Sprintf("%s: absent; %s matched because the field is missing", path, n.Op)
	case NoteShapeMismatch:
		return fmt.Sprintf("%s: expected %s, got %s", path, n.Expected, n.Actual)
	case NoteDateInvalid:
		return fmt.Sprintf("%s: not a canonical date; %s expects %s", path, n.Op, n.Expected)
	case NoteDateBoundsInverted:
		return fmt.Sprintf("%s: %s bounds are inverted; the lower bound is after the upper "+
			"bound, so nothing can match", path, n.Op)
	case NoteDanglingReference:
		return fmt.Sprintf("%s: references a %s that no longer exists; the identity was skipped",
			path, n.Expected)
	default:
		return fmt.Sprintf("%s: %s (%s)", path, n.Kind, n.Op)
	}
}

// NoteCollector accumulates evaluation notes. It is safe for concurrent use so a
// single collector can span the several evaluations one Explain fans out, and it
// DEDUPLICATES identical notes: a rule re-evaluated per candidate grant would
// otherwise report the same mismatch many times over.
//
// A nil *NoteCollector is a valid, inert collector — every method is a no-op —
// which is what lets the evaluator carry an "unset" sink without branching.
type NoteCollector struct {
	mu    sync.Mutex
	notes []Note
}

// Add records notes, skipping any exactly equal to one already held. Safe on a
// nil receiver.
func (c *NoteCollector) Add(notes ...Note) {
	if c == nil || len(notes) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, n := range notes {
		if !containsNote(c.notes, n) {
			c.notes = append(c.notes, n)
		}
	}
}

// Notes returns a copy of the notes recorded so far, in the order recorded. Safe
// on a nil receiver (returns nil).
func (c *NoteCollector) Notes() []Note {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.notes) == 0 {
		return nil
	}
	out := make([]Note, len(c.notes))
	copy(out, c.notes)
	return out
}

// Len reports how many notes have been recorded. Safe on a nil receiver.
func (c *NoteCollector) Len() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.notes)
}

func containsNote(notes []Note, n Note) bool {
	for _, have := range notes {
		if have == n {
			return true
		}
	}
	return false
}

// noteCollectorKey is the private context key the collector travels under. It is
// a distinct empty struct type, so no other package can collide with it.
type noteCollectorKey struct{}

// WithNoteCollector returns a context carrying a fresh collector, plus the
// collector itself. Every rule evaluation performed under the returned context
// records its notes there.
//
// This is opt-in by design: Check and Enumerate do NOT install a collector, so
// they pay nothing and behave exactly as before. Explain installs one and reads
// the notes back off it.
func WithNoteCollector(ctx context.Context) (context.Context, *NoteCollector) {
	c := &NoteCollector{}
	return context.WithValue(ctx, noteCollectorKey{}, c), c
}

// NoteCollectorFrom returns the collector installed in ctx, or nil when none is —
// the normal case on the decision hot path. The nil result is usable directly:
// every NoteCollector method is nil-safe.
func NoteCollectorFrom(ctx context.Context) *NoteCollector {
	if ctx == nil {
		return nil
	}
	c, _ := ctx.Value(noteCollectorKey{}).(*NoteCollector)
	return c
}
