package agentbundle

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testutil"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func fixtureBundle() *Bundle {
	return &Bundle{Manifest: Manifest{Format: Format, FormatVersion: 1, CreatedAt: "2026-10-08T00:00:00Z", TclaudeVersion: "test", Agent: Definition{Name: "worker", Harness: "claude", Profile: json.RawMessage(`{}`)}}}
}
func TestArchiveRoundTripAndIntegrity(t *testing.T) {
	b := fixtureBundle()
	b.SetHistory("claude-jsonl", "source", []byte("history\n"))
	raw, err := b.Encode()
	require.NoError(t, err)
	got, err := Decode(raw)
	require.NoError(t, err)
	assert.Equal(t, b, got)
	b.Transcript = []byte("tampered")
	_, err = b.Encode()
	require.ErrorContains(t, err, "checksum")
	b = fixtureBundle()
	b.Manifest.FormatVersion = 42
	_, err = b.Encode()
	require.ErrorContains(t, err, "format_version 42")
}
func TestArchiveRejectsUnsafeEntries(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mode      os.FileMode
		duplicate bool
	}{{"../manifest.json", 0600, false}, {"/manifest.json", 0600, false}, {"history/transcript.jsonl", os.ModeSymlink | 0600, false}, {ManifestFile, 0600, true}, {"unexpected", 0600, false}} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			z := zip.NewWriter(&buf)
			m, _ := json.Marshal(fixtureBundle().Manifest)
			w, err := z.Create(ManifestFile)
			require.NoError(t, err)
			_, err = w.Write(m)
			require.NoError(t, err)
			h := &zip.FileHeader{Name: tc.name}
			h.SetMode(tc.mode)
			w, err = z.CreateHeader(h)
			require.NoError(t, err)
			_, err = w.Write([]byte("extra"))
			require.NoError(t, err)
			require.NoError(t, z.Close())
			_, err = Decode(buf.Bytes())
			require.Error(t, err)
		})
	}
}
func TestArchiveUnknownV1FieldsAndManifestBound(t *testing.T) {
	for _, oversize := range []bool{false, true} {
		var buf bytes.Buffer
		z := zip.NewWriter(&buf)
		w, err := z.Create(ManifestFile)
		require.NoError(t, err)
		m, _ := json.Marshal(fixtureBundle().Manifest)
		m = append(m[:len(m)-1], []byte(`,"future_field":true}`)...)
		if oversize {
			m = bytes.Repeat([]byte(" "), MaxManifestBytes+1)
		}
		_, err = w.Write(m)
		require.NoError(t, err)
		require.NoError(t, z.Close())
		_, err = Decode(buf.Bytes())
		if oversize {
			require.ErrorContains(t, err, "too large")
		} else {
			require.NoError(t, err)
		}
	}
}

func TestStableMailLedgerRoundTripMemoryAndDisk(t *testing.T) {
	b := fixtureBundle()
	b.Manifest.Agent.Identity = &db.FederationIdentity{Agent: db.NewAgentID(), Home: "home", Hops: 1, Mail: true, Proofs: map[string]string{"home": "proof"}}
	b.Manifest.MailLedger = true
	b.SetHistory("claude-jsonl", "source", []byte("native history\n"))
	b.MailDeliveries = []db.FederationMailDelivery{{Sender: "sender", Envelope: "delivered", ExpiresAt: time.Now().Add(time.Hour).UTC()}}
	raw, e := b.Encode()
	require.NoError(t, e)
	got, e := Decode(raw)
	require.NoError(t, e)
	require.Equal(t, b.MailDeliveries, got.MailDeliveries)
	path := testutil.CanonicalTempDir(t) + "/bundle.zip"
	require.NoError(t, os.WriteFile(path, raw, 0600))
	archive, e := os.Open(path)
	require.NoError(t, e)
	defer archive.Close()
	got, e = DecodeFile(archive, MaxBytes, testutil.CanonicalTempDir(t))
	require.NoError(t, e)
	defer got.Close()
	require.Equal(t, b.MailDeliveries, got.MailDeliveries)
	b.MailDeliveries[0].Envelope = string(bytes.Repeat([]byte("x"), 129))
	_, e = b.Encode()
	require.ErrorContains(t, e, "ledger entry")
}
