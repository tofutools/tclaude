package agentd

import (
	"context"
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

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/usagerules"
)

// usage_fleet.go — fleet usage limits: one rule over spend summed across
// nodes. Every node answers GET /api/usage/node-summary with its own totals for
// the peers it shares usage.read with; the node holding a fleet limit reads
// its members' summaries, sums them, and runs the limit's ladder. Agent
// actions reach each member through POST /api/usage/fleet-enforce, which the
// member performs itself under the usage-rules.enforce grant it gave
// (usage_fleet_receive.go). Spawn blocks and standing stops are leases the
// holder renews every tick, so a silent holder never wedges a member.

const (
	usageFleetPollInterval = time.Minute
	// usageFleetLease is how long a member keeps a spawn block or standing
	// stop without hearing from the limit's node again.
	usageFleetLease = 3 * usageFleetPollInterval
	// usageFleetStale is the age past which a member's last summary no
	// longer counts towards a fleet limit.
	usageFleetStale = 10 * time.Minute
	// usageSummaryFresh is how long a node reuses its own computed totals.
	usageSummaryFresh          = 30 * time.Second
	usageFleetFetchConcurrency = 8
)

// usageTotals is spend per period (day | week | month) and metric.
type usageTotals map[usagerules.Period]map[usagerules.Metric]float64

var usageFleetPeriods = []usagerules.Period{usagerules.PeriodDay, usagerules.PeriodWeek, usagerules.PeriodMonth}
var usageFleetMetrics = []usagerules.Metric{usagerules.MetricTotalUSD, usagerules.MetricRealUSD, usagerules.MetricWhatIfUSD}

// usageBudgetCounts summarizes the per-agent budgets on a node.
type usageBudgetCounts struct {
	Agents int `json:"agents"` // agents with at least one budget reading
	Warn   int `json:"warn"`   // at or past 80%, or a notify step reached
	Over   int `json:"over"`   // a wrap-up or hard stop step reached
}

// usageNodeSummary is what a node shares with a peer holding usage.read.
// Grants always answers which usage grants the reading peer holds here, so a
// fleet view can say why a node shares nothing.
type usageNodeSummary struct {
	At      string                       `json:"at,omitempty"`
	Grants  []string                     `json:"grants"`
	Totals  usageTotals                  `json:"totals,omitempty"`
	Periods map[usagerules.Period]string `json:"periods,omitempty"`
	Limits  []usageRuleStatus            `json:"limits,omitempty"`
	Budgets *usageBudgetCounts           `json:"budgets,omitempty"`
	// Controls are the fleet limits of the reading peer in force here.
	Controls []usageFleetControlView `json:"controls,omitempty"`
}

var (
	usageTotalsMu sync.Mutex
	usageTotalsAt time.Time
	usageTotalsV  usageTotals
)

// localUsageTotals sums this node's counted cost ledger per period and
// metric, reusing a reading for usageSummaryFresh.
func localUsageTotals(now time.Time) (usageTotals, error) {
	usageTotalsMu.Lock()
	defer usageTotalsMu.Unlock()
	if usageTotalsV != nil && now.Sub(usageTotalsAt) < usageSummaryFresh && now.After(usageTotalsAt) &&
		now.Format("2006-01-02") == usageTotalsAt.Format("2006-01-02") {
		return usageTotalsV, nil
	}
	rows, _, _, err := countedCostDailyRows()
	if err != nil {
		return nil, err
	}
	spend := newUsageSpend(mixedCostDeltasFromRows(rows), nil)
	out := usageTotals{}
	for _, p := range usageFleetPeriods {
		from := usagerules.PeriodStartDay(p, now)
		out[p] = map[usagerules.Metric]float64{}
		for _, m := range usageFleetMetrics {
			out[p][m] = sumUsageSlices(spend.all, m, from)
		}
	}
	usageTotalsV, usageTotalsAt = out, now
	return out, nil
}

func resetUsageTotalsCache() {
	usageTotalsMu.Lock()
	usageTotalsV = nil
	usageTotalsMu.Unlock()
}

