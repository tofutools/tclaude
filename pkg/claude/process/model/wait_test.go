package model

import (
	"testing"
	"time"
)

func TestParseWaitDuration(t *testing.T) {
	day := 24 * time.Hour
	for input, want := range map[string]time.Duration{
		"90m": 90 * time.Minute, "1h30m": 90 * time.Minute, "2h": 2 * time.Hour,
		"3d": 3 * day, "1d12h": 36 * time.Hour, "2w": 14 * day,
		"3 days": 3 * day, "1 day": day, "1 hour 30 minutes": 90 * time.Minute,
		"2 weeks and 1 day": 15 * day, "1.5h": 90 * time.Minute, " 45 Seconds ": 45 * time.Second,
		"1 week, 2 days": 9 * day,
	} {
		got, err := ParseWaitDuration(input)
		if err != nil || got != want {
			t.Errorf("ParseWaitDuration(%q) = %v, %v; want %v", input, got, err, want)
		}
	}
	for _, input := range []string{"", "soon", "3", "0s", "-1h", "3 fortnights", "1h then 2m", "99999 weeks"} {
		if got, err := ParseWaitDuration(input); err == nil {
			t.Errorf("ParseWaitDuration(%q) = %v; want an error", input, got)
		}
	}
}

func TestWaitDeadline(t *testing.T) {
	start := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		name, duration, until string
		want                  time.Time
	}{
		{"duration", "2h", "", start.Add(2 * time.Hour)},
		{"until", "", "2026-11-20T09:00:00Z", time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)},
		{"until sooner than the duration", "3 days", "2026-11-20T09:00:00Z", time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)},
		{"duration sooner than until", "1h", "2026-11-20T09:00:00Z", start.Add(time.Hour)},
		{"until already past", "", "2026-11-01T00:00:00Z", time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		got, err := WaitDeadline(c.duration, c.until, start)
		if err != nil || !got.Equal(c.want) {
			t.Errorf("%s: WaitDeadline = %v, %v; want %v", c.name, got, err, c.want)
		}
	}
	for _, bad := range [][2]string{{"", ""}, {"soon", ""}, {"", "next week"}, {"", "{{ outputs.when }}"}} {
		if _, err := WaitDeadline(bad[0], bad[1], start); err == nil {
			t.Errorf("WaitDeadline(%q, %q) should fail", bad[0], bad[1])
		}
	}
}

// A literal wait duration in words validates; a bad one says why.
func TestWaitDurationValidation(t *testing.T) {
	if d := validateWaitDuration("p", "3 days"); len(d) != 0 {
		t.Fatalf("3 days is a wait duration: %#v", d)
	}
	if d := validateWaitDuration("p", "3 fortnights"); len(d) != 1 || d[0].Code != "invalid_duration" {
		t.Fatalf("3 fortnights: %#v", d)
	}
	if d := validateWaitDuration("p", "{{ outputs.delay }}"); len(d) != 0 {
		t.Fatalf("an output is bound when the step starts: %#v", d)
	}
}
