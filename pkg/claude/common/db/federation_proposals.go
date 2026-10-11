package db

import (
	"database/sql"
	"errors"
	"time"
)

// FederationGroupIdentity is stable through rename and is never reused after
// deletion. Lazy creation covers groups made after the schema migration.
func FederationGroupIdentity(groupID int64) (string, error) {
	d, err := Open()
	if err != nil {
		return "", err
	}
	_, err = d.Exec(`INSERT INTO federation_group_identities(group_id,opaque_id)
 SELECT id,'grp_'||lower(hex(randomblob(16))) FROM agent_groups WHERE id=?
 ON CONFLICT(group_id) DO NOTHING`, groupID)
	if err != nil {
		return "", err
	}
	var id string
	err = d.QueryRow(`SELECT opaque_id FROM federation_group_identities WHERE group_id=?`, groupID).Scan(&id)
	return id, err
}
func FederationGroupByIdentity(id string) (*AgentGroup, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	var group int64
	err = d.QueryRow(`SELECT group_id FROM federation_group_identities WHERE opaque_id=?`, id).Scan(&group)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return GetAgentGroupByID(group)
}

type FederationPeerProposal struct {
	ID        string
	Peer      string
	Origin    string
	Revision  int
	Body      string
	Authority string
	State     string
	CreatedAt time.Time
	Deadline  time.Time
	UpdatedAt time.Time
}

type FederationProposalEvent struct {
	ID         string
	ProposalID string
	Peer       string
	Kind       string
	Payload    string
	ExpiresAt  time.Time
}

// SaveFederationProposal commits the version transition and its outbound
// intent together. The worker can seal/requeue an intent after a crash; no
// authority change depends on a best-effort network send.
func SaveFederationProposal(p FederationPeerProposal, expectedRevision int, event *FederationProposalEvent, links ...FederationGroupLink) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	tx, err := d.Begin()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	var result sql.Result
	if expectedRevision == 0 {
		var pending int
		if err = tx.QueryRow(`SELECT count(*) FROM federation_peer_proposals WHERE peer=? AND deadline_at>? AND state IN ('sent','countered','applying')`, p.Peer, dbTime(time.Now())).Scan(&pending); err != nil {
			return false, err
		}
		if pending >= 100 {
			return false, errors.New("peer already has 100 pending proposals")
		}
		result, err = tx.Exec(`INSERT INTO federation_peer_proposals(id,peer,origin_instance,revision,body,authority,state,created_at,deadline_at,updated_at)
 VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO NOTHING`, p.ID, p.Peer, p.Origin, p.Revision, p.Body, p.Authority, p.State, dbTime(p.CreatedAt), dbTime(p.Deadline), dbTime(p.UpdatedAt))
	} else {
		result, err = tx.Exec(`UPDATE federation_peer_proposals SET revision=?,body=?,authority=?,decision='{}',state=?,updated_at=? WHERE id=? AND peer=? AND revision=? AND deadline_at>? AND state IN ('sent','countered')`, p.Revision, p.Body, p.Authority, p.State, dbTime(p.UpdatedAt), p.ID, p.Peer, expectedRevision, dbTime(time.Now()))
	}
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	if err != nil || n == 0 {
		return false, err
	}
	for _, l := range links {
		_, err = tx.Exec(`INSERT INTO federation_group_links(id,local_group_id,peer,remote_group_id,remote_group_name,direction,mode,local_state,remote_state,proposal_id,item_id,created_at,updated_at)
 VALUES(?,?,?,?,?,?,?,'pending','unknown',?,?,?,?) ON CONFLICT(proposal_id,item_id) DO UPDATE SET local_group_id=excluded.local_group_id,remote_group_id=excluded.remote_group_id,remote_group_name=excluded.remote_group_name,direction=excluded.direction,mode=excluded.mode,remote_state='unknown',remote_state_at=NULL,updated_at=excluded.updated_at WHERE federation_group_links.local_state='pending'`, l.ID, l.LocalGroupID, p.Peer, l.RemoteGroupID, l.RemoteGroupName, l.Direction, l.Mode, p.ID, l.ItemID, dbTime(p.CreatedAt), dbTime(p.UpdatedAt))
		if err != nil {
			return false, err
		}
	}
	if event != nil {
		if err = insertProposalEvent(tx, *event); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}
