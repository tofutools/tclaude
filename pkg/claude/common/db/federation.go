package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Federation state (schema v230). See migrateV229toV230 and
// pkg/claude/agentd/federation*.go.

// FederationPeer is a remote instance the operator trusts.
type FederationPeer struct {
	InstanceID string
	PubKey     []byte
	Label      string
	Name       string
	TrustedAt  time.Time
}

// TrustFederationPeer inserts or updates a trusted peer. Re-trusting with a
// different key for the same id is impossible by construction (ids derive
// from keys), so an upsert only refreshes label and name.
func TrustFederationPeer(p FederationPeer) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`INSERT INTO federation_peers(instance_id, pubkey, label, name, trusted_at) VALUES(?,?,?,?,?)
		ON CONFLICT(instance_id) DO UPDATE SET label=excluded.label, name=excluded.name`,
		p.InstanceID, p.PubKey, p.Label, p.Name, dbTime(time.Now()))
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return fmt.Errorf("label %q is already used by another peer", p.Label)
	}
	return err
}

// UntrustFederationPeer removes a peer. Its imports go with it; its cached
// catalog too. Returns false when the peer was not trusted.
func UntrustFederationPeer(instanceID string) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	tx, err := d.Begin()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec(`DELETE FROM federation_peers WHERE instance_id=?`, instanceID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	for _, q := range []string{
		`DELETE FROM federation_imports WHERE peer=?`,
		`DELETE FROM federation_exports WHERE peer=?`,
		`DELETE FROM federation_catalogs WHERE peer=?`,
	} {
		if _, err := tx.Exec(q, instanceID); err != nil {
			return false, err
		}
	}
	// Nothing more goes to an untrusted instance.
	if _, err := tx.Exec(`UPDATE federation_outbox SET state=?, last_error=?, updated_at=?
		WHERE to_instance=? AND state IN (?, ?)`,
		FedOutboxRefused, "peer untrusted locally", dbTime(time.Now()), instanceID, FedOutboxQueued, FedOutboxSent); err != nil {
		return false, err
	}
	return n > 0, tx.Commit()
}

