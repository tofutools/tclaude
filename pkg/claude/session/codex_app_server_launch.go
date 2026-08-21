package session

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/codexappserver"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	tclcommon "github.com/tofutools/tclaude/pkg/common"
)

const codexAppServerVersionProbeTimeout = 5 * time.Second

var codexAppServerVersionProbe = func(executable, cwd string, environment map[string]string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), codexAppServerVersionProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "--version")
	cmd.Dir = cwd
	cmd.Env = sortedEnvironment(environment)
	return cmd.CombinedOutput()
}

func codexAppServerPrivateWriteDir(params *NewParams) (*TclaudeLayerPrivateWriteDir, error) {
	if params == nil || !params.CodexAppServer {
		return nil, nil
	}
	generationDir := filepath.Clean(filepath.Dir(params.CodexAppServerSocket))
	ownerDir := filepath.Dir(generationDir)
	root := filepath.Join(tclcommon.TclaudeAPIDir(), "codex")
	if filepath.Dir(ownerDir) != root || filepath.Base(generationDir) != params.CodexAppServerGeneration ||
		!hexComponent(filepath.Base(ownerDir)) || !hexComponent(filepath.Base(generationDir)) {
		return nil, fmt.Errorf("invalid codex app-server private generation directory %q", generationDir)
	}
	for _, runtimePath := range []struct {
		path, basename string
	}{
		{params.CodexAppServerSocket, "app.sock"},
		{params.CodexAppServerPIDFile, "server.pid"},
		{params.CodexAppServerLogFile, "server.log"},
		{params.CodexAppServerTokenHandoff, "tui-capability.handoff"},
	} {
		path, basename := runtimePath.path, runtimePath.basename
		if filepath.Dir(filepath.Clean(path)) != generationDir || filepath.Base(path) != basename {
			return nil, fmt.Errorf("invalid codex app-server runtime path %q", path)
		}
	}
	endpoint, err := url.Parse(params.CodexAppServerURL)
	if err != nil || endpoint.Scheme != "ws" || endpoint.Hostname() != "127.0.0.1" ||
		endpoint.User != nil || endpoint.Path != "" || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return nil, fmt.Errorf("invalid codex app-server authenticated loopback endpoint")
	}
	port, err := strconv.Atoi(endpoint.Port())
	if err != nil || port < 1 || port > 65535 {
		return nil, fmt.Errorf("invalid codex app-server authenticated loopback port")
	}
	digest, err := hex.DecodeString(params.CodexAppServerTokenSHA256)
	if err != nil || len(digest) != 32 {
		return nil, fmt.Errorf("invalid codex app-server capability digest")
	}
	for _, dir := range []string{ownerDir, generationDir} {
		info, err := os.Lstat(dir)
		if err != nil {
			return nil, fmt.Errorf("inspect codex app-server private directory %q: %w", dir, err)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
			return nil, fmt.Errorf("codex app-server private directory %q must be a real 0700 directory", dir)
		}
	}
	return &TclaudeLayerPrivateWriteDir{Parent: ownerDir, Current: generationDir}, nil
}

func codexAppServerLoopbackPort(params *NewParams) int {
	if params == nil || !params.CodexAppServer {
		return 0
	}
	endpoint, err := url.Parse(params.CodexAppServerURL)
	if err != nil {
		return 0
	}
	port, _ := strconv.Atoi(endpoint.Port())
	return port
}

func hexComponent(value string) bool {
	if len(value) != 16 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func verifyCodexAppServerLaunchVersion(
	params *NewParams,
	executable, cwd string,
	environment []sandboxpolicy.EnvironmentEntry,
) error {
	if params == nil || !params.CodexAppServer {
		return nil
	}
	launchEnvironment := launchModelEnvironment(environment)
	if strings.TrimSpace(executable) == "" {
		resolved, err := codexEffectiveConfigLookPath(cwd, launchEnvironment["PATH"])
		if err != nil {
			return markCodexAppServerVersionFailure(params,
				fmt.Errorf("locate exact codex launch executable: %w", err))
		}
		executable = resolved
	}
	output, err := codexAppServerVersionProbe(executable, cwd, launchEnvironment)
	if err != nil {
		return markCodexAppServerVersionFailure(params,
			fmt.Errorf("probe exact codex launch executable %q: %w", executable, err))
	}
	version := strings.TrimSpace(string(output))
	if err := codexappserver.CheckVersion(version); err != nil {
		return markCodexAppServerVersionFailure(params,
			fmt.Errorf("exact codex launch executable %q: %w", executable, err))
	}
	version = strings.TrimSpace(strings.TrimPrefix(version, "codex-cli "))
	if err := db.SetCodexAppServerRuntimeVersion(params.CodexAppServerGeneration, version); err != nil {
		return markCodexAppServerVersionFailure(params,
			fmt.Errorf("persist exact codex launch version: %w", err))
	}
	return nil
}

func markCodexAppServerVersionFailure(params *NewParams, cause error) error {
	if params != nil && params.CodexAppServerGeneration != "" {
		_ = db.MarkCodexAppServerRuntimeUnavailable(params.CodexAppServerGeneration, cause.Error())
	}
	return cause
}
