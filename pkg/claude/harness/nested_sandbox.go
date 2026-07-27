package harness

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
)

const nestedSandboxIdentityTimeout = 3 * time.Second

// NestedSandboxContract is the descriptor-owned authority for a real harness
// sandbox that may run beneath tclaude's outer OS wall. It prepares both the
// actual harness launch and a model-free engine round-trip; callers must not
// replace the latter with a dependency or version check.
type NestedSandboxContract interface {
	CapabilityName() string
	MechanismName() string
	ResolveExecutable(context.Context) (NestedSandboxExecutable, error)
	PrepareLaunch(SpawnSpec) SpawnSpec
	PrepareProbe(string, NestedSandboxExecutable) (NestedSandboxProbe, error)
}

// NestedSandboxExecutable freezes the exact engine entry point and the identity
// disclosure captured before a live stacked probe.
type NestedSandboxExecutable struct {
	Path        string
	Version     string
	RuntimeRoot string
	fileInfo    os.FileInfo
}

// Identity is stable enough for the short-lived dashboard disclosure cache and
// deliberately includes the canonical path, version, size, and modification
// time. The launch still resolves and probes live.
func (executable NestedSandboxExecutable) Identity() string {
	if executable.fileInfo == nil {
		return strings.TrimSpace(executable.Path) + "|" + strings.TrimSpace(executable.Version)
	}
	return fmt.Sprintf("%s|%s|%d|%d",
		executable.Path, executable.Version, executable.fileInfo.Size(),
		executable.fileInfo.ModTime().UnixNano())
}

// Revalidate refuses a launch if the executable selected for the round-trip
// changed before the harness command was committed.
func (executable NestedSandboxExecutable) Revalidate() error {
	info, err := os.Stat(executable.Path)
	if err != nil {
		return fmt.Errorf("revalidate nested sandbox executable %q: %w", executable.Path, err)
	}
	if executable.fileInfo == nil || !os.SameFile(executable.fileInfo, info) ||
		executable.fileInfo.Size() != info.Size() ||
		!executable.fileInfo.ModTime().Equal(info.ModTime()) {
		return fmt.Errorf("nested sandbox executable %q changed after capability probe", executable.Path)
	}
	return nil
}

// NestedSandboxProbe is a model-free real-engine command and its temporary
// launch-owned state. Command is safe to pass to sh -c.
type NestedSandboxProbe struct {
	Command    string
	KnownPaths []string
	Cleanup    func()
}

// NestedSandboxCapabilityError names the exact missing capability used by the
// fail-closed stacked refusal contract.
type NestedSandboxCapabilityError struct {
	Capability string
	Detail     string
}

func (err *NestedSandboxCapabilityError) Error() string {
	if err == nil {
		return ""
	}
	return fmt.Sprintf("missing capability %s: %s", err.Capability, err.Detail)
}

// NestedSandboxCapability extracts a named capability from err, falling back to
// the descriptor's own capability name for ordinary execution errors.
func NestedSandboxCapability(err error, fallback string) (string, string) {
	var capabilityErr *NestedSandboxCapabilityError
	if errors.As(err, &capabilityErr) {
		return capabilityErr.Capability, capabilityErr.Detail
	}
	return fallback, err.Error()
}

type claudeNestedSandbox struct{}

func (claudeNestedSandbox) CapabilityName() string { return "stacked_claude_srt_probe" }
func (claudeNestedSandbox) MechanismName() string  { return "Claude SRT bwrap/seccomp" }

func (claudeNestedSandbox) ResolveExecutable(ctx context.Context) (NestedSandboxExecutable, error) {
	if runtime.GOOS != "linux" {
		return NestedSandboxExecutable{}, &NestedSandboxCapabilityError{
			Capability: "stacked_nested_seatbelt",
			Detail:     "macOS Seatbelt refuses applying a second harness sandbox inside tclaude's Seatbelt profile",
		}
	}
	// The launch authority is the exact Claude executable whose embedded SRT
	// runs the probe. A separately installed `srt` CLI is not representative:
	// Claude does not expose a supported switch that binds its tool runner to
	// that external package.
	return resolveNestedExecutable(ctx, "claude", "stacked_claude_srt_probe")
}