// usagePeerGrants lists the usage grants peer holds on this node.
func usagePeerGrants(peer string) []string {
	out := []string{}
	unrestricted := db.FederationPeerUnrestricted(peer)
	grants, err := db.ListEffectiveFederationPeerGrants(peer)
	if err != nil && !unrestricted {
		return out
	}
	has := func(slug string) bool {
		if unrestricted {
			return true
		}
		for _, g := range grants {
			if g.Slug == slug {
				return true
			}
		}
		return false
	}
	for _, slug := range []string{PermUsageRead, PermUsageRulesManage, PermUsageRulesEnforce} {
		if has(slug) {
			out = append(out, slug)
		}
	}
	return out
}

// collectUsageNodeSummary is this node's summary for peer ("" for the local
// dashboard, which sees everything).
func collectUsageNodeSummary(peer string, now time.Time) (usageNodeSummary, error) {
	out := usageNodeSummary{Grants: []string{}}
	if peer != "" {
		out.Grants = usagePeerGrants(peer)
		if !slicesContains(out.Grants, PermUsageRead) {
			return out, nil
		}
	}
	totals, err := localUsageTotals(now)
	if err != nil {
		return out, err
	}
	out.At = now.UTC().Format(time.RFC3339)
	out.Totals = totals
	out.Periods = map[usagerules.Period]string{}
	for _, p := range usageFleetPeriods {
		out.Periods[p] = usagerules.PeriodKey(p, now, time.Time{})
	}
	out.Limits = latestUsageLimitStatus()
	statuses, _ := latestUsageRuleStatus()
	worst := map[string]usageRuleStatus{}
	for _, s := range statuses {
		if s.Scope != string(usagerules.ScopeAgent) || s.DryRun {
			continue
		}
		if w, ok := worst[s.AgentID]; !ok || s.Pct > w.Pct {
			worst[s.AgentID] = s
		}
	}
	if len(worst) > 0 {
		c := &usageBudgetCounts{Agents: len(worst)}
		for _, s := range worst {
			switch {
			case s.Reached != "" && s.Reached != usagerules.ActionNotify:
				c.Over++
			case s.Reached != "" || s.Pct >= 80:
				c.Warn++
			}
		}
		out.Budgets = c
	}
	if peer != "" {
		out.Controls = usageFleetControlsFrom(peer, now)
	}
	return out, nil
}

