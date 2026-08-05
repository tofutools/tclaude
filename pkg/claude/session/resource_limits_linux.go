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
	"syscall"

	"github.com/spf13/cobra"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

var resourceCgroupRoot = "/sys/fs/cgroup"
var resourceProcRoot = "/proc"

const resourceSupervisorCgroup = "tclaude-supervisor"

func currentCgroupDir() (string, error) {
	return processCgroupDir("self")
}

func processCgroupDir(pid string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(resourceProcRoot, pid, "cgroup"))
	if err != nil {
		return "", fmt.Errorf("read process %s cgroup: %w", pid, err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "0::") {
			continue
		}
		rel := strings.TrimPrefix(line, "0::")
		clean := filepath.Clean("/" + rel)
		return filepath.Join(resourceCgroupRoot, strings.TrimPrefix(clean, "/")), nil
	}
	return "", fmt.Errorf("cgroup v2 unified hierarchy was not found for process %s", pid)
}

// ValidateExternalTmuxServerCgroup proves that the server answering the named
// socket was started by the configured external runtime rather than left over
// from an earlier agentd-owned launch.
func ValidateExternalTmuxServerCgroup(pid int, delegation string) error {
	current, err := processCgroupDir(strconv.Itoa(pid))
	if err != nil {
		return err
	}
	delegation = filepath.Clean(delegation)
	rel, err := filepath.Rel(delegation, current)
	if err != nil || filepath.IsAbs(rel) || rel == ".." ||
		strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("tmux server PID %d is in %s, outside %s", pid, current, delegation)
	}
	return nil
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

// ValidateResourceDelegationDir checks an explicit external cgroup root at
// agentd startup, before any launch can depend on it. The external runtime is
// expected to delegate both supported axes even when current profiles use only
// one of them.
func ValidateResourceDelegationDir(dir string) (string, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return "", nil
	}
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("resource delegation directory %q must be absolute", dir)
	}
	dir = filepath.Clean(dir)
	rel, err := filepath.Rel(resourceCgroupRoot, dir)
	if err != nil || rel == "." || filepath.IsAbs(rel) || rel == ".." ||
		strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("resource delegation directory %q must be below %s", dir, resourceCgroupRoot)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return "", fmt.Errorf("inspect resource delegation directory %q: %w", dir, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("resource delegation directory %q is not a real directory", dir)
	}
	controllersRaw, err := os.ReadFile(filepath.Join(dir, "cgroup.controllers"))
	if err != nil {
		return "", fmt.Errorf("read resource delegation controllers in %q: %w", dir, err)
	}
	available := strings.Fields(string(controllersRaw))
	for _, controller := range []string{"cpu", "memory"} {
		if !containsString(available, controller) {
			return "", fmt.Errorf("resource delegation directory %q does not provide the cgroup v2 %s controller; configure its external runtime unit with Delegate=cpu memory, or leave --resource-delegation-dir unset and configure tclaude-agentd.service with Delegate=cpu memory and DelegateSubgroup=%s", dir, controller, resourceSupervisorCgroup)
		}
	}
	return dir, nil
}

func configuredResourceDelegationDir() string {
	return ExternalResourceDelegationDir()
}

// delegationWriteRefused reports the failures that mean the delegated node
// itself rejected the write rather than the requested limit being wrong: a
// missing delegation, or a hierarchy the sandbox mounted read-only.
func delegationWriteRefused(err error) bool {
	return errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.EROFS)
}

// resourceDelegationDeniedHint explains a refused write in the delegated node.
// The two shapes it takes have different fixes. A node that resolved to the
// root of the mounted hierarchy usually means the derivation ran inside a
// cgroup namespace that has no delegated node to find, and no other path under
// that mount would serve either. A real subtree merely has to be delegated to
// the launching user.
func resourceDelegationDeniedHint(delegation string) string {
	root := filepath.Clean(resourceCgroupRoot)
	if filepath.Clean(delegation) != root {
		return fmt.Sprintf("%s is not writable by uid %d; systemd's Delegate= is what chowns a delegated subtree to the service user, and under a cgroup namespace the kernel's nsdelegate additionally refuses writes outside the namespace root. Configure the unit that owns that node with Delegate=cpu memory (agentd's own unit also needs DelegateSubgroup=%s, which keeps the delegated node process-free), or point --resource-delegation-dir at a delegated root that this user can write",
			delegation, os.Geteuid(), resourceSupervisorCgroup)
	}
	return fmt.Sprintf("tclaude derived the delegated parent from /proc/self/cgroup and got %s, the root of the mounted hierarchy, which uid %d cannot write — what an agentd inside a container or an unshared cgroup namespace sees when the host hierarchy is mounted there. The kernel's nsdelegate then refuses writes to every cgroup outside the namespace root, so no other path under that mount would serve either. Run agentd outside that namespace as a systemd unit with Delegate=cpu memory, DelegateSubgroup=%s, and OOMPolicy=continue, or give the namespace a delegated, writable cgroup root",
		root, os.Geteuid(), resourceSupervisorCgroup)
}

