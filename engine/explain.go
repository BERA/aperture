package engine

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/identity"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/rules"
	"github.com/frankbardon/aperture/scope"
)

// Trace is the structured, human-readable account of a single decision (FR-10).
// It is a STABLE PUBLIC CONTRACT: the Twirp surface (E4-S1), the MCP inspect
// tool (E4-S3), and the what-if simulator (E6-S4) serialize it, so its fields
// and their meaning are part of the API.
//
// A Trace records the whole derivation, not just the verdict: the expanded
// subject set the grants were resolved against, every grant considered with its
// per-grant outcome (action match, scope-resolver/coverage result, specificity),
// which grants decided the verdict, and the final Decision. String renders it as
// an operator-readable report.
type Trace struct {
	// Request is the question that was asked (account, principal, action, object).
	Request Request
	// Subjects is the principal's expanded subject set — itself, its roles, and
	// its groups — the set grants were resolved against.
	Subjects []model.Subject
	// Considered is every grant loaded for the subject set, in storage order,
	// each tagged with how it fared. A grant that fails the action match is still
	// listed (with ActionMatched false) so the trace shows what was ruled out.
	Considered []GrantEvaluation
	// MaxSpecificity is the top specificity among the covering candidates, the
	// tier the deny-overrides tiebreak resolved at. Zero when nothing covered.
	MaxSpecificity int
	// Notes are the diagnostic observations rule evaluation recorded while
	// resolving this decision — today, deny-safe shape mismatches and matches
	// produced by an absent field. They explain a verdict that is otherwise
	// silent: a rule that evaluated false because a metadata field was the wrong
	// shape looks identical to one that simply did not match. Empty on any
	// decision that evaluated no rule.
	//
	// Notes are DIAGNOSTIC ONLY: they never influence the verdict, and Check /
	// Enumerate do not collect them at all.
	Notes []EvaluationNote
	// Decision is the final verdict, reason, and deciding grant ids — identical
	// to what Check returns for the same request.
	Decision Decision
	// Impersonation, when non-nil, records that the trace was resolved under an
	// ACTIVE impersonation session. Subjects above is then the EFFECTIVE subject
	// set (the target's, or the operator∪target union for augment), while
	// Request.Principal remains the real operator — so the trace shows both who
	// asked and whose authority answered. Nil on the non-impersonated path.
	Impersonation *ImpersonationContext
}

// GrantEvaluation is one grant's contribution to a decision: what it is, whether
// its permission's action matched, whether it covered the object (and at what
// specificity, via which scope strategy), and whether it was a deciding grant.
type GrantEvaluation struct {
	// GrantID is the grant's id.
	GrantID string
	// Subject is the grant's subject (principal, role, or group).
	Subject model.Subject
	// PermissionID is the grant's permission reference.
	PermissionID string
	// Effect is the grant's polarity (allow or deny).
	Effect model.Effect
	// ObjectPattern is the grant's object pattern (string form).
	ObjectPattern string
	// Action is the resolved permission's action, or "" when the permission is
	// missing (a dangling grant, which is inert).
	Action string
	// Strategy is the scope strategy the permission selects ("literal" by
	// default), the resolver consulted for coverage.
	Strategy string
	// ActionMatched reports whether the permission's action equals the request's.
	ActionMatched bool
	// Covered reports whether the grant's object set (resolved through its scope
	// strategy) contains the requested object.
	Covered bool
	// Specificity is the grant pattern's specificity, meaningful when Covered.
	Specificity int
	// Deciding reports whether this grant is among the ones that produced the
	// verdict (top specificity, winning effect).
	Deciding bool
	// Outcome is a short human-readable note on this grant's disposition.
	Outcome string
}

