package csvprovider

import (
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"sync"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/provider"
)

// The attribute seam, over the same file.
//
// Attributes is *Provider's counterpart for provider.AttributeProvider: one CSV
// file, the same header grammar, the same type suffixes, the same value model,
// serving one attribute SLOT instead of one object-type.
//
//	attribute_providers:
//	  - {subject: user, kind: csv, path: users.csv}
//
//	id,department,clearance:int,teams:list,hired_at:date
//	alice,eng,3,platform|oncall,2024-03-04
//
// "Same shape and rules as an object provider" buys the VALUE MODEL for free —
// every column-suffix spelling behaves identically here, because parseTable is
// literally the same walk — and it buys nothing else. The two seams' registry
// types deliberately diverge even though their method sets line up, so this type
// is a thin adapter and the divergence stays where it belongs.
//
// # The id column holds BARE subject ids
//
// This is the one thing an author copying an object provider's file gets wrong,
// and it is the one thing this loader cannot detect for them:
//
//	users.csv     id,department      ->  alice,eng            CORRECT
//	brands.csv    id,tier            ->  brand:1,gold         (an OBJECT file)
//
// An attribute key is a principal id or an account id — an opaque handle into
// the host's directory, which Aperture never parses (see
// provider.AttributeRecord). "user:alice" is therefore a perfectly legal key,
// stored verbatim, and it is also a key no Fetch will ever present, because the
// decision path fetches by the principal's bare id. Such a file loads, caches,
// enumerates, and answers nothing. There is no error to raise — the key is
// opaque, so the loader has nothing to test it against — which is exactly why
// the asymmetry is written down here, on the type, and in seed's
// AttributeProvider.GetAll for the statement that has the same trap.
//
// The header still names the id column "id", the same spelling the object file
// uses. The seed entry's id_column: is a kind: sql concept — it names a result
// COLUMN of get_all — and, like every other kind-specific key on that block, it
// does not apply here.
//
// # Loading is EAGER, unlike *Provider's
//
// A *Provider reads its file lazily on first use, because a host wiring an
// object type during development may not have written the file yet, and the
// failure lands on that one type's fetches.
//
// An attribute file is loaded at construction instead, so a malformed one is a
// coded error at BUILD, naming the file and the row. The reason is the blast
// radius: an attribute bag is read by every rule against every object in a
// decision, so a file that cannot be parsed is not one type failing to answer,
// it is every decision for that slot failing — and a failure at boot is a
// failure the operator is present for. Reload re-reads the file afterwards, and
// leaves the current set untouched if the new one does not parse.

// compile-time assertion: an *Attributes is a usable AttributeProvider.
var _ provider.AttributeProvider = (*Attributes)(nil)

// attributeWildcardKey is the account wildcard, spelled here as a literal for
// the reason provider spells it as one: it is model.AccountWildcard, and neither
// package may import model. Refusing it at the LINE is what this copy buys — the
// registry's own key guard refuses it too, but it can only name the key, and a
// file is fixed by line number.
const attributeWildcardKey = "*"

// Attributes is a CSV-file-backed provider.AttributeProvider for one attribute
// slot. It is safe for concurrent use: the file is read under a write lock at
// construction (and again on Reload, which swaps the set atomically) and every
// read serves the in-memory set under a read lock.
//
//	attrs, err := csvprovider.NewAttributes("users.csv")
//	if err != nil { return err }
//	reg.MustRegister(provider.AttributeSlotUser, attrs, provider.WithTTL(0))
type Attributes struct {
	path string

	mu    sync.RWMutex
	byID  map[string]provider.Metadata
	order []string // preserves file order for stable List/Query output
}

// NewAttributes reads path NOW and returns the provider serving it, or the coded
// error that says why the file cannot be served — naming the file and, for
// anything below the header, the row. See the file doc for why this is eager
// where Provider is lazy.
func NewAttributes(path string) (*Attributes, error) {
	byID, order, err := parseAttributeFile(path)
	if err != nil {
		return nil, err
	}
	return &Attributes{path: path, byID: byID, order: order}, nil
}

// AttributesFromReader builds an Attributes from r. It has no path, so Reload
// returns APERTURE_CONFIG_INVALID; use it for embedded or in-memory data (and
// tests) rather than a file on disk.
func AttributesFromReader(r io.Reader) (*Attributes, error) {
	byID, order, err := parseTable(r, attributeKey)
	if err != nil {
		return nil, err
	}
	return &Attributes{byID: byID, order: order}, nil
}

// Reload re-reads the file and atomically replaces the in-memory set. Call it
// after the underlying file changes; bags the slot's cache already holds are
// served until their TTL expires, so a slot whose file is reloaded out of band
// wants a TTL rather than the 0 (never expire) that suits a fixed file. If
// parsing fails the current set is left untouched, so a bad edit degrades to
// stale data rather than to no directory at all.
func (a *Attributes) Reload() error {
	if a.path == "" {
		return aerr.New(aerr.APERTURE_CONFIG_INVALID,
			"csvprovider: reader-backed attribute provider cannot be reloaded")
	}
	byID, order, err := parseAttributeFile(a.path)
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.byID, a.order = byID, order
	a.mu.Unlock()
	return nil
}

