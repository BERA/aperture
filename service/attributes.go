package service

import (
	"context"

	"github.com/frankbardon/aperture/engine"
	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/provider"
)

// This file is the attribute directory's ONE administrative door.
//
// An attribute slot is a host directory — the user table, the service-account
// registry, the tenant catalogue. Fetching ONE bag is a decision-path read: the
// decision already named the principal and the account it is about, and the bag
// is what the rules compare against. Enumerating a slot is a categorically
// different act: `List` / `Query` over the user slot hands back the host's whole
// user table, keys and bags together, to whoever asked. That is a SYSTEM-TIER
// READ, and it is gated here exactly the way Export is — directly through
// authz.Gate.RequireSystemAdmin, not as a Mutation table entry, because it
// writes nothing (see the note on authz.MutationImport, which spells out the
// same split for Export).
//
// # Why the gate lives here and not on the registry
//
// The obvious place to put an authorization check is next to the thing it
// protects, i.e. provider.AttributeRegistry.Enumerate. It cannot go there.
// provider imports ONLY identity and errors — TestProviderPackageImportsOnlyIdentityAndErrors
// parses every non-test file in the package and enforces it — and authz imports
// engine, which imports model and storage. Putting the gate on the registry
// would drag the entire decision engine into the leaf package the decision
// engine depends on.
//
// That constraint pushes the gate up, and the facade is where it belongs on the
// merits anyway: Service is the single seam every surface drives (CLI, HTTP,
// Twirp, MCP), so a gate here is a gate for all of them at once, and the
// alternative — each surface checking for itself — is the arrangement where one
// surface eventually forgets.
//
// # The gate is on the explicit path only, and that is the whole design
//
// There are two ways a directory could be read in bulk. This file gates the
// EXPLICIT one: an operator, holding an actor, asking to list a slot.
//
// The IMPLICIT one is a scope resolver enumerating "every object of this type"
// mid-decision through scope.ObjectLister. That path has no actor — a resolver
// is inside a decision about somebody else, and there is nobody to run
// RequireSystemAdmin against — so it has no gate seam and could never grow one.
// A gate there would have nothing to check.
//
// So the implicit path is not policed; it is made IMPOSSIBLE. E1-S1 built
// provider.AttributeRegistry so it structurally cannot satisfy
// scope.ObjectLister: enumeration is called Enumerate rather than List, keyed by
// an AttributeSlot rather than a bare type string, taking an AttributeFilter
// that carries no identity.Pattern to bound with, and returning
// []provider.AttributeRecord rather than []identity.Identity. Any one of those
// makes the signature unassignable; all four make it unassignable by accident.
// provider.TestAttributeRegistryIsNotAScopeLister asserts the negative against
// the real scope.ObjectLister with *provider.Registry as the positive control,
// so the guarantee cannot decay into a comment.
//
// Read those two paragraphs together, because a gate on one path invites the
// assumption that the other is unguarded. It is not unguarded — it does not
// exist. One chokepoint, and it is this file.
//
// # What this door is NOT
//
// It is not the only way an attribute value can be seen. An Explain trace
// carries the principal and account bags a decision was evaluated against, values
// included (E5-S1, engine.Trace.Attributes), and that is a deliberate disclosure:
// an operator debugging "why was this denied?" needs to see that the tier the
// rule compared was "silver". The gate closes the BULK-READ door — the one that
// returns rows for subjects the caller never named — not every door. A caller who
// can already ask a decision about a principal can already learn things about
// that principal.
//
// And it is not audited. Like Export, it mutates nothing, so it is not a
// recorded mutation; the audit trail records writes and sampled decisions.

