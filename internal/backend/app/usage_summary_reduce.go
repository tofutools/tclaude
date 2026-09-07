package app

import (
	"fmt"
	"math/big"
	"sort"
	"strings"

	"github.com/tofutools/tclaude/internal/backend/model"
)

type summaryKey struct {
	Cumulative           bool
	Harness, Source, Day string
	Historical           bool
	Attribution          model.UsageAttribution
}
type summaryCostKey struct {
	Currency string
	Kind     model.UsageCostKind
}
type summaryAccumulator struct {
	row      UsageSummaryRow
	counters map[model.UsageUnit]*big.Int
	costs    map[summaryCostKey]*big.Rat
}

func summarizeUsage(filter UsageSummaryFilter, samples []UsageSummarySample) (UsageSummaryResult, error) {
	result := UsageSummaryResult{Filter: filter, Basis: "Observed changes in UTC; cumulative readings require a compatible prior baseline. Gaps are excluded from totals. Source and historical ledgers remain separate."}
	previous := map[string]UsageSummarySample{}
	rows := map[summaryKey]*summaryAccumulator{}
	for _, sample := range samples {
		current := sample.Observation
		prior, hasPrior := previous[sample.SourceKey]
		previous[sample.SourceKey] = sample
		if current.ObservedAt.Before(filter.After) {
			continue
		}
		if !current.ObservedAt.Before(filter.Before) {
			continue
		}
		result.Observations++
		key := summaryKey{sample.Cumulative, current.Harness, current.Source, current.ObservedAt.UTC().Format("2006-01-02"), current.Historical, current.Attribution}
		row := rows[key]
		if row == nil {
			row = &summaryAccumulator{row: UsageSummaryRow{Cumulative: key.Cumulative, Harness: key.Harness, Source: key.Source, Historical: key.Historical, Attribution: key.Attribution, Day: key.Day}, counters: map[model.UsageUnit]*big.Int{}, costs: map[summaryCostKey]*big.Rat{}}
			rows[key] = row
		}
		row.row.Observations++
		gap := func(reason string) {
			result.Gaps = append(result.Gaps, UsageSummaryGap{ObservationID: current.ID, ObservedAt: current.ObservedAt, Attribution: current.Attribution, Harness: current.Harness, Source: current.Source, Reason: reason})
		}
		compatible := hasPrior && prior.Cumulative && prior.Observation.Harness == current.Harness && prior.Observation.Source == current.Source && prior.Observation.Historical == current.Historical && prior.Observation.Attribution == current.Attribution
		if sample.Cumulative && !compatible {
			row.row.MissingBaselines++
			gap("Cumulative reading has no compatible prior baseline; lifetime total excluded from interval changes.")
			continue
		}
		counters, err := summaryCounterValues(current.Counters)
		if err != nil {
			return result, err
		}
		before, err := summaryCounterValues(prior.Observation.Counters)
		if err != nil {
			return result, err
		}
		if current.Coverage.Counters != model.UsageCoverageComplete || sample.Cumulative && prior.Observation.Coverage.Counters != model.UsageCoverageComplete {
			row.row.PartialObservations++
			gap("Counter coverage is incomplete; counter change excluded.")
		} else {
			for unit, value := range counters {
				delta := new(big.Int).SetInt64(value)
				if sample.Cumulative {
					old, exists := before[unit]
					if !exists {
						gap("Counter has no prior unit baseline: " + string(unit))
						continue
					}
					if value < old {
						row.row.Resets++
						gap("Counter decreased; reset or correction cannot be assigned as usage: " + string(unit))
						continue
					}
					delta.Sub(delta, big.NewInt(old))
				}
				if row.counters[unit] == nil {
					row.counters[unit] = new(big.Int)
				}
				row.counters[unit].Add(row.counters[unit], delta)
			}
		}
		if current.Cost == nil || current.Coverage.Cost != model.UsageCoverageComplete {
			row.row.UnpricedObservations++
			continue
		}
		cost, ok := new(big.Rat).SetString(current.Cost.Amount)
		if !ok || cost.Sign() < 0 {
			return result, fmt.Errorf("%w: malformed recorded usage cost", ErrInvalid)
		}
		if sample.Cumulative {
			old := prior.Observation.Cost
			if old == nil || old.Currency != current.Cost.Currency || old.Kind != current.Cost.Kind || prior.Observation.Coverage.Cost != model.UsageCoverageComplete {
				gap("Cost has no compatible prior currency/kind baseline; cost change excluded.")
				continue
			}
			value, ok := new(big.Rat).SetString(old.Amount)
			if !ok || value.Sign() < 0 {
				return result, ErrInvalid
			}
			cost.Sub(cost, value)
			if cost.Sign() < 0 {
				row.row.Resets++
				gap("Cost decreased; reset or correction excluded.")
				continue
			}
		}
		costKey := summaryCostKey{current.Cost.Currency, current.Cost.Kind}
		if row.costs[costKey] == nil {
			row.costs[costKey] = new(big.Rat)
		}
		row.costs[costKey].Add(row.costs[costKey], cost)
	}
	for _, value := range rows {
		for unit, count := range value.counters {
			value.row.Counters = append(value.row.Counters, ExactUsageCounter{Unit: unit, Value: count.String()})
		}
		sort.Slice(value.row.Counters, func(i, j int) bool { return value.row.Counters[i].Unit < value.row.Counters[j].Unit })
		for key, cost := range value.costs {
			value.row.Costs = append(value.row.Costs, ExactUsageCost{Amount: exactSummaryAmount(cost), Currency: key.Currency, Kind: key.Kind})
		}
		sort.Slice(value.row.Costs, func(i, j int) bool {
			a, b := value.row.Costs[i], value.row.Costs[j]
			if a.Currency != b.Currency {
				return a.Currency < b.Currency
			}
			return a.Kind < b.Kind
		})
		result.Rows = append(result.Rows, value.row)
	}
	sort.Slice(result.Rows, func(i, j int) bool {
		a, b := result.Rows[i], result.Rows[j]
		return fmt.Sprintf("%s/%s/%s/%t/%t/%v", a.Day, a.Harness, a.Source, a.Historical, a.Cumulative, a.Attribution) < fmt.Sprintf("%s/%s/%s/%t/%t/%v", b.Day, b.Harness, b.Source, b.Historical, b.Cumulative, b.Attribution)
	})
	return result, nil
}

func summaryCounterValues(counters []model.UsageCounter) (map[model.UsageUnit]int64, error) {
	result := map[model.UsageUnit]int64{}
	for _, counter := range counters {
		if _, found := result[counter.Unit]; found || counter.Value < 0 {
			return nil, ErrInvalid
		}
		result[counter.Unit] = counter.Value
	}
	return result, nil
}

func exactSummaryAmount(value *big.Rat) string {
	denominator := new(big.Int).Set(value.Denom())
	two, five := 0, 0
	for _, factor := range []struct {
		value int64
		count *int
	}{{2, &two}, {5, &five}} {
		for new(big.Int).Mod(denominator, big.NewInt(factor.value)).Sign() == 0 {
			denominator.Div(denominator, big.NewInt(factor.value))
			*factor.count++
		}
	}
	if denominator.Cmp(big.NewInt(1)) != 0 {
		return value.RatString()
	}
	digits := max(two, five)
	amount := value.FloatString(digits)
	if strings.Contains(amount, ".") {
		amount = strings.TrimRight(strings.TrimRight(amount, "0"), ".")
	}
	return amount
}
