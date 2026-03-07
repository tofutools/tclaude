package stats

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/tofutools/tclaude/pkg/common"
	"github.com/spf13/cobra"
)

// StatsCache represents the stats-cache.json file structure
type StatsCache struct {
	Version          int              `json:"version"`
	LastComputedDate string           `json:"lastComputedDate"`
	DailyActivity    []DailyActivity  `json:"dailyActivity"`
	DailyModelTokens []DailyTokens    `json:"dailyModelTokens"`
	ModelUsage       map[string]Model `json:"modelUsage"`
	TotalSessions    int              `json:"totalSessions"`
	TotalMessages    int              `json:"totalMessages"`
	LongestSession   *LongestSession  `json:"longestSession"`
	FirstSessionDate string           `json:"firstSessionDate"`
	HourCounts       map[string]int   `json:"hourCounts"`
}

type DailyActivity struct {
	Date          string `json:"date"`
	MessageCount  int    `json:"messageCount"`
	SessionCount  int    `json:"sessionCount"`
	ToolCallCount int    `json:"toolCallCount"`
}

type DailyTokens struct {
	Date          string         `json:"date"`
	TokensByModel map[string]int `json:"tokensByModel"`
}

type Model struct {
	InputTokens             int     `json:"inputTokens"`
	OutputTokens            int     `json:"outputTokens"`
	CacheReadInputTokens    int64   `json:"cacheReadInputTokens"`
	CacheCreationInputTokens int64  `json:"cacheCreationInputTokens"`
	WebSearchRequests       int     `json:"webSearchRequests"`
	CostUSD                 float64 `json:"costUSD"`
}

type LongestSession struct {
	SessionID    string `json:"sessionId"`
	Duration     int64  `json:"duration"`
	MessageCount int    `json:"messageCount"`
	Timestamp    string `json:"timestamp"`
}

type Params struct {
	Days   int  `short:"d" help:"Show activity for last N days (default: 7)" default:"7"`
	JSON   bool `long:"json" short:"j" help:"Output raw JSON"`
	Tokens bool `short:"t" help:"Show token usage details"`
}

func Cmd() *cobra.Command {
	return boa.CmdT[Params]{
		Use:         "stats",
		Short:       "Show Claude Code activity statistics",
		Long:        "Display activity statistics from Claude Code including sessions, messages, tokens, and daily activity.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(params *Params, cmd *cobra.Command, args []string) {
			if err := runUsage(params); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		},
	}.ToCobra()
}

