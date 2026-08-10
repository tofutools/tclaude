// Package statusbar provides the tclaude status-bar command for Claude Code's statusline feature.
package statusbar

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/money"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/common"
	"golang.org/x/term"
)

const (
	colorGreen       = "\033[32m"
	colorYellow      = "\033[33m"
	colorRed         = "\033[31m"
	colorCyan        = "\033[36m"
	colorDim         = "\033[2m"
	colorReset       = "\033[0m"
	barWidth         = 10
	gitCacheTTL      = 15 * time.Second
	compactionBuffer = 16.5 // percent reserved for compaction
)

// StatusLineInput represents the JSON Claude Code sends to the statusline command
type StatusLineInput struct {
	// SessionID is Claude Code's *current* conversation id — survives a
	// /clear by rotating with it, so the statusbar can key its DB writes
	// on whichever conv-id the dashboard's ResolveLocation will look up.
	// Optional: not every Claude Code version emits it; the statusbar
	// falls back to TCLAUDE_SESSION_ID (the launch-time conv-id).
	SessionID string `json:"session_id"`
	Model     struct {
		// ID is the full model ID ("claude-fable-5") — unlike the
		// display name it round-trips into `claude --model`, so the
		// statusbar persists it for reincarnate/clone/resume model
		// inheritance. Absent on older Claude Code versions.
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
	} `json:"model"`
	Version   string `json:"version"`
	Workspace struct {
		CurrentDir string `json:"current_dir"`
	} `json:"workspace"`
	ContextWindow struct {
		UsedPercentage    *float64 `json:"used_percentage"`
		TotalInputTokens  *int64   `json:"total_input_tokens"`
		TotalOutputTokens *int64   `json:"total_output_tokens"`
		ContextWindowSize *int64   `json:"context_window_size"`
	} `json:"context_window"`
	Cost struct {
		TotalCostUSD float64 `json:"total_cost_usd"`
	} `json:"cost"`
	RateLimits *RateLimits `json:"rate_limits"`
	// Effort is Claude Code's live reasoning-effort level. The block is
	// absent when the current model doesn't support the reasoning-effort
	// parameter, so Level is "" in that case (and ultracode reports as
	// "xhigh", not a distinct level).
	Effort struct {
		Level string `json:"level"` // low | medium | high | xhigh | max
	} `json:"effort"`
}

// RateLimits represents the rate limit buckets from Claude Code's statusline input.
type RateLimits struct {
	FiveHour       *RateLimitBucket `json:"five_hour"`
	SevenDay       *RateLimitBucket `json:"seven_day"`
	SevenDaySonnet *RateLimitBucket `json:"seven_day_sonnet"`
}

// RateLimitBucket represents a single rate limit bucket with usage and reset time.
type RateLimitBucket struct {
	UsedPercentage float64 `json:"used_percentage"`
	ResetsAt       int64   `json:"resets_at"` // unix timestamp
}

// GitSnapshot holds cached results from git/gh commands. Extra PR
// fields (Number, State) ride the same cache entry so the dashboard's
// agent_workspace row can store the full PR snapshot — the statusbar
// itself only renders the URL.
type GitSnapshot struct {
	RepoURL       string    `json:"repo_url"`
	Branch        string    `json:"branch"`
	DefaultBranch string    `json:"default_branch"`
	PRNumber      int       `json:"pr_number,omitempty"`
	PRURL         string    `json:"pr_url,omitempty"`
	PRState       string    `json:"pr_state,omitempty"`
	FetchedAt     time.Time `json:"fetched_at"`
}

type Params struct{}

func Cmd() *cobra.Command {
	cmd := boa.CmdT[Params]{
		Use:         "status-bar",
		Short:       "Status bar output for Claude Code statusline",
		Long:        "Reads JSON session data from stdin (provided by Claude Code) and prints status bar output.\nConfigure in ~/.claude/settings.json as a statusLine command.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(params *Params, cmd *cobra.Command, args []string) {
			if err := run(); err != nil {
				slog.Error("status-bar failed", "error", err, "module", "hooks")
				os.Exit(1)
			}
		},
	}.ToCobra()
	cmd.Hidden = true
	return cmd
}

