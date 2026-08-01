package session

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
)

// ResourceDelegationDirEnv carries agentd's external cgroup delegation root
// into the session-new subprocesses that prepare workload cgroups.
const ResourceDelegationDirEnv = "TCLAUDE_RESOURCE_DELEGATION_DIR"

// ExternalResourceDelegationDir returns the configured external delegation
// root, or empty when tclaude should derive the root from its own cgroup.
func ExternalResourceDelegationDir() string {
	return strings.TrimSpace(os.Getenv(ResourceDelegationDirEnv))
}

func externalTmuxRuntimeName() string {
	if dir := ExternalResourceDelegationDir(); dir != "" {
		if name := filepath.Base(filepath.Clean(dir)); name != "." && name != string(filepath.Separator) {
			return name
		}
	}
	return "external tmux runtime unit"
}

// RequireExternalTmuxServer prevents tmux new-session from silently creating
// a server in agentd's cgroup when the server is owned by a separate systemd
// delegation unit. show-options is a server-only command and never starts one.
func RequireExternalTmuxServer() error {
	if ExternalResourceDelegationDir() == "" {
		return nil
	}
	out, err := clcommon.TmuxCommand("show-options", "-gv", "exit-empty").CombinedOutput()
	if err == nil {
		return nil
	}
	detail := strings.TrimSpace(string(out))
	if detail != "" {
		detail = ": " + detail
	}
	return fmt.Errorf("external tmux runtime %s is unavailable; start or restart that unit before starting agentd or creating sessions%s",
		externalTmuxRuntimeName(), detail)
}

// PropagateResourceDelegationToTmux makes the setting available to panes
// created after agentd starts. The explicit cgroup path is also consumed by
// agentd and its session-new subprocesses before pane creation.
func PropagateResourceDelegationToTmux() error {
	dir := ExternalResourceDelegationDir()
	if dir == "" {
		return nil
	}
	if err := RequireExternalTmuxServer(); err != nil {
		return err
	}
	if out, err := clcommon.TmuxCommand(
		"set-environment", "-g", ResourceDelegationDirEnv, dir,
	).CombinedOutput(); err != nil {
		detail := strings.TrimSpace(string(out))
		if detail != "" {
			detail = ": " + detail
		}
		return fmt.Errorf("configure external tmux runtime %s environment%s: %w",
			externalTmuxRuntimeName(), detail, err)
	}
	return nil
}
