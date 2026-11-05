package agentd

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// agentState mirrors what `tclaude session ls` shows: status from the
// hook callbacks (idle / working / awaiting_*), last hook timestamp,
// the agent's cwd, and subagent count. Empty string fields when no
// live session row exists for the conv.
type agentState struct {
	Status        string `json:"status,omitempty"`
	StatusDetail  string `json:"status_detail,omitempty"`
	StatusWarning string `json:"status_warning,omitempty"`
	SubagentCount int    `json:"subagent_count,omitempty"`
	// TemporaryHarnessBuiltinMode is the reversible operator override currently
	// applied to this stable agent. It remains visible while the session is
	// between stop and resume so the dashboard never mistakes an unlocked
	// agent for one using its normal posture.
	TemporaryHarnessBuiltinMode string `json:"temporary_sandbox_mode,omitempty"`
	// PendingMorph is a requested morph waiting for the agent to go idle, so
	// an operator can see why the agent is about to relaunch.
	PendingMorph *db.AgentPendingMorph `json:"pending_morph,omitempty"`
	// FastMode is present when a live Codex rollout has reported an authoritative
	// thread_settings_applied snapshot, or provisionally when tclaude explicitly
	// selected the tier for this live agent's launch and Codex has not emitted a
	// settings event yet. A false pointer means known standard routing; nil means
	// inherit/unknown.
	FastMode *bool `json:"fast_mode,omitempty"`
	// BgShellCount is how many background shell commands (Claude Code
	// `Bash` with run_in_background) the agent still has running — the
	// dashboard's "⚙+N" badge. Like SubagentCount it is only ever
	// non-zero for a live agent, and it is reconciled against the agent's
	// actual descendant processes at read time rather than trusted from
	// the hook stream, which announces launches but never exits. See
	// bgShellCountOnRead / db.BgShellSet.
	BgShellCount int `json:"bg_shell_count,omitempty"`
	// MonitorCount is how many monitors (Claude Code `Monitor` watches)
	// the agent still has running — the dashboard's "👁+N" badge. It is
	// reconciled on the same read pass, and against the same process list,
	// as BgShellCount. See backgroundCountsOnRead / db.MonitorSet.
	MonitorCount int    `json:"monitor_count,omitempty"`
	LastHook     string `json:"last_hook,omitempty"`
	Cwd          string `json:"cwd,omitempty"`
	// Context-window usage, read from the same sessions row the hook
	// callbacks update — no new data source, one extra indexed read.
	// ContextPct is Claude Code's authoritative "how full" figure from
	// the statusline hook (0 = not reported yet). The token counts feed
	// the dashboard context-meter's tooltip; all zero means the
	// statusline hook hasn't fired for this session yet, which the UI
	// renders as a neutral / empty meter.
	ContextPct   float64 `json:"context_pct,omitempty"`
	TokensInput  int64   `json:"tokens_input,omitempty"`
	TokensOutput int64   `json:"tokens_output,omitempty"`
	// ContextWindowSize is the EFFECTIVE window this agent's meter is drawn
	// against — min(model window, pinned auto-compaction window) — so the
	// tooltip's denominator matches ContextPct's. AutoCompactWindow is the pin
	// itself (0 = none), so the UI can say WHY the window is smaller than the
	// model's.
	ContextWindowSize int64 `json:"context_window_size,omitempty"`
	// ContextWindowMax is tclaude's configured, catalog, or assumed model cap.
	// It is intentionally distinct from ContextWindowSize, which remains the
	// observed context snapshot.
	ContextWindowMax    int64  `json:"context_window_max,omitempty"`
	ContextWindowSource string `json:"context_window_source,omitempty"`
	// CopilotAPI reports that this agent was LAUNCHED to be driven over
	// Copilot's embedded JSON-RPC API rather than tmux send-keys. Omitted
	// (false) for every send-keys agent and for every other harness, so the two
	// drives are distinguishable at a glance while both are live. See TCL-1053.
	//
	// Launch intent, and durable: it stays true for an agent whose channel
	// never came up. That is why it is not the whole answer — see
	// CopilotAPIConnected.
	CopilotAPI bool `json:"copilot_api,omitempty"`
	// CopilotAPIConnected reports that tclaude still holds an OPEN connection to
	// this agent's embedded server.
	//
	// Precisely that, and no more. It is not a re-proof of ownership: the
	// connection was proved to belong to the agent's pane when it was
	// established (see copilotAPISession.StillOwned), and what is re-read per
	// tick is only whether that same socket is still open. Saying
	// "ownership-proved right now" would be this series' proxy-value mistake
	// committed in the field's own documentation — a true statement about the
	// past, worn as a claim about the present. A caller about to ACT on the
	// connection should call StillOwned; a caller rendering a badge should not
	// pay for a /proc walk per agent per tick.
	//
	// A separate field from CopilotAPI rather than a refinement of it, because
	// they are answers to different questions and the difference is exactly
	// where this series keeps producing bugs. "Launched with the API drive" is
	// a durable fact about a launch; "is being driven over the API" is a fact
	// about a connection, and the two disagree for the whole window between a
	// pane starting and its bootstrap completing — and forever, for a bootstrap
	// that failed. Collapsing them would give the dashboard a badge that says
	// an agent is API-driven while nothing is on the other end.
	//
	// Derived from the live handle registry and never from the recorded port.
	// The port record is a true statement about what a launch was told to bind;
	// it says nothing about whether anything is listening, nor whose. See
	// TCL-1054's carry-in note.
	CopilotAPIConnected bool `json:"copilot_api_connected,omitempty"`
	// CodexAppServer and its state disclose the explicitly selected private
	// app-server drive independently from the default send-keys path.
	CodexAppServer        bool   `json:"codex_app_server,omitempty"`
	CodexAppServerState   string `json:"codex_app_server_state,omitempty"`
	CodexAppServerHealth  string `json:"codex_app_server_health,omitempty"`
	CodexAppServerSource  string `json:"codex_app_server_source,omitempty"`
	CodexAppServerVersion string `json:"codex_app_server_version,omitempty"`
	CodexAppServerDetail  string `json:"codex_app_server_detail,omitempty"`
	CodexObserverMode     string `json:"codex_observer_mode,omitempty"`
	CodexObserverUpdated  string `json:"codex_observer_updated,omitempty"`
	// CopilotAPIChannelFailed reports that this agent's CURRENT launch has been
	// OBSERVED not to be getting an API channel — so its mail is being held and
	// will go on being held until somebody relaunches it.
	//
	// This is the field that makes a deaf agent visible. Without it the pair
	// above is ambiguous in the worst direction: `copilot_api && !connected` is
	// equally true of an agent whose bootstrap is still running, of every
	// already-running agent between an agentd restart and the reconcile sweep
	// reaching it, and of an agent that will never be reachable again. Rendering
	// that pair as trouble would flag the fleet as broken at exactly the moments
	// it is working normally.
	//
	// # It is deliberately not exhaustive, and the UI must not imply otherwise
	//
	// This says AGENTS KNOWN TO BE DEAF, not all deaf agents. An observation
	// exists only where the daemon watched an attempt fail — a launch's bootstrap
	// (copilot_api_bootstrap.go) or the startup reconcile's per-candidate attempt
	// (copilot_api_reconnect.go). A candidate the sweep ran out of time to
	// examine is un-examined rather than healthy, and reports nothing here while
	// saying so in the log.
	//
	// So a false is "nothing observed", never "fine". A UI that renders the
	// absence of this flag as an all-clear has converted a partial signal into a
	// claim about the whole fleet, which is how absence starts reading as health.
	//
	// # Not durable, and not the agent's drive
	//
	// The observation lives in process memory and is dropped by an agentd
	// restart, which is correct: at startup nobody yet knows which channels are
	// adoptable, and the reconcile is what finds out. It is also NOT a statement
	// that the agent is on send-keys — CopilotAPI above still answers that, and
	// still says API, because the agent chose the API drive and a failed
	// bootstrap has not un-chosen it. See copilotAPIChannelFailed.
	CopilotAPIChannelFailed bool  `json:"copilot_api_channel_failed,omitempty"`
	AutoCompactWindow       int64 `json:"auto_compact_window,omitempty"`
	// Model is the LLM model display name the agent is running on
	// ("Opus 4.8", "Sonnet 4.6", …), recorded by the statusline hook.
	// Empty until the statusbar has ticked at least once; the dashboard
	// renders it as the harness line under the per-row controls and in
	// the status-dot tooltip. Surfaced regardless of liveness — a frozen
	// model for an exited agent is still informative.
	Model string `json:"model,omitempty"`
	// EffortLevel is the reasoning-effort level the agent is running on
	// ("low"…"max"), recorded by the statusline hook on the same row as
	// Model. Empty until the statusbar has ticked, or when the model
	// lacks reasoning-effort support; the dashboard appends it to the
	// per-agent model line ("<logo> · Opus 4.8 high") and omits it when empty.
	EffortLevel string `json:"effort_level,omitempty"`
	// CostUSD is the agent's cumulative API cost in USD, recorded by the
	// statusline hook on the same row — but only when the session runs
	// on API/enterprise pricing (no subscription rate-limit data). 0
	// means "no cost data" (subscription plan, or no tick yet) and the
	// dashboard renders no cost badge for it. Surfaced regardless of
	// liveness, like Model — what a dead agent cost is still informative.
	CostUSD float64 `json:"cost_usd,omitempty"`
	// VirtualCostUSD is the WHAT-IF sibling of CostUSD: the agent's
	// cumulative pay-per-token-EQUIVALENT cost on a subscription session. 0
	// on pay-per-token (CostUSD carries the real figure there) or before a
	// tick. The Groups tab shows it as the per-agent cost badge — flagged
	// hypothetical — only when the WHAT-IF view is active
	// (snapshot.cost_tab_whatif). Surfaced regardless of liveness, like CostUSD.
	VirtualCostUSD float64 `json:"virtual_cost_usd,omitempty"`
	// VirtualCostCredits is Copilot's native AI-credit equivalent of
	// VirtualCostUSD. It is present only when the Copilot usage store reported a
	// positive nano-AIU total; other harnesses keep this field absent.
	VirtualCostCredits float64 `json:"virtual_cost_credits,omitempty"`
	// BrokerRefusals is how many brokered hook/statusline requests the
	// daemon has REFUSED while resolving them to THIS row, inside the
	// recorder's window. Non-zero means an agent is losing telemetry:
	// either this row is shadowing a live session whose pid it shares
	// (the stale-row case TCL-761 is about), or a caller is presenting a
	// session id that disagrees with the one its process ancestry
	// resolves to.
	//
	// Attribution is the daemon's own conclusion — the row the ancestry
	// walk produced — never the session id the refused request claimed.
	// That field is caller-chosen, and the refusal happened precisely
	// because it is not trusted; letting it decide what appears on
	// another agent's row would let any wrapped agent make a peer look
	// broken on the surface the operator uses to decide where to look.
	BrokerRefusals int `json:"broker_refusals,omitempty"`
	// BrokerRefusalDetail names the refusal kind behind BrokerRefusals.
	BrokerRefusalDetail string `json:"broker_refusal_detail,omitempty"`
	// BrokerRefusalSince is when the current run of refusals began
	// (RFC3339), so the UI can say how long this has been happening.
	BrokerRefusalSince string `json:"broker_refusal_since,omitempty"`
	// ExitReason is why a now-offline agent's session ended: a graceful
	// SessionEnd `reason`, a daemon-owned clean reason, or 'unexpected'
	// when a harness-specific reaper path has a positive abnormal-death
	// signal. Only populated for an offline agent; empty for a live one,
	// a normal Codex close with no explicit reason, or a row that exited
	// before the exit_reason column existed. The dashboard renders
	// 'unexpected' as "crashed" and everything else (incl. empty) as a
	// plain exit.
	ExitReason string `json:"exit_reason,omitempty"`
	// Harness is the coding tool this agent runs under ("claude", "codex"),
	// from the session row. Empty (a conv with no session row) renders as
	// the default (Claude Code). The dashboard badges it per row and gates
	// the rename control on the matching harness's can_rename (JOH-162).
	// Surfaced regardless of liveness — a dead Codex agent is still Codex.
	Harness string `json:"harness,omitempty"`
	// HarnessBuiltinMode is the launch-time harness-builtin sandbox mode the agent was spawned
	// under (Codex: read-only / workspace-write / danger-full-access; Claude
	// Code: on / off — its `inherit` default normalizes to "" and records
	// nothing). "" renders no sandbox badge. Surfaced regardless of liveness.
	HarnessBuiltinMode string `json:"sandbox_mode,omitempty"`
	// HarnessBuiltinModeSource names the resolution tier that CHOSE HarnessBuiltinMode — an
	// explicit flag, a named spawn profile, the group default, or the global
	// default. "" is "nothing recorded" (a pre-v158 row, or a launch with no
	// attribution to make), never "the operator chose it".
	//
	// OSSandboxSource below already carries this for a Claude Code launch that
	// recorded a verdict, folded into the deciding tier's name. This field is
	// what a harness whose MODE is its posture (Codex) has instead. The compact
	// dashboard badge no longer renders this provenance, but it remains useful
	// launch metadata for API consumers and diagnostics.
	HarnessBuiltinModeSource string `json:"sandbox_mode_source,omitempty"`
	// SandboxImplementation names who owned OS-level confinement for this
	// launch. The dashboard needs the resolved implementation, not a source
	// string heuristic: tclaude-layer renders profile filesystem rules into its
	// outer wall even though the harness's inner mode is off, while
	// harness-builtin does not.
	SandboxImplementation string `json:"sandbox_implementation,omitempty"`
	// OSSandboxState and OSSandboxSource are the launch-time verdict on whether
	// the OS sandbox ACTUALLY confined this agent ("on" / "off" /
	// "unconfigured") and what decided it — the settings file that won the
	// precedence chain, or the launch itself (TCL-729). They answer what
	// HarnessBuiltinMode cannot: Claude Code's default mode is `inherit`, which means
	// "whatever the operator's settings.json says", so a badge driven off the
	// mode alone stayed blank for a confined agent and an unconfined one alike.
	// "" is "no verdict recorded" — a pre-column row, or a harness whose mode
	// already states its posture (Codex) — and the dashboard falls back to
	// rendering the mode exactly as before. Surfaced regardless of liveness,
	// like HarnessBuiltinMode: what a dead agent ran under is still informative.
	OSSandboxState  string `json:"os_sandbox_state,omitempty"`
	OSSandboxSource string `json:"os_sandbox_source,omitempty"`
	// OSSandboxUnverified marks a verdict that a settings file OUTRANKING the
	// deciding tier could have overturned, had tclaude been able to read it. The
	// Harness-builtin uses the warning glyph; an exact tclaude-layer
	// implementation may still earn the lock because its separate outer OS wall
	// is established.
	OSSandboxUnverified bool `json:"os_sandbox_unverified,omitempty"`
	// SandboxProfiles names the tclaude sandbox profiles that were applied to
	// this launch, in resolution order (global → group → explicit).
	//
	// The profile is orthogonal to the state above: it does not decide WHETHER
	// the agent is sandboxed, it supplies the RULES. For Claude Code a profile's
	// filesystem grants are compiled into the harness's own
	// `sandbox.filesystem.*` via `--settings` on harness-builtin, while
	// tclaude-layer renders them into its outer OS wall. Its environment entries
	// are plain env vars and apply either way. The compact badge tooltip names
	// the applied profiles without expanding their rules or provenance.
	//
	// Empty means either "no profile applied" or "no snapshot recorded" — the
	// two are distinguished by SandboxProfilesRecorded, because claiming "none"
	// for a legacy row would invent a fact.
	SandboxProfiles []sandboxProfileRef `json:"sandbox_profiles,omitempty"`
	// SandboxProfilesRecorded reports that this launch recorded a resolved
	// sandbox policy at all, so an empty SandboxProfiles above is a genuine
	// "nothing applied" rather than a row from before the snapshot existed.
	SandboxProfilesRecorded bool `json:"sandbox_profiles_recorded,omitempty"`
	// SandboxAccessNotices are the immutable composition/degradation disclosures
	// frozen into the launch snapshot. The badge tooltip names them so the
	// dashboard never implies a wider/narrower surface than the launch record.
	SandboxAccessNotices []sandboxpolicy.AccessNotice `json:"sandbox_access_notices,omitempty"`
	// SandboxProfilesOmitted reports a launch that carried NO profile tier at
	// all, for either of the two reasons the daemon collapses into one flag: the
	// launch MODE cannot combine with the managed permission profile that
	// renders filesystem rules (a Codex `danger-full-access` spawn), or the
	// caller asked for none (the spawn dialog's "none", `--omit-sandbox-profiles`).
	// It is a different fact from "the tiers resolved to nothing"; the compact
	// badge intentionally displays both as "Profile: None".
	SandboxProfilesOmitted bool `json:"sandbox_profiles_omitted,omitempty"`
	// ResourceCgroup reports that this launch asked for its own Linux cgroup v2
	// — because a ceiling was authored, or because `resource-only` requests the
	// accounting boundary on its own. It is the request the launch recorded, not
	// a live kernel readback; a launch that was allowed to proceed without the
	// cgroup discloses that through a resource_limits access notice, which the
	// badge already surfaces alongside this.
	//
	// Resource limits are an axis of their own: they bound how MUCH a workload
	// may consume, never WHAT it may reach. So they are reported next to the
	// sandbox posture rather than folded into it — a `resource-only` agent has
	// a cgroup and no access boundary at all.
	ResourceCgroup bool `json:"resource_cgroup,omitempty"`
	// ResourceMemoryLimit is the operator's authored memory ceiling spelling
	// ("8GiB", "4GB"), kept verbatim so the tooltip shows what was written
	// rather than a re-rendered byte count. Empty means no memory ceiling.
	ResourceMemoryLimit string `json:"resource_memory_limit,omitempty"`
	// ResourceCPULimit is the authored CPU ceiling in cores. nil means no CPU
	// ceiling; a cgroup can exist with neither ceiling set.
	ResourceCPULimit *float64 `json:"resource_cpu_limit,omitempty"`
	// ResourcePIDsLimit is the authored process/thread ceiling rendered as
	// `pids.max`. nil means no PID ceiling; a cgroup can exist with none of the
	// three ceilings set.
	ResourcePIDsLimit *uint64 `json:"resource_pids_limit,omitempty"`
	// RemoteControl is tclaude's best-known state of whether the harness's
	// built-in Remote Access is enabled for this agent (JOH-256). It is a
	// best-known flag — the harness exposes no readback, so the dashboard
	// reflects the recorded intent and reconciles on refresh. Surfaced
	// regardless of liveness; the per-row toggle lets the operator flip an
	// agent's remote access before stepping away (JOH-259). The CAPABILITY
	// gate (which harness can be remote-controlled at all) is not here — it
	// rides the harness catalog's can_remote_control, the same place the
	// rename control reads can_rename from.
	RemoteControl bool `json:"remote_control,omitempty"`
	// Recovery fields are a bounded view of the durable Codex crash-loop
	// ledger. They deliberately expose identifiers/counts/timestamps only;
	// never pane contents, argv, environment, stderr, or harness logs.
	RecoveryStatus       string `json:"recovery_status,omitempty"`
	RecoveryDetail       string `json:"recovery_detail,omitempty"`
	RecoveryReason       string `json:"recovery_reason,omitempty"`
	RecoveryCount        int    `json:"recovery_count,omitempty"`
	RecoveryBackoff      int    `json:"recovery_backoff_seconds,omitempty"`
	RecoveryNextAttempt  string `json:"recovery_next_attempt_at,omitempty"`
	RecoveryLastExitCode *int   `json:"recovery_last_exit_code,omitempty"`
}

