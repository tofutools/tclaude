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
	"github.com/tofutools/tclaude/pkg/claude/probehelper"
)

const (
	nestedSandboxIdentityTimeout = 3 * time.Second
	nestedProbeEnvPath           = "/usr/bin/env"
	nestedProbeSystemPath        = "/usr/bin:/bin"
)

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
	// EnginePresence answers the weaker, fork-free question "is this engine
	// INSTALLED" for DISCLOSURE surfaces only. ResolveExecutable execs the
	// engine (`<engine> --version`) to freeze its identity; a 2-second
	// dashboard poll cannot afford that, and does not need it to tell an
	// operator the engine is missing.
	//
	// nil means installed, NEVER working. It is the "dependency check" the
	// contract note above forbids substituting for ResolveExecutable — the ban
	// is on the LAUNCH path, which must keep running the real round-trip.
	EnginePresence() error
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
	Command         string
	KnownPaths      []string
	ClassifyFailure func(string) string
	Cleanup         func()
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
	if err == nil {
		return fallback, ""
	}
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

func (claudeNestedSandbox) EnginePresence() error {
	return nestedEnginePresence("claude", "stacked_claude_srt_probe")
}

func (claudeNestedSandbox) PrepareLaunch(spec SpawnSpec) SpawnSpec {
	spec.HarnessBuiltinMode = ClaudeSandboxOn
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
				"allowRead": []string{probehelper.BoundPath},
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
	secretBytes := make([]byte, 24)
	if _, err := rand.Read(secretBytes); err != nil {
		cleanup()
		return NestedSandboxProbe{}, fmt.Errorf("create Claude SRT probe endpoint secret: %w", err)
	}
	secret := hex.EncodeToString(secretBytes)
	readyPath := filepath.Join(root, probehelper.EndpointFileName)
	innerPolicyPath := filepath.Join(root, probehelper.InnerPolicyFileName)
	resultPath := filepath.Join(root, "result.json")
	configDir := filepath.Join(root, "config")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		cleanup()
		return NestedSandboxProbe{}, fmt.Errorf("prepare Claude SRT probe config: %w", err)
	}
	allowed := filepath.Join(workspace, "allowed")
	denied := filepath.Join(sibling, "denied")
	marker := "TCLAUDE_STACKED_INNER_OK_" + secret
	frozenPath := "PATH=" + clcommon.ShellQuoteArg(nestedProbeSystemPath) +
		"; export PATH"
	script := frozenPath + "; set -eu; touch " + clcommon.ShellQuoteArg(allowed) +
		"; if touch " + clcommon.ShellQuoteArg(denied) +
		"; then echo 'inner deny unexpectedly writable' >&2; exit 91; fi" +
		"; " + claudeAFUnixSeccompProbeScript(probehelper.BoundPath) +
		"; echo " + clcommon.ShellQuoteArg(marker)
	startStub := frozenPath + "; " + nestedProbeEnvPath +
		" -i " + clcommon.ShellQuoteArg(probehelper.BoundPath) +
		" " + clcommon.ShellQuoteArg(probehelper.StubMode) +
		" " + clcommon.ShellQuoteArg(root) +
		" " + clcommon.ShellQuoteArg(secret) +
		" " + clcommon.ShellQuoteArg(script) +
		" " + clcommon.ShellQuoteArg(marker) +
		" & stub_pid=$!; trap 'kill \"$stub_pid\" 2>/dev/null || true' EXIT"
	waitStub := "; i=0; while [ ! -s " + clcommon.ShellQuoteArg(readyPath) +
		" ]; do if ! kill -0 \"$stub_pid\" 2>/dev/null; then" +
		" wait \"$stub_pid\"; stub_status=$?" +
		"; if [ \"$stub_status\" -eq 126 ] || [ \"$stub_status\" -eq 127 ]; then" +
		" echo " + clcommon.ShellQuoteArg(probehelper.BindingFailureMarker) + " >&2; exit 97" +
		"; fi; echo 'launch-owned Messages stub failed before readiness' >&2; exit 94; fi" +
		"; i=$((i+1)); [ \"$i\" -le 100 ] || " +
		"{ echo 'launch-owned Messages stub readiness timed out' >&2; exit 94; }" +
		"; sleep 0.05; done" +
		"; endpoint=$(cat " + clcommon.ShellQuoteArg(readyPath) + ")" +
		"; case \"$endpoint\" in http://127.0.0.1:*) ;; *) exit 95 ;; esac"
	runClaude := "; " + nestedProbeEnvPath + " -i" +
		" PATH=" + clcommon.ShellQuoteArg(nestedProbeSystemPath) +
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
			executable.Path, settingsPath, configDir, workspace, sibling,
		},
		ClassifyFailure: func(output string) string {
			if strings.Contains(output, probehelper.BindingFailureMarker) {
				return "stacked_claude_probe_helper"
			}
			evidence, err := os.ReadFile(innerPolicyPath)
			if err == nil && string(evidence) == probehelper.InnerPolicyFailureValue {
				return "stacked_claude_inner_policy"
			}
			return ""
		},
		Cleanup: cleanup,
	}, nil
}

