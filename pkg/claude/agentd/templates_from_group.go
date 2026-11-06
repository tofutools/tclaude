package agentd

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// handleTemplateFromGroup snapshots a live group's structure into a
// template — the reverse direction of instantiate. Gated on
// templates.manage. Body: { group, template_name, update }.
//
// It carries over the group's descr + default_context and one template
// agent per group member (role, descr, owner flag, the member's
// per-conv permission grants). It does NOT carry per-agent task briefs:
// a live group has no stored "initial message" per member, so
// initial_message comes through blank for the human to fill in the
// editor afterwards.
//
// A taken template name is a hard 409 unless `update` is set, which
// re-snapshots the (possibly evolved) group into the existing template
// IN PLACE (JOH-337): the roster, owner flags, permissions and context
// are re-traced from the group, while curated per-agent briefs survive
// for roster agents that match an existing template agent by name —
// members titled "<group>-<name>" (instantiate's own naming) round-trip
// back to their template-agent <name>. With `update` set and no such
// template, it is simply created. The update response reports the
// roster diff (briefs_kept / added / removed).
func handleTemplateFromGroup(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePermission(w, r, PermTemplatesManage); !ok {
		return
	}
	var body struct {
		Group        string `json:"group"`
		TemplateName string `json:"template_name"`
		Update       bool   `json:"update"`
		// Preview (JOH-393): return the snapshot WITHOUT persisting, so the
		// dashboard's drag-a-group-onto-the-dock gesture can open the template
		// editor pre-filled but unsaved (the human previews + edits, then an
		// explicit Save creates it). A preview never touches the DB — no existing
		// lookup, no create/update — so Update is irrelevant here and ignored.
		Preview bool `json:"preview"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	body.Group = strings.TrimSpace(body.Group)
	body.TemplateName = strings.TrimSpace(body.TemplateName)
	if err := validateGroupName(body.TemplateName); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "template_name: "+err.Error())
		return
	}
	g, err := db.GetAgentGroupByName(body.Group)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	if g == nil {
		writeError(w, http.StatusNotFound, "not_found", "no such group "+body.Group)
		return
	}
	members, err := db.ListAgentGroupMembers(g.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	ownerSet := map[string]bool{}
	if owners, err := db.ListAgentGroupOwners(g.ID); err == nil {
		for _, o := range owners {
			ownerSet[o.ConvID] = true
		}
	}

	// Preview (JOH-393): trace the fresh snapshot and hand it back WITHOUT
	// writing anything — the editor opens on this unsaved blueprint. existing=nil
	// so it's always the create-shape snapshot (no in-place brief/ref merge,
	// which only applies when re-snapshotting a stored template). Same response
	// shape as a real create so the client reads it identically.
	if body.Preview {
		t, _, observedNotChosen := snapshotGroupTemplate(body.TemplateName, g, members, ownerSet, nil)
		writeJSON(w, http.StatusOK, fromGroupCreateJSON{
			templateJSON: templateToJSON(t),
			BlankBriefs:  countBlankBriefs(t),
			Dropped:      observedNotChosen,
		})
		return
	}

	// Resolving `existing` and writing the template are separate DB
	// round-trips, and with update set the contract is create-or-update —
	// a concurrent create/delete in that window must not surface as a
	// spurious 409/500. Losing a create race re-resolves and updates in
	// place; losing the template under an update re-resolves and creates.
	// One retry is enough: the second pass starts from freshly observed
	// state, and a second interleaved mutation falls through to the plain
	// conflict/error paths.
	for attempt := 0; ; attempt++ {
		existing, err := db.GetGroupTemplate(body.TemplateName)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		if existing != nil && !body.Update {
			writeError(w, http.StatusConflict, "exists",
				"a template named "+body.TemplateName+" already exists (set update to re-snapshot it in place)")
			return
		}

		t, tracedByName, observedNotChosen := snapshotGroupTemplate(body.TemplateName, g, members, ownerSet, existing)

		if existing == nil {
			id, err := db.CreateGroupTemplate(t)
			if errors.Is(err, db.ErrGroupTemplateNameTaken) {
				if body.Update && attempt == 0 {
					continue // lost a create race — re-resolve and update in place
				}
				writeError(w, http.StatusConflict, "exists", err.Error())
				return
			}
			if err != nil {
				writeError(w, http.StatusInternalServerError, "io", err.Error())
				return
			}
			t.ID = id
			writeJSON(w, http.StatusCreated, fromGroupCreateJSON{
				templateJSON: templateToJSON(t),
				BlankBriefs:  countBlankBriefs(t),
				Dropped:      observedNotChosen,
			})
			return
		}

		// Update in place. Curated per-agent briefs survive where the fresh
		// roster matches an existing template agent by name (a from-group
		// snapshot itself never sets briefs), and a curated descr/context is
		// never clobbered by a blank one from the group.
		prevByName := map[string]db.GroupTemplateAgent{}
		prevOrder := []string{}
		for _, a := range existing.Agents {
			prevByName[a.Name] = a
			prevOrder = append(prevOrder, a.Name)
		}
		briefsKept, added := []string{}, []string{}
		// Seeded with the fields the TRACE declined to pin, so both kinds of
		// non-carry reach the operator through one list rather than one of them
		// having a channel and the other being silent.
		dropped := append([]snapshotFieldDrop{}, observedNotChosen...)
		newNames := map[string]bool{}
		for i := range t.Agents {
			newNames[t.Agents[i].Name] = true
			prev, ok := prevByName[t.Agents[i].Name]
			if !ok {
				added = append(added, t.Agents[i].Name)
				continue
			}
			if prev.InitialMessage != "" {
				t.Agents[i].InitialMessage = prev.InitialMessage
				briefsKept = append(briefsKept, t.Agents[i].Name)
			}
			// The spawn-profile REFERENCE is blueprint curation, not an observable
			// launch field — a live member records its resolved model/effort/harness
			// (re-traced above) but not "which profile it was launched from". So an
			// update re-snapshot preserves a curated profile ref on name-match,
			// exactly like the brief (JOH-239). The inline overrides, being
			// observable, are left as re-traced (the live group wins).
			if prev.SpawnProfile != "" {
				t.Agents[i].SpawnProfile = prev.SpawnProfile
			}
			// The role REFERENCE is likewise blueprint curation, not an
			// observable property of a live member — a re-snapshot preserves a
			// curated role_ref on name-match, exactly like the spawn-profile
			// ref and the brief (JOH-240).
			if prev.RoleRef != "" {
				t.Agents[i].RoleRef = prev.RoleRef
			}
			// The template-local profile splits the same way: its OBSERVABLE
			// fields (harness/model/effort/sandbox + live permission grants) are
			// re-traced above (the live group wins, exactly like the legacy
			// inline overrides used to be), while its NON-observable fields
			// (approval, ask-timeout, the launch toggles, deny overrides) are
			// blueprint curation and carry forward on name-match.
			inline, drop := mergeSnapshotInlineProfile(
				prev.ProfileInline, t.Agents[i].ProfileInline, tracedByName[t.Agents[i].Name])
			t.Agents[i].ProfileInline = inline
			if drop != nil {
				drop.Agent = t.Agents[i].Name
				dropped = append(dropped, *drop)
			}
		}
		removed := []string{}
		for _, n := range prevOrder {
			if !newNames[n] {
				removed = append(removed, n)
			}
		}
		// Descr describes the BLUEPRINT, not the instance — and instantiate
		// stamps groups with "Instantiated from template <name>", so pulling
		// the group's descr would clobber curated copy on every round-trip.
		// The existing template's descr wins unless it's blank. Context is
		// the opposite: it genuinely evolves in the live group (that's a key
		// thing a re-snapshot recaptures), so the group's wins unless blank.
		if existing.Descr != "" {
			t.Descr = existing.Descr
		}
		if t.DefaultContext == "" {
			t.DefaultContext = existing.DefaultContext
		}
		// A live group has no work pattern to trace — the pattern is
		// blueprint choreography (JOH-336), curated in the editor like the
		// briefs, so an update re-snapshot always keeps the existing one.
		t.WorkPattern = existing.WorkPattern
		// A live group has no template-default worktree preference to trace. Keep
		// the curated value when refreshing a template from its running group.
		t.PerAgentWorktrees = existing.PerAgentWorktrees
		if fail := checkTemplateOwnerScopes(httpTemplateOwnerScopesGate(w, r), existing, t.OwnerScopesJSON); fail != nil {
			if fail != errTemplateGateResponded {
				writeError(w, fail.Status, fail.Kind, fail.Msg)
			}
			return
		}
		t.ID = existing.ID
		if err := db.UpdateGroupTemplate(t); err != nil {
			if errors.Is(err, sql.ErrNoRows) && attempt == 0 {
				continue // template deleted underfoot — re-resolve and create
			}
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, fromGroupUpdateJSON{
			templateJSON: templateToJSON(t),
			Updated:      true,
			BriefsKept:   briefsKept,
			Added:        added,
			Removed:      removed,
			BlankBriefs:  countBlankBriefs(t),
			Dropped:      dropped,
		})
		return
	}
}

// mergeSnapshotInlineProfile combines a stored template agent's template-local
// profile (prev) with the freshly re-traced one (traced) for an update-mode
// from-group re-snapshot. The traced profile's OBSERVABLE fields win (harness /
// model / effort / sandbox + the member's live permission grants); prev's
// NON-observable, curated fields carry forward: approval and tool governance
// (never recorded on a session row), profile startup context, the ask-timeout,
// the trust_dir / auto_review / remote_control toggles, and any deny permission
// overrides (only grants are observable).
// prev's is_owner is deliberately dropped — ownership IS observable and rides
// the agent row's own owner flag. Returns nil when neither side carries
// anything.
func mergeSnapshotInlineProfile(prev, traced *db.SpawnProfile, observed bool) (*db.SpawnProfile, *snapshotFieldDrop) {
	if prev == nil {
		return traced, nil
	}
	out := &db.SpawnProfile{}
	if traced != nil {
		cp := *traced
		out = &cp
	}
	// ROOT FIX (TCL-1083): "traced-wins" means the trace wins WHEN THERE IS ONE.
	// An unobserved member is an absence of information, and blanking the
	// observable fields out of it asserts something false — "this member pins no
	// harness" — when the truth is "we learned nothing about this member". Every
	// invalid combination in the harness-only field family is manufactured by
	// that one step: the pin goes, harnessOrDefault("") answers "claude", and the
	// stored Copilot/Codex-only fields are suddenly sitting on a Claude profile.
	//
	// Keyed on `observed`, never on "the traced harness came back blank" — those
	// are different questions with the same spelling, and the second one is wrong
	// for a genuinely-traced Claude member. Live permission grants are read
	// independently of the launch trace, so they are NOT restored here: an
	// unobserved member can still have observable grants.
	if !observed {
		out.Harness, out.Model = prev.Harness, prev.Model
		out.Effort, out.Sandbox = prev.Effort, prev.Sandbox
	}
	if out.ModelProxy == "" {
		out.ModelProxy = prev.ModelProxy
	}
	if out.Approval == "" {
		out.Approval = prev.Approval
	}
	if out.ToolGovernance == "" {
		out.ToolGovernance = prev.ToolGovernance
	}
	if out.AskUserQuestionTimeout == "" {
		out.AskUserQuestionTimeout = prev.AskUserQuestionTimeout
	}
	if out.StartupContext == "" {
		out.StartupContext = prev.StartupContext
	}
	out.AutoReview = prev.AutoReview
	out.TrustDir = prev.TrustDir
	out.RemoteControl = prev.RemoteControl
	out.AutoMemory = prev.AutoMemory
	out.PeerMessaging = prev.PeerMessaging
	if out.SSHWorkaround == nil {
		out.SSHWorkaround = prev.SSHWorkaround
	}
	// Worktree preparation happens before the member launch and is not observable
	// from its running session, so preserve the template's curated setting.
	if out.FetchLatestWorktree == nil {
		out.FetchLatestWorktree = prev.FetchLatestWorktree
	}
	// The drive IS observable (traceMemberLaunch reads it off the member's
	// durable relaunch profile), so a traced answer wins — including a traced
	// false, which is how a member moved back to send-keys clears the template's
	// previous opt-in. nil means "could not observe", and falls back to prev.
	if out.CopilotAPI == nil {
		out.CopilotAPI = prev.CopilotAPI
	}
	if out.CodexAppServer == nil {
		out.CodexAppServer = prev.CodexAppServer
	}
	if out.FastMode == nil {
		out.FastMode = prev.FastMode
	}
	// ContextFeatures IS observable, so the traced value wins like harness/model —
	// INCLUDING an observed "trims nothing", which must be able to CLEAR the
	// template's previous trims. That is why the signal here is nil-vs-empty
	// rather than len()==0: the snapshot pass writes a non-nil empty map for a
	// member it traced that trims nothing, and leaves nil when it could not
	// observe the member at all (a pruned session row). Keying on len()==0 would
	// conflate the two and make the trims permanently unclearable.
	if out.ContextFeatures == nil {
		out.ContextFeatures = prev.ContextFeatures
	}
	// A template-local environment is curated launch configuration; no session
	// trace can reconstruct which profile tier authored it, so carry it forward.
	if out.Environment == nil {
		out.Environment = append([]sandboxpolicy.EnvironmentEntry(nil), prev.Environment...)
	}
	// The window is observable too, but "" is ambiguous here (a member that pins
	// nothing and a member that could not be traced both read ""), so a blank
	// falls back to the template's previous value like Approval does rather than
	// silently clearing it.
	if out.AutoCompactWindow == "" {
		out.AutoCompactWindow = prev.AutoCompactWindow
	}
	// The Copilot context cap is observable (traceMemberLaunch reads it off the
	// member's durable relaunch profile), but 0 is ambiguous exactly the way the
	// window's "" is — a member that pins no cap and a member that could not be
	// traced both read 0 — so a zero falls back to the template's previous value
	// rather than silently clearing a curated cap (TCL-1062).
	//
	// TCL-1062 gated this one field on the merged profile resolving to Copilot,
	// because a cap on any other harness 400s the agent at deploy. That gate is
	// now the general one below, which covers the whole family and — the part
	// TCL-1062 could not do — DISCLOSES what it took.
	if out.ContextWindowMax == 0 {
		out.ContextWindowMax = prev.ContextWindowMax
	}
	// The implementation is observable, but only a non-default one is traced (see
	// traceMemberLaunch), so "" is ambiguous here the same way the window's is:
	// a harness-builtin member and an untraceable one both read "". Fall back to
	// the template's previous value rather than silently clearing a pin.
	if out.SandboxImplementation == "" {
		out.SandboxImplementation = prev.SandboxImplementation
	}
	for slug, override := range prev.PermissionOverrides {
		if override.Effect != db.PermEffectGrant {
			if out.PermissionOverrides == nil {
				out.PermissionOverrides = map[string]db.PermissionOverride{}
			}
			if _, ok := out.PermissionOverrides[slug]; !ok {
				out.PermissionOverrides[slug] = override
			}
		}
	}
	// Gate LAST, so it judges the finished profile — and before the emptiness
	// check, because dropping the only field a profile carried has to be able to
	// empty it.
	drop := dropLaunchFieldsForeignToHarness(out)
	if out.Harness == "" && out.Model == "" && out.ModelProxy == "" && out.Effort == "" && out.Sandbox == "" &&
		out.Approval == "" && out.ToolGovernance == "" && out.AskUserQuestionTimeout == "" &&
		out.StartupContext == "" &&
		out.AutoCompactWindow == "" && out.SandboxImplementation == "" &&
		out.ContextWindowMax == 0 &&
		out.AutoReview == nil && out.TrustDir == nil && out.RemoteControl == nil && out.AutoMemory == nil && out.PeerMessaging == nil &&
		out.SSHWorkaround == nil && out.FetchLatestWorktree == nil &&
		out.CopilotAPI == nil && out.CodexAppServer == nil && out.FastMode == nil &&
		len(out.ContextFeatures) == 0 && len(out.Environment) == 0 &&
		out.IsOwner == nil && len(out.PermissionOverrides) == 0 {
		return nil, drop
	}
	return out, drop
}

// snapshotFieldDrop is one agent's disclosure that a re-snapshot refused to
// carry launch fields forward. A drop is silent otherwise: the merge would
// simply return a smaller profile, the template would save with a 200, and the
// operator would find out at deploy or never. Rendered per agent by the CLI and
// the dashboard, as a sibling of the blank_briefs warning on the same response
// (JOH-344), which solves the same problem — a snapshot-time loss nobody can
// see — for a different field.
type snapshotFieldDrop struct {
	Agent  string   `json:"agent"`
	Fields []string `json:"fields"`
	// Reason is written for an operator deciding whether to act, not for someone
	// debugging the merge. See dropLaunchFieldsForeignToHarness for why it names
	// the harness rather than the event.
	Reason string `json:"reason"`
}

// dropLaunchFieldsForeignToHarness zeroes every carried launch field the merged
// profile's harness cannot accept, and reports what it took. Without it, a
// harness-only field carried onto a profile of another harness is not merely
// useless: resolveInt/Bool/StringLaunchField treat a profile whose harness
// matches the resolved launch as a self-consistent tier and fail it, so the
// member 400s at deploy and never spawns (TCL-1083).
//
// The validators are the harness package's own — the same ones the deploy path
// runs — rather than a hand-written list of which field belongs to which
// harness. A second list would drift from the first, and drift here is invisible
// until a deploy fails.
//
// Model and Effort are deliberately absent: they are never carried across a
// harness by this merge. Either the trace supplied both harness and model
// together, or the member was unobserved and both came from the stored profile
// together; neither path can pair one harness's model with another's name.
//
// NOT covered, deliberately: a carried tri-state FALSE. Several validators guard
// on `*requested && !h.Can…()`, so a `false` is accepted on any harness, keeps
// its tier, and suppresses a lower one that says true. That is a different bug —
// an observed default recorded as though it were a decision — tracked as
// TCL-1090. A green version of this function does not close it.
func dropLaunchFieldsForeignToHarness(out *db.SpawnProfile) *snapshotFieldDrop {
	name := harnessOrDefault(out.Harness)
	h, err := harness.ResolveSpawnable(name)
	if err != nil {
		// An unresolvable harness is not a judgement this function is entitled to
		// make: it would drop every gated field on the strength of a name it does
		// not understand. Leave the profile alone and let the deploy path speak.
		return nil
	}
	dropped := []string{}
	if out.ModelProxy != "" && out.ModelProxy != "off" && !h.SupportsModelProxy() {
		out.ModelProxy = ""
		dropped = append(dropped, "model_proxy")
	}
	if out.ContextWindowMax != 0 {
		if _, err := harness.ResolveCopilotContextWindow(h, out.ContextWindowMax); err != nil {
			out.ContextWindowMax = 0
			dropped = append(dropped, "context_window_max")
		}
	}
	if out.CopilotAPI != nil {
		if _, err := harness.ResolveCopilotAPI(h, out.CopilotAPI); err != nil {
			out.CopilotAPI = nil
			dropped = append(dropped, "copilot_api")
		}
	}
	if out.CodexAppServer != nil {
		if _, err := harness.ResolveCodexAppServer(h, out.CodexAppServer); err != nil {
			out.CodexAppServer = nil
			dropped = append(dropped, "codex_app_server")
		}
	}
	if out.FastMode != nil {
		if _, err := harness.ResolveFastMode(h, out.FastMode); err != nil {
			out.FastMode = nil
			dropped = append(dropped, "fast_mode")
		}
	}
	if out.SSHWorkaround != nil {
		if _, err := harness.ResolveSSHWorkaround(h, out.SSHWorkaround); err != nil {
			out.SSHWorkaround = nil
			dropped = append(dropped, "ssh_workaround")
		}
	}
	if out.PeerMessaging != nil {
		if _, err := harness.ResolvePeerMessaging(h, out.PeerMessaging); err != nil {
			out.PeerMessaging = nil
			dropped = append(dropped, "peer_messaging")
		}
	}
	if out.AutoMemory != nil {
		if _, err := harness.ResolveAutoMemory(h, out.AutoMemory); err != nil {
			out.AutoMemory = nil
			dropped = append(dropped, "auto_memory")
		}
	}
	if out.RemoteControl != nil {
		if _, err := harness.ResolveRemoteControl(h, *out.RemoteControl); err != nil {
			out.RemoteControl = nil
			dropped = append(dropped, "remote_control")
		}
	}
	if out.AutoReview != nil {
		if _, err := harness.ResolveAutoReview(h, *out.AutoReview); err != nil {
			out.AutoReview = nil
			dropped = append(dropped, "auto_review")
		}
	}
	if out.TrustDir != nil {
		if _, err := harness.ResolveTrustDir(h, *out.TrustDir); err != nil {
			out.TrustDir = nil
			dropped = append(dropped, "trust_dir")
		}
	}
	if out.ToolGovernance != "" {
		if _, err := harness.ValidateToolGovernance(h, out.ToolGovernance); err != nil {
			out.ToolGovernance = ""
			dropped = append(dropped, "tool_governance")
		}
	}
	if out.AskUserQuestionTimeout != "" {
		if _, err := harness.ResolveAskTimeoutMode(h, out.AskUserQuestionTimeout); err != nil {
			out.AskUserQuestionTimeout = ""
			dropped = append(dropped, "ask_user_question_timeout")
		}
	}
	if out.AutoCompactWindow != "" {
		if _, err := harness.ResolveAutoCompactWindow(h, out.AutoCompactWindow); err != nil {
			out.AutoCompactWindow = ""
			dropped = append(dropped, "auto_compact_window")
		}
	}
	if out.SandboxImplementation != "" {
		if _, err := validateSandboxImplementationForHarness(h, out.SandboxImplementation); err != nil {
			out.SandboxImplementation = ""
			dropped = append(dropped, "sandbox_implementation")
		}
	}
	if out.Approval != "" {
		if _, err := harness.ValidateApprovalPolicy(h, out.Approval); err != nil {
			out.Approval = ""
			dropped = append(dropped, "approval")
		}
	}
	if len(out.ContextFeatures) > 0 {
		if _, err := harness.ResolveContextFeatures(h, out.ContextFeatures); err != nil {
			out.ContextFeatures = nil
			dropped = append(dropped, "context_features")
		}
	}
	if len(dropped) == 0 {
		return nil
	}
	// The reason names the harness the fields were judged against, NOT the event
	// that produced it. "harness changed to claude" would be false for a profile
	// whose harness was never changed by anyone — it would send an operator
	// hunting for a change nobody made. What is always true, and always
	// actionable, is which harness the profile now resolves to.
	return &snapshotFieldDrop{
		Fields: dropped,
		Reason: fmt.Sprintf("template-local profile resolves to harness %q, which does not accept them", name),
	}
}

// snapshotGroupTemplate builds the template a from-group snapshot of
// this roster would store: one agent per group member, pure owners
// appended, descr/context taken from the group verbatim (the update
// path re-merges those against the existing template afterwards). In
// update mode (existing != nil) member names are recovered against the
// existing template via recoverTemplateAgentName so the re-snapshot
// round-trips.
// The second return maps each snapshotted agent NAME to whether its member was
// observed at all (templateAgentLaunch.Traced). The update-mode merge needs it
// and cannot recover it from the built template: an unobserved member and a
// fully-default observed one both produce the same blanks, and both can produce
// no inline profile at all.
//
// The third return discloses, per agent, the launch fields this snapshot
// OBSERVED but declined to pin because nobody chose them (TCL-1090). It cannot
// be recovered from the built template either — the whole point is that those
// fields are absent from it — so it travels out separately or not at all.
func snapshotGroupTemplate(name string, g *db.AgentGroup, members []*db.AgentGroupMember, ownerSet map[string]bool, existing *db.GroupTemplate) (*db.GroupTemplate, map[string]bool, []snapshotFieldDrop) {
	tracedByName := map[string]bool{}
	observedNotChosen := []snapshotFieldDrop{}
	t := &db.GroupTemplate{
		Name:           name,
		Descr:          g.Descr,
		DefaultContext: g.DefaultContext,
		Agents:         []db.GroupTemplateAgent{},
		// The group's owner-scope narrowing travels with the snapshot for the
		// same reason a member's grant scopes do (see addAgent below): a
		// template that dropped it would deploy forces whose owners hold
		// strictly MORE authority than the group it was traced from.
		OwnerScopesJSON: g.OwnerScopesJSON,
	}
	existingNames := map[string]bool{}
	if existing != nil {
		for _, a := range existing.Agents {
			existingNames[a.Name] = true
		}
	}
	memberSet := map[string]bool{}
	// "all" is the work_pattern broadcast target (and rejected as an
	// agent name at create/PATCH) — pre-claiming it makes the derive
	// fallback disambiguate a member literally titled "all" instead of
	// snapshotting an unroutable roster name.
	usedNames := map[string]bool{"all": true}
	addAgent := func(convID, role, descr string, owner bool) {
		name := ""
		if existing != nil {
			name = recoverTemplateAgentName(convID, g.Name, usedNames, existingNames)
		}
		if name == "" {
			name = deriveTemplateAgentName(convID, role, len(t.Agents)+1, usedNames)
		}
		// Snapshot the member's grants WITH their scopes: reading only the
		// slug list would bake a narrowly scoped live grant into the template
		// as an unscoped one, so every deploy of the snapshot would hand out
		// strictly more authority than the group it was traced from.
		perms := grantedOverridesForConv(convID)
		// Re-trace the member's OBSERVABLE launch fields (JOH-239) so a round-trip
		// preserves each member's launch shape. The spawn-profile REFERENCE is
		// blueprint curation, not observable — it is preserved by name-match in the
		// update path (handleTemplateFromGroup), like the per-agent brief.
		//
		// The traced fields + the member's live permission grants land in a
		// template-LOCAL profile (profile_inline), NOT the five legacy inline
		// fields + the legacy permissions list: the local profile is first-class
		// in the editor (viewable, editable, removable), so a fresh snapshot never
		// starts life behind the read-only "legacy inline" notice.
		launch := traceMemberLaunch(convID)
		tracedByName[name] = launch.Traced
		if len(launch.ObservedNotChosen) > 0 {
			// Reason names WHY the value does not speak for this member, not an
			// event: nothing changed, and an operator sent hunting for a change
			// that never happened is worse served than one told nothing.
			observedNotChosen = append(observedNotChosen, snapshotFieldDrop{
				Agent:  name,
				Fields: append([]string(nil), launch.ObservedNotChosen...),
				Reason: "observed on the live member but chosen by nobody (the harness default " +
					"resolved it), so the template does not pin it and the group/global " +
					"default profiles decide at deploy",
			})
		}
		var inline *db.SpawnProfile
		if launch.Harness != "" || launch.Model != "" || launch.ModelProxy != "" || launch.Effort != "" || launch.Sandbox != "" || launch.Approval != "" || launch.AutoReviewSet || launch.SSHWorkaroundSet || len(launch.ContextFeatures) > 0 || launch.AutoCompactWindow != "" || launch.ContextWindowMax > 0 || launch.CopilotAPISet || launch.CodexAppServerSet || launch.FastModeSet || launch.SandboxImplementation != "" || len(perms) > 0 {
			po := make(map[string]db.PermissionOverride, len(perms))
			for slug, override := range perms {
				po[slug] = override
			}
			inline = &db.SpawnProfile{
				Harness:               launch.Harness,
				Model:                 launch.Model,
				ModelProxy:            launch.ModelProxy,
				Effort:                launch.Effort,
				Sandbox:               launch.Sandbox,
				Approval:              launch.Approval,
				AutoCompactWindow:     launch.AutoCompactWindow,
				ContextWindowMax:      launch.ContextWindowMax,
				SandboxImplementation: launch.SandboxImplementation,
				CopilotAPI:            copilotAPIPointer(launch),
				CodexAppServer:        codexAppServerPointer(launch),
				FastMode:              fastModePointer(launch),
				PermissionOverrides:   po,
				ContextFeatures:       launch.ContextFeatures,
			}
			if launch.ContextFeaturesSet && inline.ContextFeatures == nil {
				// Traced, and the answer was "trims nothing". Recorded as a non-nil
				// empty map so an update-mode re-snapshot can tell this apart from an
				// unobservable member and actually clear the template's old trims.
				// It persists as absent either way (omitempty), so the stored shape is
				// unchanged.
				inline.ContextFeatures = map[string]string{}
			}
			if launch.AutoReviewSet {
				autoReview := launch.AutoReview
				inline.AutoReview = &autoReview
			}
			if launch.SSHWorkaroundSet {
				sshWorkaround := launch.SSHWorkaround
				inline.SSHWorkaround = &sshWorkaround
			}
		}
		t.Agents = append(t.Agents, db.GroupTemplateAgent{
			Ordinal:       len(t.Agents),
			Name:          name,
			Role:          role,
			Descr:         descr,
			IsOwner:       owner,
			Permissions:   []db.PermissionGrant{},
			ProfileInline: inline,
		})
	}
	for _, m := range members {
		memberSet[m.ConvID] = true
		addAgent(m.ConvID, m.Role, m.Descr, ownerSet[m.ConvID])
	}
	// Pure owners — owners that aren't members — still belong in the
	// snapshot so the template's owner isn't silently dropped. Collect
	// and sort them so the resulting ordinals are reproducible across
	// two snapshots of the same group (a bare map range is unordered).
	pureOwners := []string{}
	for ownerConv := range ownerSet {
		if !memberSet[ownerConv] {
			pureOwners = append(pureOwners, ownerConv)
		}
	}
	sort.Strings(pureOwners)
	for _, ownerConv := range pureOwners {
		addAgent(ownerConv, "owner", "", true)
	}
	return t, tracedByName, observedNotChosen
}

// fromGroupUpdateJSON is the update-mode from-group response: the fresh
// template plus a roster-diff report. templateJSON embeds flat, so
// callers that only know the create shape (the dashboard's editor-open
// path, older CLIs) keep working unchanged.
type fromGroupUpdateJSON struct {
	templateJSON
	Updated    bool     `json:"updated"`
	BriefsKept []string `json:"briefs_kept"`
	Added      []string `json:"added"`
	Removed    []string `json:"removed"`
	// BlankBriefs counts agents still left with a blank per-agent brief after
	// the snapshot — see countBlankBriefs (JOH-344).
	BlankBriefs int `json:"blank_briefs"`
	// Dropped discloses launch fields the merge refused to carry forward, per
	// agent. Empty on the overwhelmingly common path; when it is not, it is the
	// only place the operator learns a curated field went away, so it is rendered
	// by both the CLI and the dashboard like blank_briefs.
	Dropped []snapshotFieldDrop `json:"dropped,omitempty"`
}

// fromGroupCreateJSON is the create-mode from-group response: the fresh
// template plus the blank-brief count (JOH-344). A from-group snapshot cannot
// recover per-agent briefs from a live group, so every agent comes through
// blank; blank_briefs lets the CLI/dashboard warn that a deploy of this
// template would tell its agents nothing. templateJSON embeds flat, so a
// consumer that only reads the template shape is unaffected.
type fromGroupCreateJSON struct {
	templateJSON
	BlankBriefs int `json:"blank_briefs"`
	// Dropped carries the same per-agent disclosure as the update response. A
	// CREATE has no curated fields to lose, but it can still decline to pin an
	// OBSERVED-not-chosen value (TCL-1090), and that decision is invisible in the
	// resulting template — which is exactly why it has to be said here.
	Dropped []snapshotFieldDrop `json:"dropped,omitempty"`
}

// countBlankBriefs counts template agents whose per-agent brief
// (initial_message) is blank — the agents a deploy would spawn with nothing to
// do. A from-group snapshot has no briefs to trace from a live group, so a
// fresh snapshot's agents are all blank; an update re-snapshot keeps curated
// briefs on name-match, so this reflects only those still empty afterwards.
func countBlankBriefs(t *db.GroupTemplate) int {
	n := 0
	for _, a := range t.Agents {
		if strings.TrimSpace(a.InitialMessage) == "" {
			n++
		}
	}
	return n
}

// recoverTemplateAgentName maps a live member back to an agent of the
// existing template during an update re-snapshot: a member titled
// "<group>-<name>" (what instantiate names its spawns) — or exactly
// "<name>" — for a template agent <name> keeps that name. Returns ""
// when the member matches no existing template agent (or the name was
// already claimed), letting the caller fall back to deriveTemplateAgentName.
//
// Titles are agent-controlled (self.rename is default-granted), so this
// matching is deliberately content-integrity only: a member squatting on
// another's title can at most inherit that agent's curated BRIEF slot in
// the blueprint. Owner flags and permissions are always re-traced from
// the live conv, the re-snapshot itself is human-initiated, and the
// roster diff in the response makes a hijacked slot visible.
func recoverTemplateAgentName(convID, groupName string, used, existingNames map[string]bool) string {
	title := sanitizeAgentName(agent.FreshTitle(convID))
	if title == "" {
		return ""
	}
	candidates := []string{}
	if stripped, ok := strings.CutPrefix(title, groupName+"-"); ok {
		candidates = append(candidates, stripped)
	}
	candidates = append(candidates, title)
	for _, c := range candidates {
		if c != "" && existingNames[c] && !used[c] {
			used[c] = true
			return c
		}
	}
	return ""
}

// deriveTemplateAgentName picks a template-agent name when snapshotting
// a live group: the member's conversation title, sanitised into a
// slug-ish handle (the name becomes part of a /rename title at
// instantiate time). Falls back to the role, then to "agent-<n>", and
// disambiguates collisions with a numeric suffix. The human edits the
// template afterwards anyway, so this only needs to be a sensible
// starting point.
func deriveTemplateAgentName(convID, role string, ordinal int, used map[string]bool) string {
	base := sanitizeAgentName(agent.FreshTitle(convID))
	if base == "" {
		base = sanitizeAgentName(role)
	}
	if base == "" {
		base = fmt.Sprintf("agent-%d", ordinal)
	}
	name := base
	for i := 2; used[name]; i++ {
		name = fmt.Sprintf("%s-%d", base, i)
	}
	used[name] = true
	return name
}

// sanitizeAgentName reduces an arbitrary title to a template-agent
// name: runs of non-[A-Za-z0-9._-] characters collapse to a single
// dash, and leading/trailing dashes are trimmed.
func sanitizeAgentName(s string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range s {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-'
		if ok {
			b.WriteRune(r)
			lastDash = false
		} else if !lastDash {
			b.WriteRune('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}
