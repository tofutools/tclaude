package agentd

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// tags_handlers.go serves the per-agent tag-set endpoints:
//
//	GET/PUT /v1/whoami/tags        → read/replace the CALLER's own tags (self.tags)
//	GET/PUT /v1/agent/{conv}/tags  → read/replace ANOTHER agent's tags (agent.tags / owner)
//
// The write is a REPLACE-SET: a PUT body {"tags": [...]} sets the agent's
// tag set to exactly that list (empty clears it). add/rm compose on top
// of this client-side in the CLI (read → mutate → replace). Like the
// task-ref endpoints this is a pure DB write — no tmux send-keys — so
// there is no injection sink; the only guard is the tag charset/length
// validation (db.NormalizeAgentTag), UI hygiene rather than security.
//
// Tags are dashboard/CLI-only (they never ride the agent-facing
// /v1/peers surface), so even the cross-agent READ stays behind the same
// manager gate as the write — an agent can't enumerate peers' tags, only
// a manager (slug or group owner) can.

// handleWhoamiTags reads (GET) or replaces (PUT/POST) the calling agent's
// own tag set. Permission-gated on self.tags.
func handleWhoamiTags(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodPut, http.MethodPost:
	default:
		writeError(w, http.StatusMethodNotAllowed, "method", "GET, PUT or POST only")
		return
	}
	convID, ok := requirePermission(w, r, PermSelfTags)
	if !ok {
		return
	}
	if convID == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			"this endpoint operates on the calling agent's own tags; use /v1/agent/{conv}/tags to act on another agent")
		return
	}
	if r.Method == http.MethodGet {
		writeTagsResponse(w, convID, convID)
		return
	}
	runTagsReplace(w, r, convID, convID)
}

// handleAgentTags reads (GET) or replaces (PUT/POST) ANOTHER agent's tag
// set. Routed via handleAgentByConv. Auth: agent.tags slug OR caller owns
// a group containing the target.
func handleAgentTags(w http.ResponseWriter, r *http.Request, targetConv string) {
	switch r.Method {
	case http.MethodGet, http.MethodPut, http.MethodPost:
	default:
		writeError(w, http.StatusMethodNotAllowed, "method", "GET, PUT or POST only")
		return
	}
	caller, ok := requireCrossAgentPermission(w, r, PermAgentTags, targetConv)
	if !ok {
		return
	}
	if r.Method == http.MethodGet {
		writeTagsResponse(w, targetConv, caller)
		return
	}
	runTagsReplace(w, r, targetConv, caller)
}

// runTagsReplace decodes the request body, validates the tags, replaces
// the target agent's tag set, and responds with the resulting set.
func runTagsReplace(w http.ResponseWriter, r *http.Request, target, caller string) {
	var body struct {
		Tags []string `json:"tags"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	agentID, err := db.AgentIDForConv(target)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db", err.Error())
		return
	}
	if agentID == "" {
		writeError(w, http.StatusNotFound, "not_found", "no agent enrolled for conv "+short8(target))
		return
	}
	// ReplaceAgentTags validates + de-dupes + caps; surface a bad tag as a
	// 400 while a DB failure below stays a 500.
	if err := db.ReplaceAgentTags(agentID, body.Tags); err != nil {
		if isTagValidationError(err) {
			writeError(w, http.StatusBadRequest, "invalid_tag", err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "db", err.Error())
		return
	}
	writeTagsResponse(w, target, caller)
}

// writeTagsResponse loads and returns a conv's current tag set.
func writeTagsResponse(w http.ResponseWriter, convID, caller string) {
	agentID, err := db.AgentIDForConv(convID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db", err.Error())
		return
	}
	if agentID == "" {
		writeError(w, http.StatusNotFound, "not_found", "no agent enrolled for conv "+short8(convID))
		return
	}
	tags, err := db.ListAgentTags(agentID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db", err.Error())
		return
	}
	resp := map[string]any{
		"conv_id": convID,
		"tags":    tags,
	}
	if caller != "" && caller != convID {
		resp["caller_conv"] = caller
		stampCallerAgentID(resp, caller)
	}
	writeJSON(w, http.StatusOK, resp)
}

// isTagValidationError reports whether err is a tag-shape rejection (bad
// charset / too long / too many) rather than an I/O failure, so the
// handler can map it to a 400 instead of a 500. The db layer returns
// plain errors, so this matches on the messages those validators produce
// — kept in lockstep with db.NormalizeAgentTag / normalizeAgentTagSet.
func isTagValidationError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, needle := range []string{
		"tag is empty",
		"tag is too long",
		"tag contains a control character",
		"tag must not contain a comma",
		"too many tags",
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}
