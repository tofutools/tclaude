package db

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Standing-order target kinds. These deliberately reuse cron's persisted
// string values for compatibility, but "conv" is only the historical spelling
// for a SINGLE STABLE AGENT target. The durable key is always target_agent;
// the current conversation is resolved from that actor at read/delivery time.
const (
	StandingTargetConv  = CronTargetConv
	StandingTargetGroup = CronTargetGroup
)

// Standing-order disabled_reason markers. Same values and same meaning as the
// cron markers, so the group retire/resume machinery can treat both tables
// identically: only rows tclaude itself paused carry a marker, and only those
// are auto-re-enabled.
const (
	StandingDisabledReasonNone         = CronDisabledReasonNone
	StandingDisabledReasonAgentRetired = CronDisabledReasonAgentRetired
	StandingDisabledReasonGroupRetired = CronDisabledReasonGroupRetired
)

// Trigger events. v1 ships exactly one, but the column is a string rather than
// a flag so adding tool/command triggers later is a data change and not a
// schema change — and so the dashboard's trigger column never has to be
// rewritten.
const (
	// StandingTriggerSessionStart matches the harness SessionStart lifecycle
	// event. Its sources are the harness's own `source` values, normalized by
	// NormalizeTriggerSources.
	StandingTriggerSessionStart = "session.start"
)

// SessionStart sources a trigger may select. An order with no sources matches
// every source.
const (
	StandingSourceStartup = "startup"
	StandingSourceResume  = "resume"
	StandingSourceClear   = "clear"
	StandingSourceCompact = "compact"
)

// Timing guarantees an order may REQUIRE. There is deliberately no per-order
// fallback field: an order states the timing it needs, and a harness that
// cannot meet it produces a visible `unsupported-timing` outcome rather than a
// silent downgrade. Per-order fallback would mean one order behaving
// differently on two agents in the same group, which is the hardest kind of
// behaviour to reason about and the hardest to explain after the fact.
const (
	// StandingTimingSameContinuation requires the reminder to reach the next
	// model request within the current turn (hook context).
	StandingTimingSameContinuation = "same-continuation"
	// StandingTimingNextTurn accepts durable delivery that may wait until the
	// current turn ends (the ordinary message path).
	StandingTimingNextTurn = "next-turn"
)

// Cadence controls how often a matching order re-delivers.
const (
	// StandingCadenceAlways delivers on every matching event. The right
	// default for session.start, which fires rarely and whose whole point is
	// to re-state guidance after a boundary — including after each compaction.
	StandingCadenceAlways = "always"
	// StandingCadenceOncePerGeneration delivers at most once per
	// (order, revision, conversation generation). Note the consequence for
	// SessionStart: compaction does NOT rotate the conv id, so an order with
	// this cadence is delivered on the first boundary of a conversation and
	// then stays quiet through later compactions of the same conversation.
	// That is a real choice between "tell me once" and "tell me after every
	// compaction", and the operator makes it explicitly.
	StandingCadenceOncePerGeneration = "once-per-generation"
)

