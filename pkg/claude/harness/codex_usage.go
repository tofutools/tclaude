package harness

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/filefollow"
)

// Subscription rate-limit telemetry for Codex (JOH-214). Claude Code surfaces
// the account-wide 5-hour / weekly limits through the Anthropic usage API (see
// common/usageapi). Codex has no such endpoint wired into tclaude, but it
// records the same figures locally: every `token_count` event_msg in a
// rollout carries a `rate_limits` block alongside the `info` block that
// codex_telemetry.go already reads. So tclaude lifts the limits straight off
// the rollout — no OAuth token, no network call — which means the readout is
// only ever as fresh as the last Codex turn, but updates the instant an agent
// runs (the caller caps staleness, see agentd/codex_usage.go).
//
// The rate_limits block looks like:
//
//	"rate_limits": {
//	  "limit_id": "codex",
//	  "primary":   {"used_percent": 2.0, "window_minutes": 300,   "resets_at": 1781442692},
//	  "secondary": {"used_percent": 1.0, "window_minutes": 10080, "resets_at": 1781987376}
//	}
//
// Codex can multiplex other quota identities through the same field. The
// dashboard's subscription readout is specifically the account-wide "codex"
// quota, so identity is selected before durations are classified; a
// model-specific weekly-shaped quota must not replace the account figure.
//
// "primary"/"secondary" are slots, not durations — a free account with no
// 5-hour tier reports its weekly cap in the primary slot — so windows are
// classified by window_minutes (≈300 → 5-hour, ≈10080 → weekly), not by
// slot, mirroring how aistat resolves the same data.

// codexRateLimitWindow is one window inside a token_count rate_limits block.
type codexRateLimitWindow struct {
	UsedPercent   float64 `json:"used_percent"`
	WindowMinutes int     `json:"window_minutes"`
	ResetsAt      int64   `json:"resets_at"` // unix epoch seconds; 0 when absent
}

// codexRateLimits is the `rate_limits` block of a token_count event_msg. Both
// slots are pointers so an absent or null window stays nil rather than
// decoding to a zero-percent window that would render as "0%".
type codexRateLimits struct {
	LimitID   string                `json:"limit_id"`
	LimitName string                `json:"limit_name"`
	PlanType  string                `json:"plan_type"`
	Primary   *codexRateLimitWindow `json:"primary"`
	Secondary *codexRateLimitWindow `json:"secondary"`
}

// CodexRateLimitWindow is one resolved rolling-limit window — the percent
// consumed and when it resets — the cross-harness analog of a
// usageapi.CachedBucket. ResetsAt is zero when the rollout carried no
// reset timestamp.
type CodexRateLimitWindow struct {
	UsedPercent float64
	ResetsAt    time.Time
}

// CodexUsage is the latest known subscription rate-limit snapshot for the
// Codex account, classified into the two windows the dashboard renders. A
// window is nil when the most recent snapshot did not report it. Observed is
// the timestamp of the token_count event the figures came from, so the caller
// can cap staleness the same way the Claude readout does.
type CodexUsage struct {
	FiveHour  *CodexRateLimitWindow // window ≈ 300 min
	Weekly    *CodexRateLimitWindow // window ≈ 10080 min (7 days)
	Observed  time.Time
	LimitID   string
	LimitName string
	PlanType  string
}

// codexWindowToleranceNum/Den express the ±tolerance band around a known
// window duration as a fraction (1/20 = 5%), matching aistat's classifier.
const (
	codexWindowToleranceNum = 1
	codexWindowToleranceDen = 20
	codexFiveHourMinutes    = 300   // 5h
	codexWeeklyMinutes      = 10080 // 7d
	codexAccountLimitID     = "codex"

	// Some supported filesystems expose mtimes at coarser precision than the
	// millisecond timestamps inside Codex rollouts. Keep the newest-first
	// optimization, but scan files in the same coarse-mtime bucket before
	// concluding that none can contain a newer event. Two seconds covers the
	// coarsest common timestamp granularity without widening the scan
	// unboundedly.
	codexRolloutMtimeSlack = 2 * time.Second
)

