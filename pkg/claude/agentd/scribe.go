package agentd

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// Scribe summon (JOH-361) — the reusable "summon a pre-briefed, pre-granted
// scribe agent by chat" primitive behind the dashboard's "Edit with agent"
// buttons. A scribe is an ordinary agent the human talks to; its whole value
// is that it comes up already briefed on the task and already holding the
// permission slugs to do it (e.g. templates.manage to edit summoning circles).
//
// Deliberately generic: the request is {name, slugs, brief} and the endpoint
// knows nothing about templates. The Templates tab is the first caller; the
// settings-editing follow-up (JOH-362) reuses the same endpoint for spawn
// profiles / the role library by passing different slugs + a different brief.
//
// Shape decisions:
//   - Reuse-if-alive on a STABLE name: repeat clicks re-brief and re-focus the
//     one live scribe rather than littering a new agent per click.
//   - The scribe lives in its own eponymous one-member group. executeSpawn is
//     group-bound (no group-less spawn primitive), and an eponymous group makes
//     reuse-if-alive an unambiguous name lookup rather than a global
//     title-selector guess.
//   - Stable, shared, pre-trusted cwd (~/.tclaude/scribe, NOT $HOME): a scribe
//     edits daemon-side state through the `tclaude agent` CLI, so it needs no
//     repo checkout — but it does need a directory it can START in unprompted.
//     $HOME made the harness ask the human to approve the folder on every
//     launch (JOH-369). See scribeWorkdir for why the dir is stable + shared.

// scribeGroupDescr is the descr stamped on a scribe's eponymous group so the
// Groups tab explains what the stray one-member group is.
const scribeGroupDescr = "Ad-hoc scribe agent — summoned from the dashboard to edit tclaude state by chat (JOH-361)."

// scribeGranter is the audit label for the birth-time / reuse-time permission
// grants a summon applies, distinct from <human-dashboard> so a forensic query
// can tell a scribe's auto-grant apart from a hand-typed dashboard grant.
const scribeGranter = "<scribe-summon>"

// scribeSummonRequest is the wire body of POST /api/scribe and its /v1 twin.
type scribeSummonRequest struct {
	// Name is the scribe's stable display name AND the name of its dedicated
	// one-member group. Reuse-if-alive keys on it.
	Name string `json:"name"`
	// Slugs are the permission slugs to grant the scribe at birth (and re-grant
	// on reuse), e.g. ["templates.manage"]. Each is validated against the slug
	// registry; an unknown slug is a 400 listing the known slugs.
	Slugs []string `json:"slugs"`
	// DenySlugs are explicit permission denies applied at birth and on reuse.
	// This lets a capability-reducing scribe defend its safety boundary even
	// when an older live incarnation or a global default granted more power.
	DenySlugs []string `json:"deny_slugs,omitempty"`
	// Exclusive turns Slugs into the exact positive capability set: every
	// other registered permission is denied at birth and again on reuse.
	Exclusive bool `json:"exclusive,omitempty"`
	// Brief is the pre-briefing delivered to the scribe's inbox — the concept
	// pointer + scope anchor the human's chat starts from. Same charset/length
	// rule as any spawn initial_message.
	Brief string `json:"brief"`
}

// scribeOutcome is summonScribe's result: the live scribe's conv-id, whether an
// existing one was reused, and the open-window handshake (native vs the
// in-browser terminal fallback), mirroring the spawn/open-window responses.
type scribeOutcome struct {
	ConvID    string
	Reused    bool
	FocusMode string // "native" | "browser" | ""
	FocusWS   string // set when FocusMode == "browser"
}

