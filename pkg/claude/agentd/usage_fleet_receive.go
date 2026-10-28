package agentd

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/usagerules"
)

// usage_fleet_receive.go — the member side of a peer's fleet usage limit.
// A peer holding usage-rules.enforce here sends POST /api/usage/fleet-enforce;
// this node performs the fired steps on its own agents in the granted groups
// (an unscoped grant also covers agents in no group), records each once, and
// tells its operator. A spawn block or standing hard stop is a lease the peer
// renews while its limit stays over; it lapses by itself if the peer goes
// quiet, and the grant is checked again on every use.

const usageFleetMaxLease = 15 * time.Minute

// usageFleetMaxControls bounds the leases one peer holds here: each costs
// every spawn a grant check.
const usageFleetMaxControls = 32

var errUsageFleetTooManyControls = fmt.Errorf("a peer may hold at most %d fleet controls here", usageFleetMaxControls)

// usageFleetControl is a peer's spawn block or standing stop in force here.
type usageFleetControl struct {
	peer     string
	ruleKey  string
	ruleName string
	fleet    string
	metric   usagerules.Metric
	period   usagerules.Period
	value    float64
	budget   float64
	pct      float64
	block    bool
	stop     bool
	until    time.Time
}

// usageFleetControlView is a control as the dashboard and the peer see it.
type usageFleetControlView struct {
	Peer      string            `json:"peer"`
	PeerLabel string            `json:"peer_label"`
	RuleKey   string            `json:"rule_key"`
	RuleName  string            `json:"rule_name"`
	Fleet     string            `json:"fleet"`
	Metric    usagerules.Metric `json:"metric"`
	Period    usagerules.Period `json:"period"`
	Value     float64           `json:"value"`
	Budget    float64           `json:"budget"`
	Pct       float64           `json:"pct"`
	Block     bool              `json:"block"`
	Stop      bool              `json:"stop"`
	Until     string            `json:"until"`
}

var (
	usageFleetControlsMu sync.Mutex
	usageFleetControls   = map[string]*usageFleetControl{}
)

func resetUsageFleetControls() {
	usageFleetControlsMu.Lock()
	usageFleetControls = map[string]*usageFleetControl{}
	usageFleetControlsMu.Unlock()
}

// liveUsageFleetControls returns the unexpired controls (of peer, or of
// every peer when peer is ""), dropping expired ones.
func liveUsageFleetControls(peer string, now time.Time) []usageFleetControl {
	usageFleetControlsMu.Lock()
	defer usageFleetControlsMu.Unlock()
	var out []usageFleetControl
	for key, c := range usageFleetControls {
		if !now.Before(c.until) {
			delete(usageFleetControls, key)
			continue
		}
		if peer == "" || c.peer == peer {
			out = append(out, *c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].peer+out[i].ruleKey < out[j].peer+out[j].ruleKey })
	return out
}

func usageFleetControlViews(controls []usageFleetControl) []usageFleetControlView {
	out := make([]usageFleetControlView, 0, len(controls))
	labels := map[string]string{}
	for _, c := range controls {
		label, ok := labels[c.peer]
		if !ok {
			label = c.peer
			if p, err := db.GetFederationPeer(c.peer); err == nil && p != nil {
				label = peerDisplay(p)
			}
			labels[c.peer] = label
		}
		out = append(out, usageFleetControlView{Peer: c.peer, PeerLabel: label, RuleKey: c.ruleKey, RuleName: c.ruleName, Fleet: c.fleet,
			Metric: c.metric, Period: c.period, Value: c.value, Budget: c.budget, Pct: c.pct, Block: c.block, Stop: c.stop,
			Until: c.until.UTC().Format(time.RFC3339)})
	}
	return out
}

func usageFleetControlsFrom(peer string, now time.Time) []usageFleetControlView {
	return usageFleetControlViews(liveUsageFleetControls(peer, now))
}

// usageFleetEnforceScope reports whether peer holds usage-rules.enforce here
// at all, and whether unscoped (covering agents in no group too).
func usageFleetEnforceScope(peer string) (any, unscoped bool) {
	if db.FederationPeerUnrestricted(peer) {
		return true, true
	}
	grants, err := db.ListEffectiveFederationPeerGrants(peer)
	if err != nil {
		return false, false
	}
	for _, g := range grants {
		if g.Slug != PermUsageRulesEnforce {
			continue
		}
		any = true
		if g.Scope == "" {
			unscoped = true
		}
	}
	return any, unscoped
}

// usageFleetEnforceCovers reports whether peer's grant covers an agent in
// groups (none: an agent in no group).
func usageFleetEnforceCovers(r *fedGrantResolver, peer string, groupIDs []int64, unscoped bool) bool {
	if len(groupIDs) == 0 {
		return unscoped
	}
	for _, id := range groupIDs {
		if r.allows(peer, id, PermUsageRulesEnforce) {
			return true
		}
	}
	return false
}

