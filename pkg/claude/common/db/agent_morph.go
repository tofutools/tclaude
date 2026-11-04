package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// AgentMorphForm is the launch form a morph rewrites. It is a slice of
// AgentRelaunchProfile, not a parallel policy: applying a form writes these
// fields into the durable relaunch profile, and the ordinary resume path
// replays them. nil = leave the field unchanged.
type AgentMorphForm struct {
	// Harness is set only on a cross-harness form: the harness the form is
	// for. A cross-harness morph translates the conversation into a new one
	// of that harness instead of resuming the same one, and ApplyTo never
	// applies a form for another harness. "" = the agent's own harness.
	Harness string `json:"harness,omitempty"`
	// HistoryWindowTokens overrides only the cross-harness history budget, not
	// the destination model or its native context capacity. Persisted for idle apply.
	HistoryWindowTokens *int64 `json:"history_window_tokens,omitempty"`
	// HarnessBuiltinMode is the harness-native sandbox mode a cross-harness
	// form launches under. It is recorded for `morph --back` across
	// harnesses; a same-harness morph never changes sandbox fields.
	HarnessBuiltinMode *string `json:"sandbox_mode,omitempty"`
	// TrustDir pre-trusts the working directory in the destination harness
	// before a cross-harness morph launches it, as spawn's trust_dir does, so
	// the pane does not stop on that harness's folder-trust dialog.
	TrustDir bool `json:"trust_dir,omitempty"`
	// Model is the bare model id (Claude's [1m] suffix lives in
	// ContextWindowSize, exactly as relaunchProfileForSpawn records it).
	Model             *string `json:"model,omitempty"`
	ContextWindowSize *int64  `json:"context_window_size,omitempty"`
	Effort            *string `json:"effort,omitempty"`
	Approval          *string `json:"approval,omitempty"`
	Tools             *string `json:"tools,omitempty"`
	AskTimeout        *string `json:"ask_timeout,omitempty"`
	AutoCompactWindow *string `json:"auto_compact_window,omitempty"`
	// Profile is a display label: the spawn profile the form came from.
	Profile string `json:"profile,omitempty"`
}

// AgentPendingMorph is a morph waiting for its target to go idle. It lives in
// the relaunch-profile JSON so it survives daemon restarts. ConvID pins the
// generation it was requested against; a reincarnation or move drops it.
type AgentPendingMorph struct {
	ConfirmHistoryLoss    bool           `json:"confirm_history_loss,omitempty"`
	ProceedWithoutSummary bool           `json:"proceed_without_summary,omitempty"`
	Target                AgentMorphForm `json:"target"`
	Back                  bool           `json:"back,omitempty"`
	ActorConv             string         `json:"actor_conv,omitempty"`
	Actor                 string         `json:"actor"`
	ConvID                string         `json:"conv_id"`
	RequestedAt           time.Time      `json:"requested_at"`
	ExpiresAt             time.Time      `json:"expires_at"`
	// History is the requested delivery rung of a cross-harness morph: ""
	// (native, falling back to a context file), "context_file", or
	// "handoff" (the agent writes a handoff for its successor first), or
	// "summary" (a confirmed large cut waits for a source-written summary).
	History string `json:"history,omitempty"`
	// Handoff is the source-written handoff or summary a pending morph waits for.
	Handoff *AgentMorphHandoff `json:"handoff,omitempty"`
	// HandoffRequired marks a handoff morph chosen because the history
	// cannot be read or a large cut requires a summary: without that response it is
	// dropped at expiry instead of falling back to the context-file rung.
	HandoffRequired bool   `json:"handoff_required,omitempty"`
	HandoffReason   string `json:"handoff_reason,omitempty"`
}

// AgentMorphHandoff is the handoff an agent submits for the agent that
// continues its work after a cross-harness morph.
type AgentMorphHandoff struct {
	Body        string    `json:"body,omitempty"`
	SubmittedAt time.Time `json:"submitted_at,omitempty"`
}

// ErrNoHandoffRequested is SubmitAgentMorphHandoff's refusal when the agent
// has no pending morph waiting for a handoff.
var ErrNoHandoffRequested = errors.New("no pending morph is waiting for a handoff from this agent")

