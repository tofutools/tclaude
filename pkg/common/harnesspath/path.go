// Package harnesspath shares managed harness discovery across every CLI,
// daemon and pane launch without running an installer.
package harnesspath

import (
	"os"
	"os/exec"
	"path/filepath"
)

func UserPrefix(home string) string {
	return filepath.Join(home, ".local", "share", "tclaude", "harnesses", "npm")
}
func BinaryDir(home string) string { return filepath.Join(UserPrefix(home), "bin") }
func Enable(home string) error {
	dir := BinaryDir(home)
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return nil
	}
	for _, p := range filepath.SplitList(os.Getenv("PATH")) {
		if p == dir {
			return nil
		}
	}
	return os.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// ManagedExecutable pins only a managed binary already selected by PATH. This
// keeps a bootstrap/login shell from selecting an older ambient installation.
func ManagedExecutable(name string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	home, err = filepath.EvalSymlinks(home)
	if err != nil {
		return ""
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return ""
	}
	if filepath.Clean(path) != filepath.Join(BinaryDir(home), name) {
		return ""
	}
	return path
}
