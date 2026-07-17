package db

import (
	"fmt"
	"log/slog"
	"time"
)

const AccessRequestStatusPending = "pending"

// AccessRequest is one human approval request shown in the dashboard's
// Messages → Access requests folder. Pending rows are the durable log of what
// was asked; only the in-memory agentd registry makes them actionable.
type AccessRequest struct {
	ID              string
	Perm            string
	ConvID          string
	AgentID         string
	ConvTitle       string
	Method          string
	Path            string
	RawQuery        string
	BodyPreview     string
	BodyLabel       string
	TargetGroup     string
	TargetConvID    string
	TargetConvTitle string
	AutoGrantable   bool
	Status          string
	CreatedAt       time.Time
	DeadlineAt      time.Time
	DecidedAt       time.Time
}

// UpsertAccessRequest records or updates one access request. A caller-captured
// AgentID wins; legacy callers that omit it still resolve the stable requester
// from ConvID. Keeping the captured ID avoids re-attributing a request if its
// original conversation metadata later disappears or changes.
func UpsertAccessRequest(ar *AccessRequest) error {
	if ar == nil || ar.ID == "" {
		return nil
	}
	d, err := Open()
	if err != nil {
		return err
	}
	created := ar.CreatedAt
	if created.IsZero() {
		created = time.Now()
	}
	status := ar.Status
	if status == "" {
		status = AccessRequestStatusPending
	}
	deadline := ""
	if !ar.DeadlineAt.IsZero() {
		deadline = ar.DeadlineAt.Format(time.RFC3339Nano)
	}
	decided := ""
	if !ar.DecidedAt.IsZero() {
		decided = ar.DecidedAt.Format(time.RFC3339Nano)
	}
	autoGrantable := 0
	if ar.AutoGrantable {
		autoGrantable = 1
	}
	explicitAgentID := 0
	if ar.AgentID != "" {
		explicitAgentID = 1
	}
	_, err = d.Exec(`
		INSERT INTO access_requests
			(id, perm, conv_id, agent_id, conv_title, method, path, raw_query,
			 body_preview, body_label, target_group, target_conv_id, target_conv_title,
			 auto_grantable, status, created_at, deadline_at, decided_at)
		VALUES
			(?, ?, ?, COALESCE(NULLIF(?, ''),
				(SELECT agent_id FROM agent_conversations WHERE conv_id = ?), ''),
			 ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			perm = excluded.perm,
			conv_id = excluded.conv_id,
			agent_id = CASE WHEN ? != 0 THEN excluded.agent_id ELSE access_requests.agent_id END,
			conv_title = excluded.conv_title,
			method = excluded.method,
			path = excluded.path,
			raw_query = excluded.raw_query,
			body_preview = excluded.body_preview,
			body_label = excluded.body_label,
			target_group = excluded.target_group,
			target_conv_id = excluded.target_conv_id,
			target_conv_title = excluded.target_conv_title,
			auto_grantable = excluded.auto_grantable,
			status = excluded.status,
			created_at = excluded.created_at,
			deadline_at = excluded.deadline_at,
			decided_at = excluded.decided_at`,
		ar.ID, ar.Perm, ar.ConvID, ar.AgentID, ar.ConvID, ar.ConvTitle, ar.Method, ar.Path, ar.RawQuery,
		ar.BodyPreview, ar.BodyLabel, ar.TargetGroup, ar.TargetConvID, ar.TargetConvTitle,
		autoGrantable, status, created.Format(time.RFC3339Nano), deadline, decided, explicitAgentID)
	if err != nil {
		return fmt.Errorf("upsert access request: %w", err)
	}
	return nil
}

// ListRecentHandledAccessRequests returns the newest handled requests. Pending
// rows are deliberately excluded because only in-memory pending requests can be
// decided; after an agentd restart a persisted pending row is historical, not
// actionable.
func ListRecentHandledAccessRequests(limit int) ([]*AccessRequest, error) {
	if limit <= 0 {
		return nil, nil
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`
		SELECT id, perm, conv_id, agent_id, conv_title, method, path, raw_query,
		       body_preview, body_label, target_group, target_conv_id, target_conv_title,
		       auto_grantable, status, created_at, deadline_at, decided_at
		FROM access_requests
		WHERE status != ?
		ORDER BY decided_at DESC, created_at DESC, id DESC
		LIMIT ?`, AccessRequestStatusPending, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []*AccessRequest
	for rows.Next() {
		var ar AccessRequest
		var autoGrantable int
		var created, deadline, decided string
		if err := rows.Scan(&ar.ID, &ar.Perm, &ar.ConvID, &ar.AgentID, &ar.ConvTitle,
			&ar.Method, &ar.Path, &ar.RawQuery, &ar.BodyPreview, &ar.BodyLabel,
			&ar.TargetGroup, &ar.TargetConvID, &ar.TargetConvTitle, &autoGrantable,
			&ar.Status, &created, &deadline, &decided); err != nil {
			return nil, err
		}
		ar.AutoGrantable = autoGrantable != 0
		ar.CreatedAt = parseAccessRequestTime(ar.ID, "created_at", created)
		ar.DeadlineAt = parseAccessRequestTime(ar.ID, "deadline_at", deadline)
		ar.DecidedAt = parseAccessRequestTime(ar.ID, "decided_at", decided)
		out = append(out, &ar)
	}
	return out, rows.Err()
}

func parseAccessRequestTime(id, field, value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		slog.Warn("access_requests: unparseable timestamp, leaving zero",
			"id", id, "field", field, "value", value, "error", err)
		return time.Time{}
	}
	return t
}
