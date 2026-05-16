package agentd

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/agent"
)

// Dashboard one-shot message route — the cookie-authenticated twin of
// POST /v1/messages. The browser cannot speak SO_PEERCRED, so this
// parallel endpoint accepts the human's send and funnels it through
// the SAME dispatchSend core the /v1 path uses: a solo target or a
// "group:NAME" multicast, identical validation, identical authority
// gate on the From conv. Nothing here re-implements routing — the
// only dashboard-specific step is resolving the human-picked From
// selector into the conv-id the message is attributed to.
//
// Wired into the dashboard mux from registerDashboardEditRoutes.

func registerDashboardMessageRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/message", handleDashboardMessageCreate)
}

// handleDashboardMessageCreate sends one immediate message from the
// dashboard. Body: {from, to, subject, body, role, members}. `to` is a
// solo selector or a "group:NAME" multicast token — exactly the
// grammar POST /v1/messages accepts, so a group send fans out to every
// member. `members`, set only when the group-scoped modal sent to a
// ticked subset, narrows that fan-out to the listed conv-ids (it can
// only shrink reach — see fanOutToGroup). `from` is the sender the
// message is attributed to (and replied to); the human picks it,
// mirroring the cron form's Owner field.
//
// The cookie + Origin pin in checkDashboardAuth is the human-consent
// layer. dispatchSend then enforces the From conv's own group
// standing (member/owner for a multicast, shared-group / message.direct
// for a 1:1), so the dashboard cannot send anything the From agent
// could not send itself — the gate is not skipped, just reached
// through a different front door.
func handleDashboardMessageCreate(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	var body struct {
		From    string   `json:"from"`
		To      string   `json:"to"`
		Subject string   `json:"subject"`
		Body    string   `json:"body"`
		Role    string   `json:"role"`
		Members []string `json:"members"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	if strings.TrimSpace(body.From) == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			"from is required (the sender agent the message is attributed to)")
		return
	}
	if strings.TrimSpace(body.To) == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			"to is required (a solo target or a 'group:NAME' multicast)")
		return
	}
	res, matches, err := agent.ResolveSelector(body.From)
	if errors.Is(err, agent.ErrAmbiguous) {
		// Mirror handleMessages' handling of an ambiguous To: return the
		// candidate set so the dashboard can disambiguate rather than
		// flattening it into a misleading 404.
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":      "from matches multiple conversations",
			"code":       "ambiguous",
			"candidates": peerEntriesFromResolved(matches),
		})
		return
	}
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "resolve from: "+err.Error())
		return
	}
	dispatchSend(w, res.ConvID, &sendReq{
		To:      body.To,
		Subject: body.Subject,
		Body:    body.Body,
		Role:    body.Role,
		Members: body.Members,
	})
}
