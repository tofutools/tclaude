package agentd

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/federation/bundletransfer"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/paf"
	"github.com/tofutools/tclaude/pkg/paf/privacy"
	"github.com/tofutools/tclaude/pkg/paf/seal"
)

// Resilience snapshots: opt-in, periodic, encrypted snapshots of agents
// sent off-node, so an agent can be recovered if its host is lost.
//
// A policy names agents (one, or a group's members), a cadence and a
// destination. The scheduler takes a snapshot only while an agent is idle,
// sends nothing when the conversation has not changed, seals it to the
// destination and this node, and offers it to the peer as a "snapshot"
// bundle. The peer keeps it opaquely (see resilienceCommit) under its
// snapshots.receive grant and never imports it.
//
// Policies and per-agent state live in a JSON file in the data directory,
// not in the database: they are operator configuration plus a small status
// record, and a file keeps this feature free of schema migrations.

const (
	resilienceMinEvery    = 10 * time.Minute
	resilienceTick        = time.Minute
	resilienceOfferTTL    = 24 * time.Hour
	resilienceMaxBackoff  = 6 * time.Hour
	resilienceNotifyAfter = 3
	// resilienceDefaultMaxAgeDays is how long a receiver keeps a snapshot
	// when the policy names no age.
	resilienceDefaultMaxAgeDays = 30
	// resilienceHeldPeerBytes bounds what this node keeps for one peer.
	resilienceHeldPeerBytes int64 = 2 << 30
	// resilienceHeldPeerAgents bounds how many source agents one peer may
	// have snapshots kept for, so tiny snapshots cannot pile up files.
	resilienceHeldPeerAgents = 200
)

// resilienceReceiving keeps one custody retry pass at a time, off the
// scheduler goroutine: a slow sender must not stall our own snapshots.
var resilienceReceiving atomic.Bool

type resilienceDest struct {
	// Kind is "peer" or "board".
	Kind string `json:"kind"`
	Peer string `json:"peer,omitempty"`
	// Board and Recipients name a board and the members a snapshot is
	// sealed to; this node is always added.
	Board      string   `json:"board,omitempty"`
	Recipients []string `json:"recipients,omitempty"`
}

func (d resilienceDest) String() string {
	if d.Kind == "board" {
		return "board:" + d.Board
	}
	return "peer:" + d.Peer
}

// resilienceBoardItem is a snapshot a policy published on a board.
type resilienceBoardItem struct {
	Item      string    `json:"item"`
	Version   string    `json:"version"`
	At        time.Time `json:"at"`
	ExpiresAt time.Time `json:"expires_at"`
}

type resiliencePolicy struct {
	ID           string         `json:"id"`
	Target       string         `json:"target"`
	EverySeconds int64          `json:"every_seconds"`
	Dest         resilienceDest `json:"dest"`
	Keep         int            `json:"keep"`
	MaxAgeDays   int            `json:"max_age_days"`
	CreatedAt    time.Time      `json:"created_at"`
}

func (p resiliencePolicy) every() time.Duration { return time.Duration(p.EverySeconds) * time.Second }

// resilienceState is one agent's record under one policy.
type resilienceState struct {
	Agent         string    `json:"agent"`
	LastAttempt   time.Time `json:"last_attempt,omitempty"`
	LastGood      time.Time `json:"last_good,omitempty"`
	LastDigest    string    `json:"last_digest,omitempty"`
	LastBytes     int64     `json:"last_bytes,omitempty"`
	Pending       string    `json:"pending_offer,omitempty"`
	PendingDigest string    `json:"pending_digest,omitempty"`
	PendingBytes  int64     `json:"pending_bytes,omitempty"`
	PendingAt     time.Time `json:"pending_at,omitempty"`
	NextDue       time.Time `json:"next_due,omitempty"`
	Deferred      string    `json:"deferred,omitempty"`
	LastError     string    `json:"last_error,omitempty"`
	Failures      int       `json:"failures,omitempty"`
	Notified      bool      `json:"failure_notified,omitempty"`
	Findings      int       `json:"findings,omitempty"`
	// BoardItems are this agent's snapshots on the policy's board, newest
	// last; those beyond the keep count are deleted from the board.
	BoardItems []resilienceBoardItem `json:"board_items,omitempty"`
}

