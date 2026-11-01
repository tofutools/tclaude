package agentd

import (
	"context"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// A use-limited sudo grant is spent at the permission gates. The gate reserves a
// use before the action, so parallel requests cannot pass the limit, and the
// meter around the request gives it back unless the request succeeded. The rule
// that keeps this honest is in the resolvers: a limited grant only authorizes a
// request that carries a meter (see sudoSourcesForRequest). Anything else, such
// as a trigger, a schedule or a context-free check, has nothing to count with
// and is refused rather than let the limit be walked past.

// countableSudoSlugs are the slugs a sudo grant can limit by uses. Each names a
// discrete action behind requirePermission, requireGroupPermission or
// requireCrossAgentPermission, which count it. A slug that should not be here
// says why in sudoUseLimitRefusal.
var countableSudoSlugs = []string{
	PermAgentSpawn, PermGroupsMembersSpawn,

	PermSelfRename, PermSelfCompact, PermSelfInterrupt, PermSelfClone, PermSelfMorph,
	PermSelfRemoteControl, PermSelfTask, PermSelfPR, PermSelfTags, PermSelfDirRepair, PermSelfProcess,

	PermAgentStop, PermAgentResume, PermAgentRetire, PermAgentDelete, PermAgentRename, PermAgentCompact,
	PermAgentInterrupt, PermAgentClone, PermAgentMorph, PermAgentPromote, PermAgentReincarnate,
	PermAgentRemoteControl, PermAgentTask, PermAgentTags, PermAgentPR, PermAgentProcess,
	PermAgentSandboxImplementation,

	PermGroupsMembersStop, PermGroupsMembersResume, PermGroupsMembersRetire, PermGroupsMembersDelete,
	PermGroupsMembersRename, PermGroupsMembersCompact, PermGroupsMembersInterrupt, PermGroupsMembersClone,
	PermGroupsMembersMorph, PermGroupsMembersPromote, PermGroupsMembersReincarnate,
	PermGroupsMembersRemoteControl, PermGroupsMembersTask, PermGroupsMembersTags, PermGroupsMembersPR,
	PermGroupsMembersProcess, PermGroupsMembersAdd, PermGroupsMembersRemove, PermGroupsMembersUpdate,

	PermGroupsCreate, PermGroupsDelete, PermGroupsRename, PermGroupsClone, PermGroupsArchive, PermGroupsNest,
	PermGroupsAttachment, PermGroupsImport, PermGroupsOwnersManage, PermGroupsLinkAdd, PermGroupsLinkRemove,

	// One save spends one use of each slug it changes: each is its own grant.
	PermGroupsSettingsDescription, PermGroupsSettingsDefaultDir, PermGroupsSettingsDefaultContext,
	PermGroupsSettingsEnvironment, PermGroupsSettingsDefaultSpawnTarget, PermGroupsSettingsDefaultProfile,
	PermGroupsSettingsMaxMembers, PermGroupsSettingsNotifications, PermGroupsSettingsRemoteControlPolicy,
	PermGroupsSettingsMemberPermissions, PermGroupsSettingsOwnerScopes,

	PermMessageDirect, PermHumanNotify, PermHumanClipboard, PermDashboardFormsCapture,

	PermTemplatesManage, PermTemplatesUse, PermProfilesManage, PermRolesManage, PermPromptsManage,
	PermProcessTemplatesManage, PermProcessRunsStart, PermProcessRunsManage, PermProcessAdvance,
	PermUsageRulesManage, PermSettingsDefaultModel,
}

// handlerCountedSudoSlugs count each spawn in the spawn handlers instead: one
// request can launch several agents, and only a launch attempt spends a use.
var handlerCountedSudoSlugs = []string{PermAgentSpawn, PermGroupsMembersSpawn}

// laterWorkSudoSlugs set up work that fires when no request is there to count it.
var laterWorkSudoSlugs = []string{PermSelfSchedule, PermAgentSchedule, PermGroupsMembersSchedule,
	PermGroupsMessagesSchedule, PermTriggersManage, PermGroupsTriggersManage}

// chattySudoSlugs are checked again and again within one task, so a count would
// not mean anything a person asked for.
var chattySudoSlugs = []string{PermBoardsWrite, PermDashboardMark, PermDashboardGuide, PermDashboardCapture, PermKnowledgeRead,
	PermHTTP, PermModelsProxy, PermSessionsAttach, PermSessionsShell, PermSessionsTakeover, PermSessionsWatch,
	PermRoutesConsume, PermRoutesPublish, PermRoutesPublishHost}

// peerSudoSlugs are spent across a peer link, where this node's count does not reach.
var peerSudoSlugs = []string{PermGroupsSettings, PermAgentMove, PermSelfTeleport, PermSelfTeleportPermissions, PermJobsRun}

// sudoUseLimitRefusal says why a slug takes no use limit, and is empty when it
// can have one. The editor shows it on the row.
func sudoUseLimitRefusal(slug string) string {
	switch {
	case slices.Contains(countableSudoSlugs, slug):
		return ""
	case strings.HasPrefix(slug, PermGroupsMembersSpawn+"."):
		return "It is a choice within a spawn. Limit agent.spawn or groups.members.spawn instead."
	case slug == PermGroupsAdmin:
		return "It covers many actions at once. Limit the specific permissions instead."
	case slices.Contains(laterWorkSudoSlugs, slug):
		return "It sets up work that runs later, when there is no request to count."
	case slices.Contains(chattySudoSlugs, slug), strings.HasPrefix(slug, "proxy."):
		return "One task checks it many times, so a count would not mean one action."
	case slices.Contains(peerSudoSlugs, slug), strings.HasPrefix(slug, "node."):
		return "It acts across a peer link, where uses are not counted."
	case strings.HasSuffix(slug, ".read"), strings.HasSuffix(slug, ".export"), strings.HasSuffix(slug, "-read"):
		return "It only reads, so there is no action to count."
	default:
		return "It has no single action to count."
	}
}

// sudoUseMeter is the uses one request reserved, one per slug however many
// times the handler passes the gate: one request is one action. held keeps each
// grant readable after its last use is reserved, because a used-up grant drops
// out of the active list and later checks in the same request must still see it.
// seen is every limited grant this request's checks read: a gate spends from it
// rather than reading again, because a parallel request may have taken the last
// use in between, and a grant gone from a fresh read must not look like no limit.
type sudoUseMeter struct {
	mu   sync.Mutex
	held map[string]sudoPermSource
	seen map[string]sudoPermSource
	// kept holds the uses even if the request fails: see keepSudoUses.
	kept bool
}

type sudoUseMeterKey struct{}

func sudoUseMeterFrom(r *http.Request) *sudoUseMeter {
	meter, _ := r.Context().Value(sudoUseMeterKey{}).(*sudoUseMeter)
	return meter
}

func withSudoUseMeter(r *http.Request) (*http.Request, *sudoUseMeter) {
	meter := &sudoUseMeter{held: map[string]sudoPermSource{}, seen: map[string]sudoPermSource{}}
	return r.WithContext(context.WithValue(r.Context(), sudoUseMeterKey{}, meter)), meter
}

// countSudoUses puts a meter on every request and settles it afterwards.
// Anything but a finished success gives the uses back.
func countSudoUses(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r, meter := withSudoUseMeter(r)
		rec := &statusRec{ResponseWriter: w, code: http.StatusOK}
		finished := false
		defer func() { meter.settle(finished && rec.code < http.StatusBadRequest) }()
		h.ServeHTTP(rec, r)
		finished = true
	})
}

