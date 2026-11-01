package agentd_test

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/paf"
	"github.com/tofutools/tclaude/pkg/pwf"
	"github.com/tofutools/tclaude/pkg/testharness"
)

const pwfMemberConv = "qa00-1111-2222-3333-4444"

// pwfAssigneeFlow stores a root that pins a child version whose step names an
// agent holding the role qa here, instead of asking for a profile.
func pwfAssigneeFlow(t *testing.T) (*testharness.Flow, string) {
	t.Helper()
	return pwfAssigneeFlowWith(t, "assignee: "+pwfMemberConv+"\n")
}

// pwfAssigneeFlowWith puts selector in place of the child step's profile.
func pwfAssigneeFlowWith(t *testing.T, selector string) (*testharness.Flow, string) {
	t.Helper()
	f, _ := processAuthoringFlow(t)
	f.HaveGroup("webshop")
	f.HaveMemberWithRole("webshop", pwfMemberConv, "qa")
	child := strings.Replace(pwfChildSource, "profile: reviewer\n", selector, 1)
	rec := processTemplateRequest(t, f, http.MethodPost, "/v1/process/templates/pwf-review", map[string]any{"source": child})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	_, childRef, err := pwf.ParseTemplate([]byte(child))
	require.NoError(t, err)
	_, version, _ := strings.Cut(childRef, "@")
	root := strings.Replace(pwfRootSource, "templateRef: pwf-review\n", "templateRef: pwf-review\n      version: "+version+"\n", 1)
	rec = processTemplateRequest(t, f, http.MethodPost, "/v1/process/templates/pwf-ship", map[string]any{"source": root})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	return f, childRef
}

func pwfExportOpen(t *testing.T, f *testharness.Flow, query string) *pwf.Archive {
	t.Helper()
	rec := pwfRaw(t, f, http.MethodGet, "/v1/process/templates/pwf-ship/export"+query, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	file := rec.Body.Bytes()
	a, err := pwf.Open(bytes.NewReader(file), int64(len(file)), paf.Limits{})
	require.NoError(t, err)
	return a
}

// check says which role each named agent would become, the export uses it
// only when asked, and the root's pin follows the rewritten child. The stored
// templates do not change.
func TestProcessWorkflowExportConvertsAssigneesToRoles(t *testing.T) {
	f, childRef := pwfAssigneeFlow(t)

	check := pwfRaw(t, f, http.MethodGet, "/v1/process/templates/pwf-ship/export?check=1", nil)
	require.Equal(t, http.StatusOK, check.Code, check.Body.String())
	var view struct {
		Assignees []struct {
			Name, Role string
			Steps      []string
		}
	}
	testharness.DecodeJSON(t, check, &view)
	require.Len(t, view.Assignees, 1)
	assert.Equal(t, pwfMemberConv, view.Assignees[0].Name)
	assert.Equal(t, "qa", view.Assignees[0].Role)

	asWritten := pwfExportOpen(t, f, "")
	assert.Len(t, asWritten.Workflow.Assignees, 1, "without being asked an export keeps the assignee")

	for _, query := range []string{"?assignees=roles", "?assignee=" + pwfMemberConv + "=qa"} {
		a := pwfExportOpen(t, f, query)
		assert.Empty(t, a.Workflow.Assignees, query)
		assert.Empty(t, a.Problems, "the pin follows the rewritten child: %s", query)
		assert.Empty(t, a.Paths, query)
		assert.Contains(t, a.Workflow.Roles, pwf.Use{Name: "qa", Steps: []string{"pwf-review:review"}}, query)
		assert.Nil(t, a.Templates[childRef], "the child's ref changed: %s", query)
	}

	assert.Len(t, pwfExportOpen(t, f, "").Workflow.Assignees, 1, "the stored template is unchanged")

	titled := pwfExportOpen(t, f, "?assignees=roles&title=Shipping&summary=Ships%20it&author=Acme&licence=MIT")
	assert.Equal(t, "Shipping", titled.Workflow.Title, "conversion keeps the exporter's text")
	assert.Equal(t, "Ships it", titled.Workflow.Summary)
	assert.Equal(t, "Acme", titled.Workflow.Author)
	assert.Equal(t, "MIT", titled.Workflow.Licence)

	bad := pwfRaw(t, f, http.MethodGet, "/v1/process/templates/pwf-ship/export?assignee=nobody=qa", nil)
	assert.Equal(t, http.StatusBadRequest, bad.Code, bad.Body.String())
}

// A step that has a profile already says who can take it. The agent's role
// would narrow that, so it only loses the name, unless a role is asked for.
func TestProcessWorkflowExportKeepsAStepsProfile(t *testing.T) {
	f, _ := pwfAssigneeFlowWith(t, "profile: reviewer\n      assignee: "+pwfMemberConv+"\n")
	reviewer := pwf.Use{Name: "reviewer", Steps: []string{"pwf-review:review"}}

	held := pwfExportOpen(t, f, "?assignees=roles")
	assert.Empty(t, held.Workflow.Assignees)
	assert.Contains(t, held.Workflow.Roles, reviewer)
	assert.NotContains(t, held.Workflow.Roles, pwf.Use{Name: "qa", Steps: reviewer.Steps})

	chosen := pwfExportOpen(t, f, "?assignee="+pwfMemberConv+"=qa")
	assert.Empty(t, chosen.Workflow.Assignees)
	assert.Contains(t, chosen.Workflow.Roles, pwf.Use{Name: "qa", Steps: reviewer.Steps})
}

// An agent with several roles has no obvious one: it stays named.
func TestProcessWorkflowExportKeepsAnAgentWithSeveralRoles(t *testing.T) {
	f, _ := pwfAssigneeFlow(t)
	f.HaveGroup("infra")
	f.HaveMemberWithRole("infra", pwfMemberConv, "ops")
	assert.Len(t, pwfExportOpen(t, f, "?assignees=roles").Workflow.Assignees, 1)
}

// strict refuses what is not portable and says all of it, and passes once
// the assignee has a role.
func TestProcessWorkflowExportStrict(t *testing.T) {
	f, _ := pwfAssigneeFlow(t)

	rec := pwfRaw(t, f, http.MethodGet, "/v1/process/templates/pwf-ship/export?strict=1", nil)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "process_export_not_portable")
	assert.Contains(t, rec.Body.String(), "pwf-review:review names agent")

	pwfExportOpen(t, f, "?strict=1&assignees=roles")

	source := strings.Replace(pwfRootSource, "run: gh", "run: /home/alice/bin/gh", 1)
	put := processTemplateRequest(t, f, http.MethodPost, "/v1/process/templates/pwf-paths", map[string]any{"source": strings.Replace(source, "id: pwf-ship", "id: pwf-paths", 1)})
	require.Equal(t, http.StatusCreated, put.Code, put.Body.String())
	rec = pwfRaw(t, f, http.MethodGet, "/v1/process/templates/pwf-paths/export?strict=1", nil)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "/home/alice/bin/gh (reveals a username)")
}
