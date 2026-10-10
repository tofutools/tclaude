package proto

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

const FrameAdminRequest = "hub_admin_request"
const FrameAdminResult = "hub_admin_result"
const MaxAdminPayload = 64 << 10
const MaxAdminResult = 256 << 10

// These capabilities authorize hub infrastructure only, never node content.
// hub.exec is deliberately absent: a future implementation must also require
// the hub host's independent, default-off accept_remote_scripts switch.
var HubAdminBootstrapCapabilities = []string{"hub.admins.manage", "hub.admissions.manage", "hub.invites.manage", "hub.spaces.manage", "hub.settings.manage", "hub.identity.manage", "hub.health.read", "hub.logs.read"}

// Supported capabilities may grow, but bootstrap never gains elevated authority.
var HubAdminCapabilities = append(append([]string(nil), HubAdminBootstrapCapabilities...), "hub.exec", "hub.update")

// Elevated capabilities require the granting admin to hold that exact capability.
// Phase 1 has none; future hub.exec must be registered here as elevated.
var HubAdminElevatedCapabilities = map[string]bool{"hub.exec": true}

type HubAdminRequest struct {
	ID         string          `json:"id"`
	HubID      string          `json:"hub_id"`
	Nonce      string          `json:"nonce"`
	Generation string          `json:"generation"`
	Method     string          `json:"method"`
	Payload    json.RawMessage `json:"payload"`
	IssuedAt   time.Time       `json:"issued_at"`
	ExpiresAt  time.Time       `json:"expires_at"`
	Signature  []byte          `json:"signature"`
}
type HubAdminResult struct {
	ID         string          `json:"id"`
	Generation string          `json:"generation"`
	Status     int             `json:"status"`
	Code       string          `json:"code,omitempty"`
	Error      string          `json:"error,omitempty"`
	Body       json.RawMessage `json:"body,omitempty"`
}

func (r *HubAdminRequest) signingBytes() []byte {
	digest := sha256.Sum256(r.Payload)
	// JSON quoting unambiguously separates all variable fields.
	raw, _ := json.Marshal([]string{"tclaude-hub-admin-v1", r.HubID, r.Nonce, r.Generation, r.ID, r.Method, r.IssuedAt.UTC().Format(time.RFC3339Nano), r.ExpiresAt.UTC().Format(time.RFC3339Nano), hex.EncodeToString(digest[:])})
	return raw
}
func (r *HubAdminRequest) Sign(id *Identity) { r.Signature = ed25519.Sign(id.Priv, r.signingBytes()) }
func (r *HubAdminRequest) Verify(pub ed25519.PublicKey, hubID, nonce string, now time.Time) error {
	if !ValidStreamID(r.ID) || r.HubID != hubID || r.Nonce != nonce || len(r.Method) > 64 || len(r.Payload) > MaxAdminPayload || !json.Valid(r.Payload) {
		return fmt.Errorf("invalid hub admin request")
	}
	if r.IssuedAt.After(now.Add(30*time.Second)) || !r.ExpiresAt.After(now) || r.ExpiresAt.Before(r.IssuedAt) || r.ExpiresAt.Sub(r.IssuedAt) > time.Minute {
		return fmt.Errorf("hub admin request expired or outside validity window")
	}
	if len(pub) != ed25519.PublicKeySize || !ed25519.Verify(pub, r.signingBytes(), r.Signature) {
		return fmt.Errorf("hub admin signature does not verify")
	}
	return nil
}