// Evaluation outcomes. These strings are the shared vocabulary between the
// evaluator, the delivery ledger, `orders explain`, and the dashboard — one
// spelling, so the UI never has to re-derive meaning from prose.
const (
	// StandingOutcomeDelivered — matched, and the reminder reached the agent
	// on the transport its timing asked for.
	StandingOutcomeDelivered = "delivered"
	// StandingOutcomeNoMatch — evaluated cleanly; the trigger did not match.
	StandingOutcomeNoMatch = "no-match"
	// StandingOutcomeSuppressedCadence — matched, but already delivered in
	// this cadence epoch.
	StandingOutcomeSuppressedCadence = "suppressed-cadence"
	// StandingOutcomeDisabled — the order is disabled; see disabled_reason.
	StandingOutcomeDisabled = "disabled"
	// StandingOutcomeOutOfScope — this agent is not in the order's target set.
	StandingOutcomeOutOfScope = "out-of-scope"
	// StandingOutcomeUnsupportedTiming — the harness cannot honour the
	// required timing, so NOTHING was delivered. Deliberately distinct from a
	// degraded delivery.
	StandingOutcomeUnsupportedTiming = "unsupported-timing"
	// StandingOutcomeDegradedTransport — delivered, but on a weaker transport
	// than the order asked for.
	StandingOutcomeDegradedTransport = "degraded-transport"
	// StandingOutcomeNotEvaluatedTrimmed — the hook payload was trimmed before
	// it reached the evaluator, so the trigger COULD NOT BE EVALUATED. This is
	// not the same as no-match and must never be presented as one: the
	// brokered hook path drops ToolInput/ToolResponse on oversized events
	// (see trimOversizedHookBody), and "we could not tell" is a different
	// answer from "it did not fire".
	StandingOutcomeNotEvaluatedTrimmed = "not-evaluated-trimmed"
	// StandingOutcomeTransportUnimplemented — the order matched and the
	// harness could in principle carry it, but the transport its timing
	// selects is not wired up yet, so nothing was delivered.
	// It is deliberately its own outcome rather than being folded into
	// unsupported-timing: the harness is not the limitation, tclaude is.
	StandingOutcomeTransportUnimplemented = "transport-unimplemented"
	// StandingOutcomeNotEvaluatedBusy — the order matched, but another
	// delivery path held this conversation's delivery lock, so the cadence
	// read-modify-write could not be performed safely and the order was NOT
	// delivered on this boundary.
	//
	// It is recorded rather than dropped silently because a skipped delivery
	// looks identical to a healthy one from the outside, and it deliberately
	// does NOT count as a delivery: the cadence stays open so the next
	// boundary delivers the order.
	StandingOutcomeNotEvaluatedBusy = "not-evaluated-busy"
	// StandingOutcomeDeliveryFailed — a supported transport was attempted but
	// failed before the reminder was durably queued or written. It remains
	// retryable and must not satisfy cadence.
	StandingOutcomeDeliveryFailed = "delivery-failed"
)

// Transports a delivery may use.
const (
	StandingTransportHookContext = "hook-context"
	StandingTransportMessage     = "message"
	StandingTransportNone        = "none"
)

// StandingSummaryMaxLen caps the standing-order text that becomes
// model-visible context. Long orders re-injected at every boundary would
// recreate exactly the context bloat this feature exists to reduce, and the
// full text of anything longer belongs behind `orders show`.
const StandingSummaryMaxLen = 2000

// ErrStandingOrderNameTaken is the canonical rejection for a duplicate order
// name. Names are the stable handle the CLI and the skill address orders by,
// so they are unique across the store.
var ErrStandingOrderNameTaken = errors.New("standing order name already exists")

// ErrStandingOrderInvalid classifies a write rejected by Validate.
var ErrStandingOrderInvalid = errors.New("invalid standing order")

// ErrStandingOrderRevisionConflict reports an edit based on a stale revision.
// Dashboard dialogs carry the revision they opened so two operator tabs cannot
// silently overwrite one another's trigger or model-visible text.
var ErrStandingOrderRevisionConflict = errors.New("standing order revision conflict")

// StandingOrder is a row in agent_standing_orders: durable guidance delivered
// when a trigger matches rather than on a wall clock.
//
// The target fields mirror AgentCronJob exactly — OwnerAgent / TargetAgent are
// the stable, rotation-immune actor keys; OwnerConv / TargetConv are read-only
// current-generation routing facts resolved at read time; TargetKind
// discriminates and TargetRole filters a group fan-out against the live roster
// at delivery time.
// Reusing that shape is what lets the group retire/resume hygiene, the
// operator-attribution stamp, and the dashboard's target column serve both
// tables.
type StandingOrder struct {
	ID   int64
	Name string
	// Revision bumps whenever the delivered TEXT or the trigger changes. It is
	// part of the cadence key, so editing an order re-arms it rather than
	// leaving the recipient pinned to a stale "already delivered" record.
	Revision int64

	OwnerAgent  string
	OwnerConv   string
	TargetKind  string // StandingTargetConv (single stable agent) | StandingTargetGroup
	TargetAgent string
	TargetConv  string
	GroupID     int64
	TargetRole  string

	// Summary is the text injected into the agent's context. Short by
	// construction (StandingSummaryMaxLen).
	Summary string

	TriggerEvent string
	// TriggerSources filters a lifecycle trigger to particular sources. Empty
	// means "every source".
	TriggerSources []string

	Timing  string
	Cadence string

	Enabled        bool
	DisabledReason string
	// OperatorAuthored records that the human created this order directly,
	// with no sender agent to attribute it to. As with cron, it is an EXPLICIT
	// marker rather than something inferred from an empty owner: other
	// internal paths also leave the owner empty, so empty-owner alone cannot
	// safely mean "the operator". It matters more here than it does for cron,
	// because an order's text becomes high-authority model-visible context and
	// the recipient should be able to tell who authored it.
	OperatorAuthored bool

	CreatedAt time.Time
	UpdatedAt time.Time
}