func (claudeNestedSandbox) PrepareLaunch(spec SpawnSpec) SpawnSpec {
	spec.SandboxMode = ClaudeSandboxOn
	spec.PermissionProfile = ""
	spec.StrongNestedSandbox = true
	return spec
}

func (claudeNestedSandbox) PrepareProbe(
	cwd string,
	executable NestedSandboxExecutable,
) (NestedSandboxProbe, error) {
	root, err := os.MkdirTemp(cwd, ".tclaude-stacked-srt-probe-")
	if err != nil {
		return NestedSandboxProbe{}, fmt.Errorf("prepare Claude SRT probe directory: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(root) }
	workspace := filepath.Join(root, "workspace")
	sibling := filepath.Join(root, "private")
	for _, dir := range []string{workspace, sibling} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			cleanup()
			return NestedSandboxProbe{}, fmt.Errorf("prepare Claude SRT probe shape: %w", err)
		}
	}
	settingsPath := filepath.Join(root, "settings.json")
	settings := map[string]any{
		"sandbox": map[string]any{
			"enabled":                   true,
			"failIfUnavailable":         true,
			"allowUnsandboxedCommands":  false,
			"enableWeakerNestedSandbox": false,
			"network": map[string]any{
				"allowedDomains":      []string{},
				"deniedDomains":       []string{"*"},
				"strictAllowlist":     true,
				"allowAllUnixSockets": false,
			},
			"filesystem": map[string]any{
				"denyRead":  []string{},
				"allowRead": []string{},
				"allowWrite": []string{
					workspace,
				},
				"denyWrite": []string{sibling},
			},
		},
		"permissions": map[string]any{
			"allow": []string{"Bash"},
		},
	}
	encoded, err := json.Marshal(settings)
	if err != nil {
		cleanup()
		return NestedSandboxProbe{}, err
	}
	if err := os.WriteFile(settingsPath, encoded, 0o600); err != nil {
		cleanup()
		return NestedSandboxProbe{}, fmt.Errorf("write Claude SRT probe settings: %w", err)
	}
	serverPath := filepath.Join(root, "stub.py")
	if err := os.WriteFile(serverPath, []byte(stackedClaudeProbeServer), 0o600); err != nil {
		cleanup()
		return NestedSandboxProbe{}, fmt.Errorf("write Claude SRT probe stub: %w", err)
	}
	secretBytes := make([]byte, 24)
	if _, err := rand.Read(secretBytes); err != nil {
		cleanup()
		return NestedSandboxProbe{}, fmt.Errorf("create Claude SRT probe endpoint secret: %w", err)
	}
	secret := hex.EncodeToString(secretBytes)
	readyPath := filepath.Join(root, "endpoint")
	resultPath := filepath.Join(root, "result.json")
	configDir := filepath.Join(root, "config")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		cleanup()
		return NestedSandboxProbe{}, fmt.Errorf("prepare Claude SRT probe config: %w", err)
	}
	allowed := filepath.Join(workspace, "allowed")
	denied := filepath.Join(sibling, "denied")
	marker := "TCLAUDE_STACKED_INNER_OK_" + secret
	script := "set -eu; touch " + clcommon.ShellQuoteArg(allowed) +
		"; if touch " + clcommon.ShellQuoteArg(denied) +
		"; then echo 'inner deny unexpectedly writable' >&2; exit 91; fi" +
		"; if python3 -c " + clcommon.ShellQuoteArg("import socket; socket.socket(socket.AF_UNIX)") +
		"; then echo 'SRT seccomp unexpectedly allowed AF_UNIX' >&2; exit 92; fi" +
		"; echo " + clcommon.ShellQuoteArg(marker)
	pathValue := strings.TrimSpace(os.Getenv("PATH"))
	if pathValue == "" {
		pathValue = "/usr/local/bin:/usr/bin:/bin"
	}
	startStub := "python3 " + clcommon.ShellQuoteArg(serverPath) +
		" " + clcommon.ShellQuoteArg(readyPath) +
		" " + clcommon.ShellQuoteArg(secret) +
		" " + clcommon.ShellQuoteArg(script) +
		" " + clcommon.ShellQuoteArg(marker) +
		" & stub_pid=$!; trap 'kill \"$stub_pid\" 2>/dev/null || true' EXIT"
	waitStub := "; i=0; while [ ! -s " + clcommon.ShellQuoteArg(readyPath) +
		" ]; do i=$((i+1)); [ \"$i\" -le 100 ] || exit 94; sleep 0.05; done" +
		"; endpoint=$(cat " + clcommon.ShellQuoteArg(readyPath) + ")" +
		"; case \"$endpoint\" in http://127.0.0.1:*) ;; *) exit 95 ;; esac"
	runClaude := "; env -i" +
		" PATH=" + clcommon.ShellQuoteArg(pathValue) +
		" HOME=" + clcommon.ShellQuoteArg(configDir) +
		" CLAUDE_CONFIG_DIR=" + clcommon.ShellQuoteArg(configDir) +
		" ANTHROPIC_BASE_URL=\"$endpoint/" + secret + "\"" +
		" ANTHROPIC_API_KEY=" + clcommon.ShellQuoteArg("tclaude-stacked-"+secret) +
		" CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1" +
		" " + clcommon.ShellQuoteArg(executable.Path) +
		" --bare --safe-mode --setting-sources '' --no-session-persistence" +
		" -p " + clcommon.ShellQuoteArg("Run the Bash command supplied by the local capability service.") +
		" --settings " + clcommon.ShellQuoteArg(settingsPath) +
		" --output-format json --max-turns 2" +
		" --model claude-sonnet-4-5-20250929" +
		" >" + clcommon.ShellQuoteArg(resultPath) +
		"; grep -F " + clcommon.ShellQuoteArg("TCLAUDE_STACKED_STUB_OK_"+secret) +
		" " + clcommon.ShellQuoteArg(resultPath) + " >/dev/null"
	return NestedSandboxProbe{
		Command: startStub + waitStub + runClaude,
		KnownPaths: []string{
			executable.Path, settingsPath, serverPath, configDir, workspace, sibling,
		},
		Cleanup: cleanup,
	}, nil
}

