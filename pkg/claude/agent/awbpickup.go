package agent

import "time"

// Wire types for the operator-only AWB ready-pickup status surface
// (`tclaude pickup`, GET /v1/pickup, POST /v1/pickup/{process}/reset).
// They live here rather than in agentd so the CLI can decode them without
// importing the daemon.

// AWB pickup process states, derived by the daemon from the dispatch row plus
// the live issue and agent state.
const (
	AWBPickupStatePolling   = "polling"   // nothing in flight; waiting for a ready issue
	AWBPickupStateHeld      = "held"      // nothing in flight; usage ceiling holds new pickups
	AWBPickupStateError     = "error"     // the last poll failed
	AWBPickupStateStarting  = "starting"  // issue selected/claimed, agent not spawned yet
	AWBPickupStateWorking   = "working"   // spawned agent is running
	AWBPickupStateAgentIdle = "idle"      // spawned agent is idle, issue still open
	AWBPickupStateAwaiting  = "awaiting"  // monitored PR/commit recorded, waiting for it to reach main
	AWBPickupStateReleasing = "releasing" // issue closed; the next poll releases the process
	AWBPickupStateStuck     = "stuck"     // agent is gone but the issue is still open
	AWBPickupStateOrphaned  = "orphaned"  // dispatch row for a process no longer configured
)

// AWBPickupList is the GET /v1/pickup response.
type AWBPickupList struct {
	Processes []AWBPickupProcess `json:"processes"`
}

// AWBPickupProcess is one ready-polling process and its current dispatch.
type AWBPickupProcess struct {
	Process    string   `json:"process"`
	Workspace  string   `json:"workspace"`
	Labels     []string `json:"labels,omitempty"`
	Group      string   `json:"group,omitempty"`
	Interval   string   `json:"interval,omitempty"`
	// Configured is false for a leftover dispatch row whose process the
	// running daemon did not start (removed or renamed in config).
	Configured bool `json:"configured"`
	// LastPollAt / LastError describe the most recent poll this daemon ran.
	LastPollAt    *time.Time `json:"last_poll_at,omitempty"`
	LastError     string     `json:"last_error,omitempty"`
	RateLimitHold string     `json:"rate_limit_hold,omitempty"`
	State         string     `json:"state"`
	// Hint is a one-line human explanation of State, when it needs one.
	Hint     string             `json:"hint,omitempty"`
	Dispatch *AWBPickupDispatch `json:"dispatch,omitempty"`
}

// AWBPickupDispatch is the in-flight issue of a process, with the live
// state of the issue in AWB and of the agent working it.
type AWBPickupDispatch struct {
	IssueID     string          `json:"issue_id"`
	Phase       string          `json:"phase"`
	AgentID     string          `json:"agent_id"`
	LatestError string          `json:"latest_error,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
	Issue       *AWBPickupIssue `json:"issue,omitempty"`
	IssueError  string          `json:"issue_error,omitempty"`
	Agent       *AWBPickupAgent `json:"agent,omitempty"`
}

// AWBPickupIssue is the live AWB view of a dispatched issue.
type AWBPickupIssue struct {
	Status         string   `json:"status"`
	Title          string   `json:"title,omitempty"`
	Assignees      []string `json:"assignees,omitempty"`
	PullRequestURL string   `json:"pull_request_url,omitempty"`
	CommitHash     string   `json:"commit_hash,omitempty"`
	URL            string   `json:"url,omitempty"`
}

// AWBPickupAgent is the live view of the agent reserved for a dispatch.
type AWBPickupAgent struct {
	// Exists is false when no agent row exists for the reserved id yet.
	Exists        bool   `json:"exists"`
	PendingSpawn  bool   `json:"pending_spawn,omitempty"`
	Name          string `json:"name,omitempty"`
	Retired       bool   `json:"retired,omitempty"`
	ConvID        string `json:"conv_id,omitempty"`
	SessionStatus string `json:"session_status,omitempty"`
}

// AWBPickupResetRequest is the POST /v1/pickup/{process}/reset body.
// IssueID, when set, makes the reset a compare-and-set: it only removes the
// dispatch if the process still holds that issue.
type AWBPickupResetRequest struct {
	IssueID string `json:"issue_id,omitempty"`
}

// AWBPickupResetResponse reports what a reset removed.
type AWBPickupResetResponse struct {
	Process  string             `json:"process"`
	Reset    bool               `json:"reset"`
	Dispatch *AWBPickupDispatch `json:"dispatch,omitempty"`
}
