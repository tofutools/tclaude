package session

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/launchundo"
	"github.com/tofutools/tclaude/pkg/claude/common/managedinstructions"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/common"
)

type NewParams struct {
	ManagedInstructionsRoot string `long:"managed-instructions-root" optional:"true" help:"Internal: registered managed instruction launch directory"`
	// ManagedLaunch marks agentd's forked session wrapper. The daemon already
	// resolved profile precedence, so the child must use the exact passed shape.
	ManagedLaunch bool `long:"managed-launch" help:"Internal: launch parameters were resolved by agentd"`
	// DarwinRouteCapable is an explicit internal launch seam. It is never
	// inferred from the host environment or ordinary sandbox selection.
	DarwinRouteCapable     bool   `short:"Q" long:"darwin-route-capable" help:"Internal: launch exact Darwin route slots"`
	DarwinRouteAgentID     string `short:"I" long:"darwin-route-agent-id" optional:"true" help:"Internal: stable agent identity for Darwin route slots"`
	AllowUnenforcedSandbox bool   `long:"allow-unenforced-sandbox" help:"Internal: operator authorized launch when sandbox enforcement is unavailable"`
	SandboxContinuation    bool   `short:"-" long:"sandbox-continuation" help:"Internal: this launch continues an existing agent's recorded sandbox posture (reincarnation, no-copy clone)"`
	// SandboxChosenBy is the resolution tier that supplied --sandbox, as
	// resolved by the daemon spawn boundary ("explicit", `global default
	// profile "x"`, …). The badge attributes the launch's containment to it, so
	// an operator who never chose a sandbox can see which default profile did.
	// Internal: only the daemon has the tier stack to know. A direct
	// `session new --sandbox on` is attributed as explicit by runNew itself.
	SandboxChosenBy       string `short:"T" long:"sandbox-chosen-by" optional:"true" help:"Internal: which resolution tier chose --sandbox"`
	SandboxSnapshotPath   string `short:"U" long:"sandbox-snapshot-path" optional:"true" help:"Internal: private effective sandbox snapshot handoff"`
	SandboxSnapshotDigest string `short:"V" long:"sandbox-snapshot-digest" optional:"true" help:"Internal: expected effective sandbox snapshot digest"`
	ResourceCgroupDir     string `short:"W" long:"resource-cgroup-dir" optional:"true" help:"Internal: prepared Linux resource cgroup shared with a managed server"`
	Dir                   string `short:"C" long:"dir" optional:"true" help:"Directory to start session in (defaults to current directory)"`
	Cwd                   string `long:"cwd" optional:"true" help:"Working directory for the new session; dashboard/agent-spawn-compatible alias for --dir"`
	// CwdWriteProof is an internal daemon-to-session capability. The harness
	// command checks its marker only after tmux has established the pane's cwd
	// inode. Hidden from normal CLI help.
	CwdWriteProof string `long:"cwd-write-proof" optional:"true" help:"Internal: verify a daemon-issued cwd marker before launching the harness"`
	DirWriteProof string `short:"Z" long:"dir-write-proof" optional:"true" help:"Internal: verify daemon-issued repository-root markers before launching the harness"`
	// CodexGitCommonDir is the historical name for an internal, daemon-pinned
	// linked-worktree metadata result. The managed Codex profile and Claude
	// Code's per-session sandbox allowWrite overlay both consume it. Hidden from
	// normal CLI help.
	CodexGitCommonDir string `long:"codex-git-common-dir" optional:"true" help:"Internal: pinned Git common dir for repository sandbox grants"`
	// CodexGitCommonDirPinned disambiguates an intentionally empty daemon pin
	// from a direct launch that should derive the common dir from cwd.
	CodexGitCommonDirPinned bool `long:"codex-git-common-dir-pinned" help:"Internal: use the daemon-pinned Git common-dir result, including an empty result"`
	// GitWorktreeWriteDirs is the exact proof-pinned repository grant set. It
	// avoids re-deriving permission roots from a mutable path in the child.
	GitWorktreeWriteDirs       []string `short:"X" long:"git-worktree-write-dir" optional:"true" help:"Internal: proof-pinned repository sandbox write root"`
	GitWorktreeWriteDirsPinned bool     `short:"Y" long:"git-worktree-write-dirs-pinned" help:"Internal: use the daemon-pinned repository write roots, including an empty set"`
	Resume                     string   `long:"resume" short:"r" optional:"true" help:"Resume an existing conversation by ID, or an agent by name or agent ID"`
	ResumeLast                 bool     `long:"resume-last" help:"Resume the newest conversation in the current directory, skipping retired ones (Claude Code only)"`
	Global                     bool     `short:"g" help:"Search for conversation across all projects (with --resume)"`
	Label                      string   `long:"label" optional:"true" help:"Custom tmux label for a solo session. Keeps the launch solo (no directory auto-join, a notice says so); to join a group with a custom name use -n NAME instead"`
	Detached                   bool     `long:"detached" short:"d" help:"Start detached (don't attach to session)"`
	WaitForRateLimit           bool     `long:"wait-for-rate-limit" short:"w" help:"Wait for rate limit (5-hour and 7-day) to reset before starting session"`

	// Effort sets the chosen harness's reasoning effort. A fresh human launch
	// fills an omitted value from the global default spawn profile; if that is
	// also blank the harness receives no override. A non-empty value is
	// normalized and validated by the harness catalog in runNew.
	Effort string `long:"effort" optional:"true" help:"Reasoning effort (per-harness; Copilot also accepts none|minimal). Unset = global profile, then the harness default"`

	// Model picks the model for the chosen harness. A fresh human launch fills
	// an omitted value from the global default spawn profile; if that is also
	// blank the harness receives no override. A non-empty value is normalized
	// and validated by the harness catalog in runNew.
	ModelProxy string `long:"model-proxy" optional:"true" help:"Model gateway <name>@<trusted peer>, for Claude Code, Codex or Copilot"`
	// ClaudeSubscription is set by agentd for an agent that runs on a peer
	// operator's own Claude subscription; the token never passes through here.
	ClaudeSubscription bool   `long:"claude-subscription" optional:"true" hidden:"true" help:"Run Claude Code on the agent's peer-provided subscription token"`
	Model              string `long:"model" optional:"true" help:"Harness model or alias. Unset = global profile, then the harness default"`

	// Harness selects the coding tool this session runs. A fresh human launch
	// fills an omitted value from the global default spawn profile, or chooses an
	// installed harness when no profile exists (Claude preferred). "codex"
	// launches OpenAI Codex CLI in the tmux pane via
	// the codex Spawner. The chosen harness's ModelCatalog validates
	// --model/--effort and its Spawner builds the launch command, so the
	// rest of runNew stays harness-agnostic. Direct `--harness shell` keeps the
	// lightweight session path; agentd's managed form uses the registered shell
	// pseudo-harness so profiles and tclaude's sandbox still apply. See shell.go.
	Harness string `long:"harness" optional:"true" help:"Coding harness to launch: claude | codex | opencode | copilot | gemini | shell | pi. Unset = global profile, then an installed harness (claude preferred)"`

	// Shell is shorthand for --harness shell: it sets Harness to
	// ShellHarnessName in runNew before any harness resolution happens.
	// Mutually exclusive with an explicit --harness naming anything else.
	Shell bool `long:"shell" short:"s" help:"Start a plain interactive shell instead of a coding harness (shorthand for --harness shell)"`

	// Sandbox selects a harness's launch containment. On a direct `session new`
	// it is opt-in: unset emits no flag, so each harness uses its own config
	// (Codex's config.toml sandbox_mode; Claude Code's settings.json) — the human
	// running session new is the trust root, so tclaude doesn't override it. Pass
	// a value explicitly. Codex's modes are its native --sandbox enum; the
	// special value tclaude-agent (SandboxManagedProfile) is a shorthand
	// normalized to --permission-profile tclaude-agent. Claude Code has no
	// --sandbox flag, so its modes (inherit/on/off) are delivered as a
	// `claude --settings '<json>'` override; inherit normalizes to "" (omit). The
	// daemon spawn path (agentd / `agent spawn`) defaults Codex to the managed
	// profile (a spawned agent is the untrusted party) and Claude to inherit (no
	// override — its settings.json is the operator's chosen posture). See
	// JOH-192 / JOH-207.
	Sandbox string `long:"sandbox" optional:"true" help:"The HARNESS'S OWN sandbox setting (per-harness) — not who enforces containment, which is --sandbox-impl. Codex: tclaude-agent (managed profile = workspace-write + agentd socket) | workspace-write | read-only | danger-full-access. Claude Code: inherit | on (force OS sandbox on via --settings) | off. OpenCode: access-control | tclaude-layer | off. Copilot: inherit | off. Gemini CLI: inherit | off, and seatbelt on macOS. pi: off. Unset = global profile, then no override (each harness uses its own config)"`

	// SandboxImplementation selects who owns OS-level containment. The default
	// is the harness's historical behavior; tclaude-layer is a
	// whole-process wrapper (bubblewrap on Linux, Seatbelt on macOS). OpenCode's
	// historical behavior is a command filter, not confinement.
	SandboxImpl string `long:"sandbox-impl" optional:"true" help:"Sandbox implementation: harness-builtin (only for a harness with a real built-in OS sandbox) | tclaude-layer (tclaude’s built-in sandbox, harness OS sandbox off) | resource-only (Linux only; no access confinement, but the launch gets a per-launch cgroup: the profile's CPU/memory limits if it authored any, otherwise accounting and OOM attribution only; no bwrap or namespaces) | off (no OS sandbox). Unset = global profile, then historical harness behavior; for OpenCode that is a command filter, not confinement"`
	// sandboxImplExplicit preserves the decision/replay boundary after
	// applyRecordedLaunchPosture fills an omitted --sandbox-impl on resume.
	// It is internal state, not a CLI parameter.
	sandboxImplExplicit bool
	// copilotAPIDriveCarriedFrom names the conversation an omitted --copilot-api
	// was filled from by applyRecordedLaunchPosture, and is empty when this launch
	// asked for the drive itself. Internal state, not a CLI parameter.
	//
	// It exists because the two cases get different answers — an asserted drive is
	// refused, an inferred one is disclosed and dropped — and by the time runNew
	// reads CopilotAPI they are the same bool. Same reason as sandboxImplExplicit
	// above: the carryover is the only place the distinction still exists.
	copilotAPIDriveCarriedFrom string
	// recordedLaunchFlags names the flags applyRecordedLaunchPosture found a
	// recorded value for (applied, a recorded no-op, or dropped), so the default
	// profile only fills what the conversation never recorded. Internal state,
	// not a CLI parameter.
	recordedLaunchFlags map[string]bool
	// copilotAPIDriveDropped marks a launch that resolved the drive and then could
	// not honour it, so the posture write asserts nil instead of false and the
	// conversation's recorded choice survives. Internal state, not a CLI parameter.
	copilotAPIDriveDropped bool

	// AskUserQuestionTimeout is the per-session Claude Code AskUserQuestion
	// idle-timeout override (never|60s|5m|10m), delivered via `--settings`
	// alongside the sandbox block. inherit/unset omits it, so the agent uses the
	// operator's own settings.json value. A Claude-Code-only knob; a value for a
	// harness with no AskUserQuestion dialog (Codex) errors.
	AskUserQuestionTimeout string `long:"ask-user-question-timeout" optional:"true" help:"Claude Code AskUserQuestion idle-timeout override: inherit (use settings.json as-is) | never (wait for a human) | 60s | 5m | 10m (auto-continue with the default answer after the interval — keeps an unattended agent moving). Unset = inherit. Not applicable to Codex"`

	// PermissionProfile selects a tclaude-managed Codex permission profile to
	// run under, emitted as `codex -p <name>`. It is how the daemon keeps a
	// sandboxed Codex agent able to reach the agentd socket (JOH-207): the
	// daemon spawn path passes it (the tclaude-agent profile) in place of
	// --sandbox workspace-write, because Codex ignores permission profiles when
	// a --sandbox is present and only a profile can allowlist the socket. It is
	// mutually exclusive with --sandbox; the managed profile name is ensured on
	// disk before launch. Not applicable to Claude Code. Rarely set by hand.
	PermissionProfile string `long:"permission-profile" optional:"true" help:"Codex permission profile to run under (codex -p <name>); mutually exclusive with --sandbox. Not applicable to claude"`

	// Approval selects a harness's launch-time approval/permission posture.
	// Codex takes an --ask-for-approval policy; Claude Code has no approval flag
	// — its posture is the --permission-mode the spawner emits — but the value
	// rides through this same field (validated per-harness by claudeApproval /
	// codexApproval). On a direct `session new` it is opt-in: unset emits no flag,
	// so each harness uses its own config (the human running session new is the
	// trust root and can answer prompts — tclaude doesn't force a posture). The
	// daemon spawn path defaults it to each harness's safe value (Codex: never,
	// so a detached pane can't deadlock; Claude: inherit, no override). See
	// JOH-200.
	Approval string `long:"ask-for-approval" optional:"true" help:"Launch approval/permission posture (per-harness). Codex policy: untrusted|on-failure|on-request|never. Claude Code permission mode: inherit|plan|acceptEdits|default|auto|dontAsk|bypassPermissions. Gemini CLI approval mode: yolo|auto_edit|default|plan|inherit. pi: auto|ask|read-only. OpenCode: deny|ask|allow-tools. Copilot: inherit|allow-tools|yolo. Unset = global profile, then no override (each harness uses its own config)"`

	// ToolGovernance controls OpenCode's homogeneous built-in tool block. A
	// direct session launch validates but does not default it; daemon launches
	// resolve blank to allow and pass the concrete value through this flag.
	ToolGovernance string `long:"tools" optional:"true" help:"OpenCode tool governance: allow | ask | deny for bash/glob/grep/lsp/skill, with other tools and sub-agents (task) off; or all, which allows every OpenCode tool, sub-agents included, with optional per-tool entries all,question=deny,plan=deny,env=allow (.env reads are denied under all unless env=allow). all needs sandbox-impl tclaude-layer or off and approval deny or allow-tools. Unset = no override for a direct launch. Not applicable to claude/codex"`

	// AutoReview opts into the harness's guardian subagent (Codex's `-c
	// approvals_reviewer=auto_review`), which auto-decides approval prompts in
	// your place. Off by default (you review). Gated on the harness having an
	// approvals subsystem (Codex); set for claude is an error. Same per-spawn
	// opt-in on the direct `session new` path as on the daemon path —
	// experimental/undocumented upstream. See JOH-200 part 2.
	AutoReview bool `long:"auto-review" help:"EXPERIMENTAL: route Codex approval prompts to the guardian subagent (auto-decides in your place) instead of asking you. Off by default. Not applicable to claude"`

	// TrustDir opts into pre-trusting the launch cwd, so a detached pane
	// doesn't freeze on the harness's "do you trust this folder?" dialog
	// (JOH-205 for Codex, JOH-369 for Claude Code, TCL-973 for Copilot). Each
	// harness records trust in its own config file, written BEFORE launch since
	// none of them exposes a per-invocation trust flag:
	//
	//   codex   → [projects."<cwd>"] trust_level = "trusted" in ~/.codex/config.toml
	//   claude  → projects.<cwd>.hasTrustDialogAccepted = true in ~/.claude/.claude.json
	//   copilot → <cwd> appended to trustedFolders in $COPILOT_HOME/config.json
	//
	// OFF by default and NEVER auto-defaulted on this path: editing a config
	// tclaude does not own is a side effect the user must explicitly request
	// (dashboard checkbox / this flag). Rejected for a harness with no
	// dir-trust dialog. The write is atomic + idempotent
	// (harness.EnsureDirTrustedForLaunch).
	TrustDir bool `long:"trust-dir" help:"Pre-trust the launch directory so a detached pane doesn't freeze on the harness's trust-folder dialog: codex gets [projects.\"<cwd>\"] trust_level=\"trusted\" in ~/.codex/config.toml, claude gets projects.<cwd>.hasTrustDialogAccepted=true in ~/.claude/.claude.json, copilot gets the directory appended to trustedFolders in $COPILOT_HOME/config.json, and gemini and pi record it in their own trust stores. Off by default; edits that harness's config, so opt-in only"`

	// RemoteControl arms Claude Code's built-in Remote Access at launch
	// (`claude --remote-control`), so the session is reachable from
	// claude.ai/code + the Claude mobile app from its first turn (JOH-258). Off
	// by default. Gated on the harness having Remote Access (Claude Code); set
	// for Codex is an error. The daemon spawn path sets it from the dashboard /
	// `agent spawn` opt-in; a direct human `session new` may set it too. Needs
	// the operator logged into claude.ai (OAuth) for the session to actually
	// pair — outside tclaude's control.
	RemoteControl bool `long:"remote-control" help:"Start with Claude Code Remote Access ON (claude --remote-control), so the session is reachable from the Claude app. Off by default. Requires a claude.ai login to pair. Not applicable to codex"`

	// AutoMemory opts back INTO Claude Code's auto-memory system, which
	// tclaude disables by default: agents sharing a repo share one
	// per-project memory store and cross-pollute each other's notes. Off by
	// default, so the launch injects CLAUDE_CODE_DISABLE_AUTO_MEMORY=1;
	// --auto-memory sets it to "0" (Claude Code's documented force-enable)
	// instead. The daemon spawn path resolves the profile's tri-state into
	// this definite bool; a direct human `session new` may set it too.
	AutoMemory bool `long:"auto-memory" help:"Keep Claude Code's built-in auto memory ON for this session. Off by default for every Claude Code session tclaude launches (it injects CLAUDE_CODE_DISABLE_AUTO_MEMORY=1), because agents sharing a checkout cross-pollute one project memory store. Does not affect CLAUDE.md. Not applicable to codex"`

	// PeerMessaging opts back INTO Claude Code's own cross-session messaging
	// mesh, which tclaude closes by default: it is a second, unmanaged
	// coordination channel with none of the group, permission or audit
	// properties tclaude's own agent messaging has. Off by default, so the
	// launch injects crossSessionInbound=refuse, isolatePeerMachines=true and a
	// ListAgents deny; --peer-messaging injects nothing instead and leaves
	// Claude Code's own defaults in charge. In-harness subagent messaging is
	// unaffected either way. The daemon spawn path resolves the profile's
	// tri-state into this definite bool; a direct human `session new` may set it
	// too. See harness/peer_messaging.go.
	PeerMessaging bool `long:"peer-messaging" help:"Keep Claude Code's built-in cross-session messaging ON for this session. Off by default for every Claude Code session tclaude launches (it refuses inbound peer messages, denies the ListAgents tool and requires approval for cross-machine sends), because it is a second coordination channel outside tclaude's groups, permissions and audit trail. Use 'tclaude agent send' instead. Does not affect in-harness subagents. Not applicable to codex"`

	// ContextFeatures trims Claude Code's startup context for this session —
	// bundled skills, unused tool schemas, system-prompt blocks (TCL-597). Unset
	// changes nothing, so the session keeps whatever the operator's own
	// environment and settings give it. The daemon spawn path forwards the
	// resolved profile/spawn map here; a direct human `session new` may set it
	// too. Claude-Code-only; a value for a harness without these switches is an
	// error rather than a silent drop.
	ContextFeatures string `long:"context-features" optional:"true" help:"Trim Claude Code startup context for this session: comma-separated <feature>[=on|off] (a bare feature means off). E.g. bundled-skills,artifact,workflows. Unset changes nothing. Run 'tclaude session new --help-context-features' for the catalog. Not applicable to codex"`

	// HelpContextFeatures prints the catalog and exits. A dedicated flag rather
	// than a giant help string keeps `--help` readable while still making the
	// slugs discoverable from the CLI (the dashboard shows the same catalog with
	// its descriptions inline).
	HelpContextFeatures bool `long:"help-context-features" help:"List the startup-context features --context-features can trim, then exit"`

	// AutoCompactWindow pins the context capacity Claude Code's auto-compaction
	// calculations run against (CLAUDE_CODE_AUTO_COMPACT_WINDOW). Unset leaves the
	// model's own default threshold in charge. The point is long-lived agents: on
	// a 1M model Claude Code compacts near the very top of the window, well after
	// answer quality has started to slide, so pinning it lower keeps the agent
	// sharp without anyone babysitting `/compact`. The daemon spawn path forwards
	// the resolved profile/spawn value here; a direct human `session new` may set
	// it too. Claude-Code-only; a value for a harness without the knob is an error
	// rather than a silent drop.
	AutoCompactWindow string `long:"auto-compact-window" optional:"true" help:"Context capacity in tokens for Claude Code's auto-compaction (CLAUDE_CODE_AUTO_COMPACT_WINDOW). Accepts 450000, 450k or 0.5M. Pin it below a 1M model's real window to compact earlier, while the agent is still sharp. Capped at the model's actual window; also decouples the compaction threshold from the status line's percentage. Unset uses the model default. Not applicable to codex"`

	// ContextWindowMax is tclaude's configured Copilot context-meter
	// denominator. Copilot does not report a cap or accept this as a harness
	// flag, so the value is persisted as launch intent and kept separate from
	// the observed context_window_size snapshot.
	ContextWindowMax int64 `long:"context-window-max" optional:"true" help:"Configured Copilot context cap in tokens for the context meter. Unset uses the observed model's static assumption. Copilot only; not a harness-reported value"`

	// FastMode selects Codex's request service tier for this launch. "inherit"
	// deliberately emits no override, preserving the operator's config.toml.
	FastMode string `long:"fast-mode" optional:"true" help:"Codex request speed: inherit (use config.toml) | on (fast, higher credit cost) | off (standard tier). Codex only"`

	// CopilotAPI selects the API-backed Copilot mode. Here it pins which mode
	// the conversation takes and, with CopilotAPIPort, makes the pane bind the
	// embedded server.
	//
	// The channel itself is established by agentd, for launches agentd started, so
	// this flag is only serviceable on an agentd-resolved launch. A direct
	// `session new --copilot-api` used to get the bound pane WITHOUT a daemon-held
	// connection to it — an unauthenticated loopback endpoint nothing would dial,
	// which is strictly worse than the send-keys default. It is now refused;
	// resolveCopilotAPIDriveForLaunch owns that decision and the reasoning.
	CopilotAPI bool `long:"copilot-api" help:"EXPERIMENTAL: launch this Copilot agent with its embedded JSON-RPC server (copilot --ui-server) so tclaude agentd can drive it over typed calls instead of tmux send-keys. Refuses unless the launch dir is already trusted (or --trust-dir). The endpoint is unauthenticated and loopback-bound. Off by default. Copilot only"`

	// CopilotAPIPort is the loopback port the embedded JSON-RPC server binds.
	//
	// Supplied by agentd, which allocates it so the consumer holds the number
	// before the process exists (TCL-1054). Its presence together with
	// --managed-launch is what tells this process that the daemon which will
	// actually drive the endpoint is on the other end; see
	// copilotAPIDriveIsDaemonOwned.
	//
	// Unset with --copilot-api used to allocate one here, so that a launch driven
	// from a terminal "still got a working pane". The pane worked and the drive did
	// not: nothing in agentd was waiting on that number, because nothing in agentd
	// started the launch. That allocation is gone (TCL-1084).
	CopilotAPIPort int `long:"copilot-api-port" optional:"true" help:"Loopback port for the API-backed Copilot agent's embedded JSON-RPC server. Chosen by tclaude agentd, which allocates it before forking this process; a launch that is not agentd's cannot get the drive at all, so there is nothing for it to pass here. Requires --copilot-api"`

	// CodexAppServer is the deliberately opt-in Codex API drive. The private
	// runtime paths are minted by agentd; accepting the public toggle without
	// those paths would produce a remote TUI nobody owns, so it is refused.
	CodexAppServer bool `long:"codex-app-server" help:"EXPERIMENTAL: launch Codex against a private tclaude-owned app-server and bind agentd to the TUI-created thread. Off by default; Codex 0.147.0 or newer"`
	// CodexAppServerSpecified preserves Cobra's omitted-vs-explicit-false bit
	// while a top-level launch is adapted to the group spawn boundary. It is
	// runtime adapter state, not another CLI/config parameter.
	CodexAppServerSpecified    bool   `boa:"ignore" json:"-"`
	CodexAppServerSocket       string `long:"codex-app-server-socket" optional:"true" help:"Internal: agentd-minted private Codex app-server Unix socket"`
	CodexAppServerURL          string `long:"codex-app-server-url" optional:"true" help:"Internal: agentd-minted authenticated Codex app-server loopback endpoint"`
	CodexAppServerTokenSHA256  string `long:"codex-app-server-token-sha256" optional:"true" help:"Internal: digest of the per-generation Codex app-server capability"`
	CodexAppServerTokenHandoff string `long:"codex-app-server-token-handoff" optional:"true" help:"Internal: one-shot private Codex TUI capability handoff"`
	CodexAppServerPIDFile      string `long:"codex-app-server-pid-file" optional:"true" help:"Internal: agentd-minted Codex app-server pid file"`
	CodexAppServerLogFile      string `long:"codex-app-server-log-file" optional:"true" help:"Internal: agentd-minted Codex app-server log file"`
	CodexAppServerGeneration   string `long:"codex-app-server-generation" optional:"true" help:"Internal: agentd-minted Codex app-server runtime generation"`

	// SendKeys is the deliberate override for the Copilot API drive refusal,
	// spelled exactly as `tclaude conv resume --send-keys` (TCL-1076) so an
	// operator who learns the escape on one surface is not walled out on the
	// other. It suppresses only the refusal for a CARRIED drive on a live managed
	// agent; the launch is on send-keys either way.
	SendKeys bool `long:"send-keys" help:"Relaunch a conversation that chose the Copilot API drive on tmux send-keys instead of being refused. The API channel exists only under tclaude agentd, so this launch is on send-keys with or without the flag; without it, a relaunch of a LIVE MANAGED agent is refused rather than started, because its mail holds rather than falling back to keystrokes"`

	// --join-group makes the new session auto-join an existing agent group
	// the moment its conv-id materialises. Routed through the daemon's
	// `groups.members.spawn` orchestration; not compatible with --resume / --label.
	JoinGroup string `long:"join-group" optional:"true" help:"Spawn the session and add it to an existing agent group (shorthand for agent spawn + foreground attach)"`
	Name      string `long:"name" optional:"true" help:"Display name for the session (claude --name; becomes its conversation title). With --join-group it is the new agent's name"`
	Role      string `long:"role" optional:"true" help:"Role tag for the new member in --join-group (e.g. 'tech-lead')"`
	Descr     string `long:"descr" optional:"true" help:"Description of the new member's purpose in --join-group"`

	// Automatic group discovery is resolved from explicit CLI flags over the
	// config file in RunNewFromCommand. Programmatic RunNew callers retain their
	// historical solo-session behavior because these zero values stay false.
	AutoJoinGroup         bool `long:"auto-join-group" default:"true" help:"Automatically join an active group whose configured default directory matches the normalized launch directory (default: config session.auto_join_group, see 'tclaude config get', then true). Solo-only flags (--label, --session-id, --permission-profile, --wait-for-rate-limit, --global, --send-keys) keep a launch solo; passing this flag explicitly with one is an error. Pass --auto-join-group=false to stay solo"`
	AutoJoinOrCreateGroup bool `long:"auto-join-or-create-group" help:"When directory auto-join finds no group, create one for the normalized launch directory and join it (default: config session.auto_join_or_create_group, then false)"`
	NoDaemon              bool `long:"no-daemon" help:"Start a solo session without agentd-backed group discovery or agent features; useful for non-interactive launches when agentd is unavailable"`

	// Group-spawn parity fields. They apply when --join-group or directory
	// discovery selects/creates a group and are then handed to agent.RunSpawn,
	// the exact CLI path used by `tclaude agent spawn`.
	Profile             string `long:"profile" optional:"true" help:"Spawn profile to apply when this launch joins a group; explicit launch flags override it"`
	SandboxProfile      string `long:"sandbox-profile" optional:"true" help:"Human-only filesystem/environment sandbox profile for a group-joined launch"`
	OmitSandboxProfiles bool   `long:"omit-sandbox-profiles" help:"Human-only: omit global, group, and explicit tclaude sandbox profiles for a group-joined launch"`
	Worktree            string `long:"worktree" optional:"true" help:"Create or reuse this git worktree branch for a group-joined launch"`
	WorktreeBase        string `long:"worktree-base" optional:"true" help:"Base branch for a newly-created --worktree"`
	WorktreeRepo        string `long:"worktree-repo" optional:"true" help:"Repository in which to create --worktree when it differs from the launch directory"`
	InitialMessage      string `long:"initial-message" optional:"true" help:"Task brief for a group-joined agent, delivered with its welcome (the dashboard spawn dialog's initial message). Refused on a launch that stays solo"`
	File                string `long:"file" optional:"true" help:"Read --initial-message from this file ('-' reads stdin)"`
	ReplyTo             string `long:"reply-to" optional:"true" help:"Recipient for replies to the group-joined agent's startup brief"`
	SpawnTimeout        string `long:"timeout" optional:"true" help:"How long to wait for a group-joined agent to register (default 30s, max 5m)"`
	AskHuman            string `long:"ask-human" optional:"true" help:"On group create/spawn permission denial, ask the human with this timeout"`
	AutoFocus           bool   `long:"auto-focus" help:"Open another terminal focused on the group-joined agent after spawn"`
	NoGroupContext      bool   `long:"no-group-context" help:"Do not include the selected group's shared startup context"`
	Task                string `long:"task" optional:"true" help:"Task-reference URL for the group-joined agent"`
	TaskLabel           string `long:"task-label" optional:"true" help:"Display label overriding the value derived from --task"`
	Owner               bool   `long:"owner" help:"Make the group-joined agent a group owner (requires groups.owners.manage authority)"`
	NoOwner             bool   `long:"no-owner" help:"Do not make the group-joined agent an owner even if a profile would"`

	// InitialPrompt is a first-turn prompt the harness submits itself at
	// launch (its own positional [PROMPT] arg) — used for a harness whose
	// conv-id is only knowable after the first turn (Codex). The daemon
	// spawn path sets it so a freshly-spawned Codex takes a turn on its own
	// and its conv-id materialises without a human typing the first message
	// (JOH-205); a direct human `session new` leaves it empty and types
	// their own first message. Ignored by a harness that reports its conv-id
	// at launch (Claude Code) and on a --resume. See codexSpawner.BuildCommand.
	InitialPrompt string `long:"initial-prompt" optional:"true" help:"First-turn prompt the harness submits itself at launch (its positional [prompt]). When this launch joins a group it becomes the agent's startup brief instead, like -i (pass only one of the two). Daemon spawns set it automatically (Claude Code: the agent welcome; Codex: a conv-id seed)"`

	// SessionID pins the conversation id for a FRESH launch of a harness that
	// accepts a preset id — Claude Code (`claude --session-id <uuid>`) and
	// GitHub Copilot CLI (`copilot --session-id <uuid>`) — so the conv-id is
	// known before the pane starts. The daemon's launch-enrollment spawn path
	// sets it so the agent can be enrolled + named via launch args instead of
	// post-connect tmux injection. A direct human launch may set it to choose a
	// specific id. Mutually exclusive with --resume; both harnesses require a
	// valid UUID. (OpenCode also accepts one, but only as an internal
	// agentd-minted `ses_` id on a managed launch.)
	SessionID string `long:"session-id" optional:"true" help:"Use a specific conversation id (UUID) for a fresh Claude Code, Copilot, Gemini CLI or pi session (--session-id). OpenCode ids come from agentd, and Codex takes none. Mutually exclusive with --resume"`

	// RouteHelper* are an internal daemon-to-session handoff. The daemon mints
	// the launch generation and opaque credential before the pane starts; callers
	// provide those with the pre-enrolled identity and explicit route groups.
	RouteHelperAgentID                     string  `long:"route-helper-agent-id" optional:"true" help:"Internal: stable identity for the Linux group-route helper"`
	RouteHelperConvID                      string  `long:"route-helper-conv-id" optional:"true" help:"Internal: conversation identity for the Linux group-route helper"`
	RouteHelperLaunchGeneration            string  `long:"route-helper-launch-generation" optional:"true" help:"Internal: launch generation for the Linux group-route helper"`
	RouteHelperCredentialHandoffSocketPath string  `long:"route-helper-credential-handoff-socket" optional:"true" help:"Internal: one-shot credential FD handoff socket for the Linux group-route helper"`
	RouteHelperGroupIDs                    []int64 `long:"route-helper-group-id" optional:"true" help:"Internal: explicit route-enabled group for the Linux group-route helper"`
	RouteHelperProxyOnly                   bool    `long:"route-helper-proxy-only" optional:"true" help:"Internal: carry route authority to the Darwin filtering proxy without a namespace helper"`
}

