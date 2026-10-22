//go:build !linux

package session

import (
	"fmt"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

func tclaudeLayerCommandWithRouteHelper(
	string, []string, []TclaudeLayerPrivateWriteDir, []string,
	[]TclaudeLayerReadOnlyBind, []string, sandboxpolicy.MountPlan,
	TclaudeLayerRouteHelper, string,
) (string, error) {
	return "", fmt.Errorf("linux group-route helper is unavailable")
}
