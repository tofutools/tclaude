package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

const RelaunchProfileVersion = 1

// TemporaryHarnessBuiltinModeSource is the attribution paired with the reversible
// operator override on every relaunch surface.
const TemporaryHarnessBuiltinModeSource = "temporary dashboard unlock"

// AssignedSandboxImplementationSource is the attribution paired with a durable
// operator reassignment of an existing agent's sandbox implementation. It is not
// a resolution tier: no spawn profile chose this posture, an operator replaced it
// after the fact, and the badge must say so rather than keep crediting whichever
// tier supplied the mode this assignment overwrote.
const AssignedSandboxImplementationSource = "operator sandbox assignment"

// AgentRelaunchProfile is mutable launch intent owned by the stable agent.
//
// The Go identifiers on its sandbox-mode fields say harness-builtin; the
// persisted spellings do not (TCL-1023). The column is still `sandbox_mode`,
// its attribution `sandbox_mode_source`, and the durable override key
// `temporary_sandbox_mode`, because renaming a persisted name buys nothing a
// reader of this code gains from the field name and costs a migration plus a
// rewrite of every AgentRelaunchProfile JSON blob already on disk. The same
// holds for the dashboard payload keys and the session-state file's
// `sandboxMode`: those are wire and on-disk compatibility surfaces and keep
// their spelling deliberately.
//
// What the rename does guarantee is narrower and is the property to preserve:
// no Go IDENTIFIER anywhere in the tree calls this axis a bare `sandbox mode`
// any more. A new field or local that does is reintroducing the ambiguity this
// rename removed — the persisted keys are the exception, not the precedent.
// Pointer fields distinguish an observed/selected zero value from unknown
// legacy state. Unknown authority-bearing values are resolved fail-closed by
// the lifecycle layer rather than silently replaced with today's defaults.
type AgentRelaunchProfile struct {
	Version            int     `json:"version"`
	HarnessBuiltinMode *string `json:"sandbox_mode,omitempty"`
	// SandboxImplementation is the durable owner of OS-level confinement.
	// nil is legacy/unknown; harness-builtin is the explicit compatibility
	// default for every new launch.
	SandboxImplementation *string `json:"sandbox_implementation,omitempty"`
	// HarnessBuiltinModeSource names the resolution tier that CHOSE HarnessBuiltinMode — an
	// explicit flag, or the named/group-default/global-default spawn profile
	// that carried it. It is durable because the badge attributes the launch's
	// containment to it, and an agent that has been relaunched a few times must
	// not lose that attribution and fall back to an anonymous "this launch".
	// nil = unknown (legacy record, or a launch that recorded none).
	HarnessBuiltinModeSource *string `json:"sandbox_mode_source,omitempty"`
	// TemporaryHarnessBuiltinMode is a reversible operator override applied only to
	// relaunches of this stable agent. HarnessBuiltinMode above remains the agent's
	// normal posture so clearing this field restores the exact recorded mode
	// instead of trying to reconstruct it from the currently-running session.
	//
	// The override lives in the existing versioned JSON spine rather than a
	// session row because it must survive the stop→resume gap and daemon
	// restarts. Session projection deliberately never copies it from process
	// telemetry; only the explicit operator action sets or clears it.
	TemporaryHarnessBuiltinMode *string `json:"temporary_sandbox_mode,omitempty"`
	ApprovalPolicy              *string `json:"approval_policy,omitempty"`
	ToolGovernance              *string `json:"tools,omitempty"`
	ApprovalAutoReview          *bool   `json:"approval_auto_review,omitempty"`
	ModelID                     *string `json:"model_id,omitempty"`
	Effort                      *string `json:"effort,omitempty"`
	ContextWindowSize           *int64  `json:"context_window_size,omitempty"`
	// ConfiguredContextWindowMax is the Copilot launch-intent cap. It is
	// deliberately distinct from ContextWindowSize, which is an observed
	// context snapshot used by status/dashboard projections.
	ConfiguredContextWindowMax *int64 `json:"context_window_max,omitempty"`
	// CopilotAPI is the durable "this agent is driven over the Copilot API
	// rather than tmux send-keys" posture. nil means unknown/legacy, which
	// resolves to the send-keys path — so an agent recorded before this field
	// existed relaunches exactly as it did before. See TCL-1053.
	CopilotAPI *bool `json:"copilot_api,omitempty"`
	// FastMode preserves an explicit Codex tier across relaunches. nil means
	// inherit config.toml; true/false force fast/standard respectively.
	FastMode               *bool   `json:"fast_mode,omitempty"`
	AskUserQuestionTimeout *string `json:"ask_user_question_timeout,omitempty"`
	RemoteControl          *bool   `json:"remote_control,omitempty"`
	AutoMemory             *bool   `json:"auto_memory,omitempty"`
	// SSHWorkaround is the durable Codex Git-over-SSH compatibility posture.
	// nil means unknown/legacy; false is an explicit opt-out.
	SSHWorkaround *bool `json:"ssh_workaround,omitempty"`
	// AutoCompactWindow is the durable CLAUDE_CODE_AUTO_COMPACT_WINDOW token
	// count ("" = known intent to pin nothing; nil = unknown/legacy). Distinct
	// from ContextWindowSize above, which is an OBSERVED statusline value used to
	// re-derive a 1M model's suffix — this one is operator intent.
	AutoCompactWindow *string `json:"auto_compact_window,omitempty"`
	// ContextFeatures is the durable startup-context trim map (slug → "on" |
	// "off"). A non-nil pointer to a possibly-empty map means "known intent",
	// which is what lets an agent deliberately trimmed back to nothing stay that
	// way across a relaunch instead of reverting to unknown/legacy. See TCL-597.
	ContextFeatures *map[string]string `json:"context_features,omitempty"`
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

// AgentRelaunchProfileRaw returns the stored profile blob verbatim, for a caller
// that intends to compare-and-set it.
//
// Verbatim rather than decoded, because a decode-then-re-encode round trip is
// exactly what a compare-and-set must NOT do: the value being guarded is the
// bytes another writer would overwrite, and any normalisation on the way through
// makes the comparison a comparison of two normalisations instead. An empty
// string means the agent exists with no profile yet (the column is
// `TEXT NOT NULL DEFAULT ”`), which is a different case from an unknown agent
// and is reported as such.
func AgentRelaunchProfileRaw(agentID string) (string, error) {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return "", errors.New("AgentRelaunchProfileRaw: agent_id required")
	}
	d, err := Open()
	if err != nil {
		return "", err
	}
	var raw string
	err = d.QueryRow(`SELECT relaunch_profile FROM agents WHERE agent_id = ?`,
		agentID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("AgentRelaunchProfileRaw: unknown agent %s", agentID)
	}
	if err != nil {
		return "", err
	}
	return raw, nil
}

