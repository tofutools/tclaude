package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/common"
)

// SpawnResponse is the daemon's response shape for
// POST /v1/groups/{name}/spawn — used by both `tclaude agent spawn`
// and `tclaude --join-group`. Mirrors the keys handleGroupSpawn writes
// in pkg/claude/agentd/lifecycle.go.
type SpawnResponse struct {
	Group string `json:"group"`
	// AgentID is the spawned agent's stable actor key — the canonical ID
	// the CLI leads with; ConvID is the live generation behind it (kept as
	// the snapshot/hover). AgentID remains present when an asynchronous Codex
	// spawn is pending and only ConvID has not materialised yet.
	AgentID     string `json:"agent_id,omitempty"`
	ConvID      string `json:"conv_id"`
	Label       string `json:"label"`
	TmuxSession string `json:"tmux_session"`
	AttachCmd   string `json:"attach_cmd"`

	// Resolved echoes the launch shape after the daemon's default-resolution
	// chain, with per-field provenance so the caller can SEE where each value
	// came from — the mistake-preventer for TCL-304, where a blank spawn
	// silently inherited a default profile's harness/model (flipping vendor).
	// Additive on the wire: an older client that ignores it keeps working. nil
	// only for a response from a daemon that predates this field.
	Resolved *ResolvedLaunch `json:"resolved,omitempty"`

	// TaskRefURL / TaskRefState echo a --task spawn's requested link and its
	// verified binding state: "bound" once the daemon read the link back off
	// the enrolled actor, "pending" while a delayed (pending) spawn still owes
	// the binding to its back-fill enrollment (TCL-568). Both empty when the
	// spawn carried no task link, or on a response from an older daemon.
	TaskRefURL   string `json:"task_ref_url,omitempty"`
	TaskRefState string `json:"task_ref_state,omitempty"`
}

// ResolvedLaunch is the resolved launch shape echoed in a spawn response — the
// harness/model/effort that actually took effect, each tagged with the tier of
// the resolution chain it came from. See ResolvedField.Source for the tier
// vocabulary. Started as the three vendor-carrying fields the TCL-304 incident
// turned on, plus the sandbox implementation (TCL-769); add more here (and in
// resolveLaunchProvenance) if a future silently-defaulted field needs
// surfacing.
type ResolvedLaunch struct {
	Harness ResolvedField `json:"harness"`
	Model   ResolvedField `json:"model"`
	Effort  ResolvedField `json:"effort"`
	// SandboxImpl names WHO OWNS OS-level containment for this launch and which
	// tier chose it. It is echoed rather than left to Notes because a spawn that
	// silently inherited the experimental tclaude-layer from a group or global
	// default profile is exactly the surprise the echo exists to prevent — an
	// operator must be able to see that the wall around their agent was picked
	// by a profile they did not name. A blank Value is the legacy
	// harness-builtin path, echoed as "(harness default)" like any unpinned
	// field. See TCL-769.
	SandboxImpl ResolvedField `json:"sandbox_implementation"`
	// SandboxPolicy exposes the frozen profile chain, canonical grants, and
	// environment names. Environment values remain private in the snapshot.
	SandboxPolicy *ResolvedSandboxPolicy `json:"sandbox_policy,omitempty"`
	// Notes disclose ignored profile fields outside the three echoed values
	// above (for example a foreign sandbox or auto-review setting).
	Notes []string `json:"notes,omitempty"`
	// Info discloses useful details about a safe launch boundary that do not
	// require operator action. Keep these separate so clients do not style or
	// announce them as warnings.
	Info []string `json:"info,omitempty"`
	// Warnings disclose a resolved launch that is permitted but risky — today,
	// an approval posture that runs commands unattended while no OS sandbox is
	// provably active (TCL-586). They are separate from Notes because they say
	// something about the agent's blast radius rather than about which tier won
	// a field, and the CLI labels them accordingly.
	Warnings []string `json:"warnings,omitempty"`
}

type ResolvedSandboxPolicy struct {
	Version     int                             `json:"version"`
	Applied     []sandboxpolicy.AppliedProfile  `json:"applied"`
	Filesystem  []sandboxpolicy.FilesystemGrant `json:"filesystem"`
	Environment []string                        `json:"environment"`
}

func SummarizeSandboxPolicy(snapshot sandboxpolicy.Snapshot) *ResolvedSandboxPolicy {
	environment := make([]string, 0, len(snapshot.Effective.Environment)+len(snapshot.Effective.AgentDirectories))
	for _, entry := range snapshot.Effective.Environment {
		environment = append(environment, entry.Name)
	}
	environment = append(environment, snapshot.Effective.AgentDirectories...)
	sort.Strings(environment)
	out := &ResolvedSandboxPolicy{
		Version:     snapshot.Version,
		Applied:     append([]sandboxpolicy.AppliedProfile(nil), snapshot.Applied...),
		Filesystem:  append([]sandboxpolicy.FilesystemGrant(nil), snapshot.Effective.Filesystem...),
		Environment: environment,
	}
	return out
}

// ResolvedField pairs a resolved launch value with its provenance. Value is the
// value that took effect ("" when nothing pinned it and the harness uses its
// own default). Source names the resolution tier that supplied Value, one of:
//
//	explicit                            — a flag / request field set it
//	profile "<name>"                    — a CLI --profile filled it (CLI-side only)
//	group default profile "<name>"      — the group's default spawn profile
//	global default profile "<name>"     — the dashboard/global default profile
//	harness default                     — nothing pinned it; the harness decides
//	default profile (applied at launch) — a post-snapshot safety-net fill
//
// The daemon fills every Source tier after resolving the complete launch shape.
type ResolvedField struct {
	Value  string `json:"value"`
	Source string `json:"source"`
	// Note discloses an ambient profile value that was skipped because it was
	// incompatible with the resolved harness. The value still falls through to
	// the next profile tier / harness default; the skip is never silent.
	Note string `json:"note,omitempty"`
}

// Provenance source labels for ResolvedField.Source. The profile-name tiers are
// formatted with the profile name appended (see provGroupProfile etc.).
const (
	ProvExplicit       = "explicit"
	ProvHarnessDefault = "harness default"
	ProvLaunchDefault  = "default profile (applied at launch)"
)

// ProvGroupProfileSource / ProvGlobalProfileSource / ProvCLIProfileSource format
// the three name-bearing provenance tiers. Exported so the daemon (which fills
// the group/global tiers) and the CLI relabel path (which fills the --profile
// tier) emit byte-identical Source strings.
func ProvGroupProfileSource(name string) string  { return `group default profile ` + quoteName(name) }
func ProvGlobalProfileSource(name string) string { return `global default profile ` + quoteName(name) }
func ProvCLIProfileSource(name string) string    { return `profile ` + quoteName(name) }

// quoteName wraps a profile name in double quotes for a provenance label,
// matching the spec's `global default profile "gpt5.6-sol-high"` shape.
func quoteName(name string) string { return `"` + name + `"` }