const recoveredStatusMaxAge = time.Minute

// stateForConvInSessions chooses the most-recent live session, otherwise the
// most-recent historical row. Live hook state flows through unchanged; offline
// rows report exited rather than showing a frozen idle/working hook state.
// The gathered snapshot supplies the same liveness set and batched rows to
// every consumer, including context read-through and live reconciliation.
func stateForConvInSessions(rows []*db.SessionRow, aliveSet map[string]struct{}) agentState {
	return stateForConvInSessionsTimed(rows, aliveSet, nil)
}

func stateForConvInSessionsTimed(rows []*db.SessionRow, aliveSet map[string]struct{}, recordCodexTelemetry func(codexTelemetryTiming)) agentState {
	return stateForConvInSessionsBatched(rows, aliveSet, nil, recordCodexTelemetry, nil)
}

// recordRowWork, when non-nil, accumulates the named per-row side reads that
// are neither batched nor reported as their own top-level phase — the
// process-table reconcile and the per-session context point query. They are
// paid by whichever snapshot phase first resolves the conv (in practice the
// preload warm loop, whose conv set is a superset of every later surface's),
// and were invisible in /api/perf before.
func stateForConvInSessionsBatched(
	rows []*db.SessionRow,
	aliveSet map[string]struct{},
	contextBatch *codexContextWriteBatch,
	recordCodexTelemetry func(codexTelemetryTiming),
	recordRowWork func(string, time.Duration),
) agentState {
	if len(rows) == 0 {
		return agentState{}
	}
	timed := func(name string, fn func()) {
		if recordRowWork == nil {
			fn()
			return
		}
		start := time.Now()
		fn()
		recordRowWork(name, time.Since(start))
	}
	pick := rows[0] // already sorted most-recent first
	alive := false
	for _, r := range rows {
		if r.TmuxSession == "" {
			continue
		}
		if _, ok := aliveSet[r.TmuxSession]; ok {
			pick = r
			alive = true
			break
		}
	}
	out := agentState{
		Status: pick.Status,
		// Keep implementation provenance in the session row, but do not ship
		// it as the dashboard's regular status detail. Hook/tool details remain
		// visible through the shared display boundary.
		StatusDetail: session.StatusDetailForDisplay(pick.StatusDetail),
		Cwd:          pick.Cwd,
		// Harness + sandbox are launch properties of the row, surfaced
		// regardless of liveness (a dead Codex agent is still Codex). The
		// exited override below only touches Status/StatusDetail.
		Harness:                  pick.Harness,
		HarnessBuiltinMode:       pick.HarnessBuiltinMode,
		SandboxImplementation:    dashboardSandboxImplementation(pick.SandboxImplementation),
		HarnessBuiltinModeSource: pick.HarnessBuiltinModeSource,
		OSSandboxState:           pick.OSSandboxState,
		OSSandboxSource:          pick.OSSandboxSource,
		OSSandboxUnverified:      pick.OSSandboxUnverified,
		// The sandbox profiles ride the same session row the verdict does — the
		// row cache already bulk-loads it, so naming them costs no extra query
		// on the poll.
		SandboxProfiles:         sandboxProfileRefs(pick.EffectiveSandbox),
		SandboxProfilesRecorded: pick.EffectiveSandbox != nil,
		SandboxProfilesOmitted:  pick.EffectiveSandbox != nil && pick.EffectiveSandbox.ProfilesOmitted,
		// RemoteControl is tclaude's best-known Remote Access flag for the
		// conv (JOH-256), surfaced regardless of liveness like the other
		// launch/row properties — the dashboard reflects the recorded intent
		// and reconciles on the next refresh (the harness has no readback).
		RemoteControl: pick.RemoteControl,
	}
	// resource-only and tclaude-layer request the cgroup through the
	// implementation alone, so this read must not require a launch snapshot to
	// find the request: a direct `tclaude session new --sandbox-impl …` and a
	// CLI resume both record a row with no snapshot at all, and a resource-only
	// launch is one whose only purpose IS the cgroup. Both enforcement seams
	// (session.newSession, prepareResourceCgroup) resolve it the same way.
	var limits sandboxpolicy.ResourceLimits
	if pick.EffectiveSandbox != nil {
		out.SandboxAccessNotices = append([]sandboxpolicy.AccessNotice(nil),
			pick.EffectiveSandbox.Effective.AccessNotices...)
		limits = pick.EffectiveSandbox.Effective.ResourceLimits
	}
	implementation, implErr := sandboxpolicy.NormalizeImplementation(pick.SandboxImplementation)
	if implErr != nil {
		// An unrecognized implementation string cannot be resource-only, but an
		// authored ceiling still requires the cgroup on its own.
		implementation = sandboxpolicy.ImplementationHarnessBuiltin
	}
	out.ResourceCgroup = resourceCgroupRequested(limits, implementation)
	out.ResourceMemoryLimit = limits.Memory
	if limits.CPU != nil {
		cpu := *limits.CPU
		out.ResourceCPULimit = &cpu
	}
	if limits.PIDs != nil {
		pids := *limits.PIDs
		out.ResourcePIDsLimit = &pids
	}
	// Codex records collaboration-child lifecycle in its rollout even when an
	// explicit interrupt does not invoke the configured SubagentStop hook. The
	// same read-through scan that refreshes context telemetry reconstructs the
	// authoritative interrupted IDs and removes them from the shared hook ledger
	// below. Other lifecycle still comes from hooks: Codex rollouts do not carry a
	// terminal normal-completion activity event.
	codexRefresh := refreshCodexContextSnapshotOnReadBatched(
		pick, alive, contextBatch, recordCodexTelemetry,
	)
	// Copilot follows the same read-through principle with a much smaller
	// projection: its durable log carries no per-call usage and no live
	// context window, so there is no batching or cost history to fold in.
	refreshCopilotContextSnapshotOnRead(pick, alive)
	// Gemini's session file carries per-call usage; see gemini_context_refresh.go.
	refreshGeminiContextSnapshotOnRead(pick, alive)
	codexInterruptedSubagents := codexRefresh.interruptedSubagents
	if codexRefresh.hasFastMode {
		fastMode := codexRefresh.fastMode
		out.FastMode = &fastMode
	}
	// Sub-agents run INSIDE the harness process, so a dead session has
	// none by definition — a stale count on an exited row must not render
	// a "🤖+N" badge. For a live row, prefer the TTL-filtered ledger over
	// the raw cached count so a phantom entry (a sub-agent whose
	// SubagentStop was lost) stops being displayed as soon as it expires,
	// even if no hook has fired since to sweep it from storage. An empty
	// ledger with a non-zero count is a row last written by a pre-ledger
	// hook binary — surface its raw count rather than hiding real work.
	if alive {
		out.StatusWarning = statusExtensionWarning(pick.Harness, pick.CreatedAt, pick.LastHook, time.Now())
		// The reconcile itself (ledgers, background counts, and the status
		// settle that keeps the pill consistent with the badges) is shared
		// with the detached terminal's status; see reconcileLiveRowStatus.
		live := reconcileLiveRowStatus(pick, codexInterruptedSubagents, func() (counts backgroundCounts) {
			timed(rowWorkBgReconcile, func() { counts = backgroundCountsOnRead(pick, alive) })
			return counts
		})
		out.Status, out.StatusDetail = live.Status, live.StatusDetail
		out.SubagentCount = live.SubagentCount
		out.BgShellCount, out.MonitorCount = live.BgShellCount, live.MonitorCount
	}
	if !pick.LastHook.IsZero() {
		out.LastHook = pick.LastHook.Format(time.RFC3339)
	}
	// Context-window usage rides on the same sessions row — the
	// statusline hook (UpdateContextSnapshot) keeps it current. We
	// surface it regardless of liveness: a frozen context_pct for an
	// exited agent is genuinely informative ("it died at 80%"), unlike
	// a frozen "idle" status that would mislabel a dead agent.
	var snap db.ContextSnapshot
	var snapErr error
	timed(rowWorkContextSnapshot, func() { snap, snapErr = db.GetContextSnapshot(pick.ID) })
	if snapErr == nil {
		// Batched Codex persistence commits after every row has been assembled.
		// Overlay the rollout-derived values now so the response remains
		// read-through fresh while several agents share one durable commit.
		switch {
		case codexRefresh.contextReset:
			snap.ContextPct = 0
			snap.TokensInput = 0
			snap.TokensOutput = 0
		case codexRefresh.context != nil:
			snap.ContextPct = codexRefresh.context.Pct
			snap.TokensInput = codexRefresh.context.TokensInput
			snap.TokensOutput = codexRefresh.context.TokensOutput
			snap.ContextWindowSize = codexRefresh.context.WindowSize
		}
		// OpenCode keys context onto a per-generation session-row id, so a
		// resumed or offline conv can pick a fresh all-zero row; fall back to
		// the conv's last-known populated snapshot (no-op for other harnesses
		// and for a picked row that already has data). See TCL-701 / the
		// OpenCode offline context-meter fix.
		snap = openCodeContextSnapshotFallback(pick, snap)
		out.ContextPct = snap.ContextPct
		out.TokensInput = snap.TokensInput
		out.TokensOutput = snap.TokensOutput
		// The meter measures against the window compaction ACTUALLY fires at: the
		// smaller of the model's real window and any pinned auto-compaction
		// window. ContextPct already arrives re-based (the status line stores it
		// that way), so re-basing the denominator here is what keeps the tooltip's
		// "x / y tokens" agreeing with the percentage beside it. The durable
		// snapshot keeps the model's real window untouched — the relaunch layer
		// reads that one to re-derive a 1M model's [1m] suffix.
		//
		// Gated on a POPULATED snapshot. A zero window here does not mean "unknown
		// model window, so the pin stands" — it is the all-zero sentinel for "the
		// statusline hook has never fired", which is the normal state of every
		// freshly spawned agent (the launch records its pin long before the first
		// render). Substituting the pin would fabricate a window out of nothing and
		// make snapshotPopulated / has_snapshot report usage that was never
		// observed, costing `agent context-info` and the meter their ability to
		// distinguish "not reported yet" from a genuine 0%.
		out.ContextWindowSize = snap.ContextWindowSize
		if snapshotPopulated(snap) {
			out.ContextWindowSize = harness.EffectiveContextWindow(
				snap.ContextWindowSize, harness.AutoCompactWindowTokens(pick.AutoCompactWindow))
			out.AutoCompactWindow = harness.AutoCompactWindowTokens(pick.AutoCompactWindow)
		}
		if pick.Harness == harness.CopilotName {
			// One read for both pieces of launch intent — this runs for every
			// Copilot agent on every snapshot tick.
			intent := copilotLaunchIntentForConv(pick.ConvID)
			out.CopilotAPI = intent.API
			// Asked of the connection registry, not of intent and not of the
			// port record. Cheap by construction — a map read plus a
			// non-blocking channel check, deliberately NOT the ownership
			// re-proof, which is a kernel-table walk and belongs on the paths
			// that act rather than on one that renders. Asked only for an agent
			// launched on the API drive, since for anyone else the answer is a
			// foregone false.
			if intent.API {
				out.CopilotAPIConnected = copilotAPISessions.Connected(pick.ConvID)
				// Same shape and the same cost — a map read under the registry's
				// lock — and asked only when the agent is on the drive, since an
				// observation about a channel is meaningless for an agent that
				// never had one. Read at snapshot time from state rather than
				// latched when something happened: an agent that went deaf through
				// a path nobody instrumented still reads correctly here, and a
				// relaunch clears it without anything having to remember to.
				out.CopilotAPIChannelFailed = copilotAPIChannelFailed(pick.ConvID)
			}
			// A configured cap is launch intent and remains usable before Copilot
			// has disclosed an observed model. Only the static fallback needs an
			// observed model id; keeping these branches separate prevents a blank
			// model from hiding an operator-supplied denominator.
			out.ContextWindowMax = intent.ContextWindowMax
			switch reading, apiOwned := lookupCopilotAPIState(pick.ConvID); {
			case out.ContextWindowMax > 0:
				out.ContextWindowSource = "configured"
			// A limit Copilot itself reported, which only the API drive can
			// obtain. It sits ABOVE the static table for the same reason
			// copilotAPIEffectiveContextWindow puts it there — a measurement
			// beats an assumption — and the two must agree, or the tooltip's
			// "x / y tokens" would describe a different ratio than the
			// percentage beside it.
			case apiOwned && reading.PromptTokenLimit > 0:
				out.ContextWindowMax = reading.PromptTokenLimit
				out.ContextWindowSource = "reported"
			default:
				if model := strings.TrimSpace(snap.Model); model != "" {
					if out.ContextWindowMax = copilotCatalogContextWindow(
						model, copilotContextTierForSession(pick)); out.ContextWindowMax > 0 {
						out.ContextWindowSource = "catalog"
					} else if out.ContextWindowMax = harness.CopilotContextWindowDefault(model); out.ContextWindowMax > 0 {
						out.ContextWindowSource = "assumed"
					}
				}
			}
		}
		if pick.Harness == harness.CodexName {
			posture, postureErr := db.RecordedLaunchPostureForConv(pick.ConvID)
			if postureErr == nil && posture != nil && posture.CodexAppServer != nil && *posture.CodexAppServer {
				out.CodexAppServer = true
				if posture.CodexAppServerSource != nil {
					out.CodexAppServerSource = strings.TrimSpace(*posture.CodexAppServerSource)
				}
				runtime, runtimeErr := db.GetCodexAppServerRuntimeByConvID(pick.ConvID)
				if runtimeErr != nil || runtime == nil {
					out.CodexAppServerHealth = "disconnected"
					out.CodexAppServerDetail = "the app-server drive is selected but no runtime generation is recorded"
				} else {
					out.CodexAppServerState = runtime.State
					observation, connected := codexAppServerObservationForConv(pick.ConvID)
					handle := codexAppServerHandleForConv(pick.ConvID)
					connected = connected && handle != nil && handle.runtime.Generation == runtime.Generation
					out.CodexAppServerHealth = codexAppServerHealth(runtime, connected, observation, time.Now().UTC())
					out.CodexAppServerVersion = runtime.CodexVersion
					out.CodexAppServerDetail = redactCodexDiagnosticDetail(runtime.Detail)
					out.CodexObserverMode = "post-bind non-subscribing snapshots; context from rollout"
					if connected {
						updated := observation.StatusAt
						if observation.UsageAt.After(updated) {
							updated = observation.UsageAt
						}
						if !updated.IsZero() {
							out.CodexObserverUpdated = updated.Format(time.RFC3339)
						}
					}
				}
			}
		}
		out.Model = snap.Model
		out.EffortLevel = snap.EffortLevel
		out.CostUSD = snap.CostUSD
		out.VirtualCostUSD = snap.VirtualCostUSD
		if pick.Harness == harness.CopilotName && snap.VirtualCostUSD > 0 {
			out.VirtualCostCredits = harness.CopilotVirtualCreditsFromUSD(snap.VirtualCostUSD)
		}
	}
	// No live tmux session — the agent's process is gone. Report it as
	// exited rather than letting the frozen hook status (typically
	// "idle") masquerade as a running state. StatusDetail is cleared so
	// stale "idle: Bash"-style leftovers don't leak into the snapshot.
	if !alive {
		out.Status = session.StatusExited
		out.StatusDetail = ""
		// Surface WHY it ended so the dashboard can tell a clean exit
		// from an unexpected death. pick is the most-recently-updated
		// row — the SessionEnd hook and the reaper both bump the row
		// they touch, so the latest row carries the authoritative
		// reason. An empty result (NULL exit_reason — a pre-migration
		// corpse, or a death the reaper has not swept yet) renders as a
		// plain exit, never as a crash.
		if reason, err := db.GetSessionExitReason(pick.ID); err == nil {
			out.ExitReason = reason
		} else {
			slog.Warn("dashboard: read exit_reason failed",
				"session", pick.ID, "error", err)
		}
	}
	// Brokered-request refusals recorded against any of this conv's rows.
	// Purely daemon-derived; see the field comments and broker_refusals.go.
	//
	// Deliberately EVERY row, not just `pick`. The broker keys refusals on
	// the row its pid walk resolved, which for a conv with more than one
	// row need not be the one the dashboard chose to render from — and the
	// pid-reuse case this exists for produces exactly such a conv. Looking
	// only at `pick` would drop the badge on the floor in the headline
	// scenario, which is the failure mode being fixed, not a cosmetic miss.
	// The rows are already in hand, so the scan is free.
	for _, r := range rows {
		refusal := brokerRefusals.forSession(r.ID)
		if refusal == nil || refusal.Count <= out.BrokerRefusals {
			continue
		}
		out.BrokerRefusals = refusal.Count
		out.BrokerRefusalDetail = refusal.Reason
		out.BrokerRefusalSince = refusal.First.UTC().Format(time.RFC3339)
	}
	if recovery, err := db.AgentRecoveryForConv(pick.ConvID); err == nil && recovery != nil {
		now := time.Now()
		active := recoveryStatusVisible(*recovery, pick.LastHook, alive, now)
		if active {
			out.RecoveryStatus = recovery.Status
			out.RecoveryReason = recovery.ReasonCode
			out.RecoveryCount = recovery.ConsecutiveCrashes
			out.RecoveryBackoff = recovery.BackoffSeconds
			out.RecoveryLastExitCode = recovery.LastExitCode
			if !recovery.NextAttemptAt.IsZero() {
				out.RecoveryNextAttempt = recovery.NextAttemptAt.UTC().Format(time.RFC3339Nano)
			}
			out.RecoveryDetail = recoveryStateDetail(*recovery, now)
		}
	} else if err != nil {
		slog.Warn("dashboard: read agent recovery failed", "conv", pick.ConvID, "error", err)
	}
	return out
}

