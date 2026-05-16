package agentd

import (
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

// TestResolveSpawnRateLimit pins the three-tier resolution of the
// agent-spawn (clone) cooldown: the --agent-spawn-rate-limit flag wins
// over the agent.spawn_rate_limit config field, which wins over the
// built-in default. A present-but-invalid value at any tier is skipped
// so resolution falls through to the next tier.
func TestResolveSpawnRateLimit(t *testing.T) {
	cfgWith := func(v string) *config.Config {
		return &config.Config{Agent: &config.AgentConfig{SpawnRateLimit: v}}
	}

	cases := []struct {
		name       string
		flag       string
		cfg        *config.Config
		wantDur    time.Duration
		wantSource string
	}{
		{"both unset -> default", "", nil, defaultCloneCooldown, "default"},
		{"flag wins over config", "5m", cfgWith("30s"), 5 * time.Minute, "flag"},
		{"config used when no flag", "", cfgWith("30s"), 30 * time.Second, "config"},
		{"flag zero disables", "0", cfgWith("30s"), 0, "flag"},
		{"config zero disables", "", cfgWith("0s"), 0, "config"},
		{"invalid flag falls through to config", "bogus", cfgWith("45s"), 45 * time.Second, "config"},
		{"invalid flag and config -> default", "bogus", cfgWith("nope"), defaultCloneCooldown, "default"},
		{"negative flag falls through", "-1m", cfgWith("10s"), 10 * time.Second, "config"},
		{"negative config falls through to default", "", cfgWith("-1m"), defaultCloneCooldown, "default"},
		{"nil cfg with valid flag", "2m", nil, 2 * time.Minute, "flag"},
		{"nil agent section -> default", "", &config.Config{}, defaultCloneCooldown, "default"},
		{"empty config field -> default", "", cfgWith(""), defaultCloneCooldown, "default"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotDur, gotSource := resolveSpawnRateLimit(tc.flag, tc.cfg)
			if gotDur != tc.wantDur || gotSource != tc.wantSource {
				t.Fatalf("resolveSpawnRateLimit(%q, %+v) = (%v, %q), want (%v, %q)",
					tc.flag, tc.cfg, gotDur, gotSource, tc.wantDur, tc.wantSource)
			}
		})
	}
}
