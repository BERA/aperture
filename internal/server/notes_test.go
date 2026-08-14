package server_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/frankbardon/aperture/engine"
	"github.com/frankbardon/aperture/internal/wire/rpc"
	"github.com/frankbardon/aperture/rules"
)

// E5-S1: the Twirp surface must carry the evaluation notes Explain records.
//
// No proto change is needed — Explain returns the engine Trace as canonical JSON
// (trace_json), so the notes ride along with the rest of the trace. This test is
// what makes that load-bearing rather than incidental: it drives the real RPC and
// decodes the payload a client would.
func TestTwirpExplainCarriesEvaluationNotes(t *testing.T) {
	srv, _ := newRulesTestServer(t)
	c := client(srv)
	rootCtx := asPrincipal(context.Background(), t, "root")

	// A rule that applies a COLLECTION operator to principal.id, which is a
	// STRING. The principal attribute bag bypasses every loader, so this is the
	// mismatch load-time validation can never prevent — deny-safe, and noted.
	mistyped := rules.Compare(rules.OpHas, rules.Var("principal.id"), rules.Lit("alice"))
	if _, err := c.PutRule(rootCtx, &rpc.RuleRequest{
		Actor:    &rpc.Actor{Account: acct},
		RuleJson: ruleJSON(t, "vip", mistyped),
	}); err != nil {
		t.Fatalf("PutRule: %v", err)
	}

	query := &rpc.CheckRequest{
		Account: acct, Principal: "alice", Action: "read", Object: "account:acme/document:1",
	}

	// Check is deny-safe: the mismatch denies rather than failing the RPC.
	dec, err := c.Check(rootCtx, query)
	if err != nil {
		t.Fatalf("a shape mismatch must not fail Check over Twirp: %v", err)
	}
	if dec.Allow {
		t.Fatalf("a shape mismatch must deny; got allow (%s)", dec.Reason)
	}

	// Explain carries the diagnostic in trace_json.
	res, err := c.Explain(rootCtx, query)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	var tr engine.Trace
	if err := json.Unmarshal([]byte(res.TraceJson), &tr); err != nil {
		t.Fatalf("unmarshal trace_json: %v", err)
	}
	if len(tr.Notes) != 1 {
		t.Fatalf("trace notes = %+v, want exactly one\n%s", tr.Notes, res.TraceJson)
	}
	n := tr.Notes[0]
	if n.Kind != "shape_mismatch" || n.Path != "principal.id" ||
		n.Expected != "collection" || n.Actual != "string" || n.Rule != "vip" {
		t.Fatalf("note = %+v, want the shape mismatch on principal.id from rule vip", n)
	}
	if !strings.Contains(res.TraceJson, "principal.id: expected collection, got string") {
		t.Fatalf("trace_json omits the rendered note:\n%s", res.TraceJson)
	}

	// A rule with no shape problem records nothing — the channel is silent when
	// there is nothing to say.
	clean := rules.Compare(rules.OpEq, rules.Var("principal.id"), rules.Lit("alice"))
	if _, err := c.PutRule(rootCtx, &rpc.RuleRequest{
		Actor:    &rpc.Actor{Account: acct},
		RuleJson: ruleJSON(t, "vip", clean),
	}); err != nil {
		t.Fatalf("PutRule(clean): %v", err)
	}
	res, err = c.Explain(rootCtx, query)
	if err != nil {
		t.Fatalf("Explain(clean): %v", err)
	}
	var clear engine.Trace
	if err := json.Unmarshal([]byte(res.TraceJson), &clear); err != nil {
		t.Fatalf("unmarshal trace_json: %v", err)
	}
	if len(clear.Notes) != 0 {
		t.Fatalf("a well-shaped rule should record no notes, got %+v", clear.Notes)
	}
}