// SpawnRequest is the JSON body of POST /v1/groups/{name}/spawn — the
// single shared request shape behind every spawn surface. The
// `tclaude agent spawn` CLI, `tclaude --join-group`, and the agentd
// dashboard's spawn modal all marshal one of these; agentd's
// handleGroupSpawn decodes it. One type means the CLI and the
// dashboard cannot drift in which fields the daemon understands.
type SpawnRequest struct {
	// AllowUnenforcedSandbox is the dashboard-only, fresh-spawn authorization
	// to proceed when the selected implementation cannot enforce closed network
	// access. It is accepted only from the cookie-authenticated dashboard route,
	// is never profile/config-backed, and has no CLI flag. False or absent keeps
	// the ordinary fail-closed behavior.
	AllowUnenforcedSandbox bool `json:"allow_unenforced_sandbox,omitempty"`
	// SandboxProfile is an optional filesystem/environment profile.
	// Only a daemon-boundary-classified human may set it; agent callers may
	// inherit the group's/global policy but cannot select an escalation.
	SandboxProfile string `json:"sandbox_profile,omitempty"`
	// OmitSandboxProfiles explicitly suppresses every tclaude sandbox-profile
	// tier for this launch: global, group, and explicit. It is distinct from a
	// blank SandboxProfile, which still inherits the ambient tiers, and is
	// human-only because dropping inherited denies can widen authority.
	OmitSandboxProfiles bool `json:"omit_sandbox_profiles,omitempty"`
	// Profile names the CLI's explicit --profile. Launch fields remain separate
	// on the wire so the daemon can distinguish direct flags (loud on
	// incompatibility) from ambient profile values (skip + disclose when a
	// higher tier selected a foreign harness).
	Profile string `json:"profile,omitempty"`
	// SSHWorkaround controls tclaude's Codex-only Git-over-SSH compatibility
	// config. nil inherits the selected profile and then defaults on for managed
	// Codex agents; false is an explicit opt-out.
	SSHWorkaround *bool `json:"ssh_workaround,omitempty"`
	// BreakGlassAcknowledged is a tombstone. TCL-791 removed break-glass; the
	// field survives ONLY so a spawn payload still carrying it is refused
	// loudly instead of silently ignored as an unknown JSON key. It is never
	// read for any purpose other than that refusal, and never persisted.
	BreakGlassAcknowledged *bool `json:"break_glass_acknowledged,omitempty"`
	// Name, when set, becomes the new agent's conversation title:
	// runSpawnPostInit injects `/rename <name>` into the fresh pane. An
	// agent has exactly one name — its title — so there is no separate
	// per-group handle.
	Name string `json:"name,omitempty"`
	Role string `json:"role,omitempty"`
	// Descr is the short, one-line description shown on the dashboard
	// (the group-member "Description" column). Keep it terse — the
	// agent's actual task brief goes in InitialMessage instead.
	Descr string `json:"descr,omitempty"`
	// TaskURL is an optional per-agent task-reference link (http(s)) —
	// the work item the agent is on (a Linear issue, GitHub issue/PR,
	// ticket, …), rendered as a clickable label in the dashboard's Task
	// column. TaskLabel overrides the auto-derived display label.
	// Stored on the agent (not the membership), so a lead spawning
	// workers can point each at its Linear issue up front.
	TaskURL   string `json:"task_ref_url,omitempty"`
	TaskLabel string `json:"task_ref_label,omitempty"`
	// InitialMessage, when set, is delivered to the new agent as its
	// first task brief — placed in its inbox as an agent_messages row,
	// not typed into its pane, so newlines survive verbatim. Split from
	// Descr so a long brief doesn't bloat the dashboard's description
	// column.
	InitialMessage string `json:"initial_message,omitempty"`
	// Cwd is the working directory for the new CC session. Empty falls
	// back to the group's default_cwd, then the daemon's own cwd.
	Cwd string `json:"cwd,omitempty"`
	// TimeoutSeconds bounds how long the daemon waits for the new
	// conv-id to materialise. <= 0 falls back to 30s; capped at 300s.
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`

	// Effort sets the spawned agent's Claude reasoning effort. It is
	// forwarded to the new agent's `tclaude session new --effort <level>`
	// (and on to `claude`). Empty omits the flag so claude uses its own
	// default; a non-empty value must be accepted by the selected harness's
	// ModelCatalog (Copilot has two additional documented levels).
	// Every spawn surface — `tclaude agent spawn`, `tclaude --join-group`,
	// and the dashboard's spawn modal — sets this same field, so the wire
	// contract stays single-sourced.
	Effort string `json:"effort,omitempty"`

	// Model picks the Claude model the spawned agent runs on. It is
	// forwarded to the new agent's `tclaude session new --model <alias>`
	// (and on to `claude`). Empty omits the flag so claude uses its own
	// default; a non-empty value must be one of clcommon.ValidModels.
	// Same single-sourced wire contract as Effort above.
	Model string `json:"model,omitempty"`

	// Harness picks which coding harness the spawned agent runs — ""
	// (or "claude") = Claude Code, the default; "codex" = OpenAI Codex CLI
	// (JOH-160). It forwards to `tclaude session new --harness <h>`. The
	// daemon validates it against the harness registry and validates the
	// Effort/Model fields above through the chosen harness's own catalog
	// (so a Codex spawn is checked against Codex's rules, not Claude's).
	Harness string `json:"harness,omitempty"`

	// HarnessBuiltinMode picks the launch containment for a harness that takes one
	// (Codex). Empty resolves to the harness's secure default — for Codex the
	// managed tclaude-agent profile (workspace-write containment PLUS the
	// agentd-socket allowlist, so the agent can still run `tclaude agent …`),
	// forwarded as `--permission-profile tclaude-agent`. The raw Codex modes
	// (workspace-write|read-only|danger-full-access) are forwarded as `tclaude
	// session new --sandbox <mode>` and do NOT get the socket allowlist;
	// danger-full-access opts out of the sandbox entirely. Not applicable to
	// Claude Code (settings.json-driven), which rejects a non-empty value. See
	// JOH-192 / JOH-207.
	HarnessBuiltinMode string `json:"sandbox,omitempty"`

	// SandboxImplementation picks WHO OWNS OS-level containment for the new
	// agent, independently of the per-harness HarnessBuiltinMode above: "harness-builtin"
	// (the current behavior — the harness's own sandbox) or the EXPERIMENTAL
	// "tclaude-layer", which runs the whole harness process inside a
	// tclaude-owned bubblewrap mount namespace and disables the harness's own
	// sandbox inside it.
	//
	// Empty = unspecified, so the daemon's profile tier stack fills it; unset
	// everywhere means harness-builtin, which is what every spawn did before this
	// field existed. An explicit "harness-builtin" is not the same as empty: it
	// PINS the legacy implementation so a lower-tier profile cannot flip it.
	//
	// tclaude-layer is Linux-only and needs `bwrap` plus unprivileged user
	// namespaces. A host that cannot provide them REFUSES the spawn naming the
	// missing capability; it never falls back. Likewise a harness whose
	// tool-executing process lives outside the wrapped pane (OpenCode) is a 400.
	// Forwarded to `tclaude session new --sandbox-impl`. See TCL-769.
	SandboxImplementation string `json:"sandbox_implementation,omitempty"`

	// AskUserQuestionTimeout is the Claude Code AskUserQuestion idle-timeout
	// override for the new agent (never|60s|5m|10m). Empty = inherit (no
	// override — the agent uses the operator's own settings.json value). The
	// explicit values make an UNATTENDED agent auto-continue a question with its
	// default answer after the idle interval instead of stalling. Delivered
	// per-spawn via `--settings` and forwarded to `tclaude session new
	// --ask-user-question-timeout <v>`. Claude-Code-only: a value for a harness
	// with no AskUserQuestion dialog (Codex) is a 400.
	AskUserQuestionTimeout string `json:"ask_user_question_timeout,omitempty"`

	// ApprovalPolicy picks the launch-time approval policy for a harness that
	// takes one (Codex's --ask-for-approval). Empty resolves to the harness's
	// non-escalating default (Codex: never), so a daemon-owned Codex agent —
	// detached, with no human at its TUI — never deadlocks on an approval
	// prompt no one can answer. Forwarded to `tclaude session new
	// --ask-for-approval <policy>`. Not applicable to Claude Code
	// (settings.json-driven), which rejects a non-empty value. See JOH-200.
	ApprovalPolicy string `json:"approval,omitempty"`

	// ToolGovernance controls OpenCode's bash/glob/grep/lsp/task/skill block
	// uniformly. Empty is resolved by agentd to the OpenCode default (allow).
	ToolGovernance string `json:"tools,omitempty"`

	// AutoReview opts the spawned agent into the harness's guardian subagent
	// (Codex's `-c approvals_reviewer=auto_review`), which auto-decides approval
	// prompts in the human's place — the orthogonal "who answers" axis to
	// ApprovalPolicy's "when to ask". false (the default) keeps the human as
	// reviewer. The daemon gates it on the chosen harness having an approvals
	// subsystem (Codex); requesting it for Claude Code is a 400. Forwarded to
	// `tclaude session new --auto-review`. Experimental/undocumented upstream,
	// hence an explicit opt-in. See JOH-200 part 2.
	AutoReview bool `json:"auto_review,omitempty"`

	// TrustDir opts the spawned agent into pre-trusting its launch directory:
	// the daemon seeds the harness's own trust record BEFORE launch (Codex's
	// [projects."<cwd>"] trust_level in ~/.codex/config.toml, Claude Code's
	// projects.<cwd>.hasTrustDialogAccepted in ~/.claude.json), so a detached
	// pane doesn't freeze on the "do you trust this folder?" dialog (JOH-205,
	// JOH-369). false (the default) leaves the dialog in place — to be cleared
	// by focusing the pending pane. Unlike HarnessBuiltinMode/ApprovalPolicy this
	// edits a config tclaude does not own, so it is strictly opt-in (dashboard
	// checkbox / `--trust-dir`) and NEVER defaulted. Requesting it for a
	// harness with no trust dialog is a 400. Forwarded to `tclaude session new
	// --trust-dir`.
	TrustDir bool `json:"trust_dir,omitempty"`

	// RemoteControl arms the spawned agent's built-in Remote Access at launch
	// (Claude Code's --remote-control), so it is reachable from the Claude app
	// from its first turn (JOH-258). It is tri-state (*bool): a non-nil value is
	// the AUTHORITATIVE per-spawn intent — it overrides the group's remote-control
	// policy AND the group default profile's remote-control default, so "whatever
	// the spawn form shows decides" (JOH-262 revised). nil = unspecified, so the
	// daemon's policy stack (group policy → profile default → off) fills it; this
	// is the CLI's opt-in-only path (the `--remote-control` flag sets &true, its
	// absence leaves nil). The dashboard spawn modal always sends the checkbox
	// state (true OR false) for a Remote-Access-capable harness. The daemon gates a
	// non-nil true on the chosen harness having Remote Access (Claude Code);
	// requesting it for Codex is a 400. Forwarded to `tclaude session new
	// --remote-control`, and the daemon tags sessions.remote_control=1 once the
	// row materialises.
	RemoteControl *bool `json:"remote_control,omitempty"`

	// AutoMemory keeps Claude Code's built-in auto memory ON for the spawned
	// agent. Tri-state (*bool): a non-nil value is the AUTHORITATIVE per-spawn
	// intent and overrides any profile default; nil = unspecified, so the
	// daemon's profile tier stack fills it, falling back to FALSE.
	//
	// The default is off — and off is the interesting direction here: Claude
	// Code's auto memory writes a per-project memory store that every tclaude
	// agent on that repo reads, so several agents working one codebase
	// cross-pollute each other's notes. tclaude therefore injects
	// CLAUDE_CODE_DISABLE_AUTO_MEMORY=1 unless something opts back in. Setting
	// it true has the launch inject "0" (Claude Code's documented force-enable)
	// so an operator's own exported "=1" can't override the agent's posture.
	//
	// CLAUDE.md / AGENTS.md are unaffected either way. The daemon gates a
	// non-nil true on the chosen harness having an auto-memory system (Claude
	// Code); requesting it for Codex is a 400. Forwarded to `tclaude session
	// new --auto-memory`.
	AutoMemory *bool `json:"auto_memory,omitempty"`

	// ContextFeatures trims Claude Code's startup context for the spawned agent —
	// bundled skills, tool schemas for capabilities it will never use,
	// system-prompt blocks — as a map of catalog slug → "on" | "off". See
	// harness/context_features.go for the catalog and TCL-597 for why.
	//
	// The point is focus, not thrift: a worker spawned for one job reads past
	// every irrelevant capability the harness advertises, and that competes with
	// its actual brief. Trimming raises the share of the window that is about the
	// task.
	//
	// A non-nil map (INCLUDING an explicitly empty one, which means "trim
	// nothing") is the AUTHORITATIVE per-spawn intent and replaces any profile
	// default outright — the tiers do not merge, so one profile always tells the
	// whole story. nil = unspecified, so the daemon's profile tier stack fills it.
	// The daemon gates a non-empty map on the chosen harness having steerable
	// startup context (Claude Code); requesting it for Codex is a 400. Forwarded
	// to `tclaude session new --context-features`.
	// It is a POINTER so the three states survive JSON: absent/null =
	// unspecified, {} = an explicit "trim nothing", non-empty = these trims. A
	// plain map with omitempty could not express the middle case.
	ContextFeatures *map[string]string `json:"context_features,omitempty"`

	// AutoCompactWindow pins the context capacity in tokens that Claude Code's
	// auto-compaction calculations run against (CLAUDE_CODE_AUTO_COMPACT_WINDOW),
	// as a token count — "450000", "450k" and "0.5M" all mean the same thing and
	// normalize to the first. Empty = unspecified, so the daemon's profile tier
	// stack fills it; unset everywhere leaves the model's own default threshold in
	// charge.
	//
	// The point is long-lived agents: on a 1M-context model Claude Code compacts
	// near the top of the window, well after answer quality has started to slide.
	// Pinning it lower makes compaction fire while the agent is still sharp.
	// Claude Code caps the value at the model's real context window, so it is a
	// ceiling and never a floor, and setting it decouples the compaction threshold
	// from the status line's percentage.
	//
	// Claude-Code-only: a value for a harness with no such knob (Codex, OpenCode)
	// is a 400. Forwarded to `tclaude session new --auto-compact-window`.
	AutoCompactWindow string `json:"auto_compact_window,omitempty"`

	// WorktreePath / WorktreeBranch describe a git worktree the agent
	// should do its code work in, when Cwd is a parent "monorepo"
	// directory rather than the repo itself. They are purely
	// informational — the agent still launches in Cwd; the worktree
	// path rides into the welcome message so the agent knows where to
	// make edits. Empty for an ordinary spawn where Cwd already is the
	// repo (or already is the worktree).
	WorktreePath   string `json:"worktree_path,omitempty"`
	WorktreeBranch string `json:"worktree_branch,omitempty"`

	// AutoFocus, when true, opens a terminal window attached to the new
	// agent once the spawn lands. Opt-in on the wire — the dashboard's
	// spawn modal defaults its checkbox on; CLI / agent callers pass it
	// explicitly.
	AutoFocus bool `json:"auto_focus,omitempty"`
	// AutoFocusWeb routes AutoFocus to the dashboard's in-browser terminal
	// instead of asking agentd to open a native OS window. The dashboard sets
	// this when dashboard.default_terminal is "web"; CLI callers leave it off.
	AutoFocusWeb bool `json:"auto_focus_web,omitempty"`

	// IncludeGroupContext controls whether the group's default_context
	// (when set) is folded into the new agent's startup briefing. A nil
	// pointer means opt-in — every spawn path inherits the group
	// context by default, the same way it inherits default_cwd. Set it
	// to false explicitly to opt out.
	IncludeGroupContext *bool `json:"include_group_context,omitempty"`

	// ReplyTo optionally names whom the spawned agent's reply to its
	// startup briefing should reach — any selector ResolveSelector
	// accepts (conv-id / prefix / title). Omitted: the briefing's
	// sender defaults to the spawn requester (the calling agent, or ""
	// for a human-initiated spawn).
	ReplyTo string `json:"reply_to,omitempty"`

	// Attachments is an optional list of absolute file paths to surface in
	// the new agent's startup briefing — the dashboard spawn modal uploads
	// files / pasted screenshots to /api/spawn-attachments (which writes them
	// to a temp dir) and passes the resulting paths here. They are purely
	// informational: the daemon folds them into the briefing as an "Attached
	// files" section so the agent can `Read` them on its first turn. No file
	// is read by the daemon — the spawned agent runs as the same user and
	// opens the paths itself — so the paths are only sanitised (control chars
	// rejected, count/length capped), not access-checked. Empty for a spawn
	// with no attachments.
	Attachments []string `json:"attachments,omitempty"`

	// IsOwner, when true, makes the spawned agent a group owner of the target
	// group at birth — the same structural grant the Edit-agent modal's
	// "Group owner" checkbox / `tclaude agent groups owners add` confers,
	// applied during enrollment so the new agent comes up already owning the
	// group (and thus holding its owner-conferred slugs). false (the default)
	// spawns an ordinary member. Mirrors the group-template
	// GroupTemplateAgent.IsOwner field; honoured only for a human (dashboard)
	// caller or a caller that already owns the target group (the daemon
	// rejects an escalation attempt by a non-owner agent).
	IsOwner bool `json:"is_owner,omitempty"`

	// WriteProofToken answers a dir write-proof challenge (the daemon's 403
	// with code "write_proof_required"). A sandboxed agent caller must prove
	// it can itself write in the directories the spawned agent would get
	// write access to (Cwd, plus WorktreePath when set): the daemon hands out
	// a single-use token, the caller creates an empty file named after it in
	// each directory, and retries the same request with the token set here.
	// DaemonRequestWithWriteProof runs that dance transparently for the CLI.
	// Empty for the first attempt and for exempt callers (humans, fully-open
	// sandboxes).
	WriteProofToken string `json:"write_proof_token,omitempty"`

	// PermissionOverrides sets the new agent's permanent per-slug permission
	// overrides at birth — the same grant/deny rows the per-agent permission
	// editor writes, applied during enrollment so the agent's first turn sees
	// them. It maps a registered permission slug to its effect: "grant" or
	// "deny" (a slug left out, or mapped to "default"/"", carries no override
	// and inherits the global default). The daemon validates every slug
	// against the registry and every effect against {grant,deny}; an unknown
	// slug or bad effect is a 400. Like IsOwner it is gated to a human caller
	// or an owner of the target group. Empty for a spawn that takes the
	// group's default permissions.
	PermissionOverrides map[string]string `json:"permission_overrides,omitempty"`

	// Presence bits preserve an explicit JSON false across profile overlays.
	// They are populated by UnmarshalJSON and intentionally stay off the wire.
	autoReviewSpecified bool
	trustDirSpecified   bool
}

