package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/launchspec"
)

// relaunchPolicyFromFlags builds a spawn's per-agent relaunch rows from
// --relaunch-policy-file (the relaunch-policy JSON shape, checked with
// RelaunchPolicy.Validate) and the repeatable --relaunch-policy field=mode,
// which override the file's rows. nil when neither is given.
func relaunchPolicyFromFlags(pairs []string, file string) (map[string]string, error) {
	rows := map[string]string{}
	if file = strings.TrimSpace(file); file != "" {
		raw, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("--relaunch-policy-file: %w", err)
		}
		var policy launchspec.RelaunchPolicy
		if err := json.Unmarshal(raw, &policy); err != nil {
			return nil, fmt.Errorf("--relaunch-policy-file %s: %w", file, err)
		}
		for i := range policy.Rows {
			if policy.Rows[i].Scope == "" {
				policy.Rows[i].Scope = launchspec.ScopeGlobal
			}
		}
		if err := policy.Validate(); err != nil {
			return nil, fmt.Errorf("--relaunch-policy-file %s: %w", file, err)
		}
		for _, r := range policy.Rows {
			rows[r.Field] = string(r.Mode)
		}
	}
	for _, pair := range pairs {
		field, mode, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if !ok || strings.TrimSpace(field) == "" || strings.TrimSpace(mode) == "" {
			return nil, fmt.Errorf("--relaunch-policy %q: want field=mode", pair)
		}
		rows[strings.TrimSpace(field)] = strings.TrimSpace(mode)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	if err := launchspec.ValidateOverrides(rows, true); err != nil {
		return nil, fmt.Errorf("--relaunch-policy: %w", err)
	}
	return rows, nil
}