// usageFleetScopedAgents lists the active agents peer's grant covers.
func usageFleetScopedAgents(peer string, agents map[string]*db.Agent, unscoped bool) ([]string, error) {
	groups, err := db.ListAgentGroups()
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(groups))
	for _, g := range groups {
		ids = append(ids, g.ID)
	}
	memberRows, err := db.ListAgentGroupMembersBatch(ids)
	if err != nil {
		return nil, err
	}
	agentByConv, err := db.AgentIDsByConv()
	if err != nil {
		return nil, err
	}
	groupsOf := map[string][]int64{}
	for groupID, rows := range memberRows {
		for _, m := range rows {
			if id := agentByConv[m.ConvID]; id != "" {
				groupsOf[id] = append(groupsOf[id], groupID)
			}
		}
	}
	resolver := newFedGrantResolver().seedGroups(groups)
	var out []string
	for id := range agents {
		if usageFleetEnforceCovers(resolver, peer, groupsOf[id], unscoped) {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out, nil
}

// usageFleetText keeps a peer-sent label printable and short.
func usageFleetText(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, strings.ToValidUTF8(s, ""))
	s = strings.TrimSpace(s)
	if len([]rune(s)) > max {
		s = string([]rune(s)[:max])
	}
	return s
}

func validUsageFleetEnforce(req *usageFleetEnforceRequest) error {
	req.RuleKey = strings.TrimSpace(req.RuleKey)
	if req.RuleKey == "" || len(req.RuleKey) > 128 || usageFleetText(req.RuleKey, 128) != req.RuleKey {
		return fmt.Errorf("rule_key must be 1-128 printable characters")
	}
	if req.LeaseSeconds < 0 || time.Duration(req.LeaseSeconds)*time.Second > usageFleetMaxLease {
		return fmt.Errorf("lease_seconds must be between 0 and %d", int(usageFleetMaxLease/time.Second))
	}
	if req.LeaseSeconds == 0 && len(req.Events) == 0 {
		return nil // a release
	}
	req.RuleName, req.Fleet = usageFleetText(req.RuleName, 64), usageFleetText(req.Fleet, 128)
	req.PeriodKey = usageFleetText(req.PeriodKey, 128)
	if req.RuleName == "" || req.PeriodKey == "" {
		return fmt.Errorf("rule_name and period_key are required")
	}
	switch req.Metric {
	case usagerules.MetricTotalUSD, usagerules.MetricRealUSD, usagerules.MetricWhatIfUSD:
	default:
		return fmt.Errorf("unknown metric %q", req.Metric)
	}
	switch req.Period {
	case usagerules.PeriodDay, usagerules.PeriodWeek, usagerules.PeriodMonth:
	default:
		return fmt.Errorf("unknown period %q", req.Period)
	}
	for _, v := range []float64{req.Value, req.Budget, req.Pct} {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return fmt.Errorf("value, budget and pct must be non-negative numbers")
		}
	}
	if len(req.Events) > usageFleetPendingCap {
		return fmt.Errorf("at most %d events", usageFleetPendingCap)
	}
	for _, ev := range req.Events {
		if ev.Key == "" || len(ev.Key) > 64 || usageFleetText(ev.Key, 64) != ev.Key {
			return fmt.Errorf("event key must be 1-64 printable characters")
		}
		switch ev.Action {
		case usagerules.ActionNotify, usagerules.ActionWrapUp, usagerules.ActionHardStop, usagerules.ActionBlockSpawn:
		default:
			return fmt.Errorf("unknown action %q", ev.Action)
		}
	}
	return nil
}

