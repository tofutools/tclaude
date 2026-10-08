package bundletransfer

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/testutil"
)

type brokenEOF struct{ *bytes.Reader }

func (r brokenEOF) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}
func TestSpoolRequiresCompleteVerifiedPayload(t *testing.T) {
	id, err := proto.NewIdentity()
	require.NoError(t, err)
	s := Spool{Root: filepath.Join(testutil.CanonicalTempDir(t), "private")}
	raw := []byte("unchanged content")
	d := New(Config, raw, "offer", time.Now().Add(time.Hour))
	for _, tc := range []struct {
		name   string
		reader io.Reader
	}{
		{"truncated", bytes.NewReader(raw[:4])}, {"extra", bytes.NewReader(append(append([]byte{}, raw...), 0))}, {"wrong hash", strings.NewReader(strings.Repeat("x", len(raw)))}, {"missing FIN", brokenEOF{bytes.NewReader(raw)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Error(t, s.Receive("in", id.ID(), d, tc.reader))
			_, err := s.Open("in", id.ID(), d)
			require.Error(t, err)
			entries, err := os.ReadDir(s.Root)
			require.NoError(t, err)
			require.Empty(t, entries)
		})
	}
	require.NoError(t, s.Receive("in", id.ID(), d, bytes.NewReader(raw)))
	got, err := s.Read("in", id.ID(), d)
	require.NoError(t, err)
	require.Equal(t, raw, got)
	path, err := s.path("in", id.ID(), d.ID)
	require.NoError(t, err)
	st, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), st.Mode().Perm())
	require.NoError(t, os.Remove(path))
	target := filepath.Join(testutil.CanonicalTempDir(t), "secret")
	require.NoError(t, os.WriteFile(target, raw, 0600))
	require.NoError(t, os.Symlink(target, path))
	_, err = s.Read("in", id.ID(), d)
	require.Error(t, err)
	require.NoError(t, s.Receive("in", id.ID(), d, bytes.NewReader(raw)))
	st, err = os.Lstat(path)
	require.NoError(t, err)
	require.True(t, st.Mode().IsRegular())
	_, err = s.path("in", "../../outside", d.ID)
	require.Error(t, err)
	_, err = s.path("in", id.ID(), "../outside")
	require.Error(t, err)
}
func TestDescriptorBoundsAndInline(t *testing.T) {
	now := time.Now()
	d := New(Config, bytes.Repeat([]byte("x"), InlineLimit), "offer", now.Add(time.Hour))
	require.Len(t, d.Inline, InlineLimit)
	require.NoError(t, d.Validate(Config, now))
	large := New(Config, bytes.Repeat([]byte("x"), InlineLimit+1), "offer", now.Add(time.Hour))
	require.Empty(t, large.Inline)
	require.NoError(t, large.Validate(Config, now))
	for _, modify := range []func(*Descriptor){func(d *Descriptor) { d.Bytes = Config.MaxBytes + 1 }, func(d *Descriptor) { d.Type = "agent" }, func(d *Descriptor) { d.Group = "team" }, func(d *Descriptor) { d.ExpiresAt = now.Add(-time.Second) }, func(d *Descriptor) { d.SHA256 = "invalid" }, func(d *Descriptor) { d.Inline = []byte("edited") }} {
		copy := d
		modify(&copy)
		require.Error(t, copy.Validate(Config, now))
	}
}
