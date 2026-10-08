package agentd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

type fleetEvent struct {
	At       time.Time `json:"at"`
	Instance string    `json:"instance"`
	Peer     string    `json:"peer"`
	Kind     string    `json:"kind"`
	Message  string    `json:"message"`
}

// This bounded, daemon-lifetime bus survives federation runtime reloads (including
// identity rotation). A slow watcher is disconnected instead of blocking updates.
var fleetBus = struct {
	sync.Mutex
	root     string
	watchers map[chan fleetEvent]bool
}{watchers: make(map[chan fleetEvent]bool)}

func publishFleetEvent(peer, kind, message string, notify bool) {
	p, err := db.GetFederationPeer(peer)
	if err != nil || p == nil {
		return
	}
	event := fleetEvent{time.Now().UTC(), peer, proto.SafeName(peerDisplay(p), true), kind, message}
	fleetBus.Lock()
	if fleetBus.root != config.DataDir() {
		for ch := range fleetBus.watchers {
			close(ch)
		}
		fleetBus.watchers = make(map[chan fleetEvent]bool)
		fleetBus.root = config.DataDir()
	}
	for ch := range fleetBus.watchers {
		select {
		case ch <- event:
		default:
			close(ch)
			delete(fleetBus.watchers, ch)
		}
	}
	fleetBus.Unlock()
	if notify {
		group := db.FederationHumanGroup(peer)
		subject := "Fleet health: " + event.Peer + " " + kind
		if _, err := db.InsertHumanMessage(&db.HumanMessage{FromTitle: "Fleet health", GroupName: group, Subject: subject, Body: message}); err == nil {
			dispatchHumanMessageNotification("", "Fleet health", group, subject, message)
		}
	}
}

func handleFleetWatch(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "watch fleet health") {
		return
	}
	controller := http.NewResponseController(w)
	ch := make(chan fleetEvent, 64)
	fleetBus.Lock()
	if len(fleetBus.watchers) >= 64 {
		fleetBus.Unlock()
		writeError(w, 429, "watch_limit", "too many fleet watchers")
		return
	}
	if fleetBus.root != config.DataDir() {
		for old := range fleetBus.watchers {
			close(old)
		}
		fleetBus.watchers = make(map[chan fleetEvent]bool)
		fleetBus.root = config.DataDir()
	}
	fleetBus.watchers[ch] = true
	fleetBus.Unlock()
	defer func() { fleetBus.Lock(); delete(fleetBus.watchers, ch); fleetBus.Unlock() }()
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(200)
	if controller.Flush() != nil {
		return
	}
	enc := json.NewEncoder(w)
	for {
		select {
		case <-r.Context().Done():
			return
		case event, ok := <-ch:
			if !ok {
				return
			}
			// Trust can be revoked between publication and delivery.
			if p, err := db.GetFederationPeer(event.Instance); err != nil || p == nil {
				continue
			}
			_ = controller.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if enc.Encode(event) != nil {
				return
			}
			if controller.Flush() != nil {
				return
			}
		}
	}
}

type fleetHealthState struct {
	presence          map[string]bool
	signals           map[string]*fleetSignal
	memory            map[string]time.Time
	observed          map[string]time.Time
	disks             map[string]uint8
	failures          map[string][]fleetFailure
	lastFailureNotice map[string]time.Time
}
type fleetSignal struct {
	value, emitted bool
	since          time.Time
	message        string
}
type fleetFailure struct {
	id string
	at time.Time
}