// servePeerUsageFleetEnforce performs a peer fleet limit's steps here.
func servePeerUsageFleetEnforce(w http.ResponseWriter, r *http.Request, v *peerView, _ peerViewRule) {
	peer := v.peer.InstanceID
	anyGrant, unscoped := usageFleetEnforceScope(peer)
	if !anyGrant {
		writeError(w, http.StatusForbidden, "permission", "requires "+PermUsageRulesEnforce)
		return
	}
	var req usageFleetEnforceRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if err := validUsageFleetEnforce(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	reply, err := applyUsageFleetEnforce(peerDisplay(v.peer), peer, unscoped, req, time.Now())
	if errors.Is(err, errUsageFleetTooManyControls) {
		writeError(w, http.StatusTooManyRequests, "too_many_controls", err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, reply)
}

func applyUsageFleetEnforce(label, peer string, unscoped bool, req usageFleetEnforceRequest, now time.Time) (*usageFleetEnforceReply, error) {
	key := peer + "|" + req.RuleKey
	reply := &usageFleetEnforceReply{At: now.UTC().Format(time.RFC3339)}
	if req.LeaseSeconds == 0 && len(req.Events) == 0 {
		usageFleetControlsMu.Lock()
		delete(usageFleetControls, key)
		usageFleetControlsMu.Unlock()
		return reply, nil
	}
	st, err := loadUsageFleetLocal(now)
	if err != nil {
		return nil, err
	}
	agents, err := usageFleetScopedAgents(peer, st.agents, unscoped)
	if err != nil {
		return nil, err
	}
	reply.Agents = len(agents)
	if req.LeaseSeconds > 0 && (req.Block || req.Stop) {
		usageFleetControlsMu.Lock()
		held := 0
		_, renewing := usageFleetControls[key]
		for _, c := range usageFleetControls {
			if c.peer == peer && now.Before(c.until) {
				held++
			}
		}
		usageFleetControlsMu.Unlock()
		if !renewing && held >= usageFleetMaxControls {
			return nil, errUsageFleetTooManyControls
		}
	}
	stoppedNow := false
	rule := usagerules.Rule{Name: req.RuleName, Metric: req.Metric, Period: req.Period, Budget: req.Budget}
	fleet := req.Fleet
	if fleet == "" {
		fleet = "fleet"
	}
	t := usageRuleTarget{ruleKey: "peer:" + peer + ":" + req.RuleKey, subject: "fleet", scope: usagerules.ScopeFleet, rule: rule,
		owner: fmt.Sprintf("fleet limit of %s (%s)", label, fleet), fleet: fleet + " of " + label, agents: agents}
	for _, ev := range req.Events {
		var msg *db.HumanMessage
		if ev.Action != usagerules.ActionNotify {
			msg = usageFleetReceivedMessage(label, req, ev.Action, len(agents), now)
		}
		inserted, err := db.InsertUsageRuleFiring(db.UsageRuleFiring{
			RuleKey: t.ruleKey, Subject: t.subject, PeriodKey: req.PeriodKey, StepKey: ev.Key, Action: string(ev.Action),
			RuleName: req.RuleName, Value: req.Value, Budget: req.Budget, FiredAt: now,
		}, msg)
		if err != nil {
			slog.Warn("usage fleet: record received firing", "peer", peer, "rule", req.RuleKey, "error", err)
			continue
		}
		if !inserted {
			continue
		}
		if msg != nil {
			dispatchHumanMessageNotification("", msg.FromTitle, "", msg.Subject, msg.Body)
		}
		running := len(st.runningConvs(agents))
		step := usagerules.Step{Action: ev.Action}
		if ev.Action == usagerules.ActionNotify {
			step.Notify = []usagerules.Target{usagerules.TargetAgent}
		}
		st.act(t, usagerules.Firing{Step: step, Key: ev.Key}, req.Value, req.Pct)
		switch ev.Action {
		case usagerules.ActionNotify, usagerules.ActionWrapUp:
			reply.Notified += running
		case usagerules.ActionHardStop:
			reply.Stopped += running
			stoppedNow = true
		}
	}
	usageFleetControlsMu.Lock()
	if req.LeaseSeconds > 0 && (req.Block || req.Stop) {
		usageFleetControls[key] = &usageFleetControl{peer: peer, ruleKey: req.RuleKey, ruleName: req.RuleName, fleet: fleet,
			metric: req.Metric, period: req.Period, value: req.Value, budget: req.Budget, pct: req.Pct,
			block: req.Block, stop: req.Stop, until: now.Add(time.Duration(req.LeaseSeconds) * time.Second)}
	} else {
		delete(usageFleetControls, key)
	}
	usageFleetControlsMu.Unlock()
	reply.Block, reply.Stop = req.Block && req.LeaseSeconds > 0, req.Stop && req.LeaseSeconds > 0
	if reply.Stop && !stoppedNow {
		// A standing stop: an agent in scope that runs again is stopped
		// again while the peer's limit stays past its hard stop. (The
		// delivery that carries the stop itself just stopped them.)
		for _, conv := range st.runningConvs(agents) {
			res := usageRulesHardStop(conv)
			reply.Stopped++
			slog.Info("usage fleet: standing stop", "peer", peer, "rule", req.RuleKey, "conv", conv, "result", res.Action)
			title := agent.CachedTitle(conv)
			if title == "" {
				title = conv
			}
			msg := &db.HumanMessage{FromTitle: "Usage limits", CreatedAt: now,
				Subject: fmt.Sprintf("Stopped %s again: %s's fleet limit %q is past its hard stop", title, label, req.RuleName),
				Body: fmt.Sprintf("%s was running while %s's fleet limit %q (%s) stands at %.0f%% of its budget, past its hard stop. %s can raise the budget; you can revoke its %s grant.",
					title, label, req.RuleName, fleet, req.Pct, label, PermUsageRulesEnforce)}
			if _, err := db.InsertHumanMessage(msg); err == nil {
				dispatchHumanMessageNotification("", msg.FromTitle, "", msg.Subject, msg.Body)
			}
		}
	}
	return reply, nil
}

// usageFleetReceivedMessage tells this node's operator what a peer's fleet
// limit does here.
func usageFleetReceivedMessage(label string, req usageFleetEnforceRequest, action usagerules.Action, agents int, now time.Time) *db.HumanMessage {
	what := map[usagerules.Action]string{
		usagerules.ActionWrapUp:     "asks your agents in its scope to wrap up",
		usagerules.ActionHardStop:   "stops your agents in its scope",
		usagerules.ActionBlockSpawn: "blocks new spawns in its scope",
	}[action]
	fleet := req.Fleet
	if fleet == "" {
		fleet = "fleet"
	}
	return &db.HumanMessage{FromTitle: "Usage limits", CreatedAt: now,
		Subject: fmt.Sprintf("%s's fleet limit %q %s (%.0f%%)", label, req.RuleName, what, req.Pct),
		Body: fmt.Sprintf("%s's fleet limit %q over %s stands at %.0f%% of its budget for %s, and %s: %d of your agents are in the scope of its %s grant. "+
			"Revoke the grant (Fleet → grants) to stop it acting here.",
			label, req.RuleName, fleet, req.Pct, usagePeriodLabel(req.Period), what, agents, PermUsageRulesEnforce)}
}

// checkUsageFleetSpawnBlock refuses a spawn into groups (none: an agent in no
// group) while a peer's fleet limit blocks spawns there, or this node's own
// fleet limit that counts this node does.
func checkUsageFleetSpawnBlock(groups []*db.AgentGroup) *spawnFailure {
	now := time.Now()
	if l := ownUsageFleetBlock(now); l != nil {
		return &spawnFailure{Status: http.StatusConflict, Kind: "usage_limit_reached",
			Msg: fmt.Sprintf("the fleet usage limit %q blocks new spawns until its %s rolls over; raise its budget or disable it "+
				"(dashboard Usage limits & budgets → Fleet) to spawn now", l.Rule.Name, usagePeriodNoun(l.Rule.Period))}
	}
	controls := liveUsageFleetControls("", now)
	if len(controls) == 0 {
		return nil
	}
	resolver := newFedGrantResolver().seedGroups(groups)
	ids := make([]int64, 0, len(groups))
	for _, g := range groups {
		ids = append(ids, g.ID)
	}
	for _, c := range controls {
		if !c.block {
			continue
		}
		_, unscoped := usageFleetEnforceScope(c.peer)
		if !usageFleetEnforceCovers(resolver, c.peer, ids, unscoped) {
			continue
		}
		label := c.peer
		if p, err := db.GetFederationPeer(c.peer); err == nil && p != nil {
			label = peerDisplay(p)
		}
		return &spawnFailure{Status: http.StatusConflict, Kind: "usage_limit_reached",
			Msg: fmt.Sprintf("%s's fleet usage limit %q (%s) blocks new spawns here; %s can raise its budget, or you can revoke its %s grant",
				label, c.ruleName, c.fleet, label, PermUsageRulesEnforce)}
	}
	return nil
}

// ownUsageFleetBlock is this node's own enabled fleet limit that counts this
// node and whose block_spawn step fired this period and still holds.
func ownUsageFleetBlock(now time.Time) *db.UsageFleetLimit {
	statuses, _ := latestUsageFleetStatus()
	if len(statuses) == 0 {
		return nil
	}
	limits, err := db.ListUsageFleetLimits()
	if err != nil {
		slog.Warn("usage fleet: read spawn blocks", "error", err)
		return nil
	}
	for _, l := range limits {
		if !l.Enabled || !l.IncludeSelf || l.Rule.DryRun {
			continue
		}
		step := usageBlockStep(l.Rule)
		if step == nil {
			continue
		}
		held := false
		for _, s := range statuses {
			if s.FleetID == l.ID && s.Pct >= step.At {
				held = true
			}
		}
		if !held {
			continue
		}
		periods, err := db.UsageRuleFiringPeriods(usageFleetRuleKey(l.ID), usageFleetSubject(l), step.Key())
		if err != nil {
			continue
		}
		for _, p := range periods {
			if usageFiringInCurrentPeriod(l.Rule, p, now) {
				return l
			}
		}
	}
	return nil
}
