package session

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/tofutools/tclaude/pkg/claude/common/grouplabel"
)

// A session's window title names its agent as its terminal tab does, and
// keeps the "tclaude:<id>" window focus finds it by.
func TestSessionWindowTitle_NamesTheAgentByGroup(t *testing.T) {
	labels := map[string]grouplabel.Label{
		"s-lead":  {Prefix: "t", Name: "lead"},
		"s-loner": {Name: "loner"},
		"s-evil":  {Prefix: "w", Name: "#(touch /tmp/x)#[fg=red]"},
	}
	prev := sessionGroupLabelFn
	sessionGroupLabelFn = func(id string) (grouplabel.Label, bool) { l, ok := labels[id]; return l, ok }
	t.Cleanup(func() { sessionGroupLabelFn = prev })

	assert.Equal(t, "t/lead — tclaude:s-lead", sessionWindowTitle("s-lead"))
	assert.Equal(t, "loner — tclaude:s-loner", sessionWindowTitle("s-loner"))
	assert.Equal(t, "tclaude:s-shell", sessionWindowTitle("s-shell"), "no named agent: the bare id")
	assert.Equal(t, "w/(touch /tmp/x)[fg=red] — tclaude:s-evil", sessionWindowTitle("s-evil"),
		"no '#' reaches tmux's format string")
}
