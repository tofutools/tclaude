package agentd

import (
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// statusSnapshot is private gathered data, never a cached authorization result.
// All maps and values are immutable after publication. Consumers project their
// own current authority onto it; local private fields never become peer wire.
type statusSnapshot struct {
	observedAt time.Time
	alive      map[string]struct{}
	sessions   map[string][]*db.SessionRow
	states     map[string]agentState
	activity   map[string]*time.Time
	tasks      map[string]db.AgentTaskRef
}
type statusFlight struct {
	done  chan struct{}
	value *statusSnapshot
}
type statusSnapshotCache struct {
	mu     sync.Mutex
	key    string
	value  *statusSnapshot
	flight *statusFlight
}

var sharedStatusCache statusSnapshotCache

// A joining consumer returns this flight's result even if gathering exceeded
// the TTL. Otherwise a slow gather could trap every waiter in a refresh loop.
func (c *statusSnapshotCache) get(key string, ttl time.Duration, gather func() *statusSnapshot) *statusSnapshot {
	c.mu.Lock()
	if c.key != key {
		c.key = key
		c.value = nil
		c.flight = nil
	}
	if c.value != nil && time.Since(c.value.observedAt) < ttl {
		v := c.value
		c.mu.Unlock()
		return v
	}
	if c.flight != nil {
		f := c.flight
		c.mu.Unlock()
		<-f.done
		return f.value
	}
	f := &statusFlight{done: make(chan struct{})}
	c.flight = f
	c.mu.Unlock()
	value := gather()
	c.mu.Lock()
	f.value = value
	if c.key == key && c.flight == f {
		c.value = value
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
	return sharedStatusCache.get(config.DataDir(), statusSnapshotWindow(), gatherStatusSnapshot)
}

func gatherStatusSnapshot() *statusSnapshot {
	statusGatherTestHook()
	s := &statusSnapshot{observedAt: time.Now().UTC(), states: map[string]agentState{}, activity: map[string]*time.Time{}, tasks: map[string]db.AgentTaskRef{}}
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
		state := stateForConvInSessionsBatched(s.sessions[id], s.alive, &batch, nil, nil)
		state.TemporaryHarnessBuiltinMode = actors[id].TemporaryHarnessBuiltinMode
		s.states[id] = state
		s.tasks[id] = tasks[actors[id].AgentID]
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

func (s *statusSnapshot) contextFor(conv string) (db.ContextSnapshot, string, bool) {
	state := s.states[conv]
	row := pickWithLiveness(s.sessions[conv], func(tmux string) bool { _, ok := s.alive[tmux]; return ok })
	if row == nil {
		return db.ContextSnapshot{}, "", false
	}
	return db.ContextSnapshot{ContextPct: state.ContextPct, TokensInput: state.TokensInput, TokensOutput: state.TokensOutput, ContextWindowSize: state.ContextWindowSize, Model: state.Model}, row.ID, true
}
