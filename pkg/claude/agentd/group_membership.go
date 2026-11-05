package agentd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Group membership and ownership policy shared by the daemon socket
// (handleGroupMembers* / handleGroupOwners* in handlers.go) and the
// dashboard (dashboardAddMember and friends in dashboard_edit.go). Each
// surface runs its own auth and its own pre-checks first, then calls one of
// these, and writes the returned failure in its own wire format: JSON
// {error, code} on the socket, plain text on the dashboard.

// groupActiveFailure refuses a write to an archived group: a sealed group
// rejects every mutation (see requireGroupActive).
func groupActiveFailure(g *db.AgentGroup) *spawnFailure {
	if g == nil || !g.IsArchived() {
		return nil
	}
	return &spawnFailure{http.StatusConflict, "archived", errGroupArchived.Error()}
}

// resolveGroupMemberConv resolves a title / prefix / conv-id selector the
// way the CLI does. An unknown selector is not found.
func resolveGroupMemberConv(selector string) (string, *spawnFailure) {
	res, _, err := agent.ResolveSelector(selector)
	if err != nil {
		return "", &spawnFailure{http.StatusNotFound, "not_found", err.Error()}
	}
	return res.ConvID, nil
}

// decodeGroupConvBody decodes a {conv, ...} body into dst and returns the
// trimmed conv selector, refusing an empty one.
func decodeGroupConvBody(r *http.Request, dst any, conv *string) *spawnFailure {
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		return &spawnFailure{http.StatusBadRequest, "invalid_arg", err.Error()}
	}
	*conv = strings.TrimSpace(*conv)
	if *conv == "" {
		return &spawnFailure{http.StatusBadRequest, "invalid_arg", "conv is required"}
	}
	return nil
}

// addGroupMember decodes {conv, role?, descr?}, resolves conv and adds it to
// g. It returns the member's conv-id.
func addGroupMember(r *http.Request, g *db.AgentGroup) (string, *spawnFailure) {
	var body struct {
		Conv  string `json:"conv"`
		Role  string `json:"role,omitempty"`
		Descr string `json:"descr,omitempty"`
	}
	if fail := decodeGroupConvBody(r, &body, &body.Conv); fail != nil {
		return "", fail
	}
	convID, fail := resolveGroupMemberConv(body.Conv)
	if fail != nil {
		return "", fail
	}
	if err := db.AddAgentGroupMember(&db.AgentGroupMember{
		GroupID: g.ID,
		ConvID:  convID,
		Role:    body.Role,
		Descr:   body.Descr,
	}); err != nil {
		return "", &spawnFailure{http.StatusInternalServerError, "io", err.Error()}
	}
	return convID, nil
}

// updateGroupMember patches role/descr on an existing member. Only fields
// present in the body are touched; a body with neither is refused. It
// returns the member's conv-id.
func updateGroupMember(r *http.Request, g *db.AgentGroup, selector string) (string, *spawnFailure) {
	var body struct {
		Role  *string `json:"role,omitempty"`
		Descr *string `json:"descr,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return "", &spawnFailure{http.StatusBadRequest, "invalid_arg", err.Error()}
	}
	if body.Role == nil && body.Descr == nil {
		return "", &spawnFailure{http.StatusBadRequest, "invalid_arg", "at least one of role/descr is required"}
	}
	convID, fail := resolveGroupMemberConv(selector)
	if fail != nil {
		return "", fail
	}
	n, err := db.UpdateAgentGroupMember(g.ID, convID, body.Role, body.Descr)
	if err != nil {
		return "", &spawnFailure{http.StatusInternalServerError, "io", err.Error()}
	}
	if n == 0 {
		return "", &spawnFailure{http.StatusNotFound, "not_found", "no such member in group"}
	}
	return convID, nil
}

// removeGroupMember resolves selector and removes it from g.
func removeGroupMember(g *db.AgentGroup, selector string) *spawnFailure {
	convID, fail := resolveGroupMemberConv(selector)
	if fail != nil {
		return fail
	}
	if err := db.RemoveAgentGroupMember(g.ID, convID); err != nil {
		return &spawnFailure{http.StatusInternalServerError, "io", err.Error()}
	}
	return nil
}

// addGroupOwner decodes {conv}, resolves it and grants it ownership of g,
// recording grantedBy. It returns the owner's conv-id.
func addGroupOwner(r *http.Request, g *db.AgentGroup, grantedBy string) (string, *spawnFailure) {
	var body struct {
		Conv string `json:"conv"`
	}
	if fail := decodeGroupConvBody(r, &body, &body.Conv); fail != nil {
		return "", fail
	}
	convID, fail := resolveGroupMemberConv(body.Conv)
	if fail != nil {
		return "", fail
	}
	if err := db.AddAgentGroupOwner(g.ID, convID, grantedBy); err != nil {
		return "", &spawnFailure{http.StatusInternalServerError, "io", err.Error()}
	}
	return convID, nil
}

// removeGroupOwner resolves selector and revokes its ownership of g. A conv
// that was not an owner is not found.
func removeGroupOwner(g *db.AgentGroup, selector string) *spawnFailure {
	convID, fail := resolveGroupMemberConv(selector)
	if fail != nil {
		return fail
	}
	n, err := db.RemoveAgentGroupOwner(g.ID, convID)
	if err != nil {
		return &spawnFailure{http.StatusInternalServerError, "io", err.Error()}
	}
	if n == 0 {
		return &spawnFailure{http.StatusNotFound, "not_found",
			fmt.Sprintf("conv %s is not an owner of %q", convID, g.Name)}
	}
	return nil
}
