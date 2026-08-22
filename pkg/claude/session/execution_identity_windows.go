//go:build windows

package session

func currentExecutionIdentity() ExecutionUnixIdentity {
	return ExecutionUnixIdentity{UID: -1, GID: -1}
}