// ListFederationPeers returns every trusted peer.
func ListFederationPeers() ([]FederationPeer, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT instance_id, pubkey, label, name, trusted_at FROM federation_peers ORDER BY label, instance_id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []FederationPeer
	for rows.Next() {
		var p FederationPeer
		var at dbTimestamp
		if err := rows.Scan(&p.InstanceID, &p.PubKey, &p.Label, &p.Name, &at); err != nil {
			return nil, err
		}
		p.TrustedAt = at.Time()
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetFederationPeer returns a trusted peer or nil.
func GetFederationPeer(instanceID string) (*FederationPeer, error) {
	all, err := ListFederationPeers()
	if err != nil {
		return nil, err
	}
	for i := range all {
		if all[i].InstanceID == instanceID {
			return &all[i], nil
		}
	}
	return nil, nil
}

// FederationExport is one group exported to one peer (or "*" = every
// trusted peer).
type FederationExport struct {
	ID        int64
	GroupID   int64
	GroupName string
	Peer      string
	Caps      []string
	CreatedAt time.Time
}

// FederationExportAllPeers is the Peer value of an export to every trusted peer.
const FederationExportAllPeers = "*"

// UpsertFederationExport creates or replaces the caps of (group, peer).
func UpsertFederationExport(groupID int64, peer string, caps []string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`INSERT INTO federation_exports(group_id, peer, caps, created_at) VALUES(?,?,?,?)
		ON CONFLICT(group_id, peer) DO UPDATE SET caps=excluded.caps`,
		groupID, peer, strings.Join(caps, ","), dbTime(time.Now()))
	return err
}

// DeleteFederationExport removes (group, peer). Returns false when absent.
func DeleteFederationExport(groupID int64, peer string) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	res, err := d.Exec(`DELETE FROM federation_exports WHERE group_id=? AND peer=?`, groupID, peer)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ListFederationExports returns every export with its group name.
func ListFederationExports() ([]FederationExport, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT e.id, e.group_id, g.name, e.peer, e.caps, e.created_at
		FROM federation_exports e JOIN agent_groups g ON g.id = e.group_id ORDER BY g.name, e.peer`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []FederationExport
	for rows.Next() {
		var e FederationExport
		var caps string
		var at dbTimestamp
		if err := rows.Scan(&e.ID, &e.GroupID, &e.GroupName, &e.Peer, &caps, &at); err != nil {
			return nil, err
		}
		if caps != "" {
			e.Caps = strings.Split(caps, ",")
		}
		e.CreatedAt = at.Time()
		out = append(out, e)
	}
	return out, rows.Err()
}

// FederationImport links a remote exported group onto a local group.
type FederationImport struct {
	ID             int64
	LocalGroupID   int64
	LocalGroupName string
	Peer           string
	RemoteGroup    string
	CreatedAt      time.Time
}

// AddFederationImport records an import; duplicates are a no-op.
func AddFederationImport(localGroupID int64, peer, remoteGroup string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`INSERT OR IGNORE INTO federation_imports(local_group_id, peer, remote_group, created_at) VALUES(?,?,?,?)`,
		localGroupID, peer, remoteGroup, dbTime(time.Now()))
	return err
}

// DeleteFederationImport removes an import. Returns false when absent.
func DeleteFederationImport(localGroupID int64, peer, remoteGroup string) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	res, err := d.Exec(`DELETE FROM federation_imports WHERE local_group_id=? AND peer=? AND remote_group=?`, localGroupID, peer, remoteGroup)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ListFederationImports returns every import with its local group name.
func ListFederationImports() ([]FederationImport, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT i.id, i.local_group_id, g.name, i.peer, i.remote_group, i.created_at
		FROM federation_imports i JOIN agent_groups g ON g.id = i.local_group_id ORDER BY g.name, i.peer, i.remote_group`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []FederationImport
	for rows.Next() {
		var im FederationImport
		var at dbTimestamp
		if err := rows.Scan(&im.ID, &im.LocalGroupID, &im.LocalGroupName, &im.Peer, &im.RemoteGroup, &at); err != nil {
			return nil, err
		}
		im.CreatedAt = at.Time()
		out = append(out, im)
	}
	return out, rows.Err()
}

// PutFederationCatalog caches the latest catalog a peer sent us.
func PutFederationCatalog(peer, payload string, at time.Time) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`INSERT INTO federation_catalogs(peer, payload, received_at) VALUES(?,?,?)
		ON CONFLICT(peer) DO UPDATE SET payload=excluded.payload, received_at=excluded.received_at`,
		peer, payload, dbTime(at))
	return err
}

// GetFederationCatalog returns the cached catalog payload for peer.
func GetFederationCatalog(peer string) (payload string, receivedAt time.Time, err error) {
	d, err := Open()
	if err != nil {
		return "", time.Time{}, err
	}
	var at dbTimestamp
	err = d.QueryRow(`SELECT payload, received_at FROM federation_catalogs WHERE peer=?`, peer).Scan(&payload, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return "", time.Time{}, nil
	}
	return payload, at.Time(), err
}

// Outbox states.
const (
	FedOutboxQueued   = "queued"   // waiting to be handed to the hub
	FedOutboxSent     = "sent"     // routed by the hub, awaiting the peer's ack
	FedOutboxAccepted = "accepted" // peer acked: delivered to the recipient's inbox
	FedOutboxRefused  = "refused"  // peer acked with a refusal
	FedOutboxExpired  = "expired"  // never acked before expires_at
)

// FederationOutboxRow is one outbound envelope.
type FederationOutboxRow struct {
	EnvelopeID    string
	Kind          string
	ToInstance    string
	ToAgent       string
	ToLabel       string
	FromConv      string
	FromAgent     string
	InReplyTo     string
	Subject       string
	BodyPreview   string
	Sealed        []byte
	State         string
	Attempts      int
	NextAttemptAt time.Time
	LastError     string
	CreatedAt     time.Time
	ExpiresAt     time.Time
	UpdatedAt     time.Time
}

// InsertFederationOutbox queues an envelope.
func InsertFederationOutbox(r FederationOutboxRow) error {
	d, err := Open()
	if err != nil {
		return err
	}
	now := time.Now()
	_, err = d.Exec(`INSERT INTO federation_outbox(envelope_id, kind, to_instance, to_agent, to_label, from_conv, from_agent,
		in_reply_to, subject, body_preview, sealed, state, attempts, next_attempt_at, last_error, created_at, expires_at, updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,0,?,'',?,?,?)`,
		r.EnvelopeID, r.Kind, r.ToInstance, r.ToAgent, r.ToLabel, r.FromConv, r.FromAgent,
		r.InReplyTo, r.Subject, r.BodyPreview, r.Sealed, FedOutboxQueued, dbTime(now), dbTime(now), dbTime(r.ExpiresAt), dbTime(now))
	return err
}

