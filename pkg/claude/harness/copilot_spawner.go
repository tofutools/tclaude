package harness

import (
	"strconv"
	"strings"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
)

// copilotSpawner builds the `copilot` invocation that runs inside the tmux
// pane. Like the other spawners it stays pure (string in → string out) so the
// "unset omits the flag" guarantee is unit-testable without tmux, and it
// shell-quotes everything handed to `sh -c`.
//
// Every flag below is taken verbatim from Copilot CLI's documented
// command-line options table, INCLUDING its exact `=`-vs-space spelling. That
// precision is load-bearing rather than cosmetic:
//
//   - `-r`, `--resume[=VALUE]` takes an OPTIONAL value, so only the `=` form
//     binds the id. A space-separated `--resume <id>` would leave the option
//     bare — which opens the interactive session picker, or, when the picker
//     cannot be shown, exits with an error — and would then try to parse the
//     id as a positional.
//   - `--session-id ID` is documented with a space.
//   - `--name=NAME`, `--model=MODEL` and `--effort=LEVEL` are documented with
//     `=`.
//
// Working directory is NOT passed via `-C`: tclaude launches the pane with
// `tmux new-session -c <cwd>`, and Copilot uses the pane's cwd — the same way
// the Claude Code and Codex spawners rely on tmux for cwd.
type copilotSpawner struct{}

func (copilotSpawner) Binary() string { return "copilot" }

// copilotEnvScrub is emitted after the caller's EnvExports on EVERY Copilot
// launch, whatever the approval policy.
//
// COPILOT_ALLOW_ALL is measured (contract entry ambient-allow-all-env) as
// STRICTLY STRONGER than the --allow-all-tools flag it documents: exported
// alone, with no flags at all, it skipped the folder-trust dialog that no flag
// clears, reached the provider and executed an unsafe tool call. tclaude's
// EnvExports forwards the operator's whole environment, so an operator with it
// exported would silently promote every tclaude-spawned Copilot pane to
// allow-all — including one tclaude recorded as `inherit`, and with no trace
// anywhere that the recorded posture was not the one that ran.
//
// UNSET rather than pinned to a falsy value, deliberately. The parse today is
// strict case-sensitive equality against "true", so `COPILOT_ALLOW_ALL=false`
// would work — but only for as long as that stays true, and a future widening
// of the value parse would defeat a pinned falsy value silently. An unset
// variable cannot be reinterpreted.
//
// It is a scrub rather than a policy renderer because it protects the `inherit`
// token too: `inherit` means "Copilot's own posture", not "the posture an
// ambient variable happens to impose".
const copilotEnvScrub = "unset " + copilotAllowAllEnv + "; "