// stackedClaudeProbeServer is a launch-owned, credential-free Messages stub.
// Its per-launch path secret prevents an ambient local process from driving the
// probe, and it exists only until PrepareProbe's cleanup runs. The stub never
// consults a model: it deterministically asks the exact Claude CLI to execute
// one Bash tool and returns success only when the sandboxed tool result carries
// the launch nonce emitted after every posture assertion.
const stackedClaudeProbeServer = `import http.server
import json
import sys

ready_path, secret, command, marker = sys.argv[1:5]
success = "TCLAUDE_STACKED_STUB_OK_" + secret

class Handler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *args):
        pass

    def authorized(self):
        return self.path.startswith("/" + secret + "/")

    def do_HEAD(self):
        if not self.authorized():
            self.send_error(404)
            return
        self.send_response(200)
        self.send_header("content-length", "0")
        self.end_headers()

    def do_POST(self):
        if not self.authorized():
            self.send_error(404)
            return
        length = int(self.headers.get("content-length", "0"))
        request = json.loads(self.rfile.read(length))
        results = [
            block
            for message in request.get("messages", [])
            for block in (
                message.get("content", [])
                if isinstance(message.get("content"), list)
                else []
            )
            if isinstance(block, dict) and block.get("type") == "tool_result"
        ]
        if not results:
            content = [{
                "type": "tool_use",
                "id": "toolu_tclaude_stacked",
                "name": "Bash",
                "input": {"command": command},
            }]
            stop_reason = "tool_use"
        else:
            valid = any(
                not result.get("is_error", False)
                and marker in str(result.get("content", ""))
                for result in results
            )
            content = [{"type": "text", "text": success if valid else "probe refused"}]
            stop_reason = "end_turn"
        response = json.dumps({
            "id": "msg_tclaude_stacked",
            "type": "message",
            "role": "assistant",
            "model": "claude-sonnet-4-5-20250929",
            "content": content,
            "stop_reason": stop_reason,
            "stop_sequence": None,
            "usage": {"input_tokens": 1, "output_tokens": 1},
        }).encode()
        self.send_response(200)
        self.send_header("content-type", "application/json")
        self.send_header("content-length", str(len(response)))
        self.end_headers()
        self.wfile.write(response)

server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
with open(ready_path, "w") as ready:
    ready.write("http://127.0.0.1:" + str(server.server_port))
server.serve_forever()
`