func insertProposalEvent(tx *sql.Tx, e FederationProposalEvent) error {
	_, err := tx.Exec(`INSERT INTO federation_proposal_events(id,proposal_id,peer,kind,payload,expires_at) VALUES(?,?,?,?,?,?) ON CONFLICT(id) DO NOTHING`, e.ID, e.ProposalID, e.Peer, e.Kind, e.Payload, dbTime(e.ExpiresAt))
	return err
}
func GetFederationProposal(id string) (*FederationPeerProposal, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	var p FederationPeerProposal
	var c, deadline, u dbTimestamp
	err = d.QueryRow(`SELECT id,peer,origin_instance,revision,body,authority,state,created_at,deadline_at,updated_at FROM federation_peer_proposals WHERE id=?`, id).Scan(&p.ID, &p.Peer, &p.Origin, &p.Revision, &p.Body, &p.Authority, &p.State, &c, &deadline, &u)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p.CreatedAt, p.Deadline, p.UpdatedAt = c.Time(), deadline.Time(), u.Time()
	return &p, nil
}
func ListFederationProposals() ([]FederationPeerProposal, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT id,peer,origin_instance,revision,body,authority,state,created_at,deadline_at,updated_at FROM federation_peer_proposals ORDER BY updated_at DESC LIMIT 200`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FederationPeerProposal{}
	for rows.Next() {
		var p FederationPeerProposal
		var c, deadline, u dbTimestamp
		if err = rows.Scan(&p.ID, &p.Peer, &p.Origin, &p.Revision, &p.Body, &p.Authority, &p.State, &c, &deadline, &u); err != nil {
			return nil, err
		}
		p.CreatedAt, p.Deadline, p.UpdatedAt = c.Time(), deadline.Time(), u.Time()
		out = append(out, p)
	}
	return out, rows.Err()
}
func PendingFederationProposalEvents(now time.Time) ([]FederationProposalEvent, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT id,proposal_id,peer,kind,payload,expires_at FROM federation_proposal_events WHERE queued=0 AND expires_at>? LIMIT 50`, dbTime(now))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FederationProposalEvent{}
	for rows.Next() {
		var e FederationProposalEvent
		var expires dbTimestamp
		if err = rows.Scan(&e.ID, &e.ProposalID, &e.Peer, &e.Kind, &e.Payload, &expires); err != nil {
			return nil, err
		}
		e.ExpiresAt = expires.Time()
		out = append(out, e)
	}
	return out, rows.Err()
}
func MarkFederationProposalEventQueued(id string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`UPDATE federation_proposal_events SET queued=1 WHERE id=?`, id)
	return err
}
func ExpireFederationProposals(now time.Time) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`UPDATE federation_peer_proposals SET state='expired',updated_at=? WHERE deadline_at<=? AND state IN ('sent','countered','applying','pending')`, dbTime(now), dbTime(now))
	return err
}

// ProposalLocalChange is constructed only from a verified proposal item and
// this node's explicit decision (or standing exact-ingress acceptance grant).
type ProposalLocalChange struct {
	ItemID         string
	GroupID        int64
	IncludeMembers bool
	FutureLinks    bool
	Share          bool
	Link           *FederationGroupLink
}

