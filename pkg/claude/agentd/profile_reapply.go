package agentd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/launchspec"
)

// Re-applying an edited spawn profile to the agents spawned from it
// (tcl-sqs8sr). A spawn profile is copied at first spawn (launch_layers'
// spawn copy), so an edit reaches only new spawns. This file is the read-only
// half: which running agents hold a copy of a profile, and what a re-apply
// would change for each. profile_reapply_apply.go applies it, one agent at a
// time (tcl-hxuwnc). Grants are recorded with their source
// (agent_permissions.birth_source, tcl-kdpz4l) for agents spawned since v307,
// and a re-apply changes the ones that still carry the profile's record
// (profile_reapply_grants.go, tcl-fg2kqk); the preview shows each agent's
// outcome per slug.

const profileReapplyGrantsNote = "Grants: a re-apply changes only the grants the profile gave at spawn that nobody has changed since. " +
	"Explicit, sudo and changed grants are kept, a revoked grant is not given back, and group ownership is shown only. " +
	"Role text: the role label and brief follow in the next update."

// Field outcomes in a re-apply preview.
const (
	reapplyChanges     = "changes"
	reapplyKeptFixed   = "kept (fixed)"
	reapplyKeptExplict = "kept (explicit)"
	reapplyKeptSticky  = "kept (relaunch override)"
	reapplySpawnOnly   = "not reapplied (spawn only)"
	reapplyNotYet      = "not reapplied yet"
)

// profileReapplyNotYet are spawn-copy fields the re-apply operation does not
// carry yet: the model proxy needs the models.proxy gate a first spawn runs.
// Marked so that a change to one is never silently dropped.
var profileReapplyNotYet = map[string]bool{"model_proxy": true}

// reapplyRolesStatus is the roles row's status: the role's grants are
// re-applied (the grant rows), its label and brief are not yet.
const reapplyRolesStatus = "grants reapplied; role text: next update"

// reapplyMergesExplicit are fields whose explicit entries are laid over the
// profile's rather than replacing them, so a profile edit still changes them.
var reapplyMergesExplicit = map[string]bool{"profile_env": true}

// profileReapplyUsageNote covers the profile's usage-profile links, which live
// in the usage rules store rather than the profile, so no field row shows them.
const profileReapplyUsageNote = "Usage profile links: re-applied from the profile's current links, unless the agent's spawn named its own usage profiles."

// profileReapplyIgnored are a profile's own bookkeeping, not launch inputs.
var profileReapplyIgnored = map[string]bool{
	"name": true, "aliases": true, "disabled": true, "disabled_reason": true,
	"operator_only": true, "created_at": true, "updated_at": true,
}

// profileReapplyGrantKeys are shown as grant changes rather than fields.
var profileReapplyGrantKeys = map[string]bool{"permission_overrides": true, "is_owner": true}

// spawnProfileJSONGoNames maps a spawn-profile JSON name to its Go field
// name, which is what launchspec rows name (ProfileFields).
var spawnProfileJSONGoNames = func() map[string]string {
	out := map[string]string{}
	t := reflect.TypeOf(agent.SpawnProfileJSON{})
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name != "" && name != "-" {
			out[name] = f.Name
		}
	}
	return out
}()

// launchFieldForProfileField finds the launch registry row a profile field
// feeds.
func launchFieldForProfileField(goName string) (launchspec.Field, bool) {
	for _, f := range launchspec.Fields {
		for _, pf := range f.ProfileFields {
			if pf == goName {
				return f, true
			}
		}
	}
	return launchspec.Field{}, false
}

type profileReapplyField struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	From   string `json:"from"`
	To     string `json:"to"`
	Status string `json:"status"`
}

type profileReapplyGrant struct {
	Slug string `json:"slug"`
	From string `json:"from"`
	To   string `json:"to"`
	// Status is what a re-apply does to the agent's row (db.BirthPermission*),
	// or "shown only" for group ownership.
	Status string `json:"status"`
}

type profileReapplyAgent struct {
	AgentID  string                `json:"agent_id"`
	ConvID   string                `json:"conv_id"`
	Title    string                `json:"title,omitempty"`
	Online   bool                  `json:"online"`
	Tier     string                `json:"tier"`
	CopiedAt string                `json:"copied_at,omitempty"`
	Fields   []profileReapplyField `json:"fields"`
	Grants   []profileReapplyGrant `json:"grants"`
	// GrantsError replaces Grants when the copy's grants could not be read.
	GrantsError string `json:"grants_error,omitempty"`
}

func compactJSON(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var buf bytes.Buffer
	if json.Compact(&buf, raw) != nil {
		return string(raw)
	}
	return buf.String()
}

func profileJSONMap(raw []byte) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	_ = json.Unmarshal(raw, &out)
	return out
}