// SubmitAgentMorphHandoff records the agent's handoff on its pending morph.
// A later submission replaces an earlier one until the morph applies.
func SubmitAgentMorphHandoff(convID, body string) error {
	return updateAgentRelaunchProfileForConv(convID, func(_ string, p *AgentRelaunchProfile) error {
		if p.PendingMorph == nil || (p.PendingMorph.History != "handoff" && p.PendingMorph.History != "summary") {
			return ErrNoHandoffRequested
		}
		p.PendingMorph.Handoff = &AgentMorphHandoff{Body: body, SubmittedAt: time.Now().UTC()}
		return nil
	})
}

// morphFormOf captures the current values of the morphable fields. Nil string
// fields become "" (the "no override" value every reader already treats nil
// as), so restoring the captured form is exact. Approval stays nil when it is
// nil: an unknown approval posture must not turn into an explicit default.
func morphFormOf(p *AgentRelaunchProfile, profile string) AgentMorphForm {
	str := func(v *string) *string {
		if v == nil {
			return stringPtr("")
		}
		return stringPtr(*v)
	}
	var window int64
	if p.ContextWindowSize != nil {
		window = *p.ContextWindowSize
	}
	f := AgentMorphForm{
		Model:             str(p.ModelID),
		ContextWindowSize: int64Ptr(window),
		Effort:            str(p.Effort),
		Tools:             str(p.ToolGovernance),
		AskTimeout:        str(p.AskUserQuestionTimeout),
		AutoCompactWindow: str(p.AutoCompactWindow),
		Profile:           profile,
	}
	if p.ApprovalPolicy != nil {
		f.Approval = stringPtr(*p.ApprovalPolicy)
	}
	return f
}

// MorphFormOf captures p's morphable fields as a form, labelled profile.
func MorphFormOf(p *AgentRelaunchProfile, profile string) AgentMorphForm {
	return morphFormOf(p, profile)
}

// ApplyTo writes the form's non-nil fields into p.
func (f AgentMorphForm) ApplyTo(p *AgentRelaunchProfile) { applyMorphForm(p, f) }

func applyMorphForm(p *AgentRelaunchProfile, f AgentMorphForm) {
	if f.Model != nil {
		p.ModelID = stringPtr(*f.Model)
	}
	if f.ContextWindowSize != nil {
		p.ContextWindowSize = int64Ptr(*f.ContextWindowSize)
	}
	if f.Effort != nil {
		p.Effort = stringPtr(*f.Effort)
	}
	if f.Approval != nil {
		p.ApprovalPolicy = stringPtr(*f.Approval)
	}
	if f.Tools != nil {
		p.ToolGovernance = stringPtr(*f.Tools)
	}
	if f.AskTimeout != nil {
		p.AskUserQuestionTimeout = stringPtr(*f.AskTimeout)
	}
	if f.AutoCompactWindow != nil {
		p.AutoCompactWindow = stringPtr(*f.AutoCompactWindow)
	}
}

// morphExplicitValues is the explicit layer entries for the fields a morph
// form changes from the form it replaces, keyed by launchspec field key and
// spawn request field name (a test pins both against launchspec). A field
// the form sets to the value it already had is not the morph's choice. (A
// morph back normally undoes instead, see applyMorphExplicitTx; these are
// what it records when there is nothing to undo.) The model is recorded
// bare: its context window lives in the relaunch profile, as the form keeps
// it.
//
// A cross-harness form also names its harness and built-in sandbox mode; a
// same-harness form leaves both unset.
//
// The auto-compact window shares launchspec's context_window key with the
// Copilot-only ContextWindowMax, which the resolver treats as one unit. A
// morph cannot set both on one agent: the auto-compact window is Claude
// Code's and pi's only (harness.SupportsAutoCompactWindow), and a morph to
// Copilot resets the cap with the rest of the drive.
func morphExplicitValues(f, previous AgentMorphForm, at time.Time) map[string]ExplicitLaunchValue {
	out := map[string]ExplicitLaunchValue{}
	var harnessName, previousHarness *string
	if f.Harness != "" {
		harnessName, previousHarness = &f.Harness, &previous.Harness
	}
	for _, v := range []struct {
		key, field      string
		value, previous *string
	}{
		{"harness", "Harness", harnessName, previousHarness},
		{"sandbox_mode", "HarnessBuiltinMode", f.HarnessBuiltinMode, previous.HarnessBuiltinMode},
		{"model", "Model", f.Model, previous.Model},
		{"effort", "Effort", f.Effort, previous.Effort},
		{"approval", "ApprovalPolicy", f.Approval, previous.Approval},
		{"tools", "ToolGovernance", f.Tools, previous.Tools},
		{"ask_timeout", "AskUserQuestionTimeout", f.AskTimeout, previous.AskTimeout},
		{"context_window", "AutoCompactWindow", f.AutoCompactWindow, previous.AutoCompactWindow},
	} {
		if v.value == nil || (v.previous != nil && *v.previous == *v.value) {
			continue
		}
		raw, err := json.Marshal(*v.value)
		if err != nil {
			continue
		}
		out[v.key] = ExplicitLaunchValue{
			Fields:     map[string]json.RawMessage{v.field: raw},
			Provenance: ExplicitAtMorph(at),
			SetAt:      at,
		}
	}
	return out
}

