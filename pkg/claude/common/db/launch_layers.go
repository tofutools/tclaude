package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"
)

// AgentLaunchLayersVersion is the shape version of agents.launch_layers.
const AgentLaunchLayersVersion = 1

// Spawn-copy tier names, highest precedence first.
const (
	SpawnCopyTierInline        = "inline"
	SpawnCopyTierNamed         = "named"
	SpawnCopyTierGroupDefault  = "group_default"
	SpawnCopyTierGlobalDefault = "global_default"
)

// ExplicitAtSpawn is the provenance of a value given explicitly on the spawn.
const ExplicitAtSpawn = "explicit (spawn)"

// ExplicitAtResume is the provenance of a value given explicitly on a resume:
// it becomes the agent's explicit value from then on.
func ExplicitAtResume(at time.Time) string {
	return "explicit (resume, " + at.UTC().Format("2006-01-02") + ")"
}

// AgentLaunchLayers is what a relaunch resolves an agent's launch from, kept
// in layers so each can follow its own rule (docs/launch-resolution.md):
//
//   - SpawnCopy is the spawn profiles' contribution, copied once at first
//     spawn. It is one replaceable unit: re-applying an edited profile to an
//     agent replaces the whole copy (ReplaceAgentSpawnCopy) and touches
//     nothing else.
//   - Explicit holds the values given explicitly, at spawn or on a later
//     resume, keyed by launchspec field key.
//   - RelaunchOverrides are per-agent relaunch-policy rows, each naming its
//     source.
//
// Group settings, referenced profile contents and live values are not
// stored here: they are read again. Session projection never writes this
// column, unlike agents.relaunch_profile, which it rewrites on every session
// save.
//
// A nil *AgentLaunchLayers (an empty column) is an agent spawned before the
// layers existed. Its spawn copy is its stored launch shape: nothing is
// guessed about which profiles it came from.
type AgentLaunchLayers struct {
	Version           int                            `json:"version"`
	SpawnCopy         *AgentSpawnCopy                `json:"spawn_copy,omitempty"`
	Explicit          map[string]ExplicitLaunchValue `json:"explicit,omitempty"`
	RelaunchOverrides []RelaunchPolicyOverride       `json:"relaunch_overrides,omitempty"`
	// Unfolded marks layers written by a build whose CLI no longer folds a
	// --profile into the spawn request, so their explicit layer is accurate
	// and dropFoldedProfileValues must not touch it. Every write sets it: a
	// row is either new, or was decoded (and so repaired) before the write.
	Unfolded bool `json:"unfolded,omitempty"`
}

// AgentSpawnCopy is the shallow copy of the spawn profiles that took part in
// an agent's first spawn. Tiers is highest precedence first; empty means no
// profile took part.
type AgentSpawnCopy struct {
	CopiedAt time.Time       `json:"copied_at"`
	Tiers    []SpawnCopyTier `json:"tiers,omitempty"`
}

// SpawnCopyTier is one participating profile, with its identity at the time
// of the copy and its full content. Profile is the profile in the spawn
// profile export format (agentd's spawnProfileJSON), which has stable JSON
// names; db.SpawnProfile itself has none, so storing it directly would tie
// the stored copy to Go field names.
type SpawnCopyTier struct {
	Tier        string          `json:"tier"`
	ProfileID   int64           `json:"profile_id,omitempty"` // 0 for an inline (template) profile
	ProfileName string          `json:"profile_name,omitempty"`
	Profile     json.RawMessage `json:"profile"`
}

// ExplicitLaunchValue is one launchspec field's explicit value: the spawn
// request fields that carried it, and where it came from.
type ExplicitLaunchValue struct {
	Fields     map[string]json.RawMessage `json:"fields"`
	Provenance string                     `json:"provenance"`
	SetAt      time.Time                  `json:"set_at"`
}

// RelaunchPolicyOverride is a per-agent relaunch-policy row (launchspec field
// key and mode) with its source: "explicit", or the spawn profile it was
// copied from.
type RelaunchPolicyOverride struct {
	Field  string `json:"field"`
	Mode   string `json:"mode"`
	Source string `json:"source"`
}

