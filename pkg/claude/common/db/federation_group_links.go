package db

import (
	"database/sql"
	"errors"
	"time"
)

type FederationGroupLink struct {
	ID              string
	LocalGroupID    int64
	Peer            string
	RemoteGroupID   string
	RemoteGroupName string
	Direction       string
	Mode            string
	LocalState      string
	RemoteState     string
	ProposalID      string
	ItemID          string
	RemoteStateAt   time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

func ListFederationGroupLinks(groupID int64) ([]FederationGroupLink, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT id,local_group_id,peer,remote_group_id,remote_group_name,direction,mode,local_state,remote_state,proposal_id,item_id,remote_state_at,created_at,updated_at FROM federation_group_links WHERE (?=0 OR local_group_id=?) ORDER BY created_at,id`, groupID, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FederationGroupLink{}
	for rows.Next() {
		var l FederationGroupLink
		var r, c, u dbTimestamp
		if err = rows.Scan(&l.ID, &l.LocalGroupID, &l.Peer, &l.RemoteGroupID, &l.RemoteGroupName, &l.Direction, &l.Mode, &l.LocalState, &l.RemoteState, &l.ProposalID, &l.ItemID, &r, &c, &u); err != nil {
			return nil, err
		}
		l.RemoteStateAt, l.CreatedAt, l.UpdatedAt = r.Time(), c.Time(), u.Time()
		out = append(out, l)
	}
	return out, rows.Err()
}
func GetFederationGroupLink(id string) (*FederationGroupLink, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	var gid int64
	err = d.QueryRow(`SELECT local_group_id FROM federation_group_links WHERE id=?`, id).Scan(&gid)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	ls, err := ListFederationGroupLinks(gid)
	if err != nil {
		return nil, err
	}
	for _, l := range ls {
		if l.ID == id {
			return &l, nil
		}
	}
	return nil, nil
}

// RevokeFederationGroupLink preserves the tombstone, so an old acceptance or
// retry can never resurrect this authorization. Notification intent is atomic.
func RevokeFederationGroupLink(id string, e *FederationProposalEvent) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	tx, err := d.Begin()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	r, err := tx.Exec(`UPDATE federation_group_links SET local_state='revoked',updated_at=? WHERE id=? AND local_state!='revoked'`, dbTime(time.Now()), id)
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	if err != nil || n == 0 {
		return false, err
	}
	if e != nil {
		if err = insertProposalEvent(tx, *e); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

func RecordFederationGroupLinkRevoked(id, peer string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`UPDATE federation_group_links SET local_state='revoked',remote_state='revoked',remote_state_at=?,updated_at=? WHERE id=? AND peer=?`, dbTime(time.Now()), dbTime(time.Now()), id, peer)
	return err
}

func FederationMailLink(envelope string) (*FederationGroupLink, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	var id string
	err = d.QueryRow(`SELECT link_id FROM federation_link_mail WHERE envelope_id=?`, envelope).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return GetFederationGroupLink(id)
}