func NewCmd() *cobra.Command {
	cmd := boa.CmdT[NewParams]{
		Use:         "new",
		Short:       "Start a new coding session",
		Long:        "Start a new coding-harness session in tmux. Attaches by default (Ctrl+B D to detach).",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(params *NewParams, cmd *cobra.Command, args []string) {
			if err := RunNewFromCommand(params, cmd); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		},
	}.ToCobra()

	// Allow arbitrary args so post-'--' args pass through to claude without cobra rejecting them.
	cmd.Args = cobra.ArbitraryArgs
	HideInternalFlags(cmd)

	// Register completion for --resume flag
	_ = cmd.RegisterFlagCompletionFunc("resume", func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		// Check if -g flag is set (params may not be populated during completion)
		global, _ := cmd.Flags().GetBool("global")
		return clcommon.GetConversationCompletions(global), cobra.ShellCompDirectiveKeepOrder | cobra.ShellCompDirectiveNoFileComp
	})

	RegisterJoinGroupCompletion(cmd)

	return cmd
}

// RegisterJoinGroupCompletion wires `--join-group` to suggest existing
// agent group names. Reads SQLite directly — completions fire on every
// <tab> keystroke, so they bypass the daemon (same convention as
// `tclaude agent groups …` completions). Exported so the top-level
// `tclaude` cobra cmd in pkg/claude/claude.go can register it too.
func RegisterJoinGroupCompletion(cmd *cobra.Command) {
	_ = cmd.RegisterFlagCompletionFunc("join-group", func(_ *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		groups, err := db.ListAgentGroups()
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		out := make([]string, 0, len(groups))
		for _, g := range groups {
			if strings.HasPrefix(g.Name, toComplete) {
				out = append(out, g.Name)
			}
		}
		return out, cobra.ShellCompDirectiveNoFileComp
	})
}

