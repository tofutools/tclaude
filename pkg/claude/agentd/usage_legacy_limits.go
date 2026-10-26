package agentd

import (
	"log/slog"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/usagerules"
)

// migrateLegacySpendLimits converts the former cost.daily_limit_usd /
// cost.monthly_limit_usd settings into global usage limits with the same
// behavior (recorded API spend, operator alert at 80% and 100%), then clears
// them from config.json. It runs on every usage tick, so a value written by
// an older client is picked up too. Thresholds already alerted this period
// carry over, so the switch repeats no alert.
func migrateLegacySpendLimits(now time.Time) {
	cfg, err := config.Load()
	if err != nil || cfg.Cost == nil || (cfg.Cost.DailyLimitUSD <= 0 && cfg.Cost.MonthlyLimitUSD <= 0) {
		return
	}
	_, err = config.Update(func(cfg *config.Config, loadErr error) error {
		if loadErr != nil {
			return loadErr
		}
		if cfg.Cost == nil {
			return nil
		}
		limits, err := db.ListUsageLimits()
		if err != nil {
			return err
		}
		for _, legacy := range []struct {
			amount *float64
			name   string
			period usagerules.Period
			legacy string
			start  string
		}{
			{&cfg.Cost.DailyLimitUSD, "Daily API spend", usagerules.PeriodDay, "daily", now.Format("2006-01-02")},
			{&cfg.Cost.MonthlyLimitUSD, "Monthly API spend", usagerules.PeriodMonth, "monthly", now.Format("2006-01")},
		} {
			if *legacy.amount <= 0 {
				*legacy.amount = 0
				continue
			}
			r := usagerules.Rule{Name: legacy.name, Metric: usagerules.MetricRealUSD, Period: legacy.period, Budget: *legacy.amount,
				Steps: []usagerules.Step{
					{At: 80, Notify: []usagerules.Target{usagerules.TargetOperator}},
					{At: 100, Notify: []usagerules.Target{usagerules.TargetOperator}},
				}}
			r.Normalize()
			if err := r.Validate(usagerules.ScopeGlobal); err != nil {
				slog.Warn("usage rules: legacy spend limit is invalid; dropping it", "period", legacy.legacy, "error", err)
				*legacy.amount = 0
				continue
			}
			// Idempotent: a value converted before (a failed config save, or a
			// newer value typed into the old form) updates that limit rather
			// than adding another one.
			if existing := legacyUsageLimit(limits, legacy.name); existing != nil {
				existing.Rule.Budget, existing.Enabled = r.Budget, true
				if _, err := db.UpdateUsageLimit(existing); err != nil {
					return err
				}
				slog.Info("usage rules: updated the global limit converted from a legacy spend limit", "period", legacy.legacy, "budget", r.Budget)
				*legacy.amount = 0
				continue
			}
			periodKey := usagerules.PeriodKey(r.Period, now, time.Time{}) + "|b=" + formatUsageBudget(r.Budget)
			stepKey := func(threshold int) string {
				return usagerules.Step{At: float64(threshold), Action: usagerules.ActionNotify}.Key()
			}
			if err := db.ImportLegacySpendLimit(&db.UsageLimit{Scope: db.UsageScopeGlobal, Rule: r, Enabled: true},
				legacy.legacy, legacy.start, periodKey, stepKey); err != nil {
				return err
			}
			slog.Info("usage rules: converted legacy spend limit into a global usage limit", "period", legacy.legacy, "budget", r.Budget)
			*legacy.amount = 0
		}
		return nil
	})
	if err != nil {
		slog.Warn("usage rules: convert legacy spend limits", "error", err)
	}
}

func legacyUsageLimit(limits []*db.UsageLimit, name string) *db.UsageLimit {
	for _, l := range limits {
		if l.Scope == db.UsageScopeGlobal && l.Rule.Name == name {
			return l
		}
	}
	return nil
}