// ApplyFederationProposal atomically pins the decision, applies local-only
// authorizations and records an outbound receipt intent. Duplicate receipts
// are harmless; revoked link tombstones never become active again.
func ApplyFederationProposal(id, peer string, revision int, decision, state string, changes []ProposalLocalChange, event FederationProposalEvent) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	tx, err := d.Begin()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	var storedRevision int
	var storedPeer, storedState string
	var deadline dbTimestamp
	err = tx.QueryRow(`SELECT revision,peer,state,deadline_at FROM federation_peer_proposals WHERE id=?`, id).Scan(&storedRevision, &storedPeer, &storedState, &deadline)
	if err != nil {
		return false, err
	}
	if storedRevision != revision || storedPeer != peer || storedState == "declined" || storedState == "withdrawn" || storedState == "expired" || !deadline.Time().After(time.Now()) {
		return false, nil
	}
	var level string
	if err = tx.QueryRow(`SELECT trust_level FROM federation_peers WHERE instance_id=?`, peer).Scan(&level); err != nil {
		return false, err
	}
	if level != FederationTrustRestricted && level != FederationTrustUnrestricted {
		return false, nil
	}
	for _, change := range changes {
		var archived dbTimestamp
		if err = tx.QueryRow(`SELECT archived_at FROM agent_groups WHERE id=?`, change.GroupID).Scan(&archived); err != nil {
			return false, err
		}
		if !archived.Time().IsZero() {
			return false, nil
		}
		var applied int
		err = tx.QueryRow(`SELECT applied_here FROM federation_proposal_item_decisions WHERE proposal_id=? AND revision=? AND item_id=?`, id, revision, change.ItemID).Scan(&applied)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return false, err
		}
		if applied != 0 {
			continue
		}
		if change.FutureLinks {
			_, err = tx.Exec(`INSERT INTO federation_peer_grants(peer,slug,scope,spawn_policy,created_at) VALUES(?,'groups.link.accept',?,'{}',?) ON CONFLICT(peer,slug,scope) DO NOTHING`, peer, FederationGroupScope(change.GroupID), dbTime(time.Now()))
			if err != nil {
				return false, err
			}
		}
		if change.Share {
			policy := `{"include_members":false}`
			if change.IncludeMembers {
				policy = `{"include_members":true}`
			}
			_, err = tx.Exec(`INSERT INTO federation_peer_grants(peer,slug,scope,spawn_policy,created_at) VALUES(?,'groups.visibility.read',?,?,?)
 ON CONFLICT(peer,slug,scope) DO UPDATE SET spawn_policy=CASE WHEN json_extract(federation_peer_grants.spawn_policy,'$.include_members')=1 THEN federation_peer_grants.spawn_policy ELSE excluded.spawn_policy END`, peer, FederationGroupScope(change.GroupID), policy, dbTime(time.Now()))
			if err != nil {
				return false, err
			}
		}
		if l := change.Link; l != nil {
			var existing string
			err = tx.QueryRow(`SELECT local_state FROM federation_group_links WHERE proposal_id=? AND item_id=?`, id, change.ItemID).Scan(&existing)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return false, err
			}
			if existing == "revoked" {
				return false, nil
			}
			_, err = tx.Exec(`INSERT INTO federation_group_links(id,local_group_id,peer,remote_group_id,remote_group_name,direction,mode,local_state,remote_state,proposal_id,item_id,created_at,updated_at)
 VALUES(?,?,?,?,?,?,?,'active','unknown',?,?,?,?) ON CONFLICT(proposal_id,item_id) DO UPDATE SET local_group_id=excluded.local_group_id,remote_group_id=excluded.remote_group_id,remote_group_name=excluded.remote_group_name,direction=excluded.direction,mode=excluded.mode,local_state='active',updated_at=excluded.updated_at WHERE federation_group_links.local_state!='revoked'`, l.ID, change.GroupID, peer, l.RemoteGroupID, l.RemoteGroupName, l.Direction, l.Mode, id, change.ItemID, dbTime(time.Now()), dbTime(time.Now()))
			if err != nil {
				return false, err
			}
		}
		_, err = tx.Exec(`INSERT INTO federation_proposal_item_decisions(proposal_id,revision,item_id,accepted,applied_here) VALUES(?,?,?,1,1)
 ON CONFLICT(proposal_id,revision,item_id) DO UPDATE SET accepted=1,applied_here=1`, id, revision, change.ItemID)
		if err != nil {
			return false, err
		}
	}
	_, err = tx.Exec(`UPDATE federation_peer_proposals SET decision=CASE WHEN ?='{}' THEN decision ELSE ? END,state=CASE WHEN ?='pending_items' THEN state ELSE ? END,updated_at=? WHERE id=?`, decision, decision, state, state, dbTime(time.Now()), id)
	if err != nil {
		return false, err
	}
	if err = insertProposalEvent(tx, event); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func FederationProposalDecision(id string) (string, error) {
	d, err := Open()
	if err != nil {
		return "", err
	}
	var raw string
	err = d.QueryRow(`SELECT decision FROM federation_peer_proposals WHERE id=?`, id).Scan(&raw)
	return raw, err
}