// sandboxDescr returns the human-facing launch-containment descriptor for the
// session row / error messages: the permission-profile name when one is set
// (the JOH-207 path — `codex -p <name>`), otherwise the --sandbox mode (which
// may itself be ""). The two are mutually exclusive upstream, so at most one
// is non-empty.
func sandboxDescr(harnessBuiltinMode, permissionProfile string) string {
	if permissionProfile != "" {
		return permissionProfile
	}
	return harnessBuiltinMode
}

// validateReplacedPermissionProfile applies the --permission-profile rules to a
// launch whose profile the single-wall tclaude-layer implementation is about to
// discard.
//
// It exists because those rules used to run LATER than the discard and
// therefore judged the operator's flag by accident. Now that the discard
// happens up front — so the Codex managed-profile normalization and the
// mutual-exclusion check never see a profile this launch does not use — the
// judgement has to be made deliberately, or `--sandbox-impl tclaude-layer
// --permission-profile <typo>` would launch silently where every other
// implementation refuses it.
//
// requestedHarnessBuiltinMode is the mode the operator asked for, BEFORE the
// implementation forced its own: the rules are about what they typed, and
// judging the forced mode would report a conflict they could not have caused.
// The checks and their order mirror the ones further down runNew, which remain
// the only ones a non-tclaude-layer launch runs.
func validateReplacedPermissionProfile(
	h *harness.Harness, requestedHarnessBuiltinMode, permissionProfile string,
) error {
	requestedHarnessBuiltinMode = strings.TrimSpace(requestedHarnessBuiltinMode)
	// The managed-profile pseudo-mode selects the tclaude-agent profile, so a
	// DIFFERENT explicit profile beside it is a conflict the operator must fix
	// rather than have silently resolved.
	if h.Name == harness.CodexName && requestedHarnessBuiltinMode == harness.SandboxManagedProfile {
		if up := strings.TrimSpace(permissionProfile); up != "" && up != harness.CodexAgentProfile {
			return fmt.Errorf("--sandbox %s selects the managed %s profile and conflicts with --permission-profile %s",
				harness.SandboxManagedProfile, harness.CodexAgentProfile, up)
		}
		// It resolves TO the managed profile, and the mode is consumed doing so
		// — which is why the pair below is not a mutual-exclusion violation.
		return nil
	}
	profile, err := harness.ValidateCodexProfileName(permissionProfile)
	if err != nil {
		return err
	}
	if profile == "" {
		return nil
	}
	if requestedHarnessBuiltinMode != "" {
		return fmt.Errorf("--permission-profile and --sandbox are mutually exclusive: " +
			"Codex ignores a permission profile when --sandbox is set")
	}
	if h.Name != harness.CodexName {
		return fmt.Errorf("--permission-profile is a Codex launch option; harness %q has no permission profiles", h.Name)
	}
	return nil
}

// JoinGroupHandler implements `--join-group`. Set by the agent package's
// init() to avoid a session→agent import cycle (agent already depends on
// session for AttachToSession). When nil, --join-group falls back to a
// clear error.
var JoinGroupHandler func(*NewParams) error

// ErrNoAutomaticGroupMatch tells the session path that directory discovery was
// enabled but found nothing and auto-create was off. The launch then continues
// as the historical solo session. Explicit --join-group never returns it.
var ErrNoAutomaticGroupMatch = errors.New("no automatic group match")

