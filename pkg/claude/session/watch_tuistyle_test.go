package session

import (
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/assert"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/tuistyle"
)

// applyTUIColorScheme rebuilds every watch-view style from the resolved
// palette. Assert the wiring role-by-role via the profile-independent style
// getters (not rendered ANSI, which the test terminal may strip): switching to
// the high-contrast scheme must move the header / status / accent / danger
// colors to their pre-#738 values, and switching back must restore them. This
// guards against a "forgot to reassign that var" mistake the palette-data test
// can't catch. init() seeds the default; t.Cleanup restores it.
func TestApplyTUIColorScheme_WiresEveryStyle(t *testing.T) {
	t.Cleanup(func() { applyTUIColorScheme(config.TUIColorSchemeDefault) })

	check := func(scheme string) {
		applyTUIColorScheme(scheme)
		p := tuistyle.Resolve(scheme)
		assert.Equal(t, lipgloss.Color(p.Header), headerStyle.GetForeground(), "header")
		assert.Equal(t, lipgloss.Color(p.Idle), idleStyle.GetForeground(), "idle")
		assert.Equal(t, lipgloss.Color(p.Working), workingStyle.GetForeground(), "working")
		assert.Equal(t, lipgloss.Color(p.Danger), needsInput.GetForeground(), "needsInput")
		assert.Equal(t, lipgloss.Color(p.Exited), exitedStyle.GetForeground(), "exited")
		assert.Equal(t, lipgloss.Color(p.Help), helpStyle.GetForeground(), "help")
		assert.Equal(t, lipgloss.Color(p.Accent), searchStyle.GetForeground(), "search")
		assert.Equal(t, lipgloss.Color(p.Danger), confirmStyle.GetForeground(), "confirm")
		assert.Equal(t, lipgloss.Color(p.Accent), menuStyle.GetForeground(), "menu")
		assert.Equal(t, lipgloss.Color(p.Accent), filterBadge.GetForeground(), "filterBadge")
		// The selected row keeps the row's own foreground in both schemes (it
		// never sets one) and always paints the palette's background.
		assert.Equal(t, lipgloss.Color(""), selectedStyle.GetForeground(), "selected fg (none)")
		assert.Equal(t, lipgloss.Color(p.SelectedBg), selectedStyle.GetBackground(), "selected bg")
	}

	check(config.TUIColorSchemeDefault)
	check(config.TUIColorSchemeHighContrast)

	// A concrete before/after so a same-value regression can't pass silently:
	// idle is gold (178) by default, vivid yellow (226) in high contrast.
	applyTUIColorScheme(config.TUIColorSchemeDefault)
	assert.Equal(t, lipgloss.Color("178"), idleStyle.GetForeground())
	applyTUIColorScheme(config.TUIColorSchemeHighContrast)
	assert.Equal(t, lipgloss.Color("226"), idleStyle.GetForeground())
}
