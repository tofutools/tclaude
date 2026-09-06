// Package transport exposes application operations without owning domain policy.
package transport

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"

	"github.com/tofutools/tclaude/internal/backend/model"
)

var ErrUnauthenticated = errors.New("authentication required")

// Authenticator resolves a trusted caller independently of request JSON.
// Runtime-agent authentication is supplied by the host composition, not by
// accepting an AgentID or principal claimed in an HTTP body.
type Authenticator interface {
	Authenticate(*http.Request) (model.Principal, error)
}

// OperatorToken authenticates a locally provisioned operator credential. Its
// deployment must restrict the listener to a private local socket or TLS.
type OperatorToken struct{ digest [sha256.Size]byte }

func NewOperatorToken(secret string) (*OperatorToken, error) {
	if len(secret) < 32 || strings.TrimSpace(secret) != secret {
		return nil, errors.New("operator credential must contain at least 32 bytes without surrounding whitespace")
	}
	return &OperatorToken{digest: sha256.Sum256([]byte(secret))}, nil
}

func (a *OperatorToken) Authenticate(r *http.Request) (model.Principal, error) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 || !strings.HasPrefix(values[0], "Bearer ") {
		return model.Principal{}, ErrUnauthenticated
	}
	digest := sha256.Sum256([]byte(strings.TrimPrefix(values[0], "Bearer ")))
	if subtle.ConstantTimeCompare(digest[:], a.digest[:]) != 1 {
		return model.Principal{}, ErrUnauthenticated
	}
	return model.OperatorPrincipal(), nil
}
