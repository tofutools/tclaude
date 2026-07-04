package setup

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/session"
)

// This file owns the "=== AskUserQuestion Timeout ===" setup step: recommending
// (and, interactively, offering to set) the top-level "askUserQuestionTimeout"
// key in the user-level Claude Code settings.json (~/.claude/settings.json).
//
// Why tclaude cares: Claude Code (>= 2.1.x) no longer auto-continues an
// AskUserQuestion dialog by default — it waits for a human. A tclaude-spawned
// agent runs UNATTENDED, so that default makes it STALL the moment it raises a
// question. Setting askUserQuestionTimeout to an interval (e.g. "5m") makes
// Claude Code auto-continue the dialog with its default answer after the agent
// sits idle — the setting that keeps an unattended fleet acting agentically.
//
// This is a global setting (it also affects the operator's own interactive
// sessions), so — per the "don't modify by default" project directive — this
// step never silently writes it: it prints a recommendation always, offers to
// set it only in an interactive run (default no), and is skipped under --yes.
// Per-agent / per-profile overrides (the spawn dialog + profile editor) are the
// non-global way to enable it for just the agents, delivered as a `--settings`
// override that leaves settings.json untouched.

// recommendedAskTimeout is the interval this step offers to set — matching the
// value the operator runs with. It is one of Claude Code's options
// (never|60s|5m|10m); "5m" gives an unattended agent a few minutes to be
// answered before it proceeds on its own.
const recommendedAskTimeout = "5m"

// askTimeoutSettingsKey is the top-level settings.json key Claude Code reads the
// AskUserQuestion idle-timeout from.
const askTimeoutSettingsKey = "askUserQuestionTimeout"

// readClaudeAskTimeout returns the top-level "askUserQuestionTimeout" value from
// the Claude Code settings file at settingsPath. Same contract as
// readClaudeTUIMode: a missing/empty file or absent key is (present=false); a
// present value is returned as a string; a corrupt file returns an error so the
// caller never clobbers an unparseable settings.json.
func readClaudeAskTimeout(settingsPath string) (value string, present bool, err error) {
	data, rerr := os.ReadFile(settingsPath)
	if rerr != nil {
		if os.IsNotExist(rerr) {
			return "", false, nil
		}
		return "", false, rerr
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return "", false, nil
	}
	var tree map[string]any
	if uerr := json.Unmarshal(data, &tree); uerr != nil {
		return "", false, fmt.Errorf("parse %s: %w", settingsPath, uerr)
	}
	raw, ok := tree[askTimeoutSettingsKey]
	if !ok || raw == nil {
		return "", false, nil
	}
	if s, ok := raw.(string); ok {
		return s, true, nil
	}
	return fmt.Sprintf("%v", raw), true, nil
}

// writeClaudeAskTimeout sets the top-level "askUserQuestionTimeout" key in the
// Claude Code settings file at settingsPath, preserving every other key and the
// file's permission bits — the same care as enableFullscreenTUI. A missing file
// is created with just the one key; a corrupt file fails the write rather than
// being replaced (it also carries hooks, permissions, sandbox config).
func writeClaudeAskTimeout(settingsPath, value string) error {
	tree := map[string]any{}
	mode := os.FileMode(0o644)

	data, err := os.ReadFile(settingsPath)
	switch {
	case err == nil:
		if info, statErr := os.Stat(settingsPath); statErr == nil {
			mode = info.Mode().Perm()
		}
		if len(strings.TrimSpace(string(data))) > 0 {
			if uerr := json.Unmarshal(data, &tree); uerr != nil {
				return uerr
			}
			if tree == nil { // file held a literal `null`
				tree = map[string]any{}
			}
		}
	case os.IsNotExist(err):
		if mkErr := os.MkdirAll(filepath.Dir(settingsPath), 0o755); mkErr != nil {
			return mkErr
		}
	default:
		return err
	}

	tree[askTimeoutSettingsKey] = value

	out, merr := json.MarshalIndent(tree, "", "  ")
	if merr != nil {
		return merr
	}
	return os.WriteFile(settingsPath, append(out, '\n'), mode)
}