func runNew(params *NewParams) error {
	timing := config.StartupTiming("session_new", "label", params.Label, "harness", params.Harness)
	defer timing("return")
	if params.HelpContextFeatures {
		harness.PrintContextFeatureCatalog(os.Stdout)
		return nil
	}
	var effectiveSandbox *sandboxpolicy.Snapshot
	params.SandboxSnapshotPath = strings.TrimSpace(params.SandboxSnapshotPath)
	params.SandboxSnapshotDigest = strings.TrimSpace(params.SandboxSnapshotDigest)
	if (params.SandboxSnapshotPath == "") != (params.SandboxSnapshotDigest == "") {
		return fmt.Errorf("internal sandbox snapshot path and digest must be supplied together")
	}
	if params.SandboxSnapshotPath != "" {
		defer func() { _ = os.Remove(params.SandboxSnapshotPath) }()
		snapshot, err := sandboxpolicy.ReadSnapshotFile(params.SandboxSnapshotPath, params.SandboxSnapshotDigest)
		if err != nil {
			return err
		}
		effectiveSandbox = &snapshot
	}
	// Freeze which filesystem rules are concrete once for this launch. Keep the
	// original snapshot for persistence; launchSandbox is the consistent set
	// used by capability checks, write proofs, and the harness handoff.
	launchSandbox, err := SandboxSnapshotForLaunch(effectiveSandbox)
	if err != nil {
		return err
	}
	// --shell preserves the lightweight standalone shell-session path. An
	// explicit --harness shell is the registered pseudo-harness and therefore
	// uses the managed agent/profile/sandbox pipeline below.
	params.Harness = strings.TrimSpace(params.Harness)
	if params.Shell {
		if params.Harness != "" && params.Harness != ShellHarnessName {
			return fmt.Errorf("--shell conflicts with --harness %s", params.Harness)
		}
		return runNewShell(params)
	}
	if params.Harness == ShellHarnessName && !params.ManagedLaunch {
		return runNewShell(params)
	}

	opts, err := resolveNewLaunch(params, launchSandbox)
	if err != nil {
		return err
	}
	h, effort, model := opts.h, opts.effort, opts.model
	harnessBuiltinMode, effectiveHarnessBuiltinMode := opts.harnessBuiltinMode, opts.effectiveHarnessBuiltinMode
	sandboxImplementation, outerLayer, unconfined := opts.sandboxImplementation, opts.outerLayer, opts.unconfined
	networkAccess := opts.networkAccess
	approvalPolicy, recordedApprovalPolicy := opts.approvalPolicy, opts.recordedApprovalPolicy
	autoReview, remoteControl, autoMemory := opts.autoReview, opts.remoteControl, opts.autoMemory
	askTimeout, contextFeatures, autoCompactWindow := opts.askTimeout, opts.contextFeatures, opts.autoCompactWindow

	if params.JoinGroup != "" {
		if JoinGroupHandler == nil {
			return fmt.Errorf("--join-group is not wired up in this binary")
		}
		return JoinGroupHandler(params)
	}
	extraArgs := clcommon.ExtractClaudeExtraArgs()
	if params.ManagedInstructionsRoot != "" {
		if err := managedinstructions.VerifyLaunchRoot(params.ManagedInstructionsRoot); err != nil {
			return fmt.Errorf("managed instructions: %w", err)
		}
		if h.Name == harness.DefaultName && managedinstructions.HasNativeSkills(params.ManagedInstructionsRoot) && managedinstructions.ClaudeNativeAvailable() {
			extraArgs = append(extraArgs, "--plugin-dir", params.ManagedInstructionsRoot)
		}
	}

	// Pass-through mode: --help, --version etc. — run the harness binary
	// directly, no tmux.
	//
	// The posture audit below deliberately does NOT gate this branch: running
	// `copilot --help` through tclaude starts no recorded session, so there is
	// no recorded posture for a pass-through arg to contradict.
	if clcommon.ShouldRunClaudeDirect(extraArgs) {
		binary := h.Spawn.Binary()
		if h.Name == harness.OpenCodeName {
			if binary, err = harness.OpenCodeExecutable(); err != nil {
				return err
			}
		}
		cmd := exec.Command(binary, extraArgs...)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}

	// A pass-through arg lands on the same command line as the harness's
	// rendered permission flags, so one that moves the posture would make the
	// running pane broader than the row that records it — and approval lineage
	// and relaunch both reason from that row. Refuse rather than filter, so an
	// operator never believes a flag took effect that tclaude silently dropped.
	// Placed after the pass-through branch above, which starts no session and
	// therefore has no recorded posture to contradict.
	if params.ModelProxy != "" {
		if h.Name == harness.CodexName {
			if flag, ok := providerChangingArg(extraArgs, map[string]bool{"-c": true, "--config": true, "-p": true, "--profile": true, "--oss": false, "--local-provider": true, "--remote": false}); ok {
				return fmt.Errorf("model gateway cannot combine with Codex provider override %s", flag)
			}
		}
		if err := validateModelProxyExtraArgs(extraArgs); err != nil {
			return err
		}
	}
	if params.ClaudeSubscription {
		if err := validateModelProxyExtraArgs(extraArgs); err != nil {
			return errors.New("a Claude subscription launch cannot combine with pass-through --settings")
		}
	}
	if err := harness.ValidateLaunchExtraArgs(h, extraArgs); err != nil {
		return err
	}

	cwd, err := preflightNewLaunch(params.Dir)
	if err != nil {
		return err
	}
	ctx, stopLaunchContext := newLaunchContext()
	defer stopLaunchContext()
	id, err := resolveLaunchIdentity(h, params, cwd)
	if err != nil {
		return err
	}
	timing("validated", "session_id", id.sessionID)
	releaseLaunchIdentity, err := claimLaunchIdentity(ctx, params, &id)
	if err != nil {
		return err
	}
	defer releaseLaunchIdentity()
	fullConvID, sessionID, tmuxSession := id.fullConvID, id.sessionID, id.tmuxSession
	cwd = id.cwd

	// TCLAUDE_SESSION_ID is the stable session-row/routing key consumed by
	// hooks, statusline, focus, and resume flows. It is caller-controlled
	// compatibility state, not proof of daemon caller identity; agentd
	// authorizes callers from Unix-socket peer credentials and recorded PIDs.
	// Build the harness command with all environment variables forwarded.
	exitGeneration := newExitLaunchGeneration(sessionID, tmuxSession)
	// networkFiltered: the launch runs under a filtered network, so a harness
	// that can start traffic of its own outside the model route is told not
	// to (harness.SpawnSpec.NetworkFiltered).
	networkFiltered := false
	routeHelper, err := resolveLaunchRouteHelper(params, h.Name, outerLayer)
	if err != nil {
		return err
	}
	if routeHelper != nil {
		exitGeneration = routeHelper.LaunchGeneration
	}
	additionalEnv, err := baseLaunchEnv(params, h, harnessBuiltinMode, effectiveSandbox, outerLayer, sessionID, exitGeneration, cwd)
	if err != nil {
		return err
	}
	launchPermissionProfile := params.PermissionProfile
	launchProfilePath := ""
	var codexAppServerProfileOverrides []string
	var launchCodexSplitCapability *harness.CodexSplitPolicyCapability
	// launchUndo holds what this launch creates and must undo on any
	// return before the final commit: the Codex launch-profile file and its
	// native generation, the darwin route claim, the session row and the
	// private attachment dir. Each step registers once its state exists; the
	// commit at the end hands them all to the live session. Rollbacks with
	// another lifetime stay as their own defers: the snapshot file and the
	// darwin slot reservation go on every return, and the resource cgroup
	// passes to the pane when it launches.
	var launchUndo launchundo.Undo
	defer launchUndo.UnwindUnlessCommitted()
	// ordinaryNativeGeneration is the native permission generation this
	// launch registered, activated only at the final commit.
	ordinaryNativeGeneration := ""
	layerNetwork, err := resolveTclaudeLayerNetwork(h, sandboxImplementation, outerLayer, networkAccess, launchSandbox)
	if err != nil {
		return err
	}
	tclaudeLayerPosture, tclaudeLayerEngine := layerNetwork.posture, layerNetwork.engine
	tclaudeLayerPreserveCallerIdentity := layerNetwork.preserveCallerIdentity
	filteredNetworkProbe, tclaudeLayerRoot := layerNetwork.probe, layerNetwork.root
	// Pin managed Codex sessions to agentd's canonical state-free socket. That
	// socket lives outside the profile's denied ~/.tclaude private-state tree.
	if err := ApplyAgentSocketEnv(
		h.Name,
		params.Sandbox,
		params.PermissionProfile,
		outerLayer && tclaudeLayerRoot == sandboxpolicy.RootConstructed,
		additionalEnv,
	); err != nil {
		return err
	}
	// The launch-posture environment (state-root config dir, resume chooser,
	// auto memory, agent view, startup-context trims, auto-compaction window) is
	// shared with resume, which must rebuild the same pane. See
	// ApplyLaunchPostureEnv for the order and what each step sets.
	if err := ApplyLaunchPostureEnv(h, LaunchPostureEnv{
		AutoMemory: autoMemory, ContextFeatures: contextFeatures, AutoCompactWindow: autoCompactWindow,
	}, additionalEnv); err != nil {
		return err
	}
	envExports := clcommon.BuildEnvExports(additionalEnv)
	var openCodeServer openCodeLaunchServer
	if h.UsesAuthoritativeServer() {
		openCodeServer, err = resolveOpenCodeLaunchServer()
		if err != nil {
			return err
		}
		applyOpenCodeAttachEnvironment(
			additionalEnv,
			os.Getenv(clcommon.OpenCodeStateIsolationEnv),
		)
		envExports = clcommon.BuildEnvExports(additionalEnv)
	}
	if outerLayer && tclaudeLayerRoot == sandboxpolicy.RootConstructed {
		// Bubblewrap seeds this PATH too, which is sufficient for direct
		// namespace helpers. Production harness commands then replay the ambient
		// environment inside that namespace, including PATH, so the trusted
		// single-file CLI directory must be restored after those generated
		// exports and before any operator-authored pre-launch script.
		envExports = appendTclaudeLayerConstructedRootPathExport(envExports)
	}

	if err := refuseStateDirCwdUnderWorkspaceWrite(h, params, harnessBuiltinMode, cwd); err != nil {
		return err
	}

	discloseSpawnPosture(h, approvalPolicy, harnessBuiltinMode, cwd, outerLayer)

	osSandbox := resolveLaunchOSSandbox(h, params, harnessBuiltinMode, cwd, outerLayer, tclaudeLayerNetwork{
		posture: tclaudeLayerPosture, engine: tclaudeLayerEngine,
		preserveCallerIdentity: tclaudeLayerPreserveCallerIdentity, root: tclaudeLayerRoot,
	})
	sandboxChosenBy, launchOSSandbox := osSandbox.chosenBy, osSandbox.launch
	bwrapBinary, bwrapCapabilityErr := osSandbox.bwrapBinary, osSandbox.bwrapCapabilityErr
	if err := GateTclaudeLayerHarnessPosture(h, outerLayer, launchSandbox, extraArgs); err != nil {
		return err
	}
	discloseUnconfinedAccessRules(sandboxImplementation, unconfined, launchSandbox, effectiveSandbox)
	access := launchAccessState{
		posture: tclaudeLayerPosture, root: tclaudeLayerRoot,
		osSandbox: launchOSSandbox, networkFiltered: networkFiltered,
	}
	if err := planLaunchAccessEnforcement(launchAccessPlanInput{
		h: h, params: params, model: model, cwd: cwd, extraArgs: extraArgs,
		sandboxImplementation: sandboxImplementation, effectiveHarnessBuiltinMode: effectiveHarnessBuiltinMode,
		unconfined: unconfined, outerLayer: outerLayer,
		layerEngine: tclaudeLayerEngine, filteredNetworkProbe: filteredNetworkProbe,
		launchSandbox: launchSandbox, effectiveSandbox: effectiveSandbox,
	}, &access); err != nil {
		return err
	}
	// The plan's revised posture has no reader past this point: it is already
	// folded into the root and the OS-sandbox verdict copied back here.
	tclaudeLayerRoot = access.root
	launchOSSandbox, networkFiltered = access.osSandbox, access.networkFiltered
	if launchSandbox != nil {
		for _, notice := range launchSandbox.Effective.AccessNotices {
			fmt.Fprintf(os.Stdout, "Warning: %s\n", notice.Detail)
		}
	}

	if params.PermissionProfile == harness.CodexAgentProfile {
		profile, err := prepareCodexAgentProfile(params, cwd, fullConvID, sessionID, exitGeneration, outerLayer, launchSandbox, &launchUndo)
		if err != nil {
			return err
		}
		launchPermissionProfile = profile.name
		launchProfilePath = profile.path
		launchCodexSplitCapability = profile.splitCapability
		ordinaryNativeGeneration = profile.nativeGeneration
		codexAppServerProfileOverrides = profile.appServerOverrides
	}

	if err := prepareLaunchDirTrust(h, params, launchSandbox, additionalEnv, cwd); err != nil {
		return err
	}

	// A preset --session-id makes the conv-id known before launch (fresh CC
	// only). Stamp it on the session row now — the SessionStart hook reports
	// the same id — so the daemon's launch-enrollment path sees the conv-id
	// immediately rather than polling for the hook. A resume already resolved
	// fullConvID above; an unset id leaves the row's conv-id for the hook.
	rowConvID := fullConvID
	if params.SessionID != "" && fullConvID == "" {
		rowConvID = params.SessionID
	}
	// Gemini CLI has no launch-name flag and no title store, so a fresh
	// launch's --name is recorded where every tclaude surface reads titles
	// from: the conversation index. Best effort — a failed write leaves an
	// unnamed conversation, never a failed launch.
	if h.Name == harness.GeminiName && params.Name != "" && fullConvID == "" && rowConvID != "" {
		if err := db.SetConvIndexCustomTitle(rowConvID, params.Name, h.Name); err != nil {
			slog.Warn("could not record the Gemini launch name", "conv", rowConvID, "error", err)
		}
	}
	var darwinRouteReservation *DarwinRouteSlotReservation
	defer func() {
		if darwinRouteReservation != nil {
			_ = darwinRouteReservation.Release()
		}
	}()
	darwinRouteReservation, err = claimDarwinRouteLaunch(params, h.Name, outerLayer, rowConvID, exitGeneration, &launchUndo)
	if err != nil {
		return err
	}

	// A Codex launch that asks for no sandbox may still run in one: managed
	// requirements can overrule the request (codex_requirements.go). Checked
	// here, after access enforcement was planned from the mode, because the
	// verdict it records is for the record only.
	if !outerLayer {
		var requirementsEnvironment []sandboxpolicy.EnvironmentEntry
		if launchSandbox != nil {
			requirementsEnvironment = sandboxpolicy.EnvironmentForLaunch(launchSandbox)
		}
		if verdict, checked := VerifyCodexSandboxOff(h, sandboxImplementation,
			harnessBuiltinMode, cwd, requirementsEnvironment); checked {
			launchOSSandbox = verdict
		}
	}

	// launchPaneCommand is assigned once the pane command is fully assembled,
	// so the row rollback can report what this launch actually tried to run.
	launchPaneCommand := ""
	state, err := createLaunchRow(launchRowInput{
		sessionID: sessionID, tmuxSession: tmuxSession, cwd: cwd, convID: rowConvID,
		exitGeneration: exitGeneration, h: h, params: params,
		harnessBuiltinMode: harnessBuiltinMode, sandboxImplementation: sandboxImplementation,
		sandboxChosenBy: sandboxChosenBy, osSandbox: launchOSSandbox, effectiveSandbox: effectiveSandbox,
		approvalPolicy: recordedApprovalPolicy, autoReview: autoReview, askTimeout: askTimeout,
		envExports: envExports, paneCommand: &launchPaneCommand,
	}, &launchUndo)
	if err != nil {
		return err
	}
	// Save the off/unavailable verdict before refusing so a resume of an
	// existing conversation cannot leave a stale "on" badge behind.
	if bwrapCapabilityErr != nil {
		return bwrapCapabilityErr
	}

	exe, err := resolveLaunchExecutable(params, h, outerLayer, tclaudeLayerRoot, launchCodexSplitCapability)
	if err != nil {
		return err
	}
	executablePath, executableInterpreter, harnessReadPaths := exe.path, exe.interpreter, exe.readPaths
	dirs, err := planLaunchDirs(launchDirsInput{
		params: params, h: h, cwd: cwd, launchSandbox: launchSandbox,
		harnessBuiltinMode: harnessBuiltinMode, effectiveHarnessBuiltinMode: effectiveHarnessBuiltinMode,
		sandboxImplementation: sandboxImplementation, outerLayer: outerLayer, unconfined: unconfined,
	})
	if err != nil {
		return err
	}
	codexRuntimeDir, launchGitWriteDirs := dirs.codexRuntime, dirs.gitWrite
	launchWriteDirs, launchReadDirs, launchDenyDirs := dirs.write, dirs.read, dirs.deny
	// Rendered from the same snapshot ShellEnvironment reads, so what the pane
	// runs and what the harness is told about agree by construction.
	preLaunchScript, err := RenderPreLaunchScript(effectiveSandbox)
	if err != nil {
		return err
	}
	shellEnvironment := launchShellEnvironment(effectiveSandbox, outerLayer, tclaudeLayerRoot, additionalEnv)
	spawnSpec := harness.SpawnSpec{
		ModelProxy:                     params.ModelProxy,
		ClaudeSubscription:             params.ClaudeSubscription,
		ExecutablePath:                 executablePath,
		ExecutableInterpreter:          executableInterpreter,
		CodexAppServerSocket:           params.CodexAppServerSocket,
		CodexAppServerURL:              params.CodexAppServerURL,
		CodexAppServerTokenSHA256:      params.CodexAppServerTokenSHA256,
		CodexAppServerTokenHandoff:     params.CodexAppServerTokenHandoff,
		TclaudeExecutable:              clcommon.SelfTclaudePath(),
		CodexAppServerPIDFile:          params.CodexAppServerPIDFile,
		CodexAppServerLogFile:          params.CodexAppServerLogFile,
		CodexAppServerProfileOverrides: codexAppServerProfileOverrides,
		Cwd:                            cwd,
		ServerURL:                      openCodeServer.url,
		OpenCodeTransport:              openCodeServer.transport,
		OpenCodeControlSocketPath:      openCodeServer.controlSocketPath,
		OpenCodeControlSocketDevice:    openCodeServer.controlSocketDevice,
		OpenCodeControlSocketInode:     openCodeServer.controlSocketInode,
		OpenCodeServerPID:              openCodeServer.pid,
		EnvExports:                     envExports,
		PreLaunchScript:                preLaunchScript,
		ShellEnvironment:               shellEnvironment,
		ResumeID:                       fullConvID,
		SessionID:                      params.SessionID,
		Name:                           params.Name,
		Effort:                         effort,
		Model:                          model,
		ExtraArgs:                      extraArgs,
		HarnessBuiltinMode:             harnessBuiltinMode,
		SandboxWriteDirs:               launchWriteDirs,
		SandboxReadDirs:                launchReadDirs,
		SandboxDenyDirs:                launchDenyDirs,
		AskUserQuestionTimeout:         askTimeout,
		ContextFeatures:                contextFeatures,
		PeerMessaging:                  params.PeerMessaging,
		PermissionProfile:              launchPermissionProfile,
		ApprovalPolicy:                 approvalPolicy,
		AutoReview:                     autoReview,
		FastMode:                       params.FastMode,
		RemoteControl:                  remoteControl,
		CopilotAPIPort:                 params.CopilotAPIPort,
		InitialPrompt:                  params.InitialPrompt,
	}
	extensionReadPaths, err := FinishHarnessSpawnSpec(
		h, &spawnSpec, params.ManagedInstructionsRoot, outerLayer, tclaudeLayerRoot, approvalPolicy, os.Stderr)
	if err != nil {
		return err
	}
	harnessReadPaths = append(harnessReadPaths, extensionReadPaths...)
	spawnSpec.NetworkFiltered = networkFiltered
	layerSpec, err := prepareTclaudeLayerLaunch(tclaudeLayerLaunchStepInput{
		h: h, sessionID: sessionID, cwd: cwd, convID: rowConvID, outerLayer: outerLayer,
		stateRoot: spawnSpec.HarnessStateDir, gitWriteDirs: launchGitWriteDirs, launchSandbox: launchSandbox,
		codexRuntimeDir: codexRuntimeDir, darwinRouteReservation: darwinRouteReservation,
		routeHelper: routeHelper, harnessReadPaths: harnessReadPaths,
	}, &launchUndo)
	if err != nil {
		return err
	}
	harnessCmd, executionBoundaryJSON, err := assembleLaunchPaneCommand(launchPaneCommandInput{
		h: h, params: params, spawnSpec: spawnSpec, layerSpec: &layerSpec, sessionID: sessionID, cwd: cwd,
		exitGeneration: exitGeneration, outerLayer: outerLayer, sandboxImplementation: sandboxImplementation,
		effectiveSandbox: effectiveSandbox, bwrapBinary: bwrapBinary, harnessReadPaths: harnessReadPaths,
		additionalEnv: additionalEnv, timing: timing,
	})
	if err != nil {
		return err
	}
	resourceCgroupCleanup := func() {}
	resourceCgroupOwnedByPane := false
	defer func() {
		if !resourceCgroupOwnedByPane {
			resourceCgroupCleanup()
		}
	}()
	resourceCmd, resourceCleanup, err := wrapLaunchInResourceCgroup(params, sessionID, sandboxImplementation, launchSandbox, effectiveSandbox, harnessCmd)
	if err != nil {
		return err
	}
	harnessCmd, resourceCgroupCleanup = resourceCmd, resourceCleanup
	if launchProfilePath != "" {
		// The launch-specific profile must remain present until Codex exits: it
		// may not have loaded the file when tmux reports the pane ready. Keeping
		// cleanup in the pane shell also prevents one launch from ever sharing or
		// overwriting another launch's proof-scoped authority.
		harnessCmd = commandWithFileCleanup(harnessCmd, launchProfilePath)
	}
	exitGuard := newLaunchExitGuard(sessionID, tmuxSession, exitGeneration)
	defer exitGuard.abort()
	// The existing cwd proof remains the outer bootstrap below so it can report
	// readiness to the parent; the actual harness stays behind this private,
	// bounded gate until its pane-local exit hook and durable binding are ready.
	harnessCmd = exitGuard.wrap(harnessCmd)
	harnessCmd, proofReadyPath, cleanupProofReady, err := guardLaunchWithDirProof(params, launchSandbox, harnessCmd)
	if err != nil {
		return err
	}
	defer cleanupProofReady()

	// The command is now fully assembled — every wrapper layer applied. Hand it
	// to the rollback above so a launch that fails from here on says what it
	// tried to run, and emit it at debug for a launch that fails in a way the
	// rollback never sees.
	launchPaneCommand = harnessCmd
	logLaunchPaneCommand(harnessCmd, envExports, effectiveSandbox, sessionID, tmuxSession)

	// Create the detached tmux session running the harness command. The
	// managed Codex launch-profile path (when any) rides as an inert argv
	// marker so the approval monitor's startup recovery can match the live
	// pane to its profile (see CodexProfileMarkerArgs).
	timing("tmux_launch_begin")
	if err := launchDetachedTmuxSession(tmuxSession, cwd, harnessCmd, CodexProfileMarkerArgs(launchProfilePath)...); err != nil {
		return err
	}
	if darwinRouteReservation != nil {
		if err := handOffDarwinRouteAtLaunch(darwinRouteReservation,
			func() error {
				return db.ActivateDarwinRouteLaunch(params.DarwinRouteAgentID, rowConvID, exitGeneration)
			},
			func() { _ = clcommon.TmuxCommand("kill-session", "-t", clcommon.ExactTarget(tmuxSession)).Run() },
		); err != nil {
			return err
		}
		darwinRouteReservation = nil
	}
	timing("tmux_launched")
	resourceCgroupOwnedByPane = true
	// killLaunchPane tears the just-launched pane down on a late launch
	// failure. launchUndo, still uncommitted, then removes the launch-profile
	// file: a killed pane's shell never reaches its own cleanup trailer.
	killLaunchPane := func() {
		_ = clcommon.TmuxCommand("kill-session", "-t", clcommon.ExactTarget(tmuxSession)).Run()
		resourceCgroupCleanup()
	}
	if err := settleLaunchedPane(launchSettleInput{
		exitGuard: exitGuard, proofReadyPath: proofReadyPath, killLaunchPane: killLaunchPane, timing: timing,
		tmuxSession: tmuxSession, sessionID: sessionID, params: params, h: h, state: state,
		model: model, effort: effort, autoMemory: autoMemory, remoteControl: remoteControl,
		contextFeatures: contextFeatures, autoCompactWindow: autoCompactWindow,
		ordinaryNativeGeneration: ordinaryNativeGeneration, executionBoundaryJSON: executionBoundaryJSON,
	}); err != nil {
		return err
	}

	// The pane is up and bound; from here the row, the route claim, the
	// activated native generation and the launch-profile file (removed by the
	// pane's own cleanup trailer) belong to the live session. An attach
	// failure below must not undo them.
	launchUndo.Commit()
	return announceAndAttach(fmt.Sprintf("Created session %s", tmuxSession), sessionID, tmuxSession, cwd, params.Detached)
}

