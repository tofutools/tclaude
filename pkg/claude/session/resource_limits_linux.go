//go:build linux

package session

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

var resourceCgroupRoot = "/sys/fs/cgroup"

const resourceSupervisorCgroup = "tclaude-supervisor"

func currentCgroupDir() (string, error) {
	raw, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", fmt.Errorf("read current cgroup: %w", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "0::") {
			continue
		}
		rel := strings.TrimPrefix(line, "0::")
		clean := filepath.Clean("/" + rel)
		return filepath.Join(resourceCgroupRoot, strings.TrimPrefix(clean, "/")), nil
	}
	return "", errors.New("cgroup v2 unified hierarchy was not found in /proc/self/cgroup")
}

// resourceDelegationDir returns the process-free delegated node under which
// workload cgroups may be created. systemd's DelegateSubgroup setting places
// agentd and its ordinary children in tclaude-supervisor, leaving the unit
// cgroup itself available as the controller-owning inner node.
func resourceDelegationDir(current string) string {
	if filepath.Base(current) == resourceSupervisorCgroup {
		return filepath.Dir(current)
	}
	return current
}

func wrapResourceLimitedCommand(
	sessionID string,
	limits sandboxpolicy.ResourceLimits,
	command string,
	allowUnenforced bool,
) (string, func(), error) {
	current, err := currentCgroupDir()
	if err != nil {
		return "", func() {}, fmt.Errorf("resource limits unavailable: %w", err)
	}
	delegation := resourceDelegationDir(current)
	controllersRaw, err := os.ReadFile(filepath.Join(delegation, "cgroup.controllers"))
	if err != nil {
		return "", func() {}, fmt.Errorf("resource limits require a delegated cgroup v2 service subtree: %w", err)
	}
	available := strings.Fields(string(controllersRaw))
	needed := []string{}
	if limits.Memory != "" {
		needed = append(needed, "memory")
	}
	if limits.CPU != nil {
		needed = append(needed, "cpu")
	}
	for _, controller := range needed {
		if !containsString(available, controller) {
			return "", func() {}, fmt.Errorf("resource limits require delegated cgroup v2 %s controller; configure tclaude-agentd.service with Delegate=cpu memory and DelegateSubgroup=%s", controller, resourceSupervisorCgroup)
		}
	}
	enabledRaw, err := os.ReadFile(filepath.Join(delegation, "cgroup.subtree_control"))
	if err != nil {
		return "", func() {}, fmt.Errorf("inspect delegated cgroup controllers: %w", err)
	}
	enabled := strings.Fields(string(enabledRaw))
	toEnable := []string{}
	for _, controller := range needed {
		if !containsString(enabled, controller) {
			toEnable = append(toEnable, "+"+controller)
		}
	}
	if len(toEnable) > 0 {
		if err := os.WriteFile(filepath.Join(delegation, "cgroup.subtree_control"), []byte(strings.Join(toEnable, " ")), 0o644); err != nil {
			return "", func() {}, fmt.Errorf("enable delegated cgroup v2 controllers %s: %w (tclaude-agentd.service needs Delegate=cpu memory and DelegateSubgroup=%s)", strings.Join(needed, ", "), err, resourceSupervisorCgroup)
		}
	}
	digest := sha256.Sum256([]byte(sessionID))
	dir := filepath.Join(delegation, fmt.Sprintf("tclaude-%x", digest[:10]))
	if err := os.Mkdir(dir, 0o755); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return "", func() {}, fmt.Errorf("create resource cgroup for session %q: %w", sessionID, err)
		}
		// A deterministic same-session cgroup can survive a daemon crash. Only
		// reclaim that exact empty target; never sweep other launches, whose
		// newly prepared cgroups are also briefly empty before pane attachment.
		if removeErr := os.Remove(dir); removeErr != nil {
			return "", func() {}, fmt.Errorf("resource cgroup for session %q already exists and is active or not reclaimable: %w", sessionID, removeErr)
		}
		if err := os.Mkdir(dir, 0o755); err != nil {
			return "", func() {}, fmt.Errorf("recreate stale resource cgroup for session %q: %w", sessionID, err)
		}
	}
	cleanup := func() { _ = os.Remove(dir) }
	if limits.Memory != "" {
		bytes, parseErr := sandboxpolicy.ParseMemoryLimitBytes(limits.Memory)
		if parseErr != nil {
			cleanup()
			return "", func() {}, parseErr
		}
		if err := os.WriteFile(filepath.Join(dir, "memory.max"), []byte(strconv.FormatUint(bytes, 10)), 0o644); err != nil {
			cleanup()
			return "", func() {}, fmt.Errorf("set memory.max for session %q: %w", sessionID, err)
		}
	}
	if limits.CPU != nil {
		quota, quotaErr := sandboxpolicy.CPUQuotaMicros(*limits.CPU)
		if quotaErr != nil {
			cleanup()
			return "", func() {}, quotaErr
		}
		value := fmt.Sprintf("%d %d", quota, sandboxpolicy.CPUCgroupPeriodMicros)
		if err := os.WriteFile(filepath.Join(dir, "cpu.max"), []byte(value), 0o644); err != nil {
			cleanup()
			return "", func() {}, fmt.Errorf("set cpu.max for session %q: %w", sessionID, err)
		}
	}
	wrapper := clcommon.DetectAbsoluteCmd("session", "resource-limit-exec") +
		" --cgroup-dir " + clcommon.ShellQuoteArg(dir) +
		" --session-id " + clcommon.ShellQuoteArg(sessionID) +
		" --command " + clcommon.ShellQuoteArg(command)
	if allowUnenforced {
		wrapper += " --allow-unenforced"
	}
	return wrapper, cleanup, nil
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func resourceLimitExecCmd() *cobra.Command {
	var cgroupDir, command, sessionID string
	var allowUnenforced bool
	cmd := &cobra.Command{
		Use:    "resource-limit-exec",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runResourceLimitExec(cgroupDir, sessionID, command, allowUnenforced)
		},
	}
	cmd.Flags().StringVar(&cgroupDir, "cgroup-dir", "", "prepared cgroup directory")
	cmd.Flags().StringVar(&sessionID, "session-id", "", "managed session id")
	cmd.Flags().StringVar(&command, "command", "", "shell command")
	cmd.Flags().BoolVar(&allowUnenforced, "allow-unenforced", false, "operator authorized fallback without enforcement")
	return cmd
}