type codexNestedSandbox struct{}

func (codexNestedSandbox) CapabilityName() string { return "stacked_codex_bwrap_backend" }
func (codexNestedSandbox) MechanismName() string  { return "Codex bwrap managed profile" }

func (codexNestedSandbox) ResolveExecutable(ctx context.Context) (NestedSandboxExecutable, error) {
	if runtime.GOOS != "linux" {
		return NestedSandboxExecutable{}, &NestedSandboxCapabilityError{
			Capability: "stacked_nested_seatbelt",
			Detail:     "macOS Seatbelt refuses applying Codex Seatbelt inside tclaude's Seatbelt profile",
		}
	}
	launcher, err := resolveNestedExecutable(ctx, "codex", "stacked_codex_bwrap_backend")
	if err != nil {
		return NestedSandboxExecutable{}, err
	}
	return resolveCodexNativeExecutable(ctx, launcher)
}

func (codexNestedSandbox) PrepareLaunch(spec SpawnSpec) SpawnSpec {
	spec.SandboxMode = ""
	if spec.PermissionProfile == "" {
		spec.PermissionProfile = CodexAgentProfile
	}
	spec.StrongNestedSandbox = true
	return spec
}

func (codexNestedSandbox) PrepareProbe(
	cwd string,
	executable NestedSandboxExecutable,
) (NestedSandboxProbe, error) {
	root, err := os.MkdirTemp(cwd, ".tclaude-stacked-codex-probe-")
	if err != nil {
		return NestedSandboxProbe{}, fmt.Errorf("prepare Codex probe directory: %w", err)
	}
	codexStateRoot, err := codexConfigDir()
	if err != nil {
		_ = os.RemoveAll(root)
		return NestedSandboxProbe{}, fmt.Errorf("resolve Codex probe state root: %w", err)
	}
	if err := os.MkdirAll(codexStateRoot, 0o700); err != nil {
		_ = os.RemoveAll(root)
		return NestedSandboxProbe{}, fmt.Errorf("prepare Codex probe state root: %w", err)
	}
	codexHome, err := os.MkdirTemp(codexStateRoot, ".tclaude-stacked-probe-")
	if err != nil {
		_ = os.RemoveAll(root)
		return NestedSandboxProbe{}, fmt.Errorf("prepare Codex probe home: %w", err)
	}
	cleanup := func() {
		_ = os.RemoveAll(root)
		_ = os.RemoveAll(codexHome)
	}
	workspace := filepath.Join(root, "workspace")
	sibling := filepath.Join(root, "private")
	for _, dir := range []string{workspace, sibling} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			cleanup()
			return NestedSandboxProbe{}, fmt.Errorf("prepare Codex probe shape: %w", err)
		}
	}
	config := fmt.Sprintf(`default_permissions = "stacked-probe"

[features]
use_legacy_landlock = false

[permissions.stacked-probe]
extends = ":workspace"

[permissions.stacked-probe.filesystem]
%q = "none"

[permissions.stacked-probe.network]
enabled = false
`, sibling)
	configPath := filepath.Join(codexHome, "config.toml")
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		cleanup()
		return NestedSandboxProbe{}, fmt.Errorf("write Codex probe profile: %w", err)
	}
	allowed := filepath.Join(workspace, "allowed")
	denied := filepath.Join(sibling, "denied")
	script := "set -eu; touch " + clcommon.ShellQuoteArg(allowed) +
		"; if touch " + clcommon.ShellQuoteArg(denied) +
		"; then echo 'inner deny unexpectedly writable' >&2; exit 93; fi"
	command := "CODEX_HOME=" + clcommon.ShellQuoteArg(codexHome) + " " +
		clcommon.ShellQuoteArg(executable.Path) +
		" sandbox -P stacked-probe -C " + clcommon.ShellQuoteArg(workspace) +
		" -- /bin/sh -c " + clcommon.ShellQuoteArg(script)
	return NestedSandboxProbe{
		Command:    command,
		KnownPaths: []string{executable.Path, configPath, workspace, sibling},
		Cleanup:    cleanup,
	}, nil
}

