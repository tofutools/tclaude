//go:build linux || darwin

package agentd

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"golang.org/x/sys/unix"
)

const (
	openCodeInstallBootstrapFile = ".gitignore"
	openCodeFilteredConfigBase   = "filtered-config"
	openCodeFilteredHomeBase     = "filtered-home"
	// OpenCode Config.ensureGitignore writes this exact join-with-newlines
	// payload in both v1.18.5 (e5cc278d) and v1.18.6 (00ac24ee):
	// https://github.com/anomalyco/opencode/blob/v1.18.5/packages/opencode/src/config/config.ts#L295-L312
	// https://github.com/anomalyco/opencode/blob/v1.18.6/packages/opencode/src/config/config.ts#L295-L312
	//
	// This is the sole approved daemon-side compatibility payload for the
	// otherwise read-only install and config app directories. Any additional
	// OpenCode write-on-boot path must be separately reviewed; do not grow this
	// into a file list or overwrite an operator-owned file.
	openCodeInstallGitignore = "node_modules\npackage.json\npackage-lock.json\nbun.lock\n.gitignore"
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

// isolateOpenCodeFilteredConfig makes provider-empty per-agent XDG and HOME
// config bases, rather than ambient global config or ~/.opencode, the sources
// seen by the pinned server. The bases contain only OpenCode's required
// bootstrap .gitignore and are daemon-final self-bound read-only, so the
// executor cannot plant a provider source for a later reload or restart.
func isolateOpenCodeFilteredConfig(layout *openCodeStateLayout) error {
	if layout == nil || layout.allocation.Mode != db.OpenCodeStatePrivate {
		return fmt.Errorf("OpenCode filtered networking requires private state")
	}
	configBase := filepath.Join(layout.allocation.StateRoot, openCodeFilteredConfigBase)
	configApp := filepath.Join(configBase, "opencode")
	if err := os.MkdirAll(configApp, 0o700); err != nil {
		return fmt.Errorf("create OpenCode filtered config base: %w", err)
	}
	filteredHome := filepath.Join(
		layout.allocation.StateRoot, openCodeFilteredHomeBase)
	homeApp := filepath.Join(filteredHome, ".opencode")
	if err := os.MkdirAll(homeApp, 0o700); err != nil {
		return fmt.Errorf("create OpenCode filtered home: %w", err)
	}
	for _, source := range []struct {
		path, surface string
	}{
		{path: configApp, surface: "filtered XDG config"},
		{path: homeApp, surface: "filtered HOME config"},
	} {
		if _, err := ensureOpenCodeBootstrapGitignore(
			source.path, source.surface); err != nil {
			return err
		}
	}
	resolved, err := filepath.EvalSymlinks(configApp)
	if err != nil || resolved != configApp {
		return fmt.Errorf("OpenCode filtered config base %q is not canonical", configApp)
	}
	resolvedHome, err := filepath.EvalSymlinks(filteredHome)
	if err != nil || resolvedHome != filteredHome {
		return fmt.Errorf("OpenCode filtered home %q is not canonical", filteredHome)
	}
	found := false
	for index := range layout.environment {
		if layout.environment[index].Name != "XDG_CONFIG_HOME" {
			continue
		}
		layout.environment[index].Value = configBase
		found = true
	}
	if !found {
		return fmt.Errorf("OpenCode filtered private state has no XDG_CONFIG_HOME")
	}
	for _, path := range []string{configApp, homeApp} {
		layout.readOnlyBinds = append(layout.readOnlyBinds,
			session.TclaudeLayerReadOnlyBind{Source: path, Target: path})
	}
	return nil
}

// validateOpenCodeFilteredProviderSources proves that the two config roots
// selected by a filtered server still have the provider-empty shape frozen in
// its serialized launch contract. It runs immediately before every exec,
// including persisted-spec replay.
func validateOpenCodeFilteredProviderSources(stateRoot string) error {
	stateRoot = canonicalOpenCodeRuntimePath(stateRoot)
	if stateRoot == "" {
		return fmt.Errorf("OpenCode filtered provider state root is not canonical")
	}
	configBase := filepath.Join(stateRoot, openCodeFilteredConfigBase)
	filteredHome := filepath.Join(stateRoot, openCodeFilteredHomeBase)
	for _, expected := range []struct {
		path, surface string
	}{
		{
			path:    filepath.Join(configBase, "opencode"),
			surface: "XDG config directory",
		},
		{
			path:    filepath.Join(filteredHome, ".opencode"),
			surface: "HOME config directory",
		},
	} {
		if err := validateOpenCodeFilteredProviderDirectory(
			expected.path, expected.surface); err != nil {
			return err
		}
	}
	for _, path := range []string{
		filepath.Join(configBase, "opencode", openCodeInstallBootstrapFile),
		filepath.Join(filteredHome, ".opencode", openCodeInstallBootstrapFile),
	} {
		content, err := os.ReadFile(path)
		if err != nil || string(content) != openCodeInstallGitignore {
			return fmt.Errorf(
				"OpenCode filtered provider bootstrap %q is not the pinned provider-empty marker; clear the filtered config state or use network open",
				path)
		}
	}
	return nil
}

func validateOpenCodeFilteredProviderDirectory(
	path string,
	surface string,
) error {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() {
		return fmt.Errorf(
			"OpenCode filtered %s %q is not a canonical directory; clear the filtered config state or use network open",
			surface, path)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path {
		return fmt.Errorf(
			"OpenCode filtered %s %q is not canonical; clear the filtered config state or use network open",
			surface, path)
	}
	entries, err := os.ReadDir(path)
	if err != nil || len(entries) != 1 {
		return fmt.Errorf(
			"OpenCode filtered %s %q is not provider-empty; clear it or use network open",
			surface, path)
	}
	entry := entries[0]
	entryInfo, infoErr := os.Lstat(filepath.Join(path, entry.Name()))
	if entry.Name() != openCodeInstallBootstrapFile ||
		infoErr != nil || !entryInfo.Mode().IsRegular() {
		return fmt.Errorf(
			"OpenCode filtered %s %q contains unapproved provider authority; clear it or use network open",
			surface, path)
	}
	return nil
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

func openCodeControlSocketPath(agentID string) (string, error) {
	allocation, err := requireOpenCodeStateAllocation(agentID)
	if err != nil {
		return "", err
	}
	parent, err := openCodePrivateStateParent()
	if err != nil {
		return "", err
	}
	prospectiveParent, err := canonicalizeMissingOpenCodePath(parent)
	if err != nil {
		return "", fmt.Errorf("canonicalize OpenCode control parent: %w", err)
	}
	if err := refuseOpenCodeProtectedStateRoot(prospectiveParent); err != nil {
		return "", err
	}
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return "", fmt.Errorf("create OpenCode control parent: %w", err)
	}
	parent, err = filepath.EvalSymlinks(parent)
	if err != nil {
		return "", fmt.Errorf("resolve OpenCode control parent: %w", err)
	}
	parentInfo, err := os.Lstat(parent)
	if err != nil || parentInfo.Mode()&os.ModeSymlink != 0 || !parentInfo.IsDir() ||
		parentInfo.Mode().Perm() != 0o700 {
		return "", fmt.Errorf("OpenCode control parent is not a real mode-0700 directory")
	}
	if stat, ok := parentInfo.Sys().(*syscall.Stat_t); !ok ||
		stat.Uid != uint32(os.Geteuid()) {
		return "", fmt.Errorf("OpenCode control parent has the wrong owner")
	}
	controlRoot := allocation.StateRoot
	if allocation.Mode == db.OpenCodeStateLegacyShared {
		controlRoot = filepath.Join(parent, agentID)
		if err := os.Mkdir(controlRoot, 0o700); err != nil && !os.IsExist(err) {
			return "", fmt.Errorf("create legacy OpenCode control root: %w", err)
		}
	}
	info, err := os.Lstat(controlRoot)
	if err != nil {
		return "", fmt.Errorf("inspect OpenCode control root: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return "", fmt.Errorf("OpenCode control root %q is not a real mode-0700 directory", controlRoot)
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok ||
		stat.Uid != uint32(os.Geteuid()) {
		return "", fmt.Errorf("OpenCode control root has the wrong owner")
	}
	resolved, err := filepath.EvalSymlinks(controlRoot)
	if err != nil || resolved != controlRoot || filepath.Dir(controlRoot) != parent ||
		filepath.Base(controlRoot) != agentID {
		return "", fmt.Errorf("OpenCode control root is not the validated direct agent child")
	}
	if err := refuseOpenCodeProtectedStateRoot(controlRoot); err != nil {
		return "", err
	}
	return filepath.Join(controlRoot, "control.sock"), nil
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
				if path == layout.ambient.install {
					if err := ensureOpenCodeInstallGitignore(resolved); err != nil {
						return nil, err
					}
				}
				layout.readOnlyBinds = append(layout.readOnlyBinds,
					session.TclaudeLayerReadOnlyBind{Source: resolved, Target: resolved})
			} else if statErr != nil && !os.IsNotExist(statErr) {
				return nil, fmt.Errorf("inspect shared OpenCode path %q: %w", path, statErr)
			}
		}
		if err := adaptOpenCodeStateLayoutForPlatform(layout); err != nil {
			return nil, err
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
		if err := ensureOpenCodeInstallGitignore(install); err != nil {
			return nil, err
		}
		layout.readOnlyBinds = append(layout.readOnlyBinds, session.TclaudeLayerReadOnlyBind{
			Source: install, Target: install,
		})
	} else if statErr != nil && !os.IsNotExist(statErr) {
		return nil, fmt.Errorf("inspect OpenCode install: %w", statErr)
	}
	if err := adaptOpenCodeStateLayoutForPlatform(layout); err != nil {
		return nil, err
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
			return validateExistingOpenCodeCredential(destination)
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

func validateExistingOpenCodeCredential(path string) error {
	fd, err := unix.Open(
		path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("inspect existing private OpenCode credential %q: %w", path, err)
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return fmt.Errorf("inspect existing private OpenCode credential %q: %w", path, err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return fmt.Errorf("existing private OpenCode credential %q is not a regular file", path)
	}
	return nil
}

// prepareOpenCodeReadOnlyConfig supplies the one app-owned compatibility file
// OpenCode 1.18.6 writes before loading a config directory:
// https://github.com/anomalyco/opencode/blob/v1.18.6/packages/opencode/src/config/config.ts#L295-L312
//
// It runs after the durable state paths exist but before the sandbox starts.
// Only the config app directory actually named by the launch contract is
// eligible, and only when that directory is served by a daemon-final read-only
// bind — without the file, Config.loadInstanceState fails its own write and
// the first session creation answers HTTP 500 (EROFS on Linux).
//
// The file is created in the bind's SOURCE, which is what the sandbox actually
// sees. On Darwin the private-state layout adapter has already rewritten the
// config bind to same-path, so source and target coincide. On Linux the bind
// is {ambient config -> per-agent config} whenever an ambient
// ~/.config/opencode exists, so the source is the operator's real config
// directory. Seeding there is a host-visible side effect, acceptable only
// because it is create-if-absent (an operator-owned file is never rewritten,
// and a non-regular path is refused) and because the payload is byte-identical
// to what OpenCode itself writes on first run — see openCodeInstallGitignore.
// A seeded file therefore leaves no diff for OpenCode to undo.
//
// platform names the caller's OS for the refusal message only; the behavior is
// identical on both, so either path is exercisable from either host.
func prepareOpenCodeReadOnlyConfig(
	spec *session.TclaudeLayerLaunchSpec,
	platform string,
) error {
	if spec == nil || spec.Contract.HarnessName != harness.OpenCodeName ||
		len(spec.Contract.StateDirs) != 4 {
		return nil
	}
	configDir := filepath.Clean(spec.Contract.StateDirs[2])
	source := ""
	for _, bind := range spec.Contract.ReadOnlyBinds {
		if filepath.Clean(bind.Target) == configDir {
			source = filepath.Clean(bind.Source)
			break
		}
	}
	if source == "" {
		return nil
	}
	created, err := ensureOpenCodeBootstrapGitignore(source, "config")
	if err != nil {
		return fmt.Errorf(
			"opencode_read_only_config_bootstrap: refuse %s OpenCode launch because the read-only config prerequisite could not be established: %w",
			platform, err)
	}
	stateRoot := filepath.Clean(spec.Contract.StateRoot)
	if created && (!filepath.IsAbs(stateRoot) ||
		!sandboxpolicy.PathContainsOrEqual(stateRoot, source)) {
		slog.Info("created OpenCode bootstrap metadata in ambient host config before read-only confinement",
			"path", filepath.Join(source, openCodeInstallBootstrapFile))
	}
	return nil
}

func ensureOpenCodeInstallGitignore(installDir string) error {
	_, err := ensureOpenCodeBootstrapGitignore(installDir, "install")
	return err
}

func ensureOpenCodeBootstrapGitignore(dir, surface string) (bool, error) {
	path := filepath.Join(dir, openCodeInstallBootstrapFile)
	fd, err := unix.Open(path,
		unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		if err == unix.EEXIST {
			return false, validateExistingOpenCodeBootstrapGitignore(path, surface)
		}
		return false, fmt.Errorf("create OpenCode %s bootstrap %q: %w", surface, path, err)
	}
	file := os.NewFile(uintptr(fd), path)
	keep := false
	defer func() {
		_ = file.Close()
		if !keep {
			_ = os.Remove(path)
		}
	}()
	if err := unix.Fchmod(fd, 0o600); err != nil {
		return false, fmt.Errorf("secure OpenCode %s bootstrap %q: %w", surface, path, err)
	}
	if _, err := io.WriteString(file, openCodeInstallGitignore); err != nil {
		return false, fmt.Errorf("write OpenCode %s bootstrap %q: %w", surface, path, err)
	}
	if err := file.Sync(); err != nil {
		return false, fmt.Errorf("sync OpenCode %s bootstrap %q: %w", surface, path, err)
	}
	keep = true
	return true, nil
}

func validateExistingOpenCodeBootstrapGitignore(path, surface string) error {
	fd, err := unix.Open(
		path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("inspect existing OpenCode %s bootstrap %q: %w", surface, path, err)
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return fmt.Errorf("inspect existing OpenCode %s bootstrap %q: %w", surface, path, err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return fmt.Errorf("existing OpenCode %s bootstrap %q is not a regular file",
			surface, path)
	}
	return nil
}
