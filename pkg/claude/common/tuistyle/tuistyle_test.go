package tuistyle

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

// Resolve returns the default palette for the default scheme, an empty scheme,
// and any unknown value — only the explicit high-contrast id switches palettes.
// The unknown-value fallback matters: a hand-edited garbage scheme must never
// leave a view unstyled.
func TestResolve(t *testing.T) {
	assert.Equal(t, defaultPalette, Resolve(config.TUIColorSchemeDefault))
	assert.Equal(t, defaultPalette, Resolve(""))
	assert.Equal(t, defaultPalette, Resolve("neon"))
	assert.Equal(t, highContrastPalette, Resolve(config.TUIColorSchemeHighContrast))
}

// The two palettes reproduce the exact ANSI-256 colors on either side of PR
// #738 ("Fix terminal contrast"): the default is the post-#738 palette, the
// high-contrast scheme is the pre-#738 one. Pin every code so a well-meaning
// tweak to one value is a conscious, reviewed change rather than an accident —
// the whole feature is "give people back the exact old colors".
func TestDefaultPalette_MatchesPost738(t *testing.T) {
	assert.Equal(t, Palette{
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
	}, defaultPalette)
}

func TestHighContrastPalette_MatchesPre738(t *testing.T) {
	assert.Equal(t, Palette{
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
	}, highContrastPalette)
}

// The scheme-invariant roles carry the SAME value in both palettes, so a view
// can source all of its colors from a single resolved palette regardless of
// scheme.
func TestSchemeInvariantColorsMatch(t *testing.T) {
	assert.Equal(t, defaultPalette.Help, highContrastPalette.Help)
	assert.Equal(t, defaultPalette.Exited, highContrastPalette.Exited)
	assert.Equal(t, defaultPalette.SelectedBg, highContrastPalette.SelectedBg)
	assert.Equal(t, defaultPalette.Info, highContrastPalette.Info)
}
