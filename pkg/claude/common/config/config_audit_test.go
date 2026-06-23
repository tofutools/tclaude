package config

import "testing"

func TestResolvedAuditRetentionDays(t *testing.T) {
	cases := []struct {
		name     string
		cfg      *Config
		wantDays int
		wantPrune bool
	}{
		{"nil config", nil, DefaultAuditRetentionDays, true},
		{"no audit block", &Config{}, DefaultAuditRetentionDays, true},
		{"zero means default", &Config{Audit: &AuditConfig{RetentionDays: 0}}, DefaultAuditRetentionDays, true},
		{"positive override", &Config{Audit: &AuditConfig{RetentionDays: 7}}, 7, true},
		{"negative disables pruning", &Config{Audit: &AuditConfig{RetentionDays: -1}}, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			days, prune := tc.cfg.ResolvedAuditRetentionDays()
			if days != tc.wantDays || prune != tc.wantPrune {
				t.Fatalf("ResolvedAuditRetentionDays() = (%d, %v), want (%d, %v)",
					days, prune, tc.wantDays, tc.wantPrune)
			}
		})
	}
}
