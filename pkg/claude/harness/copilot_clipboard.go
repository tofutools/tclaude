package harness

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// CopilotCopyOnSelectKey is the documented Copilot settings key that copies an
// alternate-screen TUI selection when its mouse gesture completes.
const CopilotCopyOnSelectKey = "copyOnSelect"

// CopilotCopyOnSelectState is the effective user-level setting after applying
// Copilot's measured settings.json <- config.json top-level precedence.
type CopilotCopyOnSelectState struct {
	Present bool
	Valid   bool
	Enabled bool
	Source  string
}

// ResolveCopilotCopyOnSelect reports the effective copyOnSelect setting. A
// present non-boolean value is deliberately distinguished from absence so a
// setup helper never overwrites an operator value it does not understand.
func ResolveCopilotCopyOnSelect(
	getenv func(string) string,
	home string,
) (CopilotCopyOnSelectState, error) {
	merged, err := ResolveCopilotMergedSettings(getenv, home)
	if err != nil {
		return CopilotCopyOnSelectState{}, err
	}
	source, present := merged[CopilotCopyOnSelectKey]
	if !present || bytes.Equal(bytes.TrimSpace(source.Raw), []byte("null")) {
		return CopilotCopyOnSelectState{}, nil
	}
	state := CopilotCopyOnSelectState{Present: true, Source: source.Path}
	if err := json.Unmarshal(source.Raw, &state.Enabled); err == nil {
		state.Valid = true
	}
	return state, nil
}

// EnableCopilotCopyOnSelect adds copyOnSelect=true to the canonical user
// settings file only when that file still has no value. The caller first checks
// the merged two-file state so an effective legacy config.json choice is also
// respected; this second check protects concurrent tclaude setup processes.
func EnableCopilotCopyOnSelect(getenv func(string) string, home string) error {
	state, err := ResolveCopilotCopyOnSelect(getenv, home)
	if err != nil {
		return err
	}
	if state.Present {
		return nil
	}
	stateDir, err := CopilotStateDirForLaunch(getenv, home)
	if err != nil {
		return err
	}
	settingsPath := filepath.Join(stateDir, CopilotSettingsFileName)
	return EditCopilotConfigFile(settingsPath, 0o600, func(data []byte) (bool, []byte, error) {
		settings, err := parseCopilotSettingsObject(data, CopilotSettingsFileName)
		if err != nil {
			return false, nil, err
		}
		if raw, exists := settings[CopilotCopyOnSelectKey]; exists &&
			!bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return false, nil, nil
		}
		settings[CopilotCopyOnSelectKey] = json.RawMessage("true")
		out, err := json.MarshalIndent(settings, "", "  ")
		if err != nil {
			return false, nil, fmt.Errorf("encode Copilot %s: %w", CopilotSettingsFileName, err)
		}
		return true, append(out, '\n'), nil
	})
}

// AmbientCopilotCopyOnSelectState resolves the current user's Copilot home.
func AmbientCopilotCopyOnSelectState() (CopilotCopyOnSelectState, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return CopilotCopyOnSelectState{}, err
	}
	return ResolveCopilotCopyOnSelect(os.Getenv, home)
}

// EnableAmbientCopilotCopyOnSelect enables the setting in the current user's
// Copilot home.
func EnableAmbientCopilotCopyOnSelect() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	return EnableCopilotCopyOnSelect(os.Getenv, home)
}
