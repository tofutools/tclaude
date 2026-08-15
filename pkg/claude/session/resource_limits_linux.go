//go:build linux

package session

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

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

// enableDelegatedControllers turns on the controllers a workload cgroup needs in
// its parent. An empty request writes nothing at all rather than writing an
// empty string, which keeps a launch working on a delegated node whose
// cgroup.subtree_control is not writable but already carries the controllers.
var enableDelegatedControllers = func(delegation string, toEnable []string) error {
	if len(toEnable) == 0 {
		return nil
	}
	return os.WriteFile(filepath.Join(delegation, "cgroup.subtree_control"),
		[]byte(strings.Join(toEnable, " ")), 0o644)
}

// resourceDelegationProcessCount reports how many processes a cgroup holds
// directly, and whether that could be established at all. The two are different
// answers: a node proven empty rules a diagnosis out, while a cgroup.procs the
// launch could not read — which the same access control that refused the write
// can also cause — leaves it open.
func resourceDelegationProcessCount(dir string) (int, bool) {
	raw, err := os.ReadFile(filepath.Join(dir, "cgroup.procs"))
	if err != nil {
		return 0, false
	}
	return len(strings.Fields(string(raw))), true
}

// resourceDelegationBusyHint explains an EBUSY refusal from a delegated node's
// cgroup.subtree_control. cgroup v2 forbids a cgroup from both holding processes
// of its own and enabling controllers for its children, so a delegated node that
// anything still lives in cannot be configured at all — and the kernel reports
// that as "device or resource busy", which names neither the rule nor the
// processes keeping it in force.
//
// The returned text is empty when the refusal was something else, or when the
// node is proven process-free and so cannot be failing this rule, so the hint
// only appears where it is genuinely the cause.
func resourceDelegationBusyHint(delegation string, err error) string {
	if !errors.Is(err, syscall.EBUSY) {
		return ""
	}
	held, counted := resourceDelegationProcessCount(delegation)
	if counted && held == 0 {
		return ""
	}
	population := fmt.Sprintf("%s still holds %d process(es) of its own", delegation, held)
	if !counted {
		// EBUSY on this write has no other cause worth naming, so an uncountable
		// node still gets the rule and the fix — without asserting a number the
		// launch could not read.
		population = fmt.Sprintf("%s holds processes of its own (its cgroup.procs could not be read to say how many)", delegation)
	}
	return fmt.Sprintf("%s, and cgroup v2 refuses to enable controllers in a cgroup that is not process-free; every process there has to move into a child cgroup first. DelegateSubgroup=%s arranges that for a systemd service, whose main process systemd forks itself, but it has no effect on a scope: a scope's processes are started by something else and merely registered into it, so nothing places them in the subgroup. Under a scope, move into a %s child from the launcher before starting anything else",
		population, resourceSupervisorCgroup, resourceSupervisorCgroup)
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

// resourceCgroupKillWait bounds how long a reclaim waits for the kernel to
// finish killing a cgroup's members; resourceCgroupKillPoll is how often the
// populated state is re-read while waiting. Both are variables so tests can
// exercise the timeout without the production wait.
var resourceCgroupKillWait = 5 * time.Second
var resourceCgroupKillPoll = 20 * time.Millisecond

// requestResourceCgroupKill asks the kernel to SIGKILL every process in the
// cgroup at dir, including any nested descendants. A seam so tests on a fake
// cgroup filesystem can simulate the member deaths the real write causes.
var requestResourceCgroupKill = func(dir string) error {
	return os.WriteFile(filepath.Join(dir, "cgroup.kill"), []byte("1"), 0o644)
}

// resourceCgroupPopulated reports whether the cgroup at dir still holds any
// live process, per the "populated" key of its cgroup.events. A dir whose
// events file cannot be read reports unpopulated: it is either already removed
// or inaccessible, and waiting helps neither.
func resourceCgroupPopulated(dir string) bool {
	raw, err := os.ReadFile(filepath.Join(dir, "cgroup.events"))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "populated" {
			return fields[1] != "0"
		}
	}
	return false
}

// staleResourceCgroupHint tells an operator how to clear a stuck boundary by
// hand. `rm -rf` can never do it — cgroup interface files are virtual and
// return EPERM on unlink even for root — so name the only sequence that works.
func staleResourceCgroupHint(dir string) string {
	return fmt.Sprintf("processes from this session's previous launch are still inside; a cgroup cannot be deleted with rm — kill the members with `echo 1 | sudo tee %s` and then `sudo rmdir %s`",
		filepath.Join(dir, "cgroup.kill"), dir)
}

