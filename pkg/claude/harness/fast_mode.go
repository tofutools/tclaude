package harness

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

const (
	FastModeInherit = ""
	FastModeOn      = "on"
	FastModeOff     = "off"
)

// CanFastMode reports whether tclaude can select the harness's per-request
// fast service tier at launch. Codex is currently the only such harness.
func (h *Harness) CanFastMode() bool {
	return h != nil && h.Name == CodexName
}

// ResolveFastMode converts the nullable profile/API spelling into launch
// intent. nil deliberately remains empty: Codex then inherits the operator's
// global config.toml. false forces the standard tier and can override a global
// fast default.
func ResolveFastMode(h *Harness, requested *bool) (string, error) {
	if requested == nil {
		return FastModeInherit, nil
	}
	if !h.CanFastMode() {
		return "", fmt.Errorf("harness %q does not support fast mode (fast mode is a %s feature)",
			harnessName(h), CodexName)
	}
	if *requested {
		return FastModeOn, nil
	}
	return FastModeOff, nil
}

// ResolveFastModeFlag validates the CLI spelling and gates it against the
// resolved harness. "inherit" normalizes to empty so no override is emitted.
func ResolveFastModeFlag(h *Harness, raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "inherit":
		return FastModeInherit, nil
	case FastModeOn:
		on := true
		return ResolveFastMode(h, &on)
	case FastModeOff:
		off := false
		return ResolveFastMode(h, &off)
	default:
		return "", fmt.Errorf("invalid fast mode %q: expected inherit, on, or off", strings.TrimSpace(raw))
	}
}

// CodexMainConfigFastMode reads the inherited Fast-mode baseline from the
// main config.toml in the Codex state root used by a launch. It deliberately
// does not inspect generated launch profiles: an explicit tclaude FastMode is
// already known by the caller, while an inherited launch gets service_tier
// from this main config. Missing config means Codex's standard-tier default.
func CodexMainConfigFastMode(codexStateRoot string) (bool, error) {
	data, err := os.ReadFile(filepath.Join(codexStateRoot, "config.toml"))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var config struct {
		ServiceTier string `toml:"service_tier"`
	}
	if err := toml.Unmarshal(data, &config); err != nil {
		return false, err
	}
	tier := strings.TrimSpace(config.ServiceTier)
	return strings.EqualFold(tier, "fast") || strings.EqualFold(tier, "priority"), nil
}
