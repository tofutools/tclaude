package hubcmd

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/federation/hub"
	"github.com/tofutools/tclaude/pkg/federation/hubupdate"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

func TestServeParamsPlainServeWithoutSupervisorLabel(t *testing.T) {
	// Use the production command definition and its Boa validation, replacing
	// only execution so this test neither listens nor creates a hub database.
	definition := serveDefinition()
	var received *serveParams
	definition.RunFunc = func(p *serveParams, _ *cobra.Command, _ []string) { received = p }
	cmd, err := definition.ToCobraE()
	require.NoError(t, err)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--listen", "127.0.0.1:18470"})
	require.NoError(t, cmd.Execute())
	require.NotNil(t, received)
	require.Equal(t, "127.0.0.1:18470", received.Listen)
	require.Empty(t, received.SupervisorLabel)
	require.False(t, received.Supervised)
}

func TestSupervisedMacOSRequiresSupervisorLabel(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("launchd validation")
	}
	_, err := hubupdate.DetectSupervisor("")
	require.ErrorContains(t, err, "--supervisor-label")
}

func TestInviteWithURLPrintsAJoinCodeAndChecksItFirst(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "hub.sqlite")
	var out, errOut bytes.Buffer
	require.NoError(t, runInvite(&inviteParams{DBParam: DBParam{DB: db}, Space: "home", TTL: time.Hour}, &out, &errOut))
	require.True(t, strings.HasPrefix(out.String(), proto.InvitePrefix), "without --url the bare invite, as before")
	require.Empty(t, errOut.String())

	ca := filepath.Join(dir, "hub.crt")
	require.NoError(t, os.WriteFile(ca, []byte(testCertPEM(t)), 0o600))
	out.Reset()
	require.NoError(t, runInvite(&inviteParams{DBParam: DBParam{DB: db}, Space: "home", TTL: time.Hour, URL: "wss://hub.home:8470", CAFile: ca}, &out, &errOut))
	code, err := proto.DecodeJoinCode(out.String())
	require.NoError(t, err)
	require.Equal(t, "wss://hub.home:8470", code.Hub)
	require.Equal(t, "home", code.Space)
	require.True(t, strings.HasPrefix(code.Invite, proto.InvitePrefix))
	require.WithinDuration(t, time.Now().Add(time.Hour), code.Expires, time.Minute)
	require.Contains(t, code.CA, "BEGIN CERTIFICATE")
	require.Contains(t, errOut.String(), "tclaude federation join")

	st, err := hub.OpenStore(db)
	require.NoError(t, err)
	before, err := st.ListInvites()
	require.NoError(t, err)
	require.NoError(t, st.Close())
	require.Len(t, before, 2)
	for _, bad := range []inviteParams{
		{URL: "https://hub.home"},
		{CAFile: ca},
		{URL: "wss://hub.home", CAFile: filepath.Join(dir, "missing.crt")},
	} {
		bad.DBParam = DBParam{DB: db}
		bad.TTL = time.Hour
		require.Error(t, runInvite(&bad, io.Discard, io.Discard))
	}
	st, err = hub.OpenStore(db)
	require.NoError(t, err)
	defer func() { _ = st.Close() }()
	after, err := st.ListInvites()
	require.NoError(t, err)
	require.Len(t, after, 2, "a refused address or CA mints no invite")
}

func testCertPEM(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "hub.home"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}
