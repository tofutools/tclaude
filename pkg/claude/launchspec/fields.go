// Package launchspec is the registry of every input that shapes an agent's
// launch, and the relaunch policy that says which of them are read again when
// an agent is resumed, reincarnated, cloned or morphed.
//
// It is data, not resolution: the resolvers live in pkg/claude/agentd. The
// registry exists so one place states, per field, which layer owns the value
// and why, and so docs/launch-resolution.md cannot drift from it (a test
// fails when a spawn-profile, group or spawn-request field has no row, or a
// row in the doc disagrees with the registry).
package launchspec

// Layer is where a launch field's value lives once the agent exists, which
// decides what a relaunch does with it.
type Layer string

const (
	// LayerFixed values are chosen at first spawn and reused on every
	// relaunch. Only an explicit operator action changes them (a morph, an
	// explicit resume flag, a move).
	LayerFixed Layer = "fixed"
	// LayerSpawnCopy values are the spawn profile's own contribution. The
	// profile is shallow-copied at first spawn: later edits to the profile do
	// not reach running agents. An explicit flag, at spawn or at resume,
	// overrides the copied value and stays.
	LayerSpawnCopy Layer = "spawn copy"
	// LayerSpawnOnly values are consumed by the first spawn (the opening
	// brief, trust seeding, access grants) and are not part of a relaunch.
	LayerSpawnOnly Layer = "spawn only"
	// LayerGroup values are group or workspace settings. They are read again
	// on relaunch, unless the relaunch policy says to reuse the stored value.
	LayerGroup Layer = "group setting"
	// LayerReferenced values are the contents of a profile the agent refers
	// to by identity. The reference is kept; the contents are read again.
	LayerReferenced Layer = "referenced profile"
	// LayerLive values are read while the agent runs. They are not a launch
	// input, so a relaunch has nothing to decide.
	LayerLive Layer = "live"
)

// Relaunch is the short user-facing statement of what a relaunch does with a
// field on this layer, as docs/launch-resolution.md prints it.
func (l Layer) Relaunch() string {
	switch l {
	case LayerFixed:
		return "kept"
	case LayerSpawnCopy:
		return "kept (copy)"
	case LayerSpawnOnly:
		return "not reapplied"
	case LayerGroup:
		return "re-resolved"
	case LayerReferenced:
		return "re-read"
	case LayerLive:
		return "live"
	}
	return ""
}

// Field is one row of the launch registry.
type Field struct {
	// Key is the stable identifier, used by the relaunch policy table and as
	// the row key in docs/launch-resolution.md.
	Key   string
	Label string
	Layer Layer
	// Configurable marks the rows the relaunch policy can set (operator
	// decisions Q9 and Q11): group env, group remote-control policy,
	// referenced sandbox profile contents, and the harness-config posture.
	Configurable bool
	// Modes are the relaunch modes a configurable row offers, the default
	// first. Empty means re-resolve (default) or reuse.
	Modes []RelaunchMode
	// ProfileFields, GroupFields and RequestFields name the Go struct fields
	// of db.SpawnProfile, db.AgentGroup and agent.SpawnRequest that feed this
	// row. A non-empty RequestFields means the value can be set explicitly
	// on a spawn.
	ProfileFields []string
	GroupFields   []string
	RequestFields []string
	// Why is the reason for the row's layer, as the doc and the Config tab
	// show it.
	Why string
	// Gap, when set, says where today's code still differs from the row.
	// The doc prints it, so the doc never claims behaviour that has not
	// shipped; the slice that closes the gap clears it.
	Gap string
}

// Explicit reports whether a spawn request can set the field directly.
func (f Field) Explicit() bool { return len(f.RequestFields) > 0 }

