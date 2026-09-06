package agentd

import (
	"context"
	"fmt"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

type authorityPrincipalKind uint8

const (
	authorityPrincipalInvalid authorityPrincipalKind = iota
	authorityPrincipalOperator
	authorityPrincipalAgent
)

// spawnAuthorityPrincipal is supplied by a transport or durable automation
// adapter. It is never decoded from the action payload.
type spawnAuthorityPrincipal struct {
	Kind    authorityPrincipalKind
	AgentID string
	ConvID  string
}

type spawnAuthorityOrigin struct {
	Kind        string
	RuleID      int64
	FiringID    int64
	CronJobID   int64
	CronRunID   int64
	ActionIndex int
}

type spawnAuthorityRequest struct {
	Principal spawnAuthorityPrincipal
	Origin    spawnAuthorityOrigin
	Action    ActionContext
}

type spawnAuthorityOutcome uint8

const (
	spawnAuthorityInvalid spawnAuthorityOutcome = iota
	spawnAuthorityAllowed
	spawnAuthorityNotGranted
)

type spawnAuthorityAlternative struct {
	Slug        string
	Resolution  permResolution
	Allowed     bool
	Source      permSource
	Matched     string
	MatchedDims map[ScopeDim]bool
}

type spawnAuthorityDecision struct {
	Outcome         spawnAuthorityOutcome
	AuthorizedSlug  string
	Source          permSource
	Matched         string
	MatchedDims     map[ScopeDim]bool
	SudoGrantID     int64
	LoadBearingSudo int64
	AllowAnyGroup   bool
	Alternatives    map[string]spawnAuthorityAlternative
	Diagnostics     []permissionReadDiagnostic
	EvaluatedAt     time.Time
}

type spawnAuthorityEvaluator struct{}

type spawnAuthorityFactReader interface {
	Read(convID string, policy permissionReadPolicy) (permSources, map[string]bool, error)
}

type defaultSpawnAuthorityFactReader struct{}

func (defaultSpawnAuthorityFactReader) Read(convID string, policy permissionReadPolicy) (permSources, map[string]bool, error) {
	src, err := loadPermSourcesWithReadPolicy(convID, policy)
	if err != nil {
		return permSources{}, nil, err
	}
	cfg, err := config.Load()
	if err != nil {
		return src, nil, err
	}
	defaults := map[string]bool{PermAgentSpawn: cfg.HasDefaultPermission(PermAgentSpawn), PermGroupsMembersSpawn: cfg.HasDefaultPermission(PermGroupsMembersSpawn), PermGroupsAdmin: cfg.HasDefaultPermission(PermGroupsAdmin)}
	return src, defaults, nil
}

var spawnAuthorityFacts spawnAuthorityFactReader = defaultSpawnAuthorityFactReader{}

// EvaluateSpawn evaluates both standing spawn alternatives from one source
// snapshot. The returned decision is evidence for the current operation only;
// callers must re-evaluate after queueing or other freshness boundaries.
func (spawnAuthorityEvaluator) EvaluateSpawn(ctx context.Context, req spawnAuthorityRequest, readPolicy permissionReadPolicy) (spawnAuthorityDecision, error) {
	_ = ctx
	if req.Principal.Kind == authorityPrincipalInvalid || req.Principal.Kind == authorityPrincipalAgent && req.Principal.ConvID == "" {
		return spawnAuthorityDecision{Outcome: spawnAuthorityInvalid, EvaluatedAt: time.Now()}, nil
	}
	if req.Principal.Kind == authorityPrincipalOperator {
		return spawnAuthorityDecision{Outcome: spawnAuthorityAllowed, AuthorizedSlug: "operator", Source: permSourceDefault, AllowAnyGroup: true, EvaluatedAt: time.Now()}, nil
	}
	src, defaults, err := spawnAuthorityFacts.Read(req.Principal.ConvID, readPolicy)
	if err != nil {
		return spawnAuthorityDecision{}, err
	}
	dec := spawnAuthorityDecision{Outcome: spawnAuthorityNotGranted, Alternatives: map[string]spawnAuthorityAlternative{}, Diagnostics: src.diagnostics, EvaluatedAt: time.Now()}
	for _, slug := range []string{PermAgentSpawn, PermGroupsMembersSpawn} {
		v := resolveEffectivePermissionVerdictFrom(src, slug, defaults[slug], defaults[PermGroupsAdmin])
		eval := evalPermissionScope(v, req.Principal.ConvID, req.Action)
		allowed := v.Resolution == permAllow && eval.Satisfied
		matched := eval.Matched
		matchedDims := eval.MatchedDims
		if !allowed && v.Resolution != permDeny {
			owner := ownerImpliedTierFrom(src.ownedGroups, src.ownerReadErr)
			if owner.satisfiedBy(req.Principal.ConvID, slug, req.Action) {
				allowed, matched = true, "owner"
				v.Source = permSourceOwner
				if entry, ok := owner[slug]; ok {
					for _, scope := range entry.Scopes {
						if permissionScopeSatisfied(req.Principal.ConvID, scope, req.Action) {
							matched = permissionScopeDisplay(scope)
							matchedDims = make(map[ScopeDim]bool, len(scope))
							for dim := range scope {
								matchedDims[dim] = true
							}
							break
						}
					}
				}
			}
		}
		alt := spawnAuthorityAlternative{Slug: slug, Resolution: v.Resolution, Allowed: allowed, Source: v.Source, Matched: matched, MatchedDims: matchedDims}
		dec.Alternatives[slug] = alt
		if allowed && dec.Outcome != spawnAuthorityAllowed {
			dec.Outcome, dec.AuthorizedSlug, dec.Source, dec.Matched, dec.MatchedDims = spawnAuthorityAllowed, slug, v.Source, matched, matchedDims
			dec.SudoGrantID = v.SudoGrantID
			dec.AllowAnyGroup = slug == PermAgentSpawn
		}
	}
	if dec.Outcome == spawnAuthorityAllowed && dec.SudoGrantID != 0 {
		without := src
		without.sudo = map[string]sudoPermSource{}
		for _, slug := range []string{dec.AuthorizedSlug} {
			v := resolveEffectivePermissionVerdictFrom(without, slug, defaults[slug], defaults[PermGroupsAdmin])
			e := evalPermissionScope(v, req.Principal.ConvID, req.Action)
			ok := v.Resolution == permAllow && e.Satisfied
			if !ok && v.Resolution != permDeny {
				ok = ownerImpliedTierFrom(without.ownedGroups, without.ownerReadErr).satisfiedBy(req.Principal.ConvID, slug, req.Action)
			}
			if !ok {
				dec.LoadBearingSudo = dec.SudoGrantID
			}
		}
	}
	return dec, nil
}

func (d spawnAuthorityDecision) Error() error {
	if d.Outcome == spawnAuthorityInvalid {
		return fmt.Errorf("invalid spawn authority principal")
	}
	if d.Outcome != spawnAuthorityAllowed {
		return fmt.Errorf("spawn authority not granted")
	}
	return nil
}
