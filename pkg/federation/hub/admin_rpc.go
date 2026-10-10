package hub

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/federation/proto"
)

var adminMethodCapability = map[string]string{
	"status": "", "claim": "",
	"run.status": "@admin", "run.start": "hub.exec", "run.job": "@admin", "run.logs": "hub.exec", "audit": "hub.logs.read",
	"update.status": "hub.update", "update.start": "hub.update", "update.job": "hub.update",
	"admins.list": "hub.admins.manage", "admins.add": "hub.admins.manage", "admins.remove": "hub.admins.manage",
	"admissions.list": "hub.admissions.manage", "admissions.admit": "hub.admissions.manage", "admissions.revoke": "hub.admissions.manage",
	"invites.list": "hub.invites.manage", "invites.create": "hub.invites.manage", "invites.revoke": "hub.invites.manage",
	"spaces.list": "hub.spaces.manage", "spaces.set": "hub.spaces.manage",
	"settings.get": "hub.settings.manage", "settings.patch": "hub.settings.manage",
	"identity.recover": "hub.identity.manage", "identity.revoke-old": "hub.identity.manage",
	"health": "hub.health.read", "logs": "hub.logs.read",
}

func (h *Hub) adminRequest(c *conn, frame *proto.Frame, size int) {
	req := frame.AdminRequest
	if req == nil || !proto.ValidStreamID(req.ID) {
		c.send(&proto.Frame{Type: proto.FrameError, Code: proto.CodeBadFrame, Message: "invalid hub admin frame"})
		return
	}
	h.adminMu.Lock()
	generation, err := h.store.AdminGeneration()
	result := &proto.HubAdminResult{ID: req.ID, Generation: generation, Status: 200}
	fail := func(e error) {
		a := wrapAdminError(e)
		result.Status, result.Code, result.Error = a.Status, a.Code, a.Message
	}
	capability, known := adminMethodCapability[req.Method]
	switch {
	case err != nil:
		fail(err)
	case !c.limiter.allow(size, time.Now()):
		fail(adminErr(429, "rate_limited", "hub admin rate limit exceeded"))
	case req.Verify(c.pub, h.hubID, c.nonce, time.Now()) != nil:
		fail(adminErr(403, "bad_auth", "hub admin signature or validity check failed"))
	case !known:
		fail(adminErr(400, "operation", "unknown hub admin operation"))
	default:
		if e := h.store.AuthorizeAdminRequest(c.id, c.pub, req, capability); e != nil {
			fail(e)
		} else {
			body, e := h.executeAdmin(c, req)
			if e != nil {
				fail(e)
			} else {
				raw, e := json.Marshal(body)
				if e != nil {
					fail(e)
				} else if len(raw) > proto.MaxAdminResult {
					fail(adminErr(413, "result_limit", "hub admin result exceeds 256 KiB"))
				} else {
					result.Body = raw
				}
			}
		}
	}
	// Do not record payloads or arbitrary error text: tokens and configuration
	// values must not leak into logs. Operations/codes are locally enumerated.
	operation := req.Method
	if !known {
		operation = "unknown"
	}
	if e := h.store.AuditAdmin(c.id, req.ID, operation, result.Status, result.Code); e != nil {
		fail(adminErr(500, "audit", "hub audit persistence failed"))
	}
	h.log.Info("hub admin request", "instance", c.id, "operation", operation, "status", result.Status, "code", result.Code)
	// Serialize authorization, mutation and audit, but never hold global admin
	// authority behind a slow socket writer. Other instances remain responsive.
	h.adminMu.Unlock()
	// Write before policy refresh: an admin may revoke its own admission.
	// Mutations are never retried if the reply fails after execution.
	_ = c.write(&proto.Frame{Type: proto.FrameAdminResult, AdminResult: result}, 5*time.Second)
	if result.Status < 400 && (strings.HasPrefix(req.Method, "admissions.") || strings.HasPrefix(req.Method, "spaces.") || strings.HasPrefix(req.Method, "identity.") || req.Method == "settings.patch") {
		h.RefreshPolicy()
	}
}

type adminParams struct {
	Instance       string            `json:"instance"`
	Capabilities   []string          `json:"capabilities"`
	Spaces         []string          `json:"spaces"`
	Token          string            `json:"token"`
	TokenHash      string            `json:"token_hash"`
	Space          string            `json:"space"`
	TTLSeconds     int64             `json:"ttl_seconds"`
	Overrides      map[string]*int64 `json:"overrides"`
	Old            string            `json:"old"`
	New            string            `json:"new"`
	Fingerprint    string            `json:"fingerprint"`
	Apply          bool              `json:"apply"`
	Cursor         string            `json:"cursor"`
	MaxEntries     int               `json:"max_entries"`
	Script         string            `json:"script"`
	TimeoutSeconds int64             `json:"timeout_seconds"`
	JobID          string            `json:"job_id"`
	Stream         string            `json:"stream"`
	Offset         int64             `json:"offset"`
}

