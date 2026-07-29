package db

import (
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"slices"
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

// Trigger events are normalized across harnesses before evaluation.
const (
	// StandingTriggerSessionStart matches the harness SessionStart lifecycle
	// event. Its sources are the harness's own `source` values, normalized by
	// NormalizeTriggerSources.
	StandingTriggerSessionStart = "session.start"
	// StandingTriggerUserPrompt matches a submitted user prompt.
	StandingTriggerUserPrompt = "user.prompt"
	// StandingTriggerToolBefore and StandingTriggerToolAfter match tool
	// lifecycle boundaries before and after execution.
	StandingTriggerToolBefore = "tool.before"
	StandingTriggerToolAfter  = "tool.after"
)

// Match fields expose a deliberately small normalized projection of hook
// payloads. One order matches at most one field; more complex OR behavior is
// represented by separate orders so each branch has its own ledger history.
const (
	StandingMatchFieldCwd       = "cwd"
	StandingMatchFieldPrompt    = "prompt"
	StandingMatchFieldToolName  = "tool_name"
	StandingMatchFieldToolInput = "tool_input"
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
	// StandingOutcomeSuppressedCooldown — matched, but this stable recipient
	// received the same order revision too recently.
	StandingOutcomeSuppressedCooldown = "suppressed-cooldown"
	// StandingOutcomeDeferredDebounce — matched and durably scheduled for
	// trailing-edge message delivery. It is normally represented by the
	// pending table rather than one ledger row per high-frequency event.
	StandingOutcomeDeferredDebounce = "deferred-debounce"
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
	// delivery path held the applicable conversation-cadence or stable-agent
	// cooldown lock, so the rate-control read-modify-write could not be
	// performed safely and the order was NOT delivered on this boundary.
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

// StandingMatchRegexMaxLen bounds authoring mistakes. Go's regexp engine is
// RE2 and therefore linear-time, but an unbounded expression would still add
// needless compile and display cost on every matching hook.
const StandingMatchRegexMaxLen = 1024

// StandingCooldownMaxSeconds keeps an accidental dashboard value from
// suppressing an order effectively forever. Zero disables cooldown.
const StandingCooldownMaxSeconds int64 = 365 * 24 * 60 * 60

// StandingDebounceMaxSeconds bounds one authored quiet window. The scheduler
// also forces a continuously retriggered order out after a bounded maximum.
const StandingDebounceMaxSeconds int64 = 24 * 60 * 60

// ErrStandingOrderNameTaken is the canonical rejection for a duplicate order
// name. Names are the stable handle the CLI and the skill address orders by,
// so they are unique across the store.
var ErrStandingOrderNameTaken = errors.New("standing order name already exists")

// ErrStandingOrderInvalid classifies a write rejected by Validate.
var ErrStandingOrderInvalid = errors.New("invalid standing order")

// ErrStandingOrderVersionConflict reports an edit based on a stale row
// version. Dashboard dialogs carry the version they opened so two operator
// tabs cannot silently overwrite one another.
var ErrStandingOrderVersionConflict = errors.New("standing order version conflict")

// ErrStandingOrderRevisionConflict is the historical name retained for callers
// compiled against the prototype API. Revision now means delivery revision;
// row version is the compare-and-swap token.
var ErrStandingOrderRevisionConflict = ErrStandingOrderVersionConflict

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
	// Revision bumps whenever delivered guidance or its matching/cadence
	// changes. It is the delivery revision recorded in the ledger and used by
	// cadence/cooldown suppression.
	Revision int64
	// RowVersion bumps on every persisted mutation and is the sole optimistic
	// concurrency token. Administrative edits do not re-arm delivery.
	RowVersion int64

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
	// MatchField and MatchRegex are either both empty (match every event of
	// this type) or select one normalized event field and a validated RE2
	// expression. Regexes are case-sensitive unless authored with (?i).
	MatchField string
	MatchRegex string

	Timing  string
	Cadence string
	// CooldownSeconds limits successful deliveries per stable recipient agent.
	// Its history is delivery-revision-scoped. Model-visible, matching, timing,
	// and cadence edits plus manual re-enable deliberately re-arm it; changing
	// the cooldown itself does not, so the new duration applies to the last
	// successful delivery instead of granting an immediate extra delivery.
	CooldownSeconds int64
	// DebounceSeconds enables trailing-edge coalescing. Debounced delivery is
	// necessarily next-turn/message transport: an inline hook response cannot
	// wait to learn whether a later event arrives.
	DebounceSeconds int64

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
	label := o.TriggerEvent
	if o.TriggerEvent == StandingTriggerSessionStart {
		if len(o.TriggerSources) == 0 {
			label += " (any source)"
		} else {
			label += " (" + strings.Join(o.TriggerSources, ", ") + ")"
		}
	}
	if o.MatchField != "" {
		label += " where " + o.MatchField + " matches /" + o.MatchRegex + "/"
	}
	return label
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
	// TargetAgent is the durable recipient key. TargetConv remains useful
	// diagnostic history and a once-per-generation epoch, but must not define
	// a cooldown that should survive conversation rotation.
	TargetAgent string
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

	switch o.TriggerEvent {
	case StandingTriggerSessionStart, StandingTriggerUserPrompt,
		StandingTriggerToolBefore, StandingTriggerToolAfter:
	default:
		return fmt.Errorf("%w: unknown trigger event %q",
			ErrStandingOrderInvalid, o.TriggerEvent)
	}
	o.TriggerSources = NormalizeTriggerSources(o.TriggerSources)
	if o.TriggerEvent != StandingTriggerSessionStart && len(o.TriggerSources) > 0 {
		return fmt.Errorf("%w: trigger sources apply only to %q",
			ErrStandingOrderInvalid, StandingTriggerSessionStart)
	}
	for _, s := range o.TriggerSources {
		if !ValidStandingSource(s) {
			return fmt.Errorf("%w: unknown trigger source %q", ErrStandingOrderInvalid, s)
		}
	}
	o.MatchField = strings.ToLower(strings.TrimSpace(o.MatchField))
	if err := ValidateStandingMatcher(o.TriggerEvent, o.MatchField, o.MatchRegex); err != nil {
		return err
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
	if o.CooldownSeconds < 0 || o.CooldownSeconds > StandingCooldownMaxSeconds {
		return fmt.Errorf("%w: cooldown_seconds must be between 0 and %d",
			ErrStandingOrderInvalid, StandingCooldownMaxSeconds)
	}
	if o.DebounceSeconds < 0 || o.DebounceSeconds > StandingDebounceMaxSeconds {
		return fmt.Errorf("%w: debounce_seconds must be between 0 and %d",
			ErrStandingOrderInvalid, StandingDebounceMaxSeconds)
	}
	if o.DebounceSeconds > 0 && o.Timing != StandingTimingNextTurn {
		return fmt.Errorf("%w: debounce requires next-turn timing",
			ErrStandingOrderInvalid)
	}
	return nil
}

// ValidateStandingMatcher checks the matcher independently of the rest of an
// order. Writes call it through Validate, and the evaluator calls it again on
// rows read from storage so a directly edited or corrupted row fails closed.
func ValidateStandingMatcher(event, field, expression string) error {
	if (field == "") != (expression == "") {
		return fmt.Errorf("%w: match_field and match_regex must be set together",
			ErrStandingOrderInvalid)
	}
	if field == "" {
		return nil
	}
	if !validStandingMatchField(event, field) {
		return fmt.Errorf("%w: field %q cannot be matched on trigger %q",
			ErrStandingOrderInvalid, field, event)
	}
	if len(expression) > StandingMatchRegexMaxLen {
		return fmt.Errorf("%w: match_regex is %d bytes, limit is %d",
			ErrStandingOrderInvalid, len(expression), StandingMatchRegexMaxLen)
	}
	if _, err := regexp.Compile(expression); err != nil {
		return fmt.Errorf("%w: invalid RE2 match_regex: %v",
			ErrStandingOrderInvalid, err)
	}
	return nil
}

func validStandingMatchField(event, field string) bool {
	switch event {
	case StandingTriggerSessionStart:
		return field == StandingMatchFieldCwd
	case StandingTriggerUserPrompt:
		return field == StandingMatchFieldPrompt || field == StandingMatchFieldCwd
	case StandingTriggerToolBefore, StandingTriggerToolAfter:
		return field == StandingMatchFieldToolName ||
			field == StandingMatchFieldToolInput ||
			field == StandingMatchFieldCwd
	}
	return false
}

// standingSelect is the shared SELECT for reading standing orders. As with
// cronSelect, owner/target are keyed on agent_id and each LEFT JOIN resolves
// the actor back to its CURRENT conv, so a reincarnation or /clear does not
// strand an order pointed at a dead generation. LEFT JOIN + COALESCE so a
// group-target order (target_agent "") or an owner-less operator order keeps
// an empty string rather than dropping the row.
const standingSelect = `SELECT o.id, o.name, o.revision, o.row_version,
	o.owner_agent, COALESCE(ow.current_conv_id, ''),
	o.target_kind, o.target_agent, COALESCE(tg.current_conv_id, ''),
	o.group_id, o.target_role, o.summary,
	o.trigger_event, o.trigger_sources, o.match_field, o.match_regex,
	o.timing, o.cadence, o.cooldown_seconds, o.debounce_seconds,
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
		&o.ID, &o.Name, &o.Revision, &o.RowVersion,
		&o.OwnerAgent, &o.OwnerConv,
		&o.TargetKind, &o.TargetAgent, &o.TargetConv,
		&o.GroupID, &o.TargetRole, &o.Summary,
		&o.TriggerEvent, &sources, &o.MatchField, &o.MatchRegex,
		&o.Timing, &o.Cadence, &o.CooldownSeconds, &o.DebounceSeconds,
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
// server-side; the caller's values are ignored. Both versions start at 1.
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
		(name, revision, row_version, owner_agent, target_kind, target_agent, group_id, target_role,
		 summary, trigger_event, trigger_sources, timing, cadence, cooldown_seconds, debounce_seconds,
		 match_field, match_regex,
		 enabled, disabled_reason, operator_authored, created_at, updated_at)
		VALUES (?, 1, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		o.Name, o.OwnerAgent, o.TargetKind, o.TargetAgent, o.GroupID, o.TargetRole,
		o.Summary, o.TriggerEvent, strings.Join(o.TriggerSources, ","), o.Timing, o.Cadence,
		o.CooldownSeconds, o.DebounceSeconds, o.MatchField, o.MatchRegex,
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

// UpdateStandingOrder replaces every operator-editable field. RowVersion
// always advances; the delivery revision advances only when model-visible
// guidance, matching, timing, or cadence changes, or when this edit explicitly
// re-enables the order.
func UpdateStandingOrder(
	id, expectedRowVersion int64, o *StandingOrder,
) error {
	if expectedRowVersion <= 0 {
		return fmt.Errorf("%w: expected row version is required", ErrStandingOrderInvalid)
	}
	if err := o.Validate(); err != nil {
		return err
	}
	current, err := GetStandingOrder(id)
	if err != nil {
		return err
	}
	if current == nil || current.RowVersion != expectedRowVersion {
		return ErrStandingOrderVersionConflict
	}
	rearm := standingOrderDeliveryChanged(current, o) ||
		(o.Enabled && o.DisabledReason == "" &&
			(!current.Enabled || current.DisabledReason != ""))
	d, err := Open()
	if err != nil {
		return err
	}
	res, err := d.Exec(`UPDATE agent_standing_orders SET
		name = ?, revision = revision + ?, row_version = row_version + 1,
		owner_agent = ?, target_kind = ?, target_agent = ?, group_id = ?, target_role = ?,
		summary = ?, trigger_event = ?, trigger_sources = ?, timing = ?, cadence = ?,
		cooldown_seconds = ?, debounce_seconds = ?, match_field = ?, match_regex = ?,
		enabled = ?, disabled_reason = ?, operator_authored = ?, updated_at = ?
		WHERE id = ? AND row_version = ?`,
		o.Name, boolToInt(rearm), o.OwnerAgent, o.TargetKind, o.TargetAgent, o.GroupID, o.TargetRole,
		o.Summary, o.TriggerEvent, strings.Join(o.TriggerSources, ","), o.Timing, o.Cadence,
		o.CooldownSeconds, o.DebounceSeconds, o.MatchField, o.MatchRegex,
		boolToInt(o.Enabled), o.DisabledReason, boolToInt(o.OperatorAuthored), formatStandingTime(time.Now()),
		id, expectedRowVersion)
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
		return ErrStandingOrderVersionConflict
	}
	return nil
}

func standingOrderDeliveryChanged(a, b *StandingOrder) bool {
	return a.Summary != b.Summary ||
		a.TriggerEvent != b.TriggerEvent ||
		!slices.Equal(a.TriggerSources, b.TriggerSources) ||
		a.MatchField != b.MatchField ||
		a.MatchRegex != b.MatchRegex ||
		a.Timing != b.Timing ||
		a.Cadence != b.Cadence ||
		a.DebounceSeconds != b.DebounceSeconds
}

// SetStandingOrderEnabled toggles an order through the row-version CAS. A
// manual enable advances both versions to re-arm delivery; disable advances
// only row version. Every successful state change clears disabled_reason.
func SetStandingOrderEnabled(
	id int64, enabled bool, expectedRowVersion int64,
) error {
	if expectedRowVersion <= 0 {
		return fmt.Errorf("%w: expected row version is required", ErrStandingOrderInvalid)
	}
	d, err := Open()
	if err != nil {
		return err
	}
	res, err := d.Exec(`UPDATE agent_standing_orders
		SET enabled = ?, disabled_reason = '',
		    revision = revision + ?, row_version = row_version + 1, updated_at = ?
		WHERE id = ? AND row_version = ?
		AND (enabled != ? OR disabled_reason != '')`,
		boolToInt(enabled), boolToInt(enabled), formatStandingTime(time.Now()), id,
		expectedRowVersion, boolToInt(enabled))
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		current, getErr := GetStandingOrder(id)
		if getErr != nil {
			return getErr
		}
		if current != nil &&
			current.RowVersion == expectedRowVersion &&
			current.Enabled == enabled &&
			current.DisabledReason == "" {
			return nil
		}
		return ErrStandingOrderVersionConflict
	}
	return nil
}

// DeleteStandingOrder removes an order and its ledger only if the caller still
// holds the current row-version CAS token. There is deliberately no unguarded exported
// delete: a stale tab must not remove an order another writer changed.
func DeleteStandingOrder(
	id, expectedRowVersion int64,
) error {
	if expectedRowVersion <= 0 {
		return fmt.Errorf("%w: expected row version is required", ErrStandingOrderInvalid)
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
	res, err := tx.Exec(
		`DELETE FROM agent_standing_orders WHERE id = ? AND row_version = ?`,
		id, expectedRowVersion)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrStandingOrderVersionConflict
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
		`UPDATE agent_standing_orders SET enabled = 0, disabled_reason = ?,
		 row_version = row_version + 1, updated_at = ?
		 WHERE target_kind = ? AND group_id = ? AND enabled = 1`,
		StandingDisabledReasonGroupRetired, formatStandingTime(time.Now()),
		StandingTargetGroup, groupID)
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
		`UPDATE agent_standing_orders SET enabled = 1, disabled_reason = '',
		 row_version = row_version + 1, updated_at = ?
		 WHERE target_kind = ? AND group_id = ? AND disabled_reason = ?
		 AND (owner_agent = '' OR EXISTS (
			SELECT 1 FROM agents WHERE agent_id = owner_agent AND retired_at = ''
		 ))`,
		formatStandingTime(time.Now()), StandingTargetGroup, groupID,
		StandingDisabledReasonGroupRetired)
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
	if rec.Outcome == StandingOutcomeUnsupportedTiming {
		// Capability failures are stable for one order revision, recipient,
		// and harness. High-frequency action hooks can encounter the same
		// unsupported OpenCode order thousands of times; preserve the first
		// explanation without growing an unbounded row-per-hook ledger.
		res, err := d.Exec(`INSERT INTO agent_standing_order_deliveries
			(order_id, order_revision, target_conv, target_agent, epoch, outcome, transport, harness, detail, created_at)
			SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
			WHERE NOT EXISTS (
				SELECT 1 FROM agent_standing_order_deliveries
				WHERE order_id = ? AND order_revision = ? AND target_agent = ?
				AND harness = ? AND outcome = ? AND transport = ? AND detail = ?
			)`,
			rec.OrderID, rec.OrderRevision, rec.TargetConv, rec.TargetAgent, rec.Epoch,
			rec.Outcome, rec.Transport, rec.Harness, rec.Detail,
			formatStandingTime(time.Now()),
			rec.OrderID, rec.OrderRevision, rec.TargetAgent, rec.Harness, rec.Outcome,
			rec.Transport, rec.Detail)
		if err != nil {
			return 0, err
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			return 0, err
		}
		return res.LastInsertId()
	}
	res, err := d.Exec(`INSERT INTO agent_standing_order_deliveries
		(order_id, order_revision, target_conv, target_agent, epoch, outcome, transport, harness, detail, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.OrderID, rec.OrderRevision, rec.TargetConv, rec.TargetAgent, rec.Epoch,
		rec.Outcome, rec.Transport, rec.Harness, rec.Detail,
		formatStandingTime(time.Now()))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// LatestSuccessfulStandingDeliveryAt returns when this stable recipient last
// received the current order revision. A missing row returns the zero time.
//
// Pre-v170 rows have no target_agent and are intentionally not backfilled:
// current conversation mappings cannot prove who owned a historical conv at
// delivery time. Enabling cooldown therefore gives an existing order one
// clean delivery before the new stable-agent history takes effect.
func LatestSuccessfulStandingDeliveryAt(orderID, revision int64, targetAgent string) (time.Time, error) {
	targetAgent = strings.TrimSpace(targetAgent)
	if targetAgent == "" {
		return time.Time{}, fmt.Errorf("%w: cooldown lookup requires a stable target agent", ErrStandingOrderInvalid)
	}
	d, err := Open()
	if err != nil {
		return time.Time{}, err
	}
	var raw string
	err = d.QueryRow(`SELECT created_at FROM agent_standing_order_deliveries
		WHERE order_id = ? AND order_revision = ? AND target_agent = ?
		AND outcome IN (?, ?)
		ORDER BY id DESC LIMIT 1`,
		orderID, revision, targetAgent,
		StandingOutcomeDelivered, StandingOutcomeDegradedTransport).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	at := parseStandingTime(raw)
	if at.IsZero() {
		return time.Time{}, fmt.Errorf("invalid standing-order delivery time %q", raw)
	}
	return at, nil
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
	rows, err := d.Query(`SELECT id, order_id, order_revision, target_conv, target_agent, epoch,
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
			&rec.TargetAgent,
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
