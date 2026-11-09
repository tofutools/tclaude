package config

import (
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/selfupdate"
)

// SelfUpdateConfig is how tclaude updates its own binaries (updates.self).
// Method stays empty until the operator chooses one: no update is applied
// before that choice, whatever asks for it.
type SelfUpdateConfig struct {
	// Method is one of selfupdate.MethodIDs, or empty for "not chosen yet".
	Method string `json:"method,omitempty"`
	// Schedule runs the chosen method periodically. Absent means off.
	Schedule *SelfUpdateSchedule `json:"schedule,omitempty"`
	// UpdateOnStart checks (and with an applying method, applies) an update
	// each time the daemon starts.
	UpdateOnStart bool `json:"update_on_start,omitempty"`
	// PostUpdateSetup runs the NEW binary's `tclaude setup --yes` with an
	// allowlisted set of flags after a successful update.
	PostUpdateSetup *PostUpdateSetupConfig `json:"post_update_setup,omitempty"`
	// SourceCheckout is the local checkout the source_checkout method builds.
	SourceCheckout *SourceCheckoutConfig `json:"source_checkout,omitempty"`
	// Custom is the operator's own update command for the custom method.
	Custom *CustomUpdateConfig `json:"custom,omitempty"`
	// PackageManagerAck acknowledges that a tclaude-driven method installs
	// next to a package-manager-owned binary (into InstallDir) rather than
	// over it. Required before such an update runs.
	PackageManagerAck bool `json:"package_manager_ack,omitempty"`
	// InstallDir is where tclaude installs when the existing binary belongs
	// to a package manager. Default ~/.local/bin.
	InstallDir string `json:"install_dir,omitempty"`
}

// SelfUpdateSchedule is a daily or weekly local-time window.
type SelfUpdateSchedule struct {
	Frequency               string `json:"frequency,omitempty"` // off | daily | weekly
	Weekday                 string `json:"weekday,omitempty"`   // weekly: monday..sunday
	WindowStart             string `json:"window_start,omitempty"`
	WindowEnd               string `json:"window_end,omitempty"`
	DeferWhileAgentsWorking bool   `json:"defer_while_agents_working,omitempty"`
}

// PostUpdateSetupConfig selects `tclaude setup` flags from SetupFlags; it is
// never free text.
type PostUpdateSetupConfig struct {
	Enabled bool     `json:"enabled,omitempty"`
	Flags   []string `json:"flags,omitempty"`
	Harness string   `json:"harness,omitempty"`
}

// SourceCheckoutConfig names an operator-owned git checkout of tclaude.
// Ref empty means the release tag being installed.
type SourceCheckoutConfig struct {
	Path string `json:"path,omitempty"`
	Ref  string `json:"ref,omitempty"`
}

// CustomUpdateConfig is an operator-written update command, run with
// /bin/sh -c like updates.scripts.
type CustomUpdateConfig struct {
	Command        string `json:"command,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
}

const (
	ScheduleOff    = "off"
	ScheduleDaily  = "daily"
	ScheduleWeekly = "weekly"

	DefaultSelfUpdateWindowStart = "03:00"
	DefaultSelfUpdateWindowEnd   = "05:00"
	DefaultSelfUpdateWeekday     = "monday"
	DefaultCustomUpdateTimeout   = 1800
	DefaultSelfUpdateInstallDir  = "~/.local/bin"
)

// SetupFlags is the allowlist of `tclaude setup` flags a post-update setup may
// pass (without the leading dashes).
var SetupFlags = []string{
	"install-all",
	"install-agent-skills",
	"install-default-agent-permissions",
	"install-utility-skills",
	"install-sandbox-hardening",
	"install-proxy-skills",
	"install-resume-threshold-override",
	"all-harnesses",
	"absolute-paths",
}

// SetupHarnesses are the values `tclaude setup --harness` accepts.
var SetupHarnesses = []string{"claude", "codex", "opencode", "copilot", "gemini", "pi"}

var weekdays = []string{"sunday", "monday", "tuesday", "wednesday", "thursday", "friday", "saturday"}
var gitRefPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$`)

// SelfUpdate returns the effective updates.self with defaults filled in.
// Nil-safe.
func (c *Config) SelfUpdate() SelfUpdateConfig {
	var s SelfUpdateConfig
	if c != nil && c.Updates != nil && c.Updates.Self != nil {
		s = *c.Updates.Self
	}
	return s.Resolved()
}

