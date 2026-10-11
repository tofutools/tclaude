package db

import (
	"database/sql"
	"errors"
	"time"
)

var ErrFederationMailMoving = errors.New("agent mailbox is fenced for a departure; retry after the move")

type FederationAgentLocation struct {
	AgentID         string    `json:"agent_id"`
	HomeInstance    string    `json:"home_instance"`
	CurrentInstance string    `json:"current_instance"`
	Epoch           string    `json:"-"`
	HopCount        int       `json:"hop_count"`
	ArrivalOffer    string    `json:"arrival_offer"`
	UpdatedAt       time.Time `json:"updated_at"`
}

func GetFederationAgentLocation(id string) (*FederationAgentLocation, error) {
	d, e := Open()
	if e != nil {
		return nil, e
	}
	var p FederationAgentLocation
	var updated dbTimestamp
	e = d.QueryRow(`SELECT agent_id,home_instance,current_instance,epoch,hop_count,arrival_offer,updated_at FROM federation_agent_locations WHERE agent_id=?`, id).Scan(&p.AgentID, &p.HomeInstance, &p.CurrentInstance, &p.Epoch, &p.HopCount, &p.ArrivalOffer, &updated)
	if errors.Is(e, sql.ErrNoRows) {
		return nil, nil
	}
	p.UpdatedAt = updated.Time()
	return &p, e
}

// UpdateFederationAgentLocation accepts only the live home continuation. Epoch
// is the nonce hash, never the private credential. Stale and forked counters
// cannot replace a newer confirmed host.
func UpdateFederationAgentLocation(id, home, host, nonce, offer string, hops int) error {
	d, e := Open()
	if e != nil {
		return e
	}
	tx, e := d.Begin()
	if e != nil {
		return e
	}
	defer func() { _ = tx.Rollback() }()
	if _, e = tx.Exec(`UPDATE agent_federation_presence SET updated_at=updated_at WHERE agent_id=?`, id); e != nil {
		return e
	}
	p, e := scanFederationPresence(tx.QueryRow(`SELECT `+federationPresenceColumns+` FROM agent_federation_presence WHERE agent_id=?`, id))
	if e != nil {
		return e
	}
	if p == nil || p.HomeInstance != home || p.State != "away" || nonce == "" || p.ContinuationNonceHash != continuationHash(nonce) || hops < p.HopCount || host == "" || offer == "" {
		return errors.New("location update has no live home continuation")
	}
	epoch := p.ContinuationNonceHash
	var oldHost, oldOffer, oldEpoch string
	var oldHops int
	e = tx.QueryRow(`SELECT current_instance,arrival_offer,epoch,hop_count FROM federation_agent_locations WHERE agent_id=?`, id).Scan(&oldHost, &oldOffer, &oldEpoch, &oldHops)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return e
	}
	if e == nil && oldEpoch == epoch && (hops < oldHops || hops == oldHops && (host != oldHost || offer != oldOffer)) {
		return errors.New("stale or conflicting location update")
	}
	_, e = tx.Exec(`INSERT INTO federation_agent_locations(agent_id,home_instance,current_instance,epoch,hop_count,arrival_offer,updated_at) VALUES(?,?,?,?,?,?,?) ON CONFLICT(agent_id) DO UPDATE SET home_instance=excluded.home_instance,current_instance=excluded.current_instance,epoch=excluded.epoch,hop_count=excluded.hop_count,arrival_offer=excluded.arrival_offer,updated_at=excluded.updated_at`, id, home, host, epoch, hops, offer, dbTime(time.Now()))
	if e != nil {
		return e
	}
	return tx.Commit()
}

// FederationMailDestination never confuses a previous trip's route with the
// current departure. The first-hop peer remains the fallback until confirmation.
func FederationMailDestination(id, local string) (string, error) {
	p, e := GetAgentFederationPresence(id)
	if e != nil || p == nil {
		return "", e
	}
	if p.State != "away" {
		return "", nil
	}
	if p.HomeInstance != local {
		return p.HomeInstance, nil
	}
	l, e := GetFederationAgentLocation(id)
	if e != nil {
		return "", e
	}
	if l != nil && l.Epoch == p.ContinuationNonceHash {
		return l.CurrentInstance, nil
	}
	return p.CurrentPeer, nil
}

