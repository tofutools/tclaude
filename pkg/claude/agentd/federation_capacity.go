package agentd

import (
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// A single node-wide gate covers every peer, human/automatic approval, and
// local managed launch. Durable requests/pending spawns retain reservations
// across restart; this map bridges the brief pre-persistence launch interval.
var nodeAdmission = struct {
	sync.Mutex
	launches map[string]string
}{launches: map[string]string{}}

func nodeCapacityLimit() (int, error) {
	cfg, err := config.Load()
	if err != nil {
		return 0, err
	}
	if cfg == nil || cfg.Federation == nil {
		return 0, nil
	}
	return cfg.Federation.MaxLiveAgents, nil
}
func nodeCapacityUsed(exclude string) (int, error) {
	// Read durable reservations before live bindings. Enrollment atomically
	// replaces a reservation with an actor; the opposite order could miss
	// both sides of that transition. Sample tmux last, after those bindings.
	pending, err := db.ListPendingSpawns()
	if err != nil {
		return 0, err
	}
	requests, err := db.ListFederationSpawnReservations()
	if err != nil {
		return 0, err
	}
	active, _, err := db.ListAgentRosterState()
	if err != nil {
		return 0, err
	}
	refs, err := db.HostSessionRefs()
	if err != nil {
		return 0, err
	}
	alive, err := session.LiveTmuxSessions()
	if err != nil {
		return 0, fmt.Errorf("cannot observe live panes: %w", err)
	}
	liveConv := map[string]bool{}
	for _, ref := range refs {
		if _, ok := alive[ref.TmuxSession]; ok {
			liveConv[ref.ConvID] = true
		}
	}
	used := map[string]bool{}
	for _, conv := range active {
		if liveConv[conv] {
			id, err := db.AgentIDForConv(conv)
			if err != nil {
				return 0, err
			}
			if id != "" {
				used[id] = true
			}
		}
	}
	for _, row := range pending {
		id := row.AgentID
		if id == "" {
			id = "pending:" + row.Label
		}
		used[id] = true
	}
	for _, req := range requests {
		if req.Status == db.FedSpawnPending && req.Expired(time.Now()) {
			continue
		}
		id := req.ResultAgent
		if id == "" {
			id = "request:" + strconv.FormatInt(req.ID, 10)
		}
		used[id] = true
	}
	for id, dir := range nodeAdmission.launches {
		if dir == config.DataDir() {
			used[id] = true
		}
	}
	delete(used, exclude)
	return len(used), nil
}
func acquireNodeLaunch(p *spawnParams) (func(), *spawnFailure) {
	nodeAdmission.Lock()
	defer nodeAdmission.Unlock()
	limit, err := nodeCapacityLimit()
	if err != nil {
		return nil, &spawnFailure{Status: 503, Kind: "node_capacity_unknown", Msg: "cannot read node capacity"}
	}
	if limit == 0 {
		return func() {}, nil
	}
	if p.AgentID == "" {
		p.AgentID = db.NewAgentID()
	}
	used, err := nodeCapacityUsed(p.AgentID)
	if err != nil {
		return nil, &spawnFailure{Status: 503, Kind: "node_capacity_unknown", Msg: "cannot observe node capacity"}
	}
	if used >= limit {
		return nil, &spawnFailure{Status: 409, Kind: fedCodeNodeBusy, Msg: "node live and reserved agent capacity reached"}
	}
	p.nodeCapacityReserved = true
	launchID := p.AgentID
	nodeAdmission.launches[launchID] = config.DataDir()
	return func() { nodeAdmission.Lock(); delete(nodeAdmission.launches, launchID); nodeAdmission.Unlock() }, nil
}

var errNodeBusy = errors.New("node live and reserved agent capacity reached")

func insertFederationSpawnWithCapacity(req *db.FederationSpawnRequest) (int64, error) {
	nodeAdmission.Lock()
	defer nodeAdmission.Unlock()
	prior, err := db.FederationSpawnRequestByEnvelope(req.FromInstance, req.EnvelopeID)
	if err != nil {
		return 0, err
	}
	if prior != nil {
		return 0, db.ErrFederationDuplicate
	}
	// A terminal busy receipt permits placement elsewhere. Persist that verdict
	// before acknowledging it so an in-flight resend cannot later create a
	// second worker when capacity becomes available (including after restart).
	busyKey := "spawnbusy:" + req.EnvelopeID
	if busy, err := db.FederationEnvelopeSeen(req.FromInstance, busyKey); err != nil {
		return 0, err
	} else if busy {
		return 0, errNodeBusy
	}
	limit, err := nodeCapacityLimit()
	if err != nil {
		return 0, err
	}
	if limit > 0 {
		used, err := nodeCapacityUsed("")
		if err != nil {
			return 0, err
		}
		if used >= limit {
			if _, err := db.MarkFederationEnvelopeSeen(req.FromInstance, busyKey, req.ExpiresAt); err != nil {
				return 0, err
			}
			return 0, errNodeBusy
		}
	}
	return db.InsertFederationSpawnRequest(req, fedSpawnPendingLimit)
}