func runResourceLimitExec(cgroupDir, sessionID, command string, allowUnenforced bool) error {
	cgroupDir = filepath.Clean(cgroupDir)
	rel, err := filepath.Rel(resourceCgroupRoot, cgroupDir)
	if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) || !strings.HasPrefix(filepath.Base(cgroupDir), "tclaude-") || filepath.Base(cgroupDir) == resourceSupervisorCgroup {
		return errors.New("resource-limit-exec received an invalid resource cgroup path")
	}
	info, err := os.Lstat(cgroupDir)
	if err != nil {
		return fmt.Errorf("resource-limit-exec cgroup is invalid: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("resource-limit-exec cgroup is not a real directory")
	}
	gateRead, gateWrite, err := os.Pipe()
	if err != nil {
		return err
	}
	defer func() { _ = gateRead.Close(); _ = gateWrite.Close() }()
	child := exec.Command("/bin/sh", "-c",
		`IFS= read -r tclaude_resource_gate <&3 || exit 125; exec /bin/sh -c "$1"`,
		"tclaude-resource-limit", command)
	child.Stdin, child.Stdout, child.Stderr = os.Stdin, os.Stdout, os.Stderr
	child.ExtraFiles = []*os.File{gateRead}
	if err := child.Start(); err != nil {
		return err
	}
	_ = gateRead.Close()
	moveErr := os.WriteFile(filepath.Join(cgroupDir, "cgroup.procs"), []byte(strconv.Itoa(child.Process.Pid)), 0o644)
	if moveErr == nil {
		_, moveErr = io.WriteString(gateWrite, "go\n")
	}
	if moveErr != nil && allowUnenforced {
		noticeErr := fmt.Errorf("attach workload to resource cgroup before release: %w", moveErr)
		if persistErr := recordResourceLimitRuntimeOverrideForExec(sessionID, noticeErr); persistErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: record resource-limit override: %v\n", persistErr)
		}
		fmt.Fprintf(os.Stderr, "Warning: %v; launching without configured resource-limit enforcement by operator approval\n", noticeErr)
		_, moveErr = io.WriteString(gateWrite, "go\n")
	}
	_ = gateWrite.Close()
	if moveErr != nil {
		_ = child.Process.Kill()
		_ = child.Wait()
		return fmt.Errorf("attach workload to resource cgroup before release: %w", moveErr)
	}
	waitErr := child.Wait()
	if err := os.Remove(cgroupDir); err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "Warning: remove empty resource cgroup %s: %v\n", cgroupDir, err)
	}
	if waitErr == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		os.Exit(exitErr.ExitCode())
	}
	return waitErr
}