type resilienceFile struct {
	Policies []resiliencePolicy `json:"policies"`
	// State is keyed by policy id, then agent id.
	State map[string]map[string]*resilienceState `json:"state"`
	// FindingsNotified records the policies whose credential findings the
	// operator was told about.
	FindingsNotified map[string]bool `json:"findings_notified,omitempty"`
}

var resilienceMu sync.Mutex

func resiliencePath() string {
	return filepath.Join(config.DataDir(), "snapshots", "resilience.json")
}

func loadResilience() (*resilienceFile, error) {
	f := &resilienceFile{State: map[string]map[string]*resilienceState{}, FindingsNotified: map[string]bool{}}
	raw, err := os.ReadFile(resiliencePath())
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(raw, f); err != nil {
		return nil, fmt.Errorf("read %s: %w", resiliencePath(), err)
	}
	if f.State == nil {
		f.State = map[string]map[string]*resilienceState{}
	}
	if f.FindingsNotified == nil {
		f.FindingsNotified = map[string]bool{}
	}
	return f, nil
}

func saveResilience(f *resilienceFile) error {
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	path := resiliencePath()
	if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return writeSpoolFile(path, raw)
}

func (f *resilienceFile) state(policy, agentID string) *resilienceState {
	m := f.State[policy]
	if m == nil {
		m = map[string]*resilienceState{}
		f.State[policy] = m
	}
	s := m[agentID]
	if s == nil {
		s = &resilienceState{Agent: agentID}
		m[agentID] = s
	}
	return s
}

func newResiliencePolicyID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "rp_" + hex.EncodeToString(b)
}

// resilienceTargets resolves a policy target to active agent ids.
func resilienceTargets(target string) ([]string, error) {
	if group, ok := strings.CutPrefix(target, "group:"); ok {
		g, err := db.GetAgentGroupByName(group)
		if err != nil || g == nil {
			return nil, fmt.Errorf("group %q not found", group)
		}
		members, err := db.ListAgentGroupMembers(g.ID)
		if err != nil {
			return nil, err
		}
		var out []string
		for _, m := range members {
			if id, err := db.AgentIDForConv(m.ConvID); err == nil && id != "" && !slices.Contains(out, id) {
				out = append(out, id)
			}
		}
		return out, nil
	}
	a, err := db.GetAgent(target)
	if err != nil || !a.Active() {
		return nil, fmt.Errorf("agent %q not found or retired", target)
	}
	return []string{a.AgentID}, nil
}

// startResilienceScheduler runs due policies in the background.
func startResilienceScheduler(stop <-chan struct{}) {
	go func() {
		ticker := time.NewTicker(resilienceTick)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
			}
			resilienceTickOnce(time.Now(), "")
			if resilienceReceiving.CompareAndSwap(false, true) {
				go func() {
					defer resilienceReceiving.Store(false)
					resilienceReceiveRetry()
				}()
			}
		}
	}()
}

// resilienceTickOnce runs every due agent of every policy, or of one
// policy now when only is set.
func resilienceTickOnce(now time.Time, only string) {
	resilienceMu.Lock()
	defer resilienceMu.Unlock()
	f, err := loadResilience()
	if err != nil {
		slog.Warn("resilience: load", "error", err)
		return
	}
	for _, p := range f.Policies {
		if only != "" && p.ID != only {
			continue
		}
		agents, err := resilienceTargets(p.Target)
		if err != nil {
			slog.Warn("resilience: policy target", "policy", p.ID, "error", err)
			continue
		}
		for _, agentID := range agents {
			s := f.state(p.ID, agentID)
			resilienceSettle(p, s, now)
			if s.Pending != "" || (only == "" && now.Before(s.NextDue)) {
				continue
			}
			resilienceRun(f, p, s, now)
		}
	}
	if err := saveResilience(f); err != nil {
		slog.Warn("resilience: save", "error", err)
	}
}

