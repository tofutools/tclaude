package agent

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
)

// spoolTransport is the experimental file-based counterpart of the Unix
// socket transport: one HTTP request per envelope file in the agent's
// spool directory (see pkg/claude/common/agentipc spool docs). It slots
// in as an http.RoundTripper, so every daemon verb — retries, idempotency
// headers, error mapping — runs unchanged above it.
type spoolTransport struct {
	dir string
}

func (t *spoolTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("spool transport: read request body: %w", err)
		}
		body = b
	}
	env := agentipc.SpoolRequest{
		Method:     req.Method,
		RequestURI: req.URL.RequestURI(),
		Header:     req.Header.Clone(),
		Body:       body,
	}
	data, err := agentipc.EncodeSpoolRequest(env)
	if err != nil {
		return nil, fmt.Errorf("spool transport: encode request: %w", err)
	}
	id := uuid.NewString()
	reqPath := agentipc.SpoolEnvelopePath(agentipc.SpoolReqDir(t.dir), id)
	respPath := agentipc.SpoolEnvelopePath(agentipc.SpoolRespDir(t.dir), id)
	if err := agentipc.WriteSpoolFile(reqPath, data); err != nil {
		return nil, fmt.Errorf("spool transport: publish request: %w", err)
	}

	respData, err := awaitSpoolFile(req.Context(), respPath)
	if err != nil {
		// The daemon may not have consumed the request yet (down, or just
		// slow past our deadline). Withdraw it best-effort so it cannot
		// execute long after the caller has given up and possibly retried.
		_ = os.Remove(reqPath)
		return nil, err
	}
	_ = os.Remove(respPath)
	renv, err := agentipc.DecodeSpoolResponse(respData)
	if err != nil {
		return nil, fmt.Errorf("spool transport: decode response: %w", err)
	}
	header := renv.Header
	if header == nil {
		header = http.Header{}
	}
	return &http.Response{
		Status:        fmt.Sprintf("%d %s", renv.Status, http.StatusText(renv.Status)),
		StatusCode:    renv.Status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        header,
		Body:          io.NopCloser(bytes.NewReader(renv.Body)),
		ContentLength: int64(len(renv.Body)),
		Request:       req,
	}, nil
}

// awaitSpoolFile polls for the response envelope until it appears or ctx
// is done. The daemon publishes envelopes atomically (tmp+rename), so an
// existing file is always complete. Polling starts tight and backs off —
// a healthy daemon answers small requests within a few intervals.
func awaitSpoolFile(ctx context.Context, path string) ([]byte, error) {
	interval := 5 * time.Millisecond
	const maxInterval = 250 * time.Millisecond
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			return data, nil
		}
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("spool transport: read response: %w", err)
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("spool transport: no response at %s: %w", path, ctx.Err())
		case <-time.After(interval):
		}
		if interval < maxInterval {
			interval = min(interval*2, maxInterval)
		}
	}
}

// NewSpoolHTTPClient builds an http.Client that talks to agentd through
// the given spool directory instead of the Unix socket. Used by the
// transport selection below and by flow tests driving the spool path
// end to end.
func NewSpoolHTTPClient(dir string, timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: &spoolTransport{dir: dir}}
}

var (
	spoolChoiceOnce sync.Once
	spoolChoiceDir  string
)

// spoolTransportDir decides, once per process, whether this client should
// use the file-spool transport, returning the spool directory when it
// should and "" for the normal socket transport. The socket stays
// preferred: spool is used only when the environment provides a spool
// directory AND either TCLAUDE_AGENTD_TRANSPORT=spool forces it or no
// agentd socket is dialable (the network-isolated sandbox case, where
// connect(2) fails instantly). Memoized because CLI invocations are
// short-lived and flip-flopping transports mid-process would defeat the
// retry ladder above.
func spoolTransportDir() string {
	spoolChoiceOnce.Do(func() {
		dir := agentipc.SpoolDirFromEnv()
		if dir == "" {
			return
		}
		if agentipc.SpoolForced() || !anySocketDialable() {
			spoolChoiceDir = dir
		}
	})
	return spoolChoiceDir
}

// spoolReachable probes daemon liveness over the spool: a real /v1/info
// round trip with a short deadline. Costs one envelope file; used only by
// availability checks, not per request.
func spoolReachable(dir string) bool {
	client := NewSpoolHTTPClient(dir, 2*time.Second)
	req, err := http.NewRequest(http.MethodGet, "http://_/v1/info", nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return true
}