// gitCacheKey returns a hash key for the current git repo root.
func gitCacheKey() string {
	repoRoot := gitCmd("rev-parse", "--show-toplevel")
	if repoRoot == "" {
		return ""
	}
	h := sha256.Sum256([]byte(repoRoot))
	return hex.EncodeToString(h[:8])
}

func loadGitCache() *GitSnapshot {
	key := gitCacheKey()
	if key == "" {
		return nil
	}
	// Inside the sandbox the database is not writable, and this snapshot
	// is pure cache the pane can regather for itself — so it moves to the
	// pane's own /tmp rather than through the broker. See render_cache.go.
	if brokerRenders() {
		return loadLocalGitCache(key)
	}
	row, err := db.LoadGitCache(key)
	if err != nil || row == nil {
		return nil
	}
	var cached GitSnapshot
	if err := json.Unmarshal(row.Data, &cached); err != nil {
		return nil
	}
	if time.Since(cached.FetchedAt) > gitCacheTTL {
		return nil
	}
	return &cached
}

func saveGitCache(g *GitSnapshot) {
	data, err := json.Marshal(g)
	if err != nil {
		return
	}
	key := gitCacheKey()
	if key == "" {
		return
	}
	if brokerRenders() {
		saveLocalGitCache(key, g)
		return
	}
	if err := db.SaveGitCache(key, data, g.FetchedAt); err != nil {
		slog.Warn("failed to save git cache", "error", err, "module", "hooks")
	}
}

// ownedSessionID resolves which tclaude session row a statusline render
// may write, returning "" when the render must not touch any row.
//
// This is the statusline's half of the foreign-process guard ApplyHook
// already applies to hooks. Every per-session statusbar write (context
// snapshot, model, model id, effort, cost, the verbatim payload) is keyed
// by TCLAUDE_SESSION_ID — a plain environment variable that every child
// process inherits. Nothing else about the render was ever checked, so
// ANY nested Claude Code launched from an agent's own pane or Bash — a
// `claude` a human starts to try something, a harness-launched
// teammate — renders its own statusline against the PARENT's row and
// silently rewrites the parent's model, effort and context usage with its
// own. Reproduced on Claude Code 2.1.220: a child launched with the
// parent's TCLAUDE_SESSION_ID and `--model haiku` wrote "Haiku 4.5" and
// its own 200K/17% context onto the parent agent's row, which is exactly
// the "my Opus agent shows Haiku and impossible context numbers"
// dashboard report this guard exists to stop.
//
// renderConvID is Claude Code's own session_id from the payload — the
// conversation the render actually describes. A render belongs to the row
// when it names the conversation that row tracks, or the next-conv a
// transition already announced (pending_conv, so a /clear or /resume
// rotation is not read as a foreign process — same acceptance rule as
// the hook guard).
//
// Deliberately fail-soft in the three "no evidence of a mismatch" cases:
// a payload with no session_id at all (Claude Code versions that predate
// the field), an unreadable row, and a row that has not learned its
// conv-id yet (a freshly spawned agent renders before its first
// SessionStart hook lands). Failing closed there would cost real agents
// their telemetry to protect against a case we cannot even observe.
func ownedSessionID(envSessionID, renderConvID string) string {
	if envSessionID == "" || renderConvID == "" {
		return envSessionID
	}
	rowConv, pendingConv, err := db.GetSessionConvAttribution(envSessionID)
	if err != nil {
		slog.Warn("status-bar: failed to read session conv attribution; allowing write",
			"session_id", envSessionID, "error", err, "module", "hooks")
		return envSessionID
	}
	if rowConv == "" || renderConvID == rowConv || renderConvID == pendingConv {
		return envSessionID
	}
	slog.Debug("status-bar: ignoring statusline render from a foreign conversation",
		"session_id", envSessionID, "tracked_conv", rowConv, "render_conv", renderConvID,
		"module", "hooks")
	return ""
}