// Fields is the registry, in the order docs/launch-resolution.md lists it.
var Fields = []Field{
	// --- fixed at first spawn ---
	{Key: "harness", Label: "Harness", Layer: LayerFixed,
		ProfileFields: []string{"Harness"}, RequestFields: []string{"Harness"},
		Why: "The conversation belongs to one harness; switching it is a morph."},
	{Key: "model", Label: "Model", Layer: LayerFixed,
		ProfileFields: []string{"Model"}, RequestFields: []string{"Model"},
		Why: "Kept as launched; a morph or an explicit resume flag changes it.",
		Gap: "No resume path takes a model flag yet; only a morph changes it."},
	{Key: "effort", Label: "Reasoning effort", Layer: LayerFixed,
		ProfileFields: []string{"Effort"}, RequestFields: []string{"Effort"},
		Why: "Kept as launched; a morph or an explicit resume flag changes it.",
		Gap: "No resume path takes an effort flag yet; only a morph changes it."},
	{Key: "directory", Label: "Directory and worktree", Layer: LayerFixed,
		GroupFields:   []string{"DefaultCwd"},
		RequestFields: []string{"Cwd", "CallerCwd", "WorktreePath", "WorktreeBranch"},
		Why:           "The harness keys the conversation to its directory; only an explicit move changes it."},

	// --- the spawn profile's own values, copied at first spawn ---
	{Key: "spawn_profile", Label: "Spawn profiles used (named, group default, global default)", Layer: LayerSpawnCopy,
		GroupFields: []string{"DefaultProfile"}, RequestFields: []string{"Profile"},
		Why: "A spawn profile is copied at spawn; editing it, or changing a group's default, affects new spawns only."},
	{Key: "relaunch_overrides", Label: "Per-agent relaunch policy", Layer: LayerSpawnCopy,
		ProfileFields: []string{"RelaunchPolicy"}, RequestFields: []string{"RelaunchPolicy"},
		Why: "The configurable rows a spawn or its spawn profile set for this agent are copied at spawn and override the global table for it; " +
			"a row neither sets follows the global table, edits to it included."},
	{Key: "model_proxy", Label: "Model proxy", Layer: LayerSpawnCopy,
		ProfileFields: []string{"ModelProxy"}, RequestFields: []string{"ModelProxy"},
		Why: "Part of the spawn profile copy.",
		Gap: "Reincarnate and clone drop the model proxy. A re-applied spawn copy does not change it yet; the agent keeps the value from its first spawn."},
	{Key: "sandbox_mode", Label: "Sandbox mode", Layer: LayerSpawnCopy,
		ProfileFields: []string{"Sandbox"}, RequestFields: []string{"HarnessBuiltinMode"},
		Why: "Part of the spawn profile copy."},
	{Key: "sandbox_implementation", Label: "Sandbox implementation", Layer: LayerSpawnCopy,
		ProfileFields: []string{"SandboxImplementation"}, RequestFields: []string{"SandboxImplementation"},
		Why: "Part of the spawn profile copy; the host must still support it."},
	{Key: "approval", Label: "Approval policy", Layer: LayerSpawnCopy,
		ProfileFields: []string{"Approval"}, RequestFields: []string{"ApprovalPolicy"},
		Why: "Part of the spawn profile copy."},
	{Key: "tools", Label: "Tool governance", Layer: LayerSpawnCopy,
		ProfileFields: []string{"ToolGovernance"}, RequestFields: []string{"ToolGovernance"},
		Why: "Part of the spawn profile copy."},
	{Key: "ask_timeout", Label: "Ask-user timeout", Layer: LayerSpawnCopy,
		ProfileFields: []string{"AskUserQuestionTimeout"}, RequestFields: []string{"AskUserQuestionTimeout"},
		Why: "Part of the spawn profile copy."},
	{Key: "context_window", Label: "Auto-compact window and context window cap", Layer: LayerSpawnCopy,
		ProfileFields: []string{"AutoCompactWindow", "ContextWindowMax"},
		RequestFields: []string{"AutoCompactWindow", "ContextWindowMax"},
		Why:           "Part of the spawn profile copy."},
	{Key: "context_features", Label: "Context features (trims)", Layer: LayerSpawnCopy,
		ProfileFields: []string{"ContextFeatures"}, RequestFields: []string{"ContextFeatures"},
		Why: "Part of the spawn profile copy; features the harness lacks are dropped."},
	{Key: "harness_drives", Label: "Harness drives (Copilot API, Codex app-server, pi drive)", Layer: LayerSpawnCopy,
		ProfileFields: []string{"CopilotAPI", "CodexAppServer"},
		RequestFields: []string{"CopilotAPI", "CodexAppServer", "PiDrive"},
		Why:           "Part of the spawn profile copy; only harnesses with that drive accept it."},
	{Key: "fast_mode", Label: "Fast mode", Layer: LayerSpawnCopy,
		ProfileFields: []string{"FastMode"}, RequestFields: []string{"FastMode"},
		Why: "Part of the spawn profile copy."},
	{Key: "auto_review", Label: "Auto-review", Layer: LayerSpawnCopy,
		ProfileFields: []string{"AutoReview"}, RequestFields: []string{"AutoReview"},
		Why: "Part of the spawn profile copy."},
	{Key: "remote_control", Label: "Remote control (profile default)", Layer: LayerSpawnCopy,
		ProfileFields: []string{"RemoteControl"}, RequestFields: []string{"RemoteControl"},
		Why: "Part of the spawn profile copy; the group policy can still force it."},
	{Key: "auto_memory", Label: "Auto memory", Layer: LayerSpawnCopy,
		ProfileFields: []string{"AutoMemory"}, RequestFields: []string{"AutoMemory"},
		Why: "Part of the spawn profile copy."},
	{Key: "peer_messaging", Label: "Peer messaging", Layer: LayerSpawnCopy,
		ProfileFields: []string{"PeerMessaging"}, RequestFields: []string{"PeerMessaging"},
		Why: "Part of the spawn profile copy."},
	{Key: "include_group_context", Label: "Include group startup context", Layer: LayerSpawnCopy,
		ProfileFields: []string{"IncludeGroupDefaultContext"}, RequestFields: []string{"IncludeGroupContext"},
		Why: "Part of the spawn profile copy; the group context itself is a group setting."},
	{Key: "ssh_workaround", Label: "SSH workaround", Layer: LayerSpawnCopy,
		ProfileFields: []string{"SSHWorkaround"}, RequestFields: []string{"SSHWorkaround"},
		Why: "Part of the spawn profile copy."},
	{Key: "profile_env", Label: "Profile and explicit environment", Layer: LayerSpawnCopy,
		ProfileFields: []string{"Environment"}, RequestFields: []string{"Environment"},
		Why: "Part of the spawn profile copy; explicit entries stay explicit."},
	{Key: "profile_startup_context", Label: "Profile startup context", Layer: LayerSpawnCopy,
		ProfileFields: []string{"StartupContext"},
		Why:           "Part of the spawn profile copy; re-injection after compaction uses it."},
	{Key: "roles", Label: "Roles", Layer: LayerSpawnCopy,
		ProfileFields: []string{"Role", "RoleRef", "RoleRefs"}, RequestFields: []string{"Role", "RoleRef", "RoleRefs"},
		Why: "Which roles an agent has is part of the copy; their text is a referenced definition.",
	},
	{Key: "explicit_sandbox_profile", Label: "Sandbox profile named at spawn", Layer: LayerSpawnCopy,
		RequestFields: []string{"SandboxProfile", "OmitSandboxProfiles"},
		Why:           "An explicit choice stays; the named profile's contents are re-read."},
	{Key: "usage_profile_links", Label: "Usage profiles named by the spawn or linked from its spawn profile", Layer: LayerSpawnCopy,
		RequestFields: []string{"UsageProfiles"},
		Why:           "Which profiles is part of the copy; their rules are read live."},

	// --- consumed by the first spawn only ---
	{Key: "identity", Label: "Agent name and description", Layer: LayerSpawnOnly,
		ProfileFields: []string{"AgentName", "Descr"}, RequestFields: []string{"Name", "Descr"},
		Why: "Set at spawn; a rename acts on the agent itself."},
	{Key: "initial_brief", Label: "Initial message, saved prompts, attachments", Layer: LayerSpawnOnly,
		ProfileFields: []string{"InitialMessage", "PromptRefs"}, GroupFields: []string{"DefaultPrompts"},
		RequestFields: []string{"InitialMessage", "PromptRefs", "ReplyTo", "Attachments"},
		Why:           "Delivered once, as the opening brief."},
	{Key: "task_link", Label: "Task link", Layer: LayerSpawnOnly,
		RequestFields: []string{"TaskURL", "TaskLabel"},
		Why:           "Set at spawn; later changes act on the agent itself."},
	{Key: "trust_dir", Label: "Directory trust seeding", Layer: LayerSpawnOnly,
		ProfileFields: []string{"TrustDir"}, RequestFields: []string{"TrustDir", "WriteProofToken"},
		Why: "Seeded once for the launch directory, which does not move."},
	{Key: "worktree_setup", Label: "Worktree sync and fetch", Layer: LayerSpawnOnly,
		ProfileFields: []string{"SyncWorktree", "FetchLatestWorktree"},
		Why:           "Prepares the worktree before the first launch."},
	{Key: "focus", Label: "Auto-focus", Layer: LayerSpawnOnly,
		ProfileFields: []string{"AutoFocus"}, RequestFields: []string{"AutoFocus", "AutoFocusWeb"},
		Why: "Where the new agent is shown when it starts."},
	{Key: "access", Label: "Group owner and permission overrides", Layer: LayerSpawnOnly,
		ProfileFields: []string{"IsOwner", "PermissionOverrides"},
		RequestFields: []string{"IsOwner", "PermissionOverrides"},
		Why:           "Granted to the agent at spawn; after that they are the agent's own and edited directly. A profile re-apply changes only the grants the profile gave that nobody has changed since; group ownership is not re-applied."},
	{Key: "run_mode", Label: "One-shot run, timeouts and queueing", Layer: LayerSpawnOnly,
		RequestFields: []string{"NonInteractive", "RunTimeoutSeconds", "NoQueue", "TimeoutSeconds"},
		Why:           "Shape of the spawn call itself."},
	{Key: "launch_acknowledgements", Label: "Unenforced-sandbox and break-glass acknowledgements", Layer: LayerSpawnOnly,
		RequestFields: []string{"AllowUnenforcedSandbox", "BreakGlassAcknowledged"},
		Why:           "A human's consent for one fresh spawn; never replayed."},

	// --- group and workspace settings, re-resolved on relaunch ---
	{Key: "group_env", Label: "Group environment", Layer: LayerGroup, Configurable: true,
		GroupFields: []string{"Environment"},
		Why:         "Group settings are kept current, for agents spawned before this table too (operator decision).",
		Gap:         "With agentd down, watch mode and conv resume reuse the stored group environment, and say so. A clone that joins another group takes that group's environment, unless it keeps its source's."},
	{Key: "group_remote_control", Label: "Group remote-control policy", Layer: LayerGroup, Configurable: true,
		GroupFields: []string{"RemoteControl"},
		Why: "Group settings are kept current: a policy the group sets applies on relaunch, unless the spawn set remote control explicitly " +
			"(a flag, or a dashboard checkbox the operator changed); with no group policy the agent keeps its last state, " +
			"live toggles included. Still clamped to what the harness supports.",
		Gap: "Watch mode and conv resume keep the remote-control state stored at the last launch while agentd is not running, and so does a clone that joins another group for its sandbox."},
	{Key: "group_startup_context", Label: "Group startup context and mission", Layer: LayerLive,
		GroupFields: []string{"DefaultContext", "Mission", "MissionHeader"},
		Why: "Delivered in the opening brief; a resume keeps the conversation that holds it, so there is nothing to relaunch. " +
			"Re-injection after compaction reads the group's current text."},
	{Key: "group_sandbox_assignment", Label: "Group and workspace sandbox-profile assignment", Layer: LayerGroup,
		GroupFields: []string{"SandboxProfile", "SandboxProfileID"},
		Why:         "Which profile a group or the workspace assigns is a setting, kept current.",
		Gap:         "With agentd down, watch mode and conv resume reuse the stored sandbox policy, and say so."},

	// --- contents of referenced profiles ---
	{Key: "sandbox_profile_contents", Label: "Sandbox profile contents", Layer: LayerReferenced, Configurable: true,
		Why: "Edits to a sandbox profile take effect on the next relaunch (operator decision).",
		Gap: "With agentd down, watch mode and conv resume reuse the stored contents, and say so. A Codex clone with a generated profile always re-reads them, and a clone that joins another group for its sandbox takes that group's current profiles."},
	{Key: "role_contents", Label: "Role definitions (text)", Layer: LayerSpawnCopy,
		Why: "The role text is delivered in the opening brief, and re-injection after compaction repeats the text resolved at spawn, " +
			"so an edit to a role reaches new spawns only."},
	{Key: "harness_config", Label: "Harness config posture", Layer: LayerReferenced, Configurable: true,
		Modes:         []RelaunchMode{ModeExplicitWins, ModeTightenOnly, ModeChainWins},
		RequestFields: []string{"HarnessConfig"},
		Why: "Default chosen for least surprise, not maximum security: an explicit posture stays like any explicit flag. " +
			"Tighten-only, where a narrower sandbox-profile posture always applies, is the security option.",
		Gap: "With agentd down, watch mode and conv resume keep the stored posture, as does a resume that falls back to the stored policy when the profiles cannot be read at all; the setting applies on the agent's next ordinary resume."},
	{Key: "usage_assignment", Label: "Group and workspace usage-profile assignment", Layer: LayerLive,
		Why: "Usage rules are evaluated live against the current assignments."},
	{Key: "usage_profile_contents", Label: "Usage profile rules", Layer: LayerReferenced,
		Why: "Usage rules are evaluated live, every few minutes."},

	// --- live, not launch inputs ---
	{Key: "group_access", Label: "Group permissions and owners", Layer: LayerLive,
		GroupFields: []string{"OwnerScopesJSON"},
		Why:         "Applied to members at once; no relaunch needed."},
	{Key: "group_runtime", Label: "Member cap, notifications, re-injection after compaction", Layer: LayerLive,
		GroupFields: []string{"MaxMembers", "NotifyEnabled", "ReinjectAfterCompact"},
		Why:         "Read when used; not part of a launch."},
}

