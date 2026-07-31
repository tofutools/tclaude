package agentd

import (
	"log/slog"
	"time"
)

// dashboard_groups_perf.go — sub-phase timing for the snapshot's "groups"
// phase. The Debug tab showed a groups p50 well under 1 ms next to a max
// approaching 2 s with no breakdown to explain it: unlike "preload" and
// "collectors", the group loop reported one opaque number. This splits it
// into the parts that can actually stall — the three point queries fired PER
// GROUP (permissions, process state, wave choreography) and the per-conv row
// assembly for members and owners — so /api/perf names the culprit instead of
// just the phase.
//
// Codex telemetry is subtracted from every sub-phase, matching the parent's
// own markExcluding, so a member-row number here is comparable to the parent's.
// Both now subtract only the telemetry that accrued INSIDE the groups phase —
// the bulk of it is paid by the preload warm loop and excluded there.
//
// slowestGroupMs is deliberately a single number rather than one child per
// group: /api/perf aggregates children BY NAME across the ring, so per-group
// names would grow the phase tree with every group that ever existed. The
// group's identity instead rides a debug log when one iteration crosses
// groupsSlowGroupLogMs.

// groupsSlowGroupLogMs is the per-group threshold above which the slow
// iteration is logged with its group name. Well below the request-level
// perfSlowLogMs so a single stalling group is visible even when the rest of
// the poll is fast enough to keep the request under that bar.
const groupsSlowGroupLogMs = 50

// groupsPhaseMark is an open interval: wall-clock start plus the codex
// telemetry already accounted for at that instant.
type groupsPhaseMark struct {
	at           time.Time
	codexAtStart time.Duration
}

type groupsPhaseTimer struct {
	rc *snapshotRowCache

	perms       time.Duration
	process     time.Duration
	waves       time.Duration
	memberRows  time.Duration
	ownerRows   time.Duration
	sortMembers time.Duration

	slowestGroup time.Duration

	// rowWorkBase is the per-row side-read accumulator as it stood when the
	// loop began, so the loop reports only what IT paid. In practice this is
	// almost all of it: the preload warm loop resolves every conv first, so a
	// group's members are memo hits and these read ~0 here. A non-zero value
	// means a conv reached the group loop unresolved.
	rowWorkBase map[string]time.Duration
}

func newGroupsPhaseTimer(rc *snapshotRowCache) *groupsPhaseTimer {
	t := &groupsPhaseTimer{rc: rc}
	if rc != nil {
		t.rowWorkBase = rc.rowWorkSnapshot()
	}
	return t
}

func (t *groupsPhaseTimer) codexSoFar() time.Duration {
	if t.rc == nil {
		return 0
	}
	return t.rc.codexTelemetryTiming.total
}

func (t *groupsPhaseTimer) begin() groupsPhaseMark {
	return groupsPhaseMark{at: time.Now(), codexAtStart: t.codexSoFar()}
}

// elapsed returns the mark's wall-clock minus any codex telemetry that ran
// inside it. Clamped at zero: the two clocks are sampled independently, so
// rounding must never produce a negative contribution.
func (t *groupsPhaseTimer) elapsed(m groupsPhaseMark) time.Duration {
	d := time.Since(m.at) - (t.codexSoFar() - m.codexAtStart)
	return max(d, 0)
}

func (t *groupsPhaseTimer) end(dst *time.Duration, m groupsPhaseMark) {
	*dst += t.elapsed(m)
}

func (t *groupsPhaseTimer) track(dst *time.Duration, fn func()) {
	m := t.begin()
	fn()
	t.end(dst, m)
}

// observeGroup closes one group's iteration, keeping the slowest seen and
// naming it in the log when it crosses the threshold.
func (t *groupsPhaseTimer) observeGroup(name string, m groupsPhaseMark) {
	d := t.elapsed(m)
	if d > t.slowestGroup {
		t.slowestGroup = d
	}
	if durMs(d) >= groupsSlowGroupLogMs {
		slog.Debug("snapshot: slow group row build",
			"group", name, "ms", durMs(d), "module", "agentd")
	}
}

// phases renders the accumulated breakdown as children of the "groups" phase.
// The children deliberately do not partition the parent: the loop also builds
// the group view itself, and the last three entries are cross-cutting views of
// time already counted in member_rows/owner_rows — the two un-batched per-row
// side reads paid inside the loop, and the worst single group iteration.
func (t *groupsPhaseTimer) phases() []perfPhase {
	out := []perfPhase{
		{Name: "group_perms", Ms: durMs(t.perms)},
		{Name: "group_process", Ms: durMs(t.process)},
		{Name: "group_waves", Ms: durMs(t.waves)},
		{Name: "member_rows", Ms: durMs(t.memberRows)},
		{Name: "owner_rows", Ms: durMs(t.ownerRows)},
		{Name: "member_sort", Ms: durMs(t.sortMembers)},
	}
	if t.rc != nil {
		out = append(out, t.rc.rowWorkPhasesSince(t.rowWorkBase)...)
	} else {
		for _, name := range rowWorkPhaseOrder {
			out = append(out, perfPhase{Name: name})
		}
	}
	return append(out, perfPhase{Name: "slowest_group", Ms: durMs(t.slowestGroup)})
}