func wrapResourceLimitedCommand(
	sessionID string,
	limits sandboxpolicy.ResourceLimits,
	command string,
	allowUnenforced bool,
) (string, func(), error) {
	dir, cleanup, err := PrepareResourceCgroup(sessionID, limits)
	if err != nil {
		return "", func() {}, err
	}
	return wrapPreparedResourceCgroupCommand(sessionID, dir, command, allowUnenforced), cleanup, nil
}

// PrepareResourceCgroup creates and configures the shared workload boundary.
// Agentd uses this before starting an authoritative OpenCode server; ordinary
// pane-owned harnesses call it through wrapResourceLimitedCommand.
func PrepareResourceCgroup(sessionID string, limits sandboxpolicy.ResourceLimits) (string, func(), error) {
	delegation := configuredResourceDelegationDir()
	if delegation != "" {
		validated, err := ValidateResourceDelegationDir(delegation)
		if err != nil {
			return "", func() {}, err
		}
		delegation = validated
	} else {
		current, err := currentCgroupDir()
		if err != nil {
			return "", func() {}, fmt.Errorf("resource limits unavailable: %w", err)
		}
		delegation = resourceDelegationDir(current)
	}
	controllersRaw, err := os.ReadFile(filepath.Join(delegation, "cgroup.controllers"))
	if err != nil {
		return "", func() {}, fmt.Errorf("resource limits require a delegated cgroup v2 service subtree (set --resource-delegation-dir to an external delegated root, or configure tclaude-agentd.service with Delegate=cpu memory and DelegateSubgroup=%s): %w", resourceSupervisorCgroup, err)
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
			return "", func() {}, fmt.Errorf("resource limits require delegated cgroup v2 %s controller; configure the external --resource-delegation-dir runtime with Delegate=cpu memory, or configure tclaude-agentd.service with Delegate=cpu memory and DelegateSubgroup=%s", controller, resourceSupervisorCgroup)
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
			if delegationWriteRefused(err) {
				return "", func() {}, fmt.Errorf("enable delegated cgroup v2 controllers %s: %w (%s)", strings.Join(needed, ", "), err, resourceDelegationDeniedHint(delegation))
			}
			return "", func() {}, fmt.Errorf("enable delegated cgroup v2 controllers %s: %w (the external --resource-delegation-dir runtime needs Delegate=cpu memory, or tclaude-agentd.service needs Delegate=cpu memory and DelegateSubgroup=%s; a delegated node that still holds processes cannot enable controllers at all, which is what DelegateSubgroup avoids)", strings.Join(needed, ", "), err, resourceSupervisorCgroup)
		}
	}
	digest := sha256.Sum256([]byte(sessionID))
	dir := filepath.Join(delegation, fmt.Sprintf("tclaude-%x", digest[:10]))
	if err := os.Mkdir(dir, 0o755); err != nil {
		if delegationWriteRefused(err) {
			return "", func() {}, fmt.Errorf("create resource cgroup for session %q: %w (%s)", sessionID, err, resourceDelegationDeniedHint(delegation))
		}
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
	return dir, cleanup, nil
}

func wrapPreparedResourceCgroupCommand(sessionID, dir, command string, allowUnenforced bool) string {
	wrapper := clcommon.DetectAbsoluteCmd("session", "resource-limit-exec") +
		" --cgroup-dir " + clcommon.ShellQuoteArg(dir) +
		" --session-id " + clcommon.ShellQuoteArg(sessionID) +
		" --command " + clcommon.ShellQuoteArg(command)
	if allowUnenforced {
		wrapper += " --allow-unenforced"
	}
	return wrapper
}

// WrapPreparedResourceCgroupCommand renders the pane-side resource wrapper for
// a cgroup that agentd already prepared. Managed servers use this when their
// durable process tree must be forked by the external tmux runtime rather than
// by agentd itself.
func WrapPreparedResourceCgroupCommand(sessionID, dir, command string, allowUnenforced bool) string {
	return wrapPreparedResourceCgroupCommand(sessionID, dir, command, allowUnenforced)
}

