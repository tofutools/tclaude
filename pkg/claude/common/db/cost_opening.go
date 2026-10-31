package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"
)

// CostReadLimitDays is how far back, counting today, a default cost read goes:
// the dashboard poll, the Costs tab's default view and the usage totals read no
// session_cost_daily row older than this. Only a Costs request for an older
// range reads further. The limit bounds the read, not what any figure means, so
// it must stay above the longest default period: the 31 days of a month to date.
const CostReadLimitDays = 40

// The delta walk is path-dependent, so reading only recent rows needs to know
// where the older rows left it. That position is stored, as of one day:
//
//   - cost_opening_convs: each conversation's baseline in every walk, and what
//     names it in the Costs view's Ignored list.
//   - cost_opening_sessions: each session's last counter per conversation,
//     which AnnotateCostCarry needs.
//   - cost_opening_spend: the first day each conversation spent, for the facts
//     that span all history.
//
// RefreshCostOpening derives all of it from the ledger. It is a cache and never
// the source of truth: every reader falls back to the full read when it is
// missing, marked stale or unreadable. Triggers mark it stale when a row older
// than its day changes (see migrateV292toV293), and a refresh audits it against
// the ledger before replacing it.

// CostWalkPosition is where a walk stands on a conversation.
type CostWalkPosition struct {
	Baseline float64 `json:"b,omitempty"`
	Session  string  `json:"s,omitempty"`
}

// CostLabel names a conversation in the Costs view: what its older rows' deltas
// leave behind for the Ignored list.
type CostLabel struct {
	SessionID   string `json:"s,omitempty"`
	UpdatedAtNS int64  `json:"t,omitempty"`
	Model       string `json:"m,omitempty"`
	Harness     string `json:"h,omitempty"`
}

// CostOpening is a conversation's position in each walk (indexed by costWalk)
// after the rows older than the opening day.
type CostOpening struct {
	Walks [3]CostWalkPosition `json:"w"`
	Real  CostLabel           `json:"r"`
	Mixed CostLabel           `json:"x"`
}

// CostSessionOpening is a session's last counter in each column, with the
// conversation that reported it.
type CostSessionOpening struct {
	RealConv    string  `json:"rc,omitempty"`
	RealUSD     float64 `json:"r,omitempty"`
	VirtualConv string  `json:"vc,omitempty"`
	VirtualUSD  float64 `json:"v,omitempty"`
}

// costSpendOpening is the first day a conversation spent in rows older than the
// opening day. ConvID is the raw conv_id, because that is what ignoring matches.
type costSpendOpening struct {
	FirstRealDay string
	FirstAnyDay  string
}

type costOpeningState struct {
	convs    map[string]CostOpening
	sessions map[string]CostSessionOpening
	spend    map[string]costSpendOpening
}

// costOpeningDay is the day to store the opening at on now's date. A day inside
// the limit, so the opening a refresh writes today still fits tomorrow, before
// the next refresh has run.
func costOpeningDay(now time.Time) string {
	return now.AddDate(0, 0, -(CostReadLimitDays - 2)).Format(costDayFormat)
}

type costOpeningMeta struct {
	day        string
	stale      bool
	generation int64
}

func (m costOpeningMeta) usable() bool { return m.day != "" && !m.stale }

func readCostOpeningMeta(q interface {
	QueryRow(query string, args ...any) *sql.Row
}) (costOpeningMeta, error) {
	var m costOpeningMeta
	err := q.QueryRow(`SELECT day, stale, generation FROM cost_opening_meta`).Scan(&m.day, &m.stale, &m.generation)
	if errors.Is(err, sql.ErrNoRows) {
		return costOpeningMeta{}, nil
	}
	return m, err
}