func (m *sudoUseMeter) settle(keep bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if keep || m.kept {
		return
	}
	for slug, grant := range m.held {
		if err := db.ReturnSudoUse(grant.ID); err != nil {
			slog.Warn("sudo: return unused use failed", "grant", grant.ID, "slug", slug, "error", err)
		}
	}
}

// take reserves a use once per slug. It answers the request and returns false
// when none is left.
func (m *sudoUseMeter) take(w http.ResponseWriter, slug string, grant sudoPermSource) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.held[slug]; ok {
		return true
	}
	use, ok := takeSudoUse(w, grant.ID)
	if !ok {
		return false
	}
	if use.grantID != 0 {
		m.held[slug] = grant
	}
	return true
}

// giveBack returns what the request reserved so far. A handler calls it through
// giveBackSudoUses on a branch that turns out only to look, such as a dry run.
func (m *sudoUseMeter) giveBack() {
	m.settle(false)
	m.mu.Lock()
	defer m.mu.Unlock()
	clear(m.held)
}

// keepSudoUses is for a handler that has changed something and may still fail:
// a failure then must not hand back the uses that paid for the change.
func keepSudoUses(r *http.Request) {
	if meter := sudoUseMeterFrom(r); meter != nil {
		meter.mu.Lock()
		defer meter.mu.Unlock()
		meter.kept = true
	}
}