type FederationMailDelivery struct {
	Sender    string    `json:"sender"`
	Envelope  string    `json:"envelope"`
	ExpiresAt time.Time `json:"expires_at"`
}

func FederationMailDelivered(agent, sender, id string) (bool, error) {
	d, e := Open()
	if e != nil {
		return false, e
	}
	var n int
	e = d.QueryRow(`SELECT COUNT(*) FROM federation_agent_mail_deliveries WHERE agent_id=? AND sender_instance=? AND envelope_id=?`, agent, sender, id).Scan(&n)
	return n > 0, e
}

// FenceFederationMail atomically freezes inbound dispatch before taking the
// delivery ledger snapshot. It expires with the offer and is released on failure.
func FenceFederationMail(agent, token string, expiry time.Time) error {
	d, e := Open()
	if e != nil {
		return e
	}
	_, e = d.Exec(`INSERT INTO federation_agent_mail_fences(agent_id,token,expires_at) VALUES(?,?,?) ON CONFLICT(agent_id) DO UPDATE SET token=excluded.token,offer='',expires_at=excluded.expires_at WHERE federation_agent_mail_fences.expires_at<?`, agent, token, dbTime(expiry), dbTime(time.Now()))
	if e != nil {
		return e
	}
	var got string
	e = d.QueryRow(`SELECT token FROM federation_agent_mail_fences WHERE agent_id=?`, agent).Scan(&got)
	if e == nil && got != token {
		return ErrFederationMailMoving
	}
	return e
}
func BindFederationMailFence(agent, token, offer string) error {
	d, e := Open()
	if e != nil {
		return e
	}
	_, e = d.Exec(`UPDATE federation_agent_mail_fences SET offer=? WHERE agent_id=? AND token=?`, offer, agent, token)
	return e
}
func ReleaseFederationMailFence(agent, token string) error {
	d, e := Open()
	if e != nil {
		return e
	}
	_, e = d.Exec(`DELETE FROM federation_agent_mail_fences WHERE agent_id=? AND token=?`, agent, token)
	return e
}
func ClearFederationMailFence(agent string) error {
	d, e := Open()
	if e != nil {
		return e
	}
	_, e = d.Exec(`DELETE FROM federation_agent_mail_fences WHERE agent_id=?`, agent)
	return e
}
func federationMailFencedTx(tx *sql.Tx, agent string) (bool, error) {
	if agent == "" {
		return false, nil
	}
	var n int
	e := tx.QueryRow(`SELECT COUNT(*) FROM federation_agent_mail_fences f WHERE agent_id=? AND expires_at>? AND (offer='' OR EXISTS(SELECT 1 FROM federation_agent_moves m WHERE m.direction='out' AND m.id=f.offer AND m.state IN ('awaiting_confirmation','confirmed','retiring')))`, agent, dbTime(time.Now())).Scan(&n)
	return n > 0, e
}
func ListFederationMailDeliveries(agent string) ([]FederationMailDelivery, error) {
	d, e := Open()
	if e != nil {
		return nil, e
	}
	rows, e := d.Query(`SELECT sender_instance,envelope_id,expires_at FROM federation_agent_mail_deliveries WHERE agent_id=? AND expires_at>? ORDER BY sender_instance,envelope_id`, agent, dbTime(time.Now()))
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []FederationMailDelivery
	for rows.Next() {
		var m FederationMailDelivery
		var expiry dbTimestamp
		if e = rows.Scan(&m.Sender, &m.Envelope, &expiry); e != nil {
			return nil, e
		}
		m.ExpiresAt = expiry.Time()
		out = append(out, m)
	}
	return out, rows.Err()
}
func ImportFederationMailDeliveries(agent string, rows []FederationMailDelivery) error {
	d, e := Open()
	if e != nil {
		return e
	}
	tx, e := d.Begin()
	if e != nil {
		return e
	}
	defer func() { _ = tx.Rollback() }()
	for _, r := range rows {
		if r.Sender == "" || r.Envelope == "" || !r.ExpiresAt.After(time.Now()) {
			continue
		}
		_, e = tx.Exec(`INSERT INTO federation_agent_mail_deliveries(agent_id,sender_instance,envelope_id,expires_at) VALUES(?,?,?,?) ON CONFLICT(agent_id,sender_instance,envelope_id) DO UPDATE SET expires_at=MAX(expires_at,excluded.expires_at)`, agent, r.Sender, r.Envelope, dbTime(r.ExpiresAt))
		if e != nil {
			return e
		}
	}
	return tx.Commit()
}

