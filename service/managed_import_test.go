package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/frankbardon/aperture/authz"
	"github.com/frankbardon/aperture/engine"
	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/seed"
	"github.com/frankbardon/aperture/storage/memory"
)

// Import is the ONE write path that never passes through the facade's per-entity
// mutators — it hands a whole seed.Document to Apply, which reaches storage
// directly — so the gate it carries is tested here in its own right rather than
// assumed from the mutator tests.

// importDoc is a complete state file carrying ALL THREE gated sections plus a
// spread of ungated ones, so a refusal can be shown to drop the whole file and
// not merely the offending section.
func importDoc() *seed.Document {
	d := portDoc()
	d.Memberships = []seed.Membership{{Principal: "carol", Account: "acme"}}
	return d
}

// importSectionKeys maps a gated kind to the document key a refusal must name,
// so the test asserts the operator is pointed at the section to remove.
var importSectionKeys = map[string]string{
	"account":    "accounts:",
	"principal":  "principals:",
	"membership": "memberships:",
}

// TestImportRefusesADocumentCarryingAnUnmanagedSection is the bypass test: an
// import of a file with an accounts:/principals:/memberships: section must be
// refused when that kind is locked, with the SAME code the mutators use, and
// nothing from the file — not even the ungated entities — may land.
func TestImportRefusesADocumentCarryingAnUnmanagedSection(t *testing.T) {
	ctx := context.Background()
	alice := Actor{Principal: "alice", Account: "acme"}
	for kind, section := range importSectionKeys {
		t.Run(kind, func(t *testing.T) {
			svc, store, rec := newManagedFacade(t, lockedPosture(kind))
			defer rec.Close()

			err := svc.Import(ctx, alice, importDoc())
			if err == nil {
				t.Fatalf("expected the import to be refused with %s unmanaged", kind)
			}
			if got := aerr.CodeOf(err); got != aerr.APERTURE_ENTITY_UNMANAGED {
				t.Fatalf("code = %q, want %q", got, aerr.APERTURE_ENTITY_UNMANAGED)
			}
			if !strings.Contains(err.Error(), section) {
				t.Fatalf("message must name the %q section that triggered it, got %q", section, err.Error())
			}
			// Whole-document refusal: the ungated entities that Apply would have
			// written FIRST must be absent too.
			if _, e := store.GetObjectType(ctx, "document"); aerr.CodeOf(e) != aerr.APERTURE_NOT_FOUND {
				t.Fatalf("refused import applied an ungated object type: %v", e)
			}
			if _, e := store.GetPermission(ctx, "perm-read"); aerr.CodeOf(e) != aerr.APERTURE_NOT_FOUND {
				t.Fatalf("refused import applied an ungated permission: %v", e)
			}
			if _, e := store.GetPrincipal(ctx, "carol"); aerr.CodeOf(e) != aerr.APERTURE_NOT_FOUND {
				t.Fatalf("refused import applied a principal: %v", e)
			}
			if _, e := store.GetGrant(ctx, "g1"); aerr.CodeOf(e) != aerr.APERTURE_NOT_FOUND {
				t.Fatalf("refused import applied a grant: %v", e)
			}
		})
	}
}

// countingStore wraps a Storage and counts the transactions opened through it,
// so "the refusal happens BEFORE the transaction" is asserted rather than
// inferred from the absence of rows.
type countingStore struct {
	model.Storage
	atomics int
}

func (c *countingStore) Atomic(ctx context.Context, fn func(model.Storage) error) error {
	c.atomics++
	return c.Storage.Atomic(ctx, fn)
}

// TestRefusedImportOpensNoTransaction pins the ordering requirement: a refused
// import must do no storage work at all, not roll one back.
func TestRefusedImportOpensNoTransaction(t *testing.T) {
	ctx := context.Background()
	mem := memory.New()
	if err := mem.Setup(ctx); err != nil {
		t.Fatalf("setup: %v", err)
	}
	counting := &countingStore{Storage: mem}
	eng := engine.New(counting)
	svc := New(eng,
		WithStorage(counting),
		WithGate(authz.NewGate(eng)),
		WithManagedEntities(ManagedEntities{Accounts: ManagedNo}))

	err := svc.Import(ctx, Actor{Principal: "alice", Account: "acme"}, importDoc())
	if got := aerr.CodeOf(err); got != aerr.APERTURE_ENTITY_UNMANAGED {
		t.Fatalf("code = %q, want %q", got, aerr.APERTURE_ENTITY_UNMANAGED)
	}
	if counting.atomics != 0 {
		t.Fatalf("a refused import opened %d transaction(s); the gate must run first", counting.atomics)
	}
}