func encodeAgentLaunchLayers(l *AgentLaunchLayers) (string, error) {
	if l == nil {
		return "", nil
	}
	out := *l
	out.Version = AgentLaunchLayersVersion
	out.Unfolded = true
	raw, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func decodeAgentLaunchLayers(raw string) (*AgentLaunchLayers, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var l AgentLaunchLayers
	if err := json.Unmarshal([]byte(raw), &l); err != nil {
		return nil, fmt.Errorf("decode agents.launch_layers: %w", err)
	}
	if l.Version > AgentLaunchLayersVersion {
		return nil, fmt.Errorf("agents.launch_layers version %d is newer than this build (%d)",
			l.Version, AgentLaunchLayersVersion)
	}
	if !l.Unfolded {
		dropFoldedProfileValues(&l)
		l.Unfolded = true
	}
	return &l, nil
}

// foldedProfileFields are the request fields `tclaude agent spawn --profile`
// used to fill from the named profile before sending, by the launchspec key
// they are recorded under and the profile export name holding the same value.
// role_ref and role_refs are compared as one role list (foldedRoleRefs).
var foldedProfileFields = map[string]map[string]string{
	"roles":                 {"Role": "role", "RoleRef": "", "RoleRefs": ""},
	"context_window":        {"ContextWindowMax": "context_window_max"},
	"include_group_context": {"IncludeGroupContext": "include_group_default_context"},
}

// dropFoldedProfileValues repairs the explicit layer of agents spawned through
// the CLI with --profile before it stopped folding the profile into the
// request: such a value was recorded as explicit although the profile chose
// it. It runs only on rows written before that (AgentLaunchLayers.Unfolded),
// against the copy recorded at spawn: the first write after it persists the
// result, so a later re-applied copy never decides what was explicit. An explicit value equal to the named profile's in the agent's own spawn
// copy is dropped. The value is the same either way, so the only effect is
// that a re-applied copy may change it, as it would for a profile value.
func dropFoldedProfileValues(l *AgentLaunchLayers) {
	if len(l.Explicit) == 0 || l.SpawnCopy == nil {
		return
	}
	var named map[string]json.RawMessage
	for _, tier := range l.SpawnCopy.Tiers {
		if tier.Tier == SpawnCopyTierNamed {
			if json.Unmarshal(tier.Profile, &named) != nil {
				return
			}
			break
		}
	}
	if named == nil {
		return
	}
	roleRefs := foldedRoleRefs(named)
	for key, fields := range foldedProfileFields {
		value, ok := l.Explicit[key]
		if !ok {
			continue
		}
		for field, profileName := range fields {
			recorded, ok := value.Fields[field]
			if !ok {
				continue
			}
			var same bool
			switch field {
			case "RoleRef":
				var ref string
				same = json.Unmarshal(recorded, &ref) == nil && len(roleRefs) > 0 && ref == roleRefs[0]
			case "RoleRefs":
				var refs []string
				same = json.Unmarshal(recorded, &refs) == nil && slices.Equal(refs, roleRefs)
			default:
				same = sameJSONValue(recorded, named[profileName])
			}
			if same {
				delete(value.Fields, field)
			}
		}
		if len(value.Fields) == 0 {
			delete(l.Explicit, key)
		}
	}
	if len(l.Explicit) == 0 {
		l.Explicit = nil
	}
}

// foldedRoleRefs is the role list the CLI folded from a profile: role_refs,
// else role_ref alone.
func foldedRoleRefs(profile map[string]json.RawMessage) []string {
	var refs []string
	_ = json.Unmarshal(profile["role_refs"], &refs)
	if len(refs) == 0 {
		var ref string
		if json.Unmarshal(profile["role_ref"], &ref) == nil && strings.TrimSpace(ref) != "" {
			refs = []string{strings.TrimSpace(ref)}
		}
	}
	return refs
}

func sameJSONValue(a, b json.RawMessage) bool {
	if len(b) == 0 {
		return false
	}
	var va, vb any
	return json.Unmarshal(a, &va) == nil && json.Unmarshal(b, &vb) == nil && reflect.DeepEqual(va, vb)
}

// SetAgentLaunchLayers writes an agent's launch layers whole. nil clears
// them, which makes the agent read as pre-change. A no-op for an unknown
// agent.
func SetAgentLaunchLayers(agentID string, layers *AgentLaunchLayers) error {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return errors.New("SetAgentLaunchLayers: agent_id required")
	}
	raw, err := encodeAgentLaunchLayers(layers)
	if err != nil {
		return err
	}
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`UPDATE agents SET launch_layers = ? WHERE agent_id = ?`, raw, agentID)
	return err
}

