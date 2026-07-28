package agentd

import (
	"fmt"
	"strings"
	"sync"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// A spawn's LABEL is the tclaude-side session id: the sessions-table PK, the
// key the hook callback tracks conv-id rotations against, the pending_spawns
// PK, and — because `session new --label` feeds it straight to TmuxNameBase —
// the tmux session name the human attaches to. Historically it was always a
// random "spwn-XXXXXX" token (generateSpawnLabel), which is opaque but
// collision-free by construction.
//
// With config agent.spawn_label_from_name on, it is derived from the agent's
// name instead, so `tclaude agent spawn --name code-reviewer` is reachable as
// `tclaude session attach code-reviewer` and shows up under that name in
// `tmux ls`. A name is NOT unique, so the derived label has to be
// disambiguated the way `session new` disambiguates a taken tmux name
// (UniqueTmuxSessionName): the bare base first, then "-2", "-3", … .
//
// Uniqueness is checked more strictly here than in `session new`, which only
// rejects a LIVE owner. A session id owns durable per-session history —
// session_cost_daily, codex_telemetry_checkpoint, opencode_runtimes,
// notify_state — so reusing a dead namesake's id would silently conflate two
// different agents' costs and telemetry under one key. Any existing session
// row therefore counts as taken, live or not. (Exited rows are eventually
// reaped by db.CleanupOldExited, which is what keeps the suffixes from
// climbing forever.)

const (
	// spawnLabelMaxNumericSuffix bounds the "-N" ladder before the sequence
	// switches to a random suffix. `session new` climbs to 1000; a spawn label
	// is a durable PK rather than a transient tmux name, so a shorter ladder
	// keeps the worst-case probe count low and still reads naturally
	// ("code-reviewer-7").
	spawnLabelMaxNumericSuffix = 99
	// spawnLabelRandomAttempts bounds the "<base>-XXXXXX" tier that follows,
	// which keeps the name recognisable when the ladder is exhausted.
	spawnLabelRandomAttempts = 8
	// spawnLabelSuffixBudget reserves room for the longest suffix the tiers
	// above can append ("-" + 6 hex chars) so the final label still clears the
	// same agent.MaxSpawnNameLen gate the name itself passed.
	spawnLabelSuffixBudget = 7
)

// spawnLabelSequence returns a generator of candidate labels for a spawn of an
// agent called name. Each call yields the next candidate that is not already
// taken; callers that mint one label call it once, and the layered-launch path
// (reserveUniqueSpawnPrivateAttachmentRootWith) calls it repeatedly until its
// private attachment root is created exclusively.
//
// When agent.spawn_label_from_name is off (the default), or the name is empty
// or normalizes to nothing, this is exactly generateSpawnLabel — the
// historical behaviour, byte for byte.
//
// Config is read live, once per spawn, so a Config-tab toggle takes effect
// without a daemon restart (the same no-cache philosophy as
// SpawnNameNormalizeEnabled's callers).
func spawnLabelSequence(name string) func() string {
	base := spawnLabelBase(name)
	if base == "" {
		return generateSpawnLabel
	}
	taken := newSpawnLabelTakenFn()
	// suffix 0 means the bare base; the ladder then runs 2..N, matching
	// UniqueTmuxSessionName (which also skips "-1").
	suffix := 0
	randomLeft := spawnLabelRandomAttempts
	return func() string {
		for suffix <= spawnLabelMaxNumericSuffix {
			candidate := base
			if suffix > 0 {
				candidate = fmt.Sprintf("%s-%d", base, suffix)
			}
			if suffix == 0 {
				suffix = 2
			} else {
				suffix++
			}
			if !taken(candidate) {
				return candidate
			}
		}
		for randomLeft > 0 {
			randomLeft--
			if candidate := base + "-" + randomLabelToken(); !taken(candidate) {
				return candidate
			}
		}
		// Every tier exhausted — either an improbable pile of namesakes or a
		// broken database making every probe read as taken. Fall back to the
		// random token so a spawn never fails over its cosmetic label.
		return generateSpawnLabel()
	}
}

// spawnLabelBase returns the sanitized, length-budgeted stem a spawn's label is
// built from, or "" to mean "use the random token instead" — the flag is off,
// the agent is unnamed, or nothing survived normalization.
//
// NormalizeSpawnName runs unconditionally, independent of
// agent.spawn_name_normalize: that toggle governs whether an unsafe name is
// rejected at the spawn boundary, but a name can still reach here unvetted
// (handleTemplateInstantiate builds group+template names and bypasses the
// boundary gate), and a label lands in a tmux session name and a filesystem
// path, so it must be a safe token regardless.
func spawnLabelBase(name string) string {
	cfg, _ := config.Load()
	if !cfg.SpawnLabelFromNameEnabled() {
		return ""
	}
	base := agent.NormalizeSpawnName(strings.TrimSpace(name))
	if budget := agent.MaxSpawnNameLen - spawnLabelSuffixBudget; len(base) > budget {
		base = strings.TrimRight(base[:budget], "-")
	}
	return base
}

// mintedSpawnLabels remembers every name-derived label this daemon has handed
// out, closing the window between picking a candidate and the forked `session
// new` writing its row: without it two concurrent spawns of the same name both
// see "free" and the loser's SaveSession ON CONFLICT(id) would silently
// overwrite the winner's row. A random label never had this window, so nothing
// records into the set while the feature is off.
//
// Entries are never released. A label stays claimed for the daemon's lifetime
// even if its spawn fails, which costs one skipped candidate and a few bytes
// per spawn — much cheaper than reasoning about when a half-launched pane has
// stopped being able to write that row.
var mintedSpawnLabels = struct {
	sync.Mutex
	seen map[string]struct{}
}{seen: map[string]struct{}{}}

// resetMintedSpawnLabelsForTest drops the process-wide reservation set so each
// test starts from a clean slate (it outlives any per-test temp DB).
func resetMintedSpawnLabelsForTest() {
	mintedSpawnLabels.Lock()
	defer mintedSpawnLabels.Unlock()
	mintedSpawnLabels.seen = map[string]struct{}{}
}

// ResetSpawnLabelsForTest is resetMintedSpawnLabelsForTest for the flow tests,
// which live in package agentd_test.
func ResetSpawnLabelsForTest() { resetMintedSpawnLabelsForTest() }

// claimSpawnLabel reserves label in the process-wide set, reporting false if
// another in-flight spawn got there first.
func claimSpawnLabel(label string) bool {
	mintedSpawnLabels.Lock()
	defer mintedSpawnLabels.Unlock()
	if _, dup := mintedSpawnLabels.seen[label]; dup {
		return false
	}
	mintedSpawnLabels.seen[label] = struct{}{}
	return true
}

// newSpawnLabelTakenFn returns the "is this label already claimed?" predicate
// the sequence probes with, which CLAIMS the first free candidate it sees —
// so it must only be called on candidates the caller is about to use. The
// live-tmux set is snapshotted once per spawn (one `tmux ls`) and reused
// across candidates; the DB probes are indexed primary-key reads, so they stay
// per-candidate and see writes a concurrent spawn just made.
//
// A probe ERROR counts as taken: skipping to the next candidate is cheap,
// whereas guessing "free" risks overwriting a live session row's PK. If the
// database is down every candidate reads as taken and the sequence falls
// through to the random token, which is the correct degradation.
func newSpawnLabelTakenFn() func(string) bool {
	aliveTmux, err := session.LiveTmuxSessions()
	if err != nil {
		aliveTmux = nil
	}
	return func(label string) bool {
		if _, alive := aliveTmux[label]; alive {
			return true
		}
		if exists, err := db.SessionExists(label); err != nil || exists {
			return true
		}
		if pending, err := db.GetPendingSpawn(label); err != nil || pending != nil {
			return true
		}
		return !claimSpawnLabel(label)
	}
}
