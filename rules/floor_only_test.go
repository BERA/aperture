package rules

import (
	"context"
	"testing"

	"github.com/frankbardon/aperture/identity"
	"github.com/frankbardon/aperture/provider"
)

// E5-S1: the floor-only note — the promised mitigation for the leniency hazard
// TestAMissingBagWidensAnExclusiveGrant (attribute_leniency_test.go) encodes.
//
// The hazard is that an absent attribute bag is SILENT: every comparison against
// it is false, a rule stops selecting, and in an exclusive grant that widens
// access with nothing in the verdict to say so. These tests pin the three
// properties that turn it from invisible into visible without making the note
// itself a nuisance: it fires when a rule reads a host field off a floor-only
// root, it stays quiet otherwise, and it costs the decision path nothing.

// floorOnlyEngine builds an engine over the two rules a floor-only note can be
// about — one reading a host field off `principal`, one off `account` — plus the
// two that read only the floor. users/accounts nil means the slot is unwired.
func floorOnlyEngine(t *testing.T, users, accounts map[string]provider.Metadata) *Engine {
	t.Helper()
	reg := provider.NewAttributeRegistry()
	register := func(slot provider.AttributeSlot, bags map[string]provider.Metadata) {
		t.Helper()
		if bags == nil {
			return
		}
		records := make([]provider.AttributeRecord, 0, len(bags))
		for id, md := range bags {
			records = append(records, provider.AttributeRecord{ID: id, Attributes: md})
		}
		p, err := provider.NewStaticAttributes(records)
		if err != nil {
			t.Fatalf("static attributes for %s: %v", slot, err)
		}
		reg.MustRegister(slot, p)
	}
	register(provider.AttributeSlotUser, users)
	register(provider.AttributeSlotAccount, accounts)
	return NewEngine(MapSource{
		"tier":       {Name: "tier", AST: Compare(OpEq, Var("principal.tier"), Lit("gold"))},
		"plan":       {Name: "plan", AST: Compare(OpEq, Var("account.plan"), Lit("enterprise"))},
		"self":       {Name: "self", AST: Compare(OpEq, Var("principal.id"), Lit("alice"))},
		"objectOnly": {Name: "objectOnly", AST: Compare(OpEq, Var("object.state"), Lit("open"))},
	}, nil, WithPrincipalResolver(reg), WithAccountResolver(reg))
}

// notesFrom evaluates rule under a collector and returns what it recorded.
func notesFrom(t *testing.T, eng *Engine, rule, account, principal string) []Note {
	t.Helper()
	ctx, collector := WithNoteCollector(context.Background())
	if _, err := eng.Selected(ctx, rule, identity.MustParse("document:1"), account, "user", principal, "read"); err != nil {
		t.Fatalf("Selected(%s): %v", rule, err)
	}
	return collector.Notes()
}

// floorOnlyPaths returns the roots the notes report as floor-only.
func floorOnlyPaths(notes []Note) []string {
	var out []string
	for _, n := range notes {
		if n.Kind == NoteAttributesFloorOnly {
			out = append(out, n.Path)
		}
	}
	return out
}

func onlyPath(t *testing.T, notes []Note, want string) {
	t.Helper()
	got := floorOnlyPaths(notes)
	if len(got) != 1 || got[0] != want {
		t.Fatalf("floor-only notes named %v, want exactly [%s] (all notes: %+v)", got, want, notes)
	}
}

