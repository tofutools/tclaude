package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const RelaunchProfileVersion = 1

// AgentRelaunchProfile is mutable launch intent owned by the stable agent.
// Pointer fields distinguish an observed/selected zero value from unknown
// legacy state. Unknown authority-bearing values are resolved fail-closed by
// the lifecycle layer rather than silently replaced with today's defaults.
type AgentRelaunchProfile struct {
	Version                int     `json:"version"`
	SandboxMode            *string `json:"sandbox_mode,omitempty"`
	ApprovalPolicy         *string `json:"approval_policy,omitempty"`
	ApprovalAutoReview     *bool   `json:"approval_auto_review,omitempty"`
	ModelID                *string `json:"model_id,omitempty"`
	Effort                 *string `json:"effort,omitempty"`
	ContextWindowSize      *int64  `json:"context_window_size,omitempty"`
	AskUserQuestionTimeout *string `json:"ask_user_question_timeout,omitempty"`
	RemoteControl          *bool   `json:"remote_control,omitempty"`
	AutoMemory             *bool   `json:"auto_memory,omitempty"`
}

// ConversationResumeProfile is the durable resume identity intrinsic to one
// harness conversation. It is deliberately independent of agents so ordinary
// non-agent conversations and the standalone conv CLI retain their own
// lifetime and never need an agent row.
type ConversationResumeProfile struct {
	Version          int    `json:"version"`
	Harness          string `json:"harness"`
	Cwd              string `json:"cwd"`
	ResumeProvenance string `json:"resume_provenance,omitempty"`
	// SourceSession* is a projection watermark, not resume authority. It keeps
	// a late hook from an older process generation of the same conversation
	// from rolling durable state backward.
	SourceSessionID        string `json:"source_session_id,omitempty"`
	SourceSessionCreatedAt string `json:"source_session_created_at,omitempty"`
	SourceSessionRowID     int64  `json:"source_session_row_id,omitempty"`
	// FallbackRelaunch preserves the last known launch posture for ordinary,
	// unmanaged conversations. Managed lifecycle always prefers the stable
	// agent's profile; keeping this copy makes the plain conversation/session
	// CLI independent of prunable process rows without turning a conversation
	// snapshot into mutable agent policy.
	FallbackRelaunch *AgentRelaunchProfile `json:"fallback_relaunch,omitempty"`
}

func stringPtr(v string) *string { return &v }
func boolPtr(v bool) *bool       { return &v }
func int64Ptr(v int64) *int64    { return &v }

func encodeRelaunchProfile(v any) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func decodeAgentRelaunchProfile(raw string) (*AgentRelaunchProfile, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var p AgentRelaunchProfile
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return nil, fmt.Errorf("decode agent relaunch profile: %w", err)
	}
	if p.Version != RelaunchProfileVersion {
		return nil, fmt.Errorf("unsupported agent relaunch profile version %d", p.Version)
	}
	return &p, nil
}

func decodeConversationResumeProfile(raw string) (*ConversationResumeProfile, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var p ConversationResumeProfile
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return nil, fmt.Errorf("decode conversation resume profile: %w", err)
	}
	if p.Version != RelaunchProfileVersion {
		return nil, fmt.Errorf("unsupported conversation resume profile version %d", p.Version)
	}
	if p.FallbackRelaunch != nil && p.FallbackRelaunch.Version != RelaunchProfileVersion {
		return nil, fmt.Errorf("unsupported conversation fallback relaunch profile version %d", p.FallbackRelaunch.Version)
	}
	return &p, nil
}

// AgentRelaunchProfileForConv loads the stable actor's durable launch intent.
// nil means the conversation is not an agent or legacy state could not be
// reconstructed; it never falls back to a session row.
func AgentRelaunchProfileForConv(convID string) (*AgentRelaunchProfile, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	var raw string
	err = d.QueryRow(`SELECT a.relaunch_profile
		FROM agent_conversations ac
		JOIN agents a ON a.agent_id = ac.agent_id
		WHERE ac.conv_id = ?`, strings.TrimSpace(convID)).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeAgentRelaunchProfile(raw)
}

// ConversationResumeProfileForConv loads conversation-owned resume facts. It
// never consults sessions and works for conversations without an agent.
func ConversationResumeProfileForConv(convID string) (*ConversationResumeProfile, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	var raw string
	err = d.QueryRow(`SELECT profile_json FROM conversation_resume_profiles WHERE conv_id = ?`,
		strings.TrimSpace(convID)).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeConversationResumeProfile(raw)
}

