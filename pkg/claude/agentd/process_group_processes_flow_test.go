package agentd_test

import (
	"net/http"
	"os"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// tcl-8o4rjj: a process is saved to a group (pinned there, unlisted in the
// library unless published) or to the library; the group lists its saved
// processes with the start inputs a one-click start asks for. Pins and the
// listing are tclaude-side metadata and never enter template content.

type groupProcessView struct {
	TemplateID string `json:"templateId"`
	Name       string `json:"name"`
	Ref        string `json:"ref"`
	Listed     bool   `json:"listed"`
	Steps      int    `json:"steps"`
	Params     []struct {
		Name     string `json:"name"`
		Type     string `json:"type"`
		Required bool   `json:"required"`
		Default  any    `json:"default"`
	} `json:"params"`
	ProgramProfiles []string `json:"programProfiles"`
}

func shipFeatureTemplate(name string) *model.Template {
	optional := false
	return &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, Name: name, Start: "start",
		Params: map[string]model.Param{
			"issue":  {Type: "string", Description: "The issue to ship"},
			"branch": {Type: "string", Default: "main"},
			"notes":  {Type: "string", Required: &optional},
		},
		Nodes: map[string]model.Node{
			"start":     {Type: model.NodeTypeStart, Next: model.Next{"next": "implement"}},
			"implement": {Type: model.NodeTypeTask, Performer: &model.Performer{Kind: model.PerformerAgent, Profile: "dev", Prompt: "Implement {{issue}}."}, Next: model.Next{"next": "merge"}},
			"merge":     {Type: model.NodeTypeTask, Performer: &model.Performer{Kind: model.PerformerProgram, Profile: "ci", Run: "gh", Args: []string{"pr", "merge"}}, Next: model.Next{"next": "end"}},
			"end":       {Type: model.NodeTypeEnd, Result: "success"},
		},
	}
}

