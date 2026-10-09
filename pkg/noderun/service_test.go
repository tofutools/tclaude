package noderun

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func TestDurableJobAndEscapedOutput(t *testing.T) {
	dir := testutil.CanonicalTempDir(t)
	finished := make(chan Job, 1)
	script := "printf 'operator data; $(literal)'"
	output := strings.Repeat("\x01", TailBytes)
	svc, err := New(dir, func(_ context.Context, path, _ string, _ int64) Result {
		raw, err := os.ReadFile(path)
		if err != nil || string(raw) != script {
			return Result{ExitCode: 1, Error: "script changed"}
		}
		return Result{Stdout: output, Stderr: output}
	}, func(j Job) { finished <- j })
	require.NoError(t, err)
	j, err := svc.Start(Request{Script: script}, "operator", "peer-a", "node", func() bool { return true })
	require.NoError(t, err)
	select {
	case j = <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("job did not finish")
	}
	require.Equal(t, "completed", j.State)
	require.Equal(t, output, j.StdoutTail)
	svc.Close()
	info, err := os.Stat(filepath.Join(dir, j.ID, "script.sh"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), info.Mode().Perm())
	reloaded, err := New(dir, nil, nil)
	require.NoError(t, err)
	defer reloaded.Close()
	saved, err := reloaded.Job(j.ID)
	require.NoError(t, err)
	require.Equal(t, j.ScriptSHA256, saved.ScriptSHA256)
	require.Equal(t, output, saved.StderrTail)
	log, err := reloaded.Log(j.ID, "stdout", 0)
	require.NoError(t, err)
	require.Equal(t, []byte(output), log.Data)
	require.True(t, log.EOF)
	_, err = reloaded.Log(j.ID, "../../script.sh", 0)
	require.Error(t, err)
}

func TestRevocationAndWorkerLimit(t *testing.T) {
	var authorized atomic.Bool
	authorized.Store(true)
	svc, err := New(testutil.CanonicalTempDir(t), func(ctx context.Context, _, _ string, _ int64) Result {
		<-ctx.Done()
		return Result{ExitCode: 130}
	}, nil)
	require.NoError(t, err)
	defer svc.Close()
	ids := []string{}
	for range 4 {
		j, err := svc.Start(Request{Script: "fixture"}, "operator", "peer", "node", authorized.Load)
		require.NoError(t, err)
		ids = append(ids, j.ID)
	}
	_, err = svc.Start(Request{Script: "fixture"}, "operator", "peer", "node", authorized.Load)
	require.ErrorIs(t, err, ErrBusy)
	authorized.Store(false)
	require.Eventually(t, func() bool {
		for _, id := range ids {
			j, err := svc.Job(id)
			if err != nil || j.State != "canceled" {
				return false
			}
		}
		return true
	}, 3*time.Second, 10*time.Millisecond)
}

func TestRestartDoesNotReplay(t *testing.T) {
	dir := testutil.CanonicalTempDir(t)
	id := strings.Repeat("a", 32)
	require.NoError(t, os.Mkdir(filepath.Join(dir, id), 0700))
	raw, err := json.Marshal(Job{ID: id, State: "running", CreatedAt: time.Now().UTC()})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, id, "job.json"), raw, 0600))
	svc, err := New(dir, func(context.Context, string, string, int64) Result { t.Error("job replayed"); return Result{} }, nil)
	require.NoError(t, err)
	defer svc.Close()
	j, err := svc.Job(id)
	require.NoError(t, err)
	require.Equal(t, "interrupted", j.State)
	require.Equal(t, 125, j.ExitCode)
}
