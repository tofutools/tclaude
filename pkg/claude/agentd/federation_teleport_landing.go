package agentd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/agentbundle"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/federation/bundletransfer"
	"github.com/tofutools/tclaude/pkg/federation/jobrepo"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

var fedTeleportMu sync.Mutex

type teleportLandingAuthority struct{ record *db.FederationTeleport }
type teleportLandingContextKey struct{}
type teleportOfferContextKey struct{}

func teleportLandingFromRequest(r *http.Request) *teleportLandingAuthority {
	a, _ := r.Context().Value(teleportLandingContextKey{}).(*teleportLandingAuthority)
	return a
}
func normalizeTeleportLanding(p *db.FederationTeleportLanding) error {
	if p == nil {
		return nil
	}
	g, err := db.GetAgentGroupByName(p.Group)
	if p.GroupID != 0 {
		g, err = db.GetAgentGroupByID(p.GroupID)
	}
	if err != nil || g == nil || g.IsArchived() {
		return errors.New("teleport landing requires an active local group")
	}
	p.Group, p.GroupID = g.Name, g.ID
	if p.Repo != "" || p.RepoID != "" {
		ref := p.Repo
		if p.RepoID != "" {
			ref = p.RepoID
		}
		repo, err := db.GetFederationRepo(ref)
		if err != nil || repo == nil || !jobRepoAllows(repo, g.ID) {
			return errors.New("teleport landing repo must be enabled and allow the landing group")
		}
		p.Repo, p.RepoID = repo.Name, repo.ID
	}
	profile, err := db.ResolveSpawnProfile(p.SpawnProfile)
	if p.SpawnProfileID != 0 {
		profile, err = db.GetSpawnProfileByID(p.SpawnProfileID)
	}
	if err != nil || profile == nil || profile.Disabled {
		return errors.New("teleport landing requires an enabled local spawn profile")
	}
	p.SpawnProfile, p.SpawnProfileID = profile.Name, profile.ID
	if p.MaxLive < 1 || p.MaxLive > 1024 {
		return errors.New("teleport landing max_live must be 1..1024")
	}
	cwd, err := resolveSpawnCwd(p.Cwd)
	if err != nil || p.Cwd == "" {
		return errors.New("teleport landing cwd must be an existing local directory")
	}
	p.Cwd = cwd
	if p.CredentialsDefault == "" {
		p.CredentialsDefault = "local"
	}
	if len(p.CredentialsAllowed) == 0 {
		p.CredentialsAllowed = []string{"local"}
	}
	found := false
	if !bundletransfer.ValidCredentials(p.CredentialsDefault) {
		return errors.New("invalid teleport default credentials")
	}
	for _, mode := range p.CredentialsAllowed {
		if mode == "" || !bundletransfer.ValidCredentials(mode) {
			return errors.New("invalid teleport allowed credential mode")
		}
		if mode == p.CredentialsDefault {
			found = true
		}
	}
	if !found {
		return errors.New("teleport default credentials must be in credentials_allowed")
	}
	p.Limits = p.Limits.Effective()
	if p.Limits.Hour < 1 || p.Limits.Day < 1 || p.Limits.Chain < 1 || p.Limits.Chain > 128 || p.Limits.RevisitMinutes < 0 || p.Limits.RevisitMinutes > 525600 {
		return errors.New("invalid teleport landing limits")
	}
	return nil
}
func resolveTeleportLanding(peer string, group int64, credentials string) (*db.FederationTeleportLanding, *db.SpawnProfile, *db.FederationWorkerDefaults, string, error) {
	a, err := db.GetFederationNodeProfileAssignment(peer)
	if err != nil || a == nil || a.Profile.Definition.TeleportLanding == nil {
		return nil, nil, nil, "", errors.New("peer has no applied teleport landing policy")
	}
	landing := *a.Profile.Definition.TeleportLanding
	if landing.GroupID != group {
		return nil, nil, nil, "", errors.New("teleport group does not match the applied landing policy")
	}
	if err := normalizeTeleportLanding(&landing); err != nil {
		return nil, nil, nil, "", err
	}
	mode := credentials
	if mode == "" {
		mode = landing.CredentialsDefault
	}
	if mode == "" {
		mode = "local"
	}
	allowed := false
	for _, value := range landing.CredentialsAllowed {
		if value == mode {
			allowed = true
		}
	}
	if !allowed {
		return nil, nil, nil, "", errors.New("credential mode is not allowed by the receiver landing policy")
	}
	profile, err := db.GetSpawnProfileByID(landing.SpawnProfileID)
	if err != nil {
		return nil, nil, nil, "", err
	}
	if ref, err := teleportModelReference(mode, profile.Harness); err != nil {
		return nil, nil, nil, "", err
	} else if mode != "local" {
		mode = "proxy:" + ref
	}
	worker, err := db.ResolveFederationWorkerDefaults(peer)
	return &landing, profile, worker, mode, err
}
func (a *teleportLandingAuthority) check() error {
	t := a.record
	if teleportFrozen() {
		return errors.New("teleports are frozen by the operator")
	}
	if t == nil || t.Landing == nil || !fedPeerAllows(t.Peer, t.Landing.GroupID, PermAgentsTeleportReceive) {
		return errors.New("teleport receive grant revoked")
	}
	landing, profile, worker, mode, err := resolveTeleportLanding(t.Peer, t.Landing.GroupID, t.Intent.Credentials)
	if err != nil {
		return err
	}
	if err := checkTeleportRepo(t, t.Landing.GroupID); err != nil {
		return err
	}
	if err := teleportMatchesRequirements(&t.Intent, profile.Harness); err != nil {
		return err
	}
	if mode != t.Credentials || !reflect.DeepEqual(landing, t.Landing) || !reflect.DeepEqual(profile, t.Profile) || !reflect.DeepEqual(worker, t.WorkerDefaults) {
		return errors.New("teleport landing policy changed; no automatic launch")
	}
	return nil
}
func incomingTeleportRecord(peer string, sender string, d bundletransfer.Descriptor, groupID int64) (*db.FederationTeleport, error) {
	if d.Teleport == nil {
		return nil, nil
	}
	t := d.Teleport
	rt := currentFederation()
	if rt == nil || teleportFrozen() {
		return nil, errors.New("teleports are frozen or federation is disconnected")
	}
	last := t.Hops[len(t.Hops)-1]
	if last.Offer != d.ID || last.FromInstance != peer || last.FromAgent != sender || last.ToInstance != rt.id.ID() || last.ToGroup != d.Group || t.SourceAgent != sender {
		return nil, errors.New("teleport immediate sender or destination does not match signed offer")
	}
	if t.Clone != (d.Move == nil) || d.Move != nil && (d.Move.SourceAgent != t.SourceAgent || d.Move.SourceConv != t.SourceConv) {
		return nil, errors.New("teleport move/clone identity mismatch")
	}
	if err := validateTeleportLimits(t, rt.id.ID(), teleportLocalLimits()); err != nil {
		return nil, err
	}
	if err := teleportMatchesRequirements(t, ""); err != nil {
		return nil, err
	}
	mode, err := pendingTeleportCredentials(peer, t.Credentials)
	if err != nil {
		return nil, err
	}
	record := &db.FederationTeleport{Credentials: mode, Direction: "in", Peer: peer, Offer: d.ID, State: "pending", Intent: *t}
	assignment, err := db.GetFederationNodeProfileAssignment(peer)
	if err != nil {
		return nil, err
	}
	hasLanding := assignment != nil && assignment.Profile.Definition.TeleportLanding != nil && assignment.Profile.Definition.TeleportLanding.GroupID == groupID
	if fedPeerAllows(peer, groupID, PermAgentsTeleportReceive) && hasLanding {
		landing, profile, worker, mode, err := resolveTeleportLanding(peer, groupID, t.Credentials)
		if err != nil {
			return nil, err
		}
		if err = validateTeleportLimits(t, rt.id.ID(), landing.Limits); err != nil {
			return nil, err
		}
		record.State, record.Landing, record.Profile, record.WorkerDefaults, record.Credentials = "auto_pending", landing, profile, worker, mode
	} else if !fedPeerAllows(peer, groupID, PermAgentsReceive) {
		return nil, errors.New("automatic teleport needs an applied landing policy; pending offers need agents.receive")
	}
	if t.GitRef != "" {
		if !hasLanding || assignment.Profile.Definition.TeleportLanding.RepoID == "" {
			return nil, errors.New("git-ref requires an applied landing policy with an allowlisted repo")
		}
		record.Repo, err = db.GetFederationRepo(assignment.Profile.Definition.TeleportLanding.RepoID)
		if err != nil {
			return nil, err
		}
		if err := checkTeleportRepo(record, groupID); err != nil {
			return nil, err
		}
		if _, err := jobrepo.NormalizeRef(rt.ctx, t.GitRef); err != nil {
			return nil, err
		}
	}
	return record, nil
}
func reconcileFederationTeleports() {
	if !fedTeleportMu.TryLock() {
		return
	}
	defer fedTeleportMu.Unlock()
	if teleportFrozen() {
		return
	}
	rt := currentFederation()
	if rt == nil {
		return
	}
	rows, err := db.ListFederationTeleports()
	if err != nil {
		return
	}
	for _, t := range rows {
		if t.Direction != "in" || t.State != "auto_pending" {
			continue
		}
		processTeleportLanding(rt, &t)
	}
}
func processTeleportLanding(rt *fedRuntime, t *db.FederationTeleport) {
	authority := &teleportLandingAuthority{record: t}
	if err := authority.check(); err != nil {
		settleTeleportPending(t, err.Error())
		return
	}
	o, err := db.GetFederationBundleOffer("in", t.Peer, t.Offer)
	if err != nil || o == nil {
		return
	}
	if !o.Descriptor.ExpiresAt.After(time.Now()) {
		settleTeleportPending(t, "teleport offer expired")
		return
	}
	if o.State == "pending" {
		if err = rt.fetchBundle(rt.ctx, o); err != nil {
			settleTeleportPending(t, "history transfer unavailable; operator may retry the pending offer")
			return
		}
		o, err = db.GetFederationBundleOffer("in", t.Peer, t.Offer)
		if err != nil || o == nil {
			return
		}
	}
	if o.State != "ready" {
		return
	}
	fedBundleMu.Lock()
	defer fedBundleMu.Unlock()
	if err = authority.check(); err != nil {
		settleTeleportPending(t, err.Error())
		return
	}
	// The durable bundle reservation is the importer's existing one-shot gate.
	// The teleport row bridges node admission before that importer forks.
	nodeAdmission.Lock()
	limit, err := nodeCapacityLimit()
	if err == nil && limit > 0 {
		var used int
		used, err = nodeCapacityUsed("")
		if err == nil && used >= limit {
			err = errNodeBusy
		}
	}
	if err == nil {
		err = teleportPeerCapacity(t.Peer, t.Landing.MaxLive)
	}
	if err != nil {
		nodeAdmission.Unlock()
		settleTeleportPending(t, err.Error())
		return
	}
	t.TargetAgent = db.NewAgentID()
	t.State = "admitting"
	won, err := db.TransitionFederationTeleport(*t, "auto_pending")
	nodeAdmission.Unlock()
	if err != nil || !won {
		return
	}
	cwd := t.Landing.Cwd
	if t.Intent.GitRef != "" {
		checkout, err := prepareTeleportCheckout(rt.ctx, t, o.GroupID)
		if err != nil {
			t.State, t.TargetAgent = "pending", ""
			_, _ = db.TransitionFederationTeleport(*t, "admitting")
			_ = db.SetFederationBundleOfferState("in", o.Peer, o.Descriptor.ID, o.State, err.Error())
			return
		}
		cwd = checkout.Path
	}
	request := httptest.NewRequest(http.MethodPost, "/internal/teleport-landing", nil)
	request = request.WithContext(context.WithValue(rt.ctx, teleportLandingContextKey{}, authority))
	rec := httptest.NewRecorder()
	importFederationAgentOffer(rec, request, o, &fedBundleImportRequest{Group: t.Landing.Group, Cwd: cwd, configBundleRequest: configBundleRequest{Apply: true}})
	if rec.Code == 200 {
		t.State = "landed"
	} else {
		t.State = "uncertain"
		if o.ImportAgent == "" {
			t.State = "pending"
			t.TargetAgent = ""
			cleanupUnlaunchedTeleportCheckout(t)
		}
	}
	_, _ = db.TransitionFederationTeleport(*t, "admitting")
	recordFederationAudit("teleport.land", t.Peer, t.TargetAgent, t.Landing.Group, fmt.Sprintf("offer=%s predecessor=%s credentials=%s state=%s", t.Offer, t.Intent.SourceAgent, t.Credentials, t.State), rec.Code)
}
func teleportPeerCapacity(peer string, limit int) error {
	rows, err := db.ListFederationTeleports()
	if err != nil {
		return err
	}
	alive, err := session.LiveTmuxSessions()
	if err != nil {
		return err
	}
	pending, err := db.ListPendingSpawns()
	if err != nil {
		return err
	}
	reserved := map[string]bool{}
	for _, p := range pending {
		reserved[p.AgentID] = true
	}
	used := 0
	for _, t := range rows {
		if t.Direction != "in" || t.Peer != peer || t.TargetAgent == "" {
			continue
		}
		if t.State == "admitting" || t.State == "uncertain" || reserved[t.TargetAgent] || nodeAdmission.launches[t.TargetAgent] == config.DataDir() {
			used++
			continue
		}
		a, err := db.GetAgent(t.TargetAgent)
		if err != nil {
			return err
		}
		if a != nil && a.Active() {
			sessions, err := db.FindSessionsByConvID(a.CurrentConvID)
			if err != nil {
				return err
			}
			if isConvOnlineInSessions(sessions, alive) {
				used++
			}
		}
	}
	if used >= limit {
		return errors.New("teleport landing live and reserved worker limit reached")
	}
	return nil
}
func settleTeleportPending(t *db.FederationTeleport, reason string) {
	t.State = "pending"
	_, _ = db.TransitionFederationTeleport(*t, "auto_pending")
	if o, e := db.GetFederationBundleOffer("in", t.Peer, t.Offer); e == nil && o != nil {
		_ = db.SetFederationBundleOfferState("in", t.Peer, t.Offer, o.State, reason)
	}
}
func teleportBriefing(t *db.FederationTeleport) string {
	rt := currentFederation()
	node := "this node"
	if rt != nil {
		node = rt.id.ID()
	}
	return fmt.Sprintf("Teleport continuation. Origin: %s/%s. Current node: %s. Predecessor: %s/%s. Hop %d in chain %s. Credentials: %s. Paths and tools may differ; working-tree changes were not transferred. Prior inbox remains at the predecessor; this identity has a fresh inbox.\nContinuation note:\n%s", t.Intent.OriginInstance, t.Intent.OriginAgent, node, t.Peer, t.Intent.SourceAgent, len(t.Intent.Hops), t.Intent.Chain, t.Credentials, t.Intent.Note)
}
func teleportImportBundle(r *http.Request, b *agentbundle.Bundle) error {
	a := teleportLandingFromRequest(r)
	if a == nil {
		o, _ := r.Context().Value(teleportOfferContextKey{}).(*db.FederationBundleOffer)
		if o == nil || o.Descriptor.Teleport == nil {
			return nil
		}
		if teleportFrozen() {
			return errors.New("teleports are frozen by the operator")
		}
		if b.Manifest.History == nil || b.Manifest.History.SourceConvID != o.Descriptor.Teleport.SourceConv {
			return errors.New("teleport requires the offered source generation native history")
		}
		mode, err := pendingTeleportCredentials(o.Peer, o.Descriptor.Teleport.Credentials)
		if err != nil {
			return err
		}
		if err := teleportMatchesRequirements(o.Descriptor.Teleport, b.Manifest.Agent.Harness); err != nil {
			return err
		}
		frozen, err := db.GetFederationTeleport("in", o.Peer, o.Descriptor.ID)
		if err != nil || frozen == nil || frozen.Credentials != mode {
			return errors.New("teleport credential policy changed; no launch")
		}
		t := &db.FederationTeleport{Peer: o.Peer, Intent: *o.Descriptor.Teleport, Credentials: mode}
		if err := applyTeleportModelCredentials(b, mode); err != nil {
			return err
		}
		b.Manifest.Agent.StartupContext = teleportBriefing(t) + "\n\n" + b.Manifest.Agent.StartupContext
		return nil
	}
	if err := a.check(); err != nil {
		return err
	}
	if b.Manifest.History == nil || b.Manifest.History.SourceConvID != a.record.Intent.SourceConv {
		return errors.New("teleport requires the offered source generation native history")
	}
	t := a.record
	profile := profileToJSON(t.Profile)
	if profile.Harness == "" {
		profile.Harness = "claude"
	}
	if profile.Harness != b.Manifest.Agent.Harness {
		return errors.New("receiver launch harness is incompatible with transferred history")
	}
	// Only local launch settings survive. Source path placeholders and permissions
	// never select receiver paths, grants, launch hooks or named policy libraries.
	b.Manifest.Agent.Profile, _ = json.Marshal(profile)
	if err := applyTeleportModelCredentials(b, t.Credentials); err != nil {
		return err
	}
	b.Manifest.Placeholders = nil
	b.Manifest.Agent.StartupContext = teleportBriefing(t)
	b.Manifest.Agent.InitialMessage = ""
	return nil
}

