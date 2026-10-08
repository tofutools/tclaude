package db

import (
	"database/sql"
	"errors"
	"time"
)

type ModelProxyLease struct {
	ID          string    `json:"id"`
	Peer        string    `json:"peer"`
	Request     string    `json:"request"`
	Kind        string    `json:"kind"`
	Proxy       string    `json:"proxy"`
	Worker      string    `json:"worker"`
	Session     string    `json:"session"`
	Generation  string    `json:"generation"`
	Revoked     bool      `json:"revoked"`
	IdleSeconds int64     `json:"idle_seconds"`
	TouchedAt   time.Time `json:"touched_at"`
}

type ModelProxyWorkerLease struct {
	Worker  string `json:"worker"`
	Gateway string `json:"gateway"`
	Lease   string `json:"lease"`
	Request string `json:"request"`
	Kind    string `json:"kind"`
	Proxy   string `json:"proxy"`
}

func IssueModelProxyLease(l ModelProxyLease) error {
	if l.ID == "" || l.Peer == "" || l.Request == "" || l.Proxy == "" || l.IdleSeconds <= 0 {
		return ErrModelProxyRefused
	}
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`INSERT INTO model_proxy_leases(id,peer,request,kind,proxy,idle_seconds,touched_at) VALUES(?,?,?,?,?,?,?)`, l.ID, l.Peer, l.Request, l.Kind, l.Proxy, l.IdleSeconds, dbTime(time.Now()))
	return err
}
func GetModelProxyLease(id string) (*ModelProxyLease, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	var l ModelProxyLease
	var touched dbTimestamp
	err = d.QueryRow(`SELECT id,peer,request,kind,proxy,worker,session,generation,revoked,idle_seconds,touched_at FROM model_proxy_leases WHERE id=?`, id).Scan(&l.ID, &l.Peer, &l.Request, &l.Kind, &l.Proxy, &l.Worker, &l.Session, &l.Generation, &l.Revoked, &l.IdleSeconds, &touched)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	l.TouchedAt = touched.Time()
	return &l, nil
}

// Activation may bind a pending lease once; retries cannot replace its worker.
func ActivateModelProxyLease(l ModelProxyLease) error {
	if l.Worker == "" || l.Session == "" || l.Generation == "" {
		return ErrModelProxyRefused
	}
	d, err := Open()
	if err != nil {
		return err
	}
	now := dbTime(time.Now())
	result, err := d.Exec(`UPDATE model_proxy_leases SET worker=?,session=?,generation=?
 WHERE id=? AND peer=? AND request=? AND kind=? AND proxy=? AND revoked=0
 AND touched_at + idle_seconds*1000000000 > ?
 AND ((worker='' AND session='' AND generation='') OR (worker=? AND session=? AND generation=?))`, l.Worker, l.Session, l.Generation, l.ID, l.Peer, l.Request, l.Kind, l.Proxy, now, l.Worker, l.Session, l.Generation)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrModelProxyRefused
	}
	return nil
}
func CheckModelProxyLease(id, peer, proxy, session, generation string, touch bool) error {
	d, err := Open()
	if err != nil {
		return err
	}
	now := dbTime(time.Now())
	if touch {
		result, err := d.Exec(`UPDATE model_proxy_leases SET touched_at=? WHERE id=? AND peer=? AND proxy=? AND session=? AND generation=? AND worker<>'' AND revoked=0 AND touched_at+idle_seconds*1000000000>?`, now, id, peer, proxy, session, generation, now)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return ErrModelProxyRefused
		}
		return nil
	}
	var count int
	err = d.QueryRow(`SELECT COUNT(*) FROM model_proxy_leases WHERE id=? AND peer=? AND proxy=? AND session=? AND generation=? AND worker<>'' AND revoked=0 AND touched_at+idle_seconds*1000000000>?`, id, peer, proxy, session, generation, now).Scan(&count)
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrModelProxyRefused
	}
	return nil
}
func RevokeModelProxyLease(id, peer string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`UPDATE model_proxy_leases SET revoked=1 WHERE id=? AND peer=?`, id, peer)
	return err
}
func RecordModelProxyWorkerLease(l ModelProxyWorkerLease) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`INSERT INTO model_proxy_worker_leases(worker,gateway,lease,request,kind,proxy) VALUES(?,?,?,?,?,?) ON CONFLICT(worker) DO NOTHING`, l.Worker, l.Gateway, l.Lease, l.Request, l.Kind, l.Proxy)
	if err != nil {
		return err
	}
	got, err := GetModelProxyWorkerLease(l.Worker)
	if err != nil {
		return err
	}
	if got == nil || *got != l {
		return ErrModelProxyRefused
	}
	return nil
}
func GetModelProxyWorkerLease(worker string) (*ModelProxyWorkerLease, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	var l ModelProxyWorkerLease
	err = d.QueryRow(`SELECT worker,gateway,lease,request,kind,proxy FROM model_proxy_worker_leases WHERE worker=?`, worker).Scan(&l.Worker, &l.Gateway, &l.Lease, &l.Request, &l.Kind, &l.Proxy)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &l, err
}

func RevokeModelProxyLeaseSelection(proxy, peer string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`UPDATE model_proxy_leases SET revoked=1 WHERE (?='' OR proxy=?) AND (?='' OR peer=?)`, proxy, proxy, peer, peer)
	return err
}

// Completed receiver bindings remain durable until the gateway acknowledges
// revocation; a disconnect or daemon restart cannot forget the close notice.
func StaleModelProxyWorkerLeases() ([]ModelProxyWorkerLease, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT DISTINCT w.worker,w.gateway,w.lease,w.request,w.kind,w.proxy
 FROM model_proxy_worker_leases w JOIN model_proxy_launches l ON l.lease=w.lease
 LEFT JOIN sessions s ON s.id=l.session
 LEFT JOIN agents a ON a.agent_id=w.worker
 WHERE l.revoked=1 OR s.id IS NULL OR s.status='exited'
 OR s.exit_callback_generation<>l.generation OR a.retired_at IS NOT NULL OR (s.conv_id<>'' AND a.current_conv_id<>s.conv_id)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var all []ModelProxyWorkerLease
	for rows.Next() {
		var l ModelProxyWorkerLease
		if err = rows.Scan(&l.Worker, &l.Gateway, &l.Lease, &l.Request, &l.Kind, &l.Proxy); err != nil {
			return nil, err
		}
		all = append(all, l)
	}
	return all, rows.Err()
}

func ForgetClosedModelProxyWorkerLease(lease, gateway string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`DELETE FROM model_proxy_worker_leases WHERE lease=? AND gateway=? AND EXISTS (
 SELECT 1 FROM model_proxy_launches l LEFT JOIN sessions s ON s.id=l.session
 LEFT JOIN agents a ON a.agent_id=model_proxy_worker_leases.worker
 WHERE l.lease=model_proxy_worker_leases.lease AND (l.revoked=1 OR s.id IS NULL OR s.status='exited' OR s.exit_callback_generation<>l.generation OR a.retired_at IS NOT NULL OR (s.conv_id<>'' AND a.current_conv_id<>s.conv_id)))`, lease, gateway)
	return err
}

func ListModelProxyLeases() ([]ModelProxyLease, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT id FROM model_proxy_leases ORDER BY touched_at DESC LIMIT 1000`)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	all := []ModelProxyLease{}
	for _, id := range ids {
		l, err := GetModelProxyLease(id)
		if err != nil {
			return nil, err
		}
		if l != nil {
			all = append(all, *l)
		}
	}
	return all, nil
}
