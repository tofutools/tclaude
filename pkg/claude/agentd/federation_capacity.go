package agentd

import (
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/hostmetrics"
)

// A single node-wide gate covers every peer, human/automatic approval, and
// local managed launch. Durable requests/pending spawns retain reservations
// across restart; this map bridges the brief pre-persistence launch interval.
var nodeAdmission = struct {
	sync.Mutex
	launches map[string]string
}{launches: map[string]string{}}

func nodeCapacityLimit() (int, error) {
	limits, err := nodeCapacityLimits()
	return limits.MaxLiveAgents, err
}

// nodeCapacityLimits is the resolved capacity policy: capacity.* with the
// legacy federation.max_live_agents as the fallback agent cap.
func nodeCapacityLimits() (config.CapacityLimits, error) {
	cfg, err := config.Load()
	if err != nil {
		return config.CapacityLimits{}, err
	}
	return cfg.ResolvedCapacity(), nil
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
	jobs, err := db.ListFederationJobs(true)
	if err != nil {
		return 0, err
	}
	teleports, err := db.ListFederationTeleports()
	if err != nil {
		return 0, err
	}
	actors, err := db.FederationCapacityActorSessions()
	if err != nil {
		return 0, err
	}
	alive, err := session.LiveTmuxSessions()
	if err != nil {
		return 0, fmt.Errorf("cannot observe live panes: %w", err)
	}
	used := map[string]bool{}
	for _, actor := range actors {
		if _, ok := alive[actor.TmuxSession]; ok {
			used[actor.AgentID] = true
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
	for _, job := range jobs {
		id := job.WorkerID
		if id == "" {
			id = "job:" + job.ID
		}
		used[id] = true
	}
	for id, dir := range nodeAdmission.launches {
		if dir == config.DataDir() {
			used[id] = true
		}
	}
	for _, t := range teleports {
		if t.Direction == "in" && (t.State == "admitting" || t.State == "uncertain") && t.TargetAgent != "" {
			used[t.TargetAgent] = true
		}
	}
	delete(used, exclude)
	return len(used), nil
}

// capacityPreAdmittedKey marks an in-process launch whose capacity a durable
// reservation already counts (see spawnParams.capacityPreAdmitted).
type capacityPreAdmittedKey struct{}

func acquireNodeLaunch(p *spawnParams) (func(), *spawnFailure) {
	nodeAdmission.Lock()
	defer nodeAdmission.Unlock()
	limits, err := nodeCapacityLimits()
	if err != nil {
		return nil, &spawnFailure{Status: 503, Kind: "node_capacity_unknown", Msg: "cannot read node capacity"}
	}
	queued, err := db.CountActiveSpawnQueue()
	if err != nil {
		return nil, &spawnFailure{Status: 503, Kind: "node_capacity_unknown", Msg: "cannot read the spawn queue"}
	}
	// A launch from the queue always reserves, so restart recovery can tell a
	// launched entry from one that never reached the spawner.
	if !limits.Limited() && queued == 0 && !p.capacityQueueHead {
		return func() {}, nil
	}
	// One line for everyone: while spawns wait, a launch whose capacity no
	// durable reservation already counts (triggers, templates, AWB pickups,
	// bundle imports) does not take a slot ahead of the queue's head.
	if queued > 0 && !p.capacityQueueHead && !p.capacityPreAdmitted && p.pendingSpawnLabel == "" {
		return nil, &spawnFailure{Status: 409, Kind: fedCodeNodeBusy,
			Msg: fmt.Sprintf("node capacity reached: %d queued spawn(s) wait ahead", queued)}
	}
	if p.AgentID == "" {
		p.AgentID = db.NewAgentID()
	}
	if reason, err := nodeCapacityFullLocked(limits, p.AgentID, []string{p.Cwd}); err != nil {
		return nil, &spawnFailure{Status: 503, Kind: "node_capacity_unknown", Msg: "cannot observe node capacity: " + err.Error()}
	} else if reason != "" {
		return nil, &spawnFailure{Status: 409, Kind: fedCodeNodeBusy, Msg: reason}
	}
	p.nodeCapacityReserved = true
	launchID := p.AgentID
	nodeAdmission.launches[launchID] = config.DataDir()
	return func() { nodeAdmission.Lock(); delete(nodeAdmission.launches, launchID); nodeAdmission.Unlock() }, nil
}

// capacityResources is what the free-RAM and free-disk thresholds read.
// A nil RAMAvailable means the reading failed; a disk entry with Error set
// likewise. Unknown readings fail closed: the launch waits or is refused.
type capacityResources struct {
	RAMAvailable *uint64
	RAMError     string
	Disks        []hostmetrics.Disk
}

// capacityProbe samples free RAM and free space on dirs at admission time,
// never from the 15-second host cache. Tests swap it.
var capacityProbe = func(dirs []string) capacityResources {
	paths := make([]hostmetrics.Path, 0, len(dirs))
	for _, d := range dirs {
		paths = append(paths, hostmetrics.Path{Kind: "capacity", Path: d})
	}
	s := hostmetrics.Read(paths)
	out := capacityResources{Disks: s.Disks}
	if s.RAM != nil {
		avail := s.RAM.AvailableBytes
		out.RAMAvailable = &avail
	} else {
		out.RAMError = "RAM reading unavailable"
		for _, e := range s.Errors {
			if strings.HasPrefix(e, "RAM: ") {
				out.RAMError = e
			}
		}
	}
	return out
}

// nodeCapacityFullLocked returns why a new launch does not fit right now, or
// "" when it does. exclude is the launch's own reservation. The caller holds
// nodeAdmission.
func nodeCapacityFullLocked(limits config.CapacityLimits, exclude string, launchDirs []string) (string, error) {
	if limits.MaxLiveAgents > 0 {
		used, err := nodeCapacityUsed(exclude)
		if err != nil {
			return "", err
		}
		if used >= limits.MaxLiveAgents {
			return fmt.Sprintf("node capacity reached: %d of %d live and reserved agents", used, limits.MaxLiveAgents), nil
		}
	}
	if limits.MinFreeRAMMB <= 0 && limits.MinFreeDiskMB <= 0 {
		return "", nil
	}
	dirs := []string{config.DataDir()}
	if limits.MinFreeDiskMB > 0 {
		for _, d := range launchDirs {
			if strings.TrimSpace(d) != "" {
				dirs = append(dirs, d)
			}
		}
	}
	res := capacityProbe(dirs)
	const mb = 1 << 20
	if limits.MinFreeRAMMB > 0 {
		if res.RAMAvailable == nil {
			return "free RAM unknown (" + res.RAMError + "); capacity.min_free_ram_mb cannot be checked", nil
		}
		if *res.RAMAvailable < uint64(limits.MinFreeRAMMB)*mb {
			return fmt.Sprintf("free RAM %d MB is below capacity.min_free_ram_mb %d", *res.RAMAvailable/mb, limits.MinFreeRAMMB), nil
		}
	}
	if limits.MinFreeDiskMB > 0 {
		for _, d := range res.Disks {
			if d.Error != "" {
				return fmt.Sprintf("free disk at %s unknown (%s); capacity.min_free_disk_mb cannot be checked", d.Path.Path, d.Error), nil
			}
			if d.AvailableBytes < uint64(limits.MinFreeDiskMB)*mb {
				return fmt.Sprintf("free disk at %s %d MB is below capacity.min_free_disk_mb %d", d.Path.Path, d.AvailableBytes/mb, limits.MinFreeDiskMB), nil
			}
		}
	}
	return "", nil
}

// nodeCapacityFull is nodeCapacityFullLocked for callers outside the gate.
func nodeCapacityFull(limits config.CapacityLimits, launchDirs []string) (string, error) {
	nodeAdmission.Lock()
	defer nodeAdmission.Unlock()
	return nodeCapacityFullLocked(limits, "", launchDirs)
}

var errNodeBusy = errors.New("node live and reserved agent capacity reached")

// insertFederationSpawnWithCapacity stores a peer's spawn request when the
// node has room for it. When it does not, the request is refused with
// errNodeBusy, or, with capacity.when_full "queue" and allowQueue, stored as
// queued with a place in the spawn queue: position is then that place, and
// zero for a request stored as pending.
func insertFederationSpawnWithCapacity(req *db.FederationSpawnRequest, allowQueue bool) (int64, int, error) {
	nodeAdmission.Lock()
	defer nodeAdmission.Unlock()
	prior, err := db.FederationSpawnRequestByEnvelope(req.FromInstance, req.EnvelopeID)
	if err != nil {
		return 0, 0, err
	}
	if prior != nil {
		return 0, 0, db.ErrFederationDuplicate
	}
	// A terminal busy receipt permits placement elsewhere. Persist that verdict
	// before acknowledging it so an in-flight resend cannot later create a
	// second worker when capacity becomes available (including after restart).
	busyKey := "spawnbusy:" + req.EnvelopeID
	if busy, err := db.FederationEnvelopeSeen(req.FromInstance, busyKey); err != nil {
		return 0, 0, err
	} else if busy {
		return 0, 0, errNodeBusy
	}
	limits, err := nodeCapacityLimits()
	if err != nil {
		return 0, 0, err
	}
	queued, err := db.CountActiveSpawnQueue()
	if err != nil {
		return 0, 0, err
	}
	reason := ""
	if queued > 0 {
		// One line for local and remote spawns: a request does not pass those already waiting.
		reason = fmt.Sprintf("%d spawn(s) already wait for capacity", queued)
	} else if limits.Limited() {
		if reason, err = nodeCapacityFullLocked(limits, "", []string{fedSpawnRequestDir(req)}); err != nil {
			return 0, 0, err
		}
	}
	if reason == "" {
		id, err := db.InsertFederationSpawnRequest(req, fedSpawnPendingLimit)
		return id, 0, err
	}
	if limits.Queue && allowQueue {
		entry := &db.SpawnQueueEntry{GroupID: req.GroupID, Name: req.Name, RequesterName: fedSpawnRequesterLabel(req)}
		id, queueID, position, err := db.InsertQueuedFederationSpawnRequest(req, fedSpawnPendingLimit, entry, limits.MaxQueued)
		if err == nil {
			slog.Info("remote spawn request queued for capacity", "request", id, "queue_id", queueID, "position", position,
				"group", req.GroupName, "from", req.FromInstance, "reason", reason)
			kickSpawnQueue()
			return id, position, nil
		}
		if !errors.Is(err, db.ErrSpawnQueueFull) {
			return 0, 0, err
		}
		reason += fmt.Sprintf(", and the spawn queue already holds capacity.max_queued (%d) entries", limits.MaxQueued)
	}
	if _, err := db.MarkFederationEnvelopeSeen(req.FromInstance, busyKey, req.ExpiresAt); err != nil {
		return 0, 0, err
	}
	return 0, 0, fmt.Errorf("%w: %s", errNodeBusy, reason)
}

// fedSpawnRequestDir is the directory a remote spawn request would launch
// in, for the free-disk threshold: its requested one, else its group's.
func fedSpawnRequestDir(req *db.FederationSpawnRequest) string {
	if req.Overrides.Cwd != "" {
		return req.Overrides.Cwd
	}
	if g, _ := db.GetAgentGroupByID(req.GroupID); g != nil {
		return g.DefaultCwd
	}
	return ""
}

// fedSpawnRequesterLabel names a remote requester in queue listings.
func fedSpawnRequesterLabel(req *db.FederationSpawnRequest) string {
	peer := req.FromInstance
	if p, _ := db.GetFederationPeer(req.FromInstance); p != nil {
		peer = peerDisplay(p)
	}
	return req.FromName + "@" + proto.SafeName(peer, true) + " (remote)"
}

// remoteJobCapacityFullLocked is why a remote job does not fit now, or "".
// The caller holds nodeAdmission.
func remoteJobCapacityFullLocked(g *db.AgentGroup) (string, error) {
	limits, err := nodeCapacityLimits()
	if err != nil {
		return "", err
	}
	queued, err := db.CountActiveSpawnQueue()
	if err != nil {
		return "", err
	}
	if queued > 0 {
		return fmt.Sprintf("%d spawn(s) already wait for capacity", queued), nil
	}
	if !limits.Limited() {
		return "", nil
	}
	dir := ""
	if g != nil {
		dir = g.DefaultCwd
	}
	return nodeCapacityFullLocked(limits, "", []string{dir})
}