func claudeAFUnixSeccompProbeScript(helperPath string) string {
	return "if " + nestedProbeEnvPath + " -i " + clcommon.ShellQuoteArg(helperPath) +
		" " + clcommon.ShellQuoteArg(probehelper.AFUnixMode) +
		"; then echo 'SRT seccomp unexpectedly allowed AF_UNIX' >&2; exit 92" +
		"; else socket_status=$?; [ \"$socket_status\" -eq 77 ] || " +
		"{ echo 'SRT seccomp AF_UNIX probe was untestable' >&2; exit 96; }; fi"
}

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

func (codexNestedSandbox) EnginePresence() error {
	return nestedEnginePresence("codex", "stacked_codex_bwrap_backend")
}

func (codexNestedSandbox) PrepareLaunch(spec SpawnSpec) SpawnSpec {
	spec.HarnessBuiltinMode = ""
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

// nestedEnginePresence is the fork-free prefix of resolveNestedExecutable: the
// platform gate and the PATH lookup, without inspectNestedExecutable's
// `--version` exec. Disclosure only — see NestedSandboxContract.EnginePresence.
func nestedEnginePresence(name, capability string) error {
	if runtime.GOOS != "linux" {
		return &NestedSandboxCapabilityError{
			Capability: "stacked_nested_seatbelt",
			Detail:     "a second harness sandbox cannot be applied inside tclaude's macOS Seatbelt profile",
		}
	}
	if _, err := exec.LookPath(name); err != nil {
		return &NestedSandboxCapabilityError{
			Capability: capability,
			Detail:     fmt.Sprintf("model-free %s entry point is not on PATH: %v", name, err),
		}
	}
	return nil
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
	launcherPath = filepath.Clean(launcherPath)
	packageRoot := filepath.Dir(filepath.Dir(launcherPath))
	scopeRoot := filepath.Dir(filepath.Dir(packageRoot))
	if filepath.Base(filepath.Dir(launcherPath)) != "bin" ||
		filepath.Base(packageRoot) != "codex" ||
		filepath.Base(filepath.Dir(packageRoot)) != "@openai" ||
		filepath.Base(scopeRoot) != "node_modules" {
		return "", fmt.Errorf(
			"codex launcher %q is outside a recognized node_modules/@openai/codex package",
			launcherPath,
		)
	}
	candidates := []string{
		filepath.Join(
			packageRoot,
			"node_modules",
			packageParts[0],
			packageParts[1],
			"vendor",
			targetTriple,
			"bin",
			"codex",
		),
		filepath.Join(
			scopeRoot,
			packageParts[0],
			packageParts[1],
			"vendor",
			targetTriple,
			"bin",
			"codex",
		),
	}
	for _, candidate := range candidates {
		if evaluated, evalErr := filepath.EvalSymlinks(candidate); evalErr == nil {
			relative, relErr := filepath.Rel(scopeRoot, evaluated)
			insideScope := relErr == nil && relative != ".." &&
				!strings.HasPrefix(relative, ".."+string(filepath.Separator))
			if info, statErr := os.Stat(evaluated); insideScope &&
				statErr == nil && info.Mode().IsRegular() {
				return evaluated, nil
			}
		}
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
