package agentd

import (
	"encoding/json"
	"log/slog"
	"reflect"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/launchspec"
)

// Recording of an agent's launch layers at first spawn (db.AgentLaunchLayers):
// the spawn profiles that took part, copied whole with their tier and ID, and
// the values the spawn request set explicitly. Nothing here reads them back
// yet; the relaunch paths will (docs/launch-resolution.md).

// spawnCopyTier copies one participating profile, in the profile export
// format so the stored copy has stable JSON names. nil is no tier.
func spawnCopyTier(tier string, prof *db.SpawnProfile) *db.SpawnCopyTier {
	if prof == nil {
		return nil
	}
	raw, err := json.Marshal(profileToJSON(prof))
	if err != nil {
		slog.Warn("spawn: failed to copy spawn profile", "profile", prof.Name, "error", err)
		return nil
	}
	return &db.SpawnCopyTier{Tier: tier, ProfileID: prof.ID, ProfileName: prof.Name, Profile: raw}
}

// spawnCopyTiers keeps the non-nil tiers, in order.
func spawnCopyTiers(tiers ...*db.SpawnCopyTier) []db.SpawnCopyTier {
	var out []db.SpawnCopyTier
	for _, t := range tiers {
		if t != nil {
			out = append(out, *t)
		}
	}
	return out
}

// captureExplicitLaunch records, per registry field, the spawn request fields
// the caller set. It reads the request as decoded, before any profile tier
// fills it in, so only the caller's own choices count as explicit. Spawn-only
// fields (the brief, access grants, the call's own shape) are left out: the
// first spawn consumes them and a relaunch has no use for them.
func captureExplicitLaunch(req *agent.SpawnRequest, at time.Time) map[string]db.ExplicitLaunchValue {
	if req == nil {
		return nil
	}
	v := reflect.ValueOf(*req)
	out := map[string]db.ExplicitLaunchValue{}
	for _, f := range launchspec.Fields {
		if f.Layer == launchspec.LayerSpawnOnly {
			continue
		}
		fields := map[string]json.RawMessage{}
		for _, name := range f.RequestFields {
			fv := v.FieldByName(name)
			if !fv.IsValid() || fv.IsZero() {
				continue
			}
			raw, err := json.Marshal(fv.Interface())
			if err != nil {
				slog.Warn("spawn: failed to record explicit launch value", "field", name, "error", err)
				continue
			}
			fields[name] = raw
		}
		if len(fields) > 0 {
			out[f.Key] = db.ExplicitLaunchValue{Fields: fields, Provenance: db.ExplicitAtSpawn, SetAt: at}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// spawnLaunchLayers assembles the layers a first spawn records: the caller's
// own tiers (inline, named) above the default tiers. A profile that fills two
// slots (a trigger worker's profile sits in the group-default slot as well)
// is recorded once, at its higher tier.
//
// nil means nothing is recorded, and the agent reads as pre-change: a launch
// whose tiers this process did not resolve (an enrollment the pending-spawn
// sweeper rebuilt from the pending row) or one that brings an existing
// identity (a federation arrival or bundle move) has no first-spawn copy to
// take here.
func spawnLaunchLayers(p spawnParams, at time.Time) *db.AgentLaunchLayers {
	if !p.spawnCopyResolved || p.Arrival != nil || p.BundleHistory != nil {
		return nil
	}
	seen := map[int64]bool{}
	tiers := []db.SpawnCopyTier{}
	for _, group := range [][]db.SpawnCopyTier{p.spawnCopyCallerTiers, p.spawnCopyDefaultTiers} {
		for _, t := range group {
			if t.ProfileID != 0 {
				if seen[t.ProfileID] {
					continue
				}
				seen[t.ProfileID] = true
			}
			tiers = append(tiers, t)
		}
	}
	return &db.AgentLaunchLayers{
		SpawnCopy: &db.AgentSpawnCopy{CopiedAt: at, Tiers: tiers},
		Explicit:  p.explicitLaunch,
	}
}
