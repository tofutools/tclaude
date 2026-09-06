package agentd

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

type authorityPrincipalKind uint8

const (
	authorityPrincipalInvalid authorityPrincipalKind = iota
	authorityPrincipalOperator
	authorityPrincipalAgent
)

// spawnAuthorityPrincipal is supplied by an authenticated transport or a
// verified durable-automation adapter. It is never decoded from the action.
type spawnAuthorityPrincipal struct {
	Kind    authorityPrincipalKind
	AgentID string
	ConvID  string
}

type spawnAuthorityOriginKind uint8

const (
	spawnAuthorityOriginInvalid spawnAuthorityOriginKind = iota
	spawnAuthorityOriginHTTP
	spawnAuthorityOriginTrigger
	spawnAuthorityOriginCron
)

// spawnAuthorityOrigin describes causation, not authority. Its closed shape
// prevents a caller from manufacturing a new principal category through an
// origin label.
type spawnAuthorityOrigin struct {
	Kind        spawnAuthorityOriginKind
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
	SudoGrantID int64
}

type spawnAuthorityDecision struct {
	Outcome         spawnAuthorityOutcome
	Principal       spawnAuthorityPrincipal
	Origin          spawnAuthorityOrigin
	Action          ActionContext
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

// FallbackSlug chooses the approval vocabulary from the already-captured
// alternatives. It deliberately performs no further standing-policy read.
func (d spawnAuthorityDecision) FallbackSlug() string {
	if alt, ok := d.Alternatives[PermAgentSpawn]; ok && alt.Resolution != permUndecided {
		return PermAgentSpawn
	}
	return PermGroupsMembersSpawn
}

type spawnAuthorityFactsSnapshot struct {
	PrincipalVerified bool
	Sources           permSources
	Defaults          map[string]bool
}

// spawnAuthorityFactReader is the evaluator's single capture boundary. A
// production read may comprise several database queries; this interface does
// not claim database-wide atomicity. It guarantees only that one decision and
// its counterfactuals consume one returned set of facts without rereading.
type spawnAuthorityFactReader interface {
	Read(context.Context, spawnAuthorityPrincipal, permissionReadPolicy) (spawnAuthorityFactsSnapshot, error)
}

type defaultSpawnAuthorityFactReader struct{}

func (defaultSpawnAuthorityFactReader) Read(_ context.Context, principal spawnAuthorityPrincipal, policy permissionReadPolicy) (spawnAuthorityFactsSnapshot, error) {
	agentRow, err := db.GetAgent(principal.AgentID)
	if err != nil {
		return spawnAuthorityFactsSnapshot{}, fmt.Errorf("read authority principal: %w", err)
	}
	if !agentRow.Active() || agentRow.CurrentConvID != principal.ConvID {
		return spawnAuthorityFactsSnapshot{}, nil
	}
	mappedAgentID, err := db.AgentIDForConv(principal.ConvID)
	if err != nil {
		return spawnAuthorityFactsSnapshot{}, fmt.Errorf("read authority conversation association: %w", err)
	}
	if mappedAgentID != principal.AgentID {
		return spawnAuthorityFactsSnapshot{}, nil
	}
	src, err := loadPermSourcesWithReadPolicy(principal.ConvID, policy)
	if err != nil {
		return spawnAuthorityFactsSnapshot{}, err
	}
	cfg, err := config.Load()
	if err != nil {
		return spawnAuthorityFactsSnapshot{}, err
	}
	defaults := map[string]bool{
		PermAgentSpawn:         cfg.HasDefaultPermission(PermAgentSpawn),
		PermGroupsMembersSpawn: cfg.HasDefaultPermission(PermGroupsMembersSpawn),
		PermGroupsAdmin:        cfg.HasDefaultPermission(PermGroupsAdmin),
	}
	return spawnAuthorityFactsSnapshot{PrincipalVerified: true, Sources: src, Defaults: defaults}, nil
}

var spawnAuthorityFacts spawnAuthorityFactReader = defaultSpawnAuthorityFactReader{}

type spawnAuthorityEvaluator struct {
	reader spawnAuthorityFactReader
	now    func() time.Time
}

func newSpawnAuthorityEvaluator() spawnAuthorityEvaluator {
	return spawnAuthorityEvaluator{reader: spawnAuthorityFacts, now: time.Now}
}

// EvaluateSpawn evaluates both standing spawn alternatives from one captured
// fact read. The returned decision is evidence for this operation only;
// callers must re-evaluate after queueing or any other freshness boundary.
func (e spawnAuthorityEvaluator) EvaluateSpawn(ctx context.Context, req spawnAuthorityRequest, readPolicy permissionReadPolicy) (spawnAuthorityDecision, error) {
	if e.reader == nil {
		e.reader = spawnAuthorityFacts
	}
	if e.now == nil {
		e.now = time.Now
	}
	dec := spawnAuthorityDecision{
		Outcome:      spawnAuthorityInvalid,
		Principal:    req.Principal,
		Origin:       req.Origin,
		Action:       req.Action,
		Alternatives: map[string]spawnAuthorityAlternative{},
		EvaluatedAt:  e.now(),
	}
	if !validSpawnAuthorityOrigin(req.Origin) || !validSpawnAuthorityPrincipalShape(req.Principal) {
		return dec, nil
	}
	if req.Principal.Kind == authorityPrincipalOperator {
		dec.Outcome = spawnAuthorityAllowed
		dec.AuthorizedSlug = "operator"
		dec.Source = permSourceOperator
		dec.AllowAnyGroup = true
		return dec, nil
	}

	facts, err := e.reader.Read(ctx, req.Principal, readPolicy)
	if err != nil {
		return spawnAuthorityDecision{}, err
	}
	if !facts.PrincipalVerified {
		return dec, nil
	}
	dec.Outcome = spawnAuthorityNotGranted
	dec.Diagnostics = append([]permissionReadDiagnostic(nil), facts.Sources.diagnostics...)
	if facts.Sources.ownerReadErr != nil {
		dec.Diagnostics = append(dec.Diagnostics, permissionReadDiagnostic{Tier: "owner", Err: facts.Sources.ownerReadErr})
	}
	owner := ownerImpliedTierFrom(facts.Sources.ownedGroups, facts.Sources.ownerReadErr)
	for _, slug := range []string{PermAgentSpawn, PermGroupsMembersSpawn} {
		alt := evaluateSpawnAlternative(facts.Sources, facts.Defaults, owner, req.Principal.ConvID, slug, req.Action)
		dec.Alternatives[slug] = alt
		if alt.Allowed && dec.Outcome != spawnAuthorityAllowed {
			dec.Outcome = spawnAuthorityAllowed
			dec.AuthorizedSlug = slug
			dec.Source = alt.Source
			dec.Matched = alt.Matched
			dec.MatchedDims = cloneMatchedDims(alt.MatchedDims)
			dec.SudoGrantID = alt.SudoGrantID
			dec.AllowAnyGroup = slug == PermAgentSpawn
		}
	}
	if dec.Outcome == spawnAuthorityAllowed && dec.SudoGrantID != 0 {
		withoutSudo := facts.Sources
		withoutSudo.sudo = map[string]sudoPermSource{}
		stillAllowed := false
		for _, slug := range []string{PermAgentSpawn, PermGroupsMembersSpawn} {
			if evaluateSpawnAlternative(withoutSudo, facts.Defaults, owner, req.Principal.ConvID, slug, req.Action).Allowed {
				stillAllowed = true
				break
			}
		}
		if !stillAllowed {
			dec.LoadBearingSudo = dec.SudoGrantID
		}
	}
	return dec, nil
}

func validSpawnAuthorityPrincipalShape(p spawnAuthorityPrincipal) bool {
	switch p.Kind {
	case authorityPrincipalOperator:
		return p.AgentID == "" && p.ConvID == ""
	case authorityPrincipalAgent:
		return strings.HasPrefix(p.AgentID, db.AgentIDPrefix) && len(p.AgentID) > len(db.AgentIDPrefix) && p.ConvID != ""
	default:
		return false
	}
}

func validSpawnAuthorityOrigin(o spawnAuthorityOrigin) bool {
	if o.ActionIndex < 0 {
		return false
	}
	switch o.Kind {
	case spawnAuthorityOriginHTTP:
		return o.RuleID == 0 && o.FiringID == 0 && o.CronJobID == 0 && o.CronRunID == 0 && o.ActionIndex == 0
	case spawnAuthorityOriginTrigger:
		return o.RuleID > 0 && o.FiringID > 0 && o.CronJobID == 0 && o.CronRunID == 0
	case spawnAuthorityOriginCron:
		return o.RuleID > 0 && o.FiringID == 0 && o.CronJobID > 0 && o.CronRunID > 0
	default:
		return false
	}
}

func evaluateSpawnAlternative(src permSources, defaults map[string]bool, owner ownerImpliedTier, convID, slug string, action ActionContext) spawnAuthorityAlternative {
	v := resolveEffectivePermissionVerdictFrom(src, slug, defaults[slug], defaults[PermGroupsAdmin])
	scopeEval := evalPermissionScope(v, convID, action)
	alt := spawnAuthorityAlternative{
		Slug: slug, Resolution: v.Resolution, Source: v.Source,
		Matched: scopeEval.Matched, MatchedDims: cloneMatchedDims(scopeEval.MatchedDims),
	}
	if v.Resolution == permAllow && scopeEval.Satisfied {
		alt.Allowed = true
		alt.SudoGrantID = v.SudoGrantID
		return alt
	}
	if v.Resolution == permDeny {
		return alt
	}
	if matched, dims, ok := ownerSpawnEvidence(owner[slug], convID, action); ok {
		alt.Allowed = true
		alt.Resolution = permAllow
		alt.Source = permSourceOwner
		alt.Matched = matched
		alt.MatchedDims = dims
		alt.SudoGrantID = 0
	}
	return alt
}

func ownerSpawnEvidence(entry ownerTierEntry, convID string, action ActionContext) (string, map[ScopeDim]bool, bool) {
	if entry.Unrestricted {
		return "", nil, true
	}
	for _, scope := range entry.Scopes {
		if permissionScopeSatisfied(convID, scope, action) {
			dims := make(map[ScopeDim]bool, len(scope))
			for dim := range scope {
				dims[dim] = true
			}
			return permissionScopeDisplay(scope), dims, true
		}
	}
	return "", nil, false
}

func cloneMatchedDims(in map[ScopeDim]bool) map[ScopeDim]bool {
	if len(in) == 0 {
		return nil
	}
	out := make(map[ScopeDim]bool, len(in))
	for dim, matched := range in {
		out[dim] = matched
	}
	return out
}

func (d spawnAuthorityDecision) Error() error {
	if d.Outcome == spawnAuthorityInvalid {
		return fmt.Errorf("invalid spawn authority principal or origin")
	}
	if d.Outcome != spawnAuthorityAllowed {
		return fmt.Errorf("spawn authority not granted")
	}
	return nil
}
