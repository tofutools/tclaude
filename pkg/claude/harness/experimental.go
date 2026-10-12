package harness

import (
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

// ExperimentalHarnessesEnvVar opts in to experimental harnesses for one
// process tree, as a comma-separated list of names. It exists for tests and CI
// runs that must not edit the operator's config; the config key
// `experimental_harnesses` is the durable opt-in.
const ExperimentalHarnessesEnvVar = "TCLAUDE_EXPERIMENTAL_HARNESSES"

var experimentalOverride struct {
	sync.Mutex
	enabled map[string]bool
}

// SetExperimentalHarnessEnabledForTest forces the opt-in for name until the
// returned restore func runs.
func SetExperimentalHarnessEnabledForTest(name string, enabled bool) (restore func()) {
	experimentalOverride.Lock()
	defer experimentalOverride.Unlock()
	if experimentalOverride.enabled == nil {
		experimentalOverride.enabled = map[string]bool{}
	}
	prev, had := experimentalOverride.enabled[name]
	experimentalOverride.enabled[name] = enabled
	return func() {
		experimentalOverride.Lock()
		defer experimentalOverride.Unlock()
		if had {
			experimentalOverride.enabled[name] = prev
		} else {
			delete(experimentalOverride.enabled, name)
		}
	}
}

// ExperimentalHarnessEnabled reports whether the operator opted in to the
// named experimental harness, through the config key or the environment.
func ExperimentalHarnessEnabled(name string) bool {
	experimentalOverride.Lock()
	forced, ok := experimentalOverride.enabled[name]
	experimentalOverride.Unlock()
	if ok {
		return forced
	}
	for _, n := range strings.Split(os.Getenv(ExperimentalHarnessesEnvVar), ",") {
		if strings.TrimSpace(n) == name {
			return true
		}
	}
	cfg, err := config.Load()
	return err == nil && cfg != nil && slices.Contains(cfg.ExperimentalHarnesses, name)
}

// Spawnable reports whether new sessions of this harness may be launched: it
// is registered and either not experimental or opted in. Plain Resolve does
// not consult it, so recorded conversations keep resolving to their harness.
func (h *Harness) Spawnable() bool {
	return h != nil && (!h.Experimental || ExperimentalHarnessEnabled(h.Name))
}