func (rt *fedRuntime) fleetState() *fleetHealthState {
	if rt.health == nil {
		rt.health = &fleetHealthState{disks: map[string]uint8{}, signals: map[string]*fleetSignal{}, memory: map[string]time.Time{}, observed: map[string]time.Time{}, failures: map[string][]fleetFailure{}, lastFailureNotice: map[string]time.Time{}}
	}
	return rt.health
}
func effectiveFleetPolicy(p config.FederationHealthPolicy) config.FederationHealthPolicy {
	if p.Presence == nil {
		enabled := true
		p.Presence = &enabled
	}
	if p.DebounceSeconds == 0 {
		p.DebounceSeconds = 15
	}
	if p.DiskFreePercent == 0 {
		p.DiskFreePercent = 10
	}
	if p.RAMFreePercent == 0 {
		p.RAMFreePercent = 10
	}
	if p.MemorySeconds == 0 {
		p.MemorySeconds = 120
	}
	if p.FailureCount == 0 {
		p.FailureCount = 3
	}
	if p.FailureWindowSeconds == 0 {
		p.FailureWindowSeconds = 600
	}
	if p.CooldownSeconds == 0 {
		p.CooldownSeconds = 600
	}
	return p
}
func fleetPolicy(peer string) (config.FederationHealthPolicy, bool) {
	cfg, err := config.Load()
	if err != nil {
		return config.FederationHealthPolicy{}, false
	}
	p := config.FederationHealthPolicy{}
	if cfg.Federation != nil && cfg.Federation.Health != nil {
		h := cfg.Federation.Health
		p = h.Defaults
		if override, ok := h.Peers[peer]; ok {
			p = override
		} else {
			// Explicit continuation through accepted links only; no label matching.
			keys := make([]string, 0, len(h.Peers))
			for id := range h.Peers {
				keys = append(keys, id)
			}
			sort.Strings(keys)
			for _, id := range keys {
				if current, e := db.ResolveFederationIdentitySuccessor(id); e == nil && current == peer {
					p = h.Peers[id]
					break
				}
			}
		}
	}
	if p.Validate() != nil {
		return config.FederationHealthPolicy{}, false
	}
	return effectiveFleetPolicy(p), true
}
func (s *fleetHealthState) signal(peer, kind, message string, value bool, now time.Time) {
	key := peer + "/" + kind
	old := s.signals[key]
	if old == nil {
		old = &fleetSignal{}
		s.signals[key] = old
	}
	if old.value != value {
		old.value = value
		old.since = now
		old.message = message
	}
}

