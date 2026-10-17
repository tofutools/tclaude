package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"time"
)

// FederationSpawnPolicy contains receiver-owned launch settings. Empty fields
// inherit ordinary operator group-spawn defaults.
type FederationSpawnPolicy struct {
	// IncludeMembers narrows the roster opt-in on groups.visibility.read.
	IncludeMembers bool `json:"include_members,omitempty"`
	// PermissionSlugs is the optional receiver allowlist for agents.receive.permissions.
	PermissionSlugs []string `json:"permission_slugs,omitempty"`
	AllowedProfiles []string `json:"allowed_profiles,omitempty"`
	// The launch settings a peer may choose per spawn request are their own
	// grants (groups.members.spawn.cwd, .harness, …); see migrateV269toV270.
	// LegacyAllowedOverrides only decodes the list those grants replaced
	// (see UnmarshalJSON), so a caller still sending it is refused rather
	// than silently ignored. It is never written back.
	LegacyAllowedOverrides []string `json:"-"`
	RequesterPays          string   `json:"requester_pays,omitempty"`
	JobApproval            string   `json:"job_approval,omitempty"` // jobs.run only: auto (default) or manual
	Profile                string   `json:"profile,omitempty"`
	Cwd                    string   `json:"cwd,omitempty"`
	Harness                string   `json:"harness,omitempty"`
	Model                  string   `json:"model,omitempty"`
	MaxLive                int      `json:"max_live,omitempty"`
}

// Equal includes selectable profiles in inherited-policy conflict checks.
func (p FederationSpawnPolicy) Equal(other FederationSpawnPolicy) bool {
	return p.IncludeMembers == other.IncludeMembers && slices.Equal(p.PermissionSlugs, other.PermissionSlugs) && slices.Equal(p.AllowedProfiles, other.AllowedProfiles) &&
		p.RequesterPays == other.RequesterPays && p.JobApproval == other.JobApproval &&
		p.Profile == other.Profile && p.Cwd == other.Cwd && p.Harness == other.Harness &&
		p.Model == other.Model && p.MaxLive == other.MaxLive
}

