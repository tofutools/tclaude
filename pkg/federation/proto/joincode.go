package proto

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
	"unicode"
)

// JoinCodePrefix marks a join code: everything a node needs to join a hub,
// pasted as one string instead of a hub URL, an invite and a CA file.
const JoinCodePrefix = "tchj_"

// InvitePrefix marks a hub invite token.
const InvitePrefix = "tchi_"

// MaxJoinCodeBytes bounds an encoded join code. A CA chain of a few
// certificates fits with room to spare.
const MaxJoinCodeBytes = 32 << 10

// JoinCode is the decoded form. Hub is the address nodes dial; Invite is the
// single-use admission token, absent when the admin admits nodes by instance
// ID; CA is a PEM bundle to trust for the hub's TLS certificate instead of
// the system roots, for a hub with a private or self-signed certificate.
// Space and Expires describe the invite for display only: the hub decides.
//
// A join code carries a bearer invite and decides which certificate the
// node trusts for its hub, so it is a secret from a trusted hub admin, never
// something to accept from anyone else.
type JoinCode struct {
	V       int       `json:"v"`
	Hub     string    `json:"hub"`
	Invite  string    `json:"invite,omitempty"`
	Space   string    `json:"space,omitempty"`
	Expires time.Time `json:"exp,omitzero"`
	CA      string    `json:"ca,omitempty"`
}

// ErrJoinCode is returned for anything that is not a well-formed join code.
var ErrJoinCode = errors.New("invalid join code")

// EncodeJoinCode validates c and encodes it.
func EncodeJoinCode(c JoinCode) (string, error) {
	c.V = 1
	if err := c.validate(); err != nil {
		return "", err
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	code := JoinCodePrefix + base64.RawURLEncoding.EncodeToString(raw)
	if len(code) > MaxJoinCodeBytes {
		return "", fmt.Errorf("%w: longer than %d bytes (CA bundle too large)", ErrJoinCode, MaxJoinCodeBytes)
	}
	return code, nil
}

// IsJoinCode says whether s looks like a join code (it may still be invalid).
func IsJoinCode(s string) bool {
	return strings.HasPrefix(strings.TrimSpace(s), JoinCodePrefix)
}

// DecodeJoinCode parses a join code. Whitespace anywhere is ignored, since a
// pasted code is often wrapped across lines.
func DecodeJoinCode(s string) (JoinCode, error) {
	s = strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
	if len(s) > MaxJoinCodeBytes {
		return JoinCode{}, fmt.Errorf("%w: longer than %d bytes", ErrJoinCode, MaxJoinCodeBytes)
	}
	body, ok := strings.CutPrefix(s, JoinCodePrefix)
	if !ok {
		return JoinCode{}, fmt.Errorf("%w: it does not start with %s", ErrJoinCode, JoinCodePrefix)
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return JoinCode{}, fmt.Errorf("%w: it is cut short or altered", ErrJoinCode)
	}
	var c JoinCode
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return JoinCode{}, fmt.Errorf("%w: it is cut short or altered", ErrJoinCode)
	}
	if c.V != 1 {
		return JoinCode{}, fmt.Errorf("%w: version %d is not supported by this tclaude; update it", ErrJoinCode, c.V)
	}
	if err := c.validate(); err != nil {
		return JoinCode{}, err
	}
	return c, nil
}

func (c JoinCode) validate() error {
	u, err := url.Parse(c.Hub)
	if err != nil || (u.Scheme != "ws" && u.Scheme != "wss") || u.Host == "" {
		return fmt.Errorf("%w: hub address %q is not ws:// or wss://host[:port]", ErrJoinCode, c.Hub)
	}
	// The same rule the hub client applies: plain ws only to loopback.
	if host := u.Hostname(); u.Scheme == "ws" && host != "localhost" && !isLoopbackIP(host) {
		return fmt.Errorf("%w: plain ws:// is only allowed to loopback; use wss:// for %s", ErrJoinCode, host)
	}
	if c.Invite != "" && (!strings.HasPrefix(c.Invite, InvitePrefix) || strings.ContainsFunc(c.Invite, unicode.IsSpace)) {
		return fmt.Errorf("%w: the invite is not a %s… token", ErrJoinCode, InvitePrefix)
	}
	if c.CA != "" {
		if _, err := ParseCABundle(c.CA); err != nil {
			return fmt.Errorf("%w: %v", ErrJoinCode, err)
		}
	}
	return nil
}

func isLoopbackIP(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// CertInfo describes one certificate of a CA bundle, for the operator to
// compare with what the hub admin reads them.
type CertInfo struct {
	Subject    string    `json:"subject"`
	SHA256     string    `json:"sha256"`
	NotAfter   time.Time `json:"not_after"`
	SelfSigned bool      `json:"self_signed"`
}

// ParseCABundle reads a PEM bundle of certificates: at least one, and
// nothing but certificates.
func ParseCABundle(bundle string) ([]CertInfo, error) {
	rest := []byte(bundle)
	var out []CertInfo
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("the CA bundle holds a %s block; only certificates belong in it", block.Type)
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("the CA bundle has an unreadable certificate: %v", err)
		}
		sum := sha256.Sum256(cert.Raw)
		hexed := make([]string, len(sum))
		for i, b := range sum {
			hexed[i] = fmt.Sprintf("%02X", b)
		}
		subject := cert.Subject.CommonName
		if subject == "" {
			subject = cert.Subject.String()
		}
		out = append(out, CertInfo{
			Subject: subject, SHA256: strings.Join(hexed, ":"), NotAfter: cert.NotAfter,
			SelfSigned: cert.CheckSignatureFrom(cert) == nil && cert.Subject.String() == cert.Issuer.String(),
		})
	}
	if len(strings.TrimSpace(string(rest))) != 0 {
		return nil, errors.New("the CA bundle has text that is not a PEM certificate")
	}
	if len(out) == 0 {
		return nil, errors.New("the CA bundle has no certificates")
	}
	return out, nil
}
