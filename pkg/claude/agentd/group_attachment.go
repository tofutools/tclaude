package agentd

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// groupAttachmentView is the wire shape for a group's persistent reference.
// Label is the effective display label; LabelOverride is the raw human choice
// so editors can distinguish an automatic label from an explicit one.
type groupAttachmentView struct {
	URL           string `json:"attachment_url,omitempty"`
	Label         string `json:"attachment_label,omitempty"`
	LabelOverride string `json:"attachment_label_override,omitempty"`
}

func groupAttachmentViewFor(g *db.AgentGroup) groupAttachmentView {
	if g == nil || strings.TrimSpace(g.AttachmentURL) == "" {
		return groupAttachmentView{}
	}
	ref := db.AgentTaskRef{URL: g.AttachmentURL, Label: g.AttachmentLabel}
	return groupAttachmentView{
		URL:           g.AttachmentURL,
		Label:         effectiveTaskLabel(ref),
		LabelOverride: strings.TrimSpace(g.AttachmentLabel),
	}
}

// handleGroupAttachment serves the persistent group-reference API:
//
//	GET  /v1/groups/{name}/attachment  → current URL + effective label
//	POST /v1/groups/{name}/attachment  → set ({url,label}) or clear ({clear:true})
//
// Reads are open like GET /v1/groups. Writes use the dedicated
// groups.attachment capability with the standard owner-of-this-group bypass.
func handleGroupAttachment(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
	switch r.Method {
	case http.MethodGet:
		writeGroupAttachmentResponse(w, g, false)
		return
	case http.MethodPost:
	default:
		writeError(w, http.StatusMethodNotAllowed, "method", "GET or POST only")
		return
	}
	if _, ok := requireGroupPermission(w, r, PermGroupsAttachment, g); !ok {
		return
	}
	if !requireGroupActive(w, g) {
		return
	}
	var body struct {
		URL   string `json:"url"`
		Label string `json:"label"`
		Clear bool   `json:"clear"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	body.URL = strings.TrimSpace(body.URL)
	body.Label = strings.TrimSpace(body.Label)
	if body.Clear || body.URL == "" {
		n, err := db.SetAgentGroupAttachment(g.Name, "", "")
		if err != nil {
			writeError(w, http.StatusInternalServerError, "db", err.Error())
			return
		}
		if n == 0 {
			writeError(w, http.StatusNotFound, "not_found", "no such group")
			return
		}
		g.AttachmentURL, g.AttachmentLabel = "", ""
		writeGroupAttachmentResponse(w, g, true)
		return
	}
	if err := validateTaskRefURL(body.URL); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_attachment_url", err.Error())
		return
	}
	if err := validateTaskRefLabel(body.Label); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_attachment_label", err.Error())
		return
	}
	n, err := db.SetAgentGroupAttachment(g.Name, body.URL, body.Label)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db", err.Error())
		return
	}
	if n == 0 {
		writeError(w, http.StatusNotFound, "not_found", "no such group")
		return
	}
	g.AttachmentURL, g.AttachmentLabel = body.URL, body.Label
	writeGroupAttachmentResponse(w, g, false)
}

func writeGroupAttachmentResponse(w http.ResponseWriter, g *db.AgentGroup, cleared bool) {
	view := groupAttachmentViewFor(g)
	writeJSON(w, http.StatusOK, map[string]any{
		"group":                     g.Name,
		"attachment_url":            view.URL,
		"attachment_label":          view.Label,
		"attachment_label_override": view.LabelOverride,
		"cleared":                   cleared,
	})
}
