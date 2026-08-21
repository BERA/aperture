package service

import (
	"reflect"
	"testing"
)

// E2-S1, the facade half. Capabilities is the only read of the entity-management
// posture a client ever gets, so the mapping from the tri-state Managed to the
// plain booleans on the wire has to be exhaustively pinned: a single flipped or
// crossed field would hand the admin UI a control it must not offer, or hide one
// it must.

// TestCapabilities_ZeroValueIsFullyCapable pins the historical posture. A
// Service built WITHOUT WithManagedEntities — every construction site that
// predates the option — must report all three true. This is the one case where
// the tri-state earns its keep: a plain-bool ManagedEntities would report all
// three FALSE here and lock a deployment out of its own entity CRUD.
func TestCapabilities_ZeroValueIsFullyCapable(t *testing.T) {
	svc := New(nil)
	got := svc.Capabilities()
	want := Capabilities{ManageAccounts: true, ManagePrincipals: true, ManageMemberships: true}
	if got != want {
		t.Fatalf("Capabilities() on an unconfigured Service = %+v, want %+v", got, want)
	}
}

// TestCapabilities_EveryCombination walks all eight postures of the three
// switches. A table of "one flag off" cases would pass against a facade that
// crossed two fields, so every combination is enumerated rather than sampled,
// and each expectation is derived from the posture independently of the code
// under test.
func TestCapabilities_EveryCombination(t *testing.T) {
	// The explicit spellings of each state. ManagedDefault and ManagedYes are
	// behaviourally identical and both must report true, so each combination is
	// run once per spelling of "on" — that is what stops Capabilities from
	// reading == ManagedYes and silently disabling every unconfigured switch.
	for _, on := range []struct {
		name string
		val  Managed
	}{
		{"default", ManagedDefault},
		{"explicit", ManagedYes},
	} {
		t.Run("on="+on.name, func(t *testing.T) {
			for mask := 0; mask < 8; mask++ {
				accounts := mask&1 != 0
				principals := mask&2 != 0
				memberships := mask&4 != 0

				state := func(enabled bool) Managed {
					if enabled {
						return on.val
					}
					return ManagedNo
				}
				posture := ManagedEntities{
					Accounts:    state(accounts),
					Principals:  state(principals),
					Memberships: state(memberships),
				}
				want := Capabilities{
					ManageAccounts:    accounts,
					ManagePrincipals:  principals,
					ManageMemberships: memberships,
				}

				t.Run(postureName(posture), func(t *testing.T) {
					svc := New(nil, WithManagedEntities(posture))
					if got := svc.Capabilities(); got != want {
						t.Fatalf("Capabilities() for %+v = %+v, want %+v", posture, got, want)
					}
				})
			}
		})
	}
}

// TestCapabilities_IsBooleansOnly guards the acceptance criterion that the
// facade's answer carries no model data. Capabilities is the shape every surface
// translates verbatim onto an OPEN, unauthenticated endpoint, so a future field
// carrying an id, a name, or a count would become an anonymous disclosure the
// moment it was added. Three booleans, no more.
func TestCapabilities_IsBooleansOnly(t *testing.T) {
	rt := reflect.TypeOf(Capabilities{})
	want := []string{"ManageAccounts", "ManagePrincipals", "ManageMemberships"}
	if rt.NumField() != len(want) {
		t.Fatalf("Capabilities has %d fields, want exactly %d (%v) — a new field on this struct reaches an unauthenticated endpoint", rt.NumField(), len(want), want)
	}
	for i, name := range want {
		f := rt.Field(i)
		if f.Name != name {
			t.Errorf("field %d is %q, want %q", i, f.Name, name)
		}
		if f.Type.Kind() != reflect.Bool {
			t.Errorf("field %s is %s, want bool", f.Name, f.Type)
		}
	}
}

// postureName names a posture for a subtest, through Managed.String() so the
// tri-state distinction shows up in test output. It is a plain function rather
// than a method on ManagedEntities: adding a Stringer to a production type from
// a test file would change how every other test in the package formats it.
func postureName(m ManagedEntities) string {
	return "accounts=" + m.Accounts.String() +
		",principals=" + m.Principals.String() +
		",memberships=" + m.Memberships.String()
}