func resolveNestedExecutable(
	ctx context.Context,
	name, capability string,
) (NestedSandboxExecutable, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return NestedSandboxExecutable{}, &NestedSandboxCapabilityError{
			Capability: capability,
			Detail:     fmt.Sprintf("model-free %s entry point is not on PATH: %v", name, err),
		}
	}
	return inspectNestedExecutable(ctx, path, name, capability)
}

func inspectNestedExecutable(
	ctx context.Context,
	path, name, capability string,
) (NestedSandboxExecutable, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return NestedSandboxExecutable{}, &NestedSandboxCapabilityError{
			Capability: capability,
			Detail:     fmt.Sprintf("canonicalize %s entry point: %v", name, err),
		}
	}
	if evaluated, evalErr := filepath.EvalSymlinks(path); evalErr == nil {
		path = evaluated
	}
	info, err := os.Stat(path)
	if err != nil {
		return NestedSandboxExecutable{}, &NestedSandboxCapabilityError{
			Capability: capability,
			Detail:     fmt.Sprintf("inspect %s entry point: %v", name, err),
		}
	}
	if !info.Mode().IsRegular() {
		return NestedSandboxExecutable{}, &NestedSandboxCapabilityError{
			Capability: capability,
			Detail:     fmt.Sprintf("%s entry point %q is not a regular file", name, path),
		}
	}
	versionCtx, cancel := context.WithTimeout(ctx, nestedSandboxIdentityTimeout)
	defer cancel()
	output, err := exec.CommandContext(versionCtx, path, "--version").CombinedOutput()
	if err != nil {
		return NestedSandboxExecutable{}, &NestedSandboxCapabilityError{
			Capability: capability,
			Detail: fmt.Sprintf("%s --version failed: %v: %s",
				name, err, boundedNestedOutput(output)),
		}
	}
	return NestedSandboxExecutable{
		Path:     path,
		Version:  boundedNestedOutput(output),
		fileInfo: info,
	}, nil
}

