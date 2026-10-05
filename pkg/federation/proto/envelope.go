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
const EnvelopeVersion = 1

// Envelope kinds.
const (
	KindCatalog    = "catalog"
	KindCatalogReq = "catalog_req"
	KindMail       = "mail"
	KindAck        = "ack"
	// KindOperatorMail is mail from one instance's human operator to
	// another's. It carries a MailPayload and no To.Agent.
	KindOperatorMail = "operator_mail"
)

// Export capabilities a catalog group can grant.
const (
	CapRoster   = "roster"
	CapPresence = "presence"
	CapMail     = "mail"
)

// AllCaps lists every known capability in canonical order.
var AllCaps = []string{CapRoster, CapPresence, CapMail}

// MaxMailBody caps a mail envelope's body in bytes.
const MaxMailBody = 16 * 1024

// MaxEnvelopeBytes caps a sealed envelope's encoded size.
const MaxEnvelopeBytes = 256 * 1024

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
	Subject string `json:"subject,omitempty"`
	Body    string `json:"body"`
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
}

// CatalogPayload lists what an instance exports to the receiving peer.
type CatalogPayload struct {
	Groups []CatalogGroup `json:"groups"`
}

// CatalogGroup is one exported group as seen by one peer.
type CatalogGroup struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Caps        []string        `json:"caps"`
	Members     []CatalogMember `json:"members,omitempty"`
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
	enc, err := encryptPayload(recipient, env.ID, payload)
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
		return nil, fmt.Errorf("envelope too large (%d bytes)", len(raw))
	}
	return &Sealed{Env: raw, Sig: ed25519.Sign(id.Priv, raw)}, nil
}

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
	pt, err := decryptPayload(self, env.ID, env.Enc)
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