// SetAgentLaunchLayersIfUnset records an agent's launch layers unless it
// already has some, and reports whether it wrote them. The layers are taken
// once: whichever code path created the agent row (the spawn itself, a
// reserved pending spawn, the session-start hook), the first recording wins
// and a later one is a no-op.
func SetAgentLaunchLayersIfUnset(agentID string, layers *AgentLaunchLayers) (bool, error) {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return false, errors.New("SetAgentLaunchLayersIfUnset: agent_id required")
	}
	if layers == nil {
		return false, nil
	}
	raw, err := encodeAgentLaunchLayers(layers)
	if err != nil {
		return false, err
	}
	d, err := Open()
	if err != nil {
		return false, err
	}
	res, err := d.Exec(`UPDATE agents SET launch_layers = ? WHERE agent_id = ? AND launch_layers = ''`, raw, agentID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// AgentLaunchLayersFor loads an agent's launch layers. nil with no error is
// a pre-change agent, or an unknown one.
func AgentLaunchLayersFor(agentID string) (*AgentLaunchLayers, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	var raw string
	err = d.QueryRow(`SELECT launch_layers FROM agents WHERE agent_id = ?`,
		strings.TrimSpace(agentID)).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeAgentLaunchLayers(raw)
}

// AgentLaunchLayersForConv loads the launch layers of the agent that owns a
// conversation generation.
func AgentLaunchLayersForConv(convID string) (*AgentLaunchLayers, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	var raw string
	err = d.QueryRow(`
		SELECT a.launch_layers
		FROM agent_conversations ac
		JOIN agents a ON a.agent_id = ac.agent_id
		WHERE ac.conv_id = ?`, strings.TrimSpace(convID)).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeAgentLaunchLayers(raw)
}

// ReplaceAgentSpawnCopy swaps an agent's spawn copy for a new one and keeps
// its other layers. It is the primitive behind re-applying an edited spawn
// profile to chosen agents. A pre-change agent gets layers with just the
// copy.
func ReplaceAgentSpawnCopy(agentID string, spawnCopy AgentSpawnCopy) error {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return errors.New("ReplaceAgentSpawnCopy: agent_id required")
	}
	d, err := Open()
	if err != nil {
		return err
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := replaceAgentSpawnCopyTx(tx, agentID, spawnCopy); err != nil {
		return err
	}
	return tx.Commit()
}

func replaceAgentSpawnCopyTx(tx *sql.Tx, agentID string, spawnCopy AgentSpawnCopy) error {
	var raw string
	if err := tx.QueryRow(`SELECT launch_layers FROM agents WHERE agent_id = ?`, agentID).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("ReplaceAgentSpawnCopy: unknown agent %s", agentID)
		}
		return err
	}
	layers, err := decodeAgentLaunchLayers(raw)
	if err != nil {
		return err
	}
	if layers == nil {
		layers = &AgentLaunchLayers{}
	}
	layers.SpawnCopy = &spawnCopy
	encoded, err := encodeAgentLaunchLayers(layers)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE agents SET launch_layers = ? WHERE agent_id = ?`, encoded, agentID)
	return err
}

// ErrRelaunchProfileChanged reports that an agent's relaunch profile changed
// between the read a write was computed from and the write.
var ErrRelaunchProfileChanged = errors.New("the agent's launch settings changed while the spawn copy was being applied; apply it again")

// AgentRelaunchProfileRaw returns an agent's stored relaunch profile and its
// raw encoding, which ApplyAgentSpawnCopy takes back to detect a concurrent
// write.
func AgentRelaunchProfileRaw(agentID string) (*AgentRelaunchProfile, string, error) {
	d, err := Open()
	if err != nil {
		return nil, "", err
	}
	var raw string
	err = d.QueryRow(`SELECT relaunch_profile FROM agents WHERE agent_id = ?`, strings.TrimSpace(agentID)).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", fmt.Errorf("unknown agent %s", agentID)
	}
	if err != nil {
		return nil, "", err
	}
	profile, err := decodeAgentRelaunchProfile(raw)
	return profile, raw, err
}

// ApplyAgentSpawnCopy re-applies a spawn copy to an agent in one
// transaction: it writes the relaunch profile resolved from the copy and
// replaces the copy. expectRaw is the relaunch profile the resolution was
// computed from (AgentRelaunchProfileRaw); if it changed meanwhile, nothing is
// written and ErrRelaunchProfileChanged is returned.
func ApplyAgentSpawnCopy(agentID, convID, expectRaw string, profile AgentRelaunchProfile, spawnCopy AgentSpawnCopy) error {
	agentID = strings.TrimSpace(agentID)
	encoded, err := encodeRelaunchProfile(profile)
	if err != nil {
		return err
	}
	d, err := Open()
	if err != nil {
		return err
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec(`UPDATE agents SET relaunch_profile = ?
		WHERE agent_id = ? AND current_conv_id = ? AND relaunch_profile = ?`,
		encoded, agentID, convID, expectRaw)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrRelaunchProfileChanged
	}
	if err := replaceAgentSpawnCopyTx(tx, agentID, spawnCopy); err != nil {
		return err
	}
	return tx.Commit()
}

// AgentsWithSpawnCopyOf returns the non-retired agents whose spawn copy
// includes the spawn profile with this ID: the agents that keep their copy
// when the profile is edited. Retired agents are left out in the query, which
// idx_agents_active serves, so the scan stays proportional to the active
// roster.
const agentsWithSpawnCopyOfQuery = `
		SELECT DISTINCT a.agent_id
		FROM agents a, json_each(a.launch_layers, '$.spawn_copy.tiers') t
		WHERE a.retired_at IS NULL AND a.launch_layers <> '' AND json_valid(a.launch_layers)
		  AND json_extract(t.value, '$.profile_id') = ?
		ORDER BY a.agent_id`

func AgentsWithSpawnCopyOf(profileID int64) ([]string, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(agentsWithSpawnCopyOfQuery, profileID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
