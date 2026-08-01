package session

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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

// ExternalTmuxNoStartArgs adds tmux's client-level "never start the server"
// flag in external mode. This closes the race between a server probe and the
// command that follows it: even if the external server exits in between,
// new-session fails instead of creating a server in the caller's cgroup.
func ExternalTmuxNoStartArgs(args ...string) []string {
	if ExternalResourceDelegationDir() == "" {
		return args
	}
	return append([]string{"-N"}, args...)
}

// ExternalTmuxNoStartFlag is the shell-rendering counterpart used by the
// dashboard's PTY command builder.
func ExternalTmuxNoStartFlag() string {
	if ExternalResourceDelegationDir() != "" {
		return "-N"
	}
	return ""
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
	dir := ExternalResourceDelegationDir()
	if dir == "" {
		return nil
	}
	out, err := clcommon.TmuxCommand("-N", "display-message", "-p", "#{pid}").CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(out))
		if detail != "" {
			detail = ": " + detail
		}
		return fmt.Errorf("external tmux runtime %s is unavailable; start or restart that unit before starting agentd or creating sessions%s",
			externalTmuxRuntimeName(), detail)
	}
	pid, parseErr := strconv.Atoi(strings.TrimSpace(string(out)))
	if parseErr != nil || pid <= 0 {
		return fmt.Errorf("external tmux runtime %s returned an invalid server PID %q",
			externalTmuxRuntimeName(), strings.TrimSpace(string(out)))
	}
	if err := ValidateExternalTmuxServerCgroup(pid, dir); err != nil {
		return fmt.Errorf("external tmux runtime %s is not running in the configured delegation: %w",
			externalTmuxRuntimeName(), err)
	}
	return nil
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
		"-N", "set-environment", "-g", ResourceDelegationDirEnv, dir,
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

// ClearResourceDelegationFromTmux removes a stale external root when agentd
// returns to legacy mode. A missing server or missing variable is already the
// desired state and does not make ordinary agentd startup depend on tmux.
func ClearResourceDelegationFromTmux() error {
	out, err := clcommon.TmuxCommand(
		"-N", "show-environment", "-g", ResourceDelegationDirEnv,
	).CombinedOutput()
	if err != nil || !strings.HasPrefix(strings.TrimSpace(string(out)), ResourceDelegationDirEnv+"=") {
		return nil
	}
	out, err = clcommon.TmuxCommand(
		"-N", "set-environment", "-gu", ResourceDelegationDirEnv,
	).CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(out))
		if detail != "" {
			detail = ": " + detail
		}
		return fmt.Errorf("clear stale tmux resource delegation environment%s: %w", detail, err)
	}
	return nil
}
