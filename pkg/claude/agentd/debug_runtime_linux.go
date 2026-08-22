//go:build linux

package agentd

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/session"
)

func observeAgentProcess(rootPID int, harnessName string) agentDebugLiveProcess {
	out := agentDebugLiveProcess{}
	if rootPID <= 0 {
		out.Status, out.Detail = "unavailable", "no harness process PID is recorded"
		return out
	}
	pid := findHostHarnessDescendant(rootPID, harnessName)
	if pid <= 0 {
		out.Status, out.Detail = "unavailable", "no validated harness process is visible beneath the host launch boundary"
		return out
	}
	startTime, ok := debugProcStartTime(pid)
	if !ok {
		out.Status, out.Detail = "unavailable", "cannot bind the harness PID to a process generation"
		return out
	}
	out.PID = pid
	status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		out.Status, out.Detail = "unavailable", "cannot read the recorded harness process status"
		return out
	}
	if executable, exeErr := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid)); exeErr == nil {
		out.ExecutablePath = strings.TrimSuffix(executable, " (deleted)")
	}
	if uidMap, mapErr := os.ReadFile(fmt.Sprintf("/proc/%d/uid_map", pid)); mapErr == nil {
		out.UIDMap = strings.TrimSpace(string(uidMap))
	}
	if gidMap, mapErr := os.ReadFile(fmt.Sprintf("/proc/%d/gid_map", pid)); mapErr == nil {
		out.GIDMap = strings.TrimSpace(string(gidMap))
	}
	for _, line := range strings.Split(string(status), "\n") {
		name, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		values := strings.Fields(value)
		switch name {
		case "Uid":
			out.UID = parseAgentDebugIDSet(values)
		case "Gid":
			out.GID = parseAgentDebugIDSet(values)
		case "Groups":
			for _, value := range values {
				if id, parseErr := strconv.Atoi(value); parseErr == nil {
					out.Groups = append(out.Groups, id)
				}
			}
		}
	}
	environ, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
	if err != nil {
		out.Status, out.Detail = "partial", "identity observed; process environment is unreadable"
		return out
	}
	for _, pair := range strings.Split(string(environ), "\x00") {
		if strings.HasPrefix(pair, "PATH=") {
			out.PATH = strings.TrimPrefix(pair, "PATH=")
			break
		}
	}
	if after, stable := debugProcStartTime(pid); !stable || after != startTime {
		return agentDebugLiveProcess{
			Status: "unavailable",
			Detail: "harness process exited or its PID was reused during observation",
		}
	}
	out.Status = "observed"
	if out.PATH == "" {
		out.Status, out.Detail = "partial", "identity observed; PATH is absent from the process environment"
	}
	return out
}

func debugProcStartTime(pid int) (string, bool) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", false
	}
	text := string(data)
	closeParen := strings.LastIndex(text, ")")
	if closeParen < 0 || closeParen+2 >= len(text) {
		return "", false
	}
	// Fields after comm begin at field 3 (state); starttime is field 22.
	fields := strings.Fields(text[closeParen+2:])
	if len(fields) <= 19 {
		return "", false
	}
	return fields[19], true
}

func findHostHarnessDescendant(rootPID int, harnessName string) int {
	children := map[int][]int{}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	for _, entry := range entries {
		pid, parseErr := strconv.Atoi(entry.Name())
		if parseErr != nil || pid <= 1 {
			continue
		}
		if parent := debugProcParentPID(pid); parent > 0 {
			children[parent] = append(children[parent], pid)
		}
	}
	queue := []int{rootPID}
	seen := map[int]bool{}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		if seen[pid] {
			continue
		}
		seen[pid] = true
		name := session.GetProcessName(pid)
		exe := session.GetProcessExeName(pid)
		matchesExpected := name == harnessName || exe == harnessName ||
			(harnessName == "claude" && (name == "node" || exe == "node"))
		if (harnessName == "" || matchesExpected) && session.IsHarnessProcessAt(pid, name) {
			return pid
		}
		queue = append(queue, children[pid]...)
	}
	return 0
}

func debugProcParentPID(pid int) int {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	text := string(data)
	closeParen := strings.LastIndex(text, ")")
	if closeParen < 0 || closeParen+2 >= len(text) {
		return 0
	}
	fields := strings.Fields(text[closeParen+2:])
	if len(fields) < 2 {
		return 0
	}
	parent, _ := strconv.Atoi(fields[1])
	return parent
}

func parseAgentDebugIDSet(values []string) *agentDebugIDSet {
	if len(values) < 4 {
		return nil
	}
	ids := [4]int{}
	for i := range ids {
		value, err := strconv.Atoi(values[i])
		if err != nil {
			return nil
		}
		ids[i] = value
	}
	return &agentDebugIDSet{Real: ids[0], Effective: ids[1], Saved: ids[2], Filesystem: ids[3]}
}