// CompareAndSetAgentCopilotAPI flips ONE field of a stable agent's durable
// relaunch profile, atomically, and only while the stored blob is still exactly
// what the caller read.
//
// Reports false with no error when the guard did not hold, which the caller must
// treat as "somebody else owns this record right now" rather than as success.
//
// # Why this is not SetAgentRelaunchProfile
//
// SetAgentRelaunchProfile replaces the WHOLE BLOB, so every caller of it does
// read-modify-write. That is safe today only because there is exactly one
// writer: agentd. The comment above agentd's own spawn-enrollment write says
// what the hazard is in its own words — replacing the whole profile would turn
// an explicit SSHWorkaround opt-out into unknown, and then back into Codex's
// default-on posture on the next clone or reincarnation.
//
// A second writer in a second process would make that hazard live. Two
// read-modify-writes with no lock between them silently lose one side's edit,
// and the side more likely to be lost is the rarer one — an operator's
// deliberate change, erased by the busier automated writer, with nothing logged
// on either side. A durable posture change that does not stick is worse than one
// that refuses.
//
// So this does not read-modify-write at all. `json_set` edits the one member
// inside SQLite, in a single statement, preserving every other field including
// the ones no Go struct in this process knows about. There is no window between
// a read and a write to lose an edit in, because there is no read.
//
// # Why there is a compare-and-set on top of that
//
// Atomicity of one statement does not make the DECISION to write it current.
// The caller of this is the daemon-unreachable fallback path, and a daemon can
// come up between the reachability probe and this write — a probe is a reading,
// not a fact that stays true. The `relaunch_profile = ?` guard turns that from
// a silent overwrite of whatever the daemon just composed into a refusal the
// caller can report and the operator can retry.
//
// json_valid is required as well as the guard: the column defaults to the empty
// string, and json_set against a non-JSON value raises rather than no-ops. An
// agent with no profile yet is not a candidate for a targeted field edit — there
// is nothing to edit inside — and is reported by the same false return.
func CompareAndSetAgentCopilotAPI(agentID string, value bool, expected string) (bool, error) {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return false, errors.New("CompareAndSetAgentCopilotAPI: agent_id required")
	}
	if strings.TrimSpace(expected) == "" {
		return false, fmt.Errorf(
			"CompareAndSetAgentCopilotAPI: agent %s has no durable relaunch profile to "+
				"edit; a targeted field update needs an existing profile to edit inside",
			agentID)
	}
	encoded := "false"
	if value {
		encoded = "true"
	}
	d, err := Open()
	if err != nil {
		return false, err
	}
	res, err := d.Exec(`
		UPDATE agents
		   SET relaunch_profile = json_set(relaunch_profile, '$.copilot_api', json(?))
		 WHERE agent_id = ?
		   AND relaunch_profile = ?
		   AND json_valid(relaunch_profile) = 1`,
		encoded, agentID, expected)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// CopilotDriveRecord names WHICH durable record currently answers "which drive
// does this conversation run on".
type CopilotDriveRecord string

const (
	// CopilotDriveRecordAgentProfile is the stable agent's relaunch profile, which
	// wins field by field. Every Copilot agent born since the drive became
	// selectable has a non-nil value here, including an explicit false.
	CopilotDriveRecordAgentProfile CopilotDriveRecord = "agent profile"
	// CopilotDriveRecordConversationFallback is the conversation's own record. It
	// answers for legacy Copilot agents born before the field existed, and for
	// every conversation with no agent row at all — clones, and a direct
	// `session new`.
	CopilotDriveRecordConversationFallback CopilotDriveRecord = "conversation fallback"
	// CopilotDriveRecordNone means nothing records a drive for this conversation.
	// Routing reads that as send-keys, so there is nothing for a durable "off" to
	// turn off, and a caller should say so rather than writing a record to assert
	// a posture nobody chose.
	CopilotDriveRecordNone CopilotDriveRecord = "none"
)

// CopilotDriveTarget is where a durable drive change for one conversation must
// land, and what is currently recorded there.
type CopilotDriveTarget struct {
	Record  CopilotDriveRecord
	Value   bool
	AgentID string
	// Raw is the stored blob of the record named above, for the compare-and-set.
	Raw string
}

// CopilotDriveTargetForConv resolves which record a durable drive change must
// write, in the SAME precedence routing uses.
//
// That sentence is the whole contract and it is the reason this function may not
// be "simplified" later. agentd.copilotLaunchIntentForConv applies the agent
// relaunch profile field by field FIRST and lets the conversation fallback fill
// only what the agent profile left nil. A writer that picked a different record
// than the router reads would report success while changing nothing that routes —
// which is the exact failure this Copilot series keeps producing, a mechanism
// truthful about the wrong subject. Same precedence, same source, or the
// operation is theatre. If the router's precedence changes, the router changes
// first and this follows.
//
// It deliberately does NOT resolve the drive's effective boolean the way the
// router does. The router collapses "recorded false" and "recorded nothing" into
// the same routing answer, correctly, because both mean do not assume the API
// channel. A writer cannot collapse them: one is a record to edit and the other
// is a record that does not exist, and telling an operator "turned off" when
// nothing was recorded would be a claim about a write that did not happen.
//
// A read failure is returned rather than swallowed. Guessing the record for a
// write is how an operator's rollback lands somewhere nobody reads.
func CopilotDriveTargetForConv(convID string) (CopilotDriveTarget, error) {
	convID = strings.TrimSpace(convID)
	if convID == "" {
		return CopilotDriveTarget{}, errors.New("CopilotDriveTargetForConv: conv_id required")
	}
	agentID, err := AgentIDForConv(convID)
	if err != nil {
		return CopilotDriveTarget{}, err
	}
	if agentID != "" {
		profile, err := AgentRelaunchProfileForConv(convID)
		if err != nil {
			return CopilotDriveTarget{}, err
		}
		if profile != nil && profile.CopilotAPI != nil {
			raw, err := AgentRelaunchProfileRaw(agentID)
			if err != nil {
				return CopilotDriveTarget{}, err
			}
			return CopilotDriveTarget{
				Record:  CopilotDriveRecordAgentProfile,
				Value:   *profile.CopilotAPI,
				AgentID: agentID,
				Raw:     raw,
			}, nil
		}
	}
	conversation, err := ConversationResumeProfileForConv(convID)
	if err != nil {
		return CopilotDriveTarget{}, err
	}
	if conversation != nil && conversation.FallbackRelaunch != nil &&
		conversation.FallbackRelaunch.CopilotAPI != nil {
		raw, err := ConversationResumeProfileRaw(convID)
		if err != nil {
			return CopilotDriveTarget{}, err
		}
		return CopilotDriveTarget{
			Record:  CopilotDriveRecordConversationFallback,
			Value:   *conversation.FallbackRelaunch.CopilotAPI,
			AgentID: agentID,
			Raw:     raw,
		}, nil
	}
	return CopilotDriveTarget{Record: CopilotDriveRecordNone, AgentID: agentID}, nil
}