func decodeAdminParams(raw []byte) (adminParams, error) {
	var p adminParams
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&p); err != nil {
		return p, adminErr(400, "json", "invalid hub admin parameters")
	}
	if err := d.Decode(&struct{}{}); err != io.EOF {
		return p, adminErr(400, "json", "expected one JSON object")
	}
	return p, nil
}
func validateAdminSpaces(spaces []string) error {
	if len(spaces) > 32 {
		return adminErr(400, "spaces", "at most 32 spaces")
	}
	for _, space := range spaces {
		if len(space) == 0 || len(space) > 64 || proto.SafeName(space, false) != space {
			return adminErr(400, "spaces", "space names must use the supported display-name charset")
		}
	}
	return nil
}
func (h *Hub) executeAdmin(c *conn, req *proto.HubAdminRequest) (any, error) {
	if strings.HasPrefix(req.Method, "update.") {
		return h.executeUpdate(c, req.Method, req.Payload)
	}
	p, err := decodeAdminParams(req.Payload)
	if err != nil {
		return nil, err
	}
	switch req.Method {
	case "run.status", "run.start", "run.job", "run.logs":
		return h.executeRun(c, req.Method, p)
	case "audit":
		return h.auditRows(c, p)
	case "status":
		admins, err := h.store.Admins()
		if err != nil {
			return nil, err
		}
		caps, err := h.store.AdminCapabilities(c.id)
		if err != nil {
			return nil, err
		}
		admin := slices.ContainsFunc(admins, func(a Admin) bool { return a.Instance == c.id })
		settings, err := h.Settings()
		if err != nil {
			return nil, err
		}
		overridden := []string{}
		for key, s := range settings {
			if s.FlagOverridden {
				overridden = append(overridden, key)
			}
		}
		slices.Sort(overridden)
		return map[string]any{"hub_id": h.hubID, "hub_version": h.cfg.Version, "connected": true, "instance": c.id, "admin": admin, "admin_count": len(admins), "my_capabilities": caps, "available_capabilities": proto.HubAdminCapabilities, "bootstrap_claimable": len(admins) == 0, "flags_overridden": overridden}, nil
	case "claim":
		if err := h.store.ClaimAdmin(c.id, c.pub, p.Token, time.Now()); err != nil {
			return nil, err
		}
		return map[string]bool{"claimed": true}, nil
	case "admins.list":
		rows, err := h.store.Admins()
		return map[string]any{"admins": rows}, err
	case "admins.add":
		if err = h.store.SetAdmin(p.Instance, c.id, p.Capabilities); err != nil {
			return nil, err
		}
		return map[string]bool{"ok": true}, nil
	case "admins.remove":
		if err = h.store.RemoveAdmin(p.Instance); err != nil {
			return nil, err
		}
		return map[string]bool{"ok": true}, nil
	case "admissions.list", "spaces.list":
		rows, err := h.store.List()
		if err != nil {
			return nil, err
		}
		out := []map[string]any{}
		h.mu.Lock()
		online := map[string]bool{}
		for id := range h.conns {
			online[id] = true
		}
		h.mu.Unlock()
		for _, row := range rows {
			out = append(out, map[string]any{"instance": row.ID, "name": row.Name, "fingerprint": proto.InstanceFingerprint(row.ID), "spaces": row.Spaces, "admitted_at": row.AdmittedAt, "last_seen": row.LastSeen, "connected": online[row.ID], "revoked": row.Revoked})
		}
		key := "admissions"
		if req.Method == "spaces.list" {
			key = "spaces"
		}
		return pageAdminRows(out, key, p)
	case "admissions.admit":
		if err = validateAdminSpaces(p.Spaces); err != nil {
			return nil, err
		}
		if err = h.store.Admit(p.Instance, p.Spaces...); err != nil {
			return nil, adminErr(400, "instance", err.Error())
		}
		return map[string]bool{"ok": true}, nil
	case "admissions.revoke":
		if err = h.store.Revoke(p.Instance); err != nil {
			return nil, err
		}
		return map[string]bool{"ok": true}, nil
	case "spaces.set":
		if !proto.ValidInstanceID(p.Instance) {
			return nil, adminErr(400, "instance", "invalid instance ID")
		}
		if err = validateAdminSpaces(p.Spaces); err != nil {
			return nil, err
		}
		in, err := h.store.Get(p.Instance)
		if err != nil {
			return nil, err
		}
		if in == nil {
			return nil, adminErr(404, "instance", "no such admitted instance")
		}
		if err = h.store.SetSpaces(p.Instance, p.Spaces); err != nil {
			return nil, err
		}
		return map[string]bool{"ok": true}, nil
	case "invites.list":
		rows, err := h.store.ListInvites()
		if err != nil {
			return nil, err
		}
		out := []map[string]any{}
		for _, row := range rows {
			out = append(out, map[string]any{"token_hash": row.Hash, "space": row.Space, "expires_at": row.ExpiresAt, "created_at": row.CreatedAt, "used": row.UsedBy != "", "used_by": row.UsedBy})
		}
		return pageAdminRows(out, "invites", p)
	case "invites.create":
		if p.Space == "" {
			p.Space = DefaultSpace
		}
		if err = validateAdminSpaces([]string{p.Space}); err != nil {
			return nil, err
		}
		if p.TTLSeconds == 0 {
			p.TTLSeconds = 3600
		}
		if p.TTLSeconds < 60 || p.TTLSeconds > 86400*7 {
			return nil, adminErr(400, "ttl_seconds", "invite TTL must be 60..604800 seconds")
		}
		token, err := h.store.CreateInvite(p.Space, time.Duration(p.TTLSeconds)*time.Second)
		if err != nil {
			return nil, err
		}
		rows, err := h.store.ListInvites()
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			if row.Hash == hashToken(token) {
				return map[string]any{"token": token, "token_hash": row.Hash, "space": row.Space, "expires_at": row.ExpiresAt}, nil
			}
		}
		return nil, fmt.Errorf("created invite missing")
	case "invites.revoke":
		if len(p.TokenHash) != 64 {
			return nil, adminErr(400, "token_hash", "invalid token hash")
		}
		if err = h.store.RevokeInvite(p.TokenHash); err != nil {
			return nil, err
		}
		return map[string]bool{"ok": true}, nil
	case "settings.get":
		rows, err := h.Settings()
		return map[string]any{"settings": rows}, err
	case "settings.patch":
		if err = h.store.PatchSettings(p.Overrides); err != nil {
			return nil, err
		}
		if err = h.refreshSettings(); err != nil {
			return nil, err
		}
		rows, err := h.Settings()
		return map[string]any{"settings": rows}, err
	case "identity.recover":
		if !proto.ValidInstanceID(p.Old) || !proto.ValidInstanceID(p.New) || p.Old == p.New {
			return nil, adminErr(400, "instance", "use different immutable identity IDs")
		}
		old, err := h.store.Get(p.Old)
		if err != nil {
			return nil, err
		}
		if old == nil {
			return nil, adminErr(404, "instance", "old identity is not admitted")
		}
		next, err := h.store.Get(p.New)
		if err != nil {
			return nil, err
		}
		out := map[string]any{"old": adminAdmissionRow(old), "replacement": adminAdmissionRow(next), "new": p.New, "new_fingerprint": proto.InstanceFingerprint(p.New), "applied": false, "warning": "Replacement inherits old spaces; current replacement spaces are replaced. Old admission and admin capabilities are revoked; peer trust still needs explicit local recovery."}
		if p.Apply {
			if p.Fingerprint != proto.InstanceFingerprint(p.New) {
				return nil, adminErr(400, "confirmation_required", "confirm the exact replacement fingerprint")
			}
			if err = h.store.RecoverIdentity(p.Old, p.New, time.Now()); err != nil {
				return nil, err
			}
			out["applied"] = true
		}
		return out, nil
	case "identity.revoke-old":
		if !proto.ValidInstanceID(p.Instance) {
			return nil, adminErr(400, "instance", "invalid instance ID")
		}
		out := map[string]any{"instance": p.Instance, "fingerprint": proto.InstanceFingerprint(p.Instance), "applied": false, "warning": "Revoke predecessor and pending automatic rotation; an accepted successor stays current."}
		if p.Apply {
			if p.Fingerprint != proto.InstanceFingerprint(p.Instance) {
				return nil, adminErr(400, "confirmation_required", "confirm the exact predecessor fingerprint")
			}
			if err = h.store.RevokeOldIdentity(p.Instance, time.Now()); err != nil {
				return nil, err
			}
			out["applied"] = true
		}
		return out, nil
	case "health":
		var mem runtime.MemStats
		runtime.ReadMemStats(&mem)
		return map[string]any{"connected_instances": h.OnlineCount(), "streams": h.StreamCount(), "load": map[string]any{"goroutines": runtime.NumGoroutine(), "heap_bytes": mem.Alloc}, "uptime_seconds": int64(time.Since(h.started).Seconds()), "recent_errors": h.recentErrors()}, nil
	case "logs":
		return h.adminLogTail(p)
	}
	return nil, adminErr(400, "operation", "unknown hub operation")
}
func pageAdminRows(rows []map[string]any, key string, p adminParams) (any, error) {
	offset := 0
	var err error
	if p.Cursor != "" {
		offset, err = strconv.Atoi(p.Cursor)
		if err != nil || offset < 0 || offset > len(rows) {
			return nil, adminErr(400, "cursor", "invalid list cursor")
		}
	}
	limit := p.MaxEntries
	if limit == 0 {
		limit = 100
	}
	if limit < 1 || limit > 250 {
		return nil, adminErr(400, "max_entries", "max_entries must be1..250")
	}
	end := min(offset+limit, len(rows))
	out := map[string]any{key: rows[offset:end]}
	if end < len(rows) {
		out["next_cursor"] = strconv.Itoa(end)
	}
	return out, nil
}

func adminAdmissionRow(row *Instance) any {
	if row == nil {
		return nil
	}
	return map[string]any{"instance": row.ID, "name": row.Name, "fingerprint": proto.InstanceFingerprint(row.ID), "spaces": row.Spaces, "admitted_at": row.AdmittedAt, "last_seen": row.LastSeen, "revoked": row.Revoked}
}
