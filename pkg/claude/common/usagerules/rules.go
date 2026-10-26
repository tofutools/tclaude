// Package usagerules is the pure model behind usage limits and usage
// profiles: a rule measures one metric over one period against a budget and
// walks a ladder of steps (notify, wrap up, hard stop, block spawns) as the
// measured value crosses each step's percentage of that budget.
//
// The same Rule shape serves both scopes. A limit measures an aggregate
// (the whole instance, a group, or a provider's account window) with one
// counter; a usage profile is a bundle of rules applied to each agent
// individually, so every agent gets its own counter. Storage, resolution and
// actuation live in db and agentd; this package only validates rules and
// decides which steps are due.
package usagerules

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Metric selects what a rule measures.
type Metric string

const (
	// MetricTotalUSD is the dashboard's cost figure: recorded API spend where a
	// session has it, otherwise its subscription-equivalent (what-if) estimate.
	MetricTotalUSD Metric = "total_usd"
	// MetricRealUSD is recorded API spend only.
	MetricRealUSD Metric = "real_usd"
	// MetricWhatIfUSD is the subscription-equivalent estimate only.
	MetricWhatIfUSD Metric = "whatif_usd"
	// MetricWindowPct is a provider's account-wide quota window (5h / 7d).
	// It cannot be attributed to an agent, so only global limits accept it.
	MetricWindowPct Metric = "window_pct"
)

// Period selects the span a rule's counter covers.
type Period string

const (
	PeriodLifetime Period = "lifetime" // the agent's whole life, across reincarnations
	PeriodDay      Period = "day"      // daemon-local calendar day
	PeriodWeek     Period = "week"     // ISO week, daemon-local
	PeriodMonth    Period = "month"    // daemon-local calendar month
	PeriodWindow   Period = "window"   // the provider window itself (window_pct only)
)

// Action is what a step does when the measured value crosses it.
type Action string

const (
	// ActionNotify messages the step's targets.
	ActionNotify Action = "notify"
	// ActionWrapUp asks the agent(s) to write a handoff summary and go idle.
	ActionWrapUp Action = "wrap_up"
	// ActionHardStop kills the agent process(es).
	ActionHardStop Action = "hard_stop"
	// ActionBlockSpawn refuses new spawns into the scope until the period
	// rolls over. Limits only.
	ActionBlockSpawn Action = "block_spawn"
)

// Target is who a notify step reaches. For a limit, TargetAgent means every
// active agent in the limit's scope.
type Target string

const (
	TargetAgent    Target = "agent"
	TargetOperator Target = "operator"
)

// Scope says where a rule is used; validation depends on it.
type Scope string

const (
	ScopeAgent  Scope = "agent"  // a usage-profile rule, measured per agent
	ScopeGlobal Scope = "global" // a global limit
	ScopeGroup  Scope = "group"  // a group limit
)

// Step is one rung of a rule's ladder. At is a percentage of the rule's
// budget. RepeatEveryPct re-fires the step every that many percentage points
// above At; RepeatEveryMinutes re-fires it on a timer while the value stays at
// or above At. Repeats only apply to notify steps.
type Step struct {
	At                 float64  `json:"at"`
	Action             Action   `json:"action"`
	Notify             []Target `json:"notify,omitempty"`
	RepeatEveryPct     float64  `json:"repeat_every_pct,omitempty"`
	RepeatEveryMinutes int      `json:"repeat_every_minutes,omitempty"`
}

// Rule is a metric measured over a period against a budget, with a ladder of
// steps. Budget is in USD for the dollar metrics; window_pct rules measure a
// percentage directly and keep Budget at 100.
type Rule struct {
	Name   string `json:"name"`
	Metric Metric `json:"metric"`
	Period Period `json:"period"`
	// Window names the provider window for window_pct, "<provider>:<window>",
	// e.g. "anthropic:five_hour" or "openai:seven_day".
	Window string  `json:"window,omitempty"`
	Budget float64 `json:"budget"`
	// DryRun records firings and tells the operator what would have happened,
	// without messaging agents, stopping them or blocking spawns.
	DryRun bool   `json:"dry_run,omitempty"`
	Steps  []Step `json:"steps"`
}

var ruleNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 _.-]{0,63}$`)

// windowRE matches "<provider>:<window>".
var windowRE = regexp.MustCompile(`^[a-z0-9_-]{1,32}:[a-z0-9_-]{1,32}$`)

// MaxSteps bounds a ladder so a rule stays readable and cheap to evaluate.
const MaxSteps = 16

// Normalize fills defaults and orders the ladder by At. It does not validate.
func (r *Rule) Normalize() {
	r.Name = strings.TrimSpace(r.Name)
	if r.Metric == "" {
		r.Metric = MetricTotalUSD
	}
	if r.Metric == MetricWindowPct {
		r.Period = PeriodWindow
		r.Budget = 100
	}
	for i := range r.Steps {
		if r.Steps[i].Action == "" {
			r.Steps[i].Action = ActionNotify
		}
		if r.Steps[i].Action != ActionNotify {
			r.Steps[i].RepeatEveryPct = 0
			r.Steps[i].RepeatEveryMinutes = 0
		}
		r.Steps[i].Notify = dedupTargets(r.Steps[i].Notify)
	}
	sort.SliceStable(r.Steps, func(i, j int) bool { return r.Steps[i].At < r.Steps[j].At })
}

func dedupTargets(in []Target) []Target {
	var out []Target
	seen := map[Target]bool{}
	for _, t := range in {
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// Validate checks a normalized rule for the given scope.
func (r Rule) Validate(scope Scope) error {
	if !ruleNameRE.MatchString(r.Name) {
		return errors.New("rule name must be 1-64 letters, digits, spaces, '_', '.' or '-'")
	}
	switch r.Metric {
	case MetricTotalUSD, MetricRealUSD, MetricWhatIfUSD:
		if !finite(r.Budget) || r.Budget <= 0 {
			return fmt.Errorf("rule %q: budget must be a positive amount", r.Name)
		}
		switch r.Period {
		case PeriodDay, PeriodWeek, PeriodMonth:
		case PeriodLifetime:
			if scope != ScopeAgent {
				return fmt.Errorf("rule %q: a lifetime period applies to agent rules only", r.Name)
			}
		default:
			return fmt.Errorf("rule %q: unknown period %q", r.Name, r.Period)
		}
		if r.Window != "" {
			return fmt.Errorf("rule %q: window applies to window_pct rules only", r.Name)
		}
	case MetricWindowPct:
		if scope != ScopeGlobal {
			return fmt.Errorf("rule %q: provider quota windows are account-wide; use them in a global limit", r.Name)
		}
		if !windowRE.MatchString(r.Window) {
			return fmt.Errorf("rule %q: window must look like \"anthropic:five_hour\"", r.Name)
		}
	default:
		return fmt.Errorf("rule %q: unknown metric %q", r.Name, r.Metric)
	}
	if len(r.Steps) == 0 {
		return fmt.Errorf("rule %q: add at least one step", r.Name)
	}
	if len(r.Steps) > MaxSteps {
		return fmt.Errorf("rule %q: at most %d steps", r.Name, MaxSteps)
	}
	seen := map[string]bool{}
	for _, s := range r.Steps {
		if !finite(s.At) || s.At <= 0 || s.At > 1000 {
			return fmt.Errorf("rule %q: step thresholds must be between 0 and 1000%% of the budget", r.Name)
		}
		key := s.Key()
		if seen[key] {
			return fmt.Errorf("rule %q: duplicate step %s", r.Name, key)
		}
		seen[key] = true
		switch s.Action {
		case ActionNotify:
			if len(s.Notify) == 0 {
				return fmt.Errorf("rule %q: notify step at %g%% needs a target", r.Name, s.At)
			}
			for _, t := range s.Notify {
				if t != TargetAgent && t != TargetOperator {
					return fmt.Errorf("rule %q: unknown notify target %q", r.Name, t)
				}
			}
			if !finite(s.RepeatEveryPct) || s.RepeatEveryPct < 0 {
				return fmt.Errorf("rule %q: repeat_every_pct must be positive", r.Name)
			}
			if s.RepeatEveryPct > 0 && s.RepeatEveryPct < 1 {
				return fmt.Errorf("rule %q: repeat_every_pct must be at least 1", r.Name)
			}
			if s.RepeatEveryMinutes < 0 || (s.RepeatEveryMinutes > 0 && s.RepeatEveryMinutes < 5) {
				return fmt.Errorf("rule %q: repeat_every_minutes must be at least 5", r.Name)
			}
		case ActionWrapUp, ActionHardStop:
			if len(s.Notify) > 0 {
				return fmt.Errorf("rule %q: only notify steps take targets", r.Name)
			}
		case ActionBlockSpawn:
			if scope == ScopeAgent {
				return fmt.Errorf("rule %q: block_spawn applies to global and group limits only", r.Name)
			}
			if len(s.Notify) > 0 {
				return fmt.Errorf("rule %q: only notify steps take targets", r.Name)
			}
		default:
			return fmt.Errorf("rule %q: unknown action %q", r.Name, s.Action)
		}
	}
	return nil
}

// Key identifies a step within its rule, stable across reordering.
func (s Step) Key() string {
	return string(s.Action) + "@" + strconv.FormatFloat(s.At, 'g', -1, 64)
}

// PeriodKey identifies the counter span containing now. Firings are deduped
// per period key, so a step fires once per span. windowResetsAt identifies a
// provider window span; zero reads as "unknown".
func PeriodKey(p Period, now time.Time, windowResetsAt time.Time) string {
	switch p {
	case PeriodDay:
		return now.Format("2006-01-02")
	case PeriodWeek:
		y, w := now.ISOWeek()
		return fmt.Sprintf("%04d-W%02d", y, w)
	case PeriodMonth:
		return now.Format("2006-01")
	case PeriodWindow:
		if windowResetsAt.IsZero() {
			return "window"
		}
		// Providers jitter reported reset times by seconds; minutes are stable.
		return windowResetsAt.UTC().Truncate(time.Minute).Format(time.RFC3339)
	default:
		return "lifetime"
	}
}

// PeriodStartDay is the first daemon-local day key (YYYY-MM-DD) inside the
// span containing now, or "" for an unbounded lifetime span. Window spans are
// not day-bounded and also return "".
func PeriodStartDay(p Period, now time.Time) string {
	switch p {
	case PeriodDay:
		return now.Format("2006-01-02")
	case PeriodWeek:
		offset := (int(now.Weekday()) + 6) % 7 // Monday = 0
		return now.AddDate(0, 0, -offset).Format("2006-01-02")
	case PeriodMonth:
		return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location()).Format("2006-01-02")
	default:
		return ""
	}
}

// Firing is one step occurrence that is due. Key is unique per occurrence
// within a (rule, subject, period, budget): the base crossing uses the step
// key, repeats append "+p<n>" (percentage repeats) or "+t<n>" (timer repeats).
type Firing struct {
	Key    string
	Step   Step
	Repeat bool
}

// Pct converts a measured value to a percentage of budget.
func Pct(value, budget float64) float64 {
	if budget <= 0 || !finite(value) {
		return 0
	}
	return value / budget * 100
}

// Due lists the firings a rule owes at pct (percentage of budget). baseFired
// reports when a step's base crossing was recorded, which anchors timer
// repeats. Due is idempotent: callers dedupe by Key, and for each step only
// the newest repeat is listed, so a jump over several repeat marks sends one
// reminder rather than a burst.
func Due(r Rule, pct float64, now time.Time, baseFired func(stepKey string) (time.Time, bool)) []Firing {
	var out []Firing
	for _, s := range r.Steps {
		if pct < s.At {
			continue
		}
		key := s.Key()
		out = append(out, Firing{Key: key, Step: s})
		if s.Action != ActionNotify {
			continue
		}
		if s.RepeatEveryPct > 0 {
			if n := int(math.Floor((pct - s.At) / s.RepeatEveryPct)); n > 0 {
				out = append(out, Firing{Key: fmt.Sprintf("%s+p%d", key, n), Step: s, Repeat: true})
			}
		}
		if s.RepeatEveryMinutes > 0 && baseFired != nil {
			if at, ok := baseFired(key); ok {
				every := time.Duration(s.RepeatEveryMinutes) * time.Minute
				if n := int(now.Sub(at) / every); n > 0 {
					out = append(out, Firing{Key: fmt.Sprintf("%s+t%d", key, n), Step: s, Repeat: true})
				}
			}
		}
	}
	return out
}

// MaxRulesPerProfile bounds a usage profile.
const MaxRulesPerProfile = 16

// ValidateProfileRules normalizes and validates a usage profile's rules: each
// must be a valid agent rule and names must be unique, because a firing is
// keyed by the rule name.
func ValidateProfileRules(rules []Rule) ([]Rule, error) {
	if len(rules) > MaxRulesPerProfile {
		return nil, fmt.Errorf("a usage profile holds at most %d rules", MaxRulesPerProfile)
	}
	out := make([]Rule, len(rules))
	seen := map[string]bool{}
	for i, r := range rules {
		r.Normalize()
		if err := r.Validate(ScopeAgent); err != nil {
			return nil, err
		}
		lower := strings.ToLower(r.Name)
		if seen[lower] {
			return nil, fmt.Errorf("duplicate rule name %q", r.Name)
		}
		seen[lower] = true
		out[i] = r
	}
	return out, nil
}