// ListAttributes enumerates one attribute slot's directory: up to filter.Limit
// records whose bags satisfy filter.Fields, or the head of the whole slot when
// the filter is zero. It is the facade's system-tier admin read of a host
// directory, and actor must hold system-admin authority in its active account.
//
// slot is the bare slot name ("user", "machine", "account"); it crosses into a
// provider.AttributeSlot through provider.ParseAttributeSlot, the package's one
// declared crossing point, so an unknown name is APERTURE_ATTRIBUTE_SLOT_UNKNOWN
// naming the closed set rather than an empty result. The filter and the record
// type are provider's own: there is exactly one value model and one Fields
// contract in Aperture, and restating either as a facade-local struct would only
// create somewhere for them to drift apart.
//
// Failure modes, in the order they are checked:
//
//   - no attribute registry or no gate wired → APERTURE_UNIMPLEMENTED;
//   - no authenticated principal → APERTURE_UNAUTHENTICATED;
//   - authenticated but not a system-admin → APERTURE_AUTHZ_DENIED, VERBATIM
//     from the gate;
//   - an unknown slot → APERTURE_ATTRIBUTE_SLOT_UNKNOWN;
//   - a slot with no registered provider → APERTURE_ATTRIBUTE_PROVIDER_UNREGISTERED;
//   - a directory that fails → the provider's own coded error.
//
// # A refusal returns NOTHING
//
// A denied caller gets a nil slice and the coded error — no partial page, no
// count, no "0 of 12 300", and nothing that distinguishes an empty slot from a
// full one or from an unwired one. That is why the gate runs BEFORE the slot is
// resolved: parsing the slot first would let an unauthorized caller learn which
// slots this deployment has a directory for by reading which error came back.
// The gate's own error names the actor, the tier, and the authority anchor it
// failed against — facts about the CALLER, which the caller may have — and this
// method adds nothing about the slot to it.
//
// The gate's error is returned verbatim rather than wrapped, because
// aerr.Wrap RE-STAMPS: wrapping would replace APERTURE_AUTHZ_DENIED (and its
// registry fixups, which tell an operator exactly which grant to hold) with
// whatever code this layer chose, and the operator would lose the remedy.
//
// # The wiring check has to come first, and discloses nothing
//
// With no gate wired there is no authority to check, so the wiring check
// necessarily precedes it. Unlike the entity reads (see readScope), an unwired
// gate here does NOT mean "local trusted context, read freely": a bulk directory
// read has no narrower fallback to degrade to, so it refuses instead. The
// refusal reports that the surface is not wired, which is a property of this
// process's construction, not of any directory's contents.
func (s *Service) ListAttributes(ctx context.Context, actor Actor, slot string, filter provider.AttributeFilter) ([]provider.AttributeRecord, error) {
	if err := s.requireAttributeAdmin(ctx, actor); err != nil {
		return nil, err
	}
	parsed, err := provider.ParseAttributeSlot(slot)
	if err != nil {
		return nil, err // APERTURE_ATTRIBUTE_SLOT_UNKNOWN, verbatim
	}
	return s.attrs.Enumerate(ctx, parsed, filter)
}

// ExplainAttributeAuthority returns the full engine Trace behind the
// system-admin authority decision ListAttributes enforces — the same
// gate.ExplainSystemAdmin derivation, reachable from the facade an operator
// already holds.
//
// It exists because a refusal an operator cannot interrogate is a support
// ticket. APERTURE_AUTHZ_DENIED says the actor lacks system-admin authority; the
// trace says WHY — which grants were considered, which scope resolved, and
// where the derivation stopped — proving the admin check is an ordinary
// decision through the ordinary engine rather than a special-cased bypass.
//
// It discloses nothing this method's caller did not already own: the trace is of
// a Check on the CALLER'S OWN authority over the system:schema anchor, and it
// names no attribute slot, key, or bag. Whether ListAttributes would have
// returned one row or ten thousand is not in it. So it is deliberately NOT
// gated on holding the authority it explains — gating it that way would mean
// only the operators who were allowed could find out why the refused ones were
// not — and it deliberately does not require an attribute registry either: an
// operator diagnosing a refusal needs the answer whether or not this process
// wires a directory.
func (s *Service) ExplainAttributeAuthority(ctx context.Context, actor Actor) (engine.Trace, error) {
	if s.gate == nil {
		return engine.Trace{}, aerr.New(aerr.APERTURE_UNIMPLEMENTED,
			"service: the admin-authority gate is not wired; there is no authority decision to explain")
	}
	if actor.Principal == "" {
		return engine.Trace{}, aerr.New(aerr.APERTURE_UNAUTHENTICATED,
			"service: explaining attribute-read authority requires an authenticated principal")
	}
	return s.gate.ExplainSystemAdmin(ctx, actor.gateActor())
}

// requireAttributeAdmin is the single definition of "may this actor read an
// attribute directory in bulk": the surface is wired, the caller is
// authenticated, and it holds system-admin authority. Both entry points above
// call it so the three conditions cannot be checked in two different orders,
// which is how one path ends up disclosing what the other refuses to.
func (s *Service) requireAttributeAdmin(ctx context.Context, actor Actor) error {
	if s.attrs == nil {
		return aerr.New(aerr.APERTURE_UNIMPLEMENTED,
			"service: attribute providers are not wired")
	}
	if s.gate == nil {
		return aerr.New(aerr.APERTURE_UNIMPLEMENTED,
			"service: the admin-authority gate is not wired; reading an attribute directory is a system-tier read and is never served ungated")
	}
	if actor.Principal == "" {
		return aerr.New(aerr.APERTURE_UNAUTHENTICATED,
			"service: reading an attribute directory requires an authenticated principal")
	}
	// Verbatim: Wrap re-stamps, and APERTURE_AUTHZ_DENIED's registry fixups are
	// the operator's remedy.
	return s.gate.RequireSystemAdmin(ctx, actor.gateActor())
}
