package agentd

import "github.com/tofutools/tclaude/pkg/claude/common/db"

// ApplySpawnCopyForTest re-applies a spawn copy to an agent (applySpawnCopy)
// and reports a refusal as its kind, "" on success.
func ApplySpawnCopyForTest(agentID string, spawnCopy db.AgentSpawnCopy) (kind, msg string) {
	if fail := applySpawnCopy(agentID, spawnCopy); fail != nil {
		return fail.Kind, fail.Msg
	}
	return "", ""
}

// SpawnCopyTierForTest copies a profile into a spawn-copy tier, as a first
// spawn does.
func SpawnCopyTierForTest(tier string, prof *db.SpawnProfile) db.SpawnCopyTier {
	return *spawnCopyTier(tier, prof)
}