func slicesContains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// handleUsageSummary serves this node's own summary to the local dashboard
// and to agents holding usage.read.
func handleUsageSummary(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePermission(w, r, PermUsageRead); !ok {
		return
	}
	out, err := collectUsageNodeSummary("", time.Now())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// servePeerUsageSummary answers any trusted peer: the totals only with
// usage.read, the grants it holds here always.
func servePeerUsageSummary(w http.ResponseWriter, _ *http.Request, v *peerView, _ peerViewRule) {
	out, err := collectUsageNodeSummary(v.peer.InstanceID, time.Now())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// --- the holder side: reading members and running fleet limits ---

// usageFleetExchange sends one peer-view request to peer (a test seam).
var usageFleetExchange = func(ctx context.Context, peer *db.FederationPeer, method, uri string, body []byte) (int, []byte, error) {
	rt := currentFederation()
	if rt == nil {
		return 0, nil, errPeerViewOffline
	}
	header := http.Header{}
	if body != nil {
		header.Set("Content-Type", "application/json")
	}
	reply, err := exchangePeerView(ctx, rt, peer, fedPeerViewRequest{Method: method, URI: uri, Header: header, Body: body})
	if err != nil {
		return 0, nil, err
	}
	return reply.Status, reply.Body, nil
}

// usageFleetRead is the latest attempt to read one peer's summary.
type usageFleetRead struct {
	summary  usageNodeSummary // the last good one
	readAt   time.Time        // when summary was read; zero before any
	tried    time.Time
	err      string
	lastSend *usageFleetEnforceReply
	sendErr  string
}

var (
	usageFleetReadsMu sync.Mutex
	usageFleetReads   = map[string]*usageFleetRead{}
)

// readUsageFleetPeers refreshes the summaries of peers last tried more than
// maxAge ago, in parallel, and returns a copy of every peer's latest read.
func readUsageFleetPeers(ctx context.Context, peers []db.FederationPeer, maxAge time.Duration, now time.Time) map[string]usageFleetRead {
	var due []db.FederationPeer
	usageFleetReadsMu.Lock()
	for _, p := range peers {
		rd := usageFleetReads[p.InstanceID]
		if rd == nil || now.Sub(rd.tried) >= maxAge || rd.tried.After(now) {
			due = append(due, p)
		}
	}
	usageFleetReadsMu.Unlock()
	sem := make(chan struct{}, usageFleetFetchConcurrency)
	var wg sync.WaitGroup
	for i := range due {
		p := due[i]
		wg.Add(1)
		sem <- struct{}{}
		goBackground(func() {
			defer wg.Done()
			defer func() { <-sem }()
			summary, err := fetchUsageSummary(ctx, &p)
			usageFleetReadsMu.Lock()
			defer usageFleetReadsMu.Unlock()
			rd := usageFleetReads[p.InstanceID]
			if rd == nil {
				rd = &usageFleetRead{}
				usageFleetReads[p.InstanceID] = rd
			}
			rd.tried = now
			if err != nil {
				rd.err = err.Error()
				return
			}
			rd.err, rd.summary, rd.readAt = "", summary, now
		})
	}
	wg.Wait()
	out := map[string]usageFleetRead{}
	usageFleetReadsMu.Lock()
	for _, p := range peers {
		if rd := usageFleetReads[p.InstanceID]; rd != nil {
			out[p.InstanceID] = *rd
		}
	}
	usageFleetReadsMu.Unlock()
	return out
}

func fetchUsageSummary(ctx context.Context, peer *db.FederationPeer) (usageNodeSummary, error) {
	var out usageNodeSummary
	status, body, err := usageFleetExchange(ctx, peer, http.MethodGet, "/api/usage/node-summary", nil)
	if err != nil {
		if errors.Is(err, errPeerViewOffline) || errors.Is(err, errPeerViewTimeout) {
			return out, errors.New("offline")
		}
		return out, err
	}
	if status != http.StatusOK {
		return out, fmt.Errorf("summary answered %d", status)
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return out, fmt.Errorf("decode summary: %w", err)
	}
	return out, nil
}

func resetUsageFleetForTest() {
	usageFleetReadsMu.Lock()
	usageFleetReads = map[string]*usageFleetRead{}
	usageFleetReadsMu.Unlock()
	usageFleetStatusMu.Lock()
	usageFleetStatusLast, usageFleetStatusAt = nil, time.Time{}
	usageFleetStatusMu.Unlock()
	usageFleetSentMu.Lock()
	usageFleetSent = map[string]bool{}
	usageFleetPending = map[string][]usageFleetEvent{}
	usageFleetSentMu.Unlock()
	resetUsageFleetControls()
	resetUsageTotalsCache()
}

// usageFleetMember is one node's part of a fleet limit reading.
type usageFleetMember struct {
	Node  string  `json:"node"` // instance id, or "self"
	Label string  `json:"label"`
	Value float64 `json:"value"`
	// State: self | ok | stale | not_shared | offline | error. Only self, ok
	// and stale (a reading from earlier in the same period) count.
	State   string `json:"state"`
	Counted bool   `json:"counted"`
	// Enforce: self | granted | not_granted | unknown.
	Enforce string `json:"enforce"`
	ReadAt  string `json:"read_at,omitempty"`
	Error   string `json:"error,omitempty"`
	// Applied is the member's answer to the latest enforcement sent to it.
	Applied *usageFleetEnforceReply `json:"applied,omitempty"`
	SendErr string                  `json:"send_error,omitempty"`
}

// usageFleetStatus is one fleet limit's latest reading.
type usageFleetStatus struct {
	usageRuleStatus
	FleetID     int64              `json:"fleet_id"`
	Target      string             `json:"target"`
	TargetLabel string             `json:"target_label"`
	IncludeSelf bool               `json:"include_self"`
	Members     []usageFleetMember `json:"members"`
	Counted     int                `json:"counted"`
	Error       string             `json:"error,omitempty"`
}

var (
	usageFleetStatusMu   sync.RWMutex
	usageFleetStatusLast []usageFleetStatus
	usageFleetStatusAt   time.Time
	usageFleetMu         sync.Mutex // serializes fleet evaluations
)

func latestUsageFleetStatus() ([]usageFleetStatus, time.Time) {
	usageFleetStatusMu.RLock()
	defer usageFleetStatusMu.RUnlock()
	return usageFleetStatusLast, usageFleetStatusAt
}

func startUsageFleetPoller(stop <-chan struct{}) {
	go func() {
		t := time.NewTicker(usageFleetPollInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				pollUsageFleet(time.Now())
			}
		}
	}()
}

// reevaluateUsageFleet re-runs the fleet evaluation after a change.
var reevaluateUsageFleet = func() {
	goBackground(func() { pollUsageFleet(time.Now()) })
}

// usageFleetTargetLabel names a target for people; members lists its peers
// (trusted ones only).
func usageFleetTargetMembers(target string, trusted []db.FederationPeer) (label string, members []db.FederationPeer, err error) {
	switch {
	case target == db.UsageFleetTargetAll:
		return "all nodes", trusted, nil
	case strings.HasPrefix(target, db.UsageFleetTargetPoolPrefix):
		id := strings.TrimPrefix(target, db.UsageFleetTargetPoolPrefix)
		pool, err := db.GetFederationNodeGroupByID(id)
		if err != nil {
			return target, nil, err
		}
		if pool == nil {
			return "deleted node pool", nil, nil
		}
		peers, err := db.ListFederationNodeGroupMembers(id)
		if err != nil {
			return "pool " + pool.Name, nil, err
		}
		return "pool " + pool.Name, peers, nil
	case strings.HasPrefix(target, db.UsageFleetTargetPeerPrefix):
		id := strings.TrimPrefix(target, db.UsageFleetTargetPeerPrefix)
		for _, p := range trusted {
			if p.InstanceID == id {
				return "node " + peerDisplay(&p), []db.FederationPeer{p}, nil
			}
		}
		return "untrusted node", nil, nil
	}
	return target, nil, fmt.Errorf("unknown fleet target %q", target)
}

// pollUsageFleet evaluates every enabled fleet limit. Without one it costs a
// single query.
func pollUsageFleet(now time.Time) {
	usageFleetMu.Lock()
	defer usageFleetMu.Unlock()
	limits, err := db.ListUsageFleetLimits()
	if err != nil {
		slog.Warn("usage fleet: list limits", "error", err)
		return
	}
	var enabled []*db.UsageFleetLimit
	for _, l := range limits {
		if l.Enabled {
			enabled = append(enabled, l)
		}
	}
	if len(enabled) == 0 {
		usageFleetStatusMu.Lock()
		usageFleetStatusLast, usageFleetStatusAt = nil, now
		usageFleetStatusMu.Unlock()
		releaseUsageFleetSends(nil)
		return
	}
	trusted, err := db.ListFederationPeers()
	if err != nil {
		slog.Warn("usage fleet: list peers", "error", err)
		return
	}
	type bound struct {
		limit   *db.UsageFleetLimit
		label   string
		members []db.FederationPeer
		err     error
	}
	var all []bound
	need := map[string]db.FederationPeer{}
	for _, l := range enabled {
		label, members, err := usageFleetTargetMembers(l.Target, trusted)
		all = append(all, bound{limit: l, label: label, members: members, err: err})
		for _, p := range members {
			need[p.InstanceID] = p
		}
	}
	peers := make([]db.FederationPeer, 0, len(need))
	for _, p := range need {
		peers = append(peers, p)
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].InstanceID < peers[j].InstanceID })
	ctx, cancel := context.WithTimeout(context.Background(), 2*fedPeerViewTimeout)
	defer cancel()
	reads := readUsageFleetPeers(ctx, peers, usageFleetPollInterval/2, now)
	self, selfErr := localUsageTotals(now)
	local, err := loadUsageFleetLocal(now)
	if err != nil {
		// Local actions need liveness; skip the tick rather than act blind.
		slog.Warn("usage fleet: local state", "error", err)
		return
	}
	var statuses []usageFleetStatus
	want := map[string]bool{} // peer|rule pairs owed an enforcement this tick
	for _, b := range all {
		if b.err != nil {
			// The members are unknown this tick: hold what the limit holds.
			usageFleetSentMu.Lock()
			for key := range usageFleetSent {
				if strings.HasSuffix(key, "|"+usageFleetRuleKey(b.limit.ID)) {
					want[key] = true
				}
			}
			usageFleetSentMu.Unlock()
		}
		status := evaluateUsageFleetLimit(local, b.limit, b.label, b.members, reads, self, selfErr, now, want)
		if b.err != nil {
			status.Error = b.err.Error()
		}
		statuses = append(statuses, status)
	}
	releaseUsageFleetSends(want)
	usageFleetStatusMu.Lock()
	usageFleetStatusLast, usageFleetStatusAt = statuses, now
	usageFleetStatusMu.Unlock()
}

