package agentd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// A peer operator can run Claude Code here as themselves: their own
// subscription token (claude setup-token) travels in the encrypted peer
// request, stays in this daemon's memory only, and reaches Claude Code
// through an inherited pipe (CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR), never
// the environment, argv or disk. The agent's durable marker records only
// whose subscription it is, so a relaunch after this daemon restarts refuses
// rather than falling back to this node's own login.

// claudeSubscriptionMode marks a spawn request whose worker runs on the
// requesting operator's subscription. It is a mode, never the token.
const claudeSubscriptionMode = "claude-subscription"

func validClaudeSubscriptionToken(token string) bool {
	if !strings.HasPrefix(token, claudeSubscriptionTokenPrefix) || len(token) > 4096 {
		return false
	}
	for _, c := range token {
		if c <= ' ' || c > '~' {
			return false
		}
	}
	return true
}

type claudeSubscriptionHandoff struct {
	peer  string
	token string
}

type claudeSubscriptionCtxKey struct{}

func withClaudeSubscription(ctx context.Context, h claudeSubscriptionHandoff) context.Context {
	return context.WithValue(ctx, claudeSubscriptionCtxKey{}, h)
}

// claudeSubscriptionForSpawn is the token a peer request handed to this
// spawn, or none.
func claudeSubscriptionForSpawn(ctx context.Context) claudeSubscriptionHandoff {
	h, _ := ctx.Value(claudeSubscriptionCtxKey{}).(claudeSubscriptionHandoff)
	return h
}

var claudeSubscriptions = struct {
	sync.Mutex
	// agents maps an agent ID to the token it runs on.
	agents map[string]claudeSubscriptionHandoff
	// requests holds a remote spawn request's token until it launches.
	requests map[int64]claudeSubscriptionPending
	// armed records agents with a launch this daemon started that has not
	// taken its token yet. The pane wrapper takes it before starting Claude
	// Code, so neither a tool in the pane nor a launch agentd did not start
	// can take it; each relaunch arms it again.
	armed map[string]bool
}{agents: map[string]claudeSubscriptionHandoff{}, requests: map[int64]claudeSubscriptionPending{}, armed: map[string]bool{}}

type claudeSubscriptionPending struct {
	claudeSubscriptionHandoff
	expires time.Time
}

// holdClaudeSubscription keeps an agent's token in memory and records the
// non-secret marker that makes relaunches insist on it.
func holdClaudeSubscription(agentID string, h claudeSubscriptionHandoff) error {
	if err := db.SetAgentClaudeSubscription(agentID, h.peer); err != nil {
		return err
	}
	claudeSubscriptions.Lock()
	claudeSubscriptions.agents[agentID] = h
	claudeSubscriptions.Unlock()
	return nil
}

func dropClaudeSubscription(agentID string) {
	claudeSubscriptions.Lock()
	delete(claudeSubscriptions.agents, agentID)
	delete(claudeSubscriptions.armed, agentID)
	claudeSubscriptions.Unlock()
}

// heldClaudeSubscription forgets a retired agent's token: retirement ends
// its use here, and nothing relaunches a retired agent on it.
func heldClaudeSubscription(agentID string) (claudeSubscriptionHandoff, bool) {
	claudeSubscriptions.Lock()
	h, ok := claudeSubscriptions.agents[agentID]
	claudeSubscriptions.Unlock()
	if !ok {
		return h, false
	}
	if a, err := db.GetAgent(agentID); err == nil && a != nil && !a.Active() {
		dropClaudeSubscription(agentID)
		return claudeSubscriptionHandoff{}, false
	}
	return h, true
}

func holdRequestClaudeSubscription(request int64, h claudeSubscriptionHandoff) {
	claudeSubscriptions.Lock()
	defer claudeSubscriptions.Unlock()
	now := time.Now()
	for id, p := range claudeSubscriptions.requests {
		if now.After(p.expires) {
			delete(claudeSubscriptions.requests, id)
		}
	}
	claudeSubscriptions.requests[request] = claudeSubscriptionPending{h, now.Add(fedSpawnTTL)}
}

func takeRequestClaudeSubscription(request int64) (claudeSubscriptionHandoff, bool) {
	claudeSubscriptions.Lock()
	defer claudeSubscriptions.Unlock()
	p, ok := claudeSubscriptions.requests[request]
	delete(claudeSubscriptions.requests, request)
	if !ok || time.Now().After(p.expires) {
		return claudeSubscriptionHandoff{}, false
	}
	return p.claudeSubscriptionHandoff, true
}

// claudeSubscriptionLostError explains a relaunch that would otherwise
// quietly use this node's own Claude login. Its code lets the dashboard offer
// the owner to send the token again, or anyone to switch the agent to this
// node's own login.
type claudeSubscriptionLostError struct {
	peer string
	msg  string
}

func (e *claudeSubscriptionLostError) Error() string { return e.msg }

// claudeSubscriptionLostCode is the error code (and resume action suffix)
// for a relaunch refused because the token is gone.
const claudeSubscriptionLostCode = "claude_subscription_lost"

func errClaudeSubscriptionLost(peer string) error {
	who := peer
	if p, _ := db.GetFederationPeer(peer); p != nil {
		who = peerDisplay(p)
	}
	return &claudeSubscriptionLostError{peer: peer, msg: fmt.Sprintf("this agent runs on %s's Claude subscription, and this node no longer holds the token (it is kept in memory only, so a daemon restart forgets it); %s can send it again, or switch the agent to this node's own login", who, who)}
}

