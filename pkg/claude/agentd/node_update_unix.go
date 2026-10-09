//go:build linux || darwin

package agentd

import "syscall"

func execSelfUpdate(path string, args, env []string) error { return syscall.Exec(path, args, env) }