// reclaimBusyResourceCgroup kills whatever still lives in the cgroup at dir
// and waits for the kernel to report it empty. Callers own the safety
// argument: dir must be a boundary this launch is entitled to clear, which
// per-session naming provides — the only processes that can ever be inside are
// ones a launch of that same session placed there, and cgroup membership is
// inherited and inescapable, so this reaps exactly the session's strays.
func reclaimBusyResourceCgroup(dir string) error {
	if err := requestResourceCgroupKill(dir); err != nil {
		// A dir gone between the caller's check and the kill write means some
		// other owner (the pane wrapper, an exit waiter) finished the reclaim
		// first, which is the goal state rather than a failure.
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("kill remaining cgroup processes: %w", err)
	}
	deadline := time.Now().Add(resourceCgroupKillWait)
	for resourceCgroupPopulated(dir) {
		if time.Now().After(deadline) {
			return fmt.Errorf("processes remain after cgroup.kill")
		}
		time.Sleep(resourceCgroupKillPoll)
	}
	return nil
}

// KillResourceCgroupMembers reaps every process still inside a prepared
// per-session cgroup, leaving the (durable, relaunch-reused) directory itself
// in place. Managed-server teardown calls this after its polite process-tree
// kills: a double-forked server descendant survives those but can never leave
// the session's cgroup. A missing or already-empty boundary is success.
func KillResourceCgroupMembers(dir string) error {
	if dir == "" || !resourceCgroupPopulated(dir) {
		return nil
	}
	return reclaimBusyResourceCgroup(dir)
}

