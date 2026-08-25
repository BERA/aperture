package provider

import (
	"context"
	"testing"

	aerr "github.com/frankbardon/aperture/errors"
)

// E5-S3: invalidation is a security control.
//
// The behaviour under test is not "the cache got smaller". It is that a bag
// which HAS BEEN CHANGED AT THE SOURCE stops being served the moment an operator
// says so, rather than at the end of a TTL that is measured in decisions the
// subject should no longer be winning. Every case below is written that way: the
// directory's answer is changed underneath a warm cache, and what is asserted is
// which answer the next Fetch returns.

// revocable is a directory whose answers can be rewritten mid-test, so a stale
// cache and a fresh read are distinguishable by VALUE rather than by a counter.
type revocable struct {
	bags    map[string]Metadata
	fetches int
}

func (r *revocable) Fetch(_ context.Context, id string) (Metadata, error) {
	r.fetches++
	md, ok := r.bags[id]
	if !ok {
		return nil, aerr.WithContext(aerr.APERTURE_NOT_FOUND, "test: no such key", map[string]any{"key": id})
	}
	return md, nil
}

func (r *revocable) List(context.Context) ([]AttributeRecord, error) { return nil, nil }

func (r *revocable) Query(context.Context, AttributeFilter) ([]AttributeRecord, error) {
	return nil, nil
}

// clearanceOf reads the one field every case below revokes.
func clearanceOf(t *testing.T, reg *AttributeRegistry, slot AttributeSlot, id string) any {
	t.Helper()
	md, err := reg.Fetch(context.Background(), slot, id)
	if err != nil {
		t.Fatalf("fetch %s/%s: %v", slot, id, err)
	}
	return md["clearance"]
}

// TestInvalidateMakesARevocationTrueNow is the story's headline. A clearance is
// removed at the source while the bag is cached; without invalidation the
// registry keeps serving the revoked clearance (that IS the staleness window),
// and Invalidate closes it on the next fetch.
func TestInvalidateMakesARevocationTrueNow(t *testing.T) {
	dir := &revocable{bags: map[string]Metadata{
		"alice": {"clearance": "secret"},
		"bob":   {"clearance": "public"},
	}}
	reg := NewAttributeRegistry()
	reg.MustRegister(AttributeSlotUser, dir)

	if got := clearanceOf(t, reg, AttributeSlotUser, "alice"); got != "secret" {
		t.Fatalf("warm-up clearance = %v, want secret", got)
	}

	// The host revokes.
	dir.bags["alice"] = Metadata{"clearance": "none"}

	// The window: the cache is still authorizing against the old standing. This
	// assertion is the hazard stated executably, not a behaviour to preserve for
	// its own sake.
	if got := clearanceOf(t, reg, AttributeSlotUser, "alice"); got != "secret" {
		t.Fatalf("cached clearance = %v, want the stale secret (the TTL window is what Invalidate exists to close)", got)
	}

	dropped, err := reg.Invalidate(AttributeSlotUser, "alice")
	if err != nil {
		t.Fatalf("Invalidate: %v", err)
	}
	if !dropped {
		t.Fatal("Invalidate reported no entry for a key that was just fetched")
	}
	if got := clearanceOf(t, reg, AttributeSlotUser, "alice"); got != "none" {
		t.Fatalf("clearance after invalidation = %v, want none", got)
	}

	// One subject, one drop: bob's bag was never asked about and must not have
	// been collateral. A per-key invalidation that quietly cleared the slot would
	// cost every other subject a provider round-trip.
	before := dir.fetches
	if got := clearanceOf(t, reg, AttributeSlotUser, "bob"); got != "public" {
		t.Fatalf("bob's clearance = %v, want public", got)
	}
	if dir.fetches != before+1 {
		t.Fatalf("bob was fetched %d time(s); a first read of an uncached key is one fetch", dir.fetches-before)
	}
	// A key nothing has fetched reports false and no error: "there was nothing to
	// drop" is the state the caller asked for, not a failure.
	if dropped, err := reg.Invalidate(AttributeSlotUser, "carol"); err != nil || dropped {
		t.Fatalf("Invalidate(uncached key) = (%v, %v), want (false, nil)", dropped, err)
	}
}

