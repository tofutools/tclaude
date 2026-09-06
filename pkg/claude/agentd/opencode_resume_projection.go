package agentd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

const (
	openCodeProjectionReadyChildFD    = 4 // fd 3 is the managed Resume claim
	openCodeProjectionDecisionChildFD = 5
)

type openCodeResumeProjectionHandoff struct {
	readyRead     *os.File
	readyWrite    *os.File
	decisionRead  *os.File
	decisionWrite *os.File
}

var projectOpenCodeResumeBoundary = projectOpenCodeExecutionBoundary

func newOpenCodeResumeProjectionHandoff() (*openCodeResumeProjectionHandoff, error) {
	readyRead, readyWrite, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("create OpenCode projection-ready handoff: %w", err)
	}
	decisionRead, decisionWrite, err := os.Pipe()
	if err != nil {
		_ = readyRead.Close()
		_ = readyWrite.Close()
		return nil, fmt.Errorf("create OpenCode projection-decision handoff: %w", err)
	}
	return &openCodeResumeProjectionHandoff{
		readyRead: readyRead, readyWrite: readyWrite,
		decisionRead: decisionRead, decisionWrite: decisionWrite,
	}, nil
}

func (h *openCodeResumeProjectionHandoff) appendChildArgs(args []string) []string {
	return append(args,
		"--opencode-projection-ready-fd", strconv.Itoa(openCodeProjectionReadyChildFD),
		"--opencode-projection-decision-fd", strconv.Itoa(openCodeProjectionDecisionChildFD))
}

func (h *openCodeResumeProjectionHandoff) childFiles() []*os.File {
	return []*os.File{h.readyWrite, h.decisionRead}
}

// parentStarted drops the parent's copies of the child pipe ends. This is
// load-bearing: if the child dies, the parent must observe EOF rather than keep
// its own writer alive; likewise a parent rejection must become EOF in child.
func (h *openCodeResumeProjectionHandoff) parentStarted() {
	_ = h.readyWrite.Close()
	h.readyWrite = nil
	_ = h.decisionRead.Close()
	h.decisionRead = nil
}

func (h *openCodeResumeProjectionHandoff) closeDecision() {
	if h.decisionWrite != nil {
		_ = h.decisionWrite.Close()
		h.decisionWrite = nil
	}
}

func (h *openCodeResumeProjectionHandoff) close() {
	for _, file := range []*os.File{h.readyRead, h.readyWrite, h.decisionRead, h.decisionWrite} {
		if file != nil {
			_ = file.Close()
		}
	}
}

func (h *openCodeResumeProjectionHandoff) projectAndApprove(launch *openCodeLaunch) (err error) {
	if h == nil || launch == nil || h.readyRead == nil || h.decisionWrite == nil {
		return errors.New("OpenCode resume projection handoff is unavailable")
	}
	defer func() {
		if err != nil {
			h.closeDecision()
		}
	}()
	deadline := time.Now().Add(clcommon.OpenCodeResumeProjectionTimeout)
	if err := h.readyRead.SetReadDeadline(deadline); err != nil {
		return fmt.Errorf("bound OpenCode projection-ready wait: %w", err)
	}
	if err := h.decisionWrite.SetWriteDeadline(deadline); err != nil {
		return fmt.Errorf("bound OpenCode projection-decision handoff: %w", err)
	}
	var ready [1]byte
	if _, err := io.ReadFull(h.readyRead, ready[:]); err != nil {
		return fmt.Errorf("await exact OpenCode Resume launch binding: %w", err)
	}
	if ready[0] != clcommon.OpenCodeResumeProjectionReadyByte {
		return errors.New("OpenCode Resume child sent invalid projection readiness")
	}
	row, err := db.LoadSession(launch.SessionID)
	if err != nil {
		return fmt.Errorf("load exact OpenCode Resume session: %w", err)
	}
	projected, err := projectOpenCodeResumeBoundary(launch, row)
	if err != nil {
		return fmt.Errorf("project exact OpenCode Resume execution boundary: %w", err)
	}
	if !projected {
		return errors.New("exact OpenCode Resume execution boundary was not proven")
	}
	if _, err := h.decisionWrite.Write([]byte{clcommon.OpenCodeResumeProjectionApprovedByte}); err != nil {
		return fmt.Errorf("approve OpenCode Resume boundary projection: %w", err)
	}
	return nil
}