// TestAFloorOnlyBagIsNamedInTheNotes is the acceptance criterion: a rule that
// reads a host field off a root nothing answered for says WHICH root, and it
// says it for each root independently — a wired principal directory does not
// excuse an unwired account slot or the other way round.
func TestAFloorOnlyBagIsNamedInTheNotes(t *testing.T) {
	t.Run("no provider is wired for either slot", func(t *testing.T) {
		eng := floorOnlyEngine(t, nil, nil)
		onlyPath(t, notesFrom(t, eng, "tier", "acme", "alice"), "principal")
		onlyPath(t, notesFrom(t, eng, "plan", "acme", "alice"), "account")
	})

	t.Run("a provider is wired and has no record for this id", func(t *testing.T) {
		// The other operator situation, and it is deliberately the SAME note. The
		// distinction this layer could actually draw is "is a resolver installed",
		// which is not the distinction an operator cares about: a registry with no
		// user slot is installed and answers exactly like this. See
		// NoteAttributesFloorOnly.
		eng := floorOnlyEngine(t,
			map[string]provider.Metadata{"someone-else": {"tier": "gold"}},
			map[string]provider.Metadata{"other-corp": {"plan": "enterprise"}})
		onlyPath(t, notesFrom(t, eng, "tier", "acme", "alice"), "principal")
		onlyPath(t, notesFrom(t, eng, "plan", "acme", "alice"), "account")
	})

	t.Run("the account wildcard resolves no bag at all", func(t *testing.T) {
		// "*" never reaches a resolver (Engine.accountAttributes), so a rule reading
		// account.plan at platform scope really is comparing against nothing.
		eng := floorOnlyEngine(t, nil, map[string]provider.Metadata{"acme": {"plan": "enterprise"}})
		onlyPath(t, notesFrom(t, eng, "plan", accountWildcard, "alice"), "account")
	})
}

// TestARealValueRecordsNoFloorOnlyNote is the distinguishing half of the story:
// a rule that matched nothing on REAL values must not look like one that matched
// nothing because there was nothing to match. Same rule, same principal, same
// object — the only variable is whether the directory has the record.
func TestARealValueRecordsNoFloorOnlyNote(t *testing.T) {
	eng := floorOnlyEngine(t,
		map[string]provider.Metadata{"alice": {"tier": "silver"}},
		map[string]provider.Metadata{"acme": {"plan": "starter"}})

	for _, rule := range []string{"tier", "plan"} {
		notes := notesFrom(t, eng, rule, "acme", "alice")
		if paths := floorOnlyPaths(notes); len(paths) != 0 {
			t.Fatalf("rule %q compared against real values and did not match; it must not be "+
				"reported as floor-only, got %v", rule, paths)
		}
	}
}

// TestAFloorOnlyNoteNeedsTheRuleToReadTheRoot keeps the note from becoming the
// thing operators learn to ignore. Every deployment that wires no attribute
// provider has floor-only bags on every decision; only the rules that actually
// read a host field off one are exposed to the hazard.
func TestAFloorOnlyNoteNeedsTheRuleToReadTheRoot(t *testing.T) {
	eng := floorOnlyEngine(t, nil, nil)
	for _, rule := range []string{"self", "objectOnly"} {
		notes := notesFrom(t, eng, rule, "acme", "alice")
		if paths := floorOnlyPaths(notes); len(paths) != 0 {
			t.Fatalf("rule %q reads no host attribute and cannot be surprised by an unwired "+
				"slot; it must record no floor-only note, got %v", rule, paths)
		}
	}
}

