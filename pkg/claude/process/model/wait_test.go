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
		{"until already past ends at once", "", "2026-11-01T00:00:00Z", start},
		{"until the epoch ends at once", "", "1970-01-01T00:00:00Z", start},
	}
	for _, c := range cases {
		got, err := WaitDeadline(c.duration, c.until, start)
		if err != nil || !got.Equal(c.want) {
			t.Errorf("%s: WaitDeadline = %v, %v; want %v", c.name, got, err, c.want)
		}
	}
	for _, bad := range [][2]string{{"", ""}, {"soon", ""}, {"", "next week"}, {"", "{{ outputs.when }}"}, {"", "2300-01-01T00:00:00Z"}} {
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

func TestWaitCheckInterval(t *testing.T) {
	fixed := WaitCheck{Every: "5m"}
	exp := WaitCheck{Every: "1m", Backoff: WaitBackoffExponential, MaxEvery: "10m"}
	triple := WaitCheck{Every: "1m", Backoff: WaitBackoffExponential, Factor: 3}
	for _, c := range []struct {
		check  WaitCheck
		checks int
		want   time.Duration
	}{
		{fixed, 1, 5 * time.Minute}, {fixed, 9, 5 * time.Minute},
		{exp, 1, time.Minute}, {exp, 2, 2 * time.Minute}, {exp, 4, 8 * time.Minute}, {exp, 5, 10 * time.Minute}, {exp, 50, 10 * time.Minute},
		{triple, 3, 9 * time.Minute},
	} {
		if got := c.check.Interval(c.checks); got != c.want {
			t.Errorf("%+v Interval(%d) = %v, want %v", c.check, c.checks, got, c.want)
		}
	}
	if (WaitCheck{}).MaxFailed() != DefaultMaxFailedChecks || (WaitCheck{MaxFailedChecks: 5}).MaxFailed() != 5 {
		t.Fatal("maxFailedChecks defaults to 3")
	}
}

// A wait for a criterion validates its shape, its knobs and its routes.
func TestWaitCheckValidation(t *testing.T) {
	program := &Performer{Kind: PerformerProgram, Profile: "safe", Run: "./ready.sh"}
	valid := WaitCheck{Performer: program, Every: "5m", MaxChecks: 3}
	tmpl := func(check WaitCheck, next Next) *Template {
		return &Template{APIVersion: APIVersion, Kind: Kind, ID: "check", Start: "start",
			Params: map[string]Param{"ok": {Type: "boolean"}},
			Nodes: map[string]Node{
				"start": {Type: NodeTypeStart, Next: Next{"next": "hold"}},
				"hold":  {Type: NodeTypeWait, Wait: &WaitConfig{Check: &check}, Next: next},
				"done":  {Type: NodeTypeEnd, Result: "success"},
				"late":  {Type: NodeTypeEnd, Result: "failure"},
			}}
	}
	one := Next{"next": "done"}
	both := Next{WaitOutcomeMet: "done", WaitOutcomeTimedOut: "late"}
	codes := func(check WaitCheck, next Next) []string {
		var out []string
		t0 := tmpl(check, next)
		for _, d := range Validate(t0, NormalizeEdges(t0)) {
			if d.Severity == SeverityError && d.Code != "unreachable_node" {
				out = append(out, d.Code+"@"+d.Path)
			}
		}
		return out
	}
	if got := codes(valid, one); len(got) != 0 {
		t.Fatalf("valid check: %v", got)
	}
	timedOut := valid
	timedOut.OnLimit = WaitOnLimitTimedOut
	if got := codes(timedOut, both); len(got) != 0 {
		t.Fatalf("timedOut with met and timedOut routes: %v", got)
	}
	when := WaitCheck{When: "params.ok == true", Every: "1m", Within: "1h"}
	if got := codes(when, one); len(got) != 0 {
		t.Fatalf("when check: %v", got)
	}
	bad := []struct {
		name  string
		check WaitCheck
		next  Next
	}{
		{"nothing to check", WaitCheck{Every: "1m", MaxChecks: 2}, one},
		{"both when and performer", WaitCheck{When: "params.ok == true", Performer: program, Every: "1m", MaxChecks: 2}, one},
		{"no every", WaitCheck{Performer: program, MaxChecks: 2}, one},
		{"no limit", WaitCheck{Performer: program, Every: "1m"}, one},
		{"bad backoff", WaitCheck{Performer: program, Every: "1m", MaxChecks: 2, Backoff: "linear"}, one},
		{"factor without exponential", WaitCheck{Performer: program, Every: "1m", MaxChecks: 2, Factor: 2}, one},
		{"factor not above 1", WaitCheck{Performer: program, Every: "1m", MaxChecks: 2, Backoff: WaitBackoffExponential, Factor: 1}, one},
		{"timedOut needs both routes", WaitCheck{Performer: program, Every: "1m", MaxChecks: 2, OnLimit: WaitOnLimitTimedOut}, one},
		{"two routes without timedOut", WaitCheck{Performer: program, Every: "1m", MaxChecks: 2}, both},
		{"unknown param", WaitCheck{When: "params.nope == true", Every: "1m", MaxChecks: 2}, one},
		{"agent timeout", WaitCheck{Performer: &Performer{Kind: PerformerAgent, Prompt: "Ready?", Anyone: true}, Every: "1m", MaxChecks: 2, Timeout: "1m"}, one},
		{"bad every", WaitCheck{Performer: program, Every: "soon", MaxChecks: 2}, one},
	}
	for _, c := range bad {
		if got := codes(c.check, c.next); len(got) == 0 {
			t.Errorf("%s: expected an error", c.name)
		}
	}
}
