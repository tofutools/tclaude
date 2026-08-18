package harness

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
)

// codexSpawner builds the `codex` invocation that runs inside the tmux
// pane (JOH-154). The shape mirrors claudeSpawner but for Codex CLI's
// command model — most notably resume is a SUBCOMMAND (`codex resume
// <id>`), not a flag like Claude Code's `claude --resume <id>`.
//
// Like the CC spawner it stays pure (string in → string out) so the
// "unset omits the flag" guarantee is unit-testable without tmux, and it
// shell-quotes everything handed to `sh -c`.
type codexSpawner struct{}

func (codexSpawner) Binary() string { return "codex" }

// BuildCommand assembles the Codex invocation: env exports + the binary,
// the `resume <id>` subcommand when resuming, an optional
// `--dangerously-bypass-hook-trust`, an optional `--sandbox <mode>`, an
// optional `--model`, then any post-`--` passthrough args.
//
// Working directory is NOT passed via `-C/--cd`: tclaude launches the
// pane with `tmux new-session -c <cwd>`, and Codex uses the pane's cwd —
// the same way the CC spawner relies on tmux for cwd. Effort maps onto
// Codex's reasoning-effort config (JOH-155). Sandbox mode is a per-spawn
// `--sandbox` flag (JOH-192) — resolved/validated at the spawn boundary
// (ResolveHarnessBuiltinMode) and emitted verbatim here, so the user's config.toml
// sandbox_mode/profiles stay untouched.
func (codexSpawner) BuildCommand(spec SpawnSpec) string {
	binary := "codex"
	if spec.ExecutablePath != "" {
		binary = clcommon.ShellQuoteArg(spec.ExecutablePath)
	}
	bypassHookTrustArg := ""
	if spec.BypassHookTrust {
		bypassHookTrustArg = " --dangerously-bypass-hook-trust"
	}
	sandboxArgs := codexSandboxArgs(spec)
	approvalArgs := codexApprovalArgs(spec)
	fastModeArgs := codexFastModeArgs(spec)
	shellEnvironmentArgs := codexShellEnvironmentArgs(spec.ShellEnvironment)
	cmd := binary
	if spec.ResumeID != "" {
		// `codex resume <id>` — resume is a subcommand; the id is a
		// positional. Quoted defensively even though it's a UUID.
		cmd += " resume " + clcommon.ShellQuoteArg(spec.ResumeID)
	}
	if bypassHookTrustArg != "" {
		// Run configured hooks without persisted hook trust for this
		// invocation — a headless escape hatch (default off). Accepted
		// both on a fresh `codex` and on `codex resume <id>`, like
		// `--model`. No value, so nothing to quote.
		cmd += bypassHookTrustArg
	}
	cmd += sandboxArgs + approvalArgs + fastModeArgs + shellEnvironmentArgs
	if spec.Model != "" {
		// `--model` is accepted both on a fresh `codex` and on
		// `codex resume <id>` (shared option).
		cmd += " --model " + clcommon.ShellQuoteArg(spec.Model)
	}
	if spec.Effort != "" {
		// Codex has no `--effort` flag; reasoning effort is a config
		// value, set via `-c model_reasoning_effort=…`. The value is a
		// TOML-quoted string (matching Codex's own `-c model="o3"`
		// convention) and the whole `key="value"` is shell-quoted as one
		// arg. spec.Effort is a validated tclaude level; codexReasoningEffort
		// maps it onto the selected Codex model's scale.
		cmd += " -c " + clcommon.ShellQuoteArg(`model_reasoning_effort="`+codexReasoningEffort(spec.Model, spec.Effort)+`"`)
	}
	if len(spec.ExtraArgs) > 0 {
		quoted := make([]string, len(spec.ExtraArgs))
		for i, a := range spec.ExtraArgs {
			quoted[i] = clcommon.ShellQuoteArg(a)
		}
		cmd += " " + strings.Join(quoted, " ")
	}
	if spec.CodexAppServerSocket != "" {
		cmd += " --remote " + clcommon.ShellQuoteArg(spec.CodexAppServerURL) +
			" --remote-auth-token-env TCLAUDE_CODEX_APP_SERVER_TOKEN"
		// The TUI needs the capability for its own WebSocket upgrade, but model
		// tool shells must never inherit it.
		cmd += " -c " + clcommon.ShellQuoteArg(
			`shell_environment_policy.exclude=["TCLAUDE_CODEX_APP_SERVER_TOKEN"]`)
	}
	// `codex [OPTIONS] [PROMPT]` — a trailing positional the interactive TUI
	// submits itself at launch (verified against codex-cli 0.139.0:
	// "[PROMPT]  Optional user prompt to start the session"). This is how a
	// Codex spawn takes its first turn without a human keystroke, so its
	// conv-id materialises (JOH-205) — Codex self-submits, so the prompt
	// queues safely behind any startup modal (dir-trust / hooks / auth) and
	// tclaude never has to send-keys an unconfirmed pane. Only on a FRESH
	// launch: `codex resume <id>` continues an existing conversation whose
	// id is already known, so it needs no seed (and resume's positional-
	// prompt handling differs). Quoted as a single arg so the whole prompt
	// is one [PROMPT], never split into stray flags/words.
	if spec.InitialPrompt != "" && spec.ResumeID == "" {
		cmd += " " + clcommon.ShellQuoteArg(spec.InitialPrompt)
	}
	prefix := spec.EnvExports + spec.PreLaunchScript
	if spec.CodexAppServerSocket == "" {
		return prefix + cmd
	}
	// The server and TUI intentionally live under the same shell and therefore
	// the same cwd, environment, cgroup and optional outer sandbox wrapper. The
	// EXIT trap makes the server a resource of this pane generation rather than
	// a machine-global daemon. Paths are daemon-minted but still shell-quoted.
	// The app-server, not the remote TUI, owns model tool execution. Give it
	// every execution-posture argument the ordinary TUI receives: the complete
	// managed profile (or raw sandbox), stacked-backend pin, approval posture,
	// service tier, and authoritative tool environment. Codex accepts these
	// global options before the app-server subcommand.
	server := binary + codexAppServerHookTrustArgs(spec) + codexAppServerSandboxArgs(spec) +
		codexAppServerApprovalArgs(spec) + fastModeArgs + shellEnvironmentArgs
	server += " app-server --listen " +
		clcommon.ShellQuoteArg(spec.CodexAppServerURL) +
		" --ws-auth capability-token --ws-token-sha256 " +
		clcommon.ShellQuoteArg(spec.CodexAppServerTokenSHA256)
	relay := clcommon.ShellQuoteArg(spec.TclaudeExecutable) +
		" session codex-app-server-relay --socket " +
		clcommon.ShellQuoteArg(spec.CodexAppServerSocket) + " --upstream " +
		clcommon.ShellQuoteArg(strings.TrimPrefix(spec.CodexAppServerURL, "ws://"))
	pidFile := clcommon.ShellQuoteArg(spec.CodexAppServerPIDFile)
	relayPIDFile := clcommon.ShellQuoteArg(spec.CodexAppServerPIDFile + ".relay")
	logFile := clcommon.ShellQuoteArg(spec.CodexAppServerLogFile)
	proofFile := clcommon.ShellQuoteArg(filepath.Join(filepath.Dir(spec.CodexAppServerSocket), "server.proved"))
	return prefix + "umask 077; " +
		server + " >" + logFile + " 2>&1 & " +
		"tclaude_codex_server_pid=$!; " +
		"echo \"$tclaude_codex_server_pid\" >" + pidFile + "; " +
		relay + " >>" + logFile + " 2>&1 & tclaude_codex_relay_pid=$!; " +
		"echo \"$tclaude_codex_relay_pid\" >" + relayPIDFile + "; " +
		"trap 'kill \"$tclaude_codex_relay_pid\" \"$tclaude_codex_server_pid\" 2>/dev/null; wait \"$tclaude_codex_relay_pid\" \"$tclaude_codex_server_pid\" 2>/dev/null' EXIT HUP INT TERM; " +
		"tclaude_codex_wait=0; while [ ! -S " + clcommon.ShellQuoteArg(spec.CodexAppServerSocket) +
		" ] && kill -0 \"$tclaude_codex_server_pid\" 2>/dev/null && [ \"$tclaude_codex_wait\" -lt 150 ]; do " +
		"sleep 0.1; tclaude_codex_wait=$((tclaude_codex_wait + 1)); done; " +
		"[ -S " + clcommon.ShellQuoteArg(spec.CodexAppServerSocket) + " ] || exit 70; " +
		"chmod 600 " + clcommon.ShellQuoteArg(spec.CodexAppServerSocket) + " || exit 71; " +
		"tclaude_codex_wait=0; while [ ! -f " + proofFile +
		" ] && kill -0 \"$tclaude_codex_server_pid\" 2>/dev/null && [ \"$tclaude_codex_wait\" -lt 150 ]; do " +
		"sleep 0.1; tclaude_codex_wait=$((tclaude_codex_wait + 1)); done; " +
		"[ -f " + proofFile + " ] && kill -0 \"$tclaude_codex_server_pid\" 2>/dev/null || exit 74; " +
		"tclaude_codex_capability=$(" + clcommon.ShellQuoteArg(spec.TclaudeExecutable) +
		" session codex-app-server-token-consume --path " +
		clcommon.ShellQuoteArg(spec.CodexAppServerTokenHandoff) + ") || exit 72; " +
		"[ -n \"$tclaude_codex_capability\" ] || exit 73; " +
		"TCLAUDE_CODEX_APP_SERVER_TOKEN=\"$tclaude_codex_capability\" " + cmd
}