// EvaluationNote is one diagnostic observation a rule evaluation recorded while
// a grant's scope was being resolved, tied back to the grant that triggered it.
//
// It is the decision-surface projection of rules.Note: a flat, self-describing
// record every surface can serialize (the Twirp trace JSON and the MCP explain
// tool both carry Trace verbatim) without importing the rules package's types.
//
// SHAPE AND PATH ONLY. A note names the variable path, the shape expected and the
// shape found. It NEVER carries a metadata value, an object id, or anything else
// that could leak data across accounts — Explain output crosses account
// boundaries the same way an error message does.
type EvaluationNote struct {
	// GrantID is the grant whose scope resolution recorded the note.
	GrantID string
	// Rule is the rule reference that was evaluated.
	Rule string
	// Kind classifies the observation ("shape_mismatch", "absent_field").
	Kind string
	// Op is the comparison operator that made the observation ("hasAll", ...).
	Op string
	// Path is the dotted variable path of the operand ("object.tags"), or empty
	// when the operand was not a plain variable reference.
	Path string
	// Expected is the shape the operator requires ("collection", "array", ...).
	Expected string
	// Actual is the shape actually found ("string", "number", "absent", ...).
	Actual string
	// Message is the one-line rendering surfaces print.
	Message string
}

// evaluationNotes projects the rules package's notes onto the trace's own type,
// stamping each with the grant whose resolution produced it.
func evaluationNotes(grantID string, notes []rules.Note) []EvaluationNote {
	if len(notes) == 0 {
		return nil
	}
	out := make([]EvaluationNote, len(notes))
	for i, n := range notes {
		out[i] = EvaluationNote{
			GrantID:  grantID,
			Rule:     n.Rule,
			Kind:     string(n.Kind),
			Op:       n.Op,
			Path:     n.Path,
			Expected: n.Expected,
			Actual:   n.Actual,
			Message:  n.String(),
		}
	}
	return out
}

// Explain resolves the request exactly as Check does but records the full
// derivation instead of only the verdict, returning a Trace. The same
// operational errors Check raises (bad request, unknown principal, storage
// fault, unresolvable strategy) surface here too; Explain is a diagnostic, so an
// error is returned rather than rendered into the trace.
func (e *Engine) Explain(ctx context.Context, req Request) (Trace, error) {
	if err := validateRequest(req); err != nil {
		return Trace{}, err
	}
	object, err := identity.Parse(req.Object)
	if err != nil {
		return Trace{}, err
	}

	member, err := e.requireMembership(ctx, req.Account, req.Principal)
	if err != nil {
		return Trace{}, err
	}
	if !member {
		// Fail-closed: the denial precedes grant evaluation, so the trace records
		// the membership verdict and considers no grants.
		return Trace{Request: req, Decision: nonMemberDeny(req), Considered: []GrantEvaluation{}}, nil
	}

	subjects, err := e.subjectSet(ctx, req.Principal)
	if err != nil {
		return Trace{}, err
	}
	return e.explainWithSubjects(ctx, req, object, subjects)
}

