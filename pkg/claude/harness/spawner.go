package harness

// SpawnSpec is a harness-agnostic description of a session launch. The
// caller (e.g. `tclaude session new`) owns the env and resolves the
// resume id; the Spawner turns this into the concrete shell command the
// tmux pane runs. Fields left at their zero value are omitted from the
// command, so "unset" reliably means "let the harness use its own
// default".
type SpawnSpec struct {
	// EnvExports is a pre-built `export K=V; …` prefix prepended verbatim
	// to the command. The caller assembles it (tclaude identity env +
	// any pass-through), so the Spawner stays agnostic about which vars
	// matter to which harness.
	EnvExports string
	// ResumeID is the full conversation id to resume, or "" to start a
	// fresh session. The flag/sub-command form is harness-specific
	// (`claude --resume <id>` vs `codex resume <id>`).
	ResumeID string
	// Effort is a validated, normalized reasoning-effort token, or "" to
	// omit the flag entirely. Validate via ModelCatalog.ValidateEffort
	// before building the spec.
	Effort string
	// Model is a validated, normalized model token, or "" to omit the
	// flag entirely. Validate via ModelCatalog.ValidateModel first.
	Model string
	// ExtraArgs are post-`--` pass-through args, appended last and
	// shell-quoted individually by the Spawner.
	ExtraArgs []string
	// BypassHookTrust, when true, asks the harness to run its configured
	// hooks without requiring persisted hook trust for this invocation —
	// a headless escape hatch for automation that already vets its hook
	// sources. Codex maps this to `--dangerously-bypass-hook-trust`;
	// harnesses without the concept ignore it. Defaults to false (trust is
	// enforced); it is a deliberate supply-chain trade-off (repo-local
	// `./.codex` hooks become trusted), so callers opt in explicitly.
	BypassHookTrust bool
	// SandboxMode names the launch-time OS-sandbox mode for harnesses that
	// take one (Codex's `--sandbox {read-only|workspace-write|
	// danger-full-access}`). "" omits the flag entirely; the Spawner emits
	// `--sandbox <mode>` per-spawn so the user's config.toml/profiles stay
	// untouched. Harnesses without a launch sandbox flag (Claude Code —
	// settings.json-driven) ignore it. Validate via Harness.Sandbox /
	// ResolveSandboxMode before building the spec. See JOH-192.
	SandboxMode string
	// PermissionProfile names a tclaude-managed Codex permission profile to run
	// under, realised as `codex -p <name>` (a layered config-profile file whose
	// default_permissions activates the profile for this spawn only). It is the
	// path the daemon uses to keep a sandboxed Codex agent able to reach the
	// agentd socket (JOH-207): unlike `--sandbox`, a permission profile can
	// allowlist that one Unix socket. It is MUTUALLY EXCLUSIVE with SandboxMode
	// — Codex ignores permission profiles whenever a `--sandbox`/sandbox_mode is
	// present — so the spec builder sets one or the other; the Spawner emits
	// `-p` and omits `--sandbox` when this is set. "" omits it. Harnesses with
	// no permission-profile concept (Claude Code) ignore it. See JOH-207 +
	// harness.CodexAgentProfile.
	PermissionProfile string
	// ApprovalPolicy names the launch-time approval policy for harnesses that
	// take one (Codex's `--ask-for-approval {untrusted|on-failure|on-request|
	// never}`). "" omits the flag entirely; the Spawner emits
	// `--ask-for-approval <policy>` per-spawn so the user's config.toml/
	// profiles stay untouched. The daemon spawn path resolves it to the
	// harness's non-escalating default (Codex: `never`) so an unattended pane
	// can't deadlock on an approval prompt; harnesses without a launch
	// approval flag (Claude Code) ignore it. Validate via Harness.Approval /
	// ResolveApprovalPolicy before building the spec. See JOH-200.
	ApprovalPolicy string
	// AutoReview, when true, asks the harness to route approval prompts to its
	// guardian subagent (which auto-decides in the human's place) instead of
	// the human. Codex maps this to `-c approvals_reviewer=auto_review`;
	// harnesses without a guardian (Claude Code) ignore it. It is an
	// orthogonal axis to ApprovalPolicy — that decides WHEN Codex asks, this
	// decides WHO answers — so the two compose. Defaults to false (the human,
	// Codex's `user` default); it is an experimental, undocumented-upstream
	// opt-in, so callers enable it explicitly. Gate via Harness.Approval /
	// ResolveAutoReview before building the spec. See JOH-200 part 2.
	AutoReview bool
	// InitialPrompt is an optional first-turn prompt the harness submits
	// ITSELF at launch (the harness's own positional [PROMPT] arg) — not a
	// tclaude send-keys injection. It exists for a harness whose conv-id is
	// only knowable after the first turn (Codex generates the id at launch
	// but persists/exposes it — rollout file, threads row, hooks — only once
	// a turn runs; see JOH-205): seeding a turn at launch lets the conv-id
	// materialise without a human typing the first message, while keeping
	// tclaude hands-off the pane until a hook/file confirms the session is
	// past its startup gates (dir-trust / hooks-config / auth prompts). The
	// harness self-submits, so the seed safely queues behind those modals.
	// "" omits it; harnesses that report their conv-id at launch (Claude
	// Code's SessionStart hook) ignore it. Emitted only for a fresh launch,
	// never a resume.
	InitialPrompt string
}

// Spawner builds the in-tmux launch command for a harness from a
// SpawnSpec, and names the harness binary for the pass-through path
// (`--help`/`--version`, run directly without tmux).
type Spawner interface {
	// Binary is the executable name (e.g. "claude", "codex").
	Binary() string
	// BuildCommand returns the shell command string the tmux pane runs
	// under `sh -c`. The result must be safe to hand to `sh -c`: any
	// value that could carry shell metacharacters (model aliases with
	// `[1m]` brackets, pass-through args) is shell-quoted by the
	// implementation.
	BuildCommand(spec SpawnSpec) string
}
