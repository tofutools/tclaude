package agentd

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

const codexAppServerSupportedVersions = ">=0.147.0,<0.148.0"

var codexDiagnosticAbsolutePath = regexp.MustCompile(`(^|[[:space:]\(\"'=:])/[^\s,;:)\"]+`)

type codexAppServerDiagnostic struct {
	ConvID            string `json:"conv_id"`
	Harness           string `json:"harness"`
	Drive             string `json:"drive"`
	DriveSource       string `json:"drive_source"`
	Health            string `json:"health"`
	RuntimeState      string `json:"runtime_state,omitempty"`
	CodexVersion      string `json:"codex_version,omitempty"`
	SupportedVersions string `json:"supported_versions,omitempty"`
	Generation        string `json:"generation,omitempty"`
	LaunchID          string `json:"launch_id,omitempty"`
	ServerPID         int    `json:"server_pid,omitempty"`
	ServerIdentity    string `json:"server_identity"`
	SocketIdentity    string `json:"socket_identity"`
	ClientConnection  string `json:"client_connection"`
	ThreadBinding     string `json:"thread_binding"`
	ThreadID          string `json:"thread_id,omitempty"`
	ApprovalOwner     string `json:"approval_owner"`
	StatusObservedAt  string `json:"status_observed_at,omitempty"`
	UsageObservedAt   string `json:"usage_observed_at,omitempty"`
	ContextSource     string `json:"context_source"`
	MessageDelivery   string `json:"message_delivery"`
	Detail            string `json:"detail,omitempty"`
	Recovery          string `json:"recovery"`
	Rollback          string `json:"rollback"`
	CallerConv        string `json:"caller_conv,omitempty"`
	CallerAgentID     string `json:"caller_agent_id,omitempty"`
}

func handleWhoamiCodexAppServerStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method", "GET only")
		return
	}
	convID, ok := requireAgent(w, r)
	if !ok {
		return
	}
	writeCodexAppServerDiagnostic(w, convID, "")
}

func handleAgentCodexAppServerStatus(w http.ResponseWriter, r *http.Request, targetConv string) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method", "GET only")
		return
	}
	caller, ok := requireCrossAgentPermission(w, r, PermAgentContextInfo, targetConv)
	if !ok {
		return
	}
	writeCodexAppServerDiagnostic(w, targetConv, caller)
}

func writeCodexAppServerDiagnostic(w http.ResponseWriter, convID, caller string) {
	diagnostic, err := codexAppServerDiagnosticForConv(convID, time.Now().UTC())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	if caller != "" && caller != convID {
		diagnostic.CallerConv = caller
		diagnostic.CallerAgentID = peerAgentID(caller)
	}
	writeJSON(w, http.StatusOK, diagnostic)
}