// resilienceSettle reads the outcome of the agent's offer in flight.
func resilienceSettle(p resiliencePolicy, s *resilienceState, now time.Time) {
	if s.Pending == "" {
		return
	}
	o, err := db.GetFederationBundleOffer("out", p.Dest.Peer, s.Pending)
	if err != nil {
		return
	}
	switch {
	case o == nil:
		resilienceFail(p, s, now, "the offer record was lost")
	case o.State == "applied":
		s.LastGood, s.LastDigest, s.LastBytes = s.PendingAt, s.PendingDigest, s.PendingBytes
		s.Failures, s.Notified, s.LastError = 0, false, ""
	case o.State == "declined" || o.State == "expired":
		detail := o.LastError
		if detail == "" {
			detail = "the peer " + o.State + " the snapshot"
		}
		resilienceFail(p, s, now, detail)
	default:
		return
	}
	s.Pending, s.PendingDigest, s.PendingBytes = "", "", 0
}

// resilienceFail records a failure and backs off, boundedly. The operator
// is told once when failures repeat, not on every tick.
func resilienceFail(p resiliencePolicy, s *resilienceState, now time.Time, msg string) {
	s.Failures++
	s.LastError = msg
	backoff := p.every()
	for i := 1; i < s.Failures && backoff < resilienceMaxBackoff; i++ {
		backoff *= 2
	}
	s.NextDue = now.Add(min(backoff, resilienceMaxBackoff))
	if s.Failures >= resilienceNotifyAfter && !s.Notified {
		s.Notified = true
		body := fmt.Sprintf("Resilience snapshots of agent %s (policy %s, to %s) have failed %d times in a row. Last error: %s\n\nSee tclaude agent snapshot policy ls. You will not be told again until a snapshot succeeds.", s.Agent, p.ID, p.Dest, s.Failures, msg)
		if _, err := recordHumanMessage("", "Resilience snapshots failing", body); err != nil {
			slog.Warn("resilience: notify", "error", err)
		}
	}
}

