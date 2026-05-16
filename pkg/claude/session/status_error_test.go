package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The interactive `session` watch TUI exposes an "attention" grouped
// filter. It must include errored agents — a turn that ended in an
// error needs attention just as much as one awaiting permission or
// input. Pins the bug where the watch TUI's attention filter omitted
// StatusError while the CLI `--show attention` path
// (normalizeStatusFilter) already included it.
func TestModelMatchesShowFilter_AttentionIncludesError(t *testing.T) {
	m := model{statusFilter: []string{"attention"}}

	// The three needs-attention statuses all match "attention".
	assert.True(t, m.matchesShowFilter(StatusAwaitingPermission),
		"attention must show awaiting_permission")
	assert.True(t, m.matchesShowFilter(StatusAwaitingInput),
		"attention must show awaiting_input")
	assert.True(t, m.matchesShowFilter(StatusError),
		"attention must show errored agents")

	// Calm statuses do not match "attention".
	assert.False(t, m.matchesShowFilter(StatusWorking),
		"attention must not show working")
	assert.False(t, m.matchesShowFilter(StatusIdle),
		"attention must not show idle")
}

// Symmetric to the show filter: hiding "attention" must also hide
// errored agents.
func TestModelMatchesHideFilter_AttentionIncludesError(t *testing.T) {
	m := model{hideFilter: []string{"attention"}}

	assert.True(t, m.matchesHideFilter(StatusAwaitingPermission),
		"hiding attention must hide awaiting_permission")
	assert.True(t, m.matchesHideFilter(StatusAwaitingInput),
		"hiding attention must hide awaiting_input")
	assert.True(t, m.matchesHideFilter(StatusError),
		"hiding attention must hide errored agents")

	assert.False(t, m.matchesHideFilter(StatusWorking),
		"hiding attention must not hide working")
}

// A direct StatusError filter (not via the "attention" group) matches
// through the plain f == status path.
func TestModelMatchesShowFilter_DirectError(t *testing.T) {
	m := model{statusFilter: []string{StatusError}}
	assert.True(t, m.matchesShowFilter(StatusError),
		"an explicit error filter must show errored agents")
	assert.False(t, m.matchesShowFilter(StatusWorking),
		"an explicit error filter must show nothing else")
}

// StatusError must be treated as a needs-attention state — the same
// sort priority, row style and color as the awaiting_* statuses —
// across every status switch the CLI rendering routes through. Pins the
// three switches the error-status feature touched against a silent
// fall-through to a default arm.
func TestStatusError_RendersAsNeedsAttention(t *testing.T) {
	// Sort priority: 0 == "needs attention, show first".
	assert.Equal(t, 0, statusPriority(StatusError),
		"errored agents must sort into the needs-attention bucket")
	assert.Equal(t, statusPriority(StatusAwaitingPermission), statusPriority(StatusError),
		"error must share the awaiting_* sort priority")

	// Row style (watch TUI): same lipgloss style as awaiting_*.
	assert.Equal(t, getRowStyle(StatusAwaitingPermission), getRowStyle(StatusError),
		"error must use the needs-attention row style")

	// Color (session ls): the funcs can't be compared directly, so
	// compare their rendered output instead.
	const sample = "x"
	assert.Equal(t,
		getStatusColorFunc(StatusAwaitingPermission)(sample),
		getStatusColorFunc(StatusError)(sample),
		"error must render in the needs-attention color")
}
