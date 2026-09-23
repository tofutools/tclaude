//go:build linux

package session

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// runCgroupReleaseGrace is how long a released one-shot run cgroup may take
// to empty on its own before its remaining members are killed. The caller
// releases the boundary by exiting, so normally only its own dying process is
// still counted when the release arrives.
var runCgroupReleaseGrace = 2 * time.Second

// PrepareRunCgroup creates a fresh workload cgroup for one `tclaude run`
// invocation and moves the process pid into it. The requested limits are
// clamped to every ceiling already applied between the caller's current cgroup
// and the delegation root: the new cgroup is a sibling of the caller's, so
// without the clamp a limited agent could leave its own budget by asking for a
// looser one. It returns the cgroup, the limits actually written, and a release
// function that waits briefly for the members to exit, kills any that remain,
// and removes the cgroup.
func PrepareRunCgroup(pid int, requested sandboxpolicy.ResourceLimits) (string, sandboxpolicy.ResourceLimits, func(), error) {
	noop := func() {}
	if pid <= 1 {
		return "", sandboxpolicy.ResourceLimits{}, noop, fmt.Errorf("invalid caller pid %d", pid)
	}
	requested, err := sandboxpolicy.NormalizeResourceLimits(requested)
	if err != nil {
		return "", sandboxpolicy.ResourceLimits{}, noop, err
	}
	delegation, err := workloadResourceDelegationDir()
	if err != nil {
		return "", sandboxpolicy.ResourceLimits{}, noop, err
	}
	callerDir, err := processCgroupDir(strconv.Itoa(pid))
	if err != nil {
		return "", sandboxpolicy.ResourceLimits{}, noop, err
	}
	if !sandboxpolicy.PathContainsOrEqual(delegation, callerDir) {
		return "", sandboxpolicy.ResourceLimits{}, noop, fmt.Errorf(
			"the calling process is in cgroup %s, outside agentd's delegated subtree %s; a run cgroup can only be created for processes agentd launched",
			callerDir, delegation)
	}
	inherited, err := inheritedRunCgroupCeilings(callerDir, delegation)
	if err != nil {
		return "", sandboxpolicy.ResourceLimits{}, noop, err
	}
	applied, err := clampRunCgroupLimits(requested, inherited)
	if err != nil {
		return "", sandboxpolicy.ResourceLimits{}, noop, err
	}
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", sandboxpolicy.ResourceLimits{}, noop, err
	}
	dir, cleanup, err := PrepareResourceCgroup("run-"+hex.EncodeToString(suffix[:]), applied)
	if err != nil {
		return "", sandboxpolicy.ResourceLimits{}, noop, err
	}
	if err := attachWorkloadToResourceCgroup(dir, pid); err != nil {
		cleanup()
		return "", sandboxpolicy.ResourceLimits{}, noop, fmt.Errorf("move caller into run cgroup: %w", err)
	}
	release := func() {
		deadline := time.Now().Add(runCgroupReleaseGrace)
		for resourceCgroupPopulated(dir) && time.Now().Before(deadline) {
			time.Sleep(resourceCgroupKillPoll)
		}
		_ = RemoveResourceCgroup(dir)
	}
	return dir, applied, release, nil
}

// runCgroupCeilings are the tightest limits found on a cgroup path. A nil
// field means no ceiling on that axis.
type runCgroupCeilings struct {
	memoryBytes *uint64
	cpuCores    *float64
	pids        *uint64
}

// inheritedRunCgroupCeilings reads memory.max, cpu.max and pids.max from the
// caller's cgroup and each ancestor below the delegation root. The root's own
// limits apply to the new sibling cgroup anyway, so it is not consulted.
func inheritedRunCgroupCeilings(callerDir, delegation string) (runCgroupCeilings, error) {
	var out runCgroupCeilings
	delegation = filepath.Clean(delegation)
	for dir := filepath.Clean(callerDir); dir != delegation; dir = filepath.Dir(dir) {
		if !sandboxpolicy.PathContainsOrEqual(delegation, dir) {
			break
		}
		if raw, ok, err := readRunCgroupValue(dir, "memory.max"); err != nil {
			return out, err
		} else if ok && raw != "max" {
			value, err := strconv.ParseUint(raw, 10, 64)
			if err != nil {
				return out, fmt.Errorf("parse %s: %w", filepath.Join(dir, "memory.max"), err)
			}
			if out.memoryBytes == nil || value < *out.memoryBytes {
				out.memoryBytes = &value
			}
		}
		if raw, ok, err := readRunCgroupValue(dir, "cpu.max"); err != nil {
			return out, err
		} else if ok {
			fields := strings.Fields(raw)
			if len(fields) == 2 && fields[0] != "max" {
				quota, qErr := strconv.ParseUint(fields[0], 10, 64)
				period, pErr := strconv.ParseUint(fields[1], 10, 64)
				if qErr != nil || pErr != nil || period == 0 {
					return out, fmt.Errorf("parse %s: %q", filepath.Join(dir, "cpu.max"), raw)
				}
				cores := float64(quota) / float64(period)
				if out.cpuCores == nil || cores < *out.cpuCores {
					out.cpuCores = &cores
				}
			}
		}
		if raw, ok, err := readRunCgroupValue(dir, "pids.max"); err != nil {
			return out, err
		} else if ok && raw != "max" {
			value, err := strconv.ParseUint(raw, 10, 64)
			if err != nil {
				return out, fmt.Errorf("parse %s: %w", filepath.Join(dir, "pids.max"), err)
			}
			if out.pids == nil || value < *out.pids {
				out.pids = &value
			}
		}
	}
	return out, nil
}

// readRunCgroupValue reads one interface file. A file that does not exist
// means the controller is not enabled there, which imposes no ceiling.
func readRunCgroupValue(dir, name string) (string, bool, error) {
	raw, err := os.ReadFile(filepath.Join(dir, name))
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read %s: %w", filepath.Join(dir, name), err)
	}
	return strings.TrimSpace(string(raw)), true, nil
}

// clampRunCgroupLimits keeps each requested axis no looser than the inherited
// ceiling, and fills an axis the request left open with that ceiling.
func clampRunCgroupLimits(requested sandboxpolicy.ResourceLimits, inherited runCgroupCeilings) (sandboxpolicy.ResourceLimits, error) {
	out := requested
	if inherited.memoryBytes != nil && (requested.Memory == "" || requested.MemoryBytes > *inherited.memoryBytes) {
		out.Memory = strconv.FormatUint(*inherited.memoryBytes, 10) + "B"
	}
	if inherited.cpuCores != nil && (requested.CPU == nil || *requested.CPU > *inherited.cpuCores) {
		// Round down to the cpu.max quota granularity so the written quota
		// never exceeds the inherited one.
		cores := math.Floor(*inherited.cpuCores*float64(sandboxpolicy.CPUCgroupPeriodMicros)) /
			float64(sandboxpolicy.CPUCgroupPeriodMicros)
		out.CPU = &cores
	}
	if inherited.pids != nil && (requested.PIDs == nil || *requested.PIDs > *inherited.pids) {
		value := *inherited.pids
		out.PIDs = &value
	}
	return sandboxpolicy.NormalizeResourceLimits(out)
}