// resilienceRun takes and sends one snapshot of an agent.
func resilienceRun(f *resilienceFile, p resiliencePolicy, s *resilienceState, now time.Time) {
	s.LastAttempt, s.Deferred = now, ""
	a, err := db.GetAgent(s.Agent)
	if err != nil || !a.Active() || a.CurrentConvID == "" {
		resilienceFail(p, s, now, "agent not found or retired")
		return
	}
	// Only at idle points: a busy agent is tried again on the next tick.
	if row, err := db.FindSessionByConvID(a.CurrentConvID); err == nil && row != nil && row.Status != session.StatusIdle && row.Status != session.StatusExited {
		s.Deferred = "busy"
		return
	}
	var peer *db.FederationPeer
	destLabel := p.Dest.String()
	if p.Dest.Kind == "peer" {
		if peer, err = db.GetFederationPeer(p.Dest.Peer); err != nil || peer == nil {
			resilienceFail(p, s, now, "peer "+p.Dest.Peer+" is not a trusted federation peer")
			return
		}
		destLabel = peerDisplay(peer)
		var catalog proto.CatalogPayload
		if raw, _, err := db.GetFederationCatalog(peer.InstanceID); err == nil {
			_ = json.Unmarshal([]byte(raw), &catalog)
		}
		if catalog.ResilienceSnapshots < 1 {
			resilienceFail(p, s, now, "peer "+destLabel+" does not accept resilience snapshots (it needs a newer tclaude)")
			return
		}
	}
	snap, err := captureResilienceSnapshot(a.CurrentConvID, now)
	if err != nil {
		resilienceFail(p, s, now, "capture: "+err.Error())
		return
	}
	defer snap.close()
	if snap.size > bundletransfer.Snapshot.MaxBytes {
		resilienceFail(p, s, now, fmt.Sprintf("the snapshot is %d bytes; at most %d are sent", snap.size, bundletransfer.Snapshot.MaxBytes))
		return
	}
	s.Findings = snap.findings
	if snap.findings > 0 && !f.FindingsNotified[p.ID] {
		f.FindingsNotified[p.ID] = true
		what := "is sent, encrypted, to " + destLabel + " as the policy says. Remove them from the conversation, or remove the policy, if that peer should not hold them."
		if p.Dest.Kind == "board" {
			what = "is not put on " + destLabel + ": a board never takes a snapshot with suspected credentials. Remove them from the conversation for snapshots to resume."
		}
		body := fmt.Sprintf("A resilience snapshot of agent %s (policy %s) holds %d suspected credential(s) and %s\n\nThis is the only notice for this policy; tclaude agent snapshot policy ls keeps showing the count.", s.Agent, p.ID, snap.findings, what)
		if _, err := recordHumanMessage("", "Resilience snapshot holds suspected credentials", body); err != nil {
			slog.Warn("resilience: notify", "error", err)
		}
	}
	if snap.digest == s.LastDigest {
		// Nothing changed since the last good snapshot.
		s.NextDue = now.Add(p.every())
		return
	}
	if p.Dest.Kind == "board" {
		resilienceToBoard(p, s, snap, now)
		return
	}
	self, err := federationIdentity()
	if err != nil {
		resilienceFail(p, s, now, "this node has no federation identity")
		return
	}
	sealed, err := sealResilienceSnapshot(snap.file, ed25519.PublicKey(peer.PubKey), self.Pub)
	if err != nil {
		resilienceFail(p, s, now, "encrypt: "+err.Error())
		return
	}
	if int64(len(sealed)) > bundletransfer.Snapshot.MaxBytes {
		resilienceFail(p, s, now, fmt.Sprintf("the encrypted snapshot is %d bytes; at most %d are sent", len(sealed), bundletransfer.Snapshot.MaxBytes))
		return
	}
	maxAge := p.MaxAgeDays
	if maxAge == 0 {
		maxAge = resilienceDefaultMaxAgeDays
	}
	d := bundletransfer.New(bundletransfer.Snapshot, sealed, "Resilience snapshot of "+s.Agent, now.Add(resilienceOfferTTL))
	d.Resilience = &bundletransfer.ResilienceIntent{SourceAgent: s.Agent, AgentName: snap.name, Harness: snap.harness, TakenAt: now.UTC().Truncate(time.Second), Keep: p.Keep, MaxAgeDays: maxAge}
	if err := offerResilienceSnapshot(peer, d, sealed); err != nil {
		resilienceFail(p, s, now, "send: "+err.Error())
		return
	}
	s.Pending, s.PendingDigest, s.PendingBytes, s.PendingAt = d.ID, snap.digest, d.Bytes, now
	s.LastError = ""
	s.NextDue = now.Add(p.every())
}