type FederationMailCustody struct {
	SenderInstance  string
	EnvelopeID      string
	AgentID         string
	IngressInstance string
	IngressEnvelope string
	Payload         string
	State           string
	Destination     string
	AttemptID       string
	NextAttemptAt   time.Time
	ExpiresAt       time.Time
	UpdatedAt       time.Time
}

const homeMailColumns = `sender_instance,envelope_id,agent_id,ingress_instance,ingress_envelope,payload,state,destination,attempt_id,next_attempt_at,expires_at,updated_at`

func scanHomeMail(row rowScanner) (*FederationMailCustody, error) {
	var m FederationMailCustody
	var next, expires, updated dbTimestamp
	e := row.Scan(&m.SenderInstance, &m.EnvelopeID, &m.AgentID, &m.IngressInstance, &m.IngressEnvelope, &m.Payload, &m.State, &m.Destination, &m.AttemptID, &next, &expires, &updated)
	if errors.Is(e, sql.ErrNoRows) {
		return nil, nil
	}
	m.NextAttemptAt = next.Time()
	m.ExpiresAt = expires.Time()
	m.UpdatedAt = updated.Time()
	return &m, e
}
func GetFederationMailCustody(sender, id, agent string) (*FederationMailCustody, error) {
	d, e := Open()
	if e != nil {
		return nil, e
	}
	return scanHomeMail(d.QueryRow(`SELECT `+homeMailColumns+` FROM federation_mail_custody WHERE sender_instance=? AND envelope_id=? AND agent_id=?`, sender, id, agent))
}