// BuildCommand assembles the Copilot invocation: env exports + the ambient
// allow-all scrub + the binary, then either an exact `--resume=<id>` or a
// fresh launch's `--session-id` / `--name`, an optional `--model` /
// `--effort`, the resolved permission flags, any pass-through args, and
// finally the optional `-i <prompt>` first turn.
//
// The permission flags sit BEFORE ExtraArgs and after every tclaude-owned
// option, so the ordering is stable across fresh and resumed launches alike.
// Nothing here relies on what Copilot does with a DUPLICATE flag: duplicate
// handling is unmeasured, and ValidateLaunchExtraArgs refuses pass-through args
// that name anything this renders precisely so the question never arises. The
// order is fixed for reproducibility — the same profile must always produce the
// same recorded command — not because a later occurrence would win. `-i` stays
// last regardless.
//
// Fields with no documented Copilot flag (sandbox mode, auto-review,
// permission profile, remote control, hook-trust bypass, the per-session
// settings payload) are IGNORED here rather than approximated. The descriptor
// leaves those contracts nil, so the resolvers reject an explicit value long
// before a spec reaches this function.
func (copilotSpawner) BuildCommand(spec SpawnSpec) string {
	binary := "copilot"
	if spec.ExecutablePath != "" {
		binary = clcommon.ShellQuoteArg(spec.ExecutablePath)
	}
	cmd := spec.EnvExports + copilotEnvScrub + spec.PreLaunchScript + binary
	if spec.ResumeID != "" {
		// `--resume=<id>` resumes EXACTLY this conversation. tclaude always
		// knows the full id it wants, so it never uses the option's fuzzier
		// affordances (bare `--resume`'s picker, an id prefix, a session name)
		// — those could silently open a different conversation. The docs also
		// forbid combining resume with `--session-id`, `--continue`,
		// `--connect` and the worktree flags, hence the else-branch below.
		cmd += " --resume=" + clcommon.ShellQuoteArg(spec.ResumeID)
	} else {
		// `--session-id <uuid>` pins the conversation id for a FRESH launch, so
		// the daemon can enroll the agent before the pane starts. Copilot
		// creates a new session for an unmatched value only when it is a valid
		// UUID (a name or an id prefix never creates one), which is why the
		// spawn boundary validates the shape. Quoted defensively even though it
		// is a validated UUID.
		if spec.SessionID != "" {
			cmd += " --session-id " + clcommon.ShellQuoteArg(spec.SessionID)
		}
		// `--name=<name>` sets the session's display name at launch — the same
		// name `/rename` writes and `--resume` matches on. Fresh launches only:
		// the documented purpose is "set a name for the NEW session", and its
		// behavior on a resume is unverified, so tclaude renames a resumed
		// conversation through the ordinary in-pane `/rename` path instead.
		// Quoted because the name is free-ish text handed to `sh -c`.
		if spec.Name != "" {
			cmd += " --name=" + clcommon.ShellQuoteArg(spec.Name)
		}
	}
	if spec.Model != "" {
		// `--model=<model>` (`auto` lets Copilot pick). Quoted defensively:
		// the catalog gates the charset, but this string is handed to `sh -c`.
		cmd += " --model=" + clcommon.ShellQuoteArg(spec.Model)
	}
	if spec.Effort != "" {
		// `--effort=<level>`. Copilot's validated vocabulary (none/minimal plus
		// the shared low/medium/high/xhigh/max levels) passes straight through
		// with no per-model remapping of the kind Codex needs.
		cmd += " --effort=" + clcommon.ShellQuoteArg(spec.Effort)
	}
	if spec.CopilotAPIPort > 0 {
		// TUI+server mode: one process runs the interactive TUI *and* listens on
		// a TCP port speaking Content-Length-framed JSON-RPC 2.0. That single
		// process shape is why tclaude's existing "harness process under the
		// pane" liveness anchoring keeps working unchanged.
		//
		// `--host 127.0.0.1` is pinned rather than left to Copilot's default.
		// This endpoint has NO authentication of any kind (TCL-1055) — a client
		// that sends `connect` with no token is accepted even with
		// COPILOT_CONNECTION_TOKEN exported — so the loopback bind is the only
		// thing keeping it off the network, and it is far too important to
		// inherit from an undocumented default that a Copilot release could
		// change without notice. Pinning it also keeps ONE address in play:
		// tclaude reserves 127.0.0.1:<n>, the pane binds 127.0.0.1:<n>, and the
		// ownership proof reads the 127.0.0.1:<n> listener. A wildcard bind would
		// break the third even while the first two worked.
		//
		// Never combined with --acp. The two are NOT mutually exclusive and that
		// is worse than if they were: `--acp --ui-server` is accepted, ACP wins
		// silently, no TUI mounts, and the port still listens. --acp is refused
		// as a pass-through arg, so the combination cannot be reached from here.
		cmd += " --ui-server --host 127.0.0.1 --port " +
			clcommon.ShellQuoteArg(strconv.Itoa(spec.CopilotAPIPort))
	}
	// The resolved approval policy, expanded into Copilot's several permission
	// flags plus one `--add-dir` per profile-granted directory. A single
	// catalog token rendering into several flags is legal — ApprovalCatalog
	// contracts a token in and a validated token out, not a one-to-one flag
	// mapping — and it is necessary here because Copilot's prompt sources are
	// independent surfaces with a flag each. See copilot_approval.go.
	//
	// The directories are rendered under `inherit` too: they are the path axis,
	// not the approval axis, and dropping them would violate SandboxReadDirs'
	// stated contract that an adapter either renders the roots or refuses the
	// launch.
	if perms := copilotPermissionArgs(spec.ApprovalPolicy, copilotSpawnAddDirs(spec)); len(perms) > 0 {
		quoted := make([]string, len(perms))
		for i, arg := range perms {
			quoted[i] = clcommon.ShellQuoteArg(arg)
		}
		cmd += " " + strings.Join(quoted, " ")
	}
	if len(spec.ExtraArgs) > 0 {
		// Appended as plain args, each quoted individually. Copilot documents
		// no `--` pass-through separator, so tclaude does not invent one.
		quoted := make([]string, len(spec.ExtraArgs))
		for i, a := range spec.ExtraArgs {
			quoted[i] = clcommon.ShellQuoteArg(a)
		}
		cmd += " " + strings.Join(quoted, " ")
	}
	// `-i PROMPT` starts an INTERACTIVE session and automatically executes the
	// prompt — the flag a TUI pane wants. `-p/--prompt` is the headless form
	// that exits after completion, so it must never appear here. Emitted last
	// so no other option can swallow the value, and quoted as a single arg so
	// the whole prompt stays one PROMPT rather than splitting into stray
	// flags/words.
	//
	// Emitted on a resume too, and that is now MEASURED rather than hoped for.
	// The permission matrix's resume-submits-prompt entry ran the real binary
	// twice — seeding a conversation under a pinned --session-id, then
	// relaunching it on a PTY with --resume=<full-id> and a new -i prompt — and
	// the resumed request carried message roles [system, user, assistant, user]
	// while the session-state directory kept its original UUID. A fresh
	// conversation would have sent [system, user]. So a relaunch briefing lands
	// in the conversation it was written for and does not vanish silently.
	//
	// SUPPRESSED under the API drive, and this is load-bearing rather than
	// tidiness. That drive creates its own RPC session under the SAME id this
	// launch pins with `--session-id`, because the pane's startup session — the
	// one `-i` runs in — is visible over RPC but not drivable: every `session.*`
	// call against it fails "Session not found", so tclaude could never send it
	// a second turn, read its usage, or compact it. Creating at the conv id is
	// what keeps one conversation at one id, but it starts that id FRESH
	// (measured: `alreadyInUse:false`, empty `getMessages`, and the model with no
	// memory of the `-i` turn). So a prompt delivered by `-i` would run, render
	// in the pane, and then be silently discarded moments later — the agent
	// would come up having forgotten the briefing it visibly answered.
	//
	// The prompt is delivered over `session.send` after the session exists
	// instead, which lands it in the conversation that survives. See
	// agentd.bootstrapCopilotAPISession.
	if spec.InitialPrompt != "" && spec.CopilotAPIPort == 0 {
		cmd += " -i " + clcommon.ShellQuoteArg(spec.InitialPrompt)
	}
	return cmd
}
