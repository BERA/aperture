package provider

import (
	"context"
	"fmt"
	"slices"
	"strings"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/identity"
)

// Declared object references.
//
// A reference is an application-level foreign key: a declaration that one
// object-type's metadata FIELD holds identities of another object-type
// ("dataset.current_brands points at brand"). Nothing enforces it in a database
// — the host's schema may not even have a constraint to enforce — so the
// declaration lives here, beside the provider that serves the field, and is
// checked when it is used.
//
// Three decisions shape the model, and each of them closes a door on purpose:
//
//   - HOLDING SIDE ONLY. The reference is declared where the data physically
//     lives: on the type whose provider actually returns the field. There is no
//     inbound form ("brand is referenced by dataset"), because brand has no
//     column listing its datasets — an inbound declaration would describe a
//     derived view with nothing to attach to. It would not buy addressability
//     either: a second referencing field (archived_brands) makes an unnamed
//     reverse edge ambiguous. One declaration, one direction, no drift.
//   - A CLOSED SET OF ONE DESCRIPTOR KIND. A reference declares its TARGET
//     object-type and nothing else. In particular there is deliberately no
//     "type" descriptor: the value model is spelled by the loader (a CSV column
//     suffix, a cast in the developer's SQL), and a second place to declare a
//     type is a second place for the two declarations to disagree.
//   - FULL CANONICAL IDENTITIES AS VALUES. The field holds "brand:1", or
//     "account:acme/brand:1" — never a bare primary key, and never a foreign
//     key Aperture would have to template into an identity. The developer
//     composes the identity where the data is loaded, which is why an
//     enumeration can filter on one with the ordinary Filter.Fields contract
//     (Fields{"current_brands": "brand:1"}) and no new matching code.
//
// A declaration names a FIELD, and metadata fields are discovered at fetch
// rather than declared, so declaring a reference on a field no object happens to
// carry is NOT an error: it resolves to nothing, exactly as an absent field
// does. What IS an error is a value that does not point where the declaration
// says it does — see ResolveReferenceValue.

// Reference is one declared reference: the object-type holding the field, the
// metadata field name, and the object-type its values identify.
type Reference struct {
	// ObjectType is the type whose provider serves Field — the HOLDING side.
	ObjectType string
	// Field is the metadata field name whose value holds identities.
	Field string
	// Target is the object-type every identity in Field's value must belong to,
	// by terminal segment type.
	Target string
}

// String renders a reference as "dataset.current_brands -> brand".
func (r Reference) String() string {
	return fmt.Sprintf("%s.%s -> %s", r.ObjectType, r.Field, r.Target)
}

