package config

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSelfUpdateDefaultsAreOffAndUnchosen(t *testing.T) {
	s := (&Config{}).SelfUpdate()
	require.Empty(t, s.Method, "no method is chosen until the operator picks one")
	require.Equal(t, ScheduleOff, s.Schedule.Frequency)
	require.False(t, s.UpdateOnStart)
	require.False(t, s.PostUpdateSetup.Enabled)
	require.Equal(t, DefaultSelfUpdateInstallDir, s.InstallDir)
	require.Empty(t, Validate(&Config{Updates: &UpdatesConfig{Self: &s}}))
}

func TestSelfUpdateValidation(t *testing.T) {
	bad := &SelfUpdateConfig{
		Method:          "source",
		Schedule:        &SelfUpdateSchedule{Frequency: "hourly", Weekday: "Mon", WindowStart: "25:00", WindowEnd: "3:00"},
		PostUpdateSetup: &PostUpdateSetupConfig{Flags: []string{"install-agent-skills", "install-agent-skills", "check; rm -rf /"}, Harness: "vim"},
		SourceCheckout:  &SourceCheckoutConfig{Path: "relative/dir", Ref: "--upload-pack=x"},
		Custom:          &CustomUpdateConfig{TimeoutSeconds: -1},
		InstallDir:      "/usr/local/bin",
	}
	errs := strings.Join(bad.Validate(), "\n")
	for _, want := range []string{"method must be one of", "frequency", "weekday", "window_start", "window_end", "duplicate install-agent-skills", `"check; rm -rf /"`, "harness", "source_checkout.path", "source_checkout.ref", "timeout_seconds", "install_dir must not be inside"} {
		require.Contains(t, errs, want)
	}
	require.Contains(t, strings.Join((&SelfUpdateConfig{Method: "custom"}).Validate(), "\n"), "needs updates.self.custom.command")
	require.Contains(t, strings.Join((&SelfUpdateConfig{Method: "source_checkout"}).Validate(), "\n"), "needs updates.self.source_checkout.path")
	good := &SelfUpdateConfig{Method: "source_checkout", SourceCheckout: &SourceCheckoutConfig{Path: "~/git/tclaude", Ref: "main"}, InstallDir: "~/bin"}
	require.Empty(t, good.Validate())
}

func TestSelfUpdateSetupArgsAreAllowlistedFlags(t *testing.T) {
	p := PostUpdateSetupConfig{Enabled: true, Flags: []string{"install-agent-skills", "install-default-agent-permissions"}, Harness: "codex"}
	require.Equal(t, []string{"setup", "--yes", "--install-agent-skills", "--install-default-agent-permissions", "--harness", "codex"}, p.SetupArgs())
}

func TestSelfUpdateScheduleSlots(t *testing.T) {
	loc := time.FixedZone("test", 3600)
	at := func(day, hour, minute int) time.Time { return time.Date(2026, 11, day, hour, minute, 0, 0, loc) } // 2026-11-09 is a Monday
	daily := SelfUpdateSchedule{Frequency: ScheduleDaily, WindowStart: "03:00", WindowEnd: "05:00"}
	slot, in := daily.Slot(at(9, 4, 0))
	require.True(t, in)
	require.Equal(t, "2026-11-09", slot)
	_, in = daily.Slot(at(9, 5, 0))
	require.False(t, in, "the window end is exclusive")
	_, in = daily.Slot(at(9, 2, 59))
	require.False(t, in)

	wrap := SelfUpdateSchedule{Frequency: ScheduleWeekly, Weekday: "monday", WindowStart: "23:00", WindowEnd: "01:00"}
	slot, in = wrap.Slot(at(10, 0, 30))
	require.True(t, in, "a wrapped window continues past midnight")
	require.Equal(t, "2026-11-09", slot, "keyed on the day the window opened")
	_, in = wrap.Slot(at(11, 0, 30))
	require.False(t, in, "Tuesday's window is not Monday's")

	require.Equal(t, at(9, 4, 0), daily.NextRun(at(9, 4, 0), ""), "inside an unrun window, the run is now")
	require.Equal(t, at(10, 3, 0), daily.NextRun(at(9, 4, 0), "2026-11-09"))
	weekly := SelfUpdateSchedule{Frequency: ScheduleWeekly, Weekday: "monday", WindowStart: "03:00", WindowEnd: "05:00"}
	require.Equal(t, at(16, 3, 0), weekly.NextRun(at(9, 6, 0), ""))
	require.True(t, (SelfUpdateSchedule{Frequency: ScheduleOff, WindowStart: "03:00"}).NextRun(at(9, 1, 0), "").IsZero())
}
