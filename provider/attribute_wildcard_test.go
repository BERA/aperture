package provider

import (
	"context"
	"slices"
	"sync"
	"testing"

	aerr "github.com/frankbardon/aperture/errors"
)

// E2-S2: the account wildcard never reaches a provider — pinned at the seam, for
// every slot, by watching the provider rather than by reading the error.
//
// attribute.go states the rule once ("the only bag that could satisfy it is one
// account's data served as another's") and attributeKeyError implements it once,
// for all three slots, in AttributeRegistry.Fetch. That single implementation is
// exactly why the test must sweep the whole slot set: a future refactor that
// moves the guard into the account path alone would still pass every existing
// assertion, because the account path is the only one anybody thinks about.

// keyRecordingAttributes is an AttributeProvider that records the keys it is
// asked for. It records rather than counts because the claim under test is about
// WHICH key reached the host, and a count cannot state that.
type keyRecordingAttributes struct {
	mu   sync.Mutex
	keys []string
}

func (k *keyRecordingAttributes) seen() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return slices.Clone(k.keys)
}

func (k *keyRecordingAttributes) Fetch(_ context.Context, id string) (Metadata, error) {
	k.mu.Lock()
	k.keys = append(k.keys, id)
	k.mu.Unlock()
	return Metadata{"reached": id}, nil
}

func (k *keyRecordingAttributes) List(context.Context) ([]AttributeRecord, error) {
	return nil, nil
}

func (k *keyRecordingAttributes) Query(context.Context, AttributeFilter) ([]AttributeRecord, error) {
	return nil, nil
}

// TestTheWildcardNeverReachesAProvider pins the account-isolation invariant at
// the one seam that can enforce it for every caller: "*" is refused with a coded
// error BEFORE the provider is consulted, in every slot.
//
// The provider is deliberately one that would SUCCEED — it answers every key
// with a bag — so a guard that stopped working would not merely change an error
// code, it would hand the caller attributes. That is the failure being fenced
// off, and it is why the assertion is on the keys the provider saw and not only
// on the code that came back.
func TestTheWildcardNeverReachesAProvider(t *testing.T) {
	ctx := context.Background()

	for _, slot := range AttributeSlots() {
		t.Run(slot.String(), func(t *testing.T) {
			host := &keyRecordingAttributes{}
			reg := NewAttributeRegistry()
			reg.MustRegister(slot, host)

			md, err := reg.Fetch(ctx, slot, attributeWildcardKey)
			if md != nil {
				t.Fatalf("the wildcard resolved a bag %v; one account's data would be "+
					"served as every account's", md)
			}
			if code := aerr.CodeOf(err); code != aerr.APERTURE_ATTRIBUTE_PROVIDER_INVALID {
				t.Fatalf("code = %s, want APERTURE_ATTRIBUTE_PROVIDER_INVALID", code)
			}
			if seen := host.seen(); len(seen) != 0 {
				t.Fatalf("the provider was asked for %v; the wildcard must be refused "+
					"before the host is consulted", seen)
			}

			// The control: this provider answers anything, so the refusal above is
			// the guard doing its job and not a directory that simply has no data.
			if _, err := reg.Fetch(ctx, slot, "a-real-key"); err != nil {
				t.Fatalf("control fetch failed, so the refusal above proves nothing: %v", err)
			}
			if seen := host.seen(); !slices.Equal(seen, []string{"a-real-key"}) {
				t.Fatalf("provider saw %v, want exactly [a-real-key]", seen)
			}
		})
	}

	// AccountAttributes is the resolver seam the rules engine holds, and it is
	// where a caller that has not resolved the sentinel would actually arrive. It
	// must NOT collapse the refusal into its leniency: an unregistered slot is a
	// deployment with no directory and must keep deciding, while a wildcard key is
	// a call-site bug that must be visible.
	t.Run("the resolver seam does not swallow it", func(t *testing.T) {
		host := &keyRecordingAttributes{}
		reg := NewAttributeRegistry()
		reg.MustRegister(AttributeSlotAccount, host)

		bag, err := reg.AccountAttributes(ctx, attributeWildcardKey)
		if bag != nil {
			t.Fatalf("AccountAttributes resolved a bag %v for the wildcard", bag)
		}
		if code := aerr.CodeOf(err); code != aerr.APERTURE_ATTRIBUTE_PROVIDER_INVALID {
			t.Fatalf("code = %s, want APERTURE_ATTRIBUTE_PROVIDER_INVALID — a nil bag "+
				"and no error would make the call-site bug indistinguishable from a "+
				"tenant the host does not know", code)
		}
		if seen := host.seen(); len(seen) != 0 {
			t.Fatalf("the provider was asked for %v", seen)
		}
	})

	// The principal seam is the same statement for the other two slots: Attributes
	// routes by kind, and neither route may carry the wildcard through.
	t.Run("the principal seam does not swallow it", func(t *testing.T) {
		for _, kind := range []string{AttributeSlotUser.String(), AttributeSlotMachine.String()} {
			host := &keyRecordingAttributes{}
			reg := NewAttributeRegistry()
			reg.MustRegister(AttributeSlot(kind), host)

			bag, err := reg.Attributes(ctx, kind, attributeWildcardKey)
			if bag != nil {
				t.Fatalf("kind %q: Attributes resolved a bag %v for the wildcard", kind, bag)
			}
			if code := aerr.CodeOf(err); code != aerr.APERTURE_ATTRIBUTE_PROVIDER_INVALID {
				t.Fatalf("kind %q: code = %s, want APERTURE_ATTRIBUTE_PROVIDER_INVALID", kind, code)
			}
			if seen := host.seen(); len(seen) != 0 {
				t.Fatalf("kind %q: the provider was asked for %v", kind, seen)
			}
		}
	})
}
