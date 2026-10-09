package db

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

type FederationNodeProfileSpec struct {
	RequesterPays     string                        `json:"requester_pays,omitempty"`
	TeleportLanding   *FederationTeleportLanding    `json:"teleport_landing,omitempty"`
	TrustLevel        string                        `json:"trust_level"`
	Pools             []string                      `json:"pools,omitempty"`
	PeerGrants        []FederationPeerGrant         `json:"peer_grants,omitempty"`
	Labels            []string                      `json:"labels"`
	Bundle            json.RawMessage               `json:"config_bundle,omitempty"`
	WorkerPermissions map[string]PermissionOverride `json:"worker_permissions,omitempty"`
}
type FederationNodeProfile struct {
	ID         string                    `json:"id"`
	Name       string                    `json:"name"`
	Revision   int64                     `json:"revision"`
	Definition FederationNodeProfileSpec `json:"definition"`
}
type FederationNodeProfileAssignment struct {
	Profile       FederationNodeProfile `json:"profile"`
	ManagedPools  map[string]bool       `json:"managed_pools"`
	ManagedGrants map[string]bool       `json:"managed_grants"`
	OfferDigest   string                `json:"offer_digest,omitempty"`
	OfferID       string                `json:"offer_id,omitempty"`
}
type FederationWorkerDefaults struct {
	Peer        string                        `json:"peer"`
	ProfileID   string                        `json:"profile_id"`
	ProfileName string                        `json:"profile_name"`
	Revision    int64                         `json:"revision"`
	Permissions map[string]PermissionOverride `json:"permissions,omitempty"`
}

