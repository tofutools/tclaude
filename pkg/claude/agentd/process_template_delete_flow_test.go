package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
)

func TestProcessTemplateDeleteRemovesTemplate(t *testing.T) {
	f, root := processEngineFlow(t)
	fs, err := store.NewFS(root)
	require.NoError(t, err)
	_, err = fs.PutTemplate(t.Context(), processRESTTemplate("release", "v1", 20))
	require.NoError(t, err)

	rec := processTemplateRequest(t, f, http.MethodDelete, "/v1/process/templates/release", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var body struct {
		Deleted string `json:"deleted"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "release", body.Deleted)

	// The list read path is the user-visible surface; assert through it.
	listRec := processTemplateRequest(t, f, http.MethodGet, "/v1/process/templates", nil)
	require.Equal(t, http.StatusOK, listRec.Code, listRec.Body.String())
	var list struct {
		Templates []struct {
			ID string `json:"id"`
		} `json:"templates"`
	}
	require.NoError(t, json.Unmarshal(listRec.Body.Bytes(), &list))
	assert.Empty(t, list.Templates)
}

func TestProcessTemplateDeleteMissingIs404(t *testing.T) {
	f, _ := processEngineFlow(t)
	rec := processTemplateRequest(t, f, http.MethodDelete, "/v1/process/templates/absent", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
}

func TestProcessTemplateDeleteConflictsWithUnfinishedRun(t *testing.T) {
	f, root := processEngineFlow(t)
	fs, err := store.NewFS(root)
	require.NoError(t, err)
	tmpl := processRESTTemplate("release", "v1", 20)
	record, err := fs.PutTemplate(t.Context(), tmpl)
	require.NoError(t, err)
	initial := state.New("run-live", record.Ref, record.Ref, []state.NodeInit{{ID: "begin", Type: model.NodeTypeStart}})
	initial.Status = state.RunStatusRunning
	_, err = fs.CreateRun(t.Context(), store.RunRecord{ID: "run-live", TemplateRef: record.Ref}, initial)
	require.NoError(t, err)

	rec := processTemplateRequest(t, f, http.MethodDelete, "/v1/process/templates/release", nil)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())

	var body struct {
		Code   string   `json:"code"`
		RunIDs []string `json:"runIds"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "process_template_in_use", body.Code)
	assert.Equal(t, []string{"run-live"}, body.RunIDs)

	// The template must still be listed after a refused delete.
	listRec := processTemplateRequest(t, f, http.MethodGet, "/v1/process/templates", nil)
	require.Equal(t, http.StatusOK, listRec.Code)
	assert.Contains(t, listRec.Body.String(), `"release"`)
}

// A malformed id is a client error, not an apparent store fault.
func TestProcessTemplateDeleteRejectsInvalidID(t *testing.T) {
	f, _ := processEngineFlow(t)
	rec := processTemplateRequest(t, f, http.MethodDelete, "/v1/process/templates/Not%20An%20Id", nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
}

func TestProcessTemplateDelete404WhenFeatureOff(t *testing.T) {
	f := newFlow(t)
	rec := processTemplateRequest(t, f, http.MethodDelete, "/v1/process/templates/example", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
}
