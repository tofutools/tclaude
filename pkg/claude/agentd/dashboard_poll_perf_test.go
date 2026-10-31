package agentd_test

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"slices"
	"sort"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// TestDashboardPollPerfReport prints what one dashboard poll costs on a
// long-lived install: a seeded history far larger than the visible roster. It
// is a measuring tool for perf work, not an assertion, so it only runs when
// TCLAUDE_POLL_PERF_REPORT is set:
//
//	TCLAUDE_POLL_PERF_REPORT=1 scripts/test.sh ./pkg/claude/agentd/ \
//	  -run TestDashboardPollPerfReport -v -count=1 -timeout 300s
func TestDashboardPollPerfReport(t *testing.T) {
	if os.Getenv("TCLAUDE_POLL_PERF_REPORT") == "" {
		t.Skip("set TCLAUDE_POLL_PERF_REPORT=1 to measure the dashboard poll")
	}
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	seedPollPerfHistory(t, f.HaveGroup, f.HaveMember, f.HaveConvWithTitle, f.HaveRetiredAgent, func(conv, label, tmux string) {
		f.HaveAliveSession(conv, label, tmux, f.TestCwd(label))
	})
	// The daemon's refresher does this at startup; a test has no daemon.
	require.NoError(t, db.RefreshCostOpening(time.Now()))
	mux := agentd.BuildDashboardHandlerForTest()

	first := httptest.NewRecorder()
	mux.ServeHTTP(first, pollPerfRequest("/api/snapshot"))
	require.Equal(t, http.StatusOK, first.Code)
	var head struct {
		StaticVersion string `json:"static_version"`
	}
	require.NoError(t, json.Unmarshal(first.Body.Bytes(), &head))

	for _, target := range []string{
		"/api/snapshot?static_version=" + head.StaticVersion,
		"/api/mailboxes",
		"/api/costs",
	} {
		reportPollPerf(t, mux, target)
	}
	reportSnapshotKeys(t, mux, "/api/snapshot?static_version="+head.StaticVersion)
}

func pollPerfRequest(target string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, target, nil)
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	return r
}

func reportPollPerf(t *testing.T, mux http.Handler, target string) {
	t.Helper()
	const runs = 15
	var walls []time.Duration
	var body []byte
	var before, after runtime.MemStats
	cpuBefore := processCPU(t)
	runtime.ReadMemStats(&before)
	for range runs {
		rec := httptest.NewRecorder()
		start := time.Now()
		mux.ServeHTTP(rec, pollPerfRequest(target))
		walls = append(walls, time.Since(start))
		require.Equal(t, http.StatusOK, rec.Code, "%s: %s", target, rec.Body.String())
		body = rec.Body.Bytes()
	}
	runtime.ReadMemStats(&after)
	cpu := (processCPU(t) - cpuBefore) / runs
	slices.Sort(walls)
	var zipped bytes.Buffer
	zw := gzip.NewWriter(&zipped)
	_, _ = zw.Write(body)
	_ = zw.Close()
	t.Logf("%-28s wall p50 %8s  cpu/req %8s  alloc/req %7.1f MiB  raw %8d B  gzip %7d B",
		pathOnly(target), walls[runs/2].Round(10*time.Microsecond), cpu.Round(10*time.Microsecond),
		float64(after.TotalAlloc-before.TotalAlloc)/runs/(1<<20), len(body), zipped.Len())
}

func pathOnly(target string) string {
	for i, c := range target {
		if c == '?' {
			return target[:i]
		}
	}
	return target
}

func reportSnapshotKeys(t *testing.T, mux http.Handler, target string) {
	t.Helper()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, pollPerfRequest(target))
	var keys map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &keys))
	names := make([]string, 0, len(keys))
	for name := range keys {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool { return len(keys[names[i]]) > len(keys[names[j]]) })
	for _, name := range names[:min(12, len(names))] {
		t.Logf("  snapshot.%-22s %8d B", name, len(keys[name]))
	}
}

func processCPU(t *testing.T) time.Duration {
	t.Helper()
	var usage syscall.Rusage
	require.NoError(t, syscall.Getrusage(syscall.RUSAGE_SELF, &usage))
	return time.Duration(usage.Utime.Nano() + usage.Stime.Nano())
}

