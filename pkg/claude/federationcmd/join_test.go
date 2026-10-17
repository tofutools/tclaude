package federationcmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

func TestReadJoinCodeTakesExactlyOneSource(t *testing.T) {
	_, err := readJoinCode(&joinParams{}, strings.NewReader(""))
	require.ErrorContains(t, err, "exactly one")
	_, err = readJoinCode(&joinParams{Code: "tchj_a", CodeStdin: true}, strings.NewReader(""))
	require.ErrorContains(t, err, "exactly one")
	got, err := readJoinCode(&joinParams{CodeStdin: true}, strings.NewReader("tchj_abc\n"))
	require.NoError(t, err)
	require.Equal(t, "tchj_abc\n", got)
	file := filepath.Join(t.TempDir(), "code")
	require.NoError(t, os.WriteFile(file, []byte("tchj_def"), 0o600))
	got, err = readJoinCode(&joinParams{CodeFile: file}, nil)
	require.NoError(t, err)
	require.Equal(t, "tchj_def", got)
	_, err = readJoinCode(&joinParams{CodeStdin: true}, strings.NewReader(strings.Repeat("A", proto.MaxJoinCodeBytes+1)))
	require.ErrorIs(t, err, proto.ErrJoinCode)
}

func TestDescribeJoinNamesTheCertificateAndWhatItReplaces(t *testing.T) {
	exp := time.Now().Add(time.Hour)
	p := &joinPreview{HubURL: "wss://hub.home:8470", Space: "home", Invite: true, ExpiresAt: &exp,
		CA: []proto.CertInfo{{Subject: "hub.home", SHA256: "AB:CD", NotAfter: exp, SelfSigned: true}}}
	p.Current.HubURL, p.Current.Name = "wss://old.example", "desk"
	var out bytes.Buffer
	describeJoin(&out, p, "")
	s := out.String()
	for _, want := range []string{"wss://hub.home:8470", "Space:   home", "single use, expires", "trust hub.home", "self-signed", "SHA-256 AB:CD", "Name:    desk", "Replaces the current hub wss://old.example"} {
		require.Contains(t, s, want)
	}
	out.Reset()
	p.CA, p.Current.HubURL, p.Current.HubCAFile = nil, "", "/home/me/hub.crt"
	describeJoin(&out, p, "laptop")
	require.Contains(t, out.String(), "the system's trusted certificates")
	require.Contains(t, out.String(), "Stops trusting the pinned CA /home/me/hub.crt")
	require.Contains(t, out.String(), "Name:    laptop")
}
