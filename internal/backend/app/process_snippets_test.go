package app_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
	backendsqlite "github.com/tofutools/tclaude/internal/backend/sqlite"
)

func TestProcessSnippetsPreserveFragmentsCASRetryReopenAndUnavailable(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := backendsqlite.Open(path)
	require.NoError(t, err)
	service := app.New(store, providers.NewRegistry())
	selection := json.RawMessage(`{"version":1,"nodes":[{"ID":"node_a","Kind":"task","Name":"Incomplete task"},{"ID":"node_b","Kind":"end"}],"edges":[{"From":"node_a","To":"node_b"}],"positions":{"node_a":{"X":42,"Y":71}}}`)
	req := app.ProcessSnippetRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "create-snippet"}, ID: "snippet_a", Action: "create", Name: "Reusable fragment", Selection: selection}
	unauthorized := req
	unauthorized.Context.Principal = model.AgentPrincipal("agent_a")
	_, err = service.WriteProcessSnippet(ctx, unauthorized)
	require.ErrorIs(t, err, app.ErrUnauthorized)
	first, err := service.WriteProcessSnippet(ctx, req)
	require.NoError(t, err)
	require.True(t, first.Available)
	repeat, err := service.WriteProcessSnippet(ctx, req)
	require.NoError(t, err)
	require.Equal(t, first, repeat)
	changed := req
	changed.Name = "Changed"
	_, err = service.WriteProcessSnippet(ctx, changed)
	require.ErrorIs(t, err, app.ErrConflict)
	rename := app.ProcessSnippetRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "rename-snippet"}, ID: req.ID, Action: "rename", Name: "Renamed", ExpectedRevision: 1}
	renamed, err := service.WriteProcessSnippet(ctx, rename)
	require.NoError(t, err)
	require.Equal(t, model.Revision(2), renamed.Revision)
	require.JSONEq(t, string(first.Selection), string(renamed.Selection))
	stale := rename
	stale.Context.RequestID = "stale"
	_, err = service.WriteProcessSnippet(ctx, stale)
	require.ErrorIs(t, err, app.ErrConflict)
	require.NoError(t, store.Close())
	store, err = backendsqlite.Open(path)
	require.NoError(t, err)
	service = app.New(store, providers.NewRegistry())
	defer func() { _ = store.Close() }()
	items, err := service.ListProcessSnippets(ctx, model.OperatorPrincipal())
	require.NoError(t, err)
	require.Len(t, items, 1)
	require.Equal(t, renamed, items[0])
	_, err = service.ListProcessSnippets(ctx, model.AgentPrincipal("agent_a"))
	require.ErrorIs(t, err, app.ErrUnauthorized)
	// A corrupt retained envelope is manageable but never supplied for insertion.
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	defer db.Close()
	_, err = db.Exec(`UPDATE process_snippets SET selection='not JSON' WHERE id=?`, req.ID)
	require.NoError(t, err)
	items, err = service.ListProcessSnippets(ctx, model.OperatorPrincipal())
	require.NoError(t, err)
	require.False(t, items[0].Available)
	require.Empty(t, items[0].Selection)
	rename.ExpectedRevision = 2
	rename.Context.RequestID = "rename-unavailable"
	rename.Name = "Unavailable retained"
	_, err = service.WriteProcessSnippet(ctx, rename)
	require.NoError(t, err)
	del := app.ProcessSnippetRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "delete-snippet"}, ID: req.ID, Action: "delete", ExpectedRevision: 3}
	deleted, err := service.WriteProcessSnippet(ctx, del)
	require.NoError(t, err)
	require.True(t, deleted.Deleted)
	repeat, err = service.WriteProcessSnippet(ctx, del)
	require.NoError(t, err)
	require.Equal(t, deleted, repeat)
	items, err = service.ListProcessSnippets(ctx, model.OperatorPrincipal())
	require.NoError(t, err)
	require.Empty(t, items)
	// Durable create replay reports its original admission without resurrecting it.
	repeat, err = service.WriteProcessSnippet(ctx, req)
	require.NoError(t, err)
	require.Equal(t, first, repeat)
	items, err = service.ListProcessSnippets(ctx, model.OperatorPrincipal())
	require.NoError(t, err)
	require.Empty(t, items)
	for _, table := range []string{"executions", "work_runs", "operations", "authority_grants"} {
		var count int
		require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM "+table).Scan(&count))
		require.Zero(t, count)
	}
}
func TestProcessSelectionRejectsInvalidFragments(t *testing.T) {
	for _, raw := range []string{`null`, `{"version":2,"nodes":[{"ID":"n","Kind":"end"}]}`, `{"version":1,"nodes":[{"ID":"n","Kind":"end"},{"ID":"n","Kind":"end"}]}`, `{"version":1,"nodes":[{"ID":"n","Kind":"end"}],"edges":[{"From":"n","To":"missing"}]}`, `{"version":1,"nodes":[{"ID":"n","Kind":"end"}],"positions":{"missing":{"X":0,"Y":0}}}`, `{"version":1,"nodes":[{"ID":"n","Kind":"unknown"}]}`, `{"version":1,"nodes":[{"ID":"n","Kind":"end","Secret":"not supported"}]}`} {
		require.ErrorIs(t, app.ValidateProcessSelection(json.RawMessage(raw)), app.ErrInvalid, raw)
	}
}

