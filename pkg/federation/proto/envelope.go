package proto

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// EnvelopeVersion is the current envelope format version.
const EnvelopeVersion = 2

// Envelope kinds.
const (
	KindCatalog    = "catalog"
	KindCatalogReq = "catalog_req"
	KindMail       = "mail"
	KindAck        = "ack"
	// KindOperatorMail is mail from one instance's human operator to
	// another's. It carries a MailPayload and no To.Agent.
	KindOperatorMail = "operator_mail"
	// KindSpawnReq asks the recipient to spawn a worker into one of its
	// exported groups. The recipient's operator decides; KindSpawnRes
	// (InReplyTo = the request) reports the decision.
	KindSpawnReq         = "spawn_req"
	KindSpawnRes         = "spawn_res"
	KindBundleOffer      = "bundle_offer"
	KindBundleFetch      = "bundle_fetch"
	KindBundleAnswer     = "bundle_answer"
	KindBundleResult     = "bundle_result"
	KindAgentMoveConfirm = "agent_move_confirm"
	// KindRouteOpen asks the recipient to open one TCP connection to one of
	// its exported routes; KindRouteAnswer accepts (then both dial the hub
	// stream relay) or refuses it. These are real-time control envelopes,
	// not queued in the outbox.
	KindRouteOpen   = "route_open"
	KindRouteAnswer = "route_answer"
	// KindGroupMail is mail to every current member of an exported group
	// (optionally narrowed by role). The receiver resolves the members:
	// it, not the sender's catalog, is the authority on its roster.
	KindGroupMail      = "group_mail"
	KindAwayNotice     = "away_notice"
	KindAwayAnswer     = "away_answer"
	KindSessionsUpdate = "sessions_update"
	KindSessionOpen    = "session_open"
	KindSessionAnswer  = "session_answer"
)

// Export capabilities a catalog group can grant.
const (
	CapRoster   = "roster"
	CapPresence = "presence"
	CapMail     = "mail"
	// CapAttachments lets mail from the peer carry files. It only means
	// something together with CapMail.
	CapAttachments = "attachments"
	// CapSpawn lets the peer ask for a worker to be spawned into the group.
	// Every request still waits for the local operator's approval.
	CapSpawn = "spawn"
	// CapRoutes lists the group's ready routes in the catalog and lets the
	// peer open connections to them through the hub stream relay.
	CapRoutes         = "routes"
	CapSessions       = "sessions"
	CapSessionsWatch  = "sessions_watch"
	CapSessionsAttach = "sessions_attach"
	CapAgentsReceive  = "agents_receive"
)

// AllCaps lists every known capability in canonical order.
var AllCaps = []string{CapRoster, CapPresence, CapMail, CapAttachments, CapSpawn, CapRoutes, CapSessions, CapSessionsWatch, CapSessionsAttach, CapAgentsReceive}

// MaxMailBody caps a mail envelope's body in bytes.
const MaxMailBody = 16 * 1024

// Attachment limits per mail. Files travel inline in the encrypted payload,
// so they are base64-encoded twice on the wire; MaxEnvelopeBytes leaves room
// for that.
const (
	MaxAttachments     = 4
	MaxAttachmentBytes = 512 * 1024
)

// MaxEnvelopeBytes caps a sealed envelope's encoded size.
const MaxEnvelopeBytes = 1024 * 1024

// Endpoint names an instance and optionally one of its agents.
type Endpoint struct {
	Instance string `json:"instance"`
	Agent    string `json:"agent,omitempty"`
	Name     string `json:"name,omitempty"`
}