// speaks reports whether a copied profile value sets the field: a launch takes
// each field from the highest tier that speaks.
func speaks(raw json.RawMessage) bool {
	switch compactJSON(raw) {
	case "", `""`, "[]", "{}":
		return false
	}
	return true
}

// profileReapplyFields diffs an agent's copy of a profile against the profile
// now, field by field, saying what a re-apply would do with each change.
// higher are the copies of the tiers above this profile's: a field one of them
// sets is not taken from this profile, so an edit to it changes nothing.
func profileReapplyFields(copied, current map[string]json.RawMessage, layers *db.AgentLaunchLayers, higher []db.SpawnCopyTier) []profileReapplyField {
	shadowedBy := func(key string) string {
		for _, t := range higher {
			if speaks(profileJSONMap(t.Profile)[key]) {
				if t.ProfileName != "" {
					return t.ProfileName
				}
				return t.Tier
			}
		}
		return ""
	}
	sticky := map[string]bool{}
	for _, o := range layers.RelaunchOverrides {
		if o.Source == "explicit" {
			sticky[o.Field] = true
		}
	}
	keys := map[string]bool{}
	for k := range copied {
		keys[k] = true
	}
	for k := range current {
		keys[k] = true
	}
	out := []profileReapplyField{}
	for key := range keys {
		if profileReapplyIgnored[key] || profileReapplyGrantKeys[key] {
			continue
		}
		from, to := compactJSON(copied[key]), compactJSON(current[key])
		if from == to {
			continue
		}
		row := profileReapplyField{Key: key, Label: key, From: from, To: to, Status: reapplyChanges}
		if field, ok := launchFieldForProfileField(spawnProfileJSONGoNames[key]); ok {
			row.Label = field.Label
			switch {
			case field.Layer == launchspec.LayerFixed:
				row.Status = reapplyKeptFixed
			case layers.Explicit[field.Key].Provenance != "" && !reapplyMergesExplicit[field.Key]:
				row.Status = reapplyKeptExplict
			case sticky[field.Key]:
				row.Status = reapplyKeptSticky
			case field.Layer == launchspec.LayerSpawnCopy && shadowedBy(key) != "":
				row.Status = "kept (set by " + shadowedBy(key) + ")"
			case field.Layer == launchspec.LayerSpawnOnly:
				row.Status = reapplySpawnOnly
			case profileReapplyNotYet[field.Key]:
				row.Status = reapplyNotYet
			case field.Key == "roles":
				row.Status = reapplyRolesStatus
			}
		}
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

func grantWord(o db.PermissionOverride, ok bool) string {
	if !ok {
		return ""
	}
	word := string(o.Effect)
	if o.Scope != "" {
		word += " " + o.Scope
	}
	return word
}

// reapplyGrantShownOnly is the status of a grant change a re-apply does not
// make: group ownership has no birth record to re-apply by.
const reapplyGrantShownOnly = "shown only"

// reapplyGrantNotApplied is the status of a grant change in a part of the
// profile (its roles, or its overrides) that did not apply at the agent's
// spawn, so a re-apply does not make it apply now.
const reapplyGrantNotApplied = "not applied (not in effect at spawn)"

// profileReapplyGrants is what re-applying prof to agentID would do to its
// grants, given its copy of the profile (copied): the planned outcome per
// slug, read from a rolled-back run of the same writes.
func profileReapplyGrants(agentID string, copied, prof *db.SpawnProfile) ([]profileReapplyGrant, error) {
	before := *copied
	before.Name = prof.Name
	plan, err := reapplyGrantPlan(agentID, prof.ID, &before, prof, "preview")
	if err != nil {
		return nil, err
	}
	results, err := db.PlanAgentBirthPermissions(agentID, *plan)
	if err != nil {
		return nil, err
	}
	out := []profileReapplyGrant{}
	planned := map[string]bool{}
	for _, res := range results {
		planned[res.Slug] = true
		out = append(out, profileReapplyGrant{Slug: res.Slug, From: res.From, To: res.To, Status: res.Outcome})
	}
	// A change to a part of the profile that did not take effect at the spawn
	// (a higher tier or the request replaced it, or it was empty) is not
	// planned at all; list it so the edit is not silently dropped.
	if was, _, err := profileBirthGrants(&before, true, true); err == nil {
		if now, _, err := profileBirthGrants(prof, true, true); err == nil {
			for _, slug := range changedGrantSlugs(was, now) {
				if planned[slug] {
					continue
				}
				b, bok := was[slug]
				a, aok := now[slug]
				out = append(out, profileReapplyGrant{Slug: slug, From: grantWord(b, bok), To: grantWord(a, aok), Status: reapplyGrantNotApplied})
			}
		}
	}
	if was, now := isTrue(copied.IsOwner), isTrue(prof.IsOwner); was != now {
		out = append(out, profileReapplyGrant{Slug: "is_owner", From: grantWord(db.PermissionOverride{Effect: db.PermEffectGrant}, was),
			To: grantWord(db.PermissionOverride{Effect: db.PermEffectGrant}, now), Status: reapplyGrantShownOnly})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out, nil
}

// copiedGrantInputs reads the grant inputs straight from a copied profile,
// without the save-time validation that would refuse a copy naming a role or
// saved prompt deleted since.
func copiedGrantInputs(raw json.RawMessage) (*db.SpawnProfile, error) {
	var copied agent.SpawnProfileJSON
	if err := json.Unmarshal(raw, &copied); err != nil {
		return nil, err
	}
	return &db.SpawnProfile{
		RoleRef: copied.RoleRef, RoleRefs: copied.RoleRefs,
		IsOwner: copied.IsOwner, PermissionOverrides: copied.PermissionOverrides,
	}, nil
}

// profileReapplyPreview lists the active agents holding a copy of prof, with
// what a re-apply would change for each. unreadable counts the agents found
// whose launch layers could not be read, so the list and the save count agree.
func profileReapplyPreview(prof *db.SpawnProfile) (out []profileReapplyAgent, unreadable int, err error) {
	ids, err := db.AgentsWithSpawnCopyOf(prof.ID)
	if err != nil {
		return nil, 0, err
	}
	currentRaw, err := json.Marshal(profileToJSON(prof))
	if err != nil {
		return nil, 0, err
	}
	current := profileJSONMap(currentRaw)
	out = []profileReapplyAgent{}
	for _, id := range ids {
		layers, err := db.AgentLaunchLayersFor(id)
		if err != nil || layers == nil || layers.SpawnCopy == nil {
			unreadable++
			continue
		}
		var tier *db.SpawnCopyTier
		var higher []db.SpawnCopyTier
		for i := range layers.SpawnCopy.Tiers {
			if layers.SpawnCopy.Tiers[i].ProfileID == prof.ID {
				tier = &layers.SpawnCopy.Tiers[i]
				higher = layers.SpawnCopy.Tiers[:i]
				break
			}
		}
		if tier == nil {
			unreadable++
			continue
		}
		row := profileReapplyAgent{AgentID: id, Tier: tier.Tier, Fields: []profileReapplyField{}, Grants: []profileReapplyGrant{}}
		if !layers.SpawnCopy.CopiedAt.IsZero() {
			row.CopiedAt = layers.SpawnCopy.CopiedAt.UTC().Format("2006-01-02 15:04")
		}
		if conv, _ := db.CurrentConvForAgent(id); conv != "" {
			row.ConvID = conv
			row.Online = isConvOnline(conv)
			if idx, _ := db.GetConvIndex(conv); idx != nil {
				row.Title = agent.DisplayTitle(idx)
			}
		}
		row.Fields = profileReapplyFields(profileJSONMap(tier.Profile), current, layers, higher)
		was, err := copiedGrantInputs(tier.Profile)
		if err != nil {
			row.GrantsError = "this agent's copy of the profile could not be read"
		} else if row.Grants, err = profileReapplyGrants(id, was, prof); err != nil {
			row.Grants, row.GrantsError = []profileReapplyGrant{}, "grants: "+err.Error()
		}
		out = append(out, row)
	}
	return out, unreadable, nil
}

// handleSpawnProfileRunningAgents is GET /v1/spawn-profiles/{name}/running-agents:
// the re-apply preview. POST .../reapply applies it to one agent.
func handleSpawnProfileRunningAgents(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePermission(w, r, PermProfilesManage); !ok {
		return
	}
	prof, err := db.ResolveSpawnProfile(r.PathValue("name"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	if prof == nil {
		writeError(w, http.StatusNotFound, "not_found", "no such profile")
		return
	}
	agents, unreadable, err := profileReapplyPreview(prof)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	notes := []string{profileReapplyUsageNote}
	if note := reapplyDefaultTierNote(agents); note != "" {
		notes = append(notes, note)
	}
	if unreadable > 0 {
		notes = append(notes, fmt.Sprintf("%d more agent(s) hold a copy of this profile, but their launch layers could not be read, so they are not listed.", unreadable))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"profile":    prof.Name,
		"agents":     agents,
		"unreadable": unreadable,
		"apply": map[string]any{"enabled": true, "reason": "A stopped agent takes the new copy on its next launch; a running, fully idle " +
			"agent is stopped, updated and resumed; a busy agent is skipped."},
		"grants_note": profileReapplyGrantsNote,
		"notes":       notes,
	})
}

// runningAgentsWithCopy counts the active agents holding a copy of a profile,
// for the "applies to new spawns" note on save. -1 when it cannot be read.
func runningAgentsWithCopy(profileID int64) int {
	ids, err := db.AgentsWithSpawnCopyOf(profileID)
	if err != nil {
		return -1
	}
	return len(ids)
}
