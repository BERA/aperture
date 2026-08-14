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
	NoteAbsentField NoteKind = "absent_field"
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
