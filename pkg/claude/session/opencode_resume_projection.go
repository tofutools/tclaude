package session

import (
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
	"time"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
)

var openCodeResumeProjectionTimeout = clcommon.OpenCodeResumeProjectionTimeout

// releaseAfterOpenCodeResumeProjection keeps a supported managed OpenCode
// resume behind the existing workload gate until agentd has bound its exact
// authoritative server namespace to this exact pane generation. The inherited
// descriptors are private one-shot authority: parent death becomes EOF, while
// the bounded deadline remains shorter than the pane gate's own timeout.
func releaseAfterOpenCodeResumeProjection(readyFD, decisionFD int, release func() error) error {
	if readyFD == 0 && decisionFD == 0 {
		return release()
	}
	if readyFD <= 2 || decisionFD <= 2 || readyFD == decisionFD {
		return errors.New("invalid OpenCode resume projection descriptors")
	}
	ready := os.NewFile(uintptr(readyFD), "tclaude-opencode-projection-ready")
	if ready == nil {
		return errors.New("OpenCode resume projection descriptors are unavailable")
	}
	// ExtraFiles descriptors arrive in blocking mode. os.NewFile only registers
	// a descriptor with the runtime poller when it is already nonblocking; make
	// that property explicit before wrapping the decision pipe so its deadline
	// can interrupt an in-flight read on Linux and macOS.
	if err := syscall.SetNonblock(decisionFD, true); err != nil {
		_ = ready.Close()
		_ = syscall.Close(decisionFD)
		return fmt.Errorf("prepare pollable OpenCode projection decision: %w", err)
	}
	decision := os.NewFile(uintptr(decisionFD), "tclaude-opencode-projection-decision")
	if decision == nil {
		_ = ready.Close()
		_ = syscall.Close(decisionFD)
		return errors.New("OpenCode resume projection descriptors are unavailable")
	}
	defer ready.Close()
	defer decision.Close()
	if _, err := ready.Write([]byte{clcommon.OpenCodeResumeProjectionReadyByte}); err != nil {
		return fmt.Errorf("announce OpenCode projection readiness: %w", err)
	}
	if err := decision.SetReadDeadline(time.Now().Add(openCodeResumeProjectionTimeout)); err != nil {
		return fmt.Errorf("bound OpenCode projection-decision wait: %w", err)
	}
	var response [1]byte
	if _, err := io.ReadFull(decision, response[:]); err != nil {
		return fmt.Errorf("await OpenCode boundary projection: %w", err)
	}
	if response[0] != clcommon.OpenCodeResumeProjectionApprovedByte {
		return errors.New("OpenCode boundary projection was refused")
	}
	return release()
}