func codexSandboxArgs(spec SpawnSpec) string {
	args := ""
	if spec.PermissionProfile != "" {
		// `-p` layers the launch-unique managed profile containing the complete
		// filesystem, network, Unix-socket, Git, and protected-path posture.
		args += " -p " + clcommon.ShellQuoteArg(spec.PermissionProfile)
	} else if spec.HarnessBuiltinMode != "" {
		args += " --sandbox " + clcommon.ShellQuoteArg(spec.HarnessBuiltinMode)
	}
	if spec.StrongNestedSandbox {
		// Verified against codex-cli 0.145.0: false selects the bwrap/seccomp
		// backend exercised by the stacked capability probe.
		args += " -c " + clcommon.ShellQuoteArg("features.use_legacy_landlock=false")
	}
	return args
}

func codexAppServerApprovalArgs(spec SpawnSpec) string {
	args := ""
	if spec.ApprovalPolicy != "" {
		// Like --sandbox, the root approval flag parses before app-server but is
		// ignored by that branch in 0.147. Use its effective-config key.
		args += " -c " + clcommon.ShellQuoteArg(
			"approval_policy="+codexTOMLString(spec.ApprovalPolicy))
	}
	if spec.AutoReview {
		args += " -c " + clcommon.ShellQuoteArg(
			codexApprovalsReviewerKey+`="`+codexApprovalsReviewerAuto+`"`)
	}
	return args
}

