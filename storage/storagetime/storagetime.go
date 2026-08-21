// Package storagetime owns the single representation Aperture's storage layer
// uses for an instant: a signed 64-bit count of NANOSECONDS since the Unix epoch
// (1970-01-01T00:00:00Z), in UTC.
//
// Every backend under storage/ shares this one contract — INTEGER in SQLite,
// BIGINT in Postgres, and a plain time.Time in the in-memory backend — so a
// stored instant means the same thing everywhere and round-trips exactly. Text
// encodings are deliberately not used: variable-length fractional seconds
// mis-sort, and a per-dialect timestamp type would silently truncate (Postgres
// TIMESTAMPTZ is microsecond resolution, which cannot carry the nanosecond-exact
// round trip the conformance suite asserts).
//
// The package lives beneath storage/ so sqlite, memory, and postgres can all
// reach it. It imports nothing from Aperture but the errors package, and in
// particular it creates no import edge to seed/.
//
// # The zero mapping
//
// The zero time.Time is year 1, which is OUTSIDE the representable window, so
// time.Time{}.UTC().UnixNano() returns an overflowed value rather than 0. This
// package owns that mapping instead of leaving it to each call site:
//
//	Encode(time.Time{}) == 0
//	Decode(0)           == time.Time{}
//
// 0 therefore means "unset", which is what lets the columns stay NOT NULL with
// DEFAULT 0. The accepted cost is that an instant of exactly the Unix epoch is
// indistinguishable from unset. Aperture stamps its timestamps from a real clock
// and never writes the epoch, so nothing in the model can hit that collision.
//
// # The representable window
//
// Int64 nanoseconds span 1677-09-21T00:12:43.145224192Z through
// 2262-04-11T23:47:16.854775807Z. An instant outside that window is rejected with
// APERTURE_INVALID_INPUT — it is never wrapped, clamped, or written as an
// overflow value. Callers that hold a time.Time without encoding it (the
// in-memory backend does) call Validate to apply the same rule.
package storagetime

import (
	"math"
	"time"

	aerr "github.com/frankbardon/aperture/errors"
)

// Bounds of the int64-nanosecond window, as nanosecond counts.
const (
	// MinNanos is the smallest storable instant, 1677-09-21T00:12:43.145224192Z.
	MinNanos int64 = math.MinInt64
	// MaxNanos is the largest storable instant, 2262-04-11T23:47:16.854775807Z.
	MaxNanos int64 = math.MaxInt64
	// Unset is the on-disk value for "no instant recorded". It is what the zero
	// time.Time encodes to and what decodes back to the zero time.Time.
	Unset int64 = 0
)

// minTime and maxTime are the window's endpoints as instants. They are compared
// against with time.Time.Before/After rather than by calling UnixNano, because
// UnixNano is exactly the operation that overflows outside this window.
var (
	minTime = time.Unix(0, MinNanos).UTC()
	maxTime = time.Unix(0, MaxNanos).UTC()
)

// Min returns the earliest instant that can be stored,
// 1677-09-21T00:12:43.145224192Z.
func Min() time.Time { return minTime }

// Max returns the latest instant that can be stored,
// 2262-04-11T23:47:16.854775807Z.
func Max() time.Time { return maxTime }

// Representable reports whether t can be stored as int64 Unix nanoseconds. The
// zero time.Time is representable: it is the "unset" sentinel, not an instant.
func Representable(t time.Time) bool {
	if t.IsZero() {
		return true
	}
	u := t.UTC()
	return !u.Before(minTime) && !u.After(maxTime)
}

// Validate returns nil when t can be stored, and an APERTURE_INVALID_INPUT error
// otherwise. Backends that keep a time.Time in memory rather than encoding it
// call Validate so every backend refuses the same instants; a backend that
// encodes gets the same check for free from Encode.
func Validate(t time.Time) error {
	if Representable(t) {
		return nil
	}
	return aerr.WithContext(aerr.APERTURE_INVALID_INPUT,
		"timestamp is outside the storable range",
		map[string]any{
			"timestamp": t.UTC().Format(time.RFC3339Nano),
			"min":       minTime.Format(time.RFC3339Nano),
			"max":       maxTime.Format(time.RFC3339Nano),
			"unit":      "nanoseconds since the Unix epoch, UTC",
		})
}

// Encode converts an instant to its stored form: int64 NANOSECONDS since the
// Unix epoch, UTC. The zero time.Time encodes to Unset (0). An instant outside
// the representable window is rejected with APERTURE_INVALID_INPUT rather than
// written as an overflow value.
//
// This is the only place in the storage layer where a time.Time becomes an
// integer. Do not call UnixNano directly on a value that might be zero.
func Encode(t time.Time) (int64, error) {
	if t.IsZero() {
		return Unset, nil
	}
	if err := Validate(t); err != nil {
		return 0, err
	}
	return t.UTC().UnixNano(), nil
}

// Decode converts a stored int64 count of NANOSECONDS since the Unix epoch back
// to a UTC time.Time. Unset (0) decodes to the zero time.Time, so an unset
// column reads back as the zero value rather than as the Unix epoch. Every other
// int64 is representable, so Decode cannot fail.
//
// This is the only place in the storage layer where a stored integer becomes a
// time.Time.
func Decode(nanos int64) time.Time {
	if nanos == Unset {
		return time.Time{}
	}
	return time.Unix(0, nanos).UTC()
}