// UnmarshalJSON records whether the two plain-bool launch fields appeared on
// the wire. Their values alone cannot distinguish omitted from explicit false,
// but profile precedence must: an explicit false beats every profile, and a
// higher-tier profile's false beats a lower-tier true.
func (r *SpawnRequest) UnmarshalJSON(data []byte) error {
	type alias SpawnRequest
	var decoded alias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	*r = SpawnRequest(decoded)
	_, r.autoReviewSpecified = fields["auto_review"]
	_, r.trustDirSpecified = fields["trust_dir"]
	return nil
}

// MarshalJSON keeps an explicitly selected false from an explicit profile on
// the wire despite the public bool fields' historical omitempty tags.
func (r SpawnRequest) MarshalJSON() ([]byte, error) {
	type alias SpawnRequest
	data, err := json.Marshal(alias(r))
	if err != nil {
		return nil, err
	}
	if !r.autoReviewSpecified && !r.trustDirSpecified {
		return data, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	if r.autoReviewSpecified {
		fields["auto_review"] = json.RawMessage("false")
		if r.AutoReview {
			fields["auto_review"] = json.RawMessage("true")
		}
	}
	if r.trustDirSpecified {
		fields["trust_dir"] = json.RawMessage("false")
		if r.TrustDir {
			fields["trust_dir"] = json.RawMessage("true")
		}
	}
	return json.Marshal(fields)
}

// AutoReviewSpecified reports whether auto_review appeared in decoded JSON.
func (r SpawnRequest) AutoReviewSpecified() bool { return r.autoReviewSpecified }

// TrustDirSpecified reports whether trust_dir appeared in decoded JSON.
func (r SpawnRequest) TrustDirSpecified() bool { return r.trustDirSpecified }

// SpawnParams drives `tclaude agent spawn <group>`. The daemon does
// the actual spawn + group-join; this struct just shapes the request.
type SpawnParams struct {
	Group          string `pos:"true" help:"Existing group to join the new agent into"`
	Name           string `long:"name" short:"n" optional:"true" help:"Name for the new agent (e.g. 'reviewer'). Becomes its conversation title via /rename"`
	Role           string `long:"role" short:"r" optional:"true" help:"Role tag for the new member (e.g. 'tech-lead')"`
	Descr          string `long:"descr" short:"d" optional:"true" help:"Short one-line description shown on the dashboard. Keep it terse — use --initial-message for the task brief"`
	InitialMessage string `long:"initial-message" short:"m" optional:"true" help:"Task brief delivered to the new agent's inbox. Newlines are preserved — pass a full multi-line brief if you like"`
	File           string `long:"file" short:"f" optional:"true" help:"Read the task brief from this file instead of --initial-message ('-' reads stdin). Sidesteps shell quoting — best for long, multi-line, or backtick-containing briefs. Mutually exclusive with --initial-message; same 16384-byte cap"`
	ReplyTo        string `long:"reply-to" optional:"true" help:"Whom the new agent's reply to its startup brief should reach (conv-id / prefix / title). Defaults to you when you are an agent; empty for a human-initiated spawn"`
	Cwd            string `long:"cwd" short:"C" optional:"true" help:"Working directory for the new CC session (defaults to the caller's cwd)"`
	Timeout        string `long:"timeout" short:"t" optional:"true" help:"How long to wait for the new conv-id to materialise (e.g. 30s, 1m). Default 30s."`

	// Profile pre-fills the spawn fields from a saved spawn profile (JOH-210),
	// the CLI twin of the dashboard's "Load from profile" picker. An explicit
	// short is pinned so boa's short-flag enricher doesn't hand `-p` elsewhere.
	// Precedence: explicit flags override the profile, which overrides the
	// group / global / harness defaults (see mergeProfileIntoSpawn).
	Profile                   string `long:"profile" short:"p" optional:"true" help:"RECOMMENDED: pre-fill the launch shape and identity from a spawn profile preconfigured by the operator (see 'tclaude agent profiles ls') — with a profile, usually no other launch flags are needed. Explicit flags override the profile; the profile overrides group/global/harness defaults"`
	SandboxProfile            string `long:"sandbox-profile" optional:"true" help:"Human-only filesystem/environment sandbox profile for this spawn"`
	OmitSandboxProfiles       bool   `long:"omit-sandbox-profiles" help:"Human-only: omit all global, group, and explicit tclaude sandbox-profile values for this launch; mutually exclusive with --sandbox-profile"`
	IUnderstandBreakGlassRisk bool   `long:"i-understand-break-glass-risk" optional:"true" help:"REMOVED: break-glass no longer exists (TCL-791); passing this flag is an error"`

	Worktree     string `long:"worktree" short:"w" optional:"true" help:"Create (or reuse) a git worktree on this branch and spawn the agent into it. The worktree is created in the repo containing --cwd, unless --worktree-repo points elsewhere. Mirrors the dashboard spawn modal's worktree picker"`
	WorktreeBase string `long:"worktree-base" optional:"true" help:"Base branch for a newly-created --worktree (default: the repo's default branch). Ignored when the --worktree branch already exists"`
	WorktreeRepo string `long:"worktree-repo" optional:"true" help:"Repo to create the --worktree in when it differs from --cwd (the monorepo sub-repo case): the agent still launches in --cwd and the worktree path/branch ride into its welcome. Default: the repo containing --cwd, with the agent launched inside the worktree"`

	// AskHuman keeps its -a short pinned explicitly and is declared
	// before --auto-focus: boa's short-flag enricher assigns the first
	// free letter in field order, so without this --auto-focus would
	// otherwise grab -a.
	AskHuman string `long:"ask-human" short:"a" optional:"true" help:"On permission denial, ask the human via popup with this timeout. Capped at 300s. Timeout = deny."`

	AutoFocus      bool `long:"auto-focus" help:"Open a terminal window attached to the new agent once it spawns (default: off — CLI spawns are usually programmatic; the dashboard's modal defaults this on)"`
	NoGroupContext bool `long:"no-group-context" help:"Do not deliver the group's shared startup context to the new agent (default: the group context is included, same as every other spawn path)"`

	// Task and TaskLabel take no short and are declared here — after every
	// explicit-short field — for the same reason as Effort/Model/Harness
	// below: boa's short-flag enricher assigns the first free letter in field
	// order, so a no-short `--task` declared earlier would grab `-t` and then
	// collide with `--timeout`'s explicit `-t` (a pflag duplicate-shorthand
	// panic at command construction — the whole CLI, not just spawn). Declared
	// after `--timeout` claims `-t`, both simply get no short.
	// TestCommandTreeConstructs guards this class of regression.
	Task      string `long:"task" optional:"true" help:"Task-reference link (http(s)) for the new agent — e.g. its Linear issue or GitHub PR. Rendered as a clickable label in the dashboard's Task column"`
	TaskLabel string `long:"task-label" optional:"true" help:"Optional display label overriding the auto-derived one for --task (Linear->JOH-xxx, GitHub->#nnn, else host)"`

	// Effort and Model are declared last so boa's short-flag enricher
	// (which assigns the first free letter in field order) cannot steal
	// a short from any existing field. No explicit shorts — `--effort`
	// and `--model` only.
	Effort string `long:"effort" optional:"true" help:"Reasoning effort for the new agent (per-harness; Copilot also accepts none|minimal). Unset = filled by the default-profile chain, then the harness's own default. See 'Default resolution' in the command help"`
	Model  string `long:"model" optional:"true" help:"Model for the new agent. Claude: fable|fable[1m]|opus|opus[1m]|sonnet|sonnet[1m]|haiku|opusplan or a full model ID. Codex: a codex model name. Unset = filled by the default-profile chain, then the harness's own default. See 'Default resolution' in the command help"`

	// Harness picks the coding harness the new agent runs. Declared last
	// (no explicit short) for the same reason as Effort/Model — boa's
	// short-flag enricher must not steal a letter from an existing field.
	Harness string `long:"harness" optional:"true" help:"Coding harness for the new agent: claude | codex. Other launch flags never infer or pin it. Unset resolves from --profile, the group default profile, the global default profile, then claude. See 'Default resolution' in the command help"`

	// Sandbox is the launch-time harness-builtin sandbox mode for the new agent. Codex takes
	// a native --sandbox enum; Claude Code has no launch flag, so its
	// inherit/on/off modes are delivered as a `--settings` override (see
	// harness.claudeSandbox). Declared last (no explicit short) for the same
	// reason as the fields above.
	Sandbox string `long:"sandbox" optional:"true" help:"The new agent's HARNESS-BUILTIN sandbox mode: what its harness does about sandboxing, not who enforces containment (that is --sandbox-impl). Codex: tclaude-agent (managed profile, keeps agentd reachable) | workspace-write | read-only | danger-full-access. Claude Code: inherit (use settings.json as-is) | on (force the OS sandbox on via --settings, agentd reachable) | off. Unset = filled by the default-profile chain, then the harness default (Codex: tclaude-agent; Claude: inherit). See 'Default resolution' in the command help"`

	// AskUserQuestionTimeout is the Claude Code AskUserQuestion idle-timeout
	// override for the new agent, delivered via `--settings`. Declared last (no
	// explicit short) like the fields above. Unset = inherit (no override).
	AskUserQuestionTimeout string `long:"ask-user-question-timeout" optional:"true" help:"Claude Code AskUserQuestion idle-timeout for the new agent: inherit (use settings.json as-is) | never (wait for a human) | 60s | 5m | 10m (auto-continue with the default answer after the interval — keeps an unattended agent moving). Unset = inherit. Not applicable to codex"`

	// Approval is the launch-time approval/permission posture for the new
	// agent. Codex takes an --ask-for-approval policy; Claude Code's approval
	// posture is its --permission-mode, carried through this same field (the
	// spawner emits the harness-appropriate flag). Declared last (no explicit
	// short) for the same reason as the fields above. Unset resolves through the
	// profile chain and then to each harness's safe default (Codex: never;
	// Claude: auto). For an agent caller, a bare harness default is narrowed to
	// the caller's own same-harness posture when the caller cannot grant it.
	Approval string `long:"ask-for-approval" optional:"true" help:"Launch approval/permission posture for the new agent (per-harness). Codex policy: untrusted|on-failure|on-request|never. Claude Code permission mode: inherit|plan|acceptEdits|default|auto|dontAsk|bypassPermissions. Unset = filled by the profile chain, then the harness default (Codex: never; Claude: auto); an agent caller that cannot grant that default gets its own same-harness posture instead (disclosed in the resolved launch notes)"`

	// ToolGovernance is OpenCode's independent built-in tool action.
	ToolGovernance string `long:"tools" optional:"true" help:"OpenCode tool governance for bash/glob/grep/lsp/task/skill: allow | ask | deny. Unset = filled by the profile chain, then allow. Not applicable to claude/codex"`

	// AutoReview is a bool flag declared last (no explicit short) for the same
	// reason as the fields above — boa's short-flag enricher must not steal a
	// letter. Experimental opt-in (off by default). See JOH-200 part 2.
	AutoReview bool `long:"auto-review" help:"EXPERIMENTAL: route the new agent's Codex approval prompts to the guardian subagent (auto-decides in your place) instead of asking you. Off by default. Not applicable to claude"`

	RemoteControl bool `long:"remote-control" help:"Start the new agent with Claude Code Remote Access ON (claude --remote-control), so it is reachable from the Claude app. Off by default. Requires a claude.ai login to pair. Not applicable to codex"`

	// AutoMemory opts the new agent back INTO Claude Code's auto memory,
	// which tclaude disables by default. Opt-in only on the CLI, like
	// --remote-control: the flag sends &true and its absence leaves the
	// pointer nil so a profile default can still speak.
	AutoMemory bool `long:"auto-memory" help:"Keep Claude Code's built-in auto memory ON for the new agent. Off by default: tclaude disables it because agents sharing a checkout cross-pollute one project memory store. Does not affect CLAUDE.md. Not applicable to codex"`

	// ContextFeatures trims the new agent's Claude Code startup context. Unset
	// leaves the pointer nil so a profile default can still speak, matching
	// --auto-memory's opt-in-only CLI shape; an explicit "none" is how the CLI
	// says "trim nothing, ignore the profile".
	// No --help-context-features twin here: Group is a REQUIRED positional, and
	// cobra validates args before RunFunc, so a bare `agent spawn
	// --help-context-features` could only ever fail with "accepts 1 arg". The
	// catalog listing lives on `tclaude session new`, which has no required
	// positional; the help below points there rather than shipping a flag that
	// cannot work.
	ContextFeatures string `long:"context-features" optional:"true" help:"Trim the new agent's Claude Code startup context: comma-separated <feature>[=on|off] (a bare feature means off), or 'none' to override a profile and trim nothing. E.g. bundled-skills,artifact,workflows. Unset = filled by the profile chain. Run 'tclaude session new --help-context-features' for the catalog. Not applicable to codex"`

	// AutoCompactWindow pins where Claude Code auto-compacts for the new agent.
	// A plain string flag (unlike the opt-in-only bools above), so a blank still
	// defers to the profile chain and an explicit value overrides it.
	AutoCompactWindow string `long:"auto-compact-window" optional:"true" help:"Context capacity in tokens for the new agent's Claude Code auto-compaction (CLAUDE_CODE_AUTO_COMPACT_WINDOW). Accepts 450000, 450k or 0.5M. Pin it below a 1M model's real window so a long-lived agent compacts while it is still sharp. Capped at the model's actual window. Unset = filled by the profile chain, then the model default. Not applicable to codex"`

	// SandboxImpl picks WHO OWNS OS-level containment for the new agent — an axis
	// independent of --sandbox, which picks a mode WITHIN whatever sandbox is in
	// force. Blank defers to the profile chain and then to the harness's
	// historical behavior, so an unpassed flag launches exactly as it did
	// before this flag existed.
	SandboxImpl string `long:"sandbox-impl" optional:"true" help:"EXPERIMENTAL OS containment: off | harness-builtin (only for a harness with a real built-in OS sandbox) | tclaude-layer (tclaude outer wall, inner OS sandbox off) | stacked (Linux Claude/Codex only; live model-free real-engine probe, both walls). Copilot children spawned by an agent are admitted in exactly one topology: tclaude-layer. Experimental implementations refuse naming the missing capability and never fall back. Unset = profile chain, then historical harness behavior; for OpenCode that is a command filter, not confinement"`
}

// spawnCmd starts a fresh CC session and registers it in an existing
// group in one shot. Useful for "I want to delegate this in parallel"
// flows where you want the new agent to be reachable by name from the
// existing team without manually wiring up membership after the fact.
func spawnCmd() *cobra.Command {
	return boa.CmdT[SpawnParams]{
		Use:   "spawn",
		Short: "Spawn a fresh CC session and add it to an existing group",
		Long: "Prefer a spawn profile preconfigured by the operator: with --profile <name> " +
			"(see `tclaude agent profiles ls`) or a group/global default profile, usually " +
			"no other launch flags are needed. " +
			"\n\n" +
			"Launches `tclaude session new -d --global` with a generated label, " +
			"waits for the new conv-id to materialise, and adds the new conv to <group> " +
			"with the given role/descr. --name becomes the new agent's conversation " +
			"title (injected as /rename on its pane). Prints the attach command for the " +
			"new session. --descr is the short dashboard label; pass --initial-message to " +
			"deliver the new agent its first task brief to its inbox (newlines preserved). " +
			"For a long or multi-line brief, prefer --file <path> (or --file - to read " +
			"stdin) — it reads the brief from a file and so sidesteps shell quoting, " +
			"including backticks the shell would otherwise eat from an inline string. " +
			"\n\n" +
			"--worktree <branch> creates (or reuses) a git worktree on that branch and " +
			"spawns the agent into it — the CLI equivalent of the dashboard spawn modal's " +
			"worktree picker. The worktree is created in the repo containing --cwd; pass " +
			"--worktree-base to pick the branch it is cut from. For a monorepo launch dir " +
			"whose code work belongs in a nested sub-repo, point --worktree-repo at the " +
			"sub-repo: the agent then launches in --cwd and the worktree rides into its " +
			"welcome message. The checkout itself runs in the daemon, outside your " +
			"sandbox — a sandboxed caller proves it can write the repo and the " +
			"worktree's parent instead. " +
			"\n\n" +
			"Default resolution. Each launch field (--harness, --model, --effort, " +
			"--sandbox, --sandbox-impl, --ask-for-approval, --ask-user-question-timeout) is resolved " +
			"independently through this precedence, highest first:\n" +
			"  1. the explicit flag\n" +
			"  2. --profile (a saved spawn profile you name)\n" +
			"  3. the group's default spawn profile\n" +
			"  4. the global (dashboard) default spawn profile\n" +
			"  5. the harness's own default\n" +
			"For an agent caller, a posture left unset through the profile chain may be " +
			"narrowed from the harness default to the caller's own same-harness posture; " +
			"the resolved launch notes disclose that adjustment. Explicit and profile-set " +
			"postures are never silently narrowed. " +
			"The harness is resolved through that full chain FIRST; model and other " +
			"launch fields are then validated against it. An incompatible explicit " +
			"flag is a loud error with matching --harness/--model guidance. An " +
			"incompatible field from a lower profile tier is ignored, falls through " +
			"to the next tier/default, and is disclosed in the resolved-shape echo. " +
			"A spawn profile carries its OWN harness, so an unset --harness is NOT the " +
			"same as claude: if a default profile at tier 3 or 4 selects codex, a " +
			"no-flag spawn lands on codex (and that profile's model). When a policy " +
			"requires a specific vendor/model, pass explicit --harness + --model (or a " +
			"--profile that pins them) rather than relying on the default. The spawn " +
			"output echoes the resolved Harness/Model/Effort and where each came from; " +
			"inspect the defaults up front with `tclaude agent profiles default show` " +
			"and `tclaude agent groups ls` (PROFILE column). " +
			"\n\n" +
			"Requires the `groups.spawn` permission (default: human-only).",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *SpawnParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Group).SetAlternativesFunc(completeGroupNames)
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			boa.GetParamT(ctx, &p.Profile).SetAlternativesFunc(completeSpawnProfileNames)
			return nil
		},
		RunFunc: func(p *SpawnParams, _ *cobra.Command, _ []string) {
			_, rc := RunSpawn(p, os.Stdout, os.Stderr, os.Stdin)
			os.Exit(rc)
		},
	}.ToCobra()
}

