package agentd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os/exec"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

const (
	copilotQuotaPollInterval = 15 * time.Minute
	copilotQuotaTimeout      = 30 * time.Second
)

type copilotQuotaDeps struct {
	lookPath      func(string) (string, error)
	commandOutput func(context.Context, string, ...string) ([]byte, error)
	fetchQuota    func(context.Context, string, string) (copilotQuotaSnapshot, error)
	now           func() time.Time
}

var defaultCopilotQuotaDeps = copilotQuotaDeps{
	lookPath: exec.LookPath,
	commandOutput: func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, name, args...).CombinedOutput()
	},
	fetchQuota: fetchCopilotQuota,
	now:        time.Now,
}

// copilotQuotaSnapshot is Copilot CLI's normalized account.getQuota result.
// The CLI deliberately owns normalization because GitHub has shipped several
// raw quota shapes; tclaude consumes only the stable account-level fields.
type copilotQuotaSnapshot struct {
	QuotaSnapshots map[string]copilotQuotaWindow `json:"quotaSnapshots"`
}

type copilotQuotaWindow struct {
	IsUnlimitedEntitlement bool     `json:"isUnlimitedEntitlement"`
	EntitlementRequests    float64  `json:"entitlementRequests"`
	UsedRequests           float64  `json:"usedRequests"`
	RemainingPercentage    *float64 `json:"remainingPercentage"`
}

func startCopilotQuotaPoller(stop <-chan struct{}) {
	go func() {
		refreshCopilotQuotaAndLog(context.Background(), "startup")
		ticker := time.NewTicker(copilotQuotaPollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				refreshCopilotQuotaAndLog(context.Background(), "scheduled")
			}
		}
	}()
}

func refreshCopilotQuotaAndLog(parent context.Context, trigger string) {
	ctx, cancel := context.WithTimeout(parent, copilotQuotaTimeout)
	defer cancel()
	stored, skipped, err := refreshCopilotQuota(ctx, defaultCopilotQuotaDeps)
	if err != nil {
		slog.Debug("copilot-quota: refresh failed; preserving last reading", "trigger", trigger, "error", err)
		return
	}
	if skipped {
		slog.Debug("copilot-quota: refresh skipped", "trigger", trigger)
		return
	}
	slog.Debug("copilot-quota: refresh complete", "trigger", trigger, "stored", stored)
}

// refreshCopilotQuota performs one read-only account.getQuota call and stores
// the finite premium-interactions allowance as GitHub's monthly quota series.
// Unlimited plans have no percentage limit to graph and are an intentional
// no-op. A failed refresh never disturbs the last retained sample.
func refreshCopilotQuota(ctx context.Context, deps copilotQuotaDeps) (stored, skipped bool, err error) {
	paths := make(map[string]string, 2)
	for _, name := range []string{"copilot", "gh"} {
		path, lookErr := deps.lookPath(name)
		if lookErr != nil {
			if errors.Is(lookErr, exec.ErrNotFound) {
				return false, true, nil
			}
			return false, false, fmt.Errorf("find %s CLI: %w", name, lookErr)
		}
		paths[name] = path
	}
	tokenOutput, err := deps.commandOutput(ctx, paths["gh"], "auth", "token")
	if err != nil {
		return false, false, fmt.Errorf("gh authentication unavailable: %w", err)
	}
	token := strings.TrimSpace(string(tokenOutput))
	if token == "" {
		return false, false, errors.New("gh authentication unavailable: gh auth token returned an empty token")
	}
	quota, err := deps.fetchQuota(ctx, paths["copilot"], token)
	if err != nil {
		return false, false, err
	}
	window, ok := quota.QuotaSnapshots["premium_interactions"]
	if !ok || window.IsUnlimitedEntitlement || window.EntitlementRequests <= 0 {
		cleared, clearErr := db.DeleteSubscriptionUsageProvider(db.SubscriptionProviderGitHub)
		return cleared, !cleared, clearErr
	}
	usedPercent := 0.0
	if window.RemainingPercentage != nil {
		usedPercent = 100 - *window.RemainingPercentage
	} else {
		usedPercent = window.UsedRequests / window.EntitlementRequests * 100
	}
	if math.IsNaN(usedPercent) || math.IsInf(usedPercent, 0) {
		return false, false, errors.New("copilot quota returned a non-finite percentage")
	}
	usedPercent = math.Max(0, math.Min(100, usedPercent))
	observedAt := deps.now().UTC()
	resetAt := copilotMonthlyResetAt(observedAt)
	stored, err = db.SaveSubscriptionUsageSample(db.SubscriptionUsageSample{
		Provider: db.SubscriptionProviderGitHub, ObservedAt: observedAt,
		Source: "account.getQuota",
		Windows: []db.SubscriptionUsageWindow{{
			Name:        "monthly",
			UsedPercent: usedPercent, ResetsAt: resetAt,
		}},
	})
	return stored, false, err
}

func copilotMonthlyResetAt(observedAt time.Time) time.Time {
	observedAt = observedAt.UTC()
	return time.Date(observedAt.Year(), observedAt.Month()+1, 1, 0, 0, 0, 0, time.UTC)
}

func fetchCopilotQuota(ctx context.Context, copilotPath, githubToken string) (copilotQuotaSnapshot, error) {
	var result copilotQuotaSnapshot
	params := struct {
		GitHubToken string `json:"gitHubToken"`
	}{GitHubToken: githubToken}
	if err := callCopilotOneShotRPC(ctx, copilotPath, "account.getQuota", params, &result); err != nil {
		return copilotQuotaSnapshot{}, fmt.Errorf("read Copilot account quota: %w", err)
	}
	return result, nil
}
