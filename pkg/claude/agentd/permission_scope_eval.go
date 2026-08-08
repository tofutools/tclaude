package agentd

import (
	"encoding/json"
	"log/slog"
	"sort"
	"strings"
)

// ActionContext describes WHAT a gated request is about, in the typed
// vocabulary the permission registry scopes grants by. It is the other half
// of a scoped grant: the grant says "only for spawn_profile p1", the gate
// site says "this request spawns with p1", and the resolver's winning scope
// is evaluated against it.
//
// One field per ScopeDim, and nothing else — this is deliberately not a
// general fact bag. A new dimension adds a field here and a case in value();
// TestActionContextCoversEveryScopeDimension fails if the two drift.
//
// The zero value means "the call site said nothing about this action", which
// is exactly how the ~129 gate sites that pass no context behave. A scoped
// grant evaluated against it is never satisfied — see permissionScopeSatisfied.
type ActionContext struct {
	// Group is the target group NAME (the identifier group grants and the
	// dashboard use), not the numeric group id.
	Group string
	// TargetAgent is the agent the action acts upon. Phase 2 has no
	// production caller for this dimension; Phase 5 defines the identifier
	// form alongside the lineage table and the @descendants selector.
	TargetAgent string
	// SpawnProfile is the RESOLVED spawn profile name for a spawn — the
	// profile actually used after request/group-default resolution, never
	// the raw request field. Wired up in Phase 4.
	SpawnProfile string
	// ProcessTemplate is the stable, user-authored template id supplied as
	// templateId when creating a run. It deliberately is not a version ref:
	// grants remain useful when a template gets a new stored version.
	ProcessTemplate string
	// Remote is the normalized host/owner/repo key the proxy will contact.
	// URL scheme, credentials, port spelling and a trailing .git are absent.
	Remote string
}

// value projects the context onto one scope dimension. An unknown dimension
// answers "" — the fail-closed reading, since a build that cannot describe a
// dimension cannot claim to satisfy it either.
func (a ActionContext) value(dim ScopeDim) string {
	switch dim {
	case ScopeDimGroup:
		return a.Group
	case ScopeDimTargetAgent:
		return a.TargetAgent
	case ScopeDimSpawnProfile:
		return a.SpawnProfile
	case ScopeDimProcessTemplate:
		return a.ProcessTemplate
	case ScopeDimRemote:
		return a.Remote
	}
	return ""
}

// actionContextOf reads the optional variadic context the gates accept. The
// variadic shape is what keeps every existing call site compiling untouched;
// callers pass at most one, and a second is ignored rather than merged.
func actionContextOf(actx []ActionContext) ActionContext {
	if len(actx) == 0 {
		return ActionContext{}
	}
	return actx[0]
}

// permScopeEval is the outcome of evaluating one verdict's winning tier
// against an ActionContext.
type permScopeEval struct {
	// Unscoped: the winning tier imposes no constraint, so the decision is
	// byte-for-byte the pre-scope decision. Every grant that exists today is
	// unscoped, which is why Phase 2 changes no existing behaviour.
	Unscoped bool
	// Satisfied: the action may proceed as far as scope is concerned. Always
	// true when Unscoped.
	Satisfied bool
	// Matched renders the scope that authorized the action, for the audit
	// row. Empty when Unscoped (there is no scope to record) or unsatisfied.
	Matched string
}