// loadUsageFleetLocal is the slice of local state a fleet limit's own-node
// actions need: active agents and the live sessions.
func loadUsageFleetLocal(now time.Time) (*usageRulesState, error) {
	st := &usageRulesState{now: now, agents: map[string]*db.Agent{}}
	agents, err := db.ListActiveAgents()
	if err != nil {
		return nil, err
	}
	for _, a := range agents {
		st.agents[a.AgentID] = a
	}
	if st.alive, err = usageRulesLiveSessions(); err != nil {
		return nil, fmt.Errorf("list live sessions: %w", err)
	}
	return st, nil
}

func usageFleetRuleKey(id int64) string { return fmt.Sprintf("fleet:%d", id) }

// usageFleetSubject is the firing subject of a fleet limit; the target is
// part of it, so retargeting a limit re-arms its ladder.
func usageFleetSubject(l *db.UsageFleetLimit) string {
	self := ""
	if l.IncludeSelf {
		self = "+self"
	}
	return "fleet:" + l.Target + self
}

func evaluateUsageFleetLimit(local *usageRulesState, l *db.UsageFleetLimit, label string, members []db.FederationPeer,
	reads map[string]usageFleetRead, self usageTotals, selfErr error, now time.Time, want map[string]bool) usageFleetStatus {
	r := l.Rule
	periodKey := usagerules.PeriodKey(r.Period, now, time.Time{})
	out := usageFleetStatus{FleetID: l.ID, Target: l.Target, TargetLabel: label, IncludeSelf: l.IncludeSelf, Members: []usageFleetMember{}}
	value := 0.0
	if l.IncludeSelf {
		m := usageFleetMember{Node: "self", Label: "this node", State: "self", Enforce: "self"}
		if selfErr != nil {
			m.State, m.Error = "error", selfErr.Error()
		} else {
			m.Value, m.Counted = self[r.Period][r.Metric], true
			value += m.Value
		}
		out.Members = append(out.Members, m)
	}
	for _, p := range members {
		m := usageFleetMember{Node: p.InstanceID, Label: peerDisplay(&p), Enforce: "unknown"}
		rd, ok := reads[p.InstanceID]
		switch {
		case !ok || rd.readAt.IsZero():
			m.State = "offline"
			if ok && rd.err != "" && rd.err != "offline" {
				m.State, m.Error = "error", rd.err
			}
		default:
			m.ReadAt = rd.readAt.UTC().Format(time.RFC3339)
			if slicesContains(rd.summary.Grants, PermUsageRulesEnforce) {
				m.Enforce = "granted"
			} else {
				m.Enforce = "not_granted"
			}
			switch {
			case !slicesContains(rd.summary.Grants, PermUsageRead) || rd.summary.Totals == nil:
				m.State = "not_shared"
			// Each member sums its own calendar period, so a member in
			// another time zone keeps counting across this node's rollover
			// rather than dropping out for hours every day.
			case now.Sub(rd.readAt) > usageFleetStale:
				m.State = "stale"
			default:
				m.State, m.Counted = "ok", true
				if rd.err != "" {
					m.State = "stale" // an older reading of this period still counts
				}
				m.Value = rd.summary.Totals[r.Period][r.Metric]
			}
			if rd.err != "" && m.Error == "" {
				m.Error = rd.err
			}
		}
		if m.Counted {
			value += m.Value
		}
		out.Members = append(out.Members, m)
	}
	for _, m := range out.Members {
		if m.Counted {
			out.Counted++
		}
	}
	target := usageRuleTarget{ruleKey: usageFleetRuleKey(l.ID), subject: usageFleetSubject(l), scope: usagerules.ScopeFleet, rule: r,
		owner: "fleet limit (" + label + ")", fleet: label, status: usageRuleStatus{LimitID: l.ID}}
	if l.IncludeSelf {
		for id := range local.agents {
			target.agents = append(target.agents, id)
		}
	}
	fired := local.fire(target, value, periodKey)
	out.usageRuleStatus = local.statuses[len(local.statuses)-1]
	if r.DryRun {
		return out
	}
	pct := out.Pct
	block, stop := false, false
	for _, s := range r.Steps {
		if pct >= s.At && s.Action == usagerules.ActionBlockSpawn {
			block = true
		}
		if pct >= s.At && s.Action == usagerules.ActionHardStop {
			stop = true
		}
	}
	var events []usageFleetEvent
	for _, f := range fired {
		ev := usageFleetEvent{Key: f.Key, Action: f.Step.Action}
		if f.Step.Action == usagerules.ActionNotify {
			if !slicesContains(targetStrings(f.Step.Notify), string(usagerules.TargetAgent)) {
				continue // operator-only: told here already
			}
		}
		events = append(events, ev)
	}
	// Members learn the fleet's percentage only: its spend and budget would
	// let one member work out the others' spend.
	req := usageFleetEnforceRequest{RuleKey: target.ruleKey, RuleName: r.Name, Fleet: label, PeriodKey: periodKey + "|b=" + formatUsageBudget(r.Budget),
		Metric: r.Metric, Period: r.Period, Pct: math.Round(pct), Block: block, Stop: stop,
		LeaseSeconds: int(usageFleetLease / time.Second)}
	var wg sync.WaitGroup
	for i := range out.Members {
		m := &out.Members[i]
		key := m.Node + "|" + target.ruleKey
		switch {
		case m.Node == "self":
			continue
		case m.Enforce == "not_granted":
			dropUsageFleetPending(key)
			continue
		case m.Enforce != "granted":
			// Not read yet (offline, or this node just started): keep the
			// steps for when it answers.
			queueUsageFleetEvents(key, events)
			keepUsageFleetHeld(key, want)
			continue
		}
		if block || stop {
			want[key] = true
		}
		wg.Add(1)
		goBackground(func() {
			defer wg.Done()
			reply, sendErr := sendUsageFleetEnforcement(m.Node, req, events)
			m.Applied = reply
			if sendErr != nil {
				m.SendErr = sendErr.Error()
			}
		})
	}
	// Sends run in parallel: one slow member never holds the others' leases
	// past their renewal.
	wg.Wait()
	return out
}

