package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"github.com/tofutools/tclaude/internal/backend/model"
	"time"
)

// Membership labels end with that membership. Bump the agent revision so an
// older configuration form cannot restore a removed override accidentally.
func removeGroupDisplayLabels(ctx context.Context, tx *sql.Tx, group model.GroupID, agent model.AgentID, at time.Time) error {
	var raw []byte
	if err := tx.QueryRowContext(ctx, `SELECT labels_json FROM agents WHERE id=?`, agent).Scan(&raw); err != nil {
		return err
	}
	var labels model.AgentLabels
	if err := json.Unmarshal(raw, &labels); err != nil {
		return err
	}
	if _, ok := labels.Groups[group]; !ok {
		return nil
	}
	delete(labels.Groups, group)
	_, err := tx.ExecContext(ctx, `UPDATE agents SET labels_json=?,revision=revision+1,updated_at=? WHERE id=?`, agentLabelsJSON(&labels), nanos(at), agent)
	return err
}
