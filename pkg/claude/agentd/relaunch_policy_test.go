package agentd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/launchspec"
)

// The harness_config relaunch row (operator decision Q11): which posture a
// relaunch keeps for each mode, explicit posture and chain posture. "" keeps
// the chain's.
func TestRelaunchHarnessConfigModes(t *testing.T) {
	const (
		floor = sandboxpolicy.HarnessConfigAccessDefault
		read  = sandboxpolicy.HarnessConfigAccessRead
		write = sandboxpolicy.HarnessConfigAccessWrite
	)
	cases := []struct {
		mode     launchspec.RelaunchMode
		chain    sandboxpolicy.HarnessConfigAccess
		explicit string
		want     string
	}{
		{launchspec.ModeExplicitWins, write, "read", "read"},
		{launchspec.ModeExplicitWins, floor, "write", "write"},
		{launchspec.ModeExplicitWins, read, "write", "write"},
		// Tighten-only differs only for an explicit write over a floored chain.
		{launchspec.ModeTightenOnly, floor, "write", ""},
		{launchspec.ModeTightenOnly, read, "write", ""},
		{launchspec.ModeTightenOnly, write, "write", "write"},
		{launchspec.ModeTightenOnly, write, "read", "read"},
		{launchspec.ModeTightenOnly, floor, "read", "read"},
		{launchspec.ModeChainWins, write, "read", ""},
		{launchspec.ModeChainWins, floor, "write", ""},
		{launchspec.ModeExplicitWins, write, "", ""},
		// An unset mode is the default, explicit-wins.
		{launchspec.RelaunchPolicy{}.Mode(harnessConfigField), floor, "write", "write"},
	}
	for _, c := range cases {
		assert.Equalf(t, c.want, relaunchHarnessConfig(c.mode, c.chain, c.explicit),
			"mode %s, chain %q, explicit %q", c.mode, c.chain, c.explicit)
	}
}