func SaveFederationNodeProfile(p *FederationNodeProfile) error {
	d, err := Open()
	if err != nil {
		return err
	}
	raw, err := json.Marshal(p.Definition)
	if err != nil {
		return err
	}
	if p.ID == "" {
		p.ID = proto.NewEnvelopeID()
		p.Revision = 1
		_, err = d.Exec(`INSERT INTO federation_node_profiles(id,name,revision,definition,created_at) VALUES(?,?,?,?,?)`, p.ID, p.Name, p.Revision, string(raw), dbTime(time.Now()))
		return err
	}
	r, err := d.Exec(`UPDATE federation_node_profiles SET name=?,revision=revision+1,definition=? WHERE id=? AND revision=?`, p.Name, string(raw), p.ID, p.Revision)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("profile changed; reload before saving")
	}
	p.Revision++
	return nil
}
func scanNodeProfile(row interface{ Scan(...any) error }) (*FederationNodeProfile, error) {
	var p FederationNodeProfile
	var raw string
	err := row.Scan(&p.ID, &p.Name, &p.Revision, &raw)
	if err != nil {
		return nil, err
	}
	err = json.Unmarshal([]byte(raw), &p.Definition)
	return &p, err
}
func GetFederationNodeProfile(ref string) (*FederationNodeProfile, error) {
	d, e := Open()
	if e != nil {
		return nil, e
	}
	return scanNodeProfile(d.QueryRow(`SELECT id,name,revision,definition FROM federation_node_profiles WHERE id=? OR name=?`, ref, ref))
}
func ListFederationNodeProfiles() ([]FederationNodeProfile, error) {
	d, e := Open()
	if e != nil {
		return nil, e
	}
	rows, e := d.Query(`SELECT id,name,revision,definition FROM federation_node_profiles ORDER BY name`)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []FederationNodeProfile{}
	for rows.Next() {
		p, e := scanNodeProfile(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}
func DeleteFederationNodeProfile(id string) error {
	d, e := Open()
	if e != nil {
		return e
	}
	_, e = d.Exec(`DELETE FROM federation_node_profiles WHERE id=?`, id)
	return e
}
func SetDefaultFederationNodeProfile(id string) error {
	d, e := Open()
	if e != nil {
		return e
	}
	if id == "" {
		_, e = d.Exec(`DELETE FROM federation_node_profile_default`)
	} else {
		_, e = d.Exec(`INSERT INTO federation_node_profile_default(singleton,profile_id) VALUES(1,?) ON CONFLICT(singleton) DO UPDATE SET profile_id=excluded.profile_id`, id)
	}
	return e
}
func DefaultFederationNodeProfile() (*FederationNodeProfile, error) {
	d, e := Open()
	if e != nil {
		return nil, e
	}
	p, e := scanNodeProfile(d.QueryRow(`SELECT p.id,p.name,p.revision,p.definition FROM federation_node_profiles p JOIN federation_node_profile_default f ON f.profile_id=p.id`))
	if errors.Is(e, sql.ErrNoRows) {
		return nil, nil
	}
	return p, e
}
func ListFederationNodeProfilePeers(id string) ([]string, error) {
	d, e := Open()
	if e != nil {
		return nil, e
	}
	rows, e := d.Query(`SELECT peer FROM federation_node_profile_assignments WHERE profile_id=? ORDER BY peer`, id)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var p string
		if e = rows.Scan(&p); e != nil {
			return nil, e
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
func GetFederationNodeProfileAssignment(peer string) (*FederationNodeProfileAssignment, error) {
	d, e := Open()
	if e != nil {
		return nil, e
	}
	var raw string
	e = d.QueryRow(`SELECT snapshot FROM federation_node_profile_assignments WHERE peer=?`, peer).Scan(&raw)
	if errors.Is(e, sql.ErrNoRows) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	var a FederationNodeProfileAssignment
	e = json.Unmarshal([]byte(raw), &a)
	return &a, e
}

// ResolveFederationWorkerDefaults is shared by peer spawns and future remote job launchers.
// It reads the last applied revision, never an edited but unapplied profile.
func ResolveFederationWorkerDefaults(peer string) (*FederationWorkerDefaults, error) {
	a, e := GetFederationNodeProfileAssignment(peer)
	if e != nil || a == nil {
		return nil, e
	}
	return &FederationWorkerDefaults{Peer: peer, ProfileID: a.Profile.ID, ProfileName: a.Profile.Name, Revision: a.Profile.Revision, Permissions: a.Profile.Definition.WorkerPermissions}, nil
}
func RecordFederationWorkerDefaults(agentID string, w *FederationWorkerDefaults) error {
	if w == nil {
		return nil
	}
	d, e := Open()
	if e != nil {
		return e
	}
	raw, e := json.Marshal(w)
	if e != nil {
		return e
	}
	_, e = d.Exec(`INSERT INTO federation_worker_defaults(agent_id,snapshot) VALUES(?,?)`, agentID, string(raw))
	return e
}
func GetFederationWorkerDefaults(agentID string) (*FederationWorkerDefaults, error) {
	d, e := Open()
	if e != nil {
		return nil, e
	}
	var raw string
	e = d.QueryRow(`SELECT snapshot FROM federation_worker_defaults WHERE agent_id=?`, agentID).Scan(&raw)
	if errors.Is(e, sql.ErrNoRows) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	var w FederationWorkerDefaults
	e = json.Unmarshal([]byte(raw), &w)
	return &w, e
}
func MarkFederationNodeProfileOffer(peer, digest, id string) error {
	d, e := Open()
	if e != nil {
		return e
	}
	_, e = d.Exec(`UPDATE federation_node_profile_assignments SET snapshot=json_set(snapshot,'$.offer_digest',?,'$.offer_id',?) WHERE peer=?`, digest, id, peer)
	return e
}

type FederationNodeProfileChange struct {
	Item     string `json:"item"`
	Before   any    `json:"before,omitempty"`
	After    any    `json:"after,omitempty"`
	Security bool   `json:"security"`
}
type FederationNodeProfilePoolView struct {
	ID       string                `json:"id"`
	Name     string                `json:"name"`
	Grants   []FederationPeerGrant `json:"live_grants"`
	Security bool                  `json:"security"`
}
type FederationNodeProfilePlan struct {
	Pools             []FederationNodeProfilePoolView `json:"pools"`
	Profile           FederationNodeProfile           `json:"profile"`
	Peer              string                          `json:"peer"`
	Token             string                          `json:"preview_token"`
	Changes           []FederationNodeProfileChange   `json:"changes"`
	Conflicts         []string                        `json:"conflicts"`
	Applied           bool                            `json:"applied"`
	SecurityChanges   int                             `json:"security_changes"`
	FutureWorkersOnly bool                            `json:"future_workers_only"`
}
type nodeProfileState struct {
	Level      string
	Pools      map[string]bool
	Grants     map[string]FederationPeerGrant
	Assignment *FederationNodeProfileAssignment
	Key        []byte
	Label      string
}

func nodeProfileGrantKey(g FederationPeerGrant) string { return g.Slug + "\n" + g.Scope }
func nodeProfileGrantSame(a, b FederationPeerGrant) bool {
	return a.Slug == b.Slug && a.Scope == b.Scope && a.SpawnPolicy.Equal(b.SpawnPolicy)
}
func nodeProfileReadState(tx *sql.Tx, peer string) (nodeProfileState, error) {
	s := nodeProfileState{Pools: map[string]bool{}, Grants: map[string]FederationPeerGrant{}}
	e := tx.QueryRow(`SELECT trust_level,pubkey,label FROM federation_peers WHERE instance_id=?`, peer).Scan(&s.Level, &s.Key, &s.Label)
	if errors.Is(e, sql.ErrNoRows) {
		return s, nil
	}
	if e != nil {
		return s, e
	}
	rows, e := tx.Query(`SELECT group_id FROM federation_node_group_members WHERE peer=? ORDER BY group_id`, peer)
	if e != nil {
		return s, e
	}
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			_ = rows.Close()
			return s, e
		}
		s.Pools[id] = true
	}
	e = rows.Err()
	_ = rows.Close()
	if e != nil {
		return s, e
	}
	rows, e = tx.Query(`SELECT slug,scope,spawn_policy FROM federation_peer_grants WHERE peer=? ORDER BY slug,scope`, peer)
	if e != nil {
		return s, e
	}
	for rows.Next() {
		var g FederationPeerGrant
		var raw string
		if e = rows.Scan(&g.Slug, &g.Scope, &raw); e != nil {
			_ = rows.Close()
			return s, e
		}
		if e = json.Unmarshal([]byte(raw), &g.SpawnPolicy); e != nil {
			_ = rows.Close()
			return s, e
		}
		s.Grants[nodeProfileGrantKey(g)] = g
	}
	e = rows.Err()
	_ = rows.Close()
	if e != nil {
		return s, e
	}
	var raw string
	e = tx.QueryRow(`SELECT snapshot FROM federation_node_profile_assignments WHERE peer=?`, peer).Scan(&raw)
	if e == nil {
		e = json.Unmarshal([]byte(raw), &s.Assignment)
	} else if errors.Is(e, sql.ErrNoRows) {
		e = nil
	}
	return s, e
}

// PlanFederationNodeProfile recomputes a three-way reconciliation under the same
// writer transaction used to apply it. The token binds profile revision and all
// current peer policy, preventing a stale preview from overwriting a manual edit.
func PlanFederationNodeProfile(profileID, peer, token string, newPeer *FederationPeer) (*FederationNodeProfilePlan, error) {
	d, e := Open()
	if e != nil {
		return nil, e
	}
	tx, e := d.Begin()
	if e != nil {
		return nil, e
	}
	defer func() { _ = tx.Rollback() }()
	plan, e := planFederationNodeProfileTx(tx, profileID, peer, token, newPeer)
	if e != nil {
		return plan, e
	}
	if token != "" {
		if e = tx.Commit(); e != nil {
			return nil, e
		}
		plan.Applied = true
	}
	return plan, nil
}
func planFederationNodeProfileTx(tx *sql.Tx, profileID, peer, token string, newPeer *FederationPeer) (*FederationNodeProfilePlan, error) {
	var e error
	if token != "" {
		if _, e = tx.Exec(`UPDATE federation_node_profiles SET revision=revision WHERE id=?`, profileID); e != nil {
			return nil, e
		}
	}
	p, e := scanNodeProfile(tx.QueryRow(`SELECT id,name,revision,definition FROM federation_node_profiles WHERE id=?`, profileID))
	if e != nil {
		return nil, e
	}
	s, e := nodeProfileReadState(tx, peer)
	if e != nil {
		return nil, e
	}
	if s.Level == "" && newPeer == nil {
		return nil, errors.New("peer is no longer trusted")
	}
	poolViews := []FederationNodeProfilePoolView{}
	for _, id := range p.Definition.Pools {
		view := FederationNodeProfilePoolView{ID: id, Security: true, Grants: []FederationPeerGrant{}}
		if e = tx.QueryRow(`SELECT name FROM federation_node_groups WHERE id=?`, id).Scan(&view.Name); e != nil {
			return nil, fmt.Errorf("profile pool %s no longer exists: %w", id, e)
		}
		rows, err := tx.Query(`SELECT slug,scope,spawn_policy FROM federation_node_group_grants WHERE group_id=? ORDER BY slug,scope`, id)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var g FederationPeerGrant
			var raw string
			if err = rows.Scan(&g.Slug, &g.Scope, &raw); err != nil {
				_ = rows.Close()
				return nil, err
			}
			if err = json.Unmarshal([]byte(raw), &g.SpawnPolicy); err != nil {
				_ = rows.Close()
				return nil, err
			}
			view.Grants = append(view.Grants, g)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, err
		}
		poolViews = append(poolViews, view)
	}
	encoded, _ := json.Marshal([]any{p, s, newPeer, poolViews})
	sum := sha256.Sum256(encoded)
	plan := &FederationNodeProfilePlan{Pools: poolViews, Profile: *p, Peer: peer, Token: hex.EncodeToString(sum[:]), Changes: []FederationNodeProfileChange{}, Conflicts: []string{}, FutureWorkersOnly: true}
	if token != "" && token != plan.Token {
		return nil, errors.New("stale preview; preview the current profile and peer state again")
	}
	a := FederationNodeProfileAssignment{Profile: *p, ManagedPools: map[string]bool{}, ManagedGrants: map[string]bool{}}
	old := s.Assignment
	if old != nil {
		a.OfferDigest = old.OfferDigest
		a.OfferID = old.OfferID
	}
	change := func(item string, before, after any) {
		plan.Changes = append(plan.Changes, FederationNodeProfileChange{Item: item, Before: before, After: after, Security: true})
		plan.SecurityChanges++
	}
	if old != nil && s.Level != old.Profile.Definition.TrustLevel && s.Level != p.Definition.TrustLevel {
		plan.Conflicts = append(plan.Conflicts, "trust level was manually changed")
	}
	if s.Level != p.Definition.TrustLevel {
		change("trust_level", s.Level, p.Definition.TrustLevel)
	}
	desiredPools := map[string]bool{}
	for _, id := range p.Definition.Pools {
		var exists int
		if e = tx.QueryRow(`SELECT count(*) FROM federation_node_groups WHERE id=?`, id).Scan(&exists); e != nil {
			return nil, e
		}
		if exists != 1 {
			return nil, fmt.Errorf("profile pool %s no longer exists", id)
		}
		desiredPools[id] = true
	}
	poolKeys := map[string]bool{}
	for k := range desiredPools {
		poolKeys[k] = true
	}
	if old != nil {
		for k := range old.ManagedPools {
			poolKeys[k] = true
		}
	}
	for k := range poolKeys {
		want, have := desiredPools[k], s.Pools[k]
		owned := old != nil && old.ManagedPools[k]
		if owned && !have && want {
			plan.Conflicts = append(plan.Conflicts, "pool membership was manually removed: "+k)
		}
		if want && (owned || !have) {
			a.ManagedPools[k] = true
		}
		if want != have && (want || owned) {
			change("pool/"+k, have, want)
		}
	}
	desiredGrants := map[string]FederationPeerGrant{}
	for _, g := range p.Definition.PeerGrants {
		desiredGrants[nodeProfileGrantKey(g)] = g
	}
	oldGrants := map[string]FederationPeerGrant{}
	if old != nil {
		for _, g := range old.Profile.Definition.PeerGrants {
			oldGrants[nodeProfileGrantKey(g)] = g
		}
	}
	keys := map[string]bool{}
	for k := range desiredGrants {
		keys[k] = true
	}
	if old != nil {
		for k := range old.ManagedGrants {
			keys[k] = true
		}
	}
	for k := range keys {
		want, wok := desiredGrants[k]
		have, hok := s.Grants[k]
		owned := old != nil && old.ManagedGrants[k]
		equal := wok == hok && (!wok || nodeProfileGrantSame(want, have))
		unchanged := owned && hok && nodeProfileGrantSame(oldGrants[k], have)
		if !equal && hok && !unchanged {
			plan.Conflicts = append(plan.Conflicts, "peer grant was manually changed or already differs: "+k)
		}
		if !equal && owned && !hok && wok {
			plan.Conflicts = append(plan.Conflicts, "peer grant was manually removed: "+k)
		}
		if wok && (owned || !hok) {
			a.ManagedGrants[k] = true
		}
		if !equal && (wok || owned) {
			var b, n any
			if hok {
				b = have
			}
			if wok {
				n = want
			}
			change("grant/"+k, b, n)
		}
	}
	oldRequesterPays := ""
	if old != nil {
		oldRequesterPays = old.Profile.Definition.RequesterPays
	}
	if oldRequesterPays != p.Definition.RequesterPays {
		change("future_requester_pays", oldRequesterPays, p.Definition.RequesterPays)
	}
	if old == nil || !nodeProfileJSONEqual(old.Profile.Definition.WorkerPermissions, p.Definition.WorkerPermissions) {
		var before any
		if old != nil {
			before = old.Profile.Definition.WorkerPermissions
		}
		change("future_worker_permissions", before, p.Definition.WorkerPermissions)
	}
	var oldLanding *FederationTeleportLanding
	if old != nil {
		oldLanding = old.Profile.Definition.TeleportLanding
	}
	if !nodeProfileJSONEqual(oldLanding, p.Definition.TeleportLanding) {
		change("future_teleport_landing", oldLanding, p.Definition.TeleportLanding)
	}
	var oldLabels []string
	var oldBundle json.RawMessage
	if old != nil {
		oldLabels = old.Profile.Definition.Labels
		oldBundle = old.Profile.Definition.Bundle
	}
	if !nodeProfileJSONEqual(oldLabels, p.Definition.Labels) {
		plan.Changes = append(plan.Changes, FederationNodeProfileChange{Item: "offer_only/requested_labels", Before: oldLabels, After: p.Definition.Labels})
	}
	if !nodeProfileJSONEqual(oldBundle, p.Definition.Bundle) {
		plan.Changes = append(plan.Changes, FederationNodeProfileChange{Item: "offer_only/config_bundle", Before: oldBundle, After: p.Definition.Bundle})
	}
	sort.Slice(plan.Changes, func(i, j int) bool { return plan.Changes[i].Item < plan.Changes[j].Item })
	sort.Strings(plan.Conflicts)
	if token == "" {
		return plan, nil
	}
	if len(plan.Conflicts) > 0 {
		return plan, errors.New("manual-edit conflicts; resolve them before applying")
	}
	if s.Level == "" {
		_, e = tx.Exec(`INSERT INTO federation_peers(instance_id,pubkey,label,name,trusted_at,trust_level) VALUES(?,?,?,?,?,?)`, newPeer.InstanceID, newPeer.PubKey, newPeer.Label, newPeer.Name, dbTime(time.Now()), p.Definition.TrustLevel)
	} else if newPeer != nil {
		_, e = tx.Exec(`UPDATE federation_peers SET trust_level=?,label=?,name=? WHERE instance_id=?`, p.Definition.TrustLevel, newPeer.Label, newPeer.Name, peer)
	} else {
		_, e = tx.Exec(`UPDATE federation_peers SET trust_level=? WHERE instance_id=?`, p.Definition.TrustLevel, peer)
	}
	if e != nil {
		return nil, e
	}
	for _, c := range plan.Changes {
		switch {
		case len(c.Item) > 5 && c.Item[:5] == "pool/":
			id := c.Item[5:]
			if c.After == true {
				_, e = tx.Exec(`INSERT INTO federation_node_group_members(group_id,peer) VALUES(?,?) ON CONFLICT DO NOTHING`, id, peer)
			} else {
				_, e = tx.Exec(`DELETE FROM federation_node_group_members WHERE group_id=? AND peer=?`, id, peer)
			}
		case len(c.Item) > 6 && c.Item[:6] == "grant/":
			key := c.Item[6:]
			if g, ok := desiredGrants[key]; ok {
				raw, _ := json.Marshal(g.SpawnPolicy)
				_, e = tx.Exec(`INSERT INTO federation_peer_grants(peer,slug,scope,spawn_policy,created_at) VALUES(?,?,?,?,?) ON CONFLICT(peer,slug,scope) DO UPDATE SET spawn_policy=excluded.spawn_policy`, peer, g.Slug, g.Scope, string(raw), dbTime(time.Now()))
			} else {
				g := oldGrants[key]
				_, e = tx.Exec(`DELETE FROM federation_peer_grants WHERE peer=? AND slug=? AND scope=?`, peer, g.Slug, g.Scope)
			}
		}
		if e != nil {
			return nil, e
		}
	}
	raw, e := json.Marshal(a)
	if e != nil {
		return nil, e
	}
	_, e = tx.Exec(`INSERT INTO federation_node_profile_assignments(peer,profile_id,snapshot) VALUES(?,?,?) ON CONFLICT(peer) DO UPDATE SET profile_id=excluded.profile_id,snapshot=excluded.snapshot`, peer, p.ID, string(raw))
	if e != nil {
		return nil, e
	}
	return plan, nil
}
func nodeProfileJSONEqual(a, b any) bool {
	aa, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return string(aa) == string(bb)
}

// FederationTeleportLanding pins local registry identities, never source
// paths, permissions or profile handles. Names are operator input/display.
type FederationTeleportLanding struct {
	RequesterPays      string                `json:"requester_pays,omitempty"`
	Repo               string                `json:"repo,omitempty"`
	RepoID             string                `json:"repo_id,omitempty"`
	Group              string                `json:"group"`
	GroupID            int64                 `json:"group_id,omitempty"`
	Cwd                string                `json:"cwd"`
	SpawnProfile       string                `json:"spawn_profile"`
	SpawnProfileID     int64                 `json:"spawn_profile_id,omitempty"`
	MaxLive            int                   `json:"max_live"`
	CredentialsDefault string                `json:"credentials_default,omitempty"`
	CredentialsAllowed []string              `json:"credentials_allowed,omitempty"`
	Limits             config.TeleportLimits `json:"limits,omitempty"`
}
