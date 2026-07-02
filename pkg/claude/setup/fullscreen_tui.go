package setup

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/session"
)

// This file owns the "=== Fullscreen TUI ===" setup step: offering to set
// the top-level "tui" key in the user-level Claude Code settings.json
// (~/.claude/settings.json) to "fullscreen".
//
// Why tclaude cares: it runs Claude Code inside tmux panes it detaches and
// reattaches. Claude Code's classic (inline) renderer flickers, jumps the
// scrollback, and fights tmux redraws; its fullscreen renderer uses the
// alternate screen buffer (like vim/htop) — flicker-free, flat memory, and
// tmux-friendly. So tclaude only really works as intended with
// "tui": "fullscreen". The renderer can also be toggled live in Claude Code
// with /tui fullscreen, and CLAUDE_CODE_NO_FLICKER=1 is the env-var
// equivalent; this setup step just makes the persistent settings.json choice
// convenient.

// fullscreenTUIValue is the settings.json "tui" value that selects Claude
// Code's alternate-screen renderer. Claude Code's other documented value is
// "classic" (the default when the key is absent).
const fullscreenTUIValue = "fullscreen"

// readClaudeTUIMode returns the top-level "tui" value from the Claude Code
// settings file at settingsPath.
//
//   - A missing or empty file, or an absent/`null` "tui" key, is reported as
//     (present=false) with no error — "Claude Code decides on its own".
//   - A present "tui" value is returned as a string (a non-string value is
//     rendered generically so a deliberate-but-odd value still reads as
//     "present" and is left untouched).
//   - A corrupt / unreadable file returns an error, so callers never clobber
//     an unparseable settings.json (it also carries hooks, permissions, etc.).
func readClaudeTUIMode(settingsPath string) (mode string, present bool, err error) {
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
	raw, ok := tree["tui"]
	if !ok || raw == nil {
		return "", false, nil
	}
	if s, ok := raw.(string); ok {
		return s, true, nil
	}
	// Present but not a string — surface a non-empty rendering so callers
	// treat it as a deliberate, hands-off value rather than "absent".
	return fmt.Sprintf("%v", raw), true, nil
}

// enableFullscreenTUI sets the top-level "tui" key to "fullscreen" in the
// Claude Code settings file at settingsPath, preserving every other key and
// the file's permission bits (a private 0600 settings file stays 0600 — the
// same care as writeUserDefaultModel and the sandbox hardening). A missing
// file is created with just the one key. A corrupt file fails the write
// rather than being silently replaced by {"tui": "fullscreen"} — the file
// also carries hooks, permissions, and sandbox config.
func enableFullscreenTUI(settingsPath string) error {
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

	tree["tui"] = fullscreenTUIValue

	out, merr := json.MarshalIndent(tree, "", "  ")
	if merr != nil {
		return merr
	}
	return os.WriteFile(settingsPath, append(out, '\n'), mode)
}

// configureFullscreenTUI handles the "=== Fullscreen TUI ===" install step.
//
// It only prompts when the operator has NOT already made a deliberate choice:
// any existing "tui" value (fullscreen or otherwise) is left exactly as-is, so
// a re-run never nags and an intentional "classic" survives — mirroring how
// configureNotifications leaves a deliberately-disabled block alone. On a fresh
// config (no "tui" key) it offers to set "fullscreen" (default yes, honoured by
// --yes for scripted runs).
func configureFullscreenTUI(params *Params) {
	settingsPath := session.ClaudeSettingsPath()
	if settingsPath == "" {
		fmt.Println("  Warning: could not determine Claude Code settings path — skipping")
		return
	}

	mode, present, err := readClaudeTUIMode(settingsPath)
	switch {
	case err != nil:
		// Never rewrite an unparseable settings.json — it also holds hooks,
		// permissions, sandbox config. Warn and leave it for the operator.
		fmt.Printf("  ⚠ Could not read %s (%v) — leaving it untouched\n", settingsPath, err)
	case present && mode == fullscreenTUIValue:
		fmt.Println("✓ Fullscreen TUI already enabled")
	case present:
		// A deliberate, non-fullscreen choice — respect it, don't re-prompt.
		fmt.Printf("✓ Claude Code TUI mode is set to %q in your config — leaving it as-is\n", mode)
		fmt.Println("  (tclaude works best with \"tui\": \"fullscreen\"; switch with /tui fullscreen)")
	default:
		fmt.Println("  tclaude runs Claude Code in tmux; its fullscreen renderer is flicker-free")
		fmt.Println("  and tmux-friendly, so tclaude works best with it.")
		if askYesNo("Enable Claude Code fullscreen TUI mode?", true, params.Yes) {
			if werr := enableFullscreenTUI(settingsPath); werr != nil {
				fmt.Printf("  Warning: failed to enable fullscreen TUI: %v\n", werr)
				return
			}
			fmt.Println("✓ Fullscreen TUI enabled (\"tui\": \"fullscreen\")")
		} else {
			fmt.Println("  Skipped. Enable later with: tclaude setup (or /tui fullscreen in Claude Code)")
		}
	}
}

// checkFullscreenTUI reports the fullscreen-TUI status for
// `tclaude setup --check`. Read-only — it never writes.
func checkFullscreenTUI() {
	settingsPath := session.ClaudeSettingsPath()
	if settingsPath == "" {
		fmt.Println("⚠ Could not determine Claude Code settings path")
		return
	}
	mode, present, err := readClaudeTUIMode(settingsPath)
	switch {
	case err != nil:
		fmt.Printf("⚠ Could not read settings.json: %v\n", err)
	case present && mode == fullscreenTUIValue:
		fmt.Println("✓ Fullscreen TUI enabled")
	case present:
		fmt.Printf("  Claude Code TUI mode set to %q (not fullscreen)\n", mode)
	default:
		fmt.Println("✗ Fullscreen TUI not enabled")
		fmt.Println("  Run 'tclaude setup' to enable (tclaude works best with tui=fullscreen)")
	}
}
