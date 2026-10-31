package agentd

import (
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// The Steward is a scribe with a place in the fleet: the operator sets its
// access in config, and agents can address it by that name.

const (
	stewardScopeKind = "steward"
	stewardGroupName = "steward"
	// stewardAddress is what agents write to reach the Steward, whatever its
	// generated name is.
	stewardAddress = "steward"
)

// stewardBaseSlugs are what talking to the operator takes, at every level:
// seeing what they see, pointing at it, drawing with them and asking them.
var stewardBaseSlugs = []string{
	PermFleetRead, PermDashboardViewRead, PermDashboardCapture, PermDashboardGuide, PermDashboardMark, PermBoardsRead, PermBoardsWrite, PermHumanNotify,
}

// stewardOperatorReadSlugs bring a Steward to what the operator reads, on top
// of the whole fleet that fleet.read already lists. Reads are safe to give by
// default because injected text in a worker's reply can only look with them;
// writing stays with the levels below, steward.default_permissions and sudo.
// Left out on purpose: reading other agents' mail (agent.inbox-watch), though a
// capture shows whatever the operator has open, private setup text (config export), and the proxy.* reads, which spend the operator's GitHub,
// Linear and AWB credentials outside tclaude.
var stewardOperatorReadSlugs = []string{
	PermProcessTemplatesRead, PermProcessRunsRead, PermTriggersRead, PermUsageRead, PermHostRead, PermCostsRead,
	PermAgentContextInfo, PermFederationAuditRead,
}

// stewardSlugs is the access a Steward is born with under this config: its
// levels, then the operator's default permissions. The daemon decides it, not
// the summoning client.
func stewardSlugs(cfg *config.Config) []string {
	return append(stewardLevelSlugs(cfg), stewardDefaultSlugs(cfg)...)
}

// stewardLevelSlugs is what the Reads and Writes levels give.
func stewardLevelSlugs(cfg *config.Config) []string {
	slugs := append([]string{}, stewardBaseSlugs...)
	if cfg.StewardReads() == config.StewardReadsOperator {
		slugs = append(slugs, stewardOperatorReadSlugs...)
	}
	switch cfg.StewardWrites() {
	case config.StewardWritesMessaging:
		slugs = append(slugs, PermMessageDirect)
	case config.StewardWritesWorkers:
		slugs = append(slugs, PermMessageDirect, PermAgentSpawn, PermAgentTask)
	}
	return slugs
}

// stewardDefaultSlugs are the registered entries of steward.default_permissions.
// Saving through the Config tab refuses an unknown slug, so one here comes from
// a hand edit or a slug a later version dropped. It can grant nothing, so it is
// left out rather than stopping every summon.
func stewardDefaultSlugs(cfg *config.Config) []string {
	var slugs []string
	for _, slug := range cfg.StewardDefaultPermissions() {
		if IsKnownPermSlug(slug) && !slices.Contains(sudoDefaultBlocklist, slug) {
			slugs = append(slugs, slug)
		}
	}
	return slugs
}

// recursiveStewardSlugs are the entries of steward.default_permissions that
// hand out permissions themselves. The Steward reads agent mail, so standing
// grant authority on it is refused, as it is for sudo. The operator can still
// grant one to a running Steward from its permissions editor.
func recursiveStewardSlugs(cfg *config.Config) []string {
	var slugs []string
	for _, slug := range cfg.StewardDefaultPermissions() {
		if slices.Contains(sudoDefaultBlocklist, slug) {
			slugs = append(slugs, slug)
		}
	}
	return slugs
}

// unknownStewardSlugs are the entries of steward.default_permissions the slug
// registry does not know.
func unknownStewardSlugs(cfg *config.Config) []string {
	var slugs []string
	for _, slug := range cfg.StewardDefaultPermissions() {
		if !IsKnownPermSlug(slug) {
			slugs = append(slugs, slug)
		}
	}
	return slugs
}

// stewardExtraSlugs are the default permissions the levels do not already give.
func stewardExtraSlugs(cfg *config.Config) []string {
	seen := map[string]bool{}
	for _, slug := range stewardLevelSlugs(cfg) {
		seen[slug] = true
	}
	var extra []string
	for _, slug := range stewardDefaultSlugs(cfg) {
		if !seen[slug] {
			seen[slug] = true
			extra = append(extra, slug)
		}
	}
	return extra
}

// stewardAccessNote tells a Steward what it was born with, for its brief.
func stewardAccessNote(cfg *config.Config) string {
	reads := "every agent and group, boards, the dashboard view and captures of the operator's screen"
	if cfg.StewardReads() == config.StewardReadsOperator {
		reads = "what the operator reads across tclaude, including every agent and group whether or not you share one with them, and captures of their screen"
	}
	writes := "drawing on boards and the operator's view, and notifying the operator"
	switch cfg.StewardWrites() {
	case config.StewardWritesMessaging:
		writes = "messaging any agent, drawing on boards and the operator's view, and notifying the operator"
	case config.StewardWritesWorkers:
		writes = "messaging any agent, starting workers, drawing on boards and the operator's view, and notifying the operator"
	}
	note := "Your access, set by the operator: you can read " + reads + ". You can write: " + writes
	if extra := stewardExtraSlugs(cfg); len(extra) > 0 {
		return note + ". The operator also granted you these permissions: " + strings.Join(extra, ", ") + ". Everything else needs approval."
	}
	return note + "; everything else needs approval."
}

// A Steward's access has three sources, and the operator keeps them apart: what
// Config gives every Steward (stewardSlugs), what was given to one running
// instance (its own overrides and sudo, which end with it), and what the
// operator chose to store for all future Stewards (the steward_carried_*
// tables). The last is read here, at the summon, and nowhere else: being in the
// steward scope is what makes an agent a Steward, so an ordinary agent that is
// merely named "steward" never comes near it.

// stewardCarriedGranter labels what a new Steward inherited.
const stewardCarriedGranter = "steward-carried"

// stewardAccess tells the editors where a live Steward's access comes from.
type stewardAccess struct {
	// Convs are the live Stewards; other agents get the ordinary editor.
	Convs []string `json:"convs"`
	// Config is what Config > Steward gives. It is changed there, not here.
	Config []string `json:"config"`
	// Carried is what is stored for all future Stewards, slug → effect.
	Carried map[string]string `json:"carried"`
	// NotCarryable are slugs that may not be stored: see carryRefusal.
	NotCarryable []string `json:"not_carryable"`
}

func stewardAccessView(cfg *config.Config) *stewardAccess {
	carried, err := db.ListStewardCarriedPermissions()
	if err != nil {
		slog.Warn("steward: read carried permissions failed", "error", err)
	}
	view := &stewardAccess{
		Convs: liveStewardConvs(), Config: stewardSlugs(cfg),
		Carried: map[string]string{}, NotCarryable: sudoDefaultBlocklist,
	}
	for slug, override := range carried {
		view.Carried[slug] = override.Effect
	}
	return view
}

// carryRefusal says why a slug cannot be stored for future Stewards. What Config
// gives is managed in Config, and standing grant authority is refused on a
// Steward's birth for the same reason as in steward.default_permissions.
func carryRefusal(cfg *config.Config, slug string) string {
	if slices.Contains(stewardSlugs(cfg), slug) {
		return slug + " comes from Config > Steward; change it there"
	}
	if slices.Contains(sudoDefaultBlocklist, slug) {
		return slug + " cannot be given to future Stewards: it hands out permissions itself"
	}
	return ""
}

// mergeStewardCarried adds the stored grants to a new Steward's birth grants.
// Config wins a slug both name. A slug a later version dropped is left out, as
// in stewardDefaultSlugs, and so is one the rules no longer let through.
func mergeStewardCarried(in map[string]db.PermissionOverride) {
	carried, err := db.ListStewardCarriedPermissions()
	if err != nil {
		slog.Warn("steward: read carried permissions failed", "error", err)
		return
	}
	for slug, override := range carried {
		if _, config := in[slug]; config || !IsKnownPermSlug(slug) || slices.Contains(sudoDefaultBlocklist, slug) {
			continue
		}
		in[slug] = override
	}
}

// inheritStewardCarriedSudo gives a new Steward the stored temporary grants
// with their original expiry. A failure leaves it with less access, never more,
// so it logs and the summon stands.
func inheritStewardCarriedSudo(convID string) {
	grants, err := db.ListActiveStewardCarriedSudo()
	if err != nil {
		slog.Warn("steward: read carried sudo failed", "error", err)
		return
	}
	agentID, err := db.AgentIDForConv(convID)
	if err != nil || agentID == "" {
		slog.Warn("steward: resolve agent for carried sudo failed", "conv", short8(convID), "error", err)
		return
	}
	cfg, _ := config.Load()
	blocklist := resolveSudoConfig(cfg, convID, agentID, "").Blocklist
	for _, grant := range grants {
		if !IsKnownPermSlug(grant.Slug) || slices.Contains(blocklist, grant.Slug) {
			continue
		}
		if _, err := db.InheritStewardCarriedSudo(agentID, grant, stewardCarriedGranter); err != nil {
			slog.Warn("steward: inherit carried sudo failed", "slug", grant.Slug, "error", err)
		}
	}
}

// saveStewardCarry applies the editor's choices to what is stored. A slug set
// back to Default is revoked for later Stewards too, whether or not the batch
// said so; what stays stored takes the instance's current scope.
func saveStewardCarry(convID string, effects map[string]string, carry map[string]bool) error {
	current, err := agentPermissionOverrides(convID)
	if err != nil {
		return err
	}
	stored, err := db.ListStewardCarriedPermissions()
	if err != nil {
		return err
	}
	for slug, effect := range effects {
		_, wasStored := stored[slug]
		keep, chosen := carry[slug]
		if !chosen {
			keep = wasStored
		}
		switch {
		case effect != "default" && keep:
			if err := db.SetStewardCarriedPermission(slug, current[slug], dashboardGranter); err != nil {
				return err
			}
		case wasStored:
			if err := db.DeleteStewardCarriedPermission(slug); err != nil {
				return err
			}
		}
	}
	return nil
}

// stewardBrief adds to the summon's brief what the daemon owns: the access
// note and the operator's own startup prompt, last so it has the final word.
func stewardBrief(brief string, cfg *config.Config) string {
	brief = strings.TrimRight(brief, "\n") + "\n\n" + stewardAccessNote(cfg)
	if name := cfg.StewardName(); name != "" {
		brief += "\n\nThe operator calls you " + name + ". Use that name."
	}
	if prompt := cfg.StewardStartupPrompt(); prompt != "" {
		brief += "\n\nStanding instructions from the operator:\n" + prompt
	}
	return brief
}

// liveStewardConvs lists the running Stewards' conversations, newest last.
func liveStewardConvs() []string {
	g, err := db.GetAgentGroupByName(stewardGroupName)
	if err != nil || !isScribeGroup(g) {
		return nil
	}
	members, err := db.ListAgentGroupMembers(g.ID)
	if err != nil {
		return nil
	}
	descr := scribeScopeDescr(stewardScopeKind + "/")
	var convs []string
	for _, member := range members {
		if member.Descr != descr || pickAliveSession(member.ConvID) == nil {
			continue
		}
		if state, err := db.AgentState(member.ConvID); err == nil && state == db.AgentStateActive {
			convs = append(convs, member.ConvID)
		}
	}
	return convs
}

// stewardElevation is what a Steward holds beyond Config and the defaults,
// sudo aside: the snapshot lists sudo for every agent.
type stewardElevation struct {
	// Standing grants are its own, so they can be revoked.
	Standing []string `json:"standing,omitempty"`
	// Inherited ones come from a group, shared with other members, or from
	// group ownership.
	Inherited []string `json:"inherited,omitempty"`
}

// stewardElevations reads each live Steward's effective access through the
// same routine the permission gate uses, so nothing it can do goes unseen.
func stewardElevations(cfg *config.Config, state permissionsState) map[string]stewardElevation {
	usual := map[string]bool{}
	for _, slug := range append(stewardSlugs(cfg), state.Defaults...) {
		usual[slug] = true
	}
	out := map[string]stewardElevation{}
	for _, conv := range liveStewardConvs() {
		agentID, err := db.AgentIDForConv(conv)
		if err != nil || agentID == "" {
			continue
		}
		effective, _, provenance, _ := effectivePermsFor(state, conv, ownerImpliedSlugsFor(conv))
		var elevation stewardElevation
		for _, slug := range effective {
			switch source := provenance[slug]; {
			case usual[slug], hasAnyPrefix(source, permSourceSudo, permSourceDefault, permSourceMember):
			case hasAnyPrefix(source, permSourceOverride):
				elevation.Standing = append(elevation.Standing, slug)
			default:
				elevation.Inherited = append(elevation.Inherited, slug)
			}
		}
		if len(elevation.Standing)+len(elevation.Inherited) > 0 {
			out[agentID] = elevation
		}
	}
	return out
}

func hasAnyPrefix(source string, prefixes ...permSource) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(source, string(prefix)) {
			return true
		}
	}
	return false
}