func run() error {
	if os.Getenv("TCLAUDE_IGNORE_HOOKS") != "" {
		return nil
	}

	// Read JSON from stdin (only if piped, not a terminal)
	var stdinData []byte
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		var err error
		stdinData, err = io.ReadAll(os.Stdin)
		if err != nil {
			return fmt.Errorf("failed to read stdin: %w", err)
		}
	}

	// Parse input
	var input StatusLineInput
	if len(stdinData) > 0 {
		if err := json.NewDecoder(bytes.NewReader(stdinData)).Decode(&input); err != nil {
			slog.Error("status-bar: failed to parse input", "error", err, "raw_input", string(stdinData), "module", "hooks")
			return fmt.Errorf("failed to parse stdin JSON: %w", err)
		}
	} else {
		return fmt.Errorf("no input received on stdin")
	}

	envSessionID := os.Getenv("TCLAUDE_SESSION_ID")

	// Short model label for the head of the context line. Prefers CC's
	// display name ("Opus 4.6" -> "o4.6"); when that's missing or a lone
	// token it falls back to the requested full model ID so a model CC
	// doesn't recognise yet (e.g. `--model claude-opus-5`) shows "opus-5"
	// instead of an "unknown model" placeholder.
	modelLabel := shortModelLabel(input.Model.DisplayName, input.Model.ID)

	// === Line 1: git-links ===
	var line1 []string

	// Git info (skip directory display when in a git repo). getGitData
	// is the structured source; we render it here AND publish the same
	// snapshot to agent_workspace so the dashboard's ResolveLocation
	// stays as fresh as what the human sees on screen.
	gitData := getGitData()
	var branch, links string
	if gitData != nil {
		branch = gitData.Branch
		links = buildGitLinksFromData(gitData)
	}
	if branch != "" {
		line1 = append(line1, fmt.Sprintf("%s[%s]%s", colorCyan, branch, colorReset))
	}
	if links != "" {
		line1 = append(line1, "🔗 "+links)
	} else if branch == "" {
		if dir := input.Workspace.CurrentDir; dir != "" {
			line1 = append(line1, "📂 "+dir)
		}
	}

	// Everything host-touching this render implies — the write-authority
	// gate, the eleven writes (workspace snapshot, context snapshot,
	// model, pin, model id, effort, usage cache, verbatim payload, cost)
	// and the four reads the bar needs back — happens in this one call.
	//
	// That consolidation is what lets a `tclaude-layer` agent, whose
	// mount namespace puts the conversation database out of reach, swap
	// the whole lot for a single brokered round trip to agentd without
	// any of the rendering below knowing the difference. See
	// render_host.go; the write set itself is render_shared.go.
	facts := hostState(renderRequest{
		EnvSessionID:    envSessionID,
		RenderConvID:    input.SessionID,
		EnvPinnedWindow: os.Getenv(harness.AutoCompactWindowEnvVar),
		Payload:         stdinData,
		Input:           input,
		Git:             gitData,
		WantUsage:       !hasSubscriptionLimits(input),
	})

	// === Line 2: model+context bar | limit bars with reset timers | cost ===
	//
	// Re-base the percentage onto the window compaction ACTUALLY fires at.
	// Claude Code's used_percentage is always measured against the model's full
	// context window, even when CLAUDE_CODE_AUTO_COMPACT_WINDOW pins compaction
	// lower — the harness documents that decoupling explicitly. Left alone, an
	// agent pinned to 450K of a 1M window would read "21% used" while sitting
	// nearly half-way to its next compaction, which is the opposite of what the
	// bar is for.
	//
	// The pin came back through the ownership guard: a foreign render must
	// not re-base its own bar against the pin recorded for somebody else's
	// agent. Its own environment still governs it.
	derived := deriveRender(input, observedPinnedWindow(os.Getenv(harness.AutoCompactWindowEnvVar)), facts.PinnedWindow)
	effectiveWindow := derived.EffectiveWindow
	ctxPct := derived.CtxPct

	var line2 []string
	if facts.SandboxOff {
		line2 = append(line2, colorRed+"⚠ SB-OFF"+colorReset)
	}
	ctxLabel := fmt.Sprintf("%d%%", ctxPct)
	line2 = append(line2, fmt.Sprintf("%s%s %s %s",
		modelLabel, contextWindowTag(effectiveWindow), contextBar(ctxPct), ctxLabel))

	// Rate limits from Claude Code's statusline input (subscription plan) or cost (API plan).
	// Falls back to Anthropic usage API (cached) when statusline input lacks rate limit data
	// (e.g. before the first API response in a new session).
	hasLimits := false
	if rl := input.RateLimits; rl != nil {
		if rl.FiveHour != nil {
			hasLimits = true
			line2 = append(line2, fmt.Sprintf("5h %s %.0f%% %s",
				progressBar(int(rl.FiveHour.UsedPercentage)),
				rl.FiveHour.UsedPercentage,
				resetTimer(time.Unix(rl.FiveHour.ResetsAt, 0))))
		}
		if rl.SevenDay != nil {
			hasLimits = true
			line2 = append(line2, fmt.Sprintf("7d %s %.0f%% %s",
				progressBar(int(rl.SevenDay.UsedPercentage)),
				rl.SevenDay.UsedPercentage,
				resetTimer(time.Unix(rl.SevenDay.ResetsAt, 0))))
		}
		if rl.SevenDaySonnet != nil && rl.SevenDaySonnet.UsedPercentage > 0 {
			hasLimits = true
			line2 = append(line2, fmt.Sprintf("sonnet %.0f%% %s",
				rl.SevenDaySonnet.UsedPercentage,
				resetTimer(time.Unix(rl.SevenDaySonnet.ResetsAt, 0))))
		}

	}

	// Fallback: use Anthropic usage API cache when statusline input has no rate limits
	if !hasLimits {
		if usage, stale := facts.Usage, facts.UsageStale; usage != nil {
			if stale {
				slog.Warn("status-bar: using stale usage cache", "module", "hooks")
			}
			if usage.FiveHour != nil {
				hasLimits = true
				label := "5h"
				if stale {
					label = "~5h"
				}
				line2 = append(line2, fmt.Sprintf("%s %s %.0f%% %s",
					label,
					progressBar(int(usage.FiveHour.Pct)),
					usage.FiveHour.Pct,
					resetTimer(usage.FiveHour.ResetsAt)))
			}
			if usage.SevenDay != nil {
				hasLimits = true
				label := "7d"
				if stale {
					label = "~7d"
				}
				line2 = append(line2, fmt.Sprintf("%s %s %.0f%% %s",
					label,
					progressBar(int(usage.SevenDay.Pct)),
					usage.SevenDay.Pct,
					resetTimer(usage.SevenDay.ResetsAt)))
			}
			if usage.SevenDaySonnet != nil && usage.SevenDaySonnet.Pct > 0 {
				hasLimits = true
				line2 = append(line2, fmt.Sprintf("sonnet %.0f%% %s",
					usage.SevenDaySonnet.Pct,
					resetTimer(usage.SevenDaySonnet.ResetsAt)))
			}
		}
	}

	// Cost only shown on API plan (no rate limit buckets available)
	if !hasLimits && input.Cost.TotalCostUSD > 0 {
		line2 = append(line2, money.USD(input.Cost.TotalCostUSD))
	}

	// Reasoning-effort level (🧠 high) trails the first line, far right.
	// Absent when the model lacks reasoning-effort support — appended only
	// when set so there's no empty trailing token. Printed in the
	// terminal's default foreground (no colour code), matching the model
	// label at the start of this line — colorDim made it too dark to read.
	if input.Effort.Level != "" {
		line2 = append(line2, fmt.Sprintf("🧠 %s", input.Effort.Level))
	}

	fmt.Println(strings.Join(line2, " | "))

	// === Line 3: git-links ===
	if len(line1) > 0 {
		fmt.Println(strings.Join(line1, " | "))
	}

	return nil
}