// RemoveResourceCgroup retires an agentd-owned durable boundary after its
// managed server is permanently stopped. Relaunch and port-retry paths use
// KillResourceCgroupMembers instead so the same directory remains reusable.
func RemoveResourceCgroup(dir string) error {
	if dir == "" {
		return nil
	}
	if err := KillResourceCgroupMembers(dir); err != nil {
		return err
	}
	if err := os.Remove(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
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
	return wrapPreparedResourceCgroupCommand(sessionID, dir, command, allowUnenforced, false, false), cleanup, nil
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
		return "", func() {}, fmt.Errorf("a per-agent cgroup requires a delegated cgroup v2 service subtree (set --resource-delegation-dir to an external delegated root, or configure tclaude-agentd.service with Delegate=cpu memory and DelegateSubgroup=%s): %w", resourceSupervisorCgroup, err)
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
			return "", func() {}, fmt.Errorf("the configured resource limit requires the delegated cgroup v2 %s controller; configure the external --resource-delegation-dir runtime with Delegate=cpu memory, or configure tclaude-agentd.service with Delegate=cpu memory and DelegateSubgroup=%s", controller, resourceSupervisorCgroup)
		}
	}
	wanted := append([]string{}, needed...)
	if !limits.Enabled() {
		// An accounting-only boundary wants every counter the delegation carries,
		// but none of them is a ceiling: a controller this subtree lacks costs the
		// operator visibility rather than enforcement, so it degrades rather than
		// refusing. pids is included opportunistically — the documented
		// `Delegate=cpu memory` does not carry it.
		for _, controller := range []string{"memory", "cpu", "pids"} {
			if containsString(available, controller) {
				wanted = append(wanted, controller)
				continue
			}
			slog.Warn("resource cgroup: delegated subtree does not carry a controller; its per-agent counters stay unavailable",
				"session_id", sessionID, "controller", controller, "delegation", delegation)
		}
	}
	enabledRaw, err := os.ReadFile(filepath.Join(delegation, "cgroup.subtree_control"))
	if err != nil {
		return "", func() {}, fmt.Errorf("inspect delegated cgroup controllers: %w", err)
	}
	enabled := strings.Fields(string(enabledRaw))
	toEnable := []string{}
	for _, controller := range wanted {
		if !containsString(enabled, controller) {
			toEnable = append(toEnable, "+"+controller)
		}
	}
	if err := enableDelegatedControllers(delegation, toEnable); err != nil {
		busy := resourceDelegationBusyHint(delegation, err)
		switch {
		case !limits.Enabled():
			// No ceiling depends on these controllers, and the boundary is still
			// worth creating for its process tracking and OOM attribution. The
			// warning is the only trace this degradation leaves, so it carries the
			// cause when one can be established rather than only the raw errno.
			attrs := []any{
				"session_id", sessionID, "delegation", delegation,
				"controllers", strings.Join(wanted, ", "), "error", err,
			}
			if busy != "" {
				attrs = append(attrs, "cause", busy)
			}
			slog.Warn("resource cgroup: cannot enable delegated controllers for accounting; per-agent counters stay unavailable",
				attrs...)
		case busy != "":
			return "", func() {}, fmt.Errorf("enable delegated cgroup v2 controllers %s: %w (%s)", strings.Join(wanted, ", "), err, busy)
		case delegationWriteRefused(err):
			return "", func() {}, fmt.Errorf("enable delegated cgroup v2 controllers %s: %w (%s)", strings.Join(wanted, ", "), err, resourceDelegationDeniedHint(delegation))
		default:
			// A node still holding processes reports EBUSY and is diagnosed above,
			// so this carries only the delegation advice that remains.
			return "", func() {}, fmt.Errorf("enable delegated cgroup v2 controllers %s: %w (the external --resource-delegation-dir runtime needs Delegate=cpu memory, or tclaude-agentd.service needs Delegate=cpu memory and DelegateSubgroup=%s)", strings.Join(wanted, ", "), err, resourceSupervisorCgroup)
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
		// reclaim that exact target; never sweep other launches, whose newly
		// prepared cgroups are also briefly empty before pane attachment.
		if removeErr := os.Remove(dir); removeErr != nil {
			// EBUSY here means processes still live inside. A launch for this
			// session is only prepared once its prior life is stopped, so what
			// remains is a stray of that life — typically a server descendant
			// that outlived the pane or process-tree kill. Reap it and retry;
			// per-session naming makes that safe (see reclaimBusyResourceCgroup).
			if killErr := reclaimBusyResourceCgroup(dir); killErr != nil {
				return "", func() {}, fmt.Errorf("resource cgroup for session %q already exists and is active or not reclaimable: %w (%v; %s)", sessionID, removeErr, killErr, staleResourceCgroupHint(dir))
			}
			if removeErr := os.Remove(dir); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				return "", func() {}, fmt.Errorf("resource cgroup for session %q already exists and is active or not reclaimable: %w (%s)", sessionID, removeErr, staleResourceCgroupHint(dir))
			}
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

// wrapPreparedResourceCgroupCommand renders the resource-limit-exec wrapper.
// shared marks a wrapper whose child merely joins a boundary that a managed
// server owns and lives in: an attach pane. A shared wrapper must never reap
// or remove the boundary at exit — its child's death says nothing about the
// server's life, and reaping there kills the server through the shared cgroup.
func wrapPreparedResourceCgroupCommand(sessionID, dir, command string, allowUnenforced, shared, preserve bool) string {
	wrapper := clcommon.DetectAbsoluteCmd("session", "resource-limit-exec") +
		" --cgroup-dir " + clcommon.ShellQuoteArg(dir) +
		" --session-id " + clcommon.ShellQuoteArg(sessionID) +
		" --command " + clcommon.ShellQuoteArg(command)
	if allowUnenforced {
		wrapper += " --allow-unenforced"
	}
	if shared {
		wrapper += " --shared-boundary"
	}
	if preserve {
		wrapper += " --preserve-boundary"
	}
	return wrapper
}

// WrapPreparedResourceCgroupCommand renders the pane-side resource wrapper for
// a durable cgroup that agentd already prepared and owns. Managed servers use
// this when their process tree must be forked by the external tmux runtime
// rather than by agentd itself. The wrapper must leave the boundary in place:
// agentd can retry the server launch in that same boundary, and owns reaping
// its members during managed-server teardown.
func WrapPreparedResourceCgroupCommand(sessionID, dir, command string, allowUnenforced bool) string {
	return wrapPreparedResourceCgroupCommand(sessionID, dir, command, allowUnenforced, false, true)
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
	var allowUnenforced, sharedBoundary, preserveBoundary bool
	cmd := &cobra.Command{
		Use:    "resource-limit-exec",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runResourceLimitExec(cgroupDir, sessionID, command, allowUnenforced, sharedBoundary, preserveBoundary)
		},
	}
	cmd.Flags().StringVar(&cgroupDir, "cgroup-dir", "", "prepared cgroup directory")
	cmd.Flags().StringVar(&sessionID, "session-id", "", "managed session id")
	cmd.Flags().StringVar(&command, "command", "", "shell command")
	cmd.Flags().BoolVar(&allowUnenforced, "allow-unenforced", false, "operator authorized fallback without enforcement")
	cmd.Flags().BoolVar(&sharedBoundary, "shared-boundary", false, "the boundary belongs to a managed server; never reap or remove it at exit")
	cmd.Flags().BoolVar(&preserveBoundary, "preserve-boundary", false, "reap the workload but preserve the agentd-owned boundary for reuse")
	return cmd
}

func runResourceLimitExec(cgroupDir, sessionID, command string, allowUnenforced, sharedBoundary, preserveBoundary bool) error {
	if sharedBoundary && preserveBoundary {
		return errors.New("resource-limit-exec boundary cannot be both shared and preserved")
	}
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
	// Read the counter before the workload can contribute to it. A durable
	// managed-server boundary is reused across relaunches, so a nonzero reading
	// here belongs to an earlier one.
	oomBaseline := ReadResourceCgroupOOMKills(cgroupDir)
	gateRead, gateWrite, err := os.Pipe()
	if err != nil {
		return err
	}
	defer func() { _ = gateRead.Close(); _ = gateWrite.Close() }()
	// The gate shell only waits on fd 3 and execs; the INNER shell is the one
	// that interprets the harness command, so that is the one pinned
	// (clcommon.BootstrapShellArgv) rather than left to whatever /bin/sh is.
	// The inner argv rides as trailing positional parameters and is re-formed
	// with "$@" after the command is saved off, so it carries however many
	// words the resolver returns.
	gateArgs := append([]string{
		"-c",
		`IFS= read -r tclaude_resource_gate <&3 || exit 125; ` +
			`tclaude_resource_command=$1; shift; exec "$@" -c "$tclaude_resource_command"`,
		"tclaude-resource-limit", command,
	}, clcommon.BootstrapShellArgv()...)
	child := exec.Command("/bin/sh", gateArgs...)
	child.Stdin, child.Stdout, child.Stderr = os.Stdin, os.Stdout, os.Stderr
	child.ExtraFiles = []*os.File{gateRead}
	if err := child.Start(); err != nil {
		return err
	}
	// A pane teardown (tmux kill-session HUPs the pane's process group, a
	// session stop may TERM it) reaches this wrapper too. Left to the default
	// disposition it would die on the spot, and any workload descendant that
	// double-forked would outlive the pane inside the boundary — populating
	// the cgroup forever and blocking the session's next wake. Reap the whole
	// boundary instead: the kill takes the gated workload with it, so the
	// ordinary wait-and-remove path below still finishes the cleanup. SIGINT
	// stays untouched — an interactive harness owns that keystroke. A shared
	// boundary is exempt from all of this: the managed server that owns it
	// lives there too, and its lifecycle belongs to agentd, not to this pane.
	if !sharedBoundary {
		teardown := make(chan os.Signal, 1)
		signal.Notify(teardown, syscall.SIGHUP, syscall.SIGTERM)
		defer signal.Stop(teardown)
		go func() {
			for range teardown {
				_ = KillResourceCgroupMembers(cgroupDir)
			}
		}()
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
	if ResourceCgroupOOMDeath(cgroupDir, oomBaseline, waitErr) {
		if err := recordResourceLimitOOMForExec(sessionID); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: record resource-limit OOM outcome: %v\n", err)
		}
	}
	// The workload is gone, but a descendant it double-forked may not be, and
	// it can never leave the boundary. Reap it so the rmdir below succeeds and
	// nothing keeps running against a session that has ended. Never for a
	// shared boundary: there the child was an attach client and the boundary's
	// other member is the live managed server itself.
	if !sharedBoundary {
		if err := KillResourceCgroupMembers(cgroupDir); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: reap resource cgroup %s: %v\n", cgroupDir, err)
		}
		if !preserveBoundary {
			if err := os.Remove(cgroupDir); err != nil && !errors.Is(err, os.ErrNotExist) {
				fmt.Fprintf(os.Stderr, "Warning: remove empty resource cgroup %s: %v\n", cgroupDir, err)
			}
		}
	}
	if waitErr == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		// Production never returns from this; the seam exists so a test can drive
		// the non-zero exits the OOM attribution above is decided on.
		resourceLimitExecExit(resourceLimitChildExitCode(exitErr))
		return nil
	}
	return waitErr
}