// ConfigureProcessResourceCgroup asks clone3 to place cmd in the prepared
// cgroup atomically, before its program executes. The returned cleanup closes
// the directory FD after Start returns; it does not remove the cgroup.
func ConfigureProcessResourceCgroup(cmd *exec.Cmd, dir string) (func(), error) {
	dirFD, err := os.Open(dir)
	if err != nil {
		return func() {}, fmt.Errorf("open prepared resource cgroup: %w", err)
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.UseCgroupFD = true
	cmd.SysProcAttr.CgroupFD = int(dirFD.Fd())
	return func() { _ = dirFD.Close() }, nil
}

// ValidatePreparedResourceCgroup confirms that a durable managed-server
// boundary still carries the limits requested by the current relaunch.
func ValidatePreparedResourceCgroup(dir string, limits sandboxpolicy.ResourceLimits) error {
	if delegation := configuredResourceDelegationDir(); delegation != "" && filepath.Dir(filepath.Clean(dir)) != filepath.Clean(delegation) {
		return fmt.Errorf("prepared resource cgroup is outside the configured resource delegation directory")
	}
	wantMemory := "max"
	if limits.Memory != "" {
		want, err := sandboxpolicy.ParseMemoryLimitBytes(limits.Memory)
		if err != nil {
			return err
		}
		wantMemory = strconv.FormatUint(want, 10)
	}
	gotMemory, err := os.ReadFile(filepath.Join(dir, "memory.max"))
	if err != nil && (limits.Memory != "" || !errors.Is(err, os.ErrNotExist)) {
		return fmt.Errorf("prepared resource cgroup memory.max no longer matches requested limit")
	}
	if err == nil && strings.TrimSpace(string(gotMemory)) != wantMemory {
		return fmt.Errorf("prepared resource cgroup memory.max no longer matches requested limit")
	}
	wantCPU := fmt.Sprintf("max %d", sandboxpolicy.CPUCgroupPeriodMicros)
	if limits.CPU != nil {
		quota, err := sandboxpolicy.CPUQuotaMicros(*limits.CPU)
		if err != nil {
			return err
		}
		wantCPU = fmt.Sprintf("%d %d", quota, sandboxpolicy.CPUCgroupPeriodMicros)
	}
	gotCPU, err := os.ReadFile(filepath.Join(dir, "cpu.max"))
	if err != nil && (limits.CPU != nil || !errors.Is(err, os.ErrNotExist)) {
		return fmt.Errorf("prepared resource cgroup cpu.max no longer matches requested limit")
	}
	if err == nil && strings.TrimSpace(string(gotCPU)) != wantCPU {
		return fmt.Errorf("prepared resource cgroup cpu.max no longer matches requested limit")
	}
	return nil
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
			moveErr = fmt.Errorf("record required resource-limit override disclosure: %w", persistErr)
		} else {
			fmt.Fprintf(os.Stderr, "Warning: %v; launching without configured resource-limit enforcement by operator approval\n", noticeErr)
			_, moveErr = io.WriteString(gateWrite, "go\n")
		}
	}
	_ = gateWrite.Close()
	if moveErr != nil {
		_ = child.Process.Kill()
		_ = child.Wait()
		return fmt.Errorf("attach workload to resource cgroup before release: %w", moveErr)
	}
	waitErr := child.Wait()
	if ResourceCgroupOOMKilled(cgroupDir) {
		if err := recordResourceLimitOOMForExec(sessionID); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: record resource-limit OOM outcome: %v\n", err)
		}
	}
	if err := os.Remove(cgroupDir); err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "Warning: remove empty resource cgroup %s: %v\n", cgroupDir, err)
	}
	if waitErr == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		os.Exit(resourceLimitChildExitCode(exitErr))
	}
	return waitErr
}

func resourceLimitChildExitCode(exitErr *exec.ExitError) int {
	if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return 128 + int(status.Signal())
	}
	return exitErr.ExitCode()
}

func ResourceCgroupOOMKilled(dir string) bool {
	raw, err := os.ReadFile(filepath.Join(dir, "memory.events"))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[0] != "oom_kill" {
			continue
		}
		count, parseErr := strconv.ParseUint(fields[1], 10, 64)
		return parseErr == nil && count > 0
	}
	return false
}
