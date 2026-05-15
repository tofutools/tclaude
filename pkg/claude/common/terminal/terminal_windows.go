//go:build windows

package terminal

import "os/exec"

// resolveTerminalLauncher detects the Windows terminal to use once (see
// terminal.go's Resolve): Windows Terminal when wt.exe is on PATH,
// otherwise a plain cmd.exe window. The candidate preference list is
// not wired on Windows yet — it only ever had these two.
func resolveTerminalLauncher(_ []string) (string, launcher, error) {
	if path, err := exec.LookPath("wt.exe"); err == nil {
		return "windows-terminal", func(command string) error {
			return exec.Command(path, "new-tab", "cmd", "/k", command).Start()
		}, nil
	}
	return "cmd.exe", func(command string) error {
		return exec.Command("cmd", "/c", "start", "cmd", "/k", command).Start()
	}, nil
}