func (rt *fedRuntime) observeFleetPresence(online map[string]bool, now time.Time) {
	// The hub read callback must not gather snapshots or perform DB work.
	rt.healthMu.Lock()
	defer rt.healthMu.Unlock()
	s := rt.fleetState()
	if s.presence != nil {
		all := map[string]bool{}
		for id := range s.presence {
			all[id] = true
		}
		for id := range online {
			all[id] = true
		}
		for id := range all {
			if s.presence[id] != online[id] {
				message := "Node is offline"
				if online[id] {
					message = "Node is back online"
				}
				// The signal's true state represents offline; start from the old baseline.
				key := id + "/presence"
				if s.signals[key] == nil {
					s.signals[key] = &fleetSignal{value: !s.presence[id], emitted: !s.presence[id]}
				}
				s.signal(id, "presence", message, !online[id], now)
				if !online[id] {
					s.unknownResources(id)
				}
			}
		}
	}
	s.presence = online
}
func (rt *fedRuntime) observeFleetNode(peer string, cat *proto.CatalogPayload, now time.Time) {
	p, ok := fleetPolicy(peer)
	if !ok || !p.Resources {
		return
	}
	rt.healthMu.Lock()
	defer rt.healthMu.Unlock()
	s := rt.fleetState()
	if cat == nil || cat.Node == nil {
		s.unknownResources(peer)
		return
	}
	r := cat.Node.Resources
	if r.Status != "current" || r.ObservedAt == nil || now.Sub(*r.ObservedAt) > fedNodeStaleAfter || r.ObservedAt.After(now.Add(2*time.Minute)) {
		s.unknownResources(peer)
		return
	}
	if !r.ObservedAt.After(s.observed[peer]) {
		return
	}
	if old := s.observed[peer]; !old.IsZero() && r.ObservedAt.Sub(old) > fedNodeStaleAfter {
		s.unknownResources(peer)
	}
	s.observed[peer] = *r.ObservedAt
	disk := 101.0
	var disks uint8
	if r.DataDisk != nil && r.DataDisk.TotalBytes > 0 {
		disks |= 1
		disk = 100 * float64(r.DataDisk.AvailableBytes) / float64(r.DataDisk.TotalBytes)
	}
	if r.WorkDiskMinAvailablePercent != nil {
		disks |= 2
		disk = min(disk, *r.WorkDiskMinAvailablePercent)
	}
	s.disks[peer] |= disks
	if disk > 100 {
		s.cancelPending(peer, "disk")
	}
	if disk <= 100 {
		low := disk < p.DiskFreePercent
		msg := fmt.Sprintf("Disk free %.1f%% (threshold %.1f%%)", disk, p.DiskFreePercent)
		if low || disks == s.disks[peer] {
			s.signal(peer, "disk", msg, low, now)
		} else {
			s.cancelPending(peer, "disk")
		}
	}
	if r.RAM == nil || r.RAM.TotalBytes == 0 {
		delete(s.memory, peer)
		s.cancelPending(peer, "memory")
		return
	}
	free := 100 * float64(r.RAM.AvailableBytes) / float64(r.RAM.TotalBytes)
	if free < p.RAMFreePercent {
		// A return to low RAM immediately cancels an unannounced recovery.
		if signal := s.signals[peer+"/memory"]; signal != nil && !signal.value && signal.emitted {
			signal.value = true
		}
		since := s.memory[peer]
		if since.IsZero() {
			since = *r.ObservedAt
			s.memory[peer] = since
		}
		if r.ObservedAt.Sub(since) >= time.Duration(p.MemorySeconds)*time.Second {
			s.signal(peer, "memory", fmt.Sprintf("Sustained high memory: %.1f%% RAM free", free), true, now)
		}
	} else {
		delete(s.memory, peer)
		s.signal(peer, "memory", fmt.Sprintf("Memory recovered: %.1f%% RAM free", free), false, now)
	}
}
func (rt *fedRuntime) observeFleetFailure(peer, id string, now time.Time) {
	p, ok := fleetPolicy(peer)
	if !ok || !p.Failures {
		return
	}
	rt.healthMu.Lock()
	defer rt.healthMu.Unlock()
	s := rt.fleetState()
	rows := s.failures[peer][:0]
	duplicate := false
	for _, f := range s.failures[peer] {
		if now.Sub(f.at) <= time.Duration(p.FailureWindowSeconds)*time.Second {
			rows = append(rows, f)
			duplicate = duplicate || f.id == id
		}
	}
	if duplicate {
		return
	}
	rows = append(rows, fleetFailure{id, now})
	if len(rows) > 256 {
		rows = rows[len(rows)-256:]
	}
	s.failures[peer] = rows
	if len(rows) >= p.FailureCount && now.Sub(s.lastFailureNotice[peer]) >= time.Duration(p.CooldownSeconds)*time.Second {
		s.lastFailureNotice[peer] = now
		s.signal(peer, "failures", fmt.Sprintf("%d remote job/spawn failures in %ds", len(rows), p.FailureWindowSeconds), true, now)
	}
}
func (rt *fedRuntime) flushFleetHealth(now time.Time) {
	rt.healthMu.Lock()
	defer rt.healthMu.Unlock()
	s := rt.fleetState()
	for key, signal := range s.signals {
		var peer, kind string
		for i, c := range key {
			if c == '/' {
				peer, kind = key[:i], key[i+1:]
				break
			}
		}
		p, ok := fleetPolicy(peer)
		trusted, err := db.GetFederationPeer(peer)
		if !ok || err != nil || trusted == nil {
			delete(s.signals, key)
			continue
		}
		enabled := kind == "presence" && (p.Presence == nil || *p.Presence) || (kind == "disk" || kind == "memory") && p.Resources || kind == "failures" && p.Failures
		if !enabled {
			delete(s.signals, key)
			continue
		}
		if (kind == "disk" || kind == "memory") && (!rt.isOnline(peer) || now.Sub(s.observed[peer]) > fedNodeStaleAfter) {
			s.unknownResources(peer)
			continue
		}
		if signal.value == signal.emitted || now.Sub(signal.since) < time.Duration(p.DebounceSeconds)*time.Second {
			continue
		}
		signal.emitted = signal.value
		eventKind := kind
		if kind == "presence" {
			eventKind = "offline"
			if !signal.value {
				eventKind = "online"
			}
		} else if !signal.value {
			eventKind += "_recovered"
		}
		publishFleetEvent(peer, eventKind, signal.message, true)
		if kind == "failures" {
			delete(s.signals, key)
		}
	}
}