// Resolved returns a deep copy with every default filled in.
func (s SelfUpdateConfig) Resolved() SelfUpdateConfig {
	out := s
	sched := SelfUpdateSchedule{}
	if s.Schedule != nil {
		sched = *s.Schedule
	}
	if sched.Frequency == "" {
		sched.Frequency = ScheduleOff
	}
	if sched.Weekday == "" {
		sched.Weekday = DefaultSelfUpdateWeekday
	}
	if sched.WindowStart == "" {
		sched.WindowStart = DefaultSelfUpdateWindowStart
	}
	if sched.WindowEnd == "" {
		sched.WindowEnd = DefaultSelfUpdateWindowEnd
	}
	out.Schedule = &sched
	setup := PostUpdateSetupConfig{Flags: []string{}}
	if s.PostUpdateSetup != nil {
		setup = *s.PostUpdateSetup
		setup.Flags = append([]string{}, s.PostUpdateSetup.Flags...)
	}
	out.PostUpdateSetup = &setup
	checkout := SourceCheckoutConfig{}
	if s.SourceCheckout != nil {
		checkout = *s.SourceCheckout
	}
	out.SourceCheckout = &checkout
	custom := CustomUpdateConfig{}
	if s.Custom != nil {
		custom = *s.Custom
	}
	if custom.TimeoutSeconds == 0 {
		custom.TimeoutSeconds = DefaultCustomUpdateTimeout
	}
	out.Custom = &custom
	if out.InstallDir == "" {
		out.InstallDir = DefaultSelfUpdateInstallDir
	}
	return out
}

// SetupArgs is the argv after the tclaude binary for the post-update setup.
func (p PostUpdateSetupConfig) SetupArgs() []string {
	args := []string{"setup", "--yes"}
	for _, f := range p.Flags {
		args = append(args, "--"+f)
	}
	if p.Harness != "" {
		args = append(args, "--harness", p.Harness)
	}
	return args
}

// ExpandHome resolves a leading ~/ against home.
func ExpandHome(dir, home string) string {
	if dir == "~" {
		return home
	}
	if strings.HasPrefix(dir, "~/") {
		return filepath.Join(home, dir[2:])
	}
	return dir
}

func parseClock(v string) (int, bool) {
	t, err := time.Parse("15:04", v)
	if err != nil || len(v) != 5 {
		return 0, false
	}
	return t.Hour()*60 + t.Minute(), true
}

// Slot reports whether now falls in the schedule window and, if so, a stable
// key for the window occurrence (the local date the window opened), so a run
// happens at most once per occurrence. A window whose end is not after its
// start wraps past midnight.
func (s SelfUpdateSchedule) Slot(now time.Time) (string, bool) {
	start, ok1 := parseClock(s.WindowStart)
	end, ok2 := parseClock(s.WindowEnd)
	if !ok1 || !ok2 || s.Frequency != ScheduleDaily && s.Frequency != ScheduleWeekly {
		return "", false
	}
	minute := now.Hour()*60 + now.Minute()
	opened := now
	switch {
	case start < end:
		if minute < start || minute >= end {
			return "", false
		}
	case minute >= start:
	case minute < end:
		opened = now.AddDate(0, 0, -1)
	default:
		return "", false
	}
	if s.Frequency == ScheduleWeekly && weekdays[opened.Weekday()] != s.Weekday {
		return "", false
	}
	return opened.Format("2006-01-02"), true
}

// NextRun is when the next scheduled run starts: now, when now is inside a
// window occurrence other than lastSlot (the one already run), else the next
// window opening. Zero when the schedule is off.
func (s SelfUpdateSchedule) NextRun(now time.Time, lastSlot string) time.Time {
	start, ok := parseClock(s.WindowStart)
	if !ok || s.Frequency != ScheduleDaily && s.Frequency != ScheduleWeekly {
		return time.Time{}
	}
	if slot, in := s.Slot(now); in && slot != lastSlot {
		return now
	}
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	for i := 0; i <= 8; i++ {
		d := day.AddDate(0, 0, i)
		at := d.Add(time.Duration(start) * time.Minute)
		if !at.After(now) || s.Frequency == ScheduleWeekly && weekdays[d.Weekday()] != s.Weekday {
			continue
		}
		return at
	}
	return time.Time{}
}

