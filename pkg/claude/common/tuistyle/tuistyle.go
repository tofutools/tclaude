// Package tuistyle holds the color palettes for tclaude's interactive
// bubbletea "watch" TUIs — `session ls -w`, `conv ls -w`, and `agent inbox
// -w`. It is the single source of truth for the scheme-dependent ANSI-256
// colors those three views share, so switching color schemes (config
// tui.color_scheme) can't leave them out of step.
//
// It is deliberately pure data — plain lipgloss.Color code strings, no
// lipgloss dependency — so each view builds its own lipgloss.Style values from
// a resolved Palette, keeping view-specific composition (bold, background)
// local to the view.
//
// The two schemes reproduce the palettes on either side of PR #738 ("Fix
// terminal contrast"): the default is the current, light-and-dark-friendly
// palette; the high-contrast scheme is the brighter pre-#738 palette some dark
// terminals preferred.
package tuistyle

import "github.com/tofutools/tclaude/pkg/claude/common/config"

// Palette is the set of ANSI-256 color codes (lipgloss.Color strings) the
// watch views draw with. Only the roles PR #738 changed vary between schemes;
// the scheme-invariant roles (Help, Exited, SelectedBg, Info) carry the same
// value in every palette so a view can source ALL of its colors from here.
type Palette struct {
	Header     string // bold table header
	Help       string // dim help / hint line
	Exited     string // exited / gray row
	SelectedBg string // selected-row background
	// SelectedFg is the selected row's foreground. Empty means "leave the
	// row's own color" — the view skips the .Foreground() call, matching the
	// pre-#738 selected style that preserved each row's status color.
	SelectedFg string
	Accent     string // search prompt / menu / filter badge (orange)
	Danger     string // confirm / error / needs-input (red)
	Idle       string // session idle (yellow / gold)
	Working    string // session working (green)
	Info       string // semantic / reading highlight (cyan)
}

// defaultPalette is the current (PR #738) scheme: tuned to stay readable on
// both light and dark terminals — a little dimmer on a dark background.
var defaultPalette = Palette{
	Header:     "244",
	Help:       "241",
	Exited:     "240",
	SelectedBg: "238",
	SelectedFg: "255",
	Accent:     "166",
	Danger:     "160",
	Idle:       "178",
	Working:    "34",
	Info:       "39",
}

// highContrastPalette is the brighter pre-#738 scheme — higher contrast on a
// dark terminal (vivid yellow / green / red, brighter header), at the cost of
// light-terminal readability. It reproduces the exact colors PR #738 replaced,
// including the selected row inheriting its own status color (SelectedFg "").
var highContrastPalette = Palette{
	Header:     "250",
	Help:       "241",
	Exited:     "240",
	SelectedBg: "238",
	SelectedFg: "", // inherit the row's own color, as before #738
	Accent:     "214",
	Danger:     "196",
	Idle:       "226",
	Working:    "46",
	Info:       "39",
}

// Resolve returns the Palette for a color-scheme id (config tui.color_scheme).
// An unknown / empty scheme falls back to the default palette, so a
// hand-edited garbage value can never leave a view unstyled. Callers normally
// pass config's already-normalized (*Config).TUIColorScheme(), but Resolve is
// defensive either way.
func Resolve(scheme string) Palette {
	switch scheme {
	case config.TUIColorSchemeHighContrast:
		return highContrastPalette
	default:
		return defaultPalette
	}
}
