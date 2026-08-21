package storagetime_test

import (
	"math"
	"testing"
	"time"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/storage/storagetime"
)

// TestZeroTimeIsNotZeroNanos is the reason this package exists. time.Time{} is
// year 1, which is outside the int64-nanosecond window, so UnixNano on it wraps
// rather than returning 0. Any call site that reaches for UnixNano directly gets
// this value into a column and only finds out at read time.
func TestZeroTimeIsNotZeroNanos(t *testing.T) {
	if raw := (time.Time{}).UTC().UnixNano(); raw == 0 {
		t.Fatalf("premise broken: time.Time{}.UnixNano() is now %d; the zero mapping may no longer need owning", raw)
	}
}

func TestZeroRoundTrip(t *testing.T) {
	n, err := storagetime.Encode(time.Time{})
	if err != nil {
		t.Fatalf("encode zero: %v", err)
	}
	if n != 0 {
		t.Fatalf("zero time encoded to %d, want 0", n)
	}
	if got := storagetime.Decode(0); !got.IsZero() {
		t.Fatalf("0 decoded to %v, want the zero time", got)
	}
	// 0 must decode to the zero time, NOT to the Unix epoch: an unset column
	// reads back as unset.
	if storagetime.Decode(0).Equal(time.Unix(0, 0)) {
		t.Fatal("0 decoded to the Unix epoch; unset and epoch must not be the same value")
	}
}

func TestRoundTripIsNanosecondExact(t *testing.T) {
	cases := []struct {
		name string
		in   time.Time
	}{
		{"whole second", time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)},
		{"millisecond", time.Date(2026, 1, 2, 3, 4, 5, 600000000, time.UTC)},
		{"sub-microsecond", time.Date(2026, 1, 2, 3, 4, 5, 123456789, time.UTC)},
		{"one nanosecond past the epoch", time.Unix(0, 1).UTC()},
		{"before the epoch", time.Date(1900, 6, 15, 8, 30, 0, 987654321, time.UTC)},
		{"minimum", storagetime.Min()},
		{"maximum", storagetime.Max()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n, err := storagetime.Encode(tc.in)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			got := storagetime.Decode(n)
			if !got.Equal(tc.in) {
				t.Fatalf("round trip lost precision: got %v want %v (nanos %d)", got, tc.in, n)
			}
			if got.Location() != time.UTC {
				t.Fatalf("decoded in %v, want UTC", got.Location())
			}
		})
	}
}

func TestEncodeNormalizesLocationAndMonotonic(t *testing.T) {
	zone := time.FixedZone("UTC+7", 7*3600)
	utc := time.Date(2026, 5, 4, 3, 2, 1, 42, time.UTC)
	shifted := utc.In(zone)

	a, err := storagetime.Encode(utc)
	if err != nil {
		t.Fatalf("encode utc: %v", err)
	}
	b, err := storagetime.Encode(shifted)
	if err != nil {
		t.Fatalf("encode shifted: %v", err)
	}
	if a != b {
		t.Fatalf("same instant encoded differently by zone: %d vs %d", a, b)
	}

	// A wall+monotonic reading and the same reading with the monotonic clock
	// stripped are the same instant, and must encode identically.
	now := time.Now()
	m, err := storagetime.Encode(now)
	if err != nil {
		t.Fatalf("encode now: %v", err)
	}
	w, err := storagetime.Encode(now.Round(0))
	if err != nil {
		t.Fatalf("encode now without monotonic: %v", err)
	}
	if m != w {
		t.Fatalf("monotonic reading changed the encoding: %d vs %d", m, w)
	}
}

func TestBoundsAreTheInt64Window(t *testing.T) {
	if storagetime.MinNanos != math.MinInt64 || storagetime.MaxNanos != math.MaxInt64 {
		t.Fatalf("bounds are not the int64 window: [%d, %d]", storagetime.MinNanos, storagetime.MaxNanos)
	}
	if got, want := storagetime.Min().Format(time.RFC3339Nano), "1677-09-21T00:12:43.145224192Z"; got != want {
		t.Fatalf("Min() = %s, want %s (the documented window start)", got, want)
	}
	if got, want := storagetime.Max().Format(time.RFC3339Nano), "2262-04-11T23:47:16.854775807Z"; got != want {
		t.Fatalf("Max() = %s, want %s (the documented window end)", got, want)
	}
	n, err := storagetime.Encode(storagetime.Min())
	if err != nil || n != storagetime.MinNanos {
		t.Fatalf("encode Min() = (%d, %v), want (%d, nil)", n, err, storagetime.MinNanos)
	}
	n, err = storagetime.Encode(storagetime.Max())
	if err != nil || n != storagetime.MaxNanos {
		t.Fatalf("encode Max() = (%d, %v), want (%d, nil)", n, err, storagetime.MaxNanos)
	}
}

func TestOutOfRangeIsRejectedNeverOverflowed(t *testing.T) {
	cases := []struct {
		name string
		in   time.Time
	}{
		{"one nanosecond before the minimum", storagetime.Min().Add(-time.Nanosecond)},
		{"one nanosecond after the maximum", storagetime.Max().Add(time.Nanosecond)},
		{"year 1600", time.Date(1600, 1, 1, 0, 0, 0, 0, time.UTC)},
		{"year 9999", time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if storagetime.Representable(tc.in) {
				t.Fatal("reported representable")
			}
			if err := storagetime.Validate(tc.in); aerr.CodeOf(err) != aerr.APERTURE_INVALID_INPUT {
				t.Fatalf("Validate: got code %q (err %v), want %s",
					aerr.CodeOf(err), err, aerr.APERTURE_INVALID_INPUT)
			}
			n, err := storagetime.Encode(tc.in)
			if aerr.CodeOf(err) != aerr.APERTURE_INVALID_INPUT {
				t.Fatalf("Encode: got code %q (err %v), want %s",
					aerr.CodeOf(err), err, aerr.APERTURE_INVALID_INPUT)
			}
			if n != 0 {
				t.Fatalf("Encode returned %d alongside the error; a rejected instant must never yield a writable value", n)
			}
		})
	}
}

func TestRepresentableAcceptsZeroAndTheWindow(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   time.Time
	}{
		{"zero time is the unset sentinel, not an instant", time.Time{}},
		{"minimum", storagetime.Min()},
		{"maximum", storagetime.Max()},
		{"an ordinary instant", time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !storagetime.Representable(tc.in) {
				t.Fatal("reported not representable")
			}
			if err := storagetime.Validate(tc.in); err != nil {
				t.Fatalf("Validate: %v", err)
			}
		})
	}
}

// TestUnsetIsTheEpochCollision pins the accepted cost of the sentinel: an instant
// of exactly the Unix epoch is indistinguishable from unset. Aperture stamps from
// a real clock and never writes the epoch, so nothing in the model can hit it —
// but the collision is real and this records it deliberately.
func TestUnsetIsTheEpochCollision(t *testing.T) {
	n, err := storagetime.Encode(time.Unix(0, 0).UTC())
	if err != nil {
		t.Fatalf("encode epoch: %v", err)
	}
	if n != storagetime.Unset {
		t.Fatalf("the epoch encoded to %d; this test's premise is that it collides with Unset (%d)",
			n, storagetime.Unset)
	}
	if got := storagetime.Decode(n); !got.IsZero() {
		t.Fatalf("the epoch round-tripped to %v; the documented cost is that it comes back as the zero time", got)
	}
}