// configureAskUserQuestionTimeout handles the "=== AskUserQuestion Timeout ==="
// install step.
//
// It respects an existing value: any present "askUserQuestionTimeout" (an
// interval or a deliberate "never") is left exactly as-is, so a re-run never
// nags. On a fresh config (no key) it prints the recommendation and — only in an
// interactive run — offers to set the recommended interval (default no). It
// never writes under --yes: this is a global behaviour change, so a scripted
// setup only prints the advice, matching the "don't modify by default" policy.
func configureAskUserQuestionTimeout(params *Params) {
	settingsPath := session.ClaudeSettingsPath()
	if settingsPath == "" {
		fmt.Println("  Warning: could not determine Claude Code settings path — skipping")
		return
	}

	value, present, err := readClaudeAskTimeout(settingsPath)
	switch {
	case err != nil:
		// Never rewrite an unparseable settings.json — it also holds hooks,
		// permissions, sandbox config. Warn and leave it for the operator.
		fmt.Printf("  ⚠ Could not read %s (%v) — leaving it untouched\n", settingsPath, err)
		return
	case present && value == "never":
		fmt.Println("  Claude Code askUserQuestionTimeout is set to \"never\" in your config — leaving it as-is.")
		fmt.Println("  (unattended agents will WAIT for a human on a question; set an interval like \"5m\" to")
		fmt.Println("   auto-continue, or override per-agent in the dashboard spawn dialog / profile editor)")
		return
	case present:
		fmt.Printf("✓ AskUserQuestion auto-continue timeout already set to %q\n", value)
		return
	}

	// Absent — print the recommendation (the operator asked for this nudge).
	fmt.Println("  Claude Code no longer auto-continues an AskUserQuestion dialog by default — it waits")
	fmt.Println("  for a human. A tclaude-spawned agent runs unattended, so it will STALL on a question.")
	fmt.Printf("  For agentic operation it's highly recommended to set askUserQuestionTimeout (e.g. %q):\n", recommendedAskTimeout)
	fmt.Println("  the dialog then auto-continues with its default answer after the agent sits idle.")
	fmt.Println("  Note: this is a GLOBAL setting — it also affects your own interactive Claude Code")
	fmt.Println("  sessions. To enable it for only the agents, use the per-agent / per-profile")
	fmt.Println("  \"Question timeout\" selector in the dashboard spawn dialog instead (no global change).")

	if params.Yes {
		// A scripted run must not silently change global interactive behaviour.
		fmt.Printf("  (not setting it under --yes; add \"%s\": %q to %s to enable, or set it per-agent)\n",
			askTimeoutSettingsKey, recommendedAskTimeout, settingsPath)
		return
	}

	if askYesNo(fmt.Sprintf("Set askUserQuestionTimeout to %q now (applies to ALL your Claude Code sessions)?", recommendedAskTimeout), false, false) {
		if werr := writeClaudeAskTimeout(settingsPath, recommendedAskTimeout); werr != nil {
			fmt.Printf("  Warning: failed to set askUserQuestionTimeout: %v\n", werr)
			return
		}
		fmt.Printf("✓ askUserQuestionTimeout set to %q\n", recommendedAskTimeout)
	} else {
		fmt.Println("  Skipped. Enable later with: tclaude setup, per-agent in the dashboard, or by hand.")
	}
}

// checkAskUserQuestionTimeout reports the AskUserQuestion-timeout status for
// `tclaude setup --check`. Read-only — it never writes.
func checkAskUserQuestionTimeout() {
	settingsPath := session.ClaudeSettingsPath()
	if settingsPath == "" {
		fmt.Println("⚠ Could not determine Claude Code settings path")
		return
	}
	value, present, err := readClaudeAskTimeout(settingsPath)
	switch {
	case err != nil:
		fmt.Printf("⚠ Could not read settings.json: %v\n", err)
	case present && value == "never":
		fmt.Println("  askUserQuestionTimeout is \"never\" (unattended agents wait for a human)")
		fmt.Println("  Set an interval like \"5m\", or override per-agent in the dashboard")
	case present:
		fmt.Printf("✓ askUserQuestionTimeout set to %q (unattended agents auto-continue)\n", value)
	default:
		fmt.Println("✗ askUserQuestionTimeout not set — unattended agents will stall on a question")
		fmt.Println("  Run 'tclaude setup' to set it, or override per-agent in the dashboard spawn dialog")
	}
}
