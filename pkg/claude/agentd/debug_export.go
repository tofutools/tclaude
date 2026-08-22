package agentd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/common/buildversion"
)

const agentDebugExportFormatVersion = 1

type agentDebugExport struct {
	Format         string                   `json:"format"`
	FormatVersion  int                      `json:"format_version"`
	GeneratedAt    string                   `json:"generated_at"`
	Tclaude        agentDebugTclaude        `json:"tclaude"`
	Agent          agentDebugIdentity       `json:"agent"`
	Configurations agentDebugConfigurations `json:"configurations"`
	Redactions     []string                 `json:"redactions"`
}

type agentDebugConfigurations struct {
	Requested agentDebugRequestedConfig `json:"requested"`
	Resolved  agentDebugResolvedConfig  `json:"resolved"`
	Running   agentDebugRunningConfig   `json:"running"`
}

type agentDebugRequestedConfig struct {
	Recorded   bool           `json:"recorded"`
	Parameters map[string]any `json:"parameters,omitempty"`
}

type agentDebugResolvedConfig struct {
	ConversationResume *db.ConversationResumeProfile `json:"conversation_resume,omitempty"`
	DurableRelaunch    *db.AgentRelaunchProfile      `json:"durable_relaunch,omitempty"`
	EffectiveSandbox   *sandboxpolicy.Snapshot       `json:"effective_sandbox,omitempty"`
}

type agentDebugRunningConfig struct {
	Recorded bool                    `json:"recorded"`
	Launch   *agentDebugLatestLaunch `json:"launch,omitempty"`
}

type agentDebugTclaude struct {
	Version        string                        `json:"version"`
	GoVersion      string                        `json:"go_version"`
	OS             string                        `json:"os"`
	Arch           string                        `json:"arch"`
	AgentdIdentity session.ExecutionUnixIdentity `json:"agentd_identity"`
}

type agentDebugIdentity struct {
	AgentID       string `json:"agent_id"`
	CurrentConvID string `json:"current_conv_id"`
	CreatedAt     string `json:"created_at"`
	CreatedVia    string `json:"created_via"`
	PendingName   string `json:"pending_name,omitempty"`
	Retired       bool   `json:"retired"`
}

// agentDebugLatestLaunch contains launch and sandbox evidence plus one
// best-effort live process observation. It deliberately omits unrelated
// activity such as token usage and subagent ledgers.
type agentDebugLatestLaunch struct {
	SessionID                 string                     `json:"session_id"`
	Status                    string                     `json:"status"`
	StatusDetail              string                     `json:"status_detail,omitempty"`
	CreatedAt                 string                     `json:"created_at"`
	UpdatedAt                 string                     `json:"updated_at"`
	Cwd                       string                     `json:"cwd"`
	Harness                   string                     `json:"harness"`
	HarnessBuiltinMode        string                     `json:"harness_builtin_mode"`
	SandboxImplementation     string                     `json:"sandbox_implementation"`
	HarnessBuiltinModeSource  string                     `json:"harness_builtin_mode_source,omitempty"`
	OSSandboxState            string                     `json:"os_sandbox_state,omitempty"`
	OSSandboxSource           string                     `json:"os_sandbox_source,omitempty"`
	OSSandboxUnverified       bool                       `json:"os_sandbox_unverified"`
	ApprovalPolicy            string                     `json:"approval_policy,omitempty"`
	ApprovalAutoReview        bool                       `json:"approval_auto_review"`
	AskUserQuestionTimeout    string                     `json:"ask_user_question_timeout,omitempty"`
	RemoteControl             bool                       `json:"remote_control"`
	AutoMemory                bool                       `json:"auto_memory"`
	ContextFeatures           map[string]string          `json:"context_features,omitempty"`
	AutoCompactWindow         string                     `json:"auto_compact_window,omitempty"`
	Model                     string                     `json:"model,omitempty"`
	ModelID                   string                     `json:"model_id,omitempty"`
	Effort                    string                     `json:"effort,omitempty"`
	ContextWindowSize         int64                      `json:"context_window_size,omitempty"`
	EffectiveSandbox          *sandboxpolicy.Snapshot    `json:"effective_sandbox,omitempty"`
	ExecutionBoundaryRecorded bool                       `json:"execution_boundary_recorded"`
	ExecutionBoundary         *session.ExecutionBoundary `json:"execution_boundary,omitempty"`
	LiveProcess               agentDebugLiveProcess      `json:"live_process"`
}

type agentDebugIDSet struct {
	Real       int `json:"real"`
	Effective  int `json:"effective"`
	Saved      int `json:"saved"`
	Filesystem int `json:"filesystem"`
}

