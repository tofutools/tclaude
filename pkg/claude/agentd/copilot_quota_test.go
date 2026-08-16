package agentd

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func TestRefreshCopilotQuotaStoresFiniteMonthlyAllowance(t *testing.T) {
	setupTestDB(t)
	observed := time.Date(2026, 8, 13, 9, 0, 0, 0, time.UTC)
	remaining := 41.8
	var token string
	deps := copilotQuotaDeps{
		lookPath: func(name string) (string, error) { return "/bin/" + name, nil },
		commandOutput: func(_ context.Context, _ string, args ...string) ([]byte, error) {
			assert.Equal(t, []string{"auth", "token"}, args)
			return []byte("secret-token\n"), nil
		},
		fetchQuota: func(_ context.Context, path, gotToken string) (copilotQuotaSnapshot, error) {
			assert.Equal(t, "/bin/copilot", path)
			token = gotToken
			return copilotQuotaSnapshot{QuotaSnapshots: map[string]copilotQuotaWindow{
				"premium_interactions": {
					EntitlementRequests: 1000, UsedRequests: 582,
					RemainingPercentage: &remaining,
				},
			}}, nil
		},
		now: func() time.Time { return observed },
	}

	stored, skipped, err := refreshCopilotQuota(context.Background(), deps)
	require.NoError(t, err)
	assert.True(t, stored)
	assert.False(t, skipped)
	assert.Equal(t, "secret-token", token)

	rows, err := db.SubscriptionUsageHistorySince(observed.Add(-time.Minute))
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, db.SubscriptionProviderGitHub, rows[0].Provider)
	assert.Equal(t, "monthly", rows[0].WindowName)
	assert.Zero(t, rows[0].Duration) // account.getQuota reports no exact window start.
	assert.InDelta(t, 58.2, rows[0].UsedPercent, 1e-9)
	assert.Equal(t, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), rows[0].ResetsAt,
		"the documented monthly allowance resets on the first day at 00:00 UTC")
	assert.Equal(t, "account.getQuota", rows[0].Source)
}

func TestRefreshCopilotQuotaSkipsMissingCLIAndUnlimitedPlan(t *testing.T) {
	t.Run("missing CLI", func(t *testing.T) {
		setupTestDB(t)
		stored, skipped, err := refreshCopilotQuota(context.Background(), copilotQuotaDeps{
			lookPath: func(name string) (string, error) {
				if name == "gh" {
					return "", exec.ErrNotFound
				}
				return "/bin/copilot", nil
			},
		})
		require.NoError(t, err)
		assert.False(t, stored)
		assert.True(t, skipped)
	})

	t.Run("unlimited", func(t *testing.T) {
		setupTestDB(t)
		observed := time.Now().UTC().Add(-time.Minute)
		_, err := db.SaveSubscriptionUsageSample(db.SubscriptionUsageSample{
			Provider: db.SubscriptionProviderGitHub, ObservedAt: observed,
			Source: "account.getQuota", Windows: []db.SubscriptionUsageWindow{{
				Name: "monthly", UsedPercent: 74,
			}},
		})
		require.NoError(t, err)
		stored, skipped, err := refreshCopilotQuota(context.Background(), copilotQuotaDeps{
			lookPath: func(name string) (string, error) { return "/bin/" + name, nil },
			commandOutput: func(context.Context, string, ...string) ([]byte, error) {
				return []byte("token"), nil
			},
			fetchQuota: func(context.Context, string, string) (copilotQuotaSnapshot, error) {
				return copilotQuotaSnapshot{QuotaSnapshots: map[string]copilotQuotaWindow{
					"premium_interactions": {IsUnlimitedEntitlement: true, EntitlementRequests: -1},
				}}, nil
			},
			now: time.Now,
		})
		require.NoError(t, err)
		assert.True(t, stored, "clearing the obsolete finite series changes dashboard state")
		assert.False(t, skipped)
		rows, err := db.SubscriptionUsageHistorySince(observed.Add(-time.Minute))
		require.NoError(t, err)
		assert.Empty(t, rows, "an unlimited plan must not leave its old finite graph visible")
		_, _, current, hasHistory, err := db.LoadDashboardUsageCaches()
		require.NoError(t, err)
		assert.Nil(t, current)
		assert.False(t, hasHistory)
	})
}
