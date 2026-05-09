package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common"
)

// isOurHook returns true if a hook command belongs to tclaude (any binary variant,
// including absolute paths like /usr/local/bin/tclaude)
func isOurHook(command string) bool {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return false
	}
	base := filepath.Base(fields[0])
	return base == "tclaude"
}

// HookMatcher represents a hook matcher configuration
type HookMatcher struct {
	Matcher string       `json:"matcher,omitempty"`
	Hooks   []HookConfig `json:"hooks"`
}

// HookConfig represents a single hook configuration
type HookConfig struct {
	Type    string `json:"type"`
	Command string `json:"command"`
}

// HookCommand is the unified callback command for all hooks (detected at startup)
var HookCommand string

// RequiredHooks defines the hooks tclaude needs for status tracking
// All hooks use the same unified callback - it reads stdin and figures out what to do
var RequiredHooks map[string][]HookMatcher

func init() {
	initHookCommands()
}

// initHookCommands sets HookCommand and RequiredHooks based on current DetectCmd output.
func initHookCommands() {
	HookCommand = common.DetectCmd("session", "hook-callback")
	hook := HookConfig{Type: "command", Command: HookCommand}
	newMatcher := func() HookMatcher { return HookMatcher{Hooks: []HookConfig{hook}} }
	RequiredHooks = map[string][]HookMatcher{
		"UserPromptSubmit":   {newMatcher()},
		"Stop":               {newMatcher()},
		"PermissionRequest":  {newMatcher()},
		"PreToolUse":         {newMatcher()},
		"PostToolUse":        {newMatcher()},
		"PostToolUseFailure": {newMatcher()},
		"SubagentStart":      {newMatcher()},
		"SubagentStop":       {newMatcher()},
		"Notification":       {{Hooks: []HookConfig{hook}}}, // No matcher = catch all
		"SessionStart":       {newMatcher()},
		"PostCompact":        {newMatcher()},
	}
}

// ReinitHookCommand re-evaluates the hook command path using current DetectCmd settings.
// Call this after changing common.SetAbsolutePaths().
func ReinitHookCommand() {
	initHookCommands()
}

// containsCurrentHook checks if a raw matchers JSON contains the current HookCommand
func containsCurrentHook(matchersJSON string) bool {
	var matchers []HookMatcher
	if err := json.Unmarshal([]byte(matchersJSON), &matchers); err != nil {
		return false
	}
	for _, m := range matchers {
		for _, h := range m.Hooks {
			if h.Command == HookCommand {
				return true
			}
		}
	}
	return false
}

// needsHookCleanup checks if a raw matchers JSON contains stale tclaude hooks
// (wrong binary) or duplicate tclaude hooks that should be deduplicated
func needsHookCleanup(matchersJSON string) bool {
	var matchers []HookMatcher
	if err := json.Unmarshal([]byte(matchersJSON), &matchers); err != nil {
		return false
	}
	ourCount := 0
	for _, m := range matchers {
		for _, h := range m.Hooks {
			if isOurHook(h.Command) {
				if h.Command != HookCommand {
					return true // stale hook (different binary)
				}
				ourCount++
			}
		}
	}
	return ourCount > 1 // duplicate hooks
}

// removeOurHooksFromEvent removes all tclaude hooks from an event's matcher list
func removeOurHooksFromEvent(eventHooksRaw json.RawMessage) (json.RawMessage, bool, error) {
	var matchers []HookMatcher
	if err := json.Unmarshal(eventHooksRaw, &matchers); err != nil {
		return eventHooksRaw, false, err
	}

	var filtered []HookMatcher
	removed := false
	for _, m := range matchers {
		var keptHooks []HookConfig
		for _, h := range m.Hooks {
			if isOurHook(h.Command) {
				removed = true
			} else {
				keptHooks = append(keptHooks, h)
			}
		}
		// Only keep the matcher if it still has non-tclaude hooks
		if len(keptHooks) > 0 {
			filtered = append(filtered, HookMatcher{Matcher: m.Matcher, Hooks: keptHooks})
		}
	}

	if !removed {
		return eventHooksRaw, false, nil
	}
	if len(filtered) == 0 {
		return nil, true, nil // All hooks were ours, signal to delete event
	}
	newRaw, err := json.Marshal(filtered)
	return newRaw, true, err
}

// ClaudeSettingsPath returns the path to ~/.claude/settings.json
func ClaudeSettingsPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "settings.json")
}

// CheckHooksInstalled checks if tclaude hooks are installed in Claude settings.
// Returns: installed (all required hooks present with current binary), missing event names, needsRepair (stale or duplicate hooks detected).
func CheckHooksInstalled() (installed bool, missing []string, needsRepair bool) {
	settingsPath := ClaudeSettingsPath()
	if settingsPath == "" {
		return false, []string{"all"}, false
	}

	data, err := os.ReadFile(settingsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, []string{"all (settings.json not found)"}, false
		}
		return false, []string{"all (cannot read settings.json)"}, false
	}

	var settings map[string]json.RawMessage
	if err := json.Unmarshal(data, &settings); err != nil {
		return false, []string{"all (invalid settings.json)"}, false
	}

	hooksRaw, ok := settings["hooks"]
	if !ok {
		return false, []string{"all (no hooks section)"}, false
	}

	var hooks map[string]json.RawMessage
	if err := json.Unmarshal(hooksRaw, &hooks); err != nil {
		return false, []string{"all (invalid hooks section)"}, false
	}

	// Check for stale or duplicate hooks
	for _, eventHooks := range hooks {
		if needsHookCleanup(string(eventHooks)) {
			needsRepair = true
			break
		}
	}

	// Check each required hook event
	for event := range RequiredHooks {
		eventHooks, ok := hooks[event]
		if !ok || !containsCurrentHook(string(eventHooks)) {
			missing = append(missing, event)
		}
	}

	return len(missing) == 0, missing, needsRepair
}