// shortModelLabel derives the compact model tag shown at the head of the
// statusbar's context line from Claude Code's statusline model block.
//
// It prefers the human display name and keeps the long-standing compact
// style for a multi-word name ("Opus 4.6" -> "o4.6"). When the display
// name is absent or a single token — which is what Claude Code emits for
// a model it doesn't recognise yet, e.g. one selected by full ID via
// `--model claude-opus-5` — it falls back to the requested model string:
// the display name when present, otherwise the full model ID with the
// "claude-" vendor prefix and any "[1m]" context-window suffix trimmed
// ("claude-opus-5" -> "opus-5"). Only when neither field carries anything
// does it use the "ukn-mdl" (unknown model) placeholder, so the statusline
// never renders a meaningless label when a real model string is available.
//
// The label names the MODEL only. Any context-window marker Claude Code bakes
// into its display name ("Opus 5 (1M context)") is stripped here, because the
// caller appends the window the bar is actually measured against — see
// contextWindowTag. Leaving it in produced the worst possible reading: the
// mashed-together "o5(1Mcontext)" advertised a 1M window on a pane whose bar
// was re-based onto a 450k pin, so the one window number on the line was the
// one number that did not apply.
func shortModelLabel(displayName, id string) string {
	// unknownModel is the last-resort tag when neither display_name nor id
	// carries anything — a distinct "unknown model" marker rather than a
	// label that could be mistaken for a real model name.
	const unknownModel = "ukn-mdl"
	raw := strings.TrimSpace(displayName)
	if raw == "" {
		raw = strings.TrimSpace(id)
	}
	raw = strings.TrimSpace(trimContextParenthetical(raw))
	// Drop a trailing [1m]/[1M] 1M-window suffix; the label never showed it.
	if len(raw) >= len("[1m]") && strings.EqualFold(raw[len(raw)-len("[1m]"):], "[1m]") {
		raw = strings.TrimSpace(raw[:len(raw)-len("[1m]")])
	}
	if raw == "" {
		return unknownModel
	}
	if parts := strings.Fields(raw); len(parts) >= 2 {
		return strings.ToLower(string([]rune(parts[0])[0])) + strings.Join(parts[1:], "")
	}
	// Single token: a full model ID or a one-word display name. Strip the
	// "claude-" vendor prefix so "claude-opus-5" reads as "opus-5".
	return strings.TrimPrefix(strings.ToLower(raw), "claude-")
}

