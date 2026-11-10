package agentd

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Dismissing decisions (notify-human --option ...) that are no longer
// relevant, without answering them. The operator dismisses any open
// decision from the dashboard, one at a time or a whole list; an agent
// dismisses only decisions it asked, from the CLI. A dismissed decision
// leaves the "to decide" queue and stays in Messages, marked dismissed. As
// with an answer, the first of the two counts and the other gets a 409.
//
// When the operator dismisses, each asking agent gets one inbox message
// naming its dismissed decisions, so it stops waiting for an answer. An agent
// dismissing its own decisions is told by the command's output instead.

const maxDismissIDs = 500
const maxDismissReasonLen = 2048

type decisionDismissResult struct {
	ID        int64  `json:"id"`
	Dismissed bool   `json:"dismissed"`
	Error     string `json:"error,omitempty"`
	Code      string `json:"code,omitempty"`
}

// decisionDismissRefusal says why a message cannot be dismissed, "" when it can.
func decisionDismissRefusal(m *db.HumanMessage) (code, msg string) {
	switch {
	case m == nil:
		return "not_found", "no such decision"
	case len(m.Options) == 0:
		return "not_decision", "not a decision"
	case m.Decision != "":
		return "decided", "already answered: " + m.Decision
	case !m.DismissedAt.IsZero():
		return "dismissed", "already dismissed"
	}
	return "", ""
}

// dismissDecisions dismisses each message in order. mine, when non-nil,
// restricts the batch to the caller's own decisions. It returns one result per
// id and the messages it dismissed.
func dismissDecisions(ids []int64, by, reason string, mine func(*db.HumanMessage) bool) ([]decisionDismissResult, []*db.HumanMessage) {
	results := make([]decisionDismissResult, 0, len(ids))
	var dismissed []*db.HumanMessage
	for _, id := range ids {
		res := decisionDismissResult{ID: id}
		m, err := db.GetHumanMessage(id)
		if err != nil {
			res.Code, res.Error = "io", err.Error()
			results = append(results, res)
			continue
		}
		if m != nil && mine != nil && !mine(m) {
			// Indistinguishable from a missing one: an agent learns nothing
			// about decisions other agents asked.
			m = nil
		}
		if code, msg := decisionDismissRefusal(m); code != "" {
			res.Code, res.Error = code, msg
			results = append(results, res)
			continue
		}
		ok, err := db.DismissHumanMessage(id, by, reason)
		switch {
		case err != nil:
			res.Code, res.Error = "io", err.Error()
		case !ok:
			// Answered or dismissed between the read and the write.
			res.Code, res.Error = "decided", "already answered or dismissed"
		default:
			res.Dismissed = true
			dismissed = append(dismissed, m)
		}
		results = append(results, res)
	}
	for _, m := range dismissed {
		why, actor := "Dismissed in Messages", mirrorActorOperator
		if by == db.HumanDismissedByAgent {
			why, actor = "Withdrawn by its asker", "agent:"+m.FromAgent
		}
		humanMessagesEnded([]int64{m.ID}, why, actor)
	}
	return results, dismissed
}

// nudgeDismissedDecisions tells each asking agent, once, which of its
// decisions the operator dismissed. Best-effort: a decision stays dismissed if
// the message cannot be queued.
func nudgeDismissedDecisions(dismissed []*db.HumanMessage, reason string) {
	byAgent := map[string][]*db.HumanMessage{}
	for _, m := range dismissed {
		sel := m.FromAgent
		if sel == "" {
			sel = m.FromConv
		}
		if sel != "" {
			byAgent[sel] = append(byAgent[sel], m)
		}
	}
	selectors := make([]string, 0, len(byAgent))
	for sel := range byAgent {
		selectors = append(selectors, sel)
	}
	sort.Strings(selectors)
	for _, sel := range selectors {
		res, _, err := agent.ResolveSelector(sel)
		if err != nil || res == nil || res.ConvID == "" {
			slog.Info("decision dismiss: asking agent not resolvable; no nudge", "selector", sel)
			continue
		}
		msgs := byAgent[sel]
		lines := make([]string, 0, len(msgs))
		for _, m := range msgs {
			question := m.Subject
			if question == "" {
				question, _, _ = strings.Cut(m.Body, "\n")
			}
			lines = append(lines, fmt.Sprintf("- #%d: %s", m.ID, question))
		}
		subject := fmt.Sprintf("Decision #%d dismissed by the operator", msgs[0].ID)
		body := "The operator dismissed this decision without answering it, so don't wait for an answer:\n"
		if len(msgs) > 1 {
			subject = fmt.Sprintf("%d decisions dismissed by the operator", len(msgs))
			body = "The operator dismissed these decisions without answering them, so don't wait for answers:\n"
		}
		body += strings.Join(lines, "\n")
		if reason != "" {
			body += "\n\nReason: " + reason
		}
		if _, _, err := queueRegularAgentMessageWithAttachments(&db.AgentMessage{
			ToConv: res.ConvID, Subject: subject, Body: body,
			ToRecipients: []string{res.ConvID}, OperatorAuthored: true,
		}, nil); err != nil {
			slog.Warn("decision dismiss: nudge not queued", "conv", res.ConvID, "error", err)
		}
	}
}