// handleScribeSummon is the shared /v1 handler. The human (dashboard peer or a
// human on the socket) always passes; an agent caller must hold groups.spawn —
// and, because a summon applies birth-time grants, permissions.grant too. That
// is exactly the bar handleGroupSpawn sets for a birth-time-granted spawn, so
// no new privilege model is introduced.
func handleScribeSummon(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	// requirePermission hands back the caller's conv-id: a real agent resolves
	// to its conv, the human resolves to "".
	spawnerConvID, ok := requirePermission(w, r, PermGroupsSpawn)
	if !ok {
		return
	}

	var body scribeSummonRequest
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "json", err.Error())
			return
		}
	}

	name := strings.TrimSpace(body.Name)
	if name == "" {
		writeError(w, http.StatusBadRequest, "name", "name is required")
		return
	}
	// The name is the scribe's group name (and, when it clears the rename-title
	// gate, its launch --name). Validate it as a group name up front.
	if err := validateGroupName(name); err != nil {
		writeError(w, http.StatusBadRequest, "name", "invalid scribe name: "+err.Error())
		return
	}

	// Slugs → birth-time grants. Reuse the spawn-boundary normaliser so an
	// unknown slug is rejected with the same actionable "known slugs: …" list
	// the spawn endpoint gives.
	in := make(map[string]string, len(body.Slugs))
	for _, s := range body.Slugs {
		if s = strings.TrimSpace(s); s != "" {
			in[s] = db.PermEffectGrant
		}
	}
	for _, s := range body.DenySlugs {
		if s = strings.TrimSpace(s); s != "" {
			if in[s] == db.PermEffectGrant {
				writeError(w, http.StatusBadRequest, "slugs", "permission slug "+s+" cannot be both granted and denied")
				return
			}
			in[s] = db.PermEffectDeny
		}
	}
	if body.Exclusive {
		for _, spec := range permissionRegistry {
			if in[spec.Slug] != db.PermEffectGrant {
				in[spec.Slug] = db.PermEffectDeny
			}
		}
	}
	overrides, povErr := normalizeSpawnPermissionOverrides(in)
	if povErr != "" {
		writeError(w, http.StatusBadRequest, "slugs", povErr)
		return
	}
	if len(overrides) == 0 {
		writeError(w, http.StatusBadRequest, "slugs", "at least one permission slug is required to summon a scribe")
		return
	}

	// A summon applies birth-time grants, so an agent caller (not the human)
	// needs permissions.grant on top of groups.spawn — the same guard
	// handleGroupSpawn puts on a spawn carrying permission_overrides.
	if spawnerConvID != "" && resolvePermission(spawnerConvID, PermPermissionsGrant) != permAllow {
		writeError(w, http.StatusForbidden, "permission",
			"granting a scribe birth-time permissions requires the "+PermPermissionsGrant+" permission")
		return
	}

	brief := strings.TrimSpace(body.Brief)
	if brief == "" {
		writeError(w, http.StatusBadRequest, "brief", "brief is required")
		return
	}
	// The brief rides the inbox as an agent_messages row, so it must clear the
	// same charset/length rule every initial_message does.
	if !isValidInitialMessage(brief) {
		writeError(w, http.StatusBadRequest, "brief",
			fmt.Sprintf("brief must be at most %d characters; newlines and tabs are allowed "+
				"(it is delivered to the scribe's inbox, not typed into its pane), but other "+
				"control characters are not", agent.MaxInitialMessageBytes))
		return
	}

	out, fail := summonScribe(name, overrides, brief, body.Exclusive)
	if fail != nil {
		writeError(w, fail.Status, fail.Kind, fail.Msg)
		return
	}

	resp := map[string]any{
		"name":    name,
		"conv_id": out.ConvID,
		"reused":  out.Reused,
	}
	if aid := peerAgentID(out.ConvID); aid != "" {
		resp["agent_id"] = aid
	}
	// FocusMode is "browser" when no native window could be popped (headless
	// agentd / no terminal emulator) — the dashboard points at the in-browser
	// terminal instead of claiming success, the same handshake spawn uses.
	if out.FocusMode != "" {
		resp["focus_mode"] = out.FocusMode
		if out.FocusMode == "browser" {
			resp["focus_ws"] = out.FocusWS
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleDashboardScribeAPI is the cookie-auth /api twin: the dashboard cookie +
// Origin pin is the human-consent layer, so it stamps a synthetic human peer
// and delegates to the shared handler (same pattern as dashboardSpawnInGroup).
func handleDashboardScribeAPI(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	handleScribeSummon(w, asDashboardHumanPeer(r))
}

// scribeSummonMu serializes summons so the reuse-if-alive check-then-spawn is
// atomic: two near-simultaneous clicks would otherwise both see "no live
// scribe" and double-spawn — or race on the UNIQUE group-name insert and 500.
// Summons are rare human actions, so a global lock costs nothing.
var scribeSummonMu sync.Mutex

// summonScribe is the reusable core: ensure the scribe's eponymous group,
// reuse-if-alive (re-grant + re-brief + re-focus), otherwise spawn one in the
// shared, pre-trusted scribe workdir with the birth-time grants and auto-focus.
func summonScribe(name string, overrides map[string]string, brief string, exclusive bool) (*scribeOutcome, *spawnFailure) {
	scribeSummonMu.Lock()
	defer scribeSummonMu.Unlock()

	g, fail := ensureScribeGroup(name)
	if fail != nil {
		return nil, fail
	}

	// Reuse-if-alive: a repeat click on a live scribe re-grants the slugs
	// (idempotent — covers a widened slug set), re-briefs it (a repeat click may
	// carry a fresh scope: library-wide vs one specific template), and just
	// re-opens its window. The live scribe already spawned into the trusted
	// workdir, so no dir/trust work is needed on this path.
	if convID := aliveScribeConv(g); convID != "" {
		if err := applyScribeOverrides(convID, overrides, exclusive, true); err != nil {
			if exclusive {
				killScribeSession(convID)
			}
			return nil, &spawnFailure{http.StatusInternalServerError, "permission",
				"refusing to reopen capability-reducing scribe after permission override failure: " + err.Error()}
		}
		rebriefScribe(convID, brief)
		out := &scribeOutcome{ConvID: convID, Reused: true}
		openScribeWindow(convID, out)
		return out, nil
	}

	// No live scribe — spawn a fresh one. First prune any dead scribe left in
	// the group by a prior generation (a daemon restart kills the tmux session
	// but leaves the membership row; group membership is not auto-reaped on
	// death). Otherwise the "one-member" group would slowly accumulate stale
	// rows across restarts — exactly the litter reuse-if-alive exists to avoid.
	pruneDeadScribes(g)

	// Adopt the operator's configured scribe launch profile (JOH-371) BEFORE we
	// resolve the trust-seed harness and spawn. Both the trust seed
	// (scribeSpawnHarness) and the launch shape (executeSpawn →
	// applyDefaultProfile) read the group's default profile, so stamping it here
	// makes them agree by construction — a codex-profile scribe gets codex trust
	// seeding, never CC's. Fresh-spawn path only: a reused live scribe (handled
	// above) keeps the launch shape it was born with.
	stampScribeProfileFromConfig(g)

	// Resolve + create the shared scribe workdir and pre-trust it for the
	// harness this scribe will launch on, so its detached pane comes up without
	// a folder-trust prompt (JOH-369).
	cwd, fail := ensureScribeWorkdir()
	if fail != nil {
		return nil, fail
	}
	seedScribeDirTrust(scribeSpawnHarness(g), cwd)

	// executeSpawn enrolls the scribe into its group, applies the birth-time
	// grants (enrollSpawnedConv), delivers the brief to its inbox and —
	// AutoFocus — opens its terminal window. Synchronous (Async left false) so
	// the conv-id materialises for the grants + the response.
	p := spawnParams{
		Name:                name,
		Cwd:                 cwd,
		InitialMessage:      brief,
		AutoFocus:           !exclusive,
		PermissionOverrides: overrides,
	}
	outcome, spawnFail := executeSpawn(g, p)
	if spawnFail != nil {
		return nil, spawnFail
	}
	if exclusive {
		if err := applyScribeOverrides(outcome.ConvID, overrides, true, false); err != nil {
			killScribeSession(outcome.ConvID)
			return nil, &spawnFailure{http.StatusInternalServerError, "permission",
				"capability-reducing scribe stopped after permission override failure: " + err.Error()}
		}
	}
	out := &scribeOutcome{ConvID: outcome.ConvID, FocusMode: outcome.FocusMode}
	if outcome.FocusMode == "browser" {
		out.FocusWS = spawnFocusWSPath(outcome.Label)
	}
	if exclusive {
		openScribeWindow(outcome.ConvID, out)
	}
	return out, nil
}

// scribeWorkdir returns the ONE shared workdir all scribes spawn into,
// ~/.tclaude/scribe. Deliberately flat and shared across every scribe kind
// (JOH-369, revised from a per-name dir): a per-name dir would fire the
// harness's folder-trust prompt once PER scribe kind (every future JOH-362
// scribe — profiles, roles, … — re-prompting), whereas one shared path is
// trusted ONCE, ever. Still stable (not a per-summon temp dir), so a scribe's
// CC conversation — which is cwd-bound — resumes/reuses from the same place and
// the path-attached trust holds. Accepted trade-off: all scribes share this
// dir's CC per-project memory (a shared "scribe house" know-how; revisit
// per-kind isolation only if cross-contamination ever bites).
func scribeWorkdir() (string, bool) {
	dir := config.ConfigDir() // ~/.tclaude, or "" if home can't be resolved
	if dir == "" {
		return "", false
	}
	return filepath.Join(dir, "scribe"), true
}

// ensureScribeWorkdir resolves and creates the shared scribe workdir. 0700:
// it's a private per-user scratch dir agents operate in — no need for group /
// other access.
func ensureScribeWorkdir() (string, *spawnFailure) {
	dir, ok := scribeWorkdir()
	if !ok {
		return "", &spawnFailure{http.StatusInternalServerError, "home", "resolve home directory for scribe workdir"}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", &spawnFailure{http.StatusInternalServerError, "workdir", "create scribe workdir: " + err.Error()}
	}
	return dir, nil
}

// scribeSpawnHarness reports the harness this scribe summon will actually
// launch on. A scribe leaves --harness unset, so it follows the eponymous
// group's default spawn profile, then the global default profile (neither →
// the default harness). Mirrors executeSpawn → applyDefaultProfile so the dir
// we pre-trust matches the harness that will read the trust store.
func scribeSpawnHarness(g *db.AgentGroup) string {
	if prof := groupDefaultProfile(g); prof != nil {
		return harnessOrDefault(prof.Harness)
	}
	if prof := globalDefaultProfile(); prof != nil {
		return harnessOrDefault(prof.Harness)
	}
	return harness.DefaultName
}

// stampScribeProfileFromConfig points the scribe group's default spawn profile
// at config.scribe.profile (JOH-371), so the existing group-default-profile
// resolution launches the fresh scribe on that profile with ZERO new spawn
// logic: scribeSpawnHarness → seedScribeDirTrust picks the profile's harness
// for the trust pre-seed, and executeSpawn → applyDefaultProfile fills the
// launch fields from it — BOTH reading the same stamped value, so the trust
// seed can never target a different harness than the spawn (the coupling
// JOH-369 introduced).
//
// Re-stamped on every fresh summon so it tracks a config edit between summons;
// "" clears the stamp (harness default). A name whose profile was
// deleted/renamed is stamped as-is and self-heals to the default at resolution
// time (groupDefaultProfile → nil), mirroring `tclaude ask`'s live self-heal.
// Only the fresh-spawn path calls this — a reused live scribe keeps the launch
// shape it was born with (config applies to the NEXT fresh summon).
//
// Best-effort: a config-load or DB-write failure logs and leaves the group's
// existing default profile in place (worst case the scribe launches on the
// previously-stamped profile, or the harness default), never a failed summon.
// The in-memory g.DefaultProfile is updated on success so the trust seed and
// spawn this same summon runs observe the new stamp without a group reload.
func stampScribeProfileFromConfig(g *db.AgentGroup) {
	cfg, err := config.Load()
	if err != nil {
		// config.Load already degraded to defaults; keep the group's current
		// default rather than clearing it on a transient/corrupt-file error.
		slog.Warn("scribe: load config for profile stamp failed; keeping group default",
			"group", g.Name, "error", err)
		return
	}
	profile := cfg.ScribeProfileName()
	if g.DefaultProfile == profile {
		return // already stamped — idempotent, skip the write
	}
	if _, err := db.SetAgentGroupDefaultProfile(g.Name, profile); err != nil {
		slog.Warn("scribe: stamp default profile failed; keeping group default",
			"group", g.Name, "profile", profile, "error", err)
		return
	}
	g.DefaultProfile = profile
}

// seedScribeDirTrust pre-trusts the shared scribe workdir for the given harness
// so a detached scribe pane never freezes on a folder-trust prompt (JOH-369).
// Best-effort by design: a failure logs and the summon proceeds — the worst
// case is a single one-time trust dialog the human clears via the pane's focus
// button, never a failed summon. Human-consented by construction: it only ever
// pre-trusts the daemon-created scribe dir, never a caller-supplied path.
func seedScribeDirTrust(harnessName, dir string) {
	switch harnessName {
	case harness.CodexName:
		if err := harness.EnsureCodexDirTrusted(dir); err != nil {
			slog.Warn("scribe: pre-trust codex workdir failed", "dir", dir, "error", err)
		}
	case harness.DefaultName: // "claude"
		if err := harness.EnsureClaudeDirTrusted(dir); err != nil {
			slog.Warn("scribe: pre-trust claude workdir failed", "dir", dir, "error", err)
		}
	default:
		// A harness with no known trust store (or one added later without a
		// seeding path wired here). Skip rather than guess — the pane may raise
		// a one-time trust prompt the human clears via its focus button.
		slog.Warn("scribe: no dir-trust seeding for harness; pane may prompt once",
			"harness", harnessName, "dir", dir)
	}
}

// ensureScribeGroup returns the scribe's eponymous group, creating it on first
// summon. The group is a plain container for the one scribe member.
//
// It fails closed on a NON-scribe group of the same name: the caller supplies
// the name, so without this a summon whose name collided with a real working
// group would resolve that group and treat its first live member as "the
// scribe" — re-granting + re-briefing + nudging a foreign agent, or (empty
// group) spawning a stray scribe into it. Verifying the scribe-group marker
// before touching an existing group keeps the summon operating only on groups
// this machinery created.
func ensureScribeGroup(name string) (*db.AgentGroup, *spawnFailure) {
	g, err := db.GetAgentGroupByName(name)
	if err != nil {
		return nil, &spawnFailure{http.StatusInternalServerError, "group", "look up scribe group: " + err.Error()}
	}
	if g != nil {
		if !isScribeGroup(g) {
			return nil, &spawnFailure{http.StatusConflict, "group",
				fmt.Sprintf("a non-scribe group named %q already exists — rename or remove it to summon a scribe by that name", name)}
		}
		return g, nil
	}
	id, err := db.CreateAgentGroup(name, scribeGroupDescr)
	if err != nil {
		return nil, &spawnFailure{http.StatusInternalServerError, "group", "create scribe group: " + err.Error()}
	}
	g, err = db.GetAgentGroupByID(id)
	if err != nil {
		return nil, &spawnFailure{http.StatusInternalServerError, "group", "reload scribe group: " + err.Error()}
	}
	if g == nil {
		return nil, &spawnFailure{http.StatusInternalServerError, "group", "scribe group vanished after create"}
	}
	return g, nil
}

// isScribeGroup reports whether g is a group this machinery created (marked by
// its descr), so a summon never reuses / spawns into a same-named foreign group.
// The descr is stamped at creation and nothing in the scribe path rewrites it; a
// human who edits it just makes future summons fail closed (safe), not misfire.
func isScribeGroup(g *db.AgentGroup) bool {
	return g != nil && g.Descr == scribeGroupDescr
}

// aliveScribeConv returns the conv-id of a live member of the scribe group, or
// "" if none is up. The group holds a single scribe, so the first alive member
// is the scribe.
func aliveScribeConv(g *db.AgentGroup) string {
	members, err := db.ListAgentGroupMembers(g.ID)
	if err != nil {
		slog.Warn("scribe: list group members failed", "group", g.Name, "error", err)
		return ""
	}
	for _, m := range members {
		if pickAliveSession(m.ConvID) != nil {
			return m.ConvID
		}
	}
	return ""
}

// pruneDeadScribes unlinks any non-alive member from the scribe group so it
// stays a clean single-member container. Best-effort — a failed removal just
// leaves a harmless stale row (reuse-if-alive still skips it, since it isn't
// alive). Removing membership does not delete the conv, only the group link.
func pruneDeadScribes(g *db.AgentGroup) {
	members, err := db.ListAgentGroupMembers(g.ID)
	if err != nil {
		return
	}
	for _, m := range members {
		if pickAliveSession(m.ConvID) != nil {
			continue
		}
		if err := db.RemoveAgentGroupMember(g.ID, m.ConvID); err != nil {
			slog.Warn("scribe: prune dead member failed", "group", g.Name, "conv", short8(m.ConvID), "error", err)
		}
	}
}

// applyScribeOverrides (re)applies the requested permission effects. An
// exclusive capability-reducing summon fails closed on any write error; an
// ordinary scribe preserves the established best-effort behavior.
func applyScribeOverrides(convID string, overrides map[string]string, required, preserveSameEffectProvenance bool) error {
	if required {
		if _, err := db.RevokeSudoGrantsByConv(convID); err != nil {
			return fmt.Errorf("revoke active sudo grants: %w", err)
		}
	}
	// Ordinary mode clears stale summon-authored denies and applies the current
	// request in one transaction. Exclusive mode already supplies an effect for
	// every registered slug, so it needs no stale-row cleanup. Same-effect rows
	// preserve their original audit provenance in the DB layer.
	if err := db.ApplyAgentPermissionOverrides(convID, overrides, scribeGranter, !required, preserveSameEffectProvenance); err != nil {
		slog.Warn("scribe: apply permission overrides failed", "conv", short8(convID), "error", err)
		if required {
			return fmt.Errorf("apply permission overrides: %w", err)
		}
	}
	return nil
}

func killScribeSession(convID string) {
	if sess := pickAliveSession(convID); sess != nil {
		if err := clcommon.TmuxCommand("kill-session", "-t", clcommon.ExactTarget(sess.TmuxSession)).Run(); err != nil {
			slog.Error("scribe: failed to kill unsafe capability-reducing scribe", "conv", short8(convID), "error", err)
		}
	}
}

// rebriefScribe delivers a fresh brief to a reused scribe's inbox and nudges it
// if it's live — the same universal-inbox transport a self-reincarnate request
// rides. A daemon-originated system message (FromConv empty, group_id 0).
func rebriefScribe(convID, brief string) {
	id, err := db.InsertAgentMessage(&db.AgentMessage{
		GroupID:      0,
		FromConv:     "",
		ToConv:       convID,
		Subject:      "New editing task",
		Body:         brief,
		ToRecipients: []string{convID},
	})
	if err != nil {
		slog.Warn("scribe: re-brief insert failed", "conv", short8(convID), "error", err)
		return
	}
	nudgeIfAlive(id, convID)
}

// openScribeWindow opens a terminal attached to the scribe's live session,
// recording the native/browser outcome onto out — the same native-first /
// in-browser-fallback the open-window row action does.
func openScribeWindow(convID string, out *scribeOutcome) {
	sess := pickAliveSession(convID)
	if sess == nil {
		return // caller just confirmed it was alive; a race here is harmless
	}
	if err := openTerminal(openAttachCmd(sess.ID)); err != nil {
		out.FocusMode = "browser"
		out.FocusWS = "/api/open-window-ws/" + url.PathEscape(convID)
		return
	}
	out.FocusMode = "native"
}