// resolvePinnedWindow reports the auto-compaction window this pane is running
// under, in tokens, as (observed, resolved). Both are 0 when nothing is pinned —
// which is the ORDINARY case: most agents never pin a window and simply run on
// Claude Code's own per-model default. Neither an absent variable nor an absent
// session row is an error, and neither produces a diagnostic.
//
// `observed` is what this process found in its own environment; `resolved` is
// what the bar should be measured against. They differ only when the environment
// carried nothing and the session row supplied a window.
//
// WHY THE ENVIRONMENT COMES FIRST. The status line runs inside the agent's pane,
// so the variable it sees is exactly the one Claude Code is acting on — including
// a window an operator exported by hand, outside any tclaude launch, which no row
// would ever know about.
//
// WHY THE ROW IS CONSULTED AT ALL. A hook process is not guaranteed to inherit
// the pane's environment. When it does not, an environment-only read leaves the
// pane's meter measured against the model's full window while the dashboard —
// which reads this same column — measures against the pin. Same agent, same
// moment, two different answers, and the pane is the wrong one. That asymmetry is
// exactly what an operator reported after the window landed.
//
// WHY ONLY `observed` MAY BE WRITTEN BACK. The row write's safety property is
// "an observer may only ADD a pin the launch did not know about, never erase
// one". That holds for something seen live in the environment; echoing a value
// that came FROM the row back to it proves nothing and only muddies which writer
// last spoke.
func resolvePinnedWindow(envValue, sessionID string) (observed, resolved int64) {
	observed = observedPinnedWindow(envValue)
	if observed > 0 {
		return observed, observed
	}
	// The row's value is already canonical (SetSessionAutoCompactWindow stores
	// what ResolveAutoCompactWindow parsed), so AutoCompactWindowTokens is enough
	// here — the bounds check the environment path needs was applied at the launch
	// boundary that wrote it. A read error is fail-soft for the same reason an
	// unparseable environment value is: no pin, the model's window stands.
	window, err := db.GetSessionAutoCompactWindow(sessionID)
	if err != nil {
		slog.Warn("status-bar: failed to read session auto-compact window",
			"error", err, "module", "hooks")
		return observed, 0
	}
	return observed, harness.AutoCompactWindowTokens(window)
}

