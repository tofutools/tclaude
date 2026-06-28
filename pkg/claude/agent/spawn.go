package agent

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/common"
)

// SpawnResponse is the daemon's response shape for
// POST /v1/groups/{name}/spawn — used by both `tclaude agent spawn`
// and `tclaude --join-group`. Mirrors the keys handleGroupSpawn writes
// in pkg/claude/agentd/lifecycle.go.
type SpawnResponse struct {
	Group       string `json:"group"`
	ConvID      string `json:"conv_id"`
	Label       string `json:"label"`
	TmuxSession string `json:"tmux_session"`
	AttachCmd   string `json:"attach_cmd"`
}

// SpawnRequest is the JSON body of POST /v1/groups/{name}/spawn — the
// single shared request shape behind every spawn surface. The
// `tclaude agent spawn` CLI, `tclaude --join-group`, and the agentd
// dashboard's spawn modal all marshal one of these; agentd's
// handleGroupSpawn decodes it. One type means the CLI and the
// dashboard cannot drift in which fields the daemon understands.
type SpawnRequest struct {
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
	// default; a non-empty value must be one of clcommon.ValidEffortLevels.
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

	// SandboxMode picks the launch containment for a harness that takes one
	// (Codex). Empty resolves to the harness's secure default — for Codex the
	// managed tclaude-agent profile (workspace-write containment PLUS the
	// agentd-socket allowlist, so the agent can still run `tclaude agent …`),
	// forwarded as `--permission-profile tclaude-agent`. The raw Codex modes
	// (workspace-write|read-only|danger-full-access) are forwarded as `tclaude
	// session new --sandbox <mode>` and do NOT get the socket allowlist;
	// danger-full-access opts out of the sandbox entirely. Not applicable to
	// Claude Code (settings.json-driven), which rejects a non-empty value. See
	// JOH-192 / JOH-207.
	SandboxMode string `json:"sandbox,omitempty"`

	// ApprovalPolicy picks the launch-time approval policy for a harness that
	// takes one (Codex's --ask-for-approval). Empty resolves to the harness's
	// non-escalating default (Codex: never), so a daemon-owned Codex agent —
	// detached, with no human at its TUI — never deadlocks on an approval
	// prompt no one can answer. Forwarded to `tclaude session new
	// --ask-for-approval <policy>`. Not applicable to Claude Code
	// (settings.json-driven), which rejects a non-empty value. See JOH-200.
	ApprovalPolicy string `json:"approval,omitempty"`

	// AutoReview opts the spawned agent into the harness's guardian subagent
	// (Codex's `-c approvals_reviewer=auto_review`), which auto-decides approval
	// prompts in the human's place — the orthogonal "who answers" axis to
	// ApprovalPolicy's "when to ask". false (the default) keeps the human as
	// reviewer. The daemon gates it on the chosen harness having an approvals
	// subsystem (Codex); requesting it for Claude Code is a 400. Forwarded to
	// `tclaude session new --auto-review`. Experimental/undocumented upstream,
	// hence an explicit opt-in. See JOH-200 part 2.
	AutoReview bool `json:"auto_review,omitempty"`

	// TrustDir opts the spawned agent into pre-trusting its launch directory
	// for Codex: the daemon writes [projects."<cwd>"] trust_level = "trusted"
	// into the user's ~/.codex/config.toml BEFORE launch, so a detached pane
	// doesn't freeze on Codex's "do you trust this folder?" onboarding modal
	// (JOH-205). false (the default) leaves the modal in place — to be cleared
	// by focusing the pending pane. Unlike SandboxMode/ApprovalPolicy this
	// edits the user's config.toml, so it is strictly opt-in (dashboard
	// checkbox / `--trust-dir`) and NEVER defaulted. Codex-only; requesting it
	// for Claude Code is a 400. Forwarded to `tclaude session new --trust-dir`.
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
}

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

	// Effort and Model are declared last so boa's short-flag enricher
	// (which assigns the first free letter in field order) cannot steal
	// a short from any existing field. No explicit shorts — `--effort`
	// and `--model` only.
	Effort string `long:"effort" optional:"true" help:"Reasoning effort for the new agent: low|medium|high|xhigh|max. Unset = the harness's own default (no flag passed)"`
	Model  string `long:"model" optional:"true" help:"Model for the new agent. Claude: fable|fable[1m]|opus|opus[1m]|sonnet|sonnet[1m]|haiku|opusplan or a full model ID. Codex: a codex model name. Unset = the group's default model, else the harness's own default"`

	// Harness picks the coding harness the new agent runs. Declared last
	// (no explicit short) for the same reason as Effort/Model — boa's
	// short-flag enricher must not steal a letter from an existing field.
	Harness string `long:"harness" optional:"true" help:"Coding harness for the new agent: claude (default) | codex. Effort/model are validated against the chosen harness's own rules"`

	// Sandbox is the launch-time OS-sandbox mode for the new agent. Codex takes
	// a native --sandbox enum; Claude Code has no launch flag, so its
	// inherit/on/off modes are delivered as a `--settings` override (see
	// harness.claudeSandbox). Declared last (no explicit short) for the same
	// reason as the fields above.
	Sandbox string `long:"sandbox" optional:"true" help:"Launch containment for the new agent (per-harness modes). Codex: tclaude-agent (managed profile, keeps agentd reachable) | workspace-write | read-only | danger-full-access. Claude Code: inherit (use settings.json as-is) | on (force the OS sandbox on via --settings, agentd reachable) | off. Unset = the harness default (Codex: tclaude-agent; Claude: inherit)"`

	// Approval is the launch-time approval/permission posture for the new
	// agent. Codex takes an --ask-for-approval policy; Claude Code's approval
	// posture is its --permission-mode, carried through this same field (the
	// spawner emits the harness-appropriate flag). Declared last (no explicit
	// short) for the same reason as the fields above. Unset resolves to each
	// harness's safe default (Codex: never; Claude: inherit — no override).
	Approval string `long:"ask-for-approval" optional:"true" help:"Launch approval/permission posture for the new agent (per-harness). Codex policy: untrusted|on-failure|on-request|never. Claude Code permission mode: inherit|plan|acceptEdits|default|auto|dontAsk|bypassPermissions. Unset = the harness's safe default (Codex: never, so it can't block on a prompt; Claude: inherit)"`

	// AutoReview is a bool flag declared last (no explicit short) for the same
	// reason as the fields above — boa's short-flag enricher must not steal a
	// letter. Experimental opt-in (off by default). See JOH-200 part 2.
	AutoReview bool `long:"auto-review" help:"EXPERIMENTAL: route the new agent's Codex approval prompts to the guardian subagent (auto-decides in your place) instead of asking you. Off by default. Not applicable to claude"`

	RemoteControl bool `long:"remote-control" help:"Start the new agent with Claude Code Remote Access ON (claude --remote-control), so it is reachable from the Claude app. Off by default. Requires a claude.ai login to pair. Not applicable to codex"`
}