// resolvedSpawnFields holds the per-field spawn values after a --profile has
// been folded under the CLI's explicit flags. It is the CLI-side twin of the
// dashboard's applyProfileToSpawnForm: an explicit flag wins; a field the flag
// left blank takes the profile's value; a field neither sets stays blank, for
// the daemon to fill from the group default profile, global default profile,
// then the harness default.
// Pure data — RunSpawn validates these against the resolved harness and builds
// the SpawnRequest from them.
type resolvedSpawnFields struct {
	Harness                string
	Model                  string
	Effort                 string
	Sandbox                string
	SandboxImpl            string
	AskUserQuestionTimeout string
	AutoCompactWindow      string
	Approval               string
	ToolGovernance         string

	Name           string
	Role           string
	Descr          string
	InitialMessage string

	AutoReview bool
	TrustDir   bool
	AutoFocus  bool

	IsOwner             bool
	PermissionOverrides map[string]string
	// Deliberately NO ContextFeatures here. The CLI does not fold a --profile's
	// trims into the request: it sends its own map or nothing, and lets the
	// daemon's tier stack resolve the named profile (the same reason
	// remote_control is not folded in). A field here would imply otherwise.

	// IncludeGroupContext is tri-state on the wire: nil = default (the daemon
	// includes the group context), &false = exclude. --no-group-context forces
	// &false; else the profile's value; else nil.
	IncludeGroupContext *bool
}

