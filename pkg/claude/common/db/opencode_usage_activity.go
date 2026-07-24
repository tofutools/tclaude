package db

import (
	"fmt"
	"strings"
	"time"
)

// OpenCodeUsageActivity is one assistant message observed through OpenCode's
// supported event/message API. MessageID makes replayed SSE events idempotent.
type OpenCodeUsageActivity struct {
	SessionID  string
	MessageID  string
	ConvID     string
	ProviderID string
	ModelID    string
	ObservedAt time.Time
}

// OpenCodeUsageActivityRetention matches the longest Usage-tab span.
const OpenCodeUsageActivityRetention = 90 * 24 * time.Hour

func validOpenCodeUsageActivity(row OpenCodeUsageActivity) bool {
	return strings.TrimSpace(row.SessionID) != "" &&
		strings.TrimSpace(row.MessageID) != "" &&
		strings.TrimSpace(row.ProviderID) != "" &&
		strings.TrimSpace(row.ModelID) != "" &&
		!row.ObservedAt.IsZero()
}

// UpsertOpenCodeUsageActivity records a live assistant message. Repeated SSE
// updates replace the same message rather than manufacturing extra activity.
func UpsertOpenCodeUsageActivity(row OpenCodeUsageActivity) error {
	if !validOpenCodeUsageActivity(row) {
		return nil
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
	if strings.TrimSpace(row.ConvID) != "" {
		if _, err := tx.Exec(`DELETE FROM opencode_usage_activity
			WHERE conv_id = ? AND message_id = ? AND session_id <> ?`,
			row.ConvID, row.MessageID, row.SessionID); err != nil {
			return fmt.Errorf("upsert OpenCode usage activity: clear resumed duplicate: %w", err)
		}
	}
	_, err = tx.Exec(`INSERT INTO opencode_usage_activity
		(session_id, message_id, conv_id, provider_id, model_id, observed_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(session_id, message_id) DO UPDATE SET
			conv_id = excluded.conv_id,
			provider_id = excluded.provider_id,
			model_id = excluded.model_id,
			observed_at = excluded.observed_at`,
		row.SessionID, row.MessageID, row.ConvID, row.ProviderID, row.ModelID,
		row.ObservedAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("upsert OpenCode usage activity: %w", err)
	}
	cutoff := time.Now().Add(-OpenCodeUsageActivityRetention).UTC().Format(time.RFC3339Nano)
	if _, err := tx.Exec(`DELETE FROM opencode_usage_activity WHERE observed_at < ?`, cutoff); err != nil {
		return fmt.Errorf("upsert OpenCode usage activity: prune: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("upsert OpenCode usage activity: commit: %w", err)
	}
	return nil
}

func DeleteOpenCodeUsageActivity(convID, sessionID, messageID string) error {
	if strings.TrimSpace(messageID) == "" {
		return nil
	}
	d, err := Open()
	if err != nil {
		return err
	}
	var deleteErr error
	if strings.TrimSpace(convID) != "" {
		_, deleteErr = d.Exec(`DELETE FROM opencode_usage_activity
			WHERE conv_id = ? AND message_id = ?`, convID, messageID)
	} else if strings.TrimSpace(sessionID) != "" {
		_, deleteErr = d.Exec(`DELETE FROM opencode_usage_activity
			WHERE session_id = ? AND message_id = ?`, sessionID, messageID)
	}
	if deleteErr != nil {
		return fmt.Errorf("delete OpenCode usage activity: %w", deleteErr)
	}
	return nil
}

// ReplaceOpenCodeUsageActivity makes reconnect/recovery authoritative for one
// conversation from GET /session/{id}/message. A resumed conversation can use
// a different local tclaude session ID, so replacement follows convID while
// pruning history beyond the dashboard's maximum retained span.
func ReplaceOpenCodeUsageActivity(
	sessionID string,
	convID string,
	rows []OpenCodeUsageActivity,
	now time.Time,
) error {
	if strings.TrimSpace(sessionID) == "" {
		return nil
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
	clearQuery, clearKey := `DELETE FROM opencode_usage_activity WHERE session_id = ?`, sessionID
	if strings.TrimSpace(convID) != "" {
		clearQuery, clearKey = `DELETE FROM opencode_usage_activity WHERE conv_id = ?`, convID
	}
	if _, err := tx.Exec(clearQuery, clearKey); err != nil {
		return fmt.Errorf("replace OpenCode usage activity: clear: %w", err)
	}
	for _, row := range rows {
		if !validOpenCodeUsageActivity(row) {
			continue
		}
		if _, err := tx.Exec(`INSERT INTO opencode_usage_activity
			(session_id, message_id, conv_id, provider_id, model_id, observed_at)
			VALUES (?, ?, ?, ?, ?, ?)`,
			row.SessionID, row.MessageID, row.ConvID, row.ProviderID, row.ModelID,
			row.ObservedAt.UTC().Format(time.RFC3339Nano)); err != nil {
			return fmt.Errorf("replace OpenCode usage activity: insert: %w", err)
		}
	}
	cutoff := now.Add(-OpenCodeUsageActivityRetention).UTC().Format(time.RFC3339Nano)
	if _, err := tx.Exec(`DELETE FROM opencode_usage_activity WHERE observed_at < ?`, cutoff); err != nil {
		return fmt.Errorf("replace OpenCode usage activity: prune: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("replace OpenCode usage activity: commit: %w", err)
	}
	return nil
}

// OpenCodeUsageActivityBetween returns activity in [from, to], chronologically.
func OpenCodeUsageActivityBetween(from, to time.Time) ([]OpenCodeUsageActivity, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT session_id, message_id, conv_id, provider_id, model_id, observed_at
		FROM opencode_usage_activity
		WHERE observed_at >= ? AND observed_at <= ?
		ORDER BY observed_at, session_id, message_id`,
		from.UTC().Format(time.RFC3339Nano), to.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, fmt.Errorf("read OpenCode usage activity: %w", err)
	}
	defer rows.Close()
	out := make([]OpenCodeUsageActivity, 0)
	for rows.Next() {
		var row OpenCodeUsageActivity
		var observed string
		if err := rows.Scan(&row.SessionID, &row.MessageID, &row.ConvID,
			&row.ProviderID, &row.ModelID, &observed); err != nil {
			return nil, fmt.Errorf("read OpenCode usage activity: scan: %w", err)
		}
		row.ObservedAt, err = time.Parse(time.RFC3339Nano, observed)
		if err != nil {
			return nil, fmt.Errorf("read OpenCode usage activity: observed_at: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read OpenCode usage activity: rows: %w", err)
	}
	return out, nil
}

func HasOpenCodeUsageActivitySince(since time.Time) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	var have int
	err = d.QueryRow(`SELECT EXISTS(
		SELECT 1 FROM opencode_usage_activity WHERE observed_at >= ? LIMIT 1
	)`, since.UTC().Format(time.RFC3339Nano)).Scan(&have)
	if err != nil {
		return false, fmt.Errorf("check OpenCode usage activity: %w", err)
	}
	return have != 0, nil
}
