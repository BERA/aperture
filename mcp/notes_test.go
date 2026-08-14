package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/frankbardon/aperture/engine"
	"github.com/frankbardon/aperture/mcp/toolmeta"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/rules"
	"github.com/frankbardon/aperture/service"
	"github.com/frankbardon/aperture/storage/memory"
)

// E5-S1: the MCP surface must carry the evaluation notes Explain records. It
// serializes engine.Trace verbatim (ExplainOut = engine.Trace), so the assertions
// here are that the reflected OUTPUT SCHEMA advertises the notes and that an
// actual tool invocation returns them.

// notesService wires a rule-backed grant whose rule reads principal.id — a
// STRING — with a collection operator. That is the mismatch the value model can
// never prevent: the principal attribute bag bypasses every loader, exactly the
// case this policy exists for.
func notesService(t *testing.T) *service.Service {
	t.Helper()
	ctx := context.Background()
	store := memory.New()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	must(store.Setup(ctx))
	must(store.PutAccount(ctx, model.Account{ID: "acme", Name: "Acme"}))
	must(store.PutPrincipal(ctx, model.Principal{ID: "alice", Kind: model.PrincipalUser, Identity: "user:alice"}))
	must(store.PutMembership(ctx, model.Membership{PrincipalID: "alice", AccountID: "acme"}))
	must(store.PutObjectType(ctx, model.ObjectType{Name: "document", Actions: []string{"read"}}))
	must(store.PutPermission(ctx, model.Permission{
		ID: "p-doc", ObjectType: "document", Action: "read", ScopeStrategy: "inclusive;rule=tagged",
	}))
	must(store.PutGrant(ctx, model.Grant{
		ID: "g-doc", AccountID: "acme", Subject: model.Subject{Kind: model.SubjectPrincipal, ID: "alice"},
		PermissionID: "p-doc", Object: "account:acme/**", Effect: model.EffectAllow,
	}))

	ruleEngine := rules.NewEngine(rules.MapSource{"tagged": {
		Name: "tagged", AST: rules.Compare(rules.OpHas, rules.Var("principal.id"), rules.Lit("alice")),
	}}, nil)
	eng := engine.New(store, engine.WithScopeResolution(nil, engine.ScopeDeps{Rules: ruleEngine}))
	return service.New(eng)
}

// TestExplainToolCarriesEvaluationNotes drives the real tool invocation the MCP
// adapter mounts and asserts the trace it returns carries the note.
func TestExplainToolCarriesEvaluationNotes(t *testing.T) {
	var invoke InvokeFunc
	for _, d := range Tools(Config{}) {
		if d.Name == toolmeta.ToolExplain {
			invoke = d.Invoke
		}
	}
	if invoke == nil {
		t.Fatalf("no %s tool in the catalog", toolmeta.ToolExplain)
	}

	out, err := invoke(context.Background(), notesService(t), json.RawMessage(
		`{"Account":"acme","Principal":"alice","Action":"read","Object":"account:acme/document:1"}`))
	if err != nil {
		t.Fatalf("invoke %s: %v", toolmeta.ToolExplain, err)
	}
	tr, ok := out.(engine.Trace)
	if !ok {
		t.Fatalf("explain returned %T, want engine.Trace", out)
	}
	if tr.Decision.Allow {
		t.Fatalf("a shape mismatch must deny\n%s", tr.String())
	}
	if len(tr.Notes) != 1 {
		t.Fatalf("trace notes = %+v, want exactly one", tr.Notes)
	}
	if got, want := tr.Notes[0].Message, "principal.id: expected collection, got string"; got != want {
		t.Fatalf("note message = %q, want %q", got, want)
	}

	// The tool's JSON payload — what an MCP client actually reads — carries it.
	body, err := json.Marshal(tr)
	if err != nil {
		t.Fatalf("marshal trace: %v", err)
	}
	if !strings.Contains(string(body), "expected collection, got string") {
		t.Fatalf("serialized trace omits the note:\n%s", body)
	}
}

// TestExplainToolSchemaAdvertisesNotes pins the contract half: the reflected
// output schema an MCP client reads describes the notes field, so a client can
// surface it without out-of-band knowledge.
func TestExplainToolSchemaAdvertisesNotes(t *testing.T) {
	for _, name := range []string{toolmeta.ToolExplain, toolmeta.ToolExplainBatch, toolmeta.ToolSimulate} {
		ts, ok := SchemaFor(name)
		if !ok {
			t.Fatalf("no schema registered for %s", name)
		}
		if !strings.Contains(string(ts.OutputSchema), "Notes") {
			t.Errorf("%s output schema does not advertise Notes:\n%s", name, ts.OutputSchema)
		}
	}
}