// spawnCmd starts a fresh CC session and registers it in an existing
// group in one shot. Useful for "I want to delegate this in parallel"
// flows where you want the new agent to be reachable by name from the
// existing team without manually wiring up membership after the fact.
func spawnCmd() *cobra.Command {
	return boa.CmdT[SpawnParams]{
		Use:   "spawn",
		Short: "Spawn a fresh CC session and add it to an existing group",
		Long: "Launches `tclaude session new -d --global` with a generated label, " +
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
			"welcome message. " +
			"\n\n" +
			"Requires the `groups.spawn` permission (default: human-only).",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *SpawnParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Group).SetAlternativesFunc(completeGroupNames)
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *SpawnParams, _ *cobra.Command, _ []string) {
			_, rc := RunSpawn(p, os.Stdout, os.Stderr, os.Stdin)
			os.Exit(rc)
		},
	}.ToCobra()
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
	initialMessage := strings.TrimSpace(rawMessage)
	if !isValidInitialMessage(initialMessage) {
		fmt.Fprintf(stderr, "Error: REJECTED. --initial-message must be at most %d characters.\n", MaxInitialMessageBytes)
		fmt.Fprintln(stderr, "Newlines and tabs are allowed (the brief is delivered to the agent's")
		fmt.Fprintln(stderr, "inbox, not typed into its pane), but other control characters are not.")
		return nil, rcInvalidArg
	}
	// Validate --name client-side so a bad name fails fast with a clear
	// message instead of reaching the daemon. An empty name is fine (the
	// agent gets an auto-generated label); a non-empty one must be a safe
	// token. The daemon re-validates server-side (handleGroupSpawn).
	//
	// When config's agent.spawn_name_normalize is on (the default), coerce a
	// bad name to the safe charset instead of rejecting it — so `tclaude
	// agent spawn --name "code reviewer"` "just works" the same way the
	// dashboard modal does. The daemon re-normalizes (idempotent) as the
	// authoritative backstop. Disabled (explicit false) keeps the strict
	// reject below.
	name := strings.TrimSpace(p.Name)
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
	ask, err := ParseAskHuman(p.AskHuman)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return nil, rcInvalidArg
	}
	// Resolve --harness (default Claude Code) so --effort/--model are
	// validated against the chosen harness's own rules — a Codex spawn
	// accepts a Codex model and rejects a Claude Code slug, and vice
	// versa. An unknown/not-spawnable harness fails fast here.
	h, err := harness.ResolveSpawnable(strings.TrimSpace(p.Harness))
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return nil, rcInvalidArg
	}
	// Validate --effort client-side so a typo fails fast with a clear
	// message instead of reaching the daemon (where an invalid level
	// would otherwise surface only as a conv-id-poll timeout once the
	// forked `tclaude session new --effort <bad>` exits non-zero).
	effort, err := h.Models.ValidateEffort(p.Effort)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return nil, rcInvalidArg
	}
	// Same fail-fast treatment for --model.
	model, err := h.Models.ValidateModel(p.Model)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return nil, rcInvalidArg
	}
	// Resolve --sandbox to the harness's secure default (Codex:
	// workspace-write) when unset, validate an explicit value, and reject a
	// mode for a harness with no launch sandbox flag (claude). The daemon
	// re-resolves + applies the cwd-safety guard server-side.
	sandboxMode, err := harness.ResolveSandboxMode(h, p.Sandbox)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return nil, rcInvalidArg
	}
	// Resolve --ask-for-approval to the harness's non-escalating default
	// (Codex: never) when unset, validate an explicit value, and reject a
	// policy for a harness with no launch approval flag (claude). The non-
	// escalating default is what keeps the detached, unattended pane from
	// deadlocking on an approval prompt (JOH-200). The daemon re-resolves it
	// server-side.
	approvalPolicy, err := harness.ResolveApprovalPolicy(h, p.Approval)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return nil, rcInvalidArg
	}
	// Gate the experimental --auto-review opt-in: allowed only for a harness
	// with an approvals subsystem (Codex); requesting it for claude fails fast
	// here with a clear message. The daemon re-gates server-side.
	autoReview, err := harness.ResolveAutoReview(h, p.AutoReview)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return nil, rcInvalidArg
	}
	// Gate --remote-control: arming Remote Access at launch is a Claude Code
	// feature, so requesting it for a harness without it (Codex) fails fast here
	// with a clear message. The daemon re-gates server-side. See JOH-258.
	remoteControl, err := harness.ResolveRemoteControl(h, p.RemoteControl)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return nil, rcInvalidArg
	}
	cwd := p.Cwd
	if cwd == "" {
		if wd, err := os.Getwd(); err == nil {
			cwd = wd
		}
	}

	req := SpawnRequest{
		Name:           name,
		Role:           p.Role,
		Descr:          p.Descr,
		InitialMessage: initialMessage,
		ReplyTo:        strings.TrimSpace(p.ReplyTo),
		Cwd:            cwd,
		TimeoutSeconds: timeoutSeconds,
		AutoFocus:      p.AutoFocus,
		Effort:         effort,
		Model:          model,
		Harness:        h.Name,
		SandboxMode:    sandboxMode,
		ApprovalPolicy: approvalPolicy,
		AutoReview:     autoReview,
	}
	// --remote-control is opt-in only on the CLI: send &true when the flag is set,
	// and leave the pointer nil otherwise so the daemon's group/profile
	// remote-control policy fills it (the dashboard form is the surface that sends
	// an explicit false to override an inherited default). See SpawnRequest.RemoteControl.
	if remoteControl {
		on := true
		req.RemoteControl = &on
	}
	// --no-group-context maps to an explicit `false` on the wire; an
	// omitted pointer means opt-in, so the default (no flag) lets the
	// daemon include the group context as every other spawn path does.
	if p.NoGroupContext {
		no := false
		req.IncludeGroupContext = &no
	}

	// Worktree handling. The CLI resolves the worktree itself — creating
	// it in-process, the same git operation the dashboard's worktree
	// picker performs server-side — then passes the resolved cwd /
	// worktree_path / worktree_branch, the identical wire shape the
	// dashboard sends. createdWorktree is non-empty only when a fresh
	// worktree was made (vs an existing one reused), so a failed spawn
	// can tear it back down rather than leaking an orphan.
	createdWorktree := ""
	if wt := strings.TrimSpace(p.Worktree); wt != "" {
		worktreeRepo := strings.TrimSpace(p.WorktreeRepo)
		if worktreeRepo == "" {
			worktreeRepo = cwd
		}
		wtPath, createdNew, wtErr := resolveSpawnWorktree(worktreeRepo, wt, p.WorktreeBase)
		if wtErr != nil {
			fmt.Fprintf(stderr, "Error: %v\n", wtErr)
			return nil, rcInvalidArg
		}
		if createdNew {
			createdWorktree = wtPath
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
		fmt.Fprintf(stdout, "Waiting up to %s for human approval...\n", ask)
	}
	path := "/v1/groups/" + p.Group + "/spawn"
	if err := DaemonRequest(http.MethodPost, path, req, &resp, DaemonOpts{AskHuman: ask}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		// The spawn failed after we created a worktree for it. Remove the
		// now-orphaned worktree so a retry starts clean — except on a 504
		// conv-id-poll timeout: there the spawn subprocess DID launch and
		// the new CC may still be coming up inside the worktree, so
		// force-removing the dir would yank it out from under a
		// recovering session. Every other failure (guardrail rejection,
		// launch failure, bad arg) happens before any CC runs there, so
		// the worktree is a guaranteed orphan. The branch is always kept
		// (removeSpawnWorktree only drops the working dir), so a retry
		// reuses it.
		if createdWorktree != "" {
			if de, ok := err.(*DaemonError); ok && de.Status == http.StatusGatewayTimeout {
				fmt.Fprintf(stderr, "Note: kept the worktree %s — the session may still be coming up.\n",
					createdWorktree)
			} else if _, rmErr := removeSpawnWorktree(createdWorktree); rmErr != nil {
				fmt.Fprintf(stderr, "Note: could not remove the worktree created for this spawn (%s): %v\n",
					createdWorktree, rmErr)
			} else {
				fmt.Fprintf(stderr, "Note: removed the worktree created for this spawn (%s)\n", createdWorktree)
			}
		}
		return nil, MapDaemonErrorToRC(err)
	}
	fmt.Fprintf(stdout, "Spawned %s in group %q\n", short(resp.ConvID), resp.Group)
	if resp.Label != "" {
		fmt.Fprintf(stdout, "  Label:   %s\n", resp.Label)
	}
	if resp.TmuxSession != "" {
		fmt.Fprintf(stdout, "  Tmux:    %s\n", resp.TmuxSession)
	}
	if resp.AttachCmd != "" {
		fmt.Fprintf(stdout, "  Attach:  %s\n", resp.AttachCmd)
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
