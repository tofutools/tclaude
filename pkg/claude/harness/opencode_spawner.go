package harness

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/opencodeapi"
)

type openCodeSpawner struct{}

func (openCodeSpawner) Binary() string { return "opencode" }

// BuildCommand returns only the attach client. The corresponding `serve`
// process is started and supervised by agentd; launching bare `opencode` here
// would create a competing second server with split live state.
func (openCodeSpawner) BuildCommand(spec SpawnSpec) string {
	binary := "opencode"
	if spec.ExecutablePath != "" {
		binary = clcommon.ShellQuoteArg(spec.ExecutablePath)
	}
	attachURL := spec.ServerURL
	cmdPrefix := spec.EnvExports
	if spec.OpenCodeTransport == db.OpenCodeTransportUnixRelay {
		cmdPrefix += clcommon.DetectAbsoluteCmd(
			opencodeapi.UnixAttachShimMode,
			strconv.Itoa(spec.OpenCodeServerPID),
			spec.OpenCodeControlSocketPath,
			strconv.FormatInt(spec.OpenCodeControlSocketDevice, 10),
			strconv.FormatInt(spec.OpenCodeControlSocketInode, 10),
			spec.ServerURL,
		) + " -- "
		attachURL = opencodeapi.AttachURLPlaceholder
	}
	cmd := cmdPrefix + binary + " attach " + clcommon.ShellQuoteArg(attachURL)
	if spec.Cwd != "" {
		cmd += " --dir " + clcommon.ShellQuoteArg(spec.Cwd)
	}
	sessionID := spec.SessionID
	if spec.ResumeID != "" {
		sessionID = spec.ResumeID
	}
	if sessionID != "" {
		cmd += " --session " + clcommon.ShellQuoteArg(sessionID)
	}
	if len(spec.ExtraArgs) > 0 {
		quoted := make([]string, len(spec.ExtraArgs))
		for i, arg := range spec.ExtraArgs {
			quoted[i] = clcommon.ShellQuoteArg(arg)
		}
		cmd += " " + strings.Join(quoted, " ")
	}
	return cmd
}

// OpenCodeExecutable resolves the normal PATH installation and OpenCode's
// default per-user install location. The latter matters for daemon launches
// whose sanitized PATH does not include ~/.opencode/bin.
func OpenCodeExecutable() (string, error) {
	if path, err := exec.LookPath("opencode"); err == nil {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(home, ".opencode", "bin", "opencode")
	if info, err := os.Stat(path); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
		return path, nil
	}
	return exec.LookPath("opencode")
}
