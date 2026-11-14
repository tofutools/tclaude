package agentd

import (
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/tmuxctl"
	"github.com/tofutools/tclaude/pkg/claude/doctor"
)

// The Diagnostics › Doctor view polls every minute for its nav badge, and one
// of its checks is the live tclaude-layer probe, which forks bwrap on Linux
// (through `tmux run-shell` and a tclaude subprocess). A poll must not wait
// for that, so a read serves the cached checks and, once they are older than
// doctorCacheTTL, starts a single background rebuild; the badge catches up one
// poll later. With the TTL equal to the poll interval, an inline rebuild ran
// on nearly every poll.
//
// Two reads still build inline, because the operator is waiting on the answer
// itself: the tab's Re-check button (refresh=1), and the first read after a
// write that changed an answer (the default-sandbox decision, a harness config
// repin) dropped the cache via invalidateDoctorChecks.
const doctorCacheTTL = time.Minute

var doctorProbes = doctor.HostProbes

// buildDoctorChecks is the full, host-touching check run. A var so tests can
// count and stall builds.
var buildDoctorChecks = func() []doctor.Check {
	return append(doctor.Run(doctorProbes()), harnessConfigRepairChecks()...)
}

var doctorCache struct {
	sync.Mutex
	checks []doctor.Check
	at     time.Time
	// gen is bumped by every invalidation and every inline build, so a
	// background rebuild that started before either does not publish an
	// answer computed from the state a write just changed.
	gen        uint64
	refreshing bool
}

func invalidateDoctorChecks() {
	doctorCache.Lock()
	defer doctorCache.Unlock()
	doctorCache.checks = nil
	doctorCache.gen++
}

func doctorChecks(refresh bool) []doctor.Check {
	c := &doctorCache
	c.Lock()
	defer c.Unlock()
	switch {
	case refresh || c.checks == nil:
		c.gen++
		c.checks = buildDoctorChecks()
		c.at = time.Now()
	case !c.refreshing && time.Since(c.at) >= doctorCacheTTL:
		c.refreshing = true
		go refreshDoctorChecks(c.gen)
	}
	// The tmux path check is in-memory state, so it is always live.
	return append(slices.Clip(c.checks), tmuxControlModeCheck())
}

// refreshDoctorChecks rebuilds the checks off the request path and publishes
// them unless an invalidation or inline build happened meanwhile. It runs
// outside any HTTP handler, so a panic is recovered and logged; the previous
// checks stay in place and the rebuild is retried after doctorCacheTTL.
func refreshDoctorChecks(gen uint64) {
	c := &doctorCache
	var fresh []doctor.Check
	ok := false
	defer func() {
		if r := recover(); r != nil {
			slog.Error("doctor checks refresh panicked", "panic", r, "module", "agentd")
		}
		c.Lock()
		defer c.Unlock()
		c.refreshing = false
		if c.gen != gen {
			return
		}
		c.at = time.Now()
		if ok {
			c.checks = fresh
		}
	}()
	fresh = buildDoctorChecks()
	ok = true
}

// tmuxControlModeCheck reports which path agentd's tmux queries take, so the
// operator can see whether control mode is in use and, if not, why.
func tmuxControlModeCheck() doctor.Check {
	st := clcommon.TmuxControlModeStatus()
	c := doctor.Check{ID: "tmux-control-mode", Category: doctor.CategoryTools, Severity: doctor.SeverityOK}
	counts := fmt.Sprintf(" %d connect(s), %d request(s), %d failed.", st.Connects, st.Requests, st.Failures)
	if len(st.Routed) > 0 {
		names := slices.Sorted(maps.Keys(st.Routed))
		parts := make([]string, 0, len(names))
		for _, n := range names {
			parts = append(parts, fmt.Sprintf("%s %d", n, st.Routed[n]))
		}
		counts += " Answered without a tmux process: " + strings.Join(parts, ", ") + "."
	}
	switch {
	case !st.Enabled:
		c.Severity = doctor.SeverityInfo
		c.Title = "tmux control mode is off"
		c.Detail = "Every tmux query forks one tmux process. Unset " + clcommon.ControlModeEnvVar +
			" and remove tmux.control_mode=false from the config to turn it on, then restart agentd."
	case st.Connected:
		c.Title = "tmux control mode is connected"
		c.Detail = "agentd's tmux queries and commands go over one tmux -C connection (session " + tmuxctl.SessionName +
			") since " + st.Since.Format(time.RFC3339) + "." + counts
	case st.LastConnErr != "":
		c.Severity = doctor.SeverityInfo
		c.Title = "tmux control mode cannot connect; using one tmux process per query"
		c.Detail = "Last connect error: " + st.LastConnErr + ". State: " + st.Reason + "." + counts
	default:
		c.Title = "tmux control mode is idle; using one tmux process per query"
		c.Detail = "It connects once a tclaude tmux session exists. State: " + st.Reason + "." + counts
	}
	return c
}

// handleDashboardDoctor answers GET /api/doctor[?refresh=1]. Read-only: every
// fix is either a command for the operator to run or a separate, permission-
// gated dashboard write.
func handleDashboardDoctor(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"checks": doctorChecks(r.URL.Query().Get("refresh") == "1"),
	})
}

// harnessConfigRepairChecks flags agents whose explicit harness_config was
// lost on a relaunch (harness_config_repair.go). It is daemon state, not a
// host probe, so it is checked here rather than in doctor.Run.
func harnessConfigRepairChecks() []doctor.Check {
	repairs, err := findHarnessConfigRepairs(false)
	if err != nil || len(repairs) == 0 {
		return nil
	}
	widened := 0
	for _, r := range repairs {
		if r.Change == harnessConfigRepairWidened {
			widened++
		}
	}
	severity := doctor.SeverityInfo
	if widened > 0 {
		severity = doctor.SeverityWarning
	}
	return []doctor.Check{{
		ID: "harness-config-repair", Category: doctor.CategorySandbox, Severity: severity,
		Title: fmt.Sprintf("%d agent(s) lost the harness_config posture they were spawned with", len(repairs)),
		Detail: fmt.Sprintf("A relaunch before the fix in #3817 dropped an explicit --harness-config; %d of them now run "+
			"without the read-only floor they asked for. Review them and restore the posture. Agents with no recorded spawn "+
			"request (sweeper or group-template spawns) cannot be checked.", widened),
		Fix:    "tclaude agent sandbox-profiles harness-config-repairs",
		Action: doctor.ActionHarnessConfigRepair,
	}}
}
