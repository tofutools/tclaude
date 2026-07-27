//go:build !linux && !darwin

package agentd

import (
	"fmt"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

type openCodeStateLayout struct {
	allocation db.OpenCodeAgentStateAllocation
	parent     string
	stateDirs  []string
	ambient    struct {
		data, cache, config, state, install string
	}
	environment   []sandboxpolicy.EnvironmentEntry
	finalHideDirs []string
	readOnlyBinds []session.TclaudeLayerReadOnlyBind
}

func allocatePrivateOpenCodeState(string) (*db.OpenCodeAgentStateAllocation, error) {
	return nil, fmt.Errorf("per-agent OpenCode state requires Linux or macOS")
}

func requireOpenCodeStateAllocation(string) (*db.OpenCodeAgentStateAllocation, error) {
	return nil, fmt.Errorf("per-agent OpenCode state requires Linux or macOS")
}

func openCodeStateLayoutForAllocation(
	db.OpenCodeAgentStateAllocation,
) (*openCodeStateLayout, error) {
	return nil, fmt.Errorf("per-agent OpenCode state requires Linux or macOS")
}
