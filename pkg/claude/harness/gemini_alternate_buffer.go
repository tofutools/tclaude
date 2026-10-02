package harness

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// Gemini CLI's alternate-screen renderer (settings ui.useAlternateBuffer).
//
// By default Gemini renders inline and leaves its history in the terminal's
// scroll-back. tclaude runs it in tmux, whose own alternate screen makes a
// client terminal (the dashboard's included) turn the mouse wheel into
// Up/Down keys, which Gemini reads as prompt history. With the alternate
// buffer on, Gemini owns the screen and requests mouse reporting, so the
// wheel scrolls its own history, the way Claude Code's fullscreen TUI does.
// The setting has no CLI flag or environment variable, so `tclaude setup`
// offers to set it in the user-level settings.json.

// GeminiAlternateBufferState is the user-level ui.useAlternateBuffer value.
type GeminiAlternateBufferState struct {
	Present bool // the key is set
	Valid   bool // and holds a boolean
	Enabled bool
	Source  string
}

// ReadGeminiAlternateBuffer reads ui.useAlternateBuffer from the user-level
// Gemini settings. A file that is not strict JSON is an error: Gemini reads
// JSON-with-comments, which tclaude cannot round-trip.
func ReadGeminiAlternateBuffer() (GeminiAlternateBufferState, error) {
	path := geminiSettingsPath()
	if path == "" {
		return GeminiAlternateBufferState{}, errors.New("cannot determine Gemini settings path")
	}
	state := GeminiAlternateBufferState{Source: path}
	data, err := readFileAllowMissing(path)
	if err != nil {
		return state, err
	}
	_, ui, err := parseGeminiUISettings(data)
	if err != nil {
		return state, fmt.Errorf("%s: %w", path, err)
	}
	raw, ok := ui[geminiAlternateBufferKey]
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return state, nil
	}
	state.Present = true
	var enabled bool
	if json.Unmarshal(raw, &enabled) == nil {
		state.Valid, state.Enabled = true, enabled
	}
	return state, nil
}

// EnableGeminiAlternateBuffer sets ui.useAlternateBuffer to true in the
// user-level Gemini settings, keeping every other key.
func EnableGeminiAlternateBuffer() error {
	path := geminiSettingsPath()
	if path == "" {
		return errors.New("cannot determine Gemini settings path")
	}
	// Serialize with the hooks installer, which edits the same file: the same
	// in-process mutex, and the same lock file beside the resolved target.
	installGeminiHooksMu.Lock()
	defer installGeminiHooksMu.Unlock()
	target, err := atomicWriteTarget(path)
	if err != nil {
		return err
	}
	return editHarnessConfigFile("Gemini settings", target, 0o600, planGeminiAlternateBuffer, prepareAtomicWriteFile)
}

const geminiAlternateBufferKey = "useAlternateBuffer"

func planGeminiAlternateBuffer(data []byte) (bool, []byte, error) {
	settings, ui, err := parseGeminiUISettings(data)
	if err != nil {
		return false, nil, err
	}
	if bytes.Equal(bytes.TrimSpace(ui[geminiAlternateBufferKey]), []byte("true")) {
		return false, nil, nil
	}
	ui[geminiAlternateBufferKey] = json.RawMessage("true")
	rawUI, err := json.Marshal(ui)
	if err != nil {
		return false, nil, err
	}
	settings["ui"] = rawUI
	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return false, nil, err
	}
	return true, append(out, '\n'), nil
}

// parseGeminiUISettings splits strict-JSON settings into the top-level object
// and its "ui" object, both non-nil.
func parseGeminiUISettings(data []byte) (map[string]json.RawMessage, map[string]json.RawMessage, error) {
	settings := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, &settings); err != nil {
			return nil, nil, fmt.Errorf("not a strict JSON object (tclaude cannot preserve comments or rewrite other shapes): %w", err)
		}
		if settings == nil {
			settings = map[string]json.RawMessage{}
		}
	}
	ui := map[string]json.RawMessage{}
	if raw, ok := settings["ui"]; ok && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		if err := json.Unmarshal(raw, &ui); err != nil {
			return nil, nil, fmt.Errorf(`"ui" is not an object: %w`, err)
		}
		if ui == nil {
			ui = map[string]json.RawMessage{}
		}
	}
	return settings, ui, nil
}