// harnessEquivalent reports whether two explicit harness names refer to the
// same harness. Callers handle a blank profile harness separately: it is
// harness-neutral and its fields are validated against the harness selected by
// a lower resolution tier at spawn time.
func harnessEquivalent(a, b string) bool {
	return strings.TrimSpace(a) == strings.TrimSpace(b)
}

// mergeProfileIntoSpawn folds a spawn profile's fields under the CLI's explicit
// flags, mirroring the dashboard's client-side pre-fill (JOH-210). Precedence,
// per field: explicit flag > profile > blank. The daemon then fills a still-
// blank field from the group's default profile, then the global profile, then
// the harness default — so the intended order is flag > --profile > group >
// global > harness. This holds for sandbox/approval too: the CLI VALIDATES those without
// applying the harness default (see RunSpawn), so an omitted flag reaches the
// daemon still blank and the group-default-profile layer applies uniformly
// across all launch fields, not just harness/model/effort. An explicit
// `inherit` is carried through as a first-class value the daemon won't override.
//
// The launch fields (harness/model/effort/sandbox/approval/auto_review/
// trust_dir) are inherited ONLY when the effective harness matches the
// profile's — the same gate handleGroupSpawn applies to a group default
// profile. A spawn that pins a *different* --harness brings its own launch
// config (validated against that harness); copying the profile's foreign model/
// sandbox over it would just 400 at validation. A blank --harness adopts the
// profile's. Identity fields (name/role/descr/initial_message) and the harness-
// agnostic toggles (auto_focus, include_group_context, is_owner,
// permission_overrides) are inherited regardless of harness.
//
// Three profile fields are deliberately NOT applied here:
//   - remote_control: the CLI can't see the group's remote-control policy, which
//     must win over a profile default (JOH-262), and the wire carries
//     RemoteControl as an authoritative *bool with no "soft default" channel.
//     Use the explicit --remote-control flag to arm it.
//   - auto_memory: likewise carried as an authoritative *bool. Leaving it nil
//     here is what lets the daemon's profile tiers resolve it; the CLI's
//     --auto-memory flag sets it explicitly when the caller wants to force it.
//   - sync_worktree: CLI worktrees are flag-driven (--worktree <branch>) and a
//     profile carries no branch, so the toggle has nothing to act on here.
//
// The bool toggles (auto_review, auto_focus) are opt-in-only on the CLI: a
// profile's true is honoured, but a plain-bool flag can't distinguish "unset"
// from an explicit false, so there is no CLI way to force a profile's true back
// off. That asymmetry mirrors the plain-flag surface, not a precedence bug.
//
// explicitMessage is the already-resolved --initial-message / --file body (the
// profile fills it only when the caller passed none). prof may be nil (no
// --profile), making this a faithful pass-through of the flags.
func mergeProfileIntoSpawn(p *SpawnParams, explicitMessage string, prof *profileJSON) resolvedSpawnFields {
	// pick returns the explicit flag when set, else the profile's value (only
	// when a profile is present), else blank.
	pick := func(flag, profile string) string {
		if v := strings.TrimSpace(flag); v != "" {
			return v
		}
		if prof != nil {
			return strings.TrimSpace(profile)
		}
		return ""
	}

	out := resolvedSpawnFields{
		AutoReview:     p.AutoReview,
		AutoFocus:      p.AutoFocus,
		InitialMessage: explicitMessage,
	}

	// Identity fields — harness-agnostic, always inherited (flag wins).
	out.Name = pick(p.Name, profStr(prof, func(pf *profileJSON) string { return pf.AgentName }))
	out.Role = pick(p.Role, profStr(prof, func(pf *profileJSON) string { return pf.Role }))
	out.Descr = pick(p.Descr, profStr(prof, func(pf *profileJSON) string { return pf.Descr }))
	if out.InitialMessage == "" && prof != nil {
		out.InitialMessage = strings.TrimSpace(prof.InitialMessage)
	}

	// Launch fields — inherited only when the effective harness matches the
	// profile's (or the caller pinned no --harness, adopting the profile's).
	profLaunch := prof != nil && (strings.TrimSpace(p.Harness) == "" ||
		strings.TrimSpace(prof.Harness) == "" || harnessEquivalent(p.Harness, prof.Harness))
	if profLaunch {
		out.Harness = pick(p.Harness, prof.Harness)
		out.Model = pick(p.Model, prof.Model)
		out.Effort = pick(p.Effort, prof.Effort)
		out.Sandbox = pick(p.Sandbox, prof.Sandbox)
		out.AskUserQuestionTimeout = pick(p.AskUserQuestionTimeout, prof.AskUserQuestionTimeout)
		out.AutoCompactWindow = pick(p.AutoCompactWindow, prof.AutoCompactWindow)
		out.SandboxImpl = pick(p.SandboxImpl, prof.SandboxImplementation)
		out.Approval = pick(p.Approval, prof.Approval)
		out.ToolGovernance = pick(p.ToolGovernance, prof.ToolGovernance)
		if !out.AutoReview && prof.AutoReview != nil {
			out.AutoReview = *prof.AutoReview
		}
		if prof.TrustDir != nil {
			out.TrustDir = *prof.TrustDir
		}
	} else {
		out.Harness = strings.TrimSpace(p.Harness)
		out.Model = strings.TrimSpace(p.Model)
		out.Effort = strings.TrimSpace(p.Effort)
		out.Sandbox = strings.TrimSpace(p.Sandbox)
		out.AskUserQuestionTimeout = strings.TrimSpace(p.AskUserQuestionTimeout)
		out.AutoCompactWindow = strings.TrimSpace(p.AutoCompactWindow)
		out.SandboxImpl = strings.TrimSpace(p.SandboxImpl)
		out.Approval = strings.TrimSpace(p.Approval)
		out.ToolGovernance = strings.TrimSpace(p.ToolGovernance)
	}

	// Harness-agnostic toggles / access — inherited from the profile regardless
	// of harness (flag / presence wins).
	if prof != nil {
		if !out.AutoFocus && prof.AutoFocus != nil {
			out.AutoFocus = *prof.AutoFocus
		}
		if prof.IsOwner != nil {
			out.IsOwner = *prof.IsOwner
		}
		if len(prof.PermissionOverrides) > 0 {
			out.PermissionOverrides = prof.PermissionOverrides
		}
	}

	// Group context: --no-group-context forces exclude; else the profile's
	// include/exclude default; else nil (the daemon includes by default).
	switch {
	case p.NoGroupContext:
		no := false
		out.IncludeGroupContext = &no
	case prof != nil && prof.IncludeGroupDefaultContext != nil:
		v := *prof.IncludeGroupDefaultContext
		out.IncludeGroupContext = &v
	}

	return out
}

