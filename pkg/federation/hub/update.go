package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/tofutools/tclaude/pkg/federation/hubupdate"
	"github.com/tofutools/tclaude/pkg/selfupdate"
	"os"
	"path/filepath"
)

type UpdateBackend interface {
	Call(context.Context, string, string, any) (any, error)
}

func (s *Store) ActiveAdminCapability(instance, capability string) bool {
	return s.hasAdminCapability(instance, capability)
}
func (h *Hub) initUpdate() error {
	if h.cfg.UpdateBackend != nil {
		return nil
	}
	path, err := os.Executable()
	if err != nil {
		return err
	}
	binary, err := selfupdate.DiscoverBinary("tclaude-hub", path)
	if err != nil {
		binary = selfupdate.Binary{Name: "tclaude-hub", Path: path, Version: h.cfg.Version, Method: "unknown"}
	}
	svc, err := selfupdate.New(filepath.Join(filepath.Dir(h.store.path), "hub-update"), []selfupdate.Binary{binary}, selfupdate.Hooks{Supervised: true, ReleaseOnly: true, NoDowngrade: true, Started: h.store.AuditUpdateProgress, Progress: func(j selfupdate.Job) {
		if err := h.store.AuditUpdateProgress(j); err != nil {
			h.log.Error("hub update progress audit failed", "job", j.ID)
		}
	}, Finished: func(j selfupdate.Job) {
		if err := h.store.AuditUpdateOutcome(j); err != nil {
			h.log.Error("hub update outcome audit failed", "job", j.ID)
		}
	}})
	if err != nil {
		return err
	}
	// Plain serve has no guardian to finish an interrupted read-only check.
	// Do not finalize apply/rollback here: only the guardian can prove health.
	if j := svc.Pending(); j != nil && j.Action == "check" && (j.State == "running" || j.State == "restarting") {
		if err := svc.FinishSupervised(j.CurrentVersion, false, fmt.Errorf("hub exited during release check; start a new check")); err != nil {
			return err
		}
	}
	h.updates = svc
	if j := svc.Pending(); j != nil {
		return h.store.AuditUpdateOutcome(*j)
	}
	return nil
}
func (h *Hub) executeUpdate(c *conn, operation string, raw json.RawMessage) (any, error) {
	var payload map[string]any
	if json.Unmarshal(raw, &payload) != nil {
		return nil, adminErr(400, "request", "invalid update request")
	}
	if h.cfg.UpdateBackend != nil {
		out, err := h.cfg.UpdateBackend.Call(context.Background(), c.id, operation, payload)
		if e, ok := err.(*hubupdate.Error); ok {
			return nil, adminErr(e.Status, e.Code, e.Message)
		}
		return out, err
	}
	switch operation {
	case "update.status":
		return hubupdate.StatusJSON(h.updates.Status(), ""), nil
	case "update.job":
		var p struct {
			JobID string `json:"job_id"`
		}
		if json.Unmarshal(raw, &p) != nil {
			return nil, adminErr(400, "job", "invalid job")
		}
		j, err := h.updates.Job(p.JobID)
		if err != nil {
			return nil, adminErr(404, "job", "no such update job")
		}
		if err := h.store.AuditUpdateOutcome(j); err != nil {
			return nil, adminErr(503, "audit", "hub update audit unavailable")
		}
		return hubupdate.JobJSON(j), nil
	case "update.start":
		var req selfupdate.Request
		d := json.NewDecoder(bytes.NewReader(raw))
		d.DisallowUnknownFields()
		if d.Decode(&req) != nil {
			return nil, adminErr(400, "request", "invalid update request")
		}
		if err := req.Validate(); err != nil {
			return nil, adminErr(400, "request", err.Error())
		}
		if req.Action != "check" {
			return nil, adminErr(409, "not_supervised", "host must run serve --supervised under a verified systemd/launchd restart policy")
		}
		j, err := h.updates.Start(req, c.id, func() bool { return h.store.ActiveAdminCapability(c.id, "hub.update") })
		if err != nil {
			if err == selfupdate.ErrBusy {
				return nil, adminErr(409, "update_busy", err.Error())
			}
			return nil, err
		}
		return hubupdate.JobJSON(j), nil
	}
	return nil, adminErr(400, "operation", "unknown update operation")
}

func (s *Store) AuditUpdateOutcome(j selfupdate.Job) error {
	if j.State == "running" || j.State == "restarting" {
		return nil
	}
	raw, err := json.Marshal(j)
	if err != nil {
		return err
	}
	return s.auditUpdateOutcome(j.Actor, j.ID, string(raw))
}

func (s *Store) AuditUpdateProgress(j selfupdate.Job) error {
	raw, err := json.Marshal(j)
	if err != nil {
		return err
	}
	return s.AuditAdmin(j.Actor, j.ID, "update.progress", 202, string(raw))
}