// trimContextParenthetical removes a trailing parenthesised context-window
// marker from a model name ("Opus 5 (1M context)" -> "Opus 5").
//
// It matches on the word "context" rather than on "any trailing parenthetical"
// deliberately. Claude Code is free to qualify a display name for reasons that
// have nothing to do with capacity, and a future "Opus 5 (fast)" must keep its
// qualifier: dropping it would make two genuinely different models render as the
// same tag. Only the marker the status line is about to state itself, more
// precisely, is removed.
func trimContextParenthetical(name string) string {
	trimmed := strings.TrimSpace(name)
	if !strings.HasSuffix(trimmed, ")") {
		return name
	}
	open := strings.LastIndex(trimmed, "(")
	if open < 0 {
		return name
	}
	if !strings.Contains(strings.ToLower(trimmed[open:]), "context") {
		return name
	}
	return strings.TrimSpace(trimmed[:open])
}

// contextWindowTag renders the parenthesised window marker the status line
// appends to its model label: the window the context bar beside it is measured
// against — "(450k)" on an agent pinned to 450k, "(1M)" on an unpinned 1M model.
//
// This is the whole point of the label. The percentage is silently re-based onto
// the effective window, so without a marker naming that window a pinned pane and
// an unpinned one render identically and an operator has no way to tell that
// compaction is going to fire at 450k. Only ONE window is ever shown, and it is
// always the effective one; per operator decision the raw model-relative
// percentage is not surfaced alongside it.
//
// An unknown window (no pin AND no context_window_size from the harness) yields
// "", so the label degrades to the bare model tag rather than naming a window
// nobody knows.
func contextWindowTag(effectiveWindow int64) string {
	if formatted := harness.FormatContextWindowTokens(effectiveWindow); formatted != "" {
		return "(" + formatted + ")"
	}
	return ""
}

