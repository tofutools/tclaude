//go:build linux || darwin

package agentd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"golang.org/x/sys/unix"
)

type openCodeStateLayout struct {
	allocation db.OpenCodeAgentStateAllocation
	parent     string
	stateDirs  []string
	ambient    struct {
		data, cache, config, state, install string
	}
	environment   []sandboxpolicy.EnvironmentEntry
	finalHideDirs []string
	readOnlyBinds []session.TclaudeLayerReadOnlyBind
}

func allocatePrivateOpenCodeState(agentID string) (*db.OpenCodeAgentStateAllocation, error) {
	if !openCodeAgentIDRE.MatchString(agentID) {
		return nil, fmt.Errorf("invalid OpenCode state agent id %q", agentID)
	}
	if existing, err := db.GetOpenCodeAgentStateAllocation(agentID); err != nil {
		return nil, err
	} else if existing != nil {
		return validateOpenCodeStateAllocation(*existing)
	}
	parent, err := openCodePrivateStateParent()
	if err != nil {
		return nil, err
	}
	prospectiveParent, err := canonicalizeMissingOpenCodePath(parent)
	if err != nil {
		return nil, fmt.Errorf("canonicalize OpenCode private state parent: %w", err)
	}
	if err := refuseOpenCodeProtectedStateRoot(prospectiveParent); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return nil, fmt.Errorf("create OpenCode private state parent: %w", err)
	}
	parent, err = filepath.EvalSymlinks(parent)
	if err != nil {
		return nil, fmt.Errorf("resolve OpenCode private state parent: %w", err)
	}
	if err := refuseOpenCodeProtectedStateRoot(parent); err != nil {
		return nil, err
	}
	root := filepath.Join(parent, agentID)
	if err := os.Mkdir(root, 0o700); err != nil && !os.IsExist(err) {
		return nil, fmt.Errorf("create OpenCode private state root: %w", err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("resolve OpenCode private state root: %w", err)
	}
	if filepath.Dir(root) != parent || filepath.Base(root) != agentID {
		return nil, fmt.Errorf("OpenCode private state root %q is not the validated direct agent child of %q",
			root, parent)
	}
	if err := refuseOpenCodeProtectedStateRoot(root); err != nil {
		return nil, err
	}
	allocation := db.OpenCodeAgentStateAllocation{
		AgentID: agentID, Mode: db.OpenCodeStatePrivate, StateRoot: root,
	}
	inserted, err := db.InsertOpenCodeAgentStateAllocation(allocation)
	if err != nil {
		return nil, err
	}
	if !inserted {
		existing, readErr := db.GetOpenCodeAgentStateAllocation(agentID)
		if readErr != nil {
			return nil, readErr
		}
		if existing == nil {
			return nil, fmt.Errorf("OpenCode state allocation for %s disappeared during allocation", agentID)
		}
		return validateOpenCodeStateAllocation(*existing)
	}
	return &allocation, nil
}

func requireOpenCodeStateAllocation(agentID string) (*db.OpenCodeAgentStateAllocation, error) {
	if !openCodeAgentIDRE.MatchString(agentID) {
		return nil, fmt.Errorf("invalid OpenCode state agent id %q", agentID)
	}
	allocation, err := db.GetOpenCodeAgentStateAllocation(agentID)
	if err != nil {
		return nil, err
	}
	if allocation == nil {
		return nil, fmt.Errorf(
			"OpenCode tclaude-layer agent %s has no durable state allocation; refusing shared-state fallback",
			agentID)
	}
	return validateOpenCodeStateAllocation(*allocation)
}