// QueueFederationMailCustody stores the accepted payload before acknowledging
// custody. Duplicate envelopes do not create another queue entry or reset it.
func QueueFederationMailCustody(m FederationMailCustody, limit int) (bool, error) {
	d, e := Open()
	if e != nil {
		return false, e
	}
	tx, e := d.Begin()
	if e != nil {
		return false, e
	}
	defer func() { _ = tx.Rollback() }()
	fresh, e := queueFederationMailCustodyTx(tx, m, limit)
	if e != nil {
		return false, e
	}
	return fresh, tx.Commit()
}
func queueFederationMailCustodyTx(tx *sql.Tx, m FederationMailCustody, limit int) (bool, error) {
	var e error
	// Take the writer lock before quota checks, including when no row exists.
	if _, e = tx.Exec(`UPDATE federation_mail_custody SET updated_at=updated_at WHERE sender_instance=? AND envelope_id=? AND agent_id=?`, m.SenderInstance, m.EnvelopeID, m.AgentID); e != nil {
		return false, e
	}
	var n int
	e = tx.QueryRow(`SELECT COUNT(*) FROM federation_mail_custody WHERE sender_instance=? AND envelope_id=? AND agent_id=?`, m.SenderInstance, m.EnvelopeID, m.AgentID).Scan(&n)
	if e != nil {
		return false, e
	}
	if n > 0 {
		return false, nil
	}
	e = tx.QueryRow(`SELECT COUNT(*) FROM federation_mail_custody WHERE agent_id=? AND state IN ('queued','handoff') AND expires_at>?`, m.AgentID, dbTime(time.Now())).Scan(&n)
	if e != nil {
		return false, e
	}
	if limit > 0 && n >= limit {
		return false, &AgentMessageQueueFullError{Pending: n, Limit: limit}
	}
	now := time.Now()
	if m.State == "" {
		m.State = "queued"
	}
	_, e = tx.Exec(`INSERT INTO federation_mail_custody (`+homeMailColumns+`) VALUES(?,?,?,?,?,?,?,'','',?,?,?)`, m.SenderInstance, m.EnvelopeID, m.AgentID, m.IngressInstance, m.IngressEnvelope, m.Payload, m.State, dbTime(now), dbTime(m.ExpiresAt), dbTime(now))
	if e != nil {
		return false, e
	}
	return true, nil
}
func DueFederationMailCustody(now time.Time, limit int) ([]FederationMailCustody, error) {
	d, e := Open()
	if e != nil {
		return nil, e
	}
	rows, e := d.Query(`SELECT `+homeMailColumns+` FROM federation_mail_custody WHERE state='queued' AND next_attempt_at<=? ORDER BY updated_at LIMIT ?`, dbTime(now), limit)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []FederationMailCustody
	for rows.Next() {
		m, e := scanHomeMail(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}
func UpdateFederationMailCustody(m FederationMailCustody) error {
	d, e := Open()
	if e != nil {
		return e
	}
	_, e = d.Exec(`UPDATE federation_mail_custody SET state=?,destination=?,attempt_id=?,next_attempt_at=?,updated_at=? WHERE sender_instance=? AND envelope_id=? AND agent_id=? AND state IN ('queued','handoff')`, m.State, m.Destination, m.AttemptID, dbTime(m.NextAttemptAt), dbTime(time.Now()), m.SenderInstance, m.EnvelopeID, m.AgentID)
	return e
}
func WakeFederationMailCustody(agent string) error {
	d, e := Open()
	if e != nil {
		return e
	}
	_, e = d.Exec(`UPDATE federation_mail_custody SET next_attempt_at=? WHERE agent_id=? AND state='queued'`, dbTime(time.Now()), agent)
	return e
}

// ProjectedAgentFederationPresence uses confirmed location only in public views;
// departure and lease checks still read the original pinned presence record.
func ProjectedAgentFederationPresence(id string) (*AgentFederationPresence, error) {
	p, e := GetAgentFederationPresence(id)
	if e != nil || p == nil || p.State != "away" {
		return p, e
	}
	l, e := GetFederationAgentLocation(id)
	if e != nil {
		return nil, e
	}
	if l != nil && l.Epoch == p.ContinuationNonceHash {
		p.CurrentInstance = l.CurrentInstance
		p.CurrentPeer = l.CurrentInstance
		p.HopCount = l.HopCount
	}
	return p, nil
}

// InsertLocalHomeMail atomically preserves the sender's outbox copy and durable
// custody. The payload builder only encodes that newly allocated message ID.
func InsertLocalHomeMail(m *AgentMessage, attachments []AgentMessageAttachment, limit int, makeCustody func(int64) (FederationMailCustody, error), cron ...int64) (int64, int, error) {
	m.RegularSend = limit > 0
	hook := func(tx *sql.Tx, id int64) error {
		custody, e := makeCustody(id)
		if e != nil {
			return e
		}
		_, e = queueFederationMailCustodyTx(tx, custody, limit)
		return e
	}
	if len(cron) > 0 && cron[0] > 0 {
		id, _, err := insertLatestCronAgentMessage(m, cron[0], hook)
		return id, 0, err
	}
	return insertAgentMessageWithAttachmentsBounded(m, attachments, limit, hook)
}
func LocalHomeMailCustody(messageID int64) (*FederationMailCustody, error) {
	d, e := Open()
	if e != nil {
		return nil, e
	}
	return scanHomeMail(d.QueryRow(`SELECT `+homeMailColumns+` FROM federation_mail_custody WHERE json_extract(payload,'$.local_message')=? AND sender_instance=ingress_instance ORDER BY updated_at DESC LIMIT 1`, messageID))
}
func SetLocalHomeMailOutcome(messageID int64, accepted bool, local bool) error {
	d, e := Open()
	if e != nil {
		return e
	}
	if accepted && local {
		_, e = d.Exec(`UPDATE agent_messages SET nudge_cancelled_at=NULL,nudge_cancel_reason='' WHERE id=?`, messageID)
		return e
	}
	if accepted {
		_, e = d.Exec(`UPDATE agent_messages SET delivered_at=?,processed_at=?,nudge_claimed_at=NULL WHERE id=?`, dbTime(time.Now()), dbTime(time.Now()), messageID)
		return e
	}
	_, e = d.Exec(`UPDATE agent_messages SET nudge_cancelled_at=?,nudge_cancel_reason='home forwarding refused',processed_at=? WHERE id=?`, dbTime(time.Now()), dbTime(time.Now()), messageID)
	return e
}
