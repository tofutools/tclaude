package agentd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

var identityRotationMu sync.Mutex
var identitySealMu sync.RWMutex

type localIdentityRotation struct {
	Chain   []proto.Rotation `json:"chain"`
	Pending bool             `json:"pending"`
}

func identityJournalPath() string {
	return filepath.Join(filepath.Dir(FederationKeyPath()), "rotation.json")
}
func identityNextKeyPath() string {
	return filepath.Join(filepath.Dir(FederationKeyPath()), "next-instance.key")
}
func loadIdentityJournal() (localIdentityRotation, error) {
	var j localIdentityRotation
	raw, err := os.ReadFile(identityJournalPath())
	if errors.Is(err, os.ErrNotExist) {
		return j, nil
	}
	if err != nil {
		return j, err
	}
	if err = json.Unmarshal(raw, &j); err != nil {
		return j, err
	}
	if len(j.Chain) == 0 {
		return j, errors.New("empty identity rotation journal")
	}
	if err = proto.VerifyRotationChain(j.Chain, j.Chain[0].OldID, j.Chain[0].OldKey, j.Chain[len(j.Chain)-1].NewID); err != nil {
		return j, err
	}
	return j, nil
}
func saveIdentityJournal(j localIdentityRotation) error {
	return saveIdentityPublicFile(identityJournalPath(), j)
}
func saveIdentityPublicFile(path string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(identityJournalPath()), ".rotation-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if err = f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	if _, err = f.Write(raw); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	return syncIdentityDir()
}
func syncIdentityDir() error {
	d, err := os.Open(filepath.Dir(FederationKeyPath()))
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}
func rotationWindow() time.Duration {
	cfg, err := config.Load()
	seconds := 600
	if err == nil && cfg != nil && cfg.Federation != nil && cfg.Federation.IdentityRotationSeconds > 0 {
		seconds = min(cfg.Federation.IdentityRotationSeconds, 604800)
	}
	return time.Duration(seconds) * time.Second
}
func prepareLocalIdentityRotation(now time.Time) (proto.Rotation, error) {
	identityRotationMu.Lock()
	defer identityRotationMu.Unlock()
	j, err := loadIdentityJournal()
	if err != nil {
		return proto.Rotation{}, err
	}
	if j.Pending {
		return proto.Rotation{}, errors.New("an identity rotation is already pending")
	}
	if len(j.Chain) >= proto.MaxRotationHops {
		return proto.Rotation{}, errors.New("four retained rotation hops reached; re-pair offline peers before resetting the public chain")
	}
	old, err := federationIdentity()
	if err != nil {
		return proto.Rotation{}, err
	}
	next, err := proto.NewIdentity()
	if err != nil {
		return proto.Rotation{}, err
	}
	previous := ""
	if len(j.Chain) > 0 {
		if j.Chain[len(j.Chain)-1].NewID != old.ID() {
			return proto.Rotation{}, errors.New("active key is not the journal successor; use explicit local recovery")
		}
		previous = j.Chain[len(j.Chain)-1].ID
	}
	r, err := proto.NewRotation(old, next, previous, len(j.Chain)+1, now, rotationWindow())
	if err != nil {
		return r, err
	}
	if err = proto.SaveIdentity(identityNextKeyPath(), next); err != nil {
		return r, err
	}
	keyFile, e := os.OpenFile(identityNextKeyPath(), os.O_RDWR, 0)
	if e != nil {
		return r, e
	}
	e = keyFile.Sync()
	_ = keyFile.Close()
	if e != nil {
		return r, e
	}
	j.Chain = append(j.Chain, r)
	j.Pending = true
	if err = saveIdentityJournal(j); err != nil {
		_ = os.Remove(identityNextKeyPath())
		return r, err
	}
	return r, nil
}

