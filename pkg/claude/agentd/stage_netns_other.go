//go:build !linux

package agentd

// stageForeignNetnsPID is Linux-only. On macOS a Seatbelt-sandboxed agent
// shares the host's loopback, so its port is reached through the ownership
// proof like a host-network agent's.
func stageForeignNetnsPID(int) int { return 0 }
