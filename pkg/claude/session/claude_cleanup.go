package session

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

// EnsureClaudeCleanupPeriod syncs tclaude's configured Claude Code transcript-
// retention override (config claude_cleanup_period_days) into the operator's
// ~/.claude/settings.json `cleanupPeriodDays` key.
//
// Claude Code deletes conversation transcripts (and other stale session data /
// orphaned worktrees) inactive longer than cleanupPeriodDays at startup; its
// built-in default is 30 days. Operators who want their tclaude-managed
// transcripts to survive set claude_cleanup_period_days in tclaude's config
// (or the dashboard Config tab), and tclaude feeds it back into Claude Code's
// own settings file here.
//
// Live-read + idempotent: it reads the current config each call, only rewrites
// settings.json when the on-disk value actually differs, and is a no-op when the
// override is unset (≤ 0) — Claude Code's default, or a hand-set value, is left
// untouched. It's called at every session start alongside EnsureHooksInstalled,
// so a Config-tab change takes effect on the next launch without a restart. A
// sync failure is non-fatal to a session launch; it's logged and returned for
// tests, and callers proceed.
func EnsureClaudeCleanupPeriod() error {
	cfg, err := config.Load()
	if err != nil {
		// A missing / unreadable config is not this feature's problem — the
		// rest of tclaude surfaces that. Nothing to sync here.
		return nil
	}
	days, ok := cfg.ClaudeCleanupPeriodDaysOverride()
	if !ok {
		return nil
	}
	if err := applyClaudeCleanupPeriod(days); err != nil {
		slog.Warn("failed to sync Claude Code transcript retention (cleanupPeriodDays) into settings.json", "days", days, "err", err)
		return err
	}
	return nil
}

// applyClaudeCleanupPeriod writes cleanupPeriodDays=days into ~/.claude/settings.json,
// preserving every other key, and only when the on-disk value differs (so a
// repeated call from each session start is a cheap read with no write). It uses
// the same raw-message merge shape as InstallHooks so unknown settings keys ride
// through untouched.
func applyClaudeCleanupPeriod(days int) error {
	settingsPath := ClaudeSettingsPath()
	if settingsPath == "" {
		return fmt.Errorf("cannot determine Claude settings path")
	}

	claudeDir := filepath.Dir(settingsPath)
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		return fmt.Errorf("failed to create .claude directory: %w", err)
	}

	settings := map[string]json.RawMessage{}
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("failed to read settings: %w", err)
		}
		// Absent settings.json → start from an empty object and create it.
	} else if err := json.Unmarshal(data, &settings); err != nil {
		return fmt.Errorf("failed to parse settings: %w", err)
	}

	// Skip the write when settings.json already holds this exact value — the
	// common case on every session start after the first.
	if cur, ok := settings["cleanupPeriodDays"]; ok {
		var curDays int
		if json.Unmarshal(cur, &curDays) == nil && curDays == days {
			return nil
		}
	}

	val, err := json.Marshal(days)
	if err != nil {
		return fmt.Errorf("failed to serialize cleanupPeriodDays: %w", err)
	}
	settings["cleanupPeriodDays"] = val

	output, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to serialize settings: %w", err)
	}
	if err := os.WriteFile(settingsPath, output, 0o644); err != nil {
		return fmt.Errorf("failed to write settings: %w", err)
	}
	return nil
}