// Validate reports problems with updates.self.
func (s *SelfUpdateConfig) Validate() []string {
	if s == nil {
		return nil
	}
	var errs []string
	if s.Method != "" && !slices.Contains(selfupdate.MethodIDs, s.Method) {
		errs = append(errs, "updates.self.method must be one of "+strings.Join(selfupdate.MethodIDs, ", ")+" (or empty: not chosen)")
	}
	if sc := s.Schedule; sc != nil {
		if sc.Frequency != "" && sc.Frequency != ScheduleOff && sc.Frequency != ScheduleDaily && sc.Frequency != ScheduleWeekly {
			errs = append(errs, "updates.self.schedule.frequency must be off, daily or weekly")
		}
		if sc.Weekday != "" && !slices.Contains(weekdays, sc.Weekday) {
			errs = append(errs, "updates.self.schedule.weekday must be monday..sunday (lowercase)")
		}
		for name, v := range map[string]string{"window_start": sc.WindowStart, "window_end": sc.WindowEnd} {
			if _, ok := parseClock(v); v != "" && !ok {
				errs = append(errs, "updates.self.schedule."+name+" must be a 24-hour HH:MM local time")
			}
		}
		if sc.WindowStart != "" && sc.WindowStart == sc.WindowEnd {
			errs = append(errs, "updates.self.schedule window_start and window_end must differ")
		}
	}
	if p := s.PostUpdateSetup; p != nil {
		seen := map[string]bool{}
		for _, f := range p.Flags {
			if !slices.Contains(SetupFlags, f) {
				errs = append(errs, fmt.Sprintf("updates.self.post_update_setup.flags: %q is not one of %s", f, strings.Join(SetupFlags, ", ")))
			} else if seen[f] {
				errs = append(errs, "updates.self.post_update_setup.flags: duplicate "+f)
			}
			seen[f] = true
		}
		if p.Harness != "" && !slices.Contains(SetupHarnesses, p.Harness) {
			errs = append(errs, "updates.self.post_update_setup.harness must be one of "+strings.Join(SetupHarnesses, ", "))
		}
	}
	if c := s.SourceCheckout; c != nil {
		if c.Path != "" && (!filepath.IsAbs(c.Path) && !strings.HasPrefix(c.Path, "~/") || strings.ContainsAny(c.Path, "\x00\n") || len(c.Path) > 1024) {
			errs = append(errs, "updates.self.source_checkout.path must be an absolute (or ~/) path")
		}
		if c.Ref != "" && (!gitRefPattern.MatchString(c.Ref) || strings.Contains(c.Ref, "..")) {
			errs = append(errs, "updates.self.source_checkout.ref must be a plain branch or tag name")
		}
	}
	if c := s.Custom; c != nil {
		if len(c.Command) > 4096 || strings.ContainsRune(c.Command, 0) {
			errs = append(errs, "updates.self.custom.command must be at most 4096 bytes")
		}
		if c.TimeoutSeconds < 0 || c.TimeoutSeconds > 86400 {
			errs = append(errs, "updates.self.custom.timeout_seconds must be 0..86400")
		}
	}
	if s.InstallDir != "" {
		d := s.InstallDir
		if !filepath.IsAbs(d) && d != "~" && !strings.HasPrefix(d, "~/") || strings.ContainsAny(d, "\x00\n") || len(d) > 1024 {
			errs = append(errs, "updates.self.install_dir must be an absolute (or ~/) path")
		} else if selfupdate.PackageManagerTree(filepath.Clean(d)) {
			errs = append(errs, "updates.self.install_dir must not be inside a package manager's tree (/usr, /nix, a Homebrew Cellar)")
		}
	}
	switch s.Method {
	case selfupdate.MethodSourceCheckout:
		if s.SourceCheckout == nil || s.SourceCheckout.Path == "" {
			errs = append(errs, "updates.self.method source_checkout needs updates.self.source_checkout.path")
		}
	case selfupdate.MethodCustom:
		if s.Custom == nil || strings.TrimSpace(s.Custom.Command) == "" {
			errs = append(errs, "updates.self.method custom needs updates.self.custom.command")
		}
	}
	return errs
}
