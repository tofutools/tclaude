package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/common/buildversion"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func fixture(t *testing.T) (*Service, []Binary) {
	t.Helper()
	dir := testutil.CanonicalTempDir(t)
	binaries := []Binary{}
	for _, name := range []string{"tclaude", "tclaude-agentd"} {
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, []byte("old-"+name), 0755))
		binaries = append(binaries, Binary{Name: name, Path: path, Version: "v1.0.0", Method: "release"})
	}
	s, err := New(filepath.Join(dir, "updates"), binaries, Hooks{})
	require.NoError(t, err)
	s.release = func(context.Context, string) (Release, error) { return Release{Tag: "v1.1.0"}, nil }
	s.stageRelease = func(_ context.Context, _ Release, name, dir string) (string, error) {
		path := filepath.Join(dir, name)
		return path, os.WriteFile(path, []byte("new-"+name), 0700)
	}
	s.inspect = func(context.Context, string) (buildversion.Info, error) {
		return buildversion.Info{Version: "v1.1.0", ProtocolVersion: s.status.ProtocolVersion}, nil
	}
	return s, binaries
}
func waitJob(t *testing.T, s *Service, id string) Job {
	t.Helper()
	var job Job
	require.Eventually(t, func() bool {
		status := s.Status()
		if status.Job == nil || status.Job.ID != id {
			return false
		}
		job = *status.Job
		return job.State != "running"
	}, 3*time.Second, time.Millisecond)
	return job
}
func TestUpdateApplyDurableRestartAndRollback(t *testing.T) {
	s, binaries := fixture(t)
	restarted := make(chan struct{}, 1)
	s.hooks.Restart = func() error {
		job, err := s.Job(s.Status().Job.ID)
		if err != nil || job.State != "restarting" {
			return errors.New("result not durable")
		}
		restarted <- struct{}{}
		return nil
	}
	j, err := s.Start(Request{Action: "apply"}, "operator", func() bool { return true })
	require.NoError(t, err)
	require.Equal(t, "restarting", waitJob(t, s, j.ID).State)
	select {
	case <-restarted:
	case <-time.After(time.Second):
		t.Fatal("restart not requested")
	}
	_, err = s.Start(Request{Action: "check"}, "operator", func() bool { return true })
	require.ErrorIs(t, err, ErrBusy)
	for _, b := range binaries {
		raw, err := os.ReadFile(b.Path)
		require.NoError(t, err)
		require.Equal(t, "new-"+b.Name, string(raw))
	}
	recovered, err := New(s.dir, binaries, Hooks{})
	require.NoError(t, err)
	job, err := recovered.Job(j.ID)
	require.NoError(t, err)
	require.Equal(t, "succeeded", job.State, job.Error)
	j, err = recovered.Start(Request{Action: "rollback"}, "operator", func() bool { return true })
	require.NoError(t, err)
	require.Equal(t, "restarting", waitJob(t, recovered, j.ID).State)
	for _, b := range binaries {
		raw, err := os.ReadFile(b.Path)
		require.NoError(t, err)
		require.Equal(t, "old-"+b.Name, string(raw))
	}
	recovered, err = New(s.dir, binaries, Hooks{})
	require.NoError(t, err)
	job, err = recovered.Job(j.ID)
	require.NoError(t, err)
	require.Equal(t, "succeeded", job.State, job.Error)
}
func TestUpdateRevocationRestoresAllReplacedBinaries(t *testing.T) {
	s, binaries := fixture(t)
	calls := 0
	j, err := s.Start(Request{Action: "apply"}, "peer", func() bool { calls++; return calls < 3 })
	require.NoError(t, err)
	require.Equal(t, "failed", waitJob(t, s, j.ID).State)
	for _, b := range binaries {
		raw, err := os.ReadFile(b.Path)
		require.NoError(t, err)
		require.Equal(t, "old-"+b.Name, string(raw))
	}
}
func TestUpdateProtocolMismatchDoesNotReplace(t *testing.T) {
	s, binaries := fixture(t)
	s.inspect = func(context.Context, string) (buildversion.Info, error) {
		return buildversion.Info{Version: "v1.1.0", ProtocolVersion: s.status.ProtocolVersion + 1}, nil
	}
	j, err := s.Start(Request{Action: "apply"}, "operator", func() bool { return true })
	require.NoError(t, err)
	require.Contains(t, waitJob(t, s, j.ID).Error, "protocol")
	for _, b := range binaries {
		raw, err := os.ReadFile(b.Path)
		require.NoError(t, err)
		require.Equal(t, "old-"+b.Name, string(raw))
	}
	require.False(t, s.Status().RollbackAvailable)
}
func TestRollbackRefusesExternalChangesAndRecoveryDetectsMismatch(t *testing.T) {
	s, binaries := fixture(t)
	j, err := s.Start(Request{Action: "apply"}, "operator", func() bool { return true })
	require.NoError(t, err)
	waitJob(t, s, j.ID)
	require.NoError(t, os.WriteFile(binaries[1].Path, []byte("external"), 0755))
	recovered, err := New(s.dir, binaries, Hooks{})
	require.NoError(t, err)
	job, err := recovered.Job(j.ID)
	require.NoError(t, err)
	require.Equal(t, "failed", job.State)
	j, err = recovered.Start(Request{Action: "rollback"}, "operator", func() bool { return true })
	require.NoError(t, err)
	require.Contains(t, waitJob(t, recovered, j.ID).Error, "outside updater")
	raw, err := os.ReadFile(binaries[0].Path)
	require.NoError(t, err)
	require.Equal(t, "new-tclaude", string(raw))
}
func TestUpdateValidationAndSourceLatest(t *testing.T) {
	for _, req := range []Request{{Action: "other"}, {Action: "apply", Version: "latest"}, {Action: "apply", Version: "v1.2"}, {Action: "rollback", Version: "v1.0.0"}, {Action: "apply", Version: "v1.0.0;touch bad"}} {
		require.Error(t, req.Validate())
	}
	require.NoError(t, (Request{Action: "apply", Version: "v1.0.0-rc.1"}).Validate())
	s, binaries := fixture(t)
	s.binaries[0].Method = "source"
	s.stageGo = func(_ context.Context, name, version, dir string) (string, error) {
		require.Equal(t, "latest", version)
		return s.stageRelease(context.Background(), Release{}, name, dir)
	}
	j, err := s.Start(Request{Action: "apply"}, "operator", func() bool { return true })
	require.NoError(t, err)
	require.Equal(t, "restarting", waitJob(t, s, j.ID).State)
	require.Equal(t, "source", binaries[0].Method)
}
func TestReleaseArchiveAndChecksumValidation(t *testing.T) {
	archive := func(path string, kind byte) []byte {
		var out bytes.Buffer
		gz := gzip.NewWriter(&out)
		tw := tar.NewWriter(gz)
		require.NoError(t, tw.WriteHeader(&tar.Header{Name: path, Typeflag: kind, Size: 3, Mode: 0755}))
		if kind == tar.TypeReg {
			_, err := tw.Write([]byte("bin"))
			require.NoError(t, err)
		}
		require.NoError(t, tw.Close())
		require.NoError(t, gz.Close())
		return out.Bytes()
	}
	dir := testutil.CanonicalTempDir(t)
	require.NoError(t, extractBinary(archive("build/tclaude", tar.TypeReg), "tclaude", filepath.Join(dir, "ok")))
	require.Error(t, extractBinary(archive("../tclaude", tar.TypeReg), "tclaude", filepath.Join(dir, "escape")))
	require.Error(t, extractBinary(archive("build/tclaude", tar.TypeSymlink), "tclaude", filepath.Join(dir, "link")))
	sum := hex.EncodeToString(make([]byte, 32)) + "  archive.tar.gz\n"
	_, err := checksumFor([]byte(sum), "archive.tar.gz")
	require.NoError(t, err)
	_, err = checksumFor([]byte(sum+sum), "archive.tar.gz")
	require.Error(t, err)
	_, err = checksumFor([]byte(strings.Repeat("z", 64)+" archive.tar.gz"), "archive.tar.gz")
	require.Error(t, err)
}
