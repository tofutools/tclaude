package agentd

import (
	"fmt"
	"github.com/tofutools/tclaude/pkg/claude/common/agentbundle"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/federation/bundletransfer"
	"io"
	"os"
)

func agentTransferLimit() int64 {
	cfg, err := config.Load()
	if err == nil && cfg.Federation != nil && cfg.Federation.AgentTransferMaxBytes > 0 {
		return cfg.Federation.AgentTransferMaxBytes
	}
	return agentbundle.MaxBytes
}
func agentRecordLimit() int {
	cfg, err := config.Load()
	if err == nil && cfg.Federation != nil && cfg.Federation.AgentHistoryRecordMaxBytes > 0 {
		return cfg.Federation.AgentHistoryRecordMaxBytes
	}
	return harness.MaxHistoryRecordBytes
}
func agentTransferType() bundletransfer.Type {
	kind := bundletransfer.Agent
	kind.MaxBytes = agentTransferLimit()
	kind.PendingBytes = kind.MaxBytes * 2
	return kind
}

type boundedHistoryReader struct {
	io.Reader
	limit int
}

func (r boundedHistoryReader) HistoryRecordLimit() int { return r.limit }

type agentBundleDataContextKey struct{}

func snapshotAgentHistory(b *agentbundle.Bundle, h *harness.Harness, conv, cwd string) error {
	streaming, ok := h.History.(harness.StreamingHistoryTransfer)
	if !ok {
		raw, err := h.History.Export(conv, cwd)
		if err != nil {
			return err
		}
		b.SetHistory(h.History.Format(), conv, raw)
		return nil
	}
	r, err := streaming.Open(conv, cwd)
	if err != nil {
		return err
	}
	defer r.Close()
	f, err := os.CreateTemp("", ".agent-export-history-")
	if err != nil {
		return err
	}
	success := false
	defer func() {
		if !success {
			_ = os.Remove(f.Name())
		}
	}()
	n, err := io.Copy(f, io.LimitReader(r, b.Limit()+1))
	ce := f.Close()
	if err != nil {
		return err
	}
	if ce != nil {
		return ce
	}
	if n > b.Limit() {
		return b.LimitError("history")
	}
	if err = b.SetHistoryFile(h.History.Format(), conv, f.Name(), true); err != nil {
		return err
	}
	check, err := b.OpenHistory()
	if err != nil {
		return err
	}
	err = streaming.ValidateReader(boundedHistoryReader{check, agentRecordLimit()}, conv)
	_ = check.Close()
	if err != nil {
		return err
	}
	success = true
	return nil
}
func archiveAgentBundle(b *agentbundle.Bundle) (*os.File, error) {
	f, err := os.CreateTemp("", ".agent-export-archive-")
	if err != nil {
		return nil, err
	}
	if err = b.EncodeTo(f); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, fmt.Errorf("archive exceeds or cannot be encoded within federation.agent_transfer_max_bytes=%d: %w", b.Limit(), err)
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, err
	}
	return f, nil
}
func validateBundleHistory(h *harness.Harness, b *agentbundle.Bundle) error {
	if streaming, ok := h.History.(harness.StreamingHistoryTransfer); ok {
		r, err := b.OpenHistory()
		if err != nil {
			return err
		}
		defer r.Close()
		return streaming.ValidateReader(boundedHistoryReader{r, agentRecordLimit()}, b.Manifest.History.SourceConvID)
	}
	return h.History.Validate(b.Transcript, b.Manifest.History.SourceConvID)
}

func importBundleHistory(h *harness.Harness, history *bundleHistoryLaunch, cwd string) (string, func(), error) {
	if history.Path == "" {
		return h.History.Import(history.Raw, history.SourceID, cwd)
	}
	streaming, ok := h.History.(harness.StreamingHistoryTransfer)
	if !ok {
		return "", nil, fmt.Errorf("resolved harness cannot import disk-backed history")
	}
	f, err := os.Open(history.Path)
	if err != nil {
		return "", nil, err
	}
	defer f.Close()
	return streaming.ImportReader(boundedHistoryReader{f, agentRecordLimit()}, history.SourceID, cwd)
}
