package agentd

import (
	"bytes"
	"encoding/json"
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
// would change for each.
//
// Applying waits on two pieces of work. Relaunch must read the spawn copy
// (tcl-an26ed); today it reads agents.relaunch_profile, so a replaced copy
// would change nothing. And grants need their source recorded (tcl-kdpz4l):
// birth grants are written with the spawner as granter, merged from every
// profile, role and the request, so the profile's own grants cannot be told
// apart from the agent's other grants.

const (
	profileReapplyBlocked    = "Takes effect once relaunch reads the spawn copy (tcl-an26ed)."
	profileReapplyGrantsNote = "Grants: shown only, not applied. tclaude does not yet record which of an agent's grants came from this profile (tcl-kdpz4l)."
)

// Field outcomes in a re-apply preview.
const (
	reapplyChanges     = "changes"
	reapplyKeptFixed   = "kept (fixed)"
	reapplyKeptExplict = "kept (explicit)"
	reapplyKeptSticky  = "kept (relaunch override)"
	reapplySpawnOnly   = "not reapplied (spawn only)"
)

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

// profileReapplyFields diffs an agent's copy of a profile against the profile
// now, field by field, saying what a re-apply would do with each change.
func profileReapplyFields(copied, current map[string]json.RawMessage, layers *db.AgentLaunchLayers) []profileReapplyField {
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
			case layers.Explicit[field.Key].Provenance != "":
				row.Status = reapplyKeptExplict
			case sticky[field.Key]:
				row.Status = reapplyKeptSticky
			case field.Layer == launchspec.LayerSpawnOnly:
				row.Status = reapplySpawnOnly
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

// profileGrantsOf is what a profile confers at birth: its overrides on top of
// its roles' grants, plus is_owner.
func profileGrantsOf(p *db.SpawnProfile) map[string]db.PermissionOverride {
	out, err := profileRoleGrants(p)
	if err != nil || out == nil {
		out = map[string]db.PermissionOverride{}
	}
	for slug, o := range p.PermissionOverrides {
		out[slug] = o
	}
	if isTrue(p.IsOwner) {
		out["is_owner"] = db.PermissionOverride{Effect: db.PermEffectGrant}
	}
	return out
}

func profileReapplyGrants(copied, current *db.SpawnProfile) []profileReapplyGrant {
	before, after := profileGrantsOf(copied), profileGrantsOf(current)
	slugs := map[string]bool{}
	for s := range before {
		slugs[s] = true
	}
	for s := range after {
		slugs[s] = true
	}
	out := []profileReapplyGrant{}
	for slug := range slugs {
		b, bok := before[slug]
		a, aok := after[slug]
		if bok == aok && b == a {
			continue
		}
		out = append(out, profileReapplyGrant{Slug: slug, From: grantWord(b, bok), To: grantWord(a, aok)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out
}

// profileReapplyPreview lists the active agents holding a copy of prof, with
// what a re-apply would change for each.
func profileReapplyPreview(prof *db.SpawnProfile) ([]profileReapplyAgent, error) {
	ids, err := db.AgentsWithSpawnCopyOf(prof.ID)
	if err != nil {
		return nil, err
	}
	currentRaw, err := json.Marshal(profileToJSON(prof))
	if err != nil {
		return nil, err
	}
	current := profileJSONMap(currentRaw)
	out := []profileReapplyAgent{}
	for _, id := range ids {
		layers, err := db.AgentLaunchLayersFor(id)
		if err != nil || layers == nil || layers.SpawnCopy == nil {
			continue
		}
		var tier *db.SpawnCopyTier
		for i := range layers.SpawnCopy.Tiers {
			if layers.SpawnCopy.Tiers[i].ProfileID == prof.ID {
				tier = &layers.SpawnCopy.Tiers[i]
				break
			}
		}
		if tier == nil {
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
		row.Fields = profileReapplyFields(profileJSONMap(tier.Profile), current, layers)
		var copied agent.SpawnProfileJSON
		if json.Unmarshal(tier.Profile, &copied) == nil {
			if was, fail := buildProfileFromJSON(copied); fail == nil {
				row.Grants = profileReapplyGrants(was, prof)
			}
		}
		out = append(out, row)
	}
	return out, nil
}

// handleSpawnProfileRunningAgents is GET /v1/spawn-profiles/{name}/running-agents:
// the re-apply preview. Applying is not offered yet (see the file comment).
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
	agents, err := profileReapplyPreview(prof)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"profile":     prof.Name,
		"agents":      agents,
		"apply":       map[string]any{"enabled": false, "reason": profileReapplyBlocked},
		"grants_note": profileReapplyGrantsNote,
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