// UnmarshalJSON also reads the replaced allowed_overrides list.
func (p *FederationSpawnPolicy) UnmarshalJSON(b []byte) error {
	type plain FederationSpawnPolicy
	var v struct {
		plain
		Legacy []string `json:"allowed_overrides"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*p = FederationSpawnPolicy(v.plain)
	p.LegacyAllowedOverrides = v.Legacy
	return nil
}

// legacySpawnOverrideFields are the fields allowed_overrides could list.
var legacySpawnOverrideFields = []string{"cwd", "harness", "model", "effort"}

// ExpandLegacySpawnOverrides turns a spawn grant's replaced allowed_overrides
// list into the groups.members.spawn.<field> grants with its scope that took
// its place (migrateV269toV270 does the same for stored grants).
func ExpandLegacySpawnOverrides(grants []FederationPeerGrant) []FederationPeerGrant {
	have := map[[2]string]bool{}
	for _, g := range grants {
		have[[2]string{g.Slug, g.Scope}] = true
	}
	out := make([]FederationPeerGrant, 0, len(grants))
	var added []FederationPeerGrant
	for _, g := range grants {
		legacy := g.SpawnPolicy.LegacyAllowedOverrides
		g.SpawnPolicy.LegacyAllowedOverrides = nil
		out = append(out, g)
		if g.Slug != "groups.members.spawn" {
			continue
		}
		for _, field := range legacy {
			key := [2]string{"groups.members.spawn." + field, g.Scope}
			if slices.Contains(legacySpawnOverrideFields, field) && !have[key] {
				have[key] = true
				added = append(added, FederationPeerGrant{PoolID: g.PoolID, PoolName: g.PoolName, Peer: g.Peer, Slug: key[0], Scope: g.Scope})
			}
		}
	}
	return append(out, added...)
}

type FederationPeerGrant struct {
	PoolID      string                `json:"pool_id,omitempty"`
	PoolName    string                `json:"pool_name,omitempty"`
	Peer        string                `json:"peer"`
	Slug        string                `json:"slug"`
	Scope       string                `json:"scope"`
	SpawnPolicy FederationSpawnPolicy `json:"spawn_policy,omitempty"`
	CreatedAt   time.Time             `json:"created_at"`
	ExpiresAt   *time.Time            `json:"expires_at,omitempty"`
}

func FederationGroupScope(groupID int64) string { return "group=" + strconv.FormatInt(groupID, 10) }

func UpsertFederationPeerGrant(g FederationPeerGrant) error {
	d, err := Open()
	if err != nil {
		return err
	}
	policy, err := json.Marshal(g.SpawnPolicy)
	if err != nil {
		return err
	}
	var expires any
	if g.ExpiresAt != nil {
		expires = dbTime(*g.ExpiresAt)
	}
	_, err = d.Exec(`INSERT INTO federation_peer_grants(peer,slug,scope,spawn_policy,created_at,expires_at) VALUES(?,?,?,?,?,?)
 ON CONFLICT(peer,slug,scope) DO UPDATE SET spawn_policy=excluded.spawn_policy,expires_at=excluded.expires_at`, g.Peer, g.Slug, g.Scope, string(policy), dbTime(time.Now()), expires)
	return err
}

func DeleteFederationPeerGrant(peer, slug, scope string) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	res, err := d.Exec(`DELETE FROM federation_peer_grants WHERE peer=? AND slug=? AND scope=?`, peer, slug, scope)
	if err != nil {
		return false, err
	}
	// The launch settings a spawn grant let the peer choose go with it, so
	// a later spawn grant does not quietly reopen them.
	if slug == "groups.members.spawn" {
		if _, err := d.Exec(`DELETE FROM federation_peer_grants WHERE peer=? AND slug LIKE 'groups.members.spawn.%' AND scope=?`, peer, scope); err != nil {
			return false, err
		}
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func ListFederationPeerGrants(peer string) ([]FederationPeerGrant, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT peer,slug,scope,spawn_policy,created_at,expires_at FROM federation_peer_grants WHERE (?='' OR peer=?) AND (expires_at IS NULL OR expires_at>?) ORDER BY peer,slug,scope`, peer, peer, dbTime(time.Now()))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []FederationPeerGrant{}
	for rows.Next() {
		var g FederationPeerGrant
		var policy string
		var at, expires dbTimestamp
		if err := rows.Scan(&g.Peer, &g.Slug, &g.Scope, &policy, &at, &expires); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(policy), &g.SpawnPolicy); err != nil {
			return nil, err
		}
		g.CreatedAt = at.Time()
		if !expires.Time().IsZero() {
			at := expires.Time()
			g.ExpiresAt = &at
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// Auto workers survive daemon restarts; callers count only workers still live.
func ListFederationAutoWorkers(peer string) ([]string, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT agent_id FROM federation_auto_workers WHERE peer=?`, peer)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// BeginFederationSpawnRequest reserves a stable identity before any process
// starts. A crash at any later point leaves a launching request, never a
// second approval of a possibly running worker.
func BeginFederationSpawnRequest(id int64, agentID string, automatic bool) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	tx, err := d.Begin()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec(`UPDATE federation_spawn_requests SET status=?, result_agent=?, launch_label='', launch_started_at=?,automatic=?,notice_sent=0,result_sent=0,reason='' WHERE id=? AND status=?`, FedSpawnLaunching, agentID, dbTime(time.Now()), automatic, id, FedSpawnPending)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return false, nil
	}
	if automatic {
		_, err = tx.Exec(`INSERT INTO federation_auto_workers(request_id,peer,agent_id) SELECT id,from_instance,result_agent FROM federation_spawn_requests WHERE id=?`, id)
		if err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

func SetFederationSpawnLaunchLabel(agentID, label string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`UPDATE federation_spawn_requests SET launch_label=? WHERE result_agent=? AND status=?`, label, agentID, FedSpawnLaunching)
	return err
}

func FederationSpawnRequestForAgent(agentID string) (*FederationSpawnRequest, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	r, err := scanFedSpawn(d.QueryRow(`SELECT `+fedSpawnColumns+` FROM federation_spawn_requests WHERE result_agent=? AND status=? ORDER BY id DESC LIMIT 1`, agentID, FedSpawnLaunching).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

// ReturnFederationSpawnToPending is used only for a definite failure, or an
// explicit human abandonment acknowledging that a late worker may appear.
func ReturnFederationSpawnToPending(id int64, reason string) (bool, error) {
	return ReturnFederationSpawnAttemptToPending(id, "", reason)
}

func ReturnFederationSpawnAttemptToPending(id int64, agentID, reason string) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	tx, err := d.Begin()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec(`UPDATE federation_spawn_requests SET status=?,reason=?,result_agent='',launch_label='',launch_started_at=NULL,notice_sent=0 WHERE id=? AND status=? AND (?='' OR result_agent=?)`, FedSpawnPending, reason, id, FedSpawnLaunching, agentID, agentID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return false, nil
	}
	if _, err = tx.Exec(`DELETE FROM federation_auto_workers WHERE request_id=?`, id); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func ListFederationSpawnWork() ([]*FederationSpawnRequest, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT `+fedSpawnColumns+` FROM federation_spawn_requests WHERE status=? OR (status=? AND (result_sent=0 OR (automatic=1 AND notice_sent=0))) OR (status=? AND reason!='' AND notice_sent=0) ORDER BY id`, FedSpawnLaunching, FedSpawnApproved, FedSpawnPending)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*FederationSpawnRequest
	for rows.Next() {
		r, err := scanFedSpawn(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func MarkFederationSpawnNoticeSent(id int64) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`UPDATE federation_spawn_requests SET notice_sent=1 WHERE id=?`, id)
	return err
}
func MarkFederationSpawnResultSent(id int64) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`UPDATE federation_spawn_requests SET result_sent=1 WHERE id=? AND status=?`, id, FedSpawnApproved)
	return err
}

func SetFederationSpawnUnconfirmed(id int64, agentID, reason string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`UPDATE federation_spawn_requests SET reason=? WHERE id=? AND status=? AND result_agent=?`, reason, id, FedSpawnLaunching, agentID)
	return err
}

func SetPendingFederationSpawnReason(id int64, reason string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`UPDATE federation_spawn_requests SET reason=?,notice_sent=0 WHERE id=? AND status=?`, reason, id, FedSpawnPending)
	return err
}

func CompleteFederationSpawnAttempt(id int64, agentID string) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	res, err := d.Exec(`UPDATE federation_spawn_requests SET status=?,reason='',decided_at=?,notice_sent=0 WHERE id=? AND status=? AND result_agent=?`, FedSpawnApproved, dbTime(time.Now()), id, FedSpawnLaunching, agentID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}