func resolveCodexNativeExecutable(
	ctx context.Context,
	launcher NestedSandboxExecutable,
) (NestedSandboxExecutable, error) {
	const capability = "stacked_codex_bwrap_backend"
	file, err := os.Open(launcher.Path)
	if err != nil {
		return NestedSandboxExecutable{}, &NestedSandboxCapabilityError{
			Capability: capability,
			Detail:     fmt.Sprintf("open Codex entry point %q: %v", launcher.Path, err),
		}
	}
	var magic [4]byte
	_, readErr := io.ReadFull(file, magic[:])
	closeErr := file.Close()
	if readErr == nil && string(magic[:]) == "\x7fELF" {
		runtimeRoot, rootErr := codexNativeRuntimeRoot(launcher.Path)
		if rootErr != nil {
			return NestedSandboxExecutable{}, &NestedSandboxCapabilityError{
				Capability: capability,
				Detail:     rootErr.Error(),
			}
		}
		launcher.RuntimeRoot = runtimeRoot
		return launcher, nil
	}
	if closeErr != nil {
		return NestedSandboxExecutable{}, &NestedSandboxCapabilityError{
			Capability: capability,
			Detail:     fmt.Sprintf("close Codex entry point %q: %v", launcher.Path, closeErr),
		}
	}
	packageName, targetTriple, err := codexLinuxNativeTarget()
	if err != nil {
		return NestedSandboxExecutable{}, &NestedSandboxCapabilityError{
			Capability: capability,
			Detail:     err.Error(),
		}
	}
	nativePath, err := findCodexNPMNativeBackend(
		launcher.Path,
		packageName,
		targetTriple,
	)
	if err != nil {
		return NestedSandboxExecutable{}, &NestedSandboxCapabilityError{
			Capability: capability,
			Detail: fmt.Sprintf(
				"resolve the native backend behind Codex entry point %q: %v",
				launcher.Path,
				err,
			),
		}
	}
	native, err := inspectNestedExecutable(
		ctx,
		nativePath,
		"codex native backend",
		capability,
	)
	if err != nil {
		return NestedSandboxExecutable{}, err
	}
	runtimeRoot, err := codexNativeRuntimeRoot(native.Path)
	if err != nil {
		return NestedSandboxExecutable{}, &NestedSandboxCapabilityError{
			Capability: capability,
			Detail:     err.Error(),
		}
	}
	native.RuntimeRoot = runtimeRoot
	return native, nil
}

func codexLinuxNativeTarget() (string, string, error) {
	switch runtime.GOARCH {
	case "amd64":
		return "@openai/codex-linux-x64", "x86_64-unknown-linux-musl", nil
	case "arm64":
		return "@openai/codex-linux-arm64", "aarch64-unknown-linux-musl", nil
	default:
		return "", "", fmt.Errorf(
			"the official Codex npm launcher has no pinned Linux backend for architecture %s",
			runtime.GOARCH,
		)
	}
}

func findCodexNPMNativeBackend(
	launcherPath, packageName, targetTriple string,
) (string, error) {
	packageParts := strings.Split(packageName, "/")
	if len(packageParts) != 2 || packageParts[0] != "@openai" ||
		strings.TrimSpace(packageParts[1]) == "" {
		return "", fmt.Errorf("invalid Codex platform package %q", packageName)
	}
	current := filepath.Dir(filepath.Clean(launcherPath))
	for {
		candidate := filepath.Join(
			current,
			"node_modules",
			packageParts[0],
			packageParts[1],
			"vendor",
			targetTriple,
			"bin",
			"codex",
		)
		if evaluated, evalErr := filepath.EvalSymlinks(candidate); evalErr == nil {
			if info, statErr := os.Stat(evaluated); statErr == nil && info.Mode().IsRegular() {
				return evaluated, nil
			}
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	return "", fmt.Errorf("official npm platform package %s is missing", packageName)
}

func codexNativeRuntimeRoot(nativePath string) (string, error) {
	if filepath.Base(nativePath) != "codex" ||
		filepath.Base(filepath.Dir(nativePath)) != "bin" {
		return "", fmt.Errorf(
			"codex native backend %q has no recognized runtime layout",
			nativePath,
		)
	}
	root := filepath.Dir(filepath.Dir(nativePath))
	for _, relative := range []string{
		"codex-package.json",
		filepath.Join("codex-path", "rg"),
		filepath.Join("codex-resources", "bwrap"),
	} {
		path := filepath.Join(root, relative)
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			return "", fmt.Errorf(
				"codex native runtime closure is missing %q",
				path,
			)
		}
	}
	return root, nil
}

func boundedNestedOutput(output []byte) string {
	const limit = 2048
	text := strings.TrimSpace(string(output))
	if len(text) > limit {
		return text[:limit] + "…"
	}
	return text
}