func sameTeleportIntent(a, b *bundletransfer.TeleportIntent) bool { return reflect.DeepEqual(a, b) }

func pendingTeleportCredentials(peer, requested string) (string, error) {
	mode := requested
	assignment, err := db.GetFederationNodeProfileAssignment(peer)
	if err != nil {
		return "", err
	}
	var landing *db.FederationTeleportLanding
	if assignment != nil {
		landing = assignment.Profile.Definition.TeleportLanding
	}
	if mode == "" && landing != nil {
		mode = landing.CredentialsDefault
	}
	if mode == "" {
		mode = "local"
	}
	allowed := mode == "local"
	if landing != nil && len(landing.CredentialsAllowed) > 0 {
		allowed = false
		for _, candidate := range landing.CredentialsAllowed {
			if candidate == mode {
				allowed = true
			}
		}
	}
	if !allowed {
		return "", errors.New("credential mode is not allowed by the receiver landing policy")
	}
	if mode != "local" {
		ref, err := teleportModelReference(mode, "")
		if err != nil {
			return "", err
		}
		mode = "proxy:" + ref
	}
	return mode, nil
}

// Requirements travel with the offer and are rechecked locally. When launch
// settings are known, harness= matches the effective receiver harness.
func teleportMatchesRequirements(intent *bundletransfer.TeleportIntent, launchHarness string) error {
	if intent.Require == "" {
		return nil
	}
	match, err := proto.ParseNodeMatch(intent.Require)
	if err != nil {
		return err
	}
	n := localNodeMetadata()
	if n != nil && launchHarness != "" {
		copy := *n
		copy.Harnesses = nil
		for _, h := range n.Harnesses {
			if h.Name == launchHarness {
				copy.Harnesses = append(copy.Harnesses, h)
			}
		}
		n = &copy
	}
	if !match.Matches(n) {
		return errors.New("teleport requirements do not match receiver launch")
	}
	return nil
}