func queueUsageFleetEvents(key string, events []usageFleetEvent) {
	if len(events) == 0 {
		return
	}
	usageFleetSentMu.Lock()
	defer usageFleetSentMu.Unlock()
	pending := append(usageFleetPending[key], events...)
	if len(pending) > usageFleetPendingCap {
		pending = pending[len(pending)-usageFleetPendingCap:]
	}
	usageFleetPending[key] = pending
}

func dropUsageFleetPending(key string) {
	usageFleetSentMu.Lock()
	delete(usageFleetPending, key)
	usageFleetSentMu.Unlock()
}

// keepUsageFleetHeld keeps a lease this node holds on key while it cannot
// tell what to do with it, instead of releasing it.
func keepUsageFleetHeld(key string, want map[string]bool) {
	usageFleetSentMu.Lock()
	defer usageFleetSentMu.Unlock()
	if usageFleetSent[key] {
		want[key] = true
	}
}

func targetStrings(ts []usagerules.Target) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, string(t))
	}
	return out
}

// usageFleetEvent is one fired step for a member to perform.
type usageFleetEvent struct {
	Key    string            `json:"key"`
	Action usagerules.Action `json:"action"`
}

// usageFleetEnforceRequest is POST /api/usage/fleet-enforce: the limit's
// current reading, the steps that fired since the last delivery, and whether
// a spawn block or hard stop is in force (a lease of LeaseSeconds).
type usageFleetEnforceRequest struct {
	RuleKey      string            `json:"rule_key"`
	RuleName     string            `json:"rule_name"`
	Fleet        string            `json:"fleet"`
	PeriodKey    string            `json:"period_key"`
	Metric       usagerules.Metric `json:"metric"`
	Period       usagerules.Period `json:"period"`
	Value        float64           `json:"value"`
	Budget       float64           `json:"budget"`
	Pct          float64           `json:"pct"`
	Events       []usageFleetEvent `json:"events,omitempty"`
	Block        bool              `json:"block,omitempty"`
	Stop         bool              `json:"stop,omitempty"`
	LeaseSeconds int               `json:"lease_seconds"`
}