func giveBackSudoUses(r *http.Request) {
	if meter := sudoUseMeterFrom(r); meter != nil {
		meter.giveBack()
	}
}

// spentSudoUse reports whether the request has reserved a use, for an action
// that continues somewhere it cannot be counted.
func spentSudoUse(r *http.Request) bool {
	meter := sudoUseMeterFrom(r)
	if meter == nil {
		return false
	}
	meter.mu.Lock()
	defer meter.mu.Unlock()
	return len(meter.held) > 0
}

// sudoSourcesForRequest applies the rule that a limited grant needs a meter:
// without one it is removed, and with one it is noted and the grants already
// reserved are put back.
func sudoSourcesForRequest(r *http.Request, src permSources) permSources {
	meter := sudoUseMeterFrom(r)
	if meter == nil {
		return withoutLimitedSudo(src)
	}
	meter.mu.Lock()
	defer meter.mu.Unlock()
	for slug, grant := range src.sudo {
		if grant.Limited {
			meter.seen[slug] = grant
		}
	}
	for slug, grant := range meter.held {
		if _, ok := src.sudo[slug]; !ok {
			src.sudo[slug] = grant
		}
	}
	return src
}

func withoutLimitedSudo(src permSources) permSources {
	kept := make(map[string]sudoPermSource, len(src.sudo))
	for slug, grant := range src.sudo {
		if !grant.Limited {
			kept[slug] = grant
		}
	}
	src.sudo = kept
	return src
}

// limitedSudoSource returns the limited grant this allowed action rests on, if
// any. A grant whose scope misses the action did not allow it, and anything else
// that covers the action, an unlimited sudo grant included, leaves it unspent.
func limitedSudoSource(r *http.Request, convID, perm string, actx ActionContext) (sudoPermSource, bool) {
	meter := sudoUseMeterFrom(r)
	if meter == nil {
		return sudoPermSource{}, false
	}
	for _, slug := range []string{perm, actx.alternatePermission} {
		meter.mu.Lock()
		grant, ok := meter.seen[slug]
		meter.mu.Unlock()
		if !ok {
			continue
		}
		verdict := permVerdict{Resolution: permAllow, Source: permSourceSudo, SudoGrantID: grant.ID, ScopeJSON: []string{grant.ScopeJSON}}
		if !actx.bulkGroupMemberCoverage {
			if allowed, _ := permissionVerdictAllowsAction(verdict, convID, slug, actx); !allowed {
				continue
			}
		}
		src := withoutLimitedSudo(loadPermSources(convID))
		if !permissionAllowedFrom(src, convID, slug, actx) {
			return grant, true
		}
	}
	return sudoPermSource{}, false
}

// standingSiblingCovers reports whether the group sibling of a cross-agent slug
// already covers every group of the target, so a grant of the global slug is not
// what allowed the action.
func standingSiblingCovers(convID, perm string, actx ActionContext) bool {
	sibling := GroupSiblingForSlug(perm)
	if sibling == "" {
		return false
	}
	groups := actx.affectedGroups
	if groups == nil {
		var err error
		if groups, err = activeGroupNamesForConvs(actx.targetConv); err != nil {
			return false
		}
	}
	for _, group := range groups {
		candidate := actx
		candidate.Group, candidate.structuralGroup = group, group
		if !standingPermissionAllows(convID, sibling, candidate) {
			return false
		}
	}
	return len(groups) > 0
}

// spendSudoUse is what a gate calls once it has allowed an action: it reserves
// a use when a limited grant is what allowed it. Reading spends nothing.
func spendSudoUse(w http.ResponseWriter, r *http.Request, convID, perm string, actx ActionContext) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead || slices.Contains(handlerCountedSudoSlugs, perm) {
		return true
	}
	grant, ok := limitedSudoSource(r, convID, perm, actx)
	if !ok {
		return true
	}
	return sudoUseMeterFrom(r).take(w, grant.Slug, grant)
}