// Envelope is the signed unit of instance-to-instance communication. The
// hub routes it on To.Instance but cannot alter it: the signature covers
// the exact encoded bytes.
type Envelope struct {
	V         int       `json:"v"`
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	From      Endpoint  `json:"from"`
	To        Endpoint  `json:"to"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	InReplyTo string    `json:"in_reply_to,omitempty"`
	// Payload is the plaintext payload. It never travels: Seal encrypts it
	// into Enc for the recipient and Open restores it.
	Payload json.RawMessage `json:"payload,omitempty"`
	Enc     *Encrypted      `json:"enc,omitempty"`
}

// Sealed is an encoded envelope plus the origin instance's signature over
// those exact bytes.
type Sealed struct {
	Env []byte `json:"env"`
	Sig []byte `json:"sig"`
}

// MailPayload is the payload of a KindMail envelope.
type MailPayload struct {
	Subject     string              `json:"subject,omitempty"`
	Body        string              `json:"body"`
	Attachments []AttachmentPayload `json:"attachments,omitempty"`
}

// AttachmentPayload is one file carried by a mail. The receiver derives
// the content type from the (sanitized) name; senders do not get to claim
// one.
type AttachmentPayload struct {
	Name string `json:"name"`
	Data []byte `json:"data"`
}

// AttachmentBytes is the total size of p's attachments.
func (p *MailPayload) AttachmentBytes() int {
	n := 0
	for _, a := range p.Attachments {
		n += len(a.Data)
	}
	return n
}

// MaxSpawnBrief caps a spawn request's brief.
const MaxSpawnBrief = 8 * 1024

// SpawnRequestPayload is the payload of a KindSpawnReq envelope.
type SpawnRequestPayload struct {
	Group string `json:"group"`
	Name  string `json:"name,omitempty"`
	Role  string `json:"role,omitempty"`
	Brief string `json:"brief"`
}

// Spawn result statuses.
const (
	SpawnApproved = "approved"
	SpawnDenied   = "denied"
)

// SpawnResultPayload is the payload of a KindSpawnRes envelope.
type SpawnResultPayload struct {
	Status string `json:"status"`
	Agent  string `json:"agent,omitempty"`
	Name   string `json:"name,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// Ack statuses.
const (
	AckAccepted = "accepted"
	AckRefused  = "refused"
)

// AckPayload is the payload of a KindAck envelope; InReplyTo names the
// acknowledged envelope.
type AckPayload struct {
	Status string `json:"status"`
	Code   string `json:"code,omitempty"`
	Reason string `json:"reason,omitempty"`
	// Delivered counts the recipients of an accepted group_mail.
	Delivered int `json:"delivered,omitempty"`
}

// GroupMailPayload is mail to an exported group's members.
type GroupMailPayload struct {
	Group   string `json:"group"`
	Role    string `json:"role,omitempty"`
	Subject string `json:"subject,omitempty"`
	Body    string `json:"body"`
}

// CatalogPayload lists what an instance exports to the receiving peer.
type CatalogPayload struct {
	AgentMoves bool           `json:"agent_moves,omitempty"`
	Groups     []CatalogGroup `json:"groups"`
}

// CatalogGroup is one exported group as seen by one peer.
type CatalogGroup struct {
	Name        string           `json:"name"`
	Description string           `json:"description,omitempty"`
	Caps        []string         `json:"caps"`
	Members     []CatalogMember  `json:"members,omitempty"`
	Routes      []CatalogRoute   `json:"routes,omitempty"`
	Sessions    []CatalogSession `json:"sessions,omitempty"`
	SessionsAt  time.Time        `json:"sessions_at,omitempty"`
}

// CatalogSession describes a group member's current live pane. Agent is the
// stable attach target; Incarnation distinguishes reused runtime session IDs.
// WaitingObservedSince is a lower bound, reset when the observer restarts.
type CatalogSession struct {
	Agent                string     `json:"agent"`
	Session              string     `json:"session"`
	Incarnation          string     `json:"incarnation,omitempty"`
	Name                 string     `json:"name"`
	Harness              string     `json:"harness,omitempty"`
	State                string     `json:"state"`
	WaitingReason        string     `json:"waiting_reason,omitempty"`
	WaitingObservedSince *time.Time `json:"waiting_observed_since,omitempty"`
}

// SessionsUpdatePayload replaces session snapshots only, leaving catalog
// capabilities and roster/presence freshness untouched.
type SessionsUpdatePayload struct {
	Groups []SessionGroupUpdate `json:"groups"`
}
type SessionGroupUpdate struct {
	Name     string           `json:"name"`
	Sessions []CatalogSession `json:"sessions"`
	At       time.Time        `json:"at"`
}

// CatalogRoute is one ready route of an exported group (CapRoutes).
type CatalogRoute struct {
	ID        string `json:"id"`
	Publisher string `json:"publisher"`
	Name      string `json:"name"`
}

// RouteOpenPayload is the payload of a KindRouteOpen envelope.
type RouteOpenPayload struct {
	Route  string `json:"route"`
	Stream string `json:"stream"`
	Key    []byte `json:"key"`
}

// RouteAnswerPayload is the payload of a KindRouteAnswer envelope.
// InReplyTo names the open; Key is set on acceptance.
type RouteAnswerPayload struct {
	Stream string `json:"stream"`
	OK     bool   `json:"ok"`
	Key    []byte `json:"key,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// HasCap reports whether the group grants capability c.
func (g CatalogGroup) HasCap(c string) bool {
	for _, x := range g.Caps {
		if x == c {
			return true
		}
	}
	return false
}

// CatalogMember is one member of an exported group. Presence is only set
// when the group exports CapPresence.
type CatalogMember struct {
	Agent    string `json:"agent"`
	Name     string `json:"name"`
	Role     string `json:"role,omitempty"`
	Harness  string `json:"harness,omitempty"`
	Presence string `json:"presence,omitempty"`
}

// NewEnvelopeID returns a random 128-bit hex id.
func NewEnvelopeID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("crypto/rand: %v", err))
	}
	return hex.EncodeToString(b[:])
}

// NewEnvelope builds an envelope from id's instance with a JSON payload.
func NewEnvelope(id *Identity, kind string, from, to Endpoint, ttl time.Duration, payload any) (*Envelope, error) {
	from.Instance = id.ID()
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	return &Envelope{
		V: EnvelopeVersion, ID: NewEnvelopeID(), Kind: kind,
		From: from, To: to,
		CreatedAt: now, ExpiresAt: now.Add(ttl),
		Payload: raw,
	}, nil
}

// Seal encrypts env's payload to the recipient's identity key, then encodes
// and signs the envelope with id. env.From.Instance must be id's own and
// recipient must be the key of env.To.Instance. env itself is not modified.
func Seal(id *Identity, env *Envelope, recipient ed25519.PublicKey) (*Sealed, error) {
	if env.From.Instance != id.ID() {
		return nil, errors.New("envelope from.instance does not match signing identity")
	}
	if InstanceID(recipient) != env.To.Instance {
		return nil, errors.New("recipient key does not match envelope to.instance")
	}
	payload := env.Payload
	if len(payload) == 0 {
		payload = json.RawMessage("{}")
	}
	enc, err := encryptPayload(recipient, env.ID, headerAAD(env), payload)
	if err != nil {
		return nil, err
	}
	wire := *env
	wire.Payload = nil
	wire.Enc = enc
	raw, err := json.Marshal(&wire)
	if err != nil {
		return nil, err
	}
	if len(raw) > MaxEnvelopeBytes {
		return nil, fmt.Errorf("%w (%d bytes)", ErrTooLarge, len(raw))
	}
	return &Sealed{Env: raw, Sig: ed25519.Sign(id.Priv, raw)}, nil
}

// ErrTooLarge is returned by Seal when the encoded envelope exceeds
// MaxEnvelopeBytes.
var ErrTooLarge = errors.New("envelope too large")

// Errors returned by Open.
var (
	ErrBadSignature = errors.New("envelope signature does not verify")
	ErrWrongSender  = errors.New("envelope from.instance does not match the signing key")
	ErrWrongTarget  = errors.New("envelope is addressed to another instance")
	ErrExpired      = errors.New("envelope expired")
	ErrMalformed    = errors.New("malformed envelope")
)

// Open verifies s against the sender's pinned key and that it is addressed
// to self, then decrypts its payload. now bounds expiry.
func Open(s *Sealed, senderPub ed25519.PublicKey, self *Identity, now time.Time) (*Envelope, error) {
	if len(s.Env) == 0 || len(s.Env) > MaxEnvelopeBytes || len(senderPub) != ed25519.PublicKeySize {
		return nil, ErrMalformed
	}
	if !ed25519.Verify(senderPub, s.Env, s.Sig) {
		return nil, ErrBadSignature
	}
	var env Envelope
	if err := json.Unmarshal(s.Env, &env); err != nil {
		return nil, ErrMalformed
	}
	if env.V != EnvelopeVersion || env.ID == "" || env.Kind == "" {
		return nil, ErrMalformed
	}
	if env.From.Instance != InstanceID(senderPub) {
		return nil, ErrWrongSender
	}
	if env.To.Instance != self.ID() {
		return nil, ErrWrongTarget
	}
	if !env.ExpiresAt.IsZero() && now.After(env.ExpiresAt) {
		return nil, ErrExpired
	}
	// Plaintext payloads are not accepted: the hub must only ever route
	// ciphertext.
	if len(env.Payload) != 0 || env.Enc == nil {
		return nil, ErrMalformed
	}
	pt, err := decryptPayload(self, env.ID, headerAAD(&env), env.Enc)
	if err != nil {
		return nil, err
	}
	env.Payload, env.Enc = pt, nil
	return &env, nil
}

// DecodePayload unmarshals env.Payload into v.
func (e *Envelope) DecodePayload(v any) error {
	if err := json.Unmarshal(e.Payload, v); err != nil {
		return fmt.Errorf("%w: payload: %v", ErrMalformed, err)
	}
	return nil
}

// SessionOpenPayload requests access to one stable agent's current pane.
// Session and Incarnation pin discovery to one launch; it cannot follow a restart.
type SessionOpenPayload struct {
	Agent       string `json:"agent"`
	Session     string `json:"session"`
	Incarnation string `json:"incarnation"`
	Group       string `json:"group"`
	Stream      string `json:"stream"`
	Key         []byte `json:"key"`
	ReadOnly    bool   `json:"read_only"`
	Cols        int    `json:"cols"`
	Rows        int    `json:"rows"`
}
type SessionAnswerPayload struct {
	Stream string `json:"stream"`
	OK     bool   `json:"ok"`
	Key    []byte `json:"key,omitempty"`
	Reason string `json:"reason,omitempty"`
}