var resourceLimitExecExit = os.Exit

func resourceLimitChildExitCode(exitErr *exec.ExitError) int {
	if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return 128 + int(status.Signal())
	}
	return exitErr.ExitCode()
}

// ReadResourceCgroupOOMKills reads how many OOM kills the kernel has performed
// in the cgroup at dir since that cgroup was created. The counter only ever
// rises while the cgroup lives, so it says nothing on its own about which
// launch, or which process, the kills belong to; take a reading when a workload
// starts and hand it to ResourceCgroupOOMDeath when that workload exits.
func ReadResourceCgroupOOMKills(dir string) ResourceCgroupOOMCount {
	// An empty dir is a launch with no boundary at all; joining it would read
	// memory.events relative to the working directory.
	if dir == "" {
		return ResourceCgroupOOMCount{}
	}
	raw, err := os.ReadFile(filepath.Join(dir, "memory.events"))
	if err != nil {
		return ResourceCgroupOOMCount{}
	}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[0] != "oom_kill" {
			continue
		}
		count, parseErr := strconv.ParseUint(fields[1], 10, 64)
		if parseErr != nil {
			return ResourceCgroupOOMCount{}
		}
		return ResourceCgroupOOMCount{kills: count, known: true}
	}
	return ResourceCgroupOOMCount{}
}

