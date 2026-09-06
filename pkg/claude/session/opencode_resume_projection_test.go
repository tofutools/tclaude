package session

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

const openCodeProjectionTimeoutHelperEnv = "TCLAUDE_TEST_OPENCODE_PROJECTION_TIMEOUT_HELPER"

func TestManagedOpenCodeLayerResumeRequiresProjectionDescriptors(t *testing.T) {
	err := runNew(&NewParams{
		ManagedLaunch: true, Harness: harness.OpenCodeName,
		Resume: "ses_projection_required", ResumeOperationID: "op_projection_required",
		SandboxImpl: "tclaude-layer",
	})
	require.ErrorContains(t, err, "requires boundary projection handoff")
}

func TestOpenCodeProjectionDescriptorsRejectedOutsideSupportedResume(t *testing.T) {
	err := runNew(&NewParams{
		ManagedLaunch: true, Harness: harness.OpenCodeName,
		Resume: "ses_projection_unsupported", ResumeOperationID: "op_projection_unsupported",
		OpenCodeProjectionReadyFD: 4, OpenCodeProjectionDecisionFD: 5,
	})
	require.ErrorContains(t, err, "unsupported for this launch")
}

func TestReleaseAfterOpenCodeResumeProjectionWaitsForParentApproval(t *testing.T) {
	readyRead, readyWrite, err := os.Pipe()
	require.NoError(t, err)
	decisionRead, decisionWrite, err := os.Pipe()
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = readyRead.Close()
		_ = decisionWrite.Close()
	})
	require.NoError(t, readyRead.SetReadDeadline(time.Now().Add(time.Second)))
	childReadyFD, err := syscall.Dup(int(readyWrite.Fd()))
	require.NoError(t, err)
	require.NoError(t, readyWrite.Close())
	childDecisionFD, err := syscall.Dup(int(decisionRead.Fd()))
	require.NoError(t, err)
	require.NoError(t, decisionRead.Close())

	var released atomic.Bool
	done := make(chan error, 1)
	go func() {
		done <- releaseAfterOpenCodeResumeProjection(
			childReadyFD, childDecisionFD,
			func() error {
				released.Store(true)
				return nil
			},
		)
	}()

	var ready [1]byte
	_, err = io.ReadFull(readyRead, ready[:])
	require.NoError(t, err)
	assert.Equal(t, clcommon.OpenCodeResumeProjectionReadyByte, ready[0])
	assert.False(t, released.Load(), "the workload gate must remain closed before projection approval")
	_, err = decisionWrite.Write([]byte{clcommon.OpenCodeResumeProjectionApprovedByte})
	require.NoError(t, err)
	require.NoError(t, <-done)
	assert.True(t, released.Load())
}

func TestReleaseAfterOpenCodeResumeProjectionFailsClosedOnParentLoss(t *testing.T) {
	readyRead, readyWrite, err := os.Pipe()
	require.NoError(t, err)
	decisionRead, decisionWrite, err := os.Pipe()
	require.NoError(t, err)
	t.Cleanup(func() { _ = readyRead.Close() })
	require.NoError(t, readyRead.SetReadDeadline(time.Now().Add(time.Second)))
	childReadyFD, err := syscall.Dup(int(readyWrite.Fd()))
	require.NoError(t, err)
	require.NoError(t, readyWrite.Close())
	childDecisionFD, err := syscall.Dup(int(decisionRead.Fd()))
	require.NoError(t, err)
	require.NoError(t, decisionRead.Close())

	released := false
	done := make(chan error, 1)
	go func() {
		done <- releaseAfterOpenCodeResumeProjection(
			childReadyFD, childDecisionFD,
			func() error {
				released = true
				return nil
			},
		)
	}()
	var ready [1]byte
	_, err = io.ReadFull(readyRead, ready[:])
	require.NoError(t, err)
	require.NoError(t, decisionWrite.Close(), "parent loss is represented by decision-pipe EOF")
	err = <-done
	require.ErrorContains(t, err, "await OpenCode boundary projection")
	assert.False(t, released)
}

func TestReleaseAfterOpenCodeResumeProjectionInheritedPipeDeadline(t *testing.T) {
	if os.Getenv(openCodeProjectionTimeoutHelperEnv) == "1" {
		openCodeResumeProjectionTimeout = 100 * time.Millisecond
		err := releaseAfterOpenCodeResumeProjection(4, 5, func() error {
			return errors.New("release must not run without parent approval")
		})
		fmt.Fprintln(os.Stderr, err)
		if err == nil {
			os.Exit(2)
		}
		return
	}

	claim, err := os.Open(os.DevNull)
	require.NoError(t, err)
	defer claim.Close()
	readyRead, readyWrite, err := os.Pipe()
	require.NoError(t, err)
	defer readyRead.Close()
	decisionRead, decisionWrite, err := os.Pipe()
	require.NoError(t, err)
	defer decisionWrite.Close()

	cmd := exec.Command(os.Args[0], "-test.run=^TestReleaseAfterOpenCodeResumeProjectionInheritedPipeDeadline$")
	cmd.Env = append(os.Environ(), openCodeProjectionTimeoutHelperEnv+"=1")
	// Match production exactly: the private Resume claim occupies fd 3, then
	// the ready writer and decision reader arrive as fd 4 and fd 5.
	cmd.ExtraFiles = []*os.File{claim, readyWrite, decisionRead}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	require.NoError(t, cmd.Start())
	require.NoError(t, readyWrite.Close())
	require.NoError(t, decisionRead.Close())
	require.NoError(t, readyRead.SetReadDeadline(time.Now().Add(time.Second)))
	var ready [1]byte
	_, err = io.ReadFull(readyRead, ready[:])
	require.NoError(t, err)
	require.Equal(t, clcommon.OpenCodeResumeProjectionReadyByte, ready[0])

	started := time.Now()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		require.NoError(t, err, stderr.String())
		assert.GreaterOrEqual(t, time.Since(started), 75*time.Millisecond)
		assert.ErrorContains(t, errors.New(stderr.String()), "timeout")
	case <-time.After(2 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("inherited decision-pipe deadline did not interrupt the child read")
	}
}