// isClaudeSubscriptionLost reports a launch refused for a missing token.
func isClaudeSubscriptionLost(err error) bool {
	var lost *claudeSubscriptionLostError
	return errors.As(err, &lost)
}

// claudeSubscriptionRelaunchBlocked is the refusal a relaunch of agentID
// would meet, checked before anything is stopped.
func claudeSubscriptionRelaunchBlocked(agentID string) error {
	marker, err := db.GetAgentClaudeSubscription(agentID)
	if err != nil || marker == nil {
		return nil
	}
	if _, ok := heldClaudeSubscription(agentID); ok {
		return nil
	}
	return errClaudeSubscriptionLost(marker.Peer)
}

// applyClaudeSubscription marks a launch of an agent that runs on a peer's
// subscription, so the pane wrapper fetches and hands over the token. It
// refuses when the token is gone instead of launching on this node's login.
func applyClaudeSubscription(a *clcommon.SpawnArgs) error {
	agentID := a.ClaudeSubscriptionAgentID
	if agentID == "" {
		agentID = a.AgentID
	}
	if agentID == "" && a.ConvID != "" {
		agentID, _ = db.AgentIDForConv(a.ConvID)
	}
	if agentID == "" {
		return nil
	}
	marker, err := db.GetAgentClaudeSubscription(agentID)
	if err != nil {
		return fmt.Errorf("could not read the agent's Claude subscription marker: %w", err)
	}
	if marker == nil {
		return nil
	}
	if a.Harness != "" && a.Harness != harness.DefaultName {
		return errors.New("an agent on a peer's Claude subscription runs Claude Code only")
	}
	if a.ModelProxy != "" && a.ModelProxy != "off" {
		return errors.New("an agent on a peer's Claude subscription cannot also use a model gateway")
	}
	if _, ok := heldClaudeSubscription(agentID); !ok {
		return errClaudeSubscriptionLost(marker.Peer)
	}
	claudeSubscriptions.Lock()
	claudeSubscriptions.armed[agentID] = true
	claudeSubscriptions.Unlock()
	a.ClaudeSubscription = true
	return nil
}

// handleClaudeSubscriptionToken gives the pane wrapper of a verified live
// launch its agent's token, once per launch agentd armed.
func handleClaudeSubscriptionToken(w http.ResponseWriter, r *http.Request) {
	row, _ := r.Context().Value(httpProxyLaunchRowKey{}).(*db.SessionRow)
	if row == nil {
		writeError(w, 403, "auth", "the Claude subscription token requires a verified live launch pane")
		return
	}
	agentID := modelLeaseWorker(row)
	h, ok := heldClaudeSubscription(agentID)
	if agentID == "" || !ok {
		writeError(w, 404, "not_found", "no Claude subscription token is held for this launch")
		return
	}
	claudeSubscriptions.Lock()
	armed := claudeSubscriptions.armed[agentID]
	delete(claudeSubscriptions.armed, agentID)
	claudeSubscriptions.Unlock()
	if !armed {
		writeError(w, 409, "conflict", "no launch of this agent started by tclaude is waiting for its Claude subscription token")
		return
	}
	recordFederationAudit("federation.claude_subscription.use", h.peer, row.ConvID, "", "agent="+agentID+" session="+row.ID, 200)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]string{"token": h.token})
}

// liftClaudeSubscriptionToken removes claude_subscription_token from a JSON
// request body and returns it; any other body passes through untouched.
func liftClaudeSubscriptionToken(w http.ResponseWriter, r *http.Request) (string, bool) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<20))
	if err != nil {
		writeError(w, 413, "invalid_arg", "request body is too large or unreadable")
		return "", false
	}
	r.Body = io.NopCloser(bytes.NewReader(data))
	if !bytes.Contains(data, []byte(`"claude_subscription_token"`)) {
		return "", true
	}
	var fields map[string]json.RawMessage
	var token string
	if json.Unmarshal(data, &fields) != nil || json.Unmarshal(fields["claude_subscription_token"], &token) != nil || !validClaudeSubscriptionToken(token) {
		writeError(w, 400, "invalid_arg", "the Claude subscription token is not a claude setup-token")
		return "", false
	}
	delete(fields, "claude_subscription_token")
	if data, err = json.Marshal(fields); err != nil {
		writeError(w, 400, "invalid_arg", "invalid spawn body")
		return "", false
	}
	r.Body = io.NopCloser(bytes.NewReader(data))
	r.ContentLength = int64(len(data))
	return token, true
}

// memberClaudeSubscription is what a dashboard row says about it: whose
// subscription, and whether this node still holds the token to relaunch.
type memberClaudeSubscription struct {
	Peer  string `json:"peer"`
	Label string `json:"label,omitempty"`
	Held  bool   `json:"held"`
}

type claudeSubscriptionMarkers map[string]string

func snapshotClaudeSubscriptions() claudeSubscriptionMarkers {
	m, err := db.ListAgentClaudeSubscriptions()
	if err != nil {
		return nil
	}
	return m
}

func (m claudeSubscriptionMarkers) forAgent(agentID string) *memberClaudeSubscription {
	peer, ok := m[agentID]
	if !ok || agentID == "" {
		return nil
	}
	out := &memberClaudeSubscription{Peer: peer}
	if p, _ := db.GetFederationPeer(peer); p != nil {
		out.Label = peerDisplay(p)
	}
	_, out.Held = heldClaudeSubscription(agentID)
	return out
}