// withinWindow reports whether got is within ±5% of want.
func withinWindow(got, want int) bool {
	tol := want * codexWindowToleranceNum / codexWindowToleranceDen
	return got >= want-tol && got <= want+tol
}

// LatestCodexUsage scans Codex rollouts under home (~/.codex/sessions) for the
// most recent token_count event carrying a populated rate_limits block and
// returns its resolved windows. Only rollouts modified at/after since are
// read — `since` bounds the scan to "could this still describe an unreset
// window?" (the caller passes ~the weekly window length), so a long-idle
// account's weekly figure survives while truly ancient files are skipped.
//
// Rollouts are read newest-mtime first and the scan stops as soon as no
// remaining file could hold a newer snapshot: a file last written no later
// than the best observation can't (its embedded event timestamps are ≤ its
// mtime), so the common case reads just the one active rollout rather than
// every recent file to EOF.
//
// Returns (nil, nil) — the normal "no Codex usage to show" state — when Codex
// has never run, no recent rollout carries rate limits, or the sessions dir is
// absent. A non-nil error is an I/O / scan fault the caller should log.
func LatestCodexUsage(home string, since time.Time) (*CodexUsage, error) {
	paths, err := scanCodexRollouts(home)
	if err != nil {
		return nil, err
	}
	// One rollout per session id: during the .jsonl→.jsonl.zst compression
	// window both files exist for the same uuid, and reading both is wasted
	// work for an account-wide figure. Keep only those touched at/after since.
	type rolloutStat struct {
		path  string
		mtime time.Time
	}
	var stats []rolloutStat
	for _, p := range dedupCodexRollouts(paths) {
		fi, statErr := os.Stat(p)
		if statErr != nil || fi.ModTime().Before(since) {
			continue
		}
		stats = append(stats, rolloutStat{path: p, mtime: fi.ModTime()})
	}
	sort.Slice(stats, func(i, j int) bool {
		if stats[i].mtime.Equal(stats[j].mtime) {
			return stats[i].path < stats[j].path
		}
		return stats[i].mtime.After(stats[j].mtime)
	})

	var best *CodexUsage
	for _, st := range stats {
		// Newest-first: once the remaining file mtimes are outside the coarse
		// filesystem bucket around the best observation, none can contain a
		// newer event. Files inside that bounded bucket must still be read.
		if best != nil && best.Observed.After(st.mtime.Add(codexRolloutMtimeSlack)) {
			break
		}
		u, err := CodexUsageFromRollout(st.path)
		if err != nil {
			continue // tolerate one unreadable rollout, keep scanning siblings
		}
		if u == nil {
			continue
		}
		if best == nil || u.Observed.After(best.Observed) {
			best = u
		}
	}
	return best, nil
}

// LatestCodexUsageForConvs scans the rollout directory enough to locate the
// provided Codex session ids, then reads only those rollout files for the
// newest rate-limit snapshot. It is the repair-poller path: dashboard runtime
// telemetry normally updates the cache, while this narrows fallback work to
// tclaude's live Codex sessions instead of reading every recent rollout on
// every interval.
func LatestCodexUsageForConvs(home string, convIDs []string, since time.Time) (*CodexUsage, error) {
	if len(convIDs) == 0 {
		return nil, nil
	}
	want := make(map[string]struct{}, len(convIDs))
	for _, id := range convIDs {
		if id != "" {
			want[id] = struct{}{}
		}
	}
	if len(want) == 0 {
		return nil, nil
	}

	paths, err := scanCodexRollouts(home)
	if err != nil {
		return nil, err
	}
	byID := dedupCodexRollouts(paths)

	type rolloutStat struct {
		path  string
		mtime time.Time
	}
	var stats []rolloutStat
	for id := range want {
		p := byID[id]
		if p == "" {
			continue
		}
		fi, statErr := os.Stat(p)
		if statErr != nil || fi.ModTime().Before(since) {
			continue
		}
		stats = append(stats, rolloutStat{path: p, mtime: fi.ModTime()})
	}
	sort.Slice(stats, func(i, j int) bool {
		if stats[i].mtime.Equal(stats[j].mtime) {
			return stats[i].path < stats[j].path
		}
		return stats[i].mtime.After(stats[j].mtime)
	})

	var best *CodexUsage
	for _, st := range stats {
		if best != nil && best.Observed.After(st.mtime.Add(codexRolloutMtimeSlack)) {
			break
		}
		u, err := followCodexUsage(st.path)
		if err != nil || u == nil {
			continue
		}
		if best == nil || u.Observed.After(best.Observed) {
			best = u
		}
	}
	return best, nil
}