// CurrentMorphForm returns the agent's current morphable launch form plus the
// previous form recorded by its last morph (nil when it was never morphed).
func CurrentMorphForm(convID string) (current AgentMorphForm, previous *AgentMorphForm, err error) {
	p, err := AgentRelaunchProfileForConv(convID)
	if err != nil {
		return AgentMorphForm{}, nil, err
	}
	if p == nil {
		return AgentMorphForm{}, nil, fmt.Errorf("conversation %s has no durable agent launch profile", convID)
	}
	return morphFormOf(effectiveMorphBase(convID, p), p.MorphProfile), p.PreviousMorphForm, nil
}

// effectiveMorphBase composes the conversation fallback under the agent's
// profile, so a field the agent profile leaves nil is captured as the value
// resume would actually use rather than as unknown.
func effectiveMorphBase(convID string, p *AgentRelaunchProfile) *AgentRelaunchProfile {
	conversation, err := ConversationResumeProfileForConv(convID)
	if err != nil || conversation == nil {
		return p
	}
	return ComposeAgentRelaunchProfile(conversation.FallbackRelaunch, p)
}

// updateAgentRelaunchProfileForConvTx runs a read/modify/write of the current
// generation's profile in one transaction so unrelated launch intent is kept.
func updateAgentRelaunchProfileForConv(convID string, apply func(agentID string, p *AgentRelaunchProfile) error) error {
	return updateAgentRelaunchProfileForConvTx(convID, func(_ *sql.Tx, agentID string, p *AgentRelaunchProfile) error {
		return apply(agentID, p)
	})
}

