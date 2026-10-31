package agentd_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/federation/bundletransfer"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/paf"
	"github.com/tofutools/tclaude/pkg/testharness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

const lossText = "must-stay-on-this-node"

// storeConversionReport records that conv came out of a Codex to Claude
// translation, with free text in every place a report can hold it.
func storeConversionReport(t *testing.T, conv string) {
	t.Helper()
	item := paf.LossItem{Kind: "unknown_record", Severity: "warn", Count: 2, Detail: lossText}
	raw, err := json.Marshal(paf.Losses{
		Source: "codex", Target: "claude", ContextWindowSource: lossText, Items: []paf.LossItem{item},
		Prior: []paf.Losses{{Source: "pi", Target: "codex", ContextWindowSource: lossText, Items: []paf.LossItem{item}}},
	})
	require.NoError(t, err)
	require.NoError(t, db.SetConvLossReport(conv, raw))
}

func storedReport(t *testing.T, conv string) *paf.Losses {
	t.Helper()
	raw, err := db.ConvLossReport(conv)
	require.NoError(t, err)
	if raw == nil {
		return nil
	}
	l := &paf.Losses{}
	require.NoError(t, json.Unmarshal(raw, l))
	return l
}

func archiveLosses(t *testing.T, raw []byte) *paf.Losses {
	t.Helper()
	a, err := paf.Open(bytes.NewReader(raw), int64(len(raw)), paf.Options{Purpose: paf.PurposeInspect})
	require.NoError(t, err)
	l, ok := a.Losses(a.Manifest.Agents[0])
	require.True(t, ok, "the archive carries a loss report")
	return l
}

// assertCountsOnly checks that the chain arrived and that no text came with it.
func assertCountsOnly(t *testing.T, l *paf.Losses) {
	t.Helper()
	require.NotNil(t, l)
	raw, err := json.Marshal(l)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), lossText)
	assert.Contains(t, string(raw), `"unknown_record"`)
	assert.Contains(t, string(raw), `"pi"`, "the earliest conversion is still on record")
}

func fedAdvertisePAF(t *testing.T, fh *fedHarness, cat proto.CatalogPayload) {
	t.Helper()
	cat.PAFVersions = []string{paf.Version}
	require.NoError(t, db.PutFederationCatalog(fh.peer.id.ID(), string(mustJSON(t, cat)), time.Now()))
}

// Scenario: an agent whose conversation came from a harness translation is
// cloned with its history, alone and as part of a group.
//
// Expected: each copy keeps the record of the conversions it inherited. A clone
// that starts fresh inherits nothing, so it has no record.
func TestClonesKeepTheLossChain(t *testing.T) {
	f := newFlow(t)
	const source = "0f0e0d0c-0b0a-4908-8706-050403020d01"
	seedClaudeConversation(t, f, source, "clone-src", testutil.CanonicalTempDir(t))
	f.HaveGroup("team")
	f.HaveMember("team", source)
	storeConversionReport(t, source)

	rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/agent/"+source+"/clone", map[string]any{})))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var clone testharness.CloneResp
	testharness.DecodeJSON(t, rec, &clone)
	copied := storedReport(t, clone.NewConv)
	require.NotNil(t, copied)
	assert.Equal(t, storedReport(t, source), copied)

	rec = testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/groups/team/clone", map[string]any{})))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var group cloneGroupResp
	testharness.DecodeJSON(t, rec, &group)
	require.NotEmpty(t, group.Members)
	for _, m := range group.Members {
		require.Empty(t, m.Error)
		assert.Equal(t, storedReport(t, source), storedReport(t, m.NewConv))
	}

	fresh := f.AsHuman().CloneFresh(source)
	assert.Nil(t, storedReport(t, fresh.NewConv))
}

// Scenario: an agent with a conversion record is shared with a peer that reads
// PAF.
//
// Expected: the offered archive carries the chain, with no text in it.
func TestFederation_AgentOfferSendsLossCountsOnly(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	f.HaveGroup("source")
	const source = "0f0e0d0c-0b0a-4908-8706-050403020d02"
	seedClaudeConversation(t, f, source, "share-src", testutil.CanonicalTempDir(t))
	f.HaveMember("source", source)
	storeConversionReport(t, source)
	fedAdvertisePAF(t, fh, proto.CatalogPayload{})
	require.NoError(t, db.GrantAgentPermissionWithScope(source, "agent.share", fmt.Sprintf(`{"peer":["%s/destination"]}`, p.id.ID()), "human"))

	rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/federation/share-agent", map[string]any{"peer": "bob", "group": "destination", "agent": "self", "history": true}), source))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	fedEventually(t, "agent offer", func() bool { return len(p.envelopes(proto.KindBundleOffer)) >= 1 })
	var d bundletransfer.Descriptor
	require.NoError(t, p.envelopes(proto.KindBundleOffer)[0].DecodePayload(&d))
	require.NotEmpty(t, d.Inline)
	assertCountsOnly(t, archiveLosses(t, d.Inline))
}

