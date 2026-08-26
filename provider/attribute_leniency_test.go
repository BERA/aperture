package provider

import (
	"context"
	"errors"
	"testing"

	aerr "github.com/frankbardon/aperture/errors"
)

// E4-S2: the leniency contract, pinned at the seam that decides it.
//
// Two situations look identical from inside a rule and must not be:
//
//   - NOTHING WAS PROMISED — no provider registered for the slot, or a
//     registered provider reporting APERTURE_NOT_FOUND for this key. The bag is
//     the floor and the decision proceeds.
//   - SOMETHING WAS PROMISED AND FAILED — a malformed source, an IO error, a
//     query error, a context deadline. That is a coded error, surfaced as a
//     non-decision, with no empty-bag fallback anywhere on the path.
//
// E1-S3 built the first bullet's unregistered half and E2-S1 mirrored it for the
// account slot; neither pinned the APERTURE_NOT_FOUND half, and nothing pinned
// the second bullet. This file is that proof. Its rules-package twin
// (rules/attribute_leniency_test.go) carries the same contract through
// Engine.Selected, because a contract honoured at the registry and lost one
// layer up is not honoured.

// failingAttributeSource is an AttributeProvider whose every call fails with one
// chosen error. It is how an operational failure — as opposed to an absence — is
// put in front of the registry.
type failingAttributeSource struct{ err error }

func (f failingAttributeSource) Fetch(context.Context, string) (Metadata, error) {
	return nil, f.err
}

func (f failingAttributeSource) List(context.Context) ([]AttributeRecord, error) {
	return nil, f.err
}

func (f failingAttributeSource) Query(context.Context, AttributeFilter) ([]AttributeRecord, error) {
	return nil, f.err
}

// resolveBoth runs a case through BOTH resolver seams — the principal seam
// (Attributes, keyed by kind) and the account seam (AccountAttributes) — because
// the leniency contract is one contract and the two implementations of it are
// the place it can drift. A case that passes on one seam and not the other is
// the exact defect this shape catches.
func resolveBoth(t *testing.T, reg *AttributeRegistry, key string) map[string]func() (map[string]any, error) {
	t.Helper()
	ctx := context.Background()
	return map[string]func() (map[string]any, error){
		"principal": func() (map[string]any, error) { return reg.Attributes(ctx, string(AttributeSlotUser), key) },
		"account":   func() (map[string]any, error) { return reg.AccountAttributes(ctx, key) },
	}
}

// TestNothingPromisedIsTheFloor is the lenient half. Both seams answer a missing
// SOURCE with a nil bag and NO error, so the engine stamps its floor over
// nothing and the decision proceeds.
//
// The nil is the whole answer, not a placeholder: "the host knows nothing about
// this subject" is complete information, and an error here would turn a
// deployment that simply wired no machine directory into one that cannot decide
// for machine principals at all.
func TestNothingPromisedIsTheFloor(t *testing.T) {
	cases := []struct {
		name string
		// build returns a registry in which BOTH the user slot and the account
		// slot are in the state under test, so one case covers both seams.
		build func(t *testing.T) *AttributeRegistry
	}{
		{
			// No provider for the slot at all: APERTURE_ATTRIBUTE_PROVIDER_UNREGISTERED
			// one layer down, collapsed here.
			name:  "no provider is registered for the slot",
			build: func(*testing.T) *AttributeRegistry { return NewAttributeRegistry() },
		},
		{
			// A provider IS registered and simply has no row for this key:
			// APERTURE_NOT_FOUND, collapsed here. This is the half E1-S3 left
			// unpinned, and it is the more dangerous of the two — the wiring is
			// right, so nothing about the deployment looks wrong.
			name: "a registered provider has no record for this key",
			build: func(t *testing.T) *AttributeRegistry {
				t.Helper()
				reg := NewAttributeRegistry()
				reg.MustRegister(AttributeSlotUser, mustStaticAttributes(t, []AttributeRecord{
					{ID: "someone-else", Attributes: Metadata{"tier": "gold"}},
				}))
				reg.MustRegister(AttributeSlotAccount, mustStaticAttributes(t, []AttributeRecord{
					{ID: "some-other-account", Attributes: Metadata{"plan": "enterprise"}},
				}))
				return reg
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg := tc.build(t)
			for seam, resolve := range resolveBoth(t, reg, "nobody") {
				bag, err := resolve()
				if err != nil {
					t.Errorf("%s seam: a missing source must not be an error (code %s): %v",
						seam, aerr.CodeOf(err), err)
				}
				if len(bag) != 0 {
					t.Errorf("%s seam: resolved a bag %v; a missing source contributes nothing", seam, bag)
				}
			}
		})
	}
}

