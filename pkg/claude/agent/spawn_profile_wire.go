package agent

import (
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// SpawnProfileJSON is the wire shape of a spawn profile, shared by the daemon
// (agentd, which serves and stores it) and the CLI (`tclaude agent profiles`,
// whose `show --json` round-trips exactly what the dashboard editor posts).
// One definition, so a new profile option is added once. The field order is
// the daemon's, which is the order its responses and bundle files are written
// in.
//
// The five toggles are *bool so an absent/null field round-trips as unset
// (nil), distinct from an explicit false. CreatedAt / UpdatedAt are
// response-only (ignored on input). Every field is optional: a blank text
// field / absent toggle is unset.
type SpawnProfileJSON struct {
	Name    string   `json:"name"`
	Aliases []string `json:"aliases,omitempty"`
	// Disabled is the authoritative spawn gate. DisabledReason is remembered
	// independently so enabling does not discard the operator's explanation.
	// It is a pointer so CLI file input can tell an explicit false from the
	// pre-v122 shape where disabled_reason was the disable switch.
	Disabled       *bool  `json:"disabled,omitempty"`
	DisabledReason string `json:"disabled_reason,omitempty"`
	// OperatorOnly permits human/operator launches but rejects an agent caller
	// that resolves this profile at any launch tier.
	OperatorOnly bool `json:"operator_only,omitempty"`

	// Launch fields — overlap clcommon.SpawnArgs.
	Harness    string `json:"harness,omitempty"`
	Model      string `json:"model,omitempty"`
	ModelProxy string `json:"model_proxy,omitempty"`
	Effort     string `json:"effort,omitempty"`
	Sandbox    string `json:"sandbox,omitempty"`
	// SandboxImplementation pins who owns OS-level confinement:
	// "harness-builtin" (the legacy default) or the
	// "tclaude-layer" OS wrapper. "" = unset, which falls through to the next
	// precedence tier at spawn — distinct from an explicit "harness-builtin",
	// which pins the legacy implementation against a lower tier and is valid
	// only for a harness that owns a real built-in OS sandbox. A tclaude-layer
	// value on a profile whose harness cannot host the layer is a 400 at save.
	// Host capability is NOT checked at save:
	// pinning it before bwrap is installed is legitimate authoring, and the
	// launch refuses loudly if the host still cannot run it. See TCL-769.
	SandboxImplementation string `json:"sandbox_implementation,omitempty"`
	Approval              string `json:"approval,omitempty"`
	ToolGovernance        string `json:"tools,omitempty"`
	// AskUserQuestionTimeout is the profile's Claude Code AskUserQuestion
	// idle-timeout default (inherit|never|60s|5m|10m; "" = unset), delivered
	// per-spawn via `--settings`. Claude-Code-only — a value on a Codex profile
	// is a 400 (buildProfileFromJSON gates it on the profile's harness).
	AskUserQuestionTimeout string `json:"ask_user_question_timeout,omitempty"`
	// AutoCompactWindow is the profile's auto-compaction context capacity in
	// tokens ("450000"; "" = unset, the model's own threshold decides). Accepts
	// "450k" / "0.5M" on the way in and normalizes to plain digits. Claude-Code-only
	// — a value on a Codex profile is a 400 (buildProfileFromJSON gates it on the
	// profile's harness).
	AutoCompactWindow string `json:"auto_compact_window,omitempty"`
	// ContextWindowMax is a Copilot-only configured context cap. Zero
	// leaves the cap unset so the observed-model default may speak.
	ContextWindowMax int64 `json:"context_window_max,omitempty"`
	// CopilotAPI is the profile's Copilot-only API-backed-drive default —
	// tri-state (null = unset, false = tmux send-keys, true = the embedded
	// JSON-RPC API). Unset resolves to send-keys at spawn. A value on a
	// non-Copilot profile is a 400 (buildProfileFromJSON gates it on the
	// profile's harness).
	//
	// NOTE (CLI): like remote_control and auto_memory, `tclaude agent spawn
	// --profile` does NOT fold this in client-side (mergeProfileIntoSpawn).
	// The daemon resolves it down the full tier stack, so leaving it out there
	// is what lets the group/global tiers speak too.
	CopilotAPI     *bool `json:"copilot_api,omitempty"`
	CodexAppServer *bool `json:"codex_app_server,omitempty"`
	// FastMode is Codex's nullable service-tier choice: nil inherits the
	// global Codex config, true forces fast, false forces standard.
	FastMode   *bool `json:"fast_mode,omitempty"`
	AutoReview *bool `json:"auto_review,omitempty"`
	TrustDir   *bool `json:"trust_dir,omitempty"`
	// AutoMemory is the "keep Claude Code's auto memory on" default
	// (tri-state). Unset resolves to OFF at spawn, so agents sharing a repo
	// don't cross-pollute one project memory store. Claude-Code-only.
	AutoMemory *bool `json:"auto_memory,omitempty"`
	// PeerMessaging is the "keep Claude Code's own cross-session messaging
	// on" default (tri-state), unset resolving to OFF at spawn.
	PeerMessaging *bool `json:"peer_messaging,omitempty"`
	// SSHWorkaround is the Git-over-SSH compatibility default. nil leaves the
	// launch default in place; false explicitly opts out. The launch applies
	// it only to an ownership-isolating sandbox shape that needs it.
	SSHWorkaround *bool `json:"ssh_workaround,omitempty"`
	// RemoteControl is the profile's "start with Claude Code Remote Access on"
	// default — tri-state (null = unset, false = off, true = on). A group's
	// remote-control policy overrides it at spawn (JOH-262).
	//
	// NOTE (CLI): `tclaude agent spawn --profile` does not fold this in
	// client-side: the CLI can't see the group's remote-control policy, which
	// must win. The daemon resolves it down the full tier stack under that
	// policy. Use the explicit --remote-control flag to force it on.
	RemoteControl *bool `json:"remote_control,omitempty"`

	// Identity / enrollment fields (dialog-side).
	AgentName string   `json:"agent_name,omitempty"`
	Role      string   `json:"role,omitempty"`
	RoleRef   string   `json:"role_ref,omitempty"`
	RoleRefs  []string `json:"role_refs,omitempty"`
	// PromptRefs names saved prompts, in order, whose bodies compose the
	// spawned agent's initial message ahead of any typed brief.
	PromptRefs     []string `json:"prompt_refs,omitempty"`
	Descr          string   `json:"descr,omitempty"`
	InitialMessage string   `json:"initial_message,omitempty"`
	// StartupContext is profile-owned guidance injected into every resolved
	// spawn's briefing. It is deliberately separate from InitialMessage, which
	// is only a replaceable task default in the spawn dialog.
	StartupContext string                           `json:"startup_context,omitempty"`
	Environment    []sandboxpolicy.EnvironmentEntry `json:"environment,omitempty"`

	// Dialog toggles.
	SyncWorktree               *bool `json:"sync_worktree,omitempty"`
	FetchLatestWorktree        *bool `json:"fetch_latest_worktree,omitempty"`
	AutoFocus                  *bool `json:"auto_focus,omitempty"`
	IncludeGroupDefaultContext *bool `json:"include_group_default_context,omitempty"`

	// Birth-time access controls the profile pre-fills. IsOwner is
	// tri-state (null = unset). PermissionOverrides maps slug → "grant" | "deny"
	// (absent = no overrides); validated against the slug registry at save.
	IsOwner             *bool                            `json:"is_owner,omitempty"`
	PermissionOverrides map[string]db.PermissionOverride `json:"permission_overrides,omitempty"`

	// ContextFeatures is the profile's per-agent startup-context trim map (slug →
	// "on" | "off"; absent = trims nothing), validated against the catalog AND
	// the profile's own harness at save. Claude-Code-only, so a non-empty map on
	// a Codex profile is a 400. See harness/context_features.go and TCL-597.
	ContextFeatures map[string]string `json:"context_features,omitempty"`

	// RelaunchPolicy is the profile's per-agent relaunch-policy rows (field →
	// mode; absent = the global table), copied into each agent it spawns.
	// Validated against the launch registry at save. Saving a row that keeps
	// old settings (reuse, explicit-wins) needs relaunch-policy.manage for an
	// agent caller.
	RelaunchPolicy map[string]string `json:"relaunch_policy,omitempty"`

	CreatedAt string `json:"created_at,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
}
