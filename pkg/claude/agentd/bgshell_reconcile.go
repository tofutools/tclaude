package agentd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	platformexec "github.com/tofutools/tclaude/pkg/claude/platform/execution"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

const (
	bgShellReconcileMinInterval = time.Second
	bgShellCacheTTL             = 5 * time.Minute
	bgShellRefreshAfter         = db.BgShellTTL / 4
	monitorRefreshAfter         = db.MonitorTTL / 4
)

type backgroundObservationValidity uint8

const (
	backgroundObservationUnknown backgroundObservationValidity = iota
	backgroundObservationKnown
)

type backgroundObservationSubject struct {
	ProjectionRef db.BackgroundProjectionRef
	// ProcessInstance is host proof for the main PID during this sample. It is
	// intentionally empty for a current known-empty tracked ledger, which has
	// no process-table assertion to make.
	ProcessInstance string
	ShellInput      string
	MonitorInput    string
	TracksShells    bool
	TracksMonitors  bool
}

// backgroundWorkObservation is a typed assertion about the shell/monitor
// ledgers of one execution and main-process instance. It is immutable once
// collected; cache hits return these same sample boundaries and evidence ID.
type backgroundWorkObservation struct {
	Subject     backgroundObservationSubject
	Observer    string
	EvidenceID  string
	StartedAt   time.Time
	CompletedAt time.Time
	Validity    backgroundObservationValidity
	Failure     string
	Shells      session.BgShellLiveness
	Monitors    session.BgShellLiveness
	ShellSet    db.BgShellSet
	MonitorSet  db.MonitorSet
}

type backgroundCounts struct {
	Shells   int
	Monitors int
}

func (c backgroundCounts) any() bool { return c.Shells > 0 || c.Monitors > 0 }

// backgroundResolution is the pure reader interpretation of an observation.
// Counts retain the legacy TTL display fallback. ConfirmedIdle is stricter:
// unknown process evidence and expired undecided entries cannot establish it.
type backgroundResolution struct {
	Counts        backgroundCounts
	ConfirmedIdle bool
}

type backgroundObservationCacheKey struct {
	SessionID      string
	ExecutionID    platformexec.ID
	SessionCreated time.Time
	MainPID        int
	Process        string
	ShellInput     string
	MonitorInput   string
	TracksShells   bool
	TracksMonitors bool
}

type backgroundObservationCacheEntry struct {
	key         backgroundObservationCacheKey
	observation backgroundWorkObservation
}

var bgShellReconcileMu struct {
	sync.Mutex
	last map[string]backgroundObservationCacheEntry
}

var (
	bgShellDescendantCommandLines = session.DescendantCommandLines
	backgroundMainProcessInstance = hostProcessInstance
)

func backgroundResolutionOnRead(sess *db.SessionRow, alive bool) backgroundResolution {
	return resolveBackgroundObservation(observeBackgroundWork(sess, alive, time.Now()))
}