// profStr safely reads a string field from a possibly-nil profile.
func profStr(prof *profileJSON, get func(*profileJSON) string) string {
	if prof == nil {
		return ""
	}
	return get(prof)
}

// validateSpawnModel adds the resolved harness and an actionable correction to
// the catalog's detailed error. Spawn callers should never have to infer that
// an unset harness resolved through a profile chain before rejecting a model.
func validateSpawnModel(h *harness.Harness, model string) (string, error) {
	validated, err := h.Models.ValidateModel(model)
	if err == nil {
		return validated, nil
	}
	other := "codex"
	if h.Name == "codex" {
		other = harness.DefaultName
	}
	return "", fmt.Errorf("model %q is not valid for %s; pass --harness %s or a matching --model: %w",
		strings.TrimSpace(model), h.Name, other, err)
}

// validateSpawnSandboxImplementation checks the two things a CLIENT can know
// about --sandbox-impl: that the value names a real implementation, and that
// the resolved harness can host it. Blank stays blank so the daemon's profile
// tier stack can still speak — normalizing it to harness-builtin here would
// turn an unset flag into a pin that silences every lower tier.
//
// Host capability is deliberately absent. The daemon owns that gate and runs
// it live at the spawn boundary; see the call site.
func validateSpawnSandboxImplementation(h *harness.Harness, raw string) (string, error) {
	implementation, err := sandboxpolicy.NormalizeImplementation(raw)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(raw) != "" &&
		implementation == sandboxpolicy.ImplementationHarnessBuiltin {
		if err := harness.ValidateHarnessBuiltinOSSandbox(h); err != nil {
			return "", err
		}
	}
	if implementation.UsesNestedHarnessSandbox() {
		if err := session.ValidateStackedSandboxHarness(h); err != nil {
			return "", err
		}
	}
	if implementation.UsesTclaudeLayer() {
		if err := session.ValidateTclaudeLayerHarness(h.Name); err != nil {
			return "", err
		}
	}
	if strings.TrimSpace(raw) == "" {
		return "", nil
	}
	return string(implementation), nil
}