// TestInvalidateSlotAndAllClearInBulk covers the two blunt forms, and pins the
// blast radius of each: a slot clear leaves every OTHER slot's cache intact,
// and only InvalidateAll touches them all.
func TestInvalidateSlotAndAllClearInBulk(t *testing.T) {
	users := &revocable{bags: map[string]Metadata{"alice": {"clearance": "secret"}}}
	accounts := &revocable{bags: map[string]Metadata{"acme": {"clearance": "gold"}}}
	reg := NewAttributeRegistry()
	reg.MustRegister(AttributeSlotUser, users)
	reg.MustRegister(AttributeSlotAccount, accounts)

	warm := func() {
		t.Helper()
		clearanceOf(t, reg, AttributeSlotUser, "alice")
		clearanceOf(t, reg, AttributeSlotAccount, "acme")
	}
	warm()

	users.bags["alice"] = Metadata{"clearance": "none"}
	accounts.bags["acme"] = Metadata{"clearance": "bronze"}

	if err := reg.InvalidateSlot(AttributeSlotUser); err != nil {
		t.Fatalf("InvalidateSlot: %v", err)
	}
	if got := clearanceOf(t, reg, AttributeSlotUser, "alice"); got != "none" {
		t.Fatalf("user clearance after InvalidateSlot = %v, want none", got)
	}
	if got := clearanceOf(t, reg, AttributeSlotAccount, "acme"); got != "gold" {
		t.Fatalf("account clearance after a USER-slot clear = %v, want the still-cached gold", got)
	}

	reg.InvalidateAll()
	if got := clearanceOf(t, reg, AttributeSlotAccount, "acme"); got != "bronze" {
		t.Fatalf("account clearance after InvalidateAll = %v, want bronze", got)
	}
}

// TestInvalidateAllOnAnEmptyRegistryIsNotAnError: "drop everything" is
// satisfiable by a registry holding nothing, so it must not report a wiring
// problem. It is also the one invalidation form that names no slot, which is why
// it cannot be used to probe which slots a deployment wires.
func TestInvalidateAllOnAnEmptyRegistryIsNotAnError(t *testing.T) {
	NewAttributeRegistry().InvalidateAll()
}

// TestInvalidateRefusesTheSameThingsFetchDoes: the slot diagnostics and the key
// guard are the SAME ones the fetch path applies, so an operator gets the same
// coded error (and the same registry fixups) from either door, and a
// pass-through stays one coded error deep.
func TestInvalidateRefusesTheSameThingsFetchDoes(t *testing.T) {
	reg := NewAttributeRegistry()
	reg.MustRegister(AttributeSlotUser, &revocable{bags: map[string]Metadata{"alice": {}}})

	cases := []struct {
		name string
		slot AttributeSlot
		id   string
		want aerr.Code
	}{
		{"an unknown slot", AttributeSlot("unicorn"), "alice", aerr.APERTURE_ATTRIBUTE_SLOT_UNKNOWN},
		{"a slot with no provider", AttributeSlotMachine, "ci", aerr.APERTURE_ATTRIBUTE_PROVIDER_UNREGISTERED},
		{"the empty key", AttributeSlotUser, "", aerr.APERTURE_ATTRIBUTE_PROVIDER_INVALID},
		{"the account wildcard", AttributeSlotUser, "*", aerr.APERTURE_ATTRIBUTE_PROVIDER_INVALID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dropped, err := reg.Invalidate(tc.slot, tc.id)
			if aerr.CodeOf(err) != tc.want {
				t.Fatalf("Invalidate = %v, want %s", err, tc.want)
			}
			if dropped {
				t.Fatal("a refused invalidation must not report a drop")
			}
			if d := codedDepth(err); d != 1 {
				t.Fatalf("coded chain depth = %d, want exactly 1", d)
			}
		})
	}

	// InvalidateSlot reports the slot diagnostics too, and nothing else.
	if err := reg.InvalidateSlot(AttributeSlot("unicorn")); aerr.CodeOf(err) != aerr.APERTURE_ATTRIBUTE_SLOT_UNKNOWN {
		t.Errorf("InvalidateSlot(unknown) = %v, want APERTURE_ATTRIBUTE_SLOT_UNKNOWN", err)
	}
	if err := reg.InvalidateSlot(AttributeSlotAccount); aerr.CodeOf(err) != aerr.APERTURE_ATTRIBUTE_PROVIDER_UNREGISTERED {
		t.Errorf("InvalidateSlot(unwired) = %v, want APERTURE_ATTRIBUTE_PROVIDER_UNREGISTERED", err)
	}
}

