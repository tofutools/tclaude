package session

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// resolveLaunchRouteHelper validates a launch's group-route helper request
// and builds the helper the pane runs, or returns nil when the launch asks
// for none. The caller adopts the helper's launch generation as the exit
// generation.
func resolveLaunchRouteHelper(params *NewParams, harnessName string, outerLayer bool) (*TclaudeLayerRouteHelper, error) {
	if params.RouteHelperAgentID != "" || params.RouteHelperConvID != "" || params.RouteHelperLaunchGeneration != "" || params.RouteHelperCredentialHandoffSocketPath != "" || len(params.RouteHelperGroupIDs) > 0 {
		if !outerLayer || !tclaudeLayerWrapsPane(harnessName) {
			return nil, fmt.Errorf("linux group-route helper requires a pane-authoritative launch with tclaude’s sandbox")
		}
		if params.RouteHelperProxyOnly && runtime.GOOS != "darwin" {
			return nil, fmt.Errorf("darwin route proxy authority requires macOS")
		}
		if !params.RouteHelperProxyOnly && runtime.GOOS != "linux" {
			return nil, fmt.Errorf("linux group-route helper requires Linux")
		}
		if strings.TrimSpace(params.RouteHelperAgentID) == "" || strings.TrimSpace(params.RouteHelperLaunchGeneration) == "" || strings.TrimSpace(params.RouteHelperCredentialHandoffSocketPath) == "" || len(params.RouteHelperGroupIDs) == 0 {
			return nil, fmt.Errorf("linux group-route helper launch identity is incomplete")
		}
		socketFloor := sandboxpolicy.AgentdSocketFloor()
		if len(socketFloor) == 0 || strings.TrimSpace(socketFloor[0]) == "" {
			return nil, fmt.Errorf("linux group-route helper agentd socket is unavailable")
		}
		helperBinary, helperBinaryErr := os.Executable()
		if helperBinaryErr != nil || strings.TrimSpace(helperBinary) == "" {
			if helperBinaryErr == nil {
				helperBinaryErr = errors.New("executable path is empty")
			}
			return nil, fmt.Errorf("linux group-route helper executable is unavailable: %w", helperBinaryErr)
		}
		return &TclaudeLayerRouteHelper{
			BinaryPath:        filepath.Clean(helperBinary),
			SocketPath:        socketFloor[0],
			AgentID:           strings.TrimSpace(params.RouteHelperAgentID),
			ConvID:            strings.TrimSpace(params.RouteHelperConvID),
			LaunchGeneration:  strings.TrimSpace(params.RouteHelperLaunchGeneration),
			HandoffSocketPath: strings.TrimSpace(params.RouteHelperCredentialHandoffSocketPath),
			GroupIDs:          append([]int64(nil), params.RouteHelperGroupIDs...),
			ProxyOnly:         params.RouteHelperProxyOnly,
		}, nil
	}
	return nil, nil
}

// baseLaunchEnv builds the pane environment every launch starts from: the
// session-row and exit-generation keys, the managed-agent hint, the sandbox
// profile's environment, the hook broker for a launch whose hooks run behind
// a wall, and Gemini Seatbelt's trust environment. The agentd socket and
// launch-posture environment are layered on by the caller once the sandbox
// root is known.
func baseLaunchEnv(
	params *NewParams,
	h *harness.Harness,
	harnessBuiltinMode string,
	effectiveSandbox *sandboxpolicy.Snapshot,
	outerLayer bool,
	sessionID, exitGeneration, cwd string,
) (map[string]string, error) {
	additionalEnv := map[string]string{
		"TCLAUDE_SESSION_ID":      sessionID,
		"TCLAUDE_EXIT_GENERATION": exitGeneration,
	}
	applyManagedAgentHint(params.ManagedLaunch, additionalEnv)
	if effectiveSandbox != nil {
		for _, entry := range sandboxpolicy.EnvironmentForLaunch(effectiveSandbox) {
			additionalEnv[entry.Name] = entry.Value
		}
	}
	if outerLayer || harness.HooksRunInsideBuiltinSandbox(h, harnessBuiltinMode) {
		// Hook callbacks write private SQLite state, which this launch's own
		// namespace hides (or, for Gemini's Seatbelt mode, its profile makes
		// read-only). Route them through agentd instead, which applies
		// them host-side on the caller's behalf (TCL-754). Apply after the
		// profile environment so policy cannot unset the marker for an
		// outer-layer launch and silently send the pane's telemetry into the
		// throwaway database behind the wall.
		additionalEnv[HookBrokerEnvVar] = HookBrokerAgentd
	}
	if err := ApplyGeminiSeatbeltLaunchEnv(h, harnessBuiltinMode, effectiveSandbox, cwd, params.TrustDir, additionalEnv); err != nil {
		return nil, err
	}
	return additionalEnv, nil
}

