package agentd

import (
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

type dashboardSpendLimit struct {
	Period      string  `json:"period"`
	PeriodStart string  `json:"period_start"`
	SpendUSD    float64 `json:"spend_usd"`
	LimitUSD    float64 `json:"limit_usd"`
	Threshold   int     `json:"threshold"` // 0, 80, or 100
}

// evaluateSpendLimits uses raw real-cost ledger amounts, independent of the
// display multiplier and hypothetical subscription equivalents. Local calendar
// boundaries match the dashboard's today/month readouts.
func evaluateSpendLimits(cfg *config.Config, month, today float64, now time.Time) []dashboardSpendLimit {
	if cfg == nil || cfg.Cost == nil {
		return nil
	}
	var limits []dashboardSpendLimit
	for _, item := range []dashboardSpendLimit{
		{Period: "daily", PeriodStart: now.Format("2006-01-02"), SpendUSD: today, LimitUSD: cfg.Cost.DailyLimitUSD},
		{Period: "monthly", PeriodStart: now.Format("2006-01"), SpendUSD: month, LimitUSD: cfg.Cost.MonthlyLimitUSD},
	} {
		if item.LimitUSD <= 0 || math.IsNaN(item.LimitUSD) || math.IsInf(item.LimitUSD, 0) {
			continue
		}
		for _, threshold := range []int{80, 100} {
			if item.SpendUSD < item.LimitUSD*float64(threshold)/100 {
				continue
			}
			item.Threshold = threshold
			subject := fmt.Sprintf("Estimated %s spend reached %d%%", item.Period, threshold)
			body := fmt.Sprintf("Estimated %s API spend: $%.2f / $%.2f limit (%s, local calendar). Estimates from the cost ledger, before display multipliers; subscription WHAT-IF amounts are excluded. These estimates may differ from real charges. This alert does not block agent spawns.", item.Period, item.SpendUSD, item.LimitUSD, item.PeriodStart)
			message := &db.HumanMessage{FromTitle: "Spend limits", Subject: subject, Body: body, CreatedAt: now}
			inserted, err := db.InsertSpendAlert(item.Period, item.PeriodStart, threshold, message)
			if err != nil {
				slog.Warn("record spend alert", "error", err)
			} else if inserted {
				dispatchHumanMessageNotification("", message.FromTitle, "", subject, body)
			}
		}
		limits = append(limits, item)
	}
	return limits
}

// pollSpendLimits also runs without an open browser or Anthropic API polling.
func pollSpendLimits(cfg *config.Config) {
	if cfg == nil || cfg.Cost == nil || (cfg.Cost.DailyLimitUSD <= 0 && cfg.Cost.MonthlyLimitUSD <= 0) {
		return
	}
	rows, _, _, err := countedCostDailyRows()
	if err != nil {
		slog.Warn("read spend limit ledger", "error", err)
		return
	}
	now := time.Now()
	month, today := dashboardCostTotalsFromDeltas(displayedCostDeltasFromRows(rows, false), now, "real")
	evaluateSpendLimits(cfg, month, today, now)
}
