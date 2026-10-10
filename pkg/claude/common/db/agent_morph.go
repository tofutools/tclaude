package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// AgentMorphForm is the same-harness launch form a morph rewrites. It is a
// slice of AgentRelaunchProfile, not a parallel policy: applying a form writes
// these fields into the durable relaunch profile, and the ordinary resume path
// replays them. nil = leave the field unchanged.
type AgentMorphForm struct {
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
	Target      AgentMorphForm `json:"target"`
	Back        bool           `json:"back,omitempty"`
	ActorConv   string         `json:"actor_conv,omitempty"`
	Actor       string         `json:"actor"`
	ConvID      string         `json:"conv_id"`
	RequestedAt time.Time      `json:"requested_at"`
	ExpiresAt   time.Time      `json:"expires_at"`
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
	return morphFormOf(p, p.MorphProfile), p.PreviousMorphForm, nil
}

// updateAgentRelaunchProfileForConvTx runs a read/modify/write of the current
// generation's profile in one transaction so unrelated launch intent is kept.
func updateAgentRelaunchProfileForConv(convID string, apply func(agentID string, p *AgentRelaunchProfile) error) error {
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
	if err := apply(agentID, profile); err != nil {
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
func ApplyAgentMorphForConv(convID string, target AgentMorphForm) (AgentMorphForm, error) {
	var previous AgentMorphForm
	err := updateAgentRelaunchProfileForConv(convID, func(_ string, p *AgentRelaunchProfile) error {
		previous = morphFormOf(p, p.MorphProfile)
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
