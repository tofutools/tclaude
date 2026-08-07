//go:build !linux

package session

import (
	"fmt"
	"os/exec"

	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

func wrapResourceLimitedCommand(string, sandboxpolicy.ResourceLimits, string, bool) (string, func(), error) {
	return "", func() {}, fmt.Errorf("resource limits are Linux only")
}

func PrepareResourceCgroup(string, sandboxpolicy.ResourceLimits) (string, func(), error) {
	return "", func() {}, fmt.Errorf("resource limits are Linux only")
}

func wrapPreparedResourceCgroupCommand(string, string, string, bool) string { return "" }

func WrapPreparedResourceCgroupCommand(string, string, string, bool) string { return "" }

func ConfigureProcessResourceCgroup(*exec.Cmd, string) (func(), error) {
	return func() {}, fmt.Errorf("resource limits are Linux only")
}

func ValidatePreparedResourceCgroup(string, sandboxpolicy.ResourceLimits) error {
	return fmt.Errorf("resource limits are Linux only")
}

func ValidateResourceDelegationDir(string) (string, error) {
	return "", fmt.Errorf("external resource delegation is Linux only")
}

func ValidateExternalTmuxServerCgroup(int, string) error {
	return fmt.Errorf("external resource delegation is Linux only")
}

func ReadResourceCgroupOOMKills(string) ResourceCgroupOOMCount { return ResourceCgroupOOMCount{} }

func ResourceCgroupOOMDeath(string, ResourceCgroupOOMCount, error) bool { return false }

func resourceLimitExecCmd() *cobra.Command {
	return &cobra.Command{
		Use: "resource-limit-exec", Hidden: true,
		RunE: func(*cobra.Command, []string) error { return fmt.Errorf("resource limits are Linux only") },
	}
}
