package agentd

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// /v1/agent/aliases — global head-alias layer. Stable handles that
// resolve to the live head of a conv-succession chain via
// db.ResolveLatestConv. Complements per-group agent_group_members.alias
// by being NOT scoped to a group: useful for convs that aren't (yet)
// in any group, or where a global handle is more memorable.
//
// Verbs:
//
//	GET    /v1/agent/aliases             → list every handle (open)
//	POST   /v1/agent/aliases             → set handle → conv (human-only)
//	DELETE /v1/agent/aliases/{handle}    → drop handle (human-only)
//
// Mutations are gated on `requireHuman` for v1 (no claude ancestor in
// the caller's process tree). Cross-machine sync / agent-self-naming
// can ladder up later via a slug; until then humans add the handles.

type headAliasJSON struct {
	Handle    string `json:"handle"`
	Anchor    string `json:"anchor_conv_id"`
	Head      string `json:"head_conv_id"`         // anchor walked through ResolveLatestConv
	HeadTitle string `json:"head_title,omitempty"` // display title of the head, when known
	CreatedAt string `json:"created_at,omitempty"`
	// AnchorAgent is the stable agent_id of the anchored actor; ByAgent is the
	// stable agent_id of who set the alias (the actor). AnchorConvID / ByConv
	// are the conv-id snapshots. Anchor resolution is unchanged — Head still
	// derives from AnchorConvID via the succession chain (KEEP-2); AnchorAgent
	// is surfaced read-only for attribution.
	AnchorAgent string `json:"anchor_agent_id,omitempty"`
	ByAgent     string `json:"by_agent,omitempty"`
	ByConv      string `json:"by_conv,omitempty"`
}

func handleHeadAliases(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		// List is open: knowing what handles exist is read-only metadata,
		// same threat model as `/v1/peers`.
		listHeadAliases(w)
	case http.MethodPost:
		if !requireHuman(w, r, "set head alias") {
			return
		}
		setHeadAlias(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method", "GET or POST only")
	}
}

func handleHeadAliasByHandle(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/v1/agent/aliases/")
	if rest == "" || strings.Contains(rest, "/") {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			"expected /v1/agent/aliases/{handle}")
		return
	}
	handle := rest
	if u, err := url.PathUnescape(handle); err == nil {
		handle = u
	}
	switch r.Method {
	case http.MethodGet:
		// Read a single handle. Open like the list endpoint.
		row, err := db.GetHeadAlias(handle)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		if row == nil {
			writeError(w, http.StatusNotFound, "not_found",
				"no head alias named "+handle)
			return
		}
		writeJSON(w, http.StatusOK, headAliasRowToJSON(row))
	case http.MethodDelete:
		if !requireHuman(w, r, "drop head alias") {
			return
		}
		n, err := db.RemoveHeadAlias(handle)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		if n == 0 {
			writeError(w, http.StatusNotFound, "not_found",
				"no head alias named "+handle)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method", "GET or DELETE only")
	}
}

func listHeadAliases(w http.ResponseWriter) {
	rows, err := db.ListHeadAliases()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	out := make([]headAliasJSON, 0, len(rows))
	for _, h := range rows {
		out = append(out, headAliasRowToJSON(h))
	}
	writeJSON(w, http.StatusOK, out)
}

func setHeadAlias(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Handle string `json:"handle"`
		Conv   string `json:"conv"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	body.Handle = strings.TrimSpace(body.Handle)
	body.Conv = strings.TrimSpace(body.Conv)
	if err := db.ValidateHeadAliasHandle(body.Handle); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	res, _, err := agent.ResolveSelector(body.Conv)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found",
			"resolve conv: "+err.Error())
		return
	}
	if err := db.SetHeadAlias(body.Handle, res.ConvID, ""); err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	row, _ := db.GetHeadAlias(body.Handle)
	if row == nil {
		// Should never happen — we just inserted it. Fall back to a
		// minimal response shape so callers don't crash.
		writeJSON(w, http.StatusOK, headAliasJSON{
			Handle: strings.ToLower(body.Handle), Anchor: res.ConvID, Head: res.ConvID,
		})
		return
	}
	writeJSON(w, http.StatusOK, headAliasRowToJSON(row))
}

func headAliasRowToJSON(h *db.HeadAlias) headAliasJSON {
	out := headAliasJSON{
		Handle:      h.Handle,
		Anchor:      h.AnchorConvID,
		Head:        db.ResolveLatestConv(h.AnchorConvID),
		AnchorAgent: h.AnchorAgentID,
		ByAgent:     h.ByAgent,
		ByConv:      h.ByConv,
	}
	if !h.CreatedAt.IsZero() {
		out.CreatedAt = h.CreatedAt.Format(time.RFC3339)
	}
	if row := agent.FreshConvRowResolved(out.Head); row != nil {
		out.HeadTitle = agent.DisplayTitle(row)
	}
	return out
}

// requireHuman gates a mutation on the caller being the human operator
// (classHuman — the cookie-authenticated dashboard, or a CLI caller with
// a valid operator token). Fails closed: an agent, an unidentified peer,
// or an unconfirmed caller is refused. Broken out here so human-only
// endpoints can gate in a single line. Writes the response on rejection;
// returns true when the caller may proceed.
func requireHuman(w http.ResponseWriter, r *http.Request, action string) bool {
	switch classify(peerFromContext(r.Context())) {
	case classHuman:
		return true
	case classUnidentified:
		writeError(w, http.StatusUnauthorized, "auth",
			"could not determine peer PID; refusing to "+action)
	case classUnconfirmed:
		writeUnconfirmed(w)
	default: // classAgent, classAgentUnknown
		writeError(w, http.StatusForbidden, "auth",
			"only the human operator may "+action+"; this endpoint has no agent path")
	}
	return false
}
