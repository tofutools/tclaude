package noderun

import (
	"context"
	"errors"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/testutil"
	"io"
	"strings"
	"testing"
	"time"
)

func TestStreamingOutputIsLiveBoundedAndAuditedBeforeLaunch(t *testing.T) {
	started := make(chan struct{})
	written := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan Job, 1)
	svc, err := NewStreaming(testutil.CanonicalTempDir(t), func(ctx context.Context, _, _ string, _ int64, stdout, stderr io.Writer) Result {
		select {
		case <-started:
		default:
			return Result{ExitCode: 1, Error: "audit missing"}
		}
		_, _ = stdout.Write([]byte("live €"))
		_, _ = stderr.Write([]byte("warning"))
		close(written)
		select {
		case <-release:
		case <-ctx.Done():
			return Result{ExitCode: 130}
		}
		_, _ = stdout.Write([]byte(strings.Repeat("x", MaxOutputBytes)))
		return Result{}
	}, func(Job, Request) error { close(started); return nil }, func(j Job) { finished <- j })
	require.NoError(t, err)
	defer svc.Close()
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	j, err := svc.Start(Request{Script: "fixture"}, "operator", "", "hub", func() bool { return true })
	require.NoError(t, err)
	select {
	case <-written:
	case <-time.After(3 * time.Second):
		t.Fatal("missing live output")
	}
	live, err := svc.Job(j.ID)
	require.NoError(t, err)
	require.Equal(t, "running", live.State)
	require.Equal(t, "live €", live.StdoutTail)
	require.Equal(t, int64(len("live €")), live.StdoutBytes)
	chunk, err := svc.Log(j.ID, "stdout", 0)
	require.NoError(t, err)
	require.Equal(t, []byte("live €"), chunk.Data)
	close(release)
	select {
	case j = <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("job did not finish")
	}
	require.True(t, j.OutputTruncated)
	require.Equal(t, int64(MaxOutputBytes), j.StdoutBytes)
	require.Len(t, j.StdoutTail, TailBytes)
	require.Equal(t, "completed", j.State)
}
func TestStreamingAuditFailureNeverExecutes(t *testing.T) {
	svc, err := NewStreaming(testutil.CanonicalTempDir(t), func(context.Context, string, string, int64, io.Writer, io.Writer) Result {
		t.Error("unaudited script ran")
		return Result{}
	}, func(Job, Request) error { return errors.New("audit unavailable") }, nil)
	require.NoError(t, err)
	defer svc.Close()
	_, err = svc.Start(Request{Script: "fixture"}, "operator", "", "hub", func() bool { return true })
	require.ErrorContains(t, err, "audit unavailable")
}