// TestImportOfUngatedEntitiesSurvivesEveryLock — the gate is about three entity
// kinds, not about Import. A file that carries none of them must import cleanly
// with all three switches off, or locking accounts would quietly disable state
// transfer for roles, permissions, and rules as well.
func TestImportOfUngatedEntitiesSurvivesEveryLock(t *testing.T) {
	svc, store, rec := newManagedFacade(t, ManagedEntities{
		Accounts: ManagedNo, Principals: ManagedNo, Memberships: ManagedNo,
	})
	defer rec.Close()
	ctx := context.Background()

	doc := &seed.Document{
		ObjectTypes: []seed.ObjectType{{Name: "document", Actions: []string{"read", "write"}}},
		Permissions: []seed.Permission{{ID: "perm-read", ObjectType: "document", Action: "read"}},
		Roles:       []seed.Role{{ID: "role-reader", Name: "Reader", Permissions: []string{"perm-read"}}},
		Groups:      []seed.Group{{ID: "grp-readers", Name: "Readers", Members: []string{"alice"}}},
		Grants: []seed.Grant{{
			ID: "g-ungated", Account: "acme",
			Subject:    seed.Subject{Kind: "principal", ID: "alice"},
			Permission: "perm-read", Object: "account:acme/**", Effect: "allow",
		}},
		Templates: []seed.Template{{
			Name: "reader", Version: 1, Description: "read one document",
			Params: []seed.TemplateParam{{Name: "who", Type: "string"}},
			Grants: []seed.TemplateGrant{{
				Subject:    seed.Subject{Kind: "principal", ID: "${who}"},
				Permission: "perm-read", Object: "account:acme/**", Effect: "allow",
			}},
		}},
		Rules: []seed.Rule{{Name: "r-ungated", AST: json.RawMessage(`{"type":"var","name":"object.x"}`)}},
	}

	if err := svc.Import(ctx, Actor{Principal: "alice", Account: "acme"}, doc); err != nil {
		t.Fatalf("a document with no gated section must import with every switch off: %v", err)
	}
	if _, err := store.GetRole(ctx, "role-reader"); err != nil {
		t.Fatalf("ungated role did not land: %v", err)
	}
	if _, err := store.GetGrant(ctx, "g-ungated"); err != nil {
		t.Fatalf("ungated grant did not land: %v", err)
	}
}

// TestImportUnderDefaultPostureIsUnchanged — the zero ManagedEntities is what a
// facade built without WithManagedEntities holds, and it must import exactly
// what it always did, gated sections included.
func TestImportUnderDefaultPostureIsUnchanged(t *testing.T) {
	svc, store, rec := newManagedFacade(t, ManagedEntities{})
	defer rec.Close()
	ctx := context.Background()

	if err := svc.Import(ctx, Actor{Principal: "alice", Account: "acme"}, importDoc()); err != nil {
		t.Fatalf("default posture must import a full state file: %v", err)
	}
	if _, err := store.GetPrincipal(ctx, "carol"); err != nil {
		t.Fatalf("principal did not land: %v", err)
	}
	ms, err := store.MembershipsForAccount(ctx, "acme")
	if err != nil {
		t.Fatalf("memberships: %v", err)
	}
	var found bool
	for _, m := range ms {
		if m.PrincipalID == "carol" {
			found = true
		}
	}
	if !found {
		t.Fatal("membership did not land under the default posture")
	}
}

// TestImportRefusalLeaksNothingFromTheDocument — the refusal is deployment
// posture, identical for every caller, so it may name the SECTION and the switch
// and nothing else. An id from the file in the message would turn a config
// switch into a disclosure channel.
func TestImportRefusalLeaksNothingFromTheDocument(t *testing.T) {
	svc, _, rec := newManagedFacade(t, ManagedEntities{Principals: ManagedNo})
	defer rec.Close()
	ctx := context.Background()

	doc := &seed.Document{
		Principals: []seed.Principal{{ID: "secret-principal", Kind: "user", Identity: "user:secret"}},
		Grants: []seed.Grant{{
			ID: "secret-grant", Account: "secret-account",
			Subject:    seed.Subject{Kind: "principal", ID: "secret-principal"},
			Permission: "perm-read", Object: "account:secret-account/**", Effect: "allow",
		}},
	}
	err := svc.Import(ctx, Actor{Principal: "alice", Account: "acme"}, doc)
	if err == nil {
		t.Fatal("expected a refusal")
	}
	for _, leak := range []string{"secret-principal", "secret-account", "secret-grant"} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("refusal leaked %q: %q", leak, err.Error())
		}
	}
}
