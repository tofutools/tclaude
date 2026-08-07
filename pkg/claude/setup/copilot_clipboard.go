package setup

import (
	"fmt"

	"github.com/tofutools/tclaude/pkg/claude/harness"
)

func configureCopilotClipboard(params *Params) {
	state, err := harness.AmbientCopilotCopyOnSelectState()
	switch {
	case err != nil:
		fmt.Printf("  ⚠ Could not read Copilot settings (%v) — leaving them untouched\n", err)
	case state.Present && state.Valid && state.Enabled:
		fmt.Println("✓ Copilot copy-on-select already enabled")
	case state.Present && state.Valid:
		fmt.Printf("✓ Copilot copyOnSelect is disabled in %s — leaving it as-is\n", state.Source)
	case state.Present:
		fmt.Printf("  Copilot copyOnSelect has a non-boolean value in %s — leaving it as-is\n", state.Source)
	default:
		fmt.Println("  Copilot owns mouse selection in its fullscreen terminal UI. Enabling copy-on-select")
		fmt.Println("  lets the web terminal bridge that explicit mouse gesture to the browser clipboard.")
		if !askYesNo("Enable Copilot CLI copy-on-select?", true, params.Yes) {
			fmt.Println("  Skipped. Enable later with: tclaude setup")
			return
		}
		if err := harness.EnableAmbientCopilotCopyOnSelect(); err != nil {
			fmt.Printf("  Warning: failed to enable Copilot copy-on-select: %v\n", err)
			return
		}
		fmt.Println("✓ Copilot copy-on-select enabled (\"copyOnSelect\": true)")
	}
}

func checkCopilotClipboard() {
	state, err := harness.AmbientCopilotCopyOnSelectState()
	switch {
	case err != nil:
		fmt.Printf("⚠ Could not read Copilot settings: %v\n", err)
	case state.Present && state.Valid && state.Enabled:
		fmt.Println("✓ Copilot copy-on-select enabled")
	case state.Present && state.Valid:
		fmt.Printf("  Copilot copyOnSelect disabled in %s\n", state.Source)
	case state.Present:
		fmt.Printf("⚠ Copilot copyOnSelect has a non-boolean value in %s\n", state.Source)
	default:
		fmt.Println("✗ Copilot copy-on-select not enabled")
		fmt.Println("  Run 'tclaude setup' to enable it")
	}
}
