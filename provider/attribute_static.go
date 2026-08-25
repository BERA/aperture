package provider

import (
	"context"

	aerr "github.com/frankbardon/aperture/errors"
)

// The in-memory AttributeProvider.
//
// It is Static's counterpart for the attribute seam, and it exists for the same
// three callers: a seed document declaring principal attributes inline, a test
// that needs two principals and no directory, an embedded demo with its data
// compiled in. All three want the SAME provider semantics a real directory has —
// Fetch's APERTURE_NOT_FOUND, List's stable order, Query's Fields contract —
// over a slice already in memory.
//
// It is deliberately immutable: the whole record set is supplied once, at
// construction, validated once, and never changes. That is what makes it safe
// for concurrent use with no lock, and what makes the read-only bag contract
// trivially true — there is no reload that could edit a map the registry has
// already cached.

// compile-time assertion: a *StaticAttributes is a usable AttributeProvider.
var _ AttributeProvider = (*StaticAttributes)(nil)

// StaticAttributes is an in-memory AttributeProvider for one slot, built from a
// fixed slice of AttributeRecords. It is safe for concurrent use because it is
// immutable after NewStaticAttributes returns.
//
//	p, err := provider.NewStaticAttributes([]provider.AttributeRecord{
//		{ID: "u-1", Attributes: provider.Metadata{"department": "eng"}},
//	})
//	reg.MustRegister(provider.AttributeSlotUser, p, provider.WithTTL(0))
//
// A TTL of 0 (never expire) is the natural cache setting: the data cannot go
// stale, because nothing can change it.
type StaticAttributes struct {
	byID  map[string]Metadata
	order []string // preserves declaration order for stable List/Query output
}

// NewStaticAttributes builds a StaticAttributes from records, in declaration
// order.
//
// Construction is LOAD TIME, and load time is where every check happens, so a
// Fetch/List/Query can never fail for a reason the caller could have been told
// about at wiring time:
//
//   - a record with an empty key, or with the account wildcard "*" as its key,
//     is APERTURE_ATTRIBUTE_PROVIDER_INVALID — neither names one subject;
//   - a duplicate key is APERTURE_ATTRIBUTE_PROVIDER_INVALID naming it, because
//     the last writer silently winning is how one principal's attributes become
//     another's;
//   - a bag violating the shared value model is APERTURE_METADATA_INVALID naming
//     the key, the field, and the offending path — the SAME code, and the same
//     four fixups, that the same bad value would produce as object metadata.
//     There is one value model, so there is one rejection.
//
// The bag is not re-validated on the read path. This is the gate, and it is here
// so a shape the expression evaluator cannot survive never reaches a Check —
// which for an attribute bag would not be one bad object but every rule against
// every object in the decision.
//
// Every value is DEEP-COPIED into the provider, so the record set it serves is
// its own: a caller that keeps and mutates the maps it passed in cannot reach
// through into a bag the registry has already cached. Nothing is copied on READ
// — Fetch/List/Query hand out the provider's own maps by reference, per the
// read-only contract in attribute.go.
func NewStaticAttributes(records []AttributeRecord) (*StaticAttributes, error) {
	s := &StaticAttributes{
		byID:  make(map[string]Metadata, len(records)),
		order: make([]string, 0, len(records)),
	}
	for _, rec := range records {
		switch rec.ID {
		case "":
			return nil, aerr.New(aerr.APERTURE_ATTRIBUTE_PROVIDER_INVALID,
				"provider: static attribute record has an empty key")
		case attributeWildcardKey:
			return nil, aerr.WithContext(aerr.APERTURE_ATTRIBUTE_PROVIDER_INVALID,
				"provider: the account wildcard is not an attribute key",
				map[string]any{"key": attributeWildcardKey})
		}
		if _, dup := s.byID[rec.ID]; dup {
			return nil, aerr.WithContext(aerr.APERTURE_ATTRIBUTE_PROVIDER_INVALID,
				"provider: static attribute record is declared more than once",
				map[string]any{"key": rec.ID})
		}
		if err := ValidateMetadata(rec.Attributes); err != nil {
			return nil, aerr.Wrapf(aerr.APERTURE_METADATA_INVALID, err,
				"provider: attributes for %s are rejected by the value model", rec.ID)
		}
		s.byID[rec.ID] = cloneMetadata(rec.Attributes)
		s.order = append(s.order, rec.ID)
	}
	return s, nil
}

// Fetch returns id's bag, or APERTURE_NOT_FOUND when no record was declared
// under it — so a consumer can tell an unknown subject from a failed lookup.
func (s *StaticAttributes) Fetch(_ context.Context, id string) (Metadata, error) {
	md, ok := s.byID[id]
	if !ok {
		return nil, aerr.WithContext(aerr.APERTURE_NOT_FOUND,
			"provider: no static attribute record with this key",
			map[string]any{"key": id})
	}
	return md, nil
}

// List returns every record in declaration order.
func (s *StaticAttributes) List(_ context.Context) ([]AttributeRecord, error) {
	out := make([]AttributeRecord, 0, len(s.order))
	for _, id := range s.order {
		out = append(out, AttributeRecord{ID: id, Attributes: s.byID[id]})
	}
	return out, nil
}

// Query returns the records satisfying filter, in declaration order.
// AttributeFilter.Fields go through MatchFields — the shared implementation of
// the Fields contract, so a collection field matches by MEMBERSHIP and
// everything else by typed equality — and Limit is honoured directly. The
// registry re-enforces both, so honouring them here is an optimisation that also
// makes Query correct when called standalone.
func (s *StaticAttributes) Query(_ context.Context, filter AttributeFilter) ([]AttributeRecord, error) {
	out := make([]AttributeRecord, 0, len(s.order))
	for _, id := range s.order {
		md := s.byID[id]
		if !MatchFields(md, filter.Fields) {
			continue
		}
		out = append(out, AttributeRecord{ID: id, Attributes: md})
		if filter.Limit > 0 && len(out) >= filter.Limit {
			break
		}
	}
	return out, nil
}

// Len reports how many records were declared. It is the cheap way for a caller
// (a seed loader logging what it wired, a test) to confirm a set landed without
// walking List.
func (s *StaticAttributes) Len() int { return len(s.order) }
