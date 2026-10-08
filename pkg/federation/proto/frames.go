package proto

import (
	"crypto/ed25519"
	"time"
)

// ProtocolVersion is the hub wire protocol major version.
const ProtocolVersion = 1

// WSPath is the hub's WebSocket endpoint.
const WSPath = "/v1/connect"

// StreamPath is the hub's stream relay endpoint. Two instances that agreed
// on a stream id (over sealed envelopes) each dial it; the hub pairs them
// and forwards binary messages between them without interpreting them.
const StreamPath = "/v1/stream"

// MaxStreamMessage caps one binary stream message.
const MaxStreamMessage = 64 << 10

// Frame types. Every WebSocket text message is one JSON Frame.
const (
	// hub → instance
	FrameChallenge  = "challenge"
	FrameWelcome    = "welcome"
	FrameDirectory  = "directory"
	FrameDeliver    = "deliver"
	FrameSendResult = "send_result"
	FrameError      = "error"
	// FrameStreamReady tells a stream dialer that its peer has joined and
	// binary forwarding has begun.
	FrameStreamReady = "stream_ready"
	// instance → hub
	FrameHello = "hello"
	FrameSend  = "send"
)

// Send result statuses.
const (
	SendDelivered   = "delivered" // handed to the target's live connection; not an end-to-end ack
	SendOffline     = "offline"
	SendRefused     = "refused"
	SendRateLimited = "rate_limited"
)

// Error codes carried by FrameError and refusals.
const (
	CodeNotAdmitted  = "not_admitted"
	CodeBadAuth      = "bad_auth"
	CodeBadVersion   = "bad_version"
	CodeBadFrame     = "bad_frame"
	CodeNotVisible   = "not_visible"
	CodeRateLimited  = "rate_limited"
	CodeReplaced     = "replaced"
	CodeShuttingDown = "shutting_down"
	CodeStreamLimit  = "stream_limit"
	CodeStreamWait   = "stream_timeout"
)

// Frame is the single JSON shape of every hub WebSocket message; Type
// selects which fields are meaningful.
type Frame struct {
	Type                    string `json:"type"`
	IdentityRotationVersion int    `json:"identity_rotation_version,omitempty"`

	// challenge
	HubID string `json:"hub_id,omitempty"`
	Nonce string `json:"nonce,omitempty"`
	Proto int    `json:"proto,omitempty"`

	// hello
	InstanceID string `json:"instance_id,omitempty"`
	PubKey     []byte `json:"pubkey,omitempty"`
	Name       string `json:"name,omitempty"`
	Sig        []byte `json:"sig,omitempty"`
	Invite     string `json:"invite,omitempty"`
	Version    string `json:"version,omitempty"`

	// RotationChain carries public, dual-signed identity succession evidence.
	RotationChain []Rotation `json:"rotation_chain,omitempty"`

	// welcome
	Spaces []string `json:"spaces,omitempty"`

	// stream hello: the stream id and the instance expected on the other end
	Stream string `json:"stream,omitempty"`
	Peer   string `json:"peer,omitempty"`

	// directory
	Instances []DirectoryEntry `json:"instances,omitempty"`

	// send / deliver / send_result
	Ref    string  `json:"ref,omitempty"`
	To     string  `json:"to,omitempty"`
	From   string  `json:"from,omitempty"`
	Sealed *Sealed `json:"sealed,omitempty"`
	Status string  `json:"status,omitempty"`

	// error / refusals
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// DirectoryEntry is one visible instance as the hub reports it.
type DirectoryEntry struct {
	RotationChain []Rotation `json:"rotation_chain,omitempty"`
	InstanceID    string     `json:"instance_id"`
	PubKey        []byte     `json:"pubkey"`
	Name          string     `json:"name,omitempty"`
	Online        bool       `json:"online"`
	LastSeen      time.Time  `json:"last_seen,omitempty"`
	Version       string     `json:"version,omitempty"`
}

// HelloMessage is the byte string an instance signs to answer a hub
// challenge. Binding the hub id stops a malicious hub from relaying the
// answer to another hub.
func HelloMessage(hubID, nonce, instanceID string) []byte {
	return []byte("tclaude-fed-hello-v1\n" + hubID + "\n" + nonce + "\n" + instanceID)
}

// SignHello answers a challenge.
func SignHello(id *Identity, hubID, nonce string) []byte {
	return ed25519.Sign(id.Priv, HelloMessage(hubID, nonce, id.ID()))
}

// VerifyHello checks a hello frame against the challenge the hub issued and
// that the claimed id is derived from the presented key.
func VerifyHello(f *Frame, hubID, nonce string) bool {
	if len(f.PubKey) != ed25519.PublicKeySize {
		return false
	}
	pub := ed25519.PublicKey(f.PubKey)
	if InstanceID(pub) != f.InstanceID {
		return false
	}
	return ed25519.Verify(pub, HelloMessage(hubID, nonce, f.InstanceID), f.Sig)
}

// ValidStreamID reports whether s has the shape of a stream id (128-bit hex).
func ValidStreamID(s string) bool {
	if len(s) != 32 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