func accessEnforcementOptionsFromLaunchNotices(
	notices []sandboxpolicy.AccessNotice,
) harness.AccessEnforcementOptions {
	options := harness.AccessEnforcementOptions{}
	for _, notice := range notices {
		if notice.Class != sandboxpolicy.AccessNoticeClassDegradation ||
			notice.Axis != "network" ||
			notice.Effect != sandboxpolicy.AccessNoticeEffectNotEnforced {
			continue
		}
		switch notice.Reason {
		case sandboxpolicy.AccessNoticeReasonOperatorUnenforcedLaunchOverride:
			options.AllowUnenforcedNetworkClosed = true
		case sandboxpolicy.AccessNoticeReasonOperatorReducedNetworkDenyOverride:
			options.AllowReducedNetworkDeny = true
		}
	}
	return options
}

func validateSandboxImplementationDecision(
	h *harness.Harness,
	raw string,
	implementation sandboxpolicy.Implementation,
	freshResolution bool,
) error {
	if !freshResolution ||
		strings.TrimSpace(raw) == "" ||
		implementation != sandboxpolicy.ImplementationHarnessBuiltin {
		return nil
	}
	return harness.ValidateHarnessBuiltinOSSandbox(h)
}

func applyOpenCodeAttachEnvironment(environment map[string]string, isolation string) {
	if isolation != db.OpenCodeStatePrivate {
		return
	}
	for _, name := range []string{
		"XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_CONFIG_HOME", "XDG_STATE_HOME",
	} {
		if value, ok := os.LookupEnv(name); ok {
			environment[name] = value
		}
	}
	// OpenCode loads this custom directory after project config. A private
	// launch preserves native precedence through the read-only XDG config bind
	// and must neutralize an ambient custom-dir override.
	environment["OPENCODE_CONFIG_DIR"] = ""
}

