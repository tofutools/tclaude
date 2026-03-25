package statusbar

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common"
)

// StatusLineConfig matches the Claude Code statusLine settings format
type StatusLineConfig struct {
	Type    string `json:"type"`
	Command string `json:"command"`
}

// StatusLineCommand is the status-bar command (detected at startup)
var StatusLineCommand = common.DetectCmd("status-bar")

// ReinitStatusLineCommand re-evaluates the status-bar command path using current DetectCmd settings.
// Call this after changing common.SetAbsolutePaths().
func ReinitStatusLineCommand() {
	StatusLineCommand = common.DetectCmd("status-bar")
}

// isOurStatusBar returns true if the command is a tclaude status-bar command,
// including absolute paths like /usr/local/bin/tclaude status-bar
func isOurStatusBar(command string) bool {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return false
	}
	base := filepath.Base(fields[0])
	return strings.HasSuffix(command, "status-bar") && (base == "tclaude")
}

// CheckInstalled checks if the current tclaude status-bar command is configured in Claude settings.
// Returns true only if the command matches the current binary exactly, not a stale reference.
func CheckInstalled() bool {
	settingsPath := claudeSettingsPath()
	if settingsPath == "" {
		return false
	}

	data, err := os.ReadFile(settingsPath)
	if err != nil {
		return false
	}

	var settings map[string]json.RawMessage
	if err := json.Unmarshal(data, &settings); err != nil {
		return false
	}

	slRaw, ok := settings["statusLine"]
	if !ok {
		return false
	}

	var sl StatusLineConfig
	if err := json.Unmarshal(slRaw, &sl); err != nil {
		return false
	}

	return sl.Type == "command" && sl.Command == StatusLineCommand
}

// Install configures the tclaude status-bar as the statusLine command in Claude settings
func Install() error {
	settingsPath := claudeSettingsPath()
	if settingsPath == "" {
		return fmt.Errorf("cannot determine Claude settings path")
	}

	// Ensure .claude directory exists
	claudeDir := filepath.Dir(settingsPath)
	if err := os.MkdirAll(claudeDir, 0755); err != nil {
		return fmt.Errorf("failed to create .claude directory: %w", err)
	}

	// Read existing settings or start with empty object
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

	// Set the statusLine config
	sl := StatusLineConfig{
		Type:    "command",
		Command: StatusLineCommand,
	}
	slJSON, err := json.Marshal(sl)
	if err != nil {
		return fmt.Errorf("failed to serialize statusLine config: %w", err)
	}
	settings["statusLine"] = slJSON

	// Write back with pretty formatting
	output, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to serialize settings: %w", err)
	}

	if err := os.WriteFile(settingsPath, output, 0644); err != nil {
		return fmt.Errorf("failed to write settings: %w", err)
	}

	return nil
}

func claudeSettingsPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "settings.json")
}
