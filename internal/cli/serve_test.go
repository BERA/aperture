package cli

import (
	"context"
	"strings"
	"testing"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/service"

	ucli "github.com/urfave/cli/v3"
)

// resolveManaged runs the real `serve` flag set over args and returns what
// managedEntities makes of it, without booting a server.
func resolveManaged(t *testing.T, args ...string) (service.ManagedEntities, error) {
	t.Helper()
	var (
		got service.ManagedEntities
		err error
	)
	cmd := &ucli.Command{
		Name:  "serve",
		Flags: serveCommand().Flags,
		Action: func(_ context.Context, cmd *ucli.Command) error {
			got, err = managedEntities(cmd)
			return nil
		},
	}
	if runErr := cmd.Run(context.Background(), append([]string{"serve"}, args...)); runErr != nil {
		t.Fatalf("parsing %v: %v", args, runErr)
	}
	return got, err
}

// TestManagedEntities_DefaultsToManaged asserts an operator who passes nothing
// and sets nothing gets Aperture's historical behaviour.
func TestManagedEntities_DefaultsToManaged(t *testing.T) {
	t.Setenv(service.EnvManageAccounts, "")
	t.Setenv(service.EnvManagePrincipals, "")
	t.Setenv(service.EnvManageMemberships, "")

	got, err := resolveManaged(t)
	if err != nil {
		t.Fatalf("managedEntities: %v", err)
	}
	if !got.Accounts.Enabled() || !got.Principals.Enabled() || !got.Memberships.Enabled() {
		t.Fatalf("unconfigured serve is not fully managed: %+v", got)
	}
}

// TestManagedEntities_FlagsAreBooleansOfPositivePolarity asserts --manage-X=false
// disables exactly X. Positive polarity: the flag names the ENABLED state.
func TestManagedEntities_FlagsAreBooleansOfPositivePolarity(t *testing.T) {
	t.Setenv(service.EnvManageAccounts, "")
	t.Setenv(service.EnvManagePrincipals, "")
	t.Setenv(service.EnvManageMemberships, "")

	for _, tc := range []struct {
		flag string
		read func(service.ManagedEntities) service.Managed
	}{
		{"--manage-accounts=false", func(m service.ManagedEntities) service.Managed { return m.Accounts }},
		{"--manage-principals=false", func(m service.ManagedEntities) service.Managed { return m.Principals }},
		{"--manage-memberships=false", func(m service.ManagedEntities) service.Managed { return m.Memberships }},
	} {
		t.Run(tc.flag, func(t *testing.T) {
			got, err := resolveManaged(t, tc.flag)
			if err != nil {
				t.Fatalf("managedEntities: %v", err)
			}
			if tc.read(got).Enabled() {
				t.Fatalf("%s left the kind managed: %+v", tc.flag, got)
			}
		})
	}
}

// TestManagedEntities_EnvIsRead asserts the env var alone (no flag) is enough.
func TestManagedEntities_EnvIsRead(t *testing.T) {
	t.Setenv(service.EnvManageAccounts, "false")
	t.Setenv(service.EnvManagePrincipals, "")
	t.Setenv(service.EnvManageMemberships, "")

	got, err := resolveManaged(t)
	if err != nil {
		t.Fatalf("managedEntities: %v", err)
	}
	if got.Accounts.Enabled() {
		t.Fatalf("%s=false was not read: %+v", service.EnvManageAccounts, got)
	}
	if !got.Principals.Enabled() || !got.Memberships.Enabled() {
		t.Fatalf("one env var disabled more than its own kind: %+v", got)
	}
}

// TestManagedEntities_FlagBeatsEnv pins the precedence in both directions, so
// the flag is a real override and not merely a second way to say the same thing.
func TestManagedEntities_FlagBeatsEnv(t *testing.T) {
	t.Setenv(service.EnvManagePrincipals, "")
	t.Setenv(service.EnvManageMemberships, "")

	t.Run("flag disables what env enabled", func(t *testing.T) {
		t.Setenv(service.EnvManageAccounts, "true")
		got, err := resolveManaged(t, "--manage-accounts=false")
		if err != nil {
			t.Fatalf("managedEntities: %v", err)
		}
		if got.Accounts.Enabled() {
			t.Fatalf("the flag did not override the env var: %+v", got)
		}
	})

	t.Run("flag enables what env disabled", func(t *testing.T) {
		t.Setenv(service.EnvManageAccounts, "false")
		got, err := resolveManaged(t, "--manage-accounts=true")
		if err != nil {
			t.Fatalf("managedEntities: %v", err)
		}
		if !got.Accounts.Enabled() {
			t.Fatalf("the flag did not override the env var: %+v", got)
		}
	})
}

// TestManagedEntities_MalformedEnvIsConfigError asserts the coded error survives
// the CLI layer. This is the reason the flags carry no ucli.EnvVars source:
// urfave would fail the command with its own uncoded parse error first.
func TestManagedEntities_MalformedEnvIsConfigError(t *testing.T) {
	t.Setenv(service.EnvManageAccounts, "yes-please")
	t.Setenv(service.EnvManagePrincipals, "")
	t.Setenv(service.EnvManageMemberships, "")

	_, err := resolveManaged(t)
	if got := aerr.CodeOf(err); got != aerr.APERTURE_CONFIG_INVALID {
		t.Fatalf("code = %s, want %s (err=%v)", got, aerr.APERTURE_CONFIG_INVALID, err)
	}
}

// TestServeFlagsNameTheirEnvVars asserts each --manage-* flag's usage text names
// the environment variable it pairs with. The usage string is the only shipped
// explanation of the flag, so an operator reading `aperture serve --help` must
// be able to find the env var from it.
func TestServeFlagsNameTheirEnvVars(t *testing.T) {
	usage := map[string]string{}
	for _, f := range serveCommand().Flags {
		bf, ok := f.(*ucli.BoolFlag)
		if !ok {
			continue
		}
		usage[bf.Name] = bf.Usage
		if bf.Name == "manage-accounts" || bf.Name == "manage-principals" || bf.Name == "manage-memberships" {
			if !bf.Value {
				t.Errorf("--%s defaults to false; all three manage flags default to true", bf.Name)
			}
		}
	}
	for flag, env := range map[string]string{
		"manage-accounts":    service.EnvManageAccounts,
		"manage-principals":  service.EnvManagePrincipals,
		"manage-memberships": service.EnvManageMemberships,
	} {
		u, ok := usage[flag]
		if !ok {
			t.Fatalf("serve has no --%s bool flag", flag)
		}
		if !strings.Contains(u, env) {
			t.Errorf("--%s usage does not name %s: %q", flag, env, u)
		}
	}
}