func validateOpenCodeStateAllocation(
	allocation db.OpenCodeAgentStateAllocation,
) (*db.OpenCodeAgentStateAllocation, error) {
	if !openCodeAgentIDRE.MatchString(allocation.AgentID) {
		return nil, fmt.Errorf("invalid OpenCode state allocation agent id %q", allocation.AgentID)
	}
	switch allocation.Mode {
	case db.OpenCodeStateLegacyShared:
		if allocation.StateRoot != "" {
			return nil, fmt.Errorf("legacy OpenCode state allocation for %s unexpectedly names root %q",
				allocation.AgentID, allocation.StateRoot)
		}
	case db.OpenCodeStatePrivate:
		root := filepath.Clean(strings.TrimSpace(allocation.StateRoot))
		if !filepath.IsAbs(root) || filepath.Base(root) != allocation.AgentID {
			return nil, fmt.Errorf("private OpenCode state allocation for %s has invalid root %q",
				allocation.AgentID, allocation.StateRoot)
		}
		info, err := os.Lstat(root)
		if err != nil {
			return nil, fmt.Errorf("private OpenCode state root %q: %w", root, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return nil, fmt.Errorf("private OpenCode state root %q is not a real directory", root)
		}
		resolved, err := filepath.EvalSymlinks(root)
		if err != nil || resolved != root {
			return nil, fmt.Errorf("private OpenCode state root %q no longer has its allocated identity",
				root)
		}
		if err := refuseOpenCodeProtectedStateRoot(root); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("OpenCode state allocation for %s has invalid mode %q",
			allocation.AgentID, allocation.Mode)
	}
	return &allocation, nil
}

func openCodeStateLayoutForAllocation(
	allocation db.OpenCodeAgentStateAllocation,
) (*openCodeStateLayout, error) {
	validated, err := validateOpenCodeStateAllocation(allocation)
	if err != nil {
		return nil, err
	}
	parent, err := openCodePrivateStateParent()
	if err != nil {
		return nil, err
	}
	if resolved, resolveErr := filepath.EvalSymlinks(parent); resolveErr == nil {
		parent = resolved
	}
	if validated.Mode == db.OpenCodeStatePrivate {
		parent = filepath.Dir(validated.StateRoot)
	}
	layout := &openCodeStateLayout{allocation: *validated, parent: parent}
	layout.ambient.data, err = openCodeAmbientAppDir("XDG_DATA_HOME", ".local/share")
	if err != nil {
		return nil, err
	}
	layout.ambient.cache, err = openCodeAmbientAppDir("XDG_CACHE_HOME", ".cache")
	if err != nil {
		return nil, err
	}
	layout.ambient.config, err = openCodeAmbientAppDir("XDG_CONFIG_HOME", ".config")
	if err != nil {
		return nil, err
	}
	layout.ambient.state, err = openCodeAmbientAppDir("XDG_STATE_HOME", ".local/state")
	if err != nil {
		return nil, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve home for OpenCode install: %w", err)
	}
	layout.ambient.install = filepath.Join(home, ".opencode")

	if validated.Mode == db.OpenCodeStateLegacyShared {
		layout.finalHideDirs = []string{parent}
		for _, path := range []string{layout.ambient.config, layout.ambient.install} {
			if info, statErr := os.Stat(path); statErr == nil && info.IsDir() {
				resolved, resolveErr := filepath.EvalSymlinks(path)
				if resolveErr != nil {
					return nil, fmt.Errorf("resolve shared OpenCode path %q: %w", path, resolveErr)
				}
				layout.readOnlyBinds = append(layout.readOnlyBinds,
					session.TclaudeLayerReadOnlyBind{Source: resolved, Target: resolved})
			} else if statErr != nil && !os.IsNotExist(statErr) {
				return nil, fmt.Errorf("inspect shared OpenCode path %q: %w", path, statErr)
			}
		}
		return layout, nil
	}

	baseNames := []string{"data", "cache", "config", "state"}
	layout.stateDirs = make([]string, 0, len(baseNames))
	for _, name := range baseNames {
		appDir := filepath.Join(validated.StateRoot, name, "opencode")
		if err := os.MkdirAll(appDir, 0o700); err != nil {
			return nil, fmt.Errorf("create private OpenCode %s directory: %w", name, err)
		}
		resolved, err := filepath.EvalSymlinks(appDir)
		if err != nil || resolved != appDir {
			return nil, fmt.Errorf("private OpenCode %s directory %q is not canonical", name, appDir)
		}
		layout.stateDirs = append(layout.stateDirs, appDir)
		layout.environment = append(layout.environment, sandboxpolicy.EnvironmentEntry{
			Name:  "XDG_" + strings.ToUpper(name) + "_HOME",
			Value: filepath.Join(validated.StateRoot, name),
		})
	}
	layout.finalHideDirs = []string{
		layout.ambient.data, layout.ambient.cache, layout.ambient.state,
	}
	configTarget := layout.stateDirs[2]
	configSource := configTarget
	if info, statErr := os.Stat(layout.ambient.config); statErr == nil && info.IsDir() {
		configSource, err = filepath.EvalSymlinks(layout.ambient.config)
		if err != nil {
			return nil, fmt.Errorf("resolve ambient OpenCode config: %w", err)
		}
	} else if statErr != nil && !os.IsNotExist(statErr) {
		return nil, fmt.Errorf("inspect ambient OpenCode config: %w", statErr)
	}
	if configSource != configTarget {
		layout.readOnlyBinds = append(layout.readOnlyBinds, session.TclaudeLayerReadOnlyBind{
			Source: configSource, Target: configSource,
		})
	}
	layout.readOnlyBinds = append(layout.readOnlyBinds, session.TclaudeLayerReadOnlyBind{
		Source: configSource, Target: configTarget,
	})
	if info, statErr := os.Stat(layout.ambient.install); statErr == nil && info.IsDir() {
		install, resolveErr := filepath.EvalSymlinks(layout.ambient.install)
		if resolveErr != nil {
			return nil, fmt.Errorf("resolve OpenCode install: %w", resolveErr)
		}
		layout.readOnlyBinds = append(layout.readOnlyBinds, session.TclaudeLayerReadOnlyBind{
			Source: install, Target: install,
		})
	} else if statErr != nil && !os.IsNotExist(statErr) {
		return nil, fmt.Errorf("inspect OpenCode install: %w", statErr)
	}
	if err := seedOpenCodeCredentials(layout.ambient.data, layout.stateDirs[0]); err != nil {
		return nil, err
	}
	return layout, nil
}

func openCodePrivateStateParent() (string, error) {
	base, err := openCodeXDGBase("XDG_DATA_HOME", ".local/share")
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "tclaude", "opencode-agents"), nil
}