type codexUsageFollowState struct {
	latest *CodexUsage
}

type codexUsagePathFollower struct {
	stream      *filefollow.Follower[codexUsageFollowState]
	archiveInfo os.FileInfo
	archive     *CodexUsage
	usedAt      time.Time
}

var codexUsageFollowers = struct {
	sync.Mutex
	byPath map[string]*codexUsagePathFollower
}{byPath: make(map[string]*codexUsagePathFollower)}

const maxCodexUsageFollowers = 512

// followCodexUsage is the recurring repair-poller reader. Live JSONL paths
// retain a validated cursor and fold only complete appended records; immutable
// archives are decoded once per physical generation. LatestCodexUsage remains
// the intentional one-shot startup discovery path.
func followCodexUsage(path string) (*CodexUsage, error) {
	codexUsageFollowers.Lock()
	defer codexUsageFollowers.Unlock()

	entry := codexUsageFollowers.byPath[path]
	if entry == nil {
		entry = &codexUsagePathFollower{stream: newCodexUsageStream()}
		codexUsageFollowers.byPath[path] = entry
	}
	entry.usedAt = time.Now()
	pruneCodexUsageFollowers()

	if strings.HasSuffix(path, ".zst") {
		info, err := os.Stat(path)
		if err != nil {
			delete(codexUsageFollowers.byPath, path)
			return nil, err
		}
		if entry.archiveInfo != nil && os.SameFile(entry.archiveInfo, info) &&
			entry.archiveInfo.Size() == info.Size() && entry.archiveInfo.ModTime().Equal(info.ModTime()) {
			return entry.archive, nil
		}
		u, err := CodexUsageFromRollout(path)
		if err != nil {
			return nil, err
		}
		entry.stream.Reset()
		entry.archiveInfo = info
		entry.archive = u
		return u, nil
	}

	update, err := entry.stream.Refresh(path)
	if err != nil {
		if os.IsNotExist(err) {
			delete(codexUsageFollowers.byPath, path)
		}
		return nil, err
	}
	entry.archiveInfo = nil
	entry.archive = nil
	return update.State.latest, nil
}

func newCodexUsageStream() *filefollow.Follower[codexUsageFollowState] {
	return filefollow.New(filefollow.Config[codexUsageFollowState]{
		NewState:   func(string, int64) codexUsageFollowState { return codexUsageFollowState{} },
		CloneState: func(state codexUsageFollowState) codexUsageFollowState { return state },
		Scan: func(r io.Reader, _ string, state *codexUsageFollowState, strict bool) (int64, bool, error) {
			return filefollow.ScanLines(r, filefollow.LineConfig{MaxRecordBytes: maxCodexRolloutLineBytes}, func(line filefollow.Line) bool {
				if line.Oversized {
					return true
				}
				return foldCodexUsageLine(state, line.Data)
			}, strict)
		},
	})
}

func foldCodexUsageLine(state *codexUsageFollowState, line []byte) bool {
	if len(bytes.TrimSpace(line)) == 0 {
		return true
	}
	var env codexEnvelope
	if json.Unmarshal(line, &env) != nil {
		return false
	}
	if env.Type != "event_msg" {
		return true
	}
	var ev codexTokenCountEvent
	if json.Unmarshal(env.Payload, &ev) != nil {
		return false
	}
	if ev.Type != "token_count" {
		return true
	}
	if u := codexUsageFromRateLimits(ev.RateLimits, env.Timestamp); u != nil {
		state.latest = u
	}
	return true
}