// Fetch returns id's attribute bag, or APERTURE_NOT_FOUND when the file has no
// row for it — so the registry (and the resolvers above it) can tell "there is
// no such subject" from "the directory is unreachable". The two mean opposite
// things for a decision.
//
// The returned map is the provider's own and is READ-ONLY to every holder,
// transitively: it is the whole decision's view of this subject, shared across
// every object being checked. Reload never mutates a map it has handed out — it
// builds a fresh set and swaps it in.
func (a *Attributes) Fetch(_ context.Context, id string) (provider.Metadata, error) {
	a.mu.RLock()
	md, ok := a.byID[id]
	a.mu.RUnlock()
	if !ok {
		return nil, aerr.WithContext(aerr.APERTURE_NOT_FOUND,
			"csvprovider: no attribute row with this key", map[string]any{"key": id})
	}
	return md, nil
}

// List returns every record in file order.
func (a *Attributes) List(ctx context.Context) ([]provider.AttributeRecord, error) {
	return a.Query(ctx, provider.AttributeFilter{})
}

// Query returns the records satisfying filter, in file order.
// AttributeFilter.Fields go through provider.MatchFields — the shared
// implementation of the Fields contract, so a list column matches by MEMBERSHIP
// and every other column by typed equality — and Limit is honoured directly. The
// registry re-enforces both, so honouring them here is an optimisation that also
// makes Query correct when called standalone.
func (a *Attributes) Query(_ context.Context, filter provider.AttributeFilter) ([]provider.AttributeRecord, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]provider.AttributeRecord, 0, len(a.order))
	for _, id := range a.order {
		md := a.byID[id]
		if !provider.MatchFields(md, filter.Fields) {
			continue
		}
		out = append(out, provider.AttributeRecord{ID: id, Attributes: md})
		if filter.Limit > 0 && len(out) >= filter.Limit {
			break
		}
	}
	return out, nil
}

// Len reports how many rows the file held. It is the cheap way for a caller (a
// loader logging what it wired, a test) to confirm a file landed without walking
// List.
func (a *Attributes) Len() int {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return len(a.order)
}

// attributeKey is the attribute seam's row key: the id cell is a BARE subject
// id, taken verbatim, because an attribute key is an opaque handle with no
// grammar to validate against.
//
// Exactly one string is refused, and it is refused for the reason the registry
// refuses it: "*" is the all-accounts grant sentinel, so a row filed under it
// asks for the attributes of every account, and the only bag that could satisfy
// that is one account's data served as another's. The empty key is refused a
// line earlier, by the walk itself.
func attributeKey(raw string, line int) (string, string, error) {
	if raw == attributeWildcardKey {
		return "", "", aerr.WithContext(aerr.APERTURE_ATTRIBUTE_PROVIDER_INVALID,
			"csvprovider: the account wildcard is not an attribute key",
			map[string]any{"line": line, "key": attributeWildcardKey})
	}
	return raw, raw, nil
}

// parseAttributeFile opens path, parses it the attribute way, and names the file
// in whatever it returns.
func parseAttributeFile(path string) (map[string]provider.Metadata, []string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, withFile(aerr.Wrap(aerr.APERTURE_CONFIG_INVALID,
			"csvprovider: cannot open attribute data file", err), path)
	}
	defer f.Close()
	byID, order, err := parseTable(f, attributeKey)
	if err != nil {
		return nil, nil, withFile(err, path)
	}
	return byID, order, nil
}

// withFile names the file a coded error came from — in its context, and as a
// file:line suffix on its message.
//
// It is deliberately not a Wrap. Wrap RE-STAMPS and adds a layer, so wrapping
// here would either replace the code the author needs (APERTURE_METADATA_INVALID
// for a bag the value model rejects, APERTURE_ATTRIBUTE_PROVIDER_INVALID for an
// unusable key) or, at the same code, add a second Aperture-coded error to a
// chain that tests assert the DEPTH of. The file name is not a classification —
// it is one more piece of context on the same failure — so the error is rebuilt
// with the same code, the same cause and the same chain depth.
//
// The suffix is not decoration. Structured context is not rendered by every
// surface an operator reads (`aperture check` prints the code and the message
// and nothing else), and an address that only appears in a field nobody prints
// is an address nobody has. The walk knows the line and the column; only the
// caller that opened the file knows which file; together they are the whole
// address of the offending cell, so both go where the operator will see them.
func withFile(err error, path string) error {
	ce, ok := err.(*aerr.CodedError)
	if !ok || path == "" {
		return err
	}
	ctx := make(map[string]any, len(ce.Context)+1)
	maps.Copy(ctx, ce.Context)
	if _, named := ctx["path"]; !named {
		ctx["path"] = path
	}
	where := path
	if line, ok := ctx["line"].(int); ok {
		where = fmt.Sprintf("%s:%d", path, line)
	}
	return &aerr.CodedError{
		Code:    ce.Code,
		Msg:     fmt.Sprintf("%s (%s)", ce.Msg, where),
		Context: ctx,
		Inner:   ce.Inner,
	}
}
