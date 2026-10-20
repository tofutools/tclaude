package agentd

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// A teleport onto the sender operator's own Claude subscription carries only
// the mode in its offer. The sender's dashboard then sends the token through
// the encrypted peer view (POST /api/teleport-subscription, filled in by the
// sender's own node), held here in memory by offer until the landing takes it.

// claudeSubscriptionTeleportWait bounds how long an automatic landing waits
// for the token before it lands the agent offline instead.
const claudeSubscriptionTeleportWait = 2 * time.Minute

// claudeSubscriptionOfferTTL bounds how long an unclaimed token is held.
const claudeSubscriptionOfferTTL = 15 * time.Minute

// maxClaudeSubscriptionOffers caps the tokens one peer may park here.
const maxClaudeSubscriptionOffers = 8

var claudeSubscriptionOffers = struct {
	sync.Mutex
	held map[string]claudeSubscriptionPending // peer + "/" + offer
}{held: map[string]claudeSubscriptionPending{}}

func offerKey(peer, offer string) string { return peer + "/" + offer }

func holdOfferClaudeSubscription(peer, offer string, h claudeSubscriptionHandoff) bool {
	claudeSubscriptionOffers.Lock()
	defer claudeSubscriptionOffers.Unlock()
	now := time.Now()
	count := 0
	for key, p := range claudeSubscriptionOffers.held {
		if now.After(p.expires) {
			delete(claudeSubscriptionOffers.held, key)
		} else if p.peer == peer && key != offerKey(peer, offer) {
			count++
		}
	}
	if count >= maxClaudeSubscriptionOffers {
		return false
	}
	claudeSubscriptionOffers.held[offerKey(peer, offer)] = claudeSubscriptionPending{h, now.Add(claudeSubscriptionOfferTTL)}
	return true
}

func offerClaudeSubscriptionHeld(peer, offer string) bool {
	claudeSubscriptionOffers.Lock()
	defer claudeSubscriptionOffers.Unlock()
	p, ok := claudeSubscriptionOffers.held[offerKey(peer, offer)]
	return ok && time.Now().Before(p.expires)
}

func takeOfferClaudeSubscription(peer, offer string) (claudeSubscriptionHandoff, bool) {
	claudeSubscriptionOffers.Lock()
	defer claudeSubscriptionOffers.Unlock()
	p, ok := claudeSubscriptionOffers.held[offerKey(peer, offer)]
	delete(claudeSubscriptionOffers.held, offerKey(peer, offer))
	if !ok || time.Now().After(p.expires) {
		return claudeSubscriptionHandoff{}, false
	}
	return p.claudeSubscriptionHandoff, true
}

// servePeerTeleportSubscription takes the sender's token for its teleport
// offer. When the agent already landed offline without it, the token goes to
// that agent instead, ready for its next start.
func servePeerTeleportSubscription(w http.ResponseWriter, r *http.Request, v *peerView, _ peerViewRule) {
	token, ok := liftClaudeSubscriptionToken(w, r)
	if !ok {
		return
	}
	var in struct {
		Offer string `json:"offer"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&in) != nil || in.Offer == "" || len(in.Offer) > 128 {
		writeError(w, 400, "invalid_arg", "offer is required")
		return
	}
	if token == "" {
		writeError(w, 400, "invalid_arg", "no Claude subscription token was sent; update tclaude on the sending node")
		return
	}
	h := claudeSubscriptionHandoff{peer: v.peer.InstanceID, token: token}
	if row, err := db.GetFederationTeleport("in", v.peer.InstanceID, in.Offer); err == nil && row != nil {
		if row.Credentials != claudeSubscriptionMode {
			writeError(w, 409, "not_subscription", "that teleport does not run on your Claude subscription")
			return
		}
		if row.TargetAgent != "" {
			if marker, err := db.GetAgentClaudeSubscription(row.TargetAgent); err == nil && marker != nil && marker.Peer == v.peer.InstanceID {
				if err := holdClaudeSubscription(row.TargetAgent, h); err != nil {
					writeError(w, 503, "unavailable", "could not hold the token")
					return
				}
				recordFederationAudit("federation.claude_subscription.teleport", v.peer.InstanceID, "", "", "agent="+row.TargetAgent, 200)
				writeJSON(w, 200, map[string]any{"held": "agent", "agent_id": row.TargetAgent})
				return
			}
		}
	}
	if !holdOfferClaudeSubscription(v.peer.InstanceID, in.Offer, h) {
		writeError(w, 429, "busy", "too many teleports wait for your Claude subscription here")
		return
	}
	recordFederationAudit("federation.claude_subscription.teleport", v.peer.InstanceID, "", "", "offer="+in.Offer, 200)
	goBackground(reconcileFederationMoves)
	writeJSON(w, 200, map[string]any{"held": "offer"})
}