func checkTeleportRepo(t *db.FederationTeleport, group int64) error {
	if t.Intent.GitRef == "" {
		return nil
	}
	if t.Repo == nil {
		return errors.New("teleport repository is unavailable")
	}
	repo, err := db.GetFederationRepo(t.Repo.ID)
	if err != nil || repo == nil || repo.ID != t.Repo.ID || repo.Revision != t.Repo.Revision || !jobRepoAllows(repo, group) {
		return errors.New("teleport repository allowlist changed")
	}
	peer, err := db.GetFederationPeer(t.Peer)
	if err != nil || peer == nil || !fedPeerAllows(t.Peer, group, PermAgentsTeleportReceive) && !fedPeerAllows(t.Peer, group, PermAgentsReceive) {
		return errors.New("teleport repository admission revoked")
	}
	return jobrepo.Revalidate(context.Background(), repo.Definition)
}
func prepareTeleportCheckout(ctx context.Context, t *db.FederationTeleport, group int64) (*jobrepo.Checkout, error) {
	if err := checkTeleportRepo(t, group); err != nil {
		return nil, err
	}
	if t.Checkout != nil {
		return nil, errors.New("teleport checkout already reserved; inspect the prior launch")
	}
	root := filepath.Join(config.DataDir(), "federation", "teleport-checkouts", t.Peer, t.Offer)
	if err := os.MkdirAll(filepath.Dir(root), 0700); err != nil {
		return nil, err
	}
	checkout, err := jobrepo.Prepare(ctx, t.Repo.Definition, root, t.Intent.GitRef)
	if err != nil {
		return nil, err
	}
	if err := checkTeleportRepo(t, group); err != nil {
		_ = os.RemoveAll(root)
		return nil, err
	}
	t.Checkout = checkout
	won, err := db.TransitionFederationTeleport(*t, t.State)
	if err != nil || !won {
		_ = os.RemoveAll(root)
		return nil, errors.New("teleport changed while preparing checkout")
	}
	// Keep successful checkouts across restart and agent exit: their native
	// history may still resume here. Never remove a possibly running agent's cwd.
	return checkout, nil
}

