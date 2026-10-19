package agentd

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Usage-share alerts tell this node's operator when a share crosses 80% and
// 100% of its dollar cap, and when it ends: shares this node gives (it pays)
// and shares it receives while its agents use one. A share is followed from
// its first sight; a daemon restart re-learns where each one stands without
// alerting again.

// shareAlertEvery is how often shares are checked.
const shareAlertEvery = 2 * time.Minute

type shareAlertSeen struct {
	share modelShare
	level int
}

type shareAlerts struct {
	mu   sync.Mutex
	seen map[string]shareAlertSeen
}

// shareAlert is one notification to send.
type shareAlert struct{ subject, body string }

// shareLevel is how much of a capped share is spent: 0, 80 or 100.
func shareLevel(s modelShare) int {
	if s.MaxUSD <= 0 {
		return 0
	}
	switch used := s.SpentUSD / s.MaxUSD; {
	case used >= 1:
		return 100
	case used >= 0.8:
		return 80
	}
	return 0
}

// shareAlertKey follows one peer's access to one gateway, whichever grant
// is the binding one at the moment.
func shareAlertKey(side string, s modelShare) string {
	return side + "|" + s.Peer + "|" + s.Gateway
}

// sameShare reports whether two sightings show the same grant; a different
// one (re-granted, or another cap became the binding one) restarts its
// level without alerting.
func sameShare(a, b modelShare) bool {
	return a.Scope == b.Scope && a.Since.Equal(b.Since)
}

// observe compares one side's shares now with what it saw last, and returns
// the alerts. answered says whose shares this round is complete for (a peer
// that did not answer keeps its shares as they were); nil means every peer.
// A share that is gone counts as ended only once its time is up, or, for a
// received share, when its peer answered without it.
func (st *shareAlerts) observe(side string, shares []modelShare, answered map[string]bool, now time.Time) []shareAlert {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.seen == nil {
		st.seen = map[string]shareAlertSeen{}
	}
	var out []shareAlert
	present := map[string]bool{}
	for _, s := range shares {
		key := shareAlertKey(side, s)
		present[key] = true
		level := shareLevel(s)
		last, known := st.seen[key]
		st.seen[key] = shareAlertSeen{share: s, level: level}
		if known && sameShare(last.share, s) && level > last.level {
			out = append(out, shareLevelAlert(side, s, level))
		}
	}
	for key, last := range st.seen {
		if present[key] || key[:len(side)+1] != side+"|" {
			continue
		}
		s := last.share
		timeUp := s.ExpiresAt != nil && !s.ExpiresAt.After(now)
		if answered != nil && !answered[s.Peer] {
			continue
		}
		delete(st.seen, key)
		if timeUp || side == "received" {
			out = append(out, shareEndedAlert(side, s))
		}
	}
	// A cap on every gateway shows once per gateway: alert about it once.
	seen := map[shareAlert]bool{}
	unique := out[:0]
	for _, a := range out {
		if !seen[a] {
			seen[a] = true
			unique = append(unique, a)
		}
	}
	return unique
}

// shareWhere names what a share covers: its gateway, or every gateway.
func shareWhere(s modelShare) string {
	if s.Scope == "" && !s.Since.IsZero() {
		return "every gateway"
	}
	return "gateway " + s.Gateway
}

func shareWho(s modelShare) string {
	if s.PeerName != "" {
		return s.PeerName
	}
	return s.Peer
}

func shareLevelAlert(side string, s modelShare, level int) shareAlert {
	spent := fmt.Sprintf("$%.2f of $%.2f", s.SpentUSD, s.MaxUSD)
	who := shareWho(s)
	if side == "given" {
		if level >= 100 {
			return shareAlert{fmt.Sprintf("Usage share used up: %s on %s", who, shareWhere(s)),
				fmt.Sprintf("%s has spent %s you share on %s. Its model calls are refused until you raise the cap (Fleet → Usage sharing).", who, spent, shareWhere(s))}
		}
		return shareAlert{fmt.Sprintf("Usage share at 80%%: %s on %s", who, shareWhere(s)),
			fmt.Sprintf("%s has spent %s you share on %s.", who, spent, shareWhere(s))}
	}
	if level >= 100 {
		return shareAlert{fmt.Sprintf("%s's usage share is used up", who),
			fmt.Sprintf("Your agents have spent %s that %s shares on %s; their model calls through it are refused. Ask %s for more (Fleet → Usage sharing) or spawn on your own account.", spent, who, shareWhere(s), who)}
	}
	return shareAlert{fmt.Sprintf("%s's usage share is 80%% used", who),
		fmt.Sprintf("Your agents have spent %s that %s shares on %s.", spent, who, shareWhere(s))}
}

func shareEndedAlert(side string, s modelShare) shareAlert {
	who := shareWho(s)
	spent := fmt.Sprintf("$%.2f spent", s.SpentUSD)
	if s.MaxUSD > 0 {
		spent = fmt.Sprintf("$%.2f of $%.2f spent", s.SpentUSD, s.MaxUSD)
	}
	if side == "given" {
		return shareAlert{fmt.Sprintf("Usage share ended: %s on %s", who, shareWhere(s)),
			fmt.Sprintf("Your usage share with %s on %s has ended (%s).", who, shareWhere(s), spent)}
	}
	return shareAlert{fmt.Sprintf("%s's usage share ended", who),
		fmt.Sprintf("%s no longer shares %s with this node (%s). Agents billed to it lose model access; ask %s again or spawn on your own account.", who, shareWhere(s), spent, who)}
}

var usageShareAlerts shareAlerts

// runShareAlerts checks shares until ctx ends. Received shares are asked for
// only while an agent here is bound to a peer's gateway.
func (rt *fedRuntime) runShareAlerts(ctx context.Context) {
	ticker := time.NewTicker(shareAlertEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		rt.checkShareAlerts(ctx, time.Now())
	}
}

func (rt *fedRuntime) checkShareAlerts(ctx context.Context, now time.Time) {
	peers, err := db.ListFederationPeers()
	if err != nil {
		return
	}
	cfg, _ := config.Load()
	given := []modelShare{}
	for i := range peers {
		given = append(given, modelSharesFor(cfg, &peers[i])...)
	}
	alerts := usageShareAlerts.observe("given", given, nil, now)
	if bound, err := db.LiveModelProxyLaunches(); err == nil && bound {
		wait, cancel := context.WithTimeout(ctx, modelSharesReceivedWait)
		received, errs := receivedModelShares(wait, rt, peers)
		cancel()
		answered := map[string]bool{}
		for _, p := range peers {
			answered[p.InstanceID] = errs[p.InstanceID] == ""
		}
		alerts = append(alerts, usageShareAlerts.observe("received", received, answered, now)...)
	}
	for _, a := range alerts {
		_, _ = recordHumanMessage("", a.subject, a.body)
	}
}