func observeBackgroundWork(sess *db.SessionRow, alive bool, now time.Time) backgroundWorkObservation {
	started := now
	observation := backgroundWorkObservation{
		Observer:    "host-descendant-processes",
		StartedAt:   started,
		CompletedAt: started,
		Validity:    backgroundObservationKnown,
	}
	if sess == nil || !alive || sess.ID == "" {
		observation.EvidenceID = backgroundEvidenceID(observation)
		return observation
	}

	observation.Subject = backgroundObservationSubject{
		ProjectionRef: db.BackgroundProjectionRef{
			Attempt: platformexec.AttemptRef{
				ExecutionID: sess.ExecutionID, LegacySessionID: sess.ID,
			},
			SessionCreatedAt: sess.CreatedAt,
			MainPID:          sess.PID,
		},
		ShellInput:   sess.BgShellsJSON,
		MonitorInput: sess.MonitorsJSON,
	}
	if h, err := harness.Resolve(sess.Harness); err == nil {
		observation.Subject.TracksShells = h.SupportsBackgroundShells()
		observation.Subject.TracksMonitors = h.SupportsMonitors()
	}

	if observation.Subject.TracksShells {
		if observation.Subject.ShellInput != "" && !json.Valid([]byte(observation.Subject.ShellInput)) {
			observation.Failure = "invalid_shell_ledger"
		} else {
			observation.ShellSet = db.ParseBgShellSet(observation.Subject.ShellInput)
		}
	}
	if observation.Subject.TracksMonitors {
		if observation.Subject.MonitorInput != "" && !json.Valid([]byte(observation.Subject.MonitorInput)) {
			if observation.Failure == "" {
				observation.Failure = "invalid_monitor_ledger"
			}
		} else {
			observation.MonitorSet = db.ParseMonitorSet(observation.Subject.MonitorInput)
		}
	}
	if observation.Failure != "" {
		return unknownBackgroundObservation(observation, observation.Failure)
	}

	// Current known-empty tracked ledgers have direct fact-specific evidence:
	// there are no tracked entries. They make no claim about arbitrary OS
	// descendants and do not require a process scan.
	if len(observation.ShellSet) == 0 && len(observation.MonitorSet) == 0 {
		observation.EvidenceID = backgroundEvidenceID(observation)
		return observation
	}

	processInstance, ok := backgroundMainProcessInstance(sess.PID)
	if !ok || processInstance == "" {
		return unknownBackgroundObservation(observation, "main_process_unverified")
	}
	observation.Subject.ProcessInstance = processInstance
	key := backgroundCacheKey(observation.Subject)
	if cached, ok := cachedBackgroundObservation(key, now); ok {
		return cached
	}

	cmdlines, ok := bgShellDescendantCommandLines(sess.PID)
	if !ok {
		return unknownBackgroundObservation(observation, "process_enumeration_failed")
	}
	completed := time.Now()
	if now.After(completed) {
		completed = now
	}
	// Revalidate after the potentially slow scan. A PID reused or a main
	// process replaced during collection makes the whole sample unknown.
	currentInstance, ok := backgroundMainProcessInstance(sess.PID)
	if !ok || currentInstance != processInstance {
		observation.CompletedAt = completed
		return unknownBackgroundObservation(observation, "main_process_changed")
	}

	monitorCandidates := make(db.MonitorSet, len(observation.MonitorSet))
	for id, entry := range observation.MonitorSet {
		if !entry.Deadline.IsZero() && !now.Before(entry.Deadline) {
			observation.Monitors.Dead = append(observation.Monitors.Dead, id)
			continue
		}
		monitorCandidates[id] = entry
	}
	verdict := session.ReconcileBackground(observation.ShellSet, monitorCandidates, cmdlines)
	observation.Shells = verdict.Shells
	observation.Monitors.Alive = verdict.Monitors.Alive
	observation.Monitors.Undecided = verdict.Monitors.Undecided
	observation.Monitors.Dead = append(observation.Monitors.Dead, verdict.Monitors.Dead...)
	sort.Strings(observation.Monitors.Dead)
	observation.CompletedAt = completed
	observation.EvidenceID = backgroundEvidenceID(observation)
	storeBackgroundObservation(key, observation, now)
	return observation
}

func unknownBackgroundObservation(observation backgroundWorkObservation, reason string) backgroundWorkObservation {
	observation.Validity = backgroundObservationUnknown
	observation.Failure = reason
	observation.EvidenceID = backgroundEvidenceID(observation)
	return observation
}

func resolveBackgroundObservation(observation backgroundWorkObservation) backgroundResolution {
	now := observation.CompletedAt
	if observation.Validity != backgroundObservationKnown {
		return backgroundResolution{Counts: backgroundCounts{
			Shells:   observation.ShellSet.LiveCount(now),
			Monitors: observation.MonitorSet.LiveCount(now),
		}}
	}
	liveShells := observation.ShellSet.Live(now)
	liveMonitors := observation.MonitorSet.Live(now)
	resolution := backgroundResolution{}
	resolution.Counts.Shells = len(observation.Shells.Alive) +
		countBackgroundIDs(observation.Shells.Undecided, liveShells)
	resolution.Counts.Monitors = len(observation.Monitors.Alive) +
		countBackgroundIDs(observation.Monitors.Undecided, liveMonitors)
	resolution.ConfirmedIdle = !resolution.Counts.any() &&
		len(observation.Shells.Undecided) == 0 && len(observation.Monitors.Undecided) == 0
	return resolution
}

func countBackgroundIDs[V any](ids []string, entries map[string]V) int {
	n := 0
	for _, id := range ids {
		if _, ok := entries[id]; ok {
			n++
		}
	}
	return n
}

type backgroundProjectionResult struct {
	Current       bool
	ShellOutput   string
	MonitorOutput string
}