// recoveryStatusVisible keeps durable crash-loop history separate from the
// short-lived operational badge. Recovery remains persisted for the 30-minute
// healthy reset and audit trail, but a live agent stops looking exceptional on
// its first post-confirmation hook or after one minute without another hook.
func recoveryStatusVisible(r db.AgentRecovery, lastHook time.Time, alive bool, now time.Time) bool {
	// A manual resume temporarily shares the recovery ledger so its successor
	// can prove enough healthy runtime to reset the crash streak. It is not an
	// automatic recovery, however, and must never surface as one operationally.
	if r.ReasonCode == db.AgentRecoveryReasonManualResume {
		return false
	}
	if r.Status == db.AgentRecoveryStatusCancelled {
		return false
	}
	if r.Status != db.AgentRecoveryStatusRecovered {
		return true
	}
	if !alive || r.RecoveredAt.IsZero() {
		return false
	}
	if !lastHook.IsZero() && lastHook.After(r.RecoveredAt) {
		return false
	}
	return now.Before(r.RecoveredAt.Add(recoveredStatusMaxAge))
}

func recoveryStateDetail(r db.AgentRecovery, now time.Time) string {
	parts := []string{}
	if r.ConsecutiveCrashes > 0 {
		parts = append(parts, fmt.Sprintf("restart %d", r.ConsecutiveCrashes))
	}
	if r.LastExitCode != nil {
		parts = append(parts, fmt.Sprintf("exit %d", *r.LastExitCode))
	}
	if !r.NextAttemptAt.IsZero() {
		remaining := r.NextAttemptAt.Sub(now)
		if remaining < 0 {
			remaining = 0
		}
		parts = append(parts, "retry in "+remaining.Round(time.Second).String())
	}
	if r.ReasonCode != "" {
		parts = append(parts, strings.ReplaceAll(r.ReasonCode, "_", " "))
	}
	return strings.Join(parts, " · ")
}