func gitWorktreeWriteDirs(params *NewParams, harnessName, harnessBuiltinMode, cwd string) []string {
	if !spawnSandboxUsesGitWriteDirs(harnessName, harnessBuiltinMode) {
		return nil
	}
	if params.GitWorktreeWriteDirsPinned {
		return append([]string(nil), params.GitWorktreeWriteDirs...)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	commonDir := params.CodexGitCommonDir
	if !params.CodexGitCommonDirPinned {
		commonDir, err = harness.GitCommonDir(cwd)
		if err != nil {
			return nil
		}
	}
	return harness.GitWorktreeWriteDirs(cwd, commonDir, home)
}

func spawnSandboxUsesGitWriteDirs(harnessName, harnessBuiltinMode string) bool {
	if harnessName == harness.CodexName {
		return harnessBuiltinMode == harness.SandboxManagedProfile
	}
	return harnessName == harness.DefaultName && harnessBuiltinMode != harness.ClaudeSandboxOff
}

func validateCodexGitCommonDirPin(params *NewParams) error {
	params.CodexGitCommonDir = strings.TrimSpace(params.CodexGitCommonDir)
	if params.CodexGitCommonDir == "" {
		return nil
	}
	if !params.CodexGitCommonDirPinned {
		return fmt.Errorf("internal Codex git-common-dir grant requires a pinned result")
	}
	if params.CwdWriteProof == "" && params.DirWriteProof == "" {
		return fmt.Errorf("internal Codex git-common-dir grant requires a daemon write proof")
	}
	if !filepath.IsAbs(params.CodexGitCommonDir) {
		return fmt.Errorf("internal Codex git-common-dir grant must be absolute")
	}
	return nil
}

func validateGitWorktreeWriteDirPins(params *NewParams) error {
	if len(params.GitWorktreeWriteDirs) > 0 && !params.GitWorktreeWriteDirsPinned {
		return fmt.Errorf("internal Git worktree write-dir grants require a pinned result")
	}
	if len(params.GitWorktreeWriteDirs) > 0 && params.CwdWriteProof == "" && params.DirWriteProof == "" {
		return fmt.Errorf("internal Git worktree write-dir grants require a daemon write proof")
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(params.GitWorktreeWriteDirs))
	for _, dir := range params.GitWorktreeWriteDirs {
		dir = filepath.Clean(strings.TrimSpace(dir))
		if dir == "." || !filepath.IsAbs(dir) {
			return fmt.Errorf("internal Git worktree write-dir grant must be absolute")
		}
		if !seen[dir] {
			seen[dir] = true
			out = append(out, dir)
		}
	}
	params.GitWorktreeWriteDirs = out
	return nil
}

func ensureCodexManagedProfile(params *NewParams, cwd, launchID string) (string, string, error) {
	name, path, _, err := ensureCodexManagedProfileWithSnapshot(params, cwd, launchID, nil)
	return name, path, err
}

// EnsureCodexManagedOneShotProfile materializes the same launch-unique
// permission profile a managed Codex session would receive, for an agentd-owned
// headless resume. The caller owns path and must remove it after the child
// exits. capability, when non-nil, must be revalidated immediately before the
// exec boundary just like runNew does for a normal session launch.
func EnsureCodexManagedOneShotProfile(
	cwd, launchID string,
	effectiveSandbox *sandboxpolicy.Snapshot,
) (name, path string, capability *harness.CodexSplitPolicyCapability, err error) {
	return ensureCodexManagedProfileWithSnapshot(&NewParams{
		PermissionProfile: harness.CodexAgentProfile,
	}, cwd, launchID, effectiveSandbox)
}

// OneShotLaunchPosture renders the sandbox-profile fields a normal session
// launch would place on SpawnSpec, without starting a tmux session. Agentd uses
// it when a headless resume must run with the same recorded containment as the
// predecessor instead of the generic read-only `tclaude ask` posture.
func OneShotLaunchPosture(
	cwd string,
	targetHarness *harness.Harness,
	harnessBuiltinMode, approvalPolicy string,
	autoReview bool,
	effectiveSandbox *sandboxpolicy.Snapshot,
	implementations ...sandboxpolicy.Implementation,
) (harness.SpawnSpec, error) {
	if targetHarness == nil {
		return harness.SpawnSpec{}, fmt.Errorf("prepare one-shot launch posture: nil harness")
	}
	harnessName := targetHarness.Name
	implementation := sandboxpolicy.ImplementationHarnessBuiltin
	if len(implementations) > 0 {
		implementation = implementations[0]
	}
	// A one-shot replay renders a harness-NATIVE sandbox posture: there is no
	// tclaude-owned mount namespace here at all, so a recorded mount_path cannot
	// be reproduced. Refuse rather than replaying the rule at its host path,
	// which would be a different policy than the one being replayed.
	if err := sandboxpolicy.ValidateMountPathSupport(
		sandboxSnapshotActiveFilesystem(effectiveSandbox),
		implementation, runtime.GOOS,
	); err != nil {
		return harness.SpawnSpec{}, err
	}
	if err := sandboxpolicy.ValidateProtectedAncestorWriteSupport(
		sandboxSnapshotActiveFilesystem(effectiveSandbox),
		implementation, runtime.GOOS,
	); err != nil {
		return harness.SpawnSpec{}, err
	}
	// And for the same reason a recorded FILE rule cannot be replayed here: the
	// harness-native posture is rendered from directory lists, so replaying one
	// would drop the rule rather than reproduce it.
	if err := sandboxpolicy.ValidateFileGrantSupport(
		sandboxSnapshotActiveFilesystem(effectiveSandbox),
		implementation, runtime.GOOS,
	); err != nil {
		return harness.SpawnSpec{}, err
	}
	// A recorded tmpfs cannot be replayed here either: there is no namespace to
	// mount it in, and a replay silently missing the agent's scratch space is a
	// different policy than the one being replayed.
	if err := sandboxpolicy.ValidateTmpfsSupport(
		sandboxSnapshotTmpfs(effectiveSandbox),
		implementation, runtime.GOOS,
	); err != nil {
		return harness.SpawnSpec{}, err
	}
	params := &NewParams{}
	launchGitWriteDirs := gitWorktreeWriteDirs(params, harnessName, harnessBuiltinMode, cwd)
	if implementation.UsesTclaudeLayer() {
		launchGitWriteDirs = gitWorktreeWriteDirs(params, harness.DefaultName, harness.ClaudeSandboxOn, cwd)
	}
	if sandboxDenyCoversPath(effectiveSandbox, cwd) {
		launchGitWriteDirs = denyNarrowedGitWriteDirs(cwd, "", launchGitWriteDirs)
	}
	launchContractReadDirs := sandboxLaunchContractReadDirs(
		effectiveSandbox, append([]string{cwd}, launchGitWriteDirs...)...)
	launchWriteDirs := append(launchGitWriteDirs,
		sandboxSnapshotDirs(effectiveSandbox, sandboxpolicy.AccessWrite)...)
	if implementation.UsesTclaudeLayer() {
		launchWriteDirs = AppendUniqueDir(launchWriteDirs, CanonicalSandboxPath(cwd))
		for _, mount := range sandboxSnapshotTmpfs(effectiveSandbox) {
			launchWriteDirs = AppendUniqueDir(launchWriteDirs, mount.Path)
		}
	}
	launchReadDirs := append(sandboxSnapshotDirs(effectiveSandbox, sandboxpolicy.AccessRead),
		launchContractReadDirs...)
	launchDenyDirs := sandboxSnapshotDirs(effectiveSandbox, sandboxpolicy.AccessDeny)
	if err := harness.ValidateSandboxReopenUnderDeny(harnessName, harnessBuiltinMode,
		sandboxpolicy.GrantsFromDirs(launchReadDirs, launchWriteDirs, launchDenyDirs)); err != nil {
		return harness.SpawnSpec{}, err
	}
	posture := harness.SpawnSpec{
		Cwd:                cwd,
		HarnessBuiltinMode: harnessBuiltinMode,
		SandboxWriteDirs:   launchWriteDirs,
		SandboxReadDirs:    launchReadDirs,
		SandboxDenyDirs:    launchDenyDirs,
		ShellEnvironment:   sandboxSnapshotEnvironment(effectiveSandbox),
		ApprovalPolicy:     approvalPolicy,
		AutoReview:         autoReview,
	}
	return targetHarness.PrepareHostControlSandboxLaunch(posture)
}

func ensureCodexManagedProfileWithSnapshot(params *NewParams, cwd, launchID string, effectiveSandbox *sandboxpolicy.Snapshot) (string, string, *harness.CodexSplitPolicyCapability, error) {
	var writeDirs []string
	gitCommonDir := params.CodexGitCommonDir
	if params.GitWorktreeWriteDirsPinned {
		writeDirs = append([]string(nil), params.GitWorktreeWriteDirs...)
	} else if params.CodexGitCommonDirPinned && params.CodexGitCommonDir != "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", "", nil, err
		}
		writeDirs = harness.GitWorktreeWriteDirs(cwd, params.CodexGitCommonDir, home)
	} else if params.CodexGitCommonDirPinned {
		writeDirs = nil
	} else {
		commonDir, err := harness.GitCommonDir(cwd)
		if err != nil {
			return "", "", nil, err
		}
		gitCommonDir = commonDir
		home, err := os.UserHomeDir()
		if err != nil {
			return "", "", nil, err
		}
		writeDirs = harness.GitWorktreeWriteDirs(cwd, commonDir, home)
	}
	if sandboxDenyCoversPath(effectiveSandbox, cwd) {
		// The historical repository grant is the main repository's whole
		// container so an agent can create sibling worktrees directly. That is
		// incompatible with a deny covering the workspace: it would reopen every
		// sibling repo and arbitrary sibling file beneath the deny. Keep only the
		// active cwd and exact daemon-verified Git common/admin identities. Direct
		// sibling-worktree creation is intentionally unavailable under this
		// posture; tclaude must broker it before launch instead of granting the
		// container.
		writeDirs = denyNarrowedGitWriteDirs(cwd, gitCommonDir, writeDirs)
	}
	launchContractReadDirs := sandboxLaunchContractReadDirs(effectiveSandbox, append([]string{cwd}, writeDirs...)...)
	writeDirs = append(writeDirs, sandboxSnapshotDirs(effectiveSandbox, sandboxpolicy.AccessWrite)...)
	readDirs := append(sandboxSnapshotDirs(effectiveSandbox, sandboxpolicy.AccessRead), launchContractReadDirs...)
	if params.ManagedInstructionsRoot != "" {
		readDirs = AppendUniqueDir(readDirs, params.ManagedInstructionsRoot)
	}
	denyDirs := sandboxSnapshotDirs(effectiveSandbox, sandboxpolicy.AccessDeny)
	if sandboxDenyCoversPath(effectiveSandbox, cwd) {
		// `extends = ":workspace"` still grants the launch directory, but a deny
		// covering it masks that grant, and GitWorktreeWriteDirs returns nothing
		// outside a Git repository. The workspace is an invariant launch
		// requirement rather than an optional grant, so reopen it explicitly.
		if workspace := canonicalLaunchWorkspace(cwd); workspace != "" {
			writeDirs = AppendUniqueDir(writeDirs, workspace)
		}
	}
	// Codex needs the verified split-policy backend for exactly the shape it
	// cannot otherwise honor: a narrower reopen beneath a denied ancestor. The
	// shape is read from the RENDERED rules, not the authored profile — the
	// launch contract above adds reopens of its own, so `deny ~` on its own
	// still emits a split policy and must still be probed and refused on macOS.
	requireSplitPolicy := sandboxpolicy.HasReopenUnderDeny(
		sandboxpolicy.GrantsFromDirs(readDirs, writeDirs, denyDirs))
	var splitCapability *harness.CodexSplitPolicyCapability
	if requireSplitPolicy {
		if err := harness.ValidateSandboxReopenUnderDeny(harness.CodexName, harness.SandboxManagedProfile,
			sandboxpolicy.GrantsFromDirs(readDirs, writeDirs, denyDirs)); err != nil {
			return "", "", nil, err
		}
		verified, err := harness.VerifyCodexHomeSplitPolicy()
		if err != nil {
			return "", "", nil, err
		}
		splitCapability = &verified
		if verified.RequiresExecutableReopen {
			readDirs = append(readDirs, verified.ExecutablePath)
		}
	}
	networkAccess := sandboxSnapshotNetworkAccess(effectiveSandbox)
	profileName, path, err := harness.EnsureCodexAgentLaunchProfileForRules(harness.CodexSandboxRules{
		ReadDirs:    readDirs,
		WriteDirs:   writeDirs,
		DenyDirs:    denyDirs,
		UnixSockets: sandboxSnapshotUnixSockets(effectiveSandbox),
		MaterializedUnixSocketPaths: sandboxSnapshotMaterializedUnixSocketPaths(
			effectiveSandbox),
		RequireSplitPolicy: requireSplitPolicy,
	}, networkAccess, launchID)
	if err != nil {
		return "", "", nil, fmt.Errorf("ensure codex permission profile %q: %w", params.PermissionProfile, err)
	}
	return profileName, path, splitCapability, nil
}

func denyNarrowedGitWriteDirs(cwd, gitCommonDir string, candidates []string) []string {
	var out []string
	if workspace := canonicalLaunchWorkspace(cwd); workspace != "" {
		out = AppendUniqueDir(out, workspace)
	}
	common := filepath.Clean(strings.TrimSpace(gitCommonDir))
	if common == "." || !filepath.IsAbs(common) {
		return out
	}
	out = AppendUniqueDir(out, common)
	for _, candidate := range candidates {
		candidate = filepath.Clean(strings.TrimSpace(candidate))
		// A candidate at/below the verified common directory is an exact Git
		// admin identity (for linked worktrees Codex needs the more-specific
		// worktrees/<name> reopen). Ancestors such as ~/git are discarded.
		if candidate != "." && filepath.IsAbs(candidate) && sandboxpolicy.PathContainsOrEqual(common, candidate) {
			out = AppendUniqueDir(out, candidate)
		}
	}
	return out
}

// canonicalLaunchWorkspace resolves the launch directory for an explicit
// workspace reopen. It is symlink-resolved so the emitted rule names the same path the
// sandbox will see, and returns "" when the directory cannot be resolved (the
// launch then fails on the ordinary cwd checks rather than here).
func canonicalLaunchWorkspace(cwd string) string {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" || !filepath.IsAbs(cwd) {
		return ""
	}
	resolved, err := filepath.EvalSymlinks(filepath.Clean(cwd))
	if err != nil {
		return ""
	}
	return filepath.Clean(resolved)
}

// AppendUniqueDir appends dir unless dirs already holds it.
func AppendUniqueDir(dirs []string, dir string) []string {
	for _, existing := range dirs {
		if existing == dir {
			return dirs
		}
	}
	return append(dirs, dir)
}

func sandboxSnapshotNetworkAccess(snapshot *sandboxpolicy.Snapshot) sandboxpolicy.NetworkAccess {
	if snapshot == nil {
		return sandboxpolicy.NetworkAccessInherit
	}
	return snapshot.Effective.NetworkAccess
}

func sandboxSnapshotUnixSockets(
	snapshot *sandboxpolicy.Snapshot,
) *sandboxpolicy.UnixSocketRules {
	if snapshot == nil || snapshot.Effective.UnixSockets == nil {
		return nil
	}
	rules := *snapshot.Effective.UnixSockets
	rules.Allow = append([]sandboxpolicy.SocketAllowEntry(nil), rules.Allow...)
	return &rules
}

func sandboxSnapshotMaterializedUnixSocketPaths(
	snapshot *sandboxpolicy.Snapshot,
) *[]string {
	if snapshot == nil || snapshot.UnixSocketMaterialization == nil {
		return nil
	}
	paths := append([]string(nil), snapshot.UnixSocketMaterialization.Paths...)
	return &paths
}

// authoredSecretMinLen is the shortest authored value used as a fail-closed
// tripwire. At or below it a value is indistinguishable from ordinary command
// text — a profile authoring NODE_ENV=production or TERM=xterm would match
// somewhere in every launch and withhold the diagnostic forever. Short values
// are covered by the marker below instead, which does not depend on length.
const authoredSecretMinLen = 12

// authoredPaneCommandSecrets describes the sandbox profile's authored
// environment to RedactPaneCommand's fail-closed check.
//
// These are the operator's declared secrets, and the ones that reach the pane
// command through more than one channel: besides the shared env-export prefix,
// a Codex launch renders each of them again as a shell_environment_policy.set
// override on its own argv (see sandboxSnapshotEnvironment and the Codex
// spawner). Removing the prefix alone left them in the logged command.
//
// Both halves are needed. A distinctive value catches a channel this code knows
// nothing about. A marker catches the channel we DO know about regardless of
// how short its value is, which is what keeps the length floor from opening a
// hole: an authored 6-character token is still refused, by name.
//
// Only AUTHORED entries. Values inherited from the operator's environment are
// covered by removing the prefix and cannot be checked this way, because HOME,
// PWD and friends are legitimate substrings of ordinary command text.
func authoredPaneCommandSecrets(snapshot *sandboxpolicy.Snapshot) PaneCommandSecrets {
	if snapshot == nil {
		return PaneCommandSecrets{}
	}
	var secrets PaneCommandSecrets
	for _, entry := range sandboxpolicy.EnvironmentForLaunch(snapshot) {
		if len(entry.Value) > authoredSecretMinLen {
			secrets.Values = append(secrets.Values, entry.Value)
		}
		secrets.Markers = append(secrets.Markers,
			harness.CodexShellEnvironmentOverridePrefix+entry.Name+"=")
	}
	return secrets
}

func sandboxSnapshotEnvironment(snapshot *sandboxpolicy.Snapshot) map[string]string {
	entries := sandboxpolicy.EnvironmentForLaunch(snapshot)
	if len(entries) == 0 {
		return nil
	}
	out := make(map[string]string, len(entries))
	for _, entry := range entries {
		out[entry.Name] = entry.Value
	}
	return out
}

// CanonicalSandboxPath symlink-resolves a launch-time path so it can be
// compared against effective grants, which normalization has already fully
// resolved. Without this a cwd reached through a symlinked ancestor (macOS
// /var → /private/var, or a symlinked home) would not appear to be covered by a
// deny that does cover it, and the launch contract would silently skip the
// reopens the agent needs. Falls back to the lexical form when the path cannot
// be resolved; the ordinary cwd checks report that failure.
func CanonicalSandboxPath(path string) string {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "." || !filepath.IsAbs(path) {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return filepath.Clean(resolved)
	}
	return path
}

// sandboxDenyCoversPath reports whether a STRICT ancestor of path is denied by
// the effective profile. A deny at exactly path is not a "cover": normalization
// folds a same-path grant to the strongest access, so that path is simply
// denied and no reopen is owed.
func sandboxDenyCoversPath(snapshot *sandboxpolicy.Snapshot, path string) bool {
	if snapshot == nil {
		return false
	}
	path = CanonicalSandboxPath(path)
	if path == "" {
		return false
	}
	for _, grant := range snapshot.Effective.Filesystem {
		if grant.Access != sandboxpolicy.AccessDeny || grant.Path == path {
			continue
		}
		if sandboxpolicy.PathContainsOrEqual(grant.Path, path) {
			return true
		}
	}
	return false
}

// sandboxLaunchContractReadDirs is the launch contract under a deny row: every
// directory tclaude REQUIRES the agent to reach — the workspace, the verified
// Git admin paths, agent-owned directories, and the profile's own write grants
// — gets an explicit read reopen when a deny covers it.
//
// The Git-write and agent-directory paths are the load-bearing ones. Claude's
// allowWrite does NOT imply read beneath a denied ancestor (deny/allow are
// resolved per operation, most-specific-wins), so a workspace that is writable
// but unreadable is a broken agent rather than a strict one. Codex has the same
// need: its deny masks the parent, and only an explicit narrower rule reopens.
// The agentd socket is already re-allowed unconditionally elsewhere.
func sandboxLaunchContractReadDirs(snapshot *sandboxpolicy.Snapshot, candidates ...string) []string {
	if snapshot == nil {
		return nil
	}
	all := append([]string(nil), candidates...)
	// Host paths only. Every candidate here is canonicalized against the HOST
	// below, so feeding a sandbox path in would resolve whatever happens to sit
	// at that spelling on the host and emit a read reopen for it. A remapped
	// grant also needs no contract reopen: its mount is applied after the deny
	// that would have masked it, so nothing has to be repaired.
	for _, grant := range snapshot.Effective.Filesystem {
		if grant.Access == sandboxpolicy.AccessWrite && !grant.IsRemapped() {
			all = append(all, grant.Path)
		}
	}
	// Agent-owned directories are materialized by agentd and exported by name;
	// their absolute paths ride in the effective environment.
	for _, name := range snapshot.Effective.AgentDirectories {
		for _, entry := range sandboxpolicy.EnvironmentForLaunch(snapshot) {
			if entry.Name == name {
				all = append(all, entry.Value)
			}
		}
	}
	var out []string
	for _, candidate := range all {
		// Emit the canonical form: the rule must name the path the sandbox will
		// actually see, and must be comparable to the canonical deny above.
		candidate = CanonicalSandboxPath(candidate)
		if candidate == "" || !sandboxDenyCoversPath(snapshot, candidate) {
			continue
		}
		out = AppendUniqueDir(out, candidate)
	}
	sort.Strings(out)
	return out
}

