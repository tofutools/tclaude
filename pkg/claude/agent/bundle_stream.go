package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/tofutools/tclaude/pkg/claude/common/agentbundle"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/federation/bundletransfer"
)

func localAgentBundleLimit() int64 {
	cfg, err := config.Load()
	if err == nil && cfg.Federation != nil && cfg.Federation.AgentTransferMaxBytes > 0 {
		return cfg.Federation.AgentTransferMaxBytes
	}
	return agentbundle.MaxBytes
}

func downloadAgentArchive(path string, f *os.File) error {
	req, err := http.NewRequest(http.MethodGet, "http://_"+path, nil)
	if err != nil {
		return err
	}
	attachCallerIdentity(req)
	resp, err := doDaemonRequest(httpClientWithTimeout(bundletransfer.DefaultTTL), req, os.Stderr, defaultRetryPolicy())
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, err := daemonResponseBytes(resp)
		if err != nil {
			return err
		}
		return decodeDaemonError(resp.StatusCode, raw)
	}
	limit := localAgentBundleLimit()
	n, err := io.Copy(f, io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return err
	}
	if n > limit {
		return fmt.Errorf("archive exceeds federation.agent_transfer_max_bytes=%d; raise that node setting", limit)
	}
	return nil
}
func uploadAgentArchive(path string, f *os.File, out any) error {
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, "http://_"+path, f)
	if err != nil {
		return err
	}
	req.ContentLength = st.Size()
	req.GetBody = func() (io.ReadCloser, error) { return os.Open(f.Name()) }
	attachCallerIdentity(req)
	req.Header.Set("Content-Type", "application/zip")
	client := httpClientWithTimeout(bundletransfer.DefaultTTL)
	policy, err := retryPolicyForRequest(client, req, os.Stderr)
	if err != nil {
		return err
	}
	resp, err := doDaemonRequest(client, req, os.Stderr, policy)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := daemonResponseBytes(resp)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return decodeDaemonError(resp.StatusCode, raw)
	}
	if out != nil && len(raw) > 0 {
		return json.Unmarshal(raw, out)
	}
	return nil
}