func pruneCodexUsageFollowers() {
	for len(codexUsageFollowers.byPath) > maxCodexUsageFollowers {
		var oldestPath string
		var oldest time.Time
		for path, entry := range codexUsageFollowers.byPath {
			if oldestPath == "" || entry.usedAt.Before(oldest) {
				oldestPath, oldest = path, entry.usedAt
			}
		}
		delete(codexUsageFollowers.byPath, oldestPath)
	}
}

// CodexUsageFromRollout reads rolloutPath (transparently decompressing
// `.zst`) and returns the LAST token_count event whose rate_limits block is
// the account-wide Codex quota and resolves to at least one known window.
// Other quota identities are skipped even when they use a weekly-shaped
// window. Returns (nil, nil) when the rollout carries no such event — a
// session that has not yet received account rate-limit headers. A malformed
// line is skipped; only an I/O / scanner error is returned.
func CodexUsageFromRollout(rolloutPath string) (*CodexUsage, error) {
	rc, err := openCodexRollout(rolloutPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()

	state := codexUsageFollowState{}
	err = scanCodexRolloutLines(rc, rolloutPath, func(line []byte) bool {
		// The reference one-shot scan is deliberately lenient: malformed records
		// are skipped. The recurring follower treats them as append doubt and
		// rebuilds through this same fold, preserving exact parity.
		_ = foldCodexUsageLine(&state, line)
		return true
	})
	if err != nil {
		return nil, fmt.Errorf("scan codex rollout %s: %w", rolloutPath, err)
	}
	return state.latest, nil
}

// codexUsageFromRateLimits resolves an account-wide rate_limits block into a
// CodexUsage, preserving its identity and classifying each slot by
// window_minutes. Returns nil when the block is absent, belongs to another
// quota, or neither slot matches a known window duration (so an unrecognised
// limit identity or shape can't mask a good earlier snapshot).
func codexUsageFromRateLimits(rl *codexRateLimits, timestamp string) *CodexUsage {
	if rl == nil || rl.LimitID != codexAccountLimitID {
		return nil
	}
	u := &CodexUsage{
		Observed:  parseCodexEventTime(timestamp),
		LimitID:   rl.LimitID,
		LimitName: rl.LimitName,
		PlanType:  rl.PlanType,
	}
	assignCodexWindow(u, rl.Primary)
	assignCodexWindow(u, rl.Secondary)
	if u.FiveHour == nil && u.Weekly == nil {
		return nil
	}
	return u
}

// assignCodexWindow places one rate-limit window onto u's 5-hour or weekly
// field by matching window_minutes (±5%). Windows of any other duration are
// ignored — the dashboard renders only these two.
func assignCodexWindow(u *CodexUsage, w *codexRateLimitWindow) {
	if w == nil {
		return
	}
	switch {
	case withinWindow(w.WindowMinutes, codexFiveHourMinutes):
		u.FiveHour = resolveCodexWindow(w)
	case withinWindow(w.WindowMinutes, codexWeeklyMinutes):
		u.Weekly = resolveCodexWindow(w)
	}
}

// resolveCodexWindow converts a raw rollout window into the resolved shape,
// turning the unix-epoch reset into a time.Time (zero when absent).
func resolveCodexWindow(w *codexRateLimitWindow) *CodexRateLimitWindow {
	var resets time.Time
	if w.ResetsAt > 0 {
		resets = time.Unix(w.ResetsAt, 0).UTC()
	}
	return &CodexRateLimitWindow{UsedPercent: w.UsedPercent, ResetsAt: resets}
}

// parseCodexEventTime parses a rollout envelope timestamp ("2026-06-14T14:16:07.488Z",
// RFC3339 with milliseconds). Returns the zero time when empty or unparseable;
// the caller treats a zero Observed as stale, so an undated snapshot is simply
// not shown.
func parseCodexEventTime(ts string) time.Time {
	if ts == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}
