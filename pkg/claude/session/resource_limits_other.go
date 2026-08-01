//go:build !linux

package session

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

func wrapResourceLimitedCommand(string, sandboxpolicy.ResourceLimits, string, bool) (string, func(), error) {
	return "", func() {}, fmt.Errorf("resource limits are Linux only")
}

func resourceLimitExecCmd() *cobra.Command {
	return &cobra.Command{
		Use: "resource-limit-exec", Hidden: true,
		RunE: func(*cobra.Command, []string) error { return fmt.Errorf("resource limits are Linux only") },
	}
}