// RunSpawn drives `tclaude agent spawn`. Returns the daemon's response
// (nil on failure) alongside an exit code for the CLI wrapper. Flow
// tests use the returned response to assert what the user would see
// printed; the CLI wrapper just propagates the exit code. stdin backs
// `--file -` (read the brief from a pipe).
func RunSpawn(p *SpawnParams, stdout, stderr io.Writer, stdin io.Reader) (*SpawnResponse, int) {
	if p.Group == "" {
		fmt.Fprintln(stderr, "Error: group is required")
		return nil, rcInvalidArg
	}
	if p.IUnderstandBreakGlassRisk {
		fmt.Fprintln(stderr, "Error:", breakGlassFlagRemoved())
		return nil, rcInvalidArg
	}
	if p.OmitSandboxProfiles && strings.TrimSpace(p.SandboxProfile) != "" {
		fmt.Fprintln(stderr, "Error: --omit-sandbox-profiles and --sandbox-profile are mutually exclusive")
		return nil, rcInvalidArg
	}
	// --worktree-base / --worktree-repo are modifiers of --worktree;
	// rejecting them up front beats silently ignoring a flag the user
	// expected to take effect.
	if strings.TrimSpace(p.Worktree) == "" {
		if strings.TrimSpace(p.WorktreeBase) != "" {
			fmt.Fprintln(stderr, "Error: --worktree-base requires --worktree")
			return nil, rcInvalidArg
		}
		if strings.TrimSpace(p.WorktreeRepo) != "" {
			fmt.Fprintln(stderr, "Error: --worktree-repo requires --worktree")
			return nil, rcInvalidArg
		}
	}
	rawMessage, rc := resolveBodyInput(p.InitialMessage, p.File, "--initial-message", stdin, stderr)
	if rc != rcOK {
		return nil, rc
	}
	// Cap-check the explicitly-provided brief up front (fail-fast, no daemon
	// needed). A profile-sourced brief was validated at save; the merged result
	// is re-checked below once the profile is folded in.
	explicitMessage := strings.TrimSpace(rawMessage)
	if !isValidInitialMessage(explicitMessage) {
		fmt.Fprintf(stderr, "Error: REJECTED. --initial-message must be at most %d characters.\n", MaxInitialMessageBytes)
		fmt.Fprintln(stderr, "Newlines and tabs are allowed (the brief is delivered to the agent's")
		fmt.Fprintln(stderr, "inbox, not typed into its pane), but other control characters are not.")
		return nil, rcInvalidArg
	}
	timeoutSeconds := 30
	if p.Timeout != "" {
		d, err := parseDurationDays(p.Timeout)
		if err != nil || d <= 0 {
			fmt.Fprintf(stderr, "Error: invalid --timeout %q\n", p.Timeout)
			return nil, rcInvalidArg
		}
		// Cap mirrors the daemon's 5-minute hard limit.
		secs := int(d.Seconds())
		if secs > 300 {
			secs = 300
		}
		timeoutSeconds = secs
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return nil, rc
	}

	// Fetch the named --profile (reads are open on the daemon) so its saved
	// fields can pre-fill any blank the flags left. Only when --profile is set.
	var prof *profileJSON
	if strings.TrimSpace(p.Profile) != "" {
		var frc int
		if prof, frc = fetchSpawnProfile(strings.TrimSpace(p.Profile), stderr); frc != rcOK {
			return nil, frc
		}
		if profileDisabledValue(prof) {
			fmt.Fprintf(stderr, "Error: spawn profile %q is disabled: %s\n", prof.Name, prof.DisabledReason)
			return nil, rcInvalidArg
		}
	}
	// Fold the profile under the explicit flags (flag > profile > blank). The
	// daemon then fills any still-blank launch field from the group's default
	// profile, global default profile, then the harness default.
	merged := mergeProfileIntoSpawn(p, explicitMessage, prof)

	// Effective brief: an explicit one was cap-checked above; a profile one was
	// validated at save (buildProfileFromJSON runs the same isValidInitialMessage
	// gate), so this re-check is belt-and-suspenders — it defends any future
	// profile-sourced brief that reaches here unvalidated.
	initialMessage := merged.InitialMessage
	if !isValidInitialMessage(initialMessage) {
		fmt.Fprintf(stderr, "Error: REJECTED. the profile's initial message must be at most %d characters, "+
			"with no control characters other than newlines and tabs.\n", MaxInitialMessageBytes)
		return nil, rcInvalidArg
	}

	// Validate the effective --name (the flag, else the profile's agent_name)
	// client-side so a bad name fails fast with a clear message. An empty name
	// is fine (the agent gets an auto-generated label); a non-empty one must be
	// a safe token. When config's agent.spawn_name_normalize is on (the default)
	// a bad name is coerced to the safe charset — the same way the dashboard
	// modal does — else it is rejected. The daemon re-validates/normalizes
	// server-side (handleGroupSpawn) as the authoritative backstop.
	name := strings.TrimSpace(merged.Name)
	if !isValidSpawnName(name) {
		if cfg, _ := config.Load(); cfg.SpawnNameNormalizeEnabled() {
			name = NormalizeSpawnName(name)
		}
	}
	if !isValidSpawnName(name) {
		fmt.Fprintf(stderr, "Error: REJECTED. --name must be 1-%d characters from [A-Za-z0-9_-]\n", MaxSpawnNameLen)
		fmt.Fprintln(stderr, "(letters, digits, underscore, dash) — no spaces, punctuation, or unicode.")
		fmt.Fprintln(stderr, "Pick a name that uses only the allowed characters.")
		return nil, rcInvalidArg
	}

	ask, err := ParseAskHuman(p.AskHuman)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return nil, rcInvalidArg
	}
	// Harness-dependent validation is fail-fast only when the client knows the
	// harness: an explicit --harness, or a named --profile that carries one.
	// Otherwise group/global defaults can still select the vendor, so keep the
	// raw launch fields on the wire and let the authoritative daemon resolve the
	// full chain before validating them against the selected catalog.
	effort := strings.TrimSpace(p.Effort)
	model := strings.TrimSpace(p.Model)
	harnessBuiltinMode := strings.TrimSpace(p.Sandbox)
	approvalPolicy := strings.TrimSpace(p.Approval)
	toolGovernance := strings.TrimSpace(p.ToolGovernance)
	askTimeout := strings.TrimSpace(p.AskUserQuestionTimeout)
	autoCompactWindow := strings.TrimSpace(p.AutoCompactWindow)
	sandboxImpl := strings.TrimSpace(p.SandboxImpl)
	autoReview := p.AutoReview
	trustDir := false
	remoteControl := p.RemoteControl
	autoMemory := p.AutoMemory
	// --context-features is tri-state on the wire: unset leaves the map nil so the
	// daemon's profile tier stack still speaks, while an explicit `none` sends an
	// empty (non-nil) map, which is how the CLI says "ignore the profile, trim
	// nothing". Everything else parses as a slug list.
	var contextFeatures map[string]string
	contextFeaturesSet := strings.TrimSpace(p.ContextFeatures) != ""
	if contextFeaturesSet {
		if strings.EqualFold(strings.TrimSpace(p.ContextFeatures), "none") {
			contextFeatures = map[string]string{}
		} else {
			parsed, parseErr := harness.ParseContextFeatures(p.ContextFeatures)
			if parseErr != nil {
				fmt.Fprintf(stderr, "Error: %v\n", parseErr)
				return nil, rcInvalidArg
			}
			if parsed == nil {
				parsed = map[string]string{}
			}
			contextFeatures = parsed
		}
	}
	clientHarness := strings.TrimSpace(p.Harness)
	validationHarness := clientHarness
	validateMergedProfile := false
	if validationHarness == "" && prof != nil && strings.TrimSpace(prof.Harness) != "" {
		validationHarness = strings.TrimSpace(prof.Harness)
		validateMergedProfile = true
	}
	if validationHarness != "" {
		h, resolveErr := harness.ResolveSpawnable(validationHarness)
		if resolveErr != nil {
			fmt.Fprintf(stderr, "Error: %v\n", resolveErr)
			return nil, rcInvalidArg
		}
		validationFields := resolvedSpawnFields{
			Model: p.Model, Effort: p.Effort, Sandbox: p.Sandbox,
			Approval: p.Approval, ToolGovernance: p.ToolGovernance,
			AskUserQuestionTimeout: p.AskUserQuestionTimeout,
			AutoCompactWindow:      p.AutoCompactWindow,
			SandboxImpl:            p.SandboxImpl,
			AutoReview:             p.AutoReview,
		}
		if validateMergedProfile {
			validationFields = merged
		}
		if _, err = h.Models.ValidateEffort(validationFields.Effort); err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return nil, rcInvalidArg
		}
		if _, err = validateSpawnModel(h, validationFields.Model); err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return nil, rcInvalidArg
		}
		// Validate --sandbox (an explicit value / an inherited profile value) but do
		// NOT apply the harness default here: a blank stays blank so the daemon's
		// group-default-profile overlay can still fill it before the daemon applies
		// the secure default (harness.ResolveHarnessBuiltinMode) + the cwd-safety guard
		// server-side. Applying the default client-side would resolve an omitted
		// --sandbox to a concrete mode and pre-empt that overlay, so an agent spawned
		// into a group whose default profile sets a sandbox would silently ignore it.
		// An explicit `inherit` is carried through verbatim (a first-class sentinel)
		// so the daemon won't let a profile override it. A mode for a harness with no
		// launch sandbox flag is still rejected fast here.
		if _, err = harness.ValidateHarnessBuiltinMode(h, validationFields.Sandbox); err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return nil, rcInvalidArg
		}
		// Validate --ask-for-approval the same way, and for the same reason: a blank
		// defers to the daemon's group-default overlay + non-escalating default
		// (Codex: never — what keeps the detached, unattended pane from deadlocking on
		// an approval prompt, JOH-200), applied server-side by ResolveApprovalPolicy;
		// an explicit `inherit` is preserved; a policy for a harness with no launch
		// approval flag is rejected fast here.
		if _, err = harness.ValidateApprovalPolicy(h, validationFields.Approval); err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return nil, rcInvalidArg
		}
		if _, err = harness.ValidateToolGovernance(h, validationFields.ToolGovernance); err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return nil, rcInvalidArg
		}
		// Resolve --ask-user-question-timeout: a Claude-Code-only settings.json
		// override (never|60s|5m|10m), so a value for a harness with no
		// AskUserQuestion dialog (Codex) fails fast here. inherit/blank → "" (no
		// override). The daemon re-validates server-side.
		if _, err = harness.ResolveAskTimeoutMode(h, validationFields.AskUserQuestionTimeout); err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return nil, rcInvalidArg
		}
		// Resolve --auto-compact-window the same way: a Claude-Code-only env knob,
		// so a value for a harness without it fails fast here, as does an
		// unparseable or out-of-range token count. The daemon re-validates and
		// re-normalizes server-side.
		if _, err = harness.ResolveAutoCompactWindow(h, validationFields.AutoCompactWindow); err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return nil, rcInvalidArg
		}
		// Fail fast on a --sandbox-impl the value enum or this harness rejects.
		// Only those two: whether the HOST can run tclaude-layer is deliberately
		// not asked here. The daemon owns that gate, runs it live at the spawn
		// boundary, and refuses naming the missing capability — a client-side
		// probe would answer for a machine that need not be this one, and a
		// second opinion that can disagree with the authority is worse than none.
		if _, err = validateSpawnSandboxImplementation(h, validationFields.SandboxImpl); err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return nil, rcInvalidArg
		}
		// Gate the experimental --auto-review opt-in: allowed only for a harness
		// with an approvals subsystem (Codex); requesting it for claude fails fast
		// here with a clear message. The daemon re-gates server-side.
		if _, err = harness.ResolveAutoReview(h, validationFields.AutoReview); err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return nil, rcInvalidArg
		}
		// Resolve the effective trust-dir (taken from a --profile's trust_dir,
		// since the CLI has no dedicated flag). false for any harness is always
		// fine; a true for a harness with no trust dialog fails fast here. The
		// daemon re-gates server-side.
		if _, err = harness.ResolveTrustDir(h, validationFields.TrustDir); err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return nil, rcInvalidArg
		}
		// Gate --remote-control: arming Remote Access at launch is a Claude Code
		// feature, so requesting it for a harness without it (Codex) fails fast here
		// with a clear message. The daemon re-gates server-side. See JOH-258.
		if remoteControl, err = harness.ResolveRemoteControl(h, p.RemoteControl); err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return nil, rcInvalidArg
		}
		// Gate --auto-memory the same way: only a harness with an auto-memory
		// system (Claude Code) can be asked to keep it on. The daemon re-gates
		// server-side.
		if autoMemory, err = harness.ResolveAutoMemory(h, &p.AutoMemory); err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return nil, rcInvalidArg
		}
		// Gate --context-features the same way: unknown slugs, bad states, and
		// trims asked of a harness with no steerable startup context all fail fast
		// here with a clear message. The daemon re-gates server-side. When the flag
		// is absent, validate the profile's map instead (the profile is what would
		// then decide) so a bad saved profile is caught here too.
		featuresToCheck := contextFeatures
		if !contextFeaturesSet && validateMergedProfile && prof != nil {
			featuresToCheck = prof.ContextFeatures
		}
		if _, err = harness.ResolveContextFeatures(h, featuresToCheck); err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return nil, rcInvalidArg
		}
	}
	cwd := p.Cwd
	if cwd == "" {
		if wd, err := os.Getwd(); err == nil {
			cwd = wd
		}
	}

	req := SpawnRequest{
		Profile:                strings.TrimSpace(p.Profile),
		SandboxProfile:         strings.TrimSpace(p.SandboxProfile),
		OmitSandboxProfiles:    p.OmitSandboxProfiles,
		Name:                   name,
		Role:                   merged.Role,
		Descr:                  merged.Descr,
		TaskURL:                strings.TrimSpace(p.Task),
		TaskLabel:              strings.TrimSpace(p.TaskLabel),
		InitialMessage:         initialMessage,
		ReplyTo:                strings.TrimSpace(p.ReplyTo),
		Cwd:                    cwd,
		TimeoutSeconds:         timeoutSeconds,
		AutoFocus:              merged.AutoFocus,
		Effort:                 effort,
		Model:                  model,
		Harness:                clientHarness,
		HarnessBuiltinMode:     harnessBuiltinMode,
		SandboxImplementation:  sandboxImpl,
		AskUserQuestionTimeout: askTimeout,
		AutoCompactWindow:      autoCompactWindow,
		ApprovalPolicy:         approvalPolicy,
		ToolGovernance:         toolGovernance,
		AutoReview:             autoReview,
		TrustDir:               trustDir,
		IsOwner:                merged.IsOwner,
		PermissionOverrides:    merged.PermissionOverrides,
	}
	// Send the map only when the flag spoke. An empty-but-non-nil map is a real
	// instruction ("trim nothing") and must survive JSON, so it is set explicitly
	// rather than left to omitempty.
	if contextFeaturesSet {
		req.ContextFeatures = &contextFeatures
	}
	req.autoReviewSpecified = p.AutoReview
	req.trustDirSpecified = false
	// --remote-control is opt-in only on the CLI: send &true when the flag is set,
	// and leave the pointer nil otherwise so the daemon's group/profile
	// remote-control policy fills it (the dashboard form is the surface that sends
	// an explicit false to override an inherited default). See SpawnRequest.RemoteControl.
	// A --profile's remote_control is deliberately NOT applied here — the CLI can't
	// see the group's remote-control policy, which must win (see mergeProfileIntoSpawn).
	if remoteControl {
		on := true
		req.RemoteControl = &on
	}
	// --auto-memory follows the same opt-in-only discipline: send &true when the
	// flag is set, leave nil otherwise so a profile default can still speak. The
	// nil case resolves to off server-side, which is the recommended posture.
	if autoMemory {
		on := true
		req.AutoMemory = &on
	}
	// Group context: --no-group-context forces exclude, else a --profile may set
	// it; an omitted pointer means the daemon includes the group context by
	// default (every other spawn path does). Resolved in mergeProfileIntoSpawn.
	req.IncludeGroupContext = merged.IncludeGroupContext

	// Worktree handling. The daemon resolves the worktree — the same git
	// operation the dashboard's worktree picker performs server-side, run
	// outside the caller's sandbox (see spawn_worktree.go) — and the
	// resolved cwd / worktree_path / worktree_branch then ride the spawn
	// in the identical wire shape the dashboard sends. createdWorktree is
	// non-empty only when a fresh worktree was made (vs an existing one
	// reused), so a failed spawn can tear it back down rather than
	// leaking an orphan.
	createdWorktree, discardWorktree := "", ""
	if wt := strings.TrimSpace(p.Worktree); wt != "" {
		worktreeRepo := strings.TrimSpace(p.WorktreeRepo)
		if worktreeRepo == "" {
			worktreeRepo = cwd
		}
		prepared, wtErr := resolveSpawnWorktree(worktreeRepo, wt, p.WorktreeBase, ask)
		if wtErr != nil {
			fmt.Fprintf(stderr, "Error: %v\n", wtErr)
			return nil, rcInvalidArg
		}
		wtPath := prepared.Path
		if prepared.Created {
			createdWorktree, discardWorktree = wtPath, prepared.DiscardToken
		}
		// Clean both sides before comparing: a trailing slash or "/."
		// on --worktree-repo must not mis-classify an in-place worktree
		// (worktree-repo == cwd) as the monorepo case.
		if filepath.Clean(worktreeRepo) != filepath.Clean(cwd) {
			// Monorepo sub-repo case: the agent launches in --cwd (the
			// parent) and the worktree path/branch ride into its welcome.
			req.WorktreePath = wtPath
			req.WorktreeBranch = wt
		} else {
			// Common case: the agent launches inside the worktree.
			req.Cwd = wtPath
		}
	}

	var resp SpawnResponse
	if ask > 0 {
		// --ask-human is an authorization fallback, not a blanket trust-root
		// override. An owner/already-granted caller may reach a later lineage
		// denial without any popup being opened, so do not claim a human request
		// is pending before the daemon has actually needed one.
		fmt.Fprintf(stdout, "Human approval may be requested if authorization is needed (timeout %s).\n", ask)
	}
	path := "/v1/groups/" + p.Group + "/spawn"
	// DaemonRequestWithWriteProof transparently answers the daemon's dir
	// write-proof challenge (see writeproof.go): when the caller is a
	// sandboxed agent, the daemon requires proof that this process can itself
	// write in the spawn dirs before it launches an agent with write access
	// there. The proof file creation runs inside the caller's own sandbox,
	// which is exactly the capability being proven.
	spawnReq := func(writeProofToken string) any {
		req.WriteProofToken = writeProofToken
		return req
	}
	if err := DaemonRequestWithWriteProof(http.MethodPost, path, spawnReq, &resp, DaemonOpts{AskHuman: ask}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		// The spawn failed after we created a worktree for it. Remove the
		// now-orphaned worktree so a retry starts clean — except on a 504
		// conv-id-poll timeout: there the spawn subprocess DID launch and
		// the new CC may still be coming up inside the worktree, so
		// force-removing the dir would yank it out from under a
		// recovering session. Every other failure (guardrail rejection,
		// launch failure, bad arg) happens before any CC runs there, so
		// the worktree is a guaranteed orphan. A branch the daemon cut
		// for this worktree goes with it, so a retry cuts a fresh one
		// from --worktree-base instead of silently reusing a branch left
		// over from the failed attempt; a branch that already existed
		// survives the teardown.
		if createdWorktree != "" {
			if de, ok := err.(*DaemonError); ok && de.Status == http.StatusGatewayTimeout {
				fmt.Fprintf(stderr, "Note: kept the worktree %s — the session may still be coming up.\n",
					createdWorktree)
			} else {
				undoSpawnWorktree(stderr, createdWorktree, discardWorktree, "spawn", ask)
			}
		}
		return nil, MapDaemonErrorToRC(err)
	}
	fmt.Fprintf(stdout, "Spawned %s in group %q\n", shortAgentID(resp.AgentID, resp.ConvID), resp.Group)
	if resp.Label != "" {
		fmt.Fprintf(stdout, "  Label:   %s\n", resp.Label)
	}
	if resp.TmuxSession != "" {
		fmt.Fprintf(stdout, "  Tmux:    %s\n", resp.TmuxSession)
	}
	if resp.AttachCmd != "" {
		fmt.Fprintf(stdout, "  Attach:  %s\n", resp.AttachCmd)
	}
	// Report the task link honestly (TCL-568): only a daemon-verified binding
	// prints as a plain fact; a pending spawn's link is announced as deferred
	// so the caller never reads success into a write that hasn't happened yet.
	if resp.TaskRefURL != "" {
		if resp.TaskRefState == "bound" {
			fmt.Fprintf(stdout, "  Task:    %s\n", resp.TaskRefURL)
		} else {
			fmt.Fprintf(stdout, "  Task:    %s (pending — binds when the agent finishes registering)\n", resp.TaskRefURL)
		}
	}
	// Echo the resolved launch shape + provenance so a spawn that inherited a
	// default profile's harness/model (the TCL-304 incident) is visible at a
	// glance instead of needing `profiles default show` to reverse-engineer.
	if resp.Resolved != nil {
		printResolvedLaunch(stdout, resp.Resolved)
	}
	// Surface the worktree so the user can see where the agent landed
	// (or, in the monorepo case, where its code work belongs).
	if wt := strings.TrimSpace(p.Worktree); wt != "" {
		wtPath := req.WorktreePath
		if wtPath == "" {
			wtPath = req.Cwd
		}
		fmt.Fprintf(stdout, "  Worktree: %s (branch %s)\n", wtPath, wt)
	}
	return &resp, rcOK
}

