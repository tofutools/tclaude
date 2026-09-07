package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestGroupCreateStoresEnvironment(t *testing.T) {
	f := newFlow(t)
	r := agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/groups", map[string]any{
		"name": "environment-team",
		"environment": []map[string]string{
			{"name": "ZETA", "value": "last"},
			{"name": "ALPHA", "value": "first"},
		},
	}))
	rec := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	group, err := db.GetAgentGroupByName("environment-team")
	require.NoError(t, err)
	require.NotNil(t, group)
	assert.Equal(t, []sandboxpolicy.EnvironmentEntry{
		{Name: "ALPHA", Value: "first"},
		{Name: "ZETA", Value: "last"},
	}, group.Environment)
}

func TestGroupCreateRejectsInvalidEnvironmentBeforeInsert(t *testing.T) {
	f := newFlow(t)
	r := agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/groups", map[string]any{
		"name":        "invalid-environment-team",
		"environment": []map[string]string{{"name": "NOT VALID", "value": "nope"}},
	}))
	rec := testharness.Serve(f.Mux, r)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())

	group, err := db.GetAgentGroupByName("invalid-environment-team")
	require.NoError(t, err)
	assert.Nil(t, group)
}
