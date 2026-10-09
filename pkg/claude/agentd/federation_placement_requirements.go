package agentd

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/nodeinfo"
)

type federationPlacementConstraintsKey struct{}

// Requirements constrain receiver-owned configuration; they never choose or
// override a receiver's profile, harness, model or working directory.
func validateLocalPlacementRequirements(require, effectiveHarness string) error {
	if len(require) > 1024 {
		return fmt.Errorf("requirements exceed 1024 bytes")
	}
	match, err := proto.ParseNodeMatch(require)
	if err != nil {
		return err
	}
	n := nodeinfo.Base()
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("node configuration unavailable")
	}
	if cfg != nil && cfg.Federation != nil {
		n.Labels = cfg.Federation.NodeLabels
	}
	if effectiveHarness != "" {
		n.Harnesses = []proto.NodeHarness{{Name: effectiveHarness}}
	} else {
		for _, name := range harness.Names() {
			h, _ := harness.Get(name)
			if h.Spawn != nil {
				if _, err := exec.LookPath(h.Spawn.Binary()); err == nil {
					n.Harnesses = append(n.Harnesses, proto.NodeHarness{Name: name})
				}
			}
		}
	}
	if !match.Matches(&n) {
		return fmt.Errorf("requirements do not match this node and its effective launch harness")
	}
	return nil
}
func federationPolicyHarness(g *db.AgentGroup, policy db.FederationSpawnPolicy) (string, error) {
	if strings.TrimSpace(policy.Harness) != "" {
		return policy.Harness, nil
	}
	if strings.TrimSpace(policy.Profile) != "" {
		p, err := db.ResolveSpawnProfile(policy.Profile)
		if err != nil || p == nil {
			return "", fmt.Errorf("automatic spawn profile unavailable")
		}
		return harnessOrDefault(p.Harness), nil
	}
	for _, p := range []*db.SpawnProfile{groupDefaultProfile(g), globalDefaultProfile()} {
		if p != nil {
			return harnessOrDefault(p.Harness), nil
		}
	}
	return harness.DefaultName, nil
}