// ResourceCgroupOOMDeath reports whether a workload that has just exited was
// killed by the memory ceiling of the cgroup at dir. baseline is the
// ReadResourceCgroupOOMKills reading taken when that workload was started, and
// waitErr is what waiting on it returned.
func ResourceCgroupOOMDeath(dir string, baseline ResourceCgroupOOMCount, waitErr error) bool {
	return resourceLimitOOMDeath(baseline, ReadResourceCgroupOOMKills(dir), waitErr)
}

// resourceLimitOOMDeath decides whether the memory ceiling is what ended this
// workload. Two facts have to line up, because either alone misattributes.
//
// A rise in the counter is necessary: memory.events counts oom_kill for the life
// of the cgroup, which spans every relaunch into a durable managed-server
// boundary and every earlier kill the workload shrugged off, so only kills since
// this workload started can bear on how it ended. A reading that could not be
// taken at either end leaves the rise unestablished, which attributes nothing
// rather than treating an unknown baseline as zero.
//
// A rise is not sufficient. The kernel kills the greediest task in the cgroup,
// which is frequently a descendant the harness survives — an agent that runs one
// memory-hungry child and carries on is the ordinary case, not the exceptional
// one. The workload also has to have died of a kill, per
// resourceLimitWorkloadDiedOnKill.
//
// The pairing still under-reports rather than over-reports: a harness that exits
// with some other non-zero status because a descendant was killed reads as the
// ordinary failure it is, and a workload killed for an unrelated reason after
// surviving an earlier OOM is misread. What it will not do is report an OOM
// death for a workload that exited cleanly, which is the misattribution an
// operator actually sees.
func resourceLimitOOMDeath(before, after ResourceCgroupOOMCount, waitErr error) bool {
	if !before.known || !after.known || after.kills <= before.kills {
		return false
	}
	return resourceLimitWorkloadDiedOnKill(waitErr)
}

// resourceLimitWorkloadDiedOnKill reports whether a wait result is consistent
// with SIGKILL, the signal the OOM killer sends, having ended the workload.
//
// Both shapes it can take are real, and which one appears is not tclaude's
// choice. A workload killed directly is reported as signalled. But the process
// waited on here is a shell wrapping the harness, and /bin/sh forks rather than
// execs the command it is given on the systems this runs on — so the harness the
// kernel actually picks is a child of that shell. The shell reaps it and exits
// normally, relaying the death as the conventional 128+signal status. Reading
// only the signalled shape would therefore miss every real OOM on the pane path,
// and on any sandboxed managed server, whose bwrap relays the same way.
func resourceLimitWorkloadDiedOnKill(waitErr error) bool {
	var exitErr *exec.ExitError
	if !errors.As(waitErr, &exitErr) {
		return false
	}
	if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return status.Signal() == syscall.SIGKILL
	}
	return exitErr.ExitCode() == resourceLimitKilledExitCode
}