// ConversationResumeProfileRaw returns the stored conversation profile blob
// verbatim, for a caller that intends to compare-and-set it. Verbatim for the
// same reason AgentRelaunchProfileRaw is: a guard on bytes cannot be built out of
// a decode/re-encode round trip. An empty string means no row.
func ConversationResumeProfileRaw(convID string) (string, error) {
	convID = strings.TrimSpace(convID)
	if convID == "" {
		return "", errors.New("ConversationResumeProfileRaw: conv_id required")
	}
	d, err := Open()
	if err != nil {
		return "", err
	}
	var raw string
	err = d.QueryRow(`SELECT profile_json FROM conversation_resume_profiles WHERE conv_id = ?`,
		convID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return raw, nil
}

// CompareAndSetConversationCopilotAPI is the conversation-fallback twin of
// CompareAndSetAgentCopilotAPI, and exists so that BOTH records a durable drive
// change can land on are race-free rather than only the modern one.
//
// # Why this needed its own function rather than SetConversationCopilotAPI
//
// SetConversationCopilotAPI and SetSessionCopilotAPI both go through a plain
// read-modify-write of the whole conversation profile
// (updateConversationFallbackRelaunch / updateSessionFallbackRelaunch:
// load, mutate, write back) with no guard anywhere. That is sound while the
// writers are launches, which do not contend for one conversation. It stops
// being sound the moment an operator-initiated change writes the same record
// from another process, and the damage lands exactly where it hurts most:
// SetConversationCopilotAPI's own doc explains that for a conversation with no
// stable agent profile the launch-time write is the ONLY writer and one lost
// write is permanent. Those are precisely the conversations that reach this
// function, so an unguarded operator write here would be the erased-rollback
// case on the record class where erasure does not heal.
//
// # Why the parent-object guard is not defensive padding
//
// SQLite's json_set replaces or appends a LEAF; it does not create intermediate
// objects. So this can only edit a profile that already has a fallback_relaunch
// object. That is not a limitation on this path in practice, and the reason is
// worth stating rather than discovering later: the only conversations a durable
// "turn this drive off" targets here are ones whose recorded drive is currently
// ON in the fallback, and the object holding that value must exist for it to be
// recorded. A missing parent therefore means "this conversation's drive is not
// recorded here", which is a guard miss for the caller to report — not an error,
// and not something to paper over by materialising a parent this function has no
// business inventing.
//
// updated_at is bumped so an operator's change is as visible to anything reading
// recency as a launch's would be.
//
// # Why the seeding does not belong in here
//
// A caller that wants to PIN a drive onto a conversation that records none has to
// bring the record into existence first. That step stays at the policy layer, and
// the general statement of why is: A COMPARE-AND-SET ON A LEAF WHOSE PARENT MAY
// NOT EXIST IS TWO OPERATIONS PRETENDING TO BE ONE. The guard here would silently
// become a guard on nothing, and "created a record" is a different fact from
// "edited a record" that an operator needs reported — an operator told CREATED
// learns that nothing was recorded before, which is itself the diagnostic that a
// lower resolution tier was speaking for this agent. A function that quietly did
// both could not tell them apart.
func CompareAndSetConversationCopilotAPI(
	convID string, value bool, expected string,
) (bool, error) {
	convID = strings.TrimSpace(convID)
	if convID == "" {
		return false, errors.New("CompareAndSetConversationCopilotAPI: conv_id required")
	}
	if strings.TrimSpace(expected) == "" {
		return false, fmt.Errorf(
			"CompareAndSetConversationCopilotAPI: conversation %s has no resume profile "+
				"to edit; a targeted field update needs an existing profile to edit inside",
			convID)
	}
	encoded := "false"
	if value {
		encoded = "true"
	}
	d, err := Open()
	if err != nil {
		return false, err
	}
	res, err := d.Exec(`
		UPDATE conversation_resume_profiles
		   SET profile_json = json_set(
		           profile_json, '$.fallback_relaunch.copilot_api', json(?)),
		       updated_at = ?
		 WHERE conv_id = ?
		   AND profile_json = ?
		   AND json_valid(profile_json) = 1
		   AND json_type(profile_json, '$.fallback_relaunch') = 'object'`,
		encoded, dbTime(time.Now().UTC()), convID, expected)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// SetTemporaryHarnessBuiltinMode atomically sets or clears a stable agent's temporary
// sandbox override. When enabling it, normalMode/normalImplementation/
// normalSource freeze the already-resolved normal launch posture if the
// agent's durable fields are still unknown (legacy agents); this prevents the
// first overridden session projection from becoming the only remaining
// sandbox evidence.
//
// A nil override clears the temporary state. A non-nil override is stored
// verbatim after lifecycle-layer validation.
func SetTemporaryHarnessBuiltinMode(
	agentID string,
	normalMode string,
	normalImplementation string,
	normalSource string,
	override *string,
) error {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return errors.New("SetTemporaryHarnessBuiltinMode: agent_id required")
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

	var raw string
	err = tx.QueryRow(`SELECT relaunch_profile FROM agents WHERE agent_id = ?`,
		agentID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("SetTemporaryHarnessBuiltinMode: unknown agent %s", agentID)
	}
	if err != nil {
		return err
	}
	profile, err := decodeAgentRelaunchProfile(raw)
	if err != nil {
		return err
	}
	if profile == nil {
		profile = &AgentRelaunchProfile{Version: RelaunchProfileVersion}
	}
	if override != nil {
		mode := strings.TrimSpace(*override)
		if mode == "" {
			return errors.New("SetTemporaryHarnessBuiltinMode: override mode required")
		}
		if profile.HarnessBuiltinMode == nil {
			profile.HarnessBuiltinMode = stringPtr(strings.TrimSpace(normalMode))
		}
		if profile.SandboxImplementation == nil {
			profile.SandboxImplementation = stringPtr(strings.TrimSpace(normalImplementation))
		}
		if profile.HarnessBuiltinModeSource == nil {
			profile.HarnessBuiltinModeSource = stringPtr(strings.TrimSpace(normalSource))
		}
		profile.TemporaryHarnessBuiltinMode = &mode
	} else {
		// The restore caller passes the already-resolved normal implementation.
		// Besides keeping a normal clear idempotent, this repairs temporary
		// overrides created by versions that projected harness-builtin over the
		// durable tclaude-layer/stacked value; lifecycle recovered that value
		// from the pre-override session history before reaching this write.
		if profile.TemporaryHarnessBuiltinMode != nil &&
			strings.TrimSpace(normalImplementation) != "" {
			profile.SandboxImplementation =
				stringPtr(strings.TrimSpace(normalImplementation))
		}
		profile.TemporaryHarnessBuiltinMode = nil
	}
	encoded, err := encodeRelaunchProfile(*profile)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE agents SET relaunch_profile = ? WHERE agent_id = ?`,
		encoded, agentID); err != nil {
		return err
	}
	return tx.Commit()
}

// SetTemporaryHarnessBuiltinModeForConv is a routing adapter for callers that only
// have a conversation generation. The state itself is always keyed by agent.
func SetTemporaryHarnessBuiltinModeForConv(
	convID string,
	normalMode string,
	normalImplementation string,
	normalSource string,
	override *string,
) error {
	agentID, err := AgentIDForConv(convID)
	if err != nil {
		return err
	}
	if agentID == "" {
		return fmt.Errorf("SetTemporaryHarnessBuiltinModeForConv: conversation %s is not an agent", convID)
	}
	return SetTemporaryHarnessBuiltinMode(
		agentID, normalMode, normalImplementation, normalSource, override,
	)
}

// TemporaryHarnessBuiltinModeForConv reports the active reversible sandbox override
// on a stable agent. ok=false means normal relaunch behavior.
func TemporaryHarnessBuiltinModeForConv(convID string) (mode string, ok bool, err error) {
	agentID, err := AgentIDForConv(convID)
	if err != nil || agentID == "" {
		return "", false, err
	}
	return TemporaryHarnessBuiltinModeForAgent(agentID)
}

// TemporaryHarnessBuiltinModeForAgent reads the override by its actual durable key.
// Conversation-based callers are only routing adapters; rotations must not
// move or duplicate this state.
func TemporaryHarnessBuiltinModeForAgent(agentID string) (mode string, ok bool, err error) {
	d, err := Open()
	if err != nil {
		return "", false, err
	}
	var raw string
	err = d.QueryRow(`SELECT relaunch_profile FROM agents WHERE agent_id = ?`,
		strings.TrimSpace(agentID)).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	profile, err := decodeAgentRelaunchProfile(raw)
	if err != nil || profile == nil || profile.TemporaryHarnessBuiltinMode == nil {
		return "", false, err
	}
	return strings.TrimSpace(*profile.TemporaryHarnessBuiltinMode), true, nil
}

// ErrTemporarySandboxOverrideActive rejects a durable posture write while the
// reversible dashboard unlock is in force. The two states are layered — the
// override deliberately preserves the normal posture underneath so restore can
// put it back byte-for-byte — so writing the normal posture from under an active
// override would silently change what "restore" restores.
var ErrTemporarySandboxOverrideActive = errors.New(
	"this agent is running under the temporary sandbox unlock; restore its normal sandbox first")

// AssignAgentSandboxImplementation durably rewrites the sandbox posture a stable
// agent will RELAUNCH under: the implementation that owns OS-level confinement,
// the harness-builtin mode that implementation implies, and the attribution for
// that mode.
//
// All three move together because they are one posture. The implementation
// decides what the harness's own sandbox may be — `resource-only` and `off`
// resolve every harness to its no-confinement mode — so recording a new
// implementation while leaving the old mode behind would leave the durable
// record describing a launch that never happens. The caller resolves both values
// through the harness layer (harness.ResolveSandboxImplementationMode) and passes
// the results; this function only persists them atomically.
//
// The source is the operator attribution for the assignment, not a spawn-profile
// tier: the mode is no longer the one any profile chose, and crediting the
// original tier for a posture an operator replaced is the false attribution the
// relaunch profile's HarnessBuiltinModeSource exists to prevent.
func AssignAgentSandboxImplementation(
	agentID string,
	implementation string,
	harnessBuiltinMode string,
	source string,
) error {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return errors.New("AssignAgentSandboxImplementation: agent_id required")
	}
	implementation = strings.TrimSpace(implementation)
	if implementation == "" {
		return errors.New("AssignAgentSandboxImplementation: implementation required")
	}
	harnessBuiltinMode = strings.TrimSpace(harnessBuiltinMode)
	if harnessBuiltinMode == "" {
		return errors.New("AssignAgentSandboxImplementation: harness-builtin mode required")
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

	var raw string
	err = tx.QueryRow(`SELECT relaunch_profile FROM agents WHERE agent_id = ?`,
		agentID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("AssignAgentSandboxImplementation: unknown agent %s", agentID)
	}
	if err != nil {
		return err
	}
	profile, err := decodeAgentRelaunchProfile(raw)
	if err != nil {
		return err
	}
	if profile == nil {
		profile = &AgentRelaunchProfile{Version: RelaunchProfileVersion}
	}
	if profile.TemporaryHarnessBuiltinMode != nil {
		return ErrTemporarySandboxOverrideActive
	}
	profile.SandboxImplementation = stringPtr(implementation)
	profile.HarnessBuiltinMode = stringPtr(harnessBuiltinMode)
	profile.HarnessBuiltinModeSource = stringPtr(strings.TrimSpace(source))
	encoded, err := encodeRelaunchProfile(*profile)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE agents SET relaunch_profile = ? WHERE agent_id = ?`,
		encoded, agentID); err != nil {
		return err
	}
	return tx.Commit()
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
		convID, raw, dbTime(time.Now().UTC()))
	return err
}