func sandboxSnapshotDirs(snapshot *sandboxpolicy.Snapshot, access sandboxpolicy.Access) []string {
	if snapshot == nil {
		return nil
	}
	out := make([]string, 0, len(snapshot.Effective.Filesystem))
	// A rule naming a single FILE contributes nothing here. These are the
	// bare directory lists a harness is handed — `--add-dir` roots, Claude
	// Code's sandbox filesystem arrays — and a file in one of them is at best
	// ignored. The rule is not lost: the only implementation that reaches a
	// launch with one is the Linux tclaude-layer, whose applier binds it from
	// the mount plan. Every other implementation was refused by
	// ValidateFileGrantSupport before this point.
	for _, grant := range sandboxpolicy.DirectoryGrants(snapshot.Effective.Filesystem) {
		if grant.Access == access {
			// These lists describe the namespace the harness will run in, so a
			// remapped grant contributes the path it occupies THERE; every
			// surface that cannot mount at all refuses a remapped grant before
			// reaching here.
			out = append(out, grant.GuestPath())
		}
	}
	return out
}

// sandboxSnapshotHostDirs returns the host-side roots that must still be
// canonical immediately before the harness starts. These roots intentionally
// do not require proof markers: a profile grant may permit writes to existing
// descendants without permitting a new entry in the root itself.
//
// Rules naming a single file are checked by sandboxSnapshotHostFiles instead:
// the directory guard proves a path by `cd`-ing into it, which a file cannot
// satisfy, so folding the two kinds together would fail every launch carrying a
// file rule rather than check it.
func sandboxSnapshotHostDirs(snapshot *sandboxpolicy.Snapshot, access sandboxpolicy.Access) []string {
	if snapshot == nil {
		return nil
	}
	out := make([]string, 0, len(snapshot.Effective.Filesystem))
	for _, grant := range sandboxpolicy.DirectoryGrants(snapshot.Effective.Filesystem) {
		if grant.Access == access {
			out = append(out, grant.Path)
		}
	}
	return out
}

// sandboxSnapshotHostFiles is sandboxSnapshotHostDirs for the rules that name a
// regular file. The integrity question is the same one — is this still the
// thing the profile resolved, rather than a symlink swapped in since — asked
// with the check a file can answer.
func sandboxSnapshotHostFiles(snapshot *sandboxpolicy.Snapshot, access sandboxpolicy.Access) []string {
	if snapshot == nil {
		return nil
	}
	var out []string
	for _, grant := range snapshot.Effective.Filesystem {
		if grant.Access == access && sandboxpolicy.IsFileGrant(grant) {
			out = append(out, grant.Path)
		}
	}
	return out
}

func sandboxSnapshotActiveFilesystem(snapshot *sandboxpolicy.Snapshot) []sandboxpolicy.FilesystemGrant {
	if snapshot == nil {
		return nil
	}
	return snapshot.Effective.Filesystem
}

func sandboxSnapshotTmpfs(snapshot *sandboxpolicy.Snapshot) []sandboxpolicy.TmpfsMount {
	if snapshot == nil {
		return nil
	}
	return snapshot.Effective.Tmpfs
}

// SandboxSnapshotForLaunch is the snapshot as a launch sees it: its
// filesystem rules filtered by sandboxpolicy.FilesystemForLaunch, which drops
// read and write rows whose paths do not exist yet. Gates that count or check
// rules read this view, so a rule waiting for its path does not refuse the
// launch.
func SandboxSnapshotForLaunch(snapshot *sandboxpolicy.Snapshot) (*sandboxpolicy.Snapshot, error) {
	if snapshot == nil {
		return nil, nil
	}
	filesystem, err := sandboxpolicy.FilesystemForLaunch(snapshot.Effective)
	if err != nil {
		return nil, fmt.Errorf("sandbox_profile_changed: %w", err)
	}
	out := *snapshot
	out.Effective = snapshot.Effective
	out.Effective.Filesystem = filesystem
	return &out, nil
}

// SandboxSnapshotForOneShotLaunch freezes the exact active filesystem rules for
// a brokered one-shot launch. It is the exported twin of runNew's launch
// preparation: missing ordinary grants become inactive, while missing denies
// fail closed.
func SandboxSnapshotForOneShotLaunch(snapshot *sandboxpolicy.Snapshot) (*sandboxpolicy.Snapshot, error) {
	return SandboxSnapshotForLaunch(snapshot)
}

func commandWithFileCleanup(cmd, path string) string {
	return commandWithFileCleanupCommand(cmd, CodexProfileCleanupShell(path))
}

func commandWithFileCleanupCommand(cmd, cleanup string) string {
	statusVar := "tclaude_launch_status"
	return cmd + "; " + statusVar + "=$?; " + cleanup +
		"; exit $" + statusVar
}

// The CC launch-command builder lives behind the harness seam now —
// see claudeSpawner.BuildCommand in pkg/claude/harness/claude.go.

// resolveSessionDir resolves the --dir/-C value to an absolute working
// directory: the current directory when unset, else dir made absolute
// against the current directory. Shared by runNew and runNewShell.
func resolveSessionDir(dir string) (string, error) {
	cwd := dir
	if cwd == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("failed to get current directory: %w", err)
		}
		return cwd, nil
	}
	if cwd[0] != '/' {
		wd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("failed to get current directory: %w", err)
		}
		cwd = filepath.Join(wd, cwd)
	}
	return cwd, nil
}

// tmuxClientArgvLimit is a conservative bound on the total byte size of the
// argv tclaude may hand the tmux client. tmux packs the initial command's
// whole argv into ONE imsg and refuses anything over MAX_IMSGSIZE (16384)
// minus a small header with a bare "command too long" + exit 1 (client.c —
// string and check verified stable from tmux 1.8 through 3.7b). The margin
// absorbs the header plus any future per-arg framing overhead; tripping the
// pre-flight a little early is harmless, missing the real limit is not.
const tmuxClientArgvLimit = 16384 - 256

// tmuxArgvBytes mirrors the tmux client's own size computation for the
// initial command: sum of strlen(argv[i])+1 over the argv it is handed.
func tmuxArgvBytes(args []string) int {
	n := 0
	for _, a := range args {
		n += len(a) + 1
	}
	return n
}

// launchScriptStaleAfter is how long an unconsumed launch script may sit in
// the private launch-scripts dir before a sweep removes it. A script normally
// deletes itself as the pane's first action (seconds after the write), so
// anything older than this belongs to a launch whose pane died before its
// first line ran — and since the script carries the exported environment,
// stale ones should not linger. Kept comfortably above any plausible
// write-to-pane-start latency; sweeps run at every launch and at daemon
// startup (SweepStaleLaunchScripts).
const launchScriptStaleAfter = 15 * time.Minute

// SweepStaleLaunchScripts removes launch bootstrap scripts whose pane never
// ran its self-delete (killed between write and start, or dead before its
// first line). Called by agentd at startup so stranded scripts are bounded by
// daemon lifetime, not by when the next launch happens to run the per-launch
// sweep. Best-effort; a missing directory is a no-op.
func SweepStaleLaunchScripts() {
	base := strings.TrimSpace(config.DataDir())
	if base == "" {
		return
	}
	cleanupStaleLaunchScripts(filepath.Join(base, "launch-scripts"), time.Now())
}

// writeLaunchScript persists a pane bootstrap command as a private, one-shot
// script under DataDir and returns its path plus a remove func for launch
// failures. The script's first action is deleting itself: the shell keeps its
// open fd so execution continues, nothing secret-bearing outlives the launch,
// and `ps` shows `sh <path>` instead of the full env-carrying command. Panes
// killed before their first line leave a stale file; each launch sweeps those.
func writeLaunchScript(cmd string) (string, func(), error) {
	base := strings.TrimSpace(config.DataDir())
	if base == "" {
		return "", func() {}, fmt.Errorf("resolve private launch-script directory")
	}
	dir := filepath.Join(base, "launch-scripts")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", func() {}, fmt.Errorf("create launch-script directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", func() {}, fmt.Errorf("protect launch-script directory: %w", err)
	}
	cleanupStaleLaunchScripts(dir, time.Now())
	f, err := os.CreateTemp(dir, launchScriptPrefix)
	if err != nil {
		return "", func() {}, fmt.Errorf("create launch script: %w", err)
	}
	path := f.Name()
	cleanup := func() { _ = os.Remove(path) }
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		cleanup()
		return "", func() {}, fmt.Errorf("protect launch script: %w", err)
	}
	content := "# tclaude pane bootstrap — generated per launch, self-deleting.\n" +
		"rm -f -- \"$0\"\n" + cmd + "\n"
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		cleanup()
		return "", func() {}, fmt.Errorf("write launch script: %w", err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("close launch script: %w", err)
	}
	return path, cleanup, nil
}

// WriteLaunchScript creates the same private, self-deleting pane bootstrap
// used by ordinary harness launches. Managed runtimes use it when they need
// custom tmux session options without putting an unbounded command in tmux's
// size-limited initial argv.
func WriteLaunchScript(cmd string) (string, func(), error) {
	return writeLaunchScript(cmd)
}

const launchScriptPrefix = "launch-"

func cleanupStaleLaunchScripts(dir string, now time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), launchScriptPrefix) {
			continue
		}
		info, err := entry.Info()
		if err != nil || now.Sub(info.ModTime()) < launchScriptStaleAfter {
			continue
		}
		if err := os.Remove(filepath.Join(dir, entry.Name())); err == nil {
			slog.Debug("launch: removed stale pane bootstrap script", "script", entry.Name())
		}
	}
}

// LaunchDetachedTmuxSession is the exported form of
// launchDetachedTmuxSession for the conv package's resume paths, so a resume
// launches through the same script mechanism as a fresh session — same tmux
// argv O(1) guarantee, same credential hygiene (the env-carrying command
// never enters `ps`/#{pane_start_command}), same pre-flight error.
func LaunchDetachedTmuxSession(tmuxSession, cwd, cmd string, markerArgs ...string) error {
	return launchDetachedTmuxSession(tmuxSession, cwd, cmd, markerArgs...)
}