// Called outside the runtime's own waitgroup. The lifecycle lock quiesces all
// old-key streams before the private key is replaced. Journal recovery handles
// a crash after rename but before the final public metadata commit.
func activateLocalIdentityRotation(now time.Time) error {
	identityRotationMu.Lock()
	defer identityRotationMu.Unlock()
	j, err := loadIdentityJournal()
	if err != nil || !j.Pending {
		return err
	}
	r := j.Chain[len(j.Chain)-1]
	if now.Before(r.ActivateAt) {
		return nil
	}
	fedLifecycleMu.Lock()
	defer fedLifecycleMu.Unlock()
	stopFederationLocked()
	identitySealMu.Lock()
	defer identitySealMu.Unlock()
	fedIdentityMu.Lock()
	fedIdentity = nil
	fedIdentityMu.Unlock()
	if err = db.RetireLocalFederationIdentity(now); err != nil {
		return err
	}
	current, err := proto.LoadIdentity(FederationKeyPath())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if current == nil || current.ID() == r.OldID {
		next, e := proto.LoadIdentity(identityNextKeyPath())
		if e != nil {
			return e
		}
		if next.ID() != r.NewID {
			return errors.New("staged identity does not match rotation certificate")
		}
		if err = os.Rename(identityNextKeyPath(), FederationKeyPath()); err != nil {
			return err
		}
		if err = syncIdentityDir(); err != nil {
			return err
		}
	} else if current.ID() != r.NewID {
		return errors.New("active identity disagrees with rotation journal")
	}
	j.Pending = false
	if err = saveIdentityJournal(j); err != nil {
		return err
	}
	fedIdentityMu.Lock()
	fedIdentity = nil
	fedIdentityMu.Unlock()
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	recordFederationAudit("federation.identity.activated", r.NewID, "", "", fmt.Sprintf("old=%s new=%s", proto.Fingerprint(r.OldKey), proto.Fingerprint(r.NewKey)), 200)
	if cfg != nil && cfg.Federation != nil && cfg.Federation.Enabled && cfg.Federation.HubURL != "" {
		return startFederationWith(cfg.Federation)
	}
	return nil
}

func (rt *fedRuntime) observeIdentityChains(entries []proto.DirectoryEntry) {
	now := time.Now()
	for _, entry := range entries {
		for i, r := range entry.RotationChain {
			if p, _ := db.GetFederationPeer(r.OldID); p == nil {
				continue
			}
			suffix := entry.RotationChain[i:]
			if err := proto.VerifyRotationChain(suffix, r.OldID, r.OldKey, suffix[len(suffix)-1].NewID); err != nil {
				continue
			}
			first, err := db.ObserveFederationRotation(r, now, rotationWindow())
			if err == nil && first {
				recordFederationAudit("federation.identity.pending", r.OldID, "", "", fmt.Sprintf("old=%s new=%s; successor detection window started", proto.Fingerprint(r.OldKey), proto.Fingerprint(r.NewKey)), 200)
				state := "pending"
				if rows, e := db.ListFederationIdentityRotations(); e == nil {
					for _, row := range rows {
						if row.Statement.OldID == r.OldID {
							state = row.State
							break
						}
					}
				}
				rt.notifyIdentityChange(r, state)
			}
		}
	}
}
func (rt *fedRuntime) reconcileIdentityRotations() {
	rows, err := db.ListFederationIdentityRotations()
	if err != nil {
		return
	}
	for _, r := range rows {
		if r.State != "pending" || time.Now().Before(r.AcceptAfter) {
			continue
		}
		teleportLeaseMu.Lock()
		err = db.AcceptFederationRotation(r.Statement.OldID, time.Now())
		if err == nil {
			rt.teleportLeases = teleportLeaseObservations{}
		}
		teleportLeaseMu.Unlock()
		if err != nil {
			continue
		}
		recordFederationAudit("federation.identity.accepted", r.Statement.NewID, "", "", "old="+r.Statement.OldID+"; prior model capabilities revoked: peer identity rotated", 200)
		rt.notifyIdentityChange(r.Statement, "accepted")
		// Current trust checks close old-key streams; restarting the runtime also
		// drops idle streams immediately, while backup deadlines remain durable.
		go func() { _ = reloadFederationAfterRotation(rt) }()
		return
	}
}
func (rt *fedRuntime) notifyIdentityChange(r proto.Rotation, state string) {
	peer := r.OldID
	if state == "accepted" {
		peer = r.NewID
	}
	publishFleetEvent(peer, "identity_"+state, "Identity "+state+": "+r.OldID+" -> "+r.NewID, false)

	_, _ = db.InsertHumanMessage(&db.HumanMessage{FromTitle: "Federation identity", GroupName: "federation:" + r.OldID, Subject: "Peer fingerprint change: " + state, Body: fmt.Sprintf("%s -> %s\nOld fingerprint: %s\nNew fingerprint: %s\nStatement issued: %s\nActivation: %s\nA valid old-key signature cannot distinguish the owner from someone holding a stolen key. Revoke the predecessor if this change was unexpected.", r.OldID, r.NewID, proto.Fingerprint(r.OldKey), proto.Fingerprint(r.NewKey), r.IssuedAt.Format(time.RFC3339), r.ActivateAt.Format(time.RFC3339))})
}

func identityReceiptPath() string {
	return filepath.Join(filepath.Dir(FederationKeyPath()), "identity.json")
}

type localIdentityRecovery struct {
	NewID   string `json:"new_id"`
	NewKey  []byte `json:"new_key"`
	Pending bool   `json:"pending"`
}

