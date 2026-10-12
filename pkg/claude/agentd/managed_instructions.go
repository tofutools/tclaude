package agentd

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/managedinstructions"
)

func registerManagedInstructionRoutes(mux *http.ServeMux) {
	for _, method := range []string{"GET", "POST", "DELETE"} {
		mux.HandleFunc(method+" /v1/instructions/{kind}", handleManagedInstructions)
	}
}
func handleManagedInstructions(w http.ResponseWriter, r *http.Request) {
	if c := classify(peerFromContext(r.Context())); c != classHuman && c != classAgent {
		if !requireHuman(w, r, "read managed instructions") {
			return
		}
	}
	kind := r.PathValue("kind")
	if kind != "skill" && kind != "rule" {
		writeError(w, 400, "invalid_arg", "kind must be skill or rule")
		return
	}
	var body struct {
		Group     string `json:"group"`
		User      bool   `json:"user"`
		Name      string `json:"name"`
		Content   []byte `json:"content"`
		Overwrite bool   `json:"overwrite"`
	}
	if r.Method == http.MethodGet {
		body.Group = r.URL.Query().Get("group")
		body.User = r.URL.Query().Get("user") == "1"
		body.Name = r.URL.Query().Get("name")
	} else {
		if !requireHuman(w, r, "manage operator skills and rules") {
			return
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2*db.ManagedInstructionItemLimit))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&body); err != nil {
			writeError(w, 400, "invalid_arg", "expected bounded instruction JSON")
			return
		}
		if err := decoder.Decode(&struct{}{}); err != io.EOF {
			writeError(w, 400, "invalid_arg", "expected one JSON object")
			return
		}
	}
	if (body.Group == "") == !body.User {
		writeError(w, 400, "invalid_arg", "choose exactly one of group or user scope")
		return
	}
	var groupID int64
	if body.Group != "" {
		g, err := db.GetAgentGroupByName(body.Group)
		if err != nil {
			writeError(w, 500, "io", err.Error())
			return
		}
		if g == nil {
			writeError(w, 404, "not_found", "group not found")
			return
		}
		groupID = g.ID
		if p := peerFromContext(r.Context()); classify(p) == classAgent {
			agentID, err := db.AgentIDForConv(p.ConvID)
			if err != nil {
				writeError(w, 403, "permission_denied", "agent identity unavailable")
				return
			}
			memberships, err := db.ListAgentGroupMembershipsByJoin(agentID)
			if err != nil {
				writeError(w, 500, "io", err.Error())
				return
			}
			allowed := false
			for _, m := range memberships {
				if m.Group.ID == groupID {
					allowed = true
				}
			}
			if !allowed {
				writeError(w, 403, "permission_denied", "group instructions are visible to its members only")
				return
			}
		}
	}
	switch r.Method {
	case http.MethodGet:
		rows, err := db.ListManagedInstructions(groupID, kind)
		if err != nil {
			writeError(w, 500, "io", err.Error())
			return
		}
		if body.Name != "" {
			for _, row := range rows {
				if row.Name == body.Name {
					writeJSON(w, 200, row)
					return
				}
			}
			writeError(w, 404, "not_found", "instruction not found")
			return
		}
		for i := range rows {
			rows[i].Content = nil
		}
		writeJSON(w, 200, map[string]any{"items": rows})
	case http.MethodPost:
		if err := managedinstructions.Validate(kind, body.Name, body.Content); err != nil {
			writeError(w, 400, "invalid_arg", err.Error())
			return
		}
		digest, err := db.PutManagedInstruction(groupID, kind, body.Name, body.Content, body.Overwrite)
		if err != nil {
			writeError(w, 409, "conflict", err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"name": body.Name, "digest": digest})
	case http.MethodDelete:
		removed, err := db.DeleteManagedInstruction(groupID, kind, body.Name)
		if err != nil {
			writeError(w, 400, "invalid_arg", err.Error())
			return
		}
		if !removed {
			writeError(w, 404, "not_found", "instruction not found")
			return
		}
		writeJSON(w, 200, map[string]any{"removed": true})
	}
}