// seedPollPerfHistory builds the shape of an install that has been running for
// a year: a modest live roster over large append-only tables.
func seedPollPerfHistory(
	t *testing.T,
	haveGroup func(string) *db.AgentGroup,
	haveMember func(group, conv string),
	haveTitle func(conv, title string),
	haveRetired func(conv string),
	haveSession func(conv, label, tmux string),
) {
	t.Helper()
	const (
		groups       = 6
		perGroup     = 8
		retired      = 300
		messages     = 100_000
		costConvs    = 3_000
		costDays     = 10
		exited       = 10_000
		transitions  = 200
		revokedLinks = 200
		proposals    = 200
		disabledCron = 100
	)
	convs := []string{}
	groupIDs := []int64{}
	for g := range groups {
		name := fmt.Sprintf("perf-group-%d", g)
		groupIDs = append(groupIDs, haveGroup(name).ID)
		for m := range perGroup {
			conv := fmt.Sprintf("perf-%04d-aaaa-bbbb-cccc-dddddddddddd", g*perGroup+m)
			haveTitle(conv, fmt.Sprintf("worker-%d-%d", g, m))
			haveSession(conv, fmt.Sprintf("spwn-perf-%d-%d", g, m), fmt.Sprintf("tmux-perf-%d-%d", g, m))
			haveMember(name, conv)
			convs = append(convs, conv)
		}
	}
	for i := range retired {
		conv := fmt.Sprintf("gone-%04d-aaaa-bbbb-cccc-dddddddddddd", i)
		haveRetired(conv)
		convs = append(convs, conv)
	}

	d, err := db.Open()
	require.NoError(t, err)
	tx, err := d.Begin()
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	exec := func(query string, args ...any) {
		t.Helper()
		_, err := tx.Exec(query, args...)
		require.NoError(t, err)
	}
	now := time.Now()
	nowNS := now.UnixNano()

	for i := range messages {
		from, to := convs[i%len(convs)], convs[(i*7+1)%len(convs)]
		exec(`INSERT INTO agent_messages (group_id, from_conv, to_conv, subject, body, created_at, read_at)
			VALUES (?, ?, ?, 'status', 'seeded body', ?, ?)`,
			groupIDs[i%groups], from, to, nowNS-int64(messages-i)*int64(time.Minute), nowNS)
	}
	for c := range costConvs {
		conv := fmt.Sprintf("cost-%05d-aaaa-bbbb-cccc-dddddddddddd", c)
		// One conversation in twenty is recent: the rest are last year's.
		age := 30 + c%330
		if c%20 == 0 {
			age = 0
		}
		for day := range costDays {
			at := now.AddDate(0, 0, day-age-costDays+1)
			exec(`INSERT INTO session_cost_daily (session_id, day, conv_id, cost_usd, virtual_cost_usd, updated_at, model, harness)
				VALUES (?, ?, ?, ?, 0, ?, 'Opus', 'claude')`,
				conv, at.Format("2006-01-02"), conv, float64(day+1)*1.5, at.UnixNano())
		}
	}
	for i := range exited {
		exec(`INSERT INTO sessions (id, tmux_session, conv_id, status, created_at, updated_at)
			VALUES (?, ?, ?, 'exited', ?, ?)`,
			fmt.Sprintf("old-%05d", i), fmt.Sprintf("tmux-old-%05d", i), fmt.Sprintf("cost-%05d", i%costConvs),
			nowNS-int64(time.Hour)*int64(i+24*60), nowNS-int64(time.Hour)*int64(i+24*60))
	}
	for _, id := range groupIDs {
		exec(`INSERT INTO group_process_state (group_id, process, current_phase, phase_started_at)
			VALUES (?, '[{"name":"build"},{"name":"ship"}]', 'build', ?)`, id, nowNS)
		for i := range transitions {
			exec(`INSERT INTO group_process_transitions (group_id, from_phase, to_phase, at, actor)
				VALUES (?, 'build', 'ship', ?, 'human')`, id, nowNS-int64(i))
		}
	}
	for i := range proposals {
		exec(`INSERT INTO federation_peer_proposals (id, peer, origin_instance, revision, body, state, created_at, deadline_at, updated_at)
			VALUES (?, 'peer-1', 'peer-1', 1, '{"items":[]}', 'rejected', ?, ?, ?)`,
			fmt.Sprintf("prop-%d", i), nowNS, nowNS, nowNS)
	}
	for i := range revokedLinks {
		exec(`INSERT INTO federation_group_links (id, local_group_id, peer, remote_group_id, remote_group_name, direction, mode,
				local_state, remote_state, proposal_id, item_id, created_at, updated_at)
			VALUES (?, ?, 'peer-1', 'rg', 'remote', 'out', 'members->members', 'revoked', 'revoked', ?, 'item', ?, ?)`,
			fmt.Sprintf("link-%d", i), groupIDs[i%groups], fmt.Sprintf("prop-%d", i), nowNS, nowNS)
	}
	retiredAgent := ""
	require.NoError(t, tx.QueryRow(`SELECT agent_id FROM agents WHERE retired_at IS NOT NULL LIMIT 1`).Scan(&retiredAgent))
	for i := range disabledCron {
		exec(`INSERT INTO agent_cron_jobs (name, owner_agent, target_agent, interval_seconds, subject, body, enabled, created_at)
			VALUES (?, ?, ?, 600, 'nudge', 'seeded', 0, ?)`, fmt.Sprintf("old-cron-%d", i), retiredAgent, retiredAgent, nowNS)
	}
	require.NoError(t, tx.Commit())
}
