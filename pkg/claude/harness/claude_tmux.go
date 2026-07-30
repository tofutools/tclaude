package harness

import (
	"fmt"
	"path/filepath"
	"strings"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
)

const (
	claudeTmuxDarwinSandboxExec = "/usr/bin/sandbox-exec"
	claudeTmuxDarwinSocketParam = "TCLAUDE_TMUX_SOCKET"
	claudeTmuxDarwinProfile     = `(version 1)
(allow default)
(deny network-outbound
  (remote unix-socket (literal (param "` + claudeTmuxDarwinSocketParam + `"))))
`
)

// ClaudeTmuxSocketDenyPath resolves tclaude's named tmux server socket. The
// boundary is intentionally socket-specific: agents may run tmux against a
// private socket they own, but must not connect to the server hosting tclaude
// agent panes.
func ClaudeTmuxSocketDenyPath() (string, error) {
	dir, err := clcommon.TclaudeTmuxSocketDir()
	if err != nil {
		return "", fmt.Errorf("resolve Claude tmux socket deny path: %w", err)
	}
	return filepath.Join(dir, clcommon.TmuxSocketName), nil
}

// PrepareClaudeSandboxLaunch adds the tclaude tmux server socket to every
// tclaude-managed Claude sandbox launch. "inherit" still leaves the operator's
// broader Claude sandbox enabled/disabled choice alone. Linux activates the
// filesystem mask only with that inherited sandbox; macOS adds a narrow outer
// exact-socket connect deny because Seatbelt's file rules cannot enforce it.
// An explicit "off" is the visible escape hatch and emits no host-control
// settings or wrapper.
//
// Callers use this before BuildCommand / BuildAskArgv because resolving the
// host path can fail and those pure renderer interfaces deliberately do not
// return errors.
func PrepareClaudeSandboxLaunch(spec SpawnSpec) (SpawnSpec, error) {
	if strings.TrimSpace(spec.SandboxMode) == ClaudeSandboxOff {
		spec.TclaudeTmuxSocketPath = ""
		return spec, nil
	}
	path, err := ClaudeTmuxSocketDenyPath()
	if err != nil {
		return SpawnSpec{}, err
	}
	path = filepath.Clean(path)
	denies := append([]string(nil), spec.SandboxDenyDirs...)
	for _, existing := range denies {
		if filepath.Clean(strings.TrimSpace(existing)) == path {
			spec.SandboxDenyDirs = denies
			spec.TclaudeTmuxSocketPath = path
			return spec, nil
		}
	}
	spec.SandboxDenyDirs = append(denies, path)
	spec.TclaudeTmuxSocketPath = path
	return spec, nil
}

// claudeTmuxCommandPrefix supplies the macOS half of the socket boundary.
// Seatbelt checks connect(2) as network-outbound, independently of file-read;
// TestSeatbeltAssumptions/FileReadDenyDoesNotBlockUnixConnect pins that host
// behavior. A narrow outer profile therefore denies only tclaude's socket and
// still permits the tmux binary and agent-owned private socket servers.
//
// Linux needs no outer wrapper: Claude's sandbox runtime masks an existing
// denyRead file-like target (including a Unix socket) with /dev/null inside its
// bubblewrap namespace. Passing the socket as a sandbox-exec parameter keeps a
// host-controlled path out of the SBPL source and shell-quotes the whole
// -Dname=value argument at the command boundary.
func claudeTmuxCommandPrefix(spec SpawnSpec, goos string) string {
	if goos != "darwin" ||
		strings.TrimSpace(spec.SandboxMode) == ClaudeSandboxOff ||
		strings.TrimSpace(spec.TclaudeTmuxSocketPath) == "" {
		return ""
	}
	define := "-D" + claudeTmuxDarwinSocketParam + "=" + spec.TclaudeTmuxSocketPath
	return claudeTmuxDarwinSandboxExec + " -p " +
		clcommon.ShellQuoteArg(claudeTmuxDarwinProfile) + " " +
		clcommon.ShellQuoteArg(define) + " -- "
}

func claudeTmuxAskArgvPrefix(spec *SpawnSpec, goos string) []string {
	if spec == nil ||
		goos != "darwin" ||
		strings.TrimSpace(spec.SandboxMode) == ClaudeSandboxOff ||
		strings.TrimSpace(spec.TclaudeTmuxSocketPath) == "" {
		return nil
	}
	return []string{
		claudeTmuxDarwinSandboxExec,
		"-p", claudeTmuxDarwinProfile,
		"-D" + claudeTmuxDarwinSocketParam + "=" + spec.TclaudeTmuxSocketPath,
		"--",
	}
}
