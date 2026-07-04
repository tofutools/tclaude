package conv

import (
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/assert"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

// The conv watch view's selected row is the one non-mechanical bit of the
// scheme wiring: the default scheme paints a white foreground (255) over the
// dark-gray background, while the high-contrast scheme leaves the row's own
// color (matching pre-#738, where wSelectedStyle set no foreground). Assert
// both branches of that conditional. init() seeds the default; t.Cleanup
// restores it.
func TestApplyTUIColorScheme_SelectedRowInherit(t *testing.T) {
	t.Cleanup(func() { applyTUIColorScheme(config.TUIColorSchemeDefault) })

	applyTUIColorScheme(config.TUIColorSchemeDefault)
	assert.Equal(t, lipgloss.Color("255"), wSelectedStyle.GetForeground(), "default paints white fg")
	assert.Equal(t, lipgloss.Color("238"), wSelectedStyle.GetBackground())

	applyTUIColorScheme(config.TUIColorSchemeHighContrast)
	assert.Equal(t, lipgloss.Color(""), wSelectedStyle.GetForeground(), "high-contrast inherits the row's own color")
	assert.Equal(t, lipgloss.Color("238"), wSelectedStyle.GetBackground())

	// The rest of the roles still move with the scheme.
	assert.Equal(t, lipgloss.Color("250"), wHeaderStyle.GetForeground())
	assert.Equal(t, lipgloss.Color("214"), wSearchStyle.GetForeground())
	assert.Equal(t, lipgloss.Color("196"), wConfirmStyle.GetForeground())
}