type agentDebugLiveProcess struct {
	Status         string           `json:"status"`
	PID            int              `json:"pid,omitempty"`
	ExecutablePath string           `json:"executable_path,omitempty"`
	PATH           string           `json:"path,omitempty"`
	UID            *agentDebugIDSet `json:"uid,omitempty"`
	GID            *agentDebugIDSet `json:"gid,omitempty"`
	Groups         []int            `json:"groups,omitempty"`
	UIDMap         string           `json:"uid_map,omitempty"`
	GIDMap         string           `json:"gid_map,omitempty"`
	Detail         string           `json:"detail,omitempty"`
}

func buildAgentDebugExport(convID string) (*agentDebugExport, error) {
	actor, err := db.GetAgentByConv(convID)
	if err != nil {
		return nil, fmt.Errorf("load agent identity: %w", err)
	}
	if actor == nil {
		return nil, fmt.Errorf("conversation %s is not an agent", convID)
	}

	initialRaw, err := db.AgentInitialSpawnConfigForConv(convID)
	if err != nil {
		return nil, fmt.Errorf("load initial spawn request: %w", err)
	}
	initial, redactions, err := sanitizeInitialSpawnRequest(initialRaw)
	if err != nil {
		return nil, err
	}
	resume, err := db.ConversationResumeProfileForConv(convID)
	if err != nil {
		return nil, fmt.Errorf("load conversation resume profile: %w", err)
	}
	relaunch, err := db.AgentRelaunchProfileForConv(convID)
	if err != nil {
		return nil, fmt.Errorf("load durable relaunch profile: %w", err)
	}
	effective, err := db.AgentEffectiveSandboxConfigForConv(convID)
	if err != nil {
		return nil, fmt.Errorf("load effective sandbox snapshot: %w", err)
	}

	out := &agentDebugExport{
		Format: "tclaude-agent-debug", FormatVersion: agentDebugExportFormatVersion,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Tclaude: agentDebugTclaude{
			Version: buildversion.AppVersion(), GoVersion: runtime.Version(), OS: runtime.GOOS, Arch: runtime.GOARCH,
			AgentdIdentity: currentAgentdIdentity(),
		},
		Agent: agentDebugIdentity{
			AgentID: actor.AgentID, CurrentConvID: actor.CurrentConvID,
			CreatedAt: actor.CreatedAt.UTC().Format(time.RFC3339Nano), CreatedVia: actor.CreatedVia,
			PendingName: actor.PendingName, Retired: !actor.RetiredAt.IsZero(),
		},
		Configurations: agentDebugConfigurations{
			Requested: agentDebugRequestedConfig{Recorded: initialRaw != "", Parameters: initial},
			Resolved: agentDebugResolvedConfig{
				ConversationResume: resume, DurableRelaunch: relaunch, EffectiveSandbox: effective,
			},
			Running: agentDebugRunningConfig{},
		},
		Redactions: redactions,
	}
	if sessions, sessionErr := db.FindSessionsByConvID(actor.CurrentConvID); sessionErr != nil {
		return nil, fmt.Errorf("load latest launch: %w", sessionErr)
	} else if len(sessions) > 0 {
		s := sessions[0]
		// The query is ordered by activity for lifecycle callers. Select by
		// launch chronology here so a delayed hook from an old process generation
		// cannot displace the newest launch in the diagnostic export.
		for _, candidate := range sessions[1:] {
			if candidate.CreatedAt.After(s.CreatedAt) ||
				(candidate.CreatedAt.Equal(s.CreatedAt) && candidate.UpdatedAt.After(s.UpdatedAt)) {
				s = candidate
			}
		}
		context, contextErr := db.GetContextSnapshot(s.ID)
		if contextErr != nil {
			return nil, fmt.Errorf("load latest launch model and effort: %w", contextErr)
		}
		boundaryRaw, boundaryErr := db.SessionExecutionBoundary(s.ID)
		if boundaryErr != nil {
			return nil, fmt.Errorf("load latest launch execution boundary: %w", boundaryErr)
		}
		var boundary *session.ExecutionBoundary
		launchIdentity, identityErr := db.GetSessionExitLaunchIdentity(s.ID)
		if identityErr != nil {
			return nil, fmt.Errorf("load latest launch generation: %w", identityErr)
		}
		if boundaryRaw != "" {
			boundary = &session.ExecutionBoundary{}
			if err := json.Unmarshal([]byte(boundaryRaw), boundary); err != nil {
				return nil, fmt.Errorf("decode latest launch execution boundary: %w", err)
			}
			if boundary.LaunchGeneration == "" ||
				boundary.LaunchGeneration != launchIdentity.Generation {
				boundary = nil
			}
		}
		if s.Harness == "opencode" {
			boundary = openCodeDebugExecutionBoundary(s.ConvID)
		}
		out.Configurations.Running.Recorded = true
		out.Configurations.Running.Launch = &agentDebugLatestLaunch{
			SessionID: s.ID, Status: s.Status, StatusDetail: s.StatusDetail,
			CreatedAt: s.CreatedAt.UTC().Format(time.RFC3339Nano),
			UpdatedAt: s.UpdatedAt.UTC().Format(time.RFC3339Nano), Cwd: s.Cwd, Harness: s.Harness,
			HarnessBuiltinMode: s.HarnessBuiltinMode, SandboxImplementation: s.SandboxImplementation,
			HarnessBuiltinModeSource: s.HarnessBuiltinModeSource, OSSandboxState: s.OSSandboxState,
			OSSandboxSource: s.OSSandboxSource, OSSandboxUnverified: s.OSSandboxUnverified,
			ApprovalPolicy: s.ApprovalPolicy, ApprovalAutoReview: s.ApprovalAutoReview,
			AskUserQuestionTimeout: s.AskUserQuestionTimeout, RemoteControl: s.RemoteControl,
			AutoMemory: s.AutoMemory, ContextFeatures: s.ContextFeatures,
			AutoCompactWindow: s.AutoCompactWindow, Model: context.Model, ModelID: context.ModelID,
			Effort: context.EffortLevel, ContextWindowSize: context.ContextWindowSize,
			EffectiveSandbox:          s.EffectiveSandbox,
			ExecutionBoundaryRecorded: boundary != nil, ExecutionBoundary: boundary,
			LiveProcess: observeAgentProcess(liveDebugProcessRoot(s), s.Harness),
		}
	}
	return out, nil
}

