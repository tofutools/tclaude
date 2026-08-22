//go:build !linux

package agentd

func observeAgentProcess(pid int, _ string) agentDebugLiveProcess {
	return agentDebugLiveProcess{
		Status: "unsupported", PID: pid,
		Detail: "live process PATH and uid/gid observation currently requires Linux /proc",
	}
}
