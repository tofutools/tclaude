package agentd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
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
		notifyFleetHuman(peer, event.Peer, kind, message)
	}
}

// notifyFleetHuman puts a fleet notice in the operator inbox without a watch event.
func notifyFleetHuman(peer, display, kind, message string) {
	group := db.FederationHumanGroup(peer)
	subject := "Fleet health: " + display + " " + kind
	if _, err := db.InsertHumanMessage(&db.HumanMessage{FromTitle: "Fleet health", GroupName: group, Subject: subject, Body: message}); err == nil {
		dispatchHumanMessageNotification("", "Fleet health", group, subject, message)
	}
}

// Offline notices escalate instead of firing on the debounced transition, so a
// short restart (such as an upgrade) never reaches the operator inbox. After
// the listed delays a reminder repeats daily until the node returns, its trust
// is removed, or the operator silences this disconnection.
var offlineNoticeDelays = []time.Duration{5 * time.Minute, 30 * time.Minute, 4 * time.Hour}

const offlineNoticeRepeat = 24 * time.Hour

// offlineNoticeDelay is how long after going offline notice n (0-based) is due.
func offlineNoticeDelay(n int) time.Duration {
	if n < len(offlineNoticeDelays) {
		return offlineNoticeDelays[n]
	}
	return offlineNoticeDelays[len(offlineNoticeDelays)-1] + time.Duration(n-len(offlineNoticeDelays)+1)*offlineNoticeRepeat
}

