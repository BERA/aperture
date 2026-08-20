package service

import (
	"testing"

	aerr "github.com/frankbardon/aperture/errors"
)

// TestManagedEntities_ZeroValueIsManaged pins the whole point of the tri-state:
// the value a caller gets from writing ManagedEntities{} — or from a Service
// built without WithManagedEntities — must mean "everything is managed", i.e.
// Aperture's behaviour before the option existed. A plain-bool struct would mean
// the exact opposite here.
func TestManagedEntities_ZeroValueIsManaged(t *testing.T) {
	var cfg ManagedEntities
	if !cfg.Accounts.Enabled() || !cfg.Principals.Enabled() || !cfg.Memberships.Enabled() {
		t.Fatalf("zero ManagedEntities is not fully managed: %+v", cfg)
	}
}

// TestManagedEntitiesFromEnv_UnsetIsManaged asserts a process with no
// APERTURE_MANAGE_* configuration at all keeps today's behaviour.
func TestManagedEntitiesFromEnv_UnsetIsManaged(t *testing.T) {
	t.Setenv(EnvManageAccounts, "")
	t.Setenv(EnvManagePrincipals, "")
	t.Setenv(EnvManageMemberships, "")

	cfg, err := ManagedEntitiesFromEnv()
	if err != nil {
		t.Fatalf("ManagedEntitiesFromEnv: %v", err)
	}
	if cfg != (ManagedEntities{}) {
		t.Fatalf("unset env produced %+v, want the zero (all-default) value", cfg)
	}
	if !cfg.Accounts.Enabled() || !cfg.Principals.Enabled() || !cfg.Memberships.Enabled() {
		t.Fatalf("unset env is not fully managed: %+v", cfg)
	}
}

// TestManagedEntitiesFromEnv_True asserts an explicit opt-in is read as an
// explicit opt-in (not merely as the default) and is enabled.
func TestManagedEntitiesFromEnv_True(t *testing.T) {
	t.Setenv(EnvManageAccounts, "true")
	t.Setenv(EnvManagePrincipals, "TRUE")
	t.Setenv(EnvManageMemberships, "1")

	cfg, err := ManagedEntitiesFromEnv()
	if err != nil {
		t.Fatalf("ManagedEntitiesFromEnv: %v", err)
	}
	want := ManagedEntities{Accounts: ManagedYes, Principals: ManagedYes, Memberships: ManagedYes}
	if cfg != want {
		t.Fatalf("cfg = %+v, want %+v", cfg, want)
	}
	if !cfg.Accounts.Enabled() {
		t.Errorf("an explicit true is not enabled")
	}
}

// TestManagedEntitiesFromEnv_False asserts an explicit opt-out disables exactly
// its own kind and leaves the others alone — the three switches are independent.
func TestManagedEntitiesFromEnv_False(t *testing.T) {
	t.Setenv(EnvManageAccounts, "false")
	t.Setenv(EnvManagePrincipals, "0")
	t.Setenv(EnvManageMemberships, "")

	cfg, err := ManagedEntitiesFromEnv()
	if err != nil {
		t.Fatalf("ManagedEntitiesFromEnv: %v", err)
	}
	if cfg.Accounts.Enabled() {
		t.Errorf("%s=false left accounts managed", EnvManageAccounts)
	}
	if cfg.Principals.Enabled() {
		t.Errorf("%s=0 left principals managed", EnvManagePrincipals)
	}
	if !cfg.Memberships.Enabled() {
		t.Errorf("an unset %s must stay managed; the switches are independent", EnvManageMemberships)
	}
}

// TestManagedEntitiesFromEnv_MalformedIsConfigError asserts a value that is set
// but unparseable fails loudly with APERTURE_CONFIG_INVALID. A silent fallback
// to the default would hand an operator who typed "flase" the exact posture they
// were trying to turn off.
func TestManagedEntitiesFromEnv_MalformedIsConfigError(t *testing.T) {
	for _, env := range []string{EnvManageAccounts, EnvManagePrincipals, EnvManageMemberships} {
		t.Run(env, func(t *testing.T) {
			t.Setenv(EnvManageAccounts, "")
			t.Setenv(EnvManagePrincipals, "")
			t.Setenv(EnvManageMemberships, "")
			t.Setenv(env, "flase")

			cfg, err := ManagedEntitiesFromEnv()
			if got := aerr.CodeOf(err); got != aerr.APERTURE_CONFIG_INVALID {
				t.Fatalf("%s=flase: code = %s, want %s (err=%v)", env, got, aerr.APERTURE_CONFIG_INVALID, err)
			}
			if cfg != (ManagedEntities{}) {
				t.Errorf("a failed read returned a partially-populated config %+v", cfg)
			}
		})
	}
}

// TestManagedEntitiesFromEnv_WhitespaceIsTrimmed covers the shell-quoting
// accident (APERTURE_MANAGE_ACCOUNTS=" false") that would otherwise be reported
// as malformed.
func TestManagedEntitiesFromEnv_WhitespaceIsTrimmed(t *testing.T) {
	t.Setenv(EnvManageAccounts, "  false  ")
	t.Setenv(EnvManagePrincipals, "   ")
	t.Setenv(EnvManageMemberships, "")

	cfg, err := ManagedEntitiesFromEnv()
	if err != nil {
		t.Fatalf("ManagedEntitiesFromEnv: %v", err)
	}
	if cfg.Accounts.Enabled() {
		t.Errorf("a padded false left accounts managed")
	}
	if cfg.Principals != ManagedDefault {
		t.Errorf("an all-whitespace value is not the same as unset: %v", cfg.Principals)
	}
}

// TestManagedFrom asserts the bool bridge never yields the default state — a
// caller holding a bool (a CLI flag's value) has already decided.
func TestManagedFrom(t *testing.T) {
	if got := ManagedFrom(true); got != ManagedYes {
		t.Errorf("ManagedFrom(true) = %v, want ManagedYes", got)
	}
	if got := ManagedFrom(false); got != ManagedNo {
		t.Errorf("ManagedFrom(false) = %v, want ManagedNo", got)
	}
}

// TestWithManagedEntities asserts the option reaches the facade and that a
// facade built WITHOUT it is fully managed, which is what keeps every existing
// construction site behaving as before.
func TestWithManagedEntities(t *testing.T) {
	svc := New(nil)
	if got := svc.ManagedEntities(); got != (ManagedEntities{}) {
		t.Fatalf("service.New with no option = %+v, want the zero (all-managed) value", got)
	}
	if !svc.ManagedEntities().Accounts.Enabled() {
		t.Fatalf("service.New with no option does not manage accounts")
	}

	want := ManagedEntities{Accounts: ManagedNo, Principals: ManagedYes}
	svc = New(nil, WithManagedEntities(want))
	if got := svc.ManagedEntities(); got != want {
		t.Fatalf("ManagedEntities() = %+v, want %+v", got, want)
	}
}