// LaunchDetachedTmuxSessionID launches like LaunchDetachedTmuxSession and
// returns the new session's tmux id ("$N"), read from new-session itself. A
// caller that must later poll or kill exactly this session targets the id: a
// session name is released when its last pane exits and can be reused, and
// any lookup after launch could already see a replacement.
func LaunchDetachedTmuxSessionID(tmuxSession, cwd, cmd string) (string, error) {
	out, err := launchDetachedTmuxSessionOutput(tmuxSession, cwd, cmd, true)
	if err != nil {
		return "", err
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	id := strings.TrimSpace(lines[len(lines)-1])
	if !strings.HasPrefix(id, "$") {
		return "", fmt.Errorf("tmux did not report the new session id (got %q)", strings.TrimSpace(out))
	}
	return id, nil
}

// CodexProfileMarkerArgs returns the extra argv words a launch appends after
// the script path when the pane runs a managed Codex launch profile: the
// profile path itself, as an inert positional the script never reads. It is a
// MARKER, not an input — the codex approval monitor's startup recovery
// identifies which live pane owns which launch profile by inspecting
// #{pane_start_command}, and with the bootstrap in a self-deleting script the
// profile path must ride in the (daemon-controlled, non-secret) argv to stay
// discoverable. Empty path → no extra words.
func CodexProfileMarkerArgs(profilePath string) []string {
	if profilePath == "" {
		return nil
	}
	return []string{profilePath}
}

// launchDetachedTmuxSession creates the detached tmux session that hosts a
// launch, wiring the child's stdout/stderr through. cmd is the harness
// command (claudeSpawner's BuildCommand output) or the plain shell's exec
// line. markerArgs are appended to the pane argv after the script path as
// inert positionals (see CodexProfileMarkerArgs). Shared by runNew,
// runNewShell and (via LaunchDetachedTmuxSession) the conv resume paths.
//
// The command is deliberately NOT passed inline as `sh -c <cmd>`: the tmux
// client hard-caps the initial command's total argv at ~16KB (see
// tmuxClientArgvLimit) and the inline form scaled with environment size,
// sandbox rule count, path length and prompt length — a fully-loaded spawn
// measured ~18.8KB and died with tmux's cryptic "command too long". The
// bootstrap goes to a private self-deleting script instead and the pane runs
// `<shell> <script> [markers]`, which keeps the tmux argv O(1) in all of the
// above. The pre-flight check below can then only trip on a pathological
// session name / cwd, and fails with an error that names them.
//
// Known trade-off: tmux respawn-pane / resurrect plugins re-run
// #{pane_start_command}, and the script has deleted itself by then — the
// respawned pane exits immediately. Managed panes are relaunched through
// tclaude (`session new -r` / agent resume), which builds a fresh script, so
// only manual out-of-band respawns are affected.
func launchDetachedTmuxSession(tmuxSession, cwd, cmd string, markerArgs ...string) error {
	_, err := launchDetachedTmuxSessionOutput(tmuxSession, cwd, cmd, false, markerArgs...)
	return err
}

// launchDetachedTmuxSessionOutput is launchDetachedTmuxSession with the tmux
// client's output returned; printID asks new-session to print the session id.
func launchDetachedTmuxSessionOutput(tmuxSession, cwd, cmd string, printID bool, markerArgs ...string) (string, error) {
	// Never launch tmux from a dead working directory. If this process's cwd
	// has been deleted (e.g. the daemon that forked us was started from a
	// since-removed dir — Ansible's task tmpdir being the observed case), a
	// FRESH tmux server inherits it, and tmux (observed on 3.7b) then silently
	// fails to honor `new-session -c`: the pane starts in the deleted dir and
	// the harness dies on getcwd() at startup (claude/Bun exits with a bare
	// ENOENT, codex likewise). The pane's exit takes the whole per-spawn server
	// with it, so nothing is left to inspect and the spawn just never enrolls.
	// Re-home to the session's own cwd, which callers have already resolved.
	if _, err := os.Getwd(); err != nil {
		slog.Warn("tmux launch: process cwd is gone; re-homing before starting tmux",
			"session", tmuxSession, "new_cwd", cwd, "getwd_error", err)
		if cerr := os.Chdir(cwd); cerr != nil {
			return "", fmt.Errorf("process cwd is gone and re-homing to %q failed: %w", cwd, cerr)
		}
	}
	if err := RequireExternalTmuxServer(); err != nil {
		return "", err
	}
	scriptPath, cleanupScript, err := writeLaunchScript(cmd)
	if err != nil {
		return "", err
	}
	// Multi-word command → tmux execvp's it directly (spawn.c), no extra
	// shell join/quoting layer. The explicit -x/-y pin the pane to the
	// canonical size (TCL-1136) instead of tmux's default-size, which the
	// operator's tmux.conf can change out from under every harness.
	args := []string{"new-session", "-d"}
	if printID {
		args = append(args, "-P", "-F", "#{session_id}")
	}
	args = append(args, "-s", tmuxSession, "-c", cwd,
		"-x", strconv.Itoa(clcommon.CanonicalAgentPaneWidth),
		"-y", strconv.Itoa(clcommon.CanonicalAgentPaneHeight))
	args = append(args, clcommon.BootstrapShellArgv()...)
	args = append(args, scriptPath)
	args = append(args, markerArgs...)
	args = ExternalTmuxNoStartArgs(args...)
	if n := tmuxArgvBytes(args); n > tmuxClientArgvLimit {
		cleanupScript()
		return "", fmt.Errorf("tmux launch argv is %d bytes, over tclaude's %d-byte pre-flight bound "+
			"(tmux's client rejects ~16KB with an opaque \"command too long\"): "+
			"session name %d bytes, launch dir %d bytes, script path %d bytes — shorten the launch directory path",
			n, tmuxClientArgvLimit, len(tmuxSession), len(cwd), len(scriptPath))
	}
	// The tmux client's own diagnostic travels in the error rather than onto
	// this process's terminal. `new-session -d` prints nothing on success (bar the -P id), and
	// on failure ("bad session name", "can't create socket") the message is the
	// whole explanation — attaching it to the error keeps it with the caller
	// that can render it. Writing it to os.Stderr instead would land it
	// wherever this process's stderr happens to point, which since
	// StartShellSession includes an agentd console sitting in bubbletea's
	// alternate screen: the diagnostic would overwrite the rendered frame while
	// the console separately reported the same failure.
	out, err := clcommon.TmuxCommand(args...).CombinedOutput()
	if err != nil {
		// The pane never ran, so the script's self-delete never did either.
		cleanupScript()
		if detail := strings.TrimSpace(string(out)); detail != "" {
			return "", fmt.Errorf("failed to create tmux session: %w: %s", err, detail)
		}
		return "", fmt.Errorf("failed to create tmux session: %w", err)
	}
	return string(out), nil
}

// isValidSpawnCwdProofToken accepts the daemon's challenge token alphabet
// before the value enters a sh -c command. The larger length bound leaves room
// for a wire-compatible format revision.
func isValidSpawnCwdProofToken(proof string) bool {
	if len(proof) == 0 || len(proof) > 128 {
		return false
	}
	for _, r := range proof {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return true
}

// guardHarnessCommandWithDirProof prefixes the harness command with marker
// checks performed by the shell tmux starts. The relative cwd check binds to
// tmux's already-established directory inode. Every caller-selected permission
// root is also required to remain canonical and carry the same unpredictable
// marker, so the child never consumes a path substituted after daemon
// verification. Profile roots skip the marker requirement but retain the late
// path-substitution check.
func guardHarnessCommandWithDirProof(
	harnessCmd, proof, readyPath string,
	checkCwd bool,
	dirs, integrityDirs, integrityFiles []string,
) string {
	marker := clcommon.SpawnDirWriteProofPrefix + proof
	ready := clcommon.ShellQuoteArg(readyPath)
	fail := func(reason string) string {
		return "printf '%s' " + clcommon.ShellQuoteArg("error:"+reason) + " > " + ready + "; exit 126"
	}
	guard := ""
	if checkCwd {
		guard = "tclaude_cwd_proof=" + clcommon.ShellQuoteArg(marker) + "; " +
			"if [ ! -f \"$tclaude_cwd_proof\" ] || [ -L \"$tclaude_cwd_proof\" ] || [ -s \"$tclaude_cwd_proof\" ]; then " +
			"echo 'tclaude: spawn cwd write proof missing or invalid; refusing harness launch' >&2; " + fail("proof") + "; fi; "
	}
	for _, dir := range dirs {
		quotedDir := clcommon.ShellQuoteArg(dir)
		quotedMarker := clcommon.ShellQuoteArg(filepath.Join(dir, marker))
		guard += "if [ \"$(cd " + quotedDir + " 2>/dev/null && pwd -P)\" != " + quotedDir +
			" ] || [ ! -f " + quotedMarker + " ] || [ -L " + quotedMarker + " ] || [ -s " + quotedMarker + " ]; then " +
			"echo 'tclaude: repository write proof changed; refusing harness launch' >&2; " + fail("repository-proof") + "; fi; "
	}
	for _, dir := range integrityDirs {
		quotedDir := clcommon.ShellQuoteArg(dir)
		guard += "if [ \"$(cd " + quotedDir + " 2>/dev/null && pwd -P)\" != " + quotedDir +
			" ] || [ -L " + quotedDir + " ]; then " +
			"echo 'tclaude: sandbox profile directory changed; refusing harness launch' >&2; " + fail("repository-proof") + "; fi; "
	}
	// The same check for a rule naming a single file. `cd` cannot express it, so
	// the file form asks the two questions that remain answerable: the path is
	// still a regular file, and it is still not a symlink pointing somewhere the
	// profile never resolved.
	for _, file := range integrityFiles {
		quotedFile := clcommon.ShellQuoteArg(file)
		guard += "if [ ! -f " + quotedFile + " ] || [ -L " + quotedFile + " ]; then " +
			"echo 'tclaude: sandbox profile file changed; refusing harness launch' >&2; " + fail("repository-proof") + "; fi; "
	}
	return guard + "printf '%s' ok > " + ready + " || exit 126; " + harnessCmd
}

func newSpawnCwdReadinessFile() (string, func(), error) {
	// This file is written by the unsandboxed tmux bootstrap shell before it
	// execs the harness (and therefore before the harness sandbox exists), then
	// read by the unsandboxed parent. Keep it with private daemon state rather
	// than leaving proof status files at the agent-readable tclaude root.
	base := strings.TrimSpace(config.DataDir())
	if base == "" {
		return "", func() {}, fmt.Errorf("resolve private spawn-readiness directory")
	}
	dir := filepath.Join(base, "spawn-readiness")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", func() {}, fmt.Errorf("create spawn-readiness directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", func() {}, fmt.Errorf("protect spawn-readiness directory: %w", err)
	}
	f, err := os.CreateTemp(dir, "cwd-proof-")
	if err != nil {
		return "", func() {}, fmt.Errorf("create cwd-proof readiness file: %w", err)
	}
	path := f.Name()
	cleanup := func() { _ = os.Remove(path) }
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		cleanup()
		return "", func() {}, fmt.Errorf("protect cwd-proof readiness file: %w", err)
	}
	if _, err := f.WriteString("pending"); err != nil {
		_ = f.Close()
		cleanup()
		return "", func() {}, fmt.Errorf("initialize cwd-proof readiness file: %w", err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("close cwd-proof readiness file: %w", err)
	}
	return path, cleanup, nil
}

func waitForSpawnCwdReadiness(path string) error {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read cwd-proof launch readiness: %w", err)
		}
		status := strings.TrimSpace(string(raw))
		if status == "ok" {
			return nil
		}
		if strings.HasPrefix(status, "error:") {
			return fmt.Errorf("spawn cwd bootstrap refused launch (%s)", strings.TrimPrefix(status, "error:"))
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for spawn cwd bootstrap")
}

// applyTmuxWindowTitle sets this session's tmux window title to
// `tclaude:<sessionID>` when config focus.window_title is enabled (default on),
// so the title persists and drives title-based window focus/tiling. An explicit
// false leaves the terminal's own title alone. Best-effort. Shared by both
// launch paths.
func applyTmuxWindowTitle(tmuxSession, sessionID string) {
	if !windowTitleEnabledFn() {
		return
	}
	_ = clcommon.TmuxCommand("set-option", "-t", clcommon.ExactTarget(tmuxSession)+":", "set-titles", "on").Run()
	_ = clcommon.TmuxCommand("set-option", "-t", clcommon.ExactTarget(tmuxSession)+":", "set-titles-string", fmt.Sprintf("tclaude:%s", sessionID)).Run()
}

// announceAndAttach prints the created-session summary and then either reports
// the detach hint (detached launches) or attaches to the pane. createdLine is
// the harness-specific "Created …" headline (a coding session vs a shell
// session). Shared launch tail of runNew and runNewShell.
func announceAndAttach(createdLine, sessionID, tmuxSession, cwd string, detached bool) error {
	fmt.Println(createdLine)
	fmt.Printf("  Directory: %s\n", cwd)

	if detached {
		fmt.Printf("\nAttach with: tclaude session attach %s\n", tmuxSession)
		return nil
	}

	fmt.Println("\nAttaching... (Ctrl+B D to detach)")
	return AttachToSession(sessionID, tmuxSession, false)
}

// resolveResumeConv resolves a --resume id prefix to a full conversation
// id + its project path, using the harness's own conversation source:
// Claude Code keeps the established cwd-indexed resolver
// (clcommon.ResolveConvID, unchanged); any other harness resolves through
// its ConvStore (Codex reads its rollout files + state DB, not
// ~/.claude/projects). A conversation that doesn't resolve is an error;
// `global` widens the search beyond the current project.
func resolveResumeConv(h *harness.Harness, shortID string, global bool, cwd string) (fullConvID, projectPath string, err error) {
	if h.Name == harness.DefaultName {
		convInfo := clcommon.ResolveConvID(shortID, global, cwd)
		if convInfo == nil {
			return "", "", resumeNotFoundErr(shortID, global)
		}
		return convInfo.SessionID, convInfo.ProjectPath, nil
	}
	// OpenCode conversation discovery/listing is TCL-669. Managed relaunches
	// already carry the full server-issued id and the durable cwd, so they can
	// resume without pretending a partial-id ConvStore exists.
	if h.Name == harness.OpenCodeName && strings.HasPrefix(shortID, "ses_") {
		return shortID, cwd, nil
	}
	// A harness with ResumeFullIDWithoutStore (Gemini CLI): a FULL session
	// UUID plus the caller's cwd is everything `gemini --resume <uuid>` needs — Gemini resolves the id itself, scoped to
	// the project of the process cwd, which is the cwd the pane is launched in.
	// Managed relaunches always carry both (the recorded conv id and `-C` with
	// the recorded cwd), so they must not depend on tclaude's own reading of
	// Gemini's store: that reading mirrors Gemini's "has resumable content"
	// rule and so cannot see a conversation whose first turn never landed,
	// which Gemini will then report itself. The store is still consulted first
	// when there is one, because it knows the conversation's real project.
	if h.ResumesFullIDWithoutStore() && clcommon.IsValidUUID(shortID) {
		if h.SupportsConvs() {
			if ref, err := h.Convs.Resolve(shortID, cwd, true); err == nil && ref != nil {
				return ref.ConvID, ref.ProjectPath, nil
			}
		}
		return shortID, cwd, nil
	}
	if !h.SupportsConvs() {
		return "", "", fmt.Errorf("harness %q cannot resolve a conversation to resume", h.Name)
	}
	ref, err := h.Convs.Resolve(shortID, cwd, global)
	if err != nil {
		return "", "", err
	}
	if ref == nil {
		return "", "", resumeNotFoundErr(shortID, global)
	}
	return ref.ConvID, ref.ProjectPath, nil
}

// validatePresetSessionID checks an explicit `--session-id` against the
// harness's declared PresetSessionIDKind, so a malformed or unsupported id
// fails cleanly rather than at harness launch. It is mutually exclusive with
// --resume (a resume already has an id).
func validatePresetSessionID(h *harness.Harness, sessionID, resume string, managedLaunch bool) error {
	if sessionID == "" {
		return nil
	}
	if resume != "" {
		return fmt.Errorf("--session-id cannot be combined with --resume")
	}
	switch h.PresetSessionIDKind() {
	case harness.PresetSessionIDUUID:
		if !clcommon.IsValidUUID(sessionID) {
			return fmt.Errorf("--session-id must be a valid UUID, got %q", sessionID)
		}
	case harness.PresetSessionIDManagedServer:
		if !managedLaunch || !strings.HasPrefix(sessionID, "ses_") {
			return fmt.Errorf("--session-id for %s is an internal agentd-minted ses_ id", h.DisplayName)
		}
	default:
		return fmt.Errorf("--session-id is not supported for the %q harness", h.Name)
	}
	return nil
}

func resumeIDFromParam(h *harness.Harness, value string) string {
	value = strings.TrimSpace(value)
	if h.Name == harness.OpenCodeName && strings.HasPrefix(value, "ses_") {
		return value
	}
	return clcommon.ExtractIDFromCompletion(value)
}

// errResumeNotFound marks a selector that matched nothing, as opposed to an
// ambiguous one or an unreadable store.
var errResumeNotFound = errors.New("not found")

func resumeNotFoundErr(shortID string, global bool) error {
	if global {
		return fmt.Errorf("conversation %s %w", shortID, errResumeNotFound)
	}
	return fmt.Errorf("conversation %s %w in current project (use -g to search all projects)", shortID, errResumeNotFound)
}