func SetAgentRelaunchProfile(agentID string, profile AgentRelaunchProfile) error {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return errors.New("SetAgentRelaunchProfile: agent_id required")
	}
	if profile.Version != RelaunchProfileVersion {
		return fmt.Errorf("SetAgentRelaunchProfile: unsupported version %d", profile.Version)
	}
	raw, err := encodeRelaunchProfile(profile)
	if err != nil {
		return err
	}
	d, err := Open()
	if err != nil {
		return err
	}
	res, err := d.Exec(`UPDATE agents SET relaunch_profile = ? WHERE agent_id = ?`, raw, agentID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("SetAgentRelaunchProfile: agent %s does not exist", agentID)
	}
	return nil
}

func SetConversationResumeProfile(convID string, profile ConversationResumeProfile) error {
	convID = strings.TrimSpace(convID)
	if convID == "" {
		return errors.New("SetConversationResumeProfile: conv_id required")
	}
	if profile.Version != RelaunchProfileVersion {
		return fmt.Errorf("SetConversationResumeProfile: unsupported version %d", profile.Version)
	}
	raw, err := encodeRelaunchProfile(profile)
	if err != nil {
		return err
	}
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`INSERT INTO conversation_resume_profiles (conv_id, profile_json, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT(conv_id) DO UPDATE SET profile_json = excluded.profile_json, updated_at = excluded.updated_at`,
		convID, raw, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

// BackfillDurableRelaunchProfilesFromLatestSession is the explicit legacy
// bridge for records created before v145 (and tests/older binaries that wrote a
// session without the new dual-write). It persists the newest session snapshot
// once; callers then re-read the durable owners. Normal lifecycle reads never
// consume the returned session directly.
func BackfillDurableRelaunchProfilesFromLatestSession(convID string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := projectLatestSessionRelaunchProfilesForConvTx(tx, convID); err != nil {
		return err
	}
	return tx.Commit()
}

// SetConversationResumeProvenance updates only conversation-owned physical
// identity. Empty is meaningful: a failed controlled-stop capture invalidates
// unattended resume without discarding the remaining conversation facts.
func SetConversationResumeProvenance(convID, provenance string) error {
	p, err := ConversationResumeProfileForConv(convID)
	if err != nil {
		return err
	}
	if p == nil {
		return fmt.Errorf("conversation %q has no durable resume profile", convID)
	}
	p.ResumeProvenance = provenance
	return SetConversationResumeProfile(convID, *p)
}

// projectSessionRelaunchProfilesTx copies a session's current launch snapshot
// to the durable owners. This is the only session→profile bridge used after
// migration. It is called in the same transaction as launch/status/toggle
// writes, so pruning can never expose an older durable value.
type relaunchProjectionOptions struct {
	RemoteControl bool
	AutoMemory    bool
}

func projectSessionRelaunchProfilesTx(q dbExecQuerier, sessionID string, opts relaunchProjectionOptions) error {
	if strings.TrimSpace(sessionID) == "" {
		return nil
	}
	var rowID int64
	var convID, cwd, harnessName, sandboxMode, approvalPolicy, modelID, effort, askTimeout, provenance, createdAt string
	var approvalAutoReview, remoteControl, autoMemory int
	var contextWindowSize int64
	err := q.QueryRow(`SELECT rowid, conv_id, cwd, harness, sandbox_mode,
		approval_policy, approval_auto_review, model_id, effort_level,
		context_window_size, ask_user_question_timeout, remote_control,
		auto_memory, resume_provenance, created_at
		FROM sessions WHERE id = ?`, sessionID).Scan(
		&rowID, &convID, &cwd, &harnessName, &sandboxMode,
		&approvalPolicy, &approvalAutoReview, &modelID, &effort,
		&contextWindowSize, &askTimeout, &remoteControl,
		&autoMemory, &provenance, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	convID = strings.TrimSpace(convID)
	if convID == "" {
		return nil
	}
	if strings.TrimSpace(harnessName) == "" {
		harnessName = DefaultHarness
	}
	var existingConversation *ConversationResumeProfile
	var existingConversationRaw string
	err = q.QueryRow(`SELECT profile_json FROM conversation_resume_profiles WHERE conv_id = ?`, convID).
		Scan(&existingConversationRaw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil {
		existingConversation, err = decodeConversationResumeProfile(existingConversationRaw)
		if err != nil {
			return err
		}
		if existingConversation != nil && sessionProjectionIsOlder(existingConversation, createdAt, rowID) {
			return nil
		}
	}
	// Capability-incompatible legacy flags are process telemetry, not durable
	// intent. Normalize them while projecting so a stale/hand-edited Codex row
	// cannot arm Claude-only features if the stable agent later relaunches.
	if !strings.EqualFold(harnessName, DefaultHarness) {
		remoteControl = 0
		autoMemory = 0
		askTimeout = ""
		if normalized, ok := conservativeCodexApprovalProjection(approvalPolicy); ok {
			approvalPolicy = normalized
		}
	}
	conversation := ConversationResumeProfile{
		Version: RelaunchProfileVersion, Harness: harnessName,
		Cwd: strings.TrimSpace(cwd), ResumeProvenance: provenance,
		SourceSessionID: sessionID, SourceSessionCreatedAt: createdAt, SourceSessionRowID: rowID,
	}
	agent := AgentRelaunchProfile{
		Version:                RelaunchProfileVersion,
		SandboxMode:            stringPtr(sandboxMode),
		ApprovalPolicy:         stringPtr(approvalPolicy),
		ApprovalAutoReview:     boolPtr(approvalAutoReview != 0),
		AskUserQuestionTimeout: stringPtr(askTimeout),
	}
	if modelID != "" {
		agent.ModelID = stringPtr(modelID)
	}
	if effort != "" {
		agent.Effort = stringPtr(effort)
	}
	if contextWindowSize > 0 {
		agent.ContextWindowSize = int64Ptr(contextWindowSize)
	}
	if opts.RemoteControl {
		agent.RemoteControl = boolPtr(remoteControl != 0)
	}
	if opts.AutoMemory {
		agent.AutoMemory = boolPtr(autoMemory != 0)
	}
	if existingConversation != nil && existingConversation.FallbackRelaunch != nil {
		previous := existingConversation.FallbackRelaunch
		sameSourceGeneration := existingConversation.SourceSessionCreatedAt == createdAt &&
			existingConversation.SourceSessionRowID == rowID
		if agent.ModelID == nil {
			agent.ModelID = previous.ModelID
		}
		if agent.Effort == nil {
			agent.Effort = previous.Effort
		}
		if agent.ContextWindowSize == nil {
			agent.ContextWindowSize = previous.ContextWindowSize
		}
		if sameSourceGeneration && agent.RemoteControl == nil {
			agent.RemoteControl = previous.RemoteControl
		}
		if sameSourceGeneration && agent.AutoMemory == nil {
			agent.AutoMemory = previous.AutoMemory
		}
	}
	conversation.FallbackRelaunch = &agent
	conversationRaw, err := encodeRelaunchProfile(conversation)
	if err != nil {
		return err
	}
	if _, err := q.Exec(`INSERT INTO conversation_resume_profiles (conv_id, profile_json, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT(conv_id) DO UPDATE SET profile_json = excluded.profile_json, updated_at = excluded.updated_at`,
		convID, conversationRaw, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	var haveAgentSpine int
	if err := q.QueryRow(`SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'table' AND name IN ('agents', 'agent_conversations')`).Scan(&haveAgentSpine); err != nil {
		return err
	}
	if haveAgentSpine != 2 {
		return nil
	}
	var haveAgentHeadColumn int
	if err := q.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('agents')
		WHERE name = 'current_conv_id'`).Scan(&haveAgentHeadColumn); err != nil {
		return err
	}
	if haveAgentHeadColumn == 0 {
		return nil
	}

	var agentID, existingAgentRaw string
	err = q.QueryRow(`SELECT ac.agent_id, a.relaunch_profile
		FROM agent_conversations ac
		JOIN agents a ON a.agent_id = ac.agent_id AND a.current_conv_id = ac.conv_id
		WHERE ac.conv_id = ?`, convID).Scan(&agentID, &existingAgentRaw)
	if errors.Is(err, sql.ErrNoRows) {
		// Plain conversations and superseded generations keep their own
		// conversation profile but cannot overwrite current agent intent.
		return nil
	}
	if err != nil {
		return err
	}
	existingAgent, err := decodeAgentRelaunchProfile(existingAgentRaw)
	if err != nil {
		return err
	}
	if existingAgent != nil {
		merged := *existingAgent
		merged.Version = RelaunchProfileVersion
		merged.SandboxMode = agent.SandboxMode
		merged.ApprovalPolicy = agent.ApprovalPolicy
		merged.ApprovalAutoReview = agent.ApprovalAutoReview
		merged.AskUserQuestionTimeout = agent.AskUserQuestionTimeout
		if agent.ModelID != nil {
			merged.ModelID = agent.ModelID
		}
		if agent.Effort != nil {
			merged.Effort = agent.Effort
		}
		if agent.ContextWindowSize != nil {
			merged.ContextWindowSize = agent.ContextWindowSize
		}
		if agent.RemoteControl != nil {
			merged.RemoteControl = agent.RemoteControl
		}
		if agent.AutoMemory != nil {
			merged.AutoMemory = agent.AutoMemory
		}
		agent = merged
	}
	agentRaw, err := encodeRelaunchProfile(agent)
	if err != nil {
		return err
	}
	_, err = q.Exec(`UPDATE agents SET relaunch_profile = ? WHERE agent_id = ?`, agentRaw, agentID)
	return err
}

func sessionProjectionIsOlder(existing *ConversationResumeProfile, createdAt string, rowID int64) bool {
	if existing == nil || existing.SourceSessionCreatedAt == "" {
		return false
	}
	currentTime, currentErr := time.Parse(time.RFC3339Nano, existing.SourceSessionCreatedAt)
	incomingTime, incomingErr := time.Parse(time.RFC3339Nano, createdAt)
	if currentErr == nil && incomingErr == nil && !incomingTime.Equal(currentTime) {
		return incomingTime.Before(currentTime)
	}
	if (currentErr != nil || incomingErr != nil) && createdAt != existing.SourceSessionCreatedAt {
		return createdAt < existing.SourceSessionCreatedAt
	}
	return rowID < existing.SourceSessionRowID
}

// conservativeCodexApprovalProjection repairs legacy rows whose harness tag
// changed to Codex while retaining a Claude permission-mode token. The mapping
// is deliberately narrow: known foreign/blank values become Codex's least
// automatic posture; arbitrary corrupt values remain untouched so lifecycle
// validation rejects them instead of inventing authority.
func conservativeCodexApprovalProjection(policy string) (string, bool) {
	switch strings.TrimSpace(policy) {
	case "never", "untrusted", "on-failure", "on-request":
		return policy, true
	case "", "inherit", "auto", "default", "acceptEdits", "bypassPermissions", "plan", "delegate", "dontAsk":
		return "untrusted", true
	default:
		return policy, false
	}
}

func projectLatestSessionRelaunchProfilesForConvTx(q dbExecQuerier, convID string) error {
	var haveProfiles int
	if err := q.QueryRow(`SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'table' AND name = 'conversation_resume_profiles'`).Scan(&haveProfiles); err != nil {
		return err
	}
	if haveProfiles == 0 {
		return nil // enrollment during the pre-v145 migration chain
	}
	var sessionID string
	err := q.QueryRow(`SELECT id FROM sessions WHERE conv_id = ?
		ORDER BY julianday(created_at) DESC, rowid DESC LIMIT 1`, strings.TrimSpace(convID)).Scan(&sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return projectSessionRelaunchProfilesTx(q, sessionID, relaunchProjectionOptions{
		RemoteControl: true, AutoMemory: true,
	})
}

// execSessionUpdateAndProject applies an out-of-band session update and its
// durable projection atomically. The SQL text is compile-time caller-owned;
// values remain bound parameters.
func execSessionUpdateAndProject(sessionID string, opts relaunchProjectionOptions, stmt string, args ...any) error {
	d, err := Open()
	if err != nil {
		return err
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(stmt, args...); err != nil {
		return err
	}
	if err := projectSessionRelaunchProfilesTx(tx, sessionID, opts); err != nil {
		return err
	}
	return tx.Commit()
}

// seedAgentRelaunchProfileFromSpawnConfigTx records only fields explicitly
// present in the historical request. Profile/group-resolved omissions remain
// nil (unknown) rather than being upgraded into authority by today's defaults.
func seedAgentRelaunchProfileFromSpawnConfigTx(q dbExecQuerier, agentID, raw string) error {
	var spawn struct {
		SandboxMode            *string `json:"sandbox"`
		ApprovalPolicy         *string `json:"approval"`
		ApprovalAutoReview     *bool   `json:"auto_review"`
		ModelID                *string `json:"model"`
		Effort                 *string `json:"effort"`
		AskUserQuestionTimeout *string `json:"ask_user_question_timeout"`
		RemoteControl          *bool   `json:"remote_control"`
		AutoMemory             *bool   `json:"auto_memory"`
	}
	if err := json.Unmarshal([]byte(raw), &spawn); err != nil {
		return nil // audit JSON was historically best-effort; leave it unknown
	}
	p := AgentRelaunchProfile{
		Version:                RelaunchProfileVersion,
		SandboxMode:            spawn.SandboxMode,
		ApprovalPolicy:         spawn.ApprovalPolicy,
		ApprovalAutoReview:     spawn.ApprovalAutoReview,
		ModelID:                spawn.ModelID,
		Effort:                 spawn.Effort,
		AskUserQuestionTimeout: spawn.AskUserQuestionTimeout,
		RemoteControl:          spawn.RemoteControl,
		AutoMemory:             spawn.AutoMemory,
	}
	encoded, err := encodeRelaunchProfile(p)
	if err != nil {
		return err
	}
	_, err = q.Exec(`UPDATE agents SET relaunch_profile = ?
		WHERE agent_id = ? AND relaunch_profile = ''`, encoded, agentID)
	return err
}