// updateAgentRelaunchProfileForConvTx is updateAgentRelaunchProfileForConv
// with the transaction passed to apply, for a change that writes another of
// the agent's columns with the profile.
func updateAgentRelaunchProfileForConvTx(convID string, apply func(tx *sql.Tx, agentID string, p *AgentRelaunchProfile) error) error {
	convID = strings.TrimSpace(convID)
	if convID == "" {
		return errors.New("conversation required")
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
	var agentID, raw string
	err = tx.QueryRow(`SELECT a.agent_id, a.relaunch_profile
		FROM agent_conversations ac
		JOIN agents a ON a.agent_id = ac.agent_id
		WHERE ac.conv_id = ?`, convID).Scan(&agentID, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("conversation %s is not an agent", convID)
	}
	if err != nil {
		return err
	}
	profile, err := decodeAgentRelaunchProfile(raw)
	if err != nil {
		return err
	}
	if profile == nil {
		return fmt.Errorf("agent %s has no durable launch profile", agentID)
	}
	if err := apply(tx, agentID, profile); err != nil {
		return err
	}
	encoded, err := encodeRelaunchProfile(*profile)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE agents SET relaunch_profile = ? WHERE agent_id = ?`, encoded, agentID); err != nil {
		return err
	}
	return tx.Commit()
}

// ApplyAgentMorphForConv writes target into the durable relaunch profile,
// records the form it replaced for a later morph-back, and clears any pending
// morph. It returns the replaced form.
//
// The fields the form changes are also recorded as the agent's explicit
// launch values, in the same transaction, so re-applying its spawn profile
// keeps the morphed form instead of putting the profile's values back. A
// morph back (back) undoes what the morph it reverses recorded.
func ApplyAgentMorphForConv(convID string, target AgentMorphForm, back bool) (AgentMorphForm, error) {
	var previous AgentMorphForm
	conversation, err := ConversationResumeProfileForConv(convID)
	if err != nil {
		return previous, err
	}
	err = updateAgentRelaunchProfileForConvTx(convID, func(tx *sql.Tx, agentID string, p *AgentRelaunchProfile) error {
		base := p
		if conversation != nil {
			base = ComposeAgentRelaunchProfile(conversation.FallbackRelaunch, p)
		}
		previous = morphFormOf(base, p.MorphProfile)
		if err := applyMorphExplicitTx(tx, agentID, morphExplicitValues(target, previous, time.Now()), back); err != nil {
			return err
		}
		applyMorphForm(p, target)
		p.MorphProfile = target.Profile
		p.PreviousMorphForm = &previous
		p.PendingMorph = nil
		return nil
	})
	return previous, err
}

// SetAgentPendingMorphForConv stores (or, with nil, clears) the pending morph.
func SetAgentPendingMorphForConv(convID string, pending *AgentPendingMorph) error {
	return updateAgentRelaunchProfileForConv(convID, func(_ string, p *AgentRelaunchProfile) error {
		p.PendingMorph = pending
		return nil
	})
}

// PendingMorphForAgent returns the agent's pending morph, or nil.
func PendingMorphForAgent(agentID string) (*AgentPendingMorph, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	var raw string
	err = d.QueryRow(`SELECT relaunch_profile FROM agents WHERE agent_id = ?`, strings.TrimSpace(agentID)).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p, err := decodeAgentRelaunchProfile(raw)
	if err != nil || p == nil {
		return nil, err
	}
	return p.PendingMorph, nil
}

// AgentWithPendingMorph is one row of ListAgentsWithPendingMorph.
type AgentWithPendingMorph struct {
	AgentID       string
	CurrentConvID string
	Pending       AgentPendingMorph
}

// ListAgentsWithPendingMorph returns every active agent with a pending morph.
// The LIKE prefilter keeps the idle watcher from decoding every profile.
func ListAgentsWithPendingMorph() ([]AgentWithPendingMorph, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT agent_id, current_conv_id, relaunch_profile FROM agents
		WHERE retired_at IS NULL AND relaunch_profile LIKE '%"pending_morph"%'`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []AgentWithPendingMorph
	for rows.Next() {
		var agentID, conv, raw string
		if err := rows.Scan(&agentID, &conv, &raw); err != nil {
			return nil, err
		}
		p, err := decodeAgentRelaunchProfile(raw)
		if err != nil || p == nil || p.PendingMorph == nil {
			continue
		}
		out = append(out, AgentWithPendingMorph{AgentID: agentID, CurrentConvID: conv, Pending: *p.PendingMorph})
	}
	return out, rows.Err()
}

// ApplyCrossHarnessMorph rebinds the agent from oldConv to newConv, a
// translated conversation of another harness, in one transaction: the
// generation rotation (succession edge and live pointer, as reincarnate
// does), the successor's conversation resume profile, and the agent's
// relaunch profile rebuilt for that harness, and the fields the morph changes
// (target over previous, the form it replaces) recorded as explicit launch
// values, or undone for a morph back, as for a same-harness morph. Nothing is
// committed on error.
func ApplyCrossHarnessMorph(oldConv, newConv string, conversation ConversationResumeProfile, profile AgentRelaunchProfile, target, previous AgentMorphForm, back bool) error {
	oldConv, newConv = strings.TrimSpace(oldConv), strings.TrimSpace(newConv)
	if oldConv == "" || newConv == "" || oldConv == newConv {
		return errors.New("ApplyCrossHarnessMorph: two distinct conversations required")
	}
	if conversation.Version != RelaunchProfileVersion || profile.Version != RelaunchProfileVersion {
		return errors.New("ApplyCrossHarnessMorph: unsupported profile version")
	}
	agentID, err := AgentIDForConv(oldConv)
	if err != nil {
		return err
	}
	if agentID == "" {
		return fmt.Errorf("conversation %s is not an agent", oldConv)
	}
	carriedName := resolveCarriedName(oldConv)
	rawConversation, err := encodeRelaunchProfile(conversation)
	if err != nil {
		return err
	}
	rawProfile, err := encodeRelaunchProfile(profile)
	if err != nil {
		return err
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
	if err := rotateAgentConvTx(tx, oldConv, newConv, "morph", carriedName); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO conversation_resume_profiles (conv_id, profile_json, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT(conv_id) DO UPDATE SET profile_json = excluded.profile_json, updated_at = excluded.updated_at`,
		newConv, rawConversation, dbTime(time.Now().UTC())); err != nil {
		return err
	}
	res, err := tx.Exec(`UPDATE agents SET relaunch_profile = ? WHERE agent_id = ? AND current_conv_id = ?`,
		rawProfile, agentID, newConv)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("agent %s did not advance to %s", agentID, newConv)
	}
	if err := applyMorphExplicitTx(tx, agentID, morphExplicitValues(target, previous, time.Now()), back); err != nil {
		return err
	}
	return tx.Commit()
}
