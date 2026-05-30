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
	"strconv"
	"strings"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/usageapi"
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

// cachedGitData holds cached results from git/gh commands. Extra PR
// fields (Number, State) ride the same cache entry so the dashboard's
// agent_workspace row can store the full PR snapshot — the statusbar
// itself only renders the URL.
type cachedGitData struct {
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

func loadGitCache() *cachedGitData {
	key := gitCacheKey()
	if key == "" {
		return nil
	}
	row, err := db.LoadGitCache(key)
	if err != nil || row == nil {
		return nil
	}
	var cached cachedGitData
	if err := json.Unmarshal(row.Data, &cached); err != nil {
		return nil
	}
	if time.Since(cached.FetchedAt) > gitCacheTTL {
		return nil
	}
	return &cached
}

func saveGitCache(g *cachedGitData) {
	data, err := json.Marshal(g)
	if err != nil {
		return
	}
	key := gitCacheKey()
	if key == "" {
		return
	}
	if err := db.SaveGitCache(key, data, g.FetchedAt); err != nil {
		slog.Warn("failed to save git cache", "error", err, "module", "hooks")
	}
}

func run() error {

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

	// Short model label: "Opus 4.6" -> "o4.6"
	model := input.Model.DisplayName
	modelLabel := "ctx"
	if parts := strings.Fields(model); len(parts) >= 2 {
		modelLabel = strings.ToLower(string([]rune(parts[0])[0])) + strings.Join(parts[1:], "")
	}

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

	// Publish the live workspace snapshot (cwd + branch + repo/PR) to
	// agent_workspace, keyed by the session-id Claude Code is using
	// *right now*. Drives the dashboard's "where is this agent" cells on
	// CC's render cadence — independent of agent activity — so a plain
	// `git checkout` in an idle session's launch dir reaches the dashboard
	// within the next statusline render rather than after the next turn.
	// SessionID from CC's stdin tracks /clear rotations; fall back to
	// TCLAUDE_SESSION_ID when CC is too old to emit it.
	sessionID := input.SessionID
	if sessionID == "" {
		sessionID = os.Getenv("TCLAUDE_SESSION_ID")
	}
	if sessionID != "" {
		ws := db.AgentWorkspace{
			ConvID:    sessionID,
			Cwd:       input.Workspace.CurrentDir,
			UpdatedAt: time.Now(),
		}
		if gitData != nil {
			ws.Branch = gitData.Branch
			ws.RepoURL = gitData.RepoURL
			ws.DefaultBranch = gitData.DefaultBranch
			ws.PRNumber = gitData.PRNumber
			ws.PRURL = gitData.PRURL
			ws.PRState = gitData.PRState
		}
		if err := db.UpsertAgentWorkspace(ws); err != nil {
			slog.Warn("status-bar: failed to upsert agent_workspace", "error", err, "module", "hooks")
		}
	}

	// === Line 2: model+context bar | limit bars with reset timers | cost ===
	ctxPct := 0
	if input.ContextWindow.UsedPercentage != nil {
		ctxPct = int(*input.ContextWindow.UsedPercentage)
	}

	// Store context-window snapshot in DB for auto-compact + the agent
	// context-info CLI + the dashboard meter. Only write when the
	// statusline actually carried context data: Claude Code emits
	// renders whose context_window block is empty (e.g. before a
	// turn's first API response), and writing those all-zero values
	// would clobber a good snapshot — the dashboard-meter flicker bug.
	// db.UpdateContextSnapshot also guards against an all-zero write as
	// a backstop; this skip just avoids the pointless DB round-trip.
	cw := input.ContextWindow
	hasContextData := cw.UsedPercentage != nil || cw.TotalInputTokens != nil ||
		cw.TotalOutputTokens != nil || cw.ContextWindowSize != nil
	if sessionID := os.Getenv("TCLAUDE_SESSION_ID"); sessionID != "" && hasContextData {
		var tokIn, tokOut, winSize int64
		if cw.TotalInputTokens != nil {
			tokIn = *cw.TotalInputTokens
		}
		if cw.TotalOutputTokens != nil {
			tokOut = *cw.TotalOutputTokens
		}
		if cw.ContextWindowSize != nil {
			winSize = *cw.ContextWindowSize
		}
		if err := db.UpdateContextSnapshot(sessionID, float64(ctxPct), tokIn, tokOut, winSize); err != nil {
			slog.Warn("status-bar: failed to update context snapshot", "error", err, "module", "hooks")
		}
	}

	// Record the model the agent is running on so the dashboard can show
	// it per-agent. Keyed by TCLAUDE_SESSION_ID (the stable tclaude
	// session id == the sessions-row id the dashboard reads back via
	// GetContextSnapshot) — NOT input.SessionID. Written every render
	// regardless of hasContextData: model.display_name is present even
	// before a turn's first API response, and UpdateSessionModel no-ops
	// on an empty string, so we never blank a good value.
	if sessionID := os.Getenv("TCLAUDE_SESSION_ID"); sessionID != "" {
		if err := db.UpdateSessionModel(sessionID, model); err != nil {
			slog.Warn("status-bar: failed to update session model", "error", err, "module", "hooks")
		}
	}

	var line2 []string
	compactThreshold := autoCompactThreshold()
	ctxLabel := fmt.Sprintf("%d%%", ctxPct)
	if compactThreshold > 0 {
		ctxLabel = fmt.Sprintf("%d%%/%d%%", ctxPct, compactThreshold)
	}
	line2 = append(line2, fmt.Sprintf("%s %s %s", modelLabel, contextBar(ctxPct, compactThreshold), ctxLabel))

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

		// Update SQLite cache so other sessions/consumers see fresh data
		var fh, sd, sds *usageapi.CachedBucket
		if rl.FiveHour != nil {
			fh = &usageapi.CachedBucket{Pct: rl.FiveHour.UsedPercentage, ResetsAt: time.Unix(rl.FiveHour.ResetsAt, 0)}
		}
		if rl.SevenDay != nil {
			sd = &usageapi.CachedBucket{Pct: rl.SevenDay.UsedPercentage, ResetsAt: time.Unix(rl.SevenDay.ResetsAt, 0)}
		}
		if rl.SevenDaySonnet != nil {
			sds = &usageapi.CachedBucket{Pct: rl.SevenDaySonnet.UsedPercentage, ResetsAt: time.Unix(rl.SevenDaySonnet.ResetsAt, 0)}
		}
		usageapi.UpdateFromStatusLine(fh, sd, sds)
	}

	// Fallback: use Anthropic usage API cache when statusline input has no rate limits
	if !hasLimits {
		if usage, err := usageapi.GetCached(); usage != nil {
			if err != nil {
				slog.Warn("status-bar: using stale usage cache", "error", err, "module", "hooks")
			}
			if usage.FiveHour != nil {
				hasLimits = true
				label := "5h"
				if err != nil {
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
				if err != nil {
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
		line2 = append(line2, fmt.Sprintf("$%.2f", input.Cost.TotalCostUSD))
	}

	fmt.Println(strings.Join(line2, " | "))

	// === Line 3: git-links ===
	if len(line1) > 0 {
		fmt.Println(strings.Join(line1, " | "))
	}

	return nil
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
func getGitData() *cachedGitData {
	if cached := loadGitCache(); cached != nil {
		return cached
	}
	if gitCmd("rev-parse", "--git-dir") == "" {
		return nil
	}
	data := &cachedGitData{
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
func buildGitLinksFromData(data *cachedGitData) string {
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

// contextBar returns a progress bar for context usage with a compaction marker.
// When compactThreshold is set (>0), the full bar represents 0-threshold%,
// so it fills completely as usage approaches the compact limit.
// Otherwise uses the default compaction buffer (~16.5%) with a ▒ zone.
func contextBar(pct int, compactThreshold int) string {
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}

	// Custom threshold: full bar represents 0 to threshold
	if compactThreshold > 0 {
		effectiveMax := float64(compactThreshold)
		usageFraction := float64(pct) / effectiveMax * 100
		filled := int(math.Round(float64(pct) / effectiveMax * float64(barWidth)))
		if filled > barWidth {
			filled = barWidth
		}

		color := colorGreen
		if usageFraction >= 85 {
			color = colorRed
		} else if usageFraction >= 60 {
			color = colorYellow
		}

		empty := barWidth - filled
		return fmt.Sprintf("%s%s%s%s%s",
			color, strings.Repeat("█", filled),
			colorDim, strings.Repeat("░", empty),
			colorReset)
	}

	// Default: two-zone bar with compaction buffer as ▒
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

// autoCompactThreshold returns the auto-compact percentage threshold,
// checking the CLI env var first, then the config file. Returns 0 if not set.
func autoCompactThreshold() int {
	if v, err := strconv.Atoi(os.Getenv("TCLAUDE_AUTO_COMPACT")); err == nil && v > 0 {
		return v
	}
	if cfg, err := config.Load(); err == nil && cfg.AutoCompactPercent != nil {
		return *cfg.AutoCompactPercent
	}
	return 0
}
