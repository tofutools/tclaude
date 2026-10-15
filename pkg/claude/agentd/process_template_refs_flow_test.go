package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A subprocess step that references a stored template gets that template's
// problems at edit time, attributed to the ▣ node: an unloadable reference,
// in-ports the stored template does not declare or leaves unbound.
func TestProcessValidateAttributesTemplateRefProblemsToTheSubprocessNode(t *testing.T) {
	f, _ := processAuthoringFlow(t)
	child := "apiVersion: tclaude.dev/v1alpha1\nkind: ProcessTemplate\nid: child\nstart: s\n" +
		"params:\n  target:\n    type: string\n" +
		"nodes:\n  s:\n    type: start\n    next:\n      pass: done\n  done:\n    type: end\n"
	save := processTemplateRequest(t, f, http.MethodPost, "/v1/process/templates/child", map[string]any{"source": child})
	require.Equal(t, http.StatusCreated, save.Code, save.Body.String())

	// A stored template may itself reference a template that is gone.
	middle := "apiVersion: tclaude.dev/v1alpha1\nkind: ProcessTemplate\nid: middle\nstart: s\n" +
		"nodes:\n  s:\n    type: start\n    next:\n      pass: g\n" +
		"  g:\n    type: subprocess\n    subprocess:\n      templateRef: ghost\n    next:\n      pass: done\n  done:\n    type: end\n"
	save = processTemplateRequest(t, f, http.MethodPost, "/v1/process/templates/middle", map[string]any{"source": middle})
	require.Equal(t, http.StatusCreated, save.Code, save.Body.String())

	parent := "apiVersion: tclaude.dev/v1alpha1\nkind: ProcessTemplate\nid: parent\nstart: begin\n" +
		"nodes:\n" +
		"  begin:\n    type: start\n    next:\n      pass: a\n" +
		"  a:\n    type: subprocess\n    subprocess:\n      templateRef: child\n      inputs:\n        bogus: x\n    next:\n      pass: b\n" +
		"  b:\n    type: subprocess\n    subprocess:\n      templateRef: ghost\n    next:\n      pass: c\n" +
		"  c:\n    type: subprocess\n    subprocess:\n      templateRef: middle\n    next:\n      pass: done\n" +
		"  done:\n    type: end\n"
	rec := processTemplateRequest(t, f, http.MethodPost, "/v1/process/validate", map[string]any{"source": parent})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body struct {
		Diagnostics []struct {
			Scope, TargetID, Severity, Code, Message, TemplateRef string
		} `json:"diagnostics"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	byNode := map[string][]string{}
	for _, diag := range body.Diagnostics {
		require.Equal(t, "node", diag.Scope, "%+v", diag)
		assert.NotEmpty(t, diag.TemplateRef, "%+v", diag)
		byNode[diag.TargetID] = append(byNode[diag.TargetID], diag.Code+": "+diag.Message)
	}
	require.Len(t, byNode["b"], 1)
	assert.Equal(t, "subprocess_ref_unresolved: template ghost does not exist or has no saved version", byNode["b"][0])
	require.Len(t, byNode["c"], 1)
	assert.Regexp(t, `^subprocess_ref_unresolved: inside template middle: subprocess "g": resolve template "ghost": `, byNode["c"][0])
	assert.Contains(t, byNode["a"], `unknown_input: template child: in-port "bogus" is not a declared param of the subprocess body`)
	assert.Len(t, byNode["a"], 2, "the unbound required in-port is reported too: %v", byNode["a"])
}