func identityRecoveryPath() string {
	return filepath.Join(filepath.Dir(FederationKeyPath()), "recovery.json")
}
func loadIdentityRecovery() (localIdentityRecovery, error) {
	var r localIdentityRecovery
	raw, err := os.ReadFile(identityRecoveryPath())
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return r, err
	}
	if err = json.Unmarshal(raw, &r); err != nil {
		return r, err
	}
	if len(r.NewKey) != 32 || proto.InstanceID(r.NewKey) != r.NewID {
		return r, errors.New("invalid local identity recovery journal")
	}
	return r, nil
}

// Recovery generates a fresh unlinked key only by explicit operator action.
// A public intent is durable before retiring authority or replacing the key.
// Startup replays the same intent; it never mints another identity implicitly.
func recoverLocalIdentity() (*proto.Identity, error) { return completeLocalIdentityRecovery(true) }
func completeLocalIdentityRecovery(create bool) (*proto.Identity, error) {
	identityRotationMu.Lock()
	defer identityRotationMu.Unlock()
	r, err := loadIdentityRecovery()
	if err != nil {
		return nil, err
	}
	if !r.Pending && !create {
		return nil, nil
	}
	fedLifecycleMu.Lock()
	defer fedLifecycleMu.Unlock()
	stopFederationLocked()
	identitySealMu.Lock()
	defer identitySealMu.Unlock()
	if !r.Pending {
		next, e := proto.NewIdentity()
		if e != nil {
			return nil, e
		}
		// Explicit unlinked recovery also abandons a pending signed rotation whose
		// staged private key may have been lost. The public evidence is archived.
		if err = proto.SaveIdentity(identityNextKeyPath(), next); err != nil {
			return nil, err
		}
		f, e := os.OpenFile(identityNextKeyPath(), os.O_RDWR, 0)
		if e != nil {
			return nil, e
		}
		e = f.Sync()
		_ = f.Close()
		if e != nil {
			return nil, e
		}
		r = localIdentityRecovery{NewID: next.ID(), NewKey: next.Pub, Pending: true}
		if err = saveIdentityPublicFile(identityRecoveryPath(), r); err != nil {
			return nil, err
		}
	}
	fedIdentityMu.Lock()
	fedIdentity = nil
	fedIdentityMu.Unlock()
	// Never activate the new identity with predecessor capabilities still live.
	// This is idempotent when replaying a crash after the database commit.
	if err = db.RetireLocalFederationIdentity(time.Now()); err != nil {
		return nil, err
	}
	next, err := proto.LoadIdentity(FederationKeyPath())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if next == nil || next.ID() != r.NewID {
		next, err = proto.LoadIdentity(identityNextKeyPath())
		if err != nil {
			return nil, err
		}
		if next.ID() != r.NewID {
			return nil, errors.New("staged identity does not match recovery intent")
		}
		if err = os.Rename(identityNextKeyPath(), FederationKeyPath()); err != nil {
			return nil, err
		}
		if err = syncIdentityDir(); err != nil {
			return nil, err
		}
	}
	if err = os.Rename(identityJournalPath(), identityJournalPath()+".recovered-"+next.ID()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err = saveIdentityPublicFile(identityReceiptPath(), map[string]any{"instance_id": next.ID(), "public_key": next.Pub}); err != nil {
		return nil, err
	}
	r.Pending = false
	if err = saveIdentityPublicFile(identityRecoveryPath(), r); err != nil {
		return nil, err
	}
	fedIdentityMu.Lock()
	fedIdentity = next
	fedIdentityMu.Unlock()
	return next, nil
}

func resolveIdentityContinuation(instance string) (string, error) {
	current, err := db.ResolveFederationIdentitySuccessor(instance)
	if err != nil {
		return "", err
	}
	j, err := loadIdentityJournal()
	if err != nil {
		return "", err
	}
	for i, r := range j.Chain {
		if j.Pending && i == len(j.Chain)-1 {
			break
		}
		if r.OldID == current {
			current = r.NewID
		}
	}
	return current, nil
}

func reloadFederationAfterRotation(expected *fedRuntime) error {
	fedLifecycleMu.Lock()
	defer fedLifecycleMu.Unlock()
	if currentFederation() != expected {
		return nil
	}
	stopFederationLocked()
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg == nil || cfg.Federation == nil || !cfg.Federation.Enabled || cfg.Federation.HubURL == "" {
		return nil
	}
	return startFederationWith(cfg.Federation)
}

// Launch reconciliation follows an accepted successor for completion only;
// it does not start another process or rewrite the original request evidence.
func liveFederationPeer(instance string) (*db.FederationPeer, error) {
	peer, err := db.GetFederationPeer(instance)
	if err != nil || peer != nil {
		return peer, err
	}
	next, err := db.ResolveFederationIdentitySuccessor(instance)
	if err != nil {
		return nil, err
	}
	if next == instance {
		return nil, nil
	}
	return db.GetFederationPeer(next)
}
