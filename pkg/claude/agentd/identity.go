package agentd

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
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
//     This is what `requirePermission` checks: humans (no CC ancestor)
//     bypass permission checks; agents with a CC ancestor must hold
//     the requested slug to pass.
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
//
// Resolving a non-empty conv-id also opportunistically flushes any
// nudges queued for this conv while it was offline. The flush is
// debounced per-conv and runs on its own goroutine, so chatty agents
// don't pay any latency on the request that triggered it.
func withIdentity(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := &peer{}
		if uconn, ok := r.Context().Value(unixConnKey{}).(*net.UnixConn); ok && uconn != nil {
			if pid, err := peerPID(uconn); err == nil {
				p.PID = pid
				p.ConvID, p.HasClaudeAncestor = convIDForPID(pid)
			}
		}
		if p.ConvID != "" {
			maybeFlushUndelivered(p.ConvID)
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

// Permission slugs are simple dotted strings the daemon accepts in
// `agent.default_permissions` / `agent.permission_overrides`. Keep this
// list in sync with the agent-coord skill / docs.
const (
	PermSelfRename        = "self.rename"
	PermSelfCompact       = "self.compact"
	PermSelfReincarnate   = "self.reincarnate"
	PermAgentReincarnate  = "agent.reincarnate"
	PermGroupsCreate      = "groups.create"
	PermGroupsRm          = "groups.rm"
	PermGroupsStop        = "groups.stop"
	PermGroupsResume      = "groups.resume"
	PermGroupsSpawn       = "groups.spawn"
	PermGroupsOwn         = "groups.own"
	PermMemberAdd         = "member.add"
	PermMemberRemove      = "member.remove"
	PermMemberRedesignate = "member.redesignate"
)

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
	row := agent.FreshConvRow(p.ConvID)
	if row != nil {
		title = agent.DisplayTitle(row)
	}
	slog.Debug("requirePermission: resolved caller",
		"conv", p.ConvID, "row_present", row != nil, "title", title, "perm", perm)
	// Per-agent grants live in SQLite (table agent_permissions);
	// cfg only carries the global defaults list. Check the DB first,
	// then fall back to defaults.
	allowed := cfg.HasDefaultPermission(perm)
	if !allowed {
		if ok, err := db.HasAgentPermissionRow(p.ConvID, perm); err == nil && ok {
			allowed = true
		}
	}
	if !allowed {
		// Permission denied. If the caller asked for a human-override
		// popup (via X-Tclaude-Ask-Human: <duration>), open one and
		// block on the decision. Timeout = deny, so a doomed agent can
		// never get stuck waiting forever.
		if timeout := parseAskHumanHeader(r); timeout > 0 && popupBaseURL != "" {
			// Snapshot the body now so the popup can show what's being
			// approved. snapshotRequestBody replaces r.Body with a
			// fresh reader so the downstream handler still gets the
			// same bytes after we approve.
			bodyPreview := snapshotRequestBody(r)
			targetGroup, targetConvID, targetConvTitle := extractApprovalTargets(r, bodyPreview)
			req := &approvalRequest{
				id:              newApprovalID(),
				perm:            perm,
				convID:          p.ConvID,
				convTitle:       title,
				method:          r.Method,
				path:            r.URL.Path,
				rawQuery:        r.URL.RawQuery,
				bodyPreview:     bodyPreview,
				targetGroup:     targetGroup,
				targetConvID:    targetConvID,
				targetConvTitle: targetConvTitle,
				createdAt:       time.Now(),
				timeout:         timeout,
				decision:        make(chan bool, 1),
				extend:          make(chan time.Duration, 1),
			}
			if requestHumanApproval(req, popupBaseURL) {
				return p.ConvID, true
			}
			writeError(w, http.StatusForbidden, "permission",
				fmt.Sprintf("human declined or timed out after %s on permission %q", timeout, perm))
			return "", false
		}
		writeError(w, http.StatusForbidden, "permission",
			fmt.Sprintf("caller is not granted permission %q (grant via agent.default_permissions or agent.permission_overrides in ~/.tclaude/config.json; or call again with X-Tclaude-Ask-Human: <duration> to ask the human via popup)", perm))
		return "", false
	}
	return p.ConvID, true
}

// extractApprovalTargets parses the request URL + JSON body to surface
// the action's target group / target conv-id, so the popup can show
// the human concrete names rather than "Endpoint: PATCH /v1/groups/foo/members/abcd".
//
// Returns (groupName, targetConvID, targetConvTitle). Empty strings
// when there's nothing useful to display (e.g. /v1/whoami/rename has
// no group and no separate target — the requester is the target).
func extractApprovalTargets(r *http.Request, bodyPreview string) (group, targetConvID, targetConvTitle string) {
	const groupsPrefix = "/v1/groups/"
	if strings.HasPrefix(r.URL.Path, groupsPrefix) {
		rest := strings.TrimPrefix(r.URL.Path, groupsPrefix)
		parts := strings.SplitN(rest, "/", 3)
		if len(parts) >= 1 && parts[0] != "" {
			if g, err := url.PathUnescape(parts[0]); err == nil {
				group = g
			} else {
				group = parts[0]
			}
		}
		// /v1/groups/{name}/members/{conv} => target is parts[2].
		if len(parts) >= 3 && parts[1] == "members" && parts[2] != "" {
			selector := parts[2]
			if u, err := url.PathUnescape(selector); err == nil {
				selector = u
			}
			if res, _, err := agent.ResolveSelector(selector); err == nil {
				targetConvID = res.ConvID
				if res.Row != nil {
					targetConvTitle = agent.DisplayTitle(res.Row)
				}
			}
		}
	}
	// POST /v1/groups/{name}/members carries the target conv in the JSON
	// body's "conv" field. Parse the snapshot we already buffered.
	if targetConvID == "" && r.Method == http.MethodPost && bodyPreview != "" {
		var body struct {
			Conv string `json:"conv"`
		}
		if err := json.Unmarshal([]byte(bodyPreview), &body); err == nil && body.Conv != "" {
			if res, _, err := agent.ResolveSelector(body.Conv); err == nil {
				targetConvID = res.ConvID
				if res.Row != nil {
					targetConvTitle = agent.DisplayTitle(res.Row)
				}
			}
		}
	}
	return group, targetConvID, targetConvTitle
}

// parseAskHumanHeader reads the X-Tclaude-Ask-Human header. Empty/absent
// => 0 (no popup). Bare integers are seconds; everything else is parsed
// via time.ParseDuration. Hard cap at 300s — popups blocking longer than
// that defeat the "agents don't get stuck" goal of having a timeout in
// the first place.
func parseAskHumanHeader(r *http.Request) time.Duration {
	v := strings.TrimSpace(r.Header.Get("X-Tclaude-Ask-Human"))
	if v == "" {
		return 0
	}
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		if d > 300*time.Second {
			d = 300 * time.Second
		}
		return d
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		if n > 300 {
			n = 300
		}
		return time.Duration(n) * time.Second
	}
	return 0
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
