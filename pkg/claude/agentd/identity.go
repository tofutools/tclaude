package agentd

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// peerKey is the context key under which we stash the resolved peer
// identity for the lifetime of a request.
type peerKey struct{}

// peer is the identity resolved from the connecting socket peer.
//
//   - PID is the process that opened the socket. 0 if peerPID failed.
//   - ConvID is the current conv-id of the nearest claude/node ancestor,
//     read from ~/.claude/sessions/<pid>.json. Empty when the caller has
//     no claude ancestor *or* when the ancestor's session file couldn't
//     be read.
//   - HasClaudeAncestor is true iff a claude/node ancestor was observed
//     anywhere in the pid tree, regardless of session-file readability.
//     This is what `requireHuman` actually checks: the human is a
//     positive assertion (PID known AND no CC ancestor), not the
//     absence of a successful conv-id resolution.
type peer struct {
	PID               int
	ConvID            string
	HasClaudeAncestor bool
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
				p.ConvID, p.HasClaudeAncestor = convIDForPID(pid)
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

// requireHuman gates mutating endpoints to the local human only. We
// require *both* a known PID and the absence of any claude/node
// ancestor in the caller's process tree. This is a positive
// "definitely human" assertion: an agent whose per-pid session file is
// missing/unreadable still has a CC ancestor and is therefore *not*
// the human, even though `ConvID` is empty in that case.
func requireHuman(w http.ResponseWriter, r *http.Request) bool {
	p := peerFromContext(r.Context())
	if p.PID == 0 {
		writeError(w, http.StatusUnauthorized, "auth",
			"could not determine peer PID; refusing to grant human privileges")
		return false
	}
	if p.HasClaudeAncestor {
		msg := "this endpoint is restricted to the human"
		if p.ConvID != "" {
			msg = fmt.Sprintf("%s (caller is conv %s)", msg, p.ConvID)
		} else {
			msg += " (caller has a Claude Code ancestor in its pid tree)"
		}
		writeError(w, http.StatusForbidden, "auth", msg)
		return false
	}
	return true
}

// requirePermission gates an endpoint behind a named agent permission.
//
// Humans (no claude ancestor) always pass. Agents pass only when the
// active config grants them perm via DefaultPermissions or
// PermissionOverrides[<conv-id|prefix|title>]. On denial the response
// is 403 with the permission slug in the message body so the caller
// can explain to its user what to grant.
//
// Returns (convID, true) on success — convID is "" for the human path,
// the resolved conv-id for an agent. On failure the response is
// already written; the caller just returns.
func requirePermission(w http.ResponseWriter, r *http.Request, perm string) (string, bool) {
	p := peerFromContext(r.Context())
	if p.PID == 0 {
		writeError(w, http.StatusUnauthorized, "auth",
			"could not determine peer PID; refusing to evaluate permission")
		return "", false
	}
	if !p.HasClaudeAncestor {
		// The human is implicitly allowed everything.
		return "", true
	}
	if p.ConvID == "" {
		writeError(w, http.StatusForbidden, "auth",
			"caller has a Claude Code ancestor but no resolvable conv-id; cannot evaluate permission")
		return "", false
	}
	cfg, _ := config.Load()
	title := ""
	if row := agent.FreshConvRow(p.ConvID); row != nil {
		title = agent.DisplayTitle(row)
	}
	if !cfg.HasAgentPermission(p.ConvID, title, perm) {
		writeError(w, http.StatusForbidden, "permission",
			fmt.Sprintf("caller is not granted permission %q (grant via agent.default_permissions or agent.permission_overrides in ~/.tclaude/config.json)", perm))
		return "", false
	}
	return p.ConvID, true
}

// convIDForPID walks up from pid to the nearest claude/node ancestor.
// Returns the ancestor's current `sessionId` if its per-pid session
// file is readable, plus a flag indicating whether any claude/node was
// observed at all. Callers use the flag to distinguish "really the
// human" (no ancestor) from "agent we can't identify" (ancestor present
// but session file unreadable).
func convIDForPID(pid int) (convID string, hasAncestor bool) {
	cur := pid
	for cur > 1 {
		name := session.GetProcessName(cur)
		if name == "claude" || name == "node" {
			hasAncestor = true
			if id := readSessionFile(cur); id != "" {
				return id, true
			}
		}
		cur = session.GetParentPID(cur)
	}
	return "", hasAncestor
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