func handleFleetHealthConfig(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "configure fleet health") {
		return
	}
	peer := r.URL.Query().Get("peer")
	if peer != "" {
		p, err := resolveFederationPeer(peer)
		if err != nil {
			writeFedErr(w, err)
			return
		}
		peer = p.InstanceID
	}
	if r.Method == http.MethodPost {
		var p config.FederationHealthPolicy
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&p); err != nil {
			writeError(w, 400, "invalid_arg", err.Error())
			return
		}
		if err := p.Validate(); err != nil {
			writeError(w, 400, "invalid_arg", err.Error())
			return
		}
		_, err := config.Update(func(c *config.Config, e error) error {
			if e != nil {
				return e
			}
			if c.Federation == nil {
				c.Federation = &config.FederationConfig{}
			}
			if c.Federation.Health == nil {
				c.Federation.Health = &config.FederationHealthConfig{}
			}
			h := c.Federation.Health
			if peer == "" {
				h.Defaults = p
			} else {
				if h.Peers == nil {
					h.Peers = map[string]config.FederationHealthPolicy{}
				}
				h.Peers[peer] = p
			}
			return nil
		})
		if err != nil {
			writeError(w, 500, "config", err.Error())
			return
		}
	}
	p, ok := fleetPolicy(peer)
	if !ok {
		writeError(w, 500, "config", "cannot read fleet health policy")
		return
	}
	writeJSON(w, 200, p)
}

// Optional progress frame: older peers ignore it. It never settles a spawn
// request, and contains no remote error text or agent task details.
type spawnAttemptFailure struct {
	Request string `json:"request"`
	Attempt string `json:"attempt"`
}

func (rt *fedRuntime) acceptSpawnAttemptFailure(peer *db.FederationPeer, env *proto.Envelope) {
	var f spawnAttemptFailure
	if env.From.Agent != "" || env.DecodePayload(&f) != nil || !proto.ValidAgentRef(f.Attempt) || len(f.Request) > 128 {
		return
	}
	row, err := db.GetFederationOutbox(f.Request)
	if err != nil || row == nil || row.Kind != proto.KindSpawnReq || row.ToInstance != peer.InstanceID {
		return
	}
	if !rt.allowInbound(peer.InstanceID) {
		return
	}
	if fresh, err := db.MarkFederationEnvelopeSeen(peer.InstanceID, "spawnfail:"+f.Request+":"+f.Attempt, time.Now().Add(fedMailTTL)); err == nil && fresh {
		rt.observeFleetFailure(peer.InstanceID, "spawn/"+f.Request+"/"+f.Attempt, time.Now())
	}
}

func (s *fleetHealthState) cancelPending(peer, kind string) {
	if signal := s.signals[peer+"/"+kind]; signal != nil {
		signal.value = signal.emitted
	}
}
func (s *fleetHealthState) unknownResources(peer string) {
	delete(s.memory, peer)
	s.cancelPending(peer, "disk")
	s.cancelPending(peer, "memory")
}