// resilienceToBoard publishes a snapshot on the policy's board, sealed to
// the named members and this node, then deletes this agent's snapshots
// beyond the keep count. A board never takes suspected credentials, and
// every board snapshot expires: after the policy's maximum age, at most
// the board limit.
func resilienceToBoard(p resiliencePolicy, s *resilienceState, snap *resilienceCapture, now time.Time) {
	if snap.findings > 0 {
		resilienceFail(p, s, now, fmt.Sprintf("%d suspected credential(s); a board never takes them", snap.findings))
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	a, err := newBoardItemAccess(ctx)
	if err != nil {
		resilienceFail(p, s, now, "board: "+err.Error())
		return
	}
	v, err := resilienceBoardPublish(a, p, snap, now)
	if err != nil {
		var be *boardHTTPError
		if errors.As(err, &be) {
			err = errors.New(be.Message)
		}
		resilienceFail(p, s, now, "board: "+err.Error())
		return
	}
	s.BoardItems = append(s.BoardItems, resilienceBoardItem{Item: v.Item, Version: v.Version, At: now.UTC().Truncate(time.Second), ExpiresAt: v.ExpiresAt})
	s.LastGood, s.LastDigest, s.LastBytes = now, snap.digest, v.PayloadBytes
	s.Failures, s.Notified, s.LastError = 0, false, ""
	s.NextDue = now.Add(p.every())
	// Expired items are gone from the board; delete the rest beyond keep.
	// A failed delete stays listed and is tried again next time.
	var kept []resilienceBoardItem
	for i, it := range s.BoardItems {
		if !now.Before(it.ExpiresAt) {
			continue
		}
		if len(s.BoardItems)-i > p.Keep {
			if _, err := a.call("items.delete", map[string]any{"board": p.Dest.Board, "item": it.Item, "version_id": it.Version}); err == nil {
				recordFederationAudit("resilience.board.delete", "", "", p.Dest.Board, "item="+it.Item+" version="+it.Version, 200)
				continue
			} else {
				slog.Warn("resilience: delete old board snapshot", "board", p.Dest.Board, "item", it.Item, "error", err)
			}
		}
		kept = append(kept, it)
	}
	s.BoardItems = kept
}

func resilienceBoardPublish(a *boardItemAccess, p resiliencePolicy, snap *resilienceCapture, now time.Time) (proto.BoardSnapshotVersion, error) {
	var v proto.BoardSnapshotVersion
	if err := a.snapshotCapable(); err != nil {
		return v, err
	}
	members, err := a.boardMembers(p.Dest.Board)
	if err != nil {
		return v, err
	}
	peers, err := db.ListFederationPeers()
	if err != nil {
		return v, err
	}
	instances, recipients, err := boardSnapshotRecipients(a.opts.Identity, members, p.Dest.Recipients, peers)
	if err != nil {
		return v, err
	}
	if len(instances) < 2 {
		return v, errors.New("name a recipient other than this node: a copy only this node can open is lost with it")
	}
	if _, err = snap.file.Seek(0, io.SeekStart); err != nil {
		return v, err
	}
	arch, err := paf.Open(snap.file, snap.size, paf.Options{Purpose: paf.PurposeInspect})
	if err != nil {
		return v, err
	}
	// The Board rule, on exactly what is uploaded.
	if len(arch.Manifest.Agents) != 1 {
		return v, errors.New("a board snapshot holds exactly one agent")
	}
	if findings, err := privacy.ScanArchive(arch, pafContextRecordLimit()); err != nil {
		return v, err
	} else if len(findings) > 0 {
		return v, fmt.Errorf("%d suspected credential(s); a board never takes them", len(findings))
	}
	maxAge := time.Duration(p.MaxAgeDays) * 24 * time.Hour
	if maxAge == 0 || maxAge > proto.MaxBoardSnapshotTTL {
		maxAge = proto.MaxBoardSnapshotTTL
	}
	name := "resilience-" + snap.name
	if snap.name == "" || len(name) > 128 {
		name = "resilience-" + p.ID
	}
	return a.publishBoardSnapshot(p.Dest.Board, arch, name, instances, recipients, now.Add(maxAge).UTC().Truncate(time.Second))
}

type resilienceCapture struct {
	file     *os.File
	size     int64
	digest   string
	findings int
	name     string
	harness  string
}

func (c *resilienceCapture) close() {
	_ = c.file.Close()
	_ = os.Remove(c.file.Name())
}

// captureResilienceSnapshot writes an agent's snapshot PAF. The history is
// read through the harness export, which copies only complete records, so
// a harness appending at that moment is never blocked or touched; the PAF
// built from it is validated before it is sent.
func captureResilienceSnapshot(conv string, now time.Time) (*resilienceCapture, error) {
	b, err := collectAgentBundle(conv, true)
	if err != nil {
		return nil, err
	}
	defer func() { _ = b.Close() }()
	findings := 0
	for _, f := range b.Manifest.Findings {
		findings += f.Count
	}
	at := now.UTC().Truncate(time.Second)
	parts := pafHistoryParts{
		AllowedFindings: flaggedCredentials(b.Manifest.Findings),
		Snapshot:        &paf.Snapshot{Name: "resilience", TakenAt: at, State: snapshotStateIdle},
		Lineage:         []paf.LineageStep{{Op: paf.OpSnapshot, At: at, From: &paf.Ref{Conversation: conv}, Detail: "resilience snapshot"}},
	}
	// Signed by this instance, so a custodian can check where it came from.
	archive, err := archiveAgentPAFWith(b, true, parts)
	if err != nil {
		return nil, err
	}
	st, err := archive.Stat()
	if err == nil {
		_, err = archive.Seek(0, io.SeekStart)
	}
	var a *paf.Archive
	if err == nil {
		a, err = paf.Open(archive, st.Size(), paf.Options{})
	}
	if err != nil {
		_ = archive.Close()
		_ = os.Remove(archive.Name())
		return nil, fmt.Errorf("validate: %w", err)
	}
	return &resilienceCapture{file: archive, size: st.Size(), digest: conversationDigest(a), findings: findings, name: b.Manifest.Agent.Name, harness: b.Manifest.Agent.Harness}, nil
}

// conversationDigest identifies what a snapshot carries apart from when it
// was taken: every entry except agent.json (which records the time) and
// privacy.json.
func conversationDigest(a *paf.Archive) string {
	h := sha256.New()
	for _, e := range a.Manifest.Entries {
		if e.Path == paf.PrivacyPath || strings.HasSuffix(e.Path, "/agent.json") {
			continue
		}
		fmt.Fprintf(h, "%s  %s\n", e.SHA256, e.Path)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func sealResilienceSnapshot(f *os.File, keys ...ed25519.PublicKey) ([]byte, error) {
	var recipients []string
	for _, k := range keys {
		r, err := seal.Recipient(k)
		if err != nil {
			return nil, err
		}
		recipients = append(recipients, r)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := seal.Encrypt(&out, f, recipients...); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// offerResilienceSnapshot queues a snapshot offer to the peer.
func offerResilienceSnapshot(peer *db.FederationPeer, d bundletransfer.Descriptor, sealed []byte) error {
	o := db.FederationBundleOffer{Descriptor: d, Peer: peer.InstanceID, Direction: "out", State: "pending"}
	fedBundleMu.Lock()
	defer fedBundleMu.Unlock()
	if _, err := db.InsertFederationBundleOffer(o, bundletransfer.Snapshot); err != nil {
		return err
	}
	cleanup := func() {
		_ = fedBundleSpool().Remove("out", peer.InstanceID, d.ID)
		_ = db.DeleteFederationBundleOffer("out", peer.InstanceID, d.ID)
	}
	if err := fedBundleSpool().Receive("out", peer.InstanceID, d, bytes.NewReader(sealed)); err != nil {
		cleanup()
		return err
	}
	if _, err := queueFederatedEnvelope(fedOutgoing{envelopeID: d.ID, peer: peer, kind: proto.KindBundleOffer, toLabel: peerDisplay(peer), subject: "resilience snapshot " + d.ID, preview: d.Summary, ttl: time.Until(d.ExpiresAt), payload: d}); err != nil {
		cleanup()
		return err
	}
	recordFederationAudit("resilience.snapshot.out", peerDisplay(peer), "", "", "agent="+d.Resilience.SourceAgent+" offer="+d.ID, 200)
	return nil
}

// --- receiving side ---

// resilienceOfferRefusal answers the generic offer routes for a snapshot.
const resilienceOfferRefusal = "a resilience snapshot is kept for its sender, not imported; see tclaude agent snapshot held"

// validResilienceSnapshot checks a received snapshot without opening it:
// it must be an age-encrypted file.
func validResilienceSnapshot(raw []byte) error {
	if !seal.IsEncrypted(raw) {
		return errors.New("a resilience snapshot must be encrypted")
	}
	return nil
}

// heldSnapshot describes one snapshot this node keeps for a peer.
type heldSnapshot struct {
	Peer        string    `json:"peer"`
	SourceAgent string    `json:"source_agent"`
	AgentName   string    `json:"agent_name,omitempty"`
	Harness     string    `json:"harness,omitempty"`
	TakenAt     time.Time `json:"taken_at"`
	ReceivedAt  time.Time `json:"received_at"`
	ExpiresAt   time.Time `json:"expires_at"`
	Bytes       int64     `json:"bytes"`
	SHA256      string    `json:"sha256"`
	Offer       string    `json:"offer"`
}

func resilienceHeldDir() string {
	return filepath.Join(config.DataDir(), "resilience", "held")
}

// resilienceHeldPath keys a held snapshot by the authenticated sender and
// the source agent from its signed offer; both are validated ids, so the
// path cannot escape the store.
func resilienceHeldPath(peer, agentID, offer string) string {
	return filepath.Join(resilienceHeldDir(), peer, agentID, offer+".paf.age")
}

var resilienceHeldMu sync.Mutex

// resilienceCommit moves a received snapshot into custody: it checks the
// grant again, prunes the source agent's copies to the sender's keep count
// and age, bounds what the peer may hold, and marks the offer applied, so
// the sender records a good snapshot.
func resilienceCommit(o *db.FederationBundleOffer) error {
	d := o.Descriptor
	r := d.Resilience
	if r == nil || !proto.ValidInstanceID(o.Peer) || !proto.ValidAgentRef(r.SourceAgent) || !proto.ValidStreamID(d.ID) {
		return errors.New("invalid resilience snapshot identity")
	}
	resilienceHeldMu.Lock()
	defer resilienceHeldMu.Unlock()
	fedBundleMu.Lock()
	defer fedBundleMu.Unlock()
	current, err := db.GetFederationBundleOffer("in", o.Peer, d.ID)
	if err != nil {
		return err
	}
	if current == nil || current.State != "ready" || !fedBundleOfferAdmitted(current, bundletransfer.Snapshot) {
		return errors.New("offer is not ready or the snapshots.receive grant was revoked")
	}
	raw, err := fedBundleSpool().Read("in", o.Peer, d)
	if err != nil {
		return err
	}
	if err = validResilienceSnapshot(raw); err != nil {
		if db.SetFederationBundleOfferState("in", o.Peer, d.ID, "declined", err.Error()) == nil {
			queueBundleResult(current, "declined")
		}
		return err
	}
	now := time.Now().UTC()
	maxAge := r.MaxAgeDays
	if maxAge == 0 {
		maxAge = resilienceDefaultMaxAgeDays
	}
	held := heldSnapshot{Peer: o.Peer, SourceAgent: r.SourceAgent, AgentName: r.AgentName, Harness: r.Harness, TakenAt: r.TakenAt, ReceivedAt: now, ExpiresAt: now.Add(time.Duration(maxAge) * 24 * time.Hour), Bytes: d.Bytes, SHA256: d.SHA256, Offer: d.ID}
	decline := func(msg string) error {
		if db.SetFederationBundleOfferState("in", o.Peer, d.ID, "declined", msg) == nil {
			queueBundleResult(current, "declined")
		}
		return errors.New(msg)
	}
	if d.Bytes > resilienceHeldPeerBytes {
		return decline("snapshot exceeds what this node keeps for a peer")
	}
	// Make room from one listing, newest first: expired copies and this
	// agent's copies beyond keep-1 go first, then the peer's oldest copies
	// until the new one fits.
	var kept []heldSnapshot
	var total int64
	agents := map[string]bool{}
	n := 0
	for _, h := range listHeld(o.Peer) {
		if !now.Before(h.ExpiresAt) {
			removeHeld(h)
			continue
		}
		if h.SourceAgent == r.SourceAgent {
			if n++; n > r.Keep-1 {
				removeHeld(h)
				continue
			}
		}
		kept = append(kept, h)
		total += h.Bytes
		agents[h.SourceAgent] = true
	}
	if !agents[r.SourceAgent] && len(agents) >= resilienceHeldPeerAgents {
		return decline(fmt.Sprintf("this node keeps snapshots of at most %d agents for a peer", resilienceHeldPeerAgents))
	}
	for len(kept) > 0 && total+d.Bytes > resilienceHeldPeerBytes {
		oldest := kept[len(kept)-1]
		removeHeld(oldest)
		total -= oldest.Bytes
		kept = kept[:len(kept)-1]
	}
	path := resilienceHeldPath(o.Peer, r.SourceAgent, d.ID)
	if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	meta, err := json.Marshal(held)
	if err != nil {
		return err
	}
	if err = writeSpoolFile(strings.TrimSuffix(path, ".paf.age")+".json", meta); err != nil {
		return err
	}
	if err = writeSpoolFile(path, raw); err != nil {
		_ = os.Remove(strings.TrimSuffix(path, ".paf.age") + ".json")
		return err
	}
	_ = fedBundleSpool().Remove("in", o.Peer, d.ID)
	if err = db.SetFederationBundleOfferState("in", o.Peer, d.ID, "applied", ""); err != nil {
		return err
	}
	queueBundleResult(current, "applied")
	recordFederationAudit("resilience.snapshot.in", o.Peer, "", "", "agent="+r.SourceAgent+" offer="+d.ID, 200)
	return nil
}

// listHeld lists what this node keeps, optionally for one peer.
func listHeld(peer string) []heldSnapshot {
	var out []heldSnapshot
	peers := []string{peer}
	if peer == "" {
		entries, _ := os.ReadDir(resilienceHeldDir())
		peers = nil
		for _, e := range entries {
			if e.IsDir() && proto.ValidInstanceID(e.Name()) {
				peers = append(peers, e.Name())
			}
		}
	}
	for _, p := range peers {
		agents, _ := os.ReadDir(filepath.Join(resilienceHeldDir(), p))
		for _, ag := range agents {
			if !ag.IsDir() || !proto.ValidAgentRef(ag.Name()) {
				continue
			}
			files, _ := os.ReadDir(filepath.Join(resilienceHeldDir(), p, ag.Name()))
			for _, f := range files {
				if !strings.HasSuffix(f.Name(), ".json") {
					continue
				}
				raw, err := os.ReadFile(filepath.Join(resilienceHeldDir(), p, ag.Name(), f.Name()))
				var h heldSnapshot
				if err == nil && json.Unmarshal(raw, &h) == nil && h.Peer == p && h.SourceAgent == ag.Name() {
					out = append(out, h)
				}
			}
		}
	}
	slices.SortFunc(out, func(a, b heldSnapshot) int { return b.TakenAt.Compare(a.TakenAt) })
	return out
}

func removeHeld(h heldSnapshot) {
	path := resilienceHeldPath(h.Peer, h.SourceAgent, h.Offer)
	_ = os.Remove(path)
	_ = os.Remove(strings.TrimSuffix(path, ".paf.age") + ".json")
}

// resilienceReceive takes custody of an accepted snapshot offer, fetching
// it from the sender first when it was not sent inline.
func (rt *fedRuntime) resilienceReceive(o *db.FederationBundleOffer) {
	if o.State == "pending" {
		if err := rt.fetchBundle(rt.ctx, o); err != nil {
			_ = db.SetFederationBundleOfferState("in", o.Peer, o.Descriptor.ID, "pending", err.Error())
			return
		}
		o.State = "ready"
	}
	if err := resilienceCommit(o); err != nil {
		slog.Warn("resilience: keep snapshot", "peer", o.Peer, "offer", o.Descriptor.ID, "error", err)
	}
}

// resilienceReceiveRetry finishes custody of snapshot offers left pending
// or ready, for example by a restart, and expires held copies.
func resilienceReceiveRetry() {
	now := time.Now()
	resilienceHeldMu.Lock()
	for _, h := range listHeld("") {
		if !now.Before(h.ExpiresAt) {
			removeHeld(h)
		}
	}
	resilienceHeldMu.Unlock()
	rt := currentFederation()
	if rt == nil {
		return
	}
	offers, err := db.ListFederationBundleOffers("in")
	if err != nil {
		return
	}
	for i := range offers {
		o := offers[i]
		if o.Descriptor.Type == bundletransfer.Snapshot.Name && (o.State == "pending" || o.State == "ready") && o.Descriptor.ExpiresAt.After(now) {
			rt.resilienceReceive(&o)
		}
	}
}

// removeHeldPeer deletes everything this node keeps for one peer.
func removeHeldPeer(peer string) int {
	resilienceHeldMu.Lock()
	defer resilienceHeldMu.Unlock()
	held := listHeld(peer)
	for _, h := range held {
		removeHeld(h)
	}
	_ = os.RemoveAll(filepath.Join(resilienceHeldDir(), peer))
	return len(held)
}