// TestReadsBeyondFloorNamesOnlyHostFields pins the predicate the note is gated
// on, path by path, because it is the difference between a useful note and noise
// on every trace.
func TestReadsBeyondFloorNamesOnlyHostFields(t *testing.T) {
	cases := []struct {
		name string
		ast  *Node
		want bool
	}{
		{"a floor key", Compare(OpEq, Var("principal.id"), Lit("alice")), false},
		{"the other floor key", Compare(OpEq, Var("principal.kind"), Lit("user")), false},
		{"a host field", Compare(OpEq, Var("principal.tier"), Lit("gold")), true},
		{"a nested host field", Compare(OpEq, Var("principal.org.region"), Lit("eu")), true},
		{"a path under a floor key", Compare(OpEq, Var("principal.id.sub"), Lit("x")), false},
		{"another root entirely", Compare(OpEq, Var("account.plan"), Lit("free")), false},
		{"a root that merely shares a prefix", Compare(OpEq, Var("principality"), Lit("x")), false},
		{"the whole bag", Unary(OpIsEmpty, Var("principal")), true},
		{"buried on the right", Compare(OpIn, Lit("gold"), List(Var("principal.tier"))), true},
		{"buried under a boolean", And(
			Compare(OpEq, Var("object.state"), Lit("open")),
			Not(Compare(OpEq, Var("principal.tier"), Lit("gold"))),
		), true},
		{"buried in a call argument", Unary(OpExists, Call("lower", Var("principal.tier"))), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := readsBeyondFloor(tc.ast, principalRoot, principalKeyID, principalKeyKind); got != tc.want {
				t.Fatalf("readsBeyondFloor = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestTheFloorOnlyNoteCostsTheFastPathNothing is the hot-path constraint, and it
// is the one that must not be relaxed. Check and Enumerate install no collector,
// so neither the AST walk nor the note may happen for them.
//
// Both measurements run against the SAME engine, with both bags floor-only and
// no collector installed. The only difference is the rule: one reads an absent
// field off `principal` (so a floor-only note WOULD be recorded if anything
// recorded one here) and the other reads an absent field off `object` (so none
// ever would). Two structurally identical comparisons over two absent fields
// allocate identically — unless the emission moved above Selected's fast-path
// return, in which case the principal-reading rule pays for the AST walk and the
// note and the two diverge.
//
// Holding the ENGINE and the bags fixed is what makes that the only variable.
// Comparing a floor-only engine against an answered one instead would compare
// two different fetch paths and two differently-sized bag copies, and the note's
// couple of allocations would vanish into the difference.
func TestTheFloorOnlyNoteCostsTheFastPathNothing(t *testing.T) {
	obj := identity.MustParse("document:1")
	ctx := context.Background()
	// Floor-only on both roots: a resolver that answers nothing, for every
	// subject. A hand-written one rather than a real *provider.AttributeRegistry,
	// whose unregistered slot builds (and the leniency contract then discards) a
	// coded error — a genuine allocation with nothing to do with this note.
	eng := NewEngine(MapSource{
		"principalField": {Name: "principalField", AST: Compare(OpEq, Var("principal.tier"), Lit("gold"))},
		"objectField":    {Name: "objectField", AST: Compare(OpEq, Var("object.tier"), Lit("gold"))},
	}, nil, WithPrincipalResolver(fixedAttributes{}), WithAccountResolver(fixedAttributes{}))

	measure := func(rule string) float64 {
		// Warm the compiled-rule cache: the first evaluation compiles, and the
		// property under test is the cached path the Check NFR measures.
		if _, err := eng.Selected(ctx, rule, obj, "acme", "user", "alice", "read"); err != nil {
			t.Fatalf("warm-up: %v", err)
		}
		return testing.AllocsPerRun(200, func() {
			if _, err := eng.Selected(ctx, rule, obj, "acme", "user", "alice", "read"); err != nil {
				t.Fatalf("Selected: %v", err)
			}
		})
	}

	object := measure("objectField")
	if principal := measure("principalField"); principal != object {
		t.Fatalf("a rule reading a floor-only principal allocated %v times against the %v of an "+
			"otherwise identical rule reading the object — the floor-only note and the AST "+
			"walk behind it must happen only when a collector is installed, which Check and "+
			"Enumerate never do", principal, object)
	}
}

// fixedAttributes answers both resolver seams with one prebuilt bag each,
// allocating nothing itself. A nil bag is the lenient absence.
type fixedAttributes struct{ principal, account map[string]any }

func (r fixedAttributes) Attributes(context.Context, string, string) (map[string]any, error) {
	return r.principal, nil
}

func (r fixedAttributes) AccountAttributes(context.Context, string) (map[string]any, error) {
	return r.account, nil
}

// TestTheFloorOnlyNoteCarriesNoValue holds the note channel's standing rule. The
// TRACE discloses attribute values on purpose (see engine.TraceAttributes); a
// Note still must not, because it is the record that travels next to every
// object-metadata diagnostic and is held to shape-and-path only.
func TestTheFloorOnlyNoteCarriesNoValue(t *testing.T) {
	eng := floorOnlyEngine(t, nil, nil)
	for _, n := range notesFrom(t, eng, "tier", "acme", "alice") {
		if n.Kind != NoteAttributesFloorOnly {
			continue
		}
		want := Note{Kind: NoteAttributesFloorOnly, Rule: "tier", Path: principalRoot}
		if n != want {
			t.Fatalf("note = %+v, want %+v — a floor-only note names the ROOT and nothing else", n, want)
		}
		if n.String() == "" {
			t.Fatal("the note must render a one-line diagnostic")
		}
	}
}