// projectBackgroundObservation is the daemon-owned reconciliation writer.
// Interactive dashboard/terminal readers never call it.
func projectBackgroundObservation(observation backgroundWorkObservation, now time.Time) backgroundProjectionResult {
	result := backgroundProjectionResult{
		ShellOutput:   observation.Subject.ShellInput,
		MonitorOutput: observation.Subject.MonitorInput,
	}
	if observation.Validity != backgroundObservationKnown ||
		observation.Subject.ProjectionRef.Attempt.LegacySessionID == "" {
		return result
	}
	if observation.Subject.ProcessInstance != "" {
		current, ok := backgroundMainProcessInstance(observation.Subject.ProjectionRef.MainPID)
		if !ok || current != observation.Subject.ProcessInstance {
			return result
		}
	}

	if observation.Subject.TracksShells {
		next := db.ParseBgShellSet(observation.Subject.ShellInput)
		for _, id := range observation.Shells.Dead {
			next.Remove(id)
		}
		for _, id := range observation.Shells.Alive {
			if entry, ok := next[id]; ok && now.Sub(entry.Seen) > bgShellRefreshAfter {
				next.Refresh(id, now)
			}
		}
		result.ShellOutput = next.Encode()
	}
	if observation.Subject.TracksMonitors {
		next := db.ParseMonitorSet(observation.Subject.MonitorInput)
		for _, id := range observation.Monitors.Dead {
			next.Remove(id)
		}
		for _, id := range observation.Monitors.Alive {
			if entry, ok := next[id]; ok && now.Sub(entry.Seen) > monitorRefreshAfter {
				next.Refresh(id, now)
			}
		}
		result.MonitorOutput = next.Encode()
	}

	current, err := db.ProjectSessionBackgroundLedgers(
		observation.Subject.ProjectionRef,
		observation.Subject.ShellInput, observation.Subject.MonitorInput,
		result.ShellOutput, result.MonitorOutput,
	)
	if err != nil {
		slog.Warn("background projector: persist reconciled ledgers failed",
			"session_id", observation.Subject.ProjectionRef.Attempt.LegacySessionID,
			"error", err, "module", "agentd")
		return result
	}
	result.Current = current
	return result
}

func backgroundCacheKey(subject backgroundObservationSubject) backgroundObservationCacheKey {
	return backgroundObservationCacheKey{
		SessionID:      subject.ProjectionRef.Attempt.LegacySessionID,
		ExecutionID:    subject.ProjectionRef.Attempt.ExecutionID,
		SessionCreated: subject.ProjectionRef.SessionCreatedAt,
		MainPID:        subject.ProjectionRef.MainPID,
		Process:        subject.ProcessInstance,
		ShellInput:     subject.ShellInput,
		MonitorInput:   subject.MonitorInput,
		TracksShells:   subject.TracksShells,
		TracksMonitors: subject.TracksMonitors,
	}
}

func cachedBackgroundObservation(key backgroundObservationCacheKey, now time.Time) (backgroundWorkObservation, bool) {
	bgShellReconcileMu.Lock()
	defer bgShellReconcileMu.Unlock()
	entry, ok := bgShellReconcileMu.last[key.SessionID]
	if !ok || entry.key != key || now.Sub(entry.observation.CompletedAt) >= bgShellReconcileMinInterval {
		return backgroundWorkObservation{}, false
	}
	return entry.observation, true
}

func storeBackgroundObservation(key backgroundObservationCacheKey, observation backgroundWorkObservation, now time.Time) {
	bgShellReconcileMu.Lock()
	defer bgShellReconcileMu.Unlock()
	if bgShellReconcileMu.last == nil {
		bgShellReconcileMu.last = map[string]backgroundObservationCacheEntry{}
	}
	for id, entry := range bgShellReconcileMu.last {
		if now.Sub(entry.observation.CompletedAt) > bgShellCacheTTL {
			delete(bgShellReconcileMu.last, id)
		}
	}
	bgShellReconcileMu.last[key.SessionID] = backgroundObservationCacheEntry{key: key, observation: observation}
}

func backgroundEvidenceID(observation backgroundWorkObservation) string {
	h := sha256.New()
	_, _ = h.Write([]byte(observation.Observer))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(observation.Subject.ProjectionRef.Attempt.LegacySessionID))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(observation.Subject.ProjectionRef.Attempt.ExecutionID))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(strconv.Itoa(observation.Subject.ProjectionRef.MainPID)))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(observation.Subject.ProcessInstance))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(observation.Subject.ShellInput))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(observation.Subject.MonitorInput))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(strconv.Itoa(int(observation.Validity))))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(observation.Failure))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(observation.CompletedAt.UTC().Format(time.RFC3339Nano)))
	sum := h.Sum(nil)
	return "bgw_" + hex.EncodeToString(sum[:12])
}