// gitCmd runs a git command and returns trimmed stdout, or empty string on error.
func gitCmd(args ...string) string {
	out, err := exec.Command("git", args...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// getRepoHTTPS returns the HTTPS URL for the origin remote, or empty string.
func getRepoHTTPS() string {
	raw := gitCmd("remote", "get-url", "origin")
	if raw == "" {
		return ""
	}
	url := raw
	if strings.HasPrefix(url, "git@github.com:") {
		url = strings.Replace(url, "git@github.com:", "https://github.com/", 1)
	}
	url = strings.TrimSuffix(url, ".git")
	return url
}

// getGitData returns the current git snapshot for the statusbar's
// working directory (CC's launch dir): repo URL, current branch,
// default branch, and the open PR. Uses a 15s DB-backed cache (the
// shared git_cache table, keyed by a hash of the repo root) to avoid
// hammering git/gh on every statusline render. Returns nil when we
// aren't in a git repo.
func getGitData() *GitSnapshot {
	if cached := loadGitCache(); cached != nil {
		return cached
	}
	if gitCmd("rev-parse", "--git-dir") == "" {
		return nil
	}
	data := &GitSnapshot{
		RepoURL:       getRepoHTTPS(),
		Branch:        gitCmd("branch", "--show-current"),
		DefaultBranch: getDefaultBranch(),
		FetchedAt:     time.Now(),
	}
	// Only check for PR on feature branches (gh pr view is the slowest call).
	if data.RepoURL != "" && data.Branch != "" && data.DefaultBranch != "" && data.Branch != data.DefaultBranch {
		data.PRNumber, data.PRURL, data.PRState = getPRInfo(data.Branch)
	}
	saveGitCache(data)
	return data
}

// buildGitLinksFromData renders git link text from cached data.
func buildGitLinksFromData(data *GitSnapshot) string {
	if data.RepoURL == "" {
		return ""
	}

	// On default branch or no branch: just show repo URL
	if data.Branch == "" || data.Branch == data.DefaultBranch || data.DefaultBranch == "" {
		return data.RepoURL
	}

	// On a feature branch: show branch diff URL
	diffURL := fmt.Sprintf("%s/compare/%s...%s", data.RepoURL, data.DefaultBranch, data.Branch)

	if data.PRURL != "" {
		return data.PRURL
	}

	return diffURL
}

// getDefaultBranch returns the default branch name (main/master).
func getDefaultBranch() string {
	// Try symbolic ref of origin/HEAD
	ref := gitCmd("symbolic-ref", "refs/remotes/origin/HEAD", "--short")
	if ref != "" {
		// Returns something like "origin/main"
		parts := strings.SplitN(ref, "/", 2)
		if len(parts) == 2 {
			return parts[1]
		}
	}
	// Fallback: check if main or master exist
	if gitCmd("rev-parse", "--verify", "refs/heads/main") != "" {
		return "main"
	}
	if gitCmd("rev-parse", "--verify", "refs/heads/master") != "" {
		return "master"
	}
	return ""
}

// getPRInfo returns the open PR's number, URL, and state for the given
// branch via gh CLI. State is lower-cased to open|merged|closed.
// Returns (0, "", "") when there's no PR, gh isn't installed, or gh
// isn't authenticated — all best-effort, never fatal.
func getPRInfo(branch string) (number int, url, state string) {
	out, err := exec.Command("gh", "pr", "view", branch, "--json", "number,url,state").Output()
	if err != nil {
		return 0, "", ""
	}
	var pr struct {
		Number int    `json:"number"`
		URL    string `json:"url"`
		State  string `json:"state"`
	}
	if json.Unmarshal(out, &pr) != nil {
		return 0, "", ""
	}
	return pr.Number, pr.URL, strings.ToLower(pr.State)
}

// contextBar returns a progress bar for context usage with a compaction
// marker: a two-zone bar where the trailing compaction buffer (~16.5%)
// is rendered as a ▒ zone.
func contextBar(pct int) string {
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}

	// Two-zone bar with compaction buffer as ▒
	effectiveMax := 100.0 - compactionBuffer
	compactionCells := int(math.Round(compactionBuffer * float64(barWidth) / 100))
	usableCells := barWidth - compactionCells
	filled := int(float64(pct) * float64(barWidth) / 100)
	if filled > barWidth {
		filled = barWidth
	}

	usageFraction := float64(pct) / effectiveMax * 100
	color := colorGreen
	if usageFraction >= 85 {
		color = colorRed
	} else if usageFraction >= 60 {
		color = colorYellow
	}

	filledInUsable := filled
	if filledInUsable > usableCells {
		filledInUsable = usableCells
	}
	filledInCompaction := filled - filledInUsable
	emptyUsable := usableCells - filledInUsable
	emptyCompaction := compactionCells - filledInCompaction

	return fmt.Sprintf("%s%s%s%s%s%s%s%s",
		color, strings.Repeat("█", filledInUsable),
		colorDim, strings.Repeat("░", emptyUsable),
		color, strings.Repeat("█", filledInCompaction),
		colorDim+strings.Repeat("▒", emptyCompaction),
		colorReset)
}

// progressBar returns a colored progress bar like "█████░░░░░"
func progressBar(pct int) string {
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	filled := pct * barWidth / 100
	empty := barWidth - filled

	color := colorGreen
	if pct >= 80 {
		color = colorRed
	} else if pct >= 60 {
		color = colorYellow
	}

	return fmt.Sprintf("%s%s%s%s%s",
		color, strings.Repeat("█", filled),
		colorDim, strings.Repeat("░", empty),
		colorReset)
}

// resetTimer returns a human-readable time-until-reset like "4d11h", "2h30m", or "45m"
func resetTimer(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Until(t)
	if d <= 0 {
		return colorDim + "(reset)" + colorReset
	}
	days := int(d.Hours()) / 24
	h := int(d.Hours()) % 24
	m := int(d.Minutes()) % 60
	if days > 0 {
		return fmt.Sprintf("%s(%dd%dh)%s", colorDim, days, h, colorReset)
	}
	if h > 0 {
		return fmt.Sprintf("%s(%dh%dm)%s", colorDim, h, m, colorReset)
	}
	return fmt.Sprintf("%s(%dm)%s", colorDim, m, colorReset)
}