func codexAppServerDiagnosticForConv(convID string, now time.Time) (codexAppServerDiagnostic, error) {
	d := codexAppServerDiagnostic{
		ConvID:           convID,
		Drive:            "send-keys",
		DriveSource:      "harness default (app-server off)",
		Health:           "ready",
		ServerIdentity:   "not used by the send-keys drive",
		SocketIdentity:   "not used by the send-keys drive",
		ClientConnection: "not used by the send-keys drive",
		ThreadBinding:    "Codex TUI conversation store",
		ApprovalOwner:    "Codex TUI",
		ContextSource:    "rollout follower; independent of the selected drive",
		MessageDelivery:  "tmux send-keys (established compatibility drive)",
		Recovery:         "resume keeps send-keys",
		Rollback:         "already on send-keys",
	}
	sessionRow, err := db.FindSessionByConvID(convID)
	if err != nil {
		return d, err
	}
	if sessionRow != nil {
		d.Harness = sessionRow.Harness
	}
	descriptor, resolveErr := harness.Resolve(d.Harness)
	if resolveErr != nil || !descriptor.CanCodexAppServer() {
		d.Drive = "unsupported"
		d.DriveSource = "harness capability"
		d.Health = "not-applicable"
		d.ServerIdentity = "Codex app-server is unavailable for this harness"
		d.SocketIdentity = "not allocated"
		d.ClientConnection = "not applicable"
		d.ThreadBinding = "not applicable"
		d.ApprovalOwner = "selected harness"
		d.ContextSource = "selected harness telemetry"
		d.MessageDelivery = "selected harness drive"
		d.Recovery = "use this harness's normal resume path"
		d.Rollback = "not applicable"
		return d, nil
	}
	posture, err := db.RecordedLaunchPostureForConv(convID)
	if err != nil {
		return d, err
	}
	selected := posture != nil && posture.CodexAppServer != nil && *posture.CodexAppServer
	if posture != nil && posture.CodexAppServerSource != nil && strings.TrimSpace(*posture.CodexAppServerSource) != "" {
		d.DriveSource = strings.TrimSpace(*posture.CodexAppServerSource)
	} else if posture != nil && posture.CodexAppServer != nil {
		d.DriveSource = "recorded launch posture"
	}
	if !selected {
		return d, nil
	}
	d.Drive = "app-server"
	d.SupportedVersions = codexAppServerSupportedVersions
	d.ServerIdentity = "pending generation proof"
	d.SocketIdentity = "private per-generation Unix socket; path withheld"
	d.ClientConnection = "not connected"
	d.ThreadBinding = "waiting for the TUI-created thread"
	d.MessageDelivery = "held until typed RPC control is ready; no send-keys fallback"
	d.Recovery = "wait for startup; if it does not settle, inspect detail and relaunch"
	d.Rollback = "after the agent is stopped: tclaude agent resume <agent> --send-keys"
	runtime, err := db.GetCodexAppServerRuntimeByConvID(convID)
	if err != nil {
		return d, err
	}
	if runtime == nil {
		d.Health = "disconnected"
		d.Detail = "the app-server drive is selected but no runtime generation is recorded"
		return d, nil
	}
	d.RuntimeState = runtime.State
	d.CodexVersion = runtime.CodexVersion
	d.Generation = runtime.Generation
	d.LaunchID = runtime.LaunchID
	d.ServerPID = runtime.ServerPID
	d.ThreadID = runtime.ThreadID
	d.Detail = redactCodexDiagnosticDetail(runtime.Detail)
	if runtime.ServerPID > 1 {
		d.ServerIdentity = "recorded process generation; identity proof is rechecked before recovery"
	}
	if runtime.ThreadID != "" {
		d.ThreadBinding = "bound and verified against thread/read"
	}
	handle := codexAppServerHandleForConv(convID)
	connected := handle != nil && handle.runtime.Generation == runtime.Generation
	observation := codexAppServerObservationSnapshot{}
	if connected {
		d.ClientConnection = "connected to the verified generation"
		d.ServerIdentity = "verified process generation"
		d.SocketIdentity = "verified private owner-only Unix socket; path withheld"
		observation = handle.observation.snapshot()
		if !observation.StatusAt.IsZero() {
			d.StatusObservedAt = observation.StatusAt.Format(time.RFC3339)
		}
		if !observation.UsageAt.IsZero() {
			d.UsageObservedAt = observation.UsageAt.Format(time.RFC3339)
		}
	}
	switch runtime.State {
	case db.CodexAppServerWarming:
		d.Health = "warming"
	case db.CodexAppServerRecovering:
		d.Health = "degraded"
		d.ClientConnection = "daemon recovery is re-proving the recorded generation"
		d.Recovery = "automatic same-generation recovery is in progress; messages remain held"
	case db.CodexAppServerReady:
		d.Health = "ready"
		d.MessageDelivery = "typed RPC; held during approval or user-input waits"
		d.Recovery = "daemon restarts and dropped clients re-adopt the same verified thread"
		if !connected {
			d.Health = "disconnected"
			d.ClientConnection = "no live daemon client for the recorded ready generation"
			d.MessageDelivery = "held; no send-keys fallback"
			d.Recovery = "automatic recovery should re-adopt the generation; relaunch if it fails"
		} else if observation.StatusAt.IsZero() || now.Sub(observation.StatusAt) > 5*codexAppServerStatusPollInterval {
			d.Health = "degraded"
			d.Detail = appendCodexDiagnosticDetail(d.Detail, "status snapshots are stale")
			d.Recovery = "the connection is live but observation is stale; controls fail closed until it recovers"
		}
	case db.CodexAppServerUnavailable:
		d.Health = "disconnected"
		d.MessageDelivery = "held; no send-keys fallback"
		d.Recovery = "stop and resume the app-server drive, or use the explicit rollback command"
	case db.CodexAppServerDead:
		d.Health = "crashed"
		d.ClientConnection = "closed"
		d.MessageDelivery = "held until resume; no send-keys fallback"
		d.Recovery = "resume to start a new app-server generation, or use the explicit rollback command"
	default:
		d.Health = "degraded"
		d.MessageDelivery = "held because the recorded runtime state is unknown; no send-keys fallback"
		d.Recovery = "stop and resume the app-server drive, or use the explicit rollback command"
	}
	return d, nil
}

func codexAppServerHealth(
	runtime *db.CodexAppServerRuntime,
	connected bool,
	observation codexAppServerObservationSnapshot,
	now time.Time,
) string {
	if runtime == nil {
		return "disconnected"
	}
	switch runtime.State {
	case db.CodexAppServerWarming:
		return "warming"
	case db.CodexAppServerRecovering:
		return "degraded"
	case db.CodexAppServerUnavailable:
		return "disconnected"
	case db.CodexAppServerDead:
		return "crashed"
	case db.CodexAppServerReady:
		if !connected {
			return "disconnected"
		}
		if observation.StatusAt.IsZero() || now.Sub(observation.StatusAt) > 5*codexAppServerStatusPollInterval {
			return "degraded"
		}
		return "ready"
	default:
		return "degraded"
	}
}

func redactCodexDiagnosticDetail(detail string) string {
	detail = strings.TrimSpace(detail)
	if detail == "" {
		return ""
	}
	return codexDiagnosticAbsolutePath.ReplaceAllStringFunc(detail, func(match string) string {
		prefix := match[:1]
		if prefix == "/" {
			prefix = ""
		}
		return prefix + "<private path>"
	})
}

func appendCodexDiagnosticDetail(existing, next string) string {
	if existing == "" {
		return next
	}
	return fmt.Sprintf("%s; %s", existing, next)
}
