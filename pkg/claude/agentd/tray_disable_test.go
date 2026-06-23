package agentd

import (
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

// TestShouldDisableTray pins the OR between the --no-tray serve flag and
// the persistent agent.disable_tray config field — either one hides the
// tray.
func TestShouldDisableTray(t *testing.T) {
	cases := []struct {
		name    string
		flagSet bool
		cfg     *config.Config
		want    bool
	}{
		{"flag on, no config", true, nil, true},
		{"flag on, config off", true, &config.Config{Agent: &config.AgentConfig{}}, true},
		{"flag off, config on", false,
			&config.Config{Agent: &config.AgentConfig{DisableTray: true}}, true},
		{"flag off, config off", false,
			&config.Config{Agent: &config.AgentConfig{DisableTray: false}}, false},
		{"flag off, nil agent", false, &config.Config{}, false},
		{"flag off, nil config", false, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldDisableTray(tc.flagSet, tc.cfg); got != tc.want {
				t.Fatalf("shouldDisableTray(%v, %+v) = %v, want %v",
					tc.flagSet, tc.cfg, got, tc.want)
			}
		})
	}
}