func TestProcessGroupProcesses_SaveToGroupPublishPinAndList(t *testing.T) {
	f, _ := processAuthoringFlow(t)
	f.HaveGroup("webshop")
	f.HaveGroup("infra")

	library := func(all bool) map[string]bool {
		t.Helper()
		path := "/v1/process/templates"
		if all {
			path += "?all=1"
		}
		rec := processTemplateRequest(t, f, http.MethodGet, path, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var body struct {
			Templates []struct {
				ID     string `json:"id"`
				Listed bool   `json:"listed"`
			} `json:"templates"`
		}
		testharness.DecodeJSON(t, rec, &body)
		out := map[string]bool{}
		for _, tmpl := range body.Templates {
			out[tmpl.ID] = tmpl.Listed
		}
		return out
	}
	saved := func(group string) []groupProcessView {
		t.Helper()
		rec := processTemplateRequest(t, f, http.MethodGet, "/v1/process/context", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var body struct {
			Groups []struct {
				Group     string             `json:"group"`
				Processes []groupProcessView `json:"processes"`
			} `json:"groups"`
		}
		testharness.DecodeJSON(t, rec, &body)
		for _, g := range body.Groups {
			if g.Group == group {
				return g.Processes
			}
		}
		t.Fatalf("group %q not in the context: %s", group, rec.Body.String())
		return nil
	}
	create := func(body map[string]any) string {
		t.Helper()
		rec := processTemplateRequest(t, f, http.MethodPost, "/v1/process/templates", body)
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		var created struct {
			ID string `json:"id"`
		}
		testharness.DecodeJSON(t, rec, &created)
		return created.ID
	}

	// "Save to webshop": a new template, unlisted, pinned to the group.
	ship := create(map[string]any{"template": shipFeatureTemplate("ship-feature"), "listed": false, "pinGroup": "webshop"})
	// "Start now" on an unsaved step list: unlisted and not pinned.
	oneOff := create(map[string]any{"template": shipFeatureTemplate("one-off"), "listed": false})
	// An ordinary library save, as before.
	nightly := create(map[string]any{"template": shipFeatureTemplate("nightly-check")})

	assert.Equal(t, map[string]bool{nightly: true}, library(false), "unlisted templates stay out of the library")
	assert.Equal(t, map[string]bool{ship: false, oneOff: false, nightly: true}, library(true))

	webshop := saved("webshop")
	require.Len(t, webshop, 1)
	assert.Equal(t, ship, webshop[0].TemplateID)
	assert.Equal(t, "ship-feature", webshop[0].Name)
	assert.False(t, webshop[0].Listed)
	assert.Equal(t, 2, webshop[0].Steps)
	assert.Equal(t, []string{"ci"}, webshop[0].ProgramProfiles, "a start must authorize the program profile")
	require.Len(t, webshop[0].Params, 3)
	required := map[string]bool{}
	for _, p := range webshop[0].Params {
		required[p.Name] = p.Required
	}
	assert.Equal(t, map[string]bool{"branch": false, "issue": true, "notes": false}, required,
		"only an input without a default that is not optional is asked for")
	assert.Empty(t, saved("infra"))

	// Group and listing are never template content.
	rec := processTemplateRequest(t, f, http.MethodGet, "/v1/process/templates/"+ship, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "webshop")
	var view struct {
		Source string `json:"source"`
		Listed *bool  `json:"listed"`
	}
	testharness.DecodeJSON(t, rec, &view)
	assert.NotContains(t, view.Source, "listed")
	require.NotNil(t, view.Listed)
	assert.False(t, *view.Listed)

	// Publish lists it in the library without a new version.
	rec = processTemplateRequest(t, f, http.MethodPost, "/v1/process/templates/"+ship+"/listing", map[string]any{"listed": true})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.True(t, library(false)[ship])
	assert.Equal(t, webshop[0].Ref, saved("webshop")[0].Ref, "publishing does not change the template")

	// Pin a library template to the group: it lists after the first, and the
	// pin is idempotent.
	for range 2 {
		rec = processTemplateRequest(t, f, http.MethodPost, "/v1/process/groups/webshop/processes", map[string]any{"templateId": nightly})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}
	webshop = saved("webshop")
	require.Len(t, webshop, 2)
	assert.Equal(t, []string{ship, nightly}, []string{webshop[0].TemplateID, webshop[1].TemplateID}, "pin order")

	rec = processTemplateRequest(t, f, http.MethodPost, "/v1/process/groups/webshop/processes", map[string]any{"templateId": "no-such-template"})
	assert.Equal(t, http.StatusNotFound, rec.Code)
	rec = processTemplateRequest(t, f, http.MethodPost, "/v1/process/groups/nope/processes", map[string]any{"templateId": nightly})
	assert.Equal(t, http.StatusNotFound, rec.Code)

	// Unpin; unpinning again is a 404.
	rec = processTemplateRequest(t, f, http.MethodDelete, "/v1/process/groups/webshop/processes/"+nightly, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = processTemplateRequest(t, f, http.MethodDelete, "/v1/process/groups/webshop/processes/"+nightly, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)

	// A save of an existing template can pin it too.
	rec = processTemplateRequest(t, f, http.MethodGet, "/v1/process/templates/"+oneOff, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var current struct {
		Template   *model.Template `json:"template"`
		SourceHash string          `json:"sourceHash"`
	}
	testharness.DecodeJSON(t, rec, &current)
	rec = processTemplateRequest(t, f, http.MethodPost, "/v1/process/templates/"+oneOff, map[string]any{
		"template": current.Template, "sourceHash": current.SourceHash, "pinGroup": "infra",
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	require.Len(t, saved("infra"), 1)
	assert.False(t, library(true)[oneOff], "a save without listed keeps the listing")

	// Deleting a template removes its pins.
	rec = processTemplateRequest(t, f, http.MethodDelete, "/v1/process/templates/"+ship, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Empty(t, saved("webshop"))

	// Behind features.processes like every process surface.
	require.NoError(t, os.WriteFile(config.ConfigPath(), []byte(`{"features":{"processes":false}}`), 0o644))
	rec = processTemplateRequest(t, f, http.MethodPost, "/v1/process/groups/infra/processes", map[string]any{"templateId": nightly})
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// Scenario: an agent may pin only to a group it is a member or owner of.
// Holding process.runs.read (implied for any group owner) shows every group's
// runs but does not make another group's saved processes its to change.
func TestProcessGroupProcesses_AgentPinsOnlyToItsOwnGroups(t *testing.T) {
	f, root := processAuthoringFlow(t)
	f.HaveGroup("webshop")
	ops := f.HaveGroup("ops")
	const member, owner = "memb-1111-2222-3333-4444", "ownr-1111-2222-3333-4444"
	f.HaveAliveSession(member, "spwn-memb", "tmux-memb", f.TestCwd("memb"))
	f.HaveMember("webshop", member)
	f.HaveAliveSession(owner, "spwn-ownr", "tmux-ownr", f.TestCwd("ownr"))
	f.HaveEnrolledAgent(owner)
	require.NoError(t, db.AddAgentGroupOwner(ops.ID, owner, "<test>"))
	for _, conv := range []string{member, owner} {
		require.NoError(t, db.SetAgentPermissionOverride(conv, agentd.PermProcessTemplatesManage, db.PermEffectGrant, "test"))
	}
	tmpl := shipFeatureTemplate("nightly")
	tmpl.ID = "nightly"
	putProcessRuntimeTemplate(t, root, tmpl)

	pin := func(conv, group string) int {
		t.Helper()
		return agentReq(t, f, conv, http.MethodPost, "/v1/process/groups/"+group+"/processes", map[string]any{"templateId": "nightly"}).Code
	}
	assert.Equal(t, http.StatusOK, pin(member, "webshop"), "a member pins to its group")
	assert.Equal(t, http.StatusOK, pin(owner, "ops"), "an owner pins to the group it owns")
	assert.Equal(t, http.StatusNotFound, pin(owner, "webshop"), "owning another group is not enough")
	assert.Equal(t, http.StatusNotFound, pin(member, "ops"))
	rec := agentReq(t, f, owner, http.MethodDelete, "/v1/process/groups/webshop/processes/nightly", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code, "nor may it unpin there")
}

// Scenario: "Save to group" on a full group is refused before anything is
// written, so no unlisted template is left behind.
func TestProcessGroupProcesses_FullGroupRefusesBeforeSaving(t *testing.T) {
	f, root := processAuthoringFlow(t)
	g := f.HaveGroup("webshop")
	for i := range db.MaxProcessGroupPins {
		tmpl := shipFeatureTemplate("filler")
		tmpl.ID = "filler-" + strconv.Itoa(i)
		putProcessRuntimeTemplate(t, root, tmpl)
		require.NoError(t, db.PinProcessToGroup(g.ID, tmpl.ID))
	}
	rec := processTemplateRequest(t, f, http.MethodPost, "/v1/process/templates",
		map[string]any{"template": shipFeatureTemplate("one-too-many"), "listed": false, "pinGroup": "webshop"})
	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	unlisted, err := db.ProcessTemplateUnlisted()
	require.NoError(t, err)
	assert.Empty(t, unlisted, "nothing was saved")
	rec = processTemplateRequest(t, f, http.MethodPost, "/v1/process/groups/webshop/processes", map[string]any{"templateId": "filler-0"})
	assert.Equal(t, http.StatusOK, rec.Code, "re-pinning a held template stays idempotent on a full group")
}