// Scenario: an agent with a conversion record teleports to a peer that reads
// PAF. Moves and teleports build their archive where shares do.
//
// Expected: the offer carries the chain, with no text in it.
func TestFederation_TeleportSendsLossCountsOnly(t *testing.T) {
	fh := newFedHarness(t)
	fedMoveSource(t, fh)
	fedAdvertisePAF(t, fh, proto.CatalogPayload{AgentMoves: true, AgentTeleports: 1, Groups: []proto.CatalogGroup{}})
	storeConversionReport(t, moveSourceConv)
	require.NoError(t, db.GrantAgentPermissionWithScope(moveSourceConv, agentd.PermSelfTeleport, `{"peer":["`+fh.peer.id.ID()+`"]}`, "test"))

	rec := fedSelfTeleport(t, fh, map[string]any{"peer": "bob", "group": "receiver", "note": "Continue.", "credentials": "local"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	fedEventually(t, "teleport offer", func() bool { return len(fh.peer.envelopes(proto.KindBundleOffer)) >= 1 })
	var d bundletransfer.Descriptor
	require.NoError(t, fh.peer.envelopes(proto.KindBundleOffer)[0].DecodePayload(&d))
	require.NotNil(t, d.Teleport)
	require.NotEmpty(t, d.Inline)
	assertCountsOnly(t, archiveLosses(t, d.Inline))
}

// Scenario: a peer offers an agent whose PAF carries a loss chain, including
// free text a sender on an earlier release would still write.
//
// Expected: the landed conversation keeps the chain and drops the text, and an
// export from here passes the chain on.
func TestFederation_AgentOfferLandsLossChain(t *testing.T) {
	fh := newFedHarness(t)
	f := fh.f
	f.HaveGroup("receiver")
	fedReceiveAgents(t, fh, "receiver")
	const source = "0f0e0d0c-0b0a-4908-8706-050403020d03"
	seedClaudeConversation(t, f, source, "offer-src", testutil.CanonicalTempDir(t))
	storeConversionReport(t, source)
	rec := profileReq(t, f, http.MethodGet, "/v1/agent-bundle/export?agent="+source+"&history=true&format=paf", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	raw := rec.Body.Bytes()
	require.Contains(t, string(mustJSON(t, archiveLosses(t, raw))), lossText, "a local export keeps its text")

	d := bundletransfer.New(bundletransfer.Agent, raw, "Shared agent", time.Now().Add(time.Hour))
	d.Group = "receiver"
	env := fh.peer.envelope(proto.KindBundleOffer, proto.Endpoint{}, d)
	env.ID = d.ID
	fh.peer.send(env)
	require.Equal(t, proto.AckAccepted, fedAckFor(t, fh.peer, d.ID).Status)
	rec = fedHuman(t, f, http.MethodPost, "/v1/federation/bundle-offers/"+d.ID+"/import", map[string]any{"cwd": testutil.CanonicalTempDir(t), "apply": true, "name": "received"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var result struct {
		Spawn struct {
			ConvID string `json:"conv_id"`
		} `json:"spawn"`
	}
	testharness.DecodeJSON(t, rec, &result)
	assertCountsOnly(t, storedReport(t, result.Spawn.ConvID))
}

// Scenario: an agent keeps writing its transcript while it is exported, so an
// export can read its last record half written.
//
// Expected: every export succeeds and stops at the last complete record.
func TestAgentExportWhileTheTranscriptIsBeingWritten(t *testing.T) {
	f := newFlow(t)
	const source = "0f0e0d0c-0b0a-4908-8706-050403020d04"
	seedClaudeConversation(t, f, source, "busy-src", testutil.CanonicalTempDir(t))
	cc := f.World.CCs.GetByLabel("busy-src")
	require.NotNil(t, cc)
	h := mustGetHarness(t, harness.DefaultName)
	export := func() []byte {
		t.Helper()
		rec := profileReq(t, f, http.MethodGet, "/v1/agent-bundle/export?agent="+source+"&history=true", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		b, err := decodeAgentExport(t, rec.Body.Bytes())
		require.NoError(t, err)
		require.NoError(t, h.History.Validate(b.Transcript, source))
		return b.Transcript
	}
	file, err := os.OpenFile(cc.JsonlPath, os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	defer func() { _ = file.Close() }()
	record := func(i int) string {
		return fmt.Sprintf(`{"type":"user","uuid":"late-%d","sessionId":%q,"message":{"role":"user","content":"late turn %d"}}`, i, source, i)
	}

	// The fragment is on disk for the whole of this export.
	first := record(0)
	_, err = file.WriteString(first[:len(first)/2])
	require.NoError(t, err)
	assert.NotContains(t, string(export()), "late-0")
	_, err = file.WriteString(first[len(first)/2:] + "\n")
	require.NoError(t, err)
	assert.Contains(t, string(export()), "late-0")

	// And here it comes and goes while exports run.
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 1; ; i++ {
			select {
			case <-done:
				return
			default:
			}
			line := record(i)
			_, _ = file.WriteString(line[:len(line)/2])
			time.Sleep(time.Millisecond)
			_, _ = file.WriteString(line[len(line)/2:] + "\n")
		}
	}()
	defer func() { close(done); wg.Wait() }()
	for range 20 {
		export()
	}
}