const fedOutboxCols = `envelope_id, kind, to_instance, to_agent, to_label, from_conv, from_agent, in_reply_to, subject, body_preview,
	sealed, state, attempts, next_attempt_at, last_error, created_at, expires_at, updated_at`

func scanFedOutbox(rows *sql.Rows) ([]FederationOutboxRow, error) {
	defer func() { _ = rows.Close() }()
	var out []FederationOutboxRow
	for rows.Next() {
		var r FederationOutboxRow
		var next, created, exp, upd dbTimestamp
		if err := rows.Scan(&r.EnvelopeID, &r.Kind, &r.ToInstance, &r.ToAgent, &r.ToLabel, &r.FromConv, &r.FromAgent,
			&r.InReplyTo, &r.Subject, &r.BodyPreview, &r.Sealed, &r.State, &r.Attempts, &next, &r.LastError,
			&created, &exp, &upd); err != nil {
			return nil, err
		}
		r.NextAttemptAt, r.CreatedAt, r.ExpiresAt, r.UpdatedAt = next.Time(), created.Time(), exp.Time(), upd.Time()
		out = append(out, r)
	}
	return out, rows.Err()
}

// DueFederationOutbox returns queued/sent rows whose next attempt is due.
func DueFederationOutbox(now time.Time, limit int) ([]FederationOutboxRow, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT `+fedOutboxCols+` FROM federation_outbox
		WHERE state IN (?, ?) AND next_attempt_at <= ? ORDER BY created_at LIMIT ?`,
		FedOutboxQueued, FedOutboxSent, dbTime(now), limit)
	if err != nil {
		return nil, err
	}
	return scanFedOutbox(rows)
}

// ListFederationOutbox returns the newest rows first.
func ListFederationOutbox(limit int) ([]FederationOutboxRow, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT `+fedOutboxCols+` FROM federation_outbox ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	return scanFedOutbox(rows)
}

// GetFederationOutbox returns one row or nil.
func GetFederationOutbox(envelopeID string) (*FederationOutboxRow, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT `+fedOutboxCols+` FROM federation_outbox WHERE envelope_id=?`, envelopeID)
	if err != nil {
		return nil, err
	}
	all, err := scanFedOutbox(rows)
	if err != nil || len(all) == 0 {
		return nil, err
	}
	return &all[0], nil
}

// UpdateFederationOutbox records an attempt outcome. attemptInc adds to
// the attempt counter.
func UpdateFederationOutbox(envelopeID, state string, next time.Time, lastError string, attemptInc int) error {
	if next.IsZero() {
		next = time.Now()
	}
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`UPDATE federation_outbox SET state=?, next_attempt_at=?, last_error=?, attempts=attempts+?, updated_at=?
		WHERE envelope_id=?`, state, dbTime(next), lastError, attemptInc, dbTime(time.Now()), envelopeID)
	return err
}

// SettleFederationOutbox moves a row to a final state only if it is still
// pending (queued or sent), so a late duplicate ack cannot flip an outcome.
func SettleFederationOutbox(envelopeID, state, lastError string) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	res, err := d.Exec(`UPDATE federation_outbox SET state=?, last_error=?, updated_at=?
		WHERE envelope_id=? AND state IN (?, ?)`, state, lastError, dbTime(time.Now()), envelopeID, FedOutboxQueued, FedOutboxSent)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// FederationInbound marks an agent_messages row as received from a remote
// instance.
type FederationInbound struct {
	MessageID    int64
	EnvelopeID   string
	FromInstance string
	FromAgent    string
	FromName     string
	ReceivedAt   time.Time
}

// ErrFederationDuplicate is returned when an envelope was already accepted.
var ErrFederationDuplicate = errors.New("federation envelope already delivered")

// FederationEnvelopeSeen reports whether a mail envelope from fromInstance
// was already accepted.
func FederationEnvelopeSeen(fromInstance, envelopeID string) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	var n int
	err = d.QueryRow(`SELECT COUNT(*) FROM federation_seen WHERE from_instance=? AND envelope_id=?`, fromInstance, envelopeID).Scan(&n)
	return n > 0, err
}