// evalPermissionScope evaluates the winning tier's scopes (§5) against actx.
//
// Union within the tier: an agent whose two groups grant the same slug with
// different scopes may act within either. A single UNSCOPED row absorbs the
// tier — union with unscoped is unscoped — which is why an unscoped row wins
// wherever it appears in the list rather than only when it sorts first.
//
// Only the winning tier is consulted. A per-agent override that allows with
// scope S applies S even when a group grant below is broader: that is what
// lets an operator narrow one agent without touching its group.
//
// callerConvID is the acting agent, needed by relational @selectors.
func evalPermissionScope(v permVerdict, callerConvID string, actx ActionContext) permScopeEval {
	if len(v.ScopeJSON) == 0 {
		return permScopeEval{Unscoped: true, Satisfied: true}
	}
	// Deterministic order so a tier with several matching scopes always
	// records the same one on the audit row.
	raws := append([]string(nil), v.ScopeJSON...)
	sort.Strings(raws)
	out := permScopeEval{}
	for _, raw := range raws {
		scope, err := permissionScopeForEval(raw)
		if err != nil {
			// A row this build cannot decode can never authorize anything.
			// It must NOT read as unscoped: a scope naming a dimension only a
			// newer daemon knows would then widen the grant to a wildcard,
			// which is the exact inversion of what the operator wrote.
			slog.Warn("permissions: undecodable grant scope ignored (fails closed)",
				"source", string(v.Source), "error", err)
			continue
		}
		if len(scope) == 0 {
			return permScopeEval{Unscoped: true, Satisfied: true}
		}
		if !out.Satisfied && permissionScopeSatisfied(callerConvID, scope, actx) {
			out.Satisfied = true
			out.Matched = permissionScopeDisplay(scope)
		}
	}
	return out
}

// permissionScopeSatisfied applies §4's semantics to one scope: matchers
// within a dimension OR, dimensions AND, an absent dimension is
// unconstrained.
//
// A dimension the call site did not describe fails CLOSED. A scoped grant at
// an un-migrated gate site therefore degrades to "not decided here" — the
// ask-human popup, then 403 — never to a silent allow.
func permissionScopeSatisfied(callerConvID string, scope PermissionScope, actx ActionContext) bool {
	for dim, matchers := range scope {
		value := actx.value(dim)
		if value == "" {
			return false
		}
		matched := false
		for _, matcher := range matchers {
			if strings.HasPrefix(matcher, "@") {
				if permissionScopeSelectorMatches(callerConvID, dim, matcher, value) {
					matched = true
					break
				}
				continue
			}
			if permissionScopeLiteralMatches(dim, matcher, value) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

func permissionScopeLiteralMatches(dim ScopeDim, matcher, value string) bool {
	spec, ok := permissionScopeDimensions[dim]
	if !ok {
		return false
	}
	switch spec.matcher {
	case permissionScopeMatchExact:
		return matcher == value
	case permissionScopeMatchRemotePattern:
		pattern := strings.Split(strings.ToLower(strings.Trim(matcher, "/")), "/")
		target := strings.Split(strings.ToLower(strings.Trim(value, "/")), "/")
		return matchRemotePattern(pattern, target)
	default:
		return false
	}
}

// permissionScopeSelectorMatches evaluates a relational @selector matcher
// (@descendants / @self-spawned) for callerConvID against a concrete value.
//
// This is the Phase 5 seam. The selectors are parseable and persistable since
// Phase 1, but the parent→child spawn edges they range over do not exist in
// the schema yet, so every selector answers "no match" here. That direction is
// the only safe one: treating an unevaluable relational matcher as a match
// would turn a deliberately narrow grant into a wildcard.
//
// Phase 5 replaces this body with the bounded lineage walk and keeps the
// signature: dim identifies which dimension the selector was written on,
// value is the ActionContext projection being tested.
func permissionScopeSelectorMatches(callerConvID string, dim ScopeDim, selector, value string) bool {
	return false
}

// permissionScopeForEval decodes a persisted scope row for AUTHORIZATION.
//
// It differs from permissionScopeFromJSON (the display helper) in exactly one
// way, and it is the difference that matters: a value that does not decode is
// an error here, never a silent "unscoped". Empty and "{}" both mean unscoped
// per §4 and answer (nil, nil).
func permissionScopeForEval(raw string) (PermissionScope, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	scope, _, err := parsePermissionScope(json.RawMessage(raw))
	if err != nil {
		return nil, err
	}
	return scope, nil
}
