package session

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"

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
	return SyncClaudeCleanupPeriod(cfg)
}

// SyncClaudeCleanupPeriod applies an already-loaded config's transcript-
// retention override to ~/.claude/settings.json. It's the shared core of
// EnsureClaudeCleanupPeriod (which loads config first), and is also called
// directly by agentd right after the dashboard Config tab saves — so a value
// set in the UI takes effect immediately instead of only on the next session
// start. No-op when the override is unset (≤ 0); logs and returns on failure.
func SyncClaudeCleanupPeriod(cfg *config.Config) error {
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
	// Unlocked fast path for the common case (every session start after the
	// first): the value is already on disk, so there is nothing to write and
	// no reason to wait on another process's settings lock. The locked edit
	// below re-reads and re-checks, so a stale read here only costs a lock.
	if path := ClaudeSettingsPath(); path != "" && claudeCleanupPeriodOnDisk(path) == days {
		return nil
	}
	return withClaudeSettingsLock(func(settingsTarget string) error {
		return applyClaudeCleanupPeriodTo(settingsTarget, days)
	})
}

// claudeCleanupPeriodOnDisk returns settings.json's cleanupPeriodDays, or -1
// when the file, the key or a parse is missing.
func claudeCleanupPeriodOnDisk(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return -1
	}
	var settings map[string]json.RawMessage
	if json.Unmarshal(data, &settings) != nil {
		return -1
	}
	var days int
	if raw, ok := settings["cleanupPeriodDays"]; !ok || json.Unmarshal(raw, &days) != nil {
		return -1
	}
	return days
}

// applyClaudeCleanupPeriodTo is the edit itself, run under
// withClaudeSettingsLock so it serializes with InstallHooks (the other tclaude
// writer of this file) and writes the way InstallHooks does: atomically, with
// the file's mode preserved, through a symlinked settings.json.
func applyClaudeCleanupPeriodTo(settingsTarget string, days int) error {
	settings := map[string]json.RawMessage{}
	data, err := os.ReadFile(settingsTarget)
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
	if err := atomicWriteClaudeSettings(settingsTarget, output); err != nil {
		return fmt.Errorf("failed to write settings: %w", err)
	}
	return nil
}
