package usagerules

import (
	"fmt"
	"strconv"
	"strings"
)

// ParseStep reads the CLI's compact step syntax:
//
//	AT:ACTION[:TARGETS][:every=N%|every=Nm]
//
// e.g. "50:notify:agent", "80:notify:agent+operator:every=5%",
// "90:notify:operator:every=30m", "100:wrap-up", "110:hard-stop",
// "100:block-spawn". AT is a percentage of the budget; a trailing % is
// allowed. The result still needs Rule.Normalize and Validate.
func ParseStep(spec string) (Step, error) {
	parts := strings.Split(strings.TrimSpace(spec), ":")
	if len(parts) < 2 {
		return Step{}, fmt.Errorf("step %q: want AT:ACTION[:TARGETS][:every=N%%|Nm]", spec)
	}
	at, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(parts[0]), "%"), 64)
	if err != nil {
		return Step{}, fmt.Errorf("step %q: threshold must be a number (percent of budget)", spec)
	}
	s := Step{At: at, Action: Action(strings.ReplaceAll(strings.TrimSpace(parts[1]), "-", "_"))}
	for _, part := range parts[2:] {
		part = strings.TrimSpace(part)
		if every, ok := strings.CutPrefix(part, "every="); ok {
			switch {
			case strings.HasSuffix(every, "%"):
				v, err := strconv.ParseFloat(strings.TrimSuffix(every, "%"), 64)
				if err != nil {
					return Step{}, fmt.Errorf("step %q: bad repeat %q", spec, every)
				}
				s.RepeatEveryPct = v
			case strings.HasSuffix(every, "m"):
				v, err := strconv.Atoi(strings.TrimSuffix(every, "m"))
				if err != nil {
					return Step{}, fmt.Errorf("step %q: bad repeat %q", spec, every)
				}
				s.RepeatEveryMinutes = v
			default:
				return Step{}, fmt.Errorf("step %q: repeat must end in %% (of budget) or m (minutes)", spec)
			}
			continue
		}
		for _, t := range strings.Split(part, "+") {
			if t = strings.TrimSpace(t); t != "" {
				s.Notify = append(s.Notify, Target(t))
			}
		}
	}
	return s, nil
}

// FormatStep renders a step in ParseStep's syntax.
func FormatStep(s Step) string {
	out := strconv.FormatFloat(s.At, 'g', -1, 64) + ":" + strings.ReplaceAll(string(s.Action), "_", "-")
	if len(s.Notify) > 0 {
		targets := make([]string, len(s.Notify))
		for i, t := range s.Notify {
			targets[i] = string(t)
		}
		out += ":" + strings.Join(targets, "+")
	}
	if s.RepeatEveryPct > 0 {
		out += ":every=" + strconv.FormatFloat(s.RepeatEveryPct, 'g', -1, 64) + "%"
	}
	if s.RepeatEveryMinutes > 0 {
		out += ":every=" + strconv.Itoa(s.RepeatEveryMinutes) + "m"
	}
	return out
}
