package proto

import (
	"math"
	"net/url"
	"strings"
	"time"
)

const CapAgentStatus = "agents.status.read"
const KindAgentStatusUpdate = "agents_status_update"

// AgentStatus is an explicit allowlist. Never embed the private dashboard state.
type AgentStatus struct {
	Agent            string        `json:"agent"`
	Name             string        `json:"name"`
	Role             string        `json:"role,omitempty"`
	Online           bool          `json:"online"`
	Status           string        `json:"status,omitempty"`
	WaitingReason    string        `json:"waiting_reason,omitempty"`
	Harness          string        `json:"harness,omitempty"`
	Model            string        `json:"model,omitempty"`
	Effort           string        `json:"effort_level,omitempty"`
	Subagents        int           `json:"subagent_count"`
	BackgroundShells int           `json:"background_shell_count"`
	Monitors         int           `json:"monitor_count"`
	TaskURL          string        `json:"task_ref_url,omitempty"`
	TaskLabel        string        `json:"task_ref_label,omitempty"`
	Context          *AgentContext `json:"context"`
	LastActivity     *time.Time    `json:"last_activity_at"`
	ExitReason       string        `json:"exit_reason,omitempty"`
	RecoveryStatus   string        `json:"recovery_status,omitempty"`
}
type AgentContext struct {
	Percent      float64 `json:"percent"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	Window       int64   `json:"window"`
}
type AgentStatusGroupUpdate struct {
	PublishedAt time.Time     `json:"published_at"`
	Name        string        `json:"name"`
	Statuses    []AgentStatus `json:"statuses"`
	At          time.Time     `json:"at"`
}
type AgentStatusUpdatePayload struct {
	Groups []AgentStatusGroupUpdate `json:"groups"`
}

func SafeStatusTaskURL(raw string) string {
	if len(raw) > 2048 {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	path := strings.ToLower(u.Path)
	if (host == "claude.ai" && strings.HasPrefix(path, "/code")) || strings.Contains(path, "/sessions/") {
		return ""
	}
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	u.RawFragment = ""
	for _, r := range u.String() {
		if r < 0x20 || r == 0x7f {
			return ""
		}
	}
	return u.String()
}
func SanitizeAgentStatuses(in []AgentStatus) []AgentStatus {
	out := make([]AgentStatus, 0, len(in))
	seen := map[string]bool{}
	for _, s := range in {
		if !ValidAgentRef(s.Agent) || seen[s.Agent] {
			continue
		}
		seen[s.Agent] = true
		s.Name = SafeName(s.Name, false)
		s.Role = safeOptional(s.Role)
		s.Harness = safeOptional(s.Harness)
		s.Model = safeOptional(s.Model)
		s.Effort = safeOptional(s.Effort)
		s.TaskLabel = safeOptional(s.TaskLabel)
		s.TaskURL = SafeStatusTaskURL(s.TaskURL)
		if s.TaskURL == "" {
			s.TaskLabel = ""
		}
		switch s.Status {
		case "working", "running", "main_agent_idle", "error", "idle", "awaiting_permission", "awaiting_input", "exited", "starting", "unknown":
		default:
			s.Status = "unknown"
		}
		switch s.WaitingReason {
		case "permission", "question", "prompt":
		default:
			s.WaitingReason = ""
		}
		switch s.ExitReason {
		case "clean", "crashed", "stopped", "unknown":
		default:
			s.ExitReason = ""
		}
		switch s.RecoveryStatus {
		case "crashed", "restarting", "backoff", "recovered", "suppressed", "cancelled":
		default:
			s.RecoveryStatus = ""
		}
		s.Subagents = max(0, s.Subagents)
		s.BackgroundShells = max(0, s.BackgroundShells)
		s.Monitors = max(0, s.Monitors)
		if c := s.Context; c != nil && (math.IsNaN(c.Percent) || math.IsInf(c.Percent, 0) || c.Percent < 0 || c.Percent > 100 || c.InputTokens < 0 || c.OutputTokens < 0 || c.Window <= 0) {
			s.Context = nil
		}
		if s.LastActivity != nil && s.LastActivity.IsZero() {
			s.LastActivity = nil
		}
		out = append(out, s)
	}
	return out
}