func TestProcessSnippetCanonicalizesAcceptedKeysOnWriteAndRead(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := backendsqlite.Open(path)
	require.NoError(t, err)
	defer store.Close()
	service := app.New(store, providers.NewRegistry())
	raw := json.RawMessage(`{"Version":1,"Nodes":[{"ID":"discarded","id":"node_a","kind":"end"}],"Edges":[],"Positions":{"node_a":{"x":0,"y":0}}}`)
	req := app.ProcessSnippetRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "canonical"}, ID: "snippet_canonical", Action: "create", Name: "Canonical", Selection: raw}
	saved, err := service.WriteProcessSnippet(ctx, req)
	require.NoError(t, err)
	require.True(t, saved.Available)
	var wire map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(saved.Selection, &wire))
	require.Contains(t, wire, "nodes")
	require.NotContains(t, wire, "Nodes")
	var nodes []map[string]any
	require.NoError(t, json.Unmarshal(wire["nodes"], &nodes))
	require.Equal(t, "node_a", nodes[0]["ID"])
	require.Equal(t, "end", nodes[0]["Kind"])
	require.NotContains(t, nodes[0], "id")
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	defer db.Close()
	_, err = db.Exec(`UPDATE process_snippets SET selection=? WHERE id=?`, []byte(raw), req.ID)
	require.NoError(t, err)
	listed, err := service.ListProcessSnippets(ctx, model.OperatorPrincipal())
	require.NoError(t, err)
	require.Len(t, listed, 1)
	require.JSONEq(t, string(saved.Selection), string(listed[0].Selection))
}

func TestProcessSnippetLegacyReceiptRetriesAfterReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := backendsqlite.Open(path)
	require.NoError(t, err)
	service := app.New(store, providers.NewRegistry())
	raw := json.RawMessage(`{"Version":1,"Nodes":[{"id":"node_a","kind":"end"}],"Edges":[],"Positions":{}}`)
	req := app.ProcessSnippetRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "legacy"}, ID: "snippet_legacy", Action: "create", Name: "Legacy", Selection: raw}
	saved, err := service.WriteProcessSnippet(ctx, req)
	require.NoError(t, err)
	legacyIntent, err := json.Marshal(struct {
		ID, Action, Name string
		Selection        json.RawMessage
		Revision         model.Revision
	}{req.ID, req.Action, req.Name, raw, 0})
	require.NoError(t, err)
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE process_snippet_requests SET intent=? WHERE request_id=?`, legacyIntent, req.Context.RequestID)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	require.NoError(t, store.Close())
	store, err = backendsqlite.Open(path)
	require.NoError(t, err)
	defer store.Close()
	service = app.New(store, providers.NewRegistry())
	replay, err := service.WriteProcessSnippet(ctx, req)
	require.NoError(t, err)
	require.Equal(t, saved, replay)
	req.Name = "Changed"
	_, err = service.WriteProcessSnippet(ctx, req)
	require.ErrorIs(t, err, app.ErrConflict)
}