// IsGroupTarget reports whether the order fans out to a group. Callers MUST
// use this rather than GroupID > 0 — exactly as for cron, a single-agent
// order routed through a shared group also carries a non-zero GroupID.
func (o *StandingOrder) IsGroupTarget() bool {
	return o.TargetKind == StandingTargetGroup
}

// TriggerLabel renders the trigger for display. Exposed here so the CLI and
// the dashboard print the same string rather than each composing their own
// from event + sources.
func (o *StandingOrder) TriggerLabel() string {
	if len(o.TriggerSources) == 0 {
		return o.TriggerEvent + " (any source)"
	}
	return o.TriggerEvent + " (" + strings.Join(o.TriggerSources, ", ") + ")"
}

// MatchesSource reports whether the order's trigger accepts a given harness
// source value. An order with no sources accepts all of them.
func (o *StandingOrder) MatchesSource(source string) bool {
	if len(o.TriggerSources) == 0 {
		return true
	}
	source = strings.ToLower(strings.TrimSpace(source))
	for _, s := range o.TriggerSources {
		if s == source {
			return true
		}
	}
	return false
}

// StandingDelivery is a row in agent_standing_order_deliveries: one recorded
// evaluation outcome. The ledger is what makes `orders explain` answerable
// after the fact, and it is deliberately NOT the inbox — an inline reminder
// the model already saw must not also consume the recipient's unread
// backpressure budget.
type StandingDelivery struct {
	ID            int64
	OrderID       int64
	OrderRevision int64
	TargetConv    string
	// Epoch is the cadence key the delivery was recorded under. For
	// StandingCadenceOncePerGeneration it is the conversation generation.
	Epoch     string
	Outcome   string
	Transport string
	Harness   string
	Detail    string
	CreatedAt time.Time
}