// SetSessionConfiguredContextWindowMax records the configured Copilot meter
// denominator in the conversation fallback owned by the session's
// conversation. The cap is tclaude launch intent, not a sessions observation
// column, so it must be updated directly alongside the other relaunch facts.
// Managed agents also keep the stable agent profile; this fallback write makes
// a direct `session new --context-window-max` survive the next resume.
func SetSessionConfiguredContextWindowMax(sessionID string, value int64) error {
	return updateSessionFallbackRelaunch(sessionID, func(fallback *AgentRelaunchProfile) {
		max := value
		fallback.ConfiguredContextWindowMax = &max
	})
}

// SetSessionCopilotAPI records whether this session is driven over the Copilot
// API rather than tmux send-keys, in the same conversation fallback and for the
// same reason as SetSessionConfiguredContextWindowMax above: the posture is
// tclaude launch intent with no sessions column of its own, and a direct
// `session new --copilot-api` must survive the next resume.
func SetSessionCopilotAPI(sessionID string, value bool) error {
	return updateSessionFallbackRelaunch(sessionID, func(fallback *AgentRelaunchProfile) {
		api := value
		fallback.CopilotAPI = &api
	})
}

// SetSessionFastMode records explicit Codex service-tier intent. An empty mode
// clears the field back to inherit; on/off become true/false.
func SetSessionFastMode(sessionID, mode string) error {
	return updateSessionFallbackRelaunch(sessionID, func(fallback *AgentRelaunchProfile) {
		switch strings.TrimSpace(mode) {
		case "on":
			value := true
			fallback.FastMode = &value
		case "off":
			value := false
			fallback.FastMode = &value
		default:
			fallback.FastMode = nil
		}
	})
}

