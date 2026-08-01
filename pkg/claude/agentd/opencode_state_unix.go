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

	// openCodeStrandedAllocationRemedy is the way out for an operator whose
	// private OpenCode allocation no longer matches the private state parent
	// this daemon derives — the shape a changed XDG_DATA_HOME or HOME produces
	// (TCL-909).
	//
	// It is one constant rather than a sentence per site ON PURPOSE. Two
	// separate paths refuse this same situation: the control socket, which the
	// isolated and filtered postures reach, and the state-root anchor, which
	// host-open reaches. They were already telling operators different things
	// about the identical condition, and a second copy is how they would drift
	// again.
	//
	// "Recreate this agent" in as many words, because the alternative remedies
	// were considered and rejected: migrating the recorded parent would make
	// the daemon trust a value read back out of the same store the launch spec
	// comes from, which is the circularity TCL-902 closed, and no repair
	// command exists to point at. Restoring the environment is offered second
	// because it is the operator's cheaper option when the change was
	// accidental, and it mutates nothing.
	//
	// Do NOT attach this to a refusal an unaffected operator can reach. Telling
	// someone whose agent is fine to recreate it is worse than saying nothing.
	openCodeStrandedAllocationRemedy = "recreate this agent to get an allocation under the current parent, " +
		"or restore the previous XDG_DATA_HOME or HOME"
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
	// "could not be resolved OR is not canonical", because the condition is a
	// disjunction and the resolve error is one of its arms: an EACCES or ELOOP
	// reported as a canonicality verdict is a mechanism claim nobody tested.
	// The sentence names both arms rather than picking one.
	//
	// Naming WHICH arm fired, and showing the resolved value when the
	// inequality is the real one, would need the disjunction split into two
	// refusals. That is a control-flow change and this ticket is scoped to
	// message text, so it is deliberately not done here.
	//
	// The subject is "XDG config directory", not "filtered config base": the
	// value quoted is configApp — configBase/opencode — and the previous
	// wording named the parent while printing the child. It now matches the
	// vocabulary validateOpenCodeFilteredProviderSources uses for the same
	// directory.
	resolved, err := filepath.EvalSymlinks(configApp)
	if err != nil || resolved != configApp {
		return fmt.Errorf(
			"OpenCode filtered XDG config directory %q could not be resolved or is not canonical",
			configApp)
	}
	resolvedHome, err := filepath.EvalSymlinks(filteredHome)
	if err != nil || resolvedHome != filteredHome {
		return fmt.Errorf(
			"OpenCode filtered home %q could not be resolved or is not canonical",
			filteredHome)
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
	// canonicalOpenCodeRuntimePath is purely lexical — TrimSpace, Clean,
	// IsAbs — so it canonicalizes nothing and the only way it yields "" is a
	// path that is not absolute. The old sentence called that "not canonical",
	// which named a mechanism this gate does not perform: "", "   ",
	// "relative/path" and "." all reported a canonicality verdict when their
	// single fault was non-absoluteness, and it printed no value at all.
	//
	// The wording for this exact predicate already exists twice in this file
	// ("is not an absolute path", with the value quoted); this now agrees with
	// them rather than inventing a third spelling.
	//
	// spelled is kept because the assignment below overwrites the argument, and
	// the value worth showing an operator is the one they configured.
	spelled := stateRoot
	stateRoot = canonicalOpenCodeRuntimePath(stateRoot)
	if stateRoot == "" {
		return fmt.Errorf(
			"OpenCode filtered provider state root %q is not an absolute path",
			spelled)
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
				"OpenCode filtered provider bootstrap %q could not be read or is not the pinned provider-empty marker; clear the filtered config state or use network open",
				path)
		}
	}
	return nil
}

