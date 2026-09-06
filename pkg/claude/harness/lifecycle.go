package harness

// Lifecycle names the remaining in-pane feature controls a harness exposes.
// Workload Stop is intentionally absent: native stop mechanisms and retry
// policy belong to attempt-bound runtime adapters, not this descriptor.
//
// An empty token means unsupported. Tokens are compile-time constants because
// tmux pane delivery is an injection sink.
type Lifecycle interface {
	// RenameCommand renames the active native session. The caller appends the
	// already-validated title. Empty means titles are stored out of band.
	RenameCommand() string
	// CompactCommand compacts the active native context. Empty is unsupported.
	CompactCommand() string
	// RemoteControlCommand toggles the harness's built-in remote access.
	RemoteControlCommand() string
	// FastModeCommand toggles fast mode for the active thread.
	FastModeCommand() string
}