// RecordFederationProposalReceipt records peer-reported application, never
// grants local authority. Receipt hashes/selected IDs are checked by agentd.
func RecordFederationProposalReceipt(id, peer string, revision int, itemIDs []string, now time.Time) error {
	d, err := Open()
	if err != nil {
		return err
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var rev int
	var p string
	if err = tx.QueryRow(`SELECT revision,peer FROM federation_peer_proposals WHERE id=?`, id).Scan(&rev, &p); err != nil {
		return err
	}
	if rev != revision || p != peer {
		return errors.New("stale proposal receipt")
	}
	for _, item := range itemIDs {
		_, err = tx.Exec(`INSERT INTO federation_proposal_item_decisions(proposal_id,revision,item_id,accepted,applied_remote) VALUES(?,?,?,1,1)
 ON CONFLICT(proposal_id,revision,item_id) DO UPDATE SET applied_remote=1`, id, revision, item)
		if err != nil {
			return err
		}
		_, err = tx.Exec(`UPDATE federation_group_links SET remote_state='active',remote_state_at=?,updated_at=? WHERE proposal_id=? AND item_id=? AND peer=? AND local_state!='revoked'`, dbTime(now), dbTime(now), id, item, peer)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}
func FederationProposalApplied(id string, revision int) (here, remote []string, err error) {
	d, err := Open()
	if err != nil {
		return nil, nil, err
	}
	rows, err := d.Query(`SELECT item_id,applied_here,applied_remote FROM federation_proposal_item_decisions WHERE proposal_id=? AND revision=?`, id, revision)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var item string
		var h, r int
		if err = rows.Scan(&item, &h, &r); err != nil {
			return nil, nil, err
		}
		if h != 0 {
			here = append(here, item)
		}
		if r != 0 {
			remote = append(remote, item)
		}
	}
	return here, remote, rows.Err()
}

// CompleteFederationProposal updates only presentation, after the daemon has
// checked each required per-item receipt. It does not create authorization.
func CompleteFederationProposal(id string, revision int, state string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`UPDATE federation_peer_proposals SET state=?,updated_at=? WHERE id=? AND revision=? AND state='applying'`, state, dbTime(time.Now()), id, revision)
	return err
}

// Withdrawal cancels undecided items. Already applied items remain explicit
// grants/edges and can be revoked separately; it never revokes an older grant.
func WithdrawFederationProposal(id, peer string, revision int, event *FederationProposalEvent) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	tx, err := d.Begin()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.Exec(`UPDATE federation_peer_proposals SET state='withdrawn',updated_at=? WHERE id=? AND peer=? AND revision=? AND state IN ('sent','countered') AND deadline_at>?`, dbTime(time.Now()), id, peer, revision, dbTime(time.Now()))
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	if err != nil || n == 0 {
		return false, err
	}
	if _, err = tx.Exec(`UPDATE federation_group_links SET local_state='revoked',updated_at=? WHERE proposal_id=? AND local_state='pending'`, dbTime(time.Now()), id); err != nil {
		return false, err
	}
	if event != nil {
		if err = insertProposalEvent(tx, *event); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}