// InstallHooks adds tclaude hooks to Claude settings, replacing any existing tclaude hooks
func InstallHooks() error {
	settingsPath := ClaudeSettingsPath()
	if settingsPath == "" {
		return fmt.Errorf("cannot determine Claude settings path")
	}

	claudeDir := filepath.Dir(settingsPath)
	if err := os.MkdirAll(claudeDir, 0755); err != nil {
		return fmt.Errorf("failed to create .claude directory: %w", err)
	}

	var settings map[string]json.RawMessage
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("failed to read settings: %w", err)
		}
		settings = make(map[string]json.RawMessage)
	} else {
		if err := json.Unmarshal(data, &settings); err != nil {
			return fmt.Errorf("failed to parse settings: %w", err)
		}
	}

	var hooks map[string]json.RawMessage
	if hooksRaw, ok := settings["hooks"]; ok {
		if err := json.Unmarshal(hooksRaw, &hooks); err != nil {
			return fmt.Errorf("failed to parse hooks: %w", err)
		}
	} else {
		hooks = make(map[string]json.RawMessage)
	}

	// First pass: remove all tclaude hooks from all events (prevents duplicates)
	for event, eventHooksRaw := range hooks {
		newRaw, removed, err := removeOurHooksFromEvent(eventHooksRaw)
		if err != nil {
			return fmt.Errorf("failed to clean hooks from %s: %w", event, err)
		}
		if removed {
			if newRaw == nil {
				delete(hooks, event)
			} else {
				hooks[event] = newRaw
			}
		}
	}

	// Second pass: add current hooks for all required events
	for event, requiredMatchers := range RequiredHooks {
		eventHooksRaw, exists := hooks[event]
		if exists {
			var existingMatchers []json.RawMessage
			if err := json.Unmarshal(eventHooksRaw, &existingMatchers); err != nil {
				return fmt.Errorf("failed to parse %s hooks: %w", event, err)
			}
			for _, matcher := range requiredMatchers {
				matcherJSON, err := json.Marshal(matcher)
				if err != nil {
					return fmt.Errorf("failed to serialize matcher: %w", err)
				}
				existingMatchers = append(existingMatchers, matcherJSON)
			}
			newEventHooks, err := json.Marshal(existingMatchers)
			if err != nil {
				return fmt.Errorf("failed to serialize %s hooks: %w", event, err)
			}
			hooks[event] = newEventHooks
		} else {
			eventHooks, err := json.Marshal(requiredMatchers)
			if err != nil {
				return fmt.Errorf("failed to serialize %s hooks: %w", event, err)
			}
			hooks[event] = eventHooks
		}
	}

	hooksData, err := json.Marshal(hooks)
	if err != nil {
		return fmt.Errorf("failed to serialize hooks: %w", err)
	}
	settings["hooks"] = hooksData

	output, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to serialize settings: %w", err)
	}

	if err := os.WriteFile(settingsPath, output, 0644); err != nil {
		return fmt.Errorf("failed to write settings: %w", err)
	}

	return nil
}

// EnsureHooksInstalled checks and optionally installs hooks, returning true if ready.
// Stale hooks (wrong/duplicate binary) are always auto-repaired since the user already
// opted into hook management. The autoInstall flag only controls first-time installation.
func EnsureHooksInstalled(autoInstall bool, stdout, stderr *os.File) bool {
	installed, missing, needsRepair := CheckHooksInstalled()
	if installed && !needsRepair {
		return true
	}

	// Always auto-repair stale/duplicate hooks without prompting.
	// The user already opted in to hook management; we're just keeping them consistent.
	if needsRepair {
		if err := InstallHooks(); err != nil {
			fmt.Fprintf(stderr, "Warning: Failed to repair stale hooks: %v\n", err)
		}
		// Re-check after repair
		installed, missing, _ = CheckHooksInstalled()
		if installed {
			return true
		}
	}

	if !autoInstall {
		fmt.Fprintf(stderr, "Warning: tclaude session hooks not installed in Claude settings.\n")
		fmt.Fprintf(stderr, "Missing hooks for: %v\n", missing)
		fmt.Fprintf(stderr, "Install with: tclaude setup\n\n")
		return false
	}

	fmt.Fprintf(stdout, "Installing tclaude session hooks...\n")
	if err := InstallHooks(); err != nil {
		fmt.Fprintf(stderr, "Failed to install hooks: %v\n", err)
		return false
	}
	fmt.Fprintf(stdout, "Hooks installed successfully.\n\n")
	return true
}