// SetConversationCopilotAPI records a conversation's Copilot drive from the
// daemon, at the moment its conversation id first becomes real.
//
// # Why this exists next to SetSessionCopilotAPI
//
// SetSessionCopilotAPI is written by the LAUNCHED PROCESS, keyed by a session
// row, and only after that row and the pane exist. That was adequate while the
// posture drove a dashboard badge. It stopped being adequate when TCL-1058 made
// the same record decide whether a message travels over RPC or is TYPED into
// the pane, because it leaves two holes a routing decision cannot have:
//
//   - a WINDOW. Between the pane coming up and the child's write there is a live,
//     addressable conversation with no recorded posture, and "no record" is
//     indistinguishable from "this agent chose keystrokes".
//   - a PERMANENT LOSS. That write is best-effort by design (it must never fail a
//     healthy launch), and for a conversation with no stable agent profile of its
//     own — every clone, and every direct `session new --copilot-api` — it is the
//     ONLY writer. One lost write leaves the conversation reading as send-keys
//     for the rest of its life.
//
// This is keyed by conv id precisely because it runs before any session row is
// guaranteed to exist. harnessName and cwd seed a conversation profile that has
// none yet; they are ignored when one already does, since the launch's own
// projection owns those fields.
//
// Recorded for a Copilot launch even when false, which is the same rule
// relaunchProfileForSpawn follows and for the same reason: a KNOWN "this agent
// is on send-keys" is a different fact from an unknown one, and only the first
// may be acted on.
//
// # This CANNOT change routing for a managed Copilot agent, despite its name
//
// It writes the CONVERSATION FALLBACK. Routing reads
// agentd.copilotLaunchIntentForConv, which applies the stable AGENT relaunch
// profile field by field FIRST and lets the fallback fill only what the agent
// profile left nil. And for Copilot the agent profile is never nil here:
// relaunchProfileForSpawn freezes CopilotAPI for every Copilot launch INCLUDING
// FALSE, deliberately, so a relaunch replays a known posture rather than an
// unknown one.
//
// So for any managed Copilot agent the agent profile always answers, this
// record is never consulted for that field, and a caller who reaches for the
// obviously-named function to move an agent OFF the API drive changes nothing
// that routes. The lever for that is the agent profile
// (SetAgentRelaunchProfile and the targeted setter beside it).
//
// The window and permanent-loss reasons above are still why this exists and is
// still correct for what it does — it answers for conversations with no agent
// profile, which is every clone and every direct `session new`. The point of
// this section is only that "records the conversation's drive" and "decides
// where a managed agent's mail goes" are different subjects, and this function
// is the first one. Established while deriving TCL-1082; kept here rather than
// in the ticket because the name is what misleads, and the name is here.
func SetConversationCopilotAPI(convID, harnessName, cwd string, value bool) error {
	return updateConversationFallbackRelaunch(convID, harnessName, cwd,
		func(fallback *AgentRelaunchProfile) {
			api := value
			fallback.CopilotAPI = &api
		})
}

// updateConversationFallbackRelaunch is the conv-keyed twin of
// updateSessionFallbackRelaunch. A conversation with no profile yet is SEEDED
// from harnessName/cwd rather than skipped: the callers that need this run
// before the launch's own projection, so "no row yet" is the ordinary case
// rather than the missing-conversation case updateSessionFallbackRelaunch
// treats as a no-op.
func updateConversationFallbackRelaunch(
	convID, harnessName, cwd string, apply func(*AgentRelaunchProfile),
) error {
	convID = strings.TrimSpace(convID)
	if convID == "" {
		return errors.New("updateConversationFallbackRelaunch: conv_id required")
	}
	conversation, err := ConversationResumeProfileForConv(convID)
	if err != nil {
		return err
	}
	if conversation == nil {
		conversation = &ConversationResumeProfile{
			Version: RelaunchProfileVersion,
			Harness: strings.TrimSpace(harnessName),
			Cwd:     strings.TrimSpace(cwd),
		}
	}
	if conversation.FallbackRelaunch == nil {
		conversation.FallbackRelaunch = &AgentRelaunchProfile{Version: RelaunchProfileVersion}
	}
	apply(conversation.FallbackRelaunch)
	return SetConversationResumeProfile(convID, *conversation)
}

// updateSessionFallbackRelaunch loads (or seeds) the conversation resume
// profile owned by sessionID's conversation, lets apply mutate its fallback
// relaunch facts, and writes it back. A session or conversation that does not
// exist is a no-op rather than an error: these are best-effort launch-intent
// records, and failing a launch over one would be the worse trade.
func updateSessionFallbackRelaunch(sessionID string, apply func(*AgentRelaunchProfile)) error {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil
	}
	d, err := Open()
	if err != nil {
		return err
	}
	var convID, harnessName, cwd string
	err = d.QueryRow(`SELECT conv_id, harness, cwd FROM sessions WHERE id = ?`, sessionID).
		Scan(&convID, &harnessName, &cwd)
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
	conversation, err := ConversationResumeProfileForConv(convID)
	if err != nil {
		return err
	}
	if conversation == nil {
		conversation = &ConversationResumeProfile{
			Version: RelaunchProfileVersion, Harness: strings.TrimSpace(harnessName), Cwd: cwd,
		}
	}
	if conversation.FallbackRelaunch == nil {
		conversation.FallbackRelaunch = &AgentRelaunchProfile{Version: RelaunchProfileVersion}
	}
	apply(conversation.FallbackRelaunch)
	return SetConversationResumeProfile(convID, *conversation)
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
	RemoteControl     bool
	AutoMemory        bool
	ContextFeatures   bool
	AutoCompactWindow bool
}

