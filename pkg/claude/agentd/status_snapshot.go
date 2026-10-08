package agentd

import (
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// statusSnapshot is private gathered data, never a cached authorization result.
// Gathered maps and values are immutable; historical lookups use a separate
// synchronized memo. Consumers project their
// own current authority onto it; local private fields never become peer wire.
type statusSnapshot struct {
	observedAt   time.Time
	rowWork      map[string]time.Duration
	alive        map[string]struct{}
	sessions     map[string][]*db.SessionRow
	states       map[string]agentState
	activity     map[string]*time.Time
	tasks        map[string]db.AgentTaskRef
	names        map[string]string
	historicalMu sync.Mutex
	historical   map[string]*historicalStatusFlight
}
type historicalStatusFlight struct {
	done     chan struct{}
	state    agentState
	sessions []*db.SessionRow
}

type statusFlight struct {
	generation uint64
	done       chan struct{}
	value      *statusSnapshot
}
type statusSnapshotCache struct {
	mu         sync.Mutex
	key        string
	value      *statusSnapshot
	cachedAt   time.Time
	flight     *statusFlight
	generation uint64
	revision   func() uint64
}

var sharedStatusCache = statusSnapshotCache{revision: db.StatusSnapshotGeneration}

// A joining consumer returns this flight's result even if gathering exceeded
// the TTL. Otherwise a slow gather could trap every waiter in a refresh loop.
func (c *statusSnapshotCache) get(key string, ttl time.Duration, gather func() *statusSnapshot) *statusSnapshot {
	c.mu.Lock()
	generation := uint64(0)
	if c.revision != nil {
		generation = c.revision()
	}
	if c.key != key {
		c.key = key
		c.value = nil
		c.flight = nil
	}
	if c.value != nil && c.generation == generation && time.Since(c.cachedAt) < ttl {
		v := c.value
		c.mu.Unlock()
		return v
	}
	if c.flight != nil {
		f := c.flight
		c.mu.Unlock()
		<-f.done
		// A request arriving after a known mutation must not join an older gather.
		if c.revision != nil && f.generation != c.revision() {
			return c.get(key, ttl, gather)
		}
		return f.value
	}
	f := &statusFlight{done: make(chan struct{}), generation: generation}
	c.flight = f
	c.mu.Unlock()
	value := gather()
	c.mu.Lock()
	f.value = value
	if c.key == key && c.flight == f {
		c.value = value
		c.cachedAt = time.Now()
		c.generation = generation
		c.flight = nil
	}
	close(f.done)
	c.mu.Unlock()
	return value
}
func statusSnapshotWindow() time.Duration {
	cfg, err := config.Load()
	if err == nil && cfg != nil && cfg.StatusSnapshot != nil && cfg.StatusSnapshot.FreshnessMS > 0 {
		return time.Duration(min(cfg.StatusSnapshot.FreshnessMS, 60000)) * time.Millisecond
	}
	return 1500 * time.Millisecond
}
func gatheredStatusSnapshot() *statusSnapshot {
	return gatheredStatusSnapshotWithTimings(nil)
}
func gatheredStatusSnapshotWithTimings(record func([]perfPhase)) *statusSnapshot {
	return sharedStatusCache.get(config.DataDir(), statusSnapshotWindow(), func() *statusSnapshot {
		s := gatherStatusSnapshot()
		if record != nil {
			phases := []perfPhase{}
			for _, name := range rowWorkPhaseOrder {
				phases = append(phases, perfPhase{Name: name, Ms: durMs(s.rowWork[name])})
			}
			record(phases)
		}
		return s
	})
}

func gatherStatusSnapshot() *statusSnapshot {
	statusGatherTestHook()
	s := &statusSnapshot{observedAt: time.Now().UTC(), states: map[string]agentState{}, activity: map[string]*time.Time{}, tasks: map[string]db.AgentTaskRef{}, names: map[string]string{}, rowWork: map[string]time.Duration{}}
	s.alive, _ = cachedLiveTmuxSessions()
	// The common set covers every managed row the dashboard or CLI can render,
	// plus live plain wrapper sessions. It is gathered once, not per caller/peer.
	set := map[string]bool{}
	active, _, _ := db.ListAgentRosterState()
	for _, id := range active {
		set[id] = true
	}
	groups, _ := db.ListAgentGroups()
	gids := []int64{}
	for _, g := range groups {
		gids = append(gids, g.ID)
	}
	members, _ := db.ListAgentGroupMembersBatch(gids)
	for _, ms := range members {
		for _, m := range ms {
			set[m.ConvID] = true
		}
	}
	owners, _ := db.ListAgentGroupOwnersBatch(gids)
	for _, os := range owners {
		for _, o := range os {
			set[o.ConvID] = true
		}
	}
	overrides, _ := db.ListAllAgentPermissionOverrides()
	for id := range overrides {
		set[id] = true
	}
	sudo, _ := db.ListAllActiveSudoGrants()
	for _, g := range sudo {
		set[g.ConvID] = true
	}
	refs, _ := db.HostSessionRefs()
	for _, r := range refs {
		set[r.ConvID] = true
	}
	ids := make([]string, 0, len(set))
	for id := range set {
		if id != "" {
			ids = append(ids, id)
		}
	}
	s.sessions, _ = db.FindSessionsByConvIDs(ids)
	index, _ := db.GetConvIndexBatch(ids)
	actors, _ := db.AgentsByConv(ids)
	actorIDs := []string{}
	for _, a := range actors {
		if a.AgentID != "" {
			actorIDs = append(actorIDs, a.AgentID)
		}
	}
	tasks, _ := db.ListAgentTaskRefsByAgentIDs(actorIDs)
	var batch codexContextWriteBatch
	for _, id := range ids {
		state := stateForConvInSessionsBatched(s.sessions[id], s.alive, &batch, nil, func(name string, d time.Duration) { s.rowWork[name] += d })
		state.TemporaryHarnessBuiltinMode = actors[id].TemporaryHarnessBuiltinMode
		s.states[id] = state
		s.tasks[id] = tasks[actors[id].AgentID]
		// Explicit names only: summaries and first prompts are private content.
		name := actors[id].PendingName
		if row := index[id]; row != nil && row.CustomTitle != "" {
			name = row.CustomTitle
		}
		if name == "" {
			name = actors[id].AgentID
		}
		s.names[id] = name
		var at time.Time
		if row := index[id]; row != nil {
			at, _ = time.Parse(time.RFC3339Nano, row.Modified)
		}
		for _, r := range s.sessions[id] {
			if r.LastHook.After(at) {
				at = r.LastHook
			}
		}
		if !at.IsZero() {
			t := at
			s.activity[id] = &t
		}
	}
	_, _ = batch.flush()
	return s
}

// The hook lives here so flow tests can block/count the production gather at
// its single shared boundary. It is nil during normal daemon operation.
var statusHook struct {
	sync.RWMutex
	fn func()
}

func statusGatherTestHook() {
	statusHook.RLock()
	fn := statusHook.fn
	statusHook.RUnlock()
	if fn != nil {
		fn()
	}
}

// Historical, non-enrolled conversations are outside the common live roster.
// Resolve one only when explicitly requested and memoize it for this snapshot's
// lifetime, including concurrent readers. This avoids scanning all history on
// every dashboard poll while preserving stored context for human reads.
func (s *statusSnapshot) stateFor(conv string) (agentState, []*db.SessionRow) {
	if state, ok := s.states[conv]; ok {
		return state, s.sessions[conv]
	}
	s.historicalMu.Lock()
	if f := s.historical[conv]; f != nil {
		s.historicalMu.Unlock()
		<-f.done
		return f.state, f.sessions
	}
	if s.historical == nil {
		s.historical = map[string]*historicalStatusFlight{}
	}
	f := &historicalStatusFlight{done: make(chan struct{})}
	s.historical[conv] = f
	s.historicalMu.Unlock()
	f.sessions, _ = db.FindSessionsByConvID(conv)
	f.state = stateForConvInSessions(f.sessions, s.alive)
	close(f.done)
	return f.state, f.sessions
}
func (s *statusSnapshot) contextFor(conv string) (db.ContextSnapshot, string, bool) {
	state, rows := s.stateFor(conv)
	row := pickWithLiveness(rows, func(tmux string) bool { _, ok := s.alive[tmux]; return ok })
	if row == nil {
		return db.ContextSnapshot{}, "", false
	}
	return db.ContextSnapshot{ContextPct: state.ContextPct, TokensInput: state.TokensInput, TokensOutput: state.TokensOutput, ContextWindowSize: state.ContextWindowSize, Model: state.Model}, row.ID, true
}
