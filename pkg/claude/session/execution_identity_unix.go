//go:build unix

package session

import "os"

func currentExecutionIdentity() ExecutionUnixIdentity {
	groups, _ := os.Getgroups()
	return ExecutionUnixIdentity{UID: os.Getuid(), GID: os.Getgid(), Groups: groups}
}