func projectSessionRelaunchProfilesTx(q dbExecQuerier, sessionID string, opts relaunchProjectionOptions) error {
	if strings.TrimSpace(sessionID) == "" {
		return nil
	}
	var rowID int64
	var convID, cwd, harnessName, harnessBuiltinMode, sandboxImplementation, approvalPolicy, modelID, effort, askTimeout, provenance string
	var createdAtStamp migrationBridgeTimestamp
	var approvalAutoReview, remoteControl, autoMemory int
	var contextWindowSize int64
	var contextFeaturesRaw, autoCompactWindow string
	// sessions.context_features arrives in v155 and sessions.auto_compact_window
	// in v156, but this projection also runs from INSIDE the v145 migration,
	// where neither column exists yet. Selecting them unconditionally would break
	// that migration, so each column is probed and substituted with a literal ''
	// when absent — the same probe-before-read discipline the agents-spine checks
	// below use.
	haveContextFeatures, err := sessionsHaveColumn(q, "context_features")
	if err != nil {
		return err
	}
	contextFeaturesColumn := "''"
	if haveContextFeatures {
		contextFeaturesColumn = "context_features"
	}
	haveAutoCompactWindow, err := sessionsHaveColumn(q, "auto_compact_window")
	if err != nil {
		return err
	}
	autoCompactWindowColumn := "''"
	if haveAutoCompactWindow {
		autoCompactWindowColumn = "auto_compact_window"
	}
	// v158. Guarded like the columns above so a projection running against a
	// not-yet-migrated database reads "unknown" rather than failing the write.
	haveHarnessBuiltinModeSource, err := sessionsHaveColumn(q, "sandbox_mode_source")
	if err != nil {
		return err
	}
	harnessBuiltinModeSourceColumn := "''"
	if haveHarnessBuiltinModeSource {
		harnessBuiltinModeSourceColumn = "sandbox_mode_source"
	}
	haveSandboxImplementation, err := sessionsHaveColumn(q, "sandbox_implementation")
	if err != nil {
		return err
	}
	sandboxImplementationColumn := "''"
	if haveSandboxImplementation {
		sandboxImplementationColumn = "sandbox_implementation"
	}
	var harnessBuiltinModeSource string
	err = q.QueryRow(`SELECT rowid, conv_id, cwd, harness, sandbox_mode, `+sandboxImplementationColumn+`, `+harnessBuiltinModeSourceColumn+`,
		approval_policy, approval_auto_review, model_id, effort_level,
		context_window_size, ask_user_question_timeout, remote_control,
		auto_memory, `+contextFeaturesColumn+`, `+autoCompactWindowColumn+`, resume_provenance, created_at
		FROM sessions WHERE id = ?`, sessionID).Scan(
		&rowID, &convID, &cwd, &harnessName, &harnessBuiltinMode, &sandboxImplementation, &harnessBuiltinModeSource,
		&approvalPolicy, &approvalAutoReview, &modelID, &effort,
		&contextWindowSize, &askTimeout, &remoteControl,
		&autoMemory, &contextFeaturesRaw, &autoCompactWindow, &provenance, &createdAtStamp)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	createdAt := createdAtStamp.Text()
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
	// intent. Normalize them while projecting so a stale/hand-edited non-Claude
	// row cannot arm Claude-only features if the stable agent later relaunches.
	if !strings.EqualFold(harnessName, DefaultHarness) {
		remoteControl = 0
		autoMemory = 0
		askTimeout = ""
		contextFeaturesRaw = ""
		autoCompactWindow = ""
	}
	// Approval-policy normalization is Codex-specific. OpenCode legitimately
	// represents "no launch-time approval policy" with the empty string.
	if strings.EqualFold(harnessName, "codex") {
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
		HarnessBuiltinMode:     stringPtr(harnessBuiltinMode),
		ApprovalPolicy:         stringPtr(approvalPolicy),
		ApprovalAutoReview:     boolPtr(approvalAutoReview != 0),
		AskUserQuestionTimeout: stringPtr(askTimeout),
	}
	// Projected as KNOWN whenever the column exists, including empty: a relaunch
	// that re-chose the mode with nothing to attribute must ERASE the previous
	// attribution rather than let it survive onto a mode its tier never chose.
	// Absent column = pre-v158 database = genuinely unknown, so nil.
	if haveHarnessBuiltinModeSource {
		agent.HarnessBuiltinModeSource = stringPtr(harnessBuiltinModeSource)
	}
	if haveSandboxImplementation {
		agent.SandboxImplementation = stringPtr(sandboxImplementation)
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
	// Only claim to KNOW the trim intent when the column was actually there to
	// read. Pre-v155 the absent column would otherwise project as "known: trims
	// nothing", which is authority this projection never observed.
	if opts.ContextFeatures && haveContextFeatures {
		features := unmarshalStringMapColumn(contextFeaturesRaw, "sessions.context_features")
		if features == nil {
			features = map[string]string{}
		}
		agent.ContextFeatures = &features
	}
	// A pinned window projects unconditionally, like model_id and effort_level:
	// it is observable launch state, and the status line records it on every
	// render (see UpdateSessionAutoCompactWindow), so the freshest non-empty
	// value is always the right one. The EMPTY string is different — it is only
	// authority when the launch path asserted "nothing pinned", which is what the
	// opts flag carries. Pre-v156 the absent column would otherwise project as
	// "known: no window pinned", which this projection never observed.
	switch {
	case strings.TrimSpace(autoCompactWindow) != "":
		agent.AutoCompactWindow = stringPtr(strings.TrimSpace(autoCompactWindow))
	case opts.AutoCompactWindow && haveAutoCompactWindow:
		agent.AutoCompactWindow = stringPtr("")
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
		if agent.ToolGovernance == nil {
			agent.ToolGovernance = previous.ToolGovernance
		}
		if sameSourceGeneration && agent.RemoteControl == nil {
			agent.RemoteControl = previous.RemoteControl
		}
		if sameSourceGeneration && agent.AutoMemory == nil {
			agent.AutoMemory = previous.AutoMemory
		}
		if agent.SSHWorkaround == nil {
			agent.SSHWorkaround = previous.SSHWorkaround
		}
		if sameSourceGeneration && agent.ContextFeatures == nil {
			agent.ContextFeatures = previous.ContextFeatures
		}
		if sameSourceGeneration && agent.AutoCompactWindow == nil {
			agent.AutoCompactWindow = previous.AutoCompactWindow
		}
		if sameSourceGeneration && agent.FastMode == nil {
			agent.FastMode = previous.FastMode
		}
	}
	// The two Copilot launch facts carry into the CONVERSATION fallback only,
	// which is why they are applied to a copy here rather than to `agent` in the
	// block above. Both records are written from that one struct, and the
	// direction matters in a way none of the other carries have to think about.
	//
	// Why they must carry at all: neither has a session column, so this
	// projection has nothing to say about them and must not be able to erase
	// them. It did — deterministically, on the first status tick after a launch
	// recorded them — and since TCL-1058 an erased drive is not a lost badge:
	// copilotAPIDriven reads this record to decide whether a message goes over
	// RPC or gets TYPED into the pane, and a missing record is
	// indistinguishable from "this agent chose keystrokes". For a conversation
	// with no stable agent posture of its own — every clone — this fallback is
	// the only holder, so the erasure was permanent.
	//
	// Why they must NOT reach `agent`: that struct is also merged into the
	// STABLE AGENT's relaunch_profile below, where these two fields are frozen
	// at birth by relaunchProfileForSpawn and nothing else may write them. The
	// conversation fallback, by contrast, is written by any launch on the
	// conversation — including the plain-CLI resumes in `pkg/claude/conv`, which
	// cannot produce the API channel at all. Letting that flow upward would let
	// one such resume permanently un-choose a managed agent's drive, inverting
	// the precedence composeAgentRelaunchProfile is built on.
	//
	// Carried unconditionally rather than gated on sameSourceGeneration,
	// because a resume mints a new session row whose FIRST SaveSession projects
	// before session.RecordLaunchPosture re-asserts the posture — a generation
	// gate would leave the record missing across exactly that window, and that
	// window is now a routing decision. It cannot pin a stale value either: a
	// launch that RESOLVED these fields re-asserts them immediately afterwards
	// including their zeros, so a relaunch that genuinely turned the drive off
	// writes false over this a moment later. Since TCL-1076 a launch that could
	// NOT resolve them passes nil to RecordLaunchPosture instead of a zero, and
	// this carry-forward is precisely what preserves the record for it.
	conversationFallback := agent
	if existingConversation != nil && existingConversation.FallbackRelaunch != nil {
		if conversationFallback.CopilotAPI == nil {
			conversationFallback.CopilotAPI = existingConversation.FallbackRelaunch.CopilotAPI
		}
		if conversationFallback.ConfiguredContextWindowMax == nil {
			conversationFallback.ConfiguredContextWindowMax =
				existingConversation.FallbackRelaunch.ConfiguredContextWindowMax
		}
	}
	conversation.FallbackRelaunch = &conversationFallback
	conversationRaw, err := encodeRelaunchProfile(conversation)
	if err != nil {
		return err
	}
	if _, err := q.Exec(`INSERT INTO conversation_resume_profiles (conv_id, profile_json, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT(conv_id) DO UPDATE SET profile_json = excluded.profile_json, updated_at = excluded.updated_at`,
		convID, conversationRaw, dbTime(time.Now().UTC())); err != nil {
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
		// A temporary operator override is process posture, not the agent's new
		// normal intent. Keep the normal mode/source frozen while it is active;
		// clearing the override then restores those exact fields. Every other
		// projection remains unchanged.
		if existingAgent.TemporaryHarnessBuiltinMode == nil {
			merged.HarnessBuiltinMode = agent.HarnessBuiltinMode
			// The attribution travels with the mode it explains. Letting it
			// survive a projection that replaced the mode would credit the new
			// mode to whatever chose the old one.
			merged.HarnessBuiltinModeSource = agent.HarnessBuiltinModeSource
			merged.SandboxImplementation = agent.SandboxImplementation
		}
		merged.ApprovalPolicy = agent.ApprovalPolicy
		merged.ApprovalAutoReview = agent.ApprovalAutoReview
		merged.AskUserQuestionTimeout = agent.AskUserQuestionTimeout
		if agent.ToolGovernance != nil {
			merged.ToolGovernance = agent.ToolGovernance
		}
		if agent.ModelID != nil {
			merged.ModelID = agent.ModelID
		}
		if agent.Effort != nil {
			merged.Effort = agent.Effort
		}
		if agent.ContextWindowSize != nil {
			merged.ContextWindowSize = agent.ContextWindowSize
		}
		if agent.ConfiguredContextWindowMax != nil {
			merged.ConfiguredContextWindowMax = agent.ConfiguredContextWindowMax
		}
		if agent.CopilotAPI != nil {
			merged.CopilotAPI = agent.CopilotAPI
		}
		if agent.FastMode != nil {
			merged.FastMode = agent.FastMode
		}
		if agent.RemoteControl != nil {
			merged.RemoteControl = agent.RemoteControl
		}
		if agent.AutoMemory != nil {
			merged.AutoMemory = agent.AutoMemory
		}
		if agent.SSHWorkaround != nil {
			merged.SSHWorkaround = agent.SSHWorkaround
		}
		if agent.ContextFeatures != nil {
			merged.ContextFeatures = agent.ContextFeatures
		}
		if agent.AutoCompactWindow != nil {
			merged.AutoCompactWindow = agent.AutoCompactWindow
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

// sessionsHaveColumn probes for a sessions column added AFTER v145 —
// context_features (v155) and auto_compact_window (v156). This projection runs
// inside EVERY SaveSession — i.e. on every hook tick — and ALSO inside the v145
// migration, where neither column exists yet, so the probe cannot simply be
// deleted.
//
// It is deliberately NOT memoized in a package-level latch. Column existence is
// monotonic within one DATABASE, but not within a process: a single process
// legitimately opens several (legacy-import fixtures, old-schema migration
// fixtures, and every test DB), so a process-global "the column exists" latch set
// by one database makes the projection emit `SELECT context_features` against
// another that predates v155 — turning every write into an error. The pragma is
// answered from SQLite's already-loaded schema, so the per-call cost is small;
// any future caching must be keyed by database identity, not by process.
func sessionsHaveColumn(q dbExecQuerier, column string) (bool, error) {
	var n int
	if err := q.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('sessions')
		WHERE name = ?`, column).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

func sessionProjectionIsOlder(existing *ConversationResumeProfile, createdAt string, rowID int64) bool {
	if existing == nil || existing.SourceSessionCreatedAt == "" {
		return false
	}
	currentTime, currentErr := parseLegacyDBTime(existing.SourceSessionCreatedAt)
	incomingTime, incomingErr := parseLegacyDBTime(createdAt)
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
// is deliberately narrow: a known FOREIGN value — an input that was recorded
// but cannot be interpreted under this harness — becomes Codex's least
// automatic posture; arbitrary corrupt values remain untouched so lifecycle
// validation rejects them instead of inventing authority.
//
// A BLANK policy is left blank. Blank is not a foreign input, it is the ABSENCE
// of one, and projecting it to `untrusted` turned "nothing was recorded" into a
// durably recorded posture strictly less capable than what current config
// resolves an unrecorded input to (`never`) — which then prompts on a detached
// pane and is denied the in-sandbox lineage bit the agent needs to delegate.
// Reconstruction re-resolves it instead; see harness.ReconstructApprovalPolicy
// and TCL-990.
func conservativeCodexApprovalProjection(policy string) (string, bool) {
	switch strings.TrimSpace(policy) {
	case "", "never", "untrusted", "on-failure", "on-request":
		return policy, true
	case "inherit", "auto", "default", "acceptEdits", "bypassPermissions", "plan", "delegate", "dontAsk":
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
	sessionIDs, err := sessionIDsByCreatedAt(q, strings.TrimSpace(convID))
	if err != nil {
		return err
	}
	if len(sessionIDs) == 0 {
		return nil
	}
	sessionID := sessionIDs[len(sessionIDs)-1]
	return projectSessionRelaunchProfilesTx(q, sessionID, relaunchProjectionOptions{
		RemoteControl: true, AutoMemory: true, ContextFeatures: true, AutoCompactWindow: true,
	})
}

// sessionIDsByCreatedAt provides exact chronological ordering on both sides of
// v181: historical callers expose RFC3339Nano text, current callers expose
// Unix-nanosecond INTEGERs. Parsing in Go avoids SQLite's inexact
// floating-point date conversion and uses rowid as the deterministic
// equal-instant tiebreaker.
func sessionIDsByCreatedAt(q dbExecQuerier, convID string) ([]string, error) {
	query := `SELECT id, rowid, created_at FROM sessions`
	var args []any
	if convID != "" {
		query += ` WHERE conv_id = ?`
		args = append(args, convID)
	}
	rows, err := q.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type sessionStamp struct {
		id    string
		rowID int64
		at    time.Time
	}
	var stamps []sessionStamp
	for rows.Next() {
		var stamp sessionStamp
		var at migrationBridgeTimestamp
		if err := rows.Scan(&stamp.id, &stamp.rowID, &at); err != nil {
			return nil, fmt.Errorf("session created_at: %w", err)
		}
		stamp.at = at.Time()
		stamps = append(stamps, stamp)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(stamps, func(i, j int) bool {
		if !stamps[i].at.Equal(stamps[j].at) {
			return stamps[i].at.Before(stamps[j].at)
		}
		return stamps[i].rowID < stamps[j].rowID
	})
	ids := make([]string, len(stamps))
	for i := range stamps {
		ids[i] = stamps[i].id
	}
	return ids, nil
}

// execSessionUpdateAndProject applies an out-of-band session update and its
// durable projection atomically. The SQL text is compile-time caller-owned;
// values remain bound parameters.
func execSessionUpdateAndProject(sessionID string, opts relaunchProjectionOptions, stmt string, args ...any) error {
	return execSessionUpdateAndProjectTimed(sessionID, opts, nil, stmt, args...)
}

func execSessionUpdateAndProjectTimed(
	sessionID string,
	opts relaunchProjectionOptions,
	record func(ContextSnapshotWriteTiming),
	stmt string,
	args ...any,
) (err error) {
	started := time.Now()
	timing := ContextSnapshotWriteTiming{}
	defer func() {
		timing.Total = time.Since(started)
		if record != nil {
			record(timing)
		}
	}()
	stageStarted := time.Now()
	d, err := Open()
	timing.Open = time.Since(stageStarted)
	if err != nil {
		return err
	}
	stageStarted = time.Now()
	tx, err := d.Begin()
	timing.Begin = time.Since(stageStarted)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stageStarted = time.Now()
	if _, err := tx.Exec(stmt, args...); err != nil {
		timing.Update = time.Since(stageStarted)
		return err
	}
	timing.Update = time.Since(stageStarted)
	stageStarted = time.Now()
	if err := projectSessionRelaunchProfilesTx(tx, sessionID, opts); err != nil {
		timing.Projection = time.Since(stageStarted)
		return err
	}
	timing.Projection = time.Since(stageStarted)
	stageStarted = time.Now()
	err = tx.Commit()
	timing.Commit = time.Since(stageStarted)
	return err
}

// seedAgentRelaunchProfileFromSpawnConfigTx records only fields explicitly
// present in the historical request. Profile/group-resolved omissions remain
// nil (unknown) rather than being upgraded into authority by today's defaults.
func seedAgentRelaunchProfileFromSpawnConfigTx(q dbExecQuerier, agentID, raw string) error {
	var spawn struct {
		HarnessBuiltinMode     *string            `json:"sandbox"`
		SandboxImplementation  *string            `json:"sandbox_implementation"`
		ApprovalPolicy         *string            `json:"approval"`
		ToolGovernance         *string            `json:"tools"`
		ApprovalAutoReview     *bool              `json:"auto_review"`
		ModelID                *string            `json:"model"`
		Effort                 *string            `json:"effort"`
		AskUserQuestionTimeout *string            `json:"ask_user_question_timeout"`
		RemoteControl          *bool              `json:"remote_control"`
		AutoMemory             *bool              `json:"auto_memory"`
		SSHWorkaround          *bool              `json:"ssh_workaround"`
		ContextFeatures        *map[string]string `json:"context_features"`
	}
	if err := json.Unmarshal([]byte(raw), &spawn); err != nil {
		return nil // audit JSON was historically best-effort; leave it unknown
	}
	p := AgentRelaunchProfile{
		Version:                RelaunchProfileVersion,
		HarnessBuiltinMode:     spawn.HarnessBuiltinMode,
		SandboxImplementation:  spawn.SandboxImplementation,
		ApprovalPolicy:         spawn.ApprovalPolicy,
		ToolGovernance:         spawn.ToolGovernance,
		ApprovalAutoReview:     spawn.ApprovalAutoReview,
		ModelID:                spawn.ModelID,
		Effort:                 spawn.Effort,
		AskUserQuestionTimeout: spawn.AskUserQuestionTimeout,
		RemoteControl:          spawn.RemoteControl,
		AutoMemory:             spawn.AutoMemory,
		SSHWorkaround:          spawn.SSHWorkaround,
		ContextFeatures:        spawn.ContextFeatures,
	}
	encoded, err := encodeRelaunchProfile(p)
	if err != nil {
		return err
	}
	_, err = q.Exec(`UPDATE agents SET relaunch_profile = ?
		WHERE agent_id = ? AND relaunch_profile = ''`, encoded, agentID)
	return err
}