// explainWithSubjects builds a Trace over an already-resolved subject set. It is
// shared by Explain (the principal's own subject set) and ExplainAs (the
// impersonation-elevated set), so an impersonated trace records the same
// derivation against a different subject set. req.Principal stays the requesting
// principal in the trace's Request; the caller attaches any impersonation context.
func (e *Engine) explainWithSubjects(ctx context.Context, req Request, object identity.Identity, subjects []model.Subject) (Trace, error) {
	grants, err := e.store.GrantsForSubjects(ctx, req.Account, subjects)
	if err != nil {
		return Trace{}, aerr.Wrap(aerr.APERTURE_STORAGE,
			"engine: failed to load grants for subjects", err)
	}

	permCache := make(map[string]*model.Permission, len(grants))
	tr := Trace{Request: req, Subjects: subjects, Considered: make([]GrantEvaluation, 0, len(grants))}
	candidates := make([]candidate, 0, len(grants))

	for _, g := range grants {
		ev := GrantEvaluation{
			GrantID:       g.ID,
			Subject:       g.Subject,
			PermissionID:  g.PermissionID,
			Effect:        g.Effect,
			ObjectPattern: g.Object,
		}
		ok, err := e.actionMatches(ctx, g, req.Action, permCache)
		if err != nil {
			return Trace{}, err
		}
		perm := permCache[g.PermissionID]
		if perm != nil {
			ev.Action = perm.Action
			ev.Strategy = strategyOf(perm)
		} else {
			ev.Outcome = "inert: the grant's permission no longer exists"
			tr.Considered = append(tr.Considered, ev)
			continue
		}
		ev.ActionMatched = ok
		if !ok {
			ev.Outcome = fmt.Sprintf("action %q does not match the requested %q", perm.Action, req.Action)
			tr.Considered = append(tr.Considered, ev)
			continue
		}
		// Explain — and only Explain — installs an evaluation-notes collector, so
		// a rule-backed scope resolver's diagnostics travel back up through the
		// scope seam without widening any of its interfaces. One collector per
		// grant is what ties each note to the grant that produced it.
		noteCtx, collector := rules.WithNoteCollector(ctx)
		covered, spec, err := e.coverer.cover(noteCtx, req, g, perm, object)
		if err != nil {
			return Trace{}, err
		}
		tr.Notes = append(tr.Notes, evaluationNotes(g.ID, collector.Notes())...)
		ev.Covered = covered
		ev.Specificity = spec
		if covered {
			ev.Outcome = fmt.Sprintf("%s covers the object via %s scope at specificity %d", g.Effect, ev.Strategy, spec)
			candidates = append(candidates, candidate{grant: g, specificity: spec})
		} else {
			ev.Outcome = fmt.Sprintf("%s scope does not cover the object", ev.Strategy)
		}
		tr.Considered = append(tr.Considered, ev)
	}

	tr.Decision = decide(req, candidates)
	tr.MaxSpecificity = topSpecificity(candidates)

	deciding := make(map[string]struct{}, len(tr.Decision.DecidingGrantIDs))
	for _, id := range tr.Decision.DecidingGrantIDs {
		deciding[id] = struct{}{}
	}
	for i := range tr.Considered {
		if _, ok := deciding[tr.Considered[i].GrantID]; ok {
			tr.Considered[i].Deciding = true
		}
	}
	return tr, nil
}

// topSpecificity returns the highest specificity among covering candidates, or 0.
func topSpecificity(candidates []candidate) int {
	max := 0
	for _, c := range candidates {
		if c.specificity > max {
			max = c.specificity
		}
	}
	return max
}

// strategyOf returns the scope strategy key a permission selects. An empty or
// unparseable reference renders as "literal" / the raw reference rather than
// failing the trace, since Explain is descriptive.
func strategyOf(perm *model.Permission) string {
	if perm == nil {
		return ""
	}
	spec, err := scope.ParseSpec(perm.ScopeStrategy)
	if err != nil {
		return perm.ScopeStrategy
	}
	return spec.Strategy
}