// isStewardAddress reports whether a message target means "the Steward": the
// fixed address, or the name the operator gave it.
func isStewardAddress(to string) bool {
	to = strings.TrimSpace(to)
	if to == "" || !collabRoutesEnabled() {
		return false
	}
	if strings.EqualFold(to, stewardAddress) {
		return true
	}
	cfg, err := config.LoadShared()
	return err == nil && strings.EqualFold(to, cfg.StewardName())
}

// resolveStewardAddresses turns the address "steward" into the newest live
// Steward, in To and Cc alike, before any routing looks at them. It answers the
// request and returns false when no Steward is online.
func resolveStewardAddresses(w http.ResponseWriter, req *sendReq) bool {
	var steward string
	resolve := func(address *string) bool {
		if !isStewardAddress(*address) {
			return true
		}
		if steward == "" {
			stewards := liveStewardConvs()
			if len(stewards) == 0 {
				writeError(w, http.StatusNotFound, "not_found", "no Steward is online; the operator can summon one from the companion window")
				return false
			}
			steward = stewards[len(stewards)-1]
		}
		*address = steward
		return true
	}
	if !resolve(&req.To) {
		return false
	}
	for i := range req.Cc {
		if !resolve(&req.Cc[i]) {
			return false
		}
	}
	return true
}

// isLiveSteward reports whether conv is a live Steward. Its mail is untrusted
// input to an agent with the operator's reads, so being one opens no door: an
// agent answers a Steward through the reply path, which needs only the message
// it received, and writing first takes the usual group or message.direct.
func isLiveSteward(conv string) bool {
	if !collabRoutesEnabled() {
		return false
	}
	for _, steward := range liveStewardConvs() {
		if steward == conv {
			return true
		}
	}
	return false
}
