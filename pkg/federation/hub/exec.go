package hub

import (
	"encoding/json"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strconv"

	"github.com/tofutools/tclaude/pkg/noderun"
)

func (h *Hub) initExec() error {
	runner := executeHubScript
	started := func(j noderun.Job, r noderun.Request) error {
		detail := map[string]any{"job_id": j.ID, "script": r.Script, "script_sha256": j.ScriptSHA256, "script_bytes": j.ScriptBytes, "timeout_seconds": j.TimeoutSeconds, "phase": "started"}
		raw, err := json.Marshal(detail)
		if err != nil {
			return err
		}
		return h.store.AuditAdmin(j.Actor, j.ID, "exec", 202, string(raw))
	}
	finished := func(j noderun.Job) {
		detail := map[string]any{"job_id": j.ID, "script_sha256": j.ScriptSHA256, "script_bytes": j.ScriptBytes, "exit_code": j.ExitCode, "state": j.State, "duration_ms": j.DurationMS, "phase": "result"}
		raw, err := json.Marshal(detail)
		if err == nil {
			err = h.store.auditExecOutcome(j.Actor, j.ID, string(raw))
		}
		if err != nil {
			h.log.Error("hub exec outcome audit failed", "job", j.ID)
		}
	}
	svc, err := noderun.NewStreaming(filepath.Join(filepath.Dir(h.store.path), "hub-runs"), runner, started, finished)
	if err != nil {
		return err
	}
	h.runs = svc
	return nil
}
func (h *Hub) execAllowed(instance string) bool {
	h.mu.Lock()
	closed := h.closed
	h.mu.Unlock()
	if closed {
		return false
	}
	enabled, _, err := h.store.scriptSwitch(h.cfg.AcceptRemoteScripts)
	return err == nil && enabled && h.store.hasAdminCapability(instance, "hub.exec")
}
func (h *Hub) executeRun(c *conn, method string, p adminParams) (any, error) {
	switch method {
	case "run.status":
		enabled, source, err := h.store.scriptSwitch(h.cfg.AcceptRemoteScripts)
		if err != nil {
			return nil, adminErr(503, "script_config", "hub-local script configuration unavailable")
		}
		name := strconv.Itoa(os.Geteuid())
		if current, err := user.Current(); err == nil {
			name = current.Username
		}
		return map[string]any{"accept_remote_scripts": enabled, "switch_source": source, "can_exec": h.store.hasAdminCapability(c.id, "hub.exec"), "service_user": name, "warning": execThreat, "limits": map[string]any{"max_script_bytes": noderun.MaxScriptBytes, "default_timeout_seconds": 3600, "max_timeout_seconds": 86400, "max_output_bytes": noderun.MaxOutputBytes}}, nil
	case "run.start":
		if !h.execAllowed(c.id) {
			return nil, adminErr(409, "remote_scripts_disabled", "hub-local accept_remote_scripts switch must be enabled by the hub host")
		}
		if os.Geteuid() == 0 {
			return nil, adminErr(403, "hub_root_refused", "run the hub as a non-root service user before enabling scripts")
		}
		req := noderun.Request{Script: p.Script, TimeoutSeconds: p.TimeoutSeconds}
		if err := req.Validate(); err != nil {
			return nil, adminErr(400, "script", err.Error())
		}
		j, err := h.runs.Start(req, c.id, "", h.hubID, func() bool { return h.execAllowed(c.id) })
		if err != nil {
			if errors.Is(err, noderun.ErrBusy) {
				return nil, adminErr(429, "run_busy", "hub script workers busy")
			}
			return nil, err
		}
		return h.execJobJSON(j, c.id), nil
	case "run.job", "run.logs":
		j, err := h.runs.Job(p.JobID)
		if err != nil {
			return nil, adminErr(404, "job", "no such hub script job")
		}
		if method == "run.job" {
			return h.execJobJSON(j, c.id), nil
		}
		chunk, err := h.runs.Log(p.JobID, p.Stream, p.Offset)
		if errors.Is(err, os.ErrNotExist) && j.State == "running" {
			return noderun.LogChunk{Data: []byte{}, NextOffset: p.Offset, EOF: false}, nil
		}
		if err != nil {
			return nil, adminErr(400, "log", err.Error())
		}
		chunk.EOF = chunk.EOF && j.State != "running"
		return chunk, nil
	}
	return nil, adminErr(400, "operation", "unknown hub exec operation")
}
func (h *Hub) execJobJSON(j noderun.Job, instance string) map[string]any {
	raw, _ := json.Marshal(j)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	out["started_at"] = j.CreatedAt
	if j.State == "canceled" {
		out["state"] = "cancelled"
	}
	if j.State == "interrupted" {
		out["state"] = "lost"
	}
	if !h.store.hasAdminCapability(instance, "hub.exec") {
		delete(out, "stdout_tail")
		delete(out, "stderr_tail")
		delete(out, "duration_ms")
	}
	return out
}
func (h *Hub) auditRows(c *conn, p adminParams) (any, error) {
	after := int64(0)
	var err error
	if p.Cursor != "" {
		after, err = strconv.ParseInt(p.Cursor, 10, 64)
		if err != nil || after < 0 {
			return nil, adminErr(400, "cursor", "invalid audit cursor")
		}
	}
	limit := p.MaxEntries
	if limit == 0 {
		limit = 100
	}
	if limit < 1 || limit > 200 {
		return nil, adminErr(400, "max_entries", "max_entries must be1..200")
	}
	canExec := h.store.hasAdminCapability(c.id, "hub.exec")
	rows, err := h.store.db.Query(`SELECT sequence,at,instance,operation,status,detail FROM hub_admin_audit WHERE sequence>? ORDER BY sequence LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	entries := []map[string]any{}
	next := after
	budget := 256<<10 - 1024
	for rows.Next() {
		var seq int64
		var at, actor, kind, detail string
		var status int
		if err = rows.Scan(&seq, &at, &actor, &kind, &status, &detail); err != nil {
			return nil, err
		}
		value := map[string]any{}
		if kind == "exec" {
			if json.Unmarshal([]byte(detail), &value) != nil {
				return nil, errors.New("invalid exec audit")
			}
			if !canExec {
				delete(value, "script")
				value["redacted"] = true
			}
		} else {
			value["code"] = detail
		}
		row := map[string]any{"at": parseTS(at), "actor": actor, "kind": kind, "outcome": status, "detail": value}
		raw, _ := json.Marshal(row)
		if len(raw)+1 > budget {
			break
		}
		budget -= len(raw) + 1
		entries = append(entries, row)
		next = seq
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return map[string]any{"entries": entries, "next_cursor": strconv.FormatInt(next, 10)}, nil
}
