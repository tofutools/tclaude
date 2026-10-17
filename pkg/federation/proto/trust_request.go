package proto

// KindTrustRequest asks an instance to trust the sender. It is the second
// bootstrap exception (after enrollment) that an untrusted directory peer may
// deliver: it is small, signed and sealed, carries no authority, and only
// becomes a note on the receiving operator's dashboard. Trusting the sender
// still goes through the receiver's own fingerprint check.
const KindTrustRequest = "trust_request"

// MaxTrustRequestBytes caps the sealed envelope before any signature or
// decryption work; MaxTrustRequestNote caps the note in runes.
const (
	MaxTrustRequestBytes = 4096
	MaxTrustRequestNote  = 200
)

// TrustRequest is the payload. TrustsYou says the sender already trusts the
// receiver (a "trust me back"); it is the sender's own claim.
type TrustRequest struct {
	Note      string `json:"note,omitempty"`
	TrustsYou bool   `json:"trusts_you,omitempty"`
}