// openCodeLaunchServer is the agentd-managed OpenCode server an
// authoritative-server launch attaches to.
type openCodeLaunchServer struct {
	url                 string
	transport           string
	controlSocketPath   string
	controlSocketDevice int64
	controlSocketInode  int64
	pid                 int
}

// resolveOpenCodeLaunchServer reads and validates the internal server
// authority agentd hands an OpenCode launch through the environment.
func resolveOpenCodeLaunchServer() (openCodeLaunchServer, error) {
	var srv openCodeLaunchServer
	srv.url = strings.TrimSpace(os.Getenv("TCLAUDE_OPENCODE_SERVER_URL"))
	if srv.url == "" || os.Getenv("OPENCODE_SERVER_PASSWORD") == "" {
		return openCodeLaunchServer{}, fmt.Errorf("OpenCode requires an authenticated agentd-managed server; launch it through `tclaude agent spawn`")
	}
	parsed, parseErr := url.Parse(srv.url)
	if parseErr != nil || parsed.Scheme != "http" || parsed.Hostname() != "127.0.0.1" || parsed.Port() == "" {
		return openCodeLaunchServer{}, fmt.Errorf("invalid internal OpenCode server URL")
	}
	srv.transport = strings.TrimSpace(os.Getenv(clcommon.OpenCodeTransportEnv))
	if srv.transport == "" {
		srv.transport = db.OpenCodeTransportLoopbackTCP
	}
	if srv.transport == db.OpenCodeTransportUnixRelay {
		srv.controlSocketPath = strings.TrimSpace(
			os.Getenv(clcommon.OpenCodeControlSocketPathEnv))
		srv.controlSocketDevice, parseErr = strconv.ParseInt(
			os.Getenv(clcommon.OpenCodeControlSocketDeviceEnv), 10, 64)
		if parseErr != nil {
			return openCodeLaunchServer{}, fmt.Errorf("invalid internal OpenCode control socket device")
		}
		srv.controlSocketInode, parseErr = strconv.ParseInt(
			os.Getenv(clcommon.OpenCodeControlSocketInodeEnv), 10, 64)
		if parseErr != nil {
			return openCodeLaunchServer{}, fmt.Errorf("invalid internal OpenCode control socket inode")
		}
		srv.pid, parseErr = strconv.Atoi(
			os.Getenv(clcommon.OpenCodeServerPIDEnv))
		if parseErr != nil {
			return openCodeLaunchServer{}, fmt.Errorf("invalid internal OpenCode server pid")
		}
		if err := db.ValidateOpenCodeRuntimeTransport(db.OpenCodeRuntime{
			Transport:           srv.transport,
			ControlSocketPath:   srv.controlSocketPath,
			ControlSocketDevice: srv.controlSocketDevice,
			ControlSocketInode:  srv.controlSocketInode,
		}); err != nil || srv.pid <= 1 {
			return openCodeLaunchServer{}, fmt.Errorf("invalid internal OpenCode Unix-relay authority")
		}
	} else if srv.transport != db.OpenCodeTransportLoopbackTCP {
		return openCodeLaunchServer{}, fmt.Errorf("invalid internal OpenCode transport")
	}
	return srv, nil
}

// ApplyGeminiSeatbeltLaunchEnv is the launch-boundary step for Gemini's own
// Seatbelt mode. That profile hides Gemini's OAuth credentials and every trust
// store from the sandboxed CLI (gemini_seatbelt.go), so the step refuses an
// auth type that cannot work in there and carries the host's trust decision
// in, as the environment Gemini honours ahead of the file, by adding it to
// env. It reads the launch's own composed environment. Any other launch is a
// no-op. `session new` and the conv resume paths both call it.
func ApplyGeminiSeatbeltLaunchEnv(
	h *harness.Harness,
	harnessBuiltinMode string,
	effectiveSandbox *sandboxpolicy.Snapshot,
	cwd string,
	trustDir bool,
	env map[string]string,
) error {
	if h.Name != harness.GeminiName || harnessBuiltinMode != harness.GeminiSandboxSeatbelt {
		return nil
	}
	// Gemini's Seatbelt profile hides its OAuth credentials and every
	// trust store from the sandboxed CLI (gemini_seatbelt.go): refuse an
	// auth type that cannot work in there, and carry the host's trust
	// decision in as the environment Gemini honours ahead of the file.
	seatbeltEnv := launchModelEnvironment(sandboxpolicy.EnvironmentForLaunch(effectiveSandbox))
	getenv := func(name string) string { return seatbeltEnv[name] }
	if err := harness.ValidateGeminiSeatbeltLaunch(getenv, cwd); err != nil {
		return err
	}
	trustEnv, err := harness.GeminiSeatbeltLaunchEnv(getenv, cwd, trustDir)
	if err != nil {
		slog.Warn("gemini seatbelt: cannot read the trust store; the sandboxed pane may ask for folder trust",
			"cwd", cwd, "err", err)
	}
	for name, value := range trustEnv {
		env[name] = value
	}
	return nil
}