func liveDebugProcessRoot(s *db.SessionRow) int {
	if s == nil {
		return 0
	}
	if s.Harness == "opencode" {
		if openCodeRuntime, err := db.GetOpenCodeRuntimeByConvID(s.ConvID); err == nil &&
			openCodeRuntime != nil && openCodeRuntime.PID > 0 && openCodeRuntime.PID != os.Getpid() &&
			openCodeRuntimeVerified(*openCodeRuntime) {
			return openCodeRuntime.PID
		}
		return 0
	}
	if panePID := livePanePID(s.TmuxSession); panePID > 0 {
		return panePID
	}
	// A database PID has no durable process-generation identity. Never expose
	// another process's environment merely because the kernel reused it.
	return 0
}

func openCodeDebugExecutionBoundary(convID string) *session.ExecutionBoundary {
	runtime, err := db.GetOpenCodeRuntimeByConvID(convID)
	if err != nil || runtime == nil || runtime.PID <= 0 || runtime.PID == os.Getpid() ||
		!openCodeRuntimeVerified(*runtime) || strings.TrimSpace(runtime.ExecutionBoundaryJSON) == "" {
		return nil
	}
	boundary := &session.ExecutionBoundary{}
	if err := json.Unmarshal([]byte(runtime.ExecutionBoundaryJSON), boundary); err != nil {
		return nil
	}
	return boundary
}

func sanitizeInitialSpawnRequest(raw string) (map[string]any, []string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, []string{}, nil
	}
	var request map[string]any
	if err := json.Unmarshal([]byte(raw), &request); err != nil {
		return nil, nil, fmt.Errorf("decode initial spawn request: %w", err)
	}
	redactions := []string{}
	if message, ok := request["initial_message"].(string); ok {
		request["initial_message"] = fmt.Sprintf("<redacted: %d bytes>", len(message))
		redactions = append(redactions, "configurations.requested.parameters.initial_message")
	}
	if _, ok := request["write_proof_token"]; ok {
		delete(request, "write_proof_token")
		redactions = append(redactions, "configurations.requested.parameters.write_proof_token")
	}
	return request, redactions, nil
}

func writeAgentDebugExport(w http.ResponseWriter, convID string, attachment bool) {
	payload, err := buildAgentDebugExport(convID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "debug_export", err.Error())
		return
	}
	if attachment {
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="tclaude-agent-debug-%s.json"`, short8(payload.Agent.AgentID)))
	}
	writeJSON(w, http.StatusOK, payload)
}

func handleWhoamiDebugExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method", "GET only")
		return
	}
	convID, ok := requireAgent(w, r)
	if !ok {
		return
	}
	writeAgentDebugExport(w, convID, false)
}

func handleAgentDebugExport(w http.ResponseWriter, r *http.Request, targetConv string) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method", "GET only")
		return
	}
	if _, ok := requireCrossAgentPermission(w, r, PermAgentDebugExport, targetConv); !ok {
		return
	}
	writeAgentDebugExport(w, targetConv, false)
}
