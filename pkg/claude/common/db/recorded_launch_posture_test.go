package db

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// populatedRelaunchProfile fills every pointer field with a value derived from
// seed, so two profiles built with different seeds differ in every field.
func populatedRelaunchProfile(seed string) AgentRelaunchProfile {
	p := AgentRelaunchProfile{Version: RelaunchProfileVersion}
	v := reflect.ValueOf(&p).Elem()
	for i := range v.NumField() {
		field := v.Field(i)
		if field.Kind() != reflect.Pointer {
			continue
		}
		elem := reflect.New(field.Type().Elem())
		switch elem.Elem().Kind() {
		case reflect.String:
			elem.Elem().SetString(seed)
		case reflect.Bool:
			elem.Elem().SetBool(seed == "a")
		case reflect.Int64:
			// seed[0], not len(seed): every seed this test uses is one character,
			// so a length-derived value would make base and overlay IDENTICAL for
			// the only *int64 field and the coverage assertion would pass
			// vacuously — verified by mutation, deleting the ContextWindowSize
			// branch from the overlay left the whole suite green.
			elem.Elem().SetInt(int64(seed[0]))
		case reflect.Map:
			elem.Elem().Set(reflect.ValueOf(map[string]string{seed: "off"}))
		default:
			panic("populatedRelaunchProfile: unhandled field kind " + elem.Elem().Kind().String())
		}
		field.Set(elem)
	}
	return p
}

// TestComposeAgentRelaunchProfileCoversEveryField is the structural guard on the
// overlay: every pointer field must be overlayable, and a nil field must retain
// base's value. A field left out of ComposeAgentRelaunchProfile would silently
// become un-overridable — durable agent intent would stop winning over the
// conversation fallback for that one parameter, which is exactly the kind of
// per-field drift TCL-730 was made of.
func TestComposeAgentRelaunchProfileCoversEveryField(t *testing.T) {
	base := populatedRelaunchProfile("a")
	overlay := populatedRelaunchProfile("b")
	baseValue := reflect.ValueOf(base)
	overlayValue := reflect.ValueOf(overlay)
	profileType := reflect.TypeOf(AgentRelaunchProfile{})

	for i := range profileType.NumField() {
		name := profileType.Field(i).Name
		if baseValue.Field(i).Kind() != reflect.Pointer {
			continue
		}
		single := AgentRelaunchProfile{Version: RelaunchProfileVersion}
		reflect.ValueOf(&single).Elem().Field(i).Set(overlayValue.Field(i))

		merged := reflect.ValueOf(*ComposeAgentRelaunchProfile(&base, &single))
		assert.Equal(t, overlayValue.Field(i).Interface(), merged.Field(i).Interface(),
			"ComposeAgentRelaunchProfile does not overlay %s", name)
		for j := range profileType.NumField() {
			if j == i || baseValue.Field(j).Kind() != reflect.Pointer {
				continue
			}
			assert.Equal(t, baseValue.Field(j).Interface(), merged.Field(j).Interface(),
				"overlaying %s must leave %s alone", name, profileType.Field(j).Name)
		}
	}

	assert.Equal(t, &base, ComposeAgentRelaunchProfile(&base, nil), "a nil overlay changes nothing")
	assert.Equal(t, &overlay, ComposeAgentRelaunchProfile(nil, &overlay), "a nil base yields the overlay")
}

// TestRecordedLaunchPostureForConv_UnrecordedFieldsStayUnknown is the tri-state
// contract at its source: a conversation that never recorded a field must report
// nil for it, NOT the zero value. A caller that cannot tell those apart writes
// the zero back as asserted intent, and the original posture is then gone for
// good (TCL-730).
func TestRecordedLaunchPostureForConv_UnrecordedFieldsStayUnknown(t *testing.T) {
	setupTestDB(t)
	const convID = "posture-partial-conv"
	require.NoError(t, SetConversationResumeProfile(convID, ConversationResumeProfile{
		Version: RelaunchProfileVersion, Harness: DefaultHarness, Cwd: "/tmp/posture",
		FallbackRelaunch: &AgentRelaunchProfile{
			Version: RelaunchProfileVersion, AutoCompactWindow: stringPtr("450000"),
		},
	}))

	posture, err := RecordedLaunchPostureForConv(convID)
	require.NoError(t, err)
	require.NotNil(t, posture)
	require.NotNil(t, posture.AutoCompactWindow)
	assert.Equal(t, "450000", *posture.AutoCompactWindow)
	assert.Nil(t, posture.AutoMemory, "a field that was never recorded must read as unknown, not false")
	assert.Nil(t, posture.ContextFeatures)
	assert.Nil(t, posture.AskUserQuestionTimeout)
}

