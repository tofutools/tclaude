package agentd

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"

	"github.com/tofutools/tclaude/pkg/claude/session"
)

// peerKey is the context key under which we stash the resolved peer
// identity for the lifetime of a request.
type peerKey struct{}

// peer is the identity resolved from the connecting socket peer.
//
//   - PID is the process that opened the socket. Always present.
//   - ConvID is the current conv-id of the nearest claude/node ancestor,
//     read from ~/.claude/sessions/<pid>.json. Empty when the caller has
//     no claude ancestor (i.e. the local human running tclaude/curl from
//     a plain shell).
type peer struct {
	PID    int
	ConvID string
}

// peerFromContext returns the peer attached by the identity middleware.
// Always non-nil for handlers; PID may be 0 if the lookup failed.
func peerFromContext(ctx context.Context) *peer {
	v, _ := ctx.Value(peerKey{}).(*peer)
	if v == nil {
		return &peer{}
	}
	return v
}

// withIdentity is the per-request middleware that resolves the connecting
// peer's PID, walks the process tree to a claude/node ancestor, reads its
// per-pid session file, and attaches the result to the request context.
func withIdentity(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := &peer{}
		if uconn, ok := r.Context().Value(unixConnKey{}).(*net.UnixConn); ok && uconn != nil {
			if pid, err := peerPID(uconn); err == nil {
				p.PID = pid
				p.ConvID = convIDForPID(pid)
			}
		}
		ctx := context.WithValue(r.Context(), peerKey{}, p)
		h.ServeHTTP(w, r.WithContext(ctx))
	})
}

// requireAgent enforces that the caller is an agent (i.e. has a resolved
// conv-id). Returns the conv-id and true on success, or writes 401 and
// returns false.
func requireAgent(w http.ResponseWriter, r *http.Request) (string, bool) {
	p := peerFromContext(r.Context())
	if p.ConvID == "" {
		writeError(w, http.StatusUnauthorized, "auth", "no claude ancestor in caller's process tree; this endpoint requires an agent identity")
		return "", false
	}
	return p.ConvID, true
}

// requireHuman enforces that the caller is *not* an agent — used for
// mutating endpoints. Identity is established by absence: if the peer
// has no claude ancestor, we treat them as the local human.
func requireHuman(w http.ResponseWriter, r *http.Request) bool {
	p := peerFromContext(r.Context())
	if p.ConvID != "" {
		writeError(w, http.StatusForbidden, "auth",
			fmt.Sprintf("this endpoint is restricted to the human (caller is conv %s)", p.ConvID))
		return false
	}
	return true
}

// convIDForPID walks up from pid to the nearest claude/node ancestor,
// then reads ~/.claude/sessions/<ancestor-pid>.json for `sessionId`.
// Returns "" if no ancestor or pid file unavailable.
func convIDForPID(pid int) string {
	cur := pid
	for cur > 1 {
		name := session.GetProcessName(cur)
		if name == "claude" || name == "node" {
			if id := readSessionFile(cur); id != "" {
				return id
			}
		}
		cur = session.GetParentPID(cur)
	}
	return ""
}

// readSessionFile loads ~/.claude/sessions/<pid>.json and returns
// `sessionId`, or "" on any error.
func readSessionFile(pid int) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	path := filepath.Join(home, ".claude", "sessions", fmt.Sprintf("%d.json", pid))
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return ""
	}
	id, _ := m["sessionId"].(string)
	return id
}

// unixConnKey is how we smuggle the connection's *net.UnixConn into per-request
// context, since net/http hides the underlying conn from handlers. The Server's
// ConnContext hook puts it there.
type unixConnKey struct{}
