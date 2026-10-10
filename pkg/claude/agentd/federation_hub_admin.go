package agentd

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

func registerFederationHubRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/federation/hub/boards", hubOperatorRoute("boards.list"))
	mux.HandleFunc("PATCH /v1/federation/hub/boards/{board}", hubOperatorRoute("boards.patch"))
	mux.HandleFunc("DELETE /v1/federation/hub/boards/{board}", hubOperatorRoute("boards.delete"))
	mux.HandleFunc("GET /v1/federation/hub/run", hubOperatorRoute("run.status"))
	mux.HandleFunc("POST /v1/federation/hub/run", hubOperatorRoute("run.start"))
	mux.HandleFunc("GET /v1/federation/hub/run/jobs/{job_id}", hubOperatorRoute("run.job"))
	mux.HandleFunc("GET /v1/federation/hub/run/jobs/{job_id}/logs", hubOperatorRoute("run.logs"))
	mux.HandleFunc("GET /v1/federation/hub/audit", hubOperatorRoute("audit"))
	mux.HandleFunc("GET /v1/federation/hub/update", hubOperatorRoute("update.status"))
	mux.HandleFunc("POST /v1/federation/hub/update", hubOperatorRoute("update.start"))
	mux.HandleFunc("GET /v1/federation/hub/update/jobs/{job_id}", hubOperatorRoute("update.job"))
	mux.HandleFunc("GET /v1/federation/hub/status", hubOperatorRoute("status"))
	mux.HandleFunc("POST /v1/federation/hub/claim", hubOperatorRoute("claim"))
	mux.HandleFunc("GET /v1/federation/hub/admins", hubOperatorRoute("admins.list"))
	mux.HandleFunc("POST /v1/federation/hub/admins", hubOperatorRoute("admins.add"))
	mux.HandleFunc("DELETE /v1/federation/hub/admins/{instance}", hubOperatorRoute("admins.remove"))
	mux.HandleFunc("GET /v1/federation/hub/admissions", hubOperatorRoute("admissions.list"))
	mux.HandleFunc("POST /v1/federation/hub/admissions", hubOperatorRoute("admissions.admit"))
	mux.HandleFunc("DELETE /v1/federation/hub/admissions/{instance}", hubOperatorRoute("admissions.revoke"))
	mux.HandleFunc("GET /v1/federation/hub/invites", hubOperatorRoute("invites.list"))
	mux.HandleFunc("POST /v1/federation/hub/invites", hubOperatorRoute("invites.create"))
	mux.HandleFunc("DELETE /v1/federation/hub/invites/{token_hash}", hubOperatorRoute("invites.revoke"))
	mux.HandleFunc("GET /v1/federation/hub/spaces", hubOperatorRoute("spaces.list"))
	mux.HandleFunc("PUT /v1/federation/hub/spaces", hubOperatorRoute("spaces.set"))
	mux.HandleFunc("GET /v1/federation/hub/settings", hubOperatorRoute("settings.get"))
	mux.HandleFunc("PATCH /v1/federation/hub/settings", hubOperatorRoute("settings.patch"))
	mux.HandleFunc("POST /v1/federation/hub/identity/recover", hubOperatorRoute("identity.recover"))
	mux.HandleFunc("POST /v1/federation/hub/identity/revoke-old", hubOperatorRoute("identity.revoke-old"))
	mux.HandleFunc("GET /v1/federation/hub/health", hubOperatorRoute("health"))
	mux.HandleFunc("GET /v1/federation/hub/logs", hubOperatorRoute("logs"))
}

// Hub administration is local operator authority. A peer dispatcher must never
// call this handler, even if the requesting peer has unrestricted node trust.
func hubOperatorRoute(operation string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireHuman(w, r, "administer the federation hub") {
			return
		}
		w.Header().Set("Cache-Control", "private, no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
		payload := map[string]any{}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodDelete {
			decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, proto.MaxAdminPayload))
			if err := decoder.Decode(&payload); err != nil || payload == nil {
				writeError(w, 400, "invalid_arg", "expected a JSON object")
				return
			}
			var extra any
			if err := decoder.Decode(&extra); err != io.EOF {
				writeError(w, 400, "invalid_arg", "expected one JSON object")
				return
			}
		}
		for _, key := range []string{"instance", "token_hash", "job_id", "board"} {
			if value := r.PathValue(key); value != "" {
				payload[key] = value
			}
		}
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			if stream := r.URL.Query().Get("stream"); stream != "" {
				payload["stream"] = stream
			}
			if value := r.URL.Query().Get("offset"); value != "" {
				n, err := strconv.ParseInt(value, 10, 64)
				if err != nil || n < 0 {
					writeError(w, 400, "invalid_arg", "invalid offset")
					return
				}
				payload["offset"] = n
			}
			if cursor := r.URL.Query().Get("cursor"); cursor != "" {
				payload["cursor"] = cursor
			}
			if value := r.URL.Query().Get("max_entries"); value != "" {
				n, err := strconv.Atoi(value)
				if err != nil {
					writeError(w, 400, "invalid_arg", "invalid max_entries")
					return
				}
				payload["max_entries"] = n
			}
		}
		hubURL := ""
		if cfg, err := config.Load(); err == nil && cfg != nil && cfg.Federation != nil {
			hubURL = cfg.Federation.HubURL
		}
		rt := currentFederation()
		if rt == nil || rt.cl == nil {
			if operation == "status" {
				writeJSON(w, 200, map[string]any{"connected": false, "hub_url": hubURL, "admin": false, "my_capabilities": []string{}, "available_capabilities": proto.HubAdminCapabilities})
				return
			}
			writeError(w, 503, "offline", "connect federation before administering the hub")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		result, err := rt.cl.AdminCall(ctx, operation, payload)
		if err != nil {
			writeError(w, 503, "hub_unavailable", err.Error())
			return
		}
		if result.Status < 200 || result.Status >= 300 {
			writeError(w, result.Status, result.Code, result.Error)
			return
		}
		var body any
		if err = json.Unmarshal(result.Body, &body); err != nil {
			writeError(w, 502, "hub_reply", "invalid hub response")
			return
		}
		if operation == "status" {
			if status, ok := body.(map[string]any); ok {
				status["hub_url"] = hubURL
			}
		}
		writeJSON(w, result.Status, body)
	}
}
