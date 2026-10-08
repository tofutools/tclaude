package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/tofutools/tclaude/pkg/federation/bundletransfer"
)

type FederationBundleOffer struct {
	Descriptor   bundletransfer.Descriptor `json:"offer"`
	Peer         string                    `json:"peer"`
	Direction    string                    `json:"direction"`
	State        string                    `json:"state"`
	LastError    string                    `json:"last_error,omitempty"`
	GroupID      int64                     `json:"group_id,omitempty"`
	ImportLabel  string                    `json:"import_label,omitempty"`
	ImportAgent  string                    `json:"import_agent,omitempty"`
	SenderAgent  string                    `json:"sender_agent,omitempty"`
	ResultQueued bool                      `json:"-"`
	CreatedAt    time.Time                 `json:"created_at"`
}

var ErrOfferQuota = errors.New("peer bundle offer quota exceeded")

func InsertFederationBundleOffer(o FederationBundleOffer, kind bundletransfer.Type) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	tx, err := d.Begin()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	// Acquire the write lock before testing quotas, including across processes.
	_, err = tx.Exec(`UPDATE federation_bundle_offers SET state=state WHERE 0`)
	if err != nil {
		return false, err
	}
	var exists int
	err = tx.QueryRow(`SELECT count(*) FROM federation_bundle_offers WHERE direction=? AND peer=? AND id=?`, o.Direction, o.Peer, o.Descriptor.ID).Scan(&exists)
	if err != nil {
		return false, err
	}
	if exists > 0 {
		return false, nil
	}
	var count, total int64
	err = tx.QueryRow(`SELECT count(*),coalesce(sum(bytes),0) FROM federation_bundle_offers WHERE direction=? AND peer=? AND state IN ('pending','ready') AND kind=? AND expires_at>?`, o.Direction, o.Peer, kind.Name, dbTime(time.Now())).Scan(&count, &total)
	if err != nil {
		return false, err
	}
	if count >= int64(kind.PendingLimit) || total+o.Descriptor.Bytes > kind.PendingBytes {
		return false, ErrOfferQuota
	}
	descriptor := o.Descriptor
	descriptor.Inline = nil
	raw, err := json.Marshal(descriptor)
	if err != nil {
		return false, err
	}
	_, err = tx.Exec(`INSERT INTO federation_bundle_offers(id,peer,direction,kind,descriptor,bytes,state,last_error,created_at,expires_at,group_id,sender_agent) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, descriptor.ID, o.Peer, o.Direction, descriptor.Type, string(raw), descriptor.Bytes, o.State, o.LastError, dbTime(time.Now()), dbTime(descriptor.ExpiresAt), o.GroupID, o.SenderAgent)
	if err != nil {
		return false, err
	}
	return true, tx.Commit()
}
func scanFederationBundleOffer(row rowScanner) (*FederationBundleOffer, error) {
	var o FederationBundleOffer
	var raw string
	var created dbTimestamp
	err := row.Scan(&raw, &o.Peer, &o.Direction, &o.State, &o.LastError, &o.ResultQueued, &o.GroupID, &o.SenderAgent, &o.ImportAgent, &o.ImportLabel, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal([]byte(raw), &o.Descriptor); err != nil {
		return nil, err
	}
	o.CreatedAt = created.Time()
	return &o, nil
}
func GetFederationBundleOffer(direction, peer, id string) (*FederationBundleOffer, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	return scanFederationBundleOffer(d.QueryRow(`SELECT descriptor,peer,direction,state,last_error,result_queued,group_id,sender_agent,import_agent,import_label,created_at FROM federation_bundle_offers WHERE direction=? AND peer=? AND id=?`, direction, peer, id))
}
func ListFederationBundleOffers(direction string) ([]FederationBundleOffer, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT descriptor,peer,direction,state,last_error,result_queued,group_id,sender_agent,import_agent,import_label,created_at FROM federation_bundle_offers WHERE (?='' OR direction=?) ORDER BY created_at DESC`, direction, direction)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FederationBundleOffer{}
	for rows.Next() {
		o, err := scanFederationBundleOffer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *o)
	}
	return out, rows.Err()
}
func SetFederationBundleOfferState(direction, peer, id, state, detail string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`UPDATE federation_bundle_offers SET state=?,last_error=? WHERE direction=? AND peer=? AND id=? AND state IN ('pending','ready')`, state, detail, direction, peer, id)
	return err
}
func DeleteFederationBundleOffer(direction, peer, id string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`DELETE FROM federation_bundle_offers WHERE direction=? AND peer=? AND id=?`, direction, peer, id)
	return err
}

func MarkFederationBundleResultQueued(peer, id string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`UPDATE federation_bundle_offers SET result_queued=1 WHERE direction='in' AND peer=? AND id=?`, peer, id)
	return err
}

// Erase settled inline payloads while retaining operator-visible delivery metadata.
func EraseSettledFederationBundlePayload(id string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`UPDATE federation_outbox SET sealed=x'' WHERE envelope_id=? AND kind='bundle_offer' AND state NOT IN ('queued','sent')`, id)
	return err
}

func ReserveFederationBundleImport(peer, id, agentID string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	res, err := d.Exec(`UPDATE federation_bundle_offers SET import_agent=? WHERE direction='in' AND peer=? AND id=? AND state='ready' AND import_agent=''`, agentID, peer, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("offer already has a reserved launch")
	}
	return nil
}

func SetFederationBundleLaunchLabel(agentID, label string) error {
	if agentID == "" {
		return nil
	}
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`UPDATE federation_bundle_offers SET import_label=? WHERE direction='in' AND import_agent=? AND state='ready'`, label, agentID)
	return err
}

// ClearUnlaunchedFederationBundleLabel permits retry only when preparation
// proved that the subprocess boundary was never crossed. Match the label so a
// stale failure cannot clear a later launch attempt.
func ClearUnlaunchedFederationBundleLabel(agentID, label string) error {
	if agentID == "" || label == "" {
		return nil
	}
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`UPDATE federation_bundle_offers SET import_label='' WHERE direction='in' AND import_agent=? AND import_label=? AND state='ready'`, agentID, label)
	return err
}

func ReleaseUnlaunchedFederationBundleImport(peer, id, agentID string) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	res, err := d.Exec(`UPDATE federation_bundle_offers SET import_agent='',last_error='' WHERE direction='in' AND peer=? AND id=? AND import_agent=? AND import_label='' AND state='ready'`, peer, id, agentID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}