// buildCostOpening computes the opening as of day from the whole ledger, in
// walk order. It does not modify rows.
func buildCostOpening(rows []CostDailyRow, day string) costOpeningState {
	older := make([]CostDailyRow, 0, len(rows))
	for _, r := range rows {
		if r.Day < day {
			r.Opening = nil
			older = append(older, r)
		}
	}
	state := costOpeningState{
		convs:    map[string]CostOpening{},
		spend:    map[string]costSpendOpening{},
		sessions: annotateCostCarry(older, nil),
	}
	for _, walk := range costWalks {
		walkCostRows(older, walk, func(r *CostDailyRow, _ string, _, baseline float64) {
			key := costRowKey(r)
			opening := state.convs[key]
			opening.Walks[walk] = CostWalkPosition{Baseline: baseline, Session: r.SessionID}
			state.convs[key] = opening
		})
	}
	for _, label := range []struct {
		walk costWalk
		set  func(*CostOpening, CostLabel)
		get  func(*CostOpening) CostLabel
	}{
		{costWalkReal, func(o *CostOpening, l CostLabel) { o.Real = l }, func(o *CostOpening) CostLabel { return o.Real }},
		{costWalkMixed, func(o *CostOpening, l CostLabel) { o.Mixed = l }, func(o *CostOpening) CostLabel { return o.Mixed }},
	} {
		for _, d := range costDeltasOf(older, label.walk) {
			if d.ConvID == "" {
				continue
			}
			opening := state.convs[d.ConvID]
			current := label.get(&opening)
			if CompareCostInstants(d.UpdatedAtNS, current.UpdatedAtNS) >= 0 {
				current.SessionID, current.UpdatedAtNS = d.SessionID, d.UpdatedAtNS
				if d.Model != "" {
					current.Model = d.Model
				}
				if d.Harness != "" {
					current.Harness = d.Harness
				}
				label.set(&opening, current)
				state.convs[d.ConvID] = opening
			}
		}
	}
	for i := range older {
		r := &older[i]
		if r.CostUSD <= 0 && r.VirtualCostUSD <= 0 {
			continue
		}
		spend := state.spend[r.ConvID]
		if r.CostUSD > 0 && (spend.FirstRealDay == "" || r.Day < spend.FirstRealDay) {
			spend.FirstRealDay = r.Day
		}
		if spend.FirstAnyDay == "" || r.Day < spend.FirstAnyDay {
			spend.FirstAnyDay = r.Day
		}
		state.spend[r.ConvID] = spend
	}
	return state
}

// continueFromOpening returns the rows from day on, set up to walk on from
// state. The result is what a window read returns.
func continueFromOpening(rows []CostDailyRow, day string, state costOpeningState) []CostDailyRow {
	recent := make([]CostDailyRow, 0, len(rows))
	for _, r := range rows {
		if r.Day >= day {
			recent = append(recent, r)
		}
	}
	attachCostOpening(recent, state)
	return recent
}

func attachCostOpening(rows []CostDailyRow, state costOpeningState) {
	annotateCostCarry(rows, state.sessions)
	openings := make(map[string]*CostOpening, len(state.convs))
	for i := range rows {
		r := &rows[i]
		key := costRowKey(r)
		opening, seen := openings[key]
		if !seen {
			if stored, ok := state.convs[key]; ok {
				opening = &stored
			}
			openings[key] = opening
		}
		r.Opening = opening
	}
}

// verifyCostOpening reports whether walking on from state gives exactly the
// full walk's deltas for every day from day on.
func verifyCostOpening(rows []CostDailyRow, day string, state costOpeningState) error {
	recent := continueFromOpening(rows, day, state)
	for _, walk := range costWalks {
		var full []CostDelta
		for _, d := range costDeltasOf(rows, walk) {
			if d.Day >= day {
				full = append(full, d)
			}
		}
		if got := costDeltasOf(recent, walk); !slices.Equal(got, full) {
			return fmt.Errorf("walk %d gives %d deltas from the opening and %d from the full read", walk, len(got), len(full))
		}
	}
	return nil
}

// CostOpeningDue reports whether the stored opening needs RefreshCostOpening.
// It reads no ledger rows.
func CostOpeningDue(now time.Time) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	meta, err := readCostOpeningMeta(d)
	if err != nil {
		return false, err
	}
	return !meta.usable() || meta.day < costOpeningDay(now), nil
}