// Caller has positively released the unlaunched import reservation. Never call
// this for a dispatched or uncertain launch, whose cwd may still be in use.
func cleanupUnlaunchedTeleportCheckout(t *db.FederationTeleport) {
	if t.Checkout == nil {
		return
	}
	root := filepath.Join(config.DataDir(), "federation", "teleport-checkouts", t.Peer, t.Offer)
	if os.RemoveAll(root) == nil {
		t.Checkout = nil
	}
}

// Credential mode replaces source/default profile routing explicitly. "off"
// also prevents the receiver's ordinary default profile from selecting a proxy.
func teleportModelReference(mode, launchHarness string) (string, error) {
	if mode == "local" {
		return "off", nil
	}
	ref, ok := strings.CutPrefix(mode, "proxy:")
	if !ok {
		return "", errors.New("invalid teleport credential mode")
	}
	if launchHarness != "" && launchHarness != "claude" {
		return "", errors.New("proxy credentials currently require the Claude Code harness")
	}
	peer, name, err := resolveModelProxyReference(ref)
	if err != nil {
		return "", err
	}
	return name + "@" + peer.InstanceID, nil
}
func applyTeleportModelCredentials(b *agentbundle.Bundle, mode string) error {
	ref, err := teleportModelReference(mode, b.Manifest.Agent.Harness)
	if err != nil {
		return err
	}
	var profile spawnProfileJSON
	if err := json.Unmarshal(b.Manifest.Agent.Profile, &profile); err != nil {
		return err
	}
	profile.ModelProxy = ref
	b.Manifest.Agent.Profile, err = json.Marshal(profile)
	return err
}
