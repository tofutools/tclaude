package db

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"time"
)

var ErrModelProxyRefused = errors.New("model gateway launch is absent, revoked or replaced")
var ErrModelProxyBudget = errors.New("model gateway daily request or token budget exhausted")

type ModelProxyLaunch struct{ Session, Generation, Reference, BearerHash, Lease string }

// BindModelProxyLaunch never stores the bearer. A generation cannot replace
// its first credential, even after revocation; a new launch needs a new gate.
func BindModelProxyLaunch(session, reference, hash string) error {
	return BindModelProxyLaunchPendingLease(session, reference, hash, "")
}

// BindModelProxyLaunchPendingLease pins a lease before remote activation.
// Pending lease credentials cannot authorize requests until acknowledged.
func BindModelProxyLaunchPendingLease(session, reference, hash, lease string) error {
	decoded, err := hex.DecodeString(hash)
	if err != nil || len(decoded) != sha256.Size || reference == "" {
		return ErrModelProxyRefused
	}
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`INSERT INTO model_proxy_launches(session,generation,reference,bearer_hash,lease,lease_ready)
 SELECT id,exit_callback_generation,?,?,?,? FROM sessions WHERE id=? AND exit_callback_generation<>'' AND status<>'exited'
 ON CONFLICT(session,generation) DO NOTHING`, reference, hash, lease, lease == "", session)
	if err != nil {
		return err
	}
	var gotRef, gotHash, gotLease string
	var revoked bool
	err = d.QueryRow(`SELECT l.reference,l.bearer_hash,l.revoked,l.lease FROM model_proxy_launches l
 JOIN sessions s ON s.id=l.session AND s.exit_callback_generation=l.generation WHERE s.id=?`, session).Scan(&gotRef, &gotHash, &revoked, &gotLease)
	if err != nil || revoked || gotLease != lease || gotRef != reference || subtle.ConstantTimeCompare([]byte(gotHash), []byte(hash)) != 1 {
		return ErrModelProxyRefused
	}
	return nil
}
func VerifyModelProxyLaunch(session, bearer string) (*ModelProxyLaunch, error) {
	if len(bearer) != 64 {
		return nil, ErrModelProxyRefused
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	var l ModelProxyLaunch
	err = d.QueryRow(`SELECT l.session,l.generation,l.reference,l.bearer_hash,l.lease FROM model_proxy_launches l
 JOIN sessions s ON s.id=l.session AND s.exit_callback_generation=l.generation
 WHERE l.session=? AND l.revoked=0 AND l.lease_ready=1 AND s.status<>'exited'`, session).Scan(&l.Session, &l.Generation, &l.Reference, &l.BearerHash, &l.Lease)
	if err != nil {
		return nil, ErrModelProxyRefused
	}
	hash := sha256.Sum256([]byte(bearer))
	expected, e := hex.DecodeString(l.BearerHash)
	if e != nil || subtle.ConstantTimeCompare(expected, hash[:]) != 1 {
		return nil, ErrModelProxyRefused
	}
	return &l, nil
}
func RevokeModelProxyLaunch(session, generation string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`UPDATE model_proxy_launches SET revoked=1 WHERE session=? AND generation=?`, session, generation)
	return err
}

type ModelProxyBudget struct{ Requests, Tokens, PeerRequests, PeerTokens, SessionRequests, SessionTokens int64 }
type ModelProxyUsage struct {
	ID               string `json:"id"`
	Day              string `json:"day"`
	Proxy            string `json:"proxy"`
	Peer             string `json:"peer"`
	Session          string `json:"session"`
	Model            string `json:"model"`
	ChargedTokens    int64  `json:"charged_tokens"`
	InputTokens      int64  `json:"input_tokens"`
	OutputTokens     int64  `json:"output_tokens"`
	CacheReadTokens  int64  `json:"cache_read_tokens"`
	CacheWriteTokens int64  `json:"cache_write_tokens"`
	Status           int    `json:"status"`
	Complete         bool   `json:"complete"`
	RequestBytes     int64  `json:"request_bytes"`
	ResponseBytes    int64  `json:"response_bytes"`
	DurationMS       int64  `json:"duration_ms"`
}