// usageFleetEnforceReply is what the member did.
type usageFleetEnforceReply struct {
	Agents   int    `json:"agents"`   // agents in the granted scope
	Notified int    `json:"notified"` // agents messaged for these events
	Stopped  int    `json:"stopped"`
	Block    bool   `json:"block"`
	Stop     bool   `json:"stop"`
	At       string `json:"at,omitempty"`
}

var (
	usageFleetSentMu sync.Mutex
	// usageFleetSent marks peer|rule pairs holding a lease (block or stop)
	// from the last send, so a lifted limit releases it at once.
	usageFleetSent = map[string]bool{}
	// usageFleetPending holds fired events not yet delivered to a peer.
	usageFleetPending = map[string][]usageFleetEvent{}
)

// usageFleetPendingCap bounds the undelivered events kept per peer and rule.
const usageFleetPendingCap = 32

// sendUsageFleetEnforcement delivers req to peer when it carries events or a
// lease, along with any events an earlier failed send left pending.
func sendUsageFleetEnforcement(peer string, req usageFleetEnforceRequest, events []usageFleetEvent) (*usageFleetEnforceReply, error) {
	key := peer + "|" + req.RuleKey
	usageFleetSentMu.Lock()
	pending := append(usageFleetPending[key], events...)
	if len(pending) > usageFleetPendingCap {
		pending = pending[len(pending)-usageFleetPendingCap:]
	}
	held := usageFleetSent[key]
	usageFleetSentMu.Unlock()
	if len(pending) == 0 && !req.Block && !req.Stop && !held {
		return nil, nil
	}
	req.Events = pending
	reply, err := postUsageFleetEnforce(peer, req)
	usageFleetSentMu.Lock()
	defer usageFleetSentMu.Unlock()
	if err != nil {
		usageFleetPending[key] = pending
		if req.Block || req.Stop {
			// It may have applied it before the reply was lost: owe it a
			// release once the limit lifts.
			usageFleetSent[key] = true
		}
		recordUsageFleetSend(peer, nil, err)
		return nil, err
	}
	delete(usageFleetPending, key)
	if req.Block || req.Stop {
		usageFleetSent[key] = true
	} else {
		delete(usageFleetSent, key)
	}
	recordUsageFleetSend(peer, reply, nil)
	return reply, nil
}

