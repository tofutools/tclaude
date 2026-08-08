package agent

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The deploy verbs must SHOW an operator when a member's launch was decided by a
// tier they did not type.
//
// These probe the real renderers rather than reading them, because reading is
// how this was missed: TCL-1090 put a per-agent disclosure on the deploy
// response and every one of these commands dropped it on the floor — the mirror
// struct did not declare the field, so the decoder discarded it and no human
// running a deploy ever saw one. The daemon-side guards passed the whole time,
// asserting on JSON nobody rendered.
//
// One test per verb on purpose. They share the per-agent block by copy, so a
// disclosure that reaches `templates instantiate` proves nothing about
// `templates reinforce` or `task-force deploy`.

// deployResponseWithAmbientDrive is a daemon response for one member whose
// Copilot drive came from the group's default profile — the acquisition the
// operator's standing rule says can never be silent.
//
// The shape is one a deploy really produces: the harness is credited to the
// template's REFERENCED profile (a deploy resolves its own harness chain and
// never reaches the group tier for that field — TCL-1110), while the drive comes
// from the group default the referenced profile left unset.
const deployResponseWithAmbientDrive = `{
	"group":"phoenix","template":"team","spawned":1,"failed":0,
	"agents":[{"name":"worker","final_name":"phoenix-worker","conv_id":"c1",
		"resolved":{
			"harness":{"value":"copilot","source":"profile \"copilot-kit\""},
			"model":{"value":"","source":"harness default"},
			"effort":{"value":"","source":"harness default"},
			"context_window_max":{"value":"","source":"harness default"},
			"copilot_api":{"value":"api","source":"group default profile \"copilot-api-on\""},
			"fast_mode":{"value":"","source":"harness default"},
			"sandbox_implementation":{"value":"","source":"harness default"},
			"notes":["group default profile \"codex-kit\" model ignored (not valid for copilot)"],
			"info":["the harness runs its own server confinement"],
			"warnings":["this agent runs commands unattended with no OS sandbox"]
		}}]}`

// assertAmbientDriveRendered pins what a human has to be able to read off the
// screen: the field, the value, and the tier they can go and change — and the
// three disclosure channels under labels that do not disguise one as another.
func assertAmbientDriveRendered(t *testing.T, out string) {
	t.Helper()
	assert.Contains(t, out, `copilot drive: api (group default profile "copilot-api-on")`,
		"the operator must see WHICH tier put this member on the unverified API drive")
	assert.Contains(t, out, `harness: copilot (profile "copilot-kit")`,
		"an inherited harness is the TCL-304 surprise the echo exists to prevent")
	assert.Contains(t, out, `note: group default profile "codex-kit" model ignored`,
		"resolution notes ride the same echo and must reach the screen with it")
	assert.Contains(t, out, "info: the harness runs its own server confinement")
	assert.Contains(t, out, "warning: this agent runs commands unattended with no OS sandbox",
		"a blast-radius warning must not be rendered as one more provenance footnote")
	assert.NotContains(t, out, "note: this agent runs commands unattended")
}

func TestRunTemplatesInstantiate_RendersAmbientLaunchProvenance(t *testing.T) {
	var calls []capturedReq
	stubDaemon(t, &calls, ok(deployResponseWithAmbientDrive))

	var stdout, stderr bytes.Buffer
	rc := runTemplatesInstantiate(&templatesInstantiateParams{Name: "team", Group: "phoenix"},
		nil, &stdout, &stderr)
	require.Equal(t, rcOK, rc, "stderr=%q", stderr.String())
	assertAmbientDriveRendered(t, stdout.String())
}

func TestRunTemplatesReinforce_RendersAmbientLaunchProvenance(t *testing.T) {
	var calls []capturedReq
	stubDaemon(t, &calls, ok(deployResponseWithAmbientDrive))

	var stdout, stderr bytes.Buffer
	rc := runTemplatesReinforce(&templatesReinforceParams{Name: "team", Group: "phoenix"},
		nil, &stdout, &stderr)
	require.Equal(t, rcOK, rc, "stderr=%q", stderr.String())
	assertAmbientDriveRendered(t, stdout.String())
}

func TestRunTaskForceDeploy_RendersAmbientLaunchProvenance(t *testing.T) {
	var calls []capturedReq
	stubDaemon(t, &calls, ok(deployResponseWithAmbientDrive))

	var stdout, stderr bytes.Buffer
	rc := runTaskForceDeploy(&taskForceDeployParams{Name: "team", Mission: "ship it", Group: "phoenix"},
		nil, &stdout, &stderr)
	require.Equal(t, rcOK, rc, "stderr=%q", stderr.String())
	assertAmbientDriveRendered(t, stdout.String())
}

// The other arm. A roster that inherited nothing prints nothing extra: an echo
// rendered unconditionally would satisfy every test above while burying the one
// line that matters under six that do not.
func TestRunTemplatesInstantiate_QuietWhenNothingAmbientDecided(t *testing.T) {
	var calls []capturedReq
	stubDaemon(t, &calls, ok(`{
		"group":"phoenix","template":"team","spawned":1,"failed":0,
		"agents":[{"name":"worker","final_name":"phoenix-worker","conv_id":"c1",
			"resolved":{
				"harness":{"value":"claude","source":"explicit"},
				"model":{"value":"sonnet","source":"explicit"},
				"effort":{"value":"","source":"harness default"},
				"copilot_api":{"value":"","source":"harness default"},
				"sandbox_implementation":{"value":"","source":"harness default"}
			}}]}`))

	var stdout, stderr bytes.Buffer
	rc := runTemplatesInstantiate(&templatesInstantiateParams{Name: "team", Group: "phoenix"},
		nil, &stdout, &stderr)
	require.Equal(t, rcOK, rc, "stderr=%q", stderr.String())
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	for _, line := range lines {
		assert.NotContains(t, line, "harness:",
			"a value the deployer typed needs no announcement: %q", line)
		assert.NotContains(t, line, "model:", "same for an explicit model: %q", line)
	}
	require.Len(t, lines, 2, "headline + one agent line, nothing else: %q", stdout.String())
}

// A member whose spawn was REFUSED still gets its provenance printed, because
// the tier that produced the refused value is what explains the refusal.
func TestRunTemplatesInstantiate_RendersProvenanceForAFailedMember(t *testing.T) {
	var calls []capturedReq
	stubDaemon(t, &calls, ok(`{
		"group":"phoenix","template":"team","spawned":0,"failed":1,
		"agents":[{"name":"worker","final_name":"phoenix-worker",
			"error":"profile \"kit\": invalid claude model \"gpt-5\"",
			"resolved":{"notes":["profile \"kit\" effort ignored (not valid for claude)"]}}]}`))

	var stdout, stderr bytes.Buffer
	rc := runTemplatesInstantiate(&templatesInstantiateParams{Name: "team", Group: "phoenix"},
		nil, &stdout, &stderr)
	require.Equal(t, rcIOFailure, rc, "a failed member is still a non-zero exit")
	assert.Contains(t, stdout.String(), `note: profile "kit" effort ignored`)
}