// NormalizeTriggerSources lowercases, trims, de-duplicates and sorts trigger
// sources so the stored value is canonical. Sorting matters because the joined
// string is compared and displayed; two orders that select the same sources
// must not read as different.
func NormalizeTriggerSources(sources []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(sources))
	for _, s := range sources {
		s = strings.ToLower(strings.TrimSpace(s))
		if s == "" {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// ValidStandingSource reports whether s is a SessionStart source tclaude
// recognises. Unknown sources are rejected at the write path rather than
// silently stored, so an order cannot sit permanently unmatched because of a
// typo the operator never sees.
func ValidStandingSource(s string) bool {
	switch s {
	case StandingSourceStartup, StandingSourceResume, StandingSourceClear, StandingSourceCompact:
		return true
	}
	return false
}

// Validate checks an order before it is written. It normalizes in place, so
// callers get back the canonical form they will later read.
func (o *StandingOrder) Validate() error {
	o.Name = strings.TrimSpace(o.Name)
	if o.Name == "" {
		return fmt.Errorf("%w: name is required", ErrStandingOrderInvalid)
	}
	o.Summary = strings.TrimSpace(o.Summary)
	if o.Summary == "" {
		return fmt.Errorf("%w: summary is required", ErrStandingOrderInvalid)
	}
	if len(o.Summary) > StandingSummaryMaxLen {
		return fmt.Errorf("%w: summary is %d bytes, limit is %d",
			ErrStandingOrderInvalid, len(o.Summary), StandingSummaryMaxLen)
	}

	switch o.TargetKind {
	case StandingTargetConv, StandingTargetGroup:
	default:
		return fmt.Errorf("%w: target kind %q", ErrStandingOrderInvalid, o.TargetKind)
	}
	o.OwnerAgent = strings.TrimSpace(o.OwnerAgent)
	o.TargetAgent = strings.TrimSpace(o.TargetAgent)
	switch {
	case o.OwnerAgent == "" && strings.TrimSpace(o.OwnerConv) != "":
		return fmt.Errorf("%w: owner conversation cannot replace a stable owner agent id", ErrStandingOrderInvalid)
	case o.OwnerAgent != "" && !strings.HasPrefix(o.OwnerAgent, AgentIDPrefix):
		return fmt.Errorf("%w: owner agent %q is not a stable agent id", ErrStandingOrderInvalid, o.OwnerAgent)
	case o.IsGroupTarget() && o.GroupID <= 0:
		return fmt.Errorf("%w: group target needs a group id", ErrStandingOrderInvalid)
	case o.IsGroupTarget() && o.TargetAgent != "":
		return fmt.Errorf("%w: group target cannot carry a single target agent", ErrStandingOrderInvalid)
	case !o.IsGroupTarget() && o.TargetAgent == "":
		return fmt.Errorf("%w: single-agent target needs a stable agent id", ErrStandingOrderInvalid)
	case !o.IsGroupTarget() && !strings.HasPrefix(o.TargetAgent, AgentIDPrefix):
		return fmt.Errorf("%w: target agent %q is not a stable agent id", ErrStandingOrderInvalid, o.TargetAgent)
	}
	if !o.IsGroupTarget() && strings.TrimSpace(o.TargetRole) != "" {
		return fmt.Errorf("%w: target role applies only to a group target", ErrStandingOrderInvalid)
	}

	if o.TriggerEvent != StandingTriggerSessionStart {
		return fmt.Errorf("%w: unknown trigger event %q (v1 supports %q)",
			ErrStandingOrderInvalid, o.TriggerEvent, StandingTriggerSessionStart)
	}
	o.TriggerSources = NormalizeTriggerSources(o.TriggerSources)
	for _, s := range o.TriggerSources {
		if !ValidStandingSource(s) {
			return fmt.Errorf("%w: unknown trigger source %q", ErrStandingOrderInvalid, s)
		}
	}

	switch o.Timing {
	case StandingTimingSameContinuation, StandingTimingNextTurn:
	default:
		return fmt.Errorf("%w: unknown timing %q", ErrStandingOrderInvalid, o.Timing)
	}
	switch o.Cadence {
	case StandingCadenceAlways, StandingCadenceOncePerGeneration:
	default:
		return fmt.Errorf("%w: unknown cadence %q", ErrStandingOrderInvalid, o.Cadence)
	}
	return nil
}

// standingSelect is the shared SELECT for reading standing orders. As with
// cronSelect, owner/target are keyed on agent_id and each LEFT JOIN resolves
// the actor back to its CURRENT conv, so a reincarnation or /clear does not
// strand an order pointed at a dead generation. LEFT JOIN + COALESCE so a
// group-target order (target_agent "") or an owner-less operator order keeps
// an empty string rather than dropping the row.
const standingSelect = `SELECT o.id, o.name, o.revision,
	o.owner_agent, COALESCE(ow.current_conv_id, ''),
	o.target_kind, o.target_agent, COALESCE(tg.current_conv_id, ''),
	o.group_id, o.target_role, o.summary,
	o.trigger_event, o.trigger_sources, o.timing, o.cadence,
	o.enabled, o.disabled_reason, o.operator_authored,
	o.created_at, o.updated_at
	FROM agent_standing_orders o
	LEFT JOIN agents ow ON ow.agent_id = o.owner_agent
	LEFT JOIN agents tg ON tg.agent_id = o.target_agent`

func scanStandingOrder(s rowScanner) (*StandingOrder, error) {
	var (
		o          StandingOrder
		sources    string
		enabled    int
		operator   int
		createdRaw string
		updatedRaw string
	)
	if err := s.Scan(
		&o.ID, &o.Name, &o.Revision,
		&o.OwnerAgent, &o.OwnerConv,
		&o.TargetKind, &o.TargetAgent, &o.TargetConv,
		&o.GroupID, &o.TargetRole, &o.Summary,
		&o.TriggerEvent, &sources, &o.Timing, &o.Cadence,
		&enabled, &o.DisabledReason, &operator,
		&createdRaw, &updatedRaw,
	); err != nil {
		return nil, err
	}
	o.TriggerSources = NormalizeTriggerSources(strings.Split(sources, ","))
	o.Enabled = enabled != 0
	o.OperatorAuthored = operator != 0
	o.CreatedAt = parseStandingTime(createdRaw)
	o.UpdatedAt = parseStandingTime(updatedRaw)
	return &o, nil
}

func parseStandingTime(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}
	}
	return t
}

func formatStandingTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// InsertStandingOrder writes a new order. CreatedAt/UpdatedAt are stamped
// server-side; the caller's values are ignored. Revision starts at 1.
func InsertStandingOrder(o *StandingOrder) (int64, error) {
	if err := o.Validate(); err != nil {
		return 0, err
	}
	d, err := Open()
	if err != nil {
		return 0, err
	}
	now := formatStandingTime(time.Now())
	res, err := d.Exec(`INSERT INTO agent_standing_orders
		(name, revision, owner_agent, target_kind, target_agent, group_id, target_role,
		 summary, trigger_event, trigger_sources, timing, cadence,
		 enabled, disabled_reason, operator_authored, created_at, updated_at)
		VALUES (?, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		o.Name, o.OwnerAgent, o.TargetKind, o.TargetAgent, o.GroupID, o.TargetRole,
		o.Summary, o.TriggerEvent, strings.Join(o.TriggerSources, ","), o.Timing, o.Cadence,
		boolToInt(o.Enabled), o.DisabledReason, boolToInt(o.OperatorAuthored), now, now)
	if err != nil {
		if isUniqueViolation(err) {
			return 0, fmt.Errorf("InsertStandingOrder %q: %w", o.Name, ErrStandingOrderNameTaken)
		}
		return 0, err
	}
	return res.LastInsertId()
}

// GetStandingOrder reads one order by id. Returns (nil, nil) when absent.
func GetStandingOrder(id int64) (*StandingOrder, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	o, err := scanStandingOrder(d.QueryRow(standingSelect+` WHERE o.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return o, err
}

// GetStandingOrderByName reads one order by its stable name. Returns
// (nil, nil) when absent.
func GetStandingOrderByName(name string) (*StandingOrder, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	o, err := scanStandingOrder(d.QueryRow(standingSelect+` WHERE o.name = ?`, strings.TrimSpace(name)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return o, err
}

// ListStandingOrdersForExplain returns the orders a dry-run should evaluate:
// the same set the hot path would load, minus the enabled filter so a disabled
// order still reports WHY it did not fire.
//
// Sharing the retired-owner condition with ListEnabledStandingOrdersForEvent is
// the point. `explain` exists to answer "why didn't this fire", and an order
// whose owner has retired is invisible to the hot path; listing it here would
// make explain confidently predict a delivery that can never happen.
func ListStandingOrdersForExplain() ([]*StandingOrder, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	return listStandingOrders(d,
		standingSelect+` WHERE (o.owner_agent = '' OR ow.retired_at = '') ORDER BY o.id`)
}

// ListStandingOrders returns every order, ordered by id asc.
func ListStandingOrders() ([]*StandingOrder, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	return listStandingOrders(d, standingSelect+` ORDER BY o.id`)
}

// ListEnabledStandingOrdersForEvent returns the enabled orders whose trigger
// event matches. This is the hot read on the hook path, so it is a single
// indexed query rather than a full list plus a filter — and callers are
// expected to hold the result rather than re-issue it per event.
func ListEnabledStandingOrdersForEvent(event string) ([]*StandingOrder, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	return listStandingOrders(d,
		standingSelect+` WHERE o.enabled = 1 AND o.trigger_event = ?
		AND (o.owner_agent = '' OR ow.retired_at = '')
		ORDER BY o.id`, event)
}

func listStandingOrders(d *sql.DB, query string, args ...any) ([]*StandingOrder, error) {
	rows, err := d.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*StandingOrder
	for rows.Next() {
		o, err := scanStandingOrder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// UpdateStandingOrderText replaces the delivered text and bumps the revision,
// re-arming any once-per-generation cadence. Editing what an agent will be
// told must not leave recipients pinned to an "already delivered" record for
// the old wording — that would silently withhold the new text from exactly the
// agents the edit was for.
func UpdateStandingOrderText(id int64, summary string) error {
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return fmt.Errorf("%w: summary is required", ErrStandingOrderInvalid)
	}
	if len(summary) > StandingSummaryMaxLen {
		return fmt.Errorf("%w: summary is %d bytes, limit is %d",
			ErrStandingOrderInvalid, len(summary), StandingSummaryMaxLen)
	}
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`UPDATE agent_standing_orders
		SET summary = ?, revision = revision + 1, updated_at = ? WHERE id = ?`,
		summary, formatStandingTime(time.Now()), id)
	return err
}

// UpdateStandingOrder replaces every operator-editable field and bumps the
// revision atomically. expectedRevision is an optimistic-concurrency guard:
// the edit is rejected if another writer changed the order after the dialog
// opened.
func UpdateStandingOrder(id, expectedRevision int64, o *StandingOrder) error {
	if expectedRevision <= 0 {
		return fmt.Errorf("%w: expected revision is required", ErrStandingOrderInvalid)
	}
	if err := o.Validate(); err != nil {
		return err
	}
	d, err := Open()
	if err != nil {
		return err
	}
	res, err := d.Exec(`UPDATE agent_standing_orders SET
		name = ?, revision = revision + 1,
		owner_agent = ?, target_kind = ?, target_agent = ?, group_id = ?, target_role = ?,
		summary = ?, trigger_event = ?, trigger_sources = ?, timing = ?, cadence = ?,
		enabled = ?, disabled_reason = ?, operator_authored = ?, updated_at = ?
		WHERE id = ? AND revision = ?`,
		o.Name, o.OwnerAgent, o.TargetKind, o.TargetAgent, o.GroupID, o.TargetRole,
		o.Summary, o.TriggerEvent, strings.Join(o.TriggerSources, ","), o.Timing, o.Cadence,
		boolToInt(o.Enabled), o.DisabledReason, boolToInt(o.OperatorAuthored), formatStandingTime(time.Now()),
		id, expectedRevision)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("UpdateStandingOrder %q: %w", o.Name, ErrStandingOrderNameTaken)
		}
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrStandingOrderRevisionConflict
	}
	return nil
}

// SetStandingOrderEnabled toggles an order, clears any disabled_reason, and
// bumps its revision when the state changes. The bump both invalidates a
// concurrently open editor and re-arms delivery when an operator explicitly
// enables an order again.
func SetStandingOrderEnabled(id int64, enabled bool) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`UPDATE agent_standing_orders
		SET enabled = ?, disabled_reason = '', revision = revision + 1, updated_at = ?
		WHERE id = ? AND (enabled != ? OR disabled_reason != '')`,
		boolToInt(enabled), formatStandingTime(time.Now()), id, boolToInt(enabled))
	return err
}