// InsertFederationInboundMessage atomically records the envelope as seen,
// inserts m bounded like a regular send, and marks it remote. A repeated
// (sender, envelope id) returns ErrFederationDuplicate and writes nothing;
// a full backlog returns *AgentMessageQueueFullError and writes nothing.
func InsertFederationInboundMessage(m *AgentMessage, in FederationInbound, expiresAt time.Time, limit int) (int64, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	tx, err := d.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec(`INSERT OR IGNORE INTO federation_seen(from_instance, envelope_id, expires_at) VALUES(?,?,?)`,
		in.FromInstance, in.EnvelopeID, dbTime(expiresAt))
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, ErrFederationDuplicate
	}
	m.RegularSend = true
	if limit > 0 {
		pending, err := countUnprocessedRegularMessageBacklog(tx, m)
		if err != nil {
			return 0, err
		}
		if pending >= limit {
			return 0, &AgentMessageQueueFullError{Pending: pending, Limit: limit}
		}
	}
	id, err := insertAgentMessage(tx, m)
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`INSERT INTO federation_inbound(message_id, envelope_id, from_instance, from_agent, from_name, received_at)
		VALUES(?,?,?,?,?,?)`, id, in.EnvelopeID, in.FromInstance, in.FromAgent, in.FromName, dbTime(time.Now())); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

// FederationHumanGroup is the group_name snapshot stored on human_messages
// rows that came from a remote operator: it marks the row as remote and
// names the sending instance, which bounds that peer's unread backlog.
func FederationHumanGroup(instanceID string) string { return "federation:" + instanceID }

// InsertFederationInboundHumanMessage atomically records the envelope as
// seen and inserts m into the operator's Messages inbox. m.GroupName must be
// FederationHumanGroup(fromInstance). A repeated (sender, envelope id)
// returns ErrFederationDuplicate; more than unreadLimit unread messages from
// that instance returns *AgentMessageQueueFullError. Either writes nothing.
func InsertFederationInboundHumanMessage(m *HumanMessage, fromInstance, envelopeID string, expiresAt time.Time, unreadLimit int) (int64, error) {
	if m.GroupName != FederationHumanGroup(fromInstance) {
		return 0, errors.New("remote operator message must be filed under its instance")
	}
	d, err := Open()
	if err != nil {
		return 0, err
	}
	tx, err := d.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec(`INSERT OR IGNORE INTO federation_seen(from_instance, envelope_id, expires_at) VALUES(?,?,?)`,
		fromInstance, envelopeID, dbTime(expiresAt))
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, ErrFederationDuplicate
	}
	if unreadLimit > 0 {
		var pending int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM human_messages WHERE group_name=? AND read_at IS NULL`, m.GroupName).Scan(&pending); err != nil {
			return 0, err
		}
		if pending >= unreadLimit {
			return 0, &AgentMessageQueueFullError{Pending: pending, Limit: unreadLimit}
		}
	}
	id, err := insertHumanMessage(tx, m)
	if err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

// PruneFederationSeen drops replay-guard rows whose envelopes have expired.
func PruneFederationSeen(now time.Time) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`DELETE FROM federation_seen WHERE expires_at < ?`, dbTime(now))
	return err
}

func scanFedInbound(row *sql.Row) (*FederationInbound, error) {
	var in FederationInbound
	var at dbTimestamp
	err := row.Scan(&in.MessageID, &in.EnvelopeID, &in.FromInstance, &in.FromAgent, &in.FromName, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	in.ReceivedAt = at.Time()
	return &in, nil
}

// FederationInboundByEnvelope returns the inbound marker for a sender's
// envelope id, or nil (also when the message has since been deleted).
func FederationInboundByEnvelope(fromInstance, envelopeID string) (*FederationInbound, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	return scanFedInbound(d.QueryRow(`SELECT message_id, envelope_id, from_instance, from_agent, from_name, received_at
		FROM federation_inbound WHERE from_instance=? AND envelope_id=?`, fromInstance, envelopeID))
}

// FederationInboundForMessage returns the inbound marker for a message id,
// or nil when the message is local.
func FederationInboundForMessage(messageID int64) (*FederationInbound, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	return scanFedInbound(d.QueryRow(`SELECT message_id, envelope_id, from_instance, from_agent, from_name, received_at
		FROM federation_inbound WHERE message_id=?`, messageID))
}