// TestRecordedLaunchPostureForConv_NoRecordAtAll pins that an unknown
// conversation reports nothing rather than a profile full of defaults.
func TestRecordedLaunchPostureForConv_NoRecordAtAll(t *testing.T) {
	setupTestDB(t)
	posture, err := RecordedLaunchPostureForConv("never-seen-conv")
	require.NoError(t, err)
	assert.Nil(t, posture)
}

// TestRecordedLaunchPostureForConv_AgentIntentWinsFieldByField pins the tier
// order: durable agent intent overlays the conversation fallback per field, so
// an agent that pinned one parameter does not lose the rest.
func TestRecordedLaunchPostureForConv_AgentIntentWinsFieldByField(t *testing.T) {
	setupTestDB(t)
	const convID = "posture-agent-conv"
	agentID, _, err := EnsureAgentForConv(convID, "test")
	require.NoError(t, err)
	require.NoError(t, SetConversationResumeProfile(convID, ConversationResumeProfile{
		Version: RelaunchProfileVersion, Harness: DefaultHarness, Cwd: "/tmp/posture",
		FallbackRelaunch: &AgentRelaunchProfile{
			Version:                RelaunchProfileVersion,
			AutoCompactWindow:      stringPtr("450000"),
			AskUserQuestionTimeout: stringPtr("5m"),
		},
	}))
	require.NoError(t, SetAgentRelaunchProfile(agentID, AgentRelaunchProfile{
		Version: RelaunchProfileVersion, AutoCompactWindow: stringPtr("200000"),
	}))

	posture, err := RecordedLaunchPostureForConv(convID)
	require.NoError(t, err)
	require.NotNil(t, posture)
	require.NotNil(t, posture.AutoCompactWindow)
	assert.Equal(t, "200000", *posture.AutoCompactWindow, "agent intent must win")
	require.NotNil(t, posture.AskUserQuestionTimeout)
	assert.Equal(t, "5m", *posture.AskUserQuestionTimeout,
		"a field the agent profile does not mention must keep the conversation fallback's value")
}

// TestRecordedLaunchPostureForConv_LegacySessionRowIsTheWeakestTier pins that a
// pre-projection record still yields what its session row can prove, and only
// for fields the durable owners left unknown.
func TestRecordedLaunchPostureForConv_LegacySessionRowIsTheWeakestTier(t *testing.T) {
	setupTestDB(t)
	const (
		convID    = "posture-legacy-conv"
		sessionID = "posture-legacy-session"
	)
	require.NoError(t, SaveSession(&SessionRow{
		ID: sessionID, ConvID: convID, Cwd: "/tmp/posture-legacy", Status: "exited",
		Harness: DefaultHarness, SandboxMode: "on", AskUserQuestionTimeout: "10m",
	}))

	posture, err := RecordedLaunchPostureForConv(convID)
	require.NoError(t, err)
	require.NotNil(t, posture)
	require.NotNil(t, posture.SandboxMode)
	assert.Equal(t, "on", *posture.SandboxMode)
	require.NotNil(t, posture.AskUserQuestionTimeout)
	assert.Equal(t, "10m", *posture.AskUserQuestionTimeout)
	// The legacy tier reads plain columns, which cannot express "unknown", so a
	// zero column must NOT be reported as recorded intent.
	assert.Nil(t, posture.ContextFeatures, "a blank legacy column is unknown, not asserted intent")
}