// DeleteStandingOrder removes an order and its ledger rows.
func DeleteStandingOrder(id int64) error {
	d, err := Open()
	if err != nil {
		return err
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM agent_standing_order_deliveries WHERE order_id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM agent_standing_orders WHERE id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteStandingOrderRevision removes an order and its ledger only if the
// caller still holds the current revision. Dashboard editors use this so a
// stale tab cannot delete an order that another operator has since rewritten.
func DeleteStandingOrderRevision(id, expectedRevision int64) error {
	if expectedRevision <= 0 {
		return fmt.Errorf("%w: expected revision is required", ErrStandingOrderInvalid)
	}
	d, err := Open()
	if err != nil {
		return err
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec(`DELETE FROM agent_standing_orders WHERE id = ? AND revision = ?`,
		id, expectedRevision)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrStandingOrderRevisionConflict
	}
	if _, err := tx.Exec(`DELETE FROM agent_standing_order_deliveries WHERE order_id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// DisableGroupTargetStandingOrdersForRetire mirrors
// DisableGroupTargetCronJobsForRetire: it pauses every currently-ENABLED
// group-target order aimed at groupID when a retire leaves the group with no
// live members, stamping the marker so a later resume re-enables exactly these
// and not the ones the human disabled by hand.
func DisableGroupTargetStandingOrdersForRetire(groupID int64) (int, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	res, err := d.Exec(
		`UPDATE agent_standing_orders SET enabled = 0, disabled_reason = ?
		 WHERE target_kind = ? AND group_id = ? AND enabled = 1`,
		StandingDisabledReasonGroupRetired, StandingTargetGroup, groupID)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// ReenableGroupRetiredStandingOrders mirrors ReenableGroupRetiredCronJobs:
// only orders carrying the group-retired marker are restored, so an order the
// human paused stays paused.
func ReenableGroupRetiredStandingOrders(groupID int64) (int, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	res, err := d.Exec(
		`UPDATE agent_standing_orders SET enabled = 1, disabled_reason = ''
		 WHERE target_kind = ? AND group_id = ? AND disabled_reason = ?
		 AND (owner_agent = '' OR EXISTS (
			SELECT 1 FROM agents WHERE agent_id = owner_agent AND retired_at = ''
		 ))`,
		StandingTargetGroup, groupID, StandingDisabledReasonGroupRetired)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// RecordStandingDelivery appends one evaluation outcome to the ledger.
//
// Not every evaluation is recorded — see the caller. Out-of-scope and
// no-match outcomes are the overwhelming majority and carry no information an
// operator would come looking for; recording them would bury the ones that do.
func RecordStandingDelivery(rec *StandingDelivery) (int64, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	res, err := d.Exec(`INSERT INTO agent_standing_order_deliveries
		(order_id, order_revision, target_conv, epoch, outcome, transport, harness, detail, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.OrderID, rec.OrderRevision, rec.TargetConv, rec.Epoch,
		rec.Outcome, rec.Transport, rec.Harness, rec.Detail,
		formatStandingTime(time.Now()))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// StandingOrderDeliveredInEpoch reports whether a delivery has already been
// recorded for (order, revision, conv, epoch). It is the cadence check for
// StandingCadenceOncePerGeneration.
//
// It matches the outcomes that actually put text in front of the agent —
// StandingOutcomeDelivered and StandingOutcomeDegradedTransport (a weaker
// transport still delivered). An evaluation that ended in unsupported-timing,
// an unimplemented transport, or a trimmed payload delivered nothing, so it
// must not suppress the next attempt.
func StandingOrderDeliveredInEpoch(orderID, revision int64, targetConv, epoch string) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM agent_standing_order_deliveries
		WHERE order_id = ? AND order_revision = ? AND target_conv = ? AND epoch = ?
		AND outcome IN (?, ?)`,
		orderID, revision, targetConv, epoch,
		StandingOutcomeDelivered, StandingOutcomeDegradedTransport).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// ListStandingDeliveries returns the most recent ledger rows for an order,
// newest first.
func ListStandingDeliveries(orderID int64, limit int) ([]*StandingDelivery, error) {
	if limit <= 0 {
		limit = 20
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT id, order_id, order_revision, target_conv, epoch,
		outcome, transport, harness, detail, created_at
		FROM agent_standing_order_deliveries
		WHERE order_id = ? ORDER BY id DESC LIMIT ?`, orderID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*StandingDelivery
	for rows.Next() {
		var (
			rec        StandingDelivery
			createdRaw string
		)
		if err := rows.Scan(&rec.ID, &rec.OrderID, &rec.OrderRevision, &rec.TargetConv,
			&rec.Epoch, &rec.Outcome, &rec.Transport, &rec.Harness, &rec.Detail,
			&createdRaw); err != nil {
			return nil, err
		}
		rec.CreatedAt = parseStandingTime(createdRaw)
		out = append(out, &rec)
	}
	return out, rows.Err()
}

// LatestStandingDelivery returns the most recent ledger row for an order, or
// nil when it has never been evaluated in a way worth recording.
func LatestStandingDelivery(orderID int64) (*StandingDelivery, error) {
	recs, err := ListStandingDeliveries(orderID, 1)
	if err != nil || len(recs) == 0 {
		return nil, err
	}
	return recs[0], nil
}
