package harness

import (
	"context"
	"encoding/json"
	"fmt"
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
	Path     string
	Version  string
	fileInfo os.FileInfo
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
	if AsNestedSandboxCapabilityError(err, &capabilityErr) {
		return capabilityErr.Capability, capabilityErr.Detail
	}
	return fallback, err.Error()
}

// AsNestedSandboxCapabilityError is kept as a small wrapper so callers do not
// need to import errors merely to format a refusal.
func AsNestedSandboxCapabilityError(err error, target **NestedSandboxCapabilityError) bool {
	for err != nil {
		if typed, ok := err.(*NestedSandboxCapabilityError); ok {
			*target = typed
			return true
		}
		type unwrapper interface{ Unwrap() error }
		next, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = next.Unwrap()
	}
	return false
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
	return resolveNestedExecutable(ctx, "srt", "stacked_claude_srt_probe")
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
	settingsPath := filepath.Join(root, "settings.json")
	settings := map[string]any{
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
				root,
			},
			"denyWrite": []string{"/etc"},
		},
		"enableWeakerNestedSandbox": false,
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
	allowed := filepath.Join(root, "allowed")
	denied := "/etc/.tclaude-stacked-probe-denied"
	script := "set -eu; touch " + clcommon.ShellQuoteArg(allowed) +
		"; if touch " + clcommon.ShellQuoteArg(denied) +
		"; then echo 'inner deny unexpectedly writable' >&2; exit 91; fi" +
		"; if python3 -c " + clcommon.ShellQuoteArg("import socket; socket.socket(socket.AF_UNIX)") +
		"; then echo 'SRT seccomp unexpectedly allowed AF_UNIX' >&2; exit 92; fi"
	return NestedSandboxProbe{
		Command: clcommon.ShellQuoteArg(executable.Path) +
			" --settings " + clcommon.ShellQuoteArg(settingsPath) +
			" -c " + clcommon.ShellQuoteArg(script),
		KnownPaths: []string{executable.Path, settingsPath, root},
		Cleanup:    cleanup,
	}, nil
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
	return resolveNestedExecutable(ctx, "codex", "stacked_codex_bwrap_backend")
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
	cleanup := func() { _ = os.RemoveAll(root) }
	config := `default_permissions = "stacked-probe"

[features]
use_legacy_landlock = false

[permissions.stacked-probe]
extends = ":workspace"

[permissions.stacked-probe.filesystem]
"/etc" = "none"

[permissions.stacked-probe.network]
enabled = false
`
	configPath := filepath.Join(root, "config.toml")
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		cleanup()
		return NestedSandboxProbe{}, fmt.Errorf("write Codex probe profile: %w", err)
	}
	allowed := filepath.Join(root, "allowed")
	denied := "/etc/.tclaude-stacked-probe-denied"
	script := "set -eu; touch " + clcommon.ShellQuoteArg(allowed) +
		"; if touch " + clcommon.ShellQuoteArg(denied) +
		"; then echo 'inner deny unexpectedly writable' >&2; exit 93; fi"
	command := "CODEX_HOME=" + clcommon.ShellQuoteArg(root) + " " +
		clcommon.ShellQuoteArg(executable.Path) +
		" sandbox -P stacked-probe -C " + clcommon.ShellQuoteArg(root) +
		" -- /bin/sh -c " + clcommon.ShellQuoteArg(script)
	return NestedSandboxProbe{
		Command:    command,
		KnownPaths: []string{executable.Path, configPath, root},
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
	path, err = filepath.Abs(path)
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

func boundedNestedOutput(output []byte) string {
	const limit = 2048
	text := strings.TrimSpace(string(output))
	if len(text) > limit {
		return text[:limit] + "…"
	}
	return text
}
