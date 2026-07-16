package db

import (
	"database/sql"
	"encoding/json"
	"strings"
)

// legacyCodexApprovalDefault is the exact approval flag agentd emitted for an
// omitted Codex approval before sessions recorded launch posture (schema 127).
// It is intentionally local to db: importing harness here would form a cycle.
const legacyCodexApprovalDefault = "never"

type legacySpawnApproval struct {
	Approval string `json:"approval"`
}

// inferLegacyCodexApproval reconstructs only histories whose effective launch
// policy is deterministic. This is provenance reconstruction, not a claim that
// "never" is less capable than every prompt-oriented Codex policy:
//
//   - HTTP/dashboard spawns have a verbatim initial_spawn_config. Omitted or
//     explicit never both launched as never under the historical daemon
//     contract, and every old relaunch also re-applied never.
//   - clone and successor generations were launched through old lifecycle
//     paths that unconditionally re-applied the Codex daemon default (never).
//
// An original spawn with an explicit different policy might since have been
// resumed through that defaulting path, and an imported/direct/template agent
// without a request snapshot has no trustworthy evidence. Those remain unknown
// and fail closed until a current tclaude relaunch records their posture.
func inferLegacyCodexApproval(createdVia, generationReason, initialSpawnConfig string) (string, bool) {
	switch strings.TrimSpace(generationReason) {
	case "reincarnate", "clear":
		return legacyCodexApprovalDefault, true
	}
	if strings.TrimSpace(createdVia) == "clone" {
		return legacyCodexApprovalDefault, true
	}
	if strings.TrimSpace(initialSpawnConfig) == "" {
		return "", false
	}
	var cfg legacySpawnApproval
	if err := json.Unmarshal([]byte(initialSpawnConfig), &cfg); err != nil {
		return "", false
	}
	switch strings.TrimSpace(cfg.Approval) {
	case "", legacyCodexApprovalDefault:
		return legacyCodexApprovalDefault, true
	default:
		return "", false
	}
}

// LegacyCodexApprovalForConv is the runtime compatibility guard for a schema-
// 127/older session row that still has approval_policy="". It uses the same
// evidence rules as the v128 backfill, so a row missed by migration (for
// example, written later by an older binary) is not permanently stranded.
func LegacyCodexApprovalForConv(convID string) (policy string, proven bool, err error) {
	d, err := Open()
	if err != nil {
		return "", false, err
	}
	var createdVia, generationReason, initialSpawnConfig string
	err = d.QueryRow(`
		SELECT COALESCE(a.created_via, ''), COALESCE(ac.reason, ''),
		       COALESCE(a.initial_spawn_config, '')
		  FROM sessions s
		  LEFT JOIN agent_conversations ac ON ac.conv_id = s.conv_id
		  LEFT JOIN agents a ON a.agent_id = CASE
		       WHEN s.agent_id <> '' THEN s.agent_id ELSE ac.agent_id END
		 WHERE s.conv_id = ? AND s.harness = 'codex'
		 ORDER BY s.updated_at DESC LIMIT 1`, strings.TrimSpace(convID)).
		Scan(&createdVia, &generationReason, &initialSpawnConfig)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	policy, proven = inferLegacyCodexApproval(createdVia, generationReason, initialSpawnConfig)
	return policy, proven, nil
}
