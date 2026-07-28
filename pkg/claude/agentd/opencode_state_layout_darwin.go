//go:build darwin

package agentd

import (
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// adaptOpenCodeStateLayoutForPlatform preserves OpenCode's native global-
// before-project config precedence without asking Seatbelt to emulate a bind
// mount. Mutable data/cache/state remain under the private agent root. When a
// real ambient global config exists, XDG_CONFIG_HOME names its canonical base
// directly and Seatbelt makes the app directory daemon-final read-only.
//
// That means non-OpenCode programs inheriting XDG_CONFIG_HOME see the real host
// config base on Darwin. The launch disclosure names this privacy divergence;
// the sandbox still gates writes through the ordinary filesystem policy.
func adaptOpenCodeStateLayoutForPlatform(layout *openCodeStateLayout) error {
	if layout == nil || layout.allocation.Mode != db.OpenCodeStatePrivate {
		return nil
	}
	if len(layout.stateDirs) != 4 || len(layout.environment) != 4 {
		return fmt.Errorf("private OpenCode state layout has incomplete XDG roots")
	}
	privateConfig := filepath.Clean(layout.stateDirs[2])
	projectedSource := ""
	for _, bind := range layout.readOnlyBinds {
		if filepath.Clean(bind.Target) == privateConfig &&
			filepath.Clean(bind.Source) != privateConfig {
			projectedSource = filepath.Clean(bind.Source)
			break
		}
	}
	if projectedSource == "" {
		// No ambient global config exists. The already-created empty private
		// config path remains a same-path read-only target.
		return nil
	}

	configBase := filepath.Dir(filepath.Clean(layout.ambient.config))
	if resolvedBase, err := filepath.EvalSymlinks(configBase); err == nil {
		configBase = filepath.Clean(resolvedBase)
	}
	layout.environment[2].Value = configBase
	layout.stateDirs[2] = projectedSource
	filtered := make([]session.TclaudeLayerReadOnlyBind, 0, len(layout.readOnlyBinds)-1)
	for _, bind := range layout.readOnlyBinds {
		if filepath.Clean(bind.Target) == privateConfig &&
			filepath.Clean(bind.Source) != privateConfig {
			continue
		}
		filtered = append(filtered, bind)
	}
	layout.readOnlyBinds = filtered
	return nil
}

// prepareOpenCodeReadOnlyConfigForPlatform supplies the one app-owned
// compatibility file OpenCode 1.18.6 writes before loading a config directory:
// https://github.com/anomalyco/opencode/blob/v1.18.6/packages/opencode/src/config/config.ts#L295-L312
//
// It runs after the durable state paths exist but before sandbox-exec starts.
// Only the config app directory actually named by the launch contract is
// eligible, and it must already be a same-path daemon-final read-only root.
func prepareOpenCodeReadOnlyConfigForPlatform(
	spec *session.TclaudeLayerLaunchSpec,
) error {
	if spec == nil || spec.Contract.HarnessName != harness.OpenCodeName ||
		len(spec.Contract.StateDirs) != 4 {
		return nil
	}
	configDir := filepath.Clean(spec.Contract.StateDirs[2])
	readOnly := false
	for _, bind := range spec.Contract.ReadOnlyBinds {
		if filepath.Clean(bind.Source) == configDir &&
			filepath.Clean(bind.Target) == configDir {
			readOnly = true
			break
		}
	}
	if !readOnly {
		return nil
	}
	created, err := ensureOpenCodeBootstrapGitignore(configDir, "config")
	if err != nil {
		return fmt.Errorf(
			"opencode_read_only_config_bootstrap: refuse Darwin OpenCode launch because the read-only config prerequisite could not be established: %w",
			err)
	}
	stateRoot := filepath.Clean(spec.Contract.StateRoot)
	if created && (!filepath.IsAbs(stateRoot) ||
		!sandboxpolicy.PathContainsOrEqual(stateRoot, configDir)) {
		slog.Info("created OpenCode bootstrap metadata in ambient host config before read-only confinement",
			"path", filepath.Join(configDir, openCodeInstallBootstrapFile))
	}
	return nil
}