func runUsage(params *Params) error {
	stats, err := loadStats()
	if err != nil {
		return err
	}

	if params.JSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(stats)
	}

	// Print summary
	fmt.Println("Claude Code Usage Statistics")
	fmt.Println(strings.Repeat("=", 40))
	fmt.Println()

	// Overall stats
	fmt.Printf("Total Sessions:  %d\n", stats.TotalSessions)
	fmt.Printf("Total Messages:  %s\n", formatNumber(stats.TotalMessages))
	if stats.FirstSessionDate != "" {
		if t, err := time.Parse(time.RFC3339, stats.FirstSessionDate); err == nil {
			fmt.Printf("First Session:   %s\n", t.Format("2006-01-02"))
		}
	}
	fmt.Println()

	// Model usage
	if len(stats.ModelUsage) > 0 {
		fmt.Println("Model Usage")
		fmt.Println(strings.Repeat("-", 40))
		for model, usage := range stats.ModelUsage {
			modelName := formatModelName(model)
			fmt.Printf("\n%s:\n", modelName)
			fmt.Printf("  Input Tokens:   %s\n", formatNumber(int(usage.InputTokens)))
			fmt.Printf("  Output Tokens:  %s\n", formatNumber(int(usage.OutputTokens)))
			if usage.CacheReadInputTokens > 0 {
				fmt.Printf("  Cache Read:     %s\n", formatNumber(int(usage.CacheReadInputTokens)))
			}
			if usage.CacheCreationInputTokens > 0 {
				fmt.Printf("  Cache Created:  %s\n", formatNumber(int(usage.CacheCreationInputTokens)))
			}
			if usage.CostUSD > 0 {
				fmt.Printf("  Cost:           $%.2f\n", usage.CostUSD)
			}
		}
		fmt.Println()
	}

	// Daily activity (last N days)
	if len(stats.DailyActivity) > 0 {
		fmt.Printf("Daily Activity (last %d days)\n", params.Days)
		fmt.Println(strings.Repeat("-", 40))

		// Get last N days
		activities := stats.DailyActivity
		if len(activities) > params.Days {
			activities = activities[len(activities)-params.Days:]
		}

		// Print header
		fmt.Printf("%-12s %8s %8s %8s\n", "Date", "Sessions", "Messages", "Tools")
		for _, a := range activities {
			fmt.Printf("%-12s %8d %8d %8d\n", a.Date, a.SessionCount, a.MessageCount, a.ToolCallCount)
		}

		// Totals for period
		var totalSessions, totalMessages, totalTools int
		for _, a := range activities {
			totalSessions += a.SessionCount
			totalMessages += a.MessageCount
			totalTools += a.ToolCallCount
		}
		fmt.Println(strings.Repeat("-", 44))
		fmt.Printf("%-12s %8d %8d %8d\n", "Total", totalSessions, totalMessages, totalTools)
		fmt.Println()
	}

	// Token usage by day (if requested)
	if params.Tokens && len(stats.DailyModelTokens) > 0 {
		fmt.Printf("Daily Tokens (last %d days)\n", params.Days)
		fmt.Println(strings.Repeat("-", 40))

		tokens := stats.DailyModelTokens
		if len(tokens) > params.Days {
			tokens = tokens[len(tokens)-params.Days:]
		}

		fmt.Printf("%-12s %12s\n", "Date", "Tokens")
		var total int
		for _, t := range tokens {
			dayTotal := 0
			for _, count := range t.TokensByModel {
				dayTotal += count
			}
			total += dayTotal
			fmt.Printf("%-12s %12s\n", t.Date, formatNumber(dayTotal))
		}
		fmt.Println(strings.Repeat("-", 26))
		fmt.Printf("%-12s %12s\n", "Total", formatNumber(total))
		fmt.Println()
	}

	// Peak hours
	if len(stats.HourCounts) > 0 {
		fmt.Println("Activity by Hour")
		fmt.Println(strings.Repeat("-", 40))
		printHourHistogram(stats.HourCounts)
	}

	return nil
}

func loadStats() (*StatsCache, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("could not get home directory: %w", err)
	}

	statsPath := filepath.Join(home, ".claude", "stats-cache.json")
	data, err := os.ReadFile(statsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no usage data found (run Claude Code first)")
		}
		return nil, err
	}

	var stats StatsCache
	if err := json.Unmarshal(data, &stats); err != nil {
		return nil, fmt.Errorf("failed to parse stats: %w", err)
	}

	return &stats, nil
}

func formatNumber(n int) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	if n < 1000000 {
		return fmt.Sprintf("%.1fK", float64(n)/1000)
	}
	if n < 1000000000 {
		return fmt.Sprintf("%.1fM", float64(n)/1000000)
	}
	return fmt.Sprintf("%.1fB", float64(n)/1000000000)
}

func formatModelName(model string) string {
	// Clean up model names for display
	name := model
	name = strings.TrimPrefix(name, "claude-")
	name = strings.TrimSuffix(name, "-20251101")
	name = strings.TrimSuffix(name, "-20250929")
	// Capitalize first letter
	if len(name) > 0 {
		name = strings.ToUpper(name[:1]) + name[1:]
	}
	return name
}

func printHourHistogram(hourCounts map[string]int) {
	// Find max for scaling
	maxCount := 0
	for _, count := range hourCounts {
		if count > maxCount {
			maxCount = count
		}
	}

	// Sort hours
	hours := make([]int, 0, len(hourCounts))
	for h := range hourCounts {
		var hour int
		fmt.Sscanf(h, "%d", &hour)
		hours = append(hours, hour)
	}
	sort.Ints(hours)

	// Print histogram
	const maxBar = 20
	for _, hour := range hours {
		count := hourCounts[fmt.Sprintf("%d", hour)]
		barLen := 0
		if maxCount > 0 {
			barLen = (count * maxBar) / maxCount
		}
		if barLen == 0 && count > 0 {
			barLen = 1
		}
		bar := strings.Repeat("█", barLen)
		fmt.Printf("%02d:00 %s %d\n", hour, bar, count)
	}
	fmt.Println()
}
