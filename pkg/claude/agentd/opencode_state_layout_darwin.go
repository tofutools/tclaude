//go:build darwin

package agentd

import (
	"fmt"
	"path/filepath"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
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

	layout.environment[2].Value = filepath.Dir(projectedSource)
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