func offlineFor(d time.Duration) string {
	switch {
	case d < 2*time.Hour:
		return fmt.Sprintf("%d minutes", int(d.Round(time.Minute)/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%.1f hours", d.Hours())
	}
	return fmt.Sprintf("%d days", int(d/(24*time.Hour)))
}

// remindOffline sends the next due offline notice for an emitted offline
// episode. A delayed flush (suspend, busy daemon) sends one catch-up notice.
func remindOffline(peer string, signal *fleetSignal, now time.Time) {
	if signal.offlineSince.IsZero() {
		signal.offlineSince = signal.since
	}
	if signal.silenced || now.Before(signal.offlineSince.Add(offlineNoticeDelay(signal.notices))) {
		return
	}
	for signal.notices++; !now.Before(signal.offlineSince.Add(offlineNoticeDelay(signal.notices))); signal.notices++ {
	}
	p, err := db.GetFederationPeer(peer)
	if err != nil || p == nil {
		return
	}
	next := signal.offlineSince.Add(offlineNoticeDelay(signal.notices)).Sub(now)
	message := fmt.Sprintf("Node has been offline for %s (since %s). Next reminder in %s unless it reconnects. Silence this disconnection from the node's card in the Fleet map, or with `tclaude federation nodes health --peer <name> --silence`. If the node is often off on purpose, mark it part-time there (or with `--part-time`) to stop these notices for it.",
		offlineFor(now.Sub(signal.offlineSince)), signal.offlineSince.Local().Format(time.RFC1123), offlineFor(next))
	notifyFleetHuman(peer, proto.SafeName(peerDisplay(p), true), "offline", message)
}

// fleetOfflineNotice is one peer's current offline episode, for the Fleet map.
type fleetOfflineNotice struct {
	Since    time.Time  `json:"since"`
	Notices  int        `json:"notices"`
	NextAt   *time.Time `json:"next_at,omitempty"`
	Silenced bool       `json:"silenced"`
}

// offlineNotices reports the offline episodes that are emitted and still open.
func (rt *fedRuntime) offlineNotices() map[string]fleetOfflineNotice {
	rt.healthMu.Lock()
	defer rt.healthMu.Unlock()
	out := map[string]fleetOfflineNotice{}
	for key, signal := range rt.fleetState().signals {
		peer, ok := strings.CutSuffix(key, "/presence")
		if !ok || !signal.value || !signal.emitted {
			continue
		}
		since := signal.offlineSince
		if since.IsZero() {
			since = signal.since
		}
		n := fleetOfflineNotice{Since: since, Notices: signal.notices, Silenced: signal.silenced}
		if !signal.silenced {
			next := since.Add(offlineNoticeDelay(signal.notices))
			n.NextAt = &next
		}
		out[peer] = n
	}
	return out
}

// silenceOffline stops reminders for the peer's current offline episode; the
// next disconnection notifies again. It reports false when none is open.
func (rt *fedRuntime) silenceOffline(peer string) bool {
	rt.healthMu.Lock()
	defer rt.healthMu.Unlock()
	signal := rt.fleetState().signals[peer+"/presence"]
	if signal == nil || !signal.value || !signal.emitted {
		return false
	}
	signal.silenced = true
	return true
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
	// Presence only: the emitted offline episode's start, the operator
	// notices sent for it, and whether the operator silenced it.
	offlineSince time.Time
	notices      int
	silenced     bool
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
	p, err := configuredFleetPolicy(cfg, peer, fleetRotationParents)
	if err != nil || p.Validate() != nil {
		return config.FederationHealthPolicy{}, false
	}
	return effectiveFleetPolicy(p), true
}

// fleetRotationParents maps each accepted identity successor to its predecessor.
func fleetRotationParents() (map[string]string, error) {
	rows, err := db.ListFederationIdentityRotations()
	if err != nil {
		return nil, err
	}
	previous := map[string]string{}
	for _, row := range rows {
		if row.State == "accepted" || row.State == "recovered" {
			previous[row.Statement.NewID] = row.Statement.OldID
		}
	}
	return previous, nil
}

// configuredFleetPolicy is peer's policy as configured, before built-in
// defaults: its own override, the nearest accepted ancestor's, or the defaults.
// parents is only read when an ancestor walk is needed.
func configuredFleetPolicy(cfg *config.Config, peer string, parents func() (map[string]string, error)) (config.FederationHealthPolicy, error) {
	if cfg.Federation == nil || cfg.Federation.Health == nil {
		return config.FederationHealthPolicy{}, nil
	}
	h := cfg.Federation.Health
	if override, ok := h.Peers[peer]; ok {
		return override, nil
	}
	if peer == "" || len(h.Peers) == 0 {
		return h.Defaults, nil
	}
	// Walk accepted continuation links backwards so the closest explicit
	// ancestor override wins across repeated rotations. Labels never match.
	previous, err := parents()
	if err != nil {
		return config.FederationHealthPolicy{}, err
	}
	seen := map[string]bool{peer: true}
	for id := previous[peer]; id != ""; id = previous[id] {
		if seen[id] {
			return config.FederationHealthPolicy{}, fmt.Errorf("identity rotation cycle at %s", id)
		}
		seen[id] = true
		if override, ok := h.Peers[id]; ok {
			return override, nil
		}
	}
	return h.Defaults, nil
}

// fleetPartTime reports which trusted peers are part-time: their policy turns
// offline/back notices off, so their disconnections never reach Messages.
func fleetPartTime(peers []db.FederationPeer) map[string]bool {
	out := map[string]bool{}
	cfg, err := config.Load()
	if err != nil {
		return out
	}
	var parents map[string]string
	var parentsErr error
	loaded := false
	cached := func() (map[string]string, error) {
		if !loaded {
			parents, parentsErr = fleetRotationParents()
			loaded = true
		}
		return parents, parentsErr
	}
	for _, p := range peers {
		policy, err := configuredFleetPolicy(cfg, p.InstanceID, cached)
		if err == nil && policy.Presence != nil && !*policy.Presence {
			out[p.InstanceID] = true
		}
	}
	return out
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
	s.failures[peer] = rows
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
		if kind == "presence" && signal.value && signal.emitted {
			remindOffline(peer, signal, now)
		}
		if signal.value == signal.emitted || now.Sub(signal.since) < time.Duration(p.DebounceSeconds)*time.Second {
			continue
		}
		signal.emitted = signal.value
		eventKind := kind
		notify := true
		if kind == "presence" {
			// The transition itself is only a watch event: offline notices
			// follow the escalation schedule, and "back online" reaches the
			// inbox only when an unsilenced offline notice went out.
			eventKind = "offline"
			notify = false
			if !signal.value {
				eventKind = "online"
				notify = signal.notices > 0 && !signal.silenced
			}
			signal.offlineSince, signal.notices, signal.silenced = time.Time{}, 0, false
			if signal.value {
				signal.offlineSince = signal.since
			}
		} else if !signal.value {
			eventKind += "_recovered"
		}
		publishFleetEvent(peer, eventKind, signal.message, notify)
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

// handleFleetOfflineSilence silences reminders for one peer's current
// disconnection. A later disconnection notifies again.
func handleFleetOfflineSilence(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "silence fleet offline notices") {
		return
	}
	p, err := resolveFederationPeer(r.URL.Query().Get("peer"))
	if err != nil {
		writeFedErr(w, err)
		return
	}
	rt := currentFederation()
	if rt == nil || !rt.silenceOffline(p.InstanceID) {
		writeError(w, http.StatusConflict, "not_offline", "no offline notice is pending for this peer")
		return
	}
	writeJSON(w, 200, rt.offlineNotices()[p.InstanceID])
}

// handleFleetPresenceNotices turns one peer's offline/back notices on or off
// (off marks it part-time) and keeps the rest of its policy. A peer without
// its own override gets one copied from the policy it follows today.
func handleFleetPresenceNotices(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "configure fleet health") {
		return
	}
	p, err := resolveFederationPeer(r.URL.Query().Get("peer"))
	if err != nil {
		writeFedErr(w, err)
		return
	}
	var body struct {
		Presence *bool `json:"presence"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil || body.Presence == nil {
		writeError(w, 400, "invalid_arg", `body must be {"presence": true|false}`)
		return
	}
	_, err = config.Update(func(c *config.Config, e error) error {
		if e != nil {
			return e
		}
		policy, err := configuredFleetPolicy(c, p.InstanceID, fleetRotationParents)
		if err != nil {
			return err
		}
		policy.Presence = body.Presence
		if c.Federation == nil {
			c.Federation = &config.FederationConfig{}
		}
		if c.Federation.Health == nil {
			c.Federation.Health = &config.FederationHealthConfig{}
		}
		if c.Federation.Health.Peers == nil {
			c.Federation.Health.Peers = map[string]config.FederationHealthPolicy{}
		}
		c.Federation.Health.Peers[p.InstanceID] = policy
		return nil
	})
	if err != nil {
		writeError(w, 500, "config", err.Error())
		return
	}
	policy, ok := fleetPolicy(p.InstanceID)
	if !ok {
		writeError(w, 500, "config", "cannot read fleet health policy")
		return
	}
	writeJSON(w, 200, policy)
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
