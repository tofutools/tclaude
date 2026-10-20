package agentd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// The origin side of running as yourself on another node: this node's
// operator keeps their own Claude subscription token (claude setup-token)
// here, written once to a 0600 file and never read back to any page, and
// chooses it per remote spawn. The dashboard's peer proxy then puts it into
// that spawn's encrypted request.

func claudeSubscriptionTokenPath() string {
	return filepath.Join(config.DataDir(), "claude-subscription.token")
}

func readClaudeSubscriptionToken() (string, time.Time, error) {
	path := claudeSubscriptionTokenPath()
	info, err := os.Stat(path)
	if err != nil {
		return "", time.Time{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", time.Time{}, err
	}
	token := strings.TrimSpace(string(data))
	if !validClaudeSubscriptionToken(token) {
		return "", time.Time{}, errors.New("the stored Claude subscription token is invalid; set it again")
	}
	return token, info.ModTime(), nil
}

// handleClaudeSubscription reads (whether one is set, and since when), sets
// or forgets this operator's own subscription token.
func handleClaudeSubscription(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "manage your Claude subscription token") {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	switch r.Method {
	case http.MethodPut:
		var in struct {
			Token string `json:"token"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&in) != nil {
			writeError(w, 400, "invalid_arg", "invalid body")
			return
		}
		token := strings.TrimSpace(in.Token)
		if strings.HasPrefix(token, "sk-ant-api") {
			writeError(w, 400, "invalid_arg", "this is an Anthropic API key; paste the token that claude setup-token printed (it starts with sk-ant-oat)")
			return
		}
		if !validClaudeSubscriptionToken(token) {
			writeError(w, 400, "invalid_arg", "paste the token that claude setup-token printed (it starts with sk-ant-oat)")
			return
		}
		if err := writeClaudeSubscriptionToken(token); err != nil {
			writeError(w, 503, "unavailable", "could not store the token")
			return
		}
		recordFederationAudit("federation.claude_subscription.set", "", "", "", "stored", 200)
	case http.MethodDelete:
		if err := os.Remove(claudeSubscriptionTokenPath()); err != nil && !os.IsNotExist(err) {
			writeError(w, 503, "unavailable", "could not forget the token")
			return
		}
		recordFederationAudit("federation.claude_subscription.forget", "", "", "", "removed", 200)
	}
	_, at, err := readClaudeSubscriptionToken()
	out := map[string]any{"set": err == nil}
	if err == nil {
		out["set_at"] = at.UTC()
	} else if !os.IsNotExist(err) {
		out["error"] = err.Error()
	}
	writeJSON(w, 200, out)
}

func writeClaudeSubscriptionToken(token string) error {
	path := claudeSubscriptionTokenPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".claude-subscription-*")
	if err != nil {
		return err
	}
	_, werr := f.WriteString(token + "\n")
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(f.Name(), 0o600)
	}
	if werr == nil {
		werr = os.Rename(f.Name(), path)
	}
	if werr != nil {
		_ = os.Remove(f.Name())
	}
	return werr
}

// claudeSubscriptionTail names the peer routes that carry this operator's
// token: a spawn, and sending it again for an agent that runs on it.
// claudeSubscriptionSendVerb audits a spawn that carried this operator's
// token to a peer; a later resend to that peer requires one.
const claudeSubscriptionSendVerb = "federation.claude_subscription.send"

var claudeSubscriptionTail = regexp.MustCompile(`^(groups/[^/]+/spawn|agents/[^/]+/claude-subscription|teleport-subscription)$`)

// prepareClaudeSubscriptionPush swaps a remote spawn's
// "claude_subscription": true for this operator's token. It runs only for
// the local operator's own dashboard, never for a peer acting through this
// node, and only toward a peer that keeps the token in memory.
func prepareClaudeSubscriptionPush(r *http.Request, body []byte, peer *db.FederationPeer) ([]byte, error) {
	if r.Method != http.MethodPost || !claudeSubscriptionTail.MatchString(r.PathValue("tail")) || !bytes.Contains(body, []byte(`"claude_subscription"`)) {
		return body, nil
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil {
		return body, nil
	}
	var want bool
	raw, ok := fields["claude_subscription"]
	delete(fields, "claude_subscription")
	delete(fields, "claude_subscription_token")
	if ok && json.Unmarshal(raw, &want) != nil {
		return nil, errors.New("claude_subscription must be true or false")
	}
	if want {
		if r.Context().Value(remoteAuthedCtxKey{}) != nil {
			return nil, errors.New("only this node's own operator can send their Claude subscription")
		}
		cat, _, err := fedCatalogFor(peer.InstanceID)
		// 2 adds teleports onto the subscription (teleport-subscription).
		need := 1
		if r.PathValue("tail") == "teleport-subscription" {
			need = 2
		}
		if err != nil || cat == nil || cat.ClaudeSubscription < need {
			return nil, fmt.Errorf("%s cannot run an agent on your Claude subscription yet; update tclaude there", peerDisplay(peer))
		}
		// Sending it again only goes to a node this operator once spawned on
		// their subscription: a peer's own claim that an agent runs on it
		// cannot draw the token out of this node.
		resend := strings.HasPrefix(r.PathValue("tail"), "agents/")
		if resend {
			if sent, err := db.HasAuditLog(claudeSubscriptionSendVerb, "remote:"+peer.InstanceID); err != nil || !sent {
				return nil, fmt.Errorf("you never spawned an agent on your Claude subscription on %s, so it is not sent there", peerDisplay(peer))
			}
		}
		token, _, err := readClaudeSubscriptionToken()
		if err != nil {
			return nil, errors.New("set your Claude subscription token first (Fleet config → Model gateways → My Claude subscription)")
		}
		encoded, _ := json.Marshal(token)
		fields["claude_subscription_token"] = encoded
		verb := claudeSubscriptionSendVerb
		if resend {
			verb = "federation.claude_subscription.resend_send"
		}
		recordFederationAudit(verb, peer.InstanceID, "", "", r.PathValue("tail"), 202)
	}
	out, err := json.Marshal(fields)
	if err != nil || len(out) > fedPeerViewBodyLimit {
		return nil, errors.New("spawn request exceeds the encrypted peer transport limit")
	}
	return out, nil
}

