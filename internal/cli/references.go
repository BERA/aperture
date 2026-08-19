package cli

import (
	"strings"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/service"

	ucli "github.com/urfave/cli/v3"
)

// The reference edge on the command line.
//
// `aperture enumerate` can restrict its result to the identities a holder
// object's DECLARED reference field contains — "which brands belong to dataset
// X?". An edge is three plain strings, and two of them are already spelled
// together everywhere else in Aperture, so the flag takes the same spelling:
//
//	--via account:acme/dataset:x.current_brands
//
// Nothing here is typed, so unlike --field / --fields-json there is no second
// spelling and no value model to reconcile: an edge NAMES a field, it never
// carries one.
//
// The holder's object-type is NOT stated. It is the terminal segment type of
// the identity ("account:acme/dataset:x" is a dataset), which the engine derives
// itself; asking an operator to repeat it would only create a way for the two to
// disagree.
const viaFlagName = "via"

// referenceEdgeFlags is the repeatable edge flag. It is a function, not a var,
// to match metadataFilterFlags: each command gets its own flag values.
func referenceEdgeFlags() []ucli.Flag {
	return []ucli.Flag{
		&ucli.StringSliceFlag{
			Name: viaFlagName,
			Usage: "restrict the result to the objects a holder's declared reference field names, " +
				"as <holder-identity>.<field> (e.g. --via account:acme/dataset:x.current_brands); " +
				"repeatable, and several edges are ANDed. The FIELD is everything after the LAST '.'",
		},
	}
}

// parseReferenceEdges builds the enumerate reference edges from the repeatable
// --via flag.
//
// The split is on the LAST '.', because a '.' is legal inside an identity
// component ("dataset:2026.q1") while a reference field is a single metadata key
// — so the final dot is the only unambiguous boundary between the two.
//
// The result is nil when the flag was not given: nil is "no edges", which
// restricts nothing and is deliberately indistinguishable from an unrestricted
// enumeration.
//
// A malformed value is an APERTURE_INVALID_INPUT coded error naming the
// offending text, exactly as a malformed --field is — never a silent skip, since
// a dropped edge would WIDEN the result and an edge that silently widens is an
// edge that authorizes.
//
// Only the SHAPE is checked here. Whether the holder identity parses, whether
// its type serves a provider, and whether the field is a declared reference are
// the engine's to answer (engine/reference.go), because those answers carry the
// empty-vs-NOT_FOUND disclosure boundary every surface has to share.
func parseReferenceEdges(vias []string) ([]service.ReferenceEdge, error) {
	if len(vias) == 0 {
		return nil, nil
	}
	out := make([]service.ReferenceEdge, 0, len(vias))
	for _, via := range vias {
		idx := strings.LastIndexByte(via, '.')
		if idx < 0 {
			return nil, aerr.Newf(aerr.APERTURE_INVALID_INPUT,
				"--%s %s is not <holder-identity>.<field>; an edge needs a '.', "+
					"e.g. --%s account:acme/dataset:x.current_brands",
				viaFlagName, quoteFlagValue(via), viaFlagName)
		}
		holder, field := via[:idx], via[idx+1:]
		if holder == "" {
			return nil, aerr.Newf(aerr.APERTURE_INVALID_INPUT,
				"--%s %s has an empty holder identity", viaFlagName, quoteFlagValue(via))
		}
		if field == "" {
			return nil, aerr.Newf(aerr.APERTURE_INVALID_INPUT,
				"--%s %s has an empty reference field", viaFlagName, quoteFlagValue(via))
		}
		// HolderType is left empty on purpose: the engine takes it from the
		// identity's terminal segment, so the CLI cannot state a type that
		// disagrees with the id it sends.
		out = append(out, service.ReferenceEdge{HolderID: holder, Field: field})
	}
	return out, nil
}