// ReserveModelProxyRequest serializes all three daily scopes in one SQLite
// writer statement. A crash or an incomplete response keeps the reservation;
// no background reconciliation can accidentally refund an uncertain request.
func ReserveModelProxyRequest(u ModelProxyUsage, b ModelProxyBudget) error {
	if u.ChargedTokens <= 0 || b.Requests <= 0 || b.Tokens <= 0 || b.PeerRequests <= 0 || b.PeerTokens <= 0 || b.SessionRequests <= 0 || b.SessionTokens <= 0 {
		return ErrModelProxyBudget
	}
	d, err := Open()
	if err != nil {
		return err
	}
	result, err := d.Exec(`INSERT INTO model_proxy_requests(id,day,proxy,peer,session,model,charged_tokens,request_bytes,started_at)
 SELECT ?,?,?,?,?,?,?,?,? WHERE
 (SELECT COUNT(*) FROM model_proxy_requests WHERE day=? AND proxy=?) < ? AND
 (SELECT COALESCE(SUM(charged_tokens),0) FROM model_proxy_requests WHERE day=? AND proxy=?) <= ? AND
 (SELECT COUNT(*) FROM model_proxy_requests WHERE day=? AND proxy=? AND peer=?) < ? AND
 (SELECT COALESCE(SUM(charged_tokens),0) FROM model_proxy_requests WHERE day=? AND proxy=? AND peer=?) <= ? AND
 (SELECT COUNT(*) FROM model_proxy_requests WHERE day=? AND proxy=? AND peer=? AND session=?) < ? AND
 (SELECT COALESCE(SUM(charged_tokens),0) FROM model_proxy_requests WHERE day=? AND proxy=? AND peer=? AND session=?) <= ?`,
		u.ID, u.Day, u.Proxy, u.Peer, u.Session, u.Model, u.ChargedTokens, u.RequestBytes, dbTime(time.Now()),
		u.Day, u.Proxy, b.Requests, u.Day, u.Proxy, b.Tokens-u.ChargedTokens,
		u.Day, u.Proxy, u.Peer, b.PeerRequests, u.Day, u.Proxy, u.Peer, b.PeerTokens-u.ChargedTokens,
		u.Day, u.Proxy, u.Peer, u.Session, b.SessionRequests, u.Day, u.Proxy, u.Peer, u.Session, b.SessionTokens-u.ChargedTokens)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrModelProxyBudget
	}
	return nil
}
func FinishModelProxyRequest(u ModelProxyUsage) error {
	d, err := Open()
	if err != nil {
		return err
	}
	actual := u.InputTokens + u.OutputTokens + u.CacheReadTokens + u.CacheWriteTokens
	// A successful terminal usage replaces the reservation once. Incomplete
	// requests keep their charge; usage metadata can still aid the operator.
	_, err = d.Exec(`UPDATE model_proxy_requests SET charged_tokens=CASE WHEN ? THEN ? ELSE MAX(charged_tokens,?) END,
 input_tokens=?,output_tokens=?,cache_read_tokens=?,cache_write_tokens=?,status=?,complete=?,response_bytes=?,duration_ms=?
 WHERE id=? AND complete=0`, u.Complete, actual, actual, u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens, u.Status, u.Complete, u.ResponseBytes, u.DurationMS, u.ID)
	return err
}
func ListModelProxyUsage(day string) ([]ModelProxyUsage, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT id,day,proxy,peer,session,model,charged_tokens,input_tokens,output_tokens,cache_read_tokens,cache_write_tokens,status,complete,request_bytes,response_bytes,duration_ms FROM model_proxy_requests WHERE day=? ORDER BY started_at,id LIMIT 10000`, day)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ModelProxyUsage{}
	for rows.Next() {
		var u ModelProxyUsage
		err = rows.Scan(&u.ID, &u.Day, &u.Proxy, &u.Peer, &u.Session, &u.Model, &u.ChargedTokens, &u.InputTokens, &u.OutputTokens, &u.CacheReadTokens, &u.CacheWriteTokens, &u.Status, &u.Complete, &u.RequestBytes, &u.ResponseBytes, &u.DurationMS)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func SetModelProxyLaunchLease(session, generation, lease string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	result, err := d.Exec(`UPDATE model_proxy_launches SET lease_ready=1 WHERE session=? AND generation=? AND revoked=0 AND lease=?`, session, generation, lease)
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
