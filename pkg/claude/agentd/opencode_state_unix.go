//go:build linux || darwin

package agentd

import (
	"errors"
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

// errOpenCodeInvalidAgentID lets a caller recognize "that name is not an agent
// id" without applying openCodeAgentIDRE itself. A caller that re-derived the
// rule to phrase its own message would own a second copy of a refusal
// condition, and two copies can only ever drift apart; one predicate with two
// presentations cannot.
//
// A rule for whoever adds the next one, not something the compiler or a test
// can enforce: every producer of this sentence wraps it. A producer emitting
// the same operator-visible text as a bare literal renders identically while
// answering errors.Is with false, which is the same drift in a shape that is
// harder to see (TCL-911).
var errOpenCodeInvalidAgentID = errors.New("invalid OpenCode state agent id")

func allocatePrivateOpenCodeState(agentID string) (*db.OpenCodeAgentStateAllocation, error) {
	if !openCodeAgentIDRE.MatchString(agentID) {
		return nil, fmt.Errorf("%w %q", errOpenCodeInvalidAgentID, agentID)
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
		return nil, fmt.Errorf("%w %q", errOpenCodeInvalidAgentID, agentID)
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

// openCodeSeedWindowHookForTest runs at the exact instant between the seed's
// validation and its write. It exists so the check-then-use window TCL-908
// closes can be exercised deterministically: a test that raced a real attacker
// would be flaky in the direction that HIDES the defect, and an unexercised
// window is a claim rather than a property.
//
// Safety argument, stated here so an audit of this path does not have to
// re-derive it:
//
//   - It is nil in production. No non-test file assigns it; the only writer is
//     the test that arms it and restores nil in t.Cleanup.
//   - It takes no arguments, so it is told nothing about the launch.
//
// The nil-in-production point is the one carrying the guarantee, and it is the
// only one that does. Do NOT read the signature as a second guarantee: an ARMED
// hook is a closure with full package scope, so it can panic, and it can act on
// the filesystem — creating the bootstrap file itself would turn the openat
// below into EEXIST and divert the seed down its existing-file branch. "Returns
// nothing" therefore means it is told nothing and reports nothing, NOT that an
// armed hook cannot affect the outcome. In production it is never armed, which
// is why none of that is reachable.
//
// (An earlier version of this comment claimed the signature made it unable to
// abort or redirect the write. A cold reviewer refuted that by inspection. The
// correction is kept visible because a prescriptive safety comment that
// overstates its own argument is worse than a shorter true one.)
//
// It lives in production code rather than in a _test file because production
// code references it, and Go cannot resolve a non-test reference to a
// test-file declaration. The alternative was contorting the seed's shape to
// avoid the variable, which would trade a nil hook for a worse structure.
var openCodeSeedWindowHookForTest func()

// prepareOpenCodeReadOnlyConfig supplies the one app-owned compatibility file
// OpenCode 1.18.6 writes before loading a config directory:
// https://github.com/anomalyco/opencode/blob/v1.18.6/packages/opencode/src/config/config.ts#L295-L312
//
// It runs after the durable state paths exist but before the sandbox starts.
// Only the config app directory actually named by the launch contract is
// eligible — and, since the contract only names it and does not prove it, only
// when that directory is one this daemon can re-derive for itself (see
// validateOpenCodeReadOnlyConfigSeedSourceAt) — and only when it is served by a
// daemon-final read-only bind. Without the file, Config.loadInstanceState fails
// its own write and the first session creation answers HTTP 500 (EROFS on
// Linux).
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
// Because the write target comes from a bind SOURCE — which the launch
// contract's own validation does not constrain, it checks targets — the source
// is separately confined to the two directories the layouts emit before
// anything is created; see validateOpenCodeReadOnlyConfigSeedSourceAt. Neither
// of those two directories is taken on the contract's word: the contract names
// state directories, it does not prove them.
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
	configDir := canonicalOpenCodeRuntimePath(spec.Contract.StateDirs[2])
	source := openCodeReadOnlyConfigBindSource(spec.Contract)
	if configDir == "" || source == "" {
		return nil
	}
	// The descriptor is opened BEFORE validation and used for both steps, so
	// the object validated and the object written into are the same one. The
	// order matters: validating first and opening after would leave exactly the
	// window this closes (TCL-908).
	sourceFD, err := openOpenCodeBootstrapDirectory(source, "config")
	if err != nil {
		return fmt.Errorf(
			"opencode_read_only_config_bootstrap: refuse %s OpenCode launch because the read-only config prerequisite could not be established: %w",
			platform, err)
	}
	defer func() { _ = unix.Close(sourceFD) }()
	if err := validateOpenCodeReadOnlyConfigSeedSourceAt(
		sourceFD, source, configDir); err != nil {
		return fmt.Errorf(
			"opencode_read_only_config_bootstrap: refuse %s OpenCode launch because the read-only config prerequisite could not be established: %w",
			platform, err)
	}
	if openCodeSeedWindowHookForTest != nil {
		// The check-then-use window, made reachable on purpose. A test cannot
		// win a real race deterministically, and a claim that the window is
		// closed is worth no more than the test that fails when it is open.
		openCodeSeedWindowHookForTest()
	}
	created, err := ensureOpenCodeBootstrapGitignoreAt(sourceFD, source, "config")
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

// validateOpenCodeReadOnlyConfigSeedSourceAt constrains where the bootstrap may
// be written. The launch contract's own validation covers bind TARGETS, and
// the seed goes to a SOURCE, so without this a persisted spec replayed by
// runtime reconciliation could direct a daemon-side file creation anywhere.
// Only the two directories the layouts actually emit are accepted: the ambient
// OpenCode config app directory this host resolves for itself, and the config
// app directory of a per-agent state allocation this daemon owns.
//
// The contract's own config directory is deliberately NOT an acceptance
// criterion by itself (TCL-902). Every launch-contract branch lets persisted
// data name StateDirs[2]: the grandfathered branch of
// validateOpenCodeV3LaunchContract validates no state directories at all, and
// the private branch only ties StateDirs[2] to an equally contract-supplied
// XDG_CONFIG_HOME. A self-bound tampered spec would otherwise satisfy
// "source == the contract's config directory" for any directory on the host.
// Both answers below are re-derived here instead: the ambient one from the
// daemon's own environment, the per-agent one from the daemon's own private
// state parent plus its allocation store.
//
// The source side of every comparison is the OPEN DESCRIPTOR the caller will
// write through, not a path (TCL-908). Deciding on a path string and then
// re-walking it to do the work leaves a window in which the string can change
// meaning; deciding on the descriptor's kernel identity cannot, because the
// same descriptor is what openat operates on. source is carried for MESSAGES
// only — nothing here resolves it.
func validateOpenCodeReadOnlyConfigSeedSourceAt(
	sourceFD int,
	source, configDir string,
) error {
	identity, err := openCodeDirectoryIdentityOf(sourceFD, "config")
	if err != nil {
		return err
	}
	ambient, err := openCodeAmbientAppDir("XDG_CONFIG_HOME", ".config")
	if err != nil {
		return fmt.Errorf("resolve ambient OpenCode config for bootstrap: %w", err)
	}
	// Compared as DIRECTORIES rather than as strings: a host whose config base
	// or temp root reaches the same directory through a symlink (macOS
	// /var -> /private/var is the everyday case) would otherwise refuse a bind
	// the layout itself produced.
	if openCodeDirectoryIs(identity, ambient) {
		return nil
	}
	// The SECOND of the two acceptance routes. The ambient one is just above;
	// this comment used to claim there was exactly one, which a cold review
	// corrected.
	//
	// The two are not equally tight, and saying they were was the second thing
	// corrected here. The ambient route genuinely has no window: `ambient` is
	// derived from the daemon's own environment with no filesystem read, and
	// Stat-ed exactly once. This route validates a path and then Stats that
	// string, so the candidate side is resolved by the kernel a second time —
	// inherent to comparing a descriptor against a PATH, and unavoidable short
	// of opening the candidate too. What makes it safe is not the absence of a
	// second resolution but that both ends are daemon-derived and sit behind
	// the same DB-write precondition.
	//
	// An earlier shape read configDir twice — once to compare against the
	// descriptor, once to look the allocation up — which left the residual
	// window this closes: an intermediate component changing meaning between
	// those two reads proved the identity about one directory and the
	// allocation about another, and the write then landed in the first.
	// configDir is now read once on that path rather than twice. It does still
	// SHAPE the answer — the returned path is built from a local derived from
	// it — so the earlier wording here, that it "only ever selects which
	// allocation to ask about", was made wrong by the fix above and is
	// corrected rather than left standing. What carries the safety is the
	// equality check inside requireOpenCodeAllocatedConfigDir, which forces
	// that local to equal the allocation's own recorded root: selecting the
	// wrong allocation cannot buy anything, because the answer still has to be
	// the directory the descriptor is open on.
	allocated, allocErr := requireOpenCodeAllocatedConfigDir(configDir)
	if allocErr == nil && openCodeDirectoryIs(identity, allocated) {
		return nil
	}
	// Acceptance is settled above. Everything below only chooses WORDING, so a
	// path read here cannot affect the outcome — at worst it mis-words a
	// refusal that has already been decided.
	//
	// Both candidates are named, but the per-agent one leads when the source is
	// self-bound, because that is the shape the launch has. The ambient path
	// still has to appear — a legacy contract replayed after the daemon's
	// XDG_CONFIG_HOME changed also lands here, and the ambient mismatch is what
	// its operator must act on.
	if allocErr != nil && openCodeDirectoryIs(identity, configDir) {
		return fmt.Errorf(
			"read-only OpenCode config bind source %q is not an allocated per-agent config directory (%w), and does not resolve to this host's ambient OpenCode config %q",
			source, allocErr, ambient)
	}
	return fmt.Errorf(
		"read-only OpenCode config bind source %q is neither an allocated per-agent config directory nor this host's ambient OpenCode config %q",
		source, ambient)
}

// requireOpenCodeAllocatedConfigDir proves that a config app directory named by
// a launch contract is the one belonging to a private state allocation this
// daemon actually made, rather than a path a persisted spec merely asserts.
//
// It reuses requireOpenCodeStateAllocation — the same allocation authority the
// launch path itself consults — instead of restating what a legitimate
// per-agent config path looks like. The allocation store is the same durable
// database as the launch spec, so existence alone would prove little; the state
// root is therefore also required to be a direct child of the private state
// parent THIS daemon derives, the same anchor openCodeControlSocketPath applies
// to the same allocation.
func requireOpenCodeAllocatedConfigDir(configDir string) (string, error) {
	resolved := resolvedOpenCodeSeedPath(configDir)
	configBase := filepath.Dir(resolved)
	stateRoot := filepath.Dir(configBase)
	if filepath.Base(resolved) != "opencode" || filepath.Base(configBase) != "config" {
		return "", fmt.Errorf(
			"OpenCode config bootstrap target %q does not have the per-agent <state root>/config/opencode shape",
			configDir)
	}
	allocation, err := requireOpenCodeStateAllocation(filepath.Base(stateRoot))
	// Rendered differently, not decided differently: the agent-id rule stays
	// wholly inside requireOpenCodeStateAllocation. This only declines to quote
	// an operator's own directory name back at them as an "invalid agent id"
	// when their path merely happens to end in config/opencode.
	if errors.Is(err, errOpenCodeInvalidAgentID) {
		return "", fmt.Errorf(
			"OpenCode config bootstrap target %q names %q where a per-agent state root was expected",
			configDir, stateRoot)
	}
	if err != nil {
		return "", fmt.Errorf(
			"OpenCode config bootstrap target %q is not an allocated per-agent config directory: %w",
			configDir, err)
	}
	if allocation.Mode != db.OpenCodeStatePrivate ||
		resolvedOpenCodeSeedPath(allocation.StateRoot) != stateRoot {
		return "", fmt.Errorf(
			"OpenCode config bootstrap target %q does not belong to the %s state allocation of agent %s",
			configDir, allocation.Mode, allocation.AgentID)
	}
	parent, err := openCodePrivateStateParent()
	if err != nil {
		return "", fmt.Errorf("resolve OpenCode private state parent for bootstrap: %w", err)
	}
	// Both sides are compared in resolved form. The allocator records a parent it
	// has already resolved, while this derives one from the live environment, so
	// a symlinked home or XDG base makes the two disagree as strings while naming
	// the same directory.
	if filepath.Dir(stateRoot) != resolvedOpenCodeSeedPath(parent) {
		return "", fmt.Errorf(
			"OpenCode config bootstrap target %q is outside this daemon's private state parent %q; a changed XDG_DATA_HOME or HOME moves that parent away from an existing allocation",
			configDir, parent)
	}
	// Built from stateRoot — the local that was VALIDATED above — and never from
	// a fresh walk. WITHIN THIS FUNCTION'S OWN BODY each input is resolved
	// exactly once: configDir at the top, allocation.StateRoot in the equality
	// check, parent in the containment check. That is what closes the window
	// here, and the whole reason this returns a value instead of letting the
	// caller re-derive one.
	//
	// Scoped deliberately, because a stronger claim was made and refuted: the
	// allocation authority ALSO reads allocation.StateRoot, via
	// requireOpenCodeStateAllocation -> validateOpenCodeStateAllocation, which
	// Lstats it and EvalSymlinks it before this function's own equality check
	// resolves it a third time. So "resolved exactly once" is false end to end.
	// Those reads establish a different property (the recorded root is a real,
	// non-symlink, unprotected directory) and live behind the same DB-write
	// precondition, but the honest statement is about this body, not the call
	// graph.
	//
	// An earlier version returned resolvedOpenCodeSeedPath(allocation.StateRoot)
	// here, reasoning that it was proven equal to stateRoot above and so was
	// merely presentation. That was wrong, and a cold review demonstrated it with
	// a working PoC: a second walk is a second read, "proven equal above" holds
	// only if resolution is deterministic across the two calls, and that is the
	// assumption a check-then-use argument may not make. It reopened the very
	// window the rest of this change closes.
	return filepath.Join(stateRoot, "config", "opencode"), nil
}

func resolvedOpenCodeSeedPath(path string) string {
	path = filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return filepath.Clean(resolved)
	}
	return path
}

func ensureOpenCodeInstallGitignore(installDir string) error {
	_, err := ensureOpenCodeBootstrapGitignore(installDir, "install")
	return err
}

// openOpenCodeBootstrapDirectory pins the directory the bootstrap will be
// written into, as a kernel object rather than as a path string.
//
// Every subsequent step operates through this descriptor with openat, so the
// directory the caller validated and the directory written into are the same
// object by construction. Re-walking the path instead — which is what this code
// used to do — leaves a check-then-use window in which an INTERMEDIATE
// component can change meaning; O_NOFOLLOW constrains only the final one
// (TCL-908).
func openOpenCodeBootstrapDirectory(dir, surface string) (int, error) {
	fd, err := unix.Open(dir,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("open OpenCode %s bootstrap directory %q: %w",
			surface, dir, err)
	}
	return fd, nil
}

// openCodeDirectoryIdentity is a directory's identity as the kernel reports it,
// which is what "the same directory" has to mean here: a path has more than one
// true spelling, and a spelling can be made to name a different object between
// two uses. A device/inode pair taken from an OPEN descriptor cannot.
type openCodeDirectoryIdentity struct {
	device uint64
	inode  uint64
}

func openCodeDirectoryIdentityOf(fd int, surface string) (openCodeDirectoryIdentity, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return openCodeDirectoryIdentity{}, fmt.Errorf(
			"inspect OpenCode %s bootstrap directory: %w", surface, err)
	}
	return openCodeDirectoryIdentity{
		device: uint64(stat.Dev), inode: uint64(stat.Ino),
	}, nil
}

// openCodeDirectoryIs answers whether an already-open directory IS the
// directory at path. Symlinks are followed on the path side deliberately: the
// caller's candidates are daemon-derived answers such as the ambient config app
// directory, which a host legitimately reaches through a symlink, and the
// comparison is against the kernel's identity rather than against a spelling.
func openCodeDirectoryIs(identity openCodeDirectoryIdentity, path string) bool {
	var stat unix.Stat_t
	if err := unix.Stat(path, &stat); err != nil {
		return false
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return false
	}
	return identity == openCodeDirectoryIdentity{
		device: uint64(stat.Dev), inode: uint64(stat.Ino),
	}
}

func ensureOpenCodeBootstrapGitignore(dir, surface string) (bool, error) {
	dirFD, err := openOpenCodeBootstrapDirectory(dir, surface)
	if err != nil {
		return false, err
	}
	defer func() { _ = unix.Close(dirFD) }()
	return ensureOpenCodeBootstrapGitignoreAt(dirFD, dir, surface)
}

// ensureOpenCodeBootstrapGitignoreAt writes the bootstrap file relative to an
// already-open directory. dir is carried for MESSAGES only — no step resolves
// it — so a caller that has validated a descriptor can be sure the write lands
// in the object it validated.
func ensureOpenCodeBootstrapGitignoreAt(dirFD int, dir, surface string) (bool, error) {
	path := filepath.Join(dir, openCodeInstallBootstrapFile)
	fd, err := unix.Openat(dirFD, openCodeInstallBootstrapFile,
		unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		if err == unix.EEXIST {
			return false, validateExistingOpenCodeBootstrapGitignoreAt(dirFD, path, surface)
		}
		return false, fmt.Errorf("create OpenCode %s bootstrap %q: %w", surface, path, err)
	}
	file := os.NewFile(uintptr(fd), path)
	keep := false
	defer func() {
		_ = file.Close()
		if !keep {
			// Removed through the same descriptor the file was created
			// through, so the deletion cannot land in a directory other than
			// the one validated.
			//
			// Deliberately NOT claimed: that it deletes the file we created.
			// unlinkat removes whatever entry holds this NAME in the pinned
			// directory, and there is no portable unlink-if-same-inode, so a
			// writer that unlinked and recreated .gitignore between our O_EXCL
			// create and our write failure would lose theirs. The descriptor
			// constrains WHERE, not WHICH.
			_ = unix.Unlinkat(dirFD, openCodeInstallBootstrapFile, 0)
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

func validateExistingOpenCodeBootstrapGitignoreAt(dirFD int, path, surface string) error {
	fd, err := unix.Openat(dirFD, openCodeInstallBootstrapFile,
		unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
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