// printResolvedLaunch prints the resolved harness/model/effort with provenance,
// aligned with the existing Label/Tmux/Attach lines. Harness always carries a
// value (the default harness at minimum); a model/effort left unpinned prints as
// "(harness default)" so the reader sees the harness decides, not a blank line.
func printResolvedLaunch(stdout io.Writer, rl *ResolvedLaunch) {
	if rl == nil {
		return
	}
	fmt.Fprintf(stdout, "  Harness: %s\n", formatResolvedField(rl.Harness))
	fmt.Fprintf(stdout, "  Model:   %s\n", formatResolvedField(rl.Model))
	fmt.Fprintf(stdout, "  Effort:  %s\n", formatResolvedField(rl.Effort))
	// Echoed only when something actually pinned it, so a default-off launch
	// prints exactly the three lines it printed before this field existed.
	if strings.TrimSpace(rl.SandboxImpl.Value) != "" {
		fmt.Fprintf(stdout, "  Sandbox impl: %s\n", formatResolvedField(rl.SandboxImpl))
	}
	if policy := rl.SandboxPolicy; policy != nil {
		profiles := make([]string, 0, len(policy.Applied))
		for _, applied := range policy.Applied {
			profiles = append(profiles, fmt.Sprintf("%s %q", applied.Scope, applied.Name))
		}
		chain := "no sandbox profiles"
		if len(profiles) > 0 {
			chain = strings.Join(profiles, " → ")
		}
		environment := "none"
		if len(policy.Environment) > 0 {
			environment = strings.Join(policy.Environment, ",")
		}
		fmt.Fprintf(stdout, "  Policy:  v%d %s (%d filesystem rules; env: %s)\n",
			policy.Version, chain, len(policy.Filesystem), environment)
	}
	for _, note := range rl.Notes {
		fmt.Fprintf(stdout, "  Note:    %s\n", note)
	}
	for _, info := range rl.Info {
		fmt.Fprintf(stdout, "  Info:    %s\n", info)
	}
	// Warnings print after the notes so the last thing on screen is the thing
	// most worth acting on, and under their own label so they are not read as
	// one more provenance footnote.
	for _, warning := range rl.Warnings {
		fmt.Fprintf(stdout, "  Warning: %s\n", warning)
	}
}

// formatResolvedField renders one resolved field as "value (source)", or just
// "(harness default)" when nothing pinned a value (an empty value only ever
// pairs with the harness-default tier — a profile that set the field would have
// produced a non-empty value).
func formatResolvedField(f ResolvedField) string {
	var rendered string
	if strings.TrimSpace(f.Value) == "" {
		rendered = "(" + ProvHarnessDefault + ")"
	} else {
		rendered = fmt.Sprintf("%s (%s)", f.Value, f.Source)
	}
	if f.Note != "" {
		rendered += " — " + f.Note
	}
	return rendered
}
