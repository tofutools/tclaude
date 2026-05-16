package agentd

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/humannotify"
)

// resolveHumanNotifyTransport is the seam between handleNotifyHuman and
// the humannotify package. Production points it at humannotify.Resolve;
// flow tests swap in a fake transport (SetHumanNotifyTransportForTest)
// so the daemon's permission gating + delivery path runs unchanged
// without a real Telegram call.
var resolveHumanNotifyTransport = humannotify.Resolve

// notifyHumanSendTimeout bounds the outbound transport call from the
// handler's side, independent of the transport's own HTTP timeout.
const notifyHumanSendTimeout = 20 * time.Second

// notifyHumanRequest is the POST /v1/notify-human body.
type notifyHumanRequest struct {
	Body    string `json:"body"`
	Subject string `json:"subject"`
}

// notifyHumanResponse is the success body.
type notifyHumanResponse struct {
	Transport string `json:"transport"`
	Delivered bool   `json:"delivered"`
	Handle    string `json:"handle,omitempty"`
}

// handleNotifyHuman serves POST /v1/notify-human — the daemon side of
// `tclaude agent notify-human`. It gates on the human.notify permission
// (humans bypass; the X-Tclaude-Ask-Human popup is the one-off escape
// hatch via requirePermission), resolves the configured external
// transport, and sends the notification.
//
// The transport lives daemon-side on purpose: the permission gate is
// enforced here where the caller is untrusted, and the outbound network
// call originates from agentd, which runs outside the agent sandbox.
func handleNotifyHuman(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}
	callerConv, ok := requirePermission(w, r, PermHumanNotify)
	if !ok {
		return
	}

	var body notifyHumanRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	body.Body = strings.TrimSpace(body.Body)
	body.Subject = strings.TrimSpace(body.Subject)
	if body.Body == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "body is required")
		return
	}

	cfg, _ := config.Load()
	transport, err := resolveHumanNotifyTransport(cfg)
	if err != nil {
		if errors.Is(err, humannotify.ErrNotConfigured) {
			writeError(w, http.StatusPreconditionFailed, "not_configured",
				"no human-notify transport configured — add a human_notify section to "+config.ConfigPath())
			return
		}
		// A transport is named but mis-configured (e.g. empty bot token).
		writeError(w, http.StatusPreconditionFailed, "not_configured", err.Error())
		return
	}

	n := humannotify.Notification{
		FromConv:  callerConv,
		FromTitle: notifyHumanCallerTitle(callerConv),
		Group:     notifyHumanCallerGroup(callerConv),
		Subject:   body.Subject,
		Body:      body.Body,
		SentAt:    time.Now(),
	}
	ctx, cancel := context.WithTimeout(r.Context(), notifyHumanSendTimeout)
	defer cancel()
	handle, err := transport.Send(ctx, n)
	if err != nil {
		writeError(w, http.StatusBadGateway, "transport_failed",
			"human-notify transport failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, notifyHumanResponse{
		Transport: transport.Name(),
		Delivered: true,
		Handle:    handle,
	})
}

// notifyHumanCallerTitle resolves a caller conv-id to its display title
// for the notification's attribution line. Empty for the human path
// (callerConv == "") or when the conv has no resolvable title.
func notifyHumanCallerTitle(callerConv string) string {
	if callerConv == "" {
		return ""
	}
	if row := agent.FreshConvRowResolved(callerConv); row != nil {
		return agent.DisplayTitle(row)
	}
	return ""
}

// notifyHumanCallerGroup returns one group name the caller belongs to,
// for the notification's "which project" context. Empty when the caller
// is ungrouped or is the human. When the caller is in several groups
// the first is used — the attribution line is a hint, not an audit.
func notifyHumanCallerGroup(callerConv string) string {
	if callerConv == "" {
		return ""
	}
	groups, err := db.ListGroupsForConv(callerConv)
	if err != nil || len(groups) == 0 {
		return ""
	}
	return groups[0].Name
}
