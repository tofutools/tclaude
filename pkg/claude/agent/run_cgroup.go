package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// RunCgroupRequest asks agentd to move the calling process into a fresh
// cgroup with these limits (unset axes are unlimited unless an inherited
// ceiling applies).
type RunCgroupRequest struct {
	Limits sandboxpolicy.ResourceLimits `json:"limits"`
}

// RunCgroupResponse reports the cgroup the caller now runs in and the limits
// actually written after clamping to the caller's existing ceilings.
type RunCgroupResponse struct {
	Cgroup string                       `json:"cgroup"`
	Limits sandboxpolicy.ResourceLimits `json:"limits"`
}

// RunCgroupHold keeps the connection that owns a run cgroup open. agentd
// removes the cgroup, killing anything still inside it, when that connection
// closes, so the holder keeps it until the process exits.
type RunCgroupHold struct {
	body io.ReadCloser
}

// JoinRunCgroup moves the current process into a new agentd-owned cgroup.
// Children started afterwards inherit it.
func JoinRunCgroup(limits sandboxpolicy.ResourceLimits) (*RunCgroupHold, RunCgroupResponse, error) {
	if !DaemonAvailable() {
		return nil, RunCgroupResponse{}, errors.New(daemonUnreachableMsg())
	}
	buf, err := json.Marshal(RunCgroupRequest{Limits: limits})
	if err != nil {
		return nil, RunCgroupResponse{}, err
	}
	req, err := http.NewRequest(http.MethodPost, "http://_/v1/run/cgroup", bytes.NewReader(buf))
	if err != nil {
		return nil, RunCgroupResponse{}, err
	}
	attachCallerIdentity(req)
	req.Header.Set("Content-Type", "application/json")
	// No client timeout: the response body stays open for the whole run.
	resp, err := newUnixSocketClient(0).Do(req)
	if err != nil {
		return nil, RunCgroupResponse{}, fmt.Errorf("request run cgroup: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer func() { _ = resp.Body.Close() }()
		var failure struct {
			Error string `json:"error"`
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		if json.Unmarshal(raw, &failure) == nil && failure.Error != "" {
			return nil, RunCgroupResponse{}, fmt.Errorf("run cgroup: %s", failure.Error)
		}
		return nil, RunCgroupResponse{}, fmt.Errorf("run cgroup: agentd returned %s: %s",
			resp.Status, strings.TrimSpace(string(raw)))
	}
	var out RunCgroupResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		_ = resp.Body.Close()
		return nil, RunCgroupResponse{}, fmt.Errorf("decode run cgroup response: %w", err)
	}
	return &RunCgroupHold{body: resp.Body}, out, nil
}