// TestInvalidationIsCountedPerSlot: the counters an operator reads to see that a
// drop happened are the slot's own, and a clear counts every entry it dropped.
// Pooling them across slots would hide the slot that was actually cleared.
func TestInvalidationIsCountedPerSlot(t *testing.T) {
	users := &revocable{bags: map[string]Metadata{"alice": {}, "bob": {}}}
	machines := &revocable{bags: map[string]Metadata{"ci": {}}}
	reg := NewAttributeRegistry()
	reg.MustRegister(AttributeSlotUser, users)
	reg.MustRegister(AttributeSlotMachine, machines)

	ctx := context.Background()
	for _, id := range []string{"alice", "bob"} {
		if _, err := reg.Fetch(ctx, AttributeSlotUser, id); err != nil {
			t.Fatalf("warm %s: %v", id, err)
		}
	}
	if _, err := reg.Fetch(ctx, AttributeSlotMachine, "ci"); err != nil {
		t.Fatalf("warm ci: %v", err)
	}

	if err := reg.InvalidateSlot(AttributeSlotUser); err != nil {
		t.Fatalf("InvalidateSlot: %v", err)
	}
	got, ok := reg.Stats(AttributeSlotUser)
	if !ok {
		t.Fatal("Stats(user) reported no registered provider")
	}
	if got.Invalidations != 2 {
		t.Errorf("user invalidations = %d, want 2 (one per cleared entry)", got.Invalidations)
	}
	if got.Entries != 0 {
		t.Errorf("user entries after a clear = %d, want 0", got.Entries)
	}
	machine, _ := reg.Stats(AttributeSlotMachine)
	if machine.Invalidations != 0 || machine.Entries != 1 {
		t.Errorf("machine slot = %+v, want an untouched cache (0 invalidations, 1 entry)", machine)
	}
}

// TestPerSlotCacheSettingsAreReadable is the other half of the operator story:
// the ttl: and max_size: a slot was registered with are readable back per slot,
// so a listing reports what the cache is REALLY running rather than what the
// seed file appears to ask for. The defaults are filled at registration, which
// is exactly why the reading has to come from the registry.
func TestPerSlotCacheSettingsAreReadable(t *testing.T) {
	reg := NewAttributeRegistry()
	reg.MustRegister(AttributeSlotUser, &revocable{}, WithTTL(0), WithMaxSize(7))
	reg.MustRegister(AttributeSlotAccount, &revocable{})

	user, ok := reg.CacheConfigFor(AttributeSlotUser)
	if !ok {
		t.Fatal("CacheConfigFor(user) reported no registration")
	}
	if user.TTL != 0 || user.MaxSize != 7 {
		t.Errorf("user cache config = ttl %v / max %d, want the declared 0 / 7", user.TTL, user.MaxSize)
	}
	account, ok := reg.CacheConfigFor(AttributeSlotAccount)
	if !ok {
		t.Fatal("CacheConfigFor(account) reported no registration")
	}
	if account.TTL != DefaultTTL || account.MaxSize != DefaultMaxSize {
		t.Errorf("account cache config = ttl %v / max %d, want the package defaults %v / %d",
			account.TTL, account.MaxSize, DefaultTTL, DefaultMaxSize)
	}
	if _, ok := reg.CacheConfigFor(AttributeSlotMachine); ok {
		t.Error("CacheConfigFor(unwired slot) reported a configuration")
	}
	if _, ok := reg.Stats(AttributeSlotMachine); ok {
		t.Error("Stats(unwired slot) reported counters")
	}
}