func validateOpenCodeFilteredProviderDirectory(
	path string,
	surface string,
) error {
	// This arm tests existence and directory-ness. It does NOT test
	// canonicality — that is the check immediately below — so calling its
	// failure "not a canonical directory" named the wrong mechanism and sent a
	// reader to the symlink question for a missing directory.
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() {
		return fmt.Errorf(
			"OpenCode filtered %s %q could not be inspected or is not a directory (symlinks are not followed); clear the filtered config state or use network open",
			surface, path)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path {
		return fmt.Errorf(
			"OpenCode filtered %s %q could not be resolved or is not canonical; clear the filtered config state or use network open",
			surface, path)
	}
	// "provider-empty" is what the directory is required to BE, not what this
	// condition tests. The condition is len(entries) != 1, so the sentence it
	// used to produce told an EMPTY directory that it was not empty, and blamed
	// a ReadDir failure on the directory's contents. Both are states an
	// operator would then go and look for and not find.
	entries, err := os.ReadDir(path)
	if err != nil || len(entries) != 1 {
		return fmt.Errorf(
			"OpenCode filtered %s %q could not be listed or does not hold exactly one entry (the provider-empty marker); clear it or use network open",
			surface, path)
	}
	entry := entries[0]
	entryInfo, infoErr := os.Lstat(filepath.Join(path, entry.Name()))
	if entry.Name() != openCodeInstallBootstrapFile ||
		infoErr != nil || !entryInfo.Mode().IsRegular() {
		return fmt.Errorf(
			"OpenCode filtered %s %q holds an entry %q that could not be inspected or is not the approved marker file; clear it or use network open",
			surface, path, entry.Name())
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

// openCodeRecordedStateRootDescription renders the allocation's recorded state
// root for a refusal. Presentation only; it reaches no decision.
//
// A legacy-shared allocation records NO state root: validateOpenCodeState
// Allocation requires the field to be empty, and resolvedOpenCodeSeedPath("")
// is ".", because Clean("") is "." and EvalSymlinks(".") succeeds. Printing
// that would name "." as the recorded root of an allocation that has none.
// Saying so in words is the only answer available.
//
// The emptiness test is `== ""`, not TrimSpace, so that it agrees EXACTLY with
// the validator that guarantees its precondition — that check is untrimmed, so
// a whitespace root never survives validation and cannot reach here. Using a
// looser predicate would also mean calling a recorded (if useless) value "none
// recorded", which is a small instance of the overstatement this whole pass
// exists to remove.
func openCodeRecordedStateRootDescription(recorded, resolved string) string {
	if recorded == "" {
		return "no allocated state root recorded"
	}
	return fmt.Sprintf("allocated state root %q", resolved)
}

// openCodeStatOwnerDescription renders the owner half of an ownership refusal.
// It is presentation only and reaches no decision: the two callers already
// decided, and both of their conditions are disjunctions whose arms would
// otherwise render identically.
//
// The !ok arm gets its own words rather than a zero. A Sys() assertion that did
// not yield a *syscall.Stat_t leaves stat nil, and "found uid 0" for that case
// would name root as the owner of a directory whose owner was never read —
// a confident wrong answer where no answer is available.
func openCodeStatOwnerDescription(stat *syscall.Stat_t, ok bool) string {
	if !ok || stat == nil {
		return "no readable owner"
	}
	return fmt.Sprintf("uid %d", stat.Uid)
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
	// Names the path, and names the Lstat failure as its own arm. The root's
	// counterpart below handles its Lstat error separately and so can state a
	// pure directory verdict; this one folds the error in, and an EACCES
	// reported as "is not a real mode-0700 directory" is a claim about the
	// directory's nature that was never tested.
	parentInfo, err := os.Lstat(parent)
	if err != nil || parentInfo.Mode()&os.ModeSymlink != 0 || !parentInfo.IsDir() ||
		parentInfo.Mode().Perm() != 0o700 {
		return "", fmt.Errorf(
			"OpenCode control parent %q could not be inspected or is not a real mode-0700 directory",
			parent)
	}
	// The owner refusals print the path, the owner found, and the owner
	// required. Without them a reader cannot tell a wrong uid from a Sys()
	// assertion that did not yield a *syscall.Stat_t — and in this region a
	// zero would otherwise be indistinguishable from not having looked.
	parentStat, parentStatOK := parentInfo.Sys().(*syscall.Stat_t)
	if !parentStatOK || parentStat.Uid != uint32(os.Geteuid()) {
		return "", fmt.Errorf(
			"OpenCode control parent %q owner could not be read or is not the expected one (found %s, expected uid %d)",
			parent, openCodeStatOwnerDescription(parentStat, parentStatOK), os.Geteuid())
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
	// Left as it stands, deliberately. It covers three causes — symlink, not a
	// directory, wrong permission — and it is TRUE of all three, because the
	// Lstat error is already handled one arm up. Naming which of the three
	// fired would need the disjunction split, which is a control-flow change
	// and out of this ticket's scope. Recorded rather than silently skipped:
	// vague is not the same defect as wrong, and only the wrong ones are fixed
	// here.
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return "", fmt.Errorf("OpenCode control root %q is not a real mode-0700 directory", controlRoot)
	}
	rootStat, rootStatOK := info.Sys().(*syscall.Stat_t)
	if !rootStatOK || rootStat.Uid != uint32(os.Geteuid()) {
		return "", fmt.Errorf(
			"OpenCode control root %q owner could not be read or is not the expected one (found %s, expected uid %d)",
			controlRoot, openCodeStatOwnerDescription(rootStat, rootStatOK), os.Geteuid())
	}
	resolved, err := filepath.EvalSymlinks(controlRoot)
	if err != nil {
		return "", fmt.Errorf("resolve OpenCode control root %q: %w", controlRoot, err)
	}
	if resolved != controlRoot {
		return "", fmt.Errorf(
			"OpenCode control root %q is not canonical (it resolves to %q)",
			controlRoot, resolved)
	}
	// The stranded case, split out from the three around it because it is the
	// only one an operator can act on and the only one with a cause worth
	// naming. For a private allocation controlRoot is the RECORDED state root
	// while parent is derived from the live environment, so this fires exactly
	// when the two disagree — which is what a changed XDG_DATA_HOME or HOME
	// does.
	//
	// Legacy-shared cannot reach it: controlRoot was built as
	// filepath.Join(parent, agentID) a few lines up, so its parent IS parent by
	// construction and it follows the environment instead of being stranded by
	// it. Probed, not reasoned about — a legacy-shared allocation returns a
	// path with no error both before and after the move. That matters here
	// because telling an operator who is NOT stranded to recreate their agent
	// is worse than saying nothing.
	if filepath.Dir(controlRoot) != parent {
		return "", fmt.Errorf(
			"OpenCode control root %q is outside this daemon's private state parent %q; %s",
			controlRoot, parent, openCodeStrandedAllocationRemedy)
	}
	if filepath.Base(controlRoot) != agentID {
		return "", fmt.Errorf(
			"OpenCode control root %q is not the direct child named for agent %s",
			controlRoot, agentID)
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
			return nil, fmt.Errorf(
				"private OpenCode state root %q could not be resolved or no longer has its allocated identity",
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
			// Both arms named, for the same reason as the filtered sites: the
			// EvalSymlinks error is a disjunct here, and reporting an EACCES
			// or ELOOP as a canonicality verdict claims a mechanism this line
			// did not run.
			return nil, fmt.Errorf(
				"private OpenCode %s directory %q could not be resolved or is not canonical",
				name, appDir)
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

// prepareOpenCodeTclaudeLayerState materializes the harness-owned state a
// frozen OpenCode launch spec names, after proving that the daemon can
// re-derive every directory it is about to create.
//
// session.PrepareTclaudeLayerHarnessState MkdirAll's the contract's StateRoot
// and every StateDirs entry. The guards it applies on its own are the
// contract's WriteDirs and ProfileFilesystem — supplied by the same persisted
// artifact it is validating — and StateRoot carries no WriteDirs requirement at
// all. A spec tampered with in the database could therefore cause empty
// mode-0700 directories outside the protected roots (TCL-907).
//
// That capability is bounded and nothing is exploitable today: empty
// directories, no file content, and it needs the DB write access that plants
// the spec in the first place. It is closed because the launch contract exists
// so that a persisted artifact cannot steer daemon-side filesystem operations,
// and "it only creates empty directories" describes today's payload, not the
// boundary.
//
// The proof lives here rather than in session because the authority that can
// supply it — the OpenCode allocation store plus this daemon's derived private
// state parent — is this package's. session is harness-agnostic and has no
// access to it, so pushing the check down would mean re-deriving the authority
// in a second place, which is what TCL-902 exists to have stopped doing.
func prepareOpenCodeTclaudeLayerState(spec *session.TclaudeLayerLaunchSpec) error {
	if spec == nil {
		return nil
	}
	if err := requireOpenCodeAnchoredStateTargets(spec.Contract); err != nil {
		return err
	}
	return session.PrepareTclaudeLayerHarnessState(*spec)
}

// requireOpenCodeAnchoredStateTargets proves that every directory the replay
// path is about to create is one this daemon can re-derive, rather than one the
// persisted contract merely names.
//
// A state root is proven exactly one of two ways, matching the two shapes the
// layouts actually emit:
//
//  1. a per-agent root, by the SAME allocation authority the config seed and
//     the control socket use — requireOpenCodeAllocatedStateRoot;
//  2. the legacy shared root, by re-deriving it from this host's environment
//     through session.TclaudeLayerHarnessStateRoot, which is the function
//     BuildTclaudeLayerLaunchSpec itself uses to produce it.
//
// A mutable state directory then has to be below its proven state root, or be
// one of the four ambient OpenCode XDG app directories this host derives for
// itself. The second arm is not a loophole for tampering — it is production:
// the legacy shared posture names exactly those four, and the Darwin private
// posture replaces StateDirs[2] with the resolved ambient config app directory
// when one exists (see adaptOpenCodeStateLayoutForPlatform). Both are
// re-derived here from the daemon's own environment via openCodeAmbientAppDir,
// the same derivation the layout used, so a contract cannot widen the set by
// naming something else.
//
// ReadOnlyStateDirs are covered too, and WITHOUT the ambient arm: the renderer
// already requires them to be below the state root, and no posture emits an
// ambient one. They need their own resolved check rather than inheriting the
// renderer's, because the renderer's containment test is LEXICAL against the
// unresolved state root — so an in-root name that resolves outside the root
// satisfies it. A symlink planted inside an allocated state root by the very
// agent that owns that directory is exactly that shape, which makes this the
// third mkdir sink in the same function rather than a hypothetical one.
func requireOpenCodeAnchoredStateTargets(
	contract session.TclaudeLayerLaunchContract,
) error {
	if contract.HarnessName != harness.OpenCodeName {
		return nil
	}
	stateRoot := canonicalOpenCodeRuntimePath(contract.StateRoot)
	if stateRoot == "" {
		return fmt.Errorf(
			"OpenCode launch contract state root %q is not an absolute path",
			contract.StateRoot)
	}
	resolvedRoot, err := requireOpenCodeAnchoredStateRoot(contract.StateRoot, stateRoot)
	if err != nil {
		return err
	}
	// Derived lazily, and only when a directory is not below the state root.
	// A Linux private posture has no ambient state directory at all, and
	// computing the set eagerly would make that posture fail on an environment
	// it never consults.
	var ambient map[string]bool
	// An ordered slice, not a map: a contract with a problem in both groups must
	// produce the SAME refusal every time it is replayed, and Go randomizes map
	// iteration.
	for _, group := range []struct {
		kind string
		dirs []string
		// Only mutable state directories may name an ambient directory. The
		// read-only ones are always below the state root in every posture.
		allowAmbient bool
	}{
		{kind: "state directory", dirs: contract.StateDirs, allowAmbient: true},
		{kind: "read-only state directory", dirs: contract.ReadOnlyStateDirs},
	} {
		kind := group.kind
		for index, dir := range group.dirs {
			clean := canonicalOpenCodeRuntimePath(dir)
			if clean == "" {
				return fmt.Errorf(
					"OpenCode launch contract %s %d %q is not an absolute path",
					kind, index, dir)
			}
			// Resolved before comparison, and through the missing-path walker
			// rather than EvalSymlinks: on a first launch these directories do
			// not exist yet, which is the whole reason the replay path is
			// creating them.
			resolved, resolveErr := canonicalizeMissingOpenCodePath(clean)
			if resolveErr != nil {
				return fmt.Errorf(
					"canonicalize OpenCode launch contract %s %q: %w",
					kind, dir, resolveErr)
			}
			if sandboxpolicy.PathContainsOrEqual(resolvedRoot, resolved) {
				continue
			}
			if group.allowAmbient {
				if ambient == nil {
					ambient, err = ambientOpenCodeStateAppDirs()
					if err != nil {
						return err
					}
				}
				if ambient[resolved] {
					continue
				}
			}
			// The comparison that just failed was on RESOLVED, so resolved is
			// what the refusal has to show. Quoting only the contract's
			// spelling produces a sentence that contradicts itself:
			// "<root>/escape/x is not below <root>" reads as a broken
			// containment check rather than as "escape is a symlink". Same rule
			// openCodeStateRootSubject states for the state root — a refusal
			// must not consume information its message withholds.
			//
			// Triggered on resolved != DIR, not resolved != clean, and worded
			// "tested as" rather than "resolving to". Both corrections come
			// from one finding: two different transformations sit between the
			// contract's spelling and the compared value — lexical
			// normalization (Clean/TrimSpace, via canonicalOpenCodeRuntimePath)
			// and symlink resolution — and the earlier version showed only the
			// second. A purely lexical "<root>/data/../../../../etc" therefore
			// printed the raw spelling with no parenthetical at all, which is
			// precisely the withholding this block exists to stop. Naming the
			// mechanism was also wrong: "resolving to" invites the operator to
			// apply kernel semantics to the quoted string, and the kernel does
			// not resolve it this way — Clean collapses "..", including one
			// that follows a symlink, before anything is resolved.
			//
			// "Tested as" claims only what is true and is the whole point: this
			// is the value the containment check actually ran on. The daemon's
			// mkdir applies the identical Clean before creating anything, so
			// the two agree; that consistency is what makes the shown path the
			// operative one rather than a second opinion.
			subject := fmt.Sprintf("%s %q", kind, dir)
			if resolved != dir {
				subject = fmt.Sprintf("%s %q (tested as %q)", kind, dir, resolved)
			}
			// Worded per group. The read-only arm runs with allowAmbient
			// false, so offering "nor one of this host's ambient directories"
			// there names a criterion it never applies — and a directory that
			// IS ambient would be told it is not. Worse, that sentence is the
			// one the migration note assigns to "XDG_CONFIG_HOME changed", so
			// it would point an operator at an environment change that is not
			// the cause.
			if !group.allowAmbient {
				return fmt.Errorf(
					"OpenCode launch contract %s is not below its state root %q",
					subject, resolvedRoot)
			}
			return fmt.Errorf(
				"OpenCode launch contract %s is neither below its state root %q nor one of this host's ambient OpenCode state directories",
				subject, resolvedRoot)
		}
	}
	return nil
}

// requireOpenCodeAnchoredStateRoot returns the resolved state root once it is
// proven, so a caller cannot accidentally go on to compare against the
// unproven spelling it was handed.
func requireOpenCodeAnchoredStateRoot(spelled, stateRoot string) (string, error) {
	resolved, err := canonicalizeMissingOpenCodePath(stateRoot)
	if err != nil {
		return "", fmt.Errorf(
			"canonicalize OpenCode launch contract state root %q: %w", spelled, err)
	}
	// Which of the two proofs applies is decided on the SAME value
	// validateOpenCodeV3LaunchContract branches on — the unresolved base name —
	// so a contract cannot take the private branch there and the legacy branch
	// here, or the reverse.
	if openCodeAgentIDRE.MatchString(filepath.Base(stateRoot)) {
		if err := requireOpenCodeAllocatedStateRoot(resolved,
			openCodeStateRootSubject(spelled, stateRoot, resolved), "state root"); err != nil {
			return "", err
		}
		return resolved, nil
	}
	legacy, err := session.TclaudeLayerHarnessStateRoot(harness.OpenCodeName)
	if err != nil {
		return "", fmt.Errorf("resolve this host's OpenCode state root: %w", err)
	}
	resolvedLegacy, err := canonicalizeMissingOpenCodePath(legacy)
	if err != nil {
		return "", fmt.Errorf("canonicalize this host's OpenCode state root: %w", err)
	}
	if resolved != resolvedLegacy {
		// Both operands shown in the form they were COMPARED in. Printing
		// stateRoot here put an unresolved left side against a resolved right
		// side, so on a symlinked HOME the two sat in different namespaces with
		// nothing saying why — and after a ".." the left side was a path the
		// contract never contained and the kernel would never produce, because
		// Clean collapses ".." lexically. The private arm above avoids this by
		// going through openCodeStateRootSubject; this arm did not, and no
		// per-delta review reached it because no delta touched it.
		return "", fmt.Errorf(
			"%s is neither an allocated per-agent state root nor this host's OpenCode state root %q",
			openCodeStateRootSubject(spelled, stateRoot, resolved), resolvedLegacy)
	}
	return resolved, nil
}

// openCodeStateRootSubject names a contract's state root the way an operator
// has to see it to act on a refusal.
//
// The branch above is chosen on the UNRESOLVED base name, so that the launch
// contract's validator and this cannot disagree about which arm applies, but
// the allocation is looked up under the RESOLVED one. When a symlink in ANY
// component makes those differ, quoting only the resolved path reports a
// directory the operator never wrote, with no hint that a link was followed —
// the refusal would consume information its message did not.
func openCodeStateRootSubject(spelled, stateRoot, resolved string) string {
	// Quotes what the CONTRACT said, and appends the value the check ran on
	// when they differ. Two transformations sit between them — lexical
	// normalization (canonicalOpenCodeRuntimePath) and symlink resolution — and
	// an earlier version quoted the normalized form while calling it the
	// contract's, so a contract naming "/p/link/../agt_1f" was refused with
	// "/p/agt_1f" presented as its own words.
	//
	// "Tested as", not "resolving to": Clean collapses "..", including one that
	// follows a symlink, before anything is resolved, so naming resolution as
	// the mechanism invites kernel semantics the code never applies. Claim the
	// VALUE and stop. Same wording as the state-directory arm, deliberately —
	// the two drifting apart is what this whole class came from.
	if resolved == spelled {
		return fmt.Sprintf("OpenCode launch contract state root %q", spelled)
	}
	return fmt.Sprintf(
		"OpenCode launch contract state root %q (tested as %q)", spelled, resolved)
}

// ambientOpenCodeStateAppDirs is the set of OpenCode app directories THIS host
// keeps under its ambient XDG bases, in resolved form. It reuses
// openCodeAmbientAppDir so there is one derivation of "the ambient OpenCode
// data/cache/config/state directory" shared with the layout builder and the
// config seed.
func ambientOpenCodeStateAppDirs() (map[string]bool, error) {
	dirs := make(map[string]bool, 4)
	for _, base := range []struct{ env, fallback string }{
		{"XDG_DATA_HOME", ".local/share"},
		{"XDG_CACHE_HOME", ".cache"},
		{"XDG_CONFIG_HOME", ".config"},
		{"XDG_STATE_HOME", ".local/state"},
	} {
		dir, err := openCodeAmbientAppDir(base.env, base.fallback)
		if err != nil {
			return nil, err
		}
		resolved, err := canonicalizeMissingOpenCodePath(dir)
		if err != nil {
			return nil, fmt.Errorf(
				"canonicalize ambient OpenCode state directory %q: %w", dir, err)
		}
		dirs[resolved] = true
	}
	return dirs, nil
}

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
		// "not lexically below the contract's state root", not "in ambient host
		// config" — and not merely "outside" it either. The condition IS a
		// spelling comparison, so an allocated per-agent source reached through
		// a symlinked state root fails it while being physically inside, and
		// the opposite of ambient. Naming the comparison rather than a
		// conclusion about the path's nature is the only claim this line can
		// support, in the one record an operator would later use to decide
		// whether host config was touched.
		//
		// A cold review caught that the first correction here traded a wrong
		// conclusion for a weaker one rather than for the mechanism.
		//
		// Both operands are logged for the same reason: the answer to "why is
		// this line here" is the relationship between them, and a reader who
		// cannot see the state root cannot check the claim.
		slog.Info("created OpenCode bootstrap metadata not lexically below the contract's state root before read-only confinement",
			"path", filepath.Join(source, openCodeInstallBootstrapFile),
			"state_root", stateRoot)
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
	identity, err := openCodeDirectoryIdentityOf(sourceFD, source, "config")
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
	// second resolution but that the CANDIDATE side is daemon-derived and lives
	// under this daemon's own private state parent, while the DESCRIPTOR side
	// needs no trust at all because it is the thing being proven — the whole
	// route sitting behind the same DB-write precondition.
	//
	// Stated that way deliberately. An earlier version of this comment said
	// "both ends are daemon-derived", which is false and false in the worst
	// direction: the descriptor is opened on the contract's own
	// ReadOnlyBinds[].Source, i.e. exactly the persisted-spec input this
	// function exists to constrain, as its own header says 40 lines up. A
	// safety argument that assumes the untrusted input is trusted is worse than
	// no comment, because it reads as a reason to stop checking.
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
			// "is not", NOT "does not resolve to". That wording was TRUE
			// before this change, when the test really was
			// resolvedOpenCodeSeedPath(source) against the ambient one. This
			// change made acceptance a device/inode comparison against an open
			// descriptor, and this function's own header now says "source is
			// carried for MESSAGES only — nothing here resolves it". The
			// sentence outlived the mechanism it described.
			//
			// It was also wrong in a case that has nothing to do with
			// symlinks: on a host with no ambient config at all,
			// openCodeDirectoryIs returns false on the Stat error, and the
			// operator was told the source "does not resolve to" a directory
			// that does not exist — implying it exists somewhere else.
			"read-only OpenCode config bind source %q is not an allocated per-agent config directory (%w), and is not this host's ambient OpenCode config %q",
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
// The shape check is this function's own — only the seed cares that a config
// app directory sits at <state root>/config/opencode. Everything after it is
// the same question the replay path's mkdir targets ask, so it is asked of
// requireOpenCodeAllocatedStateRoot rather than restated here.
//
// It returns the directory it settled on rather than only an error, and the
// caller compares its pinned descriptor against THAT value. The alternative —
// letting the caller re-derive the path — is a second read of the same input,
// which is the check-then-use window TCL-908 closes.
func requireOpenCodeAllocatedConfigDir(configDir string) (string, error) {
	resolved := resolvedOpenCodeSeedPath(configDir)
	configBase := filepath.Dir(resolved)
	stateRoot := filepath.Dir(configBase)
	if filepath.Base(resolved) != "opencode" || filepath.Base(configBase) != "config" {
		// The tested value is appended, matching the state-directory arm's
		// "(tested as %q)". The decision runs on resolved and the sentence
		// quoted configDir, so with a symlinked opencode leaf the refusal
		// printed a path that visibly HAS the shape it was being told it
		// lacks. That reads as a bug in the validator rather than as a symlink
		// in the operator's tree, and it is the only message in this file that
		// contradicts itself on screen.
		return "", fmt.Errorf(
			"OpenCode config bootstrap target %q does not have the per-agent <state root>/config/opencode shape (tested as %q)",
			configDir, resolved)
	}
	if err := requireOpenCodeAllocatedStateRoot(stateRoot,
		fmt.Sprintf("OpenCode config bootstrap target %q", configDir),
		"config directory"); err != nil {
		return "", err
	}
	// Built from stateRoot — the local derived from the ONE resolution of
	// configDir above, and validated by the call just made — never from a fresh
	// walk.
	//
	// Scoped deliberately, because a stronger claim was made here and refuted.
	// This body resolves configDir exactly once; it does NOT follow that each
	// input is resolved once end to end. requireOpenCodeAllocatedStateRoot
	// resolves allocation.StateRoot and the derived parent, and reaches
	// validateOpenCodeStateAllocation, which Lstats and EvalSymlinks the
	// recorded root before that equality check resolves it again. Those reads
	// establish a different property (the recorded root is a real, non-symlink,
	// unprotected directory) and live behind the same DB-write precondition. The
	// honest statement is about this body, not the call graph.
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

// requireOpenCodeAllocatedStateRoot proves that a per-agent state root named by
// a launch contract is one this daemon actually allocated, rather than a path a
// persisted artifact merely asserts. Its argument must already be resolved.
//
// It reuses requireOpenCodeStateAllocation — the same allocation authority the
// launch path itself consults — instead of restating what a legitimate
// per-agent state root looks like. The allocation store is the same durable
// database as the launch spec, so existence alone would prove little; the state
// root is therefore also required to be a direct child of the private state
// parent THIS daemon derives, the same anchor openCodeControlSocketPath applies
// to the same allocation.
//
// One predicate serves two questions that would otherwise drift: where the
// config bootstrap may write (TCL-902), and which state directories the replay
// path may create (TCL-907).
//
// subject and noun are RENDERING only — the caller's own name for the thing it
// is asking about ("OpenCode config bootstrap target %q") and for the kind of
// path it is ("config directory"). They never reach a decision. They exist so
// one rule can answer two callers without either caller's operator-visible
// sentence changing to mention the other's subject.
func requireOpenCodeAllocatedStateRoot(stateRoot, subject, noun string) error {
	allocation, err := requireOpenCodeStateAllocation(filepath.Base(stateRoot))
	// Rendered differently, not decided differently: the agent-id rule stays
	// wholly inside requireOpenCodeStateAllocation. This only declines to quote
	// an operator's own directory name back at them as an "invalid agent id"
	// when their path merely happens to sit where a per-agent root would.
	if errors.Is(err, errOpenCodeInvalidAgentID) {
		return fmt.Errorf("%s names %q where a per-agent state root was expected",
			subject, stateRoot)
	}
	if err != nil {
		return fmt.Errorf("%s is not an allocated per-agent %s: %w", subject, noun, err)
	}
	// Hoisted out of the condition so the message can print the value the
	// comparison ran on, rather than resolving a second time in the format
	// arguments — a second walk is a second read, which is the hazard the
	// comment in requireOpenCodeAllocatedConfigDir spells out.
	//
	// It is NOT fewer reads than the pre-change code: the || short-circuited
	// this call away whenever the mode already mismatched, and it now always
	// runs. No decision changes — EvalSymlinks is side-effect-free and the
	// condition is identical — but the honest comparison is against what was
	// here, not against the alternative rendering.
	allocatedRoot := resolvedOpenCodeSeedPath(allocation.StateRoot)
	if allocation.Mode != db.OpenCodeStatePrivate || allocatedRoot != stateRoot {
		// Both paths named. The condition is mode-mismatch OR path-mismatch,
		// and when it was the path the sentence named neither the allocated
		// root nor the tested one — leaving an operator with a refusal about
		// two paths and no way to see how they differed.
		//
		// The allocated root is RENDERED, not printed raw. A legacy-shared
		// allocation is required by validateOpenCodeStateAllocation to record
		// an EMPTY state root, and resolvedOpenCodeSeedPath("") is ".", so
		// quoting it would tell an operator that this allocation's recorded
		// root is "." — a confident wrong value for a field guaranteed to hold
		// none, and a path they would then go and look at. That is this
		// ticket's own defect shape, and it was introduced by this fix before
		// a cold review caught it.
		return fmt.Errorf(
			"%s does not belong to the %s state allocation of agent %s (%s, tested as %q)",
			subject, allocation.Mode, allocation.AgentID,
			openCodeRecordedStateRootDescription(allocation.StateRoot, allocatedRoot),
			stateRoot)
	}
	parent, err := openCodePrivateStateParent()
	if err != nil {
		// subject, like the four refusals around it. Before this function was
		// generalized to serve two callers it hard-coded "for bootstrap", which
		// was accurate when there was only one; generalizing correctly deleted
		// that qualifier and did not substitute the parameter that exists to
		// carry it, so the caller's name was dropped at exactly the point it
		// started to matter.
		//
		// NOT TEST-COVERED, and that is stated rather than left to look like an
		// oversight. openCodePrivateStateParent fails only when
		// openCodeXDGBase falls through to os.UserHomeDir and that fails, i.e.
		// HOME undefined. Reaching this line first requires
		// requireOpenCodeStateAllocation to have SUCCEEDED, and for a private
		// allocation that runs refuseOpenCodeProtectedStateRoot ->
		// sandboxpolicy.ProtectedPaths, which resolves the home directory live
		// on every call and is not memoized. So every HOME state that could
		// fire this branch has already refused one step earlier, for the same
		// underlying reason. Established by probe, not by reading: a test that
		// clears HOME reports "resolve protected paths for OpenCode state:
		// ... $HOME is not defined" and never arrives here. A test pinning this
		// wording would have to go red for that other reason, which is not
		// evidence about this line.
		return fmt.Errorf(
			"resolve OpenCode private state parent while checking %s: %w",
			subject, err)
	}
	// Both sides are compared in resolved form. The allocator records a parent it
	// has already resolved, while this derives one from the live environment, so
	// a symlinked home or XDG base makes the two disagree as strings while naming
	// the same directory.
	resolvedParent := resolvedOpenCodeSeedPath(parent)
	if filepath.Dir(stateRoot) != resolvedParent {
		// resolvedParent, not parent. The comparison two lines up runs on the
		// RESOLVED form, and printing the unresolved spelling is the exact
		// defect this family exists to remove: on a symlinked HOME or
		// XDG_DATA_HOME the operator is handed a path that is not the one the
		// check tested and cannot be matched against their own. The sibling
		// refusal in openCodeControlSocketPath already shows the compared
		// value; this now agrees with it.
		return fmt.Errorf(
			"%s is outside this daemon's private state parent %q; a changed XDG_DATA_HOME or HOME moves that parent away from an existing allocation — %s",
			subject, resolvedParent, openCodeStrandedAllocationRemedy)
	}
	return nil
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

func openCodeDirectoryIdentityOf(
	fd int,
	path, surface string,
) (openCodeDirectoryIdentity, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return openCodeDirectoryIdentity{}, fmt.Errorf(
			"inspect OpenCode %s bootstrap directory %q: %w", surface, path, err)
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
