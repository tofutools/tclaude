//go:build linux || darwin

package agentd

import "syscall"

// execSelfUpdate replaces this process with the restarted daemon; tests
// swap it.
var execSelfUpdate = func(path string, args, env []string) error { return syscall.Exec(path, args, env) }