type decisionDismissRequest struct {
	IDs    []int64 `json:"ids"`
	All    bool    `json:"all"`
	Reason string  `json:"reason"`
}

func decodeDecisionDismiss(w http.ResponseWriter, r *http.Request) (*decisionDismissRequest, bool) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return nil, false
	}
	var body decisionDismissRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", `body must be {"ids": [...], "reason": "..."}`)
		return nil, false
	}
	body.Reason = strings.TrimSpace(body.Reason)
	if len(body.Reason) > maxDismissReasonLen {
		writeError(w, http.StatusBadRequest, "invalid_arg", fmt.Sprintf("reason too long: %d bytes, max %d", len(body.Reason), maxDismissReasonLen))
		return nil, false
	}
	if len(body.IDs) > maxDismissIDs {
		writeError(w, http.StatusBadRequest, "invalid_arg", fmt.Sprintf("at most %d ids at a time", maxDismissIDs))
		return nil, false
	}
	return &body, true
}

// writeDecisionDismiss answers a batch: 200 with per-id results, except that a
// single id that was not dismissed gets its own status, so a one-off dismiss
// reads like an answer that lost the race.
func writeDecisionDismiss(w http.ResponseWriter, results []decisionDismissResult) {
	if len(results) == 1 && !results[0].Dismissed {
		status := http.StatusConflict
		switch results[0].Code {
		case "not_found":
			status = http.StatusNotFound
		case "not_decision":
			status = http.StatusBadRequest
		case "io":
			status = http.StatusInternalServerError
		}
		writeError(w, status, results[0].Code, results[0].Error)
		return
	}
	n := 0
	for _, res := range results {
		if res.Dismissed {
			n++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"dismissed": n, "results": results})
}

// handleDashboardHumanMessagesDismiss is POST /api/human-messages/dismiss
// {ids, reason}: the operator dismisses the listed decisions, whoever asked.
// The dashboard sends the ids it shows, so "Dismiss all" covers exactly the
// list on screen.
func handleDashboardHumanMessagesDismiss(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	body, ok := decodeDecisionDismiss(w, r)
	if !ok {
		return
	}
	if len(body.IDs) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_arg", "ids is required")
		return
	}
	results, dismissed := dismissDecisions(body.IDs, db.HumanDismissedByOperator, body.Reason, nil)
	setAuditTargetLabel(r, fmt.Sprintf("dismissed %d decision(s)", len(dismissed)))
	nudgeDismissedDecisions(dismissed, body.Reason)
	writeDecisionDismiss(w, results)
}

// handleNotifyHumanDismiss is POST /v1/notify-human/dismiss {ids | all, reason}:
// an agent dismisses decisions it asked. No slug: they are its own messages.
// Anyone else's decision reads as not found.
func handleNotifyHumanDismiss(w http.ResponseWriter, r *http.Request) {
	callerConv, ok := requireAgent(w, r)
	if !ok {
		return
	}
	body, ok := decodeDecisionDismiss(w, r)
	if !ok {
		return
	}
	if body.All == (len(body.IDs) > 0) {
		writeError(w, http.StatusBadRequest, "invalid_arg", "give either decision ids or all, exactly one")
		return
	}
	callerAgent, err := db.AgentIDForConv(callerConv)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	mine := func(m *db.HumanMessage) bool {
		if m.FromAgent != "" {
			return callerAgent != "" && m.FromAgent == callerAgent
		}
		return m.FromConv != "" && m.FromConv == callerConv
	}
	ids := body.IDs
	if body.All {
		open, err := db.ListOpenDecisionsFrom(callerAgent, callerConv)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		ids = make([]int64, 0, len(open))
		for _, m := range open {
			ids = append(ids, m.ID)
		}
		if len(ids) == 0 {
			writeJSON(w, http.StatusOK, map[string]any{"dismissed": 0, "results": []decisionDismissResult{}})
			return
		}
	}
	results, dismissed := dismissDecisions(ids, db.HumanDismissedByAgent, body.Reason, mine)
	setAuditTargetLabel(r, fmt.Sprintf("dismissed %d own decision(s)", len(dismissed)))
	if body.All {
		// --all is "whatever is open": one that another view answered
		// meanwhile is not an error for the caller.
		writeJSON(w, http.StatusOK, map[string]any{"dismissed": len(dismissed), "results": results})
		return
	}
	writeDecisionDismiss(w, results)
}
