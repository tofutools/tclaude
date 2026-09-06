package agentd

import (
	"io"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func TestOpenCodeResumeProjectionHandoffUsesDescriptorsAfterPrivateClaim(t *testing.T) {
	handoff, err := newOpenCodeResumeProjectionHandoff()
	require.NoError(t, err)
	defer handoff.close()
	args := handoff.appendChildArgs([]string{"--resume-claim-fd", "3"})
	for _, want := range []string{
		"--opencode-projection-ready-fd", "4",
		"--opencode-projection-decision-fd", "5",
	} {
		assert.True(t, slices.Contains(args, want), "projection argv missing %q: %v", want, args)
	}
	assert.Len(t, handoff.childFiles(), 2,
		"ready writer and decision reader must immediately follow claim fd 3")
}

func TestOpenCodeResumeProjectionHandoffProjectsExactRowBeforeApproval(t *testing.T) {
	setupTestDB(t)
	const sessionID = "ses_resume_projection"
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: sessionID, ConvID: sessionID, TmuxSession: "resume-projection",
		Harness: "opencode", Status: "working", CreatedAt: time.Now().UTC(),
	}))
	launch := &openCodeLaunch{SessionID: sessionID, ConvID: sessionID}
	previous := projectOpenCodeResumeBoundary
	projectOpenCodeResumeBoundary = func(gotLaunch *openCodeLaunch, row *db.SessionRow) (bool, error) {
		assert.Same(t, launch, gotLaunch, "projection must retain the exact launch authority")
		assert.Equal(t, sessionID, row.ID)
		return true, nil
	}
	t.Cleanup(func() { projectOpenCodeResumeBoundary = previous })

	handoff, err := newOpenCodeResumeProjectionHandoff()
	require.NoError(t, err)
	defer handoff.close()
	require.NoError(t, handoff.decisionRead.SetReadDeadline(time.Now().Add(time.Second)))
	_, err = handoff.readyWrite.Write([]byte{clcommon.OpenCodeResumeProjectionReadyByte})
	require.NoError(t, err)
	require.NoError(t, handoff.projectAndApprove(launch))
	var decision [1]byte
	_, err = io.ReadFull(handoff.decisionRead, decision[:])
	require.NoError(t, err)
	assert.Equal(t, clcommon.OpenCodeResumeProjectionApprovedByte, decision[0])
}

func TestOpenCodeResumeProjectionHandoffRefusalDoesNotApproveRelease(t *testing.T) {
	setupTestDB(t)
	const sessionID = "ses_resume_projection_refused"
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: sessionID, ConvID: sessionID, TmuxSession: "resume-projection-refused",
		Harness: "opencode", Status: "working", CreatedAt: time.Now().UTC(),
	}))
	previous := projectOpenCodeResumeBoundary
	projectOpenCodeResumeBoundary = func(*openCodeLaunch, *db.SessionRow) (bool, error) {
		return false, nil
	}
	t.Cleanup(func() { projectOpenCodeResumeBoundary = previous })

	handoff, err := newOpenCodeResumeProjectionHandoff()
	require.NoError(t, err)
	defer handoff.close()
	_, err = handoff.readyWrite.Write([]byte{clcommon.OpenCodeResumeProjectionReadyByte})
	require.NoError(t, err)
	err = handoff.projectAndApprove(&openCodeLaunch{SessionID: sessionID, ConvID: sessionID})
	require.ErrorContains(t, err, "was not proven")
	assert.Nil(t, handoff.decisionWrite, "refusal must close the decision pipe so the child fails closed")
}