// NotLaunchInputs lists the struct fields of db.SpawnProfile, db.AgentGroup
// and agent.SpawnRequest that are deliberately outside the registry, with the
// reason. The registry test fails on a struct field that is in neither.
var NotLaunchInputs = map[string]string{
	"SpawnProfile.ID":             "row identity",
	"SpawnProfile.Name":           "profile handle",
	"SpawnProfile.Aliases":        "profile handle",
	"SpawnProfile.Disabled":       "whether the profile may be used at all",
	"SpawnProfile.DisabledReason": "whether the profile may be used at all",
	"SpawnProfile.OperatorOnly":   "who may use the profile",
	"SpawnProfile.CreatedAt":      "bookkeeping",
	"SpawnProfile.UpdatedAt":      "bookkeeping",

	"AgentGroup.ID":                "row identity",
	"AgentGroup.Name":              "group handle",
	"AgentGroup.Descr":             "the group's own description",
	"AgentGroup.DefaultSpawnGroup": "picks a group for directory auto-join, before any launch",
	"AgentGroup.AttachmentURL":     "dashboard link",
	"AgentGroup.AttachmentLabel":   "dashboard link",
	"AgentGroup.SourceTemplate":    "deploy bookkeeping",
	"AgentGroup.SourceTemplateID":  "deploy bookkeeping",
	"AgentGroup.CreatedAt":         "bookkeeping",
	"AgentGroup.ArchivedAt":        "bookkeeping",
	"AgentGroup.ParentGroupID":     "group tree",
	"AgentGroup.RouteGeneration":   "routing bookkeeping",
}

// Lookup returns the registry row for key.
func Lookup(key string) (Field, bool) {
	for _, f := range Fields {
		if f.Key == key {
			return f, true
		}
	}
	return Field{}, false
}
