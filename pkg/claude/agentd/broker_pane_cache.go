package agentd

import (
	"strconv"
	"strings"
	"sync"
	"time"
)

// --- shared pane-facts cache for the brokered proof ---
//
// proveTclaudeLayerCaller needs three facts about the claimed session's
// pane: its pid, whether it is dead, and the launch generation tclaude
// stamped on it at spawn. It used to fetch them with one `tmux
// display-message` per brokered request — a round trip to the single-
// threaded tmux server for every statusline render and every hook event,
// for facts that change only at spawn. With ten or twenty agents that was
// a steady stream of tmux calls competing with the panes' own output.
//
// One `tmux list-panes -a` returns the same facts for every pane at once.
// This cache runs it at most once per brokerPaneCacheTTL, single-flight, so
// the cost is flat in the agent count, and the proof reads its facts from
// the cached table. A cache MISS — the session is absent, has more than one
// pane, reads dead, or carries a different generation — falls back to the
// direct probe, so a session spawned or relaunched inside the TTL is still
// proved against live tmux.
//
// Why a stale table is safe here: pane pid and generation are fixed for a
// pane's lifetime. A stale "live" for a pane that just died cannot pass the
// proof, because a dead pane shell's children are reparented and the pane
// pid is then not an ancestor of the caller. A stale "absent" or "dead" for
// a live pane is exactly the fallback case.

// brokerPaneListFormat is the list-panes format the cache reads. Change it
// together with the simulator's model of it in pkg/testharness.
const brokerPaneListFormat = "#{session_name}|#{pane_id}|#{pane_pid}|#{pane_dead}|#{@tclaude_exit_generation}"

// brokerPaneCacheTTL bounds how stale the shared table may be. Within it a
// long-lived pane's facts do not change; a new pane misses and is probed.
const brokerPaneCacheTTL = 2 * time.Second

// brokerPaneEntry is one session's pane as list-panes reported it.
type brokerPaneEntry struct {
	paneID     string
	panePID    int
	dead       bool
	generation string
	// panes counts the panes list-panes reported for the session. The
	// direct probe targets the active pane of the current window; a
	// session with several panes is ambiguous here and is probed instead.
	panes int
}

// brokerPaneListFn is the whole-table read behind the cache, indirected so
// tests can turn the cache off (TestMain does, so the existing proofs keep
// exercising the direct probe) or feed it a fixed table.
var brokerPaneListFn = listBrokerPanes

var brokerPaneCache struct {
	mu      sync.Mutex
	table   map[string]brokerPaneEntry
	taken   time.Time
	loading bool
	done    chan struct{}
}

func listBrokerPanes() (map[string]brokerPaneEntry, error) {
	out, err := tmuxOutputWithTimeout("list-panes", "-a", "-F", brokerPaneListFormat)
	if err != nil {
		return nil, err
	}
	return parseBrokerPaneList(string(out)), nil
}

// parseBrokerPaneList parses brokerPaneListFormat lines. A session with
// several panes keeps its first pane's facts and a panes count above one,
// which cachedBrokerPane treats as a miss.
func parseBrokerPaneList(out string) map[string]brokerPaneEntry {
	table := map[string]brokerPaneEntry{}
	for _, line := range strings.Split(out, "\n") {
		parts := strings.Split(strings.TrimSpace(line), "|")
		if len(parts) != 5 || parts[0] == "" {
			continue
		}
		e, seen := table[parts[0]]
		if seen {
			e.panes++
			table[parts[0]] = e
			continue
		}
		pid, _ := strconv.Atoi(parts[2])
		table[parts[0]] = brokerPaneEntry{
			paneID: parts[1], panePID: pid, dead: parts[3] == "1", generation: parts[4], panes: 1,
		}
	}
	return table
}

// cachedBrokerPanes serves the shared table, refreshing it single-flight
// when it is older than brokerPaneCacheTTL. A failed read leaves the
// previous table in place (if any) and returns nil for this caller, which
// is a miss.
func cachedBrokerPanes() map[string]brokerPaneEntry {
	c := &brokerPaneCache
	for {
		c.mu.Lock()
		if c.table != nil && time.Since(c.taken) < brokerPaneCacheTTL {
			t := c.table
			c.mu.Unlock()
			return t
		}
		if c.loading {
			done := c.done
			c.mu.Unlock()
			<-done
			continue
		}
		c.loading = true
		c.done = make(chan struct{})
		done := c.done
		c.mu.Unlock()

		table, err := brokerPaneListFn()

		c.mu.Lock()
		if err == nil && table != nil {
			c.table, c.taken = table, time.Now()
		}
		c.loading = false
		close(done)
		c.mu.Unlock()
		if err != nil {
			return nil
		}
		return table
	}
}

// cachedBrokerPane returns the cached facts for one session when they are
// usable for the proof: present, a single pane, alive, and carrying the
// generation the row expects. Anything else is a miss and the caller
// probes live tmux.
func cachedBrokerPane(tmuxSession, wantGeneration string) (lifecyclePaneProbe, bool) {
	if tmuxSession == "" || wantGeneration == "" {
		return lifecyclePaneProbe{}, false
	}
	e, ok := cachedBrokerPanes()[tmuxSession]
	if !ok || e.panes != 1 || e.dead || e.panePID <= 1 || e.generation != wantGeneration || !validLifecyclePaneID(e.paneID) {
		return lifecyclePaneProbe{}, false
	}
	return lifecyclePaneProbe{state: paneProbeLive, paneID: e.paneID, panePID: e.panePID, generation: e.generation}, true
}

// brokerPaneFacts is what the proof calls: the cache first, the direct
// probe on a miss. source names which answered, for the slow-request log.
func brokerPaneFacts(tmuxSession, wantGeneration string) (pane lifecyclePaneProbe, source string, err error) {
	if p, ok := cachedBrokerPane(tmuxSession, wantGeneration); ok {
		return p, "cache", nil
	}
	p, err := brokerLivePaneProbe(tmuxSession)
	return p, "probe", err
}
