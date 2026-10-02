package setup

import (
	"fmt"

	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// configureGeminiAlternateBuffer offers Gemini CLI's alternate-screen
// renderer, the counterpart of Claude Code's fullscreen TUI: with it, the
// mouse wheel scrolls Gemini's own history instead of arriving as Up/Down
// keys that page through prompt history. An existing value is a deliberate
// choice and is left as-is.
func configureGeminiAlternateBuffer(params *Params) {
	state, err := harness.ReadGeminiAlternateBuffer()
	switch {
	case err != nil:
		fmt.Printf("  ⚠ Could not read Gemini settings (%v) — leaving them untouched\n", err)
	case state.Present && state.Valid && state.Enabled:
		fmt.Println("✓ Gemini alternate-screen renderer already enabled")
	case state.Present && state.Valid:
		fmt.Printf("✓ Gemini ui.useAlternateBuffer is disabled in %s — leaving it as-is\n", state.Source)
		fmt.Println("  (without it, the mouse wheel in a tclaude pane pages through Gemini's prompt history)")
	case state.Present:
		fmt.Printf("  Gemini ui.useAlternateBuffer has a non-boolean value in %s — leaving it as-is\n", state.Source)
	default:
		fmt.Println("  tclaude runs Gemini CLI in tmux. Gemini's alternate-screen renderer handles the mouse")
		fmt.Println("  itself, so the wheel scrolls the conversation instead of paging through prompt history.")
		if !askYesNo("Enable Gemini CLI's alternate-screen renderer?", true, params.Yes) {
			fmt.Println("  Skipped. Enable later with: tclaude setup")
			return
		}
		if err := harness.EnableGeminiAlternateBuffer(); err != nil {
			fmt.Printf("  Warning: failed to enable Gemini's alternate-screen renderer: %v\n", err)
			return
		}
		fmt.Println("✓ Gemini alternate-screen renderer enabled (\"ui\": {\"useAlternateBuffer\": true})")
	}
}

func checkGeminiAlternateBuffer() {
	state, err := harness.ReadGeminiAlternateBuffer()
	switch {
	case err != nil:
		fmt.Printf("⚠ Could not read Gemini settings: %v\n", err)
	case state.Present && state.Valid && state.Enabled:
		fmt.Println("✓ Gemini alternate-screen renderer enabled")
	case state.Present && state.Valid:
		fmt.Printf("  Gemini ui.useAlternateBuffer disabled in %s\n", state.Source)
	case state.Present:
		fmt.Printf("⚠ Gemini ui.useAlternateBuffer has a non-boolean value in %s\n", state.Source)
	default:
		fmt.Println("✗ Gemini alternate-screen renderer not enabled")
		fmt.Println("  Run 'tclaude setup' to enable it (the mouse wheel then scrolls Gemini's history)")
	}
}