// TestSomethingPromisedAndFailedIsNotAnEmptyBag is the strict half, and it is
// the half that decides whether an outage can become an authorization change.
//
// Every case here is a source that EXISTS and could not answer. None of them may
// yield a bag: an empty bag makes every comparison against it false, which reads
// to an inclusive grant as a deliberate deny and to an exclusive one as a
// deliberate include — a verdict nobody wrote, produced by a broken host.
//
// Each case also asserts CHAIN DEPTH. The code alone is not enough: Wrap builds
// a fresh CodedError with whatever code it is handed and CodeOf reports the
// outermost, so a same-code re-stamp is invisible to a code assertion while
// still burying the original context a layer down. Depth is what proves the
// pass-through guard in attributeError actually passed through.
func TestSomethingPromisedAndFailedIsNotAnEmptyBag(t *testing.T) {
	// ioErr and deadline are the two shapes a host failure arrives in with NO
	// Aperture code on it; the rest arrive already classified.
	ioErr := errors.New("dial tcp 10.0.0.7:5432: connect: connection refused")

	cases := []struct {
		name string
		// err is what the registered provider returns.
		err error
		// want is the code the caller must read off the result.
		want aerr.Code
		// cause, when non-nil, must stay reachable through errors.Is — a
		// classification that loses the underlying failure is a classification
		// an operator cannot act on.
		cause error
	}{
		{
			name: "an IO error is classified, not swallowed",
			err:  ioErr,
			want: aerr.APERTURE_ATTRIBUTE_PROVIDER_FETCH,
			// The driver error survives the wrap, so the fixup's "inspect the
			// wrapped cause" is a real instruction.
			cause: ioErr,
		},
		{
			// A source that ran out of time knows nothing about the subject and
			// must not be read as knowing there is nothing to know.
			name:  "a context deadline is a failure, not an absence",
			err:   context.DeadlineExceeded,
			want:  aerr.APERTURE_ATTRIBUTE_PROVIDER_FETCH,
			cause: context.DeadlineExceeded,
		},
		{
			// A malformed source: the bag the host produced is not a value the
			// expression evaluator can survive. The value model's code names the
			// legal shapes and the caps, and it must reach the operator intact
			// rather than as a generic fetch failure.
			name: "a malformed source keeps the value model's code",
			err: aerr.WithContext(aerr.APERTURE_METADATA_INVALID,
				"provider: attribute value is deeper than the cap",
				map[string]any{"field": "org", "depth": 6}),
			want: aerr.APERTURE_METADATA_INVALID,
		},
		{
			// A query error from a database-backed slot. Its fixups are about
			// placeholders, reachability and timeouts; re-stamping would replace
			// them with "inspect the wrapped cause".
			name: "a query error keeps the SQL provider's code",
			err: aerr.WithContext(aerr.APERTURE_SQL_PROVIDER_QUERY,
				"sqlprovider: attribute fetch statement failed", map[string]any{"slot": "user"}),
			want: aerr.APERTURE_SQL_PROVIDER_QUERY,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg := NewAttributeRegistry()
			source := failingAttributeSource{err: tc.err}
			reg.MustRegister(AttributeSlotUser, source)
			reg.MustRegister(AttributeSlotAccount, source)

			for seam, resolve := range resolveBoth(t, reg, "alice") {
				bag, err := resolve()
				if err == nil {
					t.Fatalf("%s seam: a failed source returned no error; an empty bag "+
						"reads as a verdict nobody wrote", seam)
				}
				if bag != nil {
					t.Errorf("%s seam: a failed source yielded a bag %v; there is no "+
						"empty-bag fallback on this path", seam, bag)
				}
				if code := aerr.CodeOf(err); code != tc.want {
					t.Errorf("%s seam: code = %s, want %s", seam, code, tc.want)
				}
				if depth := codedDepth(err); depth != 1 {
					t.Errorf("%s seam: %d Aperture-coded errors in the chain, want exactly 1 — "+
						"Wrap re-stamps, so a same-code re-wrap is invisible to CodeOf and "+
						"buries the context that came with the original", seam, depth)
				}
				if tc.cause != nil && !errors.Is(err, tc.cause) {
					t.Errorf("%s seam: the cause %v is no longer reachable through errors.Is", seam, tc.cause)
				}
			}
		})
	}
}

// TestAnUnusableKeyIsNotCollapsedToTheFloor draws the third line, the one the
// two-bullet contract does not name: a key that can never identify one subject
// is neither an absence nor a source failure. It is a CALLER bug, and it is
// refused.
//
// The distinction is not pedantry. An absent source is a deployment fact —
// somebody chose not to wire a machine directory — and leniency is right for it.
// An empty key, or the account wildcard, is a call site that never resolved what
// it was asking about; the only bag that could satisfy "*" is one account's data
// served as another's. Collapsing either to the floor would hide a bug behind a
// plausible-looking deny.
func TestAnUnusableKeyIsNotCollapsedToTheFloor(t *testing.T) {
	reg := NewAttributeRegistry()
	reg.MustRegister(AttributeSlotUser, mustStaticAttributes(t, []AttributeRecord{
		{ID: "alice", Attributes: Metadata{"tier": "gold"}},
	}))
	reg.MustRegister(AttributeSlotAccount, mustStaticAttributes(t, []AttributeRecord{
		{ID: "acme", Attributes: Metadata{"plan": "enterprise"}},
	}))

	for _, key := range []string{"", "*"} {
		for seam, resolve := range resolveBoth(t, reg, key) {
			bag, err := resolve()
			if code := aerr.CodeOf(err); code != aerr.APERTURE_ATTRIBUTE_PROVIDER_INVALID {
				t.Errorf("%s seam, key %q: code = %s, want APERTURE_ATTRIBUTE_PROVIDER_INVALID",
					seam, key, code)
			}
			if bag != nil {
				t.Errorf("%s seam, key %q: yielded a bag %v; an unusable key has no floor", seam, key, bag)
			}
		}
	}
}