// RefreshCostOpening rebuilds the stored opening from the whole ledger. It is
// the one regular read of old rows, off the request path: once a day, and after
// a change to an old row. Running inside a write transaction keeps a concurrent
// write from slipping between the read and the store.
//
// The position being replaced is audited first: nothing should have changed it
// unmarked, so a difference means a write path the triggers miss. A new opening
// that does not reproduce the full walk is not stored, which leaves readers on
// the full read.
func RefreshCostOpening(now time.Time) error {
	d, err := Open()
	if err != nil {
		return err
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	meta, err := readCostOpeningMeta(tx)
	if err != nil {
		return err
	}
	rows, err := queryCostDailyRows(tx, allCostDailyRowsQuery)
	if err != nil {
		return err
	}
	if meta.usable() {
		auditCostOpening(tx, rows, meta.day)
	}
	day := costOpeningDay(now)
	state := buildCostOpening(rows, day)
	if err := verifyCostOpening(rows, day, state); err != nil {
		slog.Error("cost opening balance failed verification; cost reads stay on the full history", "day", day, "error", err)
		if _, err := tx.Exec(`UPDATE cost_opening_meta SET day = '', stale = 0, generation = generation + 1`); err != nil {
			return err
		}
		return tx.Commit()
	}
	if err := storeCostOpening(tx, state); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE cost_opening_meta SET day = ?, stale = 0, generation = generation + 1`, day); err != nil {
		return err
	}
	return tx.Commit()
}

func auditCostOpening(tx *sql.Tx, rows []CostDailyRow, day string) {
	stored, err := loadCostOpeningState(tx)
	if err != nil {
		slog.Error("cost opening balance is unreadable; replacing it", "day", day, "error", err)
		return
	}
	want := buildCostOpening(rows, day)
	if !maps.Equal(want.convs, stored.convs) || !maps.Equal(want.sessions, stored.sessions) || !maps.Equal(want.spend, stored.spend) {
		slog.Error("cost opening balance no longer matched the ledger; replacing it", "day", day)
	}
}

func storeCostOpening(tx *sql.Tx, state costOpeningState) error {
	for _, table := range []string{"cost_opening_convs", "cost_opening_sessions", "cost_opening_spend"} {
		if _, err := tx.Exec(`DELETE FROM ` + table); err != nil {
			return err
		}
	}
	for key, opening := range state.convs {
		blob, err := json.Marshal(opening)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO cost_opening_convs (walk_key, state) VALUES (?, ?)`, key, string(blob)); err != nil {
			return err
		}
	}
	for sessionID, opening := range state.sessions {
		blob, err := json.Marshal(opening)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO cost_opening_sessions (session_id, state) VALUES (?, ?)`, sessionID, string(blob)); err != nil {
			return err
		}
	}
	for convID, spend := range state.spend {
		if _, err := tx.Exec(`INSERT INTO cost_opening_spend (conv_id, first_real_day, first_any_day) VALUES (?, ?, ?)`,
			convID, spend.FirstRealDay, spend.FirstAnyDay); err != nil {
			return err
		}
	}
	return nil
}

func loadCostOpeningState(tx *sql.Tx) (costOpeningState, error) {
	state := costOpeningState{convs: map[string]CostOpening{}, sessions: map[string]CostSessionOpening{}, spend: map[string]costSpendOpening{}}
	if err := scanCostOpeningBlobs(tx, `SELECT walk_key, state FROM cost_opening_convs`, nil, state.convs); err != nil {
		return state, err
	}
	if err := scanCostOpeningBlobs(tx, `SELECT session_id, state FROM cost_opening_sessions`, nil, state.sessions); err != nil {
		return state, err
	}
	rows, err := tx.Query(`SELECT conv_id, first_real_day, first_any_day FROM cost_opening_spend`)
	if err != nil {
		return state, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var convID string
		var spend costSpendOpening
		if err := rows.Scan(&convID, &spend.FirstRealDay, &spend.FirstAnyDay); err != nil {
			return state, err
		}
		state.spend[convID] = spend
	}
	return state, rows.Err()
}

func scanCostOpeningBlobs[T any](q costRowQuerier, query string, args []any, out map[string]T) error {
	rows, err := q.Query(query, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var key, blob string
		if err := rows.Scan(&key, &blob); err != nil {
			return err
		}
		var value T
		if err := json.Unmarshal([]byte(blob), &value); err != nil {
			return fmt.Errorf("opening for %s: %w", key, err)
		}
		out[key] = value
	}
	return rows.Err()
}

// loadCostOpeningsFor reads the stored openings of the named keys, in chunks.
func loadCostOpeningsFor[T any](q costRowQuerier, query string, keys []string, out map[string]T) error {
	for _, chunk := range chunkStrings(keys, batchChunkSize) {
		clause, args := inClause(chunk)
		if err := scanCostOpeningBlobs(q, query+clause, args, out); err != nil {
			return err
		}
	}
	return nil
}

// recentCostRowsFrom scans from the day index: the walk index sorts by
// conversation, so a day filter on it would visit every older entry.
const recentCostRowsFrom = ` FROM session_cost_daily INDEXED BY idx_session_cost_daily_day WHERE day >= ?`

// CostLedger is what a window read returns.
type CostLedger struct {
	// Rows walk to the exact deltas for the window. Deltas before the window
	// are not meaningful, so callers must keep to day-keyed totals.
	Rows []CostDailyRow
	// Labels name the ignored conversations whose naming rows were not read,
	// by conv id. Only a read withIgnored from the opening sets them.
	Labels map[string]CostOpening
	// OpeningDay is the oldest day read when the stored opening stood in for
	// the older rows, else "".
	OpeningDay string
	// NeedsRefresh says the stored opening was missing, stale or unreadable,
	// so this default read had to go through the full history.
	NeedsRefresh bool
}

// CostLedgerWindow returns what a delta walk needs to produce exactly the
// deltas of the whole ledger for days in [fromDay, toDay]; toDay "" is
// open-ended. A window inside the stored opening reads only rows from its day
// on. Anything else takes the exact read: an older range because it asked for
// one, and a missing or stale opening because a total must never rest on it.
func CostLedgerWindow(fromDay, toDay string, withIgnored bool) (CostLedger, error) {
	ledger, older, err := costLedgerFromOpening(fromDay, toDay, withIgnored)
	if err == nil && ledger != nil {
		return *ledger, nil
	}
	if err != nil {
		slog.Warn("cost opening balance unusable; reading the full cost history", "error", err)
		markCostOpeningStale()
	}
	if !older {
		slog.Debug("cost opening balance missing or stale; reading the full cost history", "from", fromDay)
	}
	rows, err := costDailyRowsForWalk(fromDay, toDay, withIgnored)
	if err != nil {
		return CostLedger{}, err
	}
	return CostLedger{Rows: rows, NeedsRefresh: !older}, nil
}

// markCostOpeningStale makes a refresh rebuild state a reader could not use. The
// read goes on without it, so a failure here costs only the rebuild.
func markCostOpeningStale() {
	d, err := Open()
	if err != nil {
		return
	}
	if _, err := d.Exec(`UPDATE cost_opening_meta SET stale = 1`); err != nil {
		slog.Debug("cost opening: mark stale", "error", err)
	}
}

// costLedgerFromOpening returns nil when the opening cannot serve the window;
// older says that is only because the window starts before it.
func costLedgerFromOpening(fromDay, toDay string, withIgnored bool) (ledger *CostLedger, older bool, err error) {
	d, err := Open()
	if err != nil {
		return nil, false, err
	}
	meta, err := readCostOpeningMeta(d)
	if err != nil {
		return nil, false, err
	}
	if !meta.usable() {
		return nil, false, nil
	}
	if fromDay < meta.day {
		return nil, true, nil
	}
	query := `SELECT ` + costDailyColumns + recentCostRowsFrom
	args := []any{meta.day}
	if toDay != "" {
		query += ` AND ` + costRowsUpTo(withIgnored)
		args = append(args, toDay)
	}
	rows, err := scanCostDailyRows(d, query+` ORDER BY `+costWalkOrder, args...)
	if err != nil {
		return nil, false, err
	}
	state := costOpeningState{convs: map[string]CostOpening{}, sessions: map[string]CostSessionOpening{}}
	convKeys, sessionIDs := map[string]bool{}, map[string]bool{}
	for i := range rows {
		convKeys[costRowKey(&rows[i])] = true
		sessionIDs[rows[i].SessionID] = true
	}
	if err := loadCostOpeningsFor(d, `SELECT walk_key, state FROM cost_opening_convs WHERE walk_key `, slices.Collect(maps.Keys(convKeys)), state.convs); err != nil {
		return nil, false, err
	}
	if err := loadCostOpeningsFor(d, `SELECT session_id, state FROM cost_opening_sessions WHERE session_id `, slices.Collect(maps.Keys(sessionIDs)), state.sessions); err != nil {
		return nil, false, err
	}
	out := &CostLedger{OpeningDay: meta.day}
	if withIgnored {
		ignored, err := CostIgnoredConvs()
		if err != nil {
			return nil, false, err
		}
		out.Labels = map[string]CostOpening{}
		if err := loadCostOpeningsFor(d, `SELECT walk_key, state FROM cost_opening_convs WHERE walk_key `, slices.Collect(maps.Keys(ignored)), out.Labels); err != nil {
			return nil, false, err
		}
	}
	// The reads above are separate statements. A refresh landing between them
	// would pair rows and openings of different days.
	after, err := readCostOpeningMeta(d)
	if err != nil {
		return nil, false, err
	}
	if after != meta {
		return nil, false, nil
	}
	attachCostOpening(rows, state)
	out.Rows = rows
	return out, false, nil
}

// FirstCountedCostDay returns the earliest day with recorded spend outside the
// ignored conversations, or "" when there is none. withWhatIf also counts
// subscription estimates.
func FirstCountedCostDay(withWhatIf bool) (string, error) {
	spent, firstColumn := `cost_usd > 0`, `first_real_day`
	if withWhatIf {
		spent, firstColumn = `(cost_usd > 0 OR virtual_cost_usd > 0)`, `first_any_day`
	}
	d, err := Open()
	if err != nil {
		return "", err
	}
	source := `session_cost_daily`
	args := []any{}
	meta, err := readCostOpeningMeta(d)
	if err != nil {
		slog.Warn("cost opening balance unusable; reading the full cost history", "error", err)
	}
	if err == nil && meta.usable() {
		var first sql.NullString
		err := d.QueryRow(`SELECT MIN(` + firstColumn + `) FROM cost_opening_spend
			WHERE ` + firstColumn + ` <> '' AND ` + costNotIgnored).Scan(&first)
		if err == nil && first.Valid {
			return first.String, nil
		}
		if err == nil {
			source, args = `session_cost_daily INDEXED BY idx_session_cost_daily_day`, []any{meta.day}
		} else {
			slog.Warn("cost opening balance unusable; reading the full cost history", "error", err)
		}
	}
	clause := ``
	if len(args) > 0 {
		clause = `day >= ? AND `
	}
	var day string
	err = d.QueryRow(`SELECT day FROM `+source+`
		WHERE `+clause+spent+` AND `+costNotIgnored+` ORDER BY day LIMIT 1`, args...).Scan(&day)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return day, err
}

// HasCountedRealCost is HasAnyRealCost minus the ignored conversations: the
// same answer as finding a real-cost row among the counted history.
func HasCountedRealCost() (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	var exists int
	const realCounted = `cost_usd > 0 AND ` + costNotIgnored
	meta, err := readCostOpeningMeta(d)
	if err == nil && meta.usable() {
		err = d.QueryRow(`SELECT EXISTS(SELECT 1 FROM cost_opening_spend WHERE first_real_day <> '' AND `+costNotIgnored+`)
			OR EXISTS(SELECT 1`+recentCostRowsFrom+` AND `+realCounted+`)`, meta.day).Scan(&exists)
		if err == nil {
			return exists == 1, nil
		}
	}
	if err != nil {
		slog.Warn("cost opening balance unusable; reading the full cost history", "error", err)
	}
	err = d.QueryRow(`SELECT EXISTS(SELECT 1 FROM session_cost_daily WHERE ` + realCounted + `)`).Scan(&exists)
	return exists == 1, err
}

// costOpeningGuard keeps a rewrite of a conversation's cost history from
// spending the opening when it changes nothing. History followers zero a
// conversation's rows and write them back on every flush, which trips the
// triggers although the rows end up as they were.
type costOpeningGuard struct {
	before string
	active bool
}

func costOlderRowsFingerprint(tx *sql.Tx, convID, sessionID string) (string, error) {
	var fingerprint string
	err := tx.QueryRow(`SELECT COALESCE(group_concat(line, char(10)), '') FROM (
		SELECT session_id || '|' || conv_id || '|' || day || '|' || cost_usd || '|' || virtual_cost_usd
			|| '|' || COALESCE(updated_at, '') || '|' || model || '|' || harness AS line
		FROM session_cost_daily
		WHERE ((conv_id = ? AND conv_id <> '') OR session_id = ?)
		  AND day < (SELECT day FROM cost_opening_meta)
		ORDER BY session_id, conv_id, day)`, convID, sessionID).Scan(&fingerprint)
	return fingerprint, err
}

// guardCostOpening runs before the rewrite; settle runs after it, in the same
// transaction.
func guardCostOpening(tx *sql.Tx, convID, sessionID string) (costOpeningGuard, error) {
	meta, err := readCostOpeningMeta(tx)
	if err != nil || !meta.usable() {
		return costOpeningGuard{}, err
	}
	before, err := costOlderRowsFingerprint(tx, convID, sessionID)
	if err != nil {
		return costOpeningGuard{}, err
	}
	return costOpeningGuard{before: before, active: strings.TrimSpace(before) != ""}, nil
}

func (g costOpeningGuard) settle(tx *sql.Tx, convID, sessionID string) error {
	if !g.active {
		return nil
	}
	after, err := costOlderRowsFingerprint(tx, convID, sessionID)
	if err != nil || after != g.before {
		return err
	}
	_, err = tx.Exec(`UPDATE cost_opening_meta SET stale = 0`)
	return err
}