// DeclareReference declares that objectType's metadata field holds identities of
// target. Both types must already be registered: the declaration is wiring, and
// wiring that names a type nothing serves is a typo, not a forward reference.
// Declare references after registering every provider.
//
//	reg.MustRegister("brand", brands)
//	reg.MustRegister("dataset", datasets)
//	if err := reg.DeclareReference("dataset", "current_brands", "brand"); err != nil { ... }
//
// It is the Go spelling of a seed document's references: block, and the two
// paths reach exactly this method, so anything expressible in YAML is
// expressible here.
//
// An empty argument, an unknown target, or a field already declared on this type
// is APERTURE_PROVIDER_REFERENCE_INVALID; an unregistered holding type is
// APERTURE_PROVIDER_UNREGISTERED. A type may reference itself (a parent link),
// and several fields may reference the same target — it is the FIELD that is
// declared at most once, because it is the field that resolves.
func (r *Registry) DeclareReference(objectType, field, target string) error {
	if strings.TrimSpace(objectType) == "" || strings.TrimSpace(field) == "" || strings.TrimSpace(target) == "" {
		return aerr.WithContext(aerr.APERTURE_PROVIDER_REFERENCE_INVALID,
			"provider: a reference declaration needs an object type, a metadata field, and a target object type",
			map[string]any{"object_type": objectType, "field": field, "target": target})
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[objectType]
	if !ok {
		return aerr.WithContext(aerr.APERTURE_PROVIDER_UNREGISTERED,
			fmt.Sprintf("provider: cannot declare reference %s.%s: no provider is registered for object type %q",
				objectType, field, objectType),
			map[string]any{"object_type": objectType, "field": field, "target": target})
	}
	if _, ok := r.entries[target]; !ok {
		return aerr.WithContext(aerr.APERTURE_PROVIDER_REFERENCE_INVALID,
			fmt.Sprintf("provider: reference %s.%s points at object type %q, which has no registered provider",
				objectType, field, target),
			map[string]any{"object_type": objectType, "field": field, "target": target})
	}
	if prev, dup := e.references[field]; dup {
		return aerr.WithContext(aerr.APERTURE_PROVIDER_REFERENCE_INVALID,
			fmt.Sprintf("provider: field %s.%s already references object type %q", objectType, field, prev),
			map[string]any{"object_type": objectType, "field": field, "target": target, "declared_target": prev})
	}
	if e.references == nil {
		e.references = make(map[string]string)
	}
	e.references[field] = target
	return nil
}

// MustDeclareReference is DeclareReference that panics on error; for host
// startup wiring where a bad declaration is a programming error, matching
// MustRegister.
func (r *Registry) MustDeclareReference(objectType, field, target string) {
	if err := r.DeclareReference(objectType, field, target); err != nil {
		panic(err)
	}
}

// ReferenceTarget answers "what does dataset.current_brands point at?" — the
// declared target object-type, and whether the field is a declared reference at
// all. It is the lookup the engine resolves an enumeration through, and it reads
// the live registry rather than reparsing the document that built it.
func (r *Registry) ReferenceTarget(objectType, field string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.entries[objectType]
	if !ok {
		return "", false
	}
	target, ok := e.references[field]
	return target, ok
}

// References returns every reference declared on objectType, sorted by field
// name so the result is stable and diffable. An unregistered type, or one with
// no declarations, returns nil.
func (r *Registry) References(objectType string) []Reference {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.entries[objectType]
	if !ok || len(e.references) == 0 {
		return nil
	}
	out := make([]Reference, 0, len(e.references))
	for field, target := range e.references {
		out = append(out, Reference{ObjectType: objectType, Field: field, Target: target})
	}
	slices.SortFunc(out, func(a, b Reference) int { return strings.Compare(a.Field, b.Field) })
	return out
}

// AllReferences returns every declared reference in the registry, sorted by
// object-type then field. It is the whole wiring picture, for a host that wants
// to log or display it at startup.
func (r *Registry) AllReferences() []Reference {
	r.mu.RLock()
	types := make([]string, 0, len(r.entries))
	for typ, e := range r.entries {
		if len(e.references) > 0 {
			types = append(types, typ)
		}
	}
	r.mu.RUnlock()
	slices.Sort(types)
	var out []Reference
	for _, typ := range types {
		out = append(out, r.References(typ)...)
	}
	return out
}

// ResolveReference returns the identities id's declared reference field points
// at, fetching id's metadata through the cache exactly as Fetch does.
//
// The field must be DECLARED (APERTURE_PROVIDER_REFERENCE_INVALID otherwise) —
// resolving an undeclared field would be reading an arbitrary string as an
// identity. The field's VALUE need not be present: metadata fields are
// discovered at fetch, so an object that simply does not carry the field
// resolves to no identities and no error, the same answer a null value gives.
// A value that IS present and does not point at the declared target is
// APERTURE_PROVIDER_REFERENCE_MISMATCH — never a silent miss.
func (r *Registry) ResolveReference(ctx context.Context, id identity.Identity, field string) ([]identity.Identity, error) {
	objectType := terminalType(id)
	target, ok := r.ReferenceTarget(objectType, field)
	if !ok {
		return nil, undeclaredReference(objectType, field)
	}
	md, err := r.Fetch(ctx, id)
	if err != nil {
		return nil, err
	}
	value, present := md[field]
	if !present {
		return nil, nil
	}
	return referencedIdentities(objectType, field, target, value)
}

// ResolveReferenceValue is ResolveReference over a metadata value the caller
// already holds — the same resolution with no fetch, for a consumer that just
// enumerated a page of objects and has their metadata in hand.
//
// The accepted value shapes are exactly the two the metadata value model can
// spell for identities, so a reference field may be scalar or array-valued:
//
//   - a STRING: one identity ("brand:1");
//   - a []any of strings: many ("brand:1", "brand:2"), in order, duplicates kept;
//   - nil: no identities, no error.
//
// Everything else is APERTURE_PROVIDER_REFERENCE_MISMATCH, as is a string that
// does not parse as an identity or whose TERMINAL SEGMENT TYPE is not the
// declared target. A "team:7" sitting in a field declared to hold brands is a
// wiring or data fault, and reporting it as an empty result would turn it into a
// silent denial one layer up.
func (r *Registry) ResolveReferenceValue(objectType, field string, value any) ([]identity.Identity, error) {
	target, ok := r.ReferenceTarget(objectType, field)
	if !ok {
		return nil, undeclaredReference(objectType, field)
	}
	return referencedIdentities(objectType, field, target, value)
}

// undeclaredReference is the error for asking about a field no reference
// declares.
func undeclaredReference(objectType, field string) error {
	return aerr.WithContext(aerr.APERTURE_PROVIDER_REFERENCE_INVALID,
		fmt.Sprintf("provider: field %s.%s is not a declared reference", objectType, field),
		map[string]any{"object_type": objectType, "field": field})
}

// referencedIdentities turns one reference field's value into the identities it
// names, rejecting anything that does not point at target.
func referencedIdentities(objectType, field, target string, value any) ([]identity.Identity, error) {
	switch v := value.(type) {
	case nil:
		return nil, nil
	case string:
		id, err := referencedIdentity(objectType, field, target, v)
		if err != nil {
			return nil, err
		}
		return []identity.Identity{id}, nil
	case []any:
		out := make([]identity.Identity, 0, len(v))
		for i, elem := range v {
			s, ok := elem.(string)
			if !ok {
				return nil, aerr.WithContext(aerr.APERTURE_PROVIDER_REFERENCE_MISMATCH,
					fmt.Sprintf("provider: reference %s.%s holds a list whose element %d is %T, not an identity string",
						objectType, field, i, elem),
					map[string]any{"object_type": objectType, "field": field, "target": target,
						"index": i, "value_type": fmt.Sprintf("%T", elem)})
			}
			id, err := referencedIdentity(objectType, field, target, s)
			if err != nil {
				return nil, err
			}
			out = append(out, id)
		}
		return out, nil
	default:
		return nil, aerr.WithContext(aerr.APERTURE_PROVIDER_REFERENCE_MISMATCH,
			fmt.Sprintf("provider: reference %s.%s holds a %T; a reference field holds a canonical identity string or a list of them",
				objectType, field, value),
			map[string]any{"object_type": objectType, "field": field, "target": target,
				"value_type": fmt.Sprintf("%T", value)})
	}
}

// referencedIdentity parses one referenced value and checks it against target.
func referencedIdentity(objectType, field, target, value string) (identity.Identity, error) {
	id, err := identity.Parse(value)
	if err != nil {
		return identity.Identity{}, aerr.WithContext(aerr.APERTURE_PROVIDER_REFERENCE_MISMATCH,
			fmt.Sprintf("provider: reference %s.%s holds %q, which is not a canonical identity", objectType, field, value),
			map[string]any{"object_type": objectType, "field": field, "target": target, "value": value})
	}
	if got := terminalType(id); got != target {
		return identity.Identity{}, aerr.WithContext(aerr.APERTURE_PROVIDER_REFERENCE_MISMATCH,
			fmt.Sprintf("provider: reference %s.%s is declared to hold %s identities, but %q is a %s",
				objectType, field, target, value, got),
			map[string]any{"object_type": objectType, "field": field, "target": target,
				"value": value, "value_object_type": got})
	}
	return id, nil
}