func codexAppServerHookTrustArgs(spec SpawnSpec) string {
	if !spec.BypassHookTrust {
		return ""
	}
	// App-server exposes this launch extension as a config boolean; the TUI's
	// root flag is not forwarded into the server branch's effective config.
	return " -c " + clcommon.ShellQuoteArg("bypass_hook_trust=true")
}

func codexAppServerSandboxArgs(spec SpawnSpec) string {
	args := ""
	if spec.PermissionProfile != "" {
		overrides := spec.CodexAppServerProfileOverrides
		if len(overrides) == 0 {
			// Fail closed if a caller selects a managed profile without carrying
			// its app-server representation. A nonexistent default_permissions
			// table makes Codex reject startup instead of falling back to user
			// config and silently widening or narrowing tool execution.
			overrides = []string{`default_permissions="__tclaude_missing_app_server_profile__"`}
		}
		for _, override := range overrides {
			args += " -c " + clcommon.ShellQuoteArg(override)
		}
	} else if spec.HarnessBuiltinMode != "" {
		// Codex 0.147 accepts the root --sandbox flag before app-server but
		// ignores it when building the server's effective config. The app-server
		// supported -c seam is semantic: config/read and model tools see it.
		args += " -c " + clcommon.ShellQuoteArg(
			"sandbox_mode="+codexTOMLString(spec.HarnessBuiltinMode))
	}
	if spec.StrongNestedSandbox {
		args += " -c " + clcommon.ShellQuoteArg("features.use_legacy_landlock=false")
	}
	return args
}

func codexApprovalArgs(spec SpawnSpec) string {
	args := ""
	if spec.ApprovalPolicy != "" {
		args += " --ask-for-approval " + clcommon.ShellQuoteArg(spec.ApprovalPolicy)
	}
	if spec.AutoReview {
		args += " -c " + clcommon.ShellQuoteArg(
			codexApprovalsReviewerKey+`="`+codexApprovalsReviewerAuto+`"`)
	}
	return args
}

func codexFastModeArgs(spec SpawnSpec) string {
	if spec.FastMode == "" {
		return ""
	}
	tier := "default"
	if spec.FastMode == FastModeOn {
		tier = "fast"
	}
	return " -c " + clcommon.ShellQuoteArg(`service_tier="`+tier+`"`)
}