// String renders the trace as an operator-readable, DETERMINISTIC report: the
// question, the subject set, each grant's disposition, and the verdict. Two
// String calls on two traces of the same decision produce byte-identical output,
// so a trace can be diffed, snapshotted, or pasted into a bug report.
//
// That is not free, and it is why the three lists below are sorted here rather
// than assumed ordered. Subjects come from Storage.GroupsForPrincipal and the
// considered grants from Storage.GrantsForSubjects, neither of which promises an
// order — storage/memory iterates a Go map, so the line order genuinely varied
// run to run. The fix belongs here because the ORDER IS A RENDERING CONCERN:
// Storage keeps its freedom, Trace keeps its documented storage order in the
// struct (which the Twirp and MCP surfaces serialize verbatim), and only the
// report is normalised. Sorting is done on copies for exactly that reason —
// calling String must not reorder the caller's Trace.
func (t Trace) String() string {
	var b strings.Builder
	verdict := "DENY"
	if t.Decision.Allow {
		verdict = "ALLOW"
	}
	fmt.Fprintf(&b, "Explain %s/%s on %s in account %s\n",
		t.Request.Principal, t.Request.Action, t.Request.Object, t.Request.Account)

	// "kind:id" is the subject's whole identity, so sorting the rendered form is
	// a total order: two subjects that compare equal ARE the same subject.
	subjects := make([]string, len(t.Subjects))
	for i, s := range t.Subjects {
		subjects[i] = string(s.Kind) + ":" + s.ID
	}
	sort.Strings(subjects)
	fmt.Fprintf(&b, "  subjects: %s\n", strings.Join(subjects, ", "))

	fmt.Fprintf(&b, "  grants considered (%d):\n", len(t.Considered))
	for _, ev := range sortedEvaluations(t.Considered) {
		marker := " "
		if ev.Deciding {
			marker = "*"
		}
		fmt.Fprintf(&b, "   %s %s [%s %s] %s\n", marker, ev.GrantID, ev.Effect, ev.ObjectPattern, ev.Outcome)
	}
	// Evaluation notes come after the grants and before the verdict: they explain
	// how a grant's rule behaved, which is what an operator reads next when a
	// covering grant unexpectedly did not cover.
	if len(t.Notes) > 0 {
		fmt.Fprintf(&b, "  evaluation notes (%d):\n", len(t.Notes))
		for _, n := range sortedNotes(t.Notes) {
			fmt.Fprintf(&b, "     %s [rule %s]: %s\n", n.GrantID, n.Rule, n.Message)
		}
	}

	fmt.Fprintf(&b, "  verdict: %s (top specificity %d)\n", verdict, t.MaxSpecificity)
	fmt.Fprintf(&b, "  reason: %s\n", t.Decision.Reason)
	return b.String()
}

// sortedEvaluations returns a copy of the considered grants in a total order.
//
// GrantID alone would do — a grant is loaded at most once per decision, so no
// two evaluations in one trace can share an id — but leaning on that would make
// the report's determinism depend on a storage invariant rather than on this
// function. The key therefore continues through every remaining field, which
// makes ties possible only between records that are equal in full and so render
// identically either way. Deciding is not compared for the same reason: it is
// derived from GrantID, so equal ids already imply equal markers.
func sortedEvaluations(in []GrantEvaluation) []GrantEvaluation {
	out := slices.Clone(in)
	slices.SortFunc(out, func(a, b GrantEvaluation) int {
		return cmp.Or(
			strings.Compare(a.GrantID, b.GrantID),
			strings.Compare(string(a.Subject.Kind), string(b.Subject.Kind)),
			strings.Compare(a.Subject.ID, b.Subject.ID),
			strings.Compare(a.PermissionID, b.PermissionID),
			strings.Compare(string(a.Effect), string(b.Effect)),
			strings.Compare(a.ObjectPattern, b.ObjectPattern),
			strings.Compare(a.Action, b.Action),
			strings.Compare(a.Strategy, b.Strategy),
			strings.Compare(a.Outcome, b.Outcome),
		)
	})
	return out
}

// sortedNotes returns a copy of the evaluation notes in a total order.
//
// Notes inherit the considered grants' order, so they were nondeterministic for
// the same reason. The key spans EVERY field of EvaluationNote — a note carries
// no id of its own, and one grant can legitimately record several — so records
// that tie are identical records and render the same line.
func sortedNotes(in []EvaluationNote) []EvaluationNote {
	out := slices.Clone(in)
	slices.SortFunc(out, func(a, b EvaluationNote) int {
		return cmp.Or(
			strings.Compare(a.GrantID, b.GrantID),
			strings.Compare(a.Rule, b.Rule),
			strings.Compare(a.Kind, b.Kind),
			strings.Compare(a.Op, b.Op),
			strings.Compare(a.Path, b.Path),
			strings.Compare(a.Expected, b.Expected),
			strings.Compare(a.Actual, b.Actual),
			strings.Compare(a.Message, b.Message),
		)
	})
	return out
}
