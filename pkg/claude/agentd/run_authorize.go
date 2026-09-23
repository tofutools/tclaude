package agentd

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// The CLI executes the child locally. This endpoint authorizes its use from
// the daemon's peer-credential identity and returns the frozen sandbox policy
// when the run selects tclaude-layer. The caller cannot assert an agent ID.
type runAuthorizeRequest struct {
	SandboxImpl    string `json:"sandbox_impl"`
	SandboxProfile string `json:"sandbox_profile,omitempty"`
}

type runAuthorizeResponse struct {
	Snapshot *sandboxpolicy.Snapshot `json:"snapshot,omitempty"`
}

func handleRunAuthorize(w http.ResponseWriter, r *http.Request) {
	var input runAuthorizeRequest
	decoder := json.NewDecoder(io.LimitReader(r.Body, 8<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", fmt.Sprintf("decode run authorization: %v", err))
		return
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid_arg", "run authorization has trailing data")
		return
	}
	input.SandboxProfile = strings.TrimSpace(input.SandboxProfile)
	if input.SandboxImpl != "" && input.SandboxImpl != "harness-builtin" && input.SandboxImpl != "tclaude-layer" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid sandbox implementation")
		return
	}
	if input.SandboxProfile != "" && input.SandboxImpl != "tclaude-layer" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "sandbox profile requires tclaude-layer")
		return
	}
	profileName := ""
	if input.SandboxImpl == "tclaude-layer" {
		profileName = input.SandboxProfile
		if profileName == "" {
			global, err := db.GetGlobalSandboxProfile()
			if err != nil {
				writeError(w, http.StatusInternalServerError, "sandbox_profile", err.Error())
				return
			}
			if global != nil {
				profileName = global.Name
			}
		}
	}
	if _, ok := requirePermission(w, r, PermAgentRun, ActionContext{SandboxProfile: profileName}); !ok {
		return
	}
	response := runAuthorizeResponse{}
	if input.SandboxImpl == "tclaude-layer" {
		snapshot, err := db.ResolveEffectiveSandboxSnapshot(0, input.SandboxProfile)
		if err != nil {
			writeError(w, http.StatusBadRequest, "sandbox_profile", err.Error())
			return
		}
		response.Snapshot = &snapshot
	}
	writeJSON(w, http.StatusOK, response)
}