func recordUsageFleetSend(peer string, reply *usageFleetEnforceReply, err error) {
	usageFleetReadsMu.Lock()
	defer usageFleetReadsMu.Unlock()
	rd := usageFleetReads[peer]
	if rd == nil {
		rd = &usageFleetRead{}
		usageFleetReads[peer] = rd
	}
	rd.lastSend, rd.sendErr = reply, ""
	if err != nil {
		rd.sendErr = err.Error()
	}
}

func postUsageFleetEnforce(peer string, req usageFleetEnforceRequest) (*usageFleetEnforceReply, error) {
	p, err := db.GetFederationPeer(peer)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, errors.New("no longer trusted")
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), fedPeerViewTimeout)
	defer cancel()
	status, resp, err := usageFleetExchange(ctx, p, http.MethodPost, "/api/usage/fleet-enforce", body)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(resp, &e)
		if e.Error == "" {
			e.Error = http.StatusText(status)
		}
		return nil, fmt.Errorf("refused (%d): %s", status, e.Error)
	}
	var out usageFleetEnforceReply
	if err := json.Unmarshal(resp, &out); err != nil {
		return nil, fmt.Errorf("decode reply: %w", err)
	}
	return &out, nil
}

// releaseUsageFleetSends lifts, at once, the leases of peer|rule pairs no
// longer owed one (want nil: none is), instead of letting them run out.
func releaseUsageFleetSends(want map[string]bool) {
	usageFleetSentMu.Lock()
	var release []string
	for key := range usageFleetSent {
		if !want[key] {
			release = append(release, key)
		}
	}
	for key := range usageFleetPending {
		if want == nil {
			delete(usageFleetPending, key)
		}
	}
	usageFleetSentMu.Unlock()
	for _, key := range release {
		peer, ruleKey, _ := strings.Cut(key, "|")
		_, err := postUsageFleetEnforce(peer, usageFleetEnforceRequest{RuleKey: ruleKey, LeaseSeconds: 0})
		usageFleetSentMu.Lock()
		// A refusal (the grant was revoked, the peer untrusted) means there
		// is no lease left to lift there; only an unreachable peer is retried.
		if err == nil || strings.HasPrefix(err.Error(), "refused") || err.Error() == "no longer trusted" {
			delete(usageFleetSent, key)
		}
		usageFleetSentMu.Unlock()
		if err != nil {
			slog.Debug("usage fleet: release lease", "peer", peer, "rule", ruleKey, "error", err)
		}
	}
}