// CodexShellEnvironmentOverridePrefix is the `-c` key prefix under which a
// Codex launch re-renders each authored sandbox-profile environment entry onto
// its own argv. Exported because it is the second place an authored value
// reaches the pane command, and the launch-command redactor has to recognise
// that channel by name — a value short enough to look like ordinary command
// text cannot be used as a tripwire, but this prefix always can.
const CodexShellEnvironmentOverridePrefix = "shell_environment_policy.set."

func codexShellEnvironmentArgs(environment map[string]string) string {
	if len(environment) == 0 {
		return ""
	}
	// Codex may build tool environments from a saved user-shell snapshot. Pin
	// profile values in the documented always-wins layer on both execution
	// owners so that snapshot cannot replace an agent-owned binding.
	names := make([]string, 0, len(environment))
	for name := range environment {
		names = append(names, name)
	}
	slices.Sort(names)
	args := ""
	for _, name := range names {
		override := CodexShellEnvironmentOverridePrefix + name + "=" + codexTOMLString(environment[name])
		args += " -c " + clcommon.ShellQuoteArg(override)
	}
	return args
}

// codexTOMLString renders an arbitrary validated sandbox-profile environment
// value as a TOML basic string for Codex's -c parser. Profile values may carry
// whitespace and control characters other than NUL, so escaping only quotes
// and backslashes is insufficient here.
func codexTOMLString(value string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range value {
		switch r {
		case '\b':
			b.WriteString(`\b`)
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		case '\f':
			b.WriteString(`\f`)
		case '\r':
			b.WriteString(`\r`)
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&b, `\u%04X`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// codexModels is the ModelCatalog for Codex (JOH-154/155). It offers a small
// curated set of current Codex models while still accepting models outside the
// list: Codex owns the authoritative, per-release validation. It also rejects
// Claude Code slugs and accepts tclaude's effort levels, which codexSpawner
// maps onto Codex's reasoning-effort scale.
type codexModels struct{}

// codexKnownModels is deliberately a suggestion list, not an allow-list.
// Keeping the current first-party choices here gives every ModelCatalog-driven
// surface (spawn, profiles, and template-local launch profiles) the same
// dropdown while ValidateModel continues to pass future/custom OpenAI IDs
// through to Codex.
var codexKnownModels = []string{
	"gpt-5.6-sol",
	"gpt-5.6-terra",
	"gpt-5.6-luna",
	"gpt-5.5",
	"gpt-5.4",
	"gpt-5.4-mini",
	"gpt-5.3-codex-spark",
}

// ValidateModel rejects a Claude Code model slug/ID chosen for a Codex
// session (a clear error beats forwarding e.g. "opus" or "claude-fable-5"
// to `codex --model`, which fails opaquely at launch). Any other non-empty
// value passes through trimmed: Codex's model set changes per release and
// Codex validates it itself, so tclaude doesn't curate a list that would
// go stale. Empty stays empty → the spawner omits `--model`.
func (codexModels) ValidateModel(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	// clcommon.IsValidModel recognises CC aliases (opus, sonnet, …) and
	// claude-* full IDs; fold case the way clcommon.ValidateModel does.
	if clcommon.IsValidModel(strings.ToLower(s)) {
		return "", fmt.Errorf("%q is a Claude Code model; the codex harness uses OpenAI models (e.g. gpt-5, gpt-5-codex)", s)
	}
	return s, nil
}

// ValidateEffort accepts tclaude's effort levels — a harness-agnostic
// concept — validating them exactly as Claude Code does. The level →
// Codex reasoning-effort mapping is applied by codexSpawner.BuildCommand
// when it emits the config override (see codexReasoningEffort).
func (codexModels) ValidateEffort(s string) (string, error) {
	return clcommon.ValidateEffort(s)
}

// Models returns a copy of the curated suggestions. ValidateModel remains the
// authority and accepts custom OpenAI IDs outside this list.
func (codexModels) Models() []string       { return slices.Clone(codexKnownModels) }
func (codexModels) EffortLevels() []string { return slices.Clone(clcommon.ValidEffortLevels) }

// codexReasoningEffort maps a validated tclaude effort level onto the selected
// Codex model's scale. GPT-5.6 has a distinct max level, while older models top
// out at xhigh; all lower shared levels pass through unchanged. An unset/custom
// model retains the backwards-compatible max → xhigh mapping because tclaude
// cannot know whether that model accepts the newer level.
func codexReasoningEffort(model, effort string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	isGPT56 := model == "gpt-5.6" || strings.HasPrefix(model, "gpt-5.6-")
	if effort == "max" && !isGPT56 {
		return "xhigh"
	}
	return effort // low / medium / high / xhigh (and GPT-5.6 max) map 1:1
}
