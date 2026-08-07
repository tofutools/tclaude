package session

import (
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
)

// corpseTmux models the state agent panes actually leave behind. Panes run with
// remain-on-exit ON so the exit audit can read pane_dead_status, so an exited
// agent stays LISTED (has-session exits 0) with a DEAD pane (#{pane_dead} = 1).
// kill-session removes it, which is the only way the name comes free again.
type corpseTmux struct {
	mu sync.Mutex
	// live and dead are session names; a dead one is a remain-on-exit corpse.
	live map[string]bool
	dead map[string]bool
	// kills records every kill-session target, so a test can prove the reap is
	// aimed at the corpse and never at a live agent.
	kills []string
}

func (c *corpseTmux) Command(args ...string) *exec.Cmd {
	c.mu.Lock()
	defer c.mu.Unlock()
	// The target follows -t, which is args[2] for has-session/kill-session and
	// args[3] for `display-message -p -t <target> <format>`.
	name := ""
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "-t" {
			name = strings.TrimSuffix(strings.TrimPrefix(args[i+1], "="), ":0.0")
			break
		}
	}
	switch {
	case len(args) >= 3 && args[0] == "has-session":
		if c.live[name] || c.dead[name] {
			return exec.Command("true")
		}
		return exec.Command("false")
	case len(args) >= 3 && args[0] == "kill-session":
		c.kills = append(c.kills, name)
		delete(c.live, name)
		delete(c.dead, name)
		return exec.Command("true")
	case len(args) >= 2 && args[0] == "display-message":
		// IsTmuxSessionAlive's pane_dead probe, target "=<name>:0.0".
		if c.dead[name] {
			return exec.Command("echo", "1")
		}
		return exec.Command("echo", "0")
	}
	return exec.Command("false")
}

func (c *corpseTmux) ListSessions() (map[string]struct{}, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]struct{}{}
	for name := range c.live {
		out[name] = struct{}{}
	}
	return out, nil
}

// Scenario: a resume whose previous generation left a remain-on-exit corpse
// must still get its canonical name.
//
// The regression this pins is operator-reported and looked intermittent:
//
//	Error: failed to create tmux session: exit status 1: duplicate session: 019fde64
//
// The name was chosen by asking "is an agent alive there?", which a dead pane
// correctly answers NO — while tmux, which only cares that the name is listed,
// then refused to create it. The resume died after the daemon had already
// launched the spawn, and a later attempt succeeded once something else reaped
// the corpse.
func TestUniqueTmuxSessionName_ReapsARemainOnExitCorpseHoldingTheName(t *testing.T) {
	fake := &corpseTmux{
		live: map[string]bool{},
		dead: map[string]bool{"019fde64": true},
	}
	prev := clcommon.Default
	clcommon.Default = fake
	t.Cleanup(func() { clcommon.Default = prev })

	assert.Equal(t, "019fde64", UniqueTmuxSessionName("019fde64"),
		"the canonical name must come back free; suffixing here would drift the agent onto -2, -3, ... for good")
	assert.Equal(t, []string{"019fde64"}, fake.kills, "the corpse, and only the corpse, is reaped")
	assert.False(t, fake.dead["019fde64"], "the name is actually free afterwards")
}

// The reap is strictly for corpses: a name held by a LIVE agent falls through
// to the -N suffix and is never killed. Getting this wrong would take a session
// out from under a running agent, which is far worse than the bug being fixed.
func TestUniqueTmuxSessionName_NeverReapsALiveSession(t *testing.T) {
	fake := &corpseTmux{
		live: map[string]bool{"019fde64": true},
		dead: map[string]bool{},
	}
	prev := clcommon.Default
	clcommon.Default = fake
	t.Cleanup(func() { clcommon.Default = prev })

	assert.Equal(t, "019fde64-2", UniqueTmuxSessionName("019fde64"),
		"a live holder must be worked around, not killed")
	assert.Empty(t, fake.kills, "a live session must never be reaped")
	assert.True(t, fake.live["019fde64"], "the running agent keeps its session")
}

// Mixed: the base is a corpse and the first suffix is LIVE. The base is reaped
// and reused, so the suffix ladder is not even reached.
func TestUniqueTmuxSessionName_PrefersTheReapedBaseOverAFreeSuffix(t *testing.T) {
	fake := &corpseTmux{
		live: map[string]bool{"019fde64-2": true},
		dead: map[string]bool{"019fde64": true},
	}
	prev := clcommon.Default
	clcommon.Default = fake
	t.Cleanup(func() { clcommon.Default = prev })

	assert.Equal(t, "019fde64", UniqueTmuxSessionName("019fde64"))
	assert.Equal(t, []string{"019fde64"}, fake.kills)
	assert.True(t, fake.live["019fde64-2"], "the unrelated live session is untouched")
}