func openCodeAmbientAppDir(envName, fallback string) (string, error) {
	base, err := openCodeXDGBase(envName, fallback)
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "opencode"), nil
}

func openCodeXDGBase(envName, fallback string) (string, error) {
	if value := strings.TrimSpace(os.Getenv(envName)); filepath.IsAbs(value) {
		return filepath.Clean(value), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home for %s: %w", envName, err)
	}
	return filepath.Join(home, filepath.FromSlash(fallback)), nil
}

func refuseOpenCodeProtectedStateRoot(path string) error {
	protected, err := sandboxpolicy.ProtectedPaths()
	if err != nil {
		return fmt.Errorf("resolve protected paths for OpenCode state: %w", err)
	}
	for _, root := range protected {
		if sandboxpolicy.PathContainsOrEqual(root, path) {
			return fmt.Errorf("OpenCode private state path %q is under protected root %q", path, root)
		}
	}
	return nil
}

func canonicalizeMissingOpenCodePath(path string) (string, error) {
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("path %q is not absolute", path)
	}
	current := path
	var suffix []string
	for {
		_, err := os.Lstat(current)
		if err == nil {
			resolved, err := filepath.EvalSymlinks(current)
			if err != nil {
				return "", err
			}
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return filepath.Clean(resolved), nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}

func seedOpenCodeCredentials(sourceDir, destinationDir string) error {
	for _, name := range []string{"auth.json", "mcp-auth.json"} {
		if err := seedOpenCodeCredential(
			filepath.Join(sourceDir, name), filepath.Join(destinationDir, name)); err != nil {
			return err
		}
	}
	return nil
}

func seedOpenCodeCredential(source, destination string) error {
	sourceFD, err := unix.Open(source, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if err == unix.ENOENT {
			return nil
		}
		return fmt.Errorf("open ambient OpenCode credential %q: %w", source, err)
	}
	sourceFile := os.NewFile(uintptr(sourceFD), source)
	defer sourceFile.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(sourceFD, &stat); err != nil {
		return fmt.Errorf("inspect ambient OpenCode credential %q: %w", source, err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return fmt.Errorf("ambient OpenCode credential %q is not a regular file", source)
	}
	destinationFD, err := unix.Open(destination,
		unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		if err == unix.EEXIST {
			return nil
		}
		return fmt.Errorf("create private OpenCode credential %q: %w", destination, err)
	}
	destinationFile := os.NewFile(uintptr(destinationFD), destination)
	keep := false
	defer func() {
		_ = destinationFile.Close()
		if !keep {
			_ = os.Remove(destination)
		}
	}()
	if _, err := io.Copy(destinationFile, sourceFile); err != nil {
		return fmt.Errorf("seed private OpenCode credential %q: %w", destination, err)
	}
	if err := destinationFile.Sync(); err != nil {
		return fmt.Errorf("sync private OpenCode credential %q: %w", destination, err)
	}
	keep = true
	return nil
}
